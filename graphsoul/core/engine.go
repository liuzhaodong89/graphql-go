package core

import (
	"context"

	"github.com/graphql-go/graphql/graphsoul/build"
	Lmap "github.com/liuzhaodong89/lockfree-collection/map"
)

type SGraphResult struct {
	response map[string]any
	errors   []*FieldError
}

func (r *SGraphResult) GetResponse() map[string]any {
	return r.response
}

func (r *SGraphResult) GetErrors() []*FieldError {
	return r.errors
}

type SGraphEngine struct {
	batchCache *Lmap.Lmap[string, []*Batch]
}

func NewSGraphEngine() *SGraphEngine {
	return &SGraphEngine{
		batchCache: Lmap.New[string, []*Batch](),
	}
}

func (e *SGraphEngine) getBatchesFromCacheOrCreate(plan *build.SGraphPlan) []*Batch {
	if plan == nil {
		return nil
	}
	cacheKey := plan.GetCacheKey()
	if batches, ok := e.batchCache.Get(cacheKey); ok {
		return batches
	}

	batches := BuildBatches(plan)
	e.batchCache.Set(cacheKey, batches)
	return batches
}

func (e *SGraphEngine) Execute(plan *build.SGraphPlan) *SGraphResult {
	//组装Rundata和context
	var maxFieldId uint32
	if plan != nil {
		maxFieldId = plan.MaxFieldId()
	}
	rundata := NewRundata(plan.GetOriginalInputs(), maxFieldId)
	ctx := context.TODO()
	//组装Batches
	if plan != nil {
		batches := e.getBatchesFromCacheOrCreate(plan)
		//遍历执行Batches，判断遇到中断则返回
		for _, batch := range batches {
			br := batch.Execute(rundata, ctx)
			if br.IsInterrupt() {
				break
			}
		}
	}
	//组装结果
	result := e.assembleGraphResult(plan, rundata)

	//结果回收到缓存池
	for i := range rundata.fieldResultSlice {
		if fieldResponse := rundata.fieldResultSlice[i].Load(); fieldResponse != nil {
			ReleaseFieldResponse(fieldResponse)
		}
	}
	return result
}

func (e *SGraphEngine) assembleGraphResult(plan *build.SGraphPlan, rundata *Rundata) *SGraphResult {
	result := &SGraphResult{
		response: make(map[string]any),
		errors:   nil,
	}
	if plan == nil {
		return result
	}

	roots := plan.GetRoots()
	responseMap := make(map[string]any, len(roots))

	for _, root := range roots {
		if root.GetFieldIsList() {
			rootResult := e.buildListValues(root, rundata)
			//null值传递
			if root.GetFieldListNotNil() && rootResult == nil {
				responseMap = nil
				break
			} else {
				if rootResult == nil {
					responseMap[root.GetResponseName()] = nil
				} else {
					responseMap[root.GetResponseName()] = rootResult
				}
			}
		} else {
			if root.GetFieldType() == build.FIELD_TYPE_OBJECT {
				rootResult := e.buildObjectValue(root, rundata)
				//null值传递
				if rootResult == nil && root.GetFieldNotNil() {
					responseMap = nil
					break
				} else {
					if rootResult == nil {
						responseMap[root.GetResponseName()] = nil
					} else {
						responseMap[root.GetResponseName()] = rootResult
					}
				}
			} else if root.GetFieldType() == build.FIELD_TYPE_SCALAR || root.GetFieldType() == build.FIELD_TYPE_ENUM {
				rootResult := e.buildScalarOrEnumValue(root, nil, rundata)
				//null值传递
				if rootResult == nil && root.GetFieldNotNil() {
					responseMap = nil
					break
				} else {
					if rootResult == nil {
						responseMap[root.GetResponseName()] = nil
					} else {
						responseMap[root.GetResponseName()] = rootResult
					}
				}
			}
		}

	}
	result.response = responseMap
	result.errors = rundata.GetAllFieldErrors()
	return result

}

func (e *SGraphEngine) buildObjectValue(field *build.FieldPlan, rundata *Rundata) map[string]any {
	if field != nil {
		fieldResponse := rundata.GetFieldResultByFieldId(field.GetFieldId())
		children := field.GetChildrenFields()

		result := make(map[string]any, len(children))
		//如果当前字段的结果为空，不再遍历子字段
		if fieldResponse == nil || fieldResponse.responses == nil || (len(fieldResponse.responses) > 0 && fieldResponse.responses[0] == nil) {
			return nil
		}
		for _, child := range children {
			if child.GetFieldIsList() {
				//如果子字段是List类型且List为non-null但是出现nil，则判断当前字段是否为non-null，如果是则清空当前字段的数据返回nil
				childResult := e.buildListValues(child, rundata)
				//null值传递，如果子字段non-null但返回nil，当前字段直接返回nil
				if childResult == nil && child.GetFieldListNotNil() {
					return nil
				}
				if childResult == nil {
					result[child.GetResponseName()] = nil
				} else {
					result[child.GetResponseName()] = childResult
				}
			} else {
				switch child.GetFieldType() {
				case build.FIELD_TYPE_OBJECT:
					//null值传递
					childResult := e.buildObjectValue(child, rundata)
					if childResult == nil && child.GetFieldNotNil() {
						return nil
					}
					if childResult == nil {
						result[child.GetResponseName()] = nil
					} else {
						result[child.GetResponseName()] = childResult
					}
				case build.FIELD_TYPE_SCALAR, build.FIELD_TYPE_ENUM:
					if len(fieldResponse.responses) > 0 {
						if fieldResMap, ok := fieldResponse.responses[0].(map[string]any); ok {
							childResult := e.buildScalarOrEnumValue(child, fieldResMap, rundata)
							//null值传递
							if child.GetFieldNotNil() && childResult == nil {
								return nil
							}
							if childResult == nil {
								result[child.GetResponseName()] = nil
							} else {
								result[child.GetResponseName()] = childResult
							}
						} else {
							if fieldResponse.responses[0] == nil && child.GetFieldNotNil() {
								return nil
							}
						}
					}
				}
			}

		}
		return result
	}
	return nil
}

func (e *SGraphEngine) buildListValues(field *build.FieldPlan, rundata *Rundata) []any {
	if field != nil {
		var result []any
		fieldResponse := rundata.GetFieldResultByFieldId(field.GetFieldId())
		if fieldResponse == nil || fieldResponse.responses == nil {
			return nil
		}
		result = make([]any, 0, len(fieldResponse.responses))

		switch field.GetFieldType() {
		case build.FIELD_TYPE_OBJECT:
			for _, response := range fieldResponse.responses {
				//TODO 如果当前的array的字段类型是Object，需要循环创建对应的map并加入到切片中
				if resMap, ok := response.(map[string]any); ok {
					objectResult := e.buildObjectValueInList(field, resMap, rundata)
					if objectResult != nil {
						result = append(result, objectResult)
					} else {
						//如果当前List字段要求元素non-null但有元素是nil，则整个字段返回nil
						if field.GetFieldNotNil() {
							return nil
						}
					}
				}
			}
			return result
		case build.FIELD_TYPE_SCALAR, build.FIELD_TYPE_ENUM:
			//如果当前List字段要求元素non-null但有元素是nil，则整个字段返回nil
			if field.GetFieldNotNil() {
				for _, response := range fieldResponse.responses {
					if response == nil {
						return nil
					}
				}
			}

			result = append(result, fieldResponse.responses...)
			return result
		}
	}
	return nil
}

func (e *SGraphEngine) buildObjectValueInList(field *build.FieldPlan, currentFieldResponseMap map[string]any, rundata *Rundata) map[string]any {
	var result map[string]any
	//如果当前元素的response为空，则直接返回，不再遍历子字段
	if currentFieldResponseMap == nil {
		return result
	}
	//TODO 获取当前字段的子字段，遍历每个子字段的类型组装Map
	if field != nil {
		children := field.GetChildrenFields()
		result = make(map[string]any, len(children))
		for _, child := range children {
			if child.GetFieldIsList() {
				childResult := e.buildListValuesInListObject(child, currentFieldResponseMap, rundata)
				if childResult != nil {
					result[child.GetResponseName()] = childResult
				} else {
					//null值传递
					if field.GetFieldListNotNil() {
						return nil
					}
				}
			} else {
				switch child.GetFieldType() {
				case build.FIELD_TYPE_OBJECT:
					childResult := e.buildObjectValueInListObject(child, currentFieldResponseMap, rundata)
					if childResult != nil {
						result[child.GetResponseName()] = childResult
					} else {
						//null值传递
						if field.GetFieldNotNil() {
							return nil
						}
					}
				case build.FIELD_TYPE_SCALAR, build.FIELD_TYPE_ENUM:
					childResult := rundata.GetFieldResultByFieldId(child.GetFieldId())
					if childResult != nil && childResult.HasParentResultBinding() {
						compositeKey := build.BuildCompositeKey(child.GetParentKeyFieldNames(), currentFieldResponseMap)
						if val, ok := childResult.LookupParentResult(compositeKey); ok {
							result[child.GetResponseName()] = val
						} else {
							//null值传递
							if child.GetFieldNotNil() {
								return nil
							}
							result[child.GetResponseName()] = nil
						}
					} else {
						//TODO 这里考虑修改buildScalarOrEnumValue方法，直接从父节点数据中组装，不用再查询rundata本节点的数据
						scalarOrEnumResult := e.buildScalarOrEnumValue(child, currentFieldResponseMap, rundata)
						if scalarOrEnumResult != nil {
							result[child.GetResponseName()] = scalarOrEnumResult
						} else {
							//null值传递
							if field.GetFieldNotNil() {
								return nil
							}
						}
					}
				}
			}
		}
	}
	return result
}

func (e *SGraphEngine) buildListValuesInListObject(field *build.FieldPlan, parentFieldResponse map[string]any, rundata *Rundata) []any {
	result := make([]any, 0, len(field.GetChildrenFields()))
	if field != nil {
		fieldResult := rundata.GetFieldResultByFieldId(field.GetFieldId())
		if fieldResult == nil {
			//如果发现当前节点没有数据，判断当前字段是否non-null，如果是则返回nil
			if field.GetFieldListNotNil() {
				result = nil
			}
			return result
		}

		//筛选出当前父节点的子节点结果数组
		compositeKey := build.BuildCompositeKey(field.GetParentKeyFieldNames(), parentFieldResponse)
		currentFieldVal, _ := fieldResult.LookupParentResult(compositeKey)
		if currentFieldResponseArray, ok := currentFieldVal.([]any); ok {
			switch field.GetFieldType() {
			case build.FIELD_TYPE_OBJECT:
				for _, response := range currentFieldResponseArray {
					if responseMap, ok := response.(map[string]any); ok {
						result = append(result, e.buildObjectValueInList(field, responseMap, rundata))
					} else {
						//如果当前List中的元素要non-null，但是有nil的元素返回，则整个结果返回nil
						if response == nil && field.GetFieldNotNil() {
							return nil
						}
					}
				}
			case build.FIELD_TYPE_SCALAR, build.FIELD_TYPE_ENUM:
				//null值传递
				if field.GetFieldNotNil() {
					for _, response := range currentFieldResponseArray {
						if response == nil {
							return nil
						}
					}
				}
				result = append(result, currentFieldResponseArray...)
			}
		} else {
			//如果当前父元素对应的数组为空，判断字段是否允许List为null，不允许则整体返回nil
			if currentFieldVal == nil {
				if field.GetFieldListNotNil() {
					return nil
				}
			}
		}
	}
	return result
}

func (e *SGraphEngine) buildObjectValueInListObject(field *build.FieldPlan, parentResponse map[string]any, rundata *Rundata) map[string]any {
	var result map[string]any
	if field != nil {
		fieldResult := rundata.GetFieldResultByFieldId(field.GetFieldId())
		if fieldResult == nil {
			return result
		}
		compositeKey := build.BuildCompositeKey(field.GetParentKeyFieldNames(), parentResponse)
		if compositeKey != "" {
			responseVal, _ := fieldResult.LookupParentResult(compositeKey)
			//如果当前字段为non-null，返回值为nil，将nil直接返回
			if responseVal == nil && field.GetFieldNotNil() {
				return result
			}

			//TODO 根据子节点继续遍历生成map
			children := field.GetChildrenFields()
			result = make(map[string]any, len(children))
			for _, child := range children {
				if child.GetFieldIsList() {
					if responseMap, ok := responseVal.(map[string]any); ok {
						childResult := e.buildListValuesInListObject(child, responseMap, rundata)
						//null值传递
						if childResult == nil && field.GetFieldListNotNil() {
							return nil
						}
						result[child.GetResponseName()] = childResult
					} else {
						//TODO 报错
					}
				} else {
					switch child.GetFieldType() {
					case build.FIELD_TYPE_OBJECT:
						if responseMap, ok := responseVal.(map[string]any); ok {
							childResult := e.buildObjectValueInListObject(child, responseMap, rundata)
							//null值传递
							if childResult == nil && child.GetFieldNotNil() {
								return nil
							}
							result[child.GetResponseName()] = childResult
						} else {
							//TODO 报错
						}
					case build.FIELD_TYPE_SCALAR, build.FIELD_TYPE_ENUM:
						if fieldResMap, ok := responseVal.(map[string]any); ok {
							childResult := e.buildScalarOrEnumValueInListObject(child, fieldResMap, rundata)
							//null值传递
							if childResult == nil && field.GetFieldNotNil() {
								return nil
							}
							result[child.GetResponseName()] = childResult
						} else {
							//TODO 报错
						}
					}
				}
			}
		}
	}
	return result
}

func (e *SGraphEngine) buildScalarOrEnumValueInListObject(field *build.FieldPlan, parentResponse map[string]any, rundata *Rundata) any {
	if field != nil {
		//TODO 先获取当前节点的全部数据
		fieldResult := rundata.GetFieldResultByFieldId(field.GetFieldId())
		if fieldResult != nil {
			arrParamPlan := field.GetArrParamPlan()
			if arrParamPlan == nil {
				return parentResponse[field.GetResponseName()]
			}
			//获取parentKey的名称
			parentKeyFieldName := arrParamPlan.GetParamKey()
			//根据parentKey获取父节点response里对应的key值
			parentKeyFieldVal, ok := parentResponse[parentKeyFieldName]
			if ok {
				//根据key值获取到当前节点里对应的数据
				response, _ := fieldResult.LookupParentResult(parentKeyFieldVal)
				return response
			}
		} else {
			//TODO 如果当前节点数据为空，直接取父节点的数据写入并返回
			return parentResponse[field.GetResponseName()]
		}
	}
	return nil
}

func (e *SGraphEngine) buildScalarOrEnumValue(currentField *build.FieldPlan, parentResponse map[string]any, rundata *Rundata) any {
	if currentField != nil {
		filedType := currentField.GetFieldType()
		switch filedType {
		case build.FIELD_TYPE_SCALAR:
			currentResponse := rundata.GetFieldResultByFieldId(currentField.GetFieldId())
			if currentResponse != nil {
				return currentResponse.responses[0]
			}

			if parentResponse != nil {
				return parentResponse[currentField.GetResponseName()]
			}
		case build.FIELD_TYPE_ENUM:
			currentResponse := rundata.GetFieldResultByFieldId(currentField.GetFieldId())
			if currentResponse != nil {
				return currentResponse.responses[0]
			}

			if parentResponse != nil {
				return parentResponse[currentField.GetResponseName()]
			}
		default:
			return nil
		}
	}
	return nil
}
