package core

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/graphql-go/graphql/graphsoul/build"
)

type BatchResult struct {
	interrupt bool
}

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
	if b.concurrent && len(b.steps) > 1 {
		//并发执行step
		semaphore := atomic.Uint32{}
		wg := sync.WaitGroup{}
		wg.Add(len(b.steps))

		for _, step := range b.steps {
			go func(step Step) {
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
			return &BatchResult{interrupt: true}
		}
	} else {
		//串行执行step
		for _, step := range b.steps {
			fieldErr := step.Execute(rundata, ctx)
			if fieldErr != nil && fieldErr.errorType == FieldErrorTypeTree {
				//判断是否有需要中断整个流程的错误
				return &BatchResult{interrupt: true}
			}
		}
	}
	return &BatchResult{interrupt: false}
}

func BuildBatches(plan *build.SGraphPlan) []*Batch {
	batches := make([]*Batch, 0)
	//TODO step的类型不是按照当前field类型来的，而是按照执行场景来的。如果step的入参是切片或数组，返回值也是切片或数组，即循环遍历的场景就是IteratorStep，反之就是NormalStep
	paramDepFieldIdBatchIdMap := make(map[uint32]*Batch)
	producedAt := map[uint32]uint32{}
	if plan != nil {
		//第一个batch加入到batch切片里
		concurrent := false
		if plan.GetOperationType() == build.SGraphOperationTypeQuery {
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
			step := &NormalStep{
				fieldPlan: root,
			}
			firstBatch.steps = append(firstBatch.steps, step)

			producedAt[root.GetFieldId()] = 0

			//增加根节点的参数依赖关系到map里，方便后续子节点查找
			for _, paramPlan := range root.GetParamPlans() {
				paramDepFieldIdBatchIdMap[paramPlan.GetDependentFieldId()] = firstBatch
			}

			//针对每个根节点递归遍历，为子节点创建step和对应的batch
			children := root.GetChildrenFields()
			for _, child := range children {
				batches, producedAt = appendBatches(child, root, 0, batches, producedAt, plan.GetOperationType())
			}
		}
	}
	return batches
}

func appendBatches(fp *build.FieldPlan, parentFP *build.FieldPlan, parentResultBatchId uint32, batches []*Batch, producedAt map[uint32]uint32, op build.SGraphPlanOperationType) ([]*Batch, map[uint32]uint32) {
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

	argsPlans := make([]*build.ParamPlan, 0)
	argsPlans = append(argsPlans, fp.GetParamPlans()...)
	if fp.GetArrParamPlans() != nil {
		argsPlans = append(argsPlans, fp.GetArrParamPlans()...)
	}
	childParentResultBatchId := parentResultBatchId

	//TODO 根据参数查找最下层的batch，如果有参数没有找到batch则新建batch并加入到最下层
	var latestBatchId uint32 = 0
	for _, argsPlan := range argsPlans {
		// CONST 和 INPUT 不依赖任何 field 结果，不参与 batch 调度
		if argsPlan.GetParamType() != build.ParamTypeFieldResult && argsPlan.GetParamType() != build.ParamTypeFieldFullResult {
			continue
		}
		if at, ok := producedAt[argsPlan.GetDependentFieldId()]; ok {
			if parentResultBatchId >= at {
				latestBatchId = parentResultBatchId
			} else {
				latestBatchId = at
			}
		}
	}
	target := latestBatchId + 1
	batches = ensureBatch(batches, target, op)
	batches[target].steps = append(batches[target].steps, step)
	producedAt[fp.GetFieldId()] = target
	childParentResultBatchId = target

	// 递归处理子节点
	for _, child := range fp.GetChildrenFields() {
		batches, producedAt = appendBatches(child, fp, childParentResultBatchId, batches, producedAt, op)
	}

	return batches, producedAt
}

func ensureBatch(batches []*Batch, targetId uint32, op build.SGraphPlanOperationType) []*Batch {
	for {
		if len(batches) <= int(targetId) {
			batches = append(batches, &Batch{batchId: uint32(len(batches)), concurrent: op == build.SGraphOperationTypeQuery})
		} else {
			break
		}
	}
	return batches
}
