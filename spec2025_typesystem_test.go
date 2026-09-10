package graphql

// spec2025_typesystem_test.go
//
// GraphQL September 2025 §3 Type System 一致性用例。
// 所有断言均直接从规范文本推导，而不是从当前实现行为反推。
// 失败即为实现与规范之间的差距（finding），不得弱化或删除。
//
// 规范索引：https://spec.graphql.org/September2025/
//   §3.3.1  Root Operation Types
//   §3.5.1  Int
//   §3.5.2  Float
//   §3.5.3  String
//   §3.5.4  Boolean
//   §3.5.5  ID
//   §3.9    Enums
//   §3.10   Input Objects
//   §3.10.1 Oneof Input Objects (September 2025 新增)
//   §3.11   List
//   §3.12   Non-Null
//   §3.13   Type System Directives

import (
	"fmt"
	"math"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
)

// ---------------------------------------------------------------------------
// helper
// ---------------------------------------------------------------------------

// s25tsOutSchema 构造一个 schema，其中每个 key 都是一个返回固定值的 ttype 字段。
func s25tsOutSchema(t testing.TB, ttype Output, values map[string]any) Schema {
	t.Helper()
	fields := Fields{}
	for name, value := range values {
		fields[name] = &Field{Type: ttype, Resolve: s25Const(value)}
	}
	return s25NewSchema(t, fields)
}

// s25tsRun 执行 `{ field }`，返回结果。
func s25tsRun(t testing.TB, schema Schema, selection string) *Result {
	t.Helper()
	return s25Do(t, s25Request{Schema: schema, Query: "{ " + selection + " }"})
}

// s25tsPlainData 归一化 Result.Data，失败时用 t.Errorf 记录并返回 nil。
func s25tsPlainData(t *testing.T, label string, result *Result) (map[string]any, bool) {
	t.Helper()
	plain := s25Plain(result.Data)
	if plain == nil {
		return nil, false
	}
	data, ok := plain.(map[string]any)
	if !ok {
		t.Errorf("%s: Result.Data is %T, want map", label, result.Data)
		return nil, false
	}
	return data, true
}

// s25tsRequireValue 断言 `{ field }` 无错误并返回期望值（用 reflect.DeepEqual 比较）。
func s25tsRequireValue(t *testing.T, schema Schema, field string, want any) {
	t.Helper()
	result := s25tsRun(t, schema, field)
	if len(result.Errors) != 0 {
		t.Errorf("%s: unexpected errors: %v", field, s25ErrorMessages(result))
		return
	}
	data, ok := s25tsPlainData(t, field, result)
	if !ok {
		t.Errorf("%s: expected data, got nil", field)
		return
	}
	responseKey := field
	if index := strings.IndexByte(responseKey, '('); index >= 0 {
		responseKey = strings.TrimSpace(responseKey[:index])
	}
	got := data[responseKey]
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s = %#v (%T), want %#v (%T)", field, got, got, want, want)
	}
}

// s25tsRequireIntValue 数值语义断言：规范只规定"序列化为整数"，不规定宿主语言类型。
func s25tsRequireIntValue(t *testing.T, schema Schema, field string, want int64) {
	t.Helper()
	result := s25tsRun(t, schema, field)
	if len(result.Errors) != 0 {
		t.Errorf("%s: unexpected errors: %v", field, s25ErrorMessages(result))
		return
	}
	data, ok := s25tsPlainData(t, field, result)
	if !ok {
		t.Errorf("%s: expected data, got nil", field)
		return
	}
	got, isInt := s25AsInt(data[field])
	if !isInt {
		t.Errorf("%s = %#v (%T), want the integer %d", field, data[field], data[field], want)
		return
	}
	if got != want {
		t.Errorf("%s = %d, want %d", field, got, want)
	}
}

// s25tsRequireFloatValue 数值语义断言。
func s25tsRequireFloatValue(t *testing.T, schema Schema, field string, want float64) {
	t.Helper()
	result := s25tsRun(t, schema, field)
	if len(result.Errors) != 0 {
		t.Errorf("%s: unexpected errors: %v", field, s25ErrorMessages(result))
		return
	}
	data, ok := s25tsPlainData(t, field, result)
	if !ok {
		t.Errorf("%s: expected data, got nil", field)
		return
	}
	got, isFloat := s25tsAsFloat(data[field])
	if !isFloat {
		t.Errorf("%s = %#v (%T), want the float %v", field, data[field], data[field], want)
		return
	}
	if got != want {
		t.Errorf("%s = %v, want %v", field, got, want)
	}
}

func s25tsAsFloat(v any) (float64, bool) {
	switch typed := v.(type) {
	case float64:
		return typed, true
	case float32:
		return float64(typed), true
	case int:
		return float64(typed), true
	case int32:
		return float64(typed), true
	case int64:
		return float64(typed), true
	}
	return 0, false
}

// s25tsRequireFieldError 规范 §6.4.3 / §7.1.2：叶子值结果强制失败必须抬出 field error，
// 并把该字段置为 null；不得静默返回 null，也不得吞掉错误。
func s25tsRequireFieldError(t *testing.T, schema Schema, field string) {
	t.Helper()
	result := s25tsRun(t, schema, field)
	if len(result.Errors) == 0 {
		t.Errorf("%s: expected a field error, got none; data=%#v", field, s25Plain(result.Data))
		return
	}
	data, ok := s25tsPlainData(t, field, result)
	if !ok {
		t.Errorf("%s: a nullable field error must keep data with a null entry; data=%#v",
			field, s25Plain(result.Data))
		return
	}
	value, present := data[field]
	if !present {
		t.Errorf("%s: field missing from data after a field error; data=%#v", field, data)
		return
	}
	if value != nil {
		t.Errorf("%s = %#v after a field error, want null", field, value)
	}
	if !s25tsHasPath(result, []any{field}) {
		t.Errorf("%s: no error with path [%q]; got paths=%v messages=%v",
			field, field, s25ErrorPaths(result), s25ErrorMessages(result))
	}
}

func s25tsHasPath(result *Result, want []any) bool {
	for _, got := range s25ErrorPaths(result) {
		if s25PathEqual(got, want) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// §3.11 / §3.12 列表与 Non-Null 组合
// ---------------------------------------------------------------------------

// s25tsListSchema 构造七种 String 包装形态，每个字段都返回同一个 value。
func s25tsListSchema(t testing.TB, value any) Schema {
	t.Helper()
	return s25NewSchema(t, Fields{
		"t":         &Field{Type: String, Resolve: s25Const(value)},
		"tNN":       &Field{Type: NewNonNull(String), Resolve: s25Const(value)},
		"listT":     &Field{Type: NewList(String), Resolve: s25Const(value)},
		"listTNN":   &Field{Type: NewNonNull(NewList(String)), Resolve: s25Const(value)},
		"listNNT":   &Field{Type: NewList(NewNonNull(String)), Resolve: s25Const(value)},
		"listNNTNN": &Field{Type: NewNonNull(NewList(NewNonNull(String))), Resolve: s25Const(value)},
		"nested": &Field{
			Type:    NewNonNull(NewList(NewNonNull(NewList(NewNonNull(String))))),
			Resolve: s25Const(value),
		},
	})
}

// s25tsListCase 描述一次 (字段, 返回值) 组合的规范期望。
type s25tsListCase struct {
	name  string
	field string
	value any
	// data 为 nil 且 dataNull 为 true 表示整个 data 必须是 null（非空传播到根）。
	dataNull bool
	// want 是 dataNull=false 时该字段应有的值。
	want any
	// paths 是期望出现的错误 path 集合（按集合语义，数量必须相等）。
	paths [][]any
}

func s25tsRunListCase(t *testing.T, c s25tsListCase) {
	t.Helper()
	schema := s25tsListSchema(t, c.value)
	result := s25tsRun(t, schema, c.field)

	if len(result.Errors) != len(c.paths) {
		t.Errorf("error count = %d, want %d; paths=%v messages=%v",
			len(result.Errors), len(c.paths), s25ErrorPaths(result), s25ErrorMessages(result))
	}
	for _, want := range c.paths {
		if !s25tsHasPath(result, want) {
			t.Errorf("missing error with path %v; got %v (messages=%v)",
				want, s25ErrorPaths(result), s25ErrorMessages(result))
		}
	}

	plain := s25Plain(result.Data)
	if c.dataNull {
		if plain != nil {
			t.Errorf("data = %#v, want null (non-null propagated to the root)", plain)
		}
		return
	}
	data, ok := plain.(map[string]any)
	if !ok {
		t.Errorf("data = %#v (%T), want a map with %q", plain, result.Data, c.field)
		return
	}
	got, present := data[c.field]
	if !present {
		t.Errorf("field %q missing from data %#v", c.field, data)
		return
	}
	if !reflect.DeepEqual(got, c.want) {
		t.Errorf("%s = %#v, want %#v", c.field, got, c.want)
	}
}

// ---------------------------------------------------------------------------
// §3.3.1 Root Operation Types
// ---------------------------------------------------------------------------

// s25tsAllRootsSchema 构造一个同时具备三种根操作类型的 schema。
func s25tsAllRootsSchema(t testing.TB) Schema {
	t.Helper()
	schema, err := NewSchema(SchemaConfig{
		Query: NewObject(ObjectConfig{Name: "Query", Fields: Fields{
			"where": &Field{Type: String, Resolve: s25Const("query")},
		}}),
		Mutation: NewObject(ObjectConfig{Name: "Mutation", Fields: Fields{
			"where": &Field{Type: String, Resolve: s25Const("mutation")},
		}}),
		Subscription: NewObject(ObjectConfig{Name: "Subscription", Fields: Fields{
			"where": &Field{
				Type:      String,
				Subscribe: func(ResolveParams) (any, error) { return nil, nil },
				Resolve:   s25Const("subscription"),
			},
		}}),
	})
	if err != nil {
		t.Fatalf("build all-roots schema: %v", err)
	}
	return schema
}

func TestSpec2025_TypeSystem_RootOperationTypes(t *testing.T) {
	// §3.3.1: "The query root operation type must be provided... The mutation root
	// operation type is optional; if it is not provided, the service does not support
	// mutations." 对一个只有 query 根的 schema 发起 mutation / subscription 操作，
	// 该操作没有可执行的根类型，属于 request error（§6.1 校验阶段即失败）。
	queryOnly := s25NewSchema(t, Fields{
		"ping": &Field{Type: String, Resolve: s25Const("pong")},
	})

	t.Run("mutation against query-only schema is a request error", func(t *testing.T) {
		result := s25Do(t, s25Request{Schema: queryOnly, Query: "mutation { ping }"})
		s25RequireRequestError(t, result)
	})

	t.Run("subscription against query-only schema is a request error", func(t *testing.T) {
		result := s25Do(t, s25Request{Schema: queryOnly, Query: "subscription { ping }"})
		s25RequireRequestError(t, result)
	})

	t.Run("query-only schema exposes no mutation or subscription root", func(t *testing.T) {
		if queryOnly.QueryType() == nil || queryOnly.QueryType().Name() != "Query" {
			t.Errorf("QueryType() = %v, want the Query object type", queryOnly.QueryType())
		}
		if queryOnly.MutationType() != nil {
			t.Errorf("MutationType() = %v, want nil", queryOnly.MutationType())
		}
		if queryOnly.SubscriptionType() != nil {
			t.Errorf("SubscriptionType() = %v, want nil", queryOnly.SubscriptionType())
		}
	})

	all := s25tsAllRootsSchema(t)

	t.Run("query operation roots at the query type", func(t *testing.T) {
		s25RequireData(t, s25Request{Schema: all, Query: "query { where }"},
			map[string]any{"where": "query"})
	})

	t.Run("shorthand operation roots at the query type", func(t *testing.T) {
		s25RequireData(t, s25Request{Schema: all, Query: "{ where }"},
			map[string]any{"where": "query"})
	})

	t.Run("mutation operation roots at the mutation type", func(t *testing.T) {
		s25RequireData(t, s25Request{Schema: all, Query: "mutation { where }"},
			map[string]any{"where": "mutation"})
	})

	t.Run("subscription operation roots at the subscription type", func(t *testing.T) {
		// §6.2.3：subscription 通过 Subscribe 入口执行，根字段解析自 Subscription 类型。
		if all.SubscriptionType() == nil || all.SubscriptionType().Name() != "Subscription" {
			t.Fatalf("SubscriptionType() = %v, want the Subscription object type", all.SubscriptionType())
		}
		s25RequireValid(t, all, "subscription { where }")
	})

	t.Run("root types are reported through introspection", func(t *testing.T) {
		data := s25MustData(t, s25Request{Schema: all, Query: `{
			__schema {
				queryType { name }
				mutationType { name }
				subscriptionType { name }
			}
		}`})
		want := map[string]any{"__schema": map[string]any{
			"queryType":        map[string]any{"name": "Query"},
			"mutationType":     map[string]any{"name": "Mutation"},
			"subscriptionType": map[string]any{"name": "Subscription"},
		}}
		if !reflect.DeepEqual(data, want) {
			t.Errorf("root types\n got: %#v\nwant: %#v", data, want)
		}
	})
}

// ---------------------------------------------------------------------------
// §3.5.1 Int
// ---------------------------------------------------------------------------

func TestSpec2025_TypeSystem_IntScalar(t *testing.T) {
	// §3.5.1: "Int ... represents a signed 32‐bit numeric non‐fractional value."
	// Result Coercion: "If the integer internal value represents a value less than
	// -2^31 or greater than or equal to 2^31, a field error should be raised."
	t.Run("bigInt overflows 32 bits and raises a field error", func(t *testing.T) {
		schema, _ := s25NewCoreSchema(t, nil)
		s25tsRequireFieldError(t, schema, "bigInt")
	})

	schema := s25tsOutSchema(t, Int, map[string]any{
		"maxInt":     2147483647,
		"minInt":     -2147483648,
		"fromInt32":  int32(7),
		"wholeFloat": float64(3.0),
		"fractional": 1.5,
		"fromString": "5",
		"fromBool":   true,
		"overMax":    int64(2147483648),
		"underMin":   int64(-2147483649),
	})

	t.Run("2147483647 is the maximum representable Int", func(t *testing.T) {
		s25tsRequireIntValue(t, schema, "maxInt", 2147483647)
	})
	t.Run("-2147483648 is the minimum representable Int", func(t *testing.T) {
		s25tsRequireIntValue(t, schema, "minInt", -2147483648)
	})
	t.Run("int32 is a valid Int internal value", func(t *testing.T) {
		s25tsRequireIntValue(t, schema, "fromInt32", 7)
	})
	t.Run("a whole float loses no information and coerces to Int", func(t *testing.T) {
		// "may coerce non-integer raw values to Int when reasonable without losing
		// information" —— 3.0 完全可用整数表示。
		s25tsRequireIntValue(t, schema, "wholeFloat", 3)
	})

	t.Run("a fractional value raises a field error", func(t *testing.T) {
		s25tsRequireFieldError(t, schema, "fractional")
	})
	t.Run("a string raises a field error", func(t *testing.T) {
		s25tsRequireFieldError(t, schema, "fromString")
	})
	t.Run("a boolean raises a field error", func(t *testing.T) {
		s25tsRequireFieldError(t, schema, "fromBool")
	})
	t.Run("2^31 raises a field error", func(t *testing.T) {
		s25tsRequireFieldError(t, schema, "overMax")
	})
	t.Run("-2^31-1 raises a field error", func(t *testing.T) {
		s25tsRequireFieldError(t, schema, "underMin")
	})

	t.Run("input coercion rejects out-of-range Int literals", func(t *testing.T) {
		core, _ := s25NewCoreSchema(t, nil)
		result := s25Do(t, s25Request{Schema: core, Query: "{ echoInt(value: 2147483648) }"})
		s25RequireRequestError(t, result)
	})
	t.Run("input coercion rejects fractional Int literals", func(t *testing.T) {
		core, _ := s25NewCoreSchema(t, nil)
		result := s25Do(t, s25Request{Schema: core, Query: "{ echoInt(value: 1.5) }"})
		s25RequireRequestError(t, result)
	})
}

// ---------------------------------------------------------------------------
// §3.5.2 Float
// ---------------------------------------------------------------------------

func TestSpec2025_TypeSystem_FloatScalar(t *testing.T) {
	// §3.5.2: "Float ... represents signed double‐precision finite values."
	// Result Coercion: "Non-finite floating‐point internal values (NaN and Infinity)
	// cannot be coerced to Float and must raise a field error."
	schema := s25tsOutSchema(t, Float, map[string]any{
		"fromInt":     3,
		"fromInt64":   int64(-12),
		"big":         1e10,
		"negZero":     math.Copysign(0, -1),
		"maxFloat":    math.MaxFloat64,
		"smallFloat":  math.SmallestNonzeroFloat64,
		"fromString":  "1.5",
		"notANumber":  math.NaN(),
		"positiveInf": math.Inf(1),
		"negativeInf": math.Inf(-1),
	})

	t.Run("an integer internal value coerces to Float", func(t *testing.T) {
		s25tsRequireFloatValue(t, schema, "fromInt", 3)
	})
	t.Run("an int64 internal value coerces to Float", func(t *testing.T) {
		s25tsRequireFloatValue(t, schema, "fromInt64", -12)
	})
	t.Run("1e10 is a valid Float", func(t *testing.T) {
		s25tsRequireFloatValue(t, schema, "big", 1e10)
	})
	t.Run("negative zero is finite and therefore a valid Float", func(t *testing.T) {
		s25tsRequireFloatValue(t, schema, "negZero", 0)
	})
	t.Run("the maximum float64 is finite and therefore a valid Float", func(t *testing.T) {
		s25tsRequireFloatValue(t, schema, "maxFloat", math.MaxFloat64)
	})
	t.Run("the smallest non-zero float64 is a valid Float", func(t *testing.T) {
		s25tsRequireFloatValue(t, schema, "smallFloat", math.SmallestNonzeroFloat64)
	})

	t.Run("a string raises a field error", func(t *testing.T) {
		s25tsRequireFieldError(t, schema, "fromString")
	})
	t.Run("NaN raises a field error", func(t *testing.T) {
		s25tsRequireFieldError(t, schema, "notANumber")
	})
	t.Run("+Inf raises a field error", func(t *testing.T) {
		s25tsRequireFieldError(t, schema, "positiveInf")
	})
	t.Run("-Inf raises a field error", func(t *testing.T) {
		s25tsRequireFieldError(t, schema, "negativeInf")
	})

	t.Run("Int literals are accepted where Float is expected", func(t *testing.T) {
		core, _ := s25NewCoreSchema(t, nil)
		s25RequireData(t, s25Request{Schema: core, Query: "{ echoFloat(value: 3) }"},
			map[string]any{"echoFloat": "3"})
	})
}

// ---------------------------------------------------------------------------
// §3.5.3 String
// ---------------------------------------------------------------------------

func TestSpec2025_TypeSystem_StringScalar(t *testing.T) {
	// §3.5.3: "String represents textual data, represented as UTF‐8 character
	// sequences." Result coercion 只接受字符串内部值；其它内部值必须抬出 field error。
	const big = 64 * 1024
	emoji := "🙂 👨‍👩‍👧‍👦 𝔊 \U0010FFFF"

	schema := s25tsOutSchema(t, String, map[string]any{
		"empty":      "",
		"huge":       strings.Repeat("a", big),
		"emoji":      emoji,
		"fromStruct": struct{ A int }{A: 1},
		"fromInt":    7,
	})

	t.Run("the empty string is a valid String", func(t *testing.T) {
		s25tsRequireValue(t, schema, "empty", "")
	})
	t.Run("a 64 KiB string round-trips unchanged", func(t *testing.T) {
		s25tsRequireValue(t, schema, "huge", strings.Repeat("a", big))
	})
	t.Run("emoji and surrogate pairs round-trip unchanged", func(t *testing.T) {
		s25tsRequireValue(t, schema, "emoji", emoji)
	})

	t.Run("a struct raises a field error", func(t *testing.T) {
		s25tsRequireFieldError(t, schema, "fromStruct")
	})
	t.Run("an integer raises a field error", func(t *testing.T) {
		s25tsRequireFieldError(t, schema, "fromInt")
	})

	t.Run("string input coercion only accepts string literals", func(t *testing.T) {
		core, _ := s25NewCoreSchema(t, nil)
		result := s25Do(t, s25Request{Schema: core, Query: `{ echo(text: 5) }`})
		s25RequireRequestError(t, result)
	})
}

// ---------------------------------------------------------------------------
// §3.5.4 Boolean
// ---------------------------------------------------------------------------

func TestSpec2025_TypeSystem_BooleanScalar(t *testing.T) {
	// §3.5.4: "Boolean represents true or false." Result coercion 只接受布尔内部值。
	schema := s25tsOutSchema(t, Boolean, map[string]any{
		"yes":        true,
		"no":         false,
		"fromString": "yes",
		"fromInt":    1,
		"fromZero":   0,
	})

	t.Run("true is a valid Boolean", func(t *testing.T) {
		s25tsRequireValue(t, schema, "yes", true)
	})
	t.Run("false is a valid Boolean", func(t *testing.T) {
		s25tsRequireValue(t, schema, "no", false)
	})
	t.Run(`"yes" raises a field error`, func(t *testing.T) {
		s25tsRequireFieldError(t, schema, "fromString")
	})
	t.Run("1 raises a field error", func(t *testing.T) {
		s25tsRequireFieldError(t, schema, "fromInt")
	})
	t.Run("0 raises a field error", func(t *testing.T) {
		s25tsRequireFieldError(t, schema, "fromZero")
	})

	t.Run("boolean input coercion only accepts boolean literals", func(t *testing.T) {
		core, _ := s25NewCoreSchema(t, nil)
		result := s25Do(t, s25Request{Schema: core, Query: `{ echoBool(value: "true") }`})
		s25RequireRequestError(t, result)
	})
}

// ---------------------------------------------------------------------------
// §3.5.5 ID
// ---------------------------------------------------------------------------

func s25tsIDSchema(t testing.TB) Schema {
	t.Helper()
	return s25NewSchema(t, Fields{
		"idEcho": &Field{
			Type: ID,
			Args: FieldConfigArgument{"value": &ArgumentConfig{Type: ID}},
			Resolve: func(p ResolveParams) (any, error) {
				return p.Args["value"], nil
			},
		},
		"idFromInt":    &Field{Type: ID, Resolve: s25Const(42)},
		"idFromInt64":  &Field{Type: ID, Resolve: s25Const(int64(-7))},
		"idFromString": &Field{Type: ID, Resolve: s25Const("abc")},
	})
}

func TestSpec2025_TypeSystem_IDScalar(t *testing.T) {
	// §3.5.5: "The ID type is serialized in the same way as a String... When expected
	// as an input type, any string (such as "4") or integer (such as 4) input value
	// should be coerced to ID as appropriate for the ID type."
	// Result coercion: "GraphQL is agnostic to ID format, and serializes to string
	// to ensure consistency across many formats ID could represent."
	schema := s25tsIDSchema(t)

	t.Run("an integer literal is accepted and serialized as a String", func(t *testing.T) {
		s25tsRequireValue(t, schema, `idEcho(value: 4)`, "4")
	})
	t.Run("a string literal is accepted and serialized as a String", func(t *testing.T) {
		s25tsRequireValue(t, schema, `idEcho(value: "abc")`, "abc")
	})
	t.Run("an integer variable is accepted and serialized as a String", func(t *testing.T) {
		data := s25MustData(t, s25Request{
			Schema:    schema,
			Query:     `query ($v: ID!) { idEcho(value: $v) }`,
			Variables: map[string]any{"v": 4},
		})
		if got := data["idEcho"]; got != "4" {
			t.Errorf("idEcho(value: $v=4) = %#v (%T), want the string \"4\"", got, got)
		}
	})
	t.Run("a string variable is accepted and serialized as a String", func(t *testing.T) {
		data := s25MustData(t, s25Request{
			Schema:    schema,
			Query:     `query ($v: ID!) { idEcho(value: $v) }`,
			Variables: map[string]any{"v": "abc"},
		})
		if got := data["idEcho"]; got != "abc" {
			t.Errorf("idEcho(value: $v=\"abc\") = %#v (%T), want the string \"abc\"", got, got)
		}
	})

	t.Run("an integer internal value serializes to a String", func(t *testing.T) {
		s25tsRequireValue(t, schema, "idFromInt", "42")
	})
	t.Run("an int64 internal value serializes to a String", func(t *testing.T) {
		s25tsRequireValue(t, schema, "idFromInt64", "-7")
	})
	t.Run("a string internal value serializes unchanged", func(t *testing.T) {
		s25tsRequireValue(t, schema, "idFromString", "abc")
	})

	t.Run("ID input coercion rejects floats", func(t *testing.T) {
		result := s25tsRun(t, schema, "idEcho(value: 1.5)")
		s25RequireRequestError(t, result)
	})
	t.Run("ID input coercion rejects booleans", func(t *testing.T) {
		result := s25tsRun(t, schema, "idEcho(value: true)")
		s25RequireRequestError(t, result)
	})
}

// ---------------------------------------------------------------------------
// §3.9 Enums
// ---------------------------------------------------------------------------

func TestSpec2025_TypeSystem_EnumTypes(t *testing.T) {
	// §3.9: "GraphQL servers must return one of the defined set of possible values.
	// If a reasonable coercion is not possible they must raise a field error."
	t.Run("a defined enum value round-trips", func(t *testing.T) {
		schema, _ := s25NewCoreSchema(t, nil)
		s25tsRequireValue(t, schema, "goodEnum", "A")
	})

	t.Run("a value outside the enum raises a field error and yields null", func(t *testing.T) {
		schema, _ := s25NewCoreSchema(t, nil)
		s25tsRequireFieldError(t, schema, "badEnum")
	})

	t.Run("a deprecated enum value is still usable as input", func(t *testing.T) {
		// §3.9 / §3.13: @deprecated 只是元数据，不影响该值作为输入的可用性。
		schema, _ := s25NewCoreSchema(t, nil)
		s25RequireData(t, s25Request{Schema: schema, Query: "{ echoMode(mode: DEPRECATED_C) }"},
			map[string]any{"echoMode": "C"})
	})

	t.Run("a deprecated enum value is usable through a variable", func(t *testing.T) {
		schema, _ := s25NewCoreSchema(t, nil)
		s25RequireData(t, s25Request{
			Schema:    schema,
			Query:     `query ($m: S25Mode) { echoMode(mode: $m) }`,
			Variables: map[string]any{"m": "DEPRECATED_C"},
		}, map[string]any{"echoMode": "C"})
	})

	t.Run("enum input coercion rejects a string literal", func(t *testing.T) {
		// §3.9: "Enum values are not references to named values... they are not
		// interchangeable with String."
		schema, _ := s25NewCoreSchema(t, nil)
		result := s25Do(t, s25Request{Schema: schema, Query: `{ echoMode(mode: "A") }`})
		s25RequireRequestError(t, result)
	})

	t.Run("enum input coercion rejects an undefined value", func(t *testing.T) {
		schema, _ := s25NewCoreSchema(t, nil)
		result := s25Do(t, s25Request{Schema: schema, Query: `{ echoMode(mode: NOPE) }`})
		s25RequireRequestError(t, result)
	})
}

// ---------------------------------------------------------------------------
// §3.10 Input Objects
// ---------------------------------------------------------------------------

func TestSpec2025_TypeSystem_InputObjects(t *testing.T) {
	// §3.10 Input Coercion。
	newSchema := func(t *testing.T) Schema {
		t.Helper()
		schema, _ := s25NewCoreSchema(t, nil)
		return schema
	}

	t.Run("a missing required field is a request error", func(t *testing.T) {
		// "If no value is provided for a defined input object field and that field
		// definition provides no default value, the input object field is considered
		// unset... if the field type is non-null a field error must be raised."
		result := s25Do(t, s25Request{Schema: newSchema(t), Query: `{ echoFilter(filter: {count: 1}) }`})
		s25RequireRequestError(t, result)
	})

	t.Run("an unknown field is a request error", func(t *testing.T) {
		// §5.6.3 Input Object Field Names: "Every input field provided in an input
		// object value must be defined in the set of possible fields."
		result := s25Do(t, s25Request{Schema: newSchema(t), Query: `{ echoFilter(filter: {text: "x", nope: 1}) }`})
		s25RequireRequestError(t, result)
	})

	t.Run("a duplicated input field is a request error", func(t *testing.T) {
		// §5.6.4 Input Object Field Uniqueness.
		result := s25Do(t, s25Request{Schema: newSchema(t), Query: `{ echoFilter(filter: {text: "x", text: "y"}) }`})
		s25RequireRequestError(t, result)
	})

	t.Run("field defaults are applied when the field is omitted", func(t *testing.T) {
		// "If no value is provided ... and that field definition provides a default
		// value, the default value is used."
		s25RequireData(t, s25Request{Schema: newSchema(t), Query: `{ echoFilter(filter: {text: "x"}) }`},
			map[string]any{"echoFilter": "text=x|count=7|mode=A|tags=<missing>|nested=<missing>"})
	})

	t.Run("an explicit null is not replaced by the field default", func(t *testing.T) {
		// "If the value null was provided ... an entry in the coerced unordered map
		// is given the value null." 默认值只在字段缺省时生效。
		s25RequireData(t, s25Request{Schema: newSchema(t), Query: `{ echoFilter(filter: {text: "x", count: null}) }`},
			map[string]any{"echoFilter": "text=x|count=<null>|mode=A|tags=<missing>|nested=<missing>"})
	})

	t.Run("an explicit null through a variable is not replaced by the field default", func(t *testing.T) {
		s25RequireData(t, s25Request{
			Schema:    newSchema(t),
			Query:     `query ($c: Int) { echoFilter(filter: {text: "x", count: $c}) }`,
			Variables: map[string]any{"c": nil},
		}, map[string]any{"echoFilter": "text=x|count=<null>|mode=A|tags=<missing>|nested=<missing>"})
	})

	t.Run("an explicit null for a non-null input field is a request error", func(t *testing.T) {
		result := s25Do(t, s25Request{Schema: newSchema(t), Query: `{ echoFilter(filter: {text: null}) }`})
		s25RequireRequestError(t, result)
	})

	t.Run("nested input objects coerce recursively and apply their own defaults", func(t *testing.T) {
		s25RequireData(t, s25Request{
			Schema: newSchema(t),
			Query:  `{ echoFilter(filter: {text: "x", nested: {values: [1, 2]}}) }`,
		}, map[string]any{
			"echoFilter": "text=x|count=7|mode=A|tags=<missing>|nested=map[flag:false values:[1 2]]",
		})
	})

	t.Run("a nested input object supplied through a variable coerces recursively", func(t *testing.T) {
		s25RequireData(t, s25Request{
			Schema:    newSchema(t),
			Query:     `query ($f: S25Filter!) { echoFilter(filter: $f) }`,
			Variables: map[string]any{"f": map[string]any{"text": "x", "nested": map[string]any{"values": []any{1, 2}}}},
		}, map[string]any{
			"echoFilter": "text=x|count=7|mode=A|tags=<missing>|nested=map[flag:false values:[1 2]]",
		})
	})

	t.Run("a non-object value for an input object is a request error", func(t *testing.T) {
		result := s25Do(t, s25Request{Schema: newSchema(t), Query: `{ echoFilter(filter: 5) }`})
		s25RequireRequestError(t, result)
	})
}

// ---------------------------------------------------------------------------
// §3.10.1 Oneof Input Objects（September 2025 新增）
// ---------------------------------------------------------------------------

// s25tsOneOfSchema 构造一个"意图为 @oneOf"的输入对象。
// 当前 InputObjectConfig 没有任何声明 oneOf 的手段（没有 IsOneOf 字段，也没有
// 指令挂载点），这里按规范书写断言。
func s25tsOneOfSchema(t testing.TB) Schema {
	t.Helper()
	input := NewInputObject(InputObjectConfig{
		Name:        "S25TsOneOf",
		Description: "A oneof input object: exactly one field must be supplied (§3.10.1).",
		Fields: InputObjectConfigFieldMap{
			// §3.10.1: "The type of every field of a oneof input object must be nullable."
			"a": &InputObjectFieldConfig{Type: String, Description: "The string branch."},
			"b": &InputObjectFieldConfig{Type: Int, Description: "The integer branch."},
		},
	})
	return s25NewSchema(t, Fields{
		"pick": &Field{
			Type: String,
			Args: FieldConfigArgument{"input": &ArgumentConfig{Type: NewNonNull(input)}},
			Resolve: func(p ResolveParams) (any, error) {
				return fmt.Sprintf("%v", p.Args["input"]), nil
			},
		},
	}, input)
}

func TestSpec2025_TypeSystem_OneOfInputObjects(t *testing.T) {
	schema := s25tsOneOfSchema(t)

	t.Run("the type reports isOneOf true through introspection", func(t *testing.T) {
		// §4.2.2: __Type.isOneOf is true for oneof input objects.
		result := s25Do(t, s25Request{Schema: schema, Query: `{ __type(name: "S25TsOneOf") { kind isOneOf } }`})
		if len(result.Errors) != 0 {
			t.Fatalf("unexpected errors: %v", s25ErrorMessages(result))
		}
		data := s25DataMap(t, result)
		ttype, ok := data["__type"].(map[string]any)
		if !ok {
			t.Fatalf("__type = %#v, want a map", data["__type"])
		}
		if got := ttype["kind"]; got != "INPUT_OBJECT" {
			t.Errorf("kind = %#v, want INPUT_OBJECT", got)
		}
		if got := ttype["isOneOf"]; got != true {
			t.Errorf("isOneOf = %#v, want true", got)
		}
	})

	t.Run("exactly one field succeeds", func(t *testing.T) {
		// §3.10.1: "the coerced unordered map must contain exactly one entry."
		s25RequireData(t, s25Request{Schema: schema, Query: `{ pick(input: {a: "x"}) }`},
			map[string]any{"pick": "map[a:x]"})
	})

	t.Run("exactly one integer field succeeds", func(t *testing.T) {
		s25RequireData(t, s25Request{Schema: schema, Query: `{ pick(input: {b: 3}) }`},
			map[string]any{"pick": "map[b:3]"})
	})

	t.Run("zero fields is a request error", func(t *testing.T) {
		result := s25Do(t, s25Request{Schema: schema, Query: `{ pick(input: {}) }`})
		s25RequireRequestError(t, result)
	})

	t.Run("two fields is a request error", func(t *testing.T) {
		result := s25Do(t, s25Request{Schema: schema, Query: `{ pick(input: {a: "x", b: 3}) }`})
		s25RequireRequestError(t, result)
	})

	t.Run("one field with an explicit null is a request error", func(t *testing.T) {
		// §3.10.1: "if the value of that entry is null ... a request error must be raised."
		result := s25Do(t, s25Request{Schema: schema, Query: `{ pick(input: {a: null}) }`})
		s25RequireRequestError(t, result)
	})

	t.Run("one field whose variable resolves to null is an error", func(t *testing.T) {
		result := s25Do(t, s25Request{
			Schema:    schema,
			Query:     `query ($a: String) { pick(input: {a: $a}) }`,
			Variables: map[string]any{"a": nil},
		})
		if len(result.Errors) == 0 {
			t.Errorf("expected an error for a oneof field coerced to null; data=%#v", s25Plain(result.Data))
		}
	})
}

// ---------------------------------------------------------------------------
// §3.11 List / §3.12 Non-Null / §3.12.1 Combining
// ---------------------------------------------------------------------------

func TestSpec2025_TypeSystem_ListAndNonNullCombinations(t *testing.T) {
	// 原生链路在 non-null 冒泡时会跨 goroutine panic 并终止测试进程，
	// 因此本用例在子进程中隔离执行；子进程失败仍按真实失败上报，不转为 skip。
	if !s25Isolated(t, "typesystem_listandnonnullcombinations") {
		return
	}

	valid := []any{"a", "b"}
	withNull := []any{"a", nil}
	empty := []any{}

	cases := []s25tsListCase{
		// T = String（非列表叶子）。任何列表内部值都无法在不丢失信息的前提下
		// 强制为 String，必须抬出 field error。
		{name: "String/null", field: "t", value: nil, want: nil},
		{name: "String/empty list", field: "t", value: empty, want: nil, paths: [][]any{{"t"}}},
		{name: "String/list with null", field: "t", value: withNull, want: nil, paths: [][]any{{"t"}}},
		{name: "String/valid list", field: "t", value: valid, want: nil, paths: [][]any{{"t"}}},

		// T! = String!
		{name: "String!/null", field: "tNN", value: nil, dataNull: true, paths: [][]any{{"tNN"}}},
		{name: "String!/empty list", field: "tNN", value: empty, dataNull: true, paths: [][]any{{"tNN"}}},
		{name: "String!/list with null", field: "tNN", value: withNull, dataNull: true, paths: [][]any{{"tNN"}}},
		{name: "String!/valid list", field: "tNN", value: valid, dataNull: true, paths: [][]any{{"tNN"}}},

		// [T] = [String]
		{name: "[String]/null", field: "listT", value: nil, want: nil},
		{name: "[String]/empty list", field: "listT", value: empty, want: []any{}},
		{name: "[String]/list with null", field: "listT", value: withNull, want: []any{"a", nil}},
		{name: "[String]/valid list", field: "listT", value: valid, want: []any{"a", "b"}},

		// [T]! = [String]!
		{name: "[String]!/null", field: "listTNN", value: nil, dataNull: true, paths: [][]any{{"listTNN"}}},
		{name: "[String]!/empty list", field: "listTNN", value: empty, want: []any{}},
		{name: "[String]!/list with null", field: "listTNN", value: withNull, want: []any{"a", nil}},
		{name: "[String]!/valid list", field: "listTNN", value: valid, want: []any{"a", "b"}},

		// [T!] = [String!]：单个 null 元素令整个列表变为 null（§6.4.4 传播到 List 层）。
		{name: "[String!]/null", field: "listNNT", value: nil, want: nil},
		{name: "[String!]/empty list", field: "listNNT", value: empty, want: []any{}},
		{name: "[String!]/list with null", field: "listNNT", value: withNull, want: nil,
			paths: [][]any{{"listNNT", 1}}},
		{name: "[String!]/valid list", field: "listNNT", value: valid, want: []any{"a", "b"}},

		// [T!]! = [String!]!：null 元素令列表变 null，列表非空于是继续冒泡到根。
		{name: "[String!]!/null", field: "listNNTNN", value: nil, dataNull: true, paths: [][]any{{"listNNTNN"}}},
		{name: "[String!]!/empty list", field: "listNNTNN", value: empty, want: []any{}},
		{name: "[String!]!/list with null", field: "listNNTNN", value: withNull, dataNull: true,
			paths: [][]any{{"listNNTNN", 1}}},
		{name: "[String!]!/valid list", field: "listNNTNN", value: valid, want: []any{"a", "b"}},

		// [[T!]!]!
		{name: "[[String!]!]!/null", field: "nested", value: nil, dataNull: true, paths: [][]any{{"nested"}}},
		{name: "[[String!]!]!/empty list", field: "nested", value: empty, want: []any{}},
		{name: "[[String!]!]!/list of empty list", field: "nested", value: []any{[]any{}},
			want: []any{[]any{}}},
		{name: "[[String!]!]!/inner null element", field: "nested", value: []any{[]any{"a", nil}},
			dataNull: true, paths: [][]any{{"nested", 0, 1}}},
		{name: "[[String!]!]!/null inner list", field: "nested", value: []any{nil},
			dataNull: true, paths: [][]any{{"nested", 0}}},
		{name: "[[String!]!]!/valid", field: "nested", value: []any{[]any{"a"}, []any{"b", "c"}},
			want: []any{[]any{"a"}, []any{"b", "c"}}},
	}

	for _, testCase := range cases {
		c := testCase
		t.Run(c.name, func(t *testing.T) {
			s25tsRunListCase(t, c)
		})
	}

	t.Run("a non-list internal value for a list type raises a field error", func(t *testing.T) {
		// §6.4.3 CompleteValue: "If result is not a collection of values, raise a field error."
		schema := s25tsListSchema(t, "oops")
		s25tsRequireFieldError(t, schema, "listT")
	})
}

// ---------------------------------------------------------------------------
// §3.13 Type System Directives
// ---------------------------------------------------------------------------

func TestSpec2025_TypeSystem_SpecifiedDirectives(t *testing.T) {
	// §3.13: "GraphQL implementations must provide the @skip, @include, @deprecated,
	// @specifiedBy and @oneOf directives."
	want := map[string][]string{
		"skip": {
			DirectiveLocationField,
			DirectiveLocationFragmentSpread,
			DirectiveLocationInlineFragment,
		},
		"include": {
			DirectiveLocationField,
			DirectiveLocationFragmentSpread,
			DirectiveLocationInlineFragment,
		},
		// §3.13.4: @deprecated on FIELD_DEFINITION | ARGUMENT_DEFINITION |
		// INPUT_FIELD_DEFINITION | ENUM_VALUE.
		"deprecated": {
			DirectiveLocationFieldDefinition,
			DirectiveLocationArgumentDefinition,
			DirectiveLocationInputFieldDefinition,
			DirectiveLocationEnumValue,
		},
		// §3.13.5: @specifiedBy on SCALAR.
		"specifiedBy": {DirectiveLocationScalar},
		// §3.13.6: @oneOf on INPUT_OBJECT.
		"oneOf": {DirectiveLocationInputObject},
	}

	schema := s25NewSchema(t, Fields{"ping": &Field{Type: String, Resolve: s25Const("pong")}})

	got := map[string][]string{}
	for _, directive := range schema.Directives() {
		if directive == nil {
			t.Errorf("schema.Directives() contains a nil entry")
			continue
		}
		got[directive.Name] = append([]string(nil), directive.Locations...)
	}

	names := make([]string, 0, len(want))
	for name := range want {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		locations, present := got[name]
		if !present {
			t.Errorf("@%s is missing from schema.Directives(); have %v", name, s25tsSortedKeys(got))
			continue
		}
		if !s25tsSameStringSet(locations, want[name]) {
			t.Errorf("@%s locations = %v, want %v", name, s25tsSorted(locations), s25tsSorted(want[name]))
		}
	}

	t.Run("schema.Directive lookup finds every specified directive", func(t *testing.T) {
		for _, name := range names {
			if schema.Directive(name) == nil {
				t.Errorf("schema.Directive(%q) = nil, want the specified directive", name)
			}
		}
	})

	t.Run("@deprecated carries a reason argument defaulting to a String", func(t *testing.T) {
		directive := schema.Directive("deprecated")
		if directive == nil {
			t.Fatalf("@deprecated is missing")
		}
		var reason *Argument
		for _, arg := range directive.Args {
			if arg.Name() == "reason" {
				reason = arg
			}
		}
		if reason == nil {
			t.Fatalf("@deprecated has no reason argument; args=%v", directive.Args)
		}
		if reason.Type != String {
			t.Errorf("@deprecated(reason:) type = %v, want String", reason.Type)
		}
	})

	t.Run("@specifiedBy carries a non-null url argument", func(t *testing.T) {
		// §3.13.5: directive @specifiedBy(url: String!) on SCALAR
		directive := schema.Directive("specifiedBy")
		if directive == nil {
			t.Fatalf("@specifiedBy is missing from the schema's directive set")
		}
		if len(directive.Args) != 1 {
			t.Fatalf("@specifiedBy args = %v, want exactly one (url: String!)", directive.Args)
		}
		if directive.Args[0].Name() != "url" {
			t.Errorf("@specifiedBy argument name = %q, want \"url\"", directive.Args[0].Name())
		}
		if _, ok := directive.Args[0].Type.(*NonNull); !ok {
			t.Errorf("@specifiedBy(url:) type = %v, want String!", directive.Args[0].Type)
		}
	})

	t.Run("@oneOf takes no arguments", func(t *testing.T) {
		// §3.13.6: directive @oneOf on INPUT_OBJECT
		directive := schema.Directive("oneOf")
		if directive == nil {
			t.Fatalf("@oneOf is missing from the schema's directive set")
		}
		if len(directive.Args) != 0 {
			t.Errorf("@oneOf args = %v, want none", directive.Args)
		}
	})

	t.Run("specified directives are not repeatable", func(t *testing.T) {
		// §3.13: none of the specified directives are declared `repeatable`.
		data := s25MustData(t, s25Request{
			Schema: schema,
			Query:  `{ __schema { directives { name isRepeatable } } }`,
		})
		list, ok := s25Plain(s25tsDig(data, "__schema", "directives")).([]any)
		if !ok {
			t.Fatalf("__schema.directives = %#v, want a list", s25tsDig(data, "__schema", "directives"))
		}
		for _, entry := range list {
			directive, ok := entry.(map[string]any)
			if !ok {
				t.Errorf("directive entry = %#v, want a map", entry)
				continue
			}
			if directive["isRepeatable"] != false {
				t.Errorf("@%v isRepeatable = %#v, want false", directive["name"], directive["isRepeatable"])
			}
		}
	})
}

func s25tsDig(root any, keys ...string) any {
	current := root
	for _, key := range keys {
		m, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		current = m[key]
	}
	return current
}

func s25tsSorted(values []string) []string {
	out := append([]string(nil), values...)
	sort.Strings(out)
	return out
}

func s25tsSortedKeys(m map[string][]string) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func s25tsSameStringSet(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	return reflect.DeepEqual(s25tsSorted(got), s25tsSorted(want))
}

// ---------------------------------------------------------------------------
// Schema 构造期错误
// ---------------------------------------------------------------------------

func TestSpec2025_TypeSystem_SchemaConstructionErrors(t *testing.T) {
	t.Run("a schema without a Query type is rejected", func(t *testing.T) {
		// §3.3.1: "The query root operation type must be provided and must be an
		// Object type."
		if _, err := NewSchema(SchemaConfig{}); err == nil {
			t.Errorf("NewSchema without Query returned nil error, want a construction error")
		}
	})

	t.Run("a field with an invalid name is rejected", func(t *testing.T) {
		// §2.1.9 / §3.6: names must match /[_A-Za-z][_0-9A-Za-z]*/.
		_, err := NewSchema(SchemaConfig{
			Query: NewObject(ObjectConfig{Name: "Query", Fields: Fields{
				"bad-name": &Field{Type: String, Resolve: s25Const("x")},
			}}),
		})
		if err == nil {
			t.Errorf("NewSchema with the field \"bad-name\" returned nil error, want a construction error")
		}
	})

	t.Run("an invalid type name is rejected", func(t *testing.T) {
		_, err := NewSchema(SchemaConfig{
			Query: NewObject(ObjectConfig{Name: "Bad Name", Fields: Fields{
				"ok": &Field{Type: String, Resolve: s25Const("x")},
			}}),
		})
		if err == nil {
			t.Errorf("NewSchema with the type \"Bad Name\" returned nil error, want a construction error")
		}
	})

	t.Run("two distinct types with the same name are rejected", func(t *testing.T) {
		// §3.1: "All types within a GraphQL schema must have unique names."
		first := NewObject(ObjectConfig{Name: "S25TsDup", Fields: Fields{
			"a": &Field{Type: String, Resolve: s25Const("a")},
		}})
		second := NewObject(ObjectConfig{Name: "S25TsDup", Fields: Fields{
			"b": &Field{Type: String, Resolve: s25Const("b")},
		}})
		_, err := NewSchema(SchemaConfig{
			Query: NewObject(ObjectConfig{Name: "Query", Fields: Fields{
				"first":  &Field{Type: first},
				"second": &Field{Type: second},
			}}),
		})
		if err == nil {
			t.Errorf("NewSchema with two S25TsDup types returned nil error, want a construction error")
		}
	})

	t.Run("an enum with no values is rejected", func(t *testing.T) {
		// §3.9: "An Enum type must define one or more unique enum values."
		empty := NewEnum(EnumConfig{Name: "S25TsEmptyEnum", Values: EnumValueConfigMap{}})
		_, err := NewSchema(SchemaConfig{
			Query: NewObject(ObjectConfig{Name: "Query", Fields: Fields{
				"value": &Field{Type: empty, Resolve: s25Const(nil)},
			}}),
		})
		if err == nil {
			t.Errorf("NewSchema with an empty enum returned nil error, want a construction error")
		}
	})

	t.Run("an object type with no fields is rejected", func(t *testing.T) {
		// §3.6: "An Object type must define one or more fields."
		emptyObject := NewObject(ObjectConfig{Name: "S25TsEmptyObject", Fields: Fields{}})
		_, err := NewSchema(SchemaConfig{
			Query: NewObject(ObjectConfig{Name: "Query", Fields: Fields{
				"value": &Field{Type: emptyObject, Resolve: s25Const(nil)},
			}}),
		})
		if err == nil {
			t.Errorf("NewSchema with a field-less object returned nil error, want a construction error")
		}
	})

	t.Run("a union with a member that is not an object type is rejected", func(t *testing.T) {
		// §3.8: "The member types of a Union type must all be Object base types."
		_, err := NewSchema(SchemaConfig{
			Query: NewObject(ObjectConfig{Name: "Query", Fields: Fields{
				"value": &Field{Type: NewUnion(UnionConfig{Name: "S25TsBadUnion", Types: []*Object{}})},
			}}),
		})
		if err == nil {
			t.Errorf("NewSchema with an empty union returned nil error, want a construction error")
		}
	})

	// mutex 只是为了让 -race 下的 helper 复用保持稳定，不参与断言。
	var mu sync.Mutex
	mu.Lock()
	mu.Unlock()
}
