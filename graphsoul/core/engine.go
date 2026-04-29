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
	} else {
		batches := BuildBatches(plan)
		e.batchCache.Set(cacheKey, batches)
		return batches
	}
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
			responseMap[root.GetResponseName()] = e.buildListValues(root, rundata)
		} else {
			if root.GetFieldType() == build.FIELD_TYPE_OBJECT {
				responseMap[root.GetResponseName()] = e.buildObjectValue(root, rundata)
			} else if root.GetFieldType() == build.FIELD_TYPE_SCALAR || root.GetFieldType() == build.FIELD_TYPE_ENUM {
				responseMap[root.GetResponseName()] = e.buildScalarOrEnumValue(root, nil, rundata)
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
		for _, child := range children {
			if child.GetFieldIsList() {
				result[child.GetResponseName()] = e.buildListValues(child, rundata)
			} else {
				switch child.GetFieldType() {
				case build.FIELD_TYPE_OBJECT:
					result[child.GetResponseName()] = e.buildObjectValue(child, rundata)
				case build.FIELD_TYPE_SCALAR:
					if fieldResponse != nil && len(fieldResponse.responses) > 0 {
						if fieldResMap, ok := fieldResponse.responses[0].(map[string]any); ok {
							result[child.GetResponseName()] = e.buildScalarOrEnumValue(child, fieldResMap, rundata)
						}
					}
				case build.FIELD_TYPE_ENUM:
					if fieldResponse != nil && len(fieldResponse.responses) > 0 {
						if fieldResMap, ok := fieldResponse.responses[0].(map[string]any); ok {
							result[child.GetResponseName()] = e.buildScalarOrEnumValue(child, fieldResMap, rundata)
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
	var result []any
	if field != nil {
		fieldResponse := rundata.GetFieldResultByFieldId(field.GetFieldId())

		switch field.GetFieldType() {
		case build.FIELD_TYPE_OBJECT:
			if fieldResponse == nil {
				return result
			}
			for _, response := range fieldResponse.responses {
				//TODO 如果当前的array的字段类型是Object，需要循环创建对应的map并加入到切片中
				if resMap, ok := response.(map[string]any); ok {
					result = append(result, e.buildObjectValueInList(field, resMap, rundata))
				}
			}
			return result
		case build.FIELD_TYPE_SCALAR:
			if fieldResponse == nil {
				return result
			}
			result = append(result, fieldResponse.responses...)
		case build.FIELD_TYPE_ENUM:
			if fieldResponse == nil {
				return result
			}
			result = append(result, fieldResponse.responses...)
		}
	}
	return result
}

func (e *SGraphEngine) buildObjectValueInList(field *build.FieldPlan, currentFieldResponseMap map[string]any, rundata *Rundata) map[string]any {
	var result map[string]any
	//TODO 获取当前字段的子字段，遍历每个子字段的类型组装Map
	if field != nil {
		children := field.GetChildrenFields()
		result = make(map[string]any, len(children))
		for _, child := range children {
			if child.GetFieldIsList() {
				result[child.GetResponseName()] = e.buildListValuesInListObject(child, currentFieldResponseMap, rundata)
			} else {
				switch child.GetFieldType() {
				case build.FIELD_TYPE_OBJECT:
					result[child.GetResponseName()] = e.buildObjectValueInListObject(child, currentFieldResponseMap, rundata)
				case build.FIELD_TYPE_SCALAR, build.FIELD_TYPE_ENUM:
					childResult := rundata.GetFieldResultByFieldId(child.GetFieldId())
					if childResult != nil && childResult.HasParentResultBinding() {
						compositeKey := build.BuildCompositeKey(child.GetParentKeyFieldNames(), currentFieldResponseMap)
						if val, ok := childResult.LookupParentResult(compositeKey); ok {
							result[child.GetResponseName()] = val
						} else {
							result[child.GetResponseName()] = nil
						}
					} else {
						result[child.GetResponseName()] = e.buildScalarOrEnumValue(child, currentFieldResponseMap, rundata)
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

					}
				}
			case build.FIELD_TYPE_SCALAR:
				result = append(result, currentFieldResponseArray...)
			case build.FIELD_TYPE_ENUM:
				result = append(result, currentFieldResponseArray...)
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

			//TODO 根据子节点继续遍历生成map
			children := field.GetChildrenFields()
			result = make(map[string]any, len(children))
			for _, child := range children {
				if child.GetFieldIsList() {
					if responseMap, ok := responseVal.(map[string]any); ok {
						result[child.GetResponseName()] = e.buildListValuesInListObject(child, responseMap, rundata)
					} else {
						//TODO 报错
					}
				} else {
					switch child.GetFieldType() {
					case build.FIELD_TYPE_OBJECT:
						if responseMap, ok := responseVal.(map[string]any); ok {
							result[child.GetResponseName()] = e.buildObjectValueInListObject(child, responseMap, rundata)
						} else {
							//TODO 报错
						}
					case build.FIELD_TYPE_SCALAR:
						if fieldResMap, ok := responseVal.(map[string]any); ok {
							result[child.GetResponseName()] = e.buildScalarOrEnumValueInListObject(child, fieldResMap, rundata)
						} else {
							//TODO 报错
						}
					case build.FIELD_TYPE_ENUM:
						if fieldResMap, ok := responseVal.(map[string]any); ok {
							result[child.GetResponseName()] = e.buildScalarOrEnumValueInListObject(child, fieldResMap, rundata)
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
