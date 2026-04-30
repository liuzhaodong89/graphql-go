package core

import (
	"context"
	"errors"
	"fmt"

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
		// parentRes := rundata.GetFieldResultByFieldId(s.fieldPlan.GetParentFieldId())
		//if s.fieldPlan.IsParentFieldNotNil() && parentRes == nil {
		//	fe := rundata.AddFieldError(s.fieldPlan.GetFieldId(), FIELD_ERROR_TYPE_FIELD, errors.New("parent field is nil"), s.fieldPlan.GetPaths())
		//	fe.message = "parent field is nil"
		//	fe.fieldType = FIELD_ERROR_TYPE_FIELD
		//	return fe
		//}
		//获取参数
		params, paramErr := s.prepareParams(s.fieldPlan.GetParamPlans(), rundata)
		if paramErr != nil {
			fe := rundata.AddFieldError(s.fieldPlan.GetFieldId(), FIELD_ERROR_TYPE_FIELD, paramErr, s.fieldPlan.GetPaths())
			return fe
		}
		//方法调用
		resolverFunc := s.fieldPlan.GetResolverFunc()
		if resolverFunc == nil {
			fe := rundata.AddFieldError(s.fieldPlan.GetFieldId(), FIELD_ERROR_TYPE_FIELD, errors.New("resolver is nil"), s.fieldPlan.GetPaths())
			return fe
		}
		res, err := resolverFunc(nil, params, ctx)
		if err != nil {
			fe := rundata.AddFieldError(s.fieldPlan.GetFieldId(), FIELD_ERROR_TYPE_FIELD, err, s.fieldPlan.GetPaths())
			return fe
		}

		//结果写入Rundata
		fieldResponse := AcquireFieldResponse(FIELD_RESPONSE_TYPE_NORMAL)

		// list 字段将返回的数组展开为多条 response，与 IteratorStep 的结果结构保持一致
		if s.fieldPlan.GetFieldIsList() {
			//判断当前List能否为空，若non-null但返回nil则报错
			if s.fieldPlan.GetFieldListNotNil() && res == nil {
				nilErr := errors.New("non-null list response is nil")
				fe := rundata.AddFieldError(s.fieldPlan.GetFieldId(), FIELD_ERROR_TYPE_FIELD, nilErr, s.fieldPlan.GetPaths())
				return fe
			}
			if resArr, ok := res.([]any); ok {
				//basePath := append([]string{}, s.fieldPlan.GetPaths()...)
				for _, item := range resArr {
					//判断当前List中的元素是否为空，若non-null但返回nil则报错
					if s.fieldPlan.GetFieldNotNil() && item == nil {
						nilErr := errors.New("non-null field response is nil")
						fe := rundata.AddFieldError(s.fieldPlan.GetFieldId(), FIELD_ERROR_TYPE_FIELD, nilErr, s.fieldPlan.GetPaths())
						return fe
					}
					fieldResponse.responses = append(fieldResponse.responses, item)
					//path := append(basePath[:len(basePath):len(basePath)], strconv.Itoa(i))
					//fieldResponse.fieldPaths = append(fieldResponse.fieldPaths, path)
				}
			}
		} else {
			//如果当前字段的返回值不是List
			//判断结果能否为空，若non-null但返回nil则报错
			if res == nil && s.fieldPlan.GetFieldNotNil() {
				nilErr := errors.New("non-null field response is nil")
				fe := rundata.AddFieldError(s.fieldPlan.GetFieldId(), FIELD_ERROR_TYPE_FIELD, nilErr, s.fieldPlan.GetPaths())
				return fe
			}
			fieldResponse.responses = append(fieldResponse.responses, res)
			//fieldResponse.fieldPaths = append(fieldResponse.fieldPaths, s.fieldPlan.GetPaths())
		}
		rundata.SetFieldResult(s.fieldPlan.GetFieldId(), fieldResponse)
	}
	return nil
}

func (s *NormalStep) prepareParams(paramPlans []*build.ParamPlan, rundata *Rundata) (map[string]any, error) {
	params := make(map[string]any, len(paramPlans))
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
		fieldResponse := AcquireFieldResponse(FIELD_RESPONSE_TYPE_ARRAY)
		fieldResponse.arrayParentKeyMap = make(map[any]any)

		//判断父节点数据是否允许为空
		// parentRes := rundata.GetFieldResultByFieldId(s.fieldPlan.GetParentFieldId())
		//if s.fieldPlan.IsParentFieldNotNil() && parentRes == nil {
		//	fe := rundata.AddFieldError(s.fieldPlan.GetFieldId(), FIELD_ERROR_TYPE_FIELD, errors.New("parent field is nil"), s.fieldPlan.GetPaths())
		//	fe.message = "parent field is nil"
		//	fe.fieldType = FIELD_ERROR_TYPE_FIELD
		//	return fe
		//}

		if arrayResolverFunc := s.fieldPlan.GetArrayResolverFunc(); arrayResolverFunc != nil {
			//获取批量模式参数
			arrParams, arrParamsErr := s.prepareArrayParams(s.fieldPlan.GetArrParamPlan(), rundata)
			if arrParamsErr != nil {
				fe := rundata.AddFieldError(s.fieldPlan.GetFieldId(), FIELD_ERROR_TYPE_FIELD, arrParamsErr, s.fieldPlan.GetPaths())
				fe.message = arrParamsErr.Error()
				fe.errorType = FIELD_ERROR_TYPE_FIELD
				return fe
			}

			res, err := arrayResolverFunc(nil, arrParams, ctx)
			if err != nil {
				fe := rundata.AddFieldError(s.fieldPlan.GetFieldId(), FIELD_ERROR_TYPE_FIELD, err, s.fieldPlan.GetPaths())
				fe.message = err.Error()
				fe.errorType = FIELD_ERROR_TYPE_FIELD
				return fe
			}

			//入参是数组，默认返回值一定也是数组
			if resArr, ok := res.([]any); ok {
				//basePath := append([]string{}, s.fieldPlan.GetPaths()...)
				for _, singleResVal := range resArr {
					if s.fieldPlan.GetFieldIsList() {
						//判断当前节点的返回值类型，如果是List则判断List能否为空，若non-null但返回nil则报错
						if s.fieldPlan.GetFieldListNotNil() && singleResVal == nil {
							nilErr := errors.New("non-null list response is nil")
							fe := rundata.AddFieldError(s.fieldPlan.GetFieldId(), FIELD_ERROR_TYPE_FIELD, nilErr, s.fieldPlan.GetPaths())
							return fe
						}
					} else {
						if s.fieldPlan.GetFieldNotNil() && singleResVal != nil {
							nilErr := errors.New("non-null field response is nil")
							fe := rundata.AddFieldError(s.fieldPlan.GetFieldId(), FIELD_ERROR_TYPE_FIELD, nilErr, s.fieldPlan.GetPaths())
							return fe
						}
					}
					fieldResponse.responses = append(fieldResponse.responses, singleResVal)

					//注意！返回值数组中元素的顺序必须和入参中的顺序保持一致！
					//path := append(basePath[:len(basePath):len(basePath)], strconv.Itoa(index))
					//fieldResponse.fieldPaths = append(fieldResponse.fieldPaths, path)

					//TODO 在批量查询的结果中要写入当前结果和父节点数据之间的映射关系
					if singleResMap, resMapOk := singleResVal.(map[string]any); resMapOk {
						//返回值是Object
						fieldKeyName := s.fieldPlan.GetArrayResultParentKeyName()
						fieldKeyValue := fmt.Sprintf("%v", singleResMap[fieldKeyName])
						fieldResponse.BindParentResult(fieldKeyValue, singleResMap)
					} else if singleResAsArr, resArrOk := singleResVal.([]any); resArrOk {
						//TODO 返回值是Array
						if len(singleResAsArr) > 0 {
							//如果每个元素是List，还要检查当前字段是否允许List中的元素为空，如果non-null返回的元素有nil则报错
							for _, valInSingleResAsArr := range singleResAsArr {
								if valInSingleResAsArr == nil {
									if s.fieldPlan.GetFieldNotNil() {
										nilErr := errors.New("non-null field response is nil")
										fe := rundata.AddFieldError(s.fieldPlan.GetFieldId(), FIELD_ERROR_TYPE_FIELD, nilErr, s.fieldPlan.GetPaths())
										return fe
									}
								}
							}
							firstSingleResValInArr := singleResAsArr[0]
							if firstSingleResValInArrMap, firstSingleResValInArrMapOk := firstSingleResValInArr.(map[string]any); firstSingleResValInArrMapOk {
								fieldKeyName := s.fieldPlan.GetArrayResultParentKeyName()
								fieldKeyValue := fmt.Sprintf("%v", firstSingleResValInArrMap[fieldKeyName])
								fieldResponse.BindParentResult(fieldKeyValue, singleResAsArr)
							}
						}
					}
				}
			} else {
				//TODO如果返回值不是数组类型，应该报错
			}
		} else {
			//获取遍历模式调用项（父节点响应 + 本次参数）
			callItems, paramErr := s.prepareIteratorParams(s.fieldPlan.GetParamPlans(), rundata)
			if paramErr != nil {
				fe := rundata.AddFieldError(s.fieldPlan.GetFieldId(), FIELD_ERROR_TYPE_FIELD, paramErr, s.fieldPlan.GetPaths())
				fe.message = paramErr.Error()
				fe.errorType = FIELD_ERROR_TYPE_FIELD
				return fe
			}

			resolverFunc := s.fieldPlan.GetResolverFunc()
			if resolverFunc == nil {
				fe := rundata.AddFieldError(s.fieldPlan.GetFieldId(), FIELD_ERROR_TYPE_FIELD, errors.New("resolver is nil"), s.fieldPlan.GetPaths())
				fe.message = "resolver is nil"
				fe.errorType = FIELD_ERROR_TYPE_FIELD
				return fe
			}
			//basePath := append([]string{}, s.fieldPlan.GetPaths()...)
			for _, item := range callItems {
				res, err := resolverFunc(nil, item.params, ctx)
				if err != nil {
					fe := rundata.AddFieldError(s.fieldPlan.GetFieldId(), FIELD_ERROR_TYPE_FIELD, err, s.fieldPlan.GetPaths())
					fe.message = err.Error()
					fe.errorType = FIELD_ERROR_TYPE_FIELD
					return fe
				}
				if s.fieldPlan.GetFieldIsList() {
					//如果是List类型的字段，先判断List能否为空，若non-null但List为nil则报错
					if res == nil {
						if s.fieldPlan.GetFieldListNotNil() {
							nilErr := errors.New("non-null list response is nil")
							fe := rundata.AddFieldError(s.fieldPlan.GetFieldId(), FIELD_ERROR_TYPE_FIELD, nilErr, s.fieldPlan.GetPaths())
							return fe
						}
					} else {
						//再判断里面的元素能否为空，若non-null但元素有nil就报错
						if resArr, ok := res.([]any); ok {
							for _, val := range resArr {
								if val == nil {
									if s.fieldPlan.GetFieldNotNil() {
										nilErr := errors.New("non-null field response is nil")
										fe := rundata.AddFieldError(s.fieldPlan.GetFieldId(), FIELD_ERROR_TYPE_FIELD, nilErr, s.fieldPlan.GetPaths())
										return fe
									}
								}
							}
						} else {
							//TODO 应该返回list但不是，报错
						}
					}
				} else {
					//如果是普通类型字段，判断是否允许返回值为空，若non-null但返回值是nil则报错
					if s.fieldPlan.GetFieldNotNil() && res == nil {
						nilErr := errors.New("non-null field response is nil")
						fe := rundata.AddFieldError(s.fieldPlan.GetFieldId(), FIELD_ERROR_TYPE_FIELD, nilErr, s.fieldPlan.GetPaths())
						return fe
					}
				}
				fieldResponse.responses = append(fieldResponse.responses, res)
				//在path中要加入所属数据在父节点中的序号
				//path := append(basePath[:len(basePath):len(basePath)], strconv.Itoa(index))
				//fieldResponse.fieldPaths = append(fieldResponse.fieldPaths, path)

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
		params := make(map[string]any, len(paramPlans))
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
