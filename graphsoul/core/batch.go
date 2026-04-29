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
			go func() {
				fieldErr := step.Execute(rundata, ctx)
				if fieldErr != nil && fieldErr.fieldType == FIELD_ERROR_TYPE_TREE {
					semaphore.Add(1)
				}
				wg.Done()
			}()
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
			if fieldErr != nil && fieldErr.fieldType == FIELD_ERROR_TYPE_TREE {
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
	if plan != nil {
		//第一个batch加入到batch切片里
		firstBatch := &Batch{
			batchId:    0,
			steps:      make([]Step, 0),
			concurrent: true,
		}
		batches = append(batches, firstBatch)

		//根节点默认是普通调用
		roots := plan.GetRoots()
		for _, root := range roots {
			step := &NormalStep{
				fieldPlan: root,
			}
			firstBatch.steps = append(firstBatch.steps, step)

			//增加根节点的参数依赖关系到map里，方便后续子节点查找
			for _, paramPlan := range root.GetParamPlans() {
				paramDepFieldIdBatchIdMap[paramPlan.GetDependentFieldId()] = firstBatch
			}

			//针对每个根节点递归遍历，为子节点创建step和对应的batch
			children := root.GetChildrenFields()
			for _, child := range children {
				batches, paramDepFieldIdBatchIdMap = appendBatches(child, root, batches, paramDepFieldIdBatchIdMap)
			}
		}
	}
	return batches
}

func appendBatches(fp *build.FieldPlan, parentFP *build.FieldPlan, batches []*Batch, paramDepFieldIdBatchIdMap map[uint32]*Batch) ([]*Batch, map[uint32]*Batch) {
	if fp == nil {
		return batches, paramDepFieldIdBatchIdMap
	}
	//TODO 如果父节点非Array，当前节点有Resolver，则当前节点为Normal
	var step Step
	if !parentFP.GetFieldIsList() {
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
	if fp.GetArrParamPlan() != nil {
		argsPlans = append(argsPlans, fp.GetArrParamPlan())
	}

	//TODO 根据参数查找最下层的batch，如果有参数没有找到batch则新建batch并加入到最下层
	var latestBatch *Batch
	var latestBatchId uint32 = 0
	var newBatch *Batch
	for _, argsPlan := range argsPlans {
		// CONST 和 INPUT 不依赖任何 field 结果，不参与 batch 调度
		if argsPlan.GetParamType() != build.PARAM_TYPE_FIELD_RESULT {
			continue
		}
		depFieldId := argsPlan.GetDependentFieldId()
		b := paramDepFieldIdBatchIdMap[depFieldId]
		if b != nil {
			if b.GetBatchId() > latestBatchId {
				latestBatch = b
				latestBatchId = b.GetBatchId()
			}
		} else {
			//如果出现新建batch的场景，中断循环优先按照新建batch推进
			newBatch = &Batch{
				batchId: uint32(len(batches)),
				steps:   make([]Step, 0),
			}
			if step != nil {
				newBatch.steps = append(newBatch.steps, step)
			}
			batches = append(batches, newBatch)
			paramDepFieldIdBatchIdMap[depFieldId] = newBatch
			break
		}
	}
	if newBatch == nil && latestBatch != nil {
		if step != nil {
			latestBatch.steps = append(latestBatch.steps, step)
		}
	} else if newBatch == nil && latestBatch == nil && step != nil {
		//无参数依赖的节点，直接加入到第一个batch里
		batches[0].steps = append(batches[0].steps, step)
	}

	// 递归处理子节点
	for _, child := range fp.GetChildrenFields() {
		batches, paramDepFieldIdBatchIdMap = appendBatches(child, fp, batches, paramDepFieldIdBatchIdMap)
	}

	return batches, paramDepFieldIdBatchIdMap
}
