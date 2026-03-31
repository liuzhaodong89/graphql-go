package plan

import "context"

type Node interface {
	SetId(id uint32)
	GetId() uint32
	AddDependencies(dependencies []*Node)
	GetDependencies() []*Node
	Execute(rundata *RunData, ctx *context.Context) (interface{}, error)
	GetResult(rundata *RunData, ctx *context.Context) (interface{}, error)
}
