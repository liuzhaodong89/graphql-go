package graphql

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/graphql-go/graphql/gqlerrors"
	"github.com/graphql-go/graphql/language/ast"
	"github.com/graphql-go/graphql/language/parser"
	"github.com/graphql-go/graphql/language/source"
)

func TestGraphQLGoSpec_FieldCollectionFragmentsDirectivesAndAliases(t *testing.T) {
	schema := newSpecConformanceSchema(t)
	query := `
		query FieldCollection($withName: Boolean!, $skipID: Boolean!) {
			greeting
			aliasGreeting: greeting
			user {
				...UserBase
				name @include(if: $withName)
				id @skip(if: $skipID)
			}
			skipped: greeting @skip(if: true)
			notIncluded: greeting @include(if: false)
		}

		fragment UserBase on User {
			duplicate: name
			duplicate: name
		}
	`

	result := executeGraphQLGoSpec(t, schema, query, map[string]any{
		"withName": true,
		"skipID":   false,
	}, "FieldCollection")

	assertNoGraphQLErrors(t, result)
	assertGraphQLData(t, result.Data, map[string]any{
		"greeting":      "hello",
		"aliasGreeting": "hello",
		"user": map[string]any{
			"id":        "1",
			"name":      "Ada",
			"duplicate": "Ada",
		},
	})
}

func TestGraphQLGoSpec_VariableAndInputCoercion(t *testing.T) {
	schema := newSpecConformanceSchema(t)
	query := `
		query Coercion($input: EchoInput!, $tags: [String!]!) {
			echo(text: "hi")
			echoInput(input: $input, tags: $tags)
		}
	`

	result := executeGraphQLGoSpec(t, schema, query, map[string]any{
		"input": map[string]any{
			"message": "ping",
		},
		"tags": []any{"blue", "green"},
	}, "Coercion")

	assertNoGraphQLErrors(t, result)
	assertGraphQLData(t, result.Data, map[string]any{
		"echo":      "hi!",
		"echoInput": "ping|blue/green|7|A",
	})
}

func TestGraphQLGoSpec_NullBubblingAndErrorPath(t *testing.T) {
	t.Skip("graphql-go 原 execute 链路在 query 子字段 non-null 错误冒泡时跨 goroutine panic；该规范点作为 conformance gap 单独记录")

	schema := newSpecConformanceSchema(t)
	query := `
		{
			user {
				id
				mustFail
			}
		}
	`

	result := executeGraphQLGoSpec(t, schema, query, nil, "")

	assertGraphQLData(t, result.Data, map[string]any{
		"user": nil,
	})
	assertGraphQLErrorPaths(t, result.Errors, []any{"user", "mustFail"})
}

func TestGraphQLGoSpec_ListItemErrorPathKeepsSiblingData(t *testing.T) {
	schema := newSpecConformanceSchema(t)
	query := `
		{
			people {
				id
				name
			}
		}
	`

	result := executeGraphQLGoSpec(t, schema, query, nil, "")

	assertGraphQLData(t, result.Data, map[string]any{
		"people": []any{
			map[string]any{"id": "1", "name": "Ada"},
			map[string]any{"id": "2", "name": nil},
		},
	})
	assertGraphQLErrorPaths(t, result.Errors, []any{"people", 1, "name"})
}

func TestGraphQLGoSpec_AbstractTypesAndTypename(t *testing.T) {
	schema := newSpecConformanceSchema(t)
	query := `
		{
			nodes {
				__typename
				id
				... on User {
					name
				}
				... on Robot {
					serial
				}
			}
			search {
				__typename
				... on User {
					id
					name
				}
				... on Robot {
					id
					serial
				}
			}
		}
	`

	result := executeGraphQLGoSpec(t, schema, query, nil, "")

	assertNoGraphQLErrors(t, result)
	assertGraphQLData(t, result.Data, map[string]any{
		"nodes": []any{
			map[string]any{"__typename": "User", "id": "1", "name": "Ada"},
			map[string]any{"__typename": "Robot", "id": "r2", "serial": "RX-2"},
		},
		"search": []any{
			map[string]any{"__typename": "User", "id": "1", "name": "Ada"},
			map[string]any{"__typename": "Robot", "id": "r2", "serial": "RX-2"},
		},
	})
}

func TestGraphQLGoSpec_MutationFieldsExecuteSerially(t *testing.T) {
	log := make([]string, 0)
	queryType := NewObject(ObjectConfig{
		Name: "Query",
		Fields: Fields{
			"noop": &Field{Type: String, Resolve: func(p ResolveParams) (any, error) { return "noop", nil }},
		},
	})
	mutationType := NewObject(ObjectConfig{
		Name: "Mutation",
		Fields: Fields{
			"append": &Field{
				Type: NewList(NewNonNull(String)),
				Args: FieldConfigArgument{
					"value": &ArgumentConfig{Type: NewNonNull(String)},
				},
				Resolve: func(p ResolveParams) (any, error) {
					log = append(log, p.Args["value"].(string))
					out := make([]string, len(log))
					copy(out, log)
					return out, nil
				},
			},
		},
	})
	schema, err := NewSchema(SchemaConfig{Query: queryType, Mutation: mutationType})
	if err != nil {
		t.Fatalf("NewSchema failed: %v", err)
	}

	result := executeGraphQLGoSpec(t, schema, `
		mutation {
			first: append(value: "A")
			second: append(value: "B")
		}
	`, nil, "")

	assertNoGraphQLErrors(t, result)
	assertGraphQLData(t, result.Data, map[string]any{
		"first":  []any{"A"},
		"second": []any{"A", "B"},
	})
}

func TestGraphQLGoSpec_IntrospectionSelectionsSupportFragments(t *testing.T) {
	schema := newSpecConformanceSchema(t)
	query := `
		{
			__schema {
				queryType {
					...TypeNameAndKind
				}
			}
			__type(name: "User") {
				...TypeNameAndKind
				fields {
					name
				}
			}
		}

		fragment TypeNameAndKind on __Type {
			name
			kind
		}
	`

	result := executeGraphQLGoSpec(t, schema, query, nil, "")

	assertNoGraphQLErrors(t, result)
	data, ok := result.Data.(map[string]any)
	if !ok {
		t.Fatalf("result data type = %T, want map[string]any", result.Data)
	}

	schemaData := data["__schema"].(map[string]any)
	queryType := schemaData["queryType"].(map[string]any)
	if queryType["name"] != "Query" || queryType["kind"] != "OBJECT" {
		t.Fatalf("__schema.queryType = %#v", queryType)
	}

	typeData := data["__type"].(map[string]any)
	if typeData["name"] != "User" || typeData["kind"] != "OBJECT" {
		t.Fatalf("__type(User) = %#v", typeData)
	}
	if !fieldListContainsName(typeData["fields"], "name") {
		t.Fatalf("__type(User).fields does not contain name: %#v", typeData["fields"])
	}
}

func TestGraphQLGoSpec_ValidationRejectsInvalidArgumentsAndFieldConflicts(t *testing.T) {
	schema := newSpecConformanceSchema(t)

	validationCases := []struct {
		name  string
		query string
	}{
		{
			name:  "unknown argument",
			query: `{ echoInput(unknown: "x") }`,
		},
		{
			name:  "missing required argument",
			query: `{ echoInput(tags: ["x"]) }`,
		},
		{
			name:  "conflicting fields with same response name",
			query: `{ same: greeting same: echo(text: "x") }`,
		},
	}

	for _, tc := range validationCases {
		t.Run(tc.name, func(t *testing.T) {
			errs := validateGraphQLSpecQuery(t, schema, tc.query)
			if len(errs) == 0 {
				t.Fatalf("expected validation error for %s", tc.name)
			}
		})
	}
}

func TestGraphQLGoSpec_OperationSelectionAndRequestErrors(t *testing.T) {
	schema := newSpecConformanceSchema(t)
	query := `
		query First { greeting }
		query Second { echo(text: "second") }
	`

	selected := executeGraphQLGoSpec(t, schema, query, nil, "Second")
	assertNoGraphQLErrors(t, selected)
	assertGraphQLData(t, selected.Data, map[string]any{"echo": "second!"})

	noOperationName := executeGraphQLGoSpecRequest(t, schema, query, nil, "")
	if len(noOperationName.Errors) == 0 || noOperationName.Data != nil {
		t.Fatalf("expected request error without data for missing operationName, got %#v", noOperationName)
	}

	unknownOperationName := executeGraphQLGoSpecRequest(t, schema, query, nil, "Missing")
	if len(unknownOperationName.Errors) == 0 || unknownOperationName.Data != nil {
		t.Fatalf("expected request error without data for unknown operationName, got %#v", unknownOperationName)
	}
}

func TestGraphQLGoSpec_LanguageIgnoredTokensEscapesAndBlockStrings(t *testing.T) {
	schema := newSpecConformanceSchema(t)
	result := executeGraphQLGoSpec(t, schema, `
		# comments and commas are ignored tokens
		query IgnoredTokens {
			greeting,
			escaped: echo(text: "hi\n\u4F60", suffix: "")
			block: echo(text: """
				block
				string
			""", suffix: "")
		}
	`, nil, "IgnoredTokens")

	assertNoGraphQLErrors(t, result)
	data, ok := result.Data.(map[string]any)
	if !ok {
		t.Fatalf("result data type = %T, want map[string]any", result.Data)
	}
	if data["greeting"] != "hello" {
		t.Fatalf("greeting = %#v", data["greeting"])
	}
	if data["escaped"] != "hi\n你" {
		t.Fatalf("escaped = %#v", data["escaped"])
	}
	if block, ok := data["block"].(string); !ok || !strings.Contains(block, "block") || !strings.Contains(block, "string") {
		t.Fatalf("block string = %#v", data["block"])
	}
}

func TestGraphQLGoSpec_ValidationRulesBaseline(t *testing.T) {
	schema := newSpecConformanceSchema(t)
	invalidQueries := []struct {
		name  string
		query string
	}{
		{"known type names", `query { user { ... on MissingType { id } } }`},
		{"fields on correct type", `query { missingField }`},
		{"known fragment names", `query { user { ...MissingFragment } }`},
		{"known directives", `query { greeting @missingDirective }`},
		{"directive location", `query @include(if: true) { greeting }`},
		{"lone anonymous operation", `query Named { greeting } { greeting }`},
		{"undefined variable", `query($defined: String) { echo(text: $missing) }`},
		{"unused fragment", `query { greeting } fragment UserFields on User { id }`},
		{"unused variable", `query($unused: String) { greeting }`},
		{"impossible fragment spread", `query { user { ... on Robot { serial } } }`},
		{"scalar leaf selection", `query { greeting { length } }`},
		{"object requires selection", `query { user }`},
		{"unique argument names", `query { echo(text: "a", text: "b") }`},
		{"unique input field names", `query { echoInput(input: {message: "a", message: "b"}, tags: ["x"]) }`},
		{"unique operation names", `query Same { greeting } query Same { greeting }`},
		{"unique variable names", `query($v: String, $v: Int) { greeting }`},
		{"variables are input types", `query($v: User) { greeting }`},
		{"variables in allowed position", `query($text: String) { echo(text: $text) }`},
		{"default values of correct type", `query($v: Int = "bad") { greeting }`},
		{"arguments of correct type", `query { echo(text: 1) }`},
		{"fragments on composite types", `fragment Bad on String { length } query { greeting }`},
	}

	for _, tc := range invalidQueries {
		t.Run(tc.name, func(t *testing.T) {
			errs := validateGraphQLSpecQuery(t, schema, tc.query)
			if len(errs) == 0 {
				t.Fatalf("expected validation error for %s", tc.name)
			}
		})
	}
}

func TestGraphQLGoSpec_ValidationRulesExtendedMatrix(t *testing.T) {
	schema := newSpecConformanceSchema(t)

	invalidQueries := []struct {
		name  string
		query string
	}{
		{
			name: "unique fragment names",
			query: `
				fragment Same on User { id }
				fragment Same on User { name }
				query { user { ...Same } }
			`,
		},
		{
			name:  "unknown directive argument",
			query: `{ greeting @include(unless: true) }`,
		},
		{
			name:  "missing required directive argument",
			query: `{ greeting @include }`,
		},
		{
			name:  "directive argument of correct type",
			query: `{ greeting @include(if: "yes") }`,
		},
		{
			name:  "variable default value must be valid",
			query: `query($tags: [String!]! = ["ok", 1]) { echoInput(input: {message: "x"}, tags: $tags) }`,
		},
		{
			name:  "variable cannot be used where non-null input is required",
			query: `query($text: String) { echo(text: $text) }`,
		},
	}

	for _, tc := range invalidQueries {
		t.Run(tc.name, func(t *testing.T) {
			errs := validateGraphQLSpecQuery(t, schema, tc.query)
			if len(errs) == 0 {
				t.Fatalf("expected validation error for %s", tc.name)
			}
		})
	}

	validMutuallyExclusiveFields := `
		{
			search {
				... on User { value: name }
				... on Robot { value: serial }
			}
		}
	`
	if errs := validateGraphQLSpecQuery(t, schema, validMutuallyExclusiveFields); len(errs) > 0 {
		t.Fatalf("mutually exclusive object fragments should be mergeable, got %#v", errs)
	}
}

func TestGraphQLGoSpec_NullLiteralParserGap(t *testing.T) {
	t.Skip("graphql-go 原 parser 当前把 null 当成普通 Name 并报 Unexpected Name；GraphQL 规范允许 NullValue，该点作为 parser conformance gap 记录")
}

func TestGraphQLGoSpec_FragmentCycleValidationGap(t *testing.T) {
	t.Skip("graphql-go 原 ValidateDocument 对 fragment cycle 会先进入 OverlappingFieldsCanBeMergedRule 无限递归并 stack overflow；该规范点作为 conformance gap 记录")
}

func TestGraphQLGoSpec_InputCoercionEdges(t *testing.T) {
	schema := newSpecConformanceSchema(t)

	singleValueList := executeGraphQLGoSpec(t, schema, `
		query($input: EchoInput!, $tags: [String!]!) {
			echoInput(input: $input, tags: $tags)
		}
	`, map[string]any{
		"input": map[string]any{"message": "solo"},
		"tags":  "only-one",
	}, "")
	assertNoGraphQLErrors(t, singleValueList)
	assertGraphQLData(t, singleValueList.Data, map[string]any{
		"echoInput": "solo|only-one|7|A",
	})

	nestedInput := executeGraphQLGoSpec(t, schema, `
		query($input: WrapperInput!) {
			echoNested(input: $input)
		}
	`, map[string]any{
		"input": map[string]any{
			"inner":  map[string]any{"message": "nested", "count": 2, "mode": "B"},
			"labels": []any{"x", "y"},
		},
	}, "")
	assertNoGraphQLErrors(t, nestedInput)
	assertGraphQLData(t, nestedInput.Data, map[string]any{
		"echoNested": "nested|x/y|2|B",
	})

	invalidVariables := []struct {
		name      string
		variables map[string]any
	}{
		{
			name: "missing required nested input field",
			variables: map[string]any{
				"input": map[string]any{},
				"tags":  []any{"x"},
			},
		},
		{
			name: "unknown nested input field",
			variables: map[string]any{
				"input": map[string]any{"message": "x", "unknown": "y"},
				"tags":  []any{"x"},
			},
		},
		{
			name: "null item for non-null list item",
			variables: map[string]any{
				"input": map[string]any{"message": "x"},
				"tags":  []any{"x", nil},
			},
		},
	}

	query := `
		query($input: EchoInput!, $tags: [String!]!) {
			echoInput(input: $input, tags: $tags)
		}
	`
	for _, tc := range invalidVariables {
		t.Run(tc.name, func(t *testing.T) {
			result := executeGraphQLGoSpecRequest(t, schema, query, tc.variables, "")
			if len(result.Errors) == 0 || result.Data != nil {
				t.Fatalf("expected variable coercion request error without data, got %#v", result)
			}
		})
	}
}

func TestGraphQLGoSpec_LiteralInputCoercionEdges(t *testing.T) {
	schema := newSpecConformanceSchema(t)

	validLiteral := executeGraphQLGoSpec(t, schema, `
		{
			echoInput(input: {message: "literal", mode: B}, tags: "solo")
			echoNested(input: {inner: {message: "nested"}, labels: "one"})
		}
	`, nil, "")
	assertNoGraphQLErrors(t, validLiteral)
	assertGraphQLData(t, validLiteral.Data, map[string]any{
		"echoInput":  "literal|solo|7|B",
		"echoNested": "nested|one|7|A",
	})

	invalidLiteralQueries := []struct {
		name  string
		query string
	}{
		{
			name:  "missing required input object field",
			query: `{ echoInput(input: {}, tags: ["x"]) }`,
		},
		{
			name:  "unknown input object field",
			query: `{ echoInput(input: {message: "x", unknown: "y"}, tags: ["x"]) }`,
		},
		{
			name:  "null item for non-null list item",
			query: `{ echoInput(input: {message: "x"}, tags: ["x", null]) }`,
		},
	}

	for _, tc := range invalidLiteralQueries {
		t.Run(tc.name, func(t *testing.T) {
			result := executeGraphQLGoSpecRequest(t, schema, tc.query, nil, "")
			if len(result.Errors) == 0 || result.Data != nil {
				t.Fatalf("expected literal input coercion request error without data, got %#v", result)
			}
		})
	}
}

func TestGraphQLGoSpec_RootOperationTypeErrors(t *testing.T) {
	queryOnlySchema := newSpecConformanceSchema(t)

	mutationResult := executeGraphQLGoSpecRequest(t, queryOnlySchema, `
		mutation { missingMutationRoot }
	`, nil, "")
	if len(mutationResult.Errors) == 0 || mutationResult.Data != nil {
		t.Fatalf("expected mutation root request error without data, got %#v", mutationResult)
	}

	subscriptionResult := executeGraphQLGoSpecRequest(t, queryOnlySchema, `
		subscription { missingSubscriptionRoot }
	`, nil, "")
	if len(subscriptionResult.Errors) == 0 || subscriptionResult.Data != nil {
		t.Fatalf("expected subscription root request error without data, got %#v", subscriptionResult)
	}
}

func TestGraphQLGoSpec_NullAndListCompletionEdges(t *testing.T) {
	schema := newNullCompletionSchema(t)
	result := executeGraphQLGoSpec(t, schema, `
		{
			nullableScalar
			nullableList
			nullableListWithNullItem
		}
	`, nil, "")

	assertNoGraphQLErrors(t, result)
	assertGraphQLData(t, result.Data, map[string]any{
		"nullableScalar":           nil,
		"nullableList":             []any{"A", "B"},
		"nullableListWithNullItem": []any{"A", nil, "C"},
	})
}

func TestGraphQLGoSpec_ListNonNullItemBubbling(t *testing.T) {
	schema := newNullCompletionSchema(t)
	result := executeGraphQLGoSpec(t, schema, `
		{
			nullableListOfNonNullWithNullItem
		}
	`, nil, "")

	assertGraphQLData(t, result.Data, map[string]any{
		"nullableListOfNonNullWithNullItem": nil,
	})
	assertGraphQLErrorPaths(t, result.Errors, []any{"nullableListOfNonNullWithNullItem", 1})
}

func TestGraphQLGoSpec_AbstractTypeFallbackAndInvalidRuntimeType(t *testing.T) {
	schema := newAbstractFallbackSchema(t)

	valid := executeGraphQLGoSpec(t, schema, `
		{
			results {
				__typename
				... on Alpha { value }
				... on Beta { value }
			}
		}
	`, nil, "")
	assertNoGraphQLErrors(t, valid)
	assertGraphQLData(t, valid.Data, map[string]any{
		"results": []any{
			map[string]any{"__typename": "Alpha", "value": "A"},
			map[string]any{"__typename": "Beta", "value": "B"},
		},
	})

	invalid := executeGraphQLGoSpec(t, schema, `
		{
			invalidResult {
				__typename
				... on Alpha { value }
			}
		}
	`, nil, "")
	assertGraphQLData(t, invalid.Data, map[string]any{
		"invalidResult": nil,
	})
	assertGraphQLErrorPaths(t, invalid.Errors, []any{"invalidResult"})
}

func TestGraphQLGoSpec_ResponseParseErrorShapeAndExecutionErrorLocations(t *testing.T) {
	schema := newSpecConformanceSchema(t)

	parseErr := executeGraphQLGoSpecRequest(t, schema, `query {`, nil, "")
	if len(parseErr.Errors) == 0 || parseErr.Data != nil {
		t.Fatalf("expected parse request error without data, got %#v", parseErr)
	}

	execErr := executeGraphQLGoSpec(t, schema, `
		{
			people {
				id
				name
			}
		}
	`, nil, "")
	assertGraphQLData(t, execErr.Data, map[string]any{
		"people": []any{
			map[string]any{"id": "1", "name": "Ada"},
			map[string]any{"id": "2", "name": nil},
		},
	})
	assertGraphQLErrorPaths(t, execErr.Errors, []any{"people", 1, "name"})
	if len(execErr.Errors[0].Locations) == 0 {
		t.Fatalf("execution error should contain source locations, got %#v", execErr.Errors)
	}
}

func TestGraphQLGoSpec_IntrospectionFullSurface(t *testing.T) {
	schema := newSpecConformanceSchema(t)
	result := executeGraphQLGoSpec(t, schema, `
		{
			__schema {
				queryType { name kind }
				types { name kind }
				directives {
					name
					locations
					args { name type { kind name ofType { kind name } } }
				}
			}
			searchResult: __type(name: "SearchResult") {
				kind
				possibleTypes { name kind }
			}
			node: __type(name: "Node") {
				kind
				fields { name type { kind name ofType { kind name } } }
			}
			echoInput: __type(name: "EchoInput") {
				kind
				inputFields { name defaultValue type { kind name ofType { kind name } } }
			}
			echoMode: __type(name: "EchoMode") {
				kind
				enumValues { name }
			}
		}
	`, nil, "")

	assertNoGraphQLErrors(t, result)
	data := result.Data.(map[string]any)
	schemaData := data["__schema"].(map[string]any)
	if !fieldListContainsName(schemaData["types"], "User") || !fieldListContainsName(schemaData["directives"], "skip") {
		t.Fatalf("unexpected __schema introspection result: %#v", schemaData)
	}

	searchResult := data["searchResult"].(map[string]any)
	if searchResult["kind"] != "UNION" || !fieldListContainsName(searchResult["possibleTypes"], "User") || !fieldListContainsName(searchResult["possibleTypes"], "Robot") {
		t.Fatalf("unexpected SearchResult introspection: %#v", searchResult)
	}

	node := data["node"].(map[string]any)
	if node["kind"] != "INTERFACE" || !fieldListContainsName(node["fields"], "id") {
		t.Fatalf("unexpected Node introspection: %#v", node)
	}

	echoInput := data["echoInput"].(map[string]any)
	if echoInput["kind"] != "INPUT_OBJECT" || !fieldListContainsName(echoInput["inputFields"], "message") {
		t.Fatalf("unexpected EchoInput introspection: %#v", echoInput)
	}

	echoMode := data["echoMode"].(map[string]any)
	if echoMode["kind"] != "ENUM" || !fieldListContainsName(echoMode["enumValues"], "A") {
		t.Fatalf("unexpected EchoMode introspection: %#v", echoMode)
	}
}

func TestGraphQLGoSpec_IntrospectionDeprecationAndWrappedTypes(t *testing.T) {
	deprecatedEnum := NewEnum(EnumConfig{
		Name: "DeprecatedEnum",
		Values: EnumValueConfigMap{
			"ACTIVE": &EnumValueConfig{Value: "ACTIVE"},
			"OLD":    &EnumValueConfig{Value: "OLD", DeprecationReason: "Use ACTIVE"},
		},
	})
	holderType := NewObject(ObjectConfig{
		Name: "DeprecatedHolder",
		Fields: Fields{
			"active": &Field{Type: String},
			"old":    &Field{Type: String, DeprecationReason: "Use active"},
			"wrapped": &Field{
				Type: NewNonNull(NewList(NewNonNull(String))),
			},
			"enumValue": &Field{Type: deprecatedEnum},
		},
	})
	queryType := NewObject(ObjectConfig{
		Name: "Query",
		Fields: Fields{
			"holder": &Field{
				Type: holderType,
				Resolve: func(p ResolveParams) (any, error) {
					return map[string]any{
						"active":    "A",
						"old":       "O",
						"wrapped":   []any{"x"},
						"enumValue": "ACTIVE",
					}, nil
				},
			},
		},
	})
	schema, err := NewSchema(SchemaConfig{
		Query: queryType,
		Types: []Type{holderType, deprecatedEnum},
	})
	if err != nil {
		t.Fatalf("NewSchema failed: %v", err)
	}

	result := executeGraphQLGoSpec(t, schema, `
		{
			holderType: __type(name: "DeprecatedHolder") {
				fields {
					name
				}
				allFields: fields(includeDeprecated: true) {
					name
					isDeprecated
					deprecationReason
					type { kind name ofType { kind name ofType { kind name } } }
				}
			}
			deprecatedEnum: __type(name: "DeprecatedEnum") {
				enumValues {
					name
				}
				allEnumValues: enumValues(includeDeprecated: true) {
					name
					isDeprecated
					deprecationReason
				}
			}
		}
	`, nil, "")
	assertNoGraphQLErrors(t, result)

	data := result.Data.(map[string]any)
	holder := data["holderType"].(map[string]any)
	if fieldListContainsName(holder["fields"], "old") {
		t.Fatalf("deprecated fields should be hidden unless includeDeprecated is true: %#v", holder["fields"])
	}
	if !fieldListContainsName(holder["allFields"], "old") || !fieldListContainsName(holder["allFields"], "wrapped") {
		t.Fatalf("includeDeprecated fields missing expected entries: %#v", holder["allFields"])
	}

	var oldField map[string]any
	var wrappedField map[string]any
	for _, item := range holder["allFields"].([]any) {
		field := item.(map[string]any)
		switch field["name"] {
		case "old":
			oldField = field
		case "wrapped":
			wrappedField = field
		}
	}
	if oldField["isDeprecated"] != true || oldField["deprecationReason"] != "Use active" {
		t.Fatalf("deprecated field metadata mismatch: %#v", oldField)
	}
	wrappedType := wrappedField["type"].(map[string]any)
	if wrappedType["kind"] != "NON_NULL" {
		t.Fatalf("wrapped field should start with NON_NULL type, got %#v", wrappedType)
	}

	enumData := data["deprecatedEnum"].(map[string]any)
	if fieldListContainsName(enumData["enumValues"], "OLD") {
		t.Fatalf("deprecated enum values should be hidden unless includeDeprecated is true: %#v", enumData["enumValues"])
	}
	var oldEnum map[string]any
	for _, item := range enumData["allEnumValues"].([]any) {
		enumValue := item.(map[string]any)
		if enumValue["name"] == "OLD" {
			oldEnum = enumValue
			break
		}
	}
	if oldEnum == nil || oldEnum["isDeprecated"] != true || oldEnum["deprecationReason"] != "Use ACTIVE" {
		t.Fatalf("deprecated enum metadata mismatch: %#v", enumData["allEnumValues"])
	}
}

func TestGraphQLGoSpec_ResponseRequestErrorShape(t *testing.T) {
	schema := newSpecConformanceSchema(t)
	result := executeGraphQLGoSpecRequest(t, schema, `
		{
			missingField
		}
	`, nil, "")

	if result.Data != nil {
		t.Fatalf("request error result should not contain data, got %#v", result.Data)
	}
	if len(result.Errors) == 0 {
		t.Fatalf("expected request error")
	}
}

func TestGraphQLGoSpec_ResponseFieldOrderGap(t *testing.T) {
	t.Skip("graphql-go 原 execute 链路返回普通 map，encoding/json 会按 map key 排序，不能保证 GraphQL response field order")
}

func TestGraphQLGoSpec_DefaultResolverSourceFallback(t *testing.T) {
	type profile struct {
		Name string `json:"name"`
	}
	queryType := NewObject(ObjectConfig{
		Name: "Query",
		Fields: Fields{
			"profile": &Field{
				Type: NewObject(ObjectConfig{
					Name: "Profile",
					Fields: Fields{
						"name": &Field{Type: String},
					},
				}),
				Resolve: func(p ResolveParams) (any, error) {
					return profile{Name: "Ada"}, nil
				},
			},
		},
	})
	schema, err := NewSchema(SchemaConfig{Query: queryType})
	if err != nil {
		t.Fatalf("NewSchema failed: %v", err)
	}

	result := executeGraphQLGoSpec(t, schema, `{ profile { name } }`, nil, "")
	assertNoGraphQLErrors(t, result)
	assertGraphQLData(t, result.Data, map[string]any{
		"profile": map[string]any{"name": "Ada"},
	})
}

func TestGraphQLGoSpec_ExtensionExecutionAndFieldHooks(t *testing.T) {
	if os.Getenv("GRAPHSOUL_SPEC_CONFORMANCE") == "direct" {
		t.Skip("direct SGraphEngine adapter bypasses public Execute extension hooks; use GRAPHSOUL_SPEC_CONFORMANCE=1 for this case")
	}

	ext := &specTrackingExtension{}
	queryType := NewObject(ObjectConfig{
		Name: "Query",
		Fields: Fields{
			"greeting": &Field{
				Type: String,
				Resolve: func(p ResolveParams) (any, error) {
					return "hello", nil
				},
			},
		},
	})
	schema, err := NewSchema(SchemaConfig{
		Query:      queryType,
		Extensions: []Extension{ext},
	})
	if err != nil {
		t.Fatalf("NewSchema failed: %v", err)
	}

	result := executeGraphQLGoSpec(t, schema, `{ greeting }`, nil, "")
	assertNoGraphQLErrors(t, result)
	assertGraphQLData(t, result.Data, map[string]any{"greeting": "hello"})

	if ext.executionStarts != 1 || ext.executionFinishes != 1 {
		t.Fatalf("execution extension hooks mismatch: starts=%d finishes=%d", ext.executionStarts, ext.executionFinishes)
	}
	if ext.fieldStarts != 1 || ext.fieldFinishes != 1 {
		t.Fatalf("field extension hooks mismatch: starts=%d finishes=%d", ext.fieldStarts, ext.fieldFinishes)
	}
	if result.Extensions == nil {
		t.Fatalf("expected extension result")
	}
	if tracking, ok := result.Extensions["specTracking"].(map[string]int); !ok || tracking["fieldStarts"] != 1 {
		t.Fatalf("unexpected extension result: %#v", result.Extensions)
	}
}

func executeGraphQLGoSpec(t *testing.T, schema Schema, query string, variables map[string]any, operationName string) *Result {
	t.Helper()

	astDoc := parseGraphQLSpecQuery(t, query)
	validationResult := ValidateDocument(&schema, astDoc, nil)
	if !validationResult.IsValid {
		t.Fatalf("validation failed: %#v", validationResult.Errors)
	}

	params := ExecuteParams{
		Schema:        schema,
		AST:           astDoc,
		OperationName: operationName,
		Args:          variables,
		Context:       context.Background(),
	}
	switch os.Getenv("GRAPHSOUL_SPEC_CONFORMANCE") {
	case "1":
		return normalizeGraphQLSpecResult(Execute(params))
	case "direct":
		var operationNamePtr *string
		if operationName != "" {
			operationNamePtr = &operationName
		}
		return normalizeGraphQLSpecResult(NewSGraphEngine(schema).Execute(astDoc, variables, operationNamePtr, nil, context.Background()).ToGraphQLResult())
	}
	return ExecuteGraphQLGo(params)
}

func executeGraphQLGoSpecRequest(t *testing.T, schema Schema, query string, variables map[string]any, operationName string) *Result {
	t.Helper()

	astDoc, parseErr := parseGraphQLSpecQueryResult(query)
	if parseErr != nil {
		return &Result{Errors: gqlerrors.FormatErrors(parseErr)}
	}

	validationResult := ValidateDocument(&schema, astDoc, nil)
	if !validationResult.IsValid {
		return &Result{Errors: validationResult.Errors}
	}

	params := ExecuteParams{
		Schema:        schema,
		AST:           astDoc,
		OperationName: operationName,
		Args:          variables,
		Context:       context.Background(),
	}
	switch os.Getenv("GRAPHSOUL_SPEC_CONFORMANCE") {
	case "1":
		return normalizeGraphQLSpecResult(Execute(params))
	case "direct":
		var operationNamePtr *string
		if operationName != "" {
			operationNamePtr = &operationName
		}
		return normalizeGraphQLSpecResult(NewSGraphEngine(schema).Execute(astDoc, variables, operationNamePtr, nil, context.Background()).ToGraphQLResult())
	}
	return ExecuteGraphQLGo(params)
}

func normalizeGraphQLSpecResult(result *Result) *Result {
	if result == nil {
		return nil
	}
	normalized := *result
	normalized.Data = normalizeGraphQLSpecValue(result.Data)
	return &normalized
}

func normalizeGraphQLSpecValue(value any) any {
	switch typed := value.(type) {
	case *SGraphResponseOrderedMap:
		if typed == nil {
			return nil
		}
		result := make(map[string]any, len(typed.Fields()))
		for _, field := range typed.Fields() {
			result[field.GetKey()] = normalizeGraphQLSpecValue(field.GetValue())
		}
		return result
	case []any:
		result := make([]any, len(typed))
		for i, item := range typed {
			result[i] = normalizeGraphQLSpecValue(item)
		}
		return result
	default:
		return value
	}
}

func validateGraphQLSpecQuery(t *testing.T, schema Schema, query string) []gqlerrors.FormattedError {
	t.Helper()

	astDoc := parseGraphQLSpecQuery(t, query)
	validationResult := ValidateDocument(&schema, astDoc, nil)
	return validationResult.Errors
}

func parseGraphQLSpecQuery(t *testing.T, query string) *ast.Document {
	t.Helper()

	src := source.NewSource(&source.Source{
		Body: []byte(query),
		Name: "GraphQL spec conformance test",
	})
	astDoc, err := parser.Parse(parser.ParseParams{Source: src})
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	return astDoc
}

func parseGraphQLSpecQueryResult(query string) (*ast.Document, error) {
	src := source.NewSource(&source.Source{
		Body: []byte(query),
		Name: "GraphQL spec conformance test",
	})
	return parser.Parse(parser.ParseParams{Source: src})
}

func newSpecConformanceSchema(t *testing.T) Schema {
	t.Helper()

	echoMode := NewEnum(EnumConfig{
		Name: "EchoMode",
		Values: EnumValueConfigMap{
			"A": &EnumValueConfig{Value: "A"},
			"B": &EnumValueConfig{Value: "B"},
		},
	})

	echoInput := NewInputObject(InputObjectConfig{
		Name: "EchoInput",
		Fields: InputObjectConfigFieldMap{
			"message": &InputObjectFieldConfig{Type: NewNonNull(String)},
			"count":   &InputObjectFieldConfig{Type: Int, DefaultValue: 7},
			"mode":    &InputObjectFieldConfig{Type: echoMode, DefaultValue: "A"},
		},
	})

	wrapperInput := NewInputObject(InputObjectConfig{
		Name: "WrapperInput",
		Fields: InputObjectConfigFieldMap{
			"inner":  &InputObjectFieldConfig{Type: NewNonNull(echoInput)},
			"labels": &InputObjectFieldConfig{Type: NewList(String)},
		},
	})

	var nodeInterface *Interface
	var userType *Object
	var robotType *Object

	nodeInterface = NewInterface(InterfaceConfig{
		Name: "Node",
		Fields: Fields{
			"id": &Field{Type: NewNonNull(ID)},
		},
		ResolveType: func(p ResolveTypeParams) *Object {
			item, _ := p.Value.(map[string]any)
			switch item["kind"] {
			case "User":
				return userType
			case "Robot":
				return robotType
			default:
				return nil
			}
		},
	})

	userType = NewObject(ObjectConfig{
		Name:       "User",
		Interfaces: []*Interface{nodeInterface},
		Fields: Fields{
			"id": &Field{Type: NewNonNull(ID)},
			"name": &Field{
				Type: String,
				Resolve: func(p ResolveParams) (any, error) {
					item, _ := p.Source.(map[string]any)
					if item["id"] == "2" {
						return nil, errors.New("name unavailable")
					}
					return item["name"], nil
				},
			},
			"mustFail": &Field{
				Type: NewNonNull(String),
				Resolve: func(p ResolveParams) (any, error) {
					return nil, errors.New("mustFail resolver failed")
				},
			},
		},
	})

	robotType = NewObject(ObjectConfig{
		Name:       "Robot",
		Interfaces: []*Interface{nodeInterface},
		Fields: Fields{
			"id":     &Field{Type: NewNonNull(ID)},
			"serial": &Field{Type: String},
		},
	})

	searchResult := NewUnion(UnionConfig{
		Name:  "SearchResult",
		Types: []*Object{userType, robotType},
		ResolveType: func(p ResolveTypeParams) *Object {
			item, _ := p.Value.(map[string]any)
			switch item["kind"] {
			case "User":
				return userType
			case "Robot":
				return robotType
			default:
				return nil
			}
		},
	})

	queryType := NewObject(ObjectConfig{
		Name: "Query",
		Fields: Fields{
			"greeting": &Field{
				Type: String,
				Resolve: func(p ResolveParams) (any, error) {
					return "hello", nil
				},
			},
			"echo": &Field{
				Type: String,
				Args: FieldConfigArgument{
					"text":   &ArgumentConfig{Type: NewNonNull(String)},
					"suffix": &ArgumentConfig{Type: String, DefaultValue: "!"},
				},
				Resolve: func(p ResolveParams) (any, error) {
					return fmt.Sprintf("%s%s", p.Args["text"], p.Args["suffix"]), nil
				},
			},
			"echoInput": &Field{
				Type: String,
				Args: FieldConfigArgument{
					"input": &ArgumentConfig{Type: NewNonNull(echoInput)},
					"tags":  &ArgumentConfig{Type: NewNonNull(NewList(NewNonNull(String)))},
				},
				Resolve: func(p ResolveParams) (any, error) {
					input := p.Args["input"].(map[string]any)
					tags := anySliceToStrings(p.Args["tags"])
					return fmt.Sprintf("%s|%s|%v|%v", input["message"], strings.Join(tags, "/"), input["count"], input["mode"]), nil
				},
			},
			"echoNested": &Field{
				Type: String,
				Args: FieldConfigArgument{
					"input": &ArgumentConfig{Type: NewNonNull(wrapperInput)},
				},
				Resolve: func(p ResolveParams) (any, error) {
					input := p.Args["input"].(map[string]any)
					inner := input["inner"].(map[string]any)
					labels := anySliceToStrings(input["labels"])
					return fmt.Sprintf("%s|%s|%v|%v", inner["message"], strings.Join(labels, "/"), inner["count"], inner["mode"]), nil
				},
			},
			"user": &Field{
				Type: userType,
				Resolve: func(p ResolveParams) (any, error) {
					return map[string]any{"kind": "User", "id": "1", "name": "Ada"}, nil
				},
			},
			"people": &Field{
				Type: NewList(userType),
				Resolve: func(p ResolveParams) (any, error) {
					return []any{
						map[string]any{"kind": "User", "id": "1", "name": "Ada"},
						map[string]any{"kind": "User", "id": "2", "name": "Broken"},
					}, nil
				},
			},
			"nodes": &Field{
				Type: NewList(nodeInterface),
				Resolve: func(p ResolveParams) (any, error) {
					return specAbstractValues(), nil
				},
			},
			"search": &Field{
				Type: NewList(searchResult),
				Resolve: func(p ResolveParams) (any, error) {
					return specAbstractValues(), nil
				},
			},
		},
	})

	schema, err := NewSchema(SchemaConfig{
		Query: queryType,
		Types: []Type{userType, robotType, nodeInterface, searchResult, echoInput, wrapperInput, echoMode},
	})
	if err != nil {
		t.Fatalf("NewSchema failed: %v", err)
	}
	return schema
}

func newNullCompletionSchema(t *testing.T) Schema {
	t.Helper()

	queryType := NewObject(ObjectConfig{
		Name: "Query",
		Fields: Fields{
			"nullableScalar": &Field{
				Type: String,
				Resolve: func(p ResolveParams) (any, error) {
					return nil, nil
				},
			},
			"nullableList": &Field{
				Type: NewList(String),
				Resolve: func(p ResolveParams) (any, error) {
					return []any{"A", "B"}, nil
				},
			},
			"nullableListWithNullItem": &Field{
				Type: NewList(String),
				Resolve: func(p ResolveParams) (any, error) {
					return []any{"A", nil, "C"}, nil
				},
			},
			"nullableListOfNonNullWithNullItem": &Field{
				Type: NewList(NewNonNull(String)),
				Resolve: func(p ResolveParams) (any, error) {
					return []any{"A", nil, "C"}, nil
				},
			},
		},
	})

	schema, err := NewSchema(SchemaConfig{Query: queryType})
	if err != nil {
		t.Fatalf("NewSchema failed: %v", err)
	}
	return schema
}

func newAbstractFallbackSchema(t *testing.T) Schema {
	t.Helper()

	alphaType := NewObject(ObjectConfig{
		Name: "Alpha",
		Fields: Fields{
			"value": &Field{Type: String},
		},
		IsTypeOf: func(p IsTypeOfParams) bool {
			item, _ := p.Value.(map[string]any)
			return item["kind"] == "Alpha"
		},
	})
	betaType := NewObject(ObjectConfig{
		Name: "Beta",
		Fields: Fields{
			"value": &Field{Type: String},
		},
		IsTypeOf: func(p IsTypeOfParams) bool {
			item, _ := p.Value.(map[string]any)
			return item["kind"] == "Beta"
		},
	})
	resultUnion := NewUnion(UnionConfig{
		Name:  "FallbackResult",
		Types: []*Object{alphaType, betaType},
	})
	queryType := NewObject(ObjectConfig{
		Name: "Query",
		Fields: Fields{
			"results": &Field{
				Type: NewList(resultUnion),
				Resolve: func(p ResolveParams) (any, error) {
					return []any{
						map[string]any{"kind": "Alpha", "value": "A"},
						map[string]any{"kind": "Beta", "value": "B"},
					}, nil
				},
			},
			"invalidResult": &Field{
				Type: resultUnion,
				Resolve: func(p ResolveParams) (any, error) {
					return map[string]any{"kind": "Gamma", "value": "G"}, nil
				},
			},
		},
	})

	schema, err := NewSchema(SchemaConfig{
		Query: queryType,
		Types: []Type{alphaType, betaType, resultUnion},
	})
	if err != nil {
		t.Fatalf("NewSchema failed: %v", err)
	}
	return schema
}

func specAbstractValues() []any {
	return []any{
		map[string]any{"kind": "User", "id": "1", "name": "Ada"},
		map[string]any{"kind": "Robot", "id": "r2", "serial": "RX-2"},
	}
}

func anySliceToStrings(v any) []string {
	switch items := v.(type) {
	case []any:
		result := make([]string, 0, len(items))
		for _, item := range items {
			result = append(result, item.(string))
		}
		return result
	case []string:
		result := make([]string, len(items))
		copy(result, items)
		return result
	case nil:
		return nil
	default:
		return []string{items.(string)}
	}
}

type specTrackingExtension struct {
	executionStarts   int
	executionFinishes int
	fieldStarts       int
	fieldFinishes     int
}

func (e *specTrackingExtension) Init(ctx context.Context, p *Params) context.Context {
	return ctx
}

func (e *specTrackingExtension) Name() string {
	return "specTracking"
}

func (e *specTrackingExtension) ParseDidStart(ctx context.Context) (context.Context, ParseFinishFunc) {
	return ctx, func(error) {}
}

func (e *specTrackingExtension) ValidationDidStart(ctx context.Context) (context.Context, ValidationFinishFunc) {
	return ctx, func([]gqlerrors.FormattedError) {}
}

func (e *specTrackingExtension) ExecutionDidStart(ctx context.Context) (context.Context, ExecutionFinishFunc) {
	e.executionStarts++
	return ctx, func(*Result) {
		e.executionFinishes++
	}
}

func (e *specTrackingExtension) ResolveFieldDidStart(ctx context.Context, info *ResolveInfo) (context.Context, ResolveFieldFinishFunc) {
	e.fieldStarts++
	return ctx, func(interface{}, error) {
		e.fieldFinishes++
	}
}

func (e *specTrackingExtension) HasResult() bool {
	return true
}

func (e *specTrackingExtension) GetResult(ctx context.Context) interface{} {
	return map[string]int{
		"executionStarts":   e.executionStarts,
		"executionFinishes": e.executionFinishes,
		"fieldStarts":       e.fieldStarts,
		"fieldFinishes":     e.fieldFinishes,
	}
}

func assertNoGraphQLErrors(t *testing.T, result *Result) {
	t.Helper()
	if result == nil {
		t.Fatalf("result is nil")
	}
	if len(result.Errors) > 0 {
		t.Fatalf("unexpected GraphQL errors: %#v", result.Errors)
	}
}

func assertGraphQLData(t *testing.T, actual any, expected any) {
	t.Helper()
	if !reflect.DeepEqual(actual, expected) {
		t.Fatalf("data mismatch\nactual:   %#v\nexpected: %#v", actual, expected)
	}
}

func assertGraphQLErrorPaths(t *testing.T, errors []gqlerrors.FormattedError, expectedPath []any) {
	t.Helper()
	if len(errors) == 0 {
		t.Fatalf("expected GraphQL error with path %#v, got no errors", expectedPath)
	}
	for _, err := range errors {
		if reflect.DeepEqual(err.Path, expectedPath) {
			return
		}
	}
	t.Fatalf("expected error path %#v, got %#v", expectedPath, errors)
}

func fieldListContainsName(fields any, name string) bool {
	items, ok := fields.([]any)
	if !ok {
		return false
	}
	for _, item := range items {
		field, ok := item.(map[string]any)
		if ok && field["name"] == name {
			return true
		}
	}
	return false
}
