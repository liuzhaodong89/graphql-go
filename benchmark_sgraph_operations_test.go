package graphql

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/graphql-go/graphql/language/parser"
	"github.com/graphql-go/graphql/language/source"
)

// BenchmarkSGraphExecuteCPU 复用原生 benchmark 的查询、变量和数据集，只替换为公开 SGraph 执行入口。
func BenchmarkSGraphExecuteCPU(b *testing.B) {
	fixture := newNativeBenchmarkFixture(b)
	newBenchmarkSGraphEngine(b, &fixture.schema)
	for _, tc := range nativeBenchmarkExecutionCases(fixture) {
		tc := tc
		doc := nativeBenchmarkParseAndValidate(b, fixture.schema, tc.name, tc.query)
		params := ExecuteParams{
			Schema:  fixture.schema,
			AST:     doc,
			Args:    tc.variables,
			Context: benchmarkSGraphContext(tc.context),
		}
		warm, incompatibility := benchmarkSGraphPreflight(params, tc)

		b.Run(tc.name, func(b *testing.B) {
			if incompatibility != "" {
				b.Skip(incompatibility)
			}
			benchmarkSGraphPreparedCase(b, tc, params, warm)
		})
	}
}

// BenchmarkSGraphResolverLatency 保持 resolver 延迟和字段宽度不变，用于比较 SGraph Query 并发编排。
// Mutation 经过公开 Execute 后仍回退原生串行执行，报告中必须单独标识。
func BenchmarkSGraphResolverLatency(b *testing.B) {
	fixture := newNativeBenchmarkFixture(b)
	newBenchmarkSGraphEngine(b, &fixture.schema)
	for _, tc := range nativeBenchmarkLatencyCases() {
		tc := tc
		doc := nativeBenchmarkParseAndValidate(b, fixture.schema, tc.name, tc.query)
		params := ExecuteParams{
			Schema:  fixture.schema,
			AST:     doc,
			Args:    tc.variables,
			Context: benchmarkSGraphContext(tc.context),
		}
		warm, incompatibility := benchmarkSGraphPreflight(params, tc)

		b.Run(tc.name, func(b *testing.B) {
			if incompatibility != "" {
				b.Skip(incompatibility)
			}
			benchmarkSGraphPreparedCase(b, tc, params, warm)
		})
	}
}

// BenchmarkSGraphPipeline 只重测会受执行器替换影响的 EndToEnd；Parse 和 Validate 与原生链路共用实现。
func BenchmarkSGraphPipeline(b *testing.B) {
	fixture := newNativeBenchmarkFixture(b)
	newBenchmarkSGraphEngine(b, &fixture.schema)
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
		params := ExecuteParams{
			Schema:  fixture.schema,
			AST:     doc,
			Context: context.Background(),
		}
		warm, incompatibility := benchmarkSGraphPreflight(params, nativeBenchmarkCase{
			name:  "Pipeline/" + tier.name,
			query: query,
		})
		responseJSON, marshalErr := json.Marshal(warm)
		if marshalErr != nil {
			b.Fatalf("marshal SGraph pipeline warm-up %s failed: %v", tier.name, marshalErr)
		}

		b.Run("EndToEnd/"+tier.name, func(b *testing.B) {
			if incompatibility != "" {
				b.Skip(incompatibility)
			}
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
				benchmarkNativeResultSink = Execute(ExecuteParams{
					Schema:  fixture.schema,
					AST:     parsed,
					Context: context.Background(),
				})
			}
			b.StopTimer()
			b.ReportMetric(float64(len(query)), "request-B/op")
			b.ReportMetric(float64(len(responseJSON)), "response-B/op")
			nativeBenchmarkReportTiming(b, wallStart, cpuStart, cpuOK)
		})

		b.Run("Marshal/"+tier.name, func(b *testing.B) {
			if incompatibility != "" {
				b.Skip(incompatibility)
			}
			b.ReportAllocs()
			b.ResetTimer()
			wallStart, cpuStart, cpuOK := nativeBenchmarkStartTiming()
			for i := 0; i < b.N; i++ {
				encoded, err := json.Marshal(warm)
				if err != nil {
					b.Fatalf("marshal SGraph result %s failed: %v", tier.name, err)
				}
				benchmarkNativeBytesSink = encoded
			}
			b.StopTimer()
			b.ReportMetric(float64(len(responseJSON)), "response-B/op")
			nativeBenchmarkReportTiming(b, wallStart, cpuStart, cpuOK)
		})
	}
}

// BenchmarkSGraphConcurrentRequests 使用同一个冻结 Engine 和 plan cache 承载并发请求。
func BenchmarkSGraphConcurrentRequests(b *testing.B) {
	fixture := newNativeBenchmarkFixture(b)
	newBenchmarkSGraphEngine(b, &fixture.schema)
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
			name:    "MutationFallbackFastWide8",
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
			Context: benchmarkSGraphContext(tc.context),
		}
		warm, incompatibility := benchmarkSGraphPreflight(params, tc)
		responseJSON, marshalErr := json.Marshal(warm)
		if marshalErr != nil {
			b.Fatalf("marshal SGraph concurrent warm-up %s failed: %v", tc.name, marshalErr)
		}
		variablesJSON := nativeBenchmarkVariablesJSON(b, tc.variables)

		b.Run(tc.name, func(b *testing.B) {
			if incompatibility != "" {
				b.Skip(incompatibility)
			}
			b.ReportAllocs()
			b.ResetTimer()
			wallStart, cpuStart, cpuOK := nativeBenchmarkStartTiming()
			b.RunParallel(func(pb *testing.PB) {
				var last *Result
				for pb.Next() {
					last = Execute(params)
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

func benchmarkSGraphPreparedCase(b *testing.B, tc nativeBenchmarkCase, params ExecuteParams, warm *Result) {
	b.Helper()
	responseJSON, err := json.Marshal(warm)
	if err != nil {
		b.Fatalf("marshal SGraph warm-up result failed: %v", err)
	}
	variablesJSON := nativeBenchmarkVariablesJSON(b, tc.variables)

	b.ReportAllocs()
	b.ResetTimer()
	wallStart, cpuStart, cpuOK := nativeBenchmarkStartTiming()
	for i := 0; i < b.N; i++ {
		benchmarkNativeResultSink = Execute(params)
	}
	b.StopTimer()
	b.ReportMetric(float64(len(tc.query)), "request-B/op")
	b.ReportMetric(float64(len(variablesJSON)), "variables-B/op")
	b.ReportMetric(float64(len(responseJSON)), "response-B/op")
	nativeBenchmarkReportTiming(b, wallStart, cpuStart, cpuOK)
}

func benchmarkSGraphPreflight(params ExecuteParams, tc nativeBenchmarkCase) (*Result, string) {
	sgraphResult := Execute(params)
	if sgraphResult == nil {
		return &Result{}, "SGraph compatibility failure: execute returned nil"
	}
	expectedErrors := tc.expectedErrors
	compareNative := true
	if tc.name == "Query/NonNullResolverError" {
		// 原生链路会在字段 goroutine 中 panic，SGraph 只能按 GraphQL 预期单独校验。
		expectedErrors = 1
		compareNative = false
	}
	if len(sgraphResult.Errors) != expectedErrors {
		return sgraphResult, fmt.Sprintf(
			"SGraph compatibility failure: got %d errors, want %d: %s",
			len(sgraphResult.Errors),
			expectedErrors,
			benchmarkSGraphResultSummary(sgraphResult),
		)
	}
	if expectedErrors == 0 && sgraphResult.Data == nil {
		return sgraphResult, "SGraph compatibility failure: successful execution returned nil data"
	}
	if tc.name == "Query/NonNullResolverError" && sgraphResult.Data != nil {
		return sgraphResult, "SGraph compatibility failure: root non-null error did not null the response data"
	}
	if !compareNative {
		return sgraphResult, ""
	}

	nativeResult := ExecuteGraphQLGo(params)
	if nativeResult == nil {
		return sgraphResult, "native comparison failure: execute returned nil"
	}
	equal, compareErr := benchmarkGraphQLResultsEqual(nativeResult, sgraphResult)
	if compareErr != nil {
		return sgraphResult, "result comparison failed: " + compareErr.Error()
	}
	if !equal {
		return sgraphResult, fmt.Sprintf(
			"SGraph compatibility failure: native=%s sgraph=%s",
			benchmarkSGraphResultSummary(nativeResult),
			benchmarkSGraphResultSummary(sgraphResult),
		)
	}
	return sgraphResult, ""
}

func benchmarkGraphQLResultsEqual(nativeResult *Result, sgraphResult *Result) (bool, error) {
	nativeJSON, err := json.Marshal(nativeResult)
	if err != nil {
		return false, fmt.Errorf("marshal native result: %w", err)
	}
	sgraphJSON, err := json.Marshal(sgraphResult)
	if err != nil {
		return false, fmt.Errorf("marshal SGraph result: %w", err)
	}
	var nativeValue any
	if err := json.Unmarshal(nativeJSON, &nativeValue); err != nil {
		return false, fmt.Errorf("decode native result: %w", err)
	}
	var sgraphValue any
	if err := json.Unmarshal(sgraphJSON, &sgraphValue); err != nil {
		return false, fmt.Errorf("decode SGraph result: %w", err)
	}
	return reflect.DeepEqual(nativeValue, sgraphValue), nil
}

func benchmarkSGraphResultSummary(result *Result) string {
	encoded, err := json.Marshal(result)
	if err != nil {
		return fmt.Sprintf("<marshal error: %v>", err)
	}
	const maxSummaryBytes = 512
	if len(encoded) <= maxSummaryBytes {
		return string(encoded)
	}
	return string(encoded[:maxSummaryBytes]) + "..."
}

func benchmarkSGraphContext(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

func newBenchmarkSGraphEngine(tb testing.TB, schema *Schema) *SGraphEngine {
	tb.Helper()
	engine, err := NewSGraphEngine(schema, nil, nil)
	if err != nil {
		tb.Fatalf("create benchmark SGraph engine failed: %v", err)
	}
	if err := RegisterSGraphEngine(engine); err != nil {
		tb.Fatalf("register benchmark SGraph engine failed: %v", err)
	}
	return engine
}
