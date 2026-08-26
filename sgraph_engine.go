package graphql

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"sync"

	"github.com/graphql-go/graphql/gqlerrors"
	"github.com/graphql-go/graphql/language/ast"
)

type SGraphEngine struct {
	schema            *Schema
	directiveRegistry *DirectiveRegistry
	paramRegistry     *ParamRegistry
	resultAssembler   *SGraphResultAssembler
	planCache         *sync.Map
}

var (
	defaultSGraphEngineCache   = &sync.Map{}
	defaultSGraphEngineCacheMu sync.Mutex
)

func NewSGraphEngine(schema *Schema, directiveRegistry *DirectiveRegistry, paramRegistry *ParamRegistry) (*SGraphEngine, error) {
	if schema == nil || schema.QueryType() == nil {
		return nil, errors.New(`schema must have a query type`)
	}
	if directiveRegistry == nil {
		directiveRegistry = NewDirectiveRegistry()
	}

	return &SGraphEngine{
		schema:            schema,
		directiveRegistry: directiveRegistry.cloneAndFreeze(),
		paramRegistry:     paramRegistry.cloneAndFreeze(),
		resultAssembler:   &SGraphResultAssembler{schema: schema},
		planCache:         &sync.Map{},
	}, nil
}

// RegisterSGraphEngine 在应用启动阶段将冻结的 Engine 绑定到 Schema。
// 请求执行时只传入 Schema，执行层会根据 Schema 复用已绑定的 Engine。
func RegisterSGraphEngine(engine *SGraphEngine) error {
	if engine == nil {
		return errors.New("sgraph engine is nil")
	}
	if engine.schema == nil || engine.schema.QueryType() == nil {
		return errors.New("sgraph engine schema is nil")
	}

	cacheKey := sGraphEngineCacheKey(*engine.schema)
	defaultSGraphEngineCacheMu.Lock()
	defer defaultSGraphEngineCacheMu.Unlock()

	registered, ok := defaultSGraphEngineCache.Load(cacheKey)
	if ok {
		if registeredEngine, matched := registered.(*SGraphEngine); matched {
			if registeredEngine == engine {
				return nil
			}
			return errors.New("another sgraph engine is already registered for this schema")
		}

		return errors.New("sgraph engine registered for this schema is illegal")
	}
	defaultSGraphEngineCache.Store(cacheKey, engine)
	return nil
}

func (e *SGraphEngine) Execute(document *ast.Document, args map[string]any, operationName *string, root map[string]any, ctx context.Context) *SGraphResult {
	return e.executeWithCache(document, args, operationName, root, ctx, nil)
}

func (e *SGraphEngine) executeWithCache(document *ast.Document, args map[string]any, operationName *string, root map[string]any, ctx context.Context, extensions []Extension) *SGraphResult {
	if e == nil {
		return newSGraphErrorResult(errors.New("sgraph engine is nil"))
	}
	if e.schema == nil || e.schema.QueryType() == nil {
		return newSGraphErrorResult(errors.New("sgraph engine schema is nil"))
	}
	if e.planCache == nil || e.paramRegistry == nil || e.directiveRegistry == nil || e.resultAssembler == nil {
		return newSGraphErrorResult(errors.New("sgraph engine is not initialized"))
	}

	operationDefinition, selectErr := selectOperationDefinition(document, operationName)
	if selectErr != nil {
		return newSGraphErrorResult(selectErr)
	}
	if operationDefinition.Operation != ast.OperationTypeQuery {
		return newSGraphErrorResult(fmt.Errorf("sgraph engine does not support %s operation", operationDefinition.Operation))
	}

	cacheKey := buildPlanCacheKey(document, operationDefinition)
	cachePlan, ok := e.planCache.Load(cacheKey)
	if !ok {
		var buildErr error
		cachePlan, buildErr = compileExecutionPlan(document, e.schema, operationName, e.directiveRegistry, e.paramRegistry)
		if buildErr != nil {
			return newSGraphErrorResult(buildErr)
		}
		e.planCache.LoadOrStore(cacheKey, cachePlan)
	}

	// operation在本方法内已经完成选择，变量补全直接复用，避免再次扫描Document。
	inputs, inputErr := completeVariables(e.schema, operationDefinition, args)
	if inputErr != nil {
		return newSGraphErrorResult(inputErr)
	}

	plan, valid := cachePlan.(*SGraphExecutionPlan)
	if !valid || plan == nil {
		return newSGraphErrorResult(errors.New("sgraph engine returned invalid plan"))
	}

	return e.executePlan(plan, inputs, root, ctx, extensions)
}

func (e *SGraphEngine) createBatches(executionPlan *SGraphExecutionPlan) ([]*BatchPlan, error) {
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
	// ExecutionPlan在编译完成后冻结字段索引，Rundata在本请求内只读引用。
	rundata.executionPlan = plan
	// ResolveInfo 和 extension 所需的请求状态统一由 Rundata 持有，并通过函数参数显式向下传递。
	rundata.schema = e.schema
	//避免出现typed nil
	if root != nil {
		rundata.resolveInfoRootValue = root
	} else {
		rundata.resolveInfoRootValue = nil
	}
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
	treeInterrupted := false
	for _, batch := range batches {
		br := batch.execute(rundata, ctx)
		if br.isInterrupt() {
			treeInterrupted = true
			break
		}
	}
	if treeInterrupted {
		// Tree错误表示执行结构已不可信；不组装部分数据，统一返回data:null和已记录错误。
		rundata.flushPendingBulkBindingErrors()
		result.errors = rundata.getAllFieldErrors()
		result.extensionErrors = rundata.getAllExtensionErrors()
		return result
	}
	// Engine在构造完成后保持只读，共享请求只读取绑定的resultAssembler。
	result = e.resultAssembler.assembleGraphResult(plan, rundata, root, ctx)
	return result
}

func buildPlanCacheKey(document *ast.Document, operationDefinition *ast.OperationDefinition) string {
	return buildDocumentOperationKey(documentIdentityBody(document), operationDefinitionName(operationDefinition))
}

func completeVariables(schema *Schema, operationDefinition *ast.OperationDefinition, args map[string]any) (map[string]any, error) {
	if schema == nil {
		return nil, errors.New("schema is nil")
	}
	if operationDefinition == nil {
		return nil, errors.New("operation definition is nil")
	}
	if args == nil {
		args = map[string]any{}
	}
	return completeOperationVariables(schema, operationDefinition.VariableDefinitions, args)
}

func completeOperationVariables(schema *Schema, variableDefs []*ast.VariableDefinition, originalInputs map[string]any) (map[string]any, error) {
	if originalInputs == nil {
		originalInputs = make(map[string]any)
	}

	result := make(map[string]any)
	for _, variableDef := range variableDefs {
		name := variableDef.Variable.Name.Value

		inputType, inputTypeErr := typeFromAST(*schema, variableDef.Type)
		if inputTypeErr != nil {
			return nil, inputTypeErr
		}

		variableValue, provided := originalInputs[name]
		if !provided {
			if variableDef.DefaultValue != nil {
				result[name] = valueFromAST(variableDef.DefaultValue, inputType, nil)
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
		parsed, parsedErr := parseInputValue(inputType, variableValue)
		if parsedErr != nil {
			message := fmt.Sprintf("Variable \"$%s\": %s", name, parsedErr.Error())
			return nil, gqlerrors.NewError(message, []ast.Node{variableDef}, "", nil, nil, parsedErr)
		}
		result[name] = parsed
	}
	return result, nil
}

func parseInputValue(inputType Input, source any) (any, error) {
	if source != nil {
		sourceValue := reflect.ValueOf(source)

		if sourceValue.Kind() == reflect.Ptr && sourceValue.IsNil() {
			source = nil
		}
	}

	if nonNullType, ok := inputType.(*NonNull); ok {
		if source == nil {
			return nil, fmt.Errorf("nonNull input value is required")
		}

		inner, innerOk := nonNullType.OfType.(Input)
		if !innerOk {
			return nil, fmt.Errorf("nonNull input value is required")
		}
		return parseInputValue(inner, source)
	}

	if source == nil {
		return nil, nil
	}

	switch typedInput := inputType.(type) {
	case *List:
		inner := typedInput.OfType.(Input)

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
	case *InputObject:
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
	case *Scalar:
		parsed := typedInput.ParseValue(source)
		if parsed == nil {
			return nil, fmt.Errorf("expected scalar type %q", typedInput.Name())
		}
		return parsed, nil
	case *Enum:
		parsed := typedInput.ParseValue(source)
		if parsed == nil {
			return nil, fmt.Errorf("expected enum type %q", typedInput.Name())
		}
		return parsed, nil
	default:
		return nil, fmt.Errorf("unexpected input type %T", typedInput)
	}
}

func selectOperationDefinition(document *ast.Document, operationName *string) (*ast.OperationDefinition, error) {
	if document == nil {
		return nil, errors.New("document is nil")
	}
	var selected *ast.OperationDefinition
	for _, definition := range document.Definitions {
		operation, ok := definition.(*ast.OperationDefinition)
		if !ok {
			continue
		}
		if operationName != nil {
			if operation.Name != nil && operation.Name.Value == *operationName {
				return operation, nil
			}
			continue
		}
		if selected != nil {
			return nil, errors.New("operation name is required when document contains multiple operations")
		}
		selected = operation
	}
	if selected != nil {
		return selected, nil
	}
	if operationName != nil {
		return nil, fmt.Errorf("no operation definition found for %s", *operationName)
	}
	return nil, errors.New("operation definition is nil")
}

func getSGraphEngineForSchema(schema Schema) (*SGraphEngine, error) {
	cacheKey := sGraphEngineCacheKey(schema)
	if engine, ok := defaultSGraphEngineCache.Load(cacheKey); ok {
		if registeredEngine, matched := engine.(*SGraphEngine); matched {
			return registeredEngine, nil
		}
	}

	// 只在首次绑定 Schema 时加锁；稳态请求直接走 Lmap 的无锁读取。
	defaultSGraphEngineCacheMu.Lock()
	defer defaultSGraphEngineCacheMu.Unlock()
	if engine, ok := defaultSGraphEngineCache.Load(cacheKey); ok {
		if registeredEngine, matched := engine.(*SGraphEngine); matched {
			return registeredEngine, nil
		}
	}

	engine, err := NewSGraphEngine(&schema, nil, nil)
	if err != nil {
		return nil, err
	}
	defaultSGraphEngineCache.Store(cacheKey, engine)
	return engine, nil
}

func sGraphEngineCacheKey(schema Schema) uint {
	// Schema 是值类型，但值拷贝会共享同一个 typeMap，因此可以用它稳定标识 Schema。
	return uint(reflect.ValueOf(schema.typeMap).Pointer())
}
