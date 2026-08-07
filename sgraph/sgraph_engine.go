package sgraph

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"sort"

	"github.com/graphql-go/graphql"
	"github.com/graphql-go/graphql/gqlerrors"
	"github.com/graphql-go/graphql/language/ast"
	"github.com/graphql-go/graphql/language/printer"
	Lmap "github.com/liuzhaodong89/lockfree-collection/map"
)

type SGraphEngine struct {
	schema            *graphql.Schema
	directiveRegistry *DirectiveRegistry
	resultAssembler   *SGraphResultAssembler
	planCache         *Lmap.Lmap[string, *SGraphExecutionPlan]
}

func (e *SGraphEngine) executeWithCache(document *ast.Document, args map[string]any, operationName *string, root map[string]any, ctx context.Context, extensions []Extension) *SGraphResult {
	if e == nil {
		return newSGraphErrorResult(errors.New("sgraph engine is nil"))
	}
	if e.schema.QueryType() == nil {
		return newSGraphErrorResult(errors.New("sgraph engine schema is nil"))
	}
	if e.planCache == nil {
		e.planCache = Lmap.New[string, *SGraphExecutionPlan]()
	}
	if e.directiveRegistry == nil {
		e.directiveRegistry = newDirectiveRegistry()
	}

	cacheKey := buildPlanCacheKey(document, operationName)
	plan, ok := e.planCache.Get(cacheKey)
	if !ok {
		var buildErr error
		plan, buildErr = compileExecutionPlan(document, e.schema, operationName, e.directiveRegistry)
		if buildErr != nil {
			return newSGraphErrorResult(buildErr)
		}
		operationDef, operationDefOk := plan.schemaResolveInfo.operation.(*ast.OperationDefinition)
		if !operationDefOk {
			return newSGraphErrorResult(errors.New("invalid operation definition"))
		}
		if operationDef.Name.Value == ast.OperationTypeSubscription {
			return newSGraphErrorResult(errors.New("subscription is not supported by graphsoul execute"))
		}
		e.planCache.Set(cacheKey, plan)
	}

	inputs, inputErr := completeVariables(document, e.schema, operationName, args)
	if inputErr != nil {
		return newSGraphErrorResult(inputErr)
	}

	return e.executePlan(plan, inputs, root, ctx, extensions)
}

func (e *SGraphEngine) createBatches(executionPlan *SGraphExecutionPlan) ([]*BatchPlan, error) {
	if executionPlan == nil {
		return nil, errors.New("executionPlan is nil")
	}
	if executionPlan == nil {
		return nil, errors.New("executionPlan is nil")
	}
	executionPlan.batchesMu.Lock()
	defer executionPlan.batchesMu.Unlock()

	if executionPlan.batches != nil {
		return executionPlan.batches, nil
	}
	batches, err := coordinateBatches(executionPlan)
	if err != nil {
		return nil, err
	}

	executionPlan.batches = batches
	return executionPlan.batches, nil
}

func (e *SGraphEngine) executePlan(plan *SGraphExecutionPlan, inputs map[string]any, root map[string]any, ctx context.Context, extensions []Extension) *SGraphResult {
	result := &SGraphResult{}
	// 组装本次请求独占的 Rundata；请求数据不能写入可缓存的 plan。
	maxFieldId := plan.maxFieldId
	rundata := newRundata(inputs, maxFieldId)
	// ResolveInfo 和 extension 所需的请求状态统一由 Rundata 持有，并通过函数参数显式向下传递。
	rundata.schema = e.schema
	rundata.resolveInfoRootValue = root
	rundata.operation = plan.schemaResolveInfo.operation
	rundata.fragments = plan.schemaResolveInfo.fragments
	rundata.extensions = extensions
	// Rundata 是请求级对象；必须等 batch 执行和结果组装完成后再释放，避免并发读写和结果污染。
	defer releaseRundata(rundata)
	if ctx == nil {
		ctx = context.TODO()
	}
	//组装Batches
	batches, err := e.createBatches(plan)
	if err != nil {
		result = newSGraphErrorResult(err)
		return result
	}
	//遍历执行Batches，判断遇到中断则返回
	for _, batch := range batches {
		br := batch.execute(rundata, ctx)
		if br.isInterrupt() {
			break
		}
	}
	//组装结果
	if e.resultAssembler == nil {
		e.resultAssembler = &SGraphResultAssembler{
			schema: e.schema,
		}
	}
	result = e.resultAssembler.assembleGraphResult(plan, rundata, root, ctx)
	return result
}

func buildPlanCacheKey(document *ast.Document, operationName *string) string {
	hash := sha256.New()
	if document != nil && document.Loc != nil && document.Loc.Source != nil && len(document.Loc.Source.Body) > 0 {
		// parser 产出的 AST 保留了原始 query 字节；直接参与 hash，避免每个请求重新打印 AST。
		hash.Write(document.Loc.Source.Body)
	} else {
		// 兼容外部手工构造 AST 且没有 Source 的场景，保留旧的规范化打印逻辑兜底。
		hash.Write([]byte(fmt.Sprintf("%v", printer.Print(document))))
	}
	hash.Write([]byte{'\n'})
	if operationName != nil {
		hash.Write([]byte(*operationName))
	}
	// schema 和 directiveRegistry 已经由 SGraphEngine 实例固定；
	// 同一个 engine 内的 plan identity 只需要 document + operationName。
	return hex.EncodeToString(hash.Sum(nil))
}

func completeVariables(document *ast.Document, schema *graphql.Schema, operationName *string, args map[string]any) (map[string]any, error) {
	if document == nil {
		return nil, errors.New("document is nil")
	}
	if schema == nil {
		return nil, errors.New("schema is nil")
	}
	if args == nil {
		args = map[string]any{}
	}

	var operationDefinition *ast.OperationDefinition
	for _, def := range document.Definitions {
		opDef, ok := def.(*ast.OperationDefinition)
		if !ok {
			continue
		}
		if operationName != nil {
			if opDef.Name != nil && opDef.Name.Value == *operationName {
				operationDefinition = opDef
				break
			}
			continue
		}
		//operationName为空时，如果有多个operation则报错
		if operationDefinition != nil {
			return nil, errors.New("operation definition already exists")
		}
		operationDefinition = opDef
	}
	if operationDefinition == nil {
		return nil, errors.New("operation definition is nil")
	}
	return completeOperationVariables(schema, operationDefinition.VariableDefinitions, args)
}

func completeOperationVariables(schema *graphql.Schema, variableDefs []*ast.VariableDefinition, originalInputs map[string]any) (map[string]any, error) {
	if originalInputs == nil {
		originalInputs = make(map[string]any)
	}

	result := make(map[string]any)
	for _, variableDef := range variableDefs {
		name := variableDef.Variable.Name.Value

		inputType, inputTypeErr := graphql.InputTypeFromAST(schema, variableDef.Type)
		if inputTypeErr != nil {
			return nil, inputTypeErr
		}

		variableValue, provided := originalInputs[name]
		if !provided {
			if variableDef.DefaultValue != nil {
				defaultValue, defaultValueErr := valueFromAST(variableDef.DefaultValue, inputType, nil)
				if defaultValueErr != nil {
					return nil, defaultValueErr
				}
				result[name] = defaultValue
				continue
			}

			if isNonNullInput(inputType) {
				variableErr := fmt.Errorf("variable %s is non-null but has no value", name)
				return nil, gqlerrors.NewError(variableErr.Error(), []ast.Node{variableDef}, "", nil, nil, variableErr)
			}

			//未提供的参数值不写nil进入补充结果中
			continue
		}

		if variableValue == nil {
			if isNonNullInput(inputType) {
				variableErr := fmt.Errorf("variable %s is non-null but has nil value", name)
				return nil, gqlerrors.NewError(variableErr.Error(), []ast.Node{variableDef}, "", nil, nil, variableErr)
			}

			//显式提供了nil的参数写入补充结果中
			result[name] = variableValue
			continue
		}
		parsed, parsedErr := parseInputValue(inputType, variableDef)
		if parsedErr != nil {
			message := fmt.Sprintf("Variable \"$%s\": %s", name, parsedErr.Error())
			return nil, gqlerrors.NewError(message, []ast.Node{variableDef}, "", nil, nil, parsedErr)
		}
		result[name] = parsed
	}
	return result, nil
}

func parseInputValue(inputType graphql.Input, source any) (any, error) {
	if source != nil {
		sourceValue := reflect.ValueOf(source)

		if sourceValue.Kind() == reflect.Ptr && sourceValue.IsNil() {
			source = nil
		}
	}

	if nonNullType, ok := inputType.(*graphql.NonNull); ok {
		if source == nil {
			return nil, fmt.Errorf("nonNull input value is required")
		}

		inner, innerOk := nonNullType.OfType.(graphql.Input)
		if !innerOk {
			return nil, fmt.Errorf("nonNull input value is required")
		}
		return parseInputValue(inner, source)
	}

	if source == nil {
		return nil, nil
	}

	switch typedInput := inputType.(type) {
	case *graphql.List:
		inner := typedInput.OfType.(graphql.Input)

		if isSlice(source) {
			sourceItems := toAnySlice(source)
			result := make([]any, 0, len(sourceItems))

			for index, sourceItem := range sourceItems {
				item, itemErr := parseInputValue(inner, sourceItem)
				if itemErr != nil {
					return nil, fmt.Errorf("at index %d: %w", index, itemErr)
				}
				result = append(result, item)
			}
			return result, nil
		}

		// 允许把非List输入转换成单元素List
		single, singleErr := parseInputValue(inner, source)
		if singleErr != nil {
			return nil, singleErr
		}
		return []any{single}, nil
	case *graphql.InputObject:
		sourceMap, ok := source.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("expected input object type %q", typedInput.Name())
		}

		fieldDefs := typedInput.Fields()
		result := make(map[string]any, len(fieldDefs))

		var unknownFieldNames []string
		for sourceFieldName := range sourceMap {
			if _, exists := fieldDefs[sourceFieldName]; !exists {
				unknownFieldNames = append(unknownFieldNames, sourceFieldName)
			}
		}

		if len(unknownFieldNames) > 0 {
			sort.Strings(unknownFieldNames)
			return nil, fmt.Errorf("in field %q: field is not defined by input object %q", unknownFieldNames[0], unknownFieldNames)
		}

		fieldNames := make([]string, 0, len(fieldDefs))
		for fieldName := range fieldDefs {
			fieldNames = append(fieldNames, fieldName)
		}
		sort.Strings(fieldNames)

		for _, fieldName := range fieldNames {
			fieldDef := fieldDefs[fieldName]

			sourceFieldValue, provided := sourceMap[fieldName]
			if !provided {
				if fieldDef.DefaultValue != nil {
					result[fieldName] = fieldDef.DefaultValue
					continue
				}

				if isNonNullInput(fieldDef.Type) {
					return nil, fmt.Errorf("field %q is non-null but has no value", fieldName)
				}
				continue
			}

			parsedFieldValue, parsedFieldValueErr := parseInputValue(fieldDef.Type, sourceFieldValue)
			if parsedFieldValueErr != nil {
				return nil, fmt.Errorf("in field %q: %w", fieldName, parsedFieldValueErr)
			}

			//provided=true且结果为nil，表示显式传入nil，此时应保留
			result[fieldName] = parsedFieldValue
		}
		return result, nil
	case *graphql.Scalar:
		parsed := typedInput.ParseValue(source)
		if parsed == nil {
			return nil, fmt.Errorf("expected scalar type %q", typedInput.Name())
		}
		return parsed, nil
	case *graphql.Enum:
		parsed := typedInput.ParseValue(source)
		if parsed == nil {
			return nil, fmt.Errorf("expected enum type %q", typedInput.Name())
		}
		return parsed, nil
	default:
		return nil, fmt.Errorf("unexpected input type %T", typedInput)
	}
}
