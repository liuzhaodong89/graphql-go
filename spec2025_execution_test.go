package graphql

// spec2025_execution_test.go
//
// GraphQL September 2025 规范 §6 Execution 一致性用例。
// https://spec.graphql.org/September2025/#sec-Execution
//
// 期望值全部按规范条文推导，而非按当前实现的行为反推。
// 每个用例都用新构造的 schema，避免 sgraph 引擎重复注册。

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// 本文件私有 helper（统一 s25exec 前缀）
// ---------------------------------------------------------------------------

// s25execCore 返回一份全新的 core schema。
func s25execCore(t testing.TB) Schema {
	t.Helper()
	schema, _ := s25NewCoreSchema(t, nil)
	return schema
}

// s25execRequireScalar 断言 core schema 上单字段查询返回指定字符串。
func s25execRequireScalar(t testing.TB, query string, variables map[string]any, field, want string) {
	t.Helper()
	result := s25Do(t, s25Request{Schema: s25execCore(t), Query: query, Variables: variables})
	s25RequireNoErrors(t, result)
	data := s25DataMap(t, result)
	got, ok := data[field]
	if !ok {
		t.Fatalf("response has no entry for %q; data=%#v", field, data)
	}
	if got != want {
		t.Fatalf("%s = %#v, want %#v (query=%s vars=%#v)", field, got, want, query, variables)
	}
}

// s25execRequireRequestError 在 core schema 上执行并断言这是一次 request error。
// 规范 §6.1.2：变量输入协变失败时 operation fails without execution，
// §7.1.2 因此响应中不得出现 data 键。
func s25execRequireRequestError(t testing.TB, query string, variables map[string]any) {
	t.Helper()
	result := s25Do(t, s25Request{Schema: s25execCore(t), Query: query, Variables: variables})
	s25RequireRequestError(t, result)
}

// s25execRequireNullField 断言某个响应位置存在且为 null。
// 规范 §6.4.4：execution error 发生的位置被当作解析为 null，键仍然存在。
func s25execRequireNullField(t testing.TB, result *Result, name string) {
	t.Helper()
	data := s25DataMap(t, result)
	value, ok := data[name]
	if !ok {
		t.Fatalf("response has no entry for %q; data=%#v", name, data)
	}
	if value != nil {
		t.Fatalf("field %q = %#v, want null", name, value)
	}
}

// s25execRequireNullData 断言 data 整体为 null 且至少有一条错误。
// 规范 §6.4.4 最后一段：从根到错误源全部是 Non-Null 时，data 应当是 null。
func s25execRequireNullData(t testing.TB, result *Result) {
	t.Helper()
	if len(result.Errors) == 0 {
		t.Fatalf("expected an execution error, got none; data=%#v", s25Plain(result.Data))
	}
	if plain := s25Plain(result.Data); plain != nil {
		t.Fatalf("data = %#v, want null", plain)
	}
}

// s25execRequireOrder 断言记录到的执行顺序逐项相等。
func s25execRequireOrder(t testing.TB, got, want []string) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("execution order mismatch\n got: %v\nwant: %v", got, want)
	}
}

// s25execIDValue 取出 echoID 的 "%T:%v" 输出中的值部分。
// 规范 §3.5.5 只要求 ID 输入接受 String 与 Int，未规定内部表示，
// 因此断言只落在值上。
func s25execIDValue(t testing.TB, got string) string {
	t.Helper()
	index := strings.Index(got, ":")
	if index < 0 {
		t.Fatalf("echoID output %q is not in %%T:%%v form", got)
	}
	return got[index+1:]
}

// s25execProfile 用于构造"类型化 nil 指针"的解析结果。
type s25execProfile struct {
	Name string `graphql:"name"`
}

// s25execNewValueSchema 覆盖 §6.4.2 中 resolver 返回 nil / 类型化 nil 指针的分支。
func s25execNewValueSchema(t testing.TB) Schema {
	t.Helper()
	child := NewObject(ObjectConfig{Name: "S25ExecChild", Fields: Fields{
		"name": &Field{Type: String},
	}})
	return s25NewSchema(t, Fields{
		"nilValue": &Field{Type: String, Resolve: func(ResolveParams) (any, error) {
			return nil, nil
		}},
		"typedNilString": &Field{Type: String, Resolve: func(ResolveParams) (any, error) {
			var pointer *string
			return pointer, nil
		}},
		"typedNilObject": &Field{Type: child, Resolve: func(ResolveParams) (any, error) {
			var pointer *s25execProfile
			return pointer, nil
		}},
	}, child)
}

// s25execNewConcurrentSchema 构造两个互相等待的 Query 根字段。
// 规范 §6.2.1 / §6.3.4：query 的根字段可以并行执行；若实现串行执行，
// 两个 resolver 都会等待超时并报错。
func s25execNewConcurrentSchema(t testing.TB, timeout time.Duration) Schema {
	t.Helper()
	alphaReady := make(chan struct{})
	betaReady := make(chan struct{})
	var alphaOnce, betaOnce sync.Once

	await := func(name string, other chan struct{}) (any, error) {
		select {
		case <-other:
			return name, nil
		case <-time.After(timeout):
			return nil, fmt.Errorf("%s: timed out waiting for the sibling root field; root fields did not execute concurrently", name)
		}
	}

	return s25NewSchema(t, Fields{
		"alpha": &Field{Type: String, Resolve: func(ResolveParams) (any, error) {
			alphaOnce.Do(func() { close(alphaReady) })
			return await("alpha", betaReady)
		}},
		"beta": &Field{Type: String, Resolve: func(ResolveParams) (any, error) {
			betaOnce.Do(func() { close(betaReady) })
			return await("beta", alphaReady)
		}},
	})
}

// s25execParseOrValidationShapes 是"解析/校验失败"错误的特征子串。
var s25execParseOrValidationShapes = []string{
	"Syntax Error",
	"Cannot query field",
	"Unknown type",
	"Unknown operation",
	"must have a selection of subfields",
}

// ---------------------------------------------------------------------------
// §6.1.2 Coercing Variable Values
// ---------------------------------------------------------------------------

func TestSpec2025_Execution_VariableCoercion(t *testing.T) {
	// §6.1.2 CoerceVariableValues：
	//   - hasValue 为 false 且存在默认值（含 null）时使用默认值；
	//   - 否则变量类型 Non-Null 且（无值或值为 null）时抛 request error；
	//   - 否则有值时：null 直接写入 null，非 null 按输入协变规则协变，失败抛 request error。

	t.Run("omitted_variable_uses_variable_default", func(t *testing.T) {
		s25execRequireScalar(t,
			`query ($v: String = "varDefault") { echoNoDefault(value: $v) }`,
			nil, "echoNoDefault", "varDefault")
	})

	t.Run("omitted_nullable_variable_without_default_leaves_argument_absent", func(t *testing.T) {
		// 变量没有值也没有默认值 → coercedValues 中无该变量；
		// §6.4.1 中 hasValue 为 false 且参数自身也无默认值 → 参数不进入 coercedValues。
		s25execRequireScalar(t,
			`query ($v: String) { echoNoDefault(value: $v) }`,
			nil, "echoNoDefault", "<missing>")
	})

	t.Run("omitted_non_null_variable_is_a_request_error", func(t *testing.T) {
		s25execRequireRequestError(t, `query ($v: Int!) { echoInt(value: $v) }`, nil)
	})

	t.Run("explicit_null_for_nullable_variable", func(t *testing.T) {
		s25execRequireScalar(t,
			`query ($v: String) { echoNoDefault(value: $v) }`,
			map[string]any{"v": nil}, "echoNoDefault", "<null>")
	})

	t.Run("explicit_null_for_non_null_variable_is_a_request_error", func(t *testing.T) {
		s25execRequireRequestError(t,
			`query ($v: Int!) { echoInt(value: $v) }`,
			map[string]any{"v": nil})
	})

	t.Run("explicit_null_overrides_variable_default", func(t *testing.T) {
		// hasValue 为 true，因此默认值分支不成立；值为 null → 写入 null。
		s25execRequireScalar(t,
			`query ($v: String = "varDefault") { echoNoDefault(value: $v) }`,
			map[string]any{"v": nil}, "echoNoDefault", "<null>")
	})

	t.Run("wrong_scalar_type_is_a_request_error", func(t *testing.T) {
		// §3.5.1：Int 输入只接受整数，含数字内容的字符串也必须报错。
		s25execRequireRequestError(t,
			`query ($v: Int!) { echoInt(value: $v) }`,
			map[string]any{"v": "not-an-int"})
	})

	t.Run("single_value_is_coerced_into_a_list", func(t *testing.T) {
		// §3.11.1 List 输入协变：值不是列表时，按单元素列表协变。
		s25execRequireScalar(t,
			`query ($v: [Int!]) { echoList(values: $v) }`,
			map[string]any{"v": 5}, "echoList", "[5]")
	})

	t.Run("nested_list_variable", func(t *testing.T) {
		s25execRequireScalar(t,
			`query ($v: [[Int!]!]!) { echoNestedList(values: $v) }`,
			map[string]any{"v": []any{[]any{1, 2}, []any{3}}},
			"echoNestedList", "[[1 2] [3]]")
	})

	t.Run("nested_list_single_value_is_coerced_at_every_level", func(t *testing.T) {
		// 单值 1 对 [[Int!]!]!：外层按单元素列表协变，内层再按单元素列表协变。
		s25execRequireScalar(t,
			`query ($v: [[Int!]!]!) { echoNestedList(values: $v) }`,
			map[string]any{"v": 1},
			"echoNestedList", "[[1]]")
	})

	t.Run("input_object_field_omitted_uses_input_field_default", func(t *testing.T) {
		// §3.11.2：未提供值且字段定义有默认值 → 使用默认值。
		s25execRequireScalar(t,
			`query ($f: S25Filter!) { echoFilter(filter: $f) }`,
			map[string]any{"f": map[string]any{"text": "t"}},
			"echoFilter", "text=t|count=7|mode=A|tags=<missing>|nested=<missing>")
	})

	t.Run("input_object_field_explicit_null_stays_null", func(t *testing.T) {
		// §3.11.2：显式给出 null 且字段可空 → 条目值为 null，不回退到默认值。
		s25execRequireScalar(t,
			`query ($f: S25Filter!) { echoFilter(filter: $f) }`,
			map[string]any{"f": map[string]any{"text": "t", "count": nil}},
			"echoFilter", "text=t|count=<null>|mode=A|tags=<missing>|nested=<missing>")
	})

	t.Run("nested_input_object_field_omitted_uses_default", func(t *testing.T) {
		s25execRequireScalar(t,
			`query ($f: S25Filter!) { echoFilter(filter: $f) }`,
			map[string]any{"f": map[string]any{"text": "t", "nested": map[string]any{}}},
			"echoFilter", "text=t|count=7|mode=A|tags=<missing>|nested=map[flag:false]")
	})

	t.Run("nested_input_object_field_explicit_null_stays_null", func(t *testing.T) {
		s25execRequireScalar(t,
			`query ($f: S25Filter!) { echoFilter(filter: $f) }`,
			map[string]any{"f": map[string]any{"text": "t", "nested": map[string]any{"flag": nil}}},
			"echoFilter", "text=t|count=7|mode=A|tags=<missing>|nested=map[flag:<nil>]")
	})

	t.Run("unknown_input_object_field_is_a_request_error", func(t *testing.T) {
		// §3.11.2：输入对象不得包含该类型未定义的字段名，否则必须抛 request error。
		s25execRequireRequestError(t,
			`query ($f: S25Filter!) { echoFilter(filter: $f) }`,
			map[string]any{"f": map[string]any{"text": "t", "bogus": 1}})
	})

	t.Run("invalid_enum_value_is_a_request_error", func(t *testing.T) {
		// §3.9 Enum 输入协变：值不是该枚举的成员名 → request error。
		s25execRequireRequestError(t,
			`query ($f: S25Filter!) { echoFilter(filter: $f) }`,
			map[string]any{"f": map[string]any{"text": "t", "mode": "NOPE"}})
	})
}

// ---------------------------------------------------------------------------
// §6.1.2 + §3.5 内置标量的变量输入协变边界
// ---------------------------------------------------------------------------

func TestSpec2025_Execution_BuiltInScalarVariableCoercionBoundaries(t *testing.T) {
	t.Run("int_max_int32", func(t *testing.T) {
		s25execRequireScalar(t, `query ($v: Int!) { echoInt(value: $v) }`,
			map[string]any{"v": 2147483647}, "echoInt", "2147483647")
	})

	t.Run("int_min_int32", func(t *testing.T) {
		s25execRequireScalar(t, `query ($v: Int!) { echoInt(value: $v) }`,
			map[string]any{"v": -2147483648}, "echoInt", "-2147483648")
	})

	t.Run("float_from_integer", func(t *testing.T) {
		// §3.5.2：整数输入被加上空小数部分协变为 Float。
		s25execRequireScalar(t, `query ($v: Float) { echoFloat(value: $v) }`,
			map[string]any{"v": 3}, "echoFloat", "3")
	})

	t.Run("id_from_integer", func(t *testing.T) {
		result := s25Do(t, s25Request{
			Schema:    s25execCore(t),
			Query:     `query ($v: ID) { echoID(value: $v) }`,
			Variables: map[string]any{"v": 7},
		})
		s25RequireNoErrors(t, result)
		got, _ := s25DataMap(t, result)["echoID"].(string)
		if value := s25execIDValue(t, got); value != "7" {
			t.Fatalf("ID coerced from integer 7 = %q, want value 7 (raw=%q)", value, got)
		}
	})

	t.Run("id_from_string", func(t *testing.T) {
		result := s25Do(t, s25Request{
			Schema:    s25execCore(t),
			Query:     `query ($v: ID) { echoID(value: $v) }`,
			Variables: map[string]any{"v": "abc"},
		})
		s25RequireNoErrors(t, result)
		got, _ := s25DataMap(t, result)["echoID"].(string)
		if value := s25execIDValue(t, got); value != "abc" {
			t.Fatalf("ID coerced from string \"abc\" = %q, want value abc (raw=%q)", value, got)
		}
	})

	t.Run("boolean_true", func(t *testing.T) {
		s25execRequireScalar(t, `query ($v: Boolean) { echoBool(value: $v) }`,
			map[string]any{"v": true}, "echoBool", "true")
	})

	t.Run("boolean_false", func(t *testing.T) {
		s25execRequireScalar(t, `query ($v: Boolean) { echoBool(value: $v) }`,
			map[string]any{"v": false}, "echoBool", "false")
	})

	t.Run("string_empty", func(t *testing.T) {
		s25execRequireScalar(t, `query ($v: String) { echoNoDefault(value: $v) }`,
			map[string]any{"v": ""}, "echoNoDefault", "")
	})

	// §3.5.1 Int：只接受整数输入；越界必须抛 request error。
	t.Run("int_from_boolean_is_a_request_error", func(t *testing.T) {
		s25execRequireRequestError(t, `query ($v: Int!) { echoInt(value: $v) }`,
			map[string]any{"v": true})
	})

	t.Run("int_from_numeric_string_is_a_request_error", func(t *testing.T) {
		s25execRequireRequestError(t, `query ($v: Int!) { echoInt(value: $v) }`,
			map[string]any{"v": "5"})
	})

	t.Run("int_from_fractional_number_is_a_request_error", func(t *testing.T) {
		s25execRequireRequestError(t, `query ($v: Int!) { echoInt(value: $v) }`,
			map[string]any{"v": 1.5})
	})

	t.Run("int_above_int32_is_a_request_error", func(t *testing.T) {
		s25execRequireRequestError(t, `query ($v: Int!) { echoInt(value: $v) }`,
			map[string]any{"v": int64(2147483648)})
	})

	t.Run("int_below_int32_is_a_request_error", func(t *testing.T) {
		s25execRequireRequestError(t, `query ($v: Int!) { echoInt(value: $v) }`,
			map[string]any{"v": int64(-2147483649)})
	})

	t.Run("float_from_string_is_a_request_error", func(t *testing.T) {
		// §3.5.2：含数字内容的字符串同样必须抛 request error。
		s25execRequireRequestError(t, `query ($v: Float) { echoFloat(value: $v) }`,
			map[string]any{"v": "1.5"})
	})

	t.Run("boolean_from_number_is_a_request_error", func(t *testing.T) {
		// §3.5.4：只接受布尔输入。
		s25execRequireRequestError(t, `query ($v: Boolean) { echoBool(value: $v) }`,
			map[string]any{"v": 1})
	})

	t.Run("string_from_number_is_a_request_error", func(t *testing.T) {
		// §3.5.3：只接受合法的 Unicode 字符串输入。
		s25execRequireRequestError(t, `query ($v: String) { echoNoDefault(value: $v) }`,
			map[string]any{"v": 3})
	})

	t.Run("id_from_boolean_is_a_request_error", func(t *testing.T) {
		// §3.5.5：ID 只接受字符串与整数。
		s25execRequireRequestError(t, `query ($v: ID) { echoID(value: $v) }`,
			map[string]any{"v": true})
	})
}

// ---------------------------------------------------------------------------
// §6.2.1 Query：根字段可并行执行
// ---------------------------------------------------------------------------

func TestSpec2025_Execution_QueryRootFieldsMayExecuteConcurrently(t *testing.T) {
	// §6.2.1 ExecuteQuery 以 "normal" 模式执行根选择集，
	// §6.3.4 Normal Execution 允许（并鼓励）并行执行同一 collected fields map 的条目。
	// 两个根字段互相等待：只有真正并行时双方才能完成。
	schema := s25execNewConcurrentSchema(t, 3*time.Second)
	result := s25Do(t, s25Request{Schema: schema, Query: `{ alpha beta }`})
	s25RequireNoErrors(t, result)
	data := s25DataMap(t, result)
	if data["alpha"] != "alpha" || data["beta"] != "beta" {
		t.Fatalf("both root fields must complete; data=%v", data)
	}
}

// ---------------------------------------------------------------------------
// §6.2.2 Mutation：根字段串行执行
// ---------------------------------------------------------------------------

func TestSpec2025_Execution_MutationRootFieldsExecuteSerially(t *testing.T) {
	t.Run("root_fields_and_their_subselections_complete_in_order", func(t *testing.T) {
		// §6.2.2 ExecuteMutation 以 "serial" 模式执行根选择集；
		// §6.3.4 Serial Execution 要求每个条目"完成"后才开始下一个条目，
		// 而 §6.4 ExecuteField 的完成包含递归执行其子选择集。
		var order []string
		var mu sync.Mutex
		schema := s25NewMutationSchema(t, &order, &mu)

		result := s25Do(t, s25Request{
			Schema: schema,
			Query:  `mutation { first { slow } second { slow } third { slow } }`,
		})
		s25RequireNoErrors(t, result)

		mu.Lock()
		got := append([]string(nil), order...)
		mu.Unlock()
		s25execRequireOrder(t, got, []string{
			"first", "child:first",
			"second", "child:second",
			"third", "child:third",
		})

		data := s25DataMap(t, result)
		want := map[string]any{
			"first":  map[string]any{"slow": "child:first"},
			"second": map[string]any{"slow": "child:second"},
			"third":  map[string]any{"slow": "child:third"},
		}
		if !reflect.DeepEqual(data, want) {
			t.Fatalf("data mismatch\n got: %#v\nwant: %#v", data, want)
		}
	})

	t.Run("failing_nullable_root_field_does_not_stop_later_root_fields", func(t *testing.T) {
		// §6.4.4：可空位置上的 execution error 被"处理"为该位置解析为 null，
		// 后续兄弟位置仍然必须执行。
		var order []string
		var mu sync.Mutex
		schema := s25NewMutationSchema(t, &order, &mu)

		result := s25Do(t, s25Request{
			Schema: schema,
			Query:  `mutation { failing first { id } }`,
		})
		s25RequireErrorPathSet(t, result, []any{"failing"})

		mu.Lock()
		got := append([]string(nil), order...)
		mu.Unlock()
		s25execRequireOrder(t, got, []string{"failing", "first"})

		data := s25DataMap(t, result)
		want := map[string]any{
			"failing": nil,
			"first":   map[string]any{"id": "first"},
		}
		if !reflect.DeepEqual(data, want) {
			t.Fatalf("data mismatch\n got: %#v\nwant: %#v", data, want)
		}
	})
}

// ---------------------------------------------------------------------------
// §6.2.3 Subscription
// ---------------------------------------------------------------------------

func TestSpec2025_Execution_Subscription(t *testing.T) {
	t.Run("each_source_event_is_mapped_through_execution", func(t *testing.T) {
		// §6.2.3 / §6.2.3.2：response stream 的每个事件都是对 source event
		// 执行 ExecuteSubscriptionEvent 的结果，因此 Resolve 必须被调用。
		schema := s25NewSubscriptionSchema(t)
		stream := Subscribe(Params{Schema: schema, RequestString: `subscription { ticks }`})
		if stream == nil {
			t.Fatalf("Subscribe returned a nil response stream")
		}

		deadline := time.After(10 * time.Second)
		for index := 0; index < 3; index++ {
			select {
			case result, ok := <-stream:
				if !ok {
					t.Fatalf("response stream closed after %d events, want 3", index)
				}
				s25RequireNoErrors(t, result)
				data := s25DataMap(t, result)
				want := map[string]any{"ticks": fmt.Sprintf("tick-%d", index)}
				if !reflect.DeepEqual(data, want) {
					t.Fatalf("event %d data mismatch\n got: %#v\nwant: %#v", index, data, want)
				}
			case <-deadline:
				t.Fatalf("timed out waiting for subscription event %d", index)
			}
		}

		select {
		case result, ok := <-stream:
			if ok {
				t.Fatalf("response stream produced a 4th event: %#v (errors=%v)",
					s25Plain(result.Data), s25ErrorMessages(result))
			}
		case <-deadline:
			t.Fatalf("timed out waiting for the response stream to complete")
		}
	})

	t.Run("subscription_document_is_a_valid_request", func(t *testing.T) {
		// §6.1.1：subscription 是合法的 operation；通过 Do 执行它不得
		// 表现为解析或校验失败。
		const query = `subscription { ticks }`
		s25RequireValid(t, s25NewSubscriptionSchema(t), query)

		result := s25Do(t, s25Request{Schema: s25NewSubscriptionSchema(t), Query: query})
		for _, err := range result.Errors {
			for _, shape := range s25execParseOrValidationShapes {
				if strings.Contains(err.Message, shape) {
					t.Fatalf("executing a subscription reported a parse/validation failure: %q", err.Message)
				}
			}
		}
	})
}

// ---------------------------------------------------------------------------
// §6.3.2 Field Collection
// ---------------------------------------------------------------------------

func TestSpec2025_Execution_FieldCollection(t *testing.T) {
	t.Run("repeated_response_name_is_executed_once", func(t *testing.T) {
		// §6.3.2：同名响应键的多次引用被收集进同一个 field set，只执行一次。
		counter := s25NewCounter()
		schema, _ := s25NewCoreSchema(t, counter)
		result := s25Do(t, s25Request{Schema: schema, Query: `{ greeting greeting greeting }`})
		s25RequireNoErrors(t, result)

		data := s25DataMap(t, result)
		want := map[string]any{"greeting": "hello"}
		if !reflect.DeepEqual(data, want) {
			t.Fatalf("data mismatch\n got: %#v\nwant: %#v", data, want)
		}
		if got := counter.Get("greeting"); got != 1 {
			t.Fatalf("greeting resolver ran %d times, want exactly 1", got)
		}
	})

	t.Run("merges_across_named_and_inline_fragments", func(t *testing.T) {
		counter := s25NewCounter()
		schema, _ := s25NewCoreSchema(t, counter)
		result := s25Do(t, s25Request{Schema: schema, Query: `
			{
				greeting
				...G
				... on Query { greeting }
			}
			fragment G on Query { greeting }
		`})
		s25RequireNoErrors(t, result)

		data := s25DataMap(t, result)
		want := map[string]any{"greeting": "hello"}
		if !reflect.DeepEqual(data, want) {
			t.Fatalf("data mismatch\n got: %#v\nwant: %#v", data, want)
		}
		if got := counter.Get("greeting"); got != 1 {
			t.Fatalf("greeting resolver ran %d times, want exactly 1", got)
		}
	})

	t.Run("skip_true_removes_the_selection_from_collection", func(t *testing.T) {
		// §6.3.2：@skip(if: true) 的选择直接 continue，不进入 collected fields map，
		// 因此 resolver 完全不执行，响应中也没有该键。
		counter := s25NewCounter()
		schema, _ := s25NewCoreSchema(t, counter)
		result := s25Do(t, s25Request{Schema: schema, Query: `{ greeting @skip(if: true) }`})
		s25RequireNoErrors(t, result)

		data := s25DataMap(t, result)
		if _, ok := data["greeting"]; ok {
			t.Fatalf("greeting must not appear in the response; data=%#v", data)
		}
		if got := counter.Get("greeting"); got != 0 {
			t.Fatalf("greeting resolver ran %d times, want 0", got)
		}
	})

	t.Run("include_false_occurrence_does_not_prevent_a_later_occurrence", func(t *testing.T) {
		counter := s25NewCounter()
		schema, _ := s25NewCoreSchema(t, counter)
		result := s25Do(t, s25Request{Schema: schema, Query: `{ greeting @include(if: false) greeting }`})
		s25RequireNoErrors(t, result)

		data := s25DataMap(t, result)
		want := map[string]any{"greeting": "hello"}
		if !reflect.DeepEqual(data, want) {
			t.Fatalf("data mismatch\n got: %#v\nwant: %#v", data, want)
		}
		if got := counter.Get("greeting"); got != 1 {
			t.Fatalf("greeting resolver ran %d times, want exactly 1", got)
		}
	})

	t.Run("skip_and_include_are_evaluated_from_variables", func(t *testing.T) {
		counter := s25NewCounter()
		schema, _ := s25NewCoreSchema(t, counter)
		result := s25Do(t, s25Request{
			Schema:    schema,
			Query:     `query ($no: Boolean!, $yes: Boolean!) { greeting @skip(if: $no) z @include(if: $yes) }`,
			Variables: map[string]any{"no": true, "yes": true},
		})
		s25RequireNoErrors(t, result)

		data := s25DataMap(t, result)
		want := map[string]any{"z": "z"}
		if !reflect.DeepEqual(data, want) {
			t.Fatalf("data mismatch\n got: %#v\nwant: %#v", data, want)
		}
		if got := counter.Get("greeting"); got != 0 {
			t.Fatalf("greeting resolver ran %d times, want 0", got)
		}
	})

	t.Run("sub_selections_of_merged_occurrences_are_unioned", func(t *testing.T) {
		// §6.3.2 CollectSubfields：同一 field set 中所有 field 的选择集
		// 合并为一个 collected fields map。
		schema := s25execCore(t)
		result := s25Do(t, s25Request{Schema: schema, Query: `
			{
				user { name }
				user { legacy }
				... on Query { user { id } }
			}
		`})
		s25RequireNoErrors(t, result)

		data := s25DataMap(t, result)
		want := map[string]any{"user": map[string]any{
			"name":   "Ada",
			"legacy": "legacy",
			"id":     "u1",
		}}
		if !reflect.DeepEqual(data, want) {
			t.Fatalf("data mismatch\n got: %#v\nwant: %#v", data, want)
		}
	})
}

// ---------------------------------------------------------------------------
// §6.3.3 Executing Collected Fields + §7.1.4 序列化顺序
// ---------------------------------------------------------------------------

func TestSpec2025_Execution_ResponsePositionFollowsCollectionOrder(t *testing.T) {
	t.Run("keys_follow_the_order_fields_appear_in_the_operation", func(t *testing.T) {
		// §6.3.3 的 Note：resultMap 按字段在 operation 中首次出现的顺序排列；
		// §7.1.4 要求序列化时保持这个顺序。
		result := s25Do(t, s25Request{Schema: s25execCore(t), Query: `{ z a m }`})
		s25RequireNoErrors(t, result)
		s25RequireKeyOrder(t, s25MarshalData(t, result), "z", "a", "m")
	})

	t.Run("position_comes_from_the_first_collected_occurrence", func(t *testing.T) {
		// §6.3.2：被 @skip(if: true) 跳过的选择根本不会创建 collected fields map 条目，
		// 因此响应键的位置由第一个真正被收集到的出现位置决定。
		result := s25Do(t, s25Request{
			Schema: s25execCore(t),
			Query:  `{ z @skip(if: true) a m z }`,
		})
		s25RequireNoErrors(t, result)

		encoded := s25MarshalData(t, result)
		s25RequireKeyOrder(t, encoded, "a", "m", "z")

		data := s25DataMap(t, result)
		want := map[string]any{"a": "a", "m": "m", "z": "z"}
		if !reflect.DeepEqual(data, want) {
			t.Fatalf("data mismatch\n got: %#v\nwant: %#v", data, want)
		}
	})

	t.Run("a_later_occurrence_still_leads_when_it_is_the_first_collected_one", func(t *testing.T) {
		// 第一个出现被跳过，第二个出现仍然排在 a / m 之前。
		result := s25Do(t, s25Request{
			Schema:    s25execCore(t),
			Query:     `query ($skip: Boolean!) { z @skip(if: $skip) z a m }`,
			Variables: map[string]any{"skip": true},
		})
		s25RequireNoErrors(t, result)
		s25RequireKeyOrder(t, s25MarshalData(t, result), "z", "a", "m")
	})
}

// ---------------------------------------------------------------------------
// §6.4.1 Coercing Field Arguments
// ---------------------------------------------------------------------------

func TestSpec2025_Execution_FieldArgumentCoercion(t *testing.T) {
	t.Run("literal_argument_wins_over_argument_default", func(t *testing.T) {
		s25execRequireScalar(t, `{ echo(text: "a", suffix: "b") }`, nil, "echo", "ab")
	})

	t.Run("variable_argument_wins_over_argument_default", func(t *testing.T) {
		s25execRequireScalar(t,
			`query ($s: String) { echo(text: "a", suffix: $s) }`,
			map[string]any{"s": "v"}, "echo", "av")
	})

	t.Run("argument_default_used_when_argument_is_omitted", func(t *testing.T) {
		// §6.4.1：hasValue 不为 true 且参数定义有默认值 → 使用参数默认值。
		s25execRequireScalar(t, `{ echo(text: "a") }`, nil, "echo", "a!")
	})

	t.Run("variable_default_used_when_variable_is_omitted", func(t *testing.T) {
		// 变量默认值在 §6.1.2 阶段已写入 variableValues，
		// 因此 §6.4.1 中 hasValue 为 true，取的是变量默认值而非参数默认值。
		s25execRequireScalar(t,
			`query ($s: String = "varDefault") { echo(text: "a", suffix: $s) }`,
			nil, "echo", "avarDefault")
	})

	t.Run("explicit_null_literal_yields_null_not_the_argument_default", func(t *testing.T) {
		s25execRequireScalar(t, `{ echo(text: "a", suffix: null) }`, nil, "echo", "a<null>")
	})

	t.Run("variable_holding_null_yields_null_not_the_argument_default", func(t *testing.T) {
		s25execRequireScalar(t,
			`query ($s: String) { echo(text: "a", suffix: $s) }`,
			map[string]any{"s": nil}, "echo", "a<null>")
	})

	t.Run("omitted_argument_uses_argument_default", func(t *testing.T) {
		s25execRequireScalar(t, `{ echoOptional }`, nil, "echoOptional", "argDefault")
	})

	t.Run("omitted_variable_falls_back_to_argument_default", func(t *testing.T) {
		// 变量无值且无变量默认值 → §6.1.2 不写入 coercedValues →
		// §6.4.1 中 hasValue 为 false → 使用参数默认值。
		s25execRequireScalar(t,
			`query ($v: String) { echoOptional(value: $v) }`,
			nil, "echoOptional", "argDefault")
	})

	t.Run("explicit_null_beats_argument_default_on_echoOptional", func(t *testing.T) {
		s25execRequireScalar(t, `{ echoOptional(value: null) }`, nil, "echoOptional", "<null>")
	})

	t.Run("variable_null_beats_argument_default_on_echoOptional", func(t *testing.T) {
		s25execRequireScalar(t,
			`query ($v: String) { echoOptional(value: $v) }`,
			map[string]any{"v": nil}, "echoOptional", "<null>")
	})
}

// ---------------------------------------------------------------------------
// §6.4.2 Value Resolution
// ---------------------------------------------------------------------------

func TestSpec2025_Execution_ValueResolution(t *testing.T) {
	t.Run("explicit_resolver", func(t *testing.T) {
		s25execRequireScalar(t, `{ greeting }`, nil, "greeting", "hello")
	})

	t.Run("default_resolution_branches", func(t *testing.T) {
		// §6.4.2 ResolveFieldValue 由实现自行决定如何从 objectValue 取值。
		schema := s25NewDefaultResolverSchema(t)
		result := s25Do(t, s25Request{
			Schema: schema,
			Query:  `{ fromMap { plain thunk } fromStruct { name age } fromResolver { value } }`,
		})
		s25RequireNoErrors(t, result)

		data := s25DataMap(t, result)
		want := map[string]any{
			"fromMap":      map[string]any{"plain": "plainValue", "thunk": "thunkValue"},
			"fromStruct":   map[string]any{"name": "Ada", "age": 36},
			"fromResolver": map[string]any{"value": "resolved:value"},
		}
		if !reflect.DeepEqual(data, want) {
			t.Fatalf("data mismatch\n got: %#v\nwant: %#v", data, want)
		}
	})

	t.Run("resolver_returning_nil_yields_null", func(t *testing.T) {
		result := s25Do(t, s25Request{Schema: s25execNewValueSchema(t), Query: `{ nilValue }`})
		s25RequireNoErrors(t, result)
		s25execRequireNullField(t, result, "nilValue")
	})

	t.Run("resolver_returning_a_typed_nil_pointer_yields_null", func(t *testing.T) {
		// §6.4.3：result 是 null（或类似 null 的内部值）时返回 null。
		result := s25Do(t, s25Request{
			Schema: s25execNewValueSchema(t),
			Query:  `{ typedNilString typedNilObject { name } }`,
		})
		s25RequireNoErrors(t, result)
		s25execRequireNullField(t, result, "typedNilString")
		s25execRequireNullField(t, result, "typedNilObject")
	})

	t.Run("resolver_returning_an_error_becomes_a_field_error", func(t *testing.T) {
		result := s25Do(t, s25Request{Schema: s25execCore(t), Query: `{ boom }`})
		s25RequireErrorPathSet(t, result, []any{"boom"})
		s25RequireErrorContaining(t, result, "boom failed")
		s25execRequireNullField(t, result, "boom")
	})

	t.Run("resolver_panicking_with_a_string_becomes_a_field_error", func(t *testing.T) {
		// panic 必须被捕获并转成该响应位置上的 execution error，绝不能终止进程。
		result := s25Do(t, s25Request{Schema: s25execCore(t), Query: `{ boomPanic greeting }`})
		s25RequireErrorPathSet(t, result, []any{"boomPanic"})
		s25execRequireNullField(t, result, "boomPanic")
		if got := s25DataMap(t, result)["greeting"]; got != "hello" {
			t.Fatalf("sibling field greeting = %#v, want \"hello\"", got)
		}
	})

	t.Run("resolver_panicking_with_an_error_becomes_a_field_error", func(t *testing.T) {
		result := s25Do(t, s25Request{Schema: s25execCore(t), Query: `{ boomPanicString greeting }`})
		s25RequireErrorPathSet(t, result, []any{"boomPanicString"})
		s25execRequireNullField(t, result, "boomPanicString")
		if got := s25DataMap(t, result)["greeting"]; got != "hello" {
			t.Fatalf("sibling field greeting = %#v, want \"hello\"", got)
		}
	})
}

// ---------------------------------------------------------------------------
// §6.4.3 Value Completion —— 叶子类型
// ---------------------------------------------------------------------------

func TestSpec2025_Execution_ValueCompletionLeaf(t *testing.T) {
	// §6.4.3 CoerceResult：内部方法必须返回该类型的合法值且不得为 null，
	// 否则抛 execution error；§6.4.4 该位置随后解析为 null。

	t.Run("int_out_of_32_bit_range_is_a_field_error", func(t *testing.T) {
		result := s25Do(t, s25Request{Schema: s25execCore(t), Query: `{ bigInt }`})
		s25RequireErrorPathSet(t, result, []any{"bigInt"})
		s25execRequireNullField(t, result, "bigInt")
	})

	t.Run("enum_value_outside_the_enum_is_a_field_error", func(t *testing.T) {
		result := s25Do(t, s25Request{Schema: s25execCore(t), Query: `{ badEnum }`})
		s25RequireErrorPathSet(t, result, []any{"badEnum"})
		s25execRequireNullField(t, result, "badEnum")
	})

	t.Run("custom_scalar_serialize_returning_nil_is_a_field_error", func(t *testing.T) {
		result := s25Do(t, s25Request{Schema: s25execCore(t), Query: `{ oddOut }`})
		s25RequireErrorPathSet(t, result, []any{"oddOut"})
		s25execRequireNullField(t, result, "oddOut")
	})

	t.Run("successful_leaf_coercions", func(t *testing.T) {
		result := s25Do(t, s25Request{
			Schema: s25execCore(t),
			Query:  `{ goodEnum floatOut boolOut idOut stringOut }`,
		})
		s25RequireNoErrors(t, result)
		data := s25DataMap(t, result)
		want := map[string]any{
			"goodEnum":  "A",
			"floatOut":  1.5,
			"boolOut":   true,
			"idOut":     "42",
			"stringOut": "",
		}
		if !reflect.DeepEqual(data, want) {
			t.Fatalf("data mismatch\n got: %#v\nwant: %#v", data, want)
		}
	})
}

// ---------------------------------------------------------------------------
// §6.4.3 Value Completion —— 列表类型
// ---------------------------------------------------------------------------

func TestSpec2025_Execution_ValueCompletionList(t *testing.T) {
	t.Run("collections_of_every_shape_complete_element_wise", func(t *testing.T) {
		result := s25Do(t, s25Request{
			Schema: s25execCore(t),
			Query: `{
				stringList
				emptyList
				nullList
				matrix
				typedList
				arrayList
				pointerList
				typedNilList
			}`,
		})
		s25RequireNoErrors(t, result)

		data := s25DataMap(t, result)
		want := map[string]any{
			"stringList":   []any{"a", nil, "c"},
			"emptyList":    []any{},
			"nullList":     nil,
			"matrix":       []any{[]any{"a", "b"}, []any{"c"}},
			"typedList":    []any{"a", "b"},
			"arrayList":    []any{1, 2, 3},
			"pointerList":  []any{"a", "b"},
			"typedNilList": nil,
		}
		if !reflect.DeepEqual(data, want) {
			t.Fatalf("data mismatch\n got: %#v\nwant: %#v", data, want)
		}
	})

	t.Run("non_collection_result_for_a_list_type_is_a_field_error", func(t *testing.T) {
		// §6.4.3：result 不是集合时抛 execution error（而不是 panic）。
		result := s25Do(t, s25Request{Schema: s25execCore(t), Query: `{ notAList greeting }`})
		s25RequireErrorPathSet(t, result, []any{"notAList"})
		s25execRequireNullField(t, result, "notAList")
		if got := s25DataMap(t, result)["greeting"]; got != "hello" {
			t.Fatalf("sibling field greeting = %#v, want \"hello\"", got)
		}
	})
}

// ---------------------------------------------------------------------------
// §6.4.3 Value Completion —— 抽象类型
// ---------------------------------------------------------------------------

func TestSpec2025_Execution_ValueCompletionAbstract(t *testing.T) {
	t.Run("interface_resolves_to_a_concrete_object_type", func(t *testing.T) {
		result := s25Do(t, s25Request{
			Schema: s25execCore(t),
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

		data := s25DataMap(t, result)
		want := map[string]any{"nodes": []any{
			map[string]any{"__typename": "S25User", "id": "u1", "name": "Ada"},
			map[string]any{"__typename": "S25Robot", "id": "r1", "serial": "RX-2"},
		}}
		if !reflect.DeepEqual(data, want) {
			t.Fatalf("data mismatch\n got: %#v\nwant: %#v", data, want)
		}
	})

	t.Run("union_resolves_to_a_concrete_object_type", func(t *testing.T) {
		result := s25Do(t, s25Request{
			Schema: s25execCore(t),
			Query: `{
				search {
					__typename
					... on S25User { id name }
					... on S25Robot { serial }
				}
			}`,
		})
		s25RequireNoErrors(t, result)

		data := s25DataMap(t, result)
		want := map[string]any{"search": []any{
			map[string]any{"__typename": "S25User", "id": "u1", "name": "Ada"},
			map[string]any{"__typename": "S25Robot", "serial": "RX-2"},
		}}
		if !reflect.DeepEqual(data, want) {
			t.Fatalf("data mismatch\n got: %#v\nwant: %#v", data, want)
		}
	})

	t.Run("runtime_type_outside_the_possible_types_is_a_field_error", func(t *testing.T) {
		// §6.4.3 ResolveAbstractType 必须给出该抽象类型的某个 possible type；
		// 做不到时该响应位置抛 execution error。列表元素可空 → 元素变成 null。
		result := s25Do(t, s25Request{Schema: s25execCore(t), Query: `{ badNode { id } }`})
		s25RequireErrorPathSet(t, result, []any{"badNode", 0})

		data := s25DataMap(t, result)
		want := map[string]any{"badNode": []any{nil}}
		if !reflect.DeepEqual(data, want) {
			t.Fatalf("data mismatch\n got: %#v\nwant: %#v", data, want)
		}
	})
}

// ---------------------------------------------------------------------------
// §6.4.3 / §6.4.4 空值冒泡
// ---------------------------------------------------------------------------

func TestSpec2025_Execution_NullBubbling(t *testing.T) {
	// 原生链路在 non-null 冒泡时会跨 goroutine panic 并终止测试进程，
	// 因此本用例在子进程中隔离执行；子进程失败仍按真实失败上报，不转为 skip。
	if !s25Isolated(t, "execution-null-bubbling") {
		return
	}

	t.Run("non_null_root_field_resolving_null_nulls_the_whole_data", func(t *testing.T) {
		// §6.4.4：从根到错误源全部是 Non-Null 时 data 应为 null。
		result := s25Do(t, s25Request{Schema: s25execCore(t), Query: `{ nonNullNull }`})
		s25execRequireNullData(t, result)
		s25RequireHasErrorPath(t, result, []any{"nonNullNull"})
	})

	t.Run("null_inside_a_non_null_item_list_nulls_the_whole_list", func(t *testing.T) {
		// §6.4.4：List 包裹 Non-Null 且某个元素解析为 null 时，整个列表位置解析为 null。
		result := s25Do(t, s25Request{Schema: s25execCore(t), Query: `{ nonNullItemList }`})
		s25RequireErrorPathSet(t, result, []any{"nonNullItemList", 1})
		s25execRequireNullField(t, result, "nonNullItemList")
	})

	t.Run("null_inside_a_nested_non_null_item_list_only_nulls_the_inner_list", func(t *testing.T) {
		// matrixWithNull 的类型是 [[String!]]，外层元素可空，
		// 因此只有内层列表变成 null，外层列表保留两个 null 元素。
		result := s25Do(t, s25Request{Schema: s25execCore(t), Query: `{ matrixWithNull }`})
		s25RequireErrorPathSet(t, result,
			[]any{"matrixWithNull", 0, 1},
			[]any{"matrixWithNull", 1, 0},
		)

		data := s25DataMap(t, result)
		want := map[string]any{"matrixWithNull": []any{nil, nil}}
		if !reflect.DeepEqual(data, want) {
			t.Fatalf("data mismatch\n got: %#v\nwant: %#v", data, want)
		}
	})

	t.Run("non_null_subfield_error_nulls_the_parent_but_not_its_siblings", func(t *testing.T) {
		result := s25Do(t, s25Request{Schema: s25execCore(t), Query: `{ user { mustFail } greeting }`})
		s25RequireErrorPathSet(t, result, []any{"user", "mustFail"})

		data := s25DataMap(t, result)
		want := map[string]any{"user": nil, "greeting": "hello"}
		if !reflect.DeepEqual(data, want) {
			t.Fatalf("data mismatch\n got: %#v\nwant: %#v", data, want)
		}
	})

	t.Run("non_null_list_resolving_nil_nulls_the_whole_data", func(t *testing.T) {
		// nonNullList 的类型是 [String]!，解析为 nil → 该位置的 Non-Null 抛错并冒泡到根。
		result := s25Do(t, s25Request{Schema: s25execCore(t), Query: `{ nonNullList }`})
		s25execRequireNullData(t, result)
		s25RequireHasErrorPath(t, result, []any{"nonNullList"})
	})
}

// ---------------------------------------------------------------------------
// §6.4.4 Handling Execution Errors
// ---------------------------------------------------------------------------

func TestSpec2025_Execution_ErrorHandling(t *testing.T) {
	// 原生链路在 non-null 冒泡时会跨 goroutine panic 并终止测试进程，
	// 因此本用例在子进程中隔离执行；子进程失败仍按真实失败上报，不转为 skip。
	if !s25Isolated(t, "execution_errorhandling") {
		return
	}

	t.Run("sibling_failures_produce_one_error_each_and_keep_partial_data", func(t *testing.T) {
		// §6.4.4：每个响应位置只添加一条错误；可空位置解析为 null，其余数据保留。
		result := s25Do(t, s25Request{Schema: s25execCore(t), Query: `{ boom boomExtended greeting }`})
		s25RequireErrorPathSet(t, result, []any{"boom"}, []any{"boomExtended"})

		data := s25DataMap(t, result)
		want := map[string]any{"boom": nil, "boomExtended": nil, "greeting": "hello"}
		if !reflect.DeepEqual(data, want) {
			t.Fatalf("data mismatch\n got: %#v\nwant: %#v", data, want)
		}
	})

	t.Run("failure_inside_a_list_keeps_sibling_items", func(t *testing.T) {
		schema, _ := s25NewCoreSchema(t, nil)
		result := s25Do(t, s25Request{
			Schema: schema,
			Query:  `{ people { failForBob } }`,
			Bindings: []FieldParamBinding{
				s25ParentBinding([]string{"people", "failForBob"}, "S25User", "failForBob",
					[]string{"people"}, "Query", "people"),
			},
		})
		s25RequireErrorPathSet(t, result, []any{"people", 1, "failForBob"})

		data := s25DataMap(t, result)
		want := map[string]any{"people": []any{
			map[string]any{"failForBob": "Ada"},
			map[string]any{"failForBob": nil},
			map[string]any{"failForBob": "Cid"},
		}}
		if !reflect.DeepEqual(data, want) {
			t.Fatalf("data mismatch\n got: %#v\nwant: %#v", data, want)
		}
	})

	t.Run("extensions_are_propagated", func(t *testing.T) {
		// §7.1.6：错误可以携带 extensions，实现必须原样透出。
		result := s25Do(t, s25Request{Schema: s25execCore(t), Query: `{ boomExtended }`})
		s25RequireErrorCount(t, result, 1)
		extensions := s25ErrorExtensions(result)
		want := map[string]any{"code": "S25_CODE"}
		if !reflect.DeepEqual(extensions, want) {
			t.Fatalf("error extensions = %#v, want %#v", extensions, want)
		}
	})

	t.Run("every_execution_error_carries_locations", func(t *testing.T) {
		// §7.1.6：错误必须包含 locations 指向导致错误的请求文本位置。
		result := s25Do(t, s25Request{Schema: s25execCore(t), Query: `{ boom user { mustFail } }`})
		s25RequireErrorCount(t, result, 2)
		s25RequireAllErrorsHaveLocations(t, result)
	})

	t.Run("error_path_uses_the_alias", func(t *testing.T) {
		// §7.1.6：path 由响应名（有别名时是别名）构成。
		result := s25Do(t, s25Request{Schema: s25execCore(t), Query: `{ renamed: boom }`})
		s25RequireErrorPathSet(t, result, []any{"renamed"})
		s25execRequireNullField(t, result, "renamed")
	})

	t.Run("error_path_uses_the_alias_inside_a_list", func(t *testing.T) {
		schema, _ := s25NewCoreSchema(t, nil)
		result := s25Do(t, s25Request{
			Schema: schema,
			Query:  `{ crowd: people { flag: failForBob } }`,
			Bindings: []FieldParamBinding{
				s25ParentBinding([]string{"crowd", "flag"}, "S25User", "failForBob",
					[]string{"crowd"}, "Query", "people"),
			},
		})
		s25RequireErrorPathSet(t, result, []any{"crowd", 1, "flag"})
	})
}

// ---------------------------------------------------------------------------
// §6.2 上下文传递与取消
// ---------------------------------------------------------------------------

func TestSpec2025_Execution_ContextPropagationAndCancellation(t *testing.T) {
	t.Run("context_value_reaches_every_resolver", func(t *testing.T) {
		ctx := context.WithValue(context.Background(), s25CtxKey{}, "ctxValue")
		result := s25Do(t, s25Request{
			Schema:  s25execCore(t),
			Query:   `{ first: ctxField second: ctxField }`,
			Context: ctx,
		})
		s25RequireNoErrors(t, result)

		data := s25DataMap(t, result)
		want := map[string]any{"first": "ctxValue", "second": "ctxValue"}
		if !reflect.DeepEqual(data, want) {
			t.Fatalf("data mismatch\n got: %#v\nwant: %#v", data, want)
		}
	})

	t.Run("already_cancelled_context_produces_an_error_result", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		done := make(chan *Result, 1)
		go func() {
			done <- s25Do(t, s25Request{
				Schema:  s25execCore(t),
				Query:   `{ greeting people { name } }`,
				Context: ctx,
			})
		}()

		select {
		case result := <-done:
			if len(result.Errors) == 0 {
				t.Fatalf("execution with a cancelled context reported no error; data=%#v", s25Plain(result.Data))
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("execution with a cancelled context hung")
		}
	})
}
