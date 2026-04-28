package core

import (
	"context"

	"github.com/graphql-go/graphql/graphsoul/build"
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

type SGraphEngine struct{}

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
		batches := e.buildBatches(plan)
		//遍历执行Batches，判断遇到中断则返回
		for _, batch := range batches {
			br := batch.Execute(rundata, ctx)
			if br.IsInterrupt() {
				break
			}
		}
	}
	//组装结果
	return e.assembleGraphResult(plan, rundata)
}

func (e *SGraphEngine) buildBatches(plan *build.SGraphPlan) []*Batch {
	batches := make([]*Batch, 0)
	//TODO step的类型不是按照当前field类型来的，而是按照执行场景来的。如果step的入参是切片或数组，返回值也是切片或数组，即循环遍历的场景就是IteratorStep，反之就是NormalStep
	paramDepFieldIdBatchIdMap := make(map[uint32]*Batch)
	if plan != nil {
		//第一个batch加入到batch切片里
		firstBatch := &Batch{
			batchId:    0,
			steps:      make([]Step, 0),
			concurrent: true,
		}
		batches = append(batches, firstBatch)

		//根节点默认是普通调用
		roots := plan.GetRoots()
		for _, root := range roots {
			step := &NormalStep{
				fieldPlan: root,
			}
			firstBatch.steps = append(firstBatch.steps, step)

			//增加根节点的参数依赖关系到map里，方便后续子节点查找
			for _, paramPlan := range root.GetParamPlans() {
				paramDepFieldIdBatchIdMap[paramPlan.GetDependentFieldId()] = firstBatch
			}

			//针对每个根节点递归遍历，为子节点创建step和对应的batch
			children := root.GetChildrenFields()
			for _, child := range children {
				batches, paramDepFieldIdBatchIdMap = appendBatches(child, root, batches, paramDepFieldIdBatchIdMap)
			}
		}
	}
	return batches
}

func appendBatches(fp *build.FieldPlan, parentFP *build.FieldPlan, batches []*Batch, paramDepFieldIdBatchIdMap map[uint32]*Batch) ([]*Batch, map[uint32]*Batch) {
	if fp == nil {
		return batches, paramDepFieldIdBatchIdMap
	}
	//TODO 如果父节点非Array，当前节点有Resolver，则当前节点为Normal
	var step Step
	if !parentFP.GetFieldIsList() {
		if fp.GetResolverFunc() != nil {
			step = &NormalStep{
				fieldPlan: fp,
			}
		}
	} else {
		//TODO 如果父节点为Array，当前节点有Resolver，则当前节点为Iterator
		if fp.GetResolverFunc() != nil || fp.GetArrayResolverFunc() != nil {
			step = &IteratorStep{
				fieldPlan: fp,
			}
		}
	}

	argsPlans := make([]*build.ParamPlan, 0)
	argsPlans = append(argsPlans, fp.GetParamPlans()...)
	if fp.GetArrParamPlan() != nil {
		argsPlans = append(argsPlans, fp.GetArrParamPlan())
	}

	//TODO 根据参数查找最下层的batch，如果有参数没有找到batch则新建batch并加入到最下层
	var latestBatch *Batch
	var latestBatchId uint32 = 0
	var newBatch *Batch
	for _, argsPlan := range argsPlans {
		// CONST 和 INPUT 不依赖任何 field 结果，不参与 batch 调度
		if argsPlan.GetParamType() != build.PARAM_TYPE_FIELD_RESULT {
			continue
		}
		depFieldId := argsPlan.GetDependentFieldId()
		b := paramDepFieldIdBatchIdMap[depFieldId]
		if b != nil {
			if b.GetBatchId() > latestBatchId {
				latestBatch = b
				latestBatchId = b.GetBatchId()
			}
		} else {
			//如果出现新建batch的场景，中断循环优先按照新建batch推进
			newBatch = &Batch{
				batchId: uint32(len(batches)),
				steps:   make([]Step, 0),
			}
			if step != nil {
				newBatch.steps = append(newBatch.steps, step)
			}
			batches = append(batches, newBatch)
			paramDepFieldIdBatchIdMap[depFieldId] = newBatch
			break
		}
	}
	if newBatch == nil && latestBatch != nil {
		if step != nil {
			latestBatch.steps = append(latestBatch.steps, step)
		}
	} else if newBatch == nil && latestBatch == nil && step != nil {
		//无参数依赖的节点，直接加入到第一个batch里
		batches[0].steps = append(batches[0].steps, step)
	}

	// 递归处理子节点
	for _, child := range fp.GetChildrenFields() {
		batches, paramDepFieldIdBatchIdMap = appendBatches(child, fp, batches, paramDepFieldIdBatchIdMap)
	}

	return batches, paramDepFieldIdBatchIdMap
}

func (e *SGraphEngine) assembleGraphResult(plan *build.SGraphPlan, rundata *Rundata) *SGraphResult {
	result := &SGraphResult{
		response: make(map[string]any),
		errors:   make([]*FieldError, 0),
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
