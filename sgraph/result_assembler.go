package sgraph

import (
	"context"
	"errors"
	"fmt"
	"reflect"

	"github.com/graphql-go/graphql"
)

type SGraphResultAssembler struct {
	schema *graphql.Schema
}

func (a *SGraphResultAssembler) assembleGraphResult(plan *SGraphExecutionPlan, rundata *Rundata, root map[string]any, ctx context.Context) *SGraphResult {
	result := &SGraphResult{}
	if plan == nil {
		return result
	}

	roots := plan.roots
	orderedResponsesMap := newSGraphResponseOrderedMap(len(roots))

	for _, rootField := range roots {
		rootFieldWrapperTypeInfo := rootField.fieldWrapperTypeInfo
		//组装阶段再次判断include/skip指令
		included, includeErr := evaluateSkipIncludeDirectivesShouldExecuteField(rootField, rundata, ctx)
		if includeErr != nil {
			rundata.addFieldError(rootField.fieldId, FieldErrorTypeField, includeErr, rootField.paths)
			if rootFieldWrapperTypeInfo.notNil {
				orderedResponsesMap = nil
				break
			}
			orderedResponsesMap.set(rootField.responseName, nil)
			continue
		}
		if !included {
			continue
		}

		if rootFieldWrapperTypeInfo.isList {
			rootResult := a.buildListFieldValue(rootField, root, rundata, ctx)
			//null值冒泡
			if rootResult == nil {
				if rootFieldWrapperTypeInfo.notNil {
					orderedResponsesMap = nil
					break
				}
				orderedResponsesMap.set(rootField.responseName, nil)
			} else {
				orderedResponsesMap.set(rootField.responseName, rootResult)
			}
		} else {
			if rootField.fieldWrapperTypeInfo.fieldElementTypeEnum == FIELD_ELEMENT_TYPE_OBJECT {
				rootResult := a.buildObjectFieldValue(rootField, root, rundata, ctx)
				//null值冒泡
				if rootResult == nil && rootFieldWrapperTypeInfo.notNil {
					orderedResponsesMap = nil
					break
				} else {
					orderedResponsesMap.set(rootField.responseName, rootResult)
				}
			} else if rootFieldWrapperTypeInfo.fieldElementTypeEnum == FIELD_ELEMENT_TYPE_SCALAR || rootFieldWrapperTypeInfo.fieldElementTypeEnum == FIELD_ELEMENT_TYPE_ENUM {
				rootResult := a.buildScalarOrEnumFieldValue(rootField, root, rundata, ctx)
				//null值冒泡
				if rootResult == nil && rootFieldWrapperTypeInfo.notNil {
					orderedResponsesMap = nil
					break
				} else {
					orderedResponsesMap.set(rootField.responseName, rootResult)
				}
			}
		}
	}
	result.orderedResponses = orderedResponsesMap
	result.errors = rundata.getAllFieldErrors()
	result.extensionErrors = rundata.getAllExtensionErrors()
	return result
}

func (a *SGraphResultAssembler) buildObjectFieldValue(fieldPlan *FieldPlan, parentResponse any, rundata *Rundata, ctx context.Context) *SGraphResponseOrderedMap {
	if fieldPlan != nil {
		children := fieldPlan.childrenFields
		result := newSGraphResponseOrderedMap(len(children))

		fieldResponse, extractErr := a.extractFieldResponse(fieldPlan, parentResponse, rundata, ctx)
		//如果当前字段结果为空，不再遍历子字段
		if isNilInterfaceValue(fieldResponse) || extractErr != nil {
			return nil
		}

		//抽象类型运行时推断并校验
		runtimeTypeName, valid := a.validateAbstractFieldValue(fieldPlan, fieldResponse, rundata, ctx)
		if !valid {
			return nil
		}
		for _, child := range children {
			childWrapperTypeInfo := child.fieldWrapperTypeInfo
			//根据skip和include指令判断是否组装
			included, includeErr := evaluateSkipIncludeDirectivesShouldExecuteField(child, rundata, ctx)
			if includeErr != nil {
				rundata.addFieldError(child.fieldId, FieldErrorTypeField, includeErr, child.paths)
				if childWrapperTypeInfo.notNil {
					return nil
				}
				result.set(child.responseName, nil)
				continue
			}
			if !included {
				continue
			}
			//fragment type condition必须在组装阶段再次生效，避免无resolver的field绕过step执行阶段的动态类型过滤
			if isFieldPlanTypeCompiled(child) {
				if !evaluateCompiledTypeShouldExecuteField(child, ctx) {
					continue
				}
			} else if !evaluateRuntimeAllowedTypeShouldExecuteField(child, runtimeTypeName) {
				continue
			}
			if childWrapperTypeInfo.isList {
				//如果子字段是List类型且List为non-null但是出现nil，则判断当前字段是否为non-null，如果是则清空当前字段的数据返回nil
				childResult := a.buildListFieldValue(child, fieldResponse, rundata, ctx)
				//null值冒泡
				if childResult == nil {
					if childWrapperTypeInfo.notNil {
						return nil
					}
					result.set(child.responseName, nil)
				} else {
					result.set(child.responseName, childResult)
				}
			} else {
				switch childWrapperTypeInfo.fieldElementTypeEnum {
				case FIELD_ELEMENT_TYPE_OBJECT:
					childResult := a.buildObjectFieldValue(child, fieldResponse, rundata, ctx)
					//null值冒泡
					if childResult == nil && childWrapperTypeInfo.notNil {
						return nil
					}
					result.set(child.responseName, childResult)
				case FIELD_ELEMENT_TYPE_SCALAR, FIELD_ELEMENT_TYPE_ENUM:
					childResult := a.buildScalarOrEnumFieldValue(child, fieldResponse, rundata, ctx)
					//null值冒泡
					if childWrapperTypeInfo.notNil && childResult == nil {
						return nil
					}
					result.set(child.responseName, childResult)
				}
			}
		}
		return result
	}
	return nil
}

func (a *SGraphResultAssembler) buildListFieldValue(field *FieldPlan, parentResponse any, rundata *Rundata, ctx context.Context) []any {
	if field == nil {
		return nil
	}

	fieldResponse, extractErr := a.extractFieldResponse(field, parentResponse, rundata, ctx)
	if extractErr != nil || isNilInterfaceValue(fieldResponse) {
		return nil
	}
	fieldResponseAsList, fieldResponseAsListOk := asListValue(fieldResponse)
	if !fieldResponseAsListOk {
		err := fmt.Errorf("field response is not a list %s", FieldPlan{}.fieldName)
		rundata.addFieldError(field.fieldId, FieldErrorTypeField, err, field.paths)
		return nil
	}

	fieldWrapperTypeInfo := field.fieldWrapperTypeInfo
	return a.buildListValueItems(field, fieldWrapperTypeInfo.elementWrapperTypeInfo, fieldResponseAsList, rundata, ctx)
}

func (a *SGraphResultAssembler) buildListValueItems(field *FieldPlan, elementWrappTypeInfo *FieldWrapperTypeInfo, items []any, rundata *Rundata, ctx context.Context) []any {
	if elementWrappTypeInfo == nil {
		err := fmt.Errorf("list field %s has no element wrapper type info", field.fieldName)
		rundata.addFieldError(field.fieldId, FieldErrorTypeField, err, field.paths)
		return nil
	}

	result := make([]any, 0, len(items))
	for _, item := range items {
		if isNilInterfaceValue(item) {
			if elementWrappTypeInfo.notNil {
				if rundata.fieldErrors[field.fieldId].Load() == nil {
					err := fmt.Errorf("cannot return null for non-nullable list element of field %s", field.responseName)
					rundata.addFieldError(field.fieldId, FieldErrorTypeField, err, field.paths)
				}
				return nil
			}
			result = append(result, nil)
			continue
		}

		if elementWrappTypeInfo.isList {
			childItems, ok := asListValue(item)
			if !ok {
				err := fmt.Errorf("expected iterable list element for field %s", field.responseName)
				rundata.addFieldError(field.fieldId, FieldErrorTypeField, err, field.paths)
				if elementWrappTypeInfo.notNil {
					return nil
				}
				result = append(result, nil)
				continue
			}
			childResult := a.buildListValueItems(field, elementWrappTypeInfo.elementWrapperTypeInfo, childItems, rundata, ctx)
			if childResult == nil && elementWrappTypeInfo.notNil {
				return nil
			}
			if childResult == nil {
				result = append(result, nil)
			} else {
				result = append(result, childResult)
			}
			continue
		}

		switch elementWrappTypeInfo.fieldElementTypeEnum {
		case FIELD_ELEMENT_TYPE_SCALAR, FIELD_ELEMENT_TYPE_ENUM:
			serialized := serializeLeafValue(field, item, rundata)
			if serialized == nil && elementWrappTypeInfo.notNil {
				return nil
			}
			result = append(result, serialized)
		case FIELD_ELEMENT_TYPE_OBJECT:
			runtimeTypeName, valid := a.validateAbstractFieldValue(field, item, rundata, ctx)
			if !valid {
				//如果抽象类型校验不合法且字段non-null，直接返回nil
				if elementWrappTypeInfo.notNil {
					return nil
				}
				result = append(result, nil)
				continue
			}
			objectResult := a.buildObjectItemInListValueItems(field, item, runtimeTypeName, rundata, ctx)
			if objectResult == nil && elementWrappTypeInfo.notNil {
				return nil
			}
			result = append(result, objectResult)
		default:
			result = append(result, item)
		}
	}
	return result
}

func (a *SGraphResultAssembler) buildObjectItemInListValueItems(fieldPlan *FieldPlan, currentFieldResponse any, runtimeTypeName string, rundata *Rundata, ctx context.Context) *SGraphResponseOrderedMap {
	var result *SGraphResponseOrderedMap
	if isNilInterfaceValue(currentFieldResponse) {
		return result
	}

	//TODO 获取当前字段的子字段，遍历每个子字段的类型组装Map
	if fieldPlan != nil {
		children := fieldPlan.childrenFields
		result = newSGraphResponseOrderedMap(len(children))
		for _, child := range children {
			childWrapperTypeInfo := child.fieldWrapperTypeInfo
			//判断include和skip指令是否执行
			included, includeErr := evaluateSkipIncludeDirectivesShouldExecuteField(child, rundata, ctx)
			if includeErr != nil {
				//指令执行错误时处理null值冒泡
				rundata.addFieldError(child.fieldId, FieldErrorTypeField, includeErr, child.paths)
				if childWrapperTypeInfo.notNil {
					return nil
				}
				result.set(child.responseName, nil)
				continue
			}
			if !included {
				continue
			}
			if isFieldPlanTypeCompiled(child) {
				if !evaluateCompiledTypeShouldExecuteField(child, ctx) {
					continue
				}
			} else if !evaluateDynamicTypeShouldExecuteField(child, currentFieldResponse, ctx) {
				continue
			}

			if child.isIntrospectionTypeNameField() && child.fieldTypeScope.dynamicTypeResolver != nil {
				if runtimeTypeName == "" {
					rundata.addFieldError(child.fieldId, FieldErrorTypeField, errors.New("__typename resolved failed, value is empty"), child.paths)
					//null值冒泡
					if childWrapperTypeInfo.notNil {
						return nil
					}
					result.set(child.responseName, nil)
					continue
				}
				serialized := serializeLeafValue(child, runtimeTypeName, rundata)
				if serialized == nil && childWrapperTypeInfo.notNil {
					return nil
				}
				result.set(child.responseName, nil)
				continue
			}
			if childWrapperTypeInfo.isList {
				childResult := a.buildListValueInListValueObjectItem(child, currentFieldResponse, rundata, ctx)
				//null值冒泡
				if childResult == nil {
					if childWrapperTypeInfo.notNil {
						return nil
					}
					result.set(child.responseName, nil)
				} else {
					result.set(child.responseName, childResult)
				}
			} else {
				switch child.fieldWrapperTypeInfo.fieldElementTypeEnum {
				case FIELD_ELEMENT_TYPE_OBJECT:
					childResult := a.buildObjectItemInListValueObjectItem(child, currentFieldResponse, rundata, ctx)
					//null值冒泡
					if childResult == nil && childWrapperTypeInfo.notNil {
						return nil
					}
					result.set(child.responseName, childResult)
				case FIELD_ELEMENT_TYPE_SCALAR, FIELD_ELEMENT_TYPE_ENUM:
					childResult := rundata.getFieldResponseByFieldId(child.fieldId)
					if childResult != nil && childResult.hasBulkResponseBinding() {
						if currentFieldResponseMap, isFieldResponseMap := currentFieldResponse.(map[string]any); isFieldResponseMap {
							compositeKey := generateCompositeKey([]string{child.parentKeyFieldName}, currentFieldResponseMap)
							if val, ok := childResult.lookResponseByCompositeKey(compositeKey); ok {
								serialized := serializeLeafValue(child, val, rundata)
								if serialized == nil && childWrapperTypeInfo.notNil {
									if rundata.fieldErrors[child.fieldId].Load() == nil {
										err := fmt.Errorf("cannot return null for non-nullable field %s", child.responseName)
										rundata.addFieldError(child.fieldId, FieldErrorTypeField, err, child.paths)
									}
									return nil
								}
								result.set(child.responseName, serialized)
							} else {
								//null值冒泡
								if childWrapperTypeInfo.notNil {
									if rundata.fieldErrors[child.fieldId].Load() == nil {
										err := fmt.Errorf("cannot return null for non-nullable field %s", child.responseName)
										rundata.addFieldError(child.fieldId, FieldErrorTypeField, err, child.paths)
									}
									return nil
								}
								result.set(child.responseName, childResult)
							}
						} else {
							if childWrapperTypeInfo.notNil {
								if rundata.fieldErrors[child.fieldId].Load() == nil {
									err := fmt.Errorf("cannot return null for non-nullable field %s", child.responseName)
									rundata.addFieldError(child.fieldId, FieldErrorTypeField, err, child.paths)
								}
								return nil
							}
							result.set(child.responseName, childResult)
						}
					} else {
						//TODO 这里考虑修改buildScalarOrEnumValue方法，直接从父节点数据中组装，不用再查询rundata本节点的数据
						scalarOrEnumResult := a.buildScalarOrEnumFieldValue(child, currentFieldResponse, rundata, ctx)
						if scalarOrEnumResult != nil {
							result.set(child.responseName, scalarOrEnumResult)
						} else {
							if childWrapperTypeInfo.notNil {
								return nil
							}
							result.set(child.responseName, nil)
						}
					}
				}
			}
		}
	}
	return result
}

func (a *SGraphResultAssembler) buildScalarOrEnumFieldValue(fieldPlan *FieldPlan, parentResponse any, rundata *Rundata, ctx context.Context) any {
	if fieldPlan != nil {
		switch fieldPlan.fieldWrapperTypeInfo.fieldElementTypeEnum {
		case FIELD_ELEMENT_TYPE_ENUM, FIELD_ELEMENT_TYPE_SCALAR:
			originalResponse, extractErr := a.extractFieldResponse(fieldPlan, parentResponse, rundata, ctx)
			if extractErr != nil {
				return nil
			}
			return serializeLeafValue(fieldPlan, originalResponse, rundata)
		default:
			return nil
		}
	}
	return nil
}

func (a *SGraphResultAssembler) buildObjectItemInListValueObjectItem(fieldPlan *FieldPlan, parentRespoinse any, rundata *Rundata, ctx context.Context) *SGraphResponseOrderedMap {
	var result *SGraphResponseOrderedMap
	if fieldPlan != nil {
		//获取当前字段的结果，如果有运行过程取运行时数据，没有则从父字段结果读取
		fieldResponse, extractErr := a.extractFieldResponse(fieldPlan, parentRespoinse, rundata, ctx)
		if extractErr != nil {
			return nil
		}
		// nullable object 的原始值为 nil 时，其完成值必须是 null，不能继续组装成空对象。non-null字段的错误已经在extractFieldResponse里写入Rundata
		if isNilInterfaceValue(fieldResponse) {
			return nil
		}
		runtimeTypeName := ""
		var valid bool
		runtimeTypeName, valid = a.validateAbstractFieldValue(fieldPlan, fieldResponse, rundata, ctx)
		if !valid {
			return nil
		}

		//根据子节点继续遍历生成map
		children := fieldPlan.childrenFields
		result = newSGraphResponseOrderedMap(len(children))
		//非空抽象字段已经由validateAbstractFieldValue解析完成
		runtimeTypeResolved := runtimeTypeName != ""
		resolveRuntimeTypeName := func(child *FieldPlan) string {
			if !runtimeTypeResolved && child.fieldTypeScope.dynamicTypeResolver != nil {
				runtimeTypeName = child.fieldTypeScope.dynamicTypeResolver(fieldResponse, &ctx)
				runtimeTypeResolved = true
			}
			return runtimeTypeName
		}
		for _, child := range children {
			childWrapperTypeInfo := child.fieldWrapperTypeInfo
			included, includeErr := evaluateSkipIncludeDirectivesShouldExecuteField(child, rundata, ctx)
			if includeErr != nil {
				rundata.addFieldError(child.fieldId, FieldErrorTypeField, includeErr, child.paths)
				if childWrapperTypeInfo.notNil {
					return nil
				}
				result.set(child.responseName, nil)
				continue
			}
			if !included {
				continue
			}
			//动态类型推断，嵌套对象也要按当前对象的运行时类型过滤，支持fragment组装时判断。
			if isFieldPlanTypeCompiled(child) {
				if !evaluateCompiledTypeShouldExecuteField(child, ctx) {
					continue
				}
			} else if !evaluateRuntimeAllowedTypeShouldExecuteField(child, resolveRuntimeTypeName(child)) {
				continue
			}
			if childWrapperTypeInfo.isList {
				childResult := a.buildListValueInListValueObjectItem(child, fieldResponse, rundata, ctx)
				//null值冒泡
				if childResult == nil {
					if childWrapperTypeInfo.notNil {
						return nil
					}
					result.set(child.responseName, nil)
				} else {
					result.set(child.responseName, childResult)
				}
			} else {
				switch child.fieldWrapperTypeInfo.fieldElementTypeEnum {
				case FIELD_ELEMENT_TYPE_OBJECT:
					childResult := a.buildObjectItemInListValueObjectItem(child, fieldResponse, rundata, ctx)
					//null值冒泡
					if childResult == nil && childWrapperTypeInfo.notNil {
						return nil
					}
					result.set(child.responseName, childResult)
				case FIELD_ELEMENT_TYPE_SCALAR, FIELD_ELEMENT_TYPE_ENUM:
					childResult := a.buildScalarOrEnumItemInListValueObjectItem(child, fieldResponse, rundata, ctx)
					//null值冒泡
					if childResult == nil && childWrapperTypeInfo.notNil {
						return nil
					}
					result.set(child.responseName, childResult)
				}
			}
		}
	}
	return result
}

func (a *SGraphResultAssembler) buildScalarOrEnumItemInListValueObjectItem(fieldPlan *FieldPlan, parentRespoinse any, rundata *Rundata, ctx context.Context) any {
	if fieldPlan != nil {
		val, extractErr := a.extractFieldResponse(fieldPlan, parentRespoinse, rundata, ctx)
		if extractErr != nil {
			return nil
		}
		return serializeLeafValue(fieldPlan, val, rundata)
	}
	return nil
}

// 对于List类型的字段返回值，由于内部的null值冒泡在resolve阶段已经冒泡完成，组装阶段仅做组装处理
func (a *SGraphResultAssembler) buildListValueInListValueObjectItem(fieldPlan *FieldPlan, parentResponse any, rundata *Rundata, ctx context.Context) []any {
	if fieldPlan == nil {
		return nil
	}

	currentFieldResponse, extractErr := a.extractFieldResponse(fieldPlan, parentResponse, rundata, ctx)
	if extractErr != nil || isNilInterfaceValue(currentFieldResponse) {
		return nil
	}

	currentFieldResponseAsList, currentFieldResponseAsListOk := asListValue(currentFieldResponse)
	if !currentFieldResponseAsListOk {
		err := fmt.Errorf("expected iterable field %s validated failed while building result", fieldPlan.responseName)
		rundata.addFieldError(fieldPlan.fieldId, FieldErrorTypeField, err, fieldPlan.paths)
		return nil
	}
	fieldWrapperTypeInfo := fieldPlan.fieldWrapperTypeInfo
	return a.buildListValueItems(fieldPlan, fieldWrapperTypeInfo.elementWrapperTypeInfo, currentFieldResponseAsList, rundata, ctx)
}

func (a *SGraphResultAssembler) extractFieldResponse(fieldPlan *FieldPlan, parentResponse any, rundata *Rundata, ctx context.Context) (any, error) {
	if fieldPlan == nil {
		return nil, nil
	}

	if rundata == nil {
		return nil, fmt.Errorf("rundata is nil")
	}
	hasResolver := fieldPlan.resolverFunc != nil || fieldPlan.bulkResolverFunc != nil
	if hasResolver {
		fieldResult := rundata.getFieldResponseByFieldId(fieldPlan.fieldId)
		if fieldResult != nil {
			//父节点是List，子字段必须按照composite key渠当前父元素对应的结果
			if fieldResult.hasBulkResponseBinding() {
				parentResponseMap, ok := parentResponse.(map[string]any)
				if !ok {
					return nil, nil
				}
				parentKeyFieldName := fieldPlan.parentKeyFieldName
				if parentKeyFieldName == "" {
					return nil, nil
				}
				compositeKey := generateCompositeKey([]string{parentKeyFieldName}, parentResponseMap)
				if bindingChildResponse, bindingChildResponseOk := fieldResult.lookResponseByCompositeKey(compositeKey); bindingChildResponseOk {
					return bindingChildResponse, nil
				}
				//该父元素没有子结果时，返回空List
				if fieldPlan.bulkResolverFunc != nil && fieldPlan.fieldWrapperTypeInfo.isList {
					return []any{}, nil
				}
				return nil, nil
			}
			if fieldPlan.fieldWrapperTypeInfo.isList {
				//没有父级绑定时，直接返回整个字段结果
				return fieldResult.responseRaws, nil
			}
			if len(fieldResult.responseRaws) > 0 {
				return fieldResult.responseRaws[0], nil
			}
		}
		return nil, nil
	}

	if isNilInterfaceValue(parentResponse) {
		addNonNullCompletionErrorIfNeeded(fieldPlan, nil, rundata)
		return nil, nil
	}

	// 普通业务对象使用 schema fieldName；内省中间结果当前已经按照 responseName 生成。
	propertyKey := fieldPlan.fieldName
	if fieldPlan.responseName != propertyKey && fieldPlan.responseName != "" {
		switch fieldPlan.fieldTypeScope.staticTypeName {
		case "__Schema", "__Type", "__Field", "_InputValue", "__EnumValue", "__Directive":
			propertyKey = fieldPlan.responseName
		}
	}

	// map 中保存的是已经完成的属性值，不执行函数形式的延迟属性。
	if parentResponseMap, parentResponseMapOk := parentResponse.(map[string]any); parentResponseMapOk {
		if len(rundata.extensions) == 0 {
			result := parentResponseMap[propertyKey]
			addNonNullCompletionErrorIfNeeded(fieldPlan, result, rundata)
			return result, nil
		}
		_, _, finishHook := startSGraphResolveFieldHook(rundata, fieldPlan, ctx, -1)
		result := parentResponseMap[propertyKey]
		finishSGraphResolveFieldHook(rundata, finishHook, result, nil)
		addNonNullCompletionErrorIfNeeded(fieldPlan, result, rundata)
		return result, nil
	}

	// named map、map[string]T 等 string-key map 也直接读取属性，避免落入 DefaultResolveFn 后重新启用函数属性语义。
	if _, isFieldResolver := parentResponse.(graphql.FieldResolver); !isFieldResolver {
		parentValue := reflect.ValueOf(parentResponse)
		if parentValue.IsValid() && parentValue.Kind() == reflect.Map && parentValue.Type().Key().Kind() != reflect.String {
			mapKey := reflect.New(parentValue.Type().Key()).Elem()
			mapKey.SetString(propertyKey)

			var result any
			mapValue := parentValue.MapIndex(mapKey)
			if mapValue.IsValid() {
				result = mapValue.Interface()
			}

			if len(rundata.extensions) != 0 {
				_, _, finishHook := startSGraphResolveFieldHook(rundata, fieldPlan, ctx, -1)
				finishSGraphResolveFieldHook(rundata, finishHook, result, nil)
			}

			addNonNullCompletionErrorIfNeeded(fieldPlan, result, rundata)
			return result, nil
		}
	}

	// struct、json/graphql tag 和 FieldResolver 继续使用 graphql-go DefaultResolveFn，保留现有默认取值能力。
	fieldCtx, info, finishHook := startSGraphResolveFieldHook(rundata, fieldPlan, ctx, -1)
	result, resolveErr := callDefaultResolveFn(graphql.ResolveParams{
		Source:  parentResponse,
		Info:    info,
		Context: fieldCtx,
	})
	finishSGraphResolveFieldHook(rundata, finishHook, result, resolveErr)
	if resolveErr != nil {
		rundata.addFieldError(fieldPlan.fieldId, FieldErrorTypeField, resolveErr, fieldPlan.paths)
		return nil, resolveErr
	}

	addNonNullCompletionErrorIfNeeded(fieldPlan, result, rundata)
	return result, nil
}

// 遇到抽象类型时推断运行时类型并校验合法性。string类型返回值代表当前value运行时具体Object类型名称，bool类型返回值表示抽象类型校验是否合法。true允许继续，false代表是nil且调用方自行处理null值冒泡
func (a *SGraphResultAssembler) validateAbstractFieldValue(field *FieldPlan, value any, rundata *Rundata, ctx context.Context) (string, bool) {
	if field == nil || isNilInterfaceValue(value) {
		return "", true
	}

	//当前字段是否为抽象类型，不是抽象类不做处理直接返回
	fieldWrapperTypeInfo := field.fieldWrapperTypeInfo
	var abs graphql.Abstract
	switch tt := fieldWrapperTypeInfo.baseType.(type) {
	case *graphql.Interface:
		abs = tt
	case *graphql.Union:
		abs = tt
	default:
		return "", true
	}

	//获取全部PossibleTypes，优先执行ResolveType，否则执行IsTypeOf
	possibleTypes := a.schema.PossibleTypes(abs)
	typeName := ""
	switch t := abs.(type) {
	case *graphql.Interface:
		if t.ResolveType != nil {
			if obj := t.ResolveType(graphql.ResolveTypeParams{
				Value:   value,
				Context: ctx,
			}); obj != nil {
				typeName = obj.Name()
			}
		}
	case *graphql.Union:
		if t.ResolveType != nil {
			if obj := t.ResolveType(graphql.ResolveTypeParams{
				Value:   value,
				Context: ctx,
			}); obj != nil {
				typeName = obj.Name()
			}
		}
	}

	if typeName == "" {
		for _, possibleType := range possibleTypes {
			if possibleType.IsTypeOf == nil {
				continue
			}
			if possibleType.IsTypeOf(graphql.IsTypeOfParams{
				Value:   value,
				Context: ctx,
			}) {
				typeName = possibleType.Name()
				break
			}
		}
	}

	//无法推断运行时类型，报错返回
	if typeName == "" {
		rundata.addFieldError(field.fieldId, FieldErrorTypeField, fmt.Errorf("abstract type %s must resolve to an Object type at runtime", abs.Name()), field.paths)
		return "", false
	}

	for _, possibleType := range possibleTypes {
		if possibleType.Name() == typeName {
			return typeName, true
		}
	}

	rundata.addFieldError(field.fieldId, FieldErrorTypeField, fmt.Errorf("runtime object type %q is not a possible type for %q", typeName, abs.Name()), field.paths)
	return "", false
}

// 使组装阶段的默认resolver与graphql-go resolveField具有相同的paic转execution error语义
func callDefaultResolveFn(param graphql.ResolveParams) (result any, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			result = nil
			if recoveredErr, ok := recovered.(error); ok {
				err = recoveredErr
			} else {
				err = errors.New(fmt.Sprint(recovered))
			}
		}
	}()
	return graphql.DefaultResolveFn(param)
}

// 只在字段尚未记录执行错误时补充Non-Null完成错误，避免reoslver/default resolver的原始错误被通用的null错误覆盖
func addNonNullCompletionErrorIfNeeded(fieldPlan *FieldPlan, value any, rundata *Rundata) {
	if fieldPlan == nil || rundata == nil || !fieldPlan.fieldWrapperTypeInfo.notNil || !isNilInterfaceValue(value) || rundata.fieldErrors[fieldPlan.fieldId].Load() != nil {
		return
	}
	err := fmt.Errorf("cannot return null for non-nullable field %s", fieldPlan.fieldName)
	rundata.addFieldError(fieldPlan.fieldId, FieldErrorTypeField, err, fieldPlan.paths)
}

func serializeLeafValue(fieldPlan *FieldPlan, value any, rundata *Rundata) (serialized any) {
	//原始结果为null时不执行结果强制转换；是否违反Non-Null由字段或list元素完成逻辑处理。
	if fieldPlan == nil || isNilInterfaceValue(value) {
		return nil
	}

	//自定义Scalar/Enum的Serialize可能panic。结果强制转换失败属于字段执行错误，不能让panic逃逸并中断整个请求。
	defer func() {
		if recovered := recover(); recovered != nil {
			var serializeErr error
			if recoveredErr, ok := recovered.(error); ok {
				serializeErr = fmt.Errorf("cannot serialize leaf value for %s:%w", fieldPlan.fieldName, recoveredErr)
			} else {
				serializeErr = fmt.Errorf("cannot serialize leaf value for %s:%v", fieldPlan.fieldName, recovered)
			}
			if rundata != nil {
				rundata.addFieldError(fieldPlan.fieldId, FieldErrorTypeField, serializeErr, fieldPlan.paths)
			}
			serialized = nil
		}
	}()

	info := fieldPlan.fieldWrapperTypeInfo
	switch t := info.baseType.(type) {
	case *graphql.Scalar:
		serialized = t.Serialize(value)
	case *graphql.Enum:
		serialized = t.Serialize(value)
	default:
		return value
	}

	//序列化结果为空则报错
	if isNilInterfaceValue(serialized) || isNullish(serialized) {
		typeName := "unknown"
		if originalType := info.baseType; !isNilInterfaceValue(originalType) {
			typeName = originalType.Name()
		}
		serializeErr := fmt.Errorf("cannot serialize leaf value for %s:%s", fieldPlan.fieldName, typeName)
		if rundata != nil {
			rundata.addFieldError(fieldPlan.fieldId, FieldErrorTypeField, serializeErr, fieldPlan.paths)
		}
		return nil
	}
	return serialized
}
