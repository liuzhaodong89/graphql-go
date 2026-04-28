package core

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/graphql-go/graphql/graphsoul/build"
)

type Step interface {
	Execute(rundata *Rundata, ctx context.Context) *FieldError
	GetFieldPlan() *build.FieldPlan
}

type NormalStep struct {
	fieldPlan *build.FieldPlan
}

func (s *NormalStep) Execute(rundata *Rundata, ctx context.Context) *FieldError {
	if s.fieldPlan != nil {
		//判断父节点数据是否允许空
		parentRes := rundata.GetFieldResultByFieldId(s.fieldPlan.GetParentFieldId())
		if s.fieldPlan.IsParentFieldNotNil() && parentRes == nil {
			fe := rundata.AddFieldError(s.fieldPlan.GetFieldId(), errors.New("parent field is nil"), s.fieldPlan.GetPaths())
			fe.message = "parent field is nil"
			fe.fieldType = FIELD_ERROR_TYPE_FIELD
			return fe
		}
		//获取参数
		params, paramErr := s.prepareParams(s.fieldPlan.GetParamPlans(), rundata)
		if paramErr != nil {
			fe := rundata.AddFieldError(s.fieldPlan.GetFieldId(), paramErr, s.fieldPlan.GetPaths())
			fe.message = paramErr.Error()
			fe.fieldType = FIELD_ERROR_TYPE_FIELD
			return fe
		}
		//方法调用
		resolverFunc := s.fieldPlan.GetResolverFunc()
		if resolverFunc == nil {
			fe := rundata.AddFieldError(s.fieldPlan.GetFieldId(), errors.New("resolver is nil"), s.fieldPlan.GetPaths())
			fe.message = "resolver is nil"
			fe.fieldType = FIELD_ERROR_TYPE_FIELD
			return fe
		}
		res, err := resolverFunc(parentRes, params, ctx)
		if err != nil {
			fe := rundata.AddFieldError(s.fieldPlan.GetFieldId(), err, s.fieldPlan.GetPaths())
			fe.fieldType = FIELD_ERROR_TYPE_FIELD
			fe.message = err.Error()
			return fe
		}
		//结果写入Rundata
		fieldResponse := &FieldResponse{
			responseType: FIELD_RESPONSE_TYPE_NORMAL,
			responses:    make([]any, 0),
			fieldPaths:   make([][]string, 0),
		}
		// list 字段将返回的数组展开为多条 response，与 IteratorStep 的结果结构保持一致
		if s.fieldPlan.GetFieldIsList() {
			if resArr, ok := res.([]any); ok {
				for i, item := range resArr {
					fieldResponse.responses = append(fieldResponse.responses, item)
					path := append(append([]string{}, s.fieldPlan.GetPaths()...), strconv.Itoa(i))
					fieldResponse.fieldPaths = append(fieldResponse.fieldPaths, path)
				}
			}
		} else {
			fieldResponse.responses = append(fieldResponse.responses, res)
			fieldResponse.fieldPaths = append(fieldResponse.fieldPaths, s.fieldPlan.GetPaths())
		}
		rundata.SetFieldResult(s.fieldPlan.GetFieldId(), fieldResponse)
	}
	return nil
}

func (s *NormalStep) prepareParams(paramPlans []*build.ParamPlan, rundata *Rundata) (map[string]any, error) {
	params := make(map[string]any)
	for _, pp := range paramPlans {
		switch pp.GetParamType() {
		case build.PARAM_TYPE_CONST:
			params[pp.GetParamKey()] = pp.GetConstValue()
		case build.PARAM_TYPE_INPUT:
			params[pp.GetParamKey()] = rundata.GetOriginalParamByKey(pp.GetInputName())
		case build.PARAM_TYPE_FIELD_RESULT:
			fieldResult := rundata.GetFieldResultByFieldId(pp.GetDependentFieldId())
			if fieldResult == nil || len(fieldResult.responses) == 0 {
				return nil, fmt.Errorf("dependent field %d has no result", pp.GetDependentFieldId())
			}
			params[pp.GetParamKey()] = build.GetValueByPath(fieldResult.responses[0], pp.GetFieldResultPaths())
		}
	}
	return params, nil
}

func (s *NormalStep) GetFieldPlan() *build.FieldPlan {
	return s.fieldPlan
}

type IteratorStep struct {
	fieldPlan *build.FieldPlan
}

type iteratorCallItem struct {
	parentResponse any
	params         map[string]any
}

const ITERATOR_PARAM_DEFAULT_KEY = "ITERATOR_PARAM_DEFAULT_KEY"

func (s *IteratorStep) Execute(rundata *Rundata, ctx context.Context) *FieldError {
	if s.fieldPlan != nil {
		//初始化结果对象
		fieldResponse := &FieldResponse{
			responseType:      FIELD_RESPONSE_TYPE_ARRAY,
			responses:         make([]any, 0),
			fieldPaths:        make([][]string, 0),
			arrayParentKeyMap: make(map[any]any),
		}
		//判断父节点数据是否允许为空
		parentRes := rundata.GetFieldResultByFieldId(s.fieldPlan.GetParentFieldId())
		if s.fieldPlan.IsParentFieldNotNil() && parentRes == nil {
			fe := rundata.AddFieldError(s.fieldPlan.GetFieldId(), errors.New("parent field is nil"), s.fieldPlan.GetPaths())
			fe.message = "parent field is nil"
			fe.fieldType = FIELD_ERROR_TYPE_FIELD
			return fe
		}

		if arrayResolverFunc := s.fieldPlan.GetArrayResolverFunc(); arrayResolverFunc != nil {
			//获取批量模式参数
			arrParams, arrParamsErr := s.prepareArrayParams(s.fieldPlan.GetArrParamPlan(), rundata)
			if arrParamsErr != nil {
				fe := rundata.AddFieldError(s.fieldPlan.GetFieldId(), arrParamsErr, s.fieldPlan.GetPaths())
				fe.message = arrParamsErr.Error()
				fe.fieldType = FIELD_ERROR_TYPE_FIELD
				return fe
			}

			res, err := arrayResolverFunc(parentRes, arrParams, ctx)
			if err != nil {
				fe := rundata.AddFieldError(s.fieldPlan.GetFieldId(), err, s.fieldPlan.GetPaths())
				fe.message = err.Error()
				fe.fieldType = FIELD_ERROR_TYPE_FIELD
				return fe
			}

			if resArr, ok := res.([]any); ok {
				for index, singleResVal := range resArr {
					fieldResponse.responses = append(fieldResponse.responses, singleResVal)

					//注意！返回值数组中元素的顺序必须和入参中的顺序保持一致！
					path := append(s.fieldPlan.GetPaths(), strconv.Itoa(index))
					fieldResponse.fieldPaths = append(fieldResponse.fieldPaths, path)

					//TODO 在批量查询的结果中要写入当前结果和父节点数据之间的映射关系
					if singleResMap, resMapOk := singleResVal.(map[string]any); resMapOk {
						//TODO 返回值是Object
						fieldKeyName := s.fieldPlan.GetArrayResultParentKeyName()
						fieldKeyValue := fmt.Sprintf("%v", singleResMap[fieldKeyName])
						fieldResponse.BindParentResult(fieldKeyValue, singleResMap)
					} else if singleResInArr, resArrOk := singleResVal.([]any); resArrOk {
						//TODO 返回值是Array
						if len(singleResInArr) > 0 {
							firstSingleResValInArr := singleResInArr[0]
							if firstSingleResValInArrMap, firstSingleResValInArrMapOk := firstSingleResValInArr.(map[string]any); firstSingleResValInArrMapOk {
								fieldKeyName := s.fieldPlan.GetArrayResultParentKeyName()
								fieldKeyValue := fmt.Sprintf("%v", firstSingleResValInArrMap[fieldKeyName])
								fieldResponse.BindParentResult(fieldKeyValue, singleResInArr)
							}
						}
					}
				}
			}
		} else {
			//获取遍历模式调用项（父节点响应 + 本次参数）
			callItems, paramErr := s.prepareIteratorParams(s.fieldPlan.GetParamPlans(), rundata)
			if paramErr != nil {
				fe := rundata.AddFieldError(s.fieldPlan.GetFieldId(), paramErr, s.fieldPlan.GetPaths())
				fe.message = paramErr.Error()
				fe.fieldType = FIELD_ERROR_TYPE_FIELD
				return fe
			}
			for index, item := range callItems {
				resolverFunc := s.fieldPlan.GetResolverFunc()
				if resolverFunc == nil {
					fe := rundata.AddFieldError(s.fieldPlan.GetFieldId(), errors.New("resolver is nil"), s.fieldPlan.GetPaths())
					fe.message = "resolver is nil"
					fe.fieldType = FIELD_ERROR_TYPE_FIELD
					return fe
				}
				res, err := resolverFunc(parentRes, item.params, ctx)
				if err != nil {
					fe := rundata.AddFieldError(s.fieldPlan.GetFieldId(), err, s.fieldPlan.GetPaths())
					fe.message = err.Error()
					fe.fieldType = FIELD_ERROR_TYPE_FIELD
					return fe
				}
				fieldResponse.responses = append(fieldResponse.responses, res)
				//在path中要加入所属数据在父节点中的序号
				path := append(s.fieldPlan.GetPaths(), strconv.Itoa(index))
				fieldResponse.fieldPaths = append(fieldResponse.fieldPaths, path)

				//在批量查询的结果中要写入当前结果和父节点数据之间的映射关系
				compositeKey := ""
				if parentMap, parentOk := item.parentResponse.(map[string]any); parentOk {
					compositeKey = build.BuildCompositeKey(s.fieldPlan.GetParentKeyFieldNames(), parentMap)
				}
				fieldResponse.BindParentResult(compositeKey, res)
			}
		}
		//Rundata写入数据
		rundata.SetFieldResult(s.fieldPlan.GetFieldId(), fieldResponse)
	}
	return nil
}

func (s *IteratorStep) prepareIteratorParams(paramPlans []*build.ParamPlan, rundata *Rundata) ([]iteratorCallItem, error) {
	parentResult := rundata.GetFieldResultByFieldId(s.fieldPlan.GetParentFieldId())
	if parentResult == nil {
		return nil, fmt.Errorf("parent field %d has no result", s.fieldPlan.GetParentFieldId())
	}
	result := make([]iteratorCallItem, 0, len(parentResult.responses))
	for _, parentResponse := range parentResult.responses {
		params := make(map[string]any)
		for _, pp := range paramPlans {
			switch pp.GetParamType() {
			case build.PARAM_TYPE_CONST:
				params[pp.GetParamKey()] = pp.GetConstValue()
			case build.PARAM_TYPE_INPUT:
				params[pp.GetParamKey()] = rundata.GetOriginalParamByKey(pp.GetInputName())
			case build.PARAM_TYPE_FIELD_RESULT:
				params[pp.GetParamKey()] = build.GetValueByPath(parentResponse, pp.GetFieldResultPaths())
			}
		}
		result = append(result, iteratorCallItem{
			parentResponse: parentResponse,
			params:         params,
		})
	}
	return result, nil
}

func (s *IteratorStep) prepareArrayParams(arrParamPlan *build.ParamPlan, rundata *Rundata) (map[string]any, error) {
	params := make(map[string]any)
	if arrParamPlan == nil {
		return params, nil
	}
	switch arrParamPlan.GetParamType() {
	case build.PARAM_TYPE_CONST:
		params[arrParamPlan.GetParamKey()] = arrParamPlan.GetConstValue()
	case build.PARAM_TYPE_INPUT:
		params[arrParamPlan.GetParamKey()] = rundata.GetOriginalParamByKey(arrParamPlan.GetInputName())
	case build.PARAM_TYPE_FIELD_RESULT:
		// 遍历父节点结果集，提取每个元素中的目标字段值，组装成切片
		parentResult := rundata.GetFieldResultByFieldId(arrParamPlan.GetDependentFieldId())
		if parentResult == nil {
			return nil, fmt.Errorf("dependent field %d has no result", arrParamPlan.GetDependentFieldId())
		}
		batchValues := make([]any, 0, len(parentResult.responses))
		for _, parentResponse := range parentResult.responses {
			batchValues = append(batchValues, build.GetValueByPath(parentResponse, arrParamPlan.GetFieldResultPaths()))
		}
		params[arrParamPlan.GetParamKey()] = batchValues
	}
	return params, nil
}

func (s *IteratorStep) GetFieldPlan() *build.FieldPlan {
	return s.fieldPlan
}
