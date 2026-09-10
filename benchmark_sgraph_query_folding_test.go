package graphql

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/graphql-go/graphql/language/ast"
)

var benchmarkQueryFoldingResultSink atomic.Pointer[Result]

type queryFoldingBenchmarkFixture struct {
	schema    Schema
	document  *ast.Document
	engine    *SGraphEngine
	query     string
	variables map[string]any
}

type queryFoldingBenchmarkArm struct {
	name      string
	operation string
	sgraph    bool
}

func runQueryFoldingBenchmarkArms(b *testing.B, fixture *queryFoldingBenchmarkFixture, arms []queryFoldingBenchmarkArm) {
	b.Helper()
	if fixture == nil || fixture.engine == nil || fixture.document == nil {
		b.Fatal("query folding benchmark fixture is incomplete")
	}
	variablesJSON, err := json.Marshal(fixture.variables)
	if err != nil {
		b.Fatalf("marshal benchmark variables failed: %v", err)
	}

	// GraphQLGo执行相同的Folded operation，作为所有SGraph编排模式的结果基线。
	nativeParams := ExecuteParams{
		Schema:        fixture.schema,
		AST:           fixture.document,
		OperationName: "Folded",
		Args:          fixture.variables,
		Context:       context.Background(),
	}
	nativeWarm := ExecuteGraphQLGo(nativeParams)
	queryFoldingBenchmarkRequireSuccess(b, "GraphQLGo preflight", nativeWarm)
	nativeData := toPlainValue(nativeWarm.Data)

	for _, arm := range arms {
		arm := arm
		var execute func() *Result
		if arm.sgraph {
			operationName := arm.operation
			execute = func() *Result {
				return fixture.engine.Execute(
					fixture.document,
					fixture.variables,
					&operationName,
					nil,
					context.Background(),
				).toGraphQLResult()
			}
		} else {
			execute = func() *Result { return ExecuteGraphQLGo(nativeParams) }
		}

		warm := execute()
		queryFoldingBenchmarkRequireSuccess(b, arm.name+" preflight", warm)
		if actual := toPlainValue(warm.Data); !reflect.DeepEqual(actual, nativeData) {
			b.Fatalf("%s preflight data differs from GraphQLGo\nactual:   %#v\nexpected: %#v", arm.name, actual, nativeData)
		}
		responseJSON, err := json.Marshal(warm)
		if err != nil {
			b.Fatalf("marshal %s warm response failed: %v", arm.name, err)
		}

		b.Run(arm.name, func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			wallStart, cpuStart, cpuOK := nativeBenchmarkStartTiming()
			for index := 0; index < b.N; index++ {
				benchmarkQueryFoldingResultSink.Store(execute())
			}
			b.StopTimer()
			b.ReportMetric(float64(len(fixture.query)), "request-B/op")
			b.ReportMetric(float64(len(variablesJSON)), "variables-B/op")
			b.ReportMetric(float64(len(responseJSON)), "response-B/op")
			nativeBenchmarkReportTiming(b, wallStart, cpuStart, cpuOK)
		})
	}
}

func queryFoldingBenchmarkRequireSuccess(tb testing.TB, label string, result *Result) {
	tb.Helper()
	if result == nil {
		tb.Fatalf("%s returned nil", label)
	}
	if len(result.Errors) != 0 {
		tb.Fatalf("%s returned errors: %#v", label, result.Errors)
	}
}

func queryFoldingBenchmarkDelay(delay time.Duration) {
	if delay > 0 {
		time.Sleep(delay)
	}
}

func queryFoldingBenchmarkOperation(name, selection string) string {
	return fmt.Sprintf("query %s($id: ID!) { root(id: $id) { %s } }", name, selection)
}

func newWideQueryFoldingBenchmarkFixture(b testing.TB, width int, delay time.Duration, payloadBytes int) *queryFoldingBenchmarkFixture {
	b.Helper()
	if width < 1 {
		b.Fatal("wide benchmark width must be positive")
	}
	if payloadBytes < 1 {
		payloadBytes = 1
	}
	args := FieldConfigArgument{"id": &ArgumentConfig{Type: NewNonNull(ID)}}
	fields := make(Fields, width+1)
	fields["id"] = &Field{Type: NewNonNull(ID)}
	payload := strings.Repeat("x", payloadBytes)
	var selection strings.Builder
	for index := 0; index < width; index++ {
		fieldName := fmt.Sprintf("f%03d", index)
		selection.WriteString(fieldName)
		selection.WriteString("(id: $id) ")
		prefix := fieldName + ":"
		fields[fieldName] = &Field{
			Type: String,
			Args: args,
			Resolve: func(p ResolveParams) (any, error) {
				queryFoldingBenchmarkDelay(delay)
				return prefix + fmt.Sprint(p.Args["id"]) + payload, nil
			},
		}
	}
	pageTypeName := fmt.Sprintf("QueryFoldingWidePage%dP%dD%d", width, payloadBytes, delay.Nanoseconds())
	pageType := NewObject(ObjectConfig{Name: pageTypeName, Fields: fields})
	queryType := NewObject(ObjectConfig{Name: "Query", Fields: Fields{
		"root": &Field{
			Type: pageType,
			Args: args,
			Resolve: func(p ResolveParams) (any, error) {
				queryFoldingBenchmarkDelay(delay)
				return map[string]any{"id": p.Args["id"]}, nil
			},
		},
	}})
	schema, err := NewSchema(SchemaConfig{Query: queryType, Types: []Type{pageType}})
	if err != nil {
		b.Fatalf("create wide query folding schema failed: %v", err)
	}
	query := strings.Join([]string{
		queryFoldingBenchmarkOperation("Folded", selection.String()),
		queryFoldingBenchmarkOperation("Dependent", selection.String()),
		queryFoldingBenchmarkOperation("Mixed", selection.String()),
	}, "\n")
	document := parseAndValidateQueryFoldingDocument(b, schema, query)
	registry := NewParamRegistry()
	for _, operation := range []struct {
		name  string
		mixed bool
	}{{name: "Dependent"}, {name: "Mixed", mixed: true}} {
		bindings := make([]FieldParamBinding, 0, width)
		for index := 0; index < width; index++ {
			if operation.mixed && index%2 != 0 {
				continue
			}
			fieldName := fmt.Sprintf("f%03d", index)
			bindings = append(bindings, queryFoldingFieldBinding(
				[]string{"root", fieldName}, pageTypeName, fieldName, "id",
				[]string{"root"}, "Query", "root", "id",
			))
		}
		if err := registry.RegisterQuery(QueryParamConfig{
			DocumentBody:  query,
			OperationName: operation.name,
			FieldParams:   bindings,
		}); err != nil {
			b.Fatalf("register wide %s dependencies failed: %v", operation.name, err)
		}
	}
	engine := newQueryFoldingEngine(b, &schema, nil, registry)
	return &queryFoldingBenchmarkFixture{
		schema:    schema,
		document:  document,
		engine:    engine,
		query:     query,
		variables: map[string]any{"id": "benchmark-id"},
	}
}

func BenchmarkSGraphQueryFoldingWide(b *testing.B) {
	for _, width := range []int{8, 32, 128} {
		for _, delay := range []time.Duration{0, 200 * time.Microsecond, 5 * time.Millisecond} {
			name := fmt.Sprintf("Width%d/Delay%s", width, delay)
			b.Run(name, func(b *testing.B) {
				fixture := newWideQueryFoldingBenchmarkFixture(b, width, delay, 16)
				runQueryFoldingBenchmarkArms(b, fixture, []queryFoldingBenchmarkArm{
					{name: "SGraph/Folded", operation: "Folded", sgraph: true},
					{name: "SGraph/Mixed", operation: "Mixed", sgraph: true},
					{name: "SGraph/Dependent", operation: "Dependent", sgraph: true},
					{name: "GraphQLGo", operation: "Folded"},
				})
			})
		}
	}
}

func newDeepQueryFoldingBenchmarkFixture(b testing.TB, depth int, delay time.Duration) *queryFoldingBenchmarkFixture {
	b.Helper()
	if depth < 2 {
		b.Fatal("deep benchmark depth must include root and value")
	}
	args := FieldConfigArgument{"id": &ArgumentConfig{Type: NewNonNull(ID)}}
	typeName := fmt.Sprintf("QueryFoldingDeepNode%dD%d", depth, delay.Nanoseconds())
	var nodeType *Object
	nodeType = NewObject(ObjectConfig{Name: typeName, Fields: FieldsThunk(func() Fields {
		return Fields{
			"id": &Field{Type: NewNonNull(ID)},
			"next": &Field{
				Type: nodeType,
				Args: args,
				Resolve: func(p ResolveParams) (any, error) {
					queryFoldingBenchmarkDelay(delay)
					return map[string]any{"id": p.Args["id"]}, nil
				},
			},
			"value": &Field{
				Type: String,
				Args: args,
				Resolve: func(p ResolveParams) (any, error) {
					queryFoldingBenchmarkDelay(delay)
					return fmt.Sprint(p.Args["id"]), nil
				},
			},
		}
	})})
	queryType := NewObject(ObjectConfig{Name: "Query", Fields: Fields{
		"root": &Field{
			Type: nodeType,
			Args: args,
			Resolve: func(p ResolveParams) (any, error) {
				queryFoldingBenchmarkDelay(delay)
				return map[string]any{"id": p.Args["id"]}, nil
			},
		},
	}})
	schema, err := NewSchema(SchemaConfig{Query: queryType, Types: []Type{nodeType}})
	if err != nil {
		b.Fatalf("create deep query folding schema failed: %v", err)
	}
	var selection strings.Builder
	for level := 1; level < depth-1; level++ {
		selection.WriteString("next(id: $id) { ")
	}
	selection.WriteString("value(id: $id)")
	for level := 1; level < depth-1; level++ {
		selection.WriteString(" }")
	}
	query := strings.Join([]string{
		queryFoldingBenchmarkOperation("Folded", selection.String()),
		queryFoldingBenchmarkOperation("Dependent", selection.String()),
	}, "\n")
	document := parseAndValidateQueryFoldingDocument(b, schema, query)
	registry := NewParamRegistry()
	bindings := make([]FieldParamBinding, 0, depth-1)
	producerPath := []string{"root"}
	producerParentType := "Query"
	producerField := "root"
	for level := 1; level < depth-1; level++ {
		targetPath := append(append([]string(nil), producerPath...), "next")
		bindings = append(bindings, queryFoldingFieldBinding(
			targetPath, typeName, "next", "id",
			producerPath, producerParentType, producerField, "id",
		))
		producerPath = targetPath
		producerParentType = typeName
		producerField = "next"
	}
	valuePath := append(append([]string(nil), producerPath...), "value")
	bindings = append(bindings, queryFoldingFieldBinding(
		valuePath, typeName, "value", "id",
		producerPath, producerParentType, producerField, "id",
	))
	if err := registry.RegisterQuery(QueryParamConfig{
		DocumentBody:  query,
		OperationName: "Dependent",
		FieldParams:   bindings,
	}); err != nil {
		b.Fatalf("register deep dependencies failed: %v", err)
	}
	return &queryFoldingBenchmarkFixture{
		schema:    schema,
		document:  document,
		engine:    newQueryFoldingEngine(b, &schema, nil, registry),
		query:     query,
		variables: map[string]any{"id": "benchmark-id"},
	}
}

func BenchmarkSGraphQueryFoldingDeep(b *testing.B) {
	for _, depth := range []int{4, 8, 16} {
		for _, delay := range []time.Duration{0, 200 * time.Microsecond, 5 * time.Millisecond} {
			name := fmt.Sprintf("Depth%d/Delay%s", depth, delay)
			b.Run(name, func(b *testing.B) {
				fixture := newDeepQueryFoldingBenchmarkFixture(b, depth, delay)
				runQueryFoldingBenchmarkArms(b, fixture, []queryFoldingBenchmarkArm{
					{name: "SGraph/Folded", operation: "Folded", sgraph: true},
					{name: "SGraph/Dependent", operation: "Dependent", sgraph: true},
					{name: "GraphQLGo", operation: "Folded"},
				})
			})
		}
	}
}

func newListQueryFoldingBenchmarkFixture(b testing.TB, count int, delay time.Duration) *queryFoldingBenchmarkFixture {
	b.Helper()
	itemTypeName := fmt.Sprintf("QueryFoldingBenchListItem%dD%d", count, delay.Nanoseconds())
	pageTypeName := fmt.Sprintf("QueryFoldingBenchListPage%dD%d", count, delay.Nanoseconds())
	itemType := NewObject(ObjectConfig{Name: itemTypeName, Fields: Fields{
		"id": &Field{Type: NewNonNull(ID)},
		"score": &Field{
			Type: Int,
			Args: FieldConfigArgument{"multiplier": &ArgumentConfig{Type: NewNonNull(Int)}},
			Resolve: func(p ResolveParams) (any, error) {
				queryFoldingBenchmarkDelay(delay)
				return p.Args["multiplier"].(int), nil
			},
		},
	}})
	pageType := NewObject(ObjectConfig{Name: pageTypeName, Fields: Fields{
		"items": &Field{
			Type: NewList(itemType),
			Args: FieldConfigArgument{"count": &ArgumentConfig{Type: NewNonNull(Int)}},
			Resolve: func(p ResolveParams) (any, error) {
				queryFoldingBenchmarkDelay(delay)
				items := make([]map[string]any, p.Args["count"].(int))
				for index := range items {
					items[index] = map[string]any{"id": fmt.Sprintf("item-%d", index)}
				}
				return items, nil
			},
		},
	}})
	queryType := NewObject(ObjectConfig{Name: "Query", Fields: Fields{
		"root": &Field{
			Type: pageType,
			Args: FieldConfigArgument{"count": &ArgumentConfig{Type: NewNonNull(Int)}},
			Resolve: func(p ResolveParams) (any, error) {
				queryFoldingBenchmarkDelay(delay)
				return map[string]any{"count": p.Args["count"]}, nil
			},
		},
	}})
	schema, err := NewSchema(SchemaConfig{Query: queryType, Types: []Type{pageType, itemType}})
	if err != nil {
		b.Fatalf("create list query folding schema failed: %v", err)
	}
	selection := `items(count: $count) { id score(multiplier: $multiplier) }`
	query := fmt.Sprintf(
		"query Folded($count: Int!, $multiplier: Int!) { root(count: $count) { %s } }\nquery Dependent($count: Int!, $multiplier: Int!) { root(count: $count) { %s } }",
		selection,
		selection,
	)
	document := parseAndValidateQueryFoldingDocument(b, schema, query)
	registry := NewParamRegistry()
	if err := registry.RegisterQuery(QueryParamConfig{
		DocumentBody:  query,
		OperationName: "Dependent",
		FieldParams: []FieldParamBinding{
			queryFoldingFieldBinding(
				[]string{"root", "items"}, pageTypeName, "items", "count",
				[]string{"root"}, "Query", "root", "count",
			),
		},
	}); err != nil {
		b.Fatalf("register list dependencies failed: %v", err)
	}
	return &queryFoldingBenchmarkFixture{
		schema:    schema,
		document:  document,
		engine:    newQueryFoldingEngine(b, &schema, nil, registry),
		query:     query,
		variables: map[string]any{"count": count, "multiplier": 3},
	}
}

func BenchmarkSGraphQueryFoldingListBoundary(b *testing.B) {
	for _, count := range []int{8, 64} {
		for _, delay := range []time.Duration{0, 200 * time.Microsecond} {
			name := fmt.Sprintf("Items%d/Delay%s", count, delay)
			b.Run(name, func(b *testing.B) {
				fixture := newListQueryFoldingBenchmarkFixture(b, count, delay)
				runQueryFoldingBenchmarkArms(b, fixture, []queryFoldingBenchmarkArm{
					{name: "SGraph/Folded", operation: "Folded", sgraph: true},
					{name: "SGraph/Dependent", operation: "Dependent", sgraph: true},
					{name: "GraphQLGo", operation: "Folded"},
				})
			})
		}
	}
}

func newAbstractQueryFoldingBenchmarkFixture(b testing.TB, delay time.Duration) *queryFoldingBenchmarkFixture {
	b.Helper()
	var userType *Object
	var robotType *Object
	interfaceName := fmt.Sprintf("QueryFoldingBenchEntityD%d", delay.Nanoseconds())
	userTypeName := fmt.Sprintf("QueryFoldingBenchUserD%d", delay.Nanoseconds())
	robotTypeName := fmt.Sprintf("QueryFoldingBenchRobotD%d", delay.Nanoseconds())
	pageTypeName := fmt.Sprintf("QueryFoldingBenchAbstractPageD%d", delay.Nanoseconds())
	entityType := NewInterface(InterfaceConfig{
		Name:   interfaceName,
		Fields: Fields{"id": &Field{Type: NewNonNull(ID)}},
		ResolveType: func(p ResolveTypeParams) *Object {
			if p.Value.(map[string]any)["kind"] == "user" {
				return userType
			}
			return robotType
		},
	})
	userType = NewObject(ObjectConfig{Name: userTypeName, Interfaces: []*Interface{entityType}, Fields: Fields{
		"id":   &Field{Type: NewNonNull(ID)},
		"name": &Field{Type: String},
	}})
	robotType = NewObject(ObjectConfig{Name: robotTypeName, Interfaces: []*Interface{entityType}, Fields: Fields{
		"id":     &Field{Type: NewNonNull(ID)},
		"serial": &Field{Type: String},
	}})
	pageType := NewObject(ObjectConfig{Name: pageTypeName, Fields: Fields{
		"entity": &Field{
			Type: entityType,
			Args: FieldConfigArgument{"kind": &ArgumentConfig{Type: NewNonNull(String)}},
			Resolve: func(p ResolveParams) (any, error) {
				queryFoldingBenchmarkDelay(delay)
				if p.Args["kind"] == "user" {
					return map[string]any{"kind": "user", "id": "u1", "name": "Ada"}, nil
				}
				return map[string]any{"kind": "robot", "id": "r1", "serial": "RX"}, nil
			},
		},
	}})
	queryType := NewObject(ObjectConfig{Name: "Query", Fields: Fields{
		"root": &Field{
			Type: pageType,
			Args: FieldConfigArgument{"kind": &ArgumentConfig{Type: NewNonNull(String)}},
			Resolve: func(p ResolveParams) (any, error) {
				queryFoldingBenchmarkDelay(delay)
				return map[string]any{"kind": p.Args["kind"]}, nil
			},
		},
	}})
	schema, err := NewSchema(SchemaConfig{
		Query: queryType,
		Types: []Type{pageType, entityType, userType, robotType},
	})
	if err != nil {
		b.Fatalf("create abstract query folding schema failed: %v", err)
	}
	selection := fmt.Sprintf(`entityAlias: entity(kind: $kind) { __typename id ... on %s { name } ... on %s { serial } }`, userTypeName, robotTypeName)
	query := fmt.Sprintf(
		"query Folded($kind: String!) { root(kind: $kind) { %s } }\nquery Dependent($kind: String!) { root(kind: $kind) { %s } }",
		selection,
		selection,
	)
	document := parseAndValidateQueryFoldingDocument(b, schema, query)
	registry := NewParamRegistry()
	if err := registry.RegisterQuery(QueryParamConfig{
		DocumentBody:  query,
		OperationName: "Dependent",
		FieldParams: []FieldParamBinding{
			queryFoldingFieldBinding(
				[]string{"root", "entityAlias"}, pageTypeName, "entity", "kind",
				[]string{"root"}, "Query", "root", "kind",
			),
		},
	}); err != nil {
		b.Fatalf("register abstract dependencies failed: %v", err)
	}
	return &queryFoldingBenchmarkFixture{
		schema:    schema,
		document:  document,
		engine:    newQueryFoldingEngine(b, &schema, nil, registry),
		query:     query,
		variables: map[string]any{"kind": "user"},
	}
}

func BenchmarkSGraphQueryFoldingAbstractType(b *testing.B) {
	for _, delay := range []time.Duration{0, 200 * time.Microsecond, 5 * time.Millisecond} {
		b.Run("Delay"+delay.String(), func(b *testing.B) {
			fixture := newAbstractQueryFoldingBenchmarkFixture(b, delay)
			runQueryFoldingBenchmarkArms(b, fixture, []queryFoldingBenchmarkArm{
				{name: "SGraph/Folded", operation: "Folded", sgraph: true},
				{name: "SGraph/Dependent", operation: "Dependent", sgraph: true},
				{name: "GraphQLGo", operation: "Folded"},
			})
		})
	}
}

func newBulkQueryFoldingBenchmarkFixture(b testing.TB, groupCount int, delay time.Duration) *queryFoldingBenchmarkFixture {
	b.Helper()
	userTypeName := fmt.Sprintf("QueryFoldingBenchBulkUser%dD%d", groupCount, delay.Nanoseconds())
	groupTypeName := fmt.Sprintf("QueryFoldingBenchBulkGroup%dD%d", groupCount, delay.Nanoseconds())
	pageTypeName := fmt.Sprintf("QueryFoldingBenchBulkPage%dD%d", groupCount, delay.Nanoseconds())
	userType := NewObject(ObjectConfig{Name: userTypeName, Fields: Fields{
		"id":      &Field{Type: NewNonNull(ID)},
		"groupId": &Field{Type: NewNonNull(ID)},
	}})
	groupType := NewObject(ObjectConfig{Name: groupTypeName, Fields: Fields{
		"id":    &Field{Type: NewNonNull(ID)},
		"token": &Field{Type: String},
		"users": &Field{
			Type: NewList(userType),
			Args: FieldConfigArgument{"token": &ArgumentConfig{Type: NewNonNull(String)}},
			Resolve: func(p ResolveParams) (any, error) {
				queryFoldingBenchmarkDelay(delay)
				group := p.Source.(map[string]any)
				groupID := fmt.Sprint(group["id"])
				return []map[string]any{{"id": "user-" + groupID, "groupId": groupID}}, nil
			},
		},
	}})
	usersDefinition := groupType.Fields()["users"]
	usersDefinition.BulkResultMappedFieldName = "groupId"
	usersDefinition.BulkResolve = func(ResolveParams) (any, error) {
		queryFoldingBenchmarkDelay(delay)
		users := make([]map[string]any, groupCount)
		for index := range users {
			groupID := fmt.Sprintf("group-%d", index)
			users[index] = map[string]any{"id": "user-" + groupID, "groupId": groupID}
		}
		return users, nil
	}
	pageType := NewObject(ObjectConfig{Name: pageTypeName, Fields: Fields{
		"groups": &Field{
			Type: NewList(groupType),
			Args: FieldConfigArgument{"token": &ArgumentConfig{Type: NewNonNull(String)}},
			Resolve: func(p ResolveParams) (any, error) {
				queryFoldingBenchmarkDelay(delay)
				groups := make([]map[string]any, groupCount)
				for index := range groups {
					groups[index] = map[string]any{"id": fmt.Sprintf("group-%d", index), "token": p.Args["token"]}
				}
				return groups, nil
			},
		},
	}})
	queryType := NewObject(ObjectConfig{Name: "Query", Fields: Fields{
		"root": &Field{
			Type: pageType,
			Args: FieldConfigArgument{"token": &ArgumentConfig{Type: NewNonNull(String)}},
			Resolve: func(p ResolveParams) (any, error) {
				queryFoldingBenchmarkDelay(delay)
				return map[string]any{"token": p.Args["token"]}, nil
			},
		},
	}})
	schema, err := NewSchema(SchemaConfig{Query: queryType, Types: []Type{pageType, groupType, userType}})
	if err != nil {
		b.Fatalf("create bulk query folding schema failed: %v", err)
	}
	selection := `groups(token: $token) { id token users(token: $token) { id groupId } }`
	query := fmt.Sprintf(
		"query Folded($token: String!) { root(token: $token) { %s } }\nquery Dependent($token: String!) { root(token: $token) { %s } }",
		selection,
		selection,
	)
	document := parseAndValidateQueryFoldingDocument(b, schema, query)
	registry := NewParamRegistry()
	if err := registry.RegisterQuery(QueryParamConfig{
		DocumentBody:  query,
		OperationName: "Dependent",
		FieldParams: []FieldParamBinding{
			queryFoldingFieldBinding(
				[]string{"root", "groups"}, pageTypeName, "groups", "token",
				[]string{"root"}, "Query", "root", "token",
			),
			queryFoldingFieldBinding(
				[]string{"root", "groups", "users"}, groupTypeName, "users", "token",
				[]string{"root", "groups"}, pageTypeName, "groups", "token",
			),
		},
	}); err != nil {
		b.Fatalf("register bulk dependencies failed: %v", err)
	}
	return &queryFoldingBenchmarkFixture{
		schema:    schema,
		document:  document,
		engine:    newQueryFoldingEngine(b, &schema, nil, registry),
		query:     query,
		variables: map[string]any{"token": "benchmark-token"},
	}
}

func BenchmarkSGraphQueryFoldingBulkResolver(b *testing.B) {
	for _, groupCount := range []int{8, 64} {
		for _, delay := range []time.Duration{0, 200 * time.Microsecond} {
			name := fmt.Sprintf("Groups%d/Delay%s", groupCount, delay)
			b.Run(name, func(b *testing.B) {
				fixture := newBulkQueryFoldingBenchmarkFixture(b, groupCount, delay)
				runQueryFoldingBenchmarkArms(b, fixture, []queryFoldingBenchmarkArm{
					{name: "SGraph/Folded", operation: "Folded", sgraph: true},
					{name: "SGraph/Dependent", operation: "Dependent", sgraph: true},
					{name: "GraphQLGo", operation: "Folded"},
				})
			})
		}
	}
}

func BenchmarkSGraphQueryFoldingPayload(b *testing.B) {
	for _, payloadBytes := range []int{16, 1024, 16 * 1024} {
		name := fmt.Sprintf("Payload%dB", payloadBytes)
		b.Run(name, func(b *testing.B) {
			fixture := newWideQueryFoldingBenchmarkFixture(b, 16, 0, payloadBytes)
			runQueryFoldingBenchmarkArms(b, fixture, []queryFoldingBenchmarkArm{
				{name: "SGraph/Folded", operation: "Folded", sgraph: true},
				{name: "SGraph/Dependent", operation: "Dependent", sgraph: true},
				{name: "GraphQLGo", operation: "Folded"},
			})
		})
	}
}

func BenchmarkSGraphQueryFoldingConcurrentRequests(b *testing.B) {
	fixture := newWideQueryFoldingBenchmarkFixture(b, 32, 200*time.Microsecond, 16)
	nativeParams := ExecuteParams{
		Schema:        fixture.schema,
		AST:           fixture.document,
		OperationName: "Folded",
		Args:          fixture.variables,
		Context:       context.Background(),
	}
	for _, arm := range []queryFoldingBenchmarkArm{
		{name: "SGraph/Folded", operation: "Folded", sgraph: true},
		{name: "SGraph/Dependent", operation: "Dependent", sgraph: true},
		{name: "GraphQLGo", operation: "Folded"},
	} {
		arm := arm
		b.Run(arm.name, func(b *testing.B) {
			operationName := arm.operation
			var execute func() *Result
			if arm.sgraph {
				execute = func() *Result {
					return fixture.engine.Execute(fixture.document, fixture.variables, &operationName, nil, context.Background()).toGraphQLResult()
				}
			} else {
				execute = func() *Result { return ExecuteGraphQLGo(nativeParams) }
			}
			queryFoldingBenchmarkRequireSuccess(b, arm.name+" preflight", execute())
			b.ReportAllocs()
			b.ResetTimer()
			wallStart, cpuStart, cpuOK := nativeBenchmarkStartTiming()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					benchmarkQueryFoldingResultSink.Store(execute())
				}
			})
			b.StopTimer()
			nativeBenchmarkReportTiming(b, wallStart, cpuStart, cpuOK)
		})
	}
}
