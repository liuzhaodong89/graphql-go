package plan

import (
	"context"
	"errors"
)

type ResoloverFunc func(paramVals interface{}, ctx *context.Context) (interface{}, error)

type ResolverNode struct {
	id            uint32
	dependencies  []*Node
	resoloverFunc ResoloverFunc
}

func (r *ResolverNode) GetId() uint32 {
	return r.id
}

func (r *ResolverNode) SetId(id uint32) {
	r.id = id
}

func (r *ResolverNode) AddDependencies(nodes []*Node) {
	if r.dependencies == nil {
		r.dependencies = make([]*Node, 0)
	}
	r.dependencies = append(r.dependencies, nodes...)
}

func (r *ResolverNode) GetDependencies() []*Node {
	return r.dependencies
}

func (r *ResolverNode) GetResoloverFunc() ResoloverFunc {
	return r.resoloverFunc
}

func (r *ResolverNode) SetResoloverFunc(f ResoloverFunc) {
	r.resoloverFunc = f
}

func (r *ResolverNode) Execute(rundata *RunData, ctx *context.Context) (interface{}, error) {
	if len(r.dependencies) == 0 {
		return nil, errors.New("dependencies is empty")
	}
	var defaultDependency *Node = r.dependencies[0]
	if defaultDependency == nil {
		return nil, errors.New("param_access_node is empty")
	}
	paramAccessNode := (*defaultDependency).(*ParamAccessNode)
	paramValue, paramErr := rundata.GetNodeResultByNodeId(paramAccessNode.GetId())
	if paramErr != nil {
		return nil, paramErr
	}
	return r.resoloverFunc(paramValue, ctx)
}

func (r *ResolverNode) GetResult(rundata *RunData, ctx *context.Context) (interface{}, error) {
	return r.Execute(rundata, ctx)
}
