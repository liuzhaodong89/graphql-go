package plan

import "context"

type Step interface {
	GetId() uint32
	SetId(uint32)
	SetParamAccessNode(*ParamAccessNode)
	SetResolverNode(*ResolverNode)
	Execute(rundata *RunData, ctx *context.Context) (interface{}, error)
}
