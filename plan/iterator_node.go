package plan

import (
	"context"
	"errors"
)

type IteratorNode struct {
	id           uint32
	dependencies []*Node
}

func (it *IteratorNode) GetId() uint32 {
	return it.id
}

func (it *IteratorNode) SetId(id uint32) {
	it.id = id
}

func (it *IteratorNode) GetDependencies() []*Node {
	return it.dependencies
}

func (it *IteratorNode) AddDependencies(dependencies []*Node) {
	if it.dependencies == nil {
		it.dependencies = dependencies
	} else {
		it.dependencies = append(it.dependencies, dependencies...)
	}
}

func (it *IteratorNode) Execute(rundata *RunData, ctx *context.Context) (interface{}, error) {
	if it.dependencies == nil {
		return nil, errors.New("iterator node has no dependencies")
	}
	depIdParamValMap := make(map[uint32]interface{})
	for _, dependency := range it.dependencies {
		depResult, depErr := rundata.GetNodeResultByNodeId((*dependency).GetId())
		if depErr != nil {
			return nil, depErr
		}
		depResultArr := depResult.([]interface{})
		depIdParamValMap[(*dependency).GetId()] = depResultArr
	}
	return depIdParamValMap, nil
}

func (it *IteratorNode) GetResult(rundata *RunData, ctx *context.Context) (interface{}, error) {
	return it.Execute(rundata, ctx)
}
