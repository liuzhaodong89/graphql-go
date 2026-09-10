package graphql

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/graphql-go/graphql/gqlerrors"
	"github.com/graphql-go/graphql/language/ast"
)

type nativeContextKey struct{}

type nativeTaggedProfile struct {
	DisplayName string `graphql:"name"`
	Age         int    `json:"age"`
}

type nativeFieldResolverSource struct{}

func (nativeFieldResolverSource) Resolve(p ResolveParams) (interface{}, error) {
	return "resolved:" + p.Info.FieldName, nil
}

type nativeExtendedError struct {
	message string
}

type nativeCountingExtension struct {
	fieldStarts   atomic.Int32
	fieldFinishes atomic.Int32
	fieldErrors   atomic.Int32
}

func (e nativeExtendedError) Error() string {
	return e.message
}

func (e nativeExtendedError) Extensions() map[string]interface{} {
	return map[string]interface{}{"code": "NATIVE_TEST"}
}

func (e *nativeCountingExtension) Init(ctx context.Context, p *Params) context.Context {
	return ctx
}

func (e *nativeCountingExtension) Name() string {
	return "nativeCounting"
}

func (e *nativeCountingExtension) ParseDidStart(ctx context.Context) (context.Context, ParseFinishFunc) {
	return ctx, func(error) {}
}

func (e *nativeCountingExtension) ValidationDidStart(ctx context.Context) (context.Context, ValidationFinishFunc) {
	return ctx, func([]gqlerrors.FormattedError) {}
}

func (e *nativeCountingExtension) ExecutionDidStart(ctx context.Context) (context.Context, ExecutionFinishFunc) {
	return ctx, func(*Result) {}
}

func (e *nativeCountingExtension) ResolveFieldDidStart(ctx context.Context, info *ResolveInfo) (context.Context, ResolveFieldFinishFunc) {
	e.fieldStarts.Add(1)
	return ctx, func(_ interface{}, err error) {
		e.fieldFinishes.Add(1)
		if err != nil {
			e.fieldErrors.Add(1)
		}
	}
}

func (e *nativeCountingExtension) HasResult() bool {
	return false
}

func (e *nativeCountingExtension) GetResult(ctx context.Context) interface{} {
	return nil
}

func TestGraphQLGoNative_PublicExecuteUsesSGraphExecutor(t *testing.T) {
	extension := &specTrackingExtension{}
	profileType := NewObject(ObjectConfig{
		Name: "NativeRouteProfile",
		Fields: Fields{
			"name": &Field{Type: String},
		},
	})
	queryType := NewObject(ObjectConfig{
		Name: "Query",
		Fields: Fields{
			"profile": &Field{
				Type: profileType,
				Resolve: func(p ResolveParams) (interface{}, error) {
					return nativeTaggedProfile{DisplayName: "Ada"}, nil
				},
			},
		},
	})
	schema, err := NewSchema(SchemaConfig{Query: queryType, Extensions: []Extension{extension}})
	if err != nil {
		t.Fatalf("NewSchema failed: %v", err)
	}

	// 通过公开 Do 入口覆盖 parse -> validate -> Execute，确认 query 最终进入 SGraph executor。
	result := Do(Params{
		Schema:        schema,
		RequestString: `{ profile { name } }`,
		Context:       context.Background(),
	})

	assertNoGraphQLErrors(t, result)
	assertGraphQLData(t, result.Data, map[string]interface{}{
		"profile": map[string]interface{}{"name": "Ada"},
	})
	if _, ok := result.Data.(*SGraphResponseOrderedMap); !ok {
		t.Fatalf("public Execute data type = %T, want *SGraphResponseOrderedMap", result.Data)
	}
	if extension.executionStarts != 1 || extension.executionFinishes != 1 {
		t.Fatalf("execution hooks = %d/%d, want 1/1", extension.executionStarts, extension.executionFinishes)
	}
	if extension.fieldStarts != 2 || extension.fieldFinishes != 2 {
		t.Fatalf("field hooks = %d/%d, want 2/2", extension.fieldStarts, extension.fieldFinishes)
	}
}

func TestGraphQLGoNative_DirectiveTruthTable(t *testing.T) {
	schema := newSpecConformanceSchema(t)
	tests := []struct {
		name      string
		skip      bool
		include   bool
		wantField bool
	}{
		{name: "skip_false_include_true", skip: false, include: true, wantField: true},
		{name: "skip_true_include_true", skip: true, include: true, wantField: false},
		{name: "skip_false_include_false", skip: false, include: false, wantField: false},
		{name: "skip_true_include_false", skip: true, include: false, wantField: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := executeGraphQLGoSpec(t, schema, `
				query($skip: Boolean!, $include: Boolean!) {
					value: greeting @skip(if: $skip) @include(if: $include)
				}
			`, map[string]interface{}{"skip": tc.skip, "include": tc.include}, "")
			assertNoGraphQLErrors(t, result)
			data := graphQLResultDataMap(t, result)
			_, exists := data["value"]
			if exists != tc.wantField {
				t.Fatalf("field exists = %v, want %v; data=%#v", exists, tc.wantField, data)
			}
		})
	}
}

func TestGraphQLGoNative_RepeatedResponseNameUsesIncludedOccurrences(t *testing.T) {
	schema := newSpecConformanceSchema(t)
	tests := []struct {
		name       string
		first      bool
		skipSecond bool
		wantField  bool
	}{
		{name: "first_included", first: true, skipSecond: true, wantField: true},
		{name: "second_included", first: false, skipSecond: false, wantField: true},
		{name: "both_excluded", first: false, skipSecond: true, wantField: false},
		{name: "both_included", first: true, skipSecond: false, wantField: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := executeGraphQLGoSpec(t, schema, `
				query($first: Boolean!, $skipSecond: Boolean!) {
					value: greeting @include(if: $first)
					value: greeting @skip(if: $skipSecond)
				}
			`, map[string]interface{}{
				"first":      tc.first,
				"skipSecond": tc.skipSecond,
			}, "")
			assertNoGraphQLErrors(t, result)
			data := graphQLResultDataMap(t, result)
			_, exists := data["value"]
			if exists != tc.wantField {
				t.Fatalf("field exists = %v, want %v; data=%#v", exists, tc.wantField, data)
			}
		})
	}
}

func TestGraphQLGoNative_FragmentDirectivesMergeSubselections(t *testing.T) {
	var resolverCalls atomic.Int32
	profileType := NewObject(ObjectConfig{
		Name: "NativeMergedProfile",
		Fields: Fields{
			"id":   &Field{Type: NewNonNull(ID)},
			"name": &Field{Type: String},
			"role": &Field{Type: String},
		},
	})
	queryType := NewObject(ObjectConfig{
		Name: "Query",
		Fields: Fields{
			"viewer": &Field{
				Type: profileType,
				Resolve: func(p ResolveParams) (interface{}, error) {
					resolverCalls.Add(1)
					return map[string]interface{}{"id": "1", "name": "Ada", "role": "ADMIN"}, nil
				},
			},
		},
	})
	schema, err := NewSchema(SchemaConfig{Query: queryType})
	if err != nil {
		t.Fatalf("NewSchema failed: %v", err)
	}

	query := `
		query Merge($named: Boolean!, $inline: Boolean!) {
			profile: viewer { id }
			...ViewerName @include(if: $named)
			... on Query @include(if: $inline) {
				profile: viewer { role }
			}
		}
		fragment ViewerName on Query {
			profile: viewer { name }
		}
	`
	tests := []struct {
		name   string
		named  bool
		inline bool
		want   map[string]interface{}
	}{
		{name: "direct_only", want: map[string]interface{}{"id": "1"}},
		{name: "named_fragment", named: true, want: map[string]interface{}{"id": "1", "name": "Ada"}},
		{name: "inline_fragment", inline: true, want: map[string]interface{}{"id": "1", "role": "ADMIN"}},
		{name: "both_fragments", named: true, inline: true, want: map[string]interface{}{"id": "1", "name": "Ada", "role": "ADMIN"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resolverCalls.Store(0)
			result := executeGraphQLGoSpec(t, schema, query, map[string]interface{}{
				"named":  tc.named,
				"inline": tc.inline,
			}, "Merge")
			assertNoGraphQLErrors(t, result)
			assertGraphQLData(t, result.Data, map[string]interface{}{"profile": tc.want})
			if resolverCalls.Load() != 1 {
				t.Fatalf("merged response field resolver calls = %d, want 1", resolverCalls.Load())
			}
		})
	}
}

func TestGraphQLGoNative_ArgumentAndVariableDefaultPrecedence(t *testing.T) {
	queryType := NewObject(ObjectConfig{
		Name: "Query",
		Fields: Fields{
			"inspect": &Field{
				Type: String,
				Args: FieldConfigArgument{
					"value": &ArgumentConfig{Type: String, DefaultValue: "argument-default"},
				},
				Resolve: func(p ResolveParams) (interface{}, error) {
					value, exists := p.Args["value"]
					if !exists {
						return "missing", nil
					}
					if value == nil {
						return "null", nil
					}
					return value.(string), nil
				},
			},
		},
	})
	schema, err := NewSchema(SchemaConfig{Query: queryType})
	if err != nil {
		t.Fatalf("NewSchema failed: %v", err)
	}

	tests := []struct {
		name      string
		query     string
		variables map[string]interface{}
		want      string
	}{
		{name: "argument_omitted", query: `{ inspect }`, want: "argument-default"},
		{name: "literal_null", query: `{ inspect(value: null) }`, want: "null"},
		{name: "variable_default", query: `query($v: String = "variable-default") { inspect(value: $v) }`, want: "variable-default"},
		{name: "omitted_variable_uses_argument_default", query: `query($v: String) { inspect(value: $v) }`, variables: map[string]interface{}{}, want: "argument-default"},
		{name: "explicit_null_bypasses_defaults", query: `query($v: String = "variable-default") { inspect(value: $v) }`, variables: map[string]interface{}{"v": nil}, want: "null"},
		{name: "runtime_value_wins", query: `query($v: String = "variable-default") { inspect(value: $v) }`, variables: map[string]interface{}{"v": "runtime"}, want: "runtime"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := executeGraphQLGoSpecRequest(t, schema, tc.query, tc.variables, "")
			assertNoGraphQLErrors(t, result)
			assertGraphQLData(t, result.Data, map[string]interface{}{"inspect": tc.want})
		})
	}
}

func TestGraphQLGoNative_VariableCoercionRequestErrors(t *testing.T) {
	schema := newSpecConformanceSchema(t)
	query := `query($input: EchoInput!, $tags: [String!]!) { echoInput(input: $input, tags: $tags) }`
	tests := []struct {
		name      string
		variables map[string]interface{}
	}{
		{name: "missing_required_variable", variables: map[string]interface{}{"tags": []interface{}{"x"}}},
		{name: "null_required_variable", variables: map[string]interface{}{"input": nil, "tags": []interface{}{"x"}}},
		{name: "wrong_scalar_type", variables: map[string]interface{}{"input": map[string]interface{}{"message": 3}, "tags": []interface{}{"x"}}},
		{name: "unknown_input_field", variables: map[string]interface{}{"input": map[string]interface{}{"message": "x", "extra": true}, "tags": []interface{}{"x"}}},
		{name: "null_non_null_list_item", variables: map[string]interface{}{"input": map[string]interface{}{"message": "x"}, "tags": []interface{}{"x", nil}}},
		{name: "invalid_enum_value", variables: map[string]interface{}{"input": map[string]interface{}{"message": "x", "mode": "MISSING"}, "tags": []interface{}{"x"}}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := executeGraphQLGoSpecRequest(t, schema, query, tc.variables, "")
			if len(result.Errors) == 0 {
				t.Fatalf("expected request error, got %#v", result)
			}
			if result.Data != nil {
				t.Fatalf("request error must not execute data, got %#v", result.Data)
			}
		})
	}
}

func TestGraphQLGoNative_BuiltInScalarVariableCoercion(t *testing.T) {
	queryType := NewObject(ObjectConfig{
		Name: "Query",
		Fields: Fields{
			"intValue": &Field{
				Type:    Int,
				Args:    FieldConfigArgument{"value": &ArgumentConfig{Type: NewNonNull(Int)}},
				Resolve: func(p ResolveParams) (interface{}, error) { return p.Args["value"], nil },
			},
			"floatValue": &Field{
				Type:    Float,
				Args:    FieldConfigArgument{"value": &ArgumentConfig{Type: NewNonNull(Float)}},
				Resolve: func(p ResolveParams) (interface{}, error) { return p.Args["value"], nil },
			},
			"booleanValue": &Field{
				Type:    Boolean,
				Args:    FieldConfigArgument{"value": &ArgumentConfig{Type: NewNonNull(Boolean)}},
				Resolve: func(p ResolveParams) (interface{}, error) { return p.Args["value"], nil },
			},
			"idValue": &Field{
				Type:    ID,
				Args:    FieldConfigArgument{"value": &ArgumentConfig{Type: NewNonNull(ID)}},
				Resolve: func(p ResolveParams) (interface{}, error) { return p.Args["value"], nil },
			},
			"stringValue": &Field{
				Type:    String,
				Args:    FieldConfigArgument{"value": &ArgumentConfig{Type: NewNonNull(String)}},
				Resolve: func(p ResolveParams) (interface{}, error) { return p.Args["value"], nil },
			},
			"intList": &Field{
				Type:    NewList(NewNonNull(Int)),
				Args:    FieldConfigArgument{"value": &ArgumentConfig{Type: NewNonNull(NewList(NewNonNull(Int)))}},
				Resolve: func(p ResolveParams) (interface{}, error) { return p.Args["value"], nil },
			},
			"intMatrix": &Field{
				Type: NewList(NewNonNull(NewList(NewNonNull(Int)))),
				Args: FieldConfigArgument{
					"value": &ArgumentConfig{Type: NewNonNull(NewList(NewNonNull(NewList(NewNonNull(Int)))))},
				},
				Resolve: func(p ResolveParams) (interface{}, error) { return p.Args["value"], nil },
			},
		},
	})
	schema, err := NewSchema(SchemaConfig{Query: queryType})
	if err != nil {
		t.Fatalf("NewSchema failed: %v", err)
	}

	valid := executeGraphQLGoSpec(t, schema, `
		query($i: Int!, $f: Float!, $b: Boolean!, $id: ID!, $s: String!, $list: [Int!]!, $matrix: [[Int!]!]!) {
			intValue(value: $i)
			floatValue(value: $f)
			booleanValue(value: $b)
			idValue(value: $id)
			stringValue(value: $s)
			intList(value: $list)
			intMatrix(value: $matrix)
		}
	`, map[string]interface{}{
		"i":      int64(-2147483648),
		"f":      7,
		"b":      true,
		"id":     42,
		"s":      "text",
		"list":   9,
		"matrix": 1,
	}, "")
	assertNoGraphQLErrors(t, valid)
	assertGraphQLData(t, valid.Data, map[string]interface{}{
		"intValue":     -2147483648,
		"floatValue":   float64(7),
		"booleanValue": true,
		"idValue":      "42",
		"stringValue":  "text",
		"intList":      []interface{}{9},
		"intMatrix":    []interface{}{[]interface{}{1}},
	})

	invalid := []struct {
		name      string
		typeName  string
		fieldName string
		value     interface{}
	}{
		{name: "int_rejects_boolean", typeName: "Int", fieldName: "intValue", value: true},
		{name: "int_rejects_numeric_string", typeName: "Int", fieldName: "intValue", value: "7"},
		{name: "int_rejects_fraction", typeName: "Int", fieldName: "intValue", value: 1.5},
		{name: "int_rejects_out_of_range", typeName: "Int", fieldName: "intValue", value: int64(2147483648)},
		{name: "float_rejects_numeric_string", typeName: "Float", fieldName: "floatValue", value: "1.5"},
		{name: "boolean_rejects_integer", typeName: "Boolean", fieldName: "booleanValue", value: 1},
		{name: "string_rejects_integer", typeName: "String", fieldName: "stringValue", value: 1},
		{name: "id_rejects_boolean", typeName: "ID", fieldName: "idValue", value: true},
	}
	for _, tc := range invalid {
		t.Run(tc.name, func(t *testing.T) {
			query := fmt.Sprintf("query($value: %s!) { %s(value: $value) }", tc.typeName, tc.fieldName)
			result := executeGraphQLGoSpecRequest(t, schema, query, map[string]interface{}{"value": tc.value}, "")
			if len(result.Errors) == 0 || result.Data != nil {
				t.Fatalf("invalid %s variable must be rejected before execution, got %#v", tc.typeName, result)
			}
		})
	}
}

func TestGraphQLGoNative_InputObjectOmissionAndExplicitNullAreDistinct(t *testing.T) {
	schema := newSpecConformanceSchema(t)
	query := `query($input: EchoInput!, $tags: [String!]!) { echoInput(input: $input, tags: $tags) }`
	tests := []struct {
		name  string
		input map[string]interface{}
		want  string
	}{
		{
			name:  "omitted_field_uses_schema_default",
			input: map[string]interface{}{"message": "omitted"},
			want:  "omitted|x|7|A",
		},
		{
			name:  "explicit_null_does_not_use_schema_default",
			input: map[string]interface{}{"message": "explicit", "count": nil},
			want:  "explicit|x|<nil>|A",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := executeGraphQLGoSpecRequest(t, schema, query, map[string]interface{}{
				"input": tc.input,
				"tags":  []interface{}{"x"},
				"extra": "unused variables are ignored",
			}, "")
			assertNoGraphQLErrors(t, result)
			assertGraphQLData(t, result.Data, map[string]interface{}{"echoInput": tc.want})
		})
	}
}

func TestGraphQLGoNative_OperationSelectionMatrix(t *testing.T) {
	schema := newSpecConformanceSchema(t)
	document := `query First { greeting } query Second { alias: greeting }`

	tests := []struct {
		name          string
		operationName string
		wantError     bool
		wantData      map[string]interface{}
	}{
		{name: "select_named_operation", operationName: "Second", wantData: map[string]interface{}{"alias": "hello"}},
		{name: "missing_operation_name", wantError: true},
		{name: "unknown_operation_name", operationName: "Missing", wantError: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := executeGraphQLGoSpecRequest(t, schema, document, nil, tc.operationName)
			if tc.wantError {
				if len(result.Errors) == 0 || result.Data != nil {
					t.Fatalf("expected request error without data, got %#v", result)
				}
				return
			}
			assertNoGraphQLErrors(t, result)
			assertGraphQLData(t, result.Data, tc.wantData)
		})
	}
}

func TestGraphQLGoNative_ValidOperationAndVariableUsageCounterparts(t *testing.T) {
	queryType := NewObject(ObjectConfig{
		Name: "Query",
		Fields: Fields{
			"echo": &Field{
				Type: String,
				Args: FieldConfigArgument{
					"value":  &ArgumentConfig{Type: NewNonNull(String), DefaultValue: "location-default"},
					"suffix": &ArgumentConfig{Type: String, DefaultValue: "!"},
				},
				Resolve: func(p ResolveParams) (interface{}, error) {
					return p.Args["value"].(string) + p.Args["suffix"].(string), nil
				},
			},
		},
	})
	schema, err := NewSchema(SchemaConfig{Query: queryType})
	if err != nil {
		t.Fatalf("NewSchema failed: %v", err)
	}

	tests := []struct {
		name         string
		query        string
		variables    map[string]interface{}
		responseName string
		want         string
	}{
		{
			name:  "single_anonymous_operation",
			query: `{ echo(value: "anonymous") }`,
			want:  "anonymous!",
		},
		{
			name:  "nullable_variable_with_non_null_variable_default",
			query: `query($value: String = "variable-default") { echo(value: $value) }`,
			want:  "variable-default!",
		},
		{
			name:  "nullable_variable_at_location_with_default",
			query: `query($value: String) { echo(value: $value) }`,
			want:  "location-default!",
		},
		{
			name: "variable_used_only_by_fragment",
			query: `
				query($value: String!) { ...EchoFragment }
				fragment EchoFragment on Query { echo(value: $value) }
			`,
			variables: map[string]interface{}{"value": "fragment"},
			want:      "fragment!",
		},
		{
			name:         "equivalent_arguments_in_different_order",
			responseName: "merged",
			query: `{
				merged: echo(value: "same", suffix: "?")
				merged: echo(suffix: "?", value: "same")
			}`,
			want: "same?",
		},
		{
			name:  "inline_fragment_without_type_condition",
			query: `{ ... @include(if: true) { echo(value: "inline") } }`,
			want:  "inline!",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := executeGraphQLGoSpec(t, schema, tc.query, tc.variables, "")
			assertNoGraphQLErrors(t, result)
			responseName := tc.responseName
			if responseName == "" {
				responseName = "echo"
			}
			assertGraphQLData(t, result.Data, map[string]interface{}{responseName: tc.want})
		})
	}

	withName := executeGraphQLGoSpecRequest(t, schema, `{ echo(value: "x") }`, nil, "Named")
	if len(withName.Errors) == 0 || withName.Data != nil {
		t.Fatalf("operationName cannot select an anonymous operation, got %#v", withName)
	}
}

func TestGraphQLGoNative_RepeatedFieldExecutesResolverOnce(t *testing.T) {
	var calls atomic.Int32
	queryType := NewObject(ObjectConfig{
		Name: "Query",
		Fields: Fields{
			"value": &Field{
				Type: String,
				Resolve: func(p ResolveParams) (interface{}, error) {
					calls.Add(1)
					return "ok", nil
				},
			},
		},
	})
	schema, err := NewSchema(SchemaConfig{Query: queryType})
	if err != nil {
		t.Fatalf("NewSchema failed: %v", err)
	}

	result := executeGraphQLGoSpec(t, schema, `
		{ value ...A }
		fragment A on Query { value ...B }
		fragment B on Query { value }
	`, nil, "")
	assertNoGraphQLErrors(t, result)
	assertGraphQLData(t, result.Data, map[string]interface{}{"value": "ok"})
	if calls.Load() != 1 {
		t.Fatalf("resolver calls = %d, want 1", calls.Load())
	}
}

func TestGraphQLGoNative_RepeatedFailingFieldProducesOneError(t *testing.T) {
	var calls atomic.Int32
	queryType := NewObject(ObjectConfig{
		Name: "Query",
		Fields: Fields{
			"failure": &Field{
				Type: String,
				Resolve: func(p ResolveParams) (interface{}, error) {
					calls.Add(1)
					return nil, errors.New("failed once")
				},
			},
		},
	})
	schema, err := NewSchema(SchemaConfig{Query: queryType})
	if err != nil {
		t.Fatalf("NewSchema failed: %v", err)
	}

	result := executeGraphQLGoSpec(t, schema, `{
		value: failure
		...FailureFragment
		value: failure @include(if: true)
	}
	fragment FailureFragment on Query { value: failure }
	`, nil, "")
	assertGraphQLData(t, result.Data, map[string]interface{}{"value": nil})
	if calls.Load() != 1 || len(result.Errors) != 1 {
		t.Fatalf("repeated failing response position calls/errors = %d/%d, want 1/1: %#v", calls.Load(), len(result.Errors), result.Errors)
	}
	assertGraphQLErrorPaths(t, result.Errors, []interface{}{"value"})
}

func TestGraphQLGoNative_MutationContinuesSeriallyAfterNullableError(t *testing.T) {
	var mu sync.Mutex
	order := make([]string, 0, 3)
	queryType := NewObject(ObjectConfig{
		Name:   "Query",
		Fields: Fields{"noop": &Field{Type: String}},
	})
	mutationType := NewObject(ObjectConfig{
		Name: "Mutation",
		Fields: Fields{
			"record": &Field{
				Type: String,
				Args: FieldConfigArgument{"value": &ArgumentConfig{Type: NewNonNull(String)}},
				Resolve: func(p ResolveParams) (interface{}, error) {
					value := p.Args["value"].(string)
					mu.Lock()
					order = append(order, value)
					mu.Unlock()
					if value == "B" {
						return nil, errors.New("B failed")
					}
					return value, nil
				},
			},
		},
	})
	schema, err := NewSchema(SchemaConfig{Query: queryType, Mutation: mutationType})
	if err != nil {
		t.Fatalf("NewSchema failed: %v", err)
	}

	result := executeGraphQLGoSpec(t, schema, `mutation {
		first: record(value: "A")
		second: record(value: "B")
		third: record(value: "C")
	}`, nil, "")
	assertGraphQLData(t, result.Data, map[string]interface{}{"first": "A", "second": nil, "third": "C"})
	if len(result.Errors) != 1 {
		t.Fatalf("mutation errors = %d, want 1: %#v", len(result.Errors), result.Errors)
	}
	assertGraphQLErrorPaths(t, result.Errors, []interface{}{"second"})
	mu.Lock()
	defer mu.Unlock()
	if !reflect.DeepEqual(order, []string{"A", "B", "C"}) {
		t.Fatalf("mutation execution order = %#v, want [A B C]", order)
	}
}

func TestGraphQLGoNative_DefaultResolverBranches(t *testing.T) {
	profileType := NewObject(ObjectConfig{
		Name: "NativeDefaultProfile",
		Fields: Fields{
			"name": &Field{Type: String},
			"age":  &Field{Type: Int},
		},
	})
	resolverType := NewObject(ObjectConfig{
		Name: "NativeFieldResolver",
		Fields: Fields{
			"value": &Field{Type: String},
		},
	})
	queryType := NewObject(ObjectConfig{
		Name: "Query",
		Fields: Fields{
			"mapValue": &Field{Type: String},
			"mapFunc":  &Field{Type: String},
			"profile": &Field{
				Type: profileType,
				Resolve: func(p ResolveParams) (interface{}, error) {
					return &nativeTaggedProfile{DisplayName: "Ada", Age: 37}, nil
				},
			},
			"custom": &Field{
				Type: resolverType,
				Resolve: func(p ResolveParams) (interface{}, error) {
					return nativeFieldResolverSource{}, nil
				},
			},
		},
	})
	schema, err := NewSchema(SchemaConfig{Query: queryType})
	if err != nil {
		t.Fatalf("NewSchema failed: %v", err)
	}
	root := map[string]interface{}{
		"mapValue": "map",
		"mapFunc": func() interface{} {
			return "function"
		},
	}

	result := executeGraphQLGoSpecWithRoot(t, schema, `{ mapValue mapFunc profile { name age } custom { value } }`, root)
	assertNoGraphQLErrors(t, result)
	assertGraphQLData(t, result.Data, map[string]interface{}{
		"mapValue": "map",
		"mapFunc":  "function",
		"profile":  map[string]interface{}{"name": "Ada", "age": 37},
		"custom":   map[string]interface{}{"value": "resolved:value"},
	})
}

func TestGraphQLGoNative_QueryRootFieldsCanExecuteConcurrently(t *testing.T) {
	started := make(chan string, 2)
	release := make(chan struct{})
	resolver := func(name string) FieldResolveFn {
		return func(p ResolveParams) (interface{}, error) {
			started <- name
			<-release
			return name, nil
		}
	}
	queryType := NewObject(ObjectConfig{
		Name: "Query",
		Fields: Fields{
			"first":  &Field{Type: String, Resolve: resolver("first")},
			"second": &Field{Type: String, Resolve: resolver("second")},
		},
	})
	schema, err := NewSchema(SchemaConfig{Query: queryType})
	if err != nil {
		t.Fatalf("NewSchema failed: %v", err)
	}

	document := parseGraphQLSpecQuery(t, `{ first second }`)
	done := make(chan *Result, 1)
	go func() {
		done <- Execute(ExecuteParams{
			Schema:  schema,
			AST:     document,
			Context: context.Background(),
		})
	}()

	seen := map[string]bool{}
	for len(seen) < 2 {
		select {
		case name := <-started:
			seen[name] = true
		case <-time.After(time.Second):
			close(release)
			t.Fatalf("query fields did not start concurrently; started=%#v", seen)
		}
	}
	close(release)
	result := <-done
	assertNoGraphQLErrors(t, result)
	assertGraphQLData(t, result.Data, map[string]interface{}{"first": "first", "second": "second"})
}

func TestGraphQLGoNative_ContextPropagatesToEveryResolver(t *testing.T) {
	queryType := NewObject(ObjectConfig{
		Name: "Query",
		Fields: Fields{
			"contextValue": &Field{
				Type: String,
				Resolve: func(p ResolveParams) (interface{}, error) {
					value, _ := p.Context.Value(nativeContextKey{}).(string)
					return value, nil
				},
			},
		},
	})
	schema, err := NewSchema(SchemaConfig{Query: queryType})
	if err != nil {
		t.Fatalf("NewSchema failed: %v", err)
	}
	ctx := context.WithValue(context.Background(), nativeContextKey{}, "request-value")

	result := Do(Params{Schema: schema, RequestString: `{ contextValue }`, Context: ctx})
	assertNoGraphQLErrors(t, result)
	assertGraphQLData(t, result.Data, map[string]interface{}{"contextValue": "request-value"})
}

func TestGraphQLGoNative_ResolveInfoContainsExecutionMetadata(t *testing.T) {
	var captured ResolveInfo
	childType := NewObject(ObjectConfig{
		Name: "NativeResolveInfoChild",
		Fields: Fields{
			"value": &Field{
				Type: String,
				Args: FieldConfigArgument{
					"suffix":      &ArgumentConfig{Type: String},
					"sourceValue": &ArgumentConfig{Type: String},
				},
				Resolve: func(p ResolveParams) (interface{}, error) {
					captured = p.Info
					// 保留原生 Source 能力，同时允许 SGraph 通过显式参数依赖传入父字段结果。
					sourceValue, _ := p.Args["sourceValue"].(string)
					if source, ok := p.Source.(map[string]interface{}); ok {
						sourceValue, _ = source["value"].(string)
					}
					return sourceValue + p.Args["suffix"].(string), nil
				},
			},
		},
	})
	queryType := NewObject(ObjectConfig{
		Name: "Query",
		Fields: Fields{
			"child": &Field{
				Type: childType,
				Resolve: func(p ResolveParams) (interface{}, error) {
					return map[string]interface{}{"value": "A"}, nil
				},
			},
		},
	})
	schema, err := NewSchema(SchemaConfig{Query: queryType})
	if err != nil {
		t.Fatalf("NewSchema failed: %v", err)
	}

	query := `
		query Metadata($suffix: String!) {
			child {
				alias: value(suffix: $suffix)
			}
		}
	`
	result := executeSGraphSpecWithParamRegistry(t, schema, query, map[string]interface{}{"suffix": "B"}, "Metadata", []FieldParamBinding{
		newSGraphFieldResponseBinding([]string{"child", "alias"}, "NativeResolveInfoChild", "value", "sourceValue", []string{"child"}, "Query", "child", "value"),
	})
	assertNoGraphQLErrors(t, result)
	assertGraphQLData(t, result.Data, map[string]interface{}{"child": map[string]interface{}{"alias": "AB"}})
	if captured.FieldName != "value" || captured.ParentType.Name() != "NativeResolveInfoChild" {
		t.Fatalf("unexpected field metadata: field=%q parent=%v", captured.FieldName, captured.ParentType)
	}
	path := captured.Path.AsArray()
	if len(path) != 2 || path[0] != "child" || path[1] != "alias" {
		t.Fatalf("path = %#v, want [child alias]", path)
	}
	operation, ok := captured.Operation.(*ast.OperationDefinition)
	if len(captured.FieldASTs) != 1 || !ok || operation.Name.Value != "Metadata" {
		t.Fatalf("unexpected AST/operation metadata: %#v", captured)
	}
	if captured.VariableValues["suffix"] != "B" || captured.RootValue != nil {
		t.Fatalf("unexpected variable/root metadata: variables=%#v root=%#v", captured.VariableValues, captured.RootValue)
	}
}

func TestGraphQLGoNative_AbstractRuntimeTypeContextAndInvalidResults(t *testing.T) {
	var resolveTypeSawContext atomic.Bool
	var isTypeOfSawContext atomic.Bool

	catType := NewObject(ObjectConfig{
		Name: "NativeCat",
		Fields: Fields{
			"name": &Field{Type: String},
		},
		IsTypeOf: func(p IsTypeOfParams) bool {
			if p.Context.Value(nativeContextKey{}) == "abstract-context" {
				isTypeOfSawContext.Store(true)
			}
			value, _ := p.Value.(map[string]interface{})
			return value["kind"] == "cat"
		},
	})
	dogType := NewObject(ObjectConfig{
		Name: "NativeDog",
		Fields: Fields{
			"name": &Field{Type: String},
		},
	})
	petType := NewUnion(UnionConfig{
		Name:  "NativePet",
		Types: []*Object{catType},
		ResolveType: func(p ResolveTypeParams) *Object {
			if p.Context.Value(nativeContextKey{}) == "abstract-context" && p.Info.FieldName != "" {
				resolveTypeSawContext.Store(true)
			}
			value, _ := p.Value.(map[string]interface{})
			if value["kind"] == "cat" {
				return catType
			}
			return dogType
		},
	})
	queryType := NewObject(ObjectConfig{
		Name: "Query",
		Fields: Fields{
			"pet": &Field{
				Type: petType,
				Resolve: func(p ResolveParams) (interface{}, error) {
					return map[string]interface{}{"kind": "cat", "name": "Milo"}, nil
				},
			},
			"invalidPet": &Field{
				Type: petType,
				Resolve: func(p ResolveParams) (interface{}, error) {
					return map[string]interface{}{"kind": "dog", "name": "Rex"}, nil
				},
			},
			"invalidCat": &Field{
				Type: catType,
				Resolve: func(p ResolveParams) (interface{}, error) {
					return map[string]interface{}{"kind": "dog", "name": "Rex"}, nil
				},
			},
		},
	})
	schema, err := NewSchema(SchemaConfig{
		Query: queryType,
		Types: []Type{catType, dogType, petType},
	})
	if err != nil {
		t.Fatalf("NewSchema failed: %v", err)
	}
	document := parseGraphQLSpecQuery(t, `{
		pet { __typename ... on NativeCat { name } }
		invalidPet { __typename }
		invalidCat { name }
	}`)
	validationResult := ValidateDocument(&schema, document, nil)
	if !validationResult.IsValid {
		t.Fatalf("validation failed: %#v", validationResult.Errors)
	}

	result := Execute(ExecuteParams{
		Schema:  schema,
		AST:     document,
		Context: context.WithValue(context.Background(), nativeContextKey{}, "abstract-context"),
	})
	assertGraphQLData(t, result.Data, map[string]interface{}{
		"pet":        map[string]interface{}{"__typename": "NativeCat", "name": "Milo"},
		"invalidPet": nil,
		"invalidCat": nil,
	})
	if len(result.Errors) != 2 {
		t.Fatalf("abstract/object runtime checks returned %d errors, want 2: %#v", len(result.Errors), result.Errors)
	}
	assertGraphQLErrorPaths(t, result.Errors, []interface{}{"invalidPet"})
	assertGraphQLErrorPaths(t, result.Errors, []interface{}{"invalidCat"})
	if !resolveTypeSawContext.Load() || !isTypeOfSawContext.Load() {
		t.Fatalf("runtime type callbacks did not receive request context: ResolveType=%v IsTypeOf=%v", resolveTypeSawContext.Load(), isTypeOfSawContext.Load())
	}
}

func TestGraphQLGoNative_CustomScalarLiteralVariableAndResultCoercion(t *testing.T) {
	odd := NewScalar(ScalarConfig{
		Name: "Odd",
		Serialize: func(value interface{}) interface{} {
			integer, ok := value.(int)
			if !ok || integer%2 == 0 {
				return nil
			}
			return integer
		},
		ParseValue: func(value interface{}) interface{} {
			integer, ok := value.(int)
			if !ok || integer%2 == 0 {
				return nil
			}
			return integer
		},
		ParseLiteral: func(valueAST ast.Value) interface{} {
			value, ok := valueAST.(*ast.IntValue)
			if !ok {
				return nil
			}
			integer, err := strconv.Atoi(value.Value)
			if err != nil || integer%2 == 0 {
				return nil
			}
			return integer
		},
	})
	queryType := NewObject(ObjectConfig{
		Name: "Query",
		Fields: Fields{
			"odd": &Field{
				Type:    odd,
				Args:    FieldConfigArgument{"value": &ArgumentConfig{Type: NewNonNull(odd)}},
				Resolve: func(p ResolveParams) (interface{}, error) { return p.Args["value"], nil },
			},
		},
	})
	schema, err := NewSchema(SchemaConfig{Query: queryType, Types: []Type{odd}})
	if err != nil {
		t.Fatalf("NewSchema failed: %v", err)
	}

	literal := executeGraphQLGoSpec(t, schema, `{ odd(value: 3) }`, nil, "")
	assertNoGraphQLErrors(t, literal)
	assertGraphQLData(t, literal.Data, map[string]interface{}{"odd": 3})

	variable := executeGraphQLGoSpec(t, schema, `query($value: Odd!) { odd(value: $value) }`, map[string]interface{}{"value": 5}, "")
	assertNoGraphQLErrors(t, variable)
	assertGraphQLData(t, variable.Data, map[string]interface{}{"odd": 5})

	invalid := executeGraphQLGoSpecRequest(t, schema, `{ odd(value: 2) }`, nil, "")
	if len(invalid.Errors) == 0 || invalid.Data != nil {
		t.Fatalf("invalid custom scalar literal must be a request error, got %#v", invalid)
	}
}

func TestGraphQLGoNative_SubscriptionMapsEachEventThroughExecutor(t *testing.T) {
	events := make(chan interface{}, 2)
	events <- map[string]interface{}{"counter": 1}
	events <- map[string]interface{}{"counter": 2}
	close(events)

	queryType := NewObject(ObjectConfig{
		Name:   "Query",
		Fields: Fields{"noop": &Field{Type: String}},
	})
	subscriptionType := NewObject(ObjectConfig{
		Name: "Subscription",
		Fields: Fields{
			"counter": &Field{
				Type: Int,
				Subscribe: func(p ResolveParams) (interface{}, error) {
					return events, nil
				},
			},
		},
	})
	schema, err := NewSchema(SchemaConfig{Query: queryType, Subscription: subscriptionType})
	if err != nil {
		t.Fatalf("NewSchema failed: %v", err)
	}

	results := Subscribe(Params{
		Schema:        schema,
		RequestString: `subscription Counter { counter }`,
		OperationName: "Counter",
		Context:       context.Background(),
	})
	want := 1
	for result := range results {
		assertNoGraphQLErrors(t, result)
		assertGraphQLData(t, result.Data, map[string]interface{}{"counter": want})
		want++
	}
	if want != 3 {
		t.Fatalf("subscription event count = %d, want 2", want-1)
	}
}

func TestGraphQLGoNative_ErrorPathAliasAndExtensions(t *testing.T) {
	queryType := NewObject(ObjectConfig{
		Name: "Query",
		Fields: Fields{
			"failure": &Field{
				Type: String,
				Resolve: func(p ResolveParams) (interface{}, error) {
					return nil, nativeExtendedError{message: "failed"}
				},
			},
		},
	})
	schema, err := NewSchema(SchemaConfig{Query: queryType})
	if err != nil {
		t.Fatalf("NewSchema failed: %v", err)
	}

	result := executeGraphQLGoSpec(t, schema, `{ aliasFailure: failure }`, nil, "")
	assertGraphQLData(t, result.Data, map[string]interface{}{"aliasFailure": nil})
	assertGraphQLErrorPaths(t, result.Errors, []interface{}{"aliasFailure"})
	if got := result.Errors[0].Extensions["code"]; got != "NATIVE_TEST" {
		t.Fatalf("error extension code = %#v, want NATIVE_TEST", got)
	}
}

func TestGraphQLGoNative_MultipleSiblingErrorsKeepPartialData(t *testing.T) {
	queryType := NewObject(ObjectConfig{
		Name: "Query",
		Fields: Fields{
			"ok":   &Field{Type: String, Resolve: func(p ResolveParams) (interface{}, error) { return "ok", nil }},
			"badA": &Field{Type: String, Resolve: func(p ResolveParams) (interface{}, error) { return nil, errors.New("A") }},
			"badB": &Field{Type: String, Resolve: func(p ResolveParams) (interface{}, error) { return nil, errors.New("B") }},
		},
	})
	schema, err := NewSchema(SchemaConfig{Query: queryType})
	if err != nil {
		t.Fatalf("NewSchema failed: %v", err)
	}

	result := executeGraphQLGoSpec(t, schema, `{ ok badA badB }`, nil, "")
	assertGraphQLData(t, result.Data, map[string]interface{}{"ok": "ok", "badA": nil, "badB": nil})
	if len(result.Errors) != 2 {
		t.Fatalf("errors = %#v, want two field errors", result.Errors)
	}
	assertGraphQLErrorPaths(t, result.Errors, []interface{}{"badA"})
	assertGraphQLErrorPaths(t, result.Errors, []interface{}{"badB"})
}

func TestGraphQLGoNative_NestedListErrorsKeepOccurrencePaths(t *testing.T) {
	itemType := NewObject(ObjectConfig{
		Name: "NativeNestedErrorItem",
		Fields: Fields{
			"name": &Field{
				Type: String,
				Args: FieldConfigArgument{
					"sourceBroken": &ArgumentConfig{Type: Boolean},
					"sourceName":   &ArgumentConfig{Type: String},
				},
				Resolve: func(p ResolveParams) (interface{}, error) {
					// SGraph 不依赖 Source；父元素属性通过 ParamRegistry 显式进入 resolver 参数。
					sourceBroken, _ := p.Args["sourceBroken"].(bool)
					sourceName := p.Args["sourceName"]
					if item, ok := p.Source.(map[string]interface{}); ok {
						sourceBroken, _ = item["broken"].(bool)
						sourceName = item["name"]
					}
					if sourceBroken {
						return nil, errors.New("name unavailable")
					}
					return sourceName, nil
				},
			},
		},
	})
	queryType := NewObject(ObjectConfig{
		Name: "Query",
		Fields: Fields{
			"groups": &Field{
				Type: NewList(NewList(itemType)),
				Resolve: func(p ResolveParams) (interface{}, error) {
					return [][]map[string]interface{}{
						{{"name": "A"}, {"broken": true}},
						{{"broken": true}, {"name": "D"}},
					}, nil
				},
			},
		},
	})
	schema, err := NewSchema(SchemaConfig{Query: queryType})
	if err != nil {
		t.Fatalf("NewSchema failed: %v", err)
	}

	query := `{ groups { name } }`
	result := executeSGraphSpecWithParamRegistry(t, schema, query, nil, "", []FieldParamBinding{
		newSGraphFieldResponseBinding([]string{"groups", "name"}, "NativeNestedErrorItem", "name", "sourceBroken", []string{"groups"}, "Query", "groups", "broken"),
		newSGraphFieldResponseBinding([]string{"groups", "name"}, "NativeNestedErrorItem", "name", "sourceName", []string{"groups"}, "Query", "groups", "name"),
	})
	assertGraphQLData(t, result.Data, map[string]interface{}{
		"groups": []interface{}{
			[]interface{}{map[string]interface{}{"name": "A"}, map[string]interface{}{"name": nil}},
			[]interface{}{map[string]interface{}{"name": nil}, map[string]interface{}{"name": "D"}},
		},
	})
	if len(result.Errors) != 2 {
		t.Fatalf("nested list errors = %d, want 2: %#v", len(result.Errors), result.Errors)
	}
	assertGraphQLErrorPaths(t, result.Errors, []interface{}{"groups", 0, 1, "name"})
	assertGraphQLErrorPaths(t, result.Errors, []interface{}{"groups", 1, 0, "name"})
	for _, fieldErr := range result.Errors {
		if len(fieldErr.Locations) == 0 {
			t.Fatalf("nested list field error is missing source location: %#v", fieldErr)
		}
	}
}

func TestGraphQLGoNative_TypedListArrayAndPointerCompletion(t *testing.T) {
	values := []string{"A", "B"}
	queryType := NewObject(ObjectConfig{
		Name: "Query",
		Fields: Fields{
			"slice": &Field{Type: NewList(NewNonNull(String)), Resolve: func(p ResolveParams) (interface{}, error) {
				return []string{"A", "B"}, nil
			}},
			"array": &Field{Type: NewList(NewNonNull(Int)), Resolve: func(p ResolveParams) (interface{}, error) {
				return [3]int{1, 2, 3}, nil
			}},
			"pointer": &Field{Type: NewList(NewNonNull(String)), Resolve: func(p ResolveParams) (interface{}, error) {
				return &values, nil
			}},
		},
	})
	schema, err := NewSchema(SchemaConfig{Query: queryType})
	if err != nil {
		t.Fatalf("NewSchema failed: %v", err)
	}

	result := executeGraphQLGoSpec(t, schema, `{ slice array pointer }`, nil, "")
	assertNoGraphQLErrors(t, result)
	assertGraphQLData(t, result.Data, map[string]interface{}{
		"slice":   []interface{}{"A", "B"},
		"array":   []interface{}{1, 2, 3},
		"pointer": []interface{}{"A", "B"},
	})
}

func TestGraphQLGoNative_ListWrapperAndTypedNilCompletion(t *testing.T) {
	var nilSlicePointer *[]string
	queryType := NewObject(ObjectConfig{
		Name: "Query",
		Fields: Fields{
			"nonNullNullableItems": &Field{
				Type:    NewNonNull(NewList(String)),
				Resolve: func(p ResolveParams) (interface{}, error) { return []interface{}{"A", nil}, nil },
			},
			"nonNullNonNullItems": &Field{
				Type:    NewNonNull(NewList(NewNonNull(String))),
				Resolve: func(p ResolveParams) (interface{}, error) { return []string{}, nil },
			},
			"nested": &Field{
				Type: NewList(NewList(NewNonNull(String))),
				Resolve: func(p ResolveParams) (interface{}, error) {
					return []interface{}{[]string{"A"}, nil, []string{}}, nil
				},
			},
			"typedNil": &Field{
				Type:    NewList(String),
				Resolve: func(p ResolveParams) (interface{}, error) { return nilSlicePointer, nil },
			},
		},
	})
	schema, err := NewSchema(SchemaConfig{Query: queryType})
	if err != nil {
		t.Fatalf("NewSchema failed: %v", err)
	}

	result := executeGraphQLGoSpec(t, schema, `{ nonNullNullableItems nonNullNonNullItems nested typedNil }`, nil, "")
	assertNoGraphQLErrors(t, result)
	assertGraphQLData(t, result.Data, map[string]interface{}{
		"nonNullNullableItems": []interface{}{"A", nil},
		"nonNullNonNullItems":  []interface{}{},
		"nested": []interface{}{
			[]interface{}{"A"},
			nil,
			[]interface{}{},
		},
		"typedNil": nil,
	})
}

func TestGraphQLGoNative_InvalidListResultBecomesFieldError(t *testing.T) {
	queryType := NewObject(ObjectConfig{
		Name: "Query",
		Fields: Fields{
			"values": &Field{Type: NewList(String), Resolve: func(p ResolveParams) (interface{}, error) {
				return "not-a-list", nil
			}},
		},
	})
	schema, err := NewSchema(SchemaConfig{Query: queryType})
	if err != nil {
		t.Fatalf("NewSchema failed: %v", err)
	}

	result := executeGraphQLGoSpec(t, schema, `{ values }`, nil, "")
	assertGraphQLData(t, result.Data, map[string]interface{}{"values": nil})
	assertGraphQLErrorPaths(t, result.Errors, []interface{}{"values"})
}

func TestGraphQLGoNative_LargeBreadthAndListBoundaries(t *testing.T) {
	const fieldCount = 256
	const itemCount = 1024
	items := make([]int, itemCount)
	for i := range items {
		items[i] = i
	}
	queryType := NewObject(ObjectConfig{
		Name: "Query",
		Fields: Fields{
			"value": &Field{Type: Int, Resolve: func(p ResolveParams) (interface{}, error) { return 1, nil }},
			"items": &Field{Type: NewList(NewNonNull(Int)), Resolve: func(p ResolveParams) (interface{}, error) { return items, nil }},
		},
	})
	schema, err := NewSchema(SchemaConfig{Query: queryType})
	if err != nil {
		t.Fatalf("NewSchema failed: %v", err)
	}

	var query strings.Builder
	query.WriteByte('{')
	for i := 0; i < fieldCount; i++ {
		fmt.Fprintf(&query, " f%d: value", i)
	}
	query.WriteString(" items }")
	result := executeGraphQLGoSpec(t, schema, query.String(), nil, "")
	assertNoGraphQLErrors(t, result)
	data := graphQLResultDataMap(t, result)
	if len(data) != fieldCount+1 {
		t.Fatalf("response fields = %d, want %d", len(data), fieldCount+1)
	}
	if got := len(data["items"].([]interface{})); got != itemCount {
		t.Fatalf("list items = %d, want %d", got, itemCount)
	}
}

func TestGraphQLGoNative_EmptyAndDeepNestedBoundaries(t *testing.T) {
	const depth = 32
	var nodeType *Object
	nodeType = NewObject(ObjectConfig{
		Name: "NativeDeepNode",
		Fields: FieldsThunk(func() Fields {
			return Fields{
				"value": &Field{Type: String},
				"next":  &Field{Type: nodeType},
			}
		}),
	})
	deepValue := map[string]interface{}{"value": "leaf"}
	for i := 0; i < depth; i++ {
		deepValue = map[string]interface{}{"next": deepValue}
	}
	queryType := NewObject(ObjectConfig{
		Name: "Query",
		Fields: Fields{
			"node": &Field{
				Type:    nodeType,
				Resolve: func(p ResolveParams) (interface{}, error) { return deepValue, nil },
			},
			"empty": &Field{
				Type:    NewNonNull(NewList(NewNonNull(Int))),
				Resolve: func(p ResolveParams) (interface{}, error) { return []int{}, nil },
			},
		},
	})
	schema, err := NewSchema(SchemaConfig{Query: queryType})
	if err != nil {
		t.Fatalf("NewSchema failed: %v", err)
	}

	var query strings.Builder
	query.WriteString("{ empty node {")
	for i := 0; i < depth; i++ {
		query.WriteString(" next {")
	}
	query.WriteString(" value")
	for i := 0; i < depth; i++ {
		query.WriteString(" }")
	}
	query.WriteString(" } }")

	result := executeGraphQLGoSpec(t, schema, query.String(), nil, "")
	assertNoGraphQLErrors(t, result)
	data := graphQLResultDataMap(t, result)
	if empty, ok := data["empty"].([]interface{}); !ok || len(empty) != 0 {
		t.Fatalf("empty non-null list = %#v, want []", data["empty"])
	}
	current, ok := data["node"].(map[string]interface{})
	if !ok {
		t.Fatalf("deep node result type = %T", data["node"])
	}
	for i := 0; i < depth; i++ {
		current, ok = current["next"].(map[string]interface{})
		if !ok {
			t.Fatalf("deep node is missing level %d: %#v", i+1, current)
		}
	}
	if current["value"] != "leaf" {
		t.Fatalf("deep leaf = %#v, want leaf", current["value"])
	}
}

func TestGraphQLGoNative_ConcurrentRequestsDoNotLeakVariables(t *testing.T) {
	queryType := NewObject(ObjectConfig{
		Name: "Query",
		Fields: Fields{
			"echo": &Field{
				Type:    String,
				Args:    FieldConfigArgument{"value": &ArgumentConfig{Type: NewNonNull(String)}},
				Resolve: func(p ResolveParams) (interface{}, error) { return p.Args["value"], nil },
			},
		},
	})
	schema, err := NewSchema(SchemaConfig{Query: queryType})
	if err != nil {
		t.Fatalf("NewSchema failed: %v", err)
	}
	document := parseGraphQLSpecQuery(t, `query($value: String!) { echo(value: $value) }`)

	const requestCount = 64
	var wg sync.WaitGroup
	errorsChannel := make(chan error, requestCount)
	for i := 0; i < requestCount; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			want := fmt.Sprintf("value-%d", index)
			result := Execute(ExecuteParams{
				Schema:  schema,
				AST:     document,
				Args:    map[string]interface{}{"value": want},
				Context: context.Background(),
			})
			if len(result.Errors) != 0 {
				errorsChannel <- fmt.Errorf("request %d errors: %#v", index, result.Errors)
				return
			}
			data, ok := toPlainValue(result.Data).(map[string]interface{})
			if !ok || data["echo"] != want {
				errorsChannel <- fmt.Errorf("request %d data: %#v, want %q", index, result.Data, want)
			}
		}(i)
	}
	wg.Wait()
	close(errorsChannel)
	for err := range errorsChannel {
		t.Error(err)
	}
}

func TestGraphQLGoNative_FieldHooksRespectSelectionAndMetaFields(t *testing.T) {
	extension := &nativeCountingExtension{}
	queryType := NewObject(ObjectConfig{
		Name: "Query",
		Fields: Fields{
			"greeting": &Field{
				Type:    String,
				Resolve: func(p ResolveParams) (interface{}, error) { return "hello", nil },
			},
			"failure": &Field{
				Type:    String,
				Resolve: func(p ResolveParams) (interface{}, error) { return nil, errors.New("failed") },
			},
		},
	})
	schema, err := NewSchema(SchemaConfig{Query: queryType, Extensions: []Extension{extension}})
	if err != nil {
		t.Fatalf("NewSchema failed: %v", err)
	}

	result := executeGraphQLGoSpec(t, schema, `{
		greeting
		omitted: greeting @skip(if: true)
		rootType: __typename
		failure
	}`, nil, "")
	assertGraphQLData(t, result.Data, map[string]interface{}{
		"greeting": "hello",
		"rootType": "Query",
		"failure":  nil,
	})
	if len(result.Errors) != 1 {
		t.Fatalf("field errors = %d, want 1: %#v", len(result.Errors), result.Errors)
	}
	if extension.fieldStarts.Load() != 3 || extension.fieldFinishes.Load() != 3 {
		t.Fatalf("field hooks = %d/%d, want 3/3", extension.fieldStarts.Load(), extension.fieldFinishes.Load())
	}
	if extension.fieldErrors.Load() != 1 {
		t.Fatalf("field hook errors = %d, want 1", extension.fieldErrors.Load())
	}
}

func TestGraphQLGoNative_IntrospectionBoundaryValues(t *testing.T) {
	schema := newSpecConformanceSchema(t)
	result := executeGraphQLGoSpec(t, schema, `{
		rootType: __typename
		unknown: __type(name: "MissingType") { name }
		schemaInfo: __schema {
			queryType { name }
			mutationType { name }
			subscriptionType { name }
		}
		inputInfo: __type(name: "EchoInput") {
			inputFields { name defaultValue }
		}
	}`, nil, "")
	assertNoGraphQLErrors(t, result)
	data := graphQLResultDataMap(t, result)
	if data["rootType"] != "Query" || data["unknown"] != nil {
		t.Fatalf("introspection root/unknown type mismatch: %#v", data)
	}
	schemaInfo := data["schemaInfo"].(map[string]interface{})
	if schemaInfo["queryType"].(map[string]interface{})["name"] != "Query" || schemaInfo["mutationType"] != nil || schemaInfo["subscriptionType"] != nil {
		t.Fatalf("query-only schema roots mismatch: %#v", schemaInfo)
	}
	inputFields := data["inputInfo"].(map[string]interface{})["inputFields"].([]interface{})
	defaults := make(map[string]interface{}, len(inputFields))
	for _, item := range inputFields {
		field := item.(map[string]interface{})
		defaults[field["name"].(string)] = field["defaultValue"]
	}
	if defaults["message"] != nil || defaults["count"] != "7" || defaults["mode"] != "A" {
		t.Fatalf("input defaultValue formatting mismatch: %#v", defaults)
	}
}

func TestGraphQLGoStrict_NullLiteralIsValidInputValue(t *testing.T) {
	queryType := NewObject(ObjectConfig{
		Name: "Query",
		Fields: Fields{
			"inspect": &Field{
				Type: String,
				Args: FieldConfigArgument{"value": &ArgumentConfig{Type: String}},
				Resolve: func(p ResolveParams) (interface{}, error) {
					if p.Args["value"] == nil {
						return "null", nil
					}
					return "not-null", nil
				},
			},
		},
	})
	schema, err := NewSchema(SchemaConfig{Query: queryType})
	if err != nil {
		t.Fatalf("NewSchema failed: %v", err)
	}

	result := executeGraphQLGoSpecRequest(t, schema, `{ inspect(value: null) }`, nil, "")
	assertNoGraphQLErrors(t, result)
	assertGraphQLData(t, result.Data, map[string]interface{}{"inspect": "null"})
}

func TestGraphQLGoStrict_UniqueDirectivesPerLocation(t *testing.T) {
	schema := newSpecConformanceSchema(t)
	errs := validateGraphQLSpecQuery(t, schema, `{ greeting @skip(if: false) @skip(if: false) }`)
	if len(errs) == 0 {
		t.Fatal("duplicate non-repeatable directive at one location must be rejected")
	}
}

func TestGraphQLGoStrict_SubscriptionMustHaveSingleNonIntrospectionRootField(t *testing.T) {
	queryType := NewObject(ObjectConfig{
		Name:   "Query",
		Fields: Fields{"noop": &Field{Type: String}},
	})
	subscriptionType := NewObject(ObjectConfig{
		Name: "Subscription",
		Fields: Fields{
			"first":  &Field{Type: String},
			"second": &Field{Type: String},
		},
	})
	schema, err := NewSchema(SchemaConfig{Query: queryType, Subscription: subscriptionType})
	if err != nil {
		t.Fatalf("NewSchema failed: %v", err)
	}

	tests := []struct {
		name  string
		query string
	}{
		{name: "multiple_root_fields", query: `subscription { first second }`},
		{name: "aliased_duplicate_root_fields", query: `subscription { a: first b: first }`},
		{name: "introspection_root_field", query: `subscription { __typename }`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			errs := validateGraphQLSpecQuery(t, schema, tc.query)
			if len(errs) == 0 {
				t.Fatalf("subscription validation should reject %s", tc.name)
			}
		})
	}
}

func TestGraphQLGoStrict_InvalidLeafSerializationProducesFieldError(t *testing.T) {
	invalidScalar := NewScalar(ScalarConfig{
		Name: "NativeInvalidScalar",
		Serialize: func(value interface{}) interface{} {
			return nil
		},
	})
	invalidEnum := NewEnum(EnumConfig{
		Name: "NativeEnum",
		Values: EnumValueConfigMap{
			"VALID": &EnumValueConfig{Value: "VALID"},
		},
	})
	queryType := NewObject(ObjectConfig{
		Name: "Query",
		Fields: Fields{
			"scalar": &Field{Type: invalidScalar, Resolve: func(p ResolveParams) (interface{}, error) { return "bad", nil }},
			"enum":   &Field{Type: invalidEnum, Resolve: func(p ResolveParams) (interface{}, error) { return "MISSING", nil }},
		},
	})
	schema, err := NewSchema(SchemaConfig{Query: queryType, Types: []Type{invalidScalar, invalidEnum}})
	if err != nil {
		t.Fatalf("NewSchema failed: %v", err)
	}

	result := executeGraphQLGoSpec(t, schema, `{ scalar enum }`, nil, "")
	assertGraphQLData(t, result.Data, map[string]interface{}{"scalar": nil, "enum": nil})
	if len(result.Errors) != 2 {
		t.Fatalf("invalid scalar and enum serialization must produce two field errors, got %#v", result.Errors)
	}
	assertGraphQLErrorPaths(t, result.Errors, []interface{}{"scalar"})
	assertGraphQLErrorPaths(t, result.Errors, []interface{}{"enum"})
}

func TestGraphQLGoStrict_RequestErrorResponseOmitsDataMember(t *testing.T) {
	schema := newSpecConformanceSchema(t)
	result := executeGraphQLGoSpecRequest(t, schema, `{ missingField }`, nil, "")
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("json.Marshal failed: %v", err)
	}
	var response map[string]interface{}
	if err := json.Unmarshal(encoded, &response); err != nil {
		t.Fatalf("json.Unmarshal failed: %v", err)
	}
	if _, exists := response["data"]; exists {
		t.Fatalf("request error response must omit data; json=%s", encoded)
	}
}

func TestGraphQLGoStrict_ResponseSerializationPreservesQueryFieldOrder(t *testing.T) {
	schema := newSpecConformanceSchema(t)
	result := executeGraphQLGoSpec(t, schema, `{ z: greeting a: greeting m: greeting }`, nil, "")
	assertNoGraphQLErrors(t, result)
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("json.Marshal failed: %v", err)
	}
	response := string(encoded)
	z := strings.Index(response, `"z"`)
	a := strings.Index(response, `"a"`)
	m := strings.Index(response, `"m"`)
	if z < 0 || a < 0 || m < 0 || !(z < a && a < m) {
		t.Fatalf("response field order must follow query order z,a,m; json=%s", response)
	}
}

func TestGraphQLGoStrict_September2025IntrospectionFields(t *testing.T) {
	schema := newSpecConformanceSchema(t)
	tests := []struct {
		name  string
		query string
	}{
		{name: "schema_description", query: `{ __schema { description } }`},
		{name: "type_specifiedByURL", query: `{ __type(name: "String") { specifiedByURL } }`},
		{name: "type_isOneOf", query: `{ __type(name: "EchoInput") { isOneOf } }`},
		{name: "directive_isRepeatable", query: `{ __schema { directives { name isRepeatable } } }`},
		{name: "input_value_deprecation", query: `{ __type(name: "EchoInput") { inputFields(includeDeprecated: true) { name isDeprecated deprecationReason } } }`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := executeGraphQLGoSpecRequest(t, schema, tc.query, nil, "")
			assertNoGraphQLErrors(t, result)
			if result.Data == nil {
				t.Fatal("introspection query returned nil data")
			}
		})
	}
}

func TestGraphQLGoStrict_September2025ExecutableDescriptions(t *testing.T) {
	schema := newSpecConformanceSchema(t)
	tests := []struct {
		name  string
		query string
	}{
		{
			name: "variable_description",
			query: `
				query Described(
					"Controls whether the field is included."
					$include: Boolean!
				) {
					greeting @include(if: $include)
				}
			`,
		},
		{
			name: "fragment_description",
			query: `
				query Described { ...GreetingFields }
				"The greeting fields used by Described."
				fragment GreetingFields on Query { greeting }
			`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := executeGraphQLGoSpecRequest(t, schema, tc.query, map[string]interface{}{"include": true}, "Described")
			assertNoGraphQLErrors(t, result)
		})
	}
}

func TestGraphQLGoStrict_September2025SpecifiedDirectives(t *testing.T) {
	schema := newSpecConformanceSchema(t)
	result := executeGraphQLGoSpec(t, schema, `{ __schema { directives { name locations } } }`, nil, "")
	assertNoGraphQLErrors(t, result)

	directives := graphQLResultDataMap(t, result)["__schema"].(map[string]interface{})["directives"].([]interface{})
	locationsByName := make(map[string]map[string]bool, len(directives))
	for _, item := range directives {
		directive := item.(map[string]interface{})
		locations := make(map[string]bool)
		for _, location := range directive["locations"].([]interface{}) {
			locations[location.(string)] = true
		}
		locationsByName[directive["name"].(string)] = locations
	}
	required := map[string][]string{
		"skip":        {DirectiveLocationField, DirectiveLocationFragmentSpread, DirectiveLocationInlineFragment},
		"include":     {DirectiveLocationField, DirectiveLocationFragmentSpread, DirectiveLocationInlineFragment},
		"deprecated":  {DirectiveLocationFieldDefinition, DirectiveLocationArgumentDefinition, DirectiveLocationInputFieldDefinition, DirectiveLocationEnumValue},
		"specifiedBy": {DirectiveLocationScalar},
		"oneOf":       {DirectiveLocationInputObject},
	}
	for name, requiredLocations := range required {
		locations, exists := locationsByName[name]
		if !exists {
			t.Errorf("specified directive %q is missing: %#v", name, directives)
			continue
		}
		for _, location := range requiredLocations {
			if !locations[location] {
				t.Errorf("specified directive %q is missing location %q: %#v", name, location, locations)
			}
		}
	}
}

func TestGraphQLGoStrict_VariableDefinitionDirectives(t *testing.T) {
	variableDirective := NewDirective(DirectiveConfig{
		Name:      "variableTag",
		Locations: []string{"VARIABLE_DEFINITION"},
	})
	queryType := NewObject(ObjectConfig{
		Name: "Query",
		Fields: Fields{
			"echo": &Field{
				Type:    String,
				Args:    FieldConfigArgument{"value": &ArgumentConfig{Type: NewNonNull(String)}},
				Resolve: func(p ResolveParams) (interface{}, error) { return p.Args["value"], nil },
			},
		},
	})
	directives := append([]*Directive{}, SpecifiedDirectives...)
	directives = append(directives, variableDirective)
	schema, err := NewSchema(SchemaConfig{Query: queryType, Directives: directives})
	if err != nil {
		t.Fatalf("NewSchema failed: %v", err)
	}

	result := executeGraphQLGoSpecRequest(t, schema, `
		query($value: String! @variableTag) {
			echo(value: $value)
		}
	`, map[string]interface{}{"value": "ok"}, "")
	assertNoGraphQLErrors(t, result)
	assertGraphQLData(t, result.Data, map[string]interface{}{"echo": "ok"})
}

func TestGraphQLGoStrict_NumericLiteralBoundaries(t *testing.T) {
	valid := []string{
		`{ echo(text: "0") }`,
		`query($v: Int = -2147483648) { greeting }`,
		`query($v: Int = 2147483647) { greeting }`,
		`query($v: Float = 1e+10) { greeting }`,
		`query($v: Float = -0.01) { greeting }`,
	}
	for _, query := range valid {
		if _, err := parseGraphQLSpecQueryResult(query); err != nil {
			t.Errorf("valid numeric document failed to parse: %s: %v", query, err)
		}
	}

	invalid := []string{
		`query($v: Int = 01) { greeting }`,
		`query($v: Float = 1.) { greeting }`,
		`query($v: Float = .1) { greeting }`,
		`query($v: Float = 1e) { greeting }`,
		`query($v: Int = +1) { greeting }`,
	}
	for _, query := range invalid {
		if _, err := parseGraphQLSpecQueryResult(query); err == nil {
			t.Errorf("invalid numeric document parsed successfully: %s", query)
		}
	}
}

func TestGraphQLGoNative_SchemaConstructionBoundaries(t *testing.T) {
	tests := []struct {
		name  string
		build func() error
	}{
		{
			name: "query_root_required",
			build: func() error {
				_, err := NewSchema(SchemaConfig{})
				return err
			},
		},
		{
			name: "invalid_field_name",
			build: func() error {
				query := NewObject(ObjectConfig{Name: "Query", Fields: Fields{"bad-name": &Field{Type: String}}})
				_, err := NewSchema(SchemaConfig{Query: query})
				return err
			},
		},
		{
			name: "duplicate_type_name",
			build: func() error {
				first := NewObject(ObjectConfig{Name: "Duplicate", Fields: Fields{"a": &Field{Type: String}}})
				second := NewObject(ObjectConfig{Name: "Duplicate", Fields: Fields{"b": &Field{Type: String}}})
				query := NewObject(ObjectConfig{Name: "Query", Fields: Fields{"value": &Field{Type: first}}})
				_, err := NewSchema(SchemaConfig{Query: query, Types: []Type{second}})
				return err
			},
		},
		{
			name: "empty_enum",
			build: func() error {
				empty := NewEnum(EnumConfig{Name: "Empty", Values: EnumValueConfigMap{}})
				query := NewObject(ObjectConfig{Name: "Query", Fields: Fields{"value": &Field{Type: empty}}})
				_, err := NewSchema(SchemaConfig{Query: query})
				return err
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.build(); err == nil {
				t.Fatalf("expected schema construction error for %s", tc.name)
			}
		})
	}
}

func executeGraphQLGoSpecWithRoot(t *testing.T, schema Schema, query string, root interface{}) *Result {
	t.Helper()
	document := parseGraphQLSpecQuery(t, query)
	validationResult := ValidateDocument(&schema, document, nil)
	if !validationResult.IsValid {
		t.Fatalf("validation failed: %#v", validationResult.Errors)
	}
	return Execute(ExecuteParams{
		Schema:  schema,
		Root:    root,
		AST:     document,
		Context: context.Background(),
	})
}
