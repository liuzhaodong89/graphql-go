package exec

import (
	"context"

	"github.com/graphql-go/graphql/sgraph/plan"
	"github.com/graphql-go/graphql/sgraph/response"
)

// Executor 是执行层的入口，负责接收 SelectionPlan 并驱动完整执行流程。
//
// 执行流程：
//  1. 根据 SelectionPlan.TotalSlots 预分配 RunFrame；
//  2. 调用 buildBatches 将根字段按依赖分层为 Batch 列表；
//  3. 按层顺序执行 Batch（批内并发，批间严格串行）；
//  4. 调用 response.Build 从 RunFrame 组装最终 GraphQL 响应。
type Executor struct{}

// NewExecutor 创建一个新的 Executor 实例
func NewExecutor() *Executor {
	return &Executor{}
}

// Execute 执行一个 SelectionPlan，返回标准 GraphQL 响应结果。
//   - sel:       由 Planner.Build 生成的执行计划
//   - variables: 本次请求的 GraphQL 变量表（无变量可传 nil）
//   - ctx:       请求级 context（传递认证、取消等信息）
func (e *Executor) Execute(sel *plan.SelectionPlan, variables map[string]interface{}, ctx context.Context) *response.Result {
	// 1. 预分配执行帧
	frame := NewRunFrame(sel.TotalSlots, variables)

	// 2. 按依赖关系将根字段 Step 分层为 Batch 列表
	batches := buildBatches(sel)

	// 3. 按层顺序执行每个 Batch（每层必须等上一层全部完成）
	for _, batch := range batches {
		batch.Execute(frame, ctx)
	}

	// 4. 从 RunFrame 组装 GraphQL 响应（response 层）
	rawErrors := frame.Errors()
	errInfos := make([]response.FieldErrorInfo, len(rawErrors))
	for i, fe := range rawErrors {
		errInfos[i] = response.FieldErrorInfo{
			Path:    fe.Path,
			Message: fe.Message,
			Err:     fe.Err,
		}
	}
	return response.Build(sel, frame.Slots(), errInfos)
}
