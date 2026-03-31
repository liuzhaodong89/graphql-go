package plan

import (
	"context"
	"errors"
)

type EachNode struct {
	id            uint32
	dependencies  []*Node
	index         int32
	resoloverFunc ResoloverFunc
}

func (e *EachNode) GetId() uint32 {
	return e.id
}

func (e *EachNode) SetId(id uint32) {
	e.id = id
}

func (e *EachNode) SetIndex(index int32) {
	e.index = index
}

func (e *EachNode) GetDependencies() []*Node {
	return e.dependencies
}

func (e *EachNode) AddDependencies(dependencies []*Node) {
	if e.dependencies == nil {
		e.dependencies = dependencies
	} else {
		e.dependencies = append(e.dependencies, dependencies...)
	}
}

func (e *EachNode) Execute(rundata *RunData, ctx *context.Context) (interface{}, error) {
	if len(e.dependencies) == 0 {
		return nil, errors.New("no dependencies found")
	}
	defaultDep := e.dependencies[0]
	if defaultDep == nil {
		return nil, errors.New("no default dependency")
	}

	paramVal, paramErr := rundata.GetNodeResultByNodeId((*defaultDep).GetId())
	if paramErr != nil {
		return nil, paramErr
	}
	paramValArr := paramVal.([]interface{})
	if paramValArr != nil && len(paramValArr) >= int(e.index) {
		paramEle := paramValArr[e.index]
		if e.resoloverFunc == nil {
			return nil, errors.New("no resolover function")
		}
		return e.resoloverFunc(paramEle, ctx)
	} else {
		return nil, errors.New("no param val found")
	}
}

func (e *EachNode) GetResult(rundata *RunData, ctx *context.Context) (interface{}, error) {
	return e.Execute(rundata, ctx)
}
