package core

import (
	"context"
	"errors"
	"fmt"
	"maps"

	"github.com/graphql-go/graphql/graphsoul/build"
)

type Step interface {
	Execute(rundata *Rundata, ctx context.Context) *FieldError
	GetFieldPlan() *build.FieldPlan
}

type NormalParamContext struct {
	params         map[string]any
	parentResponse any
}

type NormalStep struct {
	fieldPlan *build.FieldPlan
}

func (s *NormalStep) Execute(rundata *Rundata, ctx context.Context) *FieldError {
	if s.fieldPlan != nil {
		//获取参数
		paramContext, paramErr := s.prepareParams(s.fieldPlan.GetParamPlans(), rundata)
		if paramContext == nil || paramErr != nil {
			fe := rundata.AddFieldError(s.fieldPlan.GetFieldId(), FieldErrorTypeField, paramErr, s.fieldPlan.GetPaths())
			return fe
		}

		//根据指令运行结果判定是否要执行
		var shouldExecute bool
		var directiveEvaluateErr error
		shouldExecute, directiveEvaluateErr = s.evaluateDirectivesShouldExecuteField(paramContext, rundata, ctx)
		if directiveEvaluateErr != nil {
			fe := rundata.AddFieldError(s.fieldPlan.GetFieldId(), FieldErrorTypeField, directiveEvaluateErr, s.fieldPlan.GetPaths())
			return fe
		}
		if !shouldExecute {
			return nil
		}

		//动态类型判定，interface和union type特性
		if isFieldPlanCompiledType(s.fieldPlan) {
			shouldExecute = shouldExecuteFieldAsCompiledType(s.fieldPlan, &ctx)
		} else {
			parentFullResult := paramContext.parentResponse
			if parentFullResult == nil {
				return nil
			}
			shouldExecute = shouldExecuteFieldAsRuntimeType(s.fieldPlan, parentFullResult, &ctx)
		}
		if !shouldExecute {
			return nil
		}

		//获取fieldResponse
		fieldResponse := AcquireFieldResponse(FieldResponseTypeNormal)

		//__typename
		if s.fieldPlan.IsIntrospectionTypeNameField() {
			if s.fieldPlan.GetRuntimeTypeResolverFunc() != nil {
				typeName := s.fieldPlan.GetRuntimeTypeResolverFunc()(paramContext.parentResponse, &ctx)
				if typeName == "" {
					fe := rundata.AddFieldError(s.fieldPlan.GetFieldId(), FieldErrorTypeField, errors.New("__typename resolved failed, value is empty"), s.fieldPlan.GetPaths())
					return fe
				}
				fieldResponse.responses = append(fieldResponse.responses, typeName)
				rundata.SetFieldResult(s.fieldPlan.GetFieldId(), fieldResponse)
				return nil
			}
		}

		//执行BeforeResolve指令
		if beforeResolvedParams, beforeResolvedParamsErr := s.appleBeforeResolveDirectives(paramContext, rundata, ctx); beforeResolvedParamsErr != nil {
			fe := rundata.AddFieldError(s.fieldPlan.GetFieldId(), FieldErrorTypeField, beforeResolvedParamsErr, s.fieldPlan.GetPaths())
			return fe
		} else {
			paramContext.params = beforeResolvedParams
		}

		//方法调用
		resolverFunc := s.fieldPlan.GetResolverFunc()
		if resolverFunc == nil {
			fe := rundata.AddFieldError(s.fieldPlan.GetFieldId(), FieldErrorTypeField, errors.New("resolver is nil"), s.fieldPlan.GetPaths())
			return fe
		}
		res, err := resolverFunc(nil, paramContext.params, ctx)
		if err != nil {
			fe := rundata.AddFieldError(s.fieldPlan.GetFieldId(), FieldErrorTypeField, err, s.fieldPlan.GetPaths())
			return fe
		}

		//执行AfterResolve指令
		afterResolvedValue, afterResolvedValueErr := s.appleAfterResolveDirectives(paramContext, res, rundata, ctx)
		if afterResolvedValueErr != nil {
			fe := rundata.AddFieldError(s.fieldPlan.GetFieldId(), FieldErrorTypeField, afterResolvedValueErr, s.fieldPlan.GetPaths())
			return fe
		}
		res = afterResolvedValue

		// list 字段将返回的数组展开为多条 response，与 IteratorStep 的结果结构保持一致
		fieldValueMetaInfo := s.fieldPlan.GetFieldValueMetaInfo()
		if fieldValueMetaInfo.IsList {
			//处理null值冒泡特性
			bubbledResult, bubbledErr := HandleResponseForNullBubbling(fieldValueMetaInfo, res)
			//根据null值冒泡后的结果组装返回数据
			if bubbledResult != nil {
				fieldResponse.responses = append(fieldResponse.responses, bubbledResult...)
				rundata.SetFieldResult(s.fieldPlan.GetFieldId(), fieldResponse)
			}
			//根据null值冒泡后的错误组装错误信息
			if bubbledErr != nil {
				fe := rundata.AddFieldError(s.fieldPlan.GetFieldId(), FieldErrorTypeField, bubbledErr, s.fieldPlan.GetPaths())
				return fe
			}
		} else {
			//如果当前字段的返回值不是List
			//判断结果能否为空，若non-null但返回nil则报错
			nullBubbledResult, nullBubbledErr := HandleResponseForNullBubbling(fieldValueMetaInfo, res)
			if nullBubbledResult != nil {
				fieldResponse.responses = append(fieldResponse.responses, nullBubbledResult...)
				rundata.SetFieldResult(s.fieldPlan.GetFieldId(), fieldResponse)
			}

			if nullBubbledErr != nil {
				fe := rundata.AddFieldError(s.fieldPlan.GetFieldId(), FieldErrorTypeField, nullBubbledErr, s.fieldPlan.GetPaths())
				return fe
			}
		}
	}
	return nil
}

func (s *NormalStep) prepareParams(paramPlans []*build.ParamPlan, rundata *Rundata) (*NormalParamContext, error) {
	result := NormalParamContext{}
	params := make(map[string]any, len(paramPlans))
	for _, pp := range paramPlans {
		switch pp.GetParamType() {
		case build.ParamTypeConst:
			params[pp.GetParamKey()] = pp.GetConstValue()
		case build.ParamTypeInput:
			params[pp.GetParamKey()] = rundata.GetOriginalParamByKey(pp.GetInputName())
		case build.ParamTypeFieldResult:
			fieldResult := rundata.GetFieldResultByFieldId(pp.GetDependentFieldId())
			if fieldResult == nil || len(fieldResult.responses) == 0 {
				return nil, fmt.Errorf("dependent field %d has no result", pp.GetDependentFieldId())
			}
			params[pp.GetParamKey()] = build.GetValueByPath(fieldResult.responses[0], pp.GetFieldResultPaths())
		case build.ParamTypeFieldFullResult:
			fieldResult := rundata.GetFieldResultByFieldId(pp.GetDependentFieldId())
			result.parentResponse = fieldResult
		}
	}
	result.params = params

	return &result, nil
}

func (s *NormalStep) GetFieldPlan() *build.FieldPlan {
	return s.fieldPlan
}

func (s *NormalStep) evaluateDirectivesShouldExecuteField(paramContext *NormalParamContext, rundata *Rundata, ctx context.Context) (bool, error) {
	for _, dp := range s.fieldPlan.GetDirectivePlans() {
		if dp.Stage != build.DirectiveStageShouldExecute {
			continue
		}

		if dp.RuntimeHandler == nil {
			return false, fmt.Errorf("directive runtime handler not defined")
		}

		ok, err := dp.RuntimeHandler.ShouldExecute(s.fieldPlan, paramContext.params, paramContext.parentResponse, rundata.originalParams, ctx)
		if err != nil || !ok {
			return ok, err
		}
	}
	return true, nil
}

func (s *NormalStep) appleBeforeResolveDirectives(paramContext *NormalParamContext, rundata *Rundata, ctx context.Context) (map[string]any, error) {
	result := make(map[string]any, len(paramContext.params))
	maps.Copy(result, paramContext.params)
	for _, dp := range s.fieldPlan.GetDirectivePlans() {
		if dp.Stage == build.DirectiveStageBeforeResolve {
			var beforeResolveResult map[string]any
			var beforeResolveResultErr error
			if beforeResolveResult, beforeResolveResultErr = dp.RuntimeHandler.BeforeResolve(s.fieldPlan, result, paramContext.parentResponse, rundata.originalParams, ctx); beforeResolveResultErr != nil {
				return nil, beforeResolveResultErr
			}
			result = beforeResolveResult
		}
	}
	return result, nil
}

func (s *NormalStep) appleAfterResolveDirectives(paramContext *NormalParamContext, currentResponse any, rundata *Rundata, ctx context.Context) (any, error) {
	current := currentResponse
	for _, dp := range s.fieldPlan.GetDirectivePlans() {
		if dp.Stage == build.DirectiveStageAfterResolve {
			next, err := dp.RuntimeHandler.AfterResolve(s.fieldPlan, paramContext.params, paramContext.parentResponse, rundata.originalParams, ctx)
			if err != nil {
				return nil, err
			}
			current = next
		}
	}
	return current, nil
}

type IteratorStep struct {
	fieldPlan *build.FieldPlan
}

type IteratorParamContext struct {
	params         map[string]any
	parentResponse any
}

type ArrayParamContext struct {
	params             map[string]any
	parentResponseList []any
}

func (s *IteratorStep) Execute(rundata *Rundata, ctx context.Context) *FieldError {
	if s.fieldPlan != nil {
		//初始化结果对象
		fieldResponse := AcquireFieldResponse(FieldResponseTypeArray)
		fieldResponse.arrayParentKeyMap = make(map[any]any)
		fieldValueMetaInfo := s.fieldPlan.GetFieldValueMetaInfo()

		if arrayResolverFunc := s.fieldPlan.GetArrayResolverFunc(); arrayResolverFunc != nil {
			//获取批量模式参数
			arrParamsContext, arrParamsErr := s.prepareArrayParams(s.fieldPlan, s.fieldPlan.GetArrParamPlans(), rundata, ctx)
			if arrParamsContext == nil || arrParamsErr != nil {
				fe := rundata.AddFieldError(s.fieldPlan.GetFieldId(), FieldErrorTypeField, arrParamsErr, s.fieldPlan.GetPaths())
				return fe
			}

			if arrParamsErr != nil {
				fe := rundata.AddFieldError(s.fieldPlan.GetFieldId(), FieldErrorTypeField, arrParamsErr, s.fieldPlan.GetPaths())
				fe.message = arrParamsErr.Error()
				fe.errorType = FieldErrorTypeField
				return fe
			}

			res, err := arrayResolverFunc(nil, arrParamsContext.params, ctx)
			if err != nil {
				fe := rundata.AddFieldError(s.fieldPlan.GetFieldId(), FieldErrorTypeField, err, s.fieldPlan.GetPaths())
				return fe
			}

			nullBubbledRes, nullBubbledErr := HandleResponseForNullBubbling(fieldValueMetaInfo, res)
			if nullBubbledErr != nil {
				fe := rundata.AddFieldError(s.fieldPlan.GetFieldId(), FieldErrorTypeField, nullBubbledErr, s.fieldPlan.GetPaths())
				return fe
			}

			if nullBubbledRes != nil {
				for _, singleResVal := range nullBubbledRes {
					fieldResponse.responses = append(fieldResponse.responses, singleResVal)

					//在批量查询的结果中要写入当前结果和父节点数据之间的映射关系
					//子节点结果要包含父节点的映射关系数据
					if singleResMap, resMapOk := singleResVal.(map[string]any); resMapOk {
						//返回值是Object
						resultFieldKeyName := s.fieldPlan.GetResultParentKeyFieldName()
						compositeKey := build.GenerateCompositeKey([]string{resultFieldKeyName}, singleResMap)
						fieldResponse.BindParentResult(compositeKey, singleResMap)
					} else if singleResAsArr, resArrOk := singleResVal.([]any); resArrOk {
						//返回值是List，只支持一层嵌套，超过一层报错
						if len(singleResAsArr) > 0 {
							firstSingleResValInArr := singleResAsArr[0]
							if firstSingleResValInArrMap, firstSingleResValInArrMapOk := firstSingleResValInArr.(map[string]any); firstSingleResValInArrMapOk {
								fieldKeyName := s.fieldPlan.GetResultParentKeyFieldName()
								compositeKey := build.GenerateCompositeKey([]string{fieldKeyName}, firstSingleResValInArrMap)
								fieldResponse.BindParentResult(compositeKey, singleResAsArr)
							} else {
								fe := rundata.AddFieldError(s.fieldPlan.GetFieldId(), FieldErrorTypeField, fmt.Errorf("response data type not supported"), s.fieldPlan.GetPaths())
								return fe
							}
						}
					}
				}
			}
		} else {
			//获取遍历模式调用项（父节点响应 + 本次参数）
			callItems, paramErr := s.prepareIteratorParams(s.fieldPlan.GetParamPlans(), rundata)
			if paramErr != nil {
				fe := rundata.AddFieldError(s.fieldPlan.GetFieldId(), FieldErrorTypeField, paramErr, s.fieldPlan.GetPaths())
				return fe
			}

			resolverFunc := s.fieldPlan.GetResolverFunc()
			if resolverFunc == nil {
				fe := rundata.AddFieldError(s.fieldPlan.GetFieldId(), FieldErrorTypeField, errors.New("resolver is nil"), s.fieldPlan.GetPaths())
				return fe
			}

			for _, item := range callItems {
				//根据指令结果评估是否执行
				var shouldExecute bool
				var directiveEvaluateErr error
				shouldExecute, directiveEvaluateErr = s.evaluateDirectivesShouldExecuteField(item, rundata, ctx)
				if directiveEvaluateErr != nil {
					fe := rundata.AddFieldError(s.fieldPlan.GetFieldId(), FieldErrorTypeField, directiveEvaluateErr, s.fieldPlan.GetPaths())
					return fe
				}
				if !shouldExecute {
					continue
				}
				//动态类型判定，interface和union type特性
				if isFieldPlanCompiledType(s.fieldPlan) {
					shouldExecute = shouldExecuteFieldAsCompiledType(s.fieldPlan, &ctx)
				} else {
					parentFullResult := item.parentResponse
					if parentFullResult == nil {
						return nil
					}
					shouldExecute = shouldExecuteFieldAsRuntimeType(s.fieldPlan, parentFullResult, &ctx)
				}
				if !shouldExecute {
					continue
				}

				//__typename
				if s.fieldPlan.IsIntrospectionTypeNameField() {
					if s.fieldPlan.GetRuntimeTypeResolverFunc() != nil {
						typeName := s.fieldPlan.GetRuntimeTypeResolverFunc()(item.parentResponse, &ctx)
						if typeName == "" {
							fe := rundata.AddFieldError(s.fieldPlan.GetFieldId(), FieldErrorTypeField, errors.New("__typename resolved failed, value is empty"), s.fieldPlan.GetPaths())
							return fe
						}
						fieldResponse.responses = append(fieldResponse.responses, typeName)
						continue
					}
				}

				//执行BeforeResolve类型的directives
				beforeResolvedParams, beforeResolvedParamsErr := s.appleBeforeResolveDirectives(item, rundata, ctx)
				if beforeResolvedParamsErr != nil {
					fe := rundata.AddFieldError(s.fieldPlan.GetFieldId(), FieldErrorTypeField, beforeResolvedParamsErr, s.fieldPlan.GetPaths())
					return fe
				}
				item.params = beforeResolvedParams

				res, err := resolverFunc(nil, item.params, ctx)
				if err != nil {
					fe := rundata.AddFieldError(s.fieldPlan.GetFieldId(), FieldErrorTypeField, err, s.fieldPlan.GetPaths())
					return fe
				}

				//执行AfterResolve类型的directives
				afterResolvedValue, afterResolvedParamsErr := s.appleAfterResolveDirectives(item, res, rundata, ctx)
				if afterResolvedParamsErr != nil {
					fe := rundata.AddFieldError(s.fieldPlan.GetFieldId(), FieldErrorTypeField, afterResolvedParamsErr, s.fieldPlan.GetPaths())
					return fe
				}
				res = afterResolvedValue

				//调用Null值冒泡方法获取结果
				nullBubbledRes, nullBubbledErr := HandleResponseForNullBubbling(fieldValueMetaInfo, res)

				//如果冒泡后的结果有错误，返回错误
				if nullBubbledErr != nil {
					fe := rundata.AddFieldError(s.fieldPlan.GetFieldId(), FieldErrorTypeField, nullBubbledErr, s.fieldPlan.GetPaths())
					return fe
				}

				//将冒泡结果作为最终结果
				fieldResponse.responses = append(fieldResponse.responses, nullBubbledRes)

				//在批量查询的结果中要写入当前结果和父节点数据之间的映射关系
				compositeKey := ""
				if parentMap, parentOk := item.parentResponse.(map[string]any); parentOk {
					compositeKey = build.GenerateCompositeKey([]string{s.fieldPlan.GetParentKeyFieldName()}, parentMap)
				}
				fieldResponse.BindParentResult(compositeKey, nullBubbledRes)
			}
		}
		//Rundata写入数据
		rundata.SetFieldResult(s.fieldPlan.GetFieldId(), fieldResponse)
	}
	return nil
}

func (s *IteratorStep) prepareIteratorParams(paramPlans []*build.ParamPlan, rundata *Rundata) ([]IteratorParamContext, error) {
	parentResult := rundata.GetFieldResultByFieldId(s.fieldPlan.GetParentFieldId())
	if parentResult == nil {
		return nil, fmt.Errorf("parent field %d has no result", s.fieldPlan.GetParentFieldId())
	}
	result := make([]IteratorParamContext, 0, len(parentResult.responses))
	for _, parentResponse := range parentResult.responses {
		params := make(map[string]any, len(paramPlans))
		for _, pp := range paramPlans {
			switch pp.GetParamType() {
			case build.ParamTypeConst:
				params[pp.GetParamKey()] = pp.GetConstValue()
			case build.ParamTypeInput:
				params[pp.GetParamKey()] = rundata.GetOriginalParamByKey(pp.GetInputName())
			case build.ParamTypeFieldResult:
				params[pp.GetParamKey()] = build.GetValueByPath(parentResponse, pp.GetFieldResultPaths())
			case build.ParamTypeFieldFullResult:
				//轮空
			}
		}
		result = append(result, IteratorParamContext{
			parentResponse: parentResponse,
			params:         params,
		})
	}
	return result, nil
}

func (s *IteratorStep) prepareArrayParams(fieldPlan *build.FieldPlan, arrParamPlans []*build.ParamPlan, rundata *Rundata, ctx context.Context) (*ArrayParamContext, error) {
	result := ArrayParamContext{}
	params := make(map[string]any)
	if len(arrParamPlans) == 0 {
		return &result, nil
	}
	for _, arrParamPlan := range arrParamPlans {
		switch arrParamPlan.GetParamType() {
		case build.ParamTypeConst:
			params[arrParamPlan.GetParamKey()] = arrParamPlan.GetConstValue()
		case build.ParamTypeInput:
			params[arrParamPlan.GetParamKey()] = rundata.GetOriginalParamByKey(arrParamPlan.GetInputName())
		case build.ParamTypeFieldResult:
			// 遍历父节点结果集，提取每个元素中的目标字段值，组装成切片
			parentResult := rundata.GetFieldResultByFieldId(arrParamPlan.GetDependentFieldId())
			if parentResult == nil {
				return nil, fmt.Errorf("dependent field %d has no result", arrParamPlan.GetDependentFieldId())
			}
			batchValues := make([]any, 0, len(parentResult.responses))
			for _, parentResponse := range parentResult.responses {
				//动态类型判定，interface和union type特性
				//批量模式下，动态类型判定如果不通过跳过当前的元素
				var shouldExecute bool
				if isFieldPlanCompiledType(s.fieldPlan) {
					shouldExecute = shouldExecuteFieldAsCompiledType(s.fieldPlan, &ctx)
				} else {
					if parentResponse == nil {
						continue
					}
					shouldExecute = shouldExecuteFieldAsRuntimeType(s.fieldPlan, parentResponse, &ctx)
				}
				if !shouldExecute {
					continue
				}
				//组装参数
				batchValues = append(batchValues, build.GetValueByPath(parentResponse, arrParamPlan.GetFieldResultPaths()))
			}
			params[arrParamPlan.GetParamKey()] = batchValues
		case build.ParamTypeFieldFullResult:
			// 取父节点结果集中的第一个作为完整的结果抽样返回
			parentResult := rundata.GetFieldResultByFieldId(arrParamPlan.GetDependentFieldId())
			if parentResult == nil {
				return nil, fmt.Errorf("parent field full result is nil. dependent field %d has no result", arrParamPlan.GetDependentFieldId())
			}
			result.parentResponseList = parentResult.responses
		}
	}
	result.params = params
	return &result, nil
}

func (s *IteratorStep) GetFieldPlan() *build.FieldPlan {
	return s.fieldPlan
}

func (s *IteratorStep) evaluateDirectivesShouldExecuteField(paramContext IteratorParamContext, rundata *Rundata, ctx context.Context) (bool, error) {
	for _, dp := range s.fieldPlan.GetDirectivePlans() {
		if dp.Stage != build.DirectiveStageShouldExecute {
			continue
		}

		if dp.RuntimeHandler == nil {
			return false, fmt.Errorf("directive runtime handler not defined")
		}

		ok, err := dp.RuntimeHandler.ShouldExecute(s.fieldPlan, paramContext.params, paramContext.parentResponse, rundata.originalParams, ctx)
		if err != nil || !ok {
			return ok, err
		}
	}
	return true, nil
}

func (s *IteratorStep) appleBeforeResolveDirectives(paramContext IteratorParamContext, rundata *Rundata, ctx context.Context) (map[string]any, error) {
	result := make(map[string]any, len(paramContext.params))
	maps.Copy(result, paramContext.params)
	for _, dp := range s.fieldPlan.GetDirectivePlans() {
		if dp.Stage == build.DirectiveStageBeforeResolve {
			var beforeResolveResult map[string]any
			var beforeResolveResultErr error
			if beforeResolveResult, beforeResolveResultErr = dp.RuntimeHandler.BeforeResolve(s.fieldPlan, result, paramContext.parentResponse, rundata.originalParams, ctx); beforeResolveResultErr != nil {
				return nil, beforeResolveResultErr
			}
			result = beforeResolveResult
		}
	}
	return result, nil
}

func (s *IteratorStep) appleAfterResolveDirectives(paramContext IteratorParamContext, currentResponse any, rundata *Rundata, ctx context.Context) (any, error) {
	current := currentResponse
	for _, dp := range s.fieldPlan.GetDirectivePlans() {
		if dp.Stage == build.DirectiveStageAfterResolve {
			next, err := dp.RuntimeHandler.AfterResolve(s.fieldPlan, paramContext.params, paramContext.parentResponse, rundata.originalParams, ctx)
			if err != nil {
				return nil, err
			}
			current = next
		}
	}
	return current, nil
}

func isFieldPlanCompiledType(fp *build.FieldPlan) bool {
	if fp.GetRuntimeTypeResolverFunc() == nil {
		return true
	}
	return false
}

func shouldExecuteFieldAsCompiledType(fp *build.FieldPlan, ctx *context.Context) bool {
	return fp.GetAllowedRuntimeTypeNames()[fp.GetCompiledTypeName()]
}

func shouldExecuteFieldAsRuntimeType(fp *build.FieldPlan, parentData any, ctx *context.Context) bool {
	if fp == nil || len(fp.GetAllowedRuntimeTypeNames()) == 0 {
		return true
	}
	if fp.GetRuntimeTypeResolverFunc() != nil {
		typeName := fp.GetRuntimeTypeResolverFunc()(parentData, ctx)
		if typeName == "" {
			return false
		}
		return fp.GetAllowedRuntimeTypeNames()[typeName]
	}

	return true
}

// [[object!]!]!返回true代表要中断返回值组装
func HandleResponseForNullBubbling(listMeta build.FieldValueMetaInfo, fieldResponse any) ([]any, error) {
	//入参检查
	if fieldResponse == nil && listMeta.NotNil {
		return nil, fmt.Errorf("non-null list response is nil")
	}
	if listMeta.IsList {
		listVal, ok := fieldResponse.([]any)
		if !ok {
			return nil, fmt.Errorf("list value is not a list")
		}
		childMeta := listMeta.ElementType
		if childMeta == nil {
			return nil, fmt.Errorf("list value has no child metadata")
		}
		if childMeta.IsList {
			//如果是List嵌套List，递归调用
			for _, childList := range listVal {
				childResult, err := HandleResponseForNullBubbling(*childMeta, childList)
				//如果子list要求non-null，但是有元素是nil，整个list返回nil
				if childResult == nil && childMeta.NotNil {
					if err == nil {
						err = fmt.Errorf("list value has no child metadata")
					}
					return nil, err
				}
			}

			//返回原始信息到上层
			return listVal, nil
		}

		//如果子元素不是List，按照子元素的信息做判断
		for _, child := range listVal {
			//如果要求元素non-null但是元素有null，整体返回nil
			if childMeta.NotNil && child == nil {
				return nil, fmt.Errorf("list value has no child metadata")
			}
		}
		//如果允许元素null，则直接返回原始元素
		return listVal, nil
	}
	if listMeta.NotNil && fieldResponse == nil {
		return nil, fmt.Errorf("non-null list response is nil")
	}
	return []any{fieldResponse}, nil
}
