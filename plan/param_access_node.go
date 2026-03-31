package plan

import (
	"context"
	"errors"
	"fmt"
)

const (
	PARAM_TYPE_INT    = 0
	PARAM_TYPE_FLOAT  = 1
	PARAM_TYPE_STRING = 2
	PARAM_TYPE_BOOL   = 3
)

type ParamAccessNode struct {
	id               uint32
	dependencies     []*Node
	sourceParamKeys  []string
	sourceParamTypes []int
	targetParamTypes []int
}

func (p *ParamAccessNode) GetId() uint32 {
	return p.id
}

func (p *ParamAccessNode) SetId(id uint32) {
	p.id = id
}

func (p *ParamAccessNode) GetDependencies() []*Node {
	return p.dependencies
}

func (p *ParamAccessNode) AddDependencies(dependencies []*Node) {
	if p.dependencies == nil {
		p.dependencies = make([]*Node, 0)
	}
	p.dependencies = append(p.dependencies, dependencies...)
}

func (p *ParamAccessNode) AddSourceParamKeys(sourceParamKeys []string) {
	if p.sourceParamKeys == nil {
		p.sourceParamKeys = make([]string, 0)
	}
	p.sourceParamKeys = append(p.sourceParamKeys, sourceParamKeys...)
}

func (p *ParamAccessNode) AddTargetParamTypes(targetParamTypes []int) {
	if p.targetParamTypes == nil {
		p.targetParamTypes = make([]int, 0)
	}
	p.targetParamTypes = append(p.targetParamTypes, targetParamTypes...)
}

func (p *ParamAccessNode) Execute(rundata *RunData, ctx *context.Context) (interface{}, error) {
	//TODO 检查当前是否有需要获取的数据key，如果没有直接跳过
	if p.sourceParamKeys == nil {
		return nil, nil
	}
	//TODO 如果有要获取的数据key，遍历搜索rundata中的originialParams是否有对应的数据，如果有且组装完成，直接返回
	blankParamKeys := p.sourceParamKeys
	paramValuesArrWithType := make([]interface{}, len(p.sourceParamKeys))
	for index, pk := range blankParamKeys {
		originParamVal, oriErr := rundata.GetOriginalParamValueByName(pk)
		if oriErr != nil {
			return nil, oriErr
		}

		if originParamVal != nil {
			targetType := p.targetParamTypes[index]
			var paramValueWithType interface{}
			switch targetType {
			case PARAM_TYPE_INT:
				paramValueWithType = originParamVal.(int)
			case PARAM_TYPE_FLOAT:
				paramValueWithType = originParamVal.(float64)
			case PARAM_TYPE_STRING:
				paramValueWithType = originParamVal.(string)
			case PARAM_TYPE_BOOL:
				paramValueWithType = originParamVal.(bool)
			default:
				return nil, fmt.Errorf("unknown type of param value: %d", targetType)
			}
			paramValuesArrWithType[index] = paramValueWithType
		} else {
			//TODO 如果参数有欠缺，针对nodeResult再进行一遍搜索，搜索前检查dependencies是否为空
			if p.dependencies == nil {
				return nil, fmt.Errorf("dependencies is nil for node: %v", p.GetId())
			}
			for _, dependency := range p.dependencies {
				depRes, depResErr := rundata.GetNodeResultByNodeId((*dependency).GetId())
				if depResErr != nil {
					return nil, depResErr
				}
				if depRes != nil {
					depResMap := depRes.(map[string]interface{})
					paramVal := depResMap[p.sourceParamKeys[index]]
					if paramVal != nil {
						targetType := p.targetParamTypes[index]
						var paramValueWithType interface{}
						switch targetType {
						case PARAM_TYPE_INT:
							paramValueWithType = paramVal.(int)
						case PARAM_TYPE_FLOAT:
							paramValueWithType = paramVal.(float64)
						case PARAM_TYPE_STRING:
							paramValueWithType = paramVal.(string)
						case PARAM_TYPE_BOOL:
							paramValueWithType = paramVal.(bool)
						default:
							return nil, fmt.Errorf("unknown type for param value: %v", p.sourceParamKeys[index])
						}
						paramValuesArrWithType[index] = paramValueWithType
						break
					}
				}
			}
		}
	}
	//TODO 检查是否都组装完成，有空的返回失败
	if len(paramValuesArrWithType) != len(p.sourceParamKeys) {
		return nil, errors.New("param values length mismatch")
	} else {
		return paramValuesArrWithType, nil
	}

	////从父节点的结果中根据表达式获取数据，并进行必要的格式转换
	//if p.dependencies == nil {
	//	return nil, errors.New("no dependencies")
	//}
	//
	//paramValWithTypeArr := make([]interface{}, 0)
	//
	//for index, dependency := range p.dependencies {
	//	depRes, depErr := rundata.GetNodeResultByNodeId((*dependency).GetId())
	//	if depErr != nil {
	//		return nil, depErr
	//	}
	//	depResMap := depRes.(map[string]interface{})
	//	paramVal := depResMap[p.sourceParamKeys[index]]
	//	var paramValWithType interface{}
	//	switch p.sourceParamTypes[index] {
	//	case TYPE_INT:
	//		paramValWithType = paramVal.(int64)
	//	case TYPE_FLOAT:
	//		paramValWithType = paramVal.(float64)
	//	case TYPE_STRING:
	//		paramValWithType = paramVal.(string)
	//	case TYPE_BOOL:
	//		paramValWithType = paramVal.(bool)
	//	default:
	//		return nil, errors.New("invalid param type")
	//	}
	//	paramValWithTypeArr[index] = paramValWithType
	//}
	//return paramValWithTypeArr, nil
}

func (p *ParamAccessNode) GetResult(rundata *RunData, ctx *context.Context) (interface{}, error) {
	return p.Execute(rundata, ctx)
}
