package core

import (
	"context"
	"errors"
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
		fieldResponse.responses = append(fieldResponse.responses, res)
		fieldResponse.fieldPaths = append(fieldResponse.fieldPaths, s.fieldPlan.GetPaths())
		rundata.SetFieldResult(s.fieldPlan.GetFieldId(), fieldResponse)
	}
	return nil
}

func (s *NormalStep) prepareParams(paramPlans []*build.ParamPlan, rundata *Rundata) (map[string]any, error) {
	//TODO
	return nil, nil
}

func (s *NormalStep) GetFieldPlan() *build.FieldPlan {
	return s.fieldPlan
}

type IteratorStep struct {
	fieldPlan *build.FieldPlan
}

const ITERATOR_PARAM_DEFAULT_KEY = "ITERATOR_PARAM_DEFAULT_KEY"

func (s *IteratorStep) Execute(rundata *Rundata, ctx context.Context) *FieldError {
	if s.fieldPlan != nil {
		//初始化结果对象
		fieldResponse := &FieldResponse{
			responseType: FIELD_RESPONSE_TYPE_ARRAY,
			responses:    make([]any, 0),
			fieldPaths:   make([][]string, 0),
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
						fieldKeyValue := singleResMap[fieldKeyName]
						fieldResponse.arrayParentKeyMap[fieldKeyValue] = singleResMap
					} else if singleResInArr, resArrOk := singleResVal.([]any); resArrOk {
						//TODO 返回值是Array
						if len(singleResInArr) > 0 {
							firstSingleResValInArr := singleResInArr[0]
							if firstSingleResValInArrMap, firstSingleResValInArrMapOk := firstSingleResValInArr.(map[string]any); firstSingleResValInArrMapOk {
								fieldKeyName := s.fieldPlan.GetArrayResultParentKeyName()
								fieldKeyValue := firstSingleResValInArrMap[fieldKeyName]
								fieldResponse.arrayParentKeyMap[fieldKeyValue] = singleResInArr
							}
						}
					}
				}
			}
		} else {
			//获取遍历模式参数
			params, paramErr := s.prepareIteratorParams(s.fieldPlan.GetParamPlans(), rundata)
			if paramErr != nil {
				fe := rundata.AddFieldError(s.fieldPlan.GetFieldId(), paramErr, s.fieldPlan.GetPaths())
				fe.message = paramErr.Error()
				fe.fieldType = FIELD_ERROR_TYPE_FIELD
				return fe
			}
			for index, param := range params {
				if paramMap, ok := param.(map[string]any); ok {
					resolverFunc := s.fieldPlan.GetArrayResolverFunc()
					res, err := resolverFunc(parentRes, paramMap, ctx)
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

					//TODO 在批量查询的结果中要写入当前结果和父节点数据之间的映射关系
					parentKeyName := s.fieldPlan.GetParentFieldKeyName()
					parentKeyValue := paramMap[parentKeyName]
					fieldResponse.arrayParentKeyMap[parentKeyValue] = res
				} else {
					fe := rundata.AddFieldError(s.fieldPlan.GetFieldId(), errors.New("parameter type is not map"), s.fieldPlan.GetPaths())
					fe.message = "parameter type is not map"
					fe.fieldType = FIELD_ERROR_TYPE_FIELD
					return fe
				}
			}
		}
		//Rundata写入数据
		rundata.SetFieldResult(s.fieldPlan.GetFieldId(), fieldResponse)
	}
	return nil
}

func (s *IteratorStep) prepareIteratorParams(paramPlans []*build.ParamPlan, rundata *Rundata) ([]any, error) {
	//TODO
	return nil, nil
}

func (s *IteratorStep) prepareArrayParams(arrParamPlan *build.ParamPlan, rundata *Rundata) (map[string]any, error) {
	return nil, nil
}

func (s *IteratorStep) GetFieldPlan() *build.FieldPlan {
	return s.fieldPlan
}
