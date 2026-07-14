package graphql

import (
	"context"
	"sync"
	"sync/atomic"
)

type BatchResult struct {
	interrupt bool
}

// 小 batch 直接串行执行，避免 goroutine 调度成本超过并发收益。
const sGraphConcurrentStepMin = 8

var (
	// BatchResult 是只读结果对象，复用静态实例减少每个 batch 的小对象分配。
	batchResultContinue  = &BatchResult{interrupt: false}
	batchResultInterrupt = &BatchResult{interrupt: true}
)

func (br *BatchResult) IsInterrupt() bool {
	return br.interrupt
}

type Batch struct {
	//id越大代表顺序越靠后
	batchId    uint32
	concurrent bool
	steps      []Step
}

func (b *Batch) GetBatchId() uint32 {
	return b.batchId
}

func (b *Batch) Execute(rundata *Rundata, ctx context.Context) *BatchResult {
	if b.concurrent && len(b.steps) >= sGraphConcurrentStepMin {
		//并发执行step
		semaphore := atomic.Uint32{}
		wg := sync.WaitGroup{}
		wg.Add(len(b.steps))

		for _, step := range b.steps {
			go func(step Step) {
				// step 作为参数传入 goroutine，避免闭包捕获循环变量导致执行错 step。
				fieldErr := step.Execute(rundata, ctx)
				if fieldErr != nil && fieldErr.errorType == FieldErrorTypeTree {
					semaphore.Add(1)
				}
				wg.Done()
			}(step)
		}

		wg.Wait()
		//判断是否有需要中断整个流程的错误
		if semaphore.Load() >= 1 {
			return batchResultInterrupt
		}
	} else {
		//串行执行step
		for _, step := range b.steps {
			fieldErr := step.Execute(rundata, ctx)
			if fieldErr != nil && fieldErr.errorType == FieldErrorTypeTree {
				//判断是否有需要中断整个流程的错误
				return batchResultInterrupt
			}
		}
	}
	return batchResultContinue
}

func BuildBatches(plan *SGraphPlan) []*Batch {
	batches := make([]*Batch, 0)
	//TODO step的类型不是按照当前field类型来的，而是按照执行场景来的。如果step的入参是切片或数组，返回值也是切片或数组，即循环遍历的场景就是IteratorStep，反之就是NormalStep
	producedAt := map[uint32]uint32{}
	if plan != nil {
		//第一个batch加入到batch切片里
		concurrent := false
		if plan.GetOperationType() == SGraphOperationTypeQuery {
			concurrent = true
		}
		firstBatch := &Batch{
			batchId:    0,
			steps:      make([]Step, 0),
			concurrent: concurrent,
		}
		batches = append(batches, firstBatch)

		//根节点默认是普通调用
		roots := plan.GetRoots()
		for _, root := range roots {
			if root.GetResolverFunc() != nil {
				step := &NormalStep{
					fieldPlan: root,
				}
				firstBatch.steps = append(firstBatch.steps, step)
				producedAt[root.GetFieldId()] = 0
			}

			//针对每个根节点递归遍历，为子节点创建step和对应的batch
			children := root.GetChildrenFields()
			for _, child := range children {
				batches, producedAt = appendBatches(child, root, batches, producedAt, plan.GetOperationType())
			}
		}
	}
	return batches
}

func appendBatches(fp *FieldPlan, parentFP *FieldPlan, batches []*Batch, producedAt map[uint32]uint32, op SGraphPlanOperationType) ([]*Batch, map[uint32]uint32) {
	if fp == nil {
		return batches, producedAt
	}
	//TODO 如果父节点非Array，当前节点有Resolver，则当前节点为Normal
	var step Step
	parentValueMetaInfo := parentFP.GetFieldValueMetaInfo()
	if !parentValueMetaInfo.IsList {
		if fp.GetResolverFunc() != nil {
			step = &NormalStep{
				fieldPlan: fp,
			}
		}
	} else {
		//TODO 如果父节点为Array，当前节点有Resolver，则当前节点为Iterator
		if fp.GetResolverFunc() != nil || fp.GetArrayResolverFunc() != nil {
			step = &IteratorStep{
				fieldPlan: fp,
			}
		}
	}

	argsPlans := make([]*ParamPlan, 0, len(fp.GetParamPlans())+len(fp.GetArrParamPlans())+len(fp.GetDirectiveParamPlans()))
	argsPlans = append(argsPlans, fp.GetParamPlans()...)
	argsPlans = append(argsPlans, fp.GetArrParamPlans()...)
	argsPlans = append(argsPlans, fp.GetDirectiveParamPlans()...)

	// step 所在 batch 只由显式字段结果参数依赖决定；父子关系本身不参与调度分层。
	target := uint32(0)
	for _, argsPlan := range argsPlans {
		if argsPlan == nil {
			continue
		}
		// CONST 和 INPUT 不依赖任何 field 结果，不参与 batch 调度
		if argsPlan.GetParamType() != ParamTypeFieldResult && argsPlan.GetParamType() != ParamTypeFieldFullResult {
			continue
		}
		if at, ok := producedAt[argsPlan.GetDependentFieldId()]; ok {
			next := at + 1
			if next > target {
				target = next
			}
		}
	}
	if step != nil {
		// 只有真实 resolver 才进入 batch；无 resolver 字段在组装阶段从父响应读取。
		batches = ensureBatch(batches, target, op)
		batches[target].steps = append(batches[target].steps, step)
		producedAt[fp.GetFieldId()] = target
	}

	// 递归处理子节点
	for _, child := range fp.GetChildrenFields() {
		batches, producedAt = appendBatches(child, fp, batches, producedAt, op)
	}

	return batches, producedAt
}

func ensureBatch(batches []*Batch, targetId uint32, op SGraphPlanOperationType) []*Batch {
	for {
		if len(batches) <= int(targetId) {
			batches = append(batches, &Batch{batchId: uint32(len(batches)), concurrent: op == SGraphOperationTypeQuery})
		} else {
			break
		}
	}
	return batches
}
