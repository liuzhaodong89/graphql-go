package graphql

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/graphql-go/graphql/language/ast"
	"github.com/graphql-go/graphql/language/parser"
	"github.com/graphql-go/graphql/language/source"
)

var (
	benchmarkNativeResultSink     *Result
	benchmarkNativeDocumentSink   *ast.Document
	benchmarkNativeValidationSink ValidationResult
	benchmarkNativeBytesSink      []byte
)

type nativeBenchmarkCase struct {
	name             string
	query            string
	variables        map[string]any
	context          context.Context
	warmContext      context.Context
	expectedErrors   int
	skipReason       string
	mutationOrderLen int
	recorder         *nativeBenchmarkRecorder
}

type nativeBenchmarkFixture struct {
	schema        Schema
	smallPayload  map[string]any
	items10       []any
	items100      []any
	items1000     []any
	items2000     []any
	matrix10      []any
	deep          map[string]any
	abstract100   []any
	fallback100   []any
	bulkInputs100 []any
	bulkInputs1K  []any
}

type nativeBenchmarkProfileKey struct{}

type nativeBenchmarkProfile struct {
	delays   [32]time.Duration
	recorder *nativeBenchmarkRecorder
}

type nativeBenchmarkRecorder struct {
	mu     sync.Mutex
	values []int
}

func (r *nativeBenchmarkRecorder) append(value int) {
	r.mu.Lock()
	r.values = append(r.values, value)
	r.mu.Unlock()
}

func (r *nativeBenchmarkRecorder) snapshot() []int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]int(nil), r.values...)
}

func (r *nativeBenchmarkRecorder) reset() {
	r.mu.Lock()
	r.values = r.values[:0]
	r.mu.Unlock()
}

// BenchmarkNativeGraphQLExecuteCPU 只计量已解析、已验证文档的 graphql-go 原生执行阶段。
func BenchmarkNativeGraphQLExecuteCPU(b *testing.B) {
	fixture := newNativeBenchmarkFixture(b)
	cases := nativeBenchmarkExecutionCases(fixture)
	for _, tc := range cases {
		tc := tc
		b.Run(tc.name, func(b *testing.B) {
			benchmarkNativeExecuteCase(b, fixture.schema, tc)
		})
	}
}

// BenchmarkNativeGraphQLResolverLatency 对比 Query 并行根字段与 Mutation 串行根字段的等待时间放大效应。
func BenchmarkNativeGraphQLResolverLatency(b *testing.B) {
	fixture := newNativeBenchmarkFixture(b)
	cases := nativeBenchmarkLatencyCases()
	for _, tc := range cases {
		tc := tc
		b.Run(tc.name, func(b *testing.B) {
			benchmarkNativeExecuteCase(b, fixture.schema, tc)
		})
	}
}

// BenchmarkNativeGraphQLPipeline 分离测量 Parse、Validate、原生 Execute 和 JSON Marshal，避免混淆阶段成本。
func BenchmarkNativeGraphQLPipeline(b *testing.B) {
	fixture := newNativeBenchmarkFixture(b)
	tiers := []struct {
		name   string
		target int
	}{
		{name: "Small256B", target: 256},
		{name: "Medium4K", target: 4 << 10},
		{name: "Large32K", target: 32 << 10},
		{name: "XLarge256K", target: 256 << 10},
	}

	for _, tier := range tiers {
		tier := tier
		query := nativeBenchmarkQueryBodyAtLeast(tier.target)
		doc := nativeBenchmarkParseAndValidate(b, fixture.schema, tier.name, query)

		b.Run("Parse/"+tier.name, func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			wallStart, cpuStart, cpuOK := nativeBenchmarkStartTiming()
			for i := 0; i < b.N; i++ {
				parsed, err := parser.Parse(parser.ParseParams{Source: source.NewSource(&source.Source{
					Body: []byte(query),
					Name: tier.name + ".graphql",
				})})
				if err != nil {
					b.Fatalf("parse %s failed: %v", tier.name, err)
				}
				benchmarkNativeDocumentSink = parsed
			}
			b.StopTimer()
			b.ReportMetric(float64(len(query)), "request-B/op")
			nativeBenchmarkReportTiming(b, wallStart, cpuStart, cpuOK)
		})

		b.Run("Validate/"+tier.name, func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			wallStart, cpuStart, cpuOK := nativeBenchmarkStartTiming()
			for i := 0; i < b.N; i++ {
				validation := ValidateDocument(&fixture.schema, doc, nil)
				if !validation.IsValid {
					b.Fatalf("validate %s failed: %#v", tier.name, validation.Errors)
				}
				benchmarkNativeValidationSink = validation
			}
			b.StopTimer()
			b.ReportMetric(float64(len(query)), "request-B/op")
			nativeBenchmarkReportTiming(b, wallStart, cpuStart, cpuOK)
		})

		b.Run("EndToEnd/"+tier.name, func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			wallStart, cpuStart, cpuOK := nativeBenchmarkStartTiming()
			for i := 0; i < b.N; i++ {
				parsed, err := parser.Parse(parser.ParseParams{Source: source.NewSource(&source.Source{
					Body: []byte(query),
					Name: tier.name + ".graphql",
				})})
				if err != nil {
					b.Fatalf("parse %s failed: %v", tier.name, err)
				}
				validation := ValidateDocument(&fixture.schema, parsed, nil)
				if !validation.IsValid {
					b.Fatalf("validate %s failed: %#v", tier.name, validation.Errors)
				}
				result := ExecuteGraphQLGo(ExecuteParams{
					Schema:  fixture.schema,
					AST:     parsed,
					Context: context.Background(),
				})
				if result == nil || len(result.Errors) != 0 {
					b.Fatalf("end-to-end %s returned errors: %#v", tier.name, result)
				}
				benchmarkNativeResultSink = result
			}
			b.StopTimer()
			b.ReportMetric(float64(len(query)), "request-B/op")
			nativeBenchmarkReportTiming(b, wallStart, cpuStart, cpuOK)
		})

		warm := ExecuteGraphQLGo(ExecuteParams{
			Schema:  fixture.schema,
			AST:     doc,
			Context: context.Background(),
		})
		if warm == nil || len(warm.Errors) != 0 {
			b.Fatalf("marshal setup %s returned errors: %#v", tier.name, warm)
		}
		responseJSON, err := json.Marshal(warm)
		if err != nil {
			b.Fatalf("marshal setup %s failed: %v", tier.name, err)
		}
		b.Run("Marshal/"+tier.name, func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			wallStart, cpuStart, cpuOK := nativeBenchmarkStartTiming()
			for i := 0; i < b.N; i++ {
				encoded, marshalErr := json.Marshal(warm)
				if marshalErr != nil {
					b.Fatalf("marshal %s failed: %v", tier.name, marshalErr)
				}
				benchmarkNativeBytesSink = encoded
			}
			b.StopTimer()
			b.ReportMetric(float64(len(responseJSON)), "response-B/op")
			nativeBenchmarkReportTiming(b, wallStart, cpuStart, cpuOK)
		})
	}
}

// BenchmarkNativeGraphQLConcurrentRequests 测量多个请求同时进入原生 Execute 时的吞吐和进程 CPU。
func BenchmarkNativeGraphQLConcurrentRequests(b *testing.B) {
	fixture := newNativeBenchmarkFixture(b)
	cases := []nativeBenchmarkCase{
		{name: "QueryFastWide8", query: nativeBenchmarkWideQuery(8), context: context.Background()},
		{
			name:      "QueryList1000",
			query:     `query($n:Int!,$bytes:Int!){items(n:$n,bytes:$bytes){id name blob score tags}}`,
			variables: map[string]any{"n": 1000, "bytes": 1024},
			context:   context.Background(),
		},
		{
			name:    "QueryLatency8",
			query:   nativeBenchmarkSlowQuery(8),
			context: nativeBenchmarkContext(nativeBenchmarkDelays(8, 200*time.Microsecond, 0), nil),
		},
		{
			name:    "MutationFastWide8",
			query:   nativeBenchmarkMutationQuery(8),
			context: context.Background(),
		},
	}

	for _, tc := range cases {
		tc := tc
		doc := nativeBenchmarkParseAndValidate(b, fixture.schema, tc.name, tc.query)
		params := ExecuteParams{
			Schema:  fixture.schema,
			AST:     doc,
			Args:    tc.variables,
			Context: tc.context,
		}
		warm := ExecuteGraphQLGo(params)
		if warm == nil || len(warm.Errors) != 0 {
			b.Fatalf("warm-up %s returned errors: %#v", tc.name, warm)
		}
		responseJSON, err := json.Marshal(warm)
		if err != nil {
			b.Fatalf("marshal warm-up %s failed: %v", tc.name, err)
		}
		variablesJSON := nativeBenchmarkVariablesJSON(b, tc.variables)

		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			wallStart, cpuStart, cpuOK := nativeBenchmarkStartTiming()
			b.RunParallel(func(pb *testing.PB) {
				var last *Result
				for pb.Next() {
					last = ExecuteGraphQLGo(params)
				}
				_ = last
			})
			b.StopTimer()
			b.ReportMetric(float64(len(tc.query)), "request-B/op")
			b.ReportMetric(float64(len(variablesJSON)), "variables-B/op")
			b.ReportMetric(float64(len(responseJSON)), "response-B/op")
			nativeBenchmarkReportTiming(b, wallStart, cpuStart, cpuOK)
		})
	}
}

func benchmarkNativeExecuteCase(b *testing.B, schema Schema, tc nativeBenchmarkCase) {
	b.Helper()
	if tc.skipReason != "" {
		b.Skip(tc.skipReason)
	}
	if tc.recorder != nil {
		// testing.B 会为校准 b.N 多次进入子 benchmark，每次预热必须独立校验 Mutation 顺序。
		tc.recorder.reset()
	}
	doc := nativeBenchmarkParseAndValidate(b, schema, tc.name, tc.query)
	warmContext := tc.warmContext
	if warmContext == nil {
		warmContext = tc.context
	}
	if warmContext == nil {
		warmContext = context.Background()
	}
	ctx := tc.context
	if ctx == nil {
		ctx = context.Background()
	}

	warm := ExecuteGraphQLGo(ExecuteParams{
		Schema:  schema,
		AST:     doc,
		Args:    tc.variables,
		Context: warmContext,
	})
	if warm == nil {
		b.Fatal("warm-up returned nil result")
	}
	if len(warm.Errors) != tc.expectedErrors {
		b.Fatalf("warm-up returned %d errors, want %d: %#v", len(warm.Errors), tc.expectedErrors, warm.Errors)
	}
	if tc.expectedErrors == 0 && warm.Data == nil {
		b.Fatal("warm-up returned nil data without errors")
	}
	if tc.mutationOrderLen > 0 {
		if tc.recorder == nil {
			b.Fatal("mutation order case has no recorder")
		}
		order := tc.recorder.snapshot()
		if len(order) != tc.mutationOrderLen {
			b.Fatalf("mutation invoked %d resolvers, want %d: %v", len(order), tc.mutationOrderLen, order)
		}
		for index, value := range order {
			if value != index {
				b.Fatalf("mutation resolver order at %d = %d, want %d: %v", index, value, index, order)
			}
		}
	}

	responseJSON, err := json.Marshal(warm)
	if err != nil {
		b.Fatalf("marshal warm-up result failed: %v", err)
	}
	variablesJSON := nativeBenchmarkVariablesJSON(b, tc.variables)
	params := ExecuteParams{
		Schema:  schema,
		AST:     doc,
		Args:    tc.variables,
		Context: ctx,
	}

	b.ReportAllocs()
	b.ResetTimer()
	wallStart, cpuStart, cpuOK := nativeBenchmarkStartTiming()
	for i := 0; i < b.N; i++ {
		benchmarkNativeResultSink = ExecuteGraphQLGo(params)
	}
	b.StopTimer()
	b.ReportMetric(float64(len(tc.query)), "request-B/op")
	b.ReportMetric(float64(len(variablesJSON)), "variables-B/op")
	b.ReportMetric(float64(len(responseJSON)), "response-B/op")
	nativeBenchmarkReportTiming(b, wallStart, cpuStart, cpuOK)
}

func nativeBenchmarkStartTiming() (time.Time, time.Duration, bool) {
	cpuStart, cpuOK := benchmarkNativeProcessCPUTime()
	return time.Now(), cpuStart, cpuOK
}

func nativeBenchmarkReportTiming(b *testing.B, wallStart time.Time, cpuStart time.Duration, cpuOK bool) {
	b.Helper()
	wallElapsed := time.Since(wallStart)
	if b.N > 0 && wallElapsed > 0 {
		b.ReportMetric(float64(b.N)/wallElapsed.Seconds(), "op/s")
	}
	if !cpuOK || b.N == 0 {
		return
	}
	cpuEnd, ok := benchmarkNativeProcessCPUTime()
	if !ok || cpuEnd < cpuStart {
		return
	}
	cpuElapsed := cpuEnd - cpuStart
	b.ReportMetric(float64(cpuElapsed.Nanoseconds())/float64(b.N), "cpu-ns/op")
	if wallElapsed > 0 {
		b.ReportMetric(float64(cpuElapsed)/float64(wallElapsed)*100, "cpu-percent")
	}
}

func nativeBenchmarkExecutionCases(fixture *nativeBenchmarkFixture) []nativeBenchmarkCase {
	inputSmall := nativeBenchmarkInput(32)
	input64K := nativeBenchmarkInput(64 << 10)
	input1M := nativeBenchmarkInput(1 << 20)

	cases := []nativeBenchmarkCase{
		{name: "Query/RootScalar1", query: `{f000}`},
		{name: "Query/RootWide8", query: nativeBenchmarkWideQuery(8)},
		{name: "Query/RootWide64", query: nativeBenchmarkWideQuery(64)},
		{name: "Query/RootWide256", query: nativeBenchmarkWideQuery(256)},
		{name: "Query/Deep4", query: nativeBenchmarkDeepOperation("query", "deep", 4)},
		{name: "Query/Deep8", query: nativeBenchmarkDeepOperation("query", "deep", 8)},
		{name: "Query/Deep16", query: nativeBenchmarkDeepOperation("query", "deep", 16)},
		{
			name:      "Query/ListObject10Small",
			query:     `query($n:Int!,$bytes:Int!){items(n:$n,bytes:$bytes){id name blob score tags}}`,
			variables: map[string]any{"n": 10, "bytes": 32},
		},
		{
			name:      "Query/ListObject100Medium",
			query:     `query($n:Int!,$bytes:Int!){items(n:$n,bytes:$bytes){id name blob score tags}}`,
			variables: map[string]any{"n": 100, "bytes": 512},
		},
		{
			name:      "Query/ListObject1000Large",
			query:     `query($n:Int!,$bytes:Int!){items(n:$n,bytes:$bytes){id name blob score tags}}`,
			variables: map[string]any{"n": 1000, "bytes": 1024},
		},
		{
			name:      "Query/ListObject2000XLarge",
			query:     `query($n:Int!,$bytes:Int!){items(n:$n,bytes:$bytes){id name blob score tags}}`,
			variables: map[string]any{"n": 2000, "bytes": 4096},
		},
		{
			name:      "Query/NestedList10x10",
			query:     `query($rows:Int!,$cols:Int!){matrix(rows:$rows,cols:$cols){id name score}}`,
			variables: map[string]any{"rows": 10, "cols": 10},
		},
		{name: "Query/ArgumentsLiteral", query: `{items(n:10,bytes:32){id name score tags}}`},
		{
			name:      "Query/VariableInputSmall",
			query:     `query($input:NativeBenchInput!){echo(input:$input)}`,
			variables: map[string]any{"input": inputSmall},
		},
		{
			name:      "Query/VariableInput64K",
			query:     `query($input:NativeBenchInput!){echo(input:$input)}`,
			variables: map[string]any{"input": input64K},
		},
		{
			name:      "Query/VariableInput1M",
			query:     `query($input:NativeBenchInput!){echo(input:$input)}`,
			variables: map[string]any{"input": input1M},
		},
		{
			name: "Query/FragmentsAliasesDirectives",
			query: `query Native($include:Boolean!){
				first:payload{id ...PayloadCore blob @include(if:$include) aliasName:name}
				second:payload{...PayloadCore}
			} fragment PayloadCore on NativeBenchPayload{name score tags}`,
			variables: map[string]any{"include": true},
		},
		{
			name:      "Query/Interface100",
			query:     `query($n:Int!){nodes(n:$n){__typename id ... on NativeBenchUser{name} ... on NativeBenchRobot{serial}}}`,
			variables: map[string]any{"n": 100},
		},
		{
			name:      "Query/UnionResolveType100",
			query:     `query($n:Int!){search(n:$n){__typename ... on NativeBenchUser{id name} ... on NativeBenchRobot{id serial}}}`,
			variables: map[string]any{"n": 100},
		},
		{
			name:      "Query/UnionIsTypeOf100",
			query:     `query($n:Int!){fallback(n:$n){__typename ... on NativeBenchAlpha{value} ... on NativeBenchBeta{value}}}`,
			variables: map[string]any{"n": 100},
		},
		{name: "Query/IntrospectionType", query: `{__type(name:"NativeBenchPayload"){name kind fields{name type{kind name ofType{kind name}}}}}`},
		{
			name: "Query/Combined",
			query: `query($n:Int!,$bytes:Int!){
				f000 payload{id name tags} deep{value next{value next{value}}}
				items(n:$n,bytes:$bytes){id name score}
				nodes(n:100){__typename id ... on NativeBenchUser{name}}
				search(n:100){__typename ... on NativeBenchRobot{serial}}
			}`,
			variables: map[string]any{"n": 100, "bytes": 512},
		},
		{name: "Query/NullableResolverError", query: `{nullableError}`, expectedErrors: 1},
		{
			name:       "Query/NonNullResolverError",
			query:      `{nonNullError}`,
			skipReason: "graphql-go native executor panics in the field goroutine for a root non-null resolver error",
		},
	}

	mutationCases := []struct {
		name  string
		query string
	}{
		{name: "Mutation/Scalar1", query: nativeBenchmarkMutationQuery(1)},
		{name: "Mutation/Wide2", query: nativeBenchmarkMutationQuery(2)},
		{name: "Mutation/Wide8", query: nativeBenchmarkMutationQuery(8)},
		{name: "Mutation/Wide32", query: nativeBenchmarkMutationQuery(32)},
	}
	for _, item := range mutationCases {
		cases = append(cases, nativeBenchmarkCase{
			name:    item.name,
			query:   item.query,
			context: context.Background(),
		})
	}

	cases = append(cases,
		nativeBenchmarkCase{
			name:      "Mutation/InputSmall",
			query:     `mutation($input:NativeBenchInput!){update(input:$input){id name score}}`,
			variables: map[string]any{"input": inputSmall},
		},
		nativeBenchmarkCase{
			name:      "Mutation/Input64K",
			query:     `mutation($input:NativeBenchInput!){update(input:$input){id name score}}`,
			variables: map[string]any{"input": input64K},
		},
		nativeBenchmarkCase{
			name:      "Mutation/Input1M",
			query:     `mutation($input:NativeBenchInput!){update(input:$input){id name score}}`,
			variables: map[string]any{"input": input1M},
		},
		nativeBenchmarkCase{
			name:      "Mutation/ListResponse100",
			query:     `mutation($input:[NativeBenchInput!]!){bulkUpdate(input:$input){id name blob score tags}}`,
			variables: map[string]any{"input": fixture.bulkInputs100},
		},
		nativeBenchmarkCase{
			name:      "Mutation/ListResponse1000",
			query:     `mutation($input:[NativeBenchInput!]!){bulkUpdate(input:$input){id name blob score tags}}`,
			variables: map[string]any{"input": fixture.bulkInputs1K},
		},
		nativeBenchmarkCase{name: "Mutation/DeepResponse", query: nativeBenchmarkDeepOperation("mutation", "writeDeep", 16)},
		nativeBenchmarkCase{
			name: "Mutation/FragmentsAliasesDirectives",
			query: `mutation NativeMutation($input:NativeBenchInput!,$include:Boolean!){
				a:write00(input:$input){...PayloadCore}
				b:write01(input:$input) @include(if:$include){id name}
			} fragment PayloadCore on NativeBenchPayload{id name score tags}`,
			variables: map[string]any{"input": inputSmall, "include": true},
		},
		nativeBenchmarkCase{name: "Mutation/NullableResolverError", query: `mutation{nullableFailure}`, expectedErrors: 1},
	)
	return cases
}

func nativeBenchmarkLatencyCases() []nativeBenchmarkCase {
	const (
		lowDelay  = 200 * time.Microsecond
		highDelay = 20 * time.Millisecond
	)
	cases := make([]nativeBenchmarkCase, 0, 12)
	for _, width := range []int{2, 8, 32} {
		cases = append(cases,
			nativeBenchmarkCase{
				name:    fmt.Sprintf("Query/LowDelay%d", width),
				query:   nativeBenchmarkSlowQuery(width),
				context: nativeBenchmarkContext(nativeBenchmarkDelays(width, lowDelay, 0), nil),
			},
			nativeBenchmarkCase{
				name:    fmt.Sprintf("Query/HighDelay%d", width),
				query:   nativeBenchmarkSlowQuery(width),
				context: nativeBenchmarkContext(nativeBenchmarkDelays(width, highDelay, 0), nil),
			},
		)
	}
	cases = append(cases, nativeBenchmarkCase{
		name:    "Query/MixedDelay8",
		query:   nativeBenchmarkSlowQuery(8),
		context: nativeBenchmarkContext(nativeBenchmarkDelays(8, lowDelay, highDelay), nil),
	})

	for _, width := range []int{2, 8} {
		for _, delayCase := range []struct {
			name  string
			delay time.Duration
		}{
			{name: "LowDelay", delay: lowDelay},
			{name: "HighDelay", delay: highDelay},
		} {
			recorder := &nativeBenchmarkRecorder{}
			delays := nativeBenchmarkDelays(width, delayCase.delay, 0)
			cases = append(cases, nativeBenchmarkCase{
				name:             fmt.Sprintf("Mutation/%s%d", delayCase.name, width),
				query:            nativeBenchmarkSlowMutationQuery(width),
				context:          nativeBenchmarkContext(delays, nil),
				warmContext:      nativeBenchmarkContext(delays, recorder),
				mutationOrderLen: width,
				recorder:         recorder,
			})
		}
	}
	recorder := &nativeBenchmarkRecorder{}
	mixed := nativeBenchmarkDelays(8, lowDelay, highDelay)
	cases = append(cases, nativeBenchmarkCase{
		name:             "Mutation/MixedDelay8",
		query:            nativeBenchmarkSlowMutationQuery(8),
		context:          nativeBenchmarkContext(mixed, nil),
		warmContext:      nativeBenchmarkContext(mixed, recorder),
		mutationOrderLen: 8,
		recorder:         recorder,
	})
	return cases
}

func newNativeBenchmarkFixture(tb testing.TB) *nativeBenchmarkFixture {
	tb.Helper()
	fixture := &nativeBenchmarkFixture{}
	fixture.smallPayload = nativeBenchmarkPayload(0, 32)
	fixture.items10 = nativeBenchmarkPayloads(10, 32)
	fixture.items100 = nativeBenchmarkPayloads(100, 512)
	fixture.items1000 = nativeBenchmarkPayloads(1000, 1024)
	fixture.items2000 = nativeBenchmarkPayloads(2000, 4096)
	fixture.matrix10 = nativeBenchmarkMatrix(10, 10, 128)
	fixture.deep = nativeBenchmarkDeepValue(16)
	fixture.abstract100 = nativeBenchmarkAbstractValues(100)
	fixture.fallback100 = nativeBenchmarkFallbackValues(100)
	fixture.bulkInputs100 = nativeBenchmarkInputList(100)
	fixture.bulkInputs1K = nativeBenchmarkInputList(1000)
	fixture.schema = fixture.buildSchema(tb)
	return fixture
}

func (fixture *nativeBenchmarkFixture) buildSchema(tb testing.TB) Schema {
	tb.Helper()
	payloadType := NewObject(ObjectConfig{
		Name: "NativeBenchPayload",
		Fields: Fields{
			"id":    &Field{Type: NewNonNull(ID)},
			"name":  &Field{Type: String},
			"blob":  &Field{Type: String},
			"score": &Field{Type: Int},
			"tags":  &Field{Type: NewList(NewNonNull(String))},
		},
	})
	inputType := NewInputObject(InputObjectConfig{
		Name: "NativeBenchInput",
		Fields: InputObjectConfigFieldMap{
			"text":  &InputObjectFieldConfig{Type: NewNonNull(String)},
			"count": &InputObjectFieldConfig{Type: Int},
			"tags":  &InputObjectFieldConfig{Type: NewList(NewNonNull(String))},
		},
	})

	deepTypes := make([]*Object, 16)
	for index := len(deepTypes) - 1; index >= 0; index-- {
		fields := Fields{"value": &Field{Type: String}}
		if index+1 < len(deepTypes) {
			fields["next"] = &Field{Type: deepTypes[index+1]}
		}
		deepTypes[index] = NewObject(ObjectConfig{
			Name:   fmt.Sprintf("NativeBenchDeep%02d", index+1),
			Fields: fields,
		})
	}

	var nodeType *Interface
	var userType *Object
	var robotType *Object
	nodeType = NewInterface(InterfaceConfig{
		Name: "NativeBenchNode",
		Fields: Fields{
			"id": &Field{Type: NewNonNull(ID)},
		},
		ResolveType: func(p ResolveTypeParams) *Object {
			value, _ := p.Value.(map[string]any)
			switch value["kind"] {
			case "user":
				return userType
			case "robot":
				return robotType
			default:
				return nil
			}
		},
	})
	userType = NewObject(ObjectConfig{
		Name:       "NativeBenchUser",
		Interfaces: []*Interface{nodeType},
		Fields: Fields{
			"id":   &Field{Type: NewNonNull(ID)},
			"name": &Field{Type: String},
		},
	})
	robotType = NewObject(ObjectConfig{
		Name:       "NativeBenchRobot",
		Interfaces: []*Interface{nodeType},
		Fields: Fields{
			"id":     &Field{Type: NewNonNull(ID)},
			"serial": &Field{Type: String},
		},
	})
	searchType := NewUnion(UnionConfig{
		Name:  "NativeBenchSearchResult",
		Types: []*Object{userType, robotType},
		ResolveType: func(p ResolveTypeParams) *Object {
			value, _ := p.Value.(map[string]any)
			switch value["kind"] {
			case "user":
				return userType
			case "robot":
				return robotType
			default:
				return nil
			}
		},
	})
	alphaType := NewObject(ObjectConfig{
		Name:   "NativeBenchAlpha",
		Fields: Fields{"value": &Field{Type: String}},
		IsTypeOf: func(p IsTypeOfParams) bool {
			value, _ := p.Value.(map[string]any)
			return value["kind"] == "alpha"
		},
	})
	betaType := NewObject(ObjectConfig{
		Name:   "NativeBenchBeta",
		Fields: Fields{"value": &Field{Type: String}},
		IsTypeOf: func(p IsTypeOfParams) bool {
			value, _ := p.Value.(map[string]any)
			return value["kind"] == "beta"
		},
	})
	fallbackType := NewUnion(UnionConfig{
		Name:  "NativeBenchFallbackResult",
		Types: []*Object{alphaType, betaType},
	})

	queryFields := Fields{
		"payload": &Field{
			Type: payloadType,
			Resolve: func(p ResolveParams) (any, error) {
				return fixture.smallPayload, nil
			},
		},
		"deep": &Field{
			Type: deepTypes[0],
			Resolve: func(p ResolveParams) (any, error) {
				return fixture.deep, nil
			},
		},
		"items": &Field{
			Type: NewList(payloadType),
			Args: FieldConfigArgument{
				"n":     &ArgumentConfig{Type: NewNonNull(Int)},
				"bytes": &ArgumentConfig{Type: NewNonNull(Int)},
			},
			Resolve: func(p ResolveParams) (any, error) {
				n, _ := p.Args["n"].(int)
				bytes, _ := p.Args["bytes"].(int)
				switch {
				case n == 10 && bytes == 32:
					return fixture.items10, nil
				case n == 100 && bytes == 512:
					return fixture.items100, nil
				case n == 1000 && bytes == 1024:
					return fixture.items1000, nil
				case n == 2000 && bytes == 4096:
					return fixture.items2000, nil
				default:
					return nil, fmt.Errorf("unsupported benchmark dataset n=%d bytes=%d", n, bytes)
				}
			},
		},
		"matrix": &Field{
			Type: NewList(NewList(payloadType)),
			Args: FieldConfigArgument{
				"rows": &ArgumentConfig{Type: NewNonNull(Int)},
				"cols": &ArgumentConfig{Type: NewNonNull(Int)},
			},
			Resolve: func(p ResolveParams) (any, error) {
				rows, _ := p.Args["rows"].(int)
				cols, _ := p.Args["cols"].(int)
				if rows != 10 || cols != 10 {
					return nil, fmt.Errorf("unsupported benchmark matrix %dx%d", rows, cols)
				}
				return fixture.matrix10, nil
			},
		},
		"echo": &Field{
			Type: Int,
			Args: FieldConfigArgument{
				"input": &ArgumentConfig{Type: NewNonNull(inputType)},
			},
			Resolve: func(p ResolveParams) (any, error) {
				input, _ := p.Args["input"].(map[string]any)
				text, _ := input["text"].(string)
				return len(text), nil
			},
		},
		"nodes": &Field{
			Type: NewList(nodeType),
			Args: FieldConfigArgument{"n": &ArgumentConfig{Type: NewNonNull(Int)}},
			Resolve: func(p ResolveParams) (any, error) {
				if p.Args["n"] != 100 {
					return nil, errors.New("unsupported benchmark abstract dataset")
				}
				return fixture.abstract100, nil
			},
		},
		"search": &Field{
			Type: NewList(searchType),
			Args: FieldConfigArgument{"n": &ArgumentConfig{Type: NewNonNull(Int)}},
			Resolve: func(p ResolveParams) (any, error) {
				if p.Args["n"] != 100 {
					return nil, errors.New("unsupported benchmark union dataset")
				}
				return fixture.abstract100, nil
			},
		},
		"fallback": &Field{
			Type: NewList(fallbackType),
			Args: FieldConfigArgument{"n": &ArgumentConfig{Type: NewNonNull(Int)}},
			Resolve: func(p ResolveParams) (any, error) {
				if p.Args["n"] != 100 {
					return nil, errors.New("unsupported benchmark fallback dataset")
				}
				return fixture.fallback100, nil
			},
		},
		"nullableError": &Field{
			Type: String,
			Resolve: func(p ResolveParams) (any, error) {
				return nil, errors.New("native benchmark nullable resolver error")
			},
		},
		"nonNullError": &Field{
			Type: NewNonNull(String),
			Resolve: func(p ResolveParams) (any, error) {
				return nil, errors.New("native benchmark non-null resolver error")
			},
		},
	}
	for index := 0; index < 256; index++ {
		value := fmt.Sprintf("value-%03d", index)
		queryFields[fmt.Sprintf("f%03d", index)] = &Field{
			Type: String,
			Resolve: func(p ResolveParams) (any, error) {
				return value, nil
			},
		}
	}
	for index := 0; index < 32; index++ {
		fieldIndex := index
		queryFields[fmt.Sprintf("qslow%02d", index)] = &Field{
			Type: String,
			Resolve: func(p ResolveParams) (any, error) {
				nativeBenchmarkWait(p.Context, fieldIndex)
				return fmt.Sprintf("qslow-%02d", fieldIndex), nil
			},
		}
	}

	mutationFields := Fields{
		"update": &Field{
			Type: payloadType,
			Args: FieldConfigArgument{"input": &ArgumentConfig{Type: NewNonNull(inputType)}},
			Resolve: func(p ResolveParams) (any, error) {
				return fixture.smallPayload, nil
			},
		},
		"bulkUpdate": &Field{
			Type: NewList(payloadType),
			Args: FieldConfigArgument{"input": &ArgumentConfig{Type: NewNonNull(NewList(NewNonNull(inputType)))}},
			Resolve: func(p ResolveParams) (any, error) {
				inputs, _ := p.Args["input"].([]any)
				switch len(inputs) {
				case 100:
					return fixture.items100, nil
				case 1000:
					return fixture.items1000, nil
				default:
					return nil, fmt.Errorf("unsupported benchmark bulk input length %d", len(inputs))
				}
			},
		},
		"writeDeep": &Field{
			Type: deepTypes[0],
			Resolve: func(p ResolveParams) (any, error) {
				return fixture.deep, nil
			},
		},
		"nullableFailure": &Field{
			Type: String,
			Resolve: func(p ResolveParams) (any, error) {
				return nil, errors.New("native benchmark mutation resolver error")
			},
		},
	}
	for index := 0; index < 32; index++ {
		fieldIndex := index
		value := fmt.Sprintf("mutation-%02d", index)
		mutationFields[fmt.Sprintf("touch%02d", index)] = &Field{
			Type: String,
			Resolve: func(p ResolveParams) (any, error) {
				return value, nil
			},
		}
		mutationFields[fmt.Sprintf("mslow%02d", index)] = &Field{
			Type: String,
			Resolve: func(p ResolveParams) (any, error) {
				nativeBenchmarkWait(p.Context, fieldIndex)
				return fmt.Sprintf("mslow-%02d", fieldIndex), nil
			},
		}
		mutationFields[fmt.Sprintf("write%02d", index)] = &Field{
			Type: payloadType,
			Args: FieldConfigArgument{"input": &ArgumentConfig{Type: NewNonNull(inputType)}},
			Resolve: func(p ResolveParams) (any, error) {
				return fixture.smallPayload, nil
			},
		}
	}

	schema, err := NewSchema(SchemaConfig{
		Query:    NewObject(ObjectConfig{Name: "NativeBenchQuery", Fields: queryFields}),
		Mutation: NewObject(ObjectConfig{Name: "NativeBenchMutation", Fields: mutationFields}),
		Types: []Type{
			payloadType,
			inputType,
			nodeType,
			userType,
			robotType,
			searchType,
			alphaType,
			betaType,
			fallbackType,
		},
	})
	if err != nil {
		tb.Fatalf("build native benchmark schema failed: %v", err)
	}
	return schema
}

func nativeBenchmarkParseAndValidate(tb testing.TB, schema Schema, name string, query string) *ast.Document {
	tb.Helper()
	doc, err := parser.Parse(parser.ParseParams{Source: source.NewSource(&source.Source{
		Body: []byte(query),
		Name: name + ".graphql",
	})})
	if err != nil {
		tb.Fatalf("parse %s failed: %v", name, err)
	}
	validation := ValidateDocument(&schema, doc, nil)
	if !validation.IsValid {
		tb.Fatalf("validate %s failed: %#v", name, validation.Errors)
	}
	return doc
}

func nativeBenchmarkVariablesJSON(tb testing.TB, variables map[string]any) []byte {
	tb.Helper()
	if len(variables) == 0 {
		return nil
	}
	encoded, err := json.Marshal(variables)
	if err != nil {
		tb.Fatalf("marshal variables failed: %v", err)
	}
	return encoded
}

func nativeBenchmarkContext(delays [32]time.Duration, recorder *nativeBenchmarkRecorder) context.Context {
	return context.WithValue(context.Background(), nativeBenchmarkProfileKey{}, &nativeBenchmarkProfile{
		delays:   delays,
		recorder: recorder,
	})
}

func nativeBenchmarkDelays(width int, defaultDelay time.Duration, lastDelay time.Duration) [32]time.Duration {
	var delays [32]time.Duration
	for index := 0; index < width && index < len(delays); index++ {
		delays[index] = defaultDelay
	}
	if width > 0 && width <= len(delays) && lastDelay > 0 {
		delays[width-1] = lastDelay
	}
	return delays
}

func nativeBenchmarkWait(ctx context.Context, index int) {
	profile, _ := ctx.Value(nativeBenchmarkProfileKey{}).(*nativeBenchmarkProfile)
	if profile == nil || index < 0 || index >= len(profile.delays) {
		return
	}
	if delay := profile.delays[index]; delay > 0 {
		time.Sleep(delay)
	}
	if profile.recorder != nil {
		profile.recorder.append(index)
	}
}

func nativeBenchmarkWideQuery(width int) string {
	var query strings.Builder
	query.Grow(width*5 + 2)
	query.WriteByte('{')
	for index := 0; index < width; index++ {
		fmt.Fprintf(&query, " f%03d", index)
	}
	query.WriteString(" }")
	return query.String()
}

func nativeBenchmarkSlowQuery(width int) string {
	var query strings.Builder
	query.Grow(width*9 + 2)
	query.WriteByte('{')
	for index := 0; index < width; index++ {
		fmt.Fprintf(&query, " qslow%02d", index)
	}
	query.WriteString(" }")
	return query.String()
}

func nativeBenchmarkMutationQuery(width int) string {
	var query strings.Builder
	query.WriteString("mutation{")
	for index := 0; index < width; index++ {
		fmt.Fprintf(&query, "m%02d:touch%02d ", index, index)
	}
	query.WriteByte('}')
	return query.String()
}

func nativeBenchmarkSlowMutationQuery(width int) string {
	var query strings.Builder
	query.WriteString("mutation{")
	for index := 0; index < width; index++ {
		fmt.Fprintf(&query, "m%02d:mslow%02d ", index, index)
	}
	query.WriteByte('}')
	return query.String()
}

func nativeBenchmarkDeepOperation(operation string, field string, depth int) string {
	var query strings.Builder
	query.WriteString(operation)
	query.WriteString("{")
	query.WriteString(field)
	query.WriteByte('{')
	for index := 0; index < depth; index++ {
		query.WriteString("value")
		if index+1 < depth {
			query.WriteString(" next{")
		}
	}
	for index := 1; index < depth; index++ {
		query.WriteByte('}')
	}
	query.WriteString("}}")
	return query.String()
}

func nativeBenchmarkQueryBodyAtLeast(target int) string {
	var query strings.Builder
	query.Grow(target + 32)
	query.WriteString("query NativeBody{")
	for index := 0; query.Len() < target-2; index++ {
		fmt.Fprintf(&query, "a%06d:f000 ", index)
	}
	query.WriteByte('}')
	return query.String()
}

func nativeBenchmarkInput(textBytes int) map[string]any {
	return map[string]any{
		"text":  strings.Repeat("x", textBytes),
		"count": 3,
		"tags":  []any{"alpha", "beta", "gamma"},
	}
}

func nativeBenchmarkInputList(count int) []any {
	inputs := make([]any, count)
	for index := range inputs {
		inputs[index] = nativeBenchmarkInput(32)
	}
	return inputs
}

func nativeBenchmarkPayload(index int, blobBytes int) map[string]any {
	return map[string]any{
		"id":    fmt.Sprintf("payload-%06d", index),
		"name":  fmt.Sprintf("Payload %06d", index),
		"blob":  strings.Repeat("x", blobBytes),
		"score": index,
		"tags":  []any{"alpha", "beta", "gamma"},
	}
}

func nativeBenchmarkPayloads(count int, blobBytes int) []any {
	values := make([]any, count)
	for index := range values {
		values[index] = nativeBenchmarkPayload(index, blobBytes)
	}
	return values
}

func nativeBenchmarkMatrix(rows int, cols int, blobBytes int) []any {
	matrix := make([]any, rows)
	for row := 0; row < rows; row++ {
		values := make([]any, cols)
		for col := 0; col < cols; col++ {
			values[col] = nativeBenchmarkPayload(row*cols+col, blobBytes)
		}
		matrix[row] = values
	}
	return matrix
}

func nativeBenchmarkDeepValue(depth int) map[string]any {
	var value map[string]any
	for index := depth - 1; index >= 0; index-- {
		current := map[string]any{"value": fmt.Sprintf("level-%02d", index+1)}
		if value != nil {
			current["next"] = value
		}
		value = current
	}
	return value
}

func nativeBenchmarkAbstractValues(count int) []any {
	values := make([]any, count)
	for index := range values {
		if index%2 == 0 {
			values[index] = map[string]any{
				"kind": "user",
				"id":   fmt.Sprintf("user-%04d", index),
				"name": fmt.Sprintf("User %04d", index),
			}
			continue
		}
		values[index] = map[string]any{
			"kind":   "robot",
			"id":     fmt.Sprintf("robot-%04d", index),
			"serial": fmt.Sprintf("RX-%04d", index),
		}
	}
	return values
}

func nativeBenchmarkFallbackValues(count int) []any {
	values := make([]any, count)
	for index := range values {
		if index%2 == 0 {
			values[index] = map[string]any{"kind": "alpha", "value": fmt.Sprintf("A-%04d", index)}
			continue
		}
		values[index] = map[string]any{"kind": "beta", "value": fmt.Sprintf("B-%04d", index)}
	}
	return values
}
