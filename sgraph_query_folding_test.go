package graphql

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/graphql-go/graphql/gqlerrors"
	"github.com/graphql-go/graphql/language/ast"
	"github.com/graphql-go/graphql/language/parser"
	"github.com/graphql-go/graphql/language/source"
)

func parseAndValidateQueryFoldingDocument(t testing.TB, schema Schema, body string) *ast.Document {
	t.Helper()
	document, err := parser.Parse(parser.ParseParams{Source: source.NewSource(&source.Source{
		Body: []byte(body),
		Name: "SGraph query folding test",
	})})
	if err != nil {
		t.Fatalf("parse query folding document failed: %v", err)
	}
	validation := ValidateDocument(&schema, document, nil)
	if !validation.IsValid {
		t.Fatalf("validate query folding document failed: %#v", validation.Errors)
	}
	return document
}

func newQueryFoldingEngine(t testing.TB, schema *Schema, directives *DirectiveRegistry, params *ParamRegistry) *SGraphEngine {
	t.Helper()
	engine, err := NewSGraphEngine(schema, directives, params)
	if err != nil {
		t.Fatalf("create SGraph query folding engine failed: %v", err)
	}
	return engine
}

func executeQueryFoldingOperation(t testing.TB, engine *SGraphEngine, document *ast.Document, operationName string, variables map[string]any, ctx context.Context) *Result {
	t.Helper()
	result := engine.Execute(document, variables, &operationName, nil, ctx).toGraphQLResult()
	if result == nil {
		t.Fatal("SGraph query folding execution returned nil")
	}
	return result
}

func requireQueryFoldingData(t testing.TB, result *Result, expected any) {
	t.Helper()
	if result == nil {
		t.Fatal("GraphQL result is nil")
	}
	if len(result.Errors) != 0 {
		t.Fatalf("unexpected GraphQL errors: %#v", result.Errors)
	}
	actual := toPlainValue(result.Data)
	if !reflect.DeepEqual(actual, expected) {
		t.Fatalf("GraphQL data mismatch\nactual:   %#v\nexpected: %#v", actual, expected)
	}
}

func queryFoldingFieldBinding(targetPath []string, targetParentType, targetField, paramName string, sourcePath []string, sourceParentType, sourceField string, resultPath ...string) FieldParamBinding {
	return FieldParamBinding{
		Target: FieldParamTarget{
			ResponsePath:   targetPath,
			ParentTypeName: targetParentType,
			FieldName:      targetField,
			ParamName:      paramName,
		},
		Source: ParamSource{
			Kind: ParamSourceFieldResponse,
			FieldResponse: &FieldResponseParamSource{
				ResponsePath:   sourcePath,
				ParentTypeName: sourceParentType,
				FieldName:      sourceField,
				ResultPath:     resultPath,
			},
		},
	}
}

func queryFoldingStepPath(step Step) string {
	var fieldPlan *FieldPlan
	switch typed := step.(type) {
	case *SingleCallStep:
		fieldPlan = typed.fieldPlan
	case *IterationCallStep:
		fieldPlan = typed.fieldPlan
	}
	if fieldPlan == nil {
		return "<nil>"
	}
	return strings.Join(fieldPlan.paths, ".")
}

func queryFoldingBatchPaths(batches []*BatchPlan) [][]string {
	result := make([][]string, len(batches))
	for batchIndex, batch := range batches {
		if batch == nil {
			result[batchIndex] = []string{"<nil-batch>"}
			continue
		}
		result[batchIndex] = make([]string, len(batch.steps))
		for stepIndex, step := range batch.steps {
			result[batchIndex][stepIndex] = queryFoldingStepPath(step)
		}
	}
	return result
}

func queryFoldingResponsePath(path *ResponsePath) string {
	if path == nil {
		return ""
	}
	parts := path.AsArray()
	result := make([]string, len(parts))
	for index, part := range parts {
		result[index] = fmt.Sprint(part)
	}
	return strings.Join(result, ".")
}

func newQueryFoldingChainSchema(t testing.TB, resolver func(path string, p ResolveParams) (any, error)) Schema {
	t.Helper()
	args := FieldConfigArgument{"id": &ArgumentConfig{Type: NewNonNull(ID)}}
	var nodeType *Object
	nodeType = NewObject(ObjectConfig{
		Name: "QueryFoldingNode",
		Fields: FieldsThunk(func() Fields {
			return Fields{
				"id": &Field{Type: NewNonNull(ID)},
				"next": &Field{
					Type: nodeType,
					Args: args,
					Resolve: func(p ResolveParams) (any, error) {
						return resolver(queryFoldingResponsePath(p.Info.Path), p)
					},
				},
				"value": &Field{
					Type: String,
					Args: args,
					Resolve: func(p ResolveParams) (any, error) {
						return resolver(queryFoldingResponsePath(p.Info.Path), p)
					},
				},
			}
		}),
	})
	queryType := NewObject(ObjectConfig{
		Name: "Query",
		Fields: Fields{
			"root": &Field{
				Type: nodeType,
				Args: args,
				Resolve: func(p ResolveParams) (any, error) {
					return resolver("root", p)
				},
			},
		},
	})
	schema, err := NewSchema(SchemaConfig{Query: queryType, Types: []Type{nodeType}})
	if err != nil {
		t.Fatalf("create query folding chain schema failed: %v", err)
	}
	return schema
}

const queryFoldingChainDocument = `
query Folded($id: ID!) {
  root(id: $id) { next(id: $id) { next(id: $id) { value(id: $id) } } }
}
query Dependent($id: ID!) {
  root(id: $id) { next(id: $id) { next(id: $id) { value(id: $id) } } }
}
query Mixed($id: ID!) {
  root(id: $id) { next(id: $id) { next(id: $id) { value(id: $id) } } }
}`

func registerQueryFoldingChainDependencies(t testing.TB, registry *ParamRegistry, operationName string, mixed bool) {
	t.Helper()
	bindings := []FieldParamBinding{
		queryFoldingFieldBinding(
			[]string{"root", "next", "next", "value"}, "QueryFoldingNode", "value", "id",
			[]string{"root", "next", "next"}, "QueryFoldingNode", "next", "id",
		),
	}
	if !mixed {
		bindings = append([]FieldParamBinding{
			queryFoldingFieldBinding(
				[]string{"root", "next"}, "QueryFoldingNode", "next", "id",
				[]string{"root"}, "Query", "root", "id",
			),
			queryFoldingFieldBinding(
				[]string{"root", "next", "next"}, "QueryFoldingNode", "next", "id",
				[]string{"root", "next"}, "QueryFoldingNode", "next", "id",
			),
		}, bindings...)
	}
	if err := registry.RegisterQuery(QueryParamConfig{
		DocumentBody:  queryFoldingChainDocument,
		OperationName: operationName,
		FieldParams:   bindings,
	}); err != nil {
		t.Fatalf("register %s query folding dependencies failed: %v", operationName, err)
	}
}

func TestSGraphQueryFoldingBatchTopology(t *testing.T) {
	schema := newQueryFoldingChainSchema(t, func(path string, p ResolveParams) (any, error) {
		id := fmt.Sprint(p.Args["id"])
		if strings.HasSuffix(path, "value") {
			return id, nil
		}
		return map[string]any{"id": id}, nil
	})
	document := parseAndValidateQueryFoldingDocument(t, schema, queryFoldingChainDocument)
	registry := NewParamRegistry()
	registerQueryFoldingChainDependencies(t, registry, "Dependent", false)
	registerQueryFoldingChainDependencies(t, registry, "Mixed", true)

	tests := []struct {
		operation string
		expected  [][]string
	}{
		{
			operation: "Folded",
			expected: [][]string{{
				"root", "root.next", "root.next.next", "root.next.next.value",
			}},
		},
		{
			operation: "Dependent",
			expected: [][]string{
				{"root"},
				{"root.next"},
				{"root.next.next"},
				{"root.next.next.value"},
			},
		},
		{
			operation: "Mixed",
			expected: [][]string{
				{"root", "root.next", "root.next.next"},
				{"root.next.next.value"},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.operation, func(t *testing.T) {
			operationName := tc.operation
			plan, err := compileExecutionPlan(document, &schema, &operationName, NewDirectiveRegistry(), registry)
			if err != nil {
				t.Fatalf("compile %s plan failed: %v", tc.operation, err)
			}
			batches, err := coordinateBatches(plan)
			if err != nil {
				t.Fatalf("coordinate %s batches failed: %v", tc.operation, err)
			}
			actual := queryFoldingBatchPaths(batches)
			if !reflect.DeepEqual(actual, tc.expected) {
				t.Fatalf("%s batch topology mismatch\nactual:   %#v\nexpected: %#v", tc.operation, actual, tc.expected)
			}
			for _, batch := range batches {
				if !batch.concurrent {
					t.Fatalf("query batch %d is not concurrent", batch.batchId)
				}
			}
		})
	}
}

func TestSGraphQueryFoldingExecutesIndependentParentAndChildConcurrently(t *testing.T) {
	parentStarted := make(chan struct{})
	childStarted := make(chan struct{})
	releaseParent := make(chan struct{})
	var parentOnce sync.Once
	var childOnce sync.Once

	schema := newQueryFoldingChainSchema(t, func(path string, p ResolveParams) (any, error) {
		id := fmt.Sprint(p.Args["id"])
		switch path {
		case "root":
			parentOnce.Do(func() { close(parentStarted) })
			<-releaseParent
			return map[string]any{"id": id}, nil
		case "root.value":
			childOnce.Do(func() { close(childStarted) })
			return id, nil
		default:
			return map[string]any{"id": id}, nil
		}
	})
	query := `query Folded($id: ID!) { root(id: $id) { value(id: $id) } }`
	document := parseAndValidateQueryFoldingDocument(t, schema, query)
	engine := newQueryFoldingEngine(t, &schema, nil, nil)
	resultChannel := make(chan *Result, 1)
	operationName := "Folded"
	go func() {
		resultChannel <- engine.Execute(document, map[string]any{"id": "concurrent"}, &operationName, nil, context.Background()).toGraphQLResult()
	}()

	select {
	case <-parentStarted:
	case <-time.After(time.Second):
		close(releaseParent)
		t.Fatal("parent resolver did not start")
	}
	select {
	case <-childStarted:
		// 子resolver在父resolver返回前启动，直接证明同批并发，不依赖耗时阈值推断。
	case <-time.After(time.Second):
		close(releaseParent)
		<-resultChannel
		t.Fatal("independent child resolver did not start while parent resolver was blocked")
	}
	close(releaseParent)
	result := <-resultChannel
	requireQueryFoldingData(t, result, map[string]any{
		"root": map[string]any{"value": "concurrent"},
	})
}

func TestSGraphQueryFoldingFieldDependencyControlsRuntimeOrderAndValue(t *testing.T) {
	var rootFinished atomic.Bool
	var childSawFinished atomic.Bool
	schema := newQueryFoldingChainSchema(t, func(path string, p ResolveParams) (any, error) {
		id := fmt.Sprint(p.Args["id"])
		switch path {
		case "root":
			rootFinished.Store(true)
			return map[string]any{"id": "from-parent"}, nil
		case "root.value":
			childSawFinished.Store(rootFinished.Load())
			return id, nil
		default:
			return map[string]any{"id": id}, nil
		}
	})
	query := `query Dependent($id: ID!) { root(id: $id) { value(id: $id) } }`
	document := parseAndValidateQueryFoldingDocument(t, schema, query)
	registry := NewParamRegistry()
	if err := registry.RegisterQuery(QueryParamConfig{
		DocumentBody:  query,
		OperationName: "Dependent",
		FieldParams: []FieldParamBinding{
			queryFoldingFieldBinding(
				[]string{"root", "value"}, "QueryFoldingNode", "value", "id",
				[]string{"root"}, "Query", "root", "id",
			),
		},
	}); err != nil {
		t.Fatal(err)
	}
	engine := newQueryFoldingEngine(t, &schema, nil, registry)
	result := executeQueryFoldingOperation(t, engine, document, "Dependent", map[string]any{"id": "from-request"}, context.Background())
	requireQueryFoldingData(t, result, map[string]any{
		"root": map[string]any{"value": "from-parent"},
	})
	if !childSawFinished.Load() {
		t.Fatal("dependent child resolver ran before its producer completed")
	}
}

func TestSGraphQueryFoldingSelectionAndPlanCacheIsolation(t *testing.T) {
	var firstCalls atomic.Int64
	var secondCalls atomic.Int64
	var thirdCalls atomic.Int64
	pageType := NewObject(ObjectConfig{
		Name: "QueryFoldingSelectionPage",
		Fields: Fields{
			"first": &Field{Type: String, Resolve: func(ResolveParams) (any, error) {
				firstCalls.Add(1)
				return "A", nil
			}},
			"second": &Field{Type: String, Resolve: func(ResolveParams) (any, error) {
				secondCalls.Add(1)
				return "B", nil
			}},
			"third": &Field{Type: String, Resolve: func(ResolveParams) (any, error) {
				thirdCalls.Add(1)
				return "C", nil
			}},
		},
	})
	queryType := NewObject(ObjectConfig{
		Name: "Query",
		Fields: Fields{
			"page": &Field{Type: pageType, Resolve: func(ResolveParams) (any, error) {
				return map[string]any{"id": "page"}, nil
			}},
		},
	})
	schema, err := NewSchema(SchemaConfig{Query: queryType, Types: []Type{pageType}})
	if err != nil {
		t.Fatal(err)
	}
	query := `
query Select($includeSecond: Boolean!, $skipThird: Boolean!) {
  page {
    alpha: first
    ...SelectionFields
    ... on QueryFoldingSelectionPage { gamma: third @skip(if: $skipThird) }
    type: __typename
  }
  schemaType: __type(name: "QueryFoldingSelectionPage") { name }
}
fragment SelectionFields on QueryFoldingSelectionPage {
  alpha: first
  beta: second @include(if: $includeSecond)
}`
	document := parseAndValidateQueryFoldingDocument(t, schema, query)
	engine := newQueryFoldingEngine(t, &schema, nil, nil)

	first := executeQueryFoldingOperation(t, engine, document, "Select", map[string]any{
		"includeSecond": true,
		"skipThird":     false,
	}, context.Background())
	requireQueryFoldingData(t, first, map[string]any{
		"page": map[string]any{
			"alpha": "A", "beta": "B", "gamma": "C", "type": "QueryFoldingSelectionPage",
		},
		"schemaType": map[string]any{"name": "QueryFoldingSelectionPage"},
	})
	encoded, err := json.Marshal(first.Data)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(encoded), `{"page":{"alpha":"A","beta":"B","gamma":"C","type":"QueryFoldingSelectionPage"},"schemaType":{"name":"QueryFoldingSelectionPage"}}`; got != want {
		t.Fatalf("response field order mismatch\ngot:  %s\nwant: %s", got, want)
	}

	second := executeQueryFoldingOperation(t, engine, document, "Select", map[string]any{
		"includeSecond": false,
		"skipThird":     true,
	}, context.Background())
	requireQueryFoldingData(t, second, map[string]any{
		"page":       map[string]any{"alpha": "A", "type": "QueryFoldingSelectionPage"},
		"schemaType": map[string]any{"name": "QueryFoldingSelectionPage"},
	})
	if firstCalls.Load() != 2 || secondCalls.Load() != 1 || thirdCalls.Load() != 1 {
		t.Fatalf("directive execution counts mismatch: first=%d second=%d third=%d", firstCalls.Load(), secondCalls.Load(), thirdCalls.Load())
	}
}

type queryFoldingGateCompiler struct{}

func (queryFoldingGateCompiler) Compile(name, location string, args map[string]any, _ *Schema) (*DirectiveCompileResult, error) {
	return &DirectiveCompileResult{RuntimePlans: []*DirectivePlan{{
		name:     name,
		location: location,
		argsRaw:  args,
		stage:    DIRECTIVE_STAGE_SHOULD_EXECUTE,
	}}}, nil
}

func (queryFoldingGateCompiler) RuntimeCompile(name, location string, argPlans []*ParamPlan, _ *Schema) (*DirectiveCompileResult, error) {
	return &DirectiveCompileResult{RuntimePlans: []*DirectivePlan{{
		name:      name,
		location:  location,
		argsPlans: argPlans,
		stage:     DIRECTIVE_STAGE_SHOULD_EXECUTE,
	}}}, nil
}

type queryFoldingGateHandler struct{}

func (queryFoldingGateHandler) ShouldExecute(_ *FieldPlan, directiveArgs, _ map[string]any, _ any, _ map[string]any, _ context.Context) (bool, error) {
	allowed, ok := directiveArgs["if"].(bool)
	if !ok {
		return false, fmt.Errorf("@gate(if:) materialized %T, want bool", directiveArgs["if"])
	}
	return allowed, nil
}

func (queryFoldingGateHandler) BeforeResolve(_ *FieldPlan, _ map[string]any, params map[string]any, _ any, _ map[string]any, _ context.Context) (map[string]any, error) {
	return params, nil
}

func (queryFoldingGateHandler) AfterResolve(_ *FieldPlan, _ map[string]any, _ map[string]any, _ any, current any, _ map[string]any, _ context.Context) (any, error) {
	return current, nil
}

func TestSGraphQueryFoldingDirectiveFieldResponseDependency(t *testing.T) {
	gateDirective := NewDirective(DirectiveConfig{
		Name:      "gate",
		Locations: []string{DirectiveLocationField},
		Args: FieldConfigArgument{
			"if": &ArgumentConfig{Type: NewNonNull(Boolean)},
		},
	})
	var shownCalls atomic.Int64
	pageType := NewObject(ObjectConfig{Name: "QueryFoldingDirectivePage", Fields: Fields{
		"gate": &Field{
			Type: Boolean,
			Args: FieldConfigArgument{"value": &ArgumentConfig{Type: NewNonNull(Boolean)}},
			Resolve: func(p ResolveParams) (any, error) {
				return p.Args["value"], nil
			},
		},
		"shown": &Field{Type: String, Resolve: func(ResolveParams) (any, error) {
			shownCalls.Add(1)
			return "visible", nil
		}},
	}})
	queryType := NewObject(ObjectConfig{Name: "Query", Fields: Fields{
		"page": &Field{Type: pageType, Resolve: func(ResolveParams) (any, error) { return map[string]any{}, nil }},
	}})
	directives := append([]*Directive(nil), SpecifiedDirectives...)
	directives = append(directives, gateDirective)
	schema, err := NewSchema(SchemaConfig{Query: queryType, Types: []Type{pageType}, Directives: directives})
	if err != nil {
		t.Fatal(err)
	}
	query := `query DirectiveFold($allowed: Boolean!) { page { gate(value: $allowed) shown @gate(if: false) } }`
	document := parseAndValidateQueryFoldingDocument(t, schema, query)
	paramRegistry := NewParamRegistry()
	if err := paramRegistry.RegisterQuery(QueryParamConfig{
		DocumentBody:  query,
		OperationName: "DirectiveFold",
		DirectiveParams: []DirectiveParamBinding{{
			Target: DirectiveParamTarget{
				Location:       DirectiveLocationField,
				DirectiveName:  "gate",
				ParamName:      "if",
				ResponsePath:   []string{"page", "shown"},
				ParentTypeName: "QueryFoldingDirectivePage",
				FieldName:      "shown",
			},
			Source: ParamSource{
				Kind: ParamSourceFieldResponse,
				FieldResponse: &FieldResponseParamSource{
					ResponsePath:   []string{"page", "gate"},
					ParentTypeName: "QueryFoldingDirectivePage",
					FieldName:      "gate",
				},
			},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	directiveRegistry := NewDirectiveRegistry()
	if err := directiveRegistry.Register("gate", queryFoldingGateCompiler{}, queryFoldingGateHandler{}); err != nil {
		t.Fatal(err)
	}
	operationName := "DirectiveFold"
	plan, err := compileExecutionPlan(document, &schema, &operationName, directiveRegistry, paramRegistry)
	if err != nil {
		t.Fatalf("compile directive dependency plan failed: %v", err)
	}
	batches, err := coordinateBatches(plan)
	if err != nil {
		t.Fatalf("coordinate directive dependency batches failed: %v", err)
	}
	expectedBatches := [][]string{{"page", "page.gate"}, {"page.shown"}}
	if actual := queryFoldingBatchPaths(batches); !reflect.DeepEqual(actual, expectedBatches) {
		t.Fatalf("directive dependency topology mismatch\nactual:   %#v\nexpected: %#v", actual, expectedBatches)
	}

	engine := newQueryFoldingEngine(t, &schema, directiveRegistry, paramRegistry)
	allowed := executeQueryFoldingOperation(t, engine, document, "DirectiveFold", map[string]any{"allowed": true}, context.Background())
	requireQueryFoldingData(t, allowed, map[string]any{"page": map[string]any{"gate": true, "shown": "visible"}})
	denied := executeQueryFoldingOperation(t, engine, document, "DirectiveFold", map[string]any{"allowed": false}, context.Background())
	// 普通directive的ShouldExecute只跳过resolver调用；只有skip/include会从响应选择集中移除字段。
	requireQueryFoldingData(t, denied, map[string]any{"page": map[string]any{"gate": false, "shown": nil}})
	if shownCalls.Load() != 1 {
		t.Fatalf("@gate controlled resolver calls = %d, want 1", shownCalls.Load())
	}
}

func TestSGraphQueryFoldingVariableTemplateDoesNotLeakThroughPlanCache(t *testing.T) {
	inputType := NewInputObject(InputObjectConfig{
		Name: "QueryFoldingInput",
		Fields: InputObjectConfigFieldMap{
			"label": &InputObjectFieldConfig{Type: NewNonNull(String)},
			"count": &InputObjectFieldConfig{Type: NewNonNull(Int)},
		},
	})
	pageType := NewObject(ObjectConfig{
		Name: "QueryFoldingInputPage",
		Fields: Fields{
			"echo": &Field{
				Type: String,
				Args: FieldConfigArgument{"input": &ArgumentConfig{Type: NewNonNull(inputType)}},
				Resolve: func(p ResolveParams) (any, error) {
					input := p.Args["input"].(map[string]any)
					return fmt.Sprintf("%s:%d", input["label"], input["count"]), nil
				},
			},
		},
	})
	queryType := NewObject(ObjectConfig{Name: "Query", Fields: Fields{
		"page": &Field{Type: pageType, Resolve: func(ResolveParams) (any, error) { return map[string]any{}, nil }},
	}})
	schema, err := NewSchema(SchemaConfig{Query: queryType, Types: []Type{inputType, pageType}})
	if err != nil {
		t.Fatal(err)
	}
	query := `query Cached($label: String!, $count: Int!) { page { echo(input: {label: $label, count: $count}) } }`
	document := parseAndValidateQueryFoldingDocument(t, schema, query)
	engine := newQueryFoldingEngine(t, &schema, nil, nil)

	for _, tc := range []struct {
		label string
		count int
	}{
		{label: "first", count: 1},
		{label: "second", count: 2},
		{label: "third", count: 3},
	} {
		result := executeQueryFoldingOperation(t, engine, document, "Cached", map[string]any{
			"label": tc.label,
			"count": tc.count,
		}, context.Background())
		requireQueryFoldingData(t, result, map[string]any{
			"page": map[string]any{"echo": fmt.Sprintf("%s:%d", tc.label, tc.count)},
		})
	}
}

func TestSGraphQueryFoldingTypedListAndIterationResolver(t *testing.T) {
	itemType := NewObject(ObjectConfig{
		Name: "QueryFoldingListItem",
		Fields: Fields{
			"id": &Field{Type: NewNonNull(ID)},
			"score": &Field{
				Type: Int,
				Args: FieldConfigArgument{
					"id":         &ArgumentConfig{Type: NewNonNull(ID)},
					"multiplier": &ArgumentConfig{Type: NewNonNull(Int)},
				},
				Resolve: func(p ResolveParams) (any, error) {
					id := p.Args["id"].(string)
					if id == "item-1" {
						return 2 * p.Args["multiplier"].(int), nil
					}
					return 4 * p.Args["multiplier"].(int), nil
				},
			},
		},
	})
	pageType := NewObject(ObjectConfig{
		Name: "QueryFoldingListPage",
		Fields: Fields{
			"items": &Field{
				Type: NewList(itemType),
				Args: FieldConfigArgument{"count": &ArgumentConfig{Type: NewNonNull(Int)}},
				Resolve: func(p ResolveParams) (any, error) {
					// 使用[]map而不是[]any，验证typed slice适配，同时保留参数依赖所需的字段路径读取语义。
					return []map[string]any{{"id": "item-1"}, {"id": "item-2"}}, nil
				},
			},
		},
	})
	queryType := NewObject(ObjectConfig{Name: "Query", Fields: Fields{
		"page": &Field{Type: pageType, Resolve: func(ResolveParams) (any, error) { return map[string]any{}, nil }},
	}})
	schema, err := NewSchema(SchemaConfig{Query: queryType, Types: []Type{pageType, itemType}})
	if err != nil {
		t.Fatal(err)
	}
	query := `query List($count: Int!, $multiplier: Int!) { page { items(count: $count) { id score(id: "unused", multiplier: $multiplier) } } }`
	document := parseAndValidateQueryFoldingDocument(t, schema, query)
	registry := NewParamRegistry()
	if err := registry.RegisterQuery(QueryParamConfig{
		DocumentBody:  query,
		OperationName: "List",
		FieldParams: []FieldParamBinding{
			queryFoldingFieldBinding(
				[]string{"page", "items", "score"}, "QueryFoldingListItem", "score", "id",
				[]string{"page", "items"}, "QueryFoldingListPage", "items", "id",
			),
		},
	}); err != nil {
		t.Fatal(err)
	}
	engine := newQueryFoldingEngine(t, &schema, nil, registry)
	result := executeQueryFoldingOperation(t, engine, document, "List", map[string]any{"count": 2, "multiplier": 3}, context.Background())
	requireQueryFoldingData(t, result, map[string]any{
		"page": map[string]any{
			"items": []any{
				map[string]any{"id": "item-1", "score": 6},
				map[string]any{"id": "item-2", "score": 12},
			},
		},
	})
}

func TestSGraphQueryFoldingBulkResolverKeepsParentMapping(t *testing.T) {
	userType := NewObject(ObjectConfig{Name: "QueryFoldingBulkUser", Fields: Fields{
		"id":      &Field{Type: NewNonNull(Int)},
		"groupId": &Field{Type: NewNonNull(ID)},
		"name":    &Field{Type: String},
	}})
	groupType := NewObject(ObjectConfig{Name: "QueryFoldingBulkGroup", Fields: Fields{
		"id": &Field{Type: NewNonNull(ID)},
		"users": &Field{
			Type: NewList(userType),
			Resolve: func(p ResolveParams) (any, error) {
				return []map[string]any{}, nil
			},
		},
	}})
	usersDefinition := groupType.Fields()["users"]
	usersDefinition.BulkResolve = func(ResolveParams) (any, error) {
		return []map[string]any{
			{"id": 20, "groupId": "g2", "name": "B"},
			{"id": 10, "groupId": "g1", "name": "A"},
			{"id": 21, "groupId": "g2", "name": "C"},
		}, nil
	}
	usersDefinition.BulkResultMappedFieldName = "groupId"
	pageType := NewObject(ObjectConfig{Name: "QueryFoldingBulkPage", Fields: Fields{
		"groups": &Field{Type: NewList(groupType), Resolve: func(ResolveParams) (any, error) {
			return []map[string]any{{"id": "g1"}, {"id": "g2"}}, nil
		}},
	}})
	queryType := NewObject(ObjectConfig{Name: "Query", Fields: Fields{
		"page": &Field{Type: pageType, Resolve: func(ResolveParams) (any, error) { return map[string]any{}, nil }},
	}})
	schema, err := NewSchema(SchemaConfig{Query: queryType, Types: []Type{pageType, groupType, userType}})
	if err != nil {
		t.Fatal(err)
	}
	query := `query Bulk { page { groups { id users { id groupId name } } } }`
	document := parseAndValidateQueryFoldingDocument(t, schema, query)
	engine := newQueryFoldingEngine(t, &schema, nil, nil)
	result := executeQueryFoldingOperation(t, engine, document, "Bulk", nil, context.Background())
	requireQueryFoldingData(t, result, map[string]any{
		"page": map[string]any{
			"groups": []any{
				map[string]any{"id": "g1", "users": []any{map[string]any{"id": 10, "groupId": "g1", "name": "A"}}},
				map[string]any{"id": "g2", "users": []any{
					map[string]any{"id": 20, "groupId": "g2", "name": "B"},
					map[string]any{"id": 21, "groupId": "g2", "name": "C"},
				}},
			},
		},
	})
}

func TestSGraphQueryFoldingAbstractRuntimeTypeAndFragments(t *testing.T) {
	var userType *Object
	var robotType *Object
	entityType := NewInterface(InterfaceConfig{
		Name:   "QueryFoldingEntity",
		Fields: Fields{"id": &Field{Type: NewNonNull(ID)}},
		ResolveType: func(p ResolveTypeParams) *Object {
			value := p.Value.(map[string]any)
			if value["kind"] == "user" {
				return userType
			}
			return robotType
		},
	})
	userType = NewObject(ObjectConfig{Name: "QueryFoldingUser", Interfaces: []*Interface{entityType}, Fields: Fields{
		"id":   &Field{Type: NewNonNull(ID)},
		"name": &Field{Type: String},
	}})
	robotType = NewObject(ObjectConfig{Name: "QueryFoldingRobot", Interfaces: []*Interface{entityType}, Fields: Fields{
		"id":     &Field{Type: NewNonNull(ID)},
		"serial": &Field{Type: String},
	}})
	pageType := NewObject(ObjectConfig{Name: "QueryFoldingAbstractPage", Fields: Fields{
		"entity": &Field{
			Type: entityType,
			Args: FieldConfigArgument{"kind": &ArgumentConfig{Type: NewNonNull(String)}},
			Resolve: func(p ResolveParams) (any, error) {
				if p.Args["kind"] == "user" {
					return map[string]any{"kind": "user", "id": "u1", "name": "Ada"}, nil
				}
				return map[string]any{"kind": "robot", "id": "r1", "serial": "RX"}, nil
			},
		},
	}})
	queryType := NewObject(ObjectConfig{Name: "Query", Fields: Fields{
		"page": &Field{Type: pageType, Resolve: func(ResolveParams) (any, error) { return map[string]any{}, nil }},
	}})
	schema, err := NewSchema(SchemaConfig{Query: queryType, Types: []Type{pageType, entityType, userType, robotType}})
	if err != nil {
		t.Fatal(err)
	}
	query := `query Abstract($kind: String!) { page { entity(kind: $kind) { __typename id ... on QueryFoldingUser { name } ... on QueryFoldingRobot { serial } } } }`
	document := parseAndValidateQueryFoldingDocument(t, schema, query)
	engine := newQueryFoldingEngine(t, &schema, nil, nil)

	userResult := executeQueryFoldingOperation(t, engine, document, "Abstract", map[string]any{"kind": "user"}, context.Background())
	requireQueryFoldingData(t, userResult, map[string]any{
		"page": map[string]any{"entity": map[string]any{"__typename": "QueryFoldingUser", "id": "u1", "name": "Ada"}},
	})
	robotResult := executeQueryFoldingOperation(t, engine, document, "Abstract", map[string]any{"kind": "robot"}, context.Background())
	requireQueryFoldingData(t, robotResult, map[string]any{
		"page": map[string]any{"entity": map[string]any{"__typename": "QueryFoldingRobot", "id": "r1", "serial": "RX"}},
	})
}

func TestSGraphQueryFoldingNullBubblingAndConcurrentErrors(t *testing.T) {
	t.Run("non-null child bubbles while sibling still executes", func(t *testing.T) {
		var siblingCalls atomic.Int64
		containerType := NewObject(ObjectConfig{Name: "QueryFoldingErrorContainer", Fields: Fields{
			"required": &Field{Type: NewNonNull(String), Resolve: func(ResolveParams) (any, error) {
				return nil, errors.New("required failed")
			}},
			"sibling": &Field{Type: String, Resolve: func(ResolveParams) (any, error) {
				siblingCalls.Add(1)
				return "ok", nil
			}},
		}})
		queryType := NewObject(ObjectConfig{Name: "Query", Fields: Fields{
			"container": &Field{Type: containerType, Resolve: func(ResolveParams) (any, error) { return map[string]any{}, nil }},
		}})
		schema, err := NewSchema(SchemaConfig{Query: queryType, Types: []Type{containerType}})
		if err != nil {
			t.Fatal(err)
		}
		query := `query Errors { container { required sibling } }`
		document := parseAndValidateQueryFoldingDocument(t, schema, query)
		engine := newQueryFoldingEngine(t, &schema, nil, nil)
		result := executeQueryFoldingOperation(t, engine, document, "Errors", nil, context.Background())
		if got := toPlainValue(result.Data); !reflect.DeepEqual(got, map[string]any{"container": nil}) {
			t.Fatalf("null bubbling data mismatch: %#v", got)
		}
		if len(result.Errors) != 1 || !reflect.DeepEqual(result.Errors[0].Path, []any{"container", "required"}) {
			t.Fatalf("null bubbling errors mismatch: %#v", result.Errors)
		}
		if siblingCalls.Load() != 1 {
			t.Fatalf("independent sibling resolver calls = %d, want 1", siblingCalls.Load())
		}
	})

	t.Run("all concurrent root errors are retained", func(t *testing.T) {
		fields := make(Fields, 16)
		var query strings.Builder
		query.WriteString("query Errors {")
		for index := 0; index < 16; index++ {
			fieldName := fmt.Sprintf("f%02d", index)
			message := "failure " + fieldName
			fields[fieldName] = &Field{Type: String, Resolve: func(ResolveParams) (any, error) {
				return nil, errors.New(message)
			}}
			query.WriteByte(' ')
			query.WriteString(fieldName)
		}
		query.WriteString(" }")
		queryType := NewObject(ObjectConfig{Name: "Query", Fields: fields})
		schema, err := NewSchema(SchemaConfig{Query: queryType})
		if err != nil {
			t.Fatal(err)
		}
		document := parseAndValidateQueryFoldingDocument(t, schema, query.String())
		engine := newQueryFoldingEngine(t, &schema, nil, nil)
		result := executeQueryFoldingOperation(t, engine, document, "Errors", nil, context.Background())
		if len(result.Errors) != 16 {
			t.Fatalf("concurrent error count = %d, want 16: %#v", len(result.Errors), result.Errors)
		}
		paths := make(map[string]bool, 16)
		for _, fieldErr := range result.Errors {
			if len(fieldErr.Path) == 1 {
				paths[fmt.Sprint(fieldErr.Path[0])] = true
			}
		}
		for index := 0; index < 16; index++ {
			fieldName := fmt.Sprintf("f%02d", index)
			if !paths[fieldName] {
				t.Fatalf("missing error path %s in %#v", fieldName, result.Errors)
			}
		}
	})

	t.Run("resolver panic becomes a field error", func(t *testing.T) {
		queryType := NewObject(ObjectConfig{Name: "Query", Fields: Fields{
			"panicField": &Field{Type: String, Resolve: func(ResolveParams) (any, error) { panic("fold panic") }},
		}})
		schema, err := NewSchema(SchemaConfig{Query: queryType})
		if err != nil {
			t.Fatal(err)
		}
		query := `query Panic { panicField }`
		document := parseAndValidateQueryFoldingDocument(t, schema, query)
		engine := newQueryFoldingEngine(t, &schema, nil, nil)
		result := executeQueryFoldingOperation(t, engine, document, "Panic", nil, context.Background())
		if len(result.Errors) != 1 || !reflect.DeepEqual(result.Errors[0].Path, []any{"panicField"}) {
			t.Fatalf("panic result errors mismatch: %#v", result.Errors)
		}
	})
}

type queryFoldingContextKey struct{}

type queryFoldingExtension struct {
	executionStarts   atomic.Int64
	executionFinishes atomic.Int64
	fieldStarts       atomic.Int64
	fieldFinishes     atomic.Int64
}

func (e *queryFoldingExtension) Init(ctx context.Context, _ *Params) context.Context { return ctx }
func (e *queryFoldingExtension) Name() string                                        { return "queryFolding" }
func (e *queryFoldingExtension) ParseDidStart(ctx context.Context) (context.Context, ParseFinishFunc) {
	return ctx, func(error) {}
}
func (e *queryFoldingExtension) ValidationDidStart(ctx context.Context) (context.Context, ValidationFinishFunc) {
	return ctx, func([]gqlerrors.FormattedError) {}
}
func (e *queryFoldingExtension) ExecutionDidStart(ctx context.Context) (context.Context, ExecutionFinishFunc) {
	e.executionStarts.Add(1)
	return ctx, func(*Result) { e.executionFinishes.Add(1) }
}
func (e *queryFoldingExtension) ResolveFieldDidStart(ctx context.Context, _ *ResolveInfo) (context.Context, ResolveFieldFinishFunc) {
	e.fieldStarts.Add(1)
	return ctx, func(any, error) { e.fieldFinishes.Add(1) }
}
func (e *queryFoldingExtension) HasResult() bool { return true }
func (e *queryFoldingExtension) GetResult(context.Context) any {
	return map[string]int64{
		"executionStarts":   e.executionStarts.Load(),
		"executionFinishes": e.executionFinishes.Load(),
		"fieldStarts":       e.fieldStarts.Load(),
		"fieldFinishes":     e.fieldFinishes.Load(),
	}
}

func TestSGraphQueryFoldingContextExtensionsAndConcurrentRequests(t *testing.T) {
	extension := &queryFoldingExtension{}
	pageType := NewObject(ObjectConfig{Name: "QueryFoldingContextPage", Fields: Fields{
		"echo": &Field{
			Type: String,
			Args: FieldConfigArgument{"value": &ArgumentConfig{Type: NewNonNull(String)}},
			Resolve: func(p ResolveParams) (any, error) {
				if p.Context.Value(queryFoldingContextKey{}) != "context-value" {
					return nil, errors.New("request context missing in child resolver")
				}
				return p.Args["value"], nil
			},
		},
	}})
	queryType := NewObject(ObjectConfig{Name: "Query", Fields: Fields{
		"page": &Field{Type: pageType, Resolve: func(p ResolveParams) (any, error) {
			if p.Context.Value(queryFoldingContextKey{}) != "context-value" {
				return nil, errors.New("request context missing in parent resolver")
			}
			return map[string]any{}, nil
		}},
	}})
	schema, err := NewSchema(SchemaConfig{Query: queryType, Types: []Type{pageType}, Extensions: []Extension{extension}})
	if err != nil {
		t.Fatal(err)
	}
	query := `query Context($value: String!) { page { echo(value: $value) } }`
	document := parseAndValidateQueryFoldingDocument(t, schema, query)
	engine := newQueryFoldingEngine(t, &schema, nil, nil)
	ctx := context.WithValue(context.Background(), queryFoldingContextKey{}, "context-value")
	if err := RegisterSGraphEngine(engine); err != nil {
		t.Fatalf("register context query folding engine failed: %v", err)
	}
	result := Execute(ExecuteParams{
		Schema:        schema,
		AST:           document,
		OperationName: "Context",
		Args:          map[string]any{"value": "one"},
		Context:       ctx,
	})
	requireQueryFoldingData(t, result, map[string]any{"page": map[string]any{"echo": "one"}})
	if extension.executionStarts.Load() != 1 || extension.executionFinishes.Load() != 1 {
		t.Fatalf("execution extension counts mismatch: starts=%d finishes=%d", extension.executionStarts.Load(), extension.executionFinishes.Load())
	}
	if extension.fieldStarts.Load() != 2 || extension.fieldFinishes.Load() != 2 {
		t.Fatalf("field extension counts mismatch: starts=%d finishes=%d", extension.fieldStarts.Load(), extension.fieldFinishes.Load())
	}
	if result.Extensions == nil || result.Extensions[extension.Name()] == nil {
		t.Fatalf("extension result missing: %#v", result.Extensions)
	}

	const requestCount = 32
	var waitGroup sync.WaitGroup
	waitGroup.Add(requestCount)
	errorsChannel := make(chan error, requestCount)
	for index := 0; index < requestCount; index++ {
		go func(index int) {
			defer waitGroup.Done()
			value := fmt.Sprintf("request-%d", index)
			current := engine.Execute(document, map[string]any{"value": value}, stringPointer("Context"), nil, ctx).toGraphQLResult()
			if len(current.Errors) != 0 {
				errorsChannel <- fmt.Errorf("request %d errors: %#v", index, current.Errors)
				return
			}
			expected := map[string]any{"page": map[string]any{"echo": value}}
			if actual := toPlainValue(current.Data); !reflect.DeepEqual(actual, expected) {
				errorsChannel <- fmt.Errorf("request %d data = %#v, want %#v", index, actual, expected)
			}
		}(index)
	}
	waitGroup.Wait()
	close(errorsChannel)
	for requestErr := range errorsChannel {
		t.Error(requestErr)
	}
}

func stringPointer(value string) *string { return &value }

func TestSGraphQueryFoldingRejectsInvalidDependencyGraphs(t *testing.T) {
	schema := newQueryFoldingChainSchema(t, func(path string, p ResolveParams) (any, error) {
		id := fmt.Sprint(p.Args["id"])
		if strings.HasSuffix(path, "value") {
			return id, nil
		}
		return map[string]any{"id": id}, nil
	})

	tests := []struct {
		name     string
		query    string
		bindings []FieldParamBinding
		contains string
	}{
		{
			name:  "self dependency",
			query: `query Invalid($id: ID!) { root(id: $id) { value(id: $id) } }`,
			bindings: []FieldParamBinding{
				queryFoldingFieldBinding([]string{"root", "value"}, "QueryFoldingNode", "value", "id", []string{"root", "value"}, "QueryFoldingNode", "value"),
			},
			contains: "cannot depend on itself",
		},
		{
			name:  "cycle",
			query: `query Invalid($id: ID!) { root(id: $id) { a: value(id: $id) b: value(id: $id) } }`,
			bindings: []FieldParamBinding{
				queryFoldingFieldBinding([]string{"root", "a"}, "QueryFoldingNode", "value", "id", []string{"root", "b"}, "QueryFoldingNode", "value"),
				queryFoldingFieldBinding([]string{"root", "b"}, "QueryFoldingNode", "value", "id", []string{"root", "a"}, "QueryFoldingNode", "value"),
			},
			contains: "dependency cycle",
		},
		{
			name:  "unknown source",
			query: `query Invalid($id: ID!) { root(id: $id) { value(id: $id) } }`,
			bindings: []FieldParamBinding{
				queryFoldingFieldBinding([]string{"root", "value"}, "QueryFoldingNode", "value", "id", []string{"root", "missing"}, "QueryFoldingNode", "value"),
			},
			contains: "unknown FIELD_RESPONSE source",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			document := parseAndValidateQueryFoldingDocument(t, schema, tc.query)
			registry := NewParamRegistry()
			if err := registry.RegisterQuery(QueryParamConfig{
				DocumentBody:  tc.query,
				OperationName: "Invalid",
				FieldParams:   tc.bindings,
			}); err != nil {
				t.Fatalf("register invalid graph fixture failed before plan compilation: %v", err)
			}
			operationName := "Invalid"
			plan, compileErr := compileExecutionPlan(document, &schema, &operationName, NewDirectiveRegistry(), registry)
			if compileErr == nil {
				_, compileErr = coordinateBatches(plan)
			}
			if compileErr == nil || !strings.Contains(compileErr.Error(), tc.contains) {
				t.Fatalf("invalid dependency error = %v, want substring %q", compileErr, tc.contains)
			}
		})
	}
}
