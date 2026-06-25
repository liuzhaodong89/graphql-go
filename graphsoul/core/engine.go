package core

import (
	"context"
	"fmt"

	"github.com/graphql-go/graphql/graphsoul/build"
	Lmap "github.com/liuzhaodong89/lockfree-collection/map"
)

type SGraphResult struct {
	response         map[string]any
	orderedResponses *SGraphResponseOrderedMap
	errors           []*FieldError
}

func (r *SGraphResult) GetResponse() map[string]any {

	return r.response
}

func (r *SGraphResult) GetOrderedResponses() *SGraphResponseOrderedMap {
	if r.orderedResponses == nil {
		return nil
	}
	return r.orderedResponses
}

func (r *SGraphResult) GetErrors() []*FieldError {
	return r.errors
}

type OrderedFieldResponse struct {
	key   string
	value any
}

func (f OrderedFieldResponse) GetKey() string {
	return f.key
}

func (f OrderedFieldResponse) GetValue() any {
	return f.value
}

type SGraphResponseOrderedMap struct {
	fieldResponses []OrderedFieldResponse
	indexs         map[string]uint8
}

func NewSGraphResponseOrderedMap(capacity int) *SGraphResponseOrderedMap {
	return &SGraphResponseOrderedMap{
		fieldResponses: make([]OrderedFieldResponse, 0, capacity),
		indexs:         make(map[string]uint8, capacity),
	}
}

func (r *SGraphResponseOrderedMap) Set(key string, value any) {
	if r == nil {
		return
	}

	if index, ok := r.indexs[key]; ok {
		r.fieldResponses[index].value = value
		return
	}

	r.indexs[key] = uint8(len(r.fieldResponses))
	r.fieldResponses = append(r.fieldResponses, OrderedFieldResponse{key, value})
}

func (r *SGraphResponseOrderedMap) Get(key string) (any, bool) {
	if r == nil {
		return nil, false
	}

	if index, ok := r.indexs[key]; !ok {
		return nil, false
	} else {
		return r.fieldResponses[index].value, true
	}

}

func (r *SGraphResponseOrderedMap) Fields() []OrderedFieldResponse {
	if r == nil {
		return nil
	}
	return r.fieldResponses
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

func (e *SGraphEngine) Execute(plan *build.SGraphPlan, ctx context.Context) *SGraphResult {
	//检查入参
	if plan == nil {
		return nil
	}
	//组装Rundata和context
	maxFieldId := plan.MaxFieldId()
	rundata := NewRundata(plan.GetOriginalInputs(), maxFieldId)
	if ctx == nil {
		ctx = context.TODO()
	}
	//组装Batches
	batches := e.getBatchesFromCacheOrCreate(plan)
	//遍历执行Batches，判断遇到中断则返回
	for _, batch := range batches {
		br := batch.Execute(rundata, ctx)
		if br.IsInterrupt() {
			break
		}
	}
	//组装结果
	result := e.assembleGraphResult(plan, rundata, ctx)

	//结果回收到缓存池
	for i := range rundata.fieldResultSlice {
		if fieldResponse := rundata.fieldResultSlice[i].Load(); fieldResponse != nil {
			ReleaseFieldResponse(fieldResponse)
		}
	}
	return result
}

func (e *SGraphEngine) assembleGraphResult(plan *build.SGraphPlan, rundata *Rundata, ctx context.Context) *SGraphResult {
	result := &SGraphResult{
		response:         make(map[string]any),
		orderedResponses: nil,
		errors:           nil,
	}
	if plan == nil {
		return result
	}

	roots := plan.GetRoots()
	//responseMap := make(map[string]any, len(roots))
	orderedResponsesMap := NewSGraphResponseOrderedMap(len(roots))

	for _, root := range roots {
		rootValueMeta := root.GetFieldValueMetaInfo()
		if rootValueMeta.IsList {
			rootResult := e.buildListValues(root, nil, rundata, ctx)
			//null值传递
			if rootValueMeta.NotNil && rootResult == nil {
				//responseMap = nil
				orderedResponsesMap = nil
				break
			} else {
				//responseMap[root.GetResponseName()] = rootResult
				orderedResponsesMap.Set(root.GetResponseName(), rootResult)
			}
		} else {
			if root.GetFieldType() == build.FieldValueTypeObject {
				rootResult := e.buildObjectValue(root, nil, rundata, ctx)
				//null值传递
				if rootResult == nil && rootValueMeta.NotNil {
					//responseMap = nil
					orderedResponsesMap = nil
					break
				} else {
					//responseMap[root.GetResponseName()] = rootResult
					orderedResponsesMap.Set(root.GetResponseName(), rootResult)
				}
			} else if root.GetFieldType() == build.FieldValueTypeScalar || root.GetFieldType() == build.FieldValueTypeEnum {
				rootResult := e.buildScalarOrEnumValue(root, nil, rundata, ctx)
				//null值传递
				if rootResult == nil && rootValueMeta.NotNil {
					//responseMap = nil
					orderedResponsesMap = nil
					break
				} else {
					//responseMap[root.GetResponseName()] = rootResult
					orderedResponsesMap.Set(root.GetResponseName(), rootResult)
				}
			}
		}

	}
	//result.response = responseMap
	result.orderedResponses = orderedResponsesMap
	result.errors = rundata.GetAllFieldErrors()
	return result

}

func (e *SGraphEngine) buildObjectValue(field *build.FieldPlan, parentResponse map[string]any, rundata *Rundata, ctx context.Context) *SGraphResponseOrderedMap {
	if field != nil {
		children := field.GetChildrenFields()
		//result := make(map[string]any, len(children))
		result := NewSGraphResponseOrderedMap(len(children))

		fieldResponse, extractErr := e.extractFieldResponse(field, parentResponse, rundata)
		//如果当前字段的结果为空，不再遍历子字段
		if fieldResponse == nil || extractErr != nil {
			return nil
		}
		fieldResponseAsMap, _ := fieldResponse.(map[string]any)
		for _, child := range children {
			childValueMeta := child.GetFieldValueMetaInfo()
			if childValueMeta.IsList {
				//如果子字段是List类型且List为non-null但是出现nil，则判断当前字段是否为non-null，如果是则清空当前字段的数据返回nil
				childResult := e.buildListValues(child, fieldResponseAsMap, rundata, ctx)
				//null值传递，如果子字段non-null但返回nil，当前字段直接返回nil
				if childResult == nil && childValueMeta.NotNil {
					return nil
				}
				//result[child.GetResponseName()] = childResult
				result.Set(child.GetResponseName(), childResult)
			} else {
				switch child.GetFieldType() {
				case build.FieldValueTypeObject:
					childResult := e.buildObjectValue(child, fieldResponseAsMap, rundata, ctx)
					//null值传递
					if childResult == nil && childValueMeta.NotNil {
						return nil
					}
					//result[child.GetResponseName()] = childResult
					result.Set(child.GetResponseName(), childResult)
				case build.FieldValueTypeScalar, build.FieldValueTypeEnum:
					if fieldResMap, ok := fieldResponse.(map[string]any); ok {
						childResult := e.buildScalarOrEnumValue(child, fieldResMap, rundata, ctx)
						//null值传递
						if childValueMeta.NotNil && childResult == nil {
							return nil
						}
						//result[child.GetResponseName()] = childResult
						result.Set(child.GetResponseName(), childResult)
					} else {
						if fieldResponse == nil {
							return nil
						}
						//result[child.GetResponseName()] = nil
						result.Set(child.GetResponseName(), nil)
					}
				}
			}
		}
		return result
	}
	return nil
}

func (e *SGraphEngine) buildListValues(field *build.FieldPlan, parentResponse map[string]any, rundata *Rundata, ctx context.Context) []any {
	if field != nil {
		var result []any

		fieldResponse, extractErr := e.extractFieldResponse(field, parentResponse, rundata)
		fieldResponseAsList, ok := fieldResponse.([]any)
		if fieldResponse == nil || extractErr != nil || !ok {
			return nil
		}
		result = make([]any, 0, len(fieldResponseAsList))
		fieldValueMeta := field.GetFieldValueMetaInfo()

		switch field.GetFieldType() {
		case build.FieldValueTypeObject:
			for _, response := range fieldResponseAsList {
				//TODO 如果当前的array的字段类型是Object，需要循环创建对应的map并加入到切片中
				if resMap, ok := response.(map[string]any); ok {
					objectResult := e.buildObjectValueInList(field, resMap, rundata, ctx)
					//null值冒泡
					if objectResult == nil && fieldValueMeta.NotNil {
						return nil
					}
					result = append(result, objectResult)
				}
			}
			return result
		case build.FieldValueTypeScalar, build.FieldValueTypeEnum:
			//null值冒泡
			if fieldValueMeta.NotNil && len(fieldResponseAsList) == 0 {
				return nil
			}

			result = append(result, fieldResponseAsList...)
			return result
		}
	}
	return nil
}

func (e *SGraphEngine) buildObjectValueInList(field *build.FieldPlan, currentFieldResponseMap map[string]any, rundata *Rundata, ctx context.Context) *SGraphResponseOrderedMap {
	//var result map[string]any
	var result *SGraphResponseOrderedMap
	//如果当前元素的response为空，则直接返回，不再遍历子字段
	if currentFieldResponseMap == nil {
		return result
	}
	//TODO 获取当前字段的子字段，遍历每个子字段的类型组装Map
	if field != nil {
		fieldValueMeta := field.GetFieldValueMetaInfo()
		children := field.GetChildrenFields()
		//result = make(map[string]any, len(children))
		result = NewSGraphResponseOrderedMap(len(children))
		for _, child := range children {
			childValueMeta := child.GetFieldValueMetaInfo()
			if childValueMeta.IsList {
				childResult := e.buildListValuesInListObject(child, currentFieldResponseMap, rundata, ctx)
				//null值冒泡
				if childResult == nil && childValueMeta.NotNil {
					return nil
				}
				//result[child.GetResponseName()] = childResult
				result.Set(child.GetResponseName(), childResult)
			} else {
				switch child.GetFieldType() {
				case build.FieldValueTypeObject:
					childResult := e.buildObjectValueInListObject(child, currentFieldResponseMap, rundata, ctx)
					//null值冒泡
					if childValueMeta.NotNil && childResult == nil {
						return nil
					}
					//result[child.GetResponseName()] = childResult
					result.Set(child.GetResponseName(), childResult)
				case build.FieldValueTypeScalar, build.FieldValueTypeEnum:
					childResult := rundata.GetFieldResultByFieldId(child.GetFieldId())
					if childResult != nil && childResult.HasParentResultBinding() {
						compositeKey := build.GenerateCompositeKey([]string{child.GetParentKeyFieldName()}, currentFieldResponseMap)
						if val, ok := childResult.LookupParentResult(compositeKey); ok {
							//result[child.GetResponseName()] = val
							result.Set(child.GetResponseName(), val)
						} else {
							//null值传递
							if childValueMeta.NotNil {
								return nil
							}
							//result[child.GetResponseName()] = nil
							result.Set(child.GetResponseName(), nil)
						}
					} else {
						//TODO 这里考虑修改buildScalarOrEnumValue方法，直接从父节点数据中组装，不用再查询rundata本节点的数据
						scalarOrEnumResult := e.buildScalarOrEnumValue(child, currentFieldResponseMap, rundata, ctx)
						if scalarOrEnumResult != nil {
							//result[child.GetResponseName()] = scalarOrEnumResult
							result.Set(child.GetResponseName(), scalarOrEnumResult)
						} else {
							//null值传递
							if fieldValueMeta.NotNil {
								return nil
							}
							//result[child.GetResponseName()] = nil
							result.Set(child.GetResponseName(), nil)
						}
					}
				}
			}
		}
	}
	return result
}

// 对于List类型的字段返回值，由于内部的null值冒泡在resolve阶段已经冒泡完成，组装阶段仅做组装处理
func (e *SGraphEngine) buildListValuesInListObject(field *build.FieldPlan, parentFieldResponse map[string]any, rundata *Rundata, ctx context.Context) []any {
	result := make([]any, 0, len(field.GetChildrenFields()))
	if field != nil {
		fieldValueMetaInfo := field.GetFieldValueMetaInfo()

		currentFieldResponse, extractErr := e.extractFieldResponse(field, parentFieldResponse, rundata)
		//筛选出当前父节点的子节点结果数组
		if extractErr != nil {
			return nil
		}
		if currentFieldResponseArray, ok := currentFieldResponse.([]any); ok {
			switch field.GetFieldType() {
			case build.FieldValueTypeObject:
				for _, response := range currentFieldResponseArray {
					if responseMap, ok := response.(map[string]any); ok {
						result = append(result, e.buildObjectValueInList(field, responseMap, rundata, ctx))
					} else {
						result = append(result, nil)
					}
				}
			case build.FieldValueTypeScalar, build.FieldValueTypeEnum:
				result = append(result, currentFieldResponseArray...)
			}
		} else {
			//如果当前父元素对应的数组为空，判断字段是否允许为null，不允许则整体返回nil
			if currentFieldResponse == nil && fieldValueMetaInfo.NotNil {
				return nil
			}
		}
	}
	return result
}

func (e *SGraphEngine) buildObjectValueInListObject(field *build.FieldPlan, parentResponse map[string]any, rundata *Rundata, ctx context.Context) *SGraphResponseOrderedMap {
	//var result map[string]any
	var result *SGraphResponseOrderedMap
	if field != nil {
		fieldValueMetaInfo := field.GetFieldValueMetaInfo()

		//获取当前字段的结果，如果有运行过程取运行时数据，没有则从父字段结果读取
		fieldResponse, extractErr := e.extractFieldResponse(field, parentResponse, rundata)
		if extractErr != nil {
			return nil
		}
		//如果当前字段为non-null，返回值为nil，将nil直接返回
		if fieldResponse == nil && fieldValueMetaInfo.NotNil {
			return nil
		}

		//根据子节点继续遍历生成map
		children := field.GetChildrenFields()
		//result = make(map[string]any, len(children))
		result = NewSGraphResponseOrderedMap(len(children))
		for _, child := range children {
			childValueMetaInfo := child.GetFieldValueMetaInfo()
			if childValueMetaInfo.IsList {
				//对于List类型字段，null值冒泡已经在resolve阶段完成，这里只做组装
				if responseMap, ok := fieldResponse.(map[string]any); ok {
					childResult := e.buildListValuesInListObject(child, responseMap, rundata, ctx)
					//null值传递
					if childResult == nil && childValueMetaInfo.NotNil {
						return nil
					}
					//result[child.GetResponseName()] = childResult
					result.Set(child.GetResponseName(), childResult)
				} else {
					//result[child.GetResponseName()] = nil
					result.Set(child.GetResponseName(), nil)
				}
			} else {
				switch child.GetFieldType() {
				case build.FieldValueTypeObject:
					if responseMap, ok := fieldResponse.(map[string]any); ok {
						childResult := e.buildObjectValueInListObject(child, responseMap, rundata, ctx)
						//null值传递
						if childResult == nil && childValueMetaInfo.NotNil {
							return nil
						}
						//result[child.GetResponseName()] = childResult
						result.Set(child.GetResponseName(), childResult)
					} else {
						//result[child.GetResponseName()] = nil
						result.Set(child.GetResponseName(), nil)
					}
				case build.FieldValueTypeScalar, build.FieldValueTypeEnum:
					if fieldResMap, ok := fieldResponse.(map[string]any); ok {
						childResult := e.buildScalarOrEnumValueInListObject(child, fieldResMap, rundata, ctx)
						//null值传递
						if childResult == nil && childValueMetaInfo.NotNil {
							return nil
						}
						//result[child.GetResponseName()] = childResult
						result.Set(child.GetResponseName(), childResult)
					} else {
						//result[child.GetResponseName()] = nil
						result.Set(child.GetResponseName(), nil)
					}
				}
			}
		}
	}
	return result
}

func (e *SGraphEngine) buildScalarOrEnumValueInListObject(field *build.FieldPlan, parentResponse map[string]any, rundata *Rundata, ctx context.Context) any {
	if field != nil {
		//TODO 先获取当前节点的全部数据
		val, extractErr := e.extractFieldResponse(field, parentResponse, rundata)
		if extractErr != nil {
			return nil
		}
		return val
		//fieldResult := rundata.GetFieldResultByFieldId(field.GetFieldId())

		//if fieldResult != nil {
		//	arrParamPlan := field.GetArrParamPlan()
		//	if arrParamPlan == nil {
		//		return parentResponse[field.GetFieldName()]
		//	}
		//	//获取parentKey的名称
		//	parentKeyFieldName := arrParamPlan.GetParamKey()
		//	//根据parentKey获取父节点response里对应的key值
		//	parentKeyFieldVal, ok := parentResponse[parentKeyFieldName]
		//	if ok {
		//		//根据key值获取到当前节点里对应的数据
		//		response, _ := fieldResult.LookupParentResult(parentKeyFieldVal)
		//		return response
		//	}
		//} else {
		//	//TODO 如果当前节点数据为空，直接取父节点的数据写入并返回
		//	return parentResponse[field.GetFieldName()]
		//}
	}
	return nil
}

func (e *SGraphEngine) buildScalarOrEnumValue(currentField *build.FieldPlan, parentResponse map[string]any, rundata *Rundata, ctx context.Context) any {
	if currentField != nil {
		filedType := currentField.GetFieldType()
		switch filedType {
		case build.FieldValueTypeScalar:
			currentResponse := rundata.GetFieldResultByFieldId(currentField.GetFieldId())
			if currentResponse != nil {
				return currentResponse.responses[0]
			}

			if parentResponse != nil {
				return parentResponse[currentField.GetFieldName()]
			}
		case build.FieldValueTypeEnum:
			currentResponse := rundata.GetFieldResultByFieldId(currentField.GetFieldId())
			if currentResponse != nil {
				return currentResponse.responses[0]
			}

			if parentResponse != nil {
				return parentResponse[currentField.GetFieldName()]
			}
		default:
			return nil
		}
	}
	return nil
}

func (e *SGraphEngine) extractFieldResponse(fieldPlan *build.FieldPlan, parentResponse map[string]any, rundata *Rundata) (any, error) {
	//当前字段有resolver，优先获取执行结果
	if fieldPlan == nil {
		return nil, nil
	}
	if rundata == nil {
		return nil, fmt.Errorf("rundata is nil")
	}
	if fieldPlan.GetResolverFunc() != nil {
		fieldResult := rundata.GetFieldResultByFieldId(fieldPlan.GetFieldId())
		if fieldResult != nil {
			if fieldPlan.GetFieldValueMetaInfo().IsList {
				//如果有父节点结果的绑定关系，根据绑定关系取数
				if fieldResult.HasParentResultBinding() {
					compositeKey := build.GenerateCompositeKey([]string{fieldPlan.GetParentKeyFieldName()}, parentResponse)
					if bindingChildResponse, ok := fieldResult.LookupParentResult(compositeKey); ok {
						return bindingChildResponse, nil
					}
				}
				//如果没有父节点结果的绑定关系，直接返回
				return fieldResult.responses, nil
			}
			if len(fieldResult.responses) > 0 {
				return fieldResult.responses[0], nil
			}
		}
		return nil, nil
	}
	//当前字段没有resolver，从父字段结果中获取执行结果
	if parentResponse != nil {
		return parentResponse[fieldPlan.GetFieldName()], nil
	}
	return nil, nil
}
