package graphsoul_test

// engine_bench_test.go: graphsoul 引擎性能压测
//
// 测试场景：
//   B1. 单个 Scalar 字段（基准）
//   B2. 10 个并发 root 字段（宽查询）
//   B3. 三层嵌套 Object（深查询）
//   B4. List-批量模式 / 100 条数据
//   B5. List-遍历模式 / 100 条数据
//   B6. 跨 Batch 参数依赖链（两跳）
//   B7. NewRundata 单独分配成本
//   B8. 单 Scalar 并发（b.RunParallel）
//   B9. 宽查询并发（b.RunParallel）
//   B10. 带 INPUT 参数的单字段

import (
	"context"
	"fmt"
	"strconv"
	"testing"

	"github.com/graphql-go/graphql/graphsoul/build"
	"github.com/graphql-go/graphql/graphsoul/core"
)

// ─────────────────────────────────────────────
// 共用 plan 构造函数
// ─────────────────────────────────────────────

func makeSingleScalarPlan() *build.SGraphPlan {
	f := build.NewFieldPlan(build.FieldPlanOptions{
		FieldId:      1,
		ResponseName: "hello",
		FieldType:    build.FIELD_TYPE_SCALAR,
		Paths:        []string{"hello"},
		ResolverFunc: func(source any, params map[string]any, ctx context.Context) (any, error) {
			return "world", nil
		},
	})
	return build.NewSGraphPlan([]*build.FieldPlan{f}, nil)
}

func makeWidePlan(n int) *build.SGraphPlan {
	fields := make([]*build.FieldPlan, n)
	for i := 0; i < n; i++ {
		name := "field" + strconv.Itoa(i)
		idx := uint32(i + 1)
		fields[i] = build.NewFieldPlan(build.FieldPlanOptions{
			FieldId:      idx,
			ResponseName: name,
			FieldType:    build.FIELD_TYPE_SCALAR,
			Paths:        []string{name},
			ResolverFunc: func(source any, params map[string]any, ctx context.Context) (any, error) {
				return "val", nil
			},
		})
	}
	return build.NewSGraphPlan(fields, nil)
}

// 三层嵌套：root(OBJECT) → child(OBJECT) → leaf(SCALAR)
func makeDeepNestedPlan() *build.SGraphPlan {
	leaf := build.NewFieldPlan(build.FieldPlanOptions{
		FieldId:       3,
		ParentFieldId: 2,
		ResponseName:  "city",
		FieldType:     build.FIELD_TYPE_SCALAR,
		Paths:         []string{"user", "address", "city"},
	})
	address := build.NewFieldPlan(build.FieldPlanOptions{
		FieldId:        2,
		ParentFieldId:  1,
		ResponseName:   "address",
		FieldType:      build.FIELD_TYPE_OBJECT,
		Paths:          []string{"user", "address"},
		ChildrenFields: []*build.FieldPlan{leaf},
		ResolverFunc: func(source any, params map[string]any, ctx context.Context) (any, error) {
			return map[string]any{"city": "Beijing"}, nil
		},
	})
	user := build.NewFieldPlan(build.FieldPlanOptions{
		FieldId:        1,
		ResponseName:   "user",
		FieldType:      build.FIELD_TYPE_OBJECT,
		Paths:          []string{"user"},
		ChildrenFields: []*build.FieldPlan{address},
		ResolverFunc: func(source any, params map[string]any, ctx context.Context) (any, error) {
			return map[string]any{"id": "u1"}, nil
		},
	})
	return build.NewSGraphPlan([]*build.FieldPlan{user}, nil)
}

// List-批量模式：root 返回 n 个父对象，child 通过 ArrayResolverFunc 一次性批量返回 n 个结果
func makeListBatchPlan(n int) *build.SGraphPlan {
	// 预建父节点列表
	parentItems := make([]any, n)
	for i := 0; i < n; i++ {
		parentItems[i] = map[string]any{"id": strconv.Itoa(i)}
	}
	// 预建批量结果（每个父对象对应一个 score 对象，含 id 用于父子关联）
	batchResults := make([]any, n)
	for i := 0; i < n; i++ {
		batchResults[i] = map[string]any{"id": strconv.Itoa(i), "score": i * 10}
	}

	scoreField := build.NewFieldPlan(build.FieldPlanOptions{
		FieldId:                  2,
		ParentFieldId:            1,
		ResponseName:             "score",
		FieldType:                build.FIELD_TYPE_SCALAR,
		Paths:                    []string{"items", "score"},
		ParentKeyFieldNames:      []string{"id"},
		ArrayResultParentKeyName: "id",
		// ArrayResolverFunc 触发 IteratorStep 批量模式
		ArrayResolverFunc: func(source any, params map[string]any, ctx context.Context) (any, error) {
			return batchResults, nil
		},
	})

	root := build.NewFieldPlan(build.FieldPlanOptions{
		FieldId:        1,
		ResponseName:   "items",
		FieldType:      build.FIELD_TYPE_OBJECT,
		FieldIsList:    true,
		Paths:          []string{"items"},
		ChildrenFields: []*build.FieldPlan{scoreField},
		ResolverFunc: func(source any, params map[string]any, ctx context.Context) (any, error) {
			return parentItems, nil
		},
	})
	return build.NewSGraphPlan([]*build.FieldPlan{root}, nil)
}

// List-遍历模式：root 返回 n 个父对象，child 对每个逐一 resolve
func makeListTraversePlan(n int) *build.SGraphPlan {
	parentItems := make([]any, n)
	for i := 0; i < n; i++ {
		parentItems[i] = map[string]any{"id": strconv.Itoa(i)}
	}

	// score 子字段：ArrayResolverFunc = nil，使用 ResolverFunc 逐条遍历
	scoreField := build.NewFieldPlan(build.FieldPlanOptions{
		FieldId:             2,
		ParentFieldId:       1,
		ResponseName:        "score",
		FieldType:           build.FIELD_TYPE_SCALAR,
		Paths:               []string{"items", "score"},
		ParentKeyFieldNames: []string{"id"},
		ResolverFunc: func(source any, params map[string]any, ctx context.Context) (any, error) {
			return 42, nil
		},
	})

	root := build.NewFieldPlan(build.FieldPlanOptions{
		FieldId:        1,
		ResponseName:   "items",
		FieldType:      build.FIELD_TYPE_OBJECT,
		FieldIsList:    true,
		Paths:          []string{"items"},
		ChildrenFields: []*build.FieldPlan{scoreField},
		ResolverFunc: func(source any, params map[string]any, ctx context.Context) (any, error) {
			return parentItems, nil
		},
	})
	return build.NewSGraphPlan([]*build.FieldPlan{root}, nil)
}

// 跨 Batch 依赖链：account(batch0, OBJECT) → orderNo(batch1, SCALAR 通过 FIELD_RESULT 读 account.id)
func makeFieldResultChainPlan() *build.SGraphPlan {
	// orderNo 是 account 的子字段，通过 FIELD_RESULT param 依赖 account 的结果 → 进入 batch1
	orderNo := build.NewFieldPlan(build.FieldPlanOptions{
		FieldId:       2,
		ParentFieldId: 1,
		ResponseName:  "orderNo",
		FieldType:     build.FIELD_TYPE_SCALAR,
		Paths:         []string{"account", "orderNo"},
		ParamPlans: []*build.ParamPlan{
			build.NewFieldResultParamPlan("userId", 1, []string{"id"}),
		},
		ResolverFunc: func(source any, params map[string]any, ctx context.Context) (any, error) {
			return fmt.Sprintf("order_of_%v", params["userId"]), nil
		},
	})
	account := build.NewFieldPlan(build.FieldPlanOptions{
		FieldId:        1,
		ResponseName:   "account",
		FieldType:      build.FIELD_TYPE_OBJECT,
		Paths:          []string{"account"},
		ChildrenFields: []*build.FieldPlan{orderNo},
		ResolverFunc: func(source any, params map[string]any, ctx context.Context) (any, error) {
			return map[string]any{"id": "u42"}, nil
		},
	})
	return build.NewSGraphPlan([]*build.FieldPlan{account}, nil)
}

func makeInputParamPlan() *build.SGraphPlan {
	f := build.NewFieldPlan(build.FieldPlanOptions{
		FieldId:      1,
		ResponseName: "greeting",
		FieldType:    build.FIELD_TYPE_SCALAR,
		Paths:        []string{"greeting"},
		ParamPlans: []*build.ParamPlan{
			build.NewInputParamPlan("msg", "inputMsg"),
		},
		ResolverFunc: func(source any, params map[string]any, ctx context.Context) (any, error) {
			return "hello " + params["msg"].(string), nil
		},
	})
	return build.NewSGraphPlan([]*build.FieldPlan{f}, map[string]any{"inputMsg": "world"})
}

// ─────────────────────────────────────────────
// B1. 单 Scalar 字段（基准）
// ─────────────────────────────────────────────

func BenchmarkEngine_SingleScalar(b *testing.B) {
	plan := makeSingleScalarPlan()
	engine := &core.SGraphEngine{}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		engine.Execute(plan)
	}
}

// ─────────────────────────────────────────────
// B2. 10 个并发 root 字段（宽查询）
// ─────────────────────────────────────────────

func BenchmarkEngine_WideQuery_10Fields(b *testing.B) {
	plan := makeWidePlan(10)
	engine := &core.SGraphEngine{}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		engine.Execute(plan)
	}
}

// ─────────────────────────────────────────────
// B3. 三层嵌套 Object
// ─────────────────────────────────────────────

func BenchmarkEngine_DeepNested_3Level(b *testing.B) {
	plan := makeDeepNestedPlan()
	engine := &core.SGraphEngine{}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		engine.Execute(plan)
	}
}

// ─────────────────────────────────────────────
// B4. List-批量模式 / 10 / 100 / 500 条数据
// ─────────────────────────────────────────────

func BenchmarkEngine_ListBatch_10Items(b *testing.B) {
	plan := makeListBatchPlan(10)
	engine := &core.SGraphEngine{}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		engine.Execute(plan)
	}
}

func BenchmarkEngine_ListBatch_100Items(b *testing.B) {
	plan := makeListBatchPlan(100)
	engine := &core.SGraphEngine{}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		engine.Execute(plan)
	}
}

func BenchmarkEngine_ListBatch_500Items(b *testing.B) {
	plan := makeListBatchPlan(500)
	engine := &core.SGraphEngine{}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		engine.Execute(plan)
	}
}

// ─────────────────────────────────────────────
// B5. List-遍历模式 / 10 / 100 条数据
// ─────────────────────────────────────────────

func BenchmarkEngine_ListTraverse_10Items(b *testing.B) {
	plan := makeListTraversePlan(10)
	engine := &core.SGraphEngine{}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		engine.Execute(plan)
	}
}

func BenchmarkEngine_ListTraverse_100Items(b *testing.B) {
	plan := makeListTraversePlan(100)
	engine := &core.SGraphEngine{}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		engine.Execute(plan)
	}
}

// ─────────────────────────────────────────────
// B6. 跨 Batch 参数依赖链（两跳）
// ─────────────────────────────────────────────

func BenchmarkEngine_FieldResultChain(b *testing.B) {
	plan := makeFieldResultChainPlan()
	engine := &core.SGraphEngine{}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		engine.Execute(plan)
	}
}

// ─────────────────────────────────────────────
// B7. NewRundata 单独分配成本（隔离 lmap 初始化开销）
// ─────────────────────────────────────────────

func BenchmarkNewRundata(b *testing.B) {
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = core.NewRundata(nil, 10)
	}
}

// ─────────────────────────────────────────────
// B8. 单 Scalar 并发压测
// ─────────────────────────────────────────────

func BenchmarkEngine_SingleScalar_Parallel(b *testing.B) {
	plan := makeSingleScalarPlan()
	engine := &core.SGraphEngine{}
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			engine.Execute(plan)
		}
	})
}

// ─────────────────────────────────────────────
// B9. 宽查询并发压测
// ─────────────────────────────────────────────

func BenchmarkEngine_WideQuery_10Fields_Parallel(b *testing.B) {
	plan := makeWidePlan(10)
	engine := &core.SGraphEngine{}
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			engine.Execute(plan)
		}
	})
}

// ─────────────────────────────────────────────
// B10. 带 INPUT 参数的单字段
// ─────────────────────────────────────────────

func BenchmarkEngine_SingleScalar_InputParam(b *testing.B) {
	plan := makeInputParamPlan()
	engine := &core.SGraphEngine{}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		engine.Execute(plan)
	}
}
