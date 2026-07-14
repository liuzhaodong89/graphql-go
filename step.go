package graphql

import (
	"context"
	"errors"
	"fmt"
	"maps"
)

type Step interface {
	Execute(rundata *Rundata, ctx context.Context) *FieldError
	GetFieldPlan() *FieldPlan
}

type NormalParamContext struct {
	params         map[string]any
	parentResponse any
}

type NormalStep struct {
	fieldPlan *FieldPlan
}

func (s *NormalStep) Execute(rundata *Rundata, ctx context.Context) *FieldError {
	if s.fieldPlan != nil {
		// 标准 @skip/@include 只依赖本次请求变量或字面量，先判断可以避免无意义的 resolver 调用。
		included, includeErr := shouldIncludeField(s.fieldPlan, rundata, ctx)
		if includeErr != nil {
			return rundata.AddFieldError(s.fieldPlan.GetFieldId(), FieldErrorTypeField, includeErr, s.fieldPlan.GetPaths())
		}
		if !included {
			return nil
		}

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
				// __typename 也是一次字段解析，动态类型 resolver 前后同样触发 field extension hook。
				fieldCtx, finishHook := startSGraphResolveFieldHook(rundata, s.fieldPlan, ctx, -1)
				typeName := s.fieldPlan.GetRuntimeTypeResolverFunc()(paramContext.parentResponse, &fieldCtx)
				if typeName == "" {
					err := errors.New("__typename resolved failed, value is empty")
					finishSGraphResolveFieldHook(rundata, finishHook, nil, err)
					fe := rundata.AddFieldError(s.fieldPlan.GetFieldId(), FieldErrorTypeField, err, s.fieldPlan.GetPaths())
					ReleaseFieldResponse(fieldResponse)
					return fe
				}
				finishSGraphResolveFieldHook(rundata, finishHook, typeName, nil)
				fieldResponse.responses = append(fieldResponse.responses, typeName)
				rundata.SetFieldResult(s.fieldPlan.GetFieldId(), fieldResponse)
				return nil
			}
		}

		//执行BeforeResolve指令
		if beforeResolvedParams, beforeResolvedParamsErr := s.appleBeforeResolveDirectives(paramContext, rundata, ctx); beforeResolvedParamsErr != nil {
			fe := rundata.AddFieldError(s.fieldPlan.GetFieldId(), FieldErrorTypeField, beforeResolvedParamsErr, s.fieldPlan.GetPaths())
			ReleaseFieldResponse(fieldResponse)
			return fe
		} else {
			paramContext.params = beforeResolvedParams
		}

		//方法调用
		resolverFunc := s.fieldPlan.GetResolverFunc()
		if resolverFunc == nil {
			fe := rundata.AddFieldError(s.fieldPlan.GetFieldId(), FieldErrorTypeField, errors.New("resolver is nil"), s.fieldPlan.GetPaths())
			ReleaseFieldResponse(fieldResponse)
			return fe
		}
		// 显式 resolver 的 source 保持 nil，执行关系仍由参数依赖决定；ctx 只负责透传 ResolveInfo/extension 信息。
		fieldCtx, finishHook := startSGraphResolveFieldHook(rundata, s.fieldPlan, ctx, -1)
		res, err := resolverFunc(nil, paramContext.params, fieldCtx)
		finishSGraphResolveFieldHook(rundata, finishHook, res, err)
		if err != nil {
			fe := rundata.AddFieldError(s.fieldPlan.GetFieldId(), FieldErrorTypeField, err, s.fieldPlan.GetPaths())
			ReleaseFieldResponse(fieldResponse)
			return fe
		}

		//执行AfterResolve指令
		afterResolvedValue, afterResolvedValueErr := s.appleAfterResolveDirectives(paramContext, res, rundata, ctx)
		if afterResolvedValueErr != nil {
			fe := rundata.AddFieldError(s.fieldPlan.GetFieldId(), FieldErrorTypeField, afterResolvedValueErr, s.fieldPlan.GetPaths())
			ReleaseFieldResponse(fieldResponse)
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
				if bubbledResult == nil {
					ReleaseFieldResponse(fieldResponse)
				}
				return fe
			}
			if bubbledResult == nil {
				// 可空 list 的 nil 结果不写入 Rundata，当前 step 结束时即可归还结果对象。
				ReleaseFieldResponse(fieldResponse)
			}
			return nil
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
				if nullBubbledResult == nil {
					ReleaseFieldResponse(fieldResponse)
				}
				return fe
			}
			if nullBubbledResult == nil {
				ReleaseFieldResponse(fieldResponse)
			}
			return nil
		}
	}
	return nil
}

func (s *NormalStep) prepareParams(paramPlans []*ParamPlan, rundata *Rundata) (*NormalParamContext, error) {
	result := NormalParamContext{}
	params := make(map[string]any, len(paramPlans))
	for _, pp := range paramPlans {
		//仅依赖变量/常量：统一交 ParamPlan 解析（Const/Input/Template）
		if v, handled, err := pp.ResolveFromInputs(rundata.originalParams); handled {
			if err != nil {
				return nil, err
			}
			params[pp.GetParamKey()] = v
			continue
		}
		//依赖字段结果：按本 step 形态取
		switch pp.GetParamType() {
		case ParamTypeFieldResult:
			fieldResult := rundata.GetFieldResultByFieldId(pp.GetDependentFieldId())
			if fieldResult == nil || len(fieldResult.responses) == 0 {
				return nil, fmt.Errorf("dependent field %d has no result", pp.GetDependentFieldId())
			}
			params[pp.GetParamKey()] = GetValueByPath(fieldResult.responses[0], pp.GetFieldResultPaths())
		case ParamTypeFieldFullResult:
			fieldResult := rundata.GetFieldResultByFieldId(pp.GetDependentFieldId())
			result.parentResponse = fieldResult
		}
	}
	result.params = params

	return &result, nil
}

func (s *NormalStep) GetFieldPlan() *FieldPlan {
	return s.fieldPlan
}

func (s *NormalStep) evaluateDirectivesShouldExecuteField(paramContext *NormalParamContext, rundata *Rundata, ctx context.Context) (bool, error) {
	for _, dp := range s.fieldPlan.GetDirectivePlans() {
		if dp.Stage != DirectiveStageShouldExecute {
			continue
		}

		if dp.RuntimeHandler == nil {
			return false, fmt.Errorf("directive runtime handler not defined")
		}

		directiveArgs, argsErr := materializeDirectiveArgs(dp, rundata)
		if argsErr != nil {
			return false, argsErr
		}
		ok, err := dp.RuntimeHandler.ShouldExecute(s.fieldPlan, directiveArgs, paramContext.params, paramContext.parentResponse, rundata.originalParams, ctx)
		if err != nil || !ok {
			return ok, err
		}
	}
	return true, nil
}

func (s *NormalStep) appleBeforeResolveDirectives(paramContext *NormalParamContext, rundata *Rundata, ctx context.Context) (map[string]any, error) {
	var result map[string]any
	for _, dp := range s.fieldPlan.GetDirectivePlans() {
		if dp.Stage == DirectiveStageBeforeResolve {
			if result == nil {
				// BeforeResolve 可能修改 resolver 参数；只有实际存在该阶段指令时才复制，普通字段直接复用原参数 map。
				result = make(map[string]any, len(paramContext.params))
				maps.Copy(result, paramContext.params)
			}
			directiveArgs, argsErr := materializeDirectiveArgs(dp, rundata)
			if argsErr != nil {
				return nil, argsErr
			}
			beforeResolveResult, beforeResolveResultErr := dp.RuntimeHandler.BeforeResolve(s.fieldPlan, directiveArgs, result, paramContext.parentResponse, rundata.originalParams, ctx)
			if beforeResolveResultErr != nil {
				return nil, beforeResolveResultErr
			}
			result = beforeResolveResult
		}
	}
	if result == nil {
		return paramContext.params, nil
	}
	return result, nil
}

func (s *NormalStep) appleAfterResolveDirectives(paramContext *NormalParamContext, currentResponse any, rundata *Rundata, ctx context.Context) (any, error) {
	current := currentResponse
	for _, dp := range s.fieldPlan.GetDirectivePlans() {
		if dp.Stage == DirectiveStageAfterResolve {
			directiveArgs, argsErr := materializeDirectiveArgs(dp, rundata)
			if argsErr != nil {
				return nil, argsErr
			}
			next, err := dp.RuntimeHandler.AfterResolve(s.fieldPlan, directiveArgs, paramContext.params, paramContext.parentResponse, current, rundata.originalParams, ctx)
			if err != nil {
				return nil, err
			}
			current = next
		}
	}
	return current, nil
}

// materializeDirectiveArgs 用本请求变量物化指令参数（指令参数只依赖变量/常量）。
func materializeDirectiveArgs(dp *DirectivePlan, rundata *Rundata) (map[string]any, error) {
	argPlans := dp.GetArgPlans()
	if len(argPlans) == 0 {
		return nil, nil
	}
	out := make(map[string]any, len(argPlans))
	for _, pp := range argPlans {
		v, handled, err := pp.ResolveFromInputs(rundata.originalParams)
		if err != nil {
			return nil, err
		}
		if !handled {
			return nil, fmt.Errorf("directive arg %q has unsupported dependency type", pp.GetParamKey())
		}
		out[pp.GetParamKey()] = v
	}
	return out, nil
}

func shouldIncludeField(fieldPlan *FieldPlan, rundata *Rundata, ctx context.Context) (bool, error) {
	if fieldPlan == nil {
		return false, nil
	}
	groups := fieldPlan.GetConditionalDirectiveGroups()
	if len(groups) == 0 {
		return true, nil
	}

	for _, group := range groups {
		// 重复 responseName 的多个 occurrence 按 OR 语义处理；单个 occurrence 内的条件按 AND 语义处理。
		groupIncluded := true
		for _, dp := range group {
			if dp == nil {
				continue
			}
			if dp.RuntimeHandler == nil {
				return false, fmt.Errorf("directive runtime handler not defined")
			}
			directiveArgs, argsErr := materializeDirectiveArgs(dp, rundata)
			if argsErr != nil {
				return false, argsErr
			}
			ok, err := dp.RuntimeHandler.ShouldExecute(fieldPlan, directiveArgs, nil, nil, rundata.originalParams, ctx)
			if err != nil {
				return false, err
			}
			if !ok {
				groupIncluded = false
				break
			}
		}
		if groupIncluded {
			return true, nil
		}
	}
	return false, nil
}

type IteratorStep struct {
	fieldPlan *FieldPlan
}

type IteratorParamContext struct {
	params         map[string]any
	parentResponse any
	index          int
}

type ArrayParamContext struct {
	params             map[string]any
	parentResponseList []any
}

func (s *IteratorStep) Execute(rundata *Rundata, ctx context.Context) *FieldError {
	if s.fieldPlan != nil {
		// 字段整体被 @skip/@include 排除时，不创建 FieldResponse，组装阶段也会省略该字段。
		included, includeErr := shouldIncludeField(s.fieldPlan, rundata, ctx)
		if includeErr != nil {
			return rundata.AddFieldError(s.fieldPlan.GetFieldId(), FieldErrorTypeField, includeErr, s.fieldPlan.GetPaths())
		}
		if !included {
			return nil
		}

		//初始化结果对象
		fieldResponse := AcquireFieldResponse(FieldResponseTypeArray)
		fieldValueMetaInfo := s.fieldPlan.GetFieldValueMetaInfo()

		if arrayResolverFunc := s.fieldPlan.GetArrayResolverFunc(); arrayResolverFunc != nil {
			//获取批量模式参数
			arrParamsContext, arrParamsErr := s.prepareArrayParams(s.fieldPlan, s.fieldPlan.GetArrParamPlans(), rundata, ctx)
			if arrParamsContext == nil || arrParamsErr != nil {
				if arrParamsErr == nil {
					arrParamsErr = errors.New("array params context is nil")
				}
				fe := rundata.AddFieldError(s.fieldPlan.GetFieldId(), FieldErrorTypeField, arrParamsErr, s.fieldPlan.GetPaths())
				ReleaseFieldResponse(fieldResponse)
				return fe
			}

			// BatchResolve 对应 GraphQL 语义上的一次字段解析，hook 包住整次批量调用。
			fieldCtx, finishHook := startSGraphResolveFieldHook(rundata, s.fieldPlan, ctx, -1)
			res, err := arrayResolverFunc(nil, arrParamsContext.params, fieldCtx)
			finishSGraphResolveFieldHook(rundata, finishHook, res, err)
			if err != nil {
				fe := rundata.AddFieldError(s.fieldPlan.GetFieldId(), FieldErrorTypeField, err, s.fieldPlan.GetPaths())
				ReleaseFieldResponse(fieldResponse)
				return fe
			}

			nullBubbledRes, nullBubbledErr := HandleResponseForNullBubbling(fieldValueMetaInfo, res)
			if nullBubbledErr != nil {
				fe := rundata.AddFieldError(s.fieldPlan.GetFieldId(), FieldErrorTypeField, nullBubbledErr, s.fieldPlan.GetPaths())
				ReleaseFieldResponse(fieldResponse)
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
						compositeKey := GenerateCompositeKey([]string{resultFieldKeyName}, singleResMap)
						// BatchResolve 的 list 字段允许一个父 key 对应多个子结果，同 key 必须追加而不是覆盖。
						if fieldValueMetaInfo.IsList {
							if existing, exists := fieldResponse.LookupParentResult(compositeKey); exists {
								if existingList, ok := existing.([]any); ok {
									fieldResponse.BindParentResult(compositeKey, append(existingList, singleResMap))
								} else {
									fieldResponse.BindParentResult(compositeKey, []any{existing, singleResMap})
								}
							} else {
								fieldResponse.BindParentResult(compositeKey, []any{singleResMap})
							}
						} else {
							fieldResponse.BindParentResult(compositeKey, singleResMap)
						}
					} else if singleResAsArr, resArrOk := singleResVal.([]any); resArrOk {
						//返回值是List，只支持一层嵌套，超过一层报错
						if len(singleResAsArr) > 0 {
							firstSingleResValInArr := singleResAsArr[0]
							if firstSingleResValInArrMap, firstSingleResValInArrMapOk := firstSingleResValInArr.(map[string]any); firstSingleResValInArrMapOk {
								fieldKeyName := s.fieldPlan.GetResultParentKeyFieldName()
								compositeKey := GenerateCompositeKey([]string{fieldKeyName}, firstSingleResValInArrMap)
								// 已分组的 batch 结果也按同一个 composite key 追加，兼容 flat list 和 grouped list 两种返回形态。
								if fieldValueMetaInfo.IsList {
									if existing, exists := fieldResponse.LookupParentResult(compositeKey); exists {
										if existingList, ok := existing.([]any); ok {
											fieldResponse.BindParentResult(compositeKey, append(existingList, singleResAsArr...))
										} else {
											merged := []any{existing}
											merged = append(merged, singleResAsArr...)
											fieldResponse.BindParentResult(compositeKey, merged)
										}
									} else {
										fieldResponse.BindParentResult(compositeKey, singleResAsArr)
									}
								} else {
									fieldResponse.BindParentResult(compositeKey, singleResAsArr)
								}
							} else {
								fe := rundata.AddFieldError(s.fieldPlan.GetFieldId(), FieldErrorTypeField, fmt.Errorf("response data type not supported"), s.fieldPlan.GetPaths())
								ReleaseFieldResponse(fieldResponse)
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
				ReleaseFieldResponse(fieldResponse)
				return fe
			}

			resolverFunc := s.fieldPlan.GetResolverFunc()
			if resolverFunc == nil {
				fe := rundata.AddFieldError(s.fieldPlan.GetFieldId(), FieldErrorTypeField, errors.New("resolver is nil"), s.fieldPlan.GetPaths())
				ReleaseFieldResponse(fieldResponse)
				return fe
			}

			for _, item := range callItems {
				//根据指令结果评估是否执行
				var shouldExecute bool
				var directiveEvaluateErr error
				shouldExecute, directiveEvaluateErr = s.evaluateDirectivesShouldExecuteField(item, rundata, ctx)
				if directiveEvaluateErr != nil {
					fe := rundata.AddFieldError(s.fieldPlan.GetFieldId(), FieldErrorTypeField, directiveEvaluateErr, s.fieldPlan.GetPaths())
					ReleaseFieldResponse(fieldResponse)
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
						ReleaseFieldResponse(fieldResponse)
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
						// list item 下的 __typename 需要带上 item.index，extension path 才能定位到具体元素。
						fieldCtx, finishHook := startSGraphResolveFieldHook(rundata, s.fieldPlan, ctx, item.index)
						typeName := s.fieldPlan.GetRuntimeTypeResolverFunc()(item.parentResponse, &fieldCtx)
						if typeName == "" {
							err := errors.New("__typename resolved failed, value is empty")
							finishSGraphResolveFieldHook(rundata, finishHook, nil, err)
							fe := rundata.AddFieldError(s.fieldPlan.GetFieldId(), FieldErrorTypeField, err, s.fieldPlan.GetPaths())
							ReleaseFieldResponse(fieldResponse)
							return fe
						}
						finishSGraphResolveFieldHook(rundata, finishHook, typeName, nil)
						fieldResponse.responses = append(fieldResponse.responses, typeName)
						// list 父节点下的 __typename 也要按 parent composite key 回填，避免组装阶段误用空 binding 导致 non-null 冒泡。
						compositeKey := ""
						if parentMap, parentOk := item.parentResponse.(map[string]any); parentOk {
							compositeKey = GenerateCompositeKey([]string{s.fieldPlan.GetParentKeyFieldName()}, parentMap)
						}
						fieldResponse.BindParentResult(compositeKey, typeName)
						continue
					}
				}

				//执行BeforeResolve类型的directives
				beforeResolvedParams, beforeResolvedParamsErr := s.applyBeforeResolveDirectives(item, rundata, ctx)
				if beforeResolvedParamsErr != nil {
					fe := rundata.AddFieldError(s.fieldPlan.GetFieldId(), FieldErrorTypeField, beforeResolvedParamsErr, s.fieldPlan.GetPaths())
					ReleaseFieldResponse(fieldResponse)
					return fe
				}
				item.params = beforeResolvedParams

				// 遍历模式下每个父元素都是一次字段解析，hook path 使用当前 item.index。
				fieldCtx, finishHook := startSGraphResolveFieldHook(rundata, s.fieldPlan, ctx, item.index)
				res, err := resolverFunc(nil, item.params, fieldCtx)
				finishSGraphResolveFieldHook(rundata, finishHook, res, err)
				if err != nil {
					fe := rundata.AddFieldError(s.fieldPlan.GetFieldId(), FieldErrorTypeField, err, s.fieldPlan.GetPaths())
					ReleaseFieldResponse(fieldResponse)
					return fe
				}

				//执行AfterResolve类型的directives
				afterResolvedValue, afterResolvedParamsErr := s.applyAfterResolveDirectives(item, res, rundata, ctx)
				if afterResolvedParamsErr != nil {
					fe := rundata.AddFieldError(s.fieldPlan.GetFieldId(), FieldErrorTypeField, afterResolvedParamsErr, s.fieldPlan.GetPaths())
					ReleaseFieldResponse(fieldResponse)
					return fe
				}
				res = afterResolvedValue

				//调用Null值冒泡方法获取结果
				nullBubbledRes, nullBubbledErr := HandleResponseForNullBubbling(fieldValueMetaInfo, res)

				//如果冒泡后的结果有错误，返回错误
				if nullBubbledErr != nil {
					fe := rundata.AddFieldError(s.fieldPlan.GetFieldId(), FieldErrorTypeField, nullBubbledErr, s.fieldPlan.GetPaths())
					ReleaseFieldResponse(fieldResponse)
					return fe
				}

				// HandleResponseForNullBubbling 统一返回 []any：
				// list 字段保留整个切片作为当前父元素的子结果；非 list 字段只取唯一完成值。
				completedValue := any(nil)
				if fieldValueMetaInfo.IsList {
					completedValue = nullBubbledRes
				} else if len(nullBubbledRes) > 0 {
					completedValue = nullBubbledRes[0]
				}
				fieldResponse.responses = append(fieldResponse.responses, completedValue)

				//在批量查询的结果中要写入当前结果和父节点数据之间的映射关系
				compositeKey := ""
				if parentMap, parentOk := item.parentResponse.(map[string]any); parentOk {
					compositeKey = GenerateCompositeKey([]string{s.fieldPlan.GetParentKeyFieldName()}, parentMap)
				}
				fieldResponse.BindParentResult(compositeKey, completedValue)
			}
		}
		//Rundata写入数据
		rundata.SetFieldResult(s.fieldPlan.GetFieldId(), fieldResponse)
	}
	return nil
}

func (s *IteratorStep) prepareIteratorParams(paramPlans []*ParamPlan, rundata *Rundata) ([]IteratorParamContext, error) {
	parentResult := rundata.GetFieldResultByFieldId(s.fieldPlan.GetParentFieldId())
	if parentResult == nil {
		return nil, fmt.Errorf("parent field %d has no result", s.fieldPlan.GetParentFieldId())
	}
	result := make([]IteratorParamContext, 0, len(parentResult.responses))
	for index, parentResponse := range parentResult.responses {
		params := make(map[string]any, len(paramPlans))
		for _, pp := range paramPlans {
			//仅依赖变量/常量（对每个元素同值）
			if v, handled, err := pp.ResolveFromInputs(rundata.originalParams); handled {
				if err != nil {
					return nil, err
				}
				params[pp.GetParamKey()] = v
				continue
			}
			//依赖字段结果：遍历模式下取自当前父元素
			switch pp.GetParamType() {
			case ParamTypeFieldResult:
				params[pp.GetParamKey()] = GetValueByPath(parentResponse, pp.GetFieldResultPaths())
			case ParamTypeFieldFullResult:
				//轮空
			}
		}
		result = append(result, IteratorParamContext{
			parentResponse: parentResponse,
			params:         params,
			// index 只用于 response path / extension 信息，不参与 resolver 参数依赖和 batch 分层。
			index: index,
		})
	}
	return result, nil
}

func (s *IteratorStep) prepareArrayParams(fieldPlan *FieldPlan, arrParamPlans []*ParamPlan, rundata *Rundata, ctx context.Context) (*ArrayParamContext, error) {
	result := ArrayParamContext{}
	params := make(map[string]any)
	if len(arrParamPlans) == 0 {
		return &result, nil
	}
	for _, arrParamPlan := range arrParamPlans {
		//仅依赖变量/常量
		if v, handled, err := arrParamPlan.ResolveFromInputs(rundata.originalParams); handled {
			if err != nil {
				return nil, err
			}
			params[arrParamPlan.GetParamKey()] = v
			continue
		}
		switch arrParamPlan.GetParamType() {
		case ParamTypeFieldResult:
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
				batchValues = append(batchValues, GetValueByPath(parentResponse, arrParamPlan.GetFieldResultPaths()))
			}
			params[arrParamPlan.GetParamKey()] = batchValues
		case ParamTypeFieldFullResult:
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

func (s *IteratorStep) GetFieldPlan() *FieldPlan {
	return s.fieldPlan
}

func (s *IteratorStep) evaluateDirectivesShouldExecuteField(paramContext IteratorParamContext, rundata *Rundata, ctx context.Context) (bool, error) {
	for _, dp := range s.fieldPlan.GetDirectivePlans() {
		if dp.Stage != DirectiveStageShouldExecute {
			continue
		}

		if dp.RuntimeHandler == nil {
			return false, fmt.Errorf("directive runtime handler not defined")
		}

		directiveArgs, argsErr := materializeDirectiveArgs(dp, rundata)
		if argsErr != nil {
			return false, argsErr
		}
		ok, err := dp.RuntimeHandler.ShouldExecute(s.fieldPlan, directiveArgs, paramContext.params, paramContext.parentResponse, rundata.originalParams, ctx)
		if err != nil || !ok {
			return ok, err
		}
	}
	return true, nil
}

func (s *IteratorStep) applyBeforeResolveDirectives(paramContext IteratorParamContext, rundata *Rundata, ctx context.Context) (map[string]any, error) {
	result := paramContext.params
	copied := false
	for _, dp := range s.fieldPlan.GetDirectivePlans() {
		if dp.Stage == DirectiveStageBeforeResolve {
			if !copied {
				// 每个 iterator 元素原本就持有独立参数 map；仅在实际修改前复制。
				result = make(map[string]any, len(paramContext.params))
				maps.Copy(result, paramContext.params)
				copied = true
			}
			directiveArgs, argsErr := materializeDirectiveArgs(dp, rundata)
			if argsErr != nil {
				return nil, argsErr
			}
			beforeResolveResult, beforeResolveResultErr := dp.RuntimeHandler.BeforeResolve(s.fieldPlan, directiveArgs, result, paramContext.parentResponse, rundata.originalParams, ctx)
			if beforeResolveResultErr != nil {
				return nil, beforeResolveResultErr
			}
			result = beforeResolveResult
		}
	}
	return result, nil
}

func (s *IteratorStep) applyAfterResolveDirectives(paramContext IteratorParamContext, currentResponse any, rundata *Rundata, ctx context.Context) (any, error) {
	current := currentResponse
	for _, dp := range s.fieldPlan.GetDirectivePlans() {
		if dp.Stage == DirectiveStageAfterResolve {
			directiveArgs, argsErr := materializeDirectiveArgs(dp, rundata)
			if argsErr != nil {
				return nil, argsErr
			}
			next, err := dp.RuntimeHandler.AfterResolve(s.fieldPlan, directiveArgs, paramContext.params, paramContext.parentResponse, current, rundata.originalParams, ctx)
			if err != nil {
				return nil, err
			}
			current = next
		}
	}
	return current, nil
}

func isFieldPlanCompiledType(fp *FieldPlan) bool {
	if fp.GetRuntimeTypeResolverFunc() == nil {
		return true
	}
	return false
}

func shouldExecuteFieldAsCompiledType(fp *FieldPlan, ctx *context.Context) bool {
	return fp.GetAllowedRuntimeTypeNames()[fp.GetCompiledTypeName()]
}

func shouldExecuteFieldAsRuntimeType(fp *FieldPlan, parentData any, ctx *context.Context) bool {
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

func shouldExecuteFieldAsResolvedRuntimeType(fp *FieldPlan, typeName string) bool {
	if fp == nil || len(fp.GetAllowedRuntimeTypeNames()) == 0 {
		return true
	}
	if typeName == "" {
		return false
	}
	return fp.GetAllowedRuntimeTypeNames()[typeName]
}

// HandleResponseForNullBubbling 按 GraphQL 类型包装递归完成 resolver 返回值。
// 返回值统一是 []any：非 list 字段返回单元素切片，list 字段返回规范化后的元素切片。
func HandleResponseForNullBubbling(meta FieldValueMetaInfo, fieldResponse any) ([]any, error) {
	result, err, _ := handleResponseForNullBubbling(meta, fieldResponse)
	return result, err
}

// handleResponseForNullBubbling 仅在元素需要规范化或发生 null 冒泡时复制 list，
// 正常的 []any 返回原切片，避免每个 list 字段都重新分配并复制全部元素。
func handleResponseForNullBubbling(meta FieldValueMetaInfo, fieldResponse any) ([]any, error, bool) {
	if isNilInterfaceValue(fieldResponse) {
		if meta.NotNil {
			return nil, fmt.Errorf("non-null response is nil"), true
		}
		if meta.IsList {
			return nil, nil, false
		}
		return []any{nil}, nil, true
	}

	if !meta.IsList {
		return []any{fieldResponse}, nil, true
	}

	listVal, ok := asListValue(fieldResponse)
	if !ok {
		return nil, fmt.Errorf("list value is not a list"), true
	}
	childMeta := meta.ElementType
	if childMeta == nil {
		return nil, fmt.Errorf("list value has no child metadata"), true
	}

	// asListValue 只有输入本身是 []any 时才不会转换；其他 typed slice 已经产生新的 []any。
	_, sourceIsAnySlice := fieldResponse.([]any)
	changed := !sourceIsAnySlice
	var result []any
	if changed {
		result = listVal
	}

	if !childMeta.IsList {
		for i, child := range listVal {
			if !isNilInterfaceValue(child) {
				continue
			}
			if childMeta.NotNil {
				return nil, fmt.Errorf("non-null response is nil"), true
			}
			// nil interface 无需改写；typed nil 必须转成 nil，保持原有完成结果。
			if child != nil {
				if result == nil {
					result = make([]any, len(listVal))
					copy(result, listVal)
				}
				result[i] = nil
				changed = true
			}
		}
		if result == nil {
			result = listVal
		}
		return result, nil, changed
	}

	var firstErr error
	for i, child := range listVal {
		childResult, childErr, childChanged := handleResponseForNullBubbling(*childMeta, child)
		if childErr != nil {
			if firstErr == nil {
				firstErr = childErr
			}
			if childMeta.NotNil {
				return nil, firstErr, true
			}
			childResult = nil
			childChanged = true
		}

		// typed nil list 在原实现中会被规范化为 nil，不能直接复用原元素。
		if childResult == nil && child != nil && isNilInterfaceValue(child) {
			childChanged = true
		}

		if childChanged {
			if result == nil {
				result = make([]any, len(listVal))
				copy(result, listVal)
			}
			result[i] = childResult
			changed = true
		}
	}

	if result == nil {
		result = listVal
	}
	return result, firstErr, changed
}
