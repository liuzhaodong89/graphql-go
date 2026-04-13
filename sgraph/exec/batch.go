package exec

import (
	"context"
	"sync"

	"github.com/graphql-go/graphql/sgraph/plan"
)

// Batch 表示可以并发执行的一组 Step（同批次的 Step 之间没有数据依赖）。
//
// 批次分层规则（由 buildBatches 按拓扑分层产生）：
//   - 根字段（SourceSlot == NoSlot）放在第 0 批；
//   - 子字段放在其父字段所在批次 + 1；
//   - 同一批次的 Step 对 query 操作可并发执行；
//   - mutation 操作的 Step 须串行执行。
type Batch struct {
	// Steps 是本批次需要执行的 Step 列表
	Steps []Step

	// Concurrent 为 true 时以并发方式执行批内所有 Step，否则串行
	Concurrent bool
}

// Execute 执行本批次所有 Step，等待全部完成后返回。
func (b *Batch) Execute(frame *RunFrame, ctx context.Context) {
	if !b.Concurrent || len(b.Steps) == 1 {
		// 串行执行：mutation 操作或单 step 批次
		for _, s := range b.Steps {
			if err := s.Execute(frame, ctx); err != nil {
				frame.AddError(s.FieldPlan().Path, err)
			}
		}
		return
	}

	// 并发执行：使用 WaitGroup 等待所有 goroutine 完成
	var wg sync.WaitGroup
	for _, s := range b.Steps {
		wg.Add(1)
		s := s // 捕获循环变量（Go 1.22 之前需要）
		go func() {
			defer wg.Done()
			if err := s.Execute(frame, ctx); err != nil {
				frame.AddError(s.FieldPlan().Path, err)
			}
		}()
	}
	wg.Wait()
}

// buildBatches 根据 SelectionPlan 中的字段依赖关系，将根字段的 Step 分层成 Batch 列表。
//
// 算法：拓扑排序按层分批
//   - 根字段层次 = 0；
//   - 子字段层次 = 父字段层次 + 1；
//   - 同层字段放入同一 Batch（对象子字段由 NormalStep 内部处理，不展开到 batch）。
//
// concurrent 由 operation 类型决定（query=true，mutation=false）。
func buildBatches(sel *plan.SelectionPlan) []Batch {
	concurrent := sel.OperationType == plan.OperationQuery

	// slotLayer 记录每个 slot 所在的批次层（索引即 SlotID）
	slotLayer := make([]int, sel.TotalSlots)

	// layerSteps[i] = 第 i 层的所有 Step
	layerSteps := make([][]Step, 0)

	// assignLayer 递归计算字段的批次层并收集 Step（仅处理根字段和列表字段顶层）
	var assignLayer func(fp *plan.FieldPlan)
	assignLayer = func(fp *plan.FieldPlan) {
		// 当前字段批次层 = 父字段批次层 + 1（根字段为 0）
		layer := 0
		if fp.SourceSlot != plan.NoSlot {
			layer = slotLayer[int(fp.SourceSlot)] + 1
		}
		slotLayer[int(fp.ResultSlot)] = layer

		// 扩展 layerSteps 切片到当前层
		for len(layerSteps) <= layer {
			layerSteps = append(layerSteps, nil)
		}

		// 根据字段类型创建对应 Step
		var s Step
		if fp.TypeKind == plan.KindList {
			s = NewIteratorStep(fp)
		} else {
			s = NewNormalStep(fp)
		}
		layerSteps[layer] = append(layerSteps[layer], s)

		// 注意：KindObject 的子字段由 NormalStep 内部递归处理，
		// KindList 的子字段由 IteratorStep 内部处理，
		// 此处均不展开到 batch，避免 source slot 尚未写入时被错误调度。
	}

	for _, fp := range sel.RootFields {
		assignLayer(fp)
	}

	// 将各层 Step 转换为 Batch
	batches := make([]Batch, 0, len(layerSteps))
	for _, steps := range layerSteps {
		if len(steps) > 0 {
			batches = append(batches, Batch{
				Steps:      steps,
				Concurrent: concurrent,
			})
		}
	}
	return batches
}
