package plan

import "context"

type RootNode struct {
	id uint32
}

func (rn *RootNode) GetId() uint32 {
	return rn.id
}

func (rn *RootNode) SetId(id uint32) {
	rn.id = id
}

func (rn *RootNode) AddDependencies(dependencies []*Node) {
	//nothing
}

func (rn *RootNode) GetDependencies() []*Node {
	return nil
}

func (rn *RootNode) Execute(rundata *RunData, ctx *context.Context) (interface{}, error) {
	if rundata != nil {
		return rundata.GetOriginalParams(), nil
	}
	return nil, nil
}

func (rn *RootNode) GetResult(rundata *RunData, ctx *context.Context) (interface{}, error) {
	return rn.Execute(rundata, ctx)
}
