package graphql

// spec2025_response_test.go
//
// GraphQL September 2025 §7 Response。

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/graphql-go/graphql/gqlerrors"
)

func s25respCore(t *testing.T) Schema {
	t.Helper()
	schema, _ := s25NewCoreSchema(t, nil)
	return schema
}

// ---------------------------------------------------------------------------
// §7.1.1 Execution Result
// ---------------------------------------------------------------------------

func TestSpec2025_Response_ExecutionResultShape(t *testing.T) {
	t.Run("successful_request_carries_data", func(t *testing.T) {
		result := s25Do(t, s25Request{Schema: s25respCore(t), Query: `{ greeting }`})
		s25RequireNoErrors(t, result)
		top := s25MarshalResult(t, result)
		if _, present := top["data"]; !present {
			t.Fatalf("a successful execution result must contain data; got keys %v", s25respKeys(top))
		}
		if _, present := top["errors"]; present {
			t.Errorf("a result with no errors must not contain an errors entry; got %s", top["errors"])
		}
	})

	t.Run("field_error_keeps_data_and_adds_errors", func(t *testing.T) {
		result := s25Do(t, s25Request{Schema: s25respCore(t), Query: `{ greeting boom }`})
		if len(result.Errors) == 0 {
			t.Fatalf("expected a field error for boom")
		}
		data := s25DataMap(t, result)
		if data["greeting"] != "hello" {
			t.Errorf("sibling data must be preserved; greeting = %v", data["greeting"])
		}
		if value, present := data["boom"]; !present || value != nil {
			t.Errorf("a failed nullable field must be present and null; got present=%v value=%v", present, value)
		}
	})

	t.Run("errors_entry_is_never_an_empty_list", func(t *testing.T) {
		// §7.1.6: "If present, the errors entry ... must contain at least one error".
		result := s25Do(t, s25Request{Schema: s25respCore(t), Query: `{ greeting }`})
		top := s25MarshalResult(t, result)
		raw, present := top["errors"]
		if !present {
			return
		}
		var errs []any
		if err := json.Unmarshal(raw, &errs); err != nil {
			t.Fatalf("errors entry is not a list: %s", raw)
		}
		if len(errs) == 0 {
			t.Errorf("errors entry must not be an empty list")
		}
	})
}

func s25respKeys(top map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(top))
	for key := range top {
		keys = append(keys, key)
	}
	return keys
}

// ---------------------------------------------------------------------------
// §7.1.3 Request Error Result
// ---------------------------------------------------------------------------

func TestSpec2025_Response_RequestErrorResultOmitsData(t *testing.T) {
	// §7.1.3: "the data entry must not be present in the result."
	cases := map[string]s25Request{
		"parse_error":            {Query: `{ greeting`},
		"validation_error":       {Query: `{ notAField }`},
		"unknown_operation_name": {Query: `query A { greeting }`, OperationName: "B"},
		"variable_coercion":      {Query: `query Q($v: String!) { echoNoDefault(value: $v) }`},
	}
	for name, template := range cases {
		t.Run(name, func(t *testing.T) {
			request := template
			request.Schema = s25respCore(t)
			result := s25Do(t, request)
			if len(result.Errors) == 0 {
				t.Fatalf("expected a request error")
			}
			if result.Data != nil {
				t.Errorf("request error result must not produce data; got %#v", s25Plain(result.Data))
			}
			top := s25MarshalResult(t, result)
			if _, present := top["data"]; present {
				t.Errorf("serialized request error result must omit the data entry entirely; got %s", top["data"])
			}
		})
	}
}

// ---------------------------------------------------------------------------
// §7.1.4 Response Position
// ---------------------------------------------------------------------------

func TestSpec2025_Response_PositionFollowsQueryOrder(t *testing.T) {
	t.Run("root_fields_keep_query_order", func(t *testing.T) {
		result := s25Do(t, s25Request{Schema: s25respCore(t), Query: `{ z a m }`})
		s25RequireNoErrors(t, result)
		s25RequireKeyOrder(t, s25MarshalData(t, result), "z", "a", "m")
	})

	t.Run("aliases_keep_query_order", func(t *testing.T) {
		result := s25Do(t, s25Request{Schema: s25respCore(t), Query: `{ zz: z aa: a mm: m }`})
		s25RequireNoErrors(t, result)
		s25RequireKeyOrder(t, s25MarshalData(t, result), "zz", "aa", "mm")
	})

	t.Run("nested_fields_keep_query_order", func(t *testing.T) {
		result := s25Do(t, s25Request{Schema: s25respCore(t), Query: `{ user { name id } }`})
		s25RequireNoErrors(t, result)
		s25RequireKeyOrder(t, s25MarshalData(t, result), "name", "id")
	})

	t.Run("first_occurrence_determines_position_even_when_skipped", func(t *testing.T) {
		// §6.3.2 CollectFields 按首次出现建立 response key 的顺序；
		// 运行期被 @skip 掉的那次出现不改变该 key 的位置。
		result := s25Do(t, s25Request{
			Schema: s25respCore(t),
			Query:  `{ a m z @skip(if: true) z }`,
		})
		s25RequireNoErrors(t, result)
		encoded := s25MarshalData(t, result)
		if !strings.Contains(encoded, `"z"`) {
			t.Fatalf("z must be present because its second occurrence is not skipped: %s", encoded)
		}
		s25RequireKeyOrder(t, encoded, "a", "m", "z")
	})

	t.Run("fragment_expansion_keeps_first_occurrence_position", func(t *testing.T) {
		result := s25Do(t, s25Request{
			Schema: s25respCore(t),
			Query: `
				{ z ...F m }
				fragment F on Query { a }
			`,
		})
		s25RequireNoErrors(t, result)
		s25RequireKeyOrder(t, s25MarshalData(t, result), "z", "a", "m")
	})
}

// ---------------------------------------------------------------------------
// §7.1.5 Data
// ---------------------------------------------------------------------------

func TestSpec2025_Response_DataEntry(t *testing.T) {
	// 原生链路在 non-null 字段冒泡时会跨 goroutine panic 并终止整个测试进程，
	// 因此本用例在子进程中隔离执行；子进程失败仍按真实失败上报。
	if !s25Isolated(t, "response-data-entry") {
		return
	}
	t.Run("data_is_null_when_a_non_null_root_field_errors", func(t *testing.T) {
		result := s25Do(t, s25Request{Schema: s25respCore(t), Query: `{ greeting nonNullNull }`})
		if len(result.Errors) == 0 {
			t.Fatalf("expected an error for the non-null root field")
		}
		if result.Data != nil {
			t.Errorf("data must be null when a non-null root field bubbles; got %#v", s25Plain(result.Data))
		}
		top := s25MarshalResult(t, result)
		raw, present := top["data"]
		if !present {
			t.Fatalf("a field error result must still carry a data entry (null), got keys %v", s25respKeys(top))
		}
		if string(raw) != "null" {
			t.Errorf("data entry = %s, want null", raw)
		}
	})

	t.Run("data_is_a_map_for_a_successful_request", func(t *testing.T) {
		result := s25Do(t, s25Request{Schema: s25respCore(t), Query: `{ greeting }`})
		s25RequireNoErrors(t, result)
		if _, ok := s25Plain(result.Data).(map[string]any); !ok {
			t.Errorf("data must be a map, got %T", result.Data)
		}
	})
}

// ---------------------------------------------------------------------------
// §7.1.6 Errors
// ---------------------------------------------------------------------------

func TestSpec2025_Response_ErrorObjectShape(t *testing.T) {
	t.Run("message_locations_and_path", func(t *testing.T) {
		result := s25Do(t, s25Request{Schema: s25respCore(t), Query: `{ aliased: boom }`})
		s25RequireErrorCount(t, result, 1)
		err := result.Errors[0]
		if err.Message == "" {
			t.Errorf("every error must carry a non-empty message")
		}
		if len(err.Locations) == 0 {
			t.Errorf("an execution error must carry locations")
		} else {
			if err.Locations[0].Line <= 0 || err.Locations[0].Column <= 0 {
				t.Errorf("location must be 1-indexed line/column, got %+v", err.Locations[0])
			}
		}
		// §7.1.6: path 使用 response name（别名），不是字段名。
		s25RequireHasErrorPath(t, result, []any{"aliased"})
	})

	t.Run("path_elements_are_strings_and_integers", func(t *testing.T) {
		schema, _ := s25NewCoreSchema(t, nil)
		result := s25Do(t, s25Request{
			Schema: schema,
			Query:  `{ people { failForBob } }`,
			Bindings: []FieldParamBinding{
				s25ParentBinding([]string{"people", "failForBob"}, "S25User", "failForBob",
					[]string{"people"}, "Query", "people"),
			},
		})
		s25RequireHasErrorPath(t, result, []any{"people", 1, "failForBob"})
		for _, err := range result.Errors {
			for index, key := range err.Path {
				switch key.(type) {
				case string:
				default:
					if _, ok := s25AsInt(key); !ok {
						t.Errorf("path element %d is %T (%v); only strings and integers are allowed", index, key, key)
					}
				}
			}
		}
	})

	t.Run("extensions_are_propagated", func(t *testing.T) {
		result := s25Do(t, s25Request{Schema: s25respCore(t), Query: `{ boomExtended }`})
		s25RequireErrorCount(t, result, 1)
		extensions := s25ErrorExtensions(result)
		if extensions == nil {
			t.Fatalf("resolver error extensions were dropped; errors=%v", s25ErrorMessages(result))
		}
		if extensions["code"] != "S25_CODE" {
			t.Errorf("extensions.code = %v, want S25_CODE", extensions["code"])
		}
	})

	t.Run("serialized_error_object_keys", func(t *testing.T) {
		result := s25Do(t, s25Request{Schema: s25respCore(t), Query: `{ boom }`})
		top := s25MarshalResult(t, result)
		var errs []map[string]any
		if err := json.Unmarshal(top["errors"], &errs); err != nil {
			t.Fatalf("errors is not a list of maps: %s", top["errors"])
		}
		if len(errs) != 1 {
			t.Fatalf("expected 1 error, got %d", len(errs))
		}
		for key := range errs[0] {
			switch key {
			case "message", "locations", "path", "extensions":
			default:
				t.Errorf("error object contains a non-reserved top-level key %q; such entries must live under extensions", key)
			}
		}
		if _, present := errs[0]["message"]; !present {
			t.Errorf("serialized error must contain message")
		}
	})

	t.Run("multiple_sibling_errors_are_all_reported", func(t *testing.T) {
		result := s25Do(t, s25Request{Schema: s25respCore(t), Query: `{ first: boom second: boom greeting }`})
		s25RequireErrorPathSet(t, result, []any{"first"}, []any{"second"})
		data := s25DataMap(t, result)
		if data["greeting"] != "hello" {
			t.Errorf("partial data must be preserved; greeting = %v", data["greeting"])
		}
	})
}

// ---------------------------------------------------------------------------
// §7.1.7 Extensions / §7.1.8 Additional Entries
// ---------------------------------------------------------------------------

type s25respExtension struct {
	executionStarts int
	fieldStarts     int
}

func (e *s25respExtension) Init(ctx context.Context, p *Params) context.Context { return ctx }
func (e *s25respExtension) Name() string                                        { return "s25Response" }

func (e *s25respExtension) ParseDidStart(ctx context.Context) (context.Context, ParseFinishFunc) {
	return ctx, func(err error) {}
}

func (e *s25respExtension) ValidationDidStart(ctx context.Context) (context.Context, ValidationFinishFunc) {
	return ctx, func([]gqlerrors.FormattedError) {}
}

func (e *s25respExtension) ExecutionDidStart(ctx context.Context) (context.Context, ExecutionFinishFunc) {
	e.executionStarts++
	return ctx, func(*Result) {}
}

func (e *s25respExtension) ResolveFieldDidStart(ctx context.Context, i *ResolveInfo) (context.Context, ResolveFieldFinishFunc) {
	e.fieldStarts++
	return ctx, func(interface{}, error) {}
}

func (e *s25respExtension) HasResult() bool { return true }
func (e *s25respExtension) GetResult(ctx context.Context) interface{} {
	return map[string]int{"executionStarts": e.executionStarts, "fieldStarts": e.fieldStarts}
}

func TestSpec2025_Response_ExtensionsEntry(t *testing.T) {
	t.Run("extensions_entry_is_a_map_and_is_reported", func(t *testing.T) {
		extension := &s25respExtension{}
		schema, err := NewSchema(SchemaConfig{
			Query: NewObject(ObjectConfig{Name: "Query", Fields: Fields{
				"greeting": &Field{Type: String, Resolve: s25Const("hello")},
			}}),
			Extensions: []Extension{extension},
		})
		if err != nil {
			t.Fatalf("build schema: %v", err)
		}
		result := s25Do(t, s25Request{Schema: schema, Query: `{ greeting }`})
		s25RequireNoErrors(t, result)
		if result.Extensions == nil {
			t.Fatalf("result must expose the extensions entry")
		}
		if _, present := result.Extensions["s25Response"]; !present {
			t.Errorf("extensions entry is missing the s25Response key; got %v", result.Extensions)
		}
		if extension.executionStarts != 1 {
			t.Errorf("ExecutionDidStart called %d times, want 1", extension.executionStarts)
		}
		if extension.fieldStarts != 1 {
			t.Errorf("ResolveFieldDidStart called %d times, want 1", extension.fieldStarts)
		}
	})

	t.Run("top_level_entries_are_restricted", func(t *testing.T) {
		// §7.1.8: 顶层只允许 data / errors / extensions。
		result := s25Do(t, s25Request{Schema: s25respCore(t), Query: `{ greeting boom }`})
		top := s25MarshalResult(t, result)
		for key := range top {
			switch key {
			case "data", "errors", "extensions":
			default:
				t.Errorf("response contains a reserved-violating top-level entry %q", key)
			}
		}
	})
}

// ---------------------------------------------------------------------------
// §7.2 Serialization Format
// ---------------------------------------------------------------------------

func TestSpec2025_Response_JSONSerialization(t *testing.T) {
	t.Run("scalars_serialize_with_json_types", func(t *testing.T) {
		result := s25Do(t, s25Request{
			Schema: s25respCore(t),
			Query:  `{ idOut floatOut boolOut stringOut nullableScalar }`,
		})
		s25RequireNoErrors(t, result)
		encoded := s25MarshalData(t, result)
		for _, want := range []string{
			`"idOut":"42"`,   // §3.5.5 ID always serializes as a String
			`"floatOut":1.5`, // §3.5.2 Float serializes as a JSON number
			`"boolOut":true`, // §3.5.4
			`"stringOut":""`, // §3.5.3 empty string is a valid String
			`"nullableScalar":null`,
		} {
			if !strings.Contains(encoded, want) {
				t.Errorf("serialized data is missing %s; got %s", want, encoded)
			}
		}
	})

	t.Run("integers_serialize_without_a_fraction", func(t *testing.T) {
		schema := s25NewSchema(t, Fields{
			"count": &Field{Type: Int, Resolve: s25Const(7)},
		})
		result := s25Do(t, s25Request{Schema: schema, Query: `{ count }`})
		s25RequireNoErrors(t, result)
		encoded := s25MarshalData(t, result)
		if !strings.Contains(encoded, `"count":7`) {
			t.Errorf("Int must serialize as 7, got %s", encoded)
		}
	})

	t.Run("strings_are_escaped", func(t *testing.T) {
		schema := s25NewSchema(t, Fields{
			"text": &Field{Type: String, Resolve: s25Const("a\"b\\c\nd")},
		})
		result := s25Do(t, s25Request{Schema: schema, Query: `{ text }`})
		s25RequireNoErrors(t, result)
		encoded := s25MarshalData(t, result)
		if !strings.Contains(encoded, `"text":"a\"b\\c\nd"`) {
			t.Errorf("string escaping is wrong: %s", encoded)
		}
	})

	t.Run("lists_serialize_as_json_arrays_preserving_order", func(t *testing.T) {
		result := s25Do(t, s25Request{Schema: s25respCore(t), Query: `{ stringList }`})
		s25RequireNoErrors(t, result)
		encoded := s25MarshalData(t, result)
		if !strings.Contains(encoded, `"stringList":["a",null,"c"]`) {
			t.Errorf("list serialization = %s, want [\"a\",null,\"c\"]", encoded)
		}
	})
}

func TestSpec2025_Response_SerializedMapOrdering(t *testing.T) {
	// §7.2.2 Serialized Map Ordering: "the ordering of the keys in the serialized map
	// should be the same as the ordering of those keys in the request".
	t.Run("data_keys_follow_request_order", func(t *testing.T) {
		result := s25Do(t, s25Request{Schema: s25respCore(t), Query: `{ z a m }`})
		s25RequireNoErrors(t, result)
		encoded := s25MarshalData(t, result)
		s25RequireKeyOrder(t, encoded, "z", "a", "m")
	})

	t.Run("nested_map_keys_follow_request_order", func(t *testing.T) {
		result := s25Do(t, s25Request{Schema: s25respCore(t), Query: `{ user { name id } }`})
		s25RequireNoErrors(t, result)
		s25RequireKeyOrder(t, s25MarshalData(t, result), "user", "name", "id")
	})

	t.Run("list_item_map_keys_follow_request_order", func(t *testing.T) {
		result := s25Do(t, s25Request{Schema: s25respCore(t), Query: `{ people { name id } }`})
		s25RequireNoErrors(t, result)
		encoded := s25MarshalData(t, result)
		first := strings.Index(encoded, `"name"`)
		second := strings.Index(encoded, `"id"`)
		if first < 0 || second < 0 || first > second {
			t.Errorf("list item keys are not in request order: %s", encoded)
		}
	})
}
