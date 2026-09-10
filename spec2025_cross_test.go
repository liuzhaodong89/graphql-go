package graphql

// spec2025_cross_test.go
//
// 场景交叉矩阵：每个用例在一次请求中同时激活多个规范维度，
// 用于发现"单维度都对、组合起来错"的问题。

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/graphql-go/graphql/gqlerrors"
)

func s25xCore(t *testing.T) Schema {
	t.Helper()
	schema, _ := s25NewCoreSchema(t, nil)
	return schema
}

// ---------------------------------------------------------------------------
// X-01 @skip/@include × 字面量/变量 × 位置
// ---------------------------------------------------------------------------

func TestSpec2025_Cross_ConditionalDirectiveMatrix(t *testing.T) {
	// §3.13.1/§3.13.2: 字段被包含 <=> !skipIf && includeIf。
	type variant struct {
		name     string
		query    string
		vars     map[string]any
		included bool
	}
	variants := make([]variant, 0, 32)

	for _, skipIf := range []bool{false, true} {
		for _, includeIf := range []bool{false, true} {
			included := !skipIf && includeIf
			suffix := fmt.Sprintf("skip_%v_include_%v", skipIf, includeIf)

			variants = append(variants,
				variant{
					name:     "field_literal_" + suffix,
					query:    fmt.Sprintf(`{ greeting @skip(if: %v) @include(if: %v) }`, skipIf, includeIf),
					included: included,
				},
				variant{
					name:     "field_variable_" + suffix,
					query:    `query Q($s: Boolean!, $i: Boolean!) { greeting @skip(if: $s) @include(if: $i) }`,
					vars:     map[string]any{"s": skipIf, "i": includeIf},
					included: included,
				},
				variant{
					name:     "inline_fragment_literal_" + suffix,
					query:    fmt.Sprintf(`{ ... on Query @skip(if: %v) @include(if: %v) { greeting } }`, skipIf, includeIf),
					included: included,
				},
				variant{
					name:     "fragment_spread_literal_" + suffix,
					query:    fmt.Sprintf(`{ ...F @skip(if: %v) @include(if: %v) } fragment F on Query { greeting }`, skipIf, includeIf),
					included: included,
				},
				variant{
					name:     "fragment_spread_variable_" + suffix,
					query:    `query Q($s: Boolean!, $i: Boolean!) { ...F @skip(if: $s) @include(if: $i) } fragment F on Query { greeting }`,
					vars:     map[string]any{"s": skipIf, "i": includeIf},
					included: included,
				},
				variant{
					name:     "nested_field_literal_" + suffix,
					query:    fmt.Sprintf(`{ user { id name @skip(if: %v) @include(if: %v) } }`, skipIf, includeIf),
					included: included,
				},
			)
		}
	}

	for _, current := range variants {
		t.Run(current.name, func(t *testing.T) {
			result := s25Do(t, s25Request{Schema: s25xCore(t), Query: current.query, Variables: current.vars})
			s25RequireNoErrors(t, result)
			data := s25DataMap(t, result)
			container := data
			if nested, ok := data["user"].(map[string]any); ok {
				container = nested
			}
			key := "greeting"
			if container["id"] != nil {
				key = "name"
			}
			_, present := container[key]
			if present != current.included {
				t.Errorf("%s present = %v, want %v (data=%v)", key, present, current.included, data)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// X-02 同一 response name 多次出现 × 条件指令 OR 语义 × 别名
// ---------------------------------------------------------------------------

func TestSpec2025_Cross_RepeatedResponseNameWithConditions(t *testing.T) {
	// §6.3.2: CollectFields 逐次出现地判断 @skip/@include；
	// 只要有一次出现被包含，该 response key 就出现在结果中。
	cases := []struct {
		name     string
		query    string
		included bool
	}{
		{"first_included", `{ greeting @include(if: true) greeting @include(if: false) }`, true},
		{"second_included", `{ greeting @include(if: false) greeting @include(if: true) }`, true},
		{"both_excluded", `{ greeting @include(if: false) greeting @skip(if: true) }`, false},
		{"both_included", `{ greeting @include(if: true) greeting @skip(if: false) }`, true},
		{"across_fragments", `{ ...A ...B } fragment A on Query { greeting @include(if: false) } fragment B on Query { greeting @include(if: true) }`, true},
		// g 与 greeting 是两个不同的 response key；greeting 只有一次出现且被排除。
		{"alias_included_field_excluded", `{ g: greeting @include(if: true) greeting @include(if: false) }`, false},
	}
	for _, current := range cases {
		t.Run(current.name, func(t *testing.T) {
			result := s25Do(t, s25Request{Schema: s25xCore(t), Query: current.query})
			s25RequireNoErrors(t, result)
			data := s25DataMap(t, result)
			key := "greeting"
			if strings.HasPrefix(current.name, "alias_") {
				if _, present := data["g"]; !present {
					t.Errorf("aliased occurrence must be present, data=%v", data)
				}
			}
			if _, present := data[key]; present != current.included {
				t.Errorf("%s present = %v, want %v (data=%v)", key, present, current.included, data)
			}
		})
	}

	t.Run("merged_occurrences_call_the_resolver_once", func(t *testing.T) {
		counter := s25NewCounter()
		schema, _ := s25NewCoreSchema(t, counter)
		result := s25Do(t, s25Request{
			Schema: schema,
			Query:  `{ greeting @include(if: true) greeting @skip(if: false) ...F } fragment F on Query { greeting }`,
		})
		s25RequireNoErrors(t, result)
		if got := counter.Get("greeting"); got != 1 {
			t.Errorf("greeting resolver called %d times, want 1", got)
		}
	})
}

// ---------------------------------------------------------------------------
// X-03 抽象类型 × list × __typename × 内联片段 × 元素错误
// ---------------------------------------------------------------------------

func TestSpec2025_Cross_AbstractListAndFragments(t *testing.T) {
	t.Run("interface_list_with_typename_and_inline_fragments", func(t *testing.T) {
		result := s25Do(t, s25Request{
			Schema: s25xCore(t),
			Query: `{
				nodes {
					__typename
					id
					... on S25User { name }
					... on S25Robot { serial }
				}
			}`,
		})
		s25RequireNoErrors(t, result)
		items := s25DataMap(t, result)["nodes"].([]any)
		if len(items) != 2 {
			t.Fatalf("nodes length = %d, want 2", len(items))
		}
		first := items[0].(map[string]any)
		second := items[1].(map[string]any)
		if first["__typename"] != "S25User" || first["name"] != "Ada" {
			t.Errorf("nodes[0] = %v, want a S25User named Ada", first)
		}
		if _, present := first["serial"]; present {
			t.Errorf("nodes[0] must not carry the S25Robot-only field serial: %v", first)
		}
		if second["__typename"] != "S25Robot" || second["serial"] != "RX-2" {
			t.Errorf("nodes[1] = %v, want a S25Robot with serial RX-2", second)
		}
		if _, present := second["name"]; present {
			t.Errorf("nodes[1] must not carry the S25User-only field name: %v", second)
		}
	})

	t.Run("union_list_with_named_fragments_and_aliases", func(t *testing.T) {
		result := s25Do(t, s25Request{
			Schema: s25xCore(t),
			Query: `
				{ search { kind: __typename ...U ...R } }
				fragment U on S25User { label: name }
				fragment R on S25Robot { label: serial }
			`,
		})
		s25RequireNoErrors(t, result)
		items := s25DataMap(t, result)["search"].([]any)
		first := items[0].(map[string]any)
		second := items[1].(map[string]any)
		if first["kind"] != "S25User" || first["label"] != "Ada" {
			t.Errorf("search[0] = %v", first)
		}
		if second["kind"] != "S25Robot" || second["label"] != "RX-2" {
			t.Errorf("search[1] = %v", second)
		}
	})

	t.Run("invalid_runtime_type_becomes_a_field_error", func(t *testing.T) {
		result := s25Do(t, s25Request{Schema: s25xCore(t), Query: `{ badNode { __typename id } }`})
		if len(result.Errors) == 0 {
			t.Fatalf("an abstract value that resolves to a type outside the schema must produce a field error")
		}
		s25RequireHasErrorPath(t, result, []any{"badNode", 0})
	})

	t.Run("abstract_element_error_keeps_sibling_root_data", func(t *testing.T) {
		result := s25Do(t, s25Request{Schema: s25xCore(t), Query: `{ greeting badNode { id } }`})
		if len(result.Errors) == 0 {
			t.Fatalf("expected a field error for badNode")
		}
		data := s25DataMap(t, result)
		if data["greeting"] != "hello" {
			t.Errorf("sibling root field must keep its value; greeting = %v", data["greeting"])
		}
	})

	t.Run("skip_inside_an_inline_fragment_on_a_list_element", func(t *testing.T) {
		result := s25Do(t, s25Request{
			Schema: s25xCore(t),
			Query:  `{ nodes { id ... on S25User { name @skip(if: true) } } }`,
		})
		s25RequireNoErrors(t, result)
		items := s25DataMap(t, result)["nodes"].([]any)
		first := items[0].(map[string]any)
		if _, present := first["name"]; present {
			t.Errorf("@skip inside an inline fragment must remove the field; got %v", first)
		}
		if first["id"] != "u1" {
			t.Errorf("nodes[0].id = %v, want u1", first["id"])
		}
	})
}

// ---------------------------------------------------------------------------
// X-04 错误 × list 下标 × 别名 × 嵌套 × 兄弟保留
// ---------------------------------------------------------------------------

func TestSpec2025_Cross_ErrorPathsAcrossListsAndAliases(t *testing.T) {
	t.Run("list_element_error_uses_index_and_keeps_siblings", func(t *testing.T) {
		schema, _ := s25NewCoreSchema(t, nil)
		result := s25Do(t, s25Request{
			Schema: schema,
			Query:  `{ people { id failForBob } }`,
			Bindings: []FieldParamBinding{
				s25ParentBinding([]string{"people", "failForBob"}, "S25User", "failForBob",
					[]string{"people"}, "Query", "people"),
			},
		})
		s25RequireErrorPathSet(t, result, []any{"people", 1, "failForBob"})
		items := s25DataMap(t, result)["people"].([]any)
		if len(items) != 3 {
			t.Fatalf("people length = %d, want 3", len(items))
		}
		if items[0].(map[string]any)["failForBob"] != "Ada" {
			t.Errorf("people[0] must keep its value, got %v", items[0])
		}
		if items[1].(map[string]any)["failForBob"] != nil {
			t.Errorf("people[1].failForBob must be null, got %v", items[1])
		}
		if items[2].(map[string]any)["failForBob"] != "Cid" {
			t.Errorf("people[2] must keep its value, got %v", items[2])
		}
	})

	t.Run("aliased_list_element_error_uses_the_alias", func(t *testing.T) {
		schema, _ := s25NewCoreSchema(t, nil)
		result := s25Do(t, s25Request{
			Schema: schema,
			Query:  `{ crew: people { check: failForBob } }`,
			Bindings: []FieldParamBinding{
				s25ParentBinding([]string{"crew", "check"}, "S25User", "failForBob",
					[]string{"crew"}, "Query", "people"),
			},
		})
		s25RequireHasErrorPath(t, result, []any{"crew", 1, "check"})
	})

	t.Run("nested_list_error_carries_every_index", func(t *testing.T) {
		result := s25Do(t, s25Request{Schema: s25xCore(t), Query: `{ matrixWithNull }`})
		if len(result.Errors) == 0 {
			t.Fatalf("[[String!]] containing nulls must produce field errors")
		}
		s25RequireErrorPathSet(t, result,
			[]any{"matrixWithNull", 0, 1},
			[]any{"matrixWithNull", 1, 0},
		)
	})

	t.Run("multiple_sibling_errors_and_partial_data", func(t *testing.T) {
		result := s25Do(t, s25Request{
			Schema: s25xCore(t),
			Query:  `{ greeting one: boom two: boom user { boom } }`,
		})
		s25RequireErrorPathSet(t, result,
			[]any{"one"},
			[]any{"two"},
			[]any{"user", "boom"},
		)
		data := s25DataMap(t, result)
		if data["greeting"] != "hello" {
			t.Errorf("greeting = %v, want hello", data["greeting"])
		}
	})
}

// ---------------------------------------------------------------------------
// X-05 变量 × input object × list 提升 × 默认值 × 显式 null × enum
// ---------------------------------------------------------------------------

func TestSpec2025_Cross_VariableAndInputObjectMatrix(t *testing.T) {
	cases := []struct {
		name  string
		query string
		vars  map[string]any
		want  string
	}{
		{
			name:  "all_defaults_applied",
			query: `query Q($f: S25Filter!) { echoFilter(filter: $f) }`,
			vars:  map[string]any{"f": map[string]any{"text": "t"}},
			want:  "text=t|count=7|mode=A|tags=<missing>|nested=<missing>",
		},
		{
			name:  "explicit_null_does_not_use_the_field_default",
			query: `query Q($f: S25Filter!) { echoFilter(filter: $f) }`,
			vars:  map[string]any{"f": map[string]any{"text": "t", "count": nil, "mode": nil}},
			want:  "text=t|count=<null>|mode=<null>|tags=<missing>|nested=<missing>",
		},
		{
			name:  "literal_input_object_with_variable_leaves",
			query: `query Q($t: String!, $c: Int) { echoFilter(filter: {text: $t, count: $c}) }`,
			vars:  map[string]any{"t": "lit", "c": 3},
			want:  "text=lit|count=3|mode=A|tags=<missing>|nested=<missing>",
		},
		{
			name:  "enum_variable",
			query: `query Q($m: S25Mode!) { echoFilter(filter: {text: "t", mode: $m}) }`,
			vars:  map[string]any{"m": "B"},
			want:  "text=t|count=7|mode=B|tags=<missing>|nested=<missing>",
		},
		{
			name:  "single_value_is_coerced_into_a_list",
			query: `query Q($tags: [String!]) { echoFilter(filter: {text: "t", tags: $tags}) }`,
			vars:  map[string]any{"tags": "solo"},
			want:  "text=t|count=7|mode=A|tags=[solo]|nested=<missing>",
		},
		{
			name:  "nested_input_object_defaults",
			query: `query Q($f: S25Filter!) { echoFilter(filter: $f) }`,
			vars:  map[string]any{"f": map[string]any{"text": "t", "nested": map[string]any{"values": []any{1, 2}}}},
			want:  "text=t|count=7|mode=A|tags=<missing>|nested=map[flag:false values:[1 2]]",
		},
	}
	for _, current := range cases {
		t.Run(current.name, func(t *testing.T) {
			result := s25Do(t, s25Request{Schema: s25xCore(t), Query: current.query, Variables: current.vars})
			s25RequireNoErrors(t, result)
			if got := s25DataMap(t, result)["echoFilter"]; got != current.want {
				t.Errorf("echoFilter = %v, want %v", got, current.want)
			}
		})
	}

	t.Run("invalid_enum_in_a_nested_input_object_is_a_request_error", func(t *testing.T) {
		result := s25Do(t, s25Request{
			Schema:    s25xCore(t),
			Query:     `query Q($f: S25Filter!) { echoFilter(filter: $f) }`,
			Variables: map[string]any{"f": map[string]any{"text": "t", "mode": "NOPE"}},
		})
		s25RequireRequestError(t, result)
	})

	t.Run("unknown_nested_input_field_is_a_request_error", func(t *testing.T) {
		result := s25Do(t, s25Request{
			Schema:    s25xCore(t),
			Query:     `query Q($f: S25Filter!) { echoFilter(filter: $f) }`,
			Variables: map[string]any{"f": map[string]any{"text": "t", "nope": 1}},
		})
		s25RequireRequestError(t, result)
	})

	t.Run("null_item_in_a_non_null_item_list_is_a_request_error", func(t *testing.T) {
		result := s25Do(t, s25Request{
			Schema:    s25xCore(t),
			Query:     `query Q($v: [Int!]) { echoList(values: $v) }`,
			Variables: map[string]any{"v": []any{1, nil}},
		})
		s25RequireRequestError(t, result)
	})
}

// ---------------------------------------------------------------------------
// X-06 内省 × 业务字段 × 片段 × 别名 × 条件指令
// ---------------------------------------------------------------------------

func TestSpec2025_Cross_IntrospectionWithBusinessFields(t *testing.T) {
	t.Run("meta_and_business_fields_in_one_selection_set", func(t *testing.T) {
		result := s25Do(t, s25Request{
			Schema: s25xCore(t),
			Query: `
				{
					__typename
					greeting
					user { __typename id }
					meta: __type(name: "S25User") { name kind }
					...M
				}
				fragment M on Query { fromFragment: __typename }
			`,
		})
		s25RequireNoErrors(t, result)
		data := s25DataMap(t, result)
		if data["__typename"] != "Query" || data["fromFragment"] != "Query" {
			t.Errorf("__typename via field/fragment = %v / %v", data["__typename"], data["fromFragment"])
		}
		if data["greeting"] != "hello" {
			t.Errorf("greeting = %v", data["greeting"])
		}
		meta := data["meta"].(map[string]any)
		if meta["name"] != "S25User" || meta["kind"] != "OBJECT" {
			t.Errorf("meta = %v", meta)
		}
	})

	t.Run("skip_on_a_meta_field_next_to_business_fields", func(t *testing.T) {
		result := s25Do(t, s25Request{
			Schema: s25xCore(t),
			Query:  `query Q($s: Boolean!) { __typename @skip(if: $s) greeting }`,
			Variables: map[string]any{
				"s": true,
			},
		})
		s25RequireNoErrors(t, result)
		data := s25DataMap(t, result)
		if _, present := data["__typename"]; present {
			t.Errorf("@skip must remove __typename; data=%v", data)
		}
		if data["greeting"] != "hello" {
			t.Errorf("greeting = %v", data["greeting"])
		}
	})
}

// ---------------------------------------------------------------------------
// X-07 自定义标量 × 变量 × 字面量 × 序列化失败 × list
// ---------------------------------------------------------------------------

func TestSpec2025_Cross_CustomScalarMatrix(t *testing.T) {
	t.Run("literal_odd_value_round_trips", func(t *testing.T) {
		result := s25Do(t, s25Request{Schema: s25xCore(t), Query: `{ odd(value: 3) }`})
		s25RequireNoErrors(t, result)
		if got := s25DataMap(t, result)["odd"]; fmt.Sprintf("%v", got) != "3" {
			t.Errorf("odd = %v, want 3", got)
		}
	})

	t.Run("variable_odd_value_round_trips", func(t *testing.T) {
		result := s25Do(t, s25Request{
			Schema:    s25xCore(t),
			Query:     `query Q($v: S25Odd) { odd(value: $v) }`,
			Variables: map[string]any{"v": 5},
		})
		s25RequireNoErrors(t, result)
		if got := s25DataMap(t, result)["odd"]; fmt.Sprintf("%v", got) != "5" {
			t.Errorf("odd = %v, want 5", got)
		}
	})

	t.Run("even_literal_is_rejected_by_validation", func(t *testing.T) {
		s25RequireInvalid(t, s25xCore(t), `{ odd(value: 4) }`)
	})

	t.Run("even_variable_is_a_request_error", func(t *testing.T) {
		result := s25Do(t, s25Request{
			Schema:    s25xCore(t),
			Query:     `query Q($v: S25Odd) { odd(value: $v) }`,
			Variables: map[string]any{"v": 4},
		})
		s25RequireRequestError(t, result)
	})

	t.Run("failed_result_serialization_is_a_field_error", func(t *testing.T) {
		// §6.4.3: 叶子结果强制转换失败必须产生 field error，而不是静默 null。
		result := s25Do(t, s25Request{Schema: s25xCore(t), Query: `{ oddOut }`})
		if len(result.Errors) == 0 {
			t.Fatalf("a custom scalar whose Serialize returns nil must produce a field error")
		}
		s25RequireHasErrorPath(t, result, []any{"oddOut"})
		if s25DataMap(t, result)["oddOut"] != nil {
			t.Errorf("oddOut must be null after a failed serialization")
		}
	})
}

// ---------------------------------------------------------------------------
// X-08 默认 resolver × 非根字段 × 报错 × 列表
// ---------------------------------------------------------------------------

func TestSpec2025_Cross_DefaultResolverBranches(t *testing.T) {
	schema := s25NewDefaultResolverSchema(t)
	result := s25Do(t, s25Request{
		Schema: schema,
		Query:  `{ fromMap { plain thunk } fromStruct { name age } fromResolver { value } }`,
	})
	s25RequireNoErrors(t, result)
	data := s25DataMap(t, result)

	fromMap := data["fromMap"].(map[string]any)
	if fromMap["plain"] != "plainValue" {
		t.Errorf("map property = %v, want plainValue", fromMap["plain"])
	}
	if fromMap["thunk"] != "thunkValue" {
		t.Errorf("func property must be invoked by the default resolver; got %v", fromMap["thunk"])
	}

	fromStruct := data["fromStruct"].(map[string]any)
	if fromStruct["name"] != "Ada" {
		t.Errorf("struct graphql tag = %v, want Ada", fromStruct["name"])
	}
	if fmt.Sprintf("%v", fromStruct["age"]) != "36" {
		t.Errorf("struct json tag = %v, want 36", fromStruct["age"])
	}

	fromResolver := data["fromResolver"].(map[string]any)
	if fromResolver["value"] != "resolved:value" {
		t.Errorf("FieldResolver source = %v, want resolved:value", fromResolver["value"])
	}
}

// ---------------------------------------------------------------------------
// X-09 Extension 钩子 × 被跳过的字段 × meta 字段 × 报错字段
// ---------------------------------------------------------------------------

type s25xExtension struct {
	mu              sync.Mutex
	executionStarts int
	fieldStarts     int
	fieldErrors     int
	fieldNames      []string
}

func (e *s25xExtension) Init(ctx context.Context, p *Params) context.Context { return ctx }
func (e *s25xExtension) Name() string                                        { return "s25Cross" }

func (e *s25xExtension) ParseDidStart(ctx context.Context) (context.Context, ParseFinishFunc) {
	return ctx, func(error) {}
}

func (e *s25xExtension) ValidationDidStart(ctx context.Context) (context.Context, ValidationFinishFunc) {
	return ctx, func([]gqlerrors.FormattedError) {}
}

func (e *s25xExtension) ExecutionDidStart(ctx context.Context) (context.Context, ExecutionFinishFunc) {
	e.mu.Lock()
	e.executionStarts++
	e.mu.Unlock()
	return ctx, func(*Result) {}
}

func (e *s25xExtension) ResolveFieldDidStart(ctx context.Context, i *ResolveInfo) (context.Context, ResolveFieldFinishFunc) {
	e.mu.Lock()
	e.fieldStarts++
	e.fieldNames = append(e.fieldNames, i.FieldName)
	e.mu.Unlock()
	return ctx, func(value interface{}, err error) {
		if err != nil {
			e.mu.Lock()
			e.fieldErrors++
			e.mu.Unlock()
		}
	}
}

func (e *s25xExtension) HasResult() bool                           { return false }
func (e *s25xExtension) GetResult(ctx context.Context) interface{} { return nil }

func TestSpec2025_Cross_ExtensionHooksRespectSelection(t *testing.T) {
	extension := &s25xExtension{}
	schema, err := NewSchema(SchemaConfig{
		Query: NewObject(ObjectConfig{Name: "Query", Fields: Fields{
			"kept":    &Field{Type: String, Resolve: s25Const("kept")},
			"skipped": &Field{Type: String, Resolve: s25Const("skipped")},
			"failing": &Field{Type: String, Resolve: s25Fail("failing failed")},
		}}),
		Extensions: []Extension{extension},
	})
	if err != nil {
		t.Fatalf("build schema: %v", err)
	}

	result := s25Do(t, s25Request{
		Schema: schema,
		Query:  `{ kept skipped @skip(if: true) failing __typename }`,
	})
	if len(result.Errors) != 1 {
		t.Fatalf("expected exactly one field error, got %v", s25ErrorMessages(result))
	}

	extension.mu.Lock()
	defer extension.mu.Unlock()
	if extension.executionStarts != 1 {
		t.Errorf("ExecutionDidStart = %d, want 1", extension.executionStarts)
	}
	for _, name := range extension.fieldNames {
		if name == "skipped" {
			t.Errorf("a field removed by @skip must not trigger the resolve-field hook; names=%v", extension.fieldNames)
		}
	}
	if extension.fieldErrors != 1 {
		t.Errorf("resolve-field finish hook saw %d errors, want 1", extension.fieldErrors)
	}
}

// ---------------------------------------------------------------------------
// X-10 mutation 串行 × 嵌套子字段 × 可空报错 × 后续根字段
// ---------------------------------------------------------------------------

func TestSpec2025_Cross_MutationSerialWithNestedSelections(t *testing.T) {
	t.Run("nested_selections_complete_before_the_next_root_field", func(t *testing.T) {
		var order []string
		var mu sync.Mutex
		schema := s25NewMutationSchema(t, &order, &mu)
		result := s25Do(t, s25Request{
			Schema:   schema,
			Query:    `mutation { first { slow } second { slow } third { slow } }`,
			Bindings: []FieldParamBinding{},
		})
		s25RequireNoErrors(t, result)
		mu.Lock()
		got := append([]string(nil), order...)
		mu.Unlock()
		want := []string{"first", "child:first", "second", "child:second", "third", "child:third"}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("mutation execution order = %v, want %v", got, want)
		}
	})

	t.Run("failing_nullable_root_field_does_not_stop_later_roots", func(t *testing.T) {
		var order []string
		var mu sync.Mutex
		schema := s25NewMutationSchema(t, &order, &mu)
		result := s25Do(t, s25Request{
			Schema: schema,
			Query:  `mutation { first { id } failing second { id } }`,
		})
		if len(result.Errors) != 1 {
			t.Fatalf("expected one error from the failing root field, got %v", s25ErrorMessages(result))
		}
		mu.Lock()
		got := append([]string(nil), order...)
		mu.Unlock()
		want := []string{"first", "failing", "second"}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("mutation execution order = %v, want %v", got, want)
		}
		data := s25DataMap(t, result)
		if data["second"] == nil {
			t.Errorf("a later mutation root must still execute after a nullable failure; data=%v", data)
		}
	})
}
