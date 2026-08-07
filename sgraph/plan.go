package sgraph

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"sync"
	"sync/atomic"

	"github.com/graphql-go/graphql"
	"github.com/graphql-go/graphql/language/ast"
)

type SchemaResolveInfo struct {
	operation ast.Definition            //请求无关的OperationDefinition，运行期填充ResolveInfo.Operation
	fragments map[string]ast.Definition //请求无关的fragments定义，运行期填充ResolveInfo.Fragments
}

type SGraphExecutionPlan struct {
	roots             []*FieldPlan
	schemaResolveInfo SchemaResolveInfo
	//operation         ast.Definition
	//fragments         map[string]ast.Definition
	maxFieldId uint32
	batches    []*BatchPlan
	batchesMu  sync.RWMutex
}

type FieldElementTypeEnum uint8

const FIELD_ELEMENT_TYPE_SCALAR FieldElementTypeEnum = 1
const FIELD_ELEMENT_TYPE_OBJECT FieldElementTypeEnum = 2
const FIELD_ELEMENT_TYPE_LIST FieldElementTypeEnum = 3
const FIELD_ELEMENT_TYPE_ENUM FieldElementTypeEnum = 4

type FieldWrapperTypeInfo struct {
	notNil                 bool                  //字段封装类是否为非空
	isList                 bool                  //字段封装类是否为List
	fieldElementTypeEnum   FieldElementTypeEnum  //字段类型枚举
	baseType               graphql.Type          //除去封装类型嵌套的原始基本类型
	elementWrapperTypeInfo *FieldWrapperTypeInfo //剥去当前封装类后的子类型
}

type ResolverFunc func(source any, params map[string]any, info graphql.ResolveInfo, ctx context.Context) (any, error)

func wrapFieldResolverFunc(fieldResolveFn graphql.FieldResolveFn) ResolverFunc {
	if fieldResolveFn == nil {
		return nil
	}
	type firstResponseGetter interface {
		GetFirstResponse() any
	}
	return func(source any, params map[string]any, info graphql.ResolveInfo, ctx context.Context) (any, error) {
		actualSource := source
		if wrappedSource, ok := source.(firstResponseGetter); ok {
			actualSource = wrappedSource.GetFirstResponse()
		}
		return fieldResolveFn(graphql.ResolveParams{
			Source:  actualSource,
			Args:    params,
			Info:    info,
			Context: ctx,
		})
	}
}

type FieldDynamicTypeResolveFunction func(value any, ctx *context.Context) string

type FieldTypeScope struct {
	declaredType        any
	allowedDynamicTypes map[string]*graphql.Object
	dynamicTypeResolver FieldDynamicTypeResolveFunction
	staticTypeName      string
}

var TypeNameResolverFunc = func(source any, params map[string]any, info graphql.ResolveInfo, ctx context.Context) (any, error) {
	if params != nil {
		if typeName, ok := params[DefaultFieldKeyTypename].(string); ok {
			return typeName, nil
		}
	}
	return nil, nil
}

// 根据FieldType封装FieldTypeScope
func wrapTypeDefinition2Scope(fieldType graphql.Type, compiler *PlanCompiler) (*FieldTypeScope, error) {
	if fieldType == nil {
		return &FieldTypeScope{}, nil
	}
	switch t := fieldType.(type) {
	case *graphql.Object:
		return wrapStaticFieldTypeScope(t)
	case *graphql.Interface:
		return wrapDynamicFieldTypeScope(compiler, t)
	case *graphql.Union:
		return wrapDynamicFieldTypeScope(compiler, t)
	default:
		return &FieldTypeScope{}, nil
	}
}

// 封装静态编译类型的FieldTypeScope
func wrapStaticFieldTypeScope(nodeType *graphql.Object) (*FieldTypeScope, error) {
	if nodeType == nil {
		return nil, errors.New("no node provided")
	}
	return &FieldTypeScope{
		declaredType: nodeType,
		allowedDynamicTypes: map[string]*graphql.Object{
			nodeType.Name(): nodeType,
		},
		staticTypeName:      nodeType.Name(),
		dynamicTypeResolver: nil,
	}, nil
}

// 封装运行时动态判定的FieldTypeScope
func wrapDynamicFieldTypeScope(compiler *PlanCompiler, nodeType graphql.Abstract) (*FieldTypeScope, error) {
	if compiler == nil {
		return nil, errors.New("no compiler provided")
	}
	possibleTypes := compiler.schema.PossibleTypes(nodeType)
	result := make(map[string]*graphql.Object, len(possibleTypes))
	for _, possibleType := range possibleTypes {
		result[possibleType.Name()] = possibleType
	}
	return &FieldTypeScope{
		declaredType:        nodeType,
		allowedDynamicTypes: result,
		staticTypeName:      nodeType.Name(),
		dynamicTypeResolver: newDynamicTypeResolverFunction(nodeType, compiler),
	}, nil
}

func newDynamicTypeResolverFunction(abs graphql.Abstract, compiler *PlanCompiler) FieldDynamicTypeResolveFunction {
	return func(value any, ctx *context.Context) string {
		if value == nil {
			return ""
		}

		switch t := abs.(type) {
		case *graphql.Interface:
			if t.ResolveType != nil {
				if obj := t.ResolveType(graphql.ResolveTypeParams{Value: value, Context: *ctx}); obj != nil {
					return obj.Name()
				}
			}
		case *graphql.Union:
			if t.ResolveType != nil {
				if obj := t.ResolveType(graphql.ResolveTypeParams{Value: value, Context: *ctx}); obj != nil {
					return obj.Name()
				}
			}
		}

		for _, possible := range compiler.schema.PossibleTypes(abs) {
			if possible.IsTypeOf == nil {
				continue
			}
			if possible.IsTypeOf(graphql.IsTypeOfParams{Value: value, Context: *ctx}) {
				return possible.Name()
			}
		}
		return ""
	}
}

func cropFieldTypeScope(compiler *PlanCompiler, parentScope *FieldTypeScope, typeCondition *ast.Named) (*FieldTypeScope, error) {
	//参数检查
	if typeCondition == nil || typeCondition.Name == nil || typeCondition.Name.Value == "" {
		return nil, errors.New("no type condition provided")
	}
	//校验typeCondition是否存在
	namedType := compiler.schema.Type(typeCondition.Name.Value)
	if namedType == nil {
		return nil, fmt.Errorf("no type name found for %s", typeCondition.Name.Value)
	}

	var possibleTypes map[string]*graphql.Object
	switch tt := namedType.(type) {
	case *graphql.Object:
		possibleTypes = map[string]*graphql.Object{tt.Name(): tt}
	case graphql.Abstract:
		possibleTypes = map[string]*graphql.Object{}
		for _, pt := range compiler.schema.PossibleTypes(tt) {
			possibleTypes[pt.Name()] = pt
		}
	default:
		return nil, fmt.Errorf("unknown type condition found for %s", typeCondition.Name.Value)
	}

	//基于possibleTypes对parentScope进行裁剪
	croppedTypeMap := make(map[string]*graphql.Object)
	for name, obj := range parentScope.allowedDynamicTypes {
		if _, ok := possibleTypes[name]; ok {
			croppedTypeMap[name] = obj
		}
	}

	if len(croppedTypeMap) == 0 {
		return nil, fmt.Errorf("no type condition matched for %s", typeCondition.Name.Value)
	}
	return &FieldTypeScope{
		declaredType:        typeCondition,
		allowedDynamicTypes: croppedTypeMap,
		staticTypeName:      parentScope.staticTypeName,
		dynamicTypeResolver: parentScope.dynamicTypeResolver,
	}, nil
}

type FieldPlan struct {
	//字段身份与结果树
	fieldId        uint32   //字段自增ID，从1开始
	parentFieldId  uint32   //父字段ID，root节点该字段为0
	fieldName      string   //schema中的字段名称
	responseName   string   //组装最后结果时的字段名称，如果有alias就使用alias
	paths          []string //从根到当前字段的静态responseName路径（含alias）
	childrenFields []*FieldPlan

	//Graphql类型和ResolveInfo元数据
	fieldASTs            []*ast.Field         //同一responseName合并后的原始fieldAST，供 esolveInfo.FieldASTs使用
	returnType           graphql.Output       //schema 中声明的字段返回类型，供ResolveInfo.ReturnType使用
	fieldWrapperTypeInfo FieldWrapperTypeInfo //字段返回值的封装类型元信息
	fieldTypeScope       *FieldTypeScope      //类型判定范围
	parentType           graphql.Composite    //字段所属的父类型，供ResolveInfo.ParentType和extension hook使用

	//Resolver和参数依赖
	paramPlans       []*ParamPlan //单次参数计划List，NormalStep和IteratorStep中遍历模式执行时使用
	bulkParamPlans   []*ParamPlan //批量参数计划，IteratorStep中批量模式执行时使用
	resolverFunc     ResolverFunc //单次执行Resolver方法，NormalStep和IteratorStep中遍历模式执行时使用
	bulkResolverFunc ResolverFunc //批量执行Resolver方法，IteratorStep中批量模式执行时使用

	//父子结果映射
	parentKeyFieldName  string //单次/遍历调用时用于标识父节点关联关系的字段名，默认是父节点中的id
	resultParentKeyName string //批量调用时返回值中代表父节点映射key的字段name，获得返回结果后要根据这个字段name获取value并作为父子映射map的key

	//Directive
	directivePlans             []*DirectivePlan
	directiveParamPlans        []*ParamPlan       //Directives依赖的参数
	skipIncludeDirectiveGroups [][]*DirectivePlan //同一responseName多次出现时，每次出现自己的 @skip/@include 条件组
}

func (fieldplan *FieldPlan) getResponseName() string {
	return fieldplan.responseName
}

func (fieldplan *FieldPlan) getFieldName() string {
	return fieldplan.fieldName
}

func (fieldplan *FieldPlan) getChildrenFields() []*FieldPlan {
	return fieldplan.childrenFields
}

func (fieldplan *FieldPlan) isIntrospectionTypeNameField() bool {
	return fieldplan.fieldName == IntrospectionFieldNameTypename
}

type DirectiveExecutionStage uint8

const DIRECTIVE_STAGE_METADATA_ONLY DirectiveExecutionStage = 1
const DIRECTIVE_STAGE_SHOULD_EXECUTE DirectiveExecutionStage = 2
const DIRECTIVE_STAGE_BEFORE_RESOLVE DirectiveExecutionStage = 3
const DIRECTIVE_STAGE_AFTER_RESOLVE DirectiveExecutionStage = 4

type DirectivePlan struct {
	name           string                  //指令名
	location       string                  //指令出现位置
	argsRaw        map[string]any          //指令原始参数集
	argsPlans      []*ParamPlan            //指令参数执行计划列表
	stage          DirectiveExecutionStage //指令执行阶段类型
	runtimeHandler DirectiveRuntimeHandler
}

type ParamTypeEnum uint8

const PARAM_TYPE_ENUM_CONST ParamTypeEnum = 1                    //常量类型
const PARAM_TYPE_ENUM_INPUT ParamTypeEnum = 2                    //输入变量类型
const PARAM_TYPE_ENUM_FIELD_RESPONSE_ATTRIBUTE ParamTypeEnum = 3 //依赖节点数据类型
const PARAM_TYPE_ENUM_FIELD_RESPONSE_RAW ParamTypeEnum = 4       //依赖节点执行结果类型，仅用于编排时固定顺序
const PARAM_TYPE_ENUM_VAR_TEMPLATE ParamTypeEnum = 5             //输入模板类型

type ParamPlan struct {
	paramKey           string        //形参名
	paramType          ParamTypeEnum //参数类型
	constValue         any           //常量值，仅在常量类型参数里使用
	inputName          string        //实参名，仅在输入型参数里使用
	inputDefaultValue  any           //入参默认值，仅在输入性参数且本次请求未提供变量值时使用
	dependentFieldId   uint32
	fieldResponsePaths []string
	templateAST        ast.Value     //含变量的复合实参AST模板（只存结构，不存请求值）
	templateType       graphql.Input //该实参的输入类型，校验时使用
}

// 解析"仅依赖变量/常量"的参数（Const/Input/VariableTemplate）。
// handled=false表示本次请求不应写入该参数，或该参数需交调用方按step形态处理。
func (pp *ParamPlan) resolveFromInputs(originalInputs map[string]any) (value any, handled bool, err error) {
	switch pp.paramType {
	case PARAM_TYPE_ENUM_INPUT:
		if val, ok := originalInputs[pp.paramKey]; ok {
			return val, true, nil
		}
		if pp.inputDefaultValue != nil {
			return pp.inputDefaultValue, true, nil
		}
		return nil, false, nil
	case PARAM_TYPE_ENUM_CONST:
		return pp.constValue, true, nil
	case PARAM_TYPE_ENUM_VAR_TEMPLATE:
		val, err := pp.materializeTemplate(originalInputs)
		return val, true, err
	default:
		return nil, false, nil
	}
}

// materializeTemplate 用本请求变量把模板物化为最终值（复用 valueFromAST 协变/校验）。
func (pp *ParamPlan) materializeTemplate(originalInputs map[string]any) (any, error) {
	if pp == nil || pp.templateAST == nil {
		return nil, nil
	}
	return valueFromAST(pp.templateAST, pp.templateType, originalInputs)
}

// newConstParamPlan 构造一个常量类型的 ParamPlan。
func newConstParamPlan(paramKey string, constValue any) *ParamPlan {
	return &ParamPlan{
		paramKey:   paramKey,
		paramType:  PARAM_TYPE_ENUM_CONST,
		constValue: constValue,
	}
}

// newInputParamPlan 构造一个来自原始输入的 ParamPlan。paramKey是形参名，inputName是实参名。
func newInputParamPlan(paramKey, inputName string) *ParamPlan {
	return &ParamPlan{
		paramKey:  paramKey,
		paramType: PARAM_TYPE_ENUM_INPUT,
		inputName: inputName,
	}
}

// newVariableTemplateParamPlan 构造"含变量的复合实参"计划：只存 AST 模板与类型，运行期物化。
func newVariableTemplateParamPlan(paramKey string, templateAST ast.Value, templateType graphql.Input) *ParamPlan {
	return &ParamPlan{
		paramKey:     paramKey,
		paramType:    PARAM_TYPE_ENUM_VAR_TEMPLATE,
		templateAST:  templateAST,
		templateType: templateType,
	}
}

// 构造一个依赖父节点完整结果的参数，通常用于指定父子节点的执行顺序而非使用父节点的response作为参数来源
func newFieldResponseRawParamPlan(dependentFieldId uint32) *ParamPlan {
	return &ParamPlan{
		paramType:        PARAM_TYPE_ENUM_FIELD_RESPONSE_RAW,
		dependentFieldId: dependentFieldId,
	}
}

type BatchPlan struct {
	batchId    uint32 //批次ID
	concurrent bool   //是否并发执行step
	steps      []Step //当前批次的所有step
}

type BatchResult struct {
	interrupt bool
}

func (br *BatchResult) isInterrupt() bool {
	return br.interrupt
}

var (
	// BatchResult 是只读结果对象，复用静态实例减少每个 batch 的小对象分配。
	batchResultContinue  = &BatchResult{interrupt: false}
	batchResultInterrupt = &BatchResult{interrupt: true}
)

func (b *BatchPlan) execute(rundata *Rundata, ctx context.Context) *BatchResult {
	if b.concurrent && len(b.steps) >= sGraphConcurrentStepMin {
		//并发执行step
		semaphore := atomic.Uint32{}
		wg := sync.WaitGroup{}
		wg.Add(len(b.steps))

		for _, step := range b.steps {
			go func(step Step) {
				// step 作为参数传入 goroutine，避免闭包捕获循环变量导致执行错 step。
				fieldErr := step.Execute(rundata, ctx)
				if fieldErr != nil && fieldErr.errorType == FieldErrorTypeTree {
					semaphore.Add(1)
				}
				wg.Done()
			}(step)
		}

		wg.Wait()
		//判断是否有需要中断整个流程的错误
		if semaphore.Load() >= 1 {
			return batchResultInterrupt
		}
	} else {
		//串行执行step
		for _, step := range b.steps {
			fieldErr := step.Execute(rundata, ctx)
			if fieldErr != nil && fieldErr.errorType == FieldErrorTypeTree {
				//判断是否有需要中断整个流程的错误
				return batchResultInterrupt
			}
		}
	}
	return batchResultContinue
}

type Step interface {
	Execute(rundata *Rundata, ctx context.Context) *FieldError
}

type SingleCallStep struct {
	fieldPlan *FieldPlan
}

func (s *SingleCallStep) Execute(rundata *Rundata, ctx context.Context) *FieldError {
	if s.fieldPlan != nil {
		//先判断skip/include指令是否执行
		include, includeErr := evaluateSkipIncludeDirectivesShouldExecuteField(s.fieldPlan, rundata, ctx)
		if includeErr != nil {
			return rundata.addFieldError(s.fieldPlan.fieldId, FieldErrorTypeField, includeErr, s.fieldPlan.paths)
		}
		//如果Directive的结果是不执行，直接返回
		if !include {
			return nil
		}

		if !fieldDependenciesAvailable(s.fieldPlan, rundata) {
			return nil
		}

		//组装参数
		paramContext, paramContextErr := prepareSingleCallFieldParams(s.fieldPlan.paramPlans, rundata)
		if paramContextErr != nil || paramContext == nil {
			return rundata.addFieldError(s.fieldPlan.fieldId, FieldErrorTypeField, paramContextErr, s.fieldPlan.paths)
		}

		_, fieldErr := innerStepCalling(s.fieldPlan, paramContext, s.fieldPlan.resolverFunc, nil, rundata, ctx)
		if fieldErr != nil {
			return fieldErr
		}
	}
	return nil
}

type IterationCallStep struct {
	fieldPlan *FieldPlan
}

func (i *IterationCallStep) Execute(rundata *Rundata, ctx context.Context) *FieldError {
	if i.fieldPlan != nil {
		//先判断skip/include指令是否执行
		include, includeErr := evaluateSkipIncludeDirectivesShouldExecuteField(i.fieldPlan, rundata, ctx)
		if includeErr != nil {
			return rundata.addFieldError(i.fieldPlan.fieldId, FieldErrorTypeField, includeErr, i.fieldPlan.paths)
		}
		//如果Directive的结果是不执行，直接返回
		if !include {
			return nil
		}

		if !fieldDependenciesAvailable(i.fieldPlan, rundata) {
			return nil
		}

		//优先执行bulk模式，不行回退到iteration模式
		if bulkResolverFunc := i.fieldPlan.bulkResolverFunc; bulkResolverFunc != nil {
			bulkParamContext, bulkParamContextErr := prepareBulkCallFieldParams(i.fieldPlan, rundata, ctx)
			if bulkParamContextErr != nil {
				return rundata.addFieldError(i.fieldPlan.fieldId, FieldErrorTypeField, bulkParamContextErr, i.fieldPlan.paths)
			}
			fieldRes, fieldErr := innerStepCalling(i.fieldPlan, bulkParamContext, bulkResolverFunc, nil, rundata, ctx)
			if fieldErr != nil {
				return fieldErr
			}
			boundErr := i.bindBulkResponse2ParentResponseWithCompositeKey(fieldRes, rundata, ctx)
			if boundErr != nil {
				return boundErr
			}
			return nil
		}
		//iteration模式
		iterationParamContextList, iterationParamContextErr := prepareIterationCallFieldParams(i.fieldPlan, rundata, ctx)
		if iterationParamContextErr != nil {
			return rundata.addFieldError(i.fieldPlan.fieldId, FieldErrorTypeField, iterationParamContextErr, i.fieldPlan.paths)
		}
		fieldResponse := acquireFieldResponse()
		var fieldErr *FieldError
		for _, iterationParamContext := range iterationParamContextList {
			fieldResponse, fieldErr = innerStepCalling(i.fieldPlan, &iterationParamContext, i.fieldPlan.resolverFunc, fieldResponse, rundata, ctx)
			if fieldErr != nil {
				return fieldErr
			}
		}
		return i.bindBulkResponse2ParentResponseWithCompositeKey(fieldResponse, rundata, ctx)

	}
	return nil
}

func (i *IterationCallStep) bindBulkResponse2ParentResponseWithCompositeKey(fieldResponse *FieldResponse, rundata *Rundata, ctx context.Context) *FieldError {
	if fieldResponse == nil {
		return rundata.addFieldError(i.fieldPlan.fieldId, FieldErrorTypeField, fmt.Errorf("field response is nil"), i.fieldPlan.paths)
	}
	responseList := fieldResponse.responseRaws
	if responseList == nil {
		return nil
	}
	for _, response := range responseList {
		//针对每个元素就是object的情况
		mapValue, mapOk := response.(map[string]any)
		if mapOk {
			parentFieldKeyName := i.fieldPlan.parentKeyFieldName
			compositeKey := generateCompositeKey([]string{parentFieldKeyName}, mapValue)
			if i.fieldPlan.fieldWrapperTypeInfo.isList {
				binded, existed := fieldResponse.lookResponseByCompositeKey(compositeKey)
				if existed {
					// 允许一个key对应多个子结果，同key必须追加而不是覆盖
					if listValues, ok := binded.([]any); ok {
						listValues = append(listValues, mapValue)
						fieldResponse.bindBulkResponsesWithCompositeKey(compositeKey, listValues)
					} else {
						fieldResponse.bindBulkResponsesWithCompositeKey(compositeKey, []any{binded, mapValue})
					}
				} else {
					fieldResponse.bindBulkResponsesWithCompositeKey(compositeKey, []any{mapValue})
				}
			} else {
				if len(responseList) > 1 {
					return rundata.addFieldError(i.fieldPlan.fieldId, FieldErrorTypeField, fmt.Errorf("field responses is list while type is not list %s", i.fieldPlan.fieldName), i.fieldPlan.paths)
				}
				fieldResponse.bindBulkResponsesWithCompositeKey(compositeKey, mapValue)
			}
		}
		//针对每个元素是一个一维list的情况
		listValue, listOk := response.([]any)
		if listOk {
			if len(listValue) > 0 {
				//取首个元素来组装composite key
				firstElement := listValue[0]
				if firstElementMap, ok := firstElement.(map[string]any); ok {
					compositeKey := generateCompositeKey([]string{i.fieldPlan.parentKeyFieldName}, firstElementMap)
					if i.fieldPlan.fieldWrapperTypeInfo.isList {
						bound, existed := fieldResponse.lookResponseByCompositeKey(compositeKey)
						if existed {
							if boundList, boundListOk := bound.([]any); boundListOk {
								fieldResponse.bindBulkResponsesWithCompositeKey(compositeKey, append(boundList, listValue...))
							} else {
								merged := []any{bound}
								merged = append(merged, listValue...)
								fieldResponse.bindBulkResponsesWithCompositeKey(compositeKey, merged)
							}
						} else {
							fieldResponse.bindBulkResponsesWithCompositeKey(compositeKey, listValue)
						}
					} else {
						fieldResponse.bindBulkResponsesWithCompositeKey(compositeKey, listValue)
					}
				} else {
					//只支持一维List，否则就报错
					return rundata.addFieldError(i.fieldPlan.fieldId, FieldErrorTypeField, fmt.Errorf("response data type not supported"), i.fieldPlan.paths)
				}
			}
		}
	}
	return nil
}

func innerStepCalling(fieldPlan *FieldPlan, paramContext ParamContext, resolverFn ResolverFunc, fieldResponse *FieldResponse, rundata *Rundata, ctx context.Context) (*FieldResponse, *FieldError) {
	//判断其他指令的计算结果当前字段是否执行
	shouldExecute, directiveEvaluateErr := evaluateCommonDirectivesShouldExecuteField(fieldPlan, paramContext, rundata, ctx)
	if directiveEvaluateErr != nil {
		return nil, rundata.addFieldError(fieldPlan.fieldId, FieldErrorTypeField, directiveEvaluateErr, fieldPlan.paths)
	}
	if !shouldExecute {
		return nil, nil
	}

	//动态类型判定
	shouldExecute = evaluateTypeShouldExecuteField(fieldPlan, paramContext.getParentResponse(), ctx)
	if !shouldExecute {
		return nil, nil
	}

	//FieldResponse
	if fieldResponse == nil {
		fieldResponse = acquireFieldResponse()
	}

	//执行BeforeResolve指令
	if beforeResolvedParams, beforeResolvedParamsErr := applyBeforeResolveDirectives(fieldPlan, paramContext, rundata, ctx); beforeResolvedParamsErr != nil {
		releaseFieldResponse(fieldResponse)
		return nil, rundata.addFieldError(fieldPlan.fieldId, FieldErrorTypeField, beforeResolvedParamsErr, fieldPlan.paths)
	} else {
		paramContext.setParams(beforeResolvedParams)
	}

	//调用resolver前出发extension hook
	fieldCtx, info, fieldHook := startSGraphResolveFieldHook(rundata, fieldPlan, ctx, -1)

	//resolver方法调用
	res, err := execResolveProcess(fieldPlan, paramContext.getParentResponse(), paramContext.getParams(), resolverFn, info, fieldCtx)

	//执行完不论成功与否，调用extension hook
	finishSGraphResolveFieldHook(rundata, fieldHook, res, err)

	if err != nil {
		releaseFieldResponse(fieldResponse)
		return nil, rundata.addFieldError(fieldPlan.fieldId, FieldErrorTypeField, err, fieldPlan.paths)
	}

	//执行AfterResolve指令
	afterResolvedResponse, afterResolvedErr := applyAfterResolveDirectives(fieldPlan, paramContext, res, rundata, ctx)
	if afterResolvedErr != nil {
		releaseFieldResponse(fieldResponse)
		return nil, rundata.addFieldError(fieldPlan.fieldId, FieldErrorTypeField, afterResolvedErr, fieldPlan.paths)
	}
	res = afterResolvedResponse

	//处理null值冒泡
	nilBubbled, nilBubbledErr := processNullValueBubbling(fieldPlan.fieldWrapperTypeInfo, res)
	if nilBubbledErr != nil {
		releaseFieldResponse(fieldResponse)
		return nil, rundata.addFieldError(fieldPlan.fieldId, FieldErrorTypeField, nilBubbledErr, fieldPlan.paths)
	}
	if nilBubbled != nil {
		if fieldPlan.fieldWrapperTypeInfo.isList {
			fieldResponse.responseRaws = append(fieldResponse.responseRaws, nilBubbled...)
		} else {
			if len(nilBubbled) > 0 {
				fieldResponse.responseRaws = append(fieldResponse.responseRaws, nilBubbled[0])
			}
		}
		rundata.setFieldResponse(fieldPlan.fieldId, fieldResponse)
	}
	return fieldResponse, nil
}

func applyBeforeResolveDirectives(fieldPlan *FieldPlan, paramContext ParamContext, rundata *Rundata, ctx context.Context) (map[string]any, error) {
	var fieldArgs map[string]any
	for _, dp := range fieldPlan.directivePlans {
		if dp.stage == DIRECTIVE_STAGE_BEFORE_RESOLVE {
			if fieldArgs == nil {
				fieldArgs = make(map[string]any, len(paramContext.getParams()))
				maps.Copy(fieldArgs, paramContext.getParams())
			}

			args, argsErr := materializeDirectiveArgs(dp, rundata)
			if argsErr != nil {
				return nil, argsErr
			}
			beforeResolvedResult, beforeResolvedErr := dp.runtimeHandler.BeforeResolve(fieldPlan, args, fieldArgs, paramContext.getParentResponse(), rundata.originalParams, ctx)
			if beforeResolvedErr != nil {
				return nil, beforeResolvedErr
			}
			fieldArgs = beforeResolvedResult
		}
	}
	return fieldArgs, nil
}

func applyAfterResolveDirectives(fieldPlan *FieldPlan, paramContext ParamContext, currentFieldResponseRaw any, rundata *Rundata, ctx context.Context) (any, error) {
	resolvedResponse := currentFieldResponseRaw
	for _, dp := range fieldPlan.directivePlans {
		if dp.stage == DIRECTIVE_STAGE_AFTER_RESOLVE {
			args, argsErr := materializeDirectiveArgs(dp, rundata)
			if argsErr != nil {
				return nil, argsErr
			}
			next, err := dp.runtimeHandler.AfterResolve(fieldPlan, args, paramContext.getParams(), paramContext.getParentResponse(), resolvedResponse, rundata.originalParams, ctx)
			if err != nil {
				return nil, err
			}
			resolvedResponse = next
		}
	}
	return resolvedResponse, nil
}

func execResolveProcess(fieldPlan *FieldPlan, source any, params map[string]any, resolverFn ResolverFunc, info graphql.ResolveInfo, ctx context.Context) (any, error) {
	//__typename特殊处理
	if fieldPlan.isIntrospectionTypeNameField() {
		if fieldPlan.fieldTypeScope != nil && fieldPlan.fieldTypeScope.dynamicTypeResolver != nil {
			typeName := fieldPlan.fieldTypeScope.dynamicTypeResolver(source, &ctx)
			if typeName == "" {
				err := errors.New("__typename resolved failed, value is empty")
				return nil, err
			}
			return typeName, nil
		}
		return nil, fmt.Errorf("field resolver is nil for __typename field")
	}

	//普通字段
	if resolverFn == nil {
		resolverFn = fieldPlan.resolverFunc
	}
	if resolverFn == nil {
		return nil, fmt.Errorf("resolver is nil for field %s", fieldPlan.fieldName)
	}
	return resolverFn(nil, params, info, ctx)
}

// 根据FieldTypeScope中的静态编译类型和运行时类型推断方法判断当前的Field是否与父对象的Type相符，不相符则返回false
func evaluateTypeShouldExecuteField(fieldPlan *FieldPlan, parentFieldResponseRaw any, ctx context.Context) bool {
	shouldExecute := false
	if isFieldPlanTypeCompiled(fieldPlan) {
		shouldExecute = evaluateCompiledTypeShouldExecuteField(fieldPlan, ctx)
	} else {
		if parentFieldResponseRaw == nil {
			return shouldExecute
		}
		shouldExecute = evaluateDynamicTypeShouldExecuteField(fieldPlan, parentFieldResponseRaw, ctx)
	}
	return shouldExecute
}

// 执行skip和include指令，并根据结果判断当前Field是否执行方法调用
func evaluateSkipIncludeDirectivesShouldExecuteField(fieldPlan *FieldPlan, rundata *Rundata, ctx context.Context) (bool, error) {
	if fieldPlan == nil {
		return false, nil
	}
	skipIncludeDirectivesGroup := fieldPlan.skipIncludeDirectiveGroups
	if len(skipIncludeDirectivesGroup) == 0 {
		return true, nil
	}
	for _, group := range skipIncludeDirectivesGroup {
		groupInclude := true
		for _, directive := range group {
			if directive == nil {
				continue
			}
			if directive.runtimeHandler == nil {
				return false, fmt.Errorf("runtime handler for directive %s is nil", directive.name)
			}
			//物化指令参数
			directiveArgs, argsErr := materializeDirectiveArgs(directive, rundata)
			if argsErr != nil {
				return false, argsErr
			}
			shouldExecute, shouldExecuteErr := directive.runtimeHandler.ShouldExecute(fieldPlan, directiveArgs, nil, nil, rundata.originalParams, ctx)
			if shouldExecuteErr != nil {
				return false, shouldExecuteErr
			}
			if !shouldExecute {
				groupInclude = false
				break
			}
		}
		//一个occurrence的指令按照and逻辑处理
		if groupInclude {
			return true, nil
		}
	}
	return false, nil
}

// 执行普通指令，根据结果判断当前Field是否执行方法调用。同一个字段上的普通directives按照AND逻辑处理结果，不包含skip和include指令
func evaluateCommonDirectivesShouldExecuteField(fieldPlan *FieldPlan, paramContext ParamContext, rundata *Rundata, ctx context.Context) (bool, error) {
	if fieldPlan == nil {
		return false, fmt.Errorf("field plan is nil")
	}
	if paramContext == nil {
		return false, fmt.Errorf("paramContext is nil")
	}
	if rundata == nil {
		return false, fmt.Errorf("rundata is nil")
	}

	for _, directive := range fieldPlan.directivePlans {
		if directive == nil {
			return false, fmt.Errorf("%s directive is nil ", directive.name)
		}
		//筛选出类型为should execute的指令
		if directive.stage != DIRECTIVE_STAGE_SHOULD_EXECUTE {
			continue
		}
		if directive.runtimeHandler == nil {
			return false, fmt.Errorf("directive %s has no runtimeHandler ", directive.name)
		}
		directiveArgs, argsErr := materializeDirectiveArgs(directive, rundata)
		if argsErr != nil {
			return false, argsErr
		}

		shouldExecute, err := directive.runtimeHandler.ShouldExecute(fieldPlan, directiveArgs, paramContext.getParams(), paramContext.getParentResponse(), rundata.originalParams, ctx)
		if err != nil {
			return false, err
		}
		//AND逻辑，有一个false就返回false
		if !shouldExecute {
			return false, nil
		}
	}
	return true, nil
}

// 根据编译期静态类型判断当前字段是否应执行方法调用
func evaluateCompiledTypeShouldExecuteField(fp *FieldPlan, ctx context.Context) bool {
	_, ok := fp.fieldTypeScope.allowedDynamicTypes[fp.fieldTypeScope.staticTypeName]
	return ok
}

// 根据运行时动态类型推断当前字段是否应执行方法调用
func evaluateDynamicTypeShouldExecuteField(fp *FieldPlan, parentResponse any, ctx context.Context) bool {
	if fp == nil || fp.fieldTypeScope == nil || len(fp.fieldTypeScope.allowedDynamicTypes) == 0 {
		return true
	}
	if fp.fieldTypeScope.dynamicTypeResolver != nil {
		typename := fp.fieldTypeScope.dynamicTypeResolver(parentResponse, &ctx)
		if typename == "" {
			return false
		}
		_, existed := fp.fieldTypeScope.allowedDynamicTypes[typename]
		return existed
	}
	return true
}

func evaluateRuntimeAllowedTypeShouldExecuteField(fp *FieldPlan, typeName string) bool {
	if fp == nil || len(fp.fieldTypeScope.allowedDynamicTypes) == 0 {
		return true
	}
	if typeName == "" {
		return false
	}
	return fp.fieldTypeScope.allowedDynamicTypes[typeName] != nil
}

// 物化指令参数
func materializeDirectiveArgs(directivePlan *DirectivePlan, rundata *Rundata) (map[string]any, error) {
	if directivePlan == nil {
		return nil, fmt.Errorf("directive plan is nil")
	}
	if rundata == nil {
		return nil, fmt.Errorf("rundata is nil")
	}
	directiveArgsPlans := directivePlan.argsPlans
	if len(directiveArgsPlans) == 0 {
		return nil, nil
	}
	args := make(map[string]any, len(directiveArgsPlans))
	for _, argPlan := range directiveArgsPlans {
		if argPlan == nil {
			return nil, fmt.Errorf("directive args plan is nil %s", directivePlan.name)
		}
		val, handled, err := argPlan.resolveFromInputs(rundata.originalParams)
		if err != nil {
			return nil, err
		}
		if !handled {
			if argPlan.paramType == PARAM_TYPE_ENUM_INPUT {
				//可选变量未提供且没有默认值时，该指令参数应该保持省略
				continue
			}

			return nil, fmt.Errorf("directive args plan has invalid param type %s", argPlan.paramType)
		}

		args[argPlan.paramKey] = val
	}
	return args, nil
}

func processNullValueBubbling(typeInfo FieldWrapperTypeInfo, fieldResponse any) ([]any, error) {
	value, _, err := innerProcessNullValueBubbling(typeInfo, fieldResponse)
	return value, err
}

// TODO 检查逻辑是否正确
// 仅在元素需要规范化或发生null值冒泡时复制list， 正常的[]any返回原切片，避免每个list字段都重新分配并复制全部元素。
// 处理null值冒泡，[]any为处理后的值，bool为原始数据是否已修改，error为报错信息
func innerProcessNullValueBubbling(typeInfo FieldWrapperTypeInfo, fieldResponse any) ([]any, bool, error) {
	//对于nil和typed nil的response进行处理
	if isNilInterfaceValue(fieldResponse) {
		//schema非空却为nil，报错
		if typeInfo.notNil {
			return nil, true, fmt.Errorf("non-null response is nil")
		}
		//schema可以空且为空List，返回nil不冒泡
		if typeInfo.isList {
			return nil, true, nil
		}
		//schema可以空且值为nil或typed nil且封装类型非List，返回{nil}不冒泡
		return []any{nil}, true, nil
	}

	//如果当前封装类型不是List，直接返回结束
	if !typeInfo.isList {
		return []any{fieldResponse}, true, nil
	}

	//针对封装类型是List且通过非空校验且当前层级不为nil的转换成slice处理
	list, ok := asListValue(fieldResponse)
	if !ok {
		return nil, true, fmt.Errorf("list value is not a list")
	}

	var result []any
	_, fieldResponseIsAnySlice := fieldResponse.([]any)
	if !fieldResponseIsAnySlice {
		result = list
	}
	responseChanged := !fieldResponseIsAnySlice

	//如果List中的元素封装类型不是List
	elementTypeInfo := typeInfo.elementWrapperTypeInfo
	if elementTypeInfo == nil {
		return nil, true, fmt.Errorf("element type info is nil")
	}
	if !elementTypeInfo.isList {
		for i, el := range list {
			if isNilInterfaceValue(el) {
				//如果元素封装类型要求非空，直接报错
				if elementTypeInfo.notNil {
					return nil, true, fmt.Errorf("element %d is nil", i)
				}

				//对于typed nil，要转换成nil
				if el != nil {
					if result == nil {
						result = make([]any, len(list))
						copy(result, list)
					}
					result[i] = nil
					responseChanged = true
				}
			}
		}
		if result == nil {
			result = list
		}
		return result, responseChanged, nil
	}

	//如果List中元素的封装类型还是List，要递归处理
	var firstErr error
	for i, el := range list {
		_, elChanged, elErr := innerProcessNullValueBubbling(*elementTypeInfo, el)
		if elErr != nil {
			if firstErr == nil {
				firstErr = elErr
			}
			//子元素报错，如果schema要求非空则直接报错返回
			if elementTypeInfo.notNil {
				return nil, true, firstErr
			}
			//schema允许空则将结果置为nil，设置数据已修改
			elChanged = true
		}

		if elChanged {
			if result == nil {
				result = make([]any, len(list))
				copy(result, list)
			}
			result[i] = nil
			responseChanged = true
		}
	}
	if result == nil {
		result = list
	}
	return result, responseChanged, firstErr
}

func fieldDependenciesAvailable(fieldPlan *FieldPlan, rundata *Rundata) bool {
	if fieldPlan == nil || rundata == nil {
		return false
	}

	for _, paramPlans := range [3][]*ParamPlan{
		fieldPlan.paramPlans,
		fieldPlan.bulkParamPlans,
		fieldPlan.directiveParamPlans,
	} {
		for _, paramPlan := range paramPlans {
			if paramPlan == nil {
				continue
			}

			paramType := paramPlan.paramType
			if paramType != PARAM_TYPE_ENUM_FIELD_RESPONSE_ATTRIBUTE && paramType != PARAM_TYPE_ENUM_FIELD_RESPONSE_RAW {
				continue
			}

			dependentFieldId := paramPlan.dependentFieldId
			if uint64(dependentFieldId) >= uint64(len(rundata.fieldResponses)) {
				return false
			}

			fieldResponse := rundata.getFieldResponseByFieldId(dependentFieldId)

			if fieldResponse == nil || len(fieldResponse.responseRaws) == 0 {
				return false
			}
		}
	}
	return true
}

type ParamContext interface {
	getParams() map[string]any
	getParentResponse() any
	getParentResponseList() []any
	setParams(map[string]any)
}

type SingleCallStepParamContext struct {
	params         map[string]any
	parentResponse any
}

func (pc *SingleCallStepParamContext) getParams() map[string]any {
	return pc.params
}

func (pc *SingleCallStepParamContext) getParentResponse() any {
	return pc.parentResponse
}

func (pc *SingleCallStepParamContext) getParentResponseList() []any {
	return []any{pc.parentResponse}
}

func (pc *SingleCallStepParamContext) setParams(m map[string]any) {
	pc.params = m
}

func prepareSingleCallFieldParams(paramPlans []*ParamPlan, rundata *Rundata) (*SingleCallStepParamContext, error) {
	if rundata == nil {
		return nil, fmt.Errorf("rundata is nil")
	}
	result := &SingleCallStepParamContext{}
	params := make(map[string]any, len(paramPlans))
	for _, pp := range paramPlans {
		if pp == nil {
			return nil, fmt.Errorf("single call param plan is nil")
		}
		val, handled, err := pp.resolveFromInputs(rundata.originalParams)
		if err != nil {
			return nil, err
		}
		if handled {
			params[pp.paramKey] = val
			continue
		}

		switch pp.paramType {
		case PARAM_TYPE_ENUM_INPUT:
			continue
		case PARAM_TYPE_ENUM_FIELD_RESPONSE_ATTRIBUTE:
			if uint64(pp.dependentFieldId) >= uint64(len(rundata.fieldResponses)) {
				return nil, fmt.Errorf("dependent field id %d is out of range", pp.dependentFieldId)
			}
			parentResponseRaw := rundata.getFieldResponseByFieldId(pp.dependentFieldId)
			if parentResponseRaw == nil || len(parentResponseRaw.responseRaws) == 0 {
				return nil, fmt.Errorf("parent response is nil for field %s", pp.dependentFieldId)
			}
			if len(parentResponseRaw.responseRaws) != 1 {
				return nil, fmt.Errorf("parent response has more than one response for field %s", pp.dependentFieldId)
			}
			params[pp.paramKey] = getValueFromMapByPaths(parentResponseRaw.responseRaws[0], pp.fieldResponsePaths)
		case PARAM_TYPE_ENUM_FIELD_RESPONSE_RAW:
			if uint64(pp.dependentFieldId) >= uint64(len(rundata.fieldResponses)) {
				return nil, fmt.Errorf("dependent field id %d is out of range", pp.dependentFieldId)
			}
			parentResponseRaw := rundata.getFieldResponseByFieldId(pp.dependentFieldId)
			if parentResponseRaw == nil || len(parentResponseRaw.responseRaws) == 0 {
				continue
			}
			if len(parentResponseRaw.responseRaws) != 1 {
				return nil, fmt.Errorf("parent response has more than one response for field %s", pp.dependentFieldId)
			}
			result.parentResponse = parentResponseRaw.responseRaws[0]
		default:
			return nil, fmt.Errorf("single call param plan has invalid param type %s", pp.paramType)
		}
	}
	result.params = params
	return result, nil
}

type IterationCallParamContext struct {
	params         map[string]any
	parentResponse any
	index          int
}

func (pc *IterationCallParamContext) getParams() map[string]any {
	return pc.params
}

func (pc *IterationCallParamContext) getParentResponse() any {
	return pc.parentResponse
}

func (pc *IterationCallParamContext) getParentResponseList() []any {
	return []any{pc.parentResponse}
}

func (pc *IterationCallParamContext) setParams(m map[string]any) {
	pc.params = m
}

func prepareIterationCallFieldParams(fieldPlan *FieldPlan, rundata *Rundata, ctx context.Context) ([]IterationCallParamContext, error) {
	if rundata == nil {
		return nil, fmt.Errorf("rundata is nil")
	}
	if fieldPlan == nil {
		return nil, fmt.Errorf("iteration step field plan is nil")
	}

	if fieldPlan.parentFieldId == 0 || uint64(fieldPlan.parentFieldId) >= uint64(len(rundata.fieldResponses)) {
		return nil, fmt.Errorf("parent field id %d is out of range", fieldPlan.parentFieldId)
	}

	for _, paramPlan := range fieldPlan.paramPlans {
		if paramPlan == nil {
			return nil, fmt.Errorf("param plan is nil")
		}

		switch paramPlan.paramType {
		case PARAM_TYPE_ENUM_INPUT, PARAM_TYPE_ENUM_CONST, PARAM_TYPE_ENUM_VAR_TEMPLATE:
			continue
		case PARAM_TYPE_ENUM_FIELD_RESPONSE_ATTRIBUTE:
			if paramPlan.dependentFieldId == 0 || uint64(paramPlan.dependentFieldId) >= uint64(len(rundata.fieldResponses)) {
				return nil, fmt.Errorf("dependent field id %d is out of range", paramPlan.dependentFieldId)
			}
		case PARAM_TYPE_ENUM_FIELD_RESPONSE_RAW:
			if paramPlan.dependentFieldId != fieldPlan.parentFieldId {
				return nil, fmt.Errorf("iteration step response raw dependency %d dose not match parent field %d", paramPlan.dependentFieldId, fieldPlan.parentFieldId)
			}
		default:
			return nil, fmt.Errorf("iteration step param plan has invalid param type %s", paramPlan.paramType)
		}
	}

	parentResponse := rundata.getFieldResponseByFieldId(fieldPlan.parentFieldId)
	if parentResponse == nil || len(parentResponse.responseRaws) == 0 {
		return []IterationCallParamContext{}, nil
	}

	result := make([]IterationCallParamContext, 0, len(parentResponse.responseRaws))
	for index, responseRaw := range parentResponse.responseRaws {
		if isNilInterfaceValue(responseRaw) {
			continue
		}

		params := make(map[string]any, len(fieldPlan.paramPlans))
		for _, paramPlan := range fieldPlan.paramPlans {
			val, handled, err := paramPlan.resolveFromInputs(rundata.originalParams)
			if err != nil {
				return nil, err
			}
			if handled {
				params[paramPlan.paramKey] = val
				continue
			}

			switch paramPlan.paramType {
			case PARAM_TYPE_ENUM_INPUT:
				continue
			case PARAM_TYPE_ENUM_FIELD_RESPONSE_RAW:
				continue
			case PARAM_TYPE_ENUM_FIELD_RESPONSE_ATTRIBUTE:
				val, err = resolveIterationFieldResponseAttributeParam(fieldPlan, paramPlan, responseRaw, fieldPlan.parentFieldId, rundata)
				if err != nil {
					return nil, err
				}
				params[paramPlan.paramKey] = val
			default:
				return nil, fmt.Errorf("iteration step param plan has invalid param type %s", paramPlan.paramType)
			}
		}

		result = append(result, IterationCallParamContext{
			parentResponse: responseRaw,
			params:         params,
			index:          index,
		})
	}
	return result, nil
}

// 根据fieldPlan和paramPlan动态判定参数值从父节点的结果取还是从dependentFieldId对应的节点结果取
func resolveIterationFieldResponseAttributeParam(fieldPlan *FieldPlan, paramPlan *ParamPlan, parentResponse any, parentFieldId uint32, rundata *Rundata) (any, error) {
	dependentFieldId := paramPlan.dependentFieldId
	//默认dependencySource就是parentResponse，从父节点结果取值
	dependencySource := parentResponse
	if dependentFieldId != parentFieldId {
		//如果dependentFieldId不等于parentFieldId，从dependentFieldId对应的FieldResponse里找参数值
		dependencyResponse := rundata.getFieldResponseByFieldId(dependentFieldId)
		if dependencyResponse == nil || len(dependencyResponse.responseRaws) == 0 {
			return nil, fmt.Errorf("dependent field id %d is out of range", dependentFieldId)
		}
		if dependencyResponse.hasBulkResponseBinding() {
			//dependentFieldId对应的FieldResponse里有批量数据，组装composite key查询对应数据作为dependencySource
			parentResponseMap, parentResponseMapOk := parentResponse.(map[string]any)
			if !parentResponseMapOk {
				return nil, fmt.Errorf("parent field %d response does not support composite-key mapping for parameter %q", parentFieldId, paramPlan.paramKey)
			}

			if fieldPlan.parentKeyFieldName == "" {
				return nil, fmt.Errorf("parent key field name is empty for field %q", fieldPlan.fieldName)
			}

			_, exists := parentResponseMap[fieldPlan.parentKeyFieldName]
			if !exists {
				return nil, fmt.Errorf("parent key field %q is empty for field %q", fieldPlan.parentKeyFieldName, fieldPlan.fieldName)
			}

			compositeKey := generateCompositeKey([]string{fieldPlan.parentKeyFieldName}, parentResponseMap)

			dependencySource, exists = dependencyResponse.lookResponseByCompositeKey(compositeKey)
			if !exists {
				return nil, fmt.Errorf("dependent field has no response for composite key %q", compositeKey)
			}
		} else {
			//没有批量数据，直接取dependencyResponse作为dependencySource
			if len(dependencyResponse.responseRaws) != 1 {
				return nil, fmt.Errorf("too many unbound responses for %d", dependentFieldId)
			}
			dependencySource = dependencyResponse.responseRaws[0]
		}
	}

	paths := paramPlan.fieldResponsePaths
	if len(paths) == 0 {
		return dependencySource, nil
	}
	if isNilInterfaceValue(dependencySource) {
		return nil, nil
	}
	//如果参数来源是List，遍历取数后返回参数值List
	if dependencyValues, ok := asListValue(dependencySource); ok {
		values := make([]any, len(dependencyValues))
		for index, dependencyValue := range dependencyValues {
			values[index] = getValueFromMapByPaths(dependencyValue, paths)
		}
		return values, nil
	}
	//根据paths直取参数值
	return getValueFromMapByPaths(dependencySource, paths), nil
}

type BulkCallParamContext struct {
	params             map[string]any
	parentResponseList []any
}

func (pc *BulkCallParamContext) getParams() map[string]any {
	return pc.params
}

func (pc *BulkCallParamContext) getParentResponse() any {
	if len(pc.parentResponseList) == 0 {
		return nil
	}
	return pc.parentResponseList[0]
}

func (pc *BulkCallParamContext) getParentResponseList() []any {
	return pc.parentResponseList
}

func (pc *BulkCallParamContext) setParams(m map[string]any) {
	pc.params = m
}

func prepareBulkCallFieldParams(fieldPlan *FieldPlan, rundata *Rundata, ctx context.Context) (*BulkCallParamContext, error) {
	if fieldPlan == nil {
		return nil, errors.New("bulk call field plan is nil")
	}
	if rundata == nil {
		return nil, errors.New("bulk call rundata is nil")
	}

	result := &BulkCallParamContext{
		params: make(map[string]any, len(fieldPlan.bulkParamPlans)),
	}

	//父字段结果中符合类型判定的元素index
	var eligibleParentIndexes []int
	parentIndexesPrepared := false
	params := make(map[string]any, len(fieldPlan.bulkParamPlans))
	for _, pp := range fieldPlan.bulkParamPlans {
		if pp == nil {
			return nil, errors.New("bulk call field param is nil")
		}
		//针对常量、输入变量和变量模板类型的参数
		val, handled, err := pp.resolveFromInputs(rundata.originalParams)
		if err != nil {
			return nil, err
		}
		if handled {
			params[pp.paramKey] = val
			continue
		}

		if (pp.paramType == PARAM_TYPE_ENUM_FIELD_RESPONSE_ATTRIBUTE || pp.paramType == PARAM_TYPE_ENUM_FIELD_RESPONSE_RAW) && uint64(pp.dependentFieldId) >= uint64(len(rundata.fieldResponses)) {
			return nil, fmt.Errorf("dependent field id %d is out of range", pp.dependentFieldId)
		}
		//针对依赖父节点数据和依赖父节点执行的参数
		switch pp.paramType {
		case PARAM_TYPE_ENUM_INPUT:
			//可选变量未提供且没有默认值，跳过
			continue
		case PARAM_TYPE_ENUM_FIELD_RESPONSE_ATTRIBUTE:
			dependentResponse := rundata.getFieldResponseByFieldId(pp.dependentFieldId)
			if dependentResponse == nil || len(dependentResponse.responseRaws) == 0 {
				return nil, fmt.Errorf("parent response is nil for field %s", pp.dependentFieldId)
			}

			//依赖参数来源非父节点而是其他祖宗节点时，不做类型校验
			if pp.dependentFieldId != fieldPlan.parentFieldId {
				bulkVals := make([]any, 0, len(dependentResponse.responseRaws))
				for _, raw := range dependentResponse.responseRaws {
					bulkVals = append(bulkVals, getValueFromMapByPaths(raw, pp.fieldResponsePaths))
				}
				result.params[pp.paramKey] = bulkVals
				continue
			}

			//判断父节点元素类型是否符合类型判定
			if !parentIndexesPrepared {
				eligibleParentIndexes = make([]int, 0, len(dependentResponse.responseRaws))

				//当前字段所属类型在编译阶段已经确定
				if evaluateCompiledTypeShouldExecuteField(fieldPlan, ctx) {
					for index, parentResponse := range dependentResponse.responseRaws {
						//父元素不存在可以执行的子字段
						if parentResponse == nil {
							continue
						}

						eligibleParentIndexes = append(eligibleParentIndexes, index)
					}
				} else {
					//interface/Union type必须逐个父元素判定运行时类型
					for index, parentResponse := range dependentResponse.responseRaws {
						if parentResponse == nil {
							continue
						}
						if !evaluateDynamicTypeShouldExecuteField(fieldPlan, parentResponse, ctx) {
							continue
						}
						eligibleParentIndexes = append(eligibleParentIndexes, index)
					}
				}
				parentIndexesPrepared = true
			}

			//根据类型判定结果添加校验通过的父节点结果元素
			values := make([]any, 0, len(eligibleParentIndexes))
			for _, index := range eligibleParentIndexes {
				values = append(values, getValueFromMapByPaths(dependentResponse.responseRaws[index], pp.fieldResponsePaths))
			}
			result.params[pp.paramKey] = values
		case PARAM_TYPE_ENUM_FIELD_RESPONSE_RAW:
			if rundata.getFieldResponseByFieldId(pp.dependentFieldId) == nil {
				return nil, fmt.Errorf("dependent field id %d has no response", pp.dependentFieldId)
			}
		default:
			return nil, fmt.Errorf("bulk call field param is unsupported")
		}
	}
	return result, nil
}

func getValueFromMapByPaths(data any, paths []string) any {
	current := data
	for _, key := range paths {
		m, ok := current.(map[string]interface{})
		if !ok {
			return nil
		}
		current = m[key]
	}
	return current
}

// 判断当前Field的类型是否为静态编译好的
func isFieldPlanTypeCompiled(fp *FieldPlan) bool {
	if fp.fieldTypeScope != nil && fp.fieldTypeScope.dynamicTypeResolver == nil {
		return true
	}
	return false
}
