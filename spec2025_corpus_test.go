package graphql

// spec2025_corpus_test.go
//
// GraphQL September 2025 规范一致性测试语料。
// 本文件只提供 schema 构造器、执行入口和断言 helper，不包含任何断言本身。
//
// 设计约束（两条执行链路共用同一份用例源码）：
//   1. 全部用例通过公开入口 Do(Params{...}) 执行，覆盖 parse -> validate -> execute。
//   2. 业务 resolver 用 s25Parent 双读父对象：原生链路读 p.Source，sgraph 链路读
//      由 ParamRegistry 注入的参数。参数在 schema 中可空且无默认值，原生链路下
//      values.go:getArgumentValues 不会把它写进 p.Args，因此对原生完全惰性。
//   3. 断言前用 toPlainValue 归一化容器；字段顺序断言不归一化，直接比对 JSON 字节序。

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/graphql-go/graphql/gqlerrors"
	"github.com/graphql-go/graphql/language/ast"
	"github.com/graphql-go/graphql/language/parser"
	"github.com/graphql-go/graphql/language/source"
)

// ---------------------------------------------------------------------------
// 父对象双读
// ---------------------------------------------------------------------------

// s25ParentArgName 是注入父结果所用的参数名。schema 中声明为可空、无默认值。
const s25ParentArgName = "parentRef"

// s25Parent 返回当前字段的父对象。
// 原生链路：p.Source 非空。
// sgraph 链路：p.Source 恒为 nil（plan.go:977），改从 ParamRegistry 注入的参数读取。
func s25Parent(p ResolveParams) map[string]any {
	if m, ok := p.Source.(map[string]any); ok && m != nil {
		return m
	}
	if raw, ok := p.Args[s25ParentArgName]; ok {
		if m, ok := raw.(map[string]any); ok {
			return m
		}
	}
	return nil
}

// s25ParentString 读取父对象上的某个字符串属性；父对象缺失时返回空串和 false。
func s25ParentString(p ResolveParams, key string) (string, bool) {
	parent := s25Parent(p)
	if parent == nil {
		return "", false
	}
	value, ok := parent[key]
	if !ok || value == nil {
		return "", false
	}
	return fmt.Sprintf("%v", value), true
}

// s25ParentArg 生成一个可空、无默认值的父对象注入参数定义。
// opaque 必须是同一个 schema 内共享的标量实例，否则 NewSchema 会因重名类型报错。
func s25ParentArg(opaque *Scalar) FieldConfigArgument {
	return FieldConfigArgument{
		s25ParentArgName: &ArgumentConfig{Type: opaque},
	}
}

// ---------------------------------------------------------------------------
// ParamRegistry 绑定
// ---------------------------------------------------------------------------

// s25ParentBinding 构造一条"把 source 字段的完整结果注入 target 字段 parentRef 参数"的绑定。
// resultPath 为空表示注入整个父结果。
func s25ParentBinding(targetPath []string, targetParentType, targetField string,
	sourcePath []string, sourceParentType, sourceField string, resultPath ...string) FieldParamBinding {
	return FieldParamBinding{
		Target: FieldParamTarget{
			ResponsePath:   append([]string(nil), targetPath...),
			ParentTypeName: targetParentType,
			FieldName:      targetField,
			ParamName:      s25ParentArgName,
		},
		Source: ParamSource{
			Kind: ParamSourceFieldResponse,
			FieldResponse: &FieldResponseParamSource{
				ResponsePath:   append([]string(nil), sourcePath...),
				ParentTypeName: sourceParentType,
				FieldName:      sourceField,
				ResultPath:     append([]string(nil), resultPath...),
			},
		},
	}
}

// s25NamedBinding 构造一条注入到自定义参数名的绑定（用于标量子路径注入）。
func s25NamedBinding(targetPath []string, targetParentType, targetField, paramName string,
	sourcePath []string, sourceParentType, sourceField string, resultPath ...string) FieldParamBinding {
	binding := s25ParentBinding(targetPath, targetParentType, targetField, sourcePath, sourceParentType, sourceField, resultPath...)
	binding.Target.ParamName = paramName
	return binding
}

// ---------------------------------------------------------------------------
// 执行入口
// ---------------------------------------------------------------------------

// s25Request 描述一次完整请求。两轮执行使用完全相同的 s25Request。
type s25Request struct {
	Schema        Schema
	Query         string
	Variables     map[string]any
	OperationName string
	Root          map[string]any
	Context       context.Context
	// Bindings 只被 sgraph 链路消费；原生链路不读 ParamRegistry。
	Bindings []FieldParamBinding
	// MetadataDirectives 列出本次请求用到的、schema 已声明但没有执行语义的自定义指令。
	// SGRAPH_USAGE_NOTES.md 的 Engine 生命周期第 2 步要求调用方在创建 Engine 前
	// 完成"自定义指令的全部注册"；纯标注型指令对应 RegisterMetadataOnly。
	// 原生链路不读 DirectiveRegistry，因此该字段对第一轮完全惰性。
	MetadataDirectives []string
}

// s25Do 执行一次请求。解析或校验失败时返回带 Errors 的 *Result，不 Fatal，
// 以便 request error 形状可以被断言。
func s25Do(t testing.TB, req s25Request) *Result {
	t.Helper()
	if len(req.Bindings) > 0 || len(req.MetadataDirectives) > 0 {
		s25BindRegistry(t, req)
	}
	return Do(Params{
		Schema:         req.Schema,
		RequestString:  req.Query,
		VariableValues: req.Variables,
		OperationName:  req.OperationName,
		RootObject:     req.Root,
		Context:        req.Context,
	})
}

// s25BindRegistry 为 sgraph 链路注册参数依赖。对原生链路无副作用：
// 原生 executor 不读取 ParamRegistry，也不使用 SGraphEngine。
func s25BindRegistry(t testing.TB, req s25Request) {
	t.Helper()
	registry := NewParamRegistry()
	if len(req.Bindings) > 0 {
		if err := registry.RegisterQuery(QueryParamConfig{
			DocumentBody:  req.Query,
			OperationName: req.OperationName,
			FieldParams:   req.Bindings,
		}); err != nil {
			t.Fatalf("register param bindings: %v", err)
		}
	}
	// 自定义指令必须在 Engine 创建前登记，否则 plan 编译阶段会报
	// "no directive compiler found for X"。纯标注型指令用 RegisterMetadataOnly。
	directives := NewDirectiveRegistry()
	for _, name := range req.MetadataDirectives {
		if err := directives.RegisterMetadataOnly(name); err != nil {
			t.Fatalf("register metadata-only directive %s: %v", name, err)
		}
	}
	engine, err := NewSGraphEngine(&req.Schema, directives, registry)
	if err != nil {
		t.Fatalf("new sgraph engine: %v", err)
	}
	if err := RegisterSGraphEngine(engine); err != nil {
		t.Fatalf("register sgraph engine: %v", err)
	}
}

// s25MustData 执行请求，要求没有任何错误，并返回归一化后的 data map。
func s25MustData(t testing.TB, req s25Request) map[string]any {
	t.Helper()
	result := s25Do(t, req)
	s25RequireNoErrors(t, result)
	return s25DataMap(t, result)
}

// ---------------------------------------------------------------------------
// 断言 helper
// ---------------------------------------------------------------------------

// s25DataMap 把 Result.Data 归一化为普通 map。sgraph 返回 *SGraphResponseOrderedMap，
// 原生返回 map[string]interface{}；归一化只改容器，不改内容。
func s25DataMap(t testing.TB, result *Result) map[string]any {
	t.Helper()
	if result == nil {
		t.Fatalf("result is nil")
	}
	plain := toPlainValue(result.Data)
	if plain == nil {
		t.Fatalf("result.Data is nil, errors=%v", s25ErrorMessages(result))
	}
	data, ok := plain.(map[string]any)
	if !ok {
		t.Fatalf("result.Data is %T, want map, errors=%v", result.Data, s25ErrorMessages(result))
	}
	return data
}

// s25Plain 归一化任意结果值。
func s25Plain(v any) any { return toPlainValue(v) }

func s25RequireNoErrors(t testing.TB, result *Result) {
	t.Helper()
	if result == nil {
		t.Fatalf("result is nil")
	}
	if len(result.Errors) != 0 {
		t.Fatalf("unexpected errors: %v", s25ErrorMessages(result))
	}
}

func s25RequireData(t testing.TB, req s25Request, expected map[string]any) {
	t.Helper()
	actual := s25MustData(t, req)
	if !reflect.DeepEqual(actual, expected) {
		t.Fatalf("data mismatch\n got: %#v\nwant: %#v", actual, expected)
	}
}

func s25ErrorMessages(result *Result) []string {
	if result == nil {
		return nil
	}
	messages := make([]string, 0, len(result.Errors))
	for _, err := range result.Errors {
		messages = append(messages, err.Message)
	}
	return messages
}

// s25RequireRequestError 断言这是一次 request error：有错误且完全没有执行数据。
// 规范 §7.1.3：request error 结果不得包含 data 条目。
func s25RequireRequestError(t testing.TB, result *Result) {
	t.Helper()
	if result == nil {
		t.Fatalf("result is nil")
	}
	if len(result.Errors) == 0 {
		t.Fatalf("expected a request error, got none; data=%#v", s25Plain(result.Data))
	}
	if result.Data != nil {
		t.Fatalf("request error result must not carry data; got %#v", s25Plain(result.Data))
	}
}

// s25RequireErrorCount 按集合语义断言错误数量。
func s25RequireErrorCount(t testing.TB, result *Result, want int) {
	t.Helper()
	if got := len(result.Errors); got != want {
		t.Fatalf("error count = %d, want %d; messages=%v", got, want, s25ErrorMessages(result))
	}
}

// s25RequireErrorPathSet 按集合语义断言错误 path：数量必须相等，且每条期望 path
// 都能在实际错误中找到。规范 §7.1.6 未规定 errors 的顺序。
func s25RequireErrorPathSet(t testing.TB, result *Result, expected ...[]any) {
	t.Helper()
	if len(result.Errors) != len(expected) {
		t.Fatalf("error count = %d, want %d; got paths=%v messages=%v",
			len(result.Errors), len(expected), s25ErrorPaths(result), s25ErrorMessages(result))
	}
	actual := s25ErrorPaths(result)
	used := make([]bool, len(actual))
	for _, want := range expected {
		matched := false
		for index, got := range actual {
			if used[index] {
				continue
			}
			if s25PathEqual(got, want) {
				used[index] = true
				matched = true
				break
			}
		}
		if !matched {
			t.Fatalf("missing error path %v; got %v", want, actual)
		}
	}
}

// s25RequireHasErrorPath 只要求存在一条 path 匹配的错误。
func s25RequireHasErrorPath(t testing.TB, result *Result, want []any) {
	t.Helper()
	for _, got := range s25ErrorPaths(result) {
		if s25PathEqual(got, want) {
			return
		}
	}
	t.Fatalf("no error with path %v; got %v (messages=%v)", want, s25ErrorPaths(result), s25ErrorMessages(result))
}

func s25ErrorPaths(result *Result) [][]any {
	if result == nil {
		return nil
	}
	paths := make([][]any, 0, len(result.Errors))
	for _, err := range result.Errors {
		paths = append(paths, append([]any(nil), err.Path...))
	}
	return paths
}

// s25PathEqual 比较响应路径。整数下标在不同链路可能是 int / int32 / float64，
// 统一按数值比较；字符串按字面量比较。
func s25PathEqual(got, want []any) bool {
	if len(got) != len(want) {
		return false
	}
	for index := range got {
		if !s25PathKeyEqual(got[index], want[index]) {
			return false
		}
	}
	return true
}

func s25PathKeyEqual(got, want any) bool {
	gotInt, gotOK := s25AsInt(got)
	wantInt, wantOK := s25AsInt(want)
	if gotOK && wantOK {
		return gotInt == wantInt
	}
	if gotOK != wantOK {
		return false
	}
	return fmt.Sprintf("%v", got) == fmt.Sprintf("%v", want)
}

func s25AsInt(v any) (int64, bool) {
	switch typed := v.(type) {
	case int:
		return int64(typed), true
	case int8:
		return int64(typed), true
	case int16:
		return int64(typed), true
	case int32:
		return int64(typed), true
	case int64:
		return typed, true
	case uint:
		return int64(typed), true
	case uint32:
		return int64(typed), true
	case uint64:
		return int64(typed), true
	case float64:
		if typed == float64(int64(typed)) {
			return int64(typed), true
		}
	}
	return 0, false
}

// s25RequireErrorContaining 断言存在一条 message 含指定子串的错误。
func s25RequireErrorContaining(t testing.TB, result *Result, substr string) {
	t.Helper()
	for _, err := range result.Errors {
		if strings.Contains(err.Message, substr) {
			return
		}
	}
	t.Fatalf("no error containing %q; got %v", substr, s25ErrorMessages(result))
}

// s25RequireAllErrorsHaveLocations 规范 §7.1.6：执行错误应给出 locations。
func s25RequireAllErrorsHaveLocations(t testing.TB, result *Result) {
	t.Helper()
	for _, err := range result.Errors {
		if len(err.Locations) == 0 {
			t.Fatalf("error %q has no locations", err.Message)
		}
	}
}

// s25ErrorExtensions 返回第一条带 extensions 的错误的 extensions。
func s25ErrorExtensions(result *Result) map[string]any {
	for _, err := range result.Errors {
		if len(err.Extensions) > 0 {
			return err.Extensions
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// JSON / 顺序断言（不经过 toPlainValue）
// ---------------------------------------------------------------------------

// s25MarshalData 直接序列化 Result.Data，保留链路自身的字段顺序语义。
func s25MarshalData(t testing.TB, result *Result) string {
	t.Helper()
	raw, err := json.Marshal(result.Data)
	if err != nil {
		t.Fatalf("marshal data: %v", err)
	}
	return string(raw)
}

// s25MarshalResult 序列化整个 Result，用于 §7.1.3 / §7.1.8 的顶层键断言。
func s25MarshalResult(t testing.TB, result *Result) map[string]json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	return top
}

// s25RequireKeyOrder 断言 JSON 文本中若干 key 按给定顺序出现。
func s25RequireKeyOrder(t testing.TB, encoded string, keys ...string) {
	t.Helper()
	previous := -1
	for _, key := range keys {
		index := strings.Index(encoded, "\""+key+"\"")
		if index < 0 {
			t.Fatalf("key %q not found in %s", key, encoded)
		}
		if index <= previous {
			t.Fatalf("key %q appears out of order in %s", key, encoded)
		}
		previous = index
	}
}

// ---------------------------------------------------------------------------
// 解析 / 校验入口
// ---------------------------------------------------------------------------

func s25Parse(query string) (*ast.Document, error) {
	return parser.Parse(parser.ParseParams{Source: source.NewSource(&source.Source{
		Body: []byte(query),
		Name: "GraphQL September 2025 conformance test",
	})})
}

// s25Validate 只做解析 + 校验，返回校验错误。解析失败时把解析错误一并返回。
func s25Validate(t testing.TB, schema Schema, query string) []gqlerrors.FormattedError {
	t.Helper()
	document, err := s25Parse(query)
	if err != nil {
		return gqlerrors.FormatErrors(err)
	}
	return ValidateDocument(&schema, document, nil).Errors
}

// s25RequireValid 断言查询通过校验。
func s25RequireValid(t testing.TB, schema Schema, query string) {
	t.Helper()
	if errs := s25Validate(t, schema, query); len(errs) != 0 {
		messages := make([]string, 0, len(errs))
		for _, err := range errs {
			messages = append(messages, err.Message)
		}
		t.Fatalf("expected valid document, got: %v", messages)
	}
}

// s25RequireInvalid 断言查询被校验拒绝。
func s25RequireInvalid(t testing.TB, schema Schema, query string) []gqlerrors.FormattedError {
	t.Helper()
	errs := s25Validate(t, schema, query)
	if len(errs) == 0 {
		t.Fatalf("expected validation errors, got none")
	}
	return errs
}

// ---------------------------------------------------------------------------
// 子进程隔离（用于可能 panic / 栈溢出的用例）
// ---------------------------------------------------------------------------

const s25ChildEnv = "SPEC2025_ISOLATED_CASE"

// s25Isolated 在子进程中执行当前测试。父进程调用后应立即 return。
// 子进程失败仍按真实失败上报，不转为 skip。
func s25Isolated(t *testing.T, caseName string) bool {
	t.Helper()
	if os.Getenv(s25ChildEnv) == caseName {
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^"+t.Name()+"$", "-test.count=1", "-test.v")
	cmd.Env = append(os.Environ(), s25ChildEnv+"="+caseName)
	output, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("isolated case %s timed out\n%s", caseName, s25Tail(output))
	}
	if err != nil {
		t.Fatalf("isolated case %s failed: %v\n%s", caseName, err, s25Tail(output))
	}
	return false
}

func s25Tail(output []byte) string {
	const limit = 8192
	if len(output) > limit {
		return string(output[len(output)-limit:])
	}
	return string(output)
}

// ---------------------------------------------------------------------------
// 公共标量 / 类型构造器
// ---------------------------------------------------------------------------

// s25NewOpaqueScalar 用于承接注入的父对象。FIELD_RESPONSE 参数不做输入协变
// （plan_compiler_param_registry.go:180），因此父结果原样通过。
func s25NewOpaqueScalar() *Scalar {
	return NewScalar(ScalarConfig{
		Name:         "S25Opaque",
		Description:  "Opaque carrier used only to transport a parent result into a resolver argument.",
		Serialize:    func(value any) any { return value },
		ParseValue:   func(value any) any { return value },
		ParseLiteral: func(value ast.Value) any { return nil },
	})
}

// s25NewOddScalar 是自定义标量：只接受奇数。
func s25NewOddScalar() *Scalar {
	coerce := func(value any) any {
		switch typed := value.(type) {
		case int:
			if typed%2 != 0 {
				return typed
			}
		case int32:
			if typed%2 != 0 {
				return int(typed)
			}
		case int64:
			if typed%2 != 0 {
				return int(typed)
			}
		case float64:
			asInt := int(typed)
			if float64(asInt) == typed && asInt%2 != 0 {
				return asInt
			}
		}
		return nil
	}
	return NewScalar(ScalarConfig{
		Name:       "S25Odd",
		Serialize:  coerce,
		ParseValue: coerce,
		ParseLiteral: func(value ast.Value) any {
			if literal, ok := value.(*ast.IntValue); ok {
				var parsed int
				if _, err := fmt.Sscanf(literal.Value, "%d", &parsed); err == nil && parsed%2 != 0 {
					return parsed
				}
			}
			return nil
		},
	})
}

// s25NewMode 是带 deprecated 值的枚举。
func s25NewMode() *Enum {
	return NewEnum(EnumConfig{
		Name:        "S25Mode",
		Description: "Execution mode used by the conformance corpus.",
		Values: EnumValueConfigMap{
			"A":            &EnumValueConfig{Value: "A", Description: "Mode A"},
			"B":            &EnumValueConfig{Value: "B"},
			"DEPRECATED_C": &EnumValueConfig{Value: "C", DeprecationReason: "use B"},
		},
	})
}

// s25CoreTypes 保存一次 schema 构建中产生的共享类型实例，供用例做类型断言。
type s25CoreTypes struct {
	Opaque *Scalar
	Odd    *Scalar
	Mode   *Enum
	Inner  *InputObject
	Filter *InputObject
	Node   *Interface
	User   *Object
	Robot  *Object
	Search *Union
	Query  *Object
}

// s25Counter 记录 resolver 调用次数，用于"只调用一次"类断言。
type s25Counter struct {
	mu     sync.Mutex
	counts map[string]int
}

func s25NewCounter() *s25Counter { return &s25Counter{counts: map[string]int{}} }

func (c *s25Counter) inc(key string) {
	c.mu.Lock()
	c.counts[key]++
	c.mu.Unlock()
}

func (c *s25Counter) Get(key string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.counts[key]
}

func (c *s25Counter) Total() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	total := 0
	for _, value := range c.counts {
		total += value
	}
	return total
}

func (c *s25Counter) Keys() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	keys := make([]string, 0, len(c.counts))
	for key := range c.counts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// s25ExtendedError 实现 gqlerrors.ExtendedError，用于 §7.1.6 extensions 断言。
type s25ExtendedError struct {
	message string
	code    string
}

func (e s25ExtendedError) Error() string { return e.message }
func (e s25ExtendedError) Extensions() map[string]any {
	return map[string]any{"code": e.code}
}
