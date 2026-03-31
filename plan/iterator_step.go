package plan

import (
	"context"
	"errors"
	"sync"
)

type IteratorStep struct {
	id            uint32
	iteratorNode  *IteratorNode
	resoloverNode *ResolverNode
}

func (it *IteratorStep) GetId() uint32 {
	return it.id
}

func (it *IteratorStep) Execute(rundata *RunData, ctx *context.Context) (interface{}, error) {
	if it.iteratorNode == nil {
		return nil, errors.New("iterator node has no dependencies")
	}
	iterResult, iterErr := it.iteratorNode.Execute(rundata, ctx)
	if iterErr != nil {
		rundata.SetNodeErrorByNodeId(it.iteratorNode.GetId(), iterErr)
		return nil, iterErr
	}
	if iterResult != nil {
		depParamValMap := iterResult.(map[uint32]interface{})

		valsArr := make([]interface{}, 0)
		for _, nodeValsArr := range depParamValMap {
			valsArr = append(valsArr, nodeValsArr)
		}
		rundata.SetNodeResultByNodeId(it.iteratorNode.GetId(), valsArr)

		if len(valsArr) > 0 {
			var n Node = it.iteratorNode
			eachNodeArr := make([]*EachNode, 0)
			for index, _ := range valsArr {
				eachNode := EachNode{
					id:            it.id,
					dependencies:  []*Node{&n},
					resoloverFunc: it.resoloverNode.resoloverFunc,
					index:         int32(index),
				}
				eachNodeArr = append(eachNodeArr, &eachNode)
			}

			waitGroup := &sync.WaitGroup{}
			waitGroup.Add(len(eachNodeArr))

			var eachNodeResultMap sync.Map

			for _, en := range eachNodeArr {
				go func() {
					re, enErr := en.GetResult(rundata, ctx)
					if enErr != nil {
						rundata.SetNodeErrorByNodeId(it.iteratorNode.GetId(), enErr)
					}
					eachNodeResultMap.Store(en.index, re)
					waitGroup.Done()
				}()
			}
			waitGroup.Wait()

			iterNodeResultMap := make(map[interface{}]interface{}, 0)
			for index, val := range valsArr {
				res, ok := eachNodeResultMap.Load(index)
				if ok {
					iterNodeResultMap[val] = res
				}
			}
			rundata.SetNodeResultByNodeId(it.iteratorNode.GetId(), iterNodeResultMap)

			//TODO 组装结果
			return iterNodeResultMap, nil
		}
	}

	return nil, nil
}
