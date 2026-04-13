package core

import (
	"context"
	"errors"
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
		rundata.SetFieldResult(s.fieldPlan.GetFieldId(), res, s.fieldPlan.GetPaths())
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
		//判断父节点数据是否允许为空
		parentRes := rundata.GetFieldResultByFieldId(s.fieldPlan.GetParentFieldId())
		if s.fieldPlan.IsParentFieldNotNil() && parentRes == nil {
			fe := rundata.AddFieldError(s.fieldPlan.GetFieldId(), errors.New("parent field is nil"), s.fieldPlan.GetPaths())
			fe.message = "parent field is nil"
			fe.fieldType = FIELD_ERROR_TYPE_FIELD
			return fe
		}

		//TODO 遍历获取数据或者批量获取数据
		if arrayResolverFunc := s.fieldPlan.GetArrayResolverFunc(); arrayResolverFunc != nil {
			//获取批量模式参数
			arrParams, arrParamsErr := s.prepareArrayParams(s.fieldPlan.GetArrParamPlans(), rundata)
			if arrParamsErr != nil {
				fe := rundata.AddFieldError(s.fieldPlan.GetFieldId(), arrParamsErr, s.fieldPlan.GetPaths())
				fe.message = arrParamsErr.Error()
				fe.fieldType = FIELD_ERROR_TYPE_FIELD
				return fe
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
			for _, param := range params {
				if paramMap, ok := param.(map[string]any); ok {
					resolverFunc := s.fieldPlan.GetArrayResolverFunc()
					res, err := resolverFunc(parentRes, paramMap, ctx)
					if err != nil {
						fe := rundata.AddFieldError(s.fieldPlan.GetFieldId(), err, s.fieldPlan.GetPaths())
						fe.message = err.Error()
						fe.fieldType = FIELD_ERROR_TYPE_FIELD
						return fe
					}
				} else {

				}
			}
		}
		//TODO Rundata写入数据
	}
	return nil
}

func (s *IteratorStep) prepareIteratorParams(paramPlans []*build.ParamPlan, rundata *Rundata) ([]any, error) {
	//TODO
	return nil, nil
}

func (s *IteratorStep) prepareArrayParams(arrParamPlans []*build.ParamPlan, rundata *Rundata) (map[string]any, error) {
	return nil, nil
}

func (s *IteratorStep) GetFieldPlan() *build.FieldPlan {
	return s.fieldPlan
}
