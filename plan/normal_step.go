package plan

import (
	"context"
	"errors"
)

type NormalStep struct {
	id              uint32
	paramAccessNode *ParamAccessNode
	resolverNode    *ResolverNode
}

func (ns *NormalStep) GetId() uint32 {
	return ns.id
}

func (ns *NormalStep) SetId(id uint32) {
	ns.id = id
}

func (ns *NormalStep) SetParamAccessNode(node *ParamAccessNode) {
	ns.paramAccessNode = node
}

func (ns *NormalStep) SetResolverNode(node *ResolverNode) {
	ns.resolverNode = node
}

func (ns *NormalStep) Execute(rundata *RunData, ctx *context.Context) (interface{}, error) {
	//执行paramAccessNode
	if ns.paramAccessNode == nil {
		return nil, errors.New("param access node is nil")
	}
	paramAccessResult, paErr := ns.paramAccessNode.Execute(rundata, ctx)
	if paErr != nil {
		rundata.SetNodeErrorByNodeId(ns.paramAccessNode.GetId(), paErr)
		return nil, paErr
	}
	setResErr := rundata.SetNodeResultByNodeId(ns.paramAccessNode.GetId(), paramAccessResult)
	if setResErr != nil {
		return nil, setResErr
	}
	//执行resolverNode
	resoloverResult, resolverErr := ns.resolverNode.GetResult(rundata, ctx)
	if resolverErr != nil {
		rundata.SetNodeErrorByNodeId(ns.paramAccessNode.GetId(), resolverErr)
		return nil, resolverErr
	}
	setResoloverResErr := rundata.SetNodeResultByNodeId(ns.resolverNode.GetId(), resoloverResult)
	if setResoloverResErr != nil {
		return nil, setResoloverResErr
	}

	//TODO组装结果
	return resoloverResult, nil
}
