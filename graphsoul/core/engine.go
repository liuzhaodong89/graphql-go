package core

import (
	"context"
	"github.com/graphql-go/graphql/graphsoul/build"
)

type SGraphResult struct {
	response map[string]any
	errors   []*FieldError
}
type SGraphEngine struct{}

func (e *SGraphEngine) Execute(plan *build.SGraphPlan, originalParams map[string]any) *SGraphResult {
	//组装Rundata和context
	rundata := NewRundata(originalParams)
	ctx := context.TODO()
	//组装Batches
	if plan != nil {
		batches := e.buildBatches(plan)
		//遍历执行Batches，判断遇到中断则返回
		for _, batch := range batches {
			br := batch.Execute(rundata, ctx)
			if br.IsInterrupt() {
				break
			}
		}
	}
	//组装结果
	return e.assembleGraphResult(plan, rundata)
}

func (e *SGraphEngine) buildBatches(plan *build.SGraphPlan) []*Batch {
	return nil
}

func (e *SGraphEngine) assembleGraphResult(plan *build.SGraphPlan, rundata *Rundata) *SGraphResult {
	return nil
}
