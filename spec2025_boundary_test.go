package graphql

// spec2025_boundary_test.go
//
// 边界值与极限值矩阵。取值同时覆盖合法边界、越界值和规模上限。

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
)

func s25bCore(t *testing.T) Schema {
	t.Helper()
	schema, _ := s25NewCoreSchema(t, nil)
	return schema
}

// s25bScalarOut 构造一个返回固定值的单字段 schema，用于结果强制转换边界。
func s25bScalarOut(t *testing.T, fieldType Output, value any) Schema {
	t.Helper()
	return s25NewSchema(t, Fields{
		"value": &Field{Type: fieldType, Resolve: s25Const(value)},
	})
}

// ---------------------------------------------------------------------------
// B-01 Int 边界
// ---------------------------------------------------------------------------

func TestSpec2025_Boundary_IntValues(t *testing.T) {
	// §3.5.1: Int 是有符号 32 位整数，范围 [-2^31, 2^31-1]。
	t.Run("input_boundaries", func(t *testing.T) {
		valid := map[string]any{
			"zero":     0,
			"one":      1,
			"minusOne": -1,
			"max":      2147483647,
			"min":      -2147483648,
		}
		for name, value := range valid {
			t.Run("valid_"+name, func(t *testing.T) {
				result := s25Do(t, s25Request{
					Schema:    s25bCore(t),
					Query:     `query Q($v: Int!) { echoInt(value: $v) }`,
					Variables: map[string]any{"v": value},
				})
				s25RequireNoErrors(t, result)
				if got := s25DataMap(t, result)["echoInt"]; fmt.Sprintf("%v", got) != fmt.Sprintf("%v", value) {
					t.Errorf("echoInt = %v, want %v", got, value)
				}
			})
		}

		invalid := map[string]any{
			"above_max":      2147483648,
			"below_min":      -2147483649,
			"fraction":       1.5,
			"numeric_string": "5",
			"boolean":        true,
		}
		for name, value := range invalid {
			t.Run("invalid_"+name, func(t *testing.T) {
				result := s25Do(t, s25Request{
					Schema:    s25bCore(t),
					Query:     `query Q($v: Int!) { echoInt(value: $v) }`,
					Variables: map[string]any{"v": value},
				})
				s25RequireRequestError(t, result)
			})
		}
	})

	t.Run("result_boundaries", func(t *testing.T) {
		t.Run("max_int32_serializes", func(t *testing.T) {
			result := s25Do(t, s25Request{Schema: s25bScalarOut(t, Int, int(2147483647)), Query: `{ value }`})
			s25RequireNoErrors(t, result)
			if fmt.Sprintf("%v", s25DataMap(t, result)["value"]) != "2147483647" {
				t.Errorf("value = %v", s25DataMap(t, result)["value"])
			}
		})
		t.Run("out_of_range_result_is_a_field_error", func(t *testing.T) {
			// §6.4.3: 叶子结果强制转换失败必须产生 field error。
			result := s25Do(t, s25Request{Schema: s25bScalarOut(t, Int, int64(3000000000)), Query: `{ value }`})
			if len(result.Errors) == 0 {
				t.Fatalf("an out-of-range Int result must produce a field error, got data=%#v", s25Plain(result.Data))
			}
			s25RequireHasErrorPath(t, result, []any{"value"})
		})
	})
}

// ---------------------------------------------------------------------------
// B-02 Float 边界
// ---------------------------------------------------------------------------

func TestSpec2025_Boundary_FloatValues(t *testing.T) {
	t.Run("valid_inputs", func(t *testing.T) {
		cases := map[string]any{
			"zero":            0.0,
			"negative_zero":   -0.0,
			"integer_coerced": 3,
			"tiny":            1e-308,
			"huge":            1.7976931348623157e308,
		}
		for name, value := range cases {
			t.Run(name, func(t *testing.T) {
				result := s25Do(t, s25Request{
					Schema:    s25bCore(t),
					Query:     `query Q($v: Float) { echoFloat(value: $v) }`,
					Variables: map[string]any{"v": value},
				})
				s25RequireNoErrors(t, result)
			})
		}
	})

	t.Run("literal_exponent_forms", func(t *testing.T) {
		for _, literal := range []string{"1.0", "1e10", "1E10", "1.5e-3", "-2.5", "0.0"} {
			t.Run(literal, func(t *testing.T) {
				result := s25Do(t, s25Request{
					Schema: s25bCore(t),
					Query:  fmt.Sprintf(`{ echoFloat(value: %s) }`, literal),
				})
				s25RequireNoErrors(t, result)
			})
		}
	})

	t.Run("numeric_string_is_rejected", func(t *testing.T) {
		result := s25Do(t, s25Request{
			Schema:    s25bCore(t),
			Query:     `query Q($v: Float) { echoFloat(value: $v) }`,
			Variables: map[string]any{"v": "1.5"},
		})
		s25RequireRequestError(t, result)
	})
}

// ---------------------------------------------------------------------------
// B-03 String 边界
// ---------------------------------------------------------------------------

func TestSpec2025_Boundary_StringValues(t *testing.T) {
	long := strings.Repeat("x", 64*1024)
	cases := map[string]string{
		"empty":            "",
		"single":           "a",
		"long_64KiB":       long,
		"unicode_bmp":      "你好",
		"surrogate_pair":   "😀",
		"control_chars":    "a\tb\nc",
		"quotes_and_slash": `a"b\c`,
	}
	for name, value := range cases {
		t.Run(name, func(t *testing.T) {
			result := s25Do(t, s25Request{
				Schema:    s25bCore(t),
				Query:     `query Q($v: String) { echoNoDefault(value: $v) }`,
				Variables: map[string]any{"v": value},
			})
			s25RequireNoErrors(t, result)
			got := s25DataMap(t, result)["echoNoDefault"]
			want := value
			if value == "" {
				// 空串是合法 String，不是缺省，也不是 null。
				want = ""
			}
			if got != want {
				if len(fmt.Sprintf("%v", got)) > 64 {
					t.Errorf("echoNoDefault length = %d, want %d", len(fmt.Sprintf("%v", got)), len(want))
				} else {
					t.Errorf("echoNoDefault = %q, want %q", got, want)
				}
			}
		})
	}

	t.Run("non_string_result_is_a_field_error", func(t *testing.T) {
		// §3.5.3: String 的结果强制转换只接受字符串。
		result := s25Do(t, s25Request{Schema: s25bScalarOut(t, String, 42), Query: `{ value }`})
		if len(result.Errors) == 0 {
			t.Fatalf("a non-string result for a String field must produce a field error, got %#v", s25Plain(result.Data))
		}
	})
}

// ---------------------------------------------------------------------------
// B-04 ID 边界
// ---------------------------------------------------------------------------

func TestSpec2025_Boundary_IDValues(t *testing.T) {
	t.Run("string_and_integer_inputs_are_accepted", func(t *testing.T) {
		for name, value := range map[string]any{"string": "abc", "numeric_string": "123", "integer": 123} {
			t.Run(name, func(t *testing.T) {
				result := s25Do(t, s25Request{
					Schema:    s25bCore(t),
					Query:     `query Q($v: ID) { echoID(value: $v) }`,
					Variables: map[string]any{"v": value},
				})
				s25RequireNoErrors(t, result)
			})
		}
	})

	t.Run("boolean_input_is_rejected", func(t *testing.T) {
		result := s25Do(t, s25Request{
			Schema:    s25bCore(t),
			Query:     `query Q($v: ID) { echoID(value: $v) }`,
			Variables: map[string]any{"v": true},
		})
		s25RequireRequestError(t, result)
	})

	t.Run("result_always_serializes_as_a_string", func(t *testing.T) {
		result := s25Do(t, s25Request{Schema: s25bScalarOut(t, ID, 42), Query: `{ value }`})
		s25RequireNoErrors(t, result)
		encoded := s25MarshalData(t, result)
		if !strings.Contains(encoded, `"value":"42"`) {
			t.Errorf("ID result must serialize as a String, got %s", encoded)
		}
	})
}

// ---------------------------------------------------------------------------
// B-05 Enum 边界
// ---------------------------------------------------------------------------

func TestSpec2025_Boundary_EnumValues(t *testing.T) {
	t.Run("all_declared_values_are_accepted", func(t *testing.T) {
		for _, name := range []string{"A", "B", "DEPRECATED_C"} {
			t.Run(name, func(t *testing.T) {
				result := s25Do(t, s25Request{
					Schema: s25bCore(t),
					Query:  fmt.Sprintf(`{ echoMode(mode: %s) }`, name),
				})
				s25RequireNoErrors(t, result)
			})
		}
	})

	t.Run("undefined_value_is_rejected", func(t *testing.T) {
		s25RequireInvalid(t, s25bCore(t), `{ echoMode(mode: NOT_A_MODE) }`)
	})

	t.Run("enum_names_are_case_sensitive", func(t *testing.T) {
		s25RequireInvalid(t, s25bCore(t), `{ echoMode(mode: a) }`)
	})

	t.Run("quoted_enum_literal_is_rejected", func(t *testing.T) {
		s25RequireInvalid(t, s25bCore(t), `{ echoMode(mode: "A") }`)
	})

	t.Run("unknown_internal_result_value_is_a_field_error", func(t *testing.T) {
		result := s25Do(t, s25Request{Schema: s25bCore(t), Query: `{ badEnum }`})
		if len(result.Errors) == 0 {
			t.Fatalf("an enum result outside the declared values must produce a field error, got %#v", s25Plain(result.Data))
		}
		s25RequireHasErrorPath(t, result, []any{"badEnum"})
	})
}

// ---------------------------------------------------------------------------
// B-06 / B-07 List 规模与嵌套
// ---------------------------------------------------------------------------

func TestSpec2025_Boundary_ListSizes(t *testing.T) {
	for _, size := range []int{0, 1, 1024, 10240} {
		t.Run(fmt.Sprintf("size_%d", size), func(t *testing.T) {
			result := s25Do(t, s25Request{Schema: s25NewBigListSchema(t, size), Query: `{ items }`})
			s25RequireNoErrors(t, result)
			items, ok := s25DataMap(t, result)["items"].([]any)
			if !ok {
				t.Fatalf("items is %T, want a list", s25DataMap(t, result)["items"])
			}
			if len(items) != size {
				t.Fatalf("items length = %d, want %d", len(items), size)
			}
			if size > 0 {
				if items[0] != "item-0" || items[size-1] != fmt.Sprintf("item-%d", size-1) {
					t.Errorf("list order is wrong: first=%v last=%v", items[0], items[size-1])
				}
			}
		})
	}

	t.Run("all_null_elements", func(t *testing.T) {
		schema := s25NewSchema(t, Fields{
			"items": &Field{Type: NewList(String), Resolve: s25Const([]any{nil, nil, nil})},
		})
		result := s25Do(t, s25Request{Schema: schema, Query: `{ items }`})
		s25RequireNoErrors(t, result)
		items := s25DataMap(t, result)["items"].([]any)
		if len(items) != 3 || items[0] != nil || items[2] != nil {
			t.Errorf("items = %v, want [nil nil nil]", items)
		}
	})

	t.Run("leading_and_trailing_nulls", func(t *testing.T) {
		schema := s25NewSchema(t, Fields{
			"items": &Field{Type: NewList(String), Resolve: s25Const([]any{nil, "b", nil})},
		})
		result := s25Do(t, s25Request{Schema: schema, Query: `{ items }`})
		s25RequireNoErrors(t, result)
		items := s25DataMap(t, result)["items"].([]any)
		if len(items) != 3 || items[0] != nil || items[1] != "b" || items[2] != nil {
			t.Errorf("items = %v, want [nil b nil]", items)
		}
	})
}

func TestSpec2025_Boundary_NestedListDepth(t *testing.T) {
	// §3.11 + §6.4.3: 多层 List 的每一层都要独立完成。
	build := func(depth int) (Output, any) {
		var fieldType Output = String
		var value any = "leaf"
		for level := 0; level < depth; level++ {
			fieldType = NewList(fieldType)
			value = []any{value}
		}
		return fieldType, value
	}
	for _, depth := range []int{1, 2, 3, 4} {
		t.Run(fmt.Sprintf("depth_%d", depth), func(t *testing.T) {
			fieldType, value := build(depth)
			result := s25Do(t, s25Request{Schema: s25bScalarOut(t, fieldType, value), Query: `{ value }`})
			s25RequireNoErrors(t, result)
			current := s25DataMap(t, result)["value"]
			for level := 0; level < depth; level++ {
				items, ok := current.([]any)
				if !ok {
					t.Fatalf("level %d is %T, want a list", level, current)
				}
				if len(items) != 1 {
					t.Fatalf("level %d length = %d, want 1", level, len(items))
				}
				current = items[0]
			}
			if current != "leaf" {
				t.Errorf("innermost value = %v, want leaf", current)
			}
		})
	}

	t.Run("empty_list_at_each_level", func(t *testing.T) {
		result := s25Do(t, s25Request{
			Schema: s25bScalarOut(t, NewList(NewList(String)), []any{[]any{}, []any{}}),
			Query:  `{ value }`,
		})
		s25RequireNoErrors(t, result)
		outer := s25DataMap(t, result)["value"].([]any)
		if len(outer) != 2 {
			t.Fatalf("outer length = %d, want 2", len(outer))
		}
		for index, inner := range outer {
			items, ok := inner.([]any)
			if !ok || len(items) != 0 {
				t.Errorf("inner[%d] = %#v, want an empty list", index, inner)
			}
		}
	})
}

// ---------------------------------------------------------------------------
// B-08 选择集深度
// ---------------------------------------------------------------------------

func TestSpec2025_Boundary_SelectionDepth(t *testing.T) {
	if !s25Isolated(t, "selection-depth") {
		return
	}
	for _, depth := range []int{1, 8, 32, 64} {
		t.Run(fmt.Sprintf("depth_%d", depth), func(t *testing.T) {
			var builder strings.Builder
			builder.WriteString("{ root ")
			for level := 0; level < depth; level++ {
				builder.WriteString("{ next ")
			}
			builder.WriteString("{ depth } ")
			for level := 0; level < depth; level++ {
				builder.WriteString("} ")
			}
			builder.WriteString("}")

			result := s25Do(t, s25Request{Schema: s25NewDeepSchema(t), Query: builder.String()})
			s25RequireNoErrors(t, result)

			current := s25DataMap(t, result)["root"]
			for level := 0; level <= depth; level++ {
				node, ok := current.(map[string]any)
				if !ok {
					t.Fatalf("level %d is %T, want a map", level, current)
				}
				if level == depth {
					if fmt.Sprintf("%v", node["depth"]) != "1" {
						t.Errorf("innermost depth = %v, want 1", node["depth"])
					}
					return
				}
				current = node["next"]
			}
		})
	}
}

// ---------------------------------------------------------------------------
// B-09 选择集广度
// ---------------------------------------------------------------------------

func TestSpec2025_Boundary_SelectionBreadth(t *testing.T) {
	for _, width := range []int{1, 64, 512} {
		t.Run(fmt.Sprintf("width_%d", width), func(t *testing.T) {
			schema := s25NewWideSchema(t, width)
			var builder strings.Builder
			builder.WriteString("{")
			for index := 0; index < width; index++ {
				fmt.Fprintf(&builder, " f%d", index)
			}
			builder.WriteString(" }")

			result := s25Do(t, s25Request{Schema: schema, Query: builder.String()})
			s25RequireNoErrors(t, result)
			data := s25DataMap(t, result)
			if len(data) != width {
				t.Fatalf("response has %d keys, want %d", len(data), width)
			}
			for index := 0; index < width; index++ {
				key := fmt.Sprintf("f%d", index)
				if data[key] != fmt.Sprintf("v%d", index) {
					t.Fatalf("%s = %v, want v%d", key, data[key], index)
				}
			}
		})
	}

	t.Run("many_aliases_of_one_field", func(t *testing.T) {
		const width = 256
		schema := s25NewWideSchema(t, 1)
		var builder strings.Builder
		builder.WriteString("{")
		for index := 0; index < width; index++ {
			fmt.Fprintf(&builder, " a%d: f0", index)
		}
		builder.WriteString(" }")

		result := s25Do(t, s25Request{Schema: schema, Query: builder.String()})
		s25RequireNoErrors(t, result)
		data := s25DataMap(t, result)
		if len(data) != width {
			t.Fatalf("response has %d keys, want %d", len(data), width)
		}
	})
}

// ---------------------------------------------------------------------------
// B-10 变量规模与嵌套
// ---------------------------------------------------------------------------

func TestSpec2025_Boundary_VariableCountAndNesting(t *testing.T) {
	for _, count := range []int{0, 1, 64, 256} {
		t.Run(fmt.Sprintf("variables_%d", count), func(t *testing.T) {
			schema := s25NewWideSchema(t, 1)
			var definitions []string
			variables := map[string]any{}
			for index := 0; index < count; index++ {
				definitions = append(definitions, fmt.Sprintf("$v%d: String", index))
				variables[fmt.Sprintf("v%d", index)] = fmt.Sprintf("value-%d", index)
			}
			query := "{ f0 }"
			if count > 0 {
				// 每个变量都必须被使用，否则违反 §5.8.4。
				var uses []string
				for index := 0; index < count; index++ {
					uses = append(uses, fmt.Sprintf("a%d: echoVar(value: $v%d)", index, index))
				}
				schema = s25NewSchema(t, Fields{
					"f0": &Field{Type: String, Resolve: s25Const("v0")},
					"echoVar": &Field{
						Type: String,
						Args: FieldConfigArgument{"value": &ArgumentConfig{Type: String}},
						Resolve: func(p ResolveParams) (any, error) {
							return p.Args["value"], nil
						},
					},
				})
				query = fmt.Sprintf("query Q(%s) { %s }", strings.Join(definitions, ", "), strings.Join(uses, " "))
			}
			result := s25Do(t, s25Request{Schema: schema, Query: query, Variables: variables})
			s25RequireNoErrors(t, result)
			data := s25DataMap(t, result)
			if count > 0 && data["a0"] != "value-0" {
				t.Errorf("a0 = %v, want value-0", data["a0"])
			}
		})
	}

	t.Run("deeply_nested_input_object", func(t *testing.T) {
		const depth = 8
		var node *InputObject
		node = NewInputObject(InputObjectConfig{
			Name: "S25BoundaryNode",
			Fields: InputObjectConfigFieldMapThunk(func() InputObjectConfigFieldMap {
				return InputObjectConfigFieldMap{
					"label": &InputObjectFieldConfig{Type: String},
					"child": &InputObjectFieldConfig{Type: node},
				}
			}),
		})
		schema := s25NewSchema(t, Fields{
			"echoNode": &Field{
				Type: String,
				Args: FieldConfigArgument{"node": &ArgumentConfig{Type: node}},
				Resolve: func(p ResolveParams) (any, error) {
					depthOf := 0
					current, _ := p.Args["node"].(map[string]any)
					for current != nil {
						depthOf++
						current, _ = current["child"].(map[string]any)
					}
					return fmt.Sprintf("%d", depthOf), nil
				},
			},
		}, node)

		value := map[string]any{"label": "leaf"}
		for level := 1; level < depth; level++ {
			value = map[string]any{"label": fmt.Sprintf("l%d", level), "child": value}
		}
		result := s25Do(t, s25Request{
			Schema:    schema,
			Query:     `query Q($n: S25BoundaryNode) { echoNode(node: $n) }`,
			Variables: map[string]any{"n": value},
		})
		s25RequireNoErrors(t, result)
		if got := s25DataMap(t, result)["echoNode"]; got != fmt.Sprintf("%d", depth) {
			t.Errorf("nested input depth = %v, want %d", got, depth)
		}
	})
}

// ---------------------------------------------------------------------------
// B-11 片段规模
// ---------------------------------------------------------------------------

func TestSpec2025_Boundary_FragmentCounts(t *testing.T) {
	t.Run("many_distinct_fragments", func(t *testing.T) {
		const count = 128
		schema := s25NewWideSchema(t, count)
		var spreads, definitions []string
		for index := 0; index < count; index++ {
			spreads = append(spreads, fmt.Sprintf("...F%d", index))
			definitions = append(definitions, fmt.Sprintf("fragment F%d on Query { f%d }", index, index))
		}
		query := fmt.Sprintf("{ %s }\n%s", strings.Join(spreads, " "), strings.Join(definitions, "\n"))
		result := s25Do(t, s25Request{Schema: schema, Query: query})
		s25RequireNoErrors(t, result)
		if len(s25DataMap(t, result)) != count {
			t.Errorf("response key count = %d, want %d", len(s25DataMap(t, result)), count)
		}
	})

	t.Run("one_fragment_spread_many_times_merges", func(t *testing.T) {
		const count = 64
		counter := s25NewCounter()
		schema, _ := s25NewCoreSchema(t, counter)
		spreads := strings.TrimSpace(strings.Repeat("...F ", count))
		query := fmt.Sprintf("{ %s }\nfragment F on Query { greeting }", spreads)
		result := s25Do(t, s25Request{Schema: schema, Query: query})
		s25RequireNoErrors(t, result)
		if got := counter.Get("greeting"); got != 1 {
			t.Errorf("greeting resolver called %d times, want 1 after merging %d spreads", got, count)
		}
	})
}

// ---------------------------------------------------------------------------
// B-12 并发与文档规模
// ---------------------------------------------------------------------------

func TestSpec2025_Boundary_ConcurrentRequestsDoNotLeakVariables(t *testing.T) {
	const requests = 128
	schema := s25NewSchema(t, Fields{
		"echoVar": &Field{
			Type: String,
			Args: FieldConfigArgument{"value": &ArgumentConfig{Type: String}},
			Resolve: func(p ResolveParams) (any, error) {
				return p.Args["value"], nil
			},
		},
	})
	const query = `query Q($v: String) { echoVar(value: $v) }`

	var wg sync.WaitGroup
	failures := make([]string, requests)
	wg.Add(requests)
	for index := 0; index < requests; index++ {
		go func(index int) {
			defer wg.Done()
			want := fmt.Sprintf("req-%d", index)
			result := Do(Params{
				Schema:         schema,
				RequestString:  query,
				VariableValues: map[string]any{"v": want},
			})
			if len(result.Errors) != 0 {
				failures[index] = fmt.Sprintf("request %d errored: %v", index, s25ErrorMessages(result))
				return
			}
			plain, ok := s25Plain(result.Data).(map[string]any)
			if !ok {
				failures[index] = fmt.Sprintf("request %d data is %T", index, result.Data)
				return
			}
			if plain["echoVar"] != want {
				failures[index] = fmt.Sprintf("request %d echoVar = %v, want %v", index, plain["echoVar"], want)
			}
		}(index)
	}
	wg.Wait()

	for _, failure := range failures {
		if failure != "" {
			t.Errorf("%s", failure)
		}
	}
}

func TestSpec2025_Boundary_LargeDocument(t *testing.T) {
	// 单文档约 1 MiB：解析与执行都必须完成。
	const width = 4096
	schema := s25NewWideSchema(t, width)
	var builder strings.Builder
	builder.WriteString("{")
	for index := 0; index < width; index++ {
		fmt.Fprintf(&builder, " a%d: f%d # padding comment to grow the document body\n", index, index)
	}
	builder.WriteString(" }")
	query := builder.String()
	if len(query) < 200*1024 {
		t.Fatalf("test document is only %d bytes; it should be large", len(query))
	}

	result := s25Do(t, s25Request{Schema: schema, Query: query})
	s25RequireNoErrors(t, result)
	if len(s25DataMap(t, result)) != width {
		t.Errorf("response key count = %d, want %d", len(s25DataMap(t, result)), width)
	}
}

func TestSpec2025_Boundary_RepeatedExecutionIsStable(t *testing.T) {
	// 同一 schema 上连续执行同一查询与不同查询交替，结果必须稳定
	// （覆盖 plan / AST 缓存命中与未命中交替）。
	schema, _ := s25NewCoreSchema(t, nil)
	queries := []string{
		`{ greeting }`,
		`{ user { id name } }`,
		`{ greeting }`,
		`{ people { id } }`,
		`{ user { id name } }`,
	}
	var snapshots []string
	for round := 0; round < 4; round++ {
		for _, query := range queries {
			result := Do(Params{Schema: schema, RequestString: query})
			if len(result.Errors) != 0 {
				t.Fatalf("query %q errored: %v", query, s25ErrorMessages(result))
			}
			encoded, err := json.Marshal(s25Plain(result.Data))
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			snapshots = append(snapshots, query+" => "+string(encoded))
		}
	}
	for index := len(queries); index < len(snapshots); index++ {
		if snapshots[index] != snapshots[index%len(queries)] {
			t.Errorf("unstable result on repeat %d:\n got %s\nwant %s",
				index, snapshots[index], snapshots[index%len(queries)])
		}
	}
}
