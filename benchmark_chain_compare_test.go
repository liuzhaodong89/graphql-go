package graphql

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/graphql-go/graphql/language/ast"
	"github.com/graphql-go/graphql/language/parser"
	"github.com/graphql-go/graphql/language/source"
)

// 两条执行链路的对比基准。
//
// 公平性原则（刻意不做任何单侧适配）：
//  1. 两条链路使用同一个 Schema 实例、同一份数据、同一个已解析的 AST 和同一组变量。
//     Schema 按普通 graphql-go 写法构造，不为 SGraph 补 resolver、不注册 ParamRegistry、
//     不改写查询。SGraph 的接入约束若导致行为差异，作为结果如实记录，而不是通过改 schema 抹平。
//  2. parse 与 validate 由两条链路共用（graphql.go 中位于链路路由之前），因此基准只测执行段：
//     原生走 ExecuteGraphQLGo，SGraph 走 Execute（executor.go 路由到 executeSGraph）。
//  3. 两侧都在计时前预热一次，使 SGraph 的 Plan 编译与 BatchPlan 协调不计入稳态耗时；
//     原生无对应缓存，预热对其为纯粹的空转，不构成不公平。
//  4. 结果一致性在预热阶段校验。不一致的用例**不跳过**，而是标记后照常计时，
//     并在报告中单列——跳过会让 SGraph 只跑自己能跑的子集，使总体对比失真。
//  5. mutation 不纳入对比：executor.go 对 mutation 直接回退 ExecuteGraphQLGo，
//     两侧测的是同一段代码，对比无信息量。
//
// 指标：ns/op 为主，辅以 B/op、allocs/op，以及用于横向解释的 request/response 字节数。

// ---------------------------------------------------------------------------
// 数据集
// ---------------------------------------------------------------------------

type chainRow struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Score int    `json:"score"`
	Tag   string `json:"tag"`
}

var (
	chainRowsOnce sync.Once
	chainRowCache map[int][]any
)

// chainRows 返回 n 行稳定数据；同一 n 复用同一份切片，避免把数据构造计入基准。
func chainRows(n int) []any {
	chainRowsOnce.Do(func() { chainRowCache = make(map[int][]any) })
	if cached, ok := chainRowCache[n]; ok {
		return cached
	}
	rows := make([]any, 0, n)
	for i := 0; i < n; i++ {
		rows = append(rows, map[string]any{
			"id":    fmt.Sprintf("id-%06d", i),
			"name":  fmt.Sprintf("name-%06d", i),
			"score": i,
			"tag":   "tag",
		})
	}
	chainRowCache[n] = rows
	return rows
}

func chainNestedRows(outer, inner int) []any {
	rows := make([]any, 0, outer)
	for i := 0; i < outer; i++ {
		rows = append(rows, map[string]any{
			"id":    fmt.Sprintf("o-%04d", i),
			"items": chainRows(inner),
		})
	}
	return rows
}

// chainSleep 模拟 resolver 的外部 IO 时延。
func chainSleep(d time.Duration) {
	if d > 0 {
		time.Sleep(d)
	}
}

// ---------------------------------------------------------------------------
// Schema：按普通 graphql-go 写法构造，不含任何 SGraph 专属配置
// ---------------------------------------------------------------------------

func newChainSchema(tb testing.TB) Schema {
	tb.Helper()

	row := NewObject(ObjectConfig{Name: "CmpRow", Fields: Fields{
		"id":    &Field{Type: NewNonNull(ID)},
		"name":  &Field{Type: String},
		"score": &Field{Type: Int},
		"tag":   &Field{Type: String},
		// 带 resolver 的子字段：两条链路都会真正调用它
		"upper": &Field{Type: String, Resolve: func(p ResolveParams) (any, error) {
			return "UP", nil
		}},
	}})

	outer := NewObject(ObjectConfig{Name: "CmpOuter", Fields: Fields{
		"id": &Field{Type: NewNonNull(ID)},
		// 无 resolver 的中间层 list：原生走默认取值，SGraph 走结果组装/内部物化
		"items": &Field{Type: NewList(row)},
	}})

	iface := NewInterface(InterfaceConfig{
		Name:   "CmpNode",
		Fields: Fields{"id": &Field{Type: NewNonNull(ID)}},
	})
	nodeA := NewObject(ObjectConfig{Name: "CmpNodeA", Interfaces: []*Interface{iface},
		Fields: Fields{"id": &Field{Type: NewNonNull(ID)}, "a": &Field{Type: String}}})
	nodeB := NewObject(ObjectConfig{Name: "CmpNodeB", Interfaces: []*Interface{iface},
		Fields: Fields{"id": &Field{Type: NewNonNull(ID)}, "b": &Field{Type: String}}})
	iface.ResolveType = func(p ResolveTypeParams) *Object {
		if m, ok := p.Value.(map[string]any); ok {
			if m["kind"] == "B" {
				return nodeB
			}
		}
		return nodeA
	}

	union := NewUnion(UnionConfig{Name: "CmpUnion", Types: []*Object{nodeA, nodeB},
		ResolveType: func(p ResolveTypeParams) *Object {
			if m, ok := p.Value.(map[string]any); ok {
				if m["kind"] == "B" {
					return nodeB
				}
			}
			return nodeA
		}})

	// 延迟字段工厂：每个字段独立 sleep，用于观察两条链路的并发编排能力
	delayedField := func(d time.Duration) *Field {
		return &Field{Type: String, Resolve: func(p ResolveParams) (any, error) {
			chainSleep(d)
			return "ok", nil
		}}
	}
	// 三级链：每级都有 resolver 且都 sleep。原生按选择集深度优先，父完成才执行子；
	// SGraph 无参数依赖声明时会把三级折叠进同一 batch 并发执行——
	// 这是两者的架构差异，不是实现优劣，报告中单独说明。
	deepC := NewObject(ObjectConfig{Name: "CmpDeepC", Fields: Fields{
		"value": delayedField(chainStageDelay)}})
	deepB := NewObject(ObjectConfig{Name: "CmpDeepB", Fields: Fields{
		"c": &Field{Type: deepC, Resolve: func(p ResolveParams) (any, error) {
			chainSleep(chainStageDelay)
			return map[string]any{}, nil
		}}}})
	deepA := NewObject(ObjectConfig{Name: "CmpDeepA", Fields: Fields{
		"b": &Field{Type: deepB, Resolve: func(p ResolveParams) (any, error) {
			chainSleep(chainStageDelay)
			return map[string]any{}, nil
		}}}})

	queryFields := Fields{
		"scalar": &Field{Type: String, Resolve: func(p ResolveParams) (any, error) { return "s", nil }},
		"rows10": &Field{Type: NewList(row), Resolve: func(p ResolveParams) (any, error) { return chainRows(10), nil }},
		"rows100": &Field{Type: NewList(row), Resolve: func(p ResolveParams) (any, error) {
			return chainRows(100), nil
		}},
		"rows1000": &Field{Type: NewList(row), Resolve: func(p ResolveParams) (any, error) {
			return chainRows(1000), nil
		}},
		"rows5000": &Field{Type: NewList(row), Resolve: func(p ResolveParams) (any, error) {
			return chainRows(5000), nil
		}},
		"nested": &Field{Type: NewList(outer), Resolve: func(p ResolveParams) (any, error) {
			return chainNestedRows(20, 20), nil
		}},
		"wrapper": &Field{Type: outer, Resolve: func(p ResolveParams) (any, error) {
			return map[string]any{"id": "w1", "items": chainRows(50)}, nil
		}},
		"nodes": &Field{Type: NewList(iface), Resolve: func(p ResolveParams) (any, error) {
			out := make([]any, 0, 100)
			for i := 0; i < 100; i++ {
				kind := "A"
				if i%2 == 1 {
					kind = "B"
				}
				out = append(out, map[string]any{"id": fmt.Sprintf("n-%03d", i), "kind": kind, "a": "va", "b": "vb"})
			}
			return out, nil
		}},
		"unions": &Field{Type: NewList(union), Resolve: func(p ResolveParams) (any, error) {
			out := make([]any, 0, 100)
			for i := 0; i < 100; i++ {
				kind := "A"
				if i%2 == 1 {
					kind = "B"
				}
				out = append(out, map[string]any{"id": fmt.Sprintf("u-%03d", i), "kind": kind, "a": "va", "b": "vb"})
			}
			return out, nil
		}},
		"echo": &Field{
			Type: String,
			Args: FieldConfigArgument{"in": &ArgumentConfig{Type: String}},
			Resolve: func(p ResolveParams) (any, error) {
				v, _ := p.Args["in"].(string)
				return v, nil
			},
		},
		"deep": &Field{Type: deepA, Resolve: func(p ResolveParams) (any, error) {
			chainSleep(chainStageDelay)
			return map[string]any{}, nil
		}},
	}
	// 并列延迟字段 d0..d15：用于测量同层并发能力
	for i := 0; i < 16; i++ {
		queryFields[fmt.Sprintf("d%d", i)] = delayedField(chainStageDelay)
	}

	schema, err := NewSchema(SchemaConfig{
		Query: NewObject(ObjectConfig{Name: "Query", Fields: queryFields}),
		Types: []Type{nodeA, nodeB},
	})
	if err != nil {
		tb.Fatalf("build compare schema: %v", err)
	}
	return schema
}

// chainStageDelay 是单个延迟 resolver 的耗时。设为 2ms：足以让调度差异显著高于噪声，
// 又不至于让整套基准运行过久。
const chainStageDelay = 2 * time.Millisecond

// ---------------------------------------------------------------------------
// 用例矩阵
// ---------------------------------------------------------------------------

type chainCase struct {
	group string // 维度分组
	name  string
	query string
	vars  map[string]any
}

// chainWideQuery 用别名把同一个标量字段重复 n 次，用于放大**请求体**而几乎不改变响应体。
func chainWideQuery(n int) string {
	var sb strings.Builder
	sb.WriteString("query W {")
	for i := 0; i < n; i++ {
		fmt.Fprintf(&sb, " a%06d: scalar", i)
	}
	sb.WriteString(" }")
	return sb.String()
}

func chainCases() []chainCase {
	return []chainCase{
		// —— 基线 ——
		{"Baseline", "SingleScalar", `{ scalar }`, nil},

		// —— 响应体规模（请求体几乎不变）——
		{"RespSize", "Rows10", `{ rows10 { id name score tag } }`, nil},
		{"RespSize", "Rows100", `{ rows100 { id name score tag } }`, nil},
		{"RespSize", "Rows1000", `{ rows1000 { id name score tag } }`, nil},
		{"RespSize", "Rows5000", `{ rows5000 { id name score tag } }`, nil},

		// —— 请求体规模（响应体几乎不变）——
		{"ReqSize", "Wide50", chainWideQuery(50), nil},
		{"ReqSize", "Wide500", chainWideQuery(500), nil},
		{"ReqSize", "Wide2000", chainWideQuery(2000), nil},

		// —— 每元素带 resolver 的子字段（N+1 形态）——
		{"PerElemResolver", "Rows100Upper", `{ rows100 { id upper } }`, nil},
		{"PerElemResolver", "Rows1000Upper", `{ rows1000 { id upper } }`, nil},

		// —— 无 resolver 中间层（原生默认取值 vs SGraph 组装/物化）——
		{"NoResolverMid", "WrapperItems", `{ wrapper { id items { id name } } }`, nil},
		{"NoResolverMid", "WrapperItemsTypename", `{ wrapper { id items { __typename id } } }`, nil},
		{"NoResolverMid", "WrapperItemsResolver", `{ wrapper { id items { id upper } } }`, nil},
		{"NoResolverMid", "NestedList20x20", `{ nested { id items { id name } } }`, nil},

		// —— 抽象类型运行时判定 ——
		{"Abstract", "Interface100", `{ nodes { __typename id ... on CmpNodeA { a } ... on CmpNodeB { b } } }`, nil},
		{"Abstract", "Union100", `{ unions { __typename ... on CmpNodeA { id a } ... on CmpNodeB { id b } } }`, nil},

		// —— 语言特性 ——
		{"Language", "AliasFragmentDirective",
			`query F($skip: Boolean!) { x: scalar y: scalar @skip(if: $skip) ...Frag } fragment Frag on Query { rows10 { id } }`,
			map[string]any{"skip": false}},
		{"Language", "VariableArg", `query V($s: String) { echo(in: $s) }`, map[string]any{"s": "hello"}},

		// —— resolver 时延与并发编排 ——
		{"Latency", "Parallel1", `{ d0 }`, nil},
		{"Latency", "Parallel4", `{ d0 d1 d2 d3 }`, nil},
		{"Latency", "Parallel16", `{ d0 d1 d2 d3 d4 d5 d6 d7 d8 d9 d10 d11 d12 d13 d14 d15 }`, nil},
		{"Latency", "Chain3Levels", `{ deep { b { c { value } } } }`, nil},
		{"Latency", "Mixed", `{ d0 d1 deep { b { c { value } } } }`, nil},
	}
}

// ---------------------------------------------------------------------------
// 执行与一致性校验
// ---------------------------------------------------------------------------

type chainPrepared struct {
	chainCase
	params      ExecuteParams
	nativeBytes int
	sgraphBytes int
	// mismatch 非空表示两条链路结果不一致；用例仍会计时，只是在报告中单列。
	mismatch string
	reqBytes int
}

func chainParse(tb testing.TB, schema Schema, query string) *ast.Document {
	tb.Helper()
	doc, err := parser.Parse(parser.ParseParams{Source: source.NewSource(&source.Source{Body: []byte(query)})})
	if err != nil {
		tb.Fatalf("parse: %v", err)
	}
	if vr := ValidateDocument(&schema, doc, nil); !vr.IsValid {
		tb.Fatalf("validate %q: %v", query, vr.Errors)
	}
	return doc
}

// chainNormalize 把结果规整成可比较形态：两条链路的 Data 容器类型不同
// （SGraph 是 *SGraphResponseOrderedMap，原生是 map），统一经 JSON 往返后比较内容。
func chainNormalize(r *Result) (any, int, []string) {
	if r == nil {
		return nil, 0, []string{"nil result"}
	}
	msgs := make([]string, 0, len(r.Errors))
	for _, e := range r.Errors {
		msgs = append(msgs, e.Message)
	}
	raw, _ := json.Marshal(r.Data)
	var normalized any
	_ = json.Unmarshal(raw, &normalized)
	return normalized, len(raw), msgs
}

func chainPrepare(tb testing.TB, schema Schema, cs []chainCase) []chainPrepared {
	tb.Helper()
	out := make([]chainPrepared, 0, len(cs))
	for _, c := range cs {
		doc := chainParse(tb, schema, c.query)
		params := ExecuteParams{Schema: schema, AST: doc, Args: c.vars, Context: context.Background()}

		nativeData, nativeSize, nativeErrs := chainNormalize(ExecuteGraphQLGo(params))
		sgraphData, sgraphSize, sgraphErrs := chainNormalize(Execute(params))

		mismatch := ""
		switch {
		case len(nativeErrs) != len(sgraphErrs):
			mismatch = fmt.Sprintf("errors native=%d sgraph=%d (%v / %v)",
				len(nativeErrs), len(sgraphErrs), nativeErrs, sgraphErrs)
		case !reflect.DeepEqual(nativeData, sgraphData):
			mismatch = fmt.Sprintf("data differs (native %dB vs sgraph %dB)", nativeSize, sgraphSize)
		}

		out = append(out, chainPrepared{
			chainCase:   c,
			params:      params,
			nativeBytes: nativeSize,
			sgraphBytes: sgraphSize,
			mismatch:    mismatch,
			reqBytes:    len(c.query),
		})
	}
	return out
}

func chainReport(b *testing.B, p chainPrepared, respBytes int) {
	b.ReportMetric(float64(p.reqBytes), "request-B")
	b.ReportMetric(float64(respBytes), "response-B")
}

// ---------------------------------------------------------------------------
// 基准入口
// ---------------------------------------------------------------------------

// BenchmarkChainNative 原生链路（ExecuteGraphQLGo）。
func BenchmarkChainNative(b *testing.B) {
	schema := newChainSchema(b)
	for _, p := range chainPrepare(b, schema, chainCases()) {
		p := p
		b.Run(p.group+"/"+p.name, func(b *testing.B) {
			ExecuteGraphQLGo(p.params) // 预热，与 SGraph 侧对称
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if r := ExecuteGraphQLGo(p.params); r == nil {
					b.Fatal("nil result")
				}
			}
			b.StopTimer()
			chainReport(b, p, p.nativeBytes)
		})
	}
}

// BenchmarkChainSGraph SGraph 链路（Execute → executeSGraph）。
func BenchmarkChainSGraph(b *testing.B) {
	schema := newChainSchema(b)
	engine, err := NewSGraphEngine(&schema, nil, nil)
	if err != nil {
		b.Fatalf("new engine: %v", err)
	}
	if err := RegisterSGraphEngine(engine); err != nil {
		b.Fatalf("register engine: %v", err)
	}
	for _, p := range chainPrepare(b, schema, chainCases()) {
		p := p
		b.Run(p.group+"/"+p.name, func(b *testing.B) {
			Execute(p.params) // 预热：编译 Plan 与协调 BatchPlan，使其不计入稳态
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if r := Execute(p.params); r == nil {
					b.Fatal("nil result")
				}
			}
			b.StopTimer()
			chainReport(b, p, p.sgraphBytes)
		})
	}
}

// BenchmarkChainNativeConcurrent / BenchmarkChainSGraphConcurrent 测并发请求下的吞吐。
func BenchmarkChainNativeConcurrent(b *testing.B) {
	schema := newChainSchema(b)
	for _, p := range chainConcurrentCases(b, schema) {
		p := p
		b.Run(p.group+"/"+p.name, func(b *testing.B) {
			ExecuteGraphQLGo(p.params)
			b.ReportAllocs()
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					ExecuteGraphQLGo(p.params)
				}
			})
		})
	}
}

func BenchmarkChainSGraphConcurrent(b *testing.B) {
	schema := newChainSchema(b)
	engine, err := NewSGraphEngine(&schema, nil, nil)
	if err != nil {
		b.Fatalf("new engine: %v", err)
	}
	if err := RegisterSGraphEngine(engine); err != nil {
		b.Fatalf("register engine: %v", err)
	}
	for _, p := range chainConcurrentCases(b, schema) {
		p := p
		b.Run(p.group+"/"+p.name, func(b *testing.B) {
			Execute(p.params)
			b.ReportAllocs()
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					Execute(p.params)
				}
			})
		})
	}
}

func chainConcurrentCases(tb testing.TB, schema Schema) []chainPrepared {
	subset := []chainCase{
		{"Concurrent", "SingleScalar", `{ scalar }`, nil},
		{"Concurrent", "Rows100", `{ rows100 { id name score tag } }`, nil},
		{"Concurrent", "Rows1000", `{ rows1000 { id name score tag } }`, nil},
		{"Concurrent", "Interface100", `{ nodes { __typename id ... on CmpNodeA { a } ... on CmpNodeB { b } } }`, nil},
		{"Concurrent", "Parallel4Delay", `{ d0 d1 d2 d3 }`, nil},
	}
	return chainPrepare(tb, schema, subset)
}

// TestChainCompareConsistency 在基准之外单独跑一次一致性检查，把两条链路的差异显式列出。
// 基准本身不跳过任何用例；这个测试负责让差异可见，避免"跑得快但结果不同"被误读为性能优势。
func TestChainCompareConsistency(t *testing.T) {
	schema := newChainSchema(t)
	engine, err := NewSGraphEngine(&schema, nil, nil)
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	if err := RegisterSGraphEngine(engine); err != nil {
		t.Fatalf("register engine: %v", err)
	}
	prepared := chainPrepare(t, schema, chainCases())
	mismatches := 0
	for _, p := range prepared {
		status := "OK  "
		if p.mismatch != "" {
			status = "DIFF"
			mismatches++
		}
		fmt.Printf("%s  %-22s %-24s req=%6dB  nativeResp=%8dB  sgraphResp=%8dB  %s\n",
			status, p.group, p.name, p.reqBytes, p.nativeBytes, p.sgraphBytes, p.mismatch)
	}
	fmt.Printf("CONSISTENCY: %d/%d 用例两链路结果一致\n", len(prepared)-mismatches, len(prepared))
}
