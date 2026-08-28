package graphql

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"sync"
	"sync/atomic"

	"github.com/graphql-go/graphql/language/ast"
)

type SchemaResolveInfo struct {
	operation ast.Definition            //请求无关的OperationDefinition，运行期填充ResolveInfo.Operation
	fragments map[string]ast.Definition //请求无关的fragments定义，运行期填充ResolveInfo.Fragments
}

type SGraphExecutionPlan struct {
	roots             []*FieldPlan
	schemaResolveInfo SchemaResolveInfo
	maxFieldId        uint32
	maxListPathDepth  int                   //整个Plan响应路径中可能同时出现的最大List下标深度
	fieldPlansById    map[uint32]*FieldPlan //编译阶段生成的只读索引，供执行错误补齐字段AST元数据
	batches           []*BatchPlan
	batchesMu         sync.RWMutex
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
	baseType               Type                  //除去封装类型嵌套的原始基本类型
	elementWrapperTypeInfo *FieldWrapperTypeInfo //剥去当前封装类后的子类型
}

type ResolverFunc func(source any, params map[string]any, info ResolveInfo, ctx context.Context) (any, error)

func wrapFieldResolverFunc(fieldResolveFn FieldResolveFn) ResolverFunc {
	if fieldResolveFn == nil {
		return nil
	}
	type firstResponseGetter interface {
		GetFirstResponse() any
	}
	return func(source any, params map[string]any, info ResolveInfo, ctx context.Context) (any, error) {
		actualSource := source
		if wrappedSource, ok := source.(firstResponseGetter); ok {
			actualSource = wrappedSource.GetFirstResponse()
		}
		return fieldResolveFn(ResolveParams{
			Source:  actualSource,
			Args:    params,
			Info:    info,
			Context: ctx,
		})
	}
}

type FieldDynamicTypeResolveFunction func(value any, info ResolveInfo, ctx *context.Context) string

type FieldTypeScope struct {
	declaredType        any
	allowedDynamicTypes map[string]*Object
	dynamicTypeResolver FieldDynamicTypeResolveFunction
	staticTypeName      string
}

var TypeNameResolverFunc = func(source any, params map[string]any, info ResolveInfo, ctx context.Context) (any, error) {
	if params != nil {
		if typeName, ok := params[DefaultFieldKeyTypename].(string); ok {
			return typeName, nil
		}
	}
	return nil, nil
}

// 根据FieldType封装FieldTypeScope
func wrapTypeDefinition2Scope(fieldType Type, compiler *PlanCompiler) (*FieldTypeScope, error) {
	if fieldType == nil {
		return &FieldTypeScope{}, nil
	}
	baseType, err := getBaseType(fieldType)
	if err != nil {
		return nil, err
	}
	switch t := baseType.(type) {
	case *Object:
		return wrapStaticFieldTypeScope(t)
	case *Interface:
		return wrapDynamicFieldTypeScope(compiler, t)
	case *Union:
		return wrapDynamicFieldTypeScope(compiler, t)
	default:
		return &FieldTypeScope{}, nil
	}
}

// 封装静态编译类型的FieldTypeScope
func wrapStaticFieldTypeScope(nodeType *Object) (*FieldTypeScope, error) {
	if nodeType == nil {
		return nil, errors.New("no node provided")
	}
	return &FieldTypeScope{
		declaredType: nodeType,
		allowedDynamicTypes: map[string]*Object{
			nodeType.Name(): nodeType,
		},
		staticTypeName:      nodeType.Name(),
		dynamicTypeResolver: nil,
	}, nil
}

// 封装运行时动态判定的FieldTypeScope
func wrapDynamicFieldTypeScope(compiler *PlanCompiler, nodeType Abstract) (*FieldTypeScope, error) {
	if compiler == nil {
		return nil, errors.New("no compiler provided")
	}
	possibleTypes := compiler.schema.PossibleTypes(nodeType)
	result := make(map[string]*Object, len(possibleTypes))
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

func newDynamicTypeResolverFunction(abs Abstract, compiler *PlanCompiler) FieldDynamicTypeResolveFunction {
	return func(value any, info ResolveInfo, ctx *context.Context) string {
		if isNilInterfaceValue(value) {
			return ""
		}

		switch t := abs.(type) {
		case *Interface:
			if t.ResolveType != nil {
				if obj := t.ResolveType(ResolveTypeParams{Value: value, Info: info, Context: *ctx}); obj != nil {
					return obj.Name()
				}
			}
		case *Union:
			if t.ResolveType != nil {
				if obj := t.ResolveType(ResolveTypeParams{Value: value, Info: info, Context: *ctx}); obj != nil {
					return obj.Name()
				}
			}
		}

		for _, possible := range compiler.schema.PossibleTypes(abs) {
			if possible.IsTypeOf == nil {
				continue
			}
			if possible.IsTypeOf(IsTypeOfParams{Value: value, Info: info, Context: *ctx}) {
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

	var possibleTypes map[string]*Object
	switch tt := namedType.(type) {
	case *Object:
		possibleTypes = map[string]*Object{tt.Name(): tt}
	case Abstract:
		possibleTypes = map[string]*Object{}
		for _, pt := range compiler.schema.PossibleTypes(tt) {
			possibleTypes[pt.Name()] = pt
		}
	default:
		return nil, fmt.Errorf("unknown type condition found for %s", typeCondition.Name.Value)
	}

	//基于possibleTypes对parentScope进行裁剪
	croppedTypeMap := make(map[string]*Object)
	for name, obj := range parentScope.allowedDynamicTypes {
		if _, ok := possibleTypes[name]; ok {
			croppedTypeMap[name] = obj
		}
	}

	if len(croppedTypeMap) == 0 {
		return nil, fmt.Errorf("no type condition matched for %s", typeCondition.Name.Value)
	}
	return &FieldTypeScope{
		declaredType:        namedType,
		allowedDynamicTypes: croppedTypeMap,
		staticTypeName:      parentScope.staticTypeName,
		dynamicTypeResolver: parentScope.dynamicTypeResolver,
	}, nil
}

type FieldPlan struct {
	//字段身份与结果树
	fieldId             uint32   //字段自增ID，从1开始
	parentFieldId       uint32   //父字段ID，root节点该字段为0
	fieldName           string   //schema中的字段名称
	responseName        string   //组装最后结果时的字段名称，如果有alias就使用alias
	paths               []string //从根到当前字段的静态responseName路径（含alias）
	pathListDepths      []int    //与paths平行，记录每个responseName之后可能出现的List下标层数
	needsOccurrencePath bool     //当前List结果需要驱动下游逐元素Step时，才保存请求级occurrence路径
	childrenFields      []*FieldPlan

	//Graphql类型和ResolveInfo元数据
	fieldASTs            []*ast.Field         //同一responseName合并后的原始fieldAST，供 esolveInfo.FieldASTs使用
	returnType           Output               //schema 中声明的字段返回类型，供ResolveInfo.ReturnType使用
	fieldWrapperTypeInfo FieldWrapperTypeInfo //字段返回值的封装类型元信息
	fieldTypeScope       *FieldTypeScope      //类型判定范围
	parentType           Composite            //字段所属的父类型，供ResolveInfo.ParentType和extension hook使用

	//Resolver和参数依赖
	paramPlans                  []*ParamPlan //单次参数计划List，NormalStep和IteratorStep中遍历模式执行时使用
	bulkParamPlans              []*ParamPlan //批量参数计划，IteratorStep中批量模式执行时使用
	resolverFunc                ResolverFunc //单次执行Resolver方法，NormalStep和IteratorStep中遍历模式执行时使用
	bulkResolverFunc            ResolverFunc //批量执行Resolver方法，IteratorStep中批量模式执行时使用
	materializeFromParentSource bool         // schema字段无resolver但下游Step依赖其结果时，从父结果读取属性并写入Rundata

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
	templateAST        ast.Value //含变量的复合实参AST模板（只存结构，不存请求值）
	templateType       Input     //该实参的输入类型，校验时使用
}

// 解析"仅依赖变量/常量"的参数（Const/Input/VariableTemplate）。
// handled=false表示本次请求不应写入该参数，或该参数需交调用方按step形态处理。
func (pp *ParamPlan) resolveFromInputs(originalInputs map[string]any) (value any, handled bool, err error) {
	switch pp.paramType {
	case PARAM_TYPE_ENUM_INPUT:
		if val, ok := originalInputs[pp.inputName]; ok {
			return val, true, nil
		}
		if pp.inputDefaultValue != nil {
			return pp.inputDefaultValue, true, nil
		}
		return nil, false, nil
	case PARAM_TYPE_ENUM_CONST:
		return pp.constValue, true, nil
	case PARAM_TYPE_ENUM_VAR_TEMPLATE:
		val := pp.materializeTemplate(originalInputs)
		return val, true, nil
	default:
		return nil, false, nil
	}
}

// materializeTemplate 用本请求变量把模板物化为最终值（复用 valueFromAST 协变/校验）。
func (pp *ParamPlan) materializeTemplate(originalInputs map[string]any) any {
	if pp == nil || pp.templateAST == nil {
		return nil
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
func newVariableTemplateParamPlan(paramKey string, templateAST ast.Value, templateType Input) *ParamPlan {
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
	if b.concurrent && len(b.steps) > 1 {
		//并发执行step
		treeInterrupted := atomic.Bool{}
		wg := sync.WaitGroup{}
		wg.Add(len(b.steps))

		for _, step := range b.steps {
			go func(step Step) {
				defer wg.Done()
				// step 作为参数传入 goroutine，避免闭包捕获循环变量导致执行错 step。
				status := executeStepSafely(step, rundata, ctx)
				if status == StepExecuteTreeError {
					treeInterrupted.Store(true)
				}
			}(step)
		}

		wg.Wait()
		//判断是否有需要中断整个流程的错误
		if treeInterrupted.Load() {
			return batchResultInterrupt
		}
	} else {
		//串行执行step
		for _, step := range b.steps {
			status := executeStepSafely(step, rundata, ctx)
			if status == StepExecuteTreeError {
				//判断是否有需要中断整个流程的错误
				return batchResultInterrupt
			}
		}
	}
	return batchResultContinue
}

type StepExecuteStatus uint8

const (
	StepExecuteSuccess    StepExecuteStatus = iota //当前Step顺利完成或按条件跳过
	StepExecuteFieldError                          //字段级错误已写入Rundata，其他Step仍可继续
	StepExecuteTreeError                           //执行结构不可继续，后续Batch必须中断
)

func executeStepSafely(step Step, rundata *Rundata, ctx context.Context) (status StepExecuteStatus) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err := recoveredValueAsError(recovered)
			if rundata != nil {
				// resolver/directive panic在innerStepCalling内按字段处理；逃逸到这里说明执行结构已不可信。
				rundata.addFieldError(0, FieldErrorTypeTree, err, nil)
			}
			status = StepExecuteTreeError
		}
	}()
	if step == nil {
		if rundata != nil {
			rundata.addFieldError(0, FieldErrorTypeTree, errors.New("batch step is nil"), nil)
		}
		return StepExecuteTreeError
	}
	return step.Execute(rundata, ctx)
}

type Step interface {
	Execute(rundata *Rundata, ctx context.Context) StepExecuteStatus
}

type SingleCallStep struct {
	fieldPlan *FieldPlan
}

func (s *SingleCallStep) Execute(rundata *Rundata, ctx context.Context) StepExecuteStatus {
	if s == nil || s.fieldPlan == nil || rundata == nil {
		if rundata != nil {
			rundata.addFieldError(0, FieldErrorTypeTree, errors.New("single call step execution state is invalid"), nil)
		}
		return StepExecuteTreeError
	}
	//先判断skip/include指令是否执行
	include, includeErr := evaluateSkipIncludeDirectivesShouldExecuteField(s.fieldPlan, rundata, ctx)
	if includeErr != nil {
		rundata.addFieldError(s.fieldPlan.fieldId, FieldErrorTypeField, includeErr, s.fieldPlan.paths)
		return StepExecuteFieldError
	}
	//如果Directive的结果是不执行，直接返回
	if !include {
		return StepExecuteSuccess
	}

	if !fieldDependenciesAvailable(s.fieldPlan, rundata) {
		return StepExecuteSuccess
	}

	//组装参数
	paramContext, paramContextErr := prepareSingleCallFieldParams(s.fieldPlan.paramPlans, rundata)
	if paramContextErr != nil || paramContext == nil {
		if paramContextErr == nil {
			paramContextErr = errors.New("single call param context is nil")
		}
		rundata.addFieldError(s.fieldPlan.fieldId, FieldErrorTypeField, paramContextErr, s.fieldPlan.paths)
		return StepExecuteFieldError
	}

	responsePath := responsePathForFieldOccurrence(s.fieldPlan, nil)
	fieldResponse, completed, completionReady, status := innerStepCalling(s.fieldPlan, paramContext, s.fieldPlan.resolverFunc, nil, rundata, ctx, responsePath, true)
	if status == StepExecuteTreeError {
		releaseFieldResponse(fieldResponse)
		return status
	}
	if !completionReady || fieldResponse == nil {
		return status
	}
	// nullable List 返回 nil 时不发布空 FieldResponse，避免组装阶段把 null 误判成空列表。
	if s.fieldPlan.fieldWrapperTypeInfo.isList && isNilInterfaceValue(completed) {
		releaseFieldResponse(fieldResponse)
		return status
	}
	rundata.setFieldResponse(s.fieldPlan.fieldId, fieldResponse)
	return status
}

type IterationCallStep struct {
	fieldPlan *FieldPlan
}

func (i *IterationCallStep) Execute(rundata *Rundata, ctx context.Context) StepExecuteStatus {
	if i == nil || i.fieldPlan == nil || rundata == nil {
		if rundata != nil {
			rundata.addFieldError(0, FieldErrorTypeTree, errors.New("iteration call step execution state is invalid"), nil)
		}
		return StepExecuteTreeError
	}
	//先判断skip/include指令是否执行
	include, includeErr := evaluateSkipIncludeDirectivesShouldExecuteField(i.fieldPlan, rundata, ctx)
	if includeErr != nil {
		rundata.addFieldError(i.fieldPlan.fieldId, FieldErrorTypeField, includeErr, i.fieldPlan.paths)
		return StepExecuteFieldError
	}
	//如果Directive的结果是不执行，直接返回
	if !include {
		return StepExecuteSuccess
	}

	if !fieldDependenciesAvailable(i.fieldPlan, rundata) {
		return StepExecuteSuccess
	}

	//优先执行bulk模式，不行回退到iteration模式
	if bulkResolverFunc := i.fieldPlan.bulkResolverFunc; bulkResolverFunc != nil {
		bulkParamContext, bulkParamContextErr := prepareBulkCallFieldParams(i.fieldPlan, rundata, ctx)
		if bulkParamContextErr != nil {
			rundata.addFieldError(i.fieldPlan.fieldId, FieldErrorTypeField, bulkParamContextErr, i.fieldPlan.paths)
			return StepExecuteFieldError
		}
		fieldRes, completed, completionReady, status := innerStepCalling(i.fieldPlan, bulkParamContext, bulkResolverFunc, nil, rundata, ctx, responsePathForFieldOccurrence(i.fieldPlan, nil), false)
		if status == StepExecuteTreeError {
			releaseFieldResponse(fieldRes)
			return status
		}
		if !completionReady || fieldRes == nil {
			return status
		}
		if i.bindBulkResponse2ParentResponseWithCompositeKey(fieldRes, completed, rundata) {
			status = StepExecuteFieldError
		}
		rundata.setFieldResponse(i.fieldPlan.fieldId, fieldRes)
		return status
	}
	//iteration模式
	iterationParamContextList, iterationParamContextErr := prepareIterationCallFieldParams(i.fieldPlan, rundata, ctx)
	if iterationParamContextErr != nil {
		rundata.addFieldError(i.fieldPlan.fieldId, FieldErrorTypeField, iterationParamContextErr, i.fieldPlan.paths)
		return StepExecuteFieldError
	}
	var fieldResponse *FieldResponse
	status := StepExecuteSuccess
	for _, iterationParamContext := range iterationParamContextList {
		responsePath := iterationParamContext.responsePath
		if iterationParamContext.prepareErr != nil {
			rundata.addFieldErrorAtResponsePath(i.fieldPlan.fieldId, FieldErrorTypeField, iterationParamContext.prepareErr, responsePath)
			status = StepExecuteFieldError
			if fieldResponse == nil {
				fieldResponse = acquireFieldResponse()
			}
			if i.bindIterationResponse(fieldResponse, iterationParamContext.parentResponse, nil, rundata, responsePath) {
				status = StepExecuteFieldError
			}
			continue
		}

		responseRawCount, responsePathCount, occurrenceCount := fieldResponseCheckpoint(fieldResponse)
		var completed any
		var completionReady bool
		var currentStatus StepExecuteStatus
		fieldResponse, completed, completionReady, currentStatus = innerStepCalling(i.fieldPlan, &iterationParamContext, i.fieldPlan.resolverFunc, fieldResponse, rundata, ctx, responsePath, true)
		if currentStatus == StepExecuteTreeError {
			releaseFieldResponse(fieldResponse)
			return currentStatus
		}
		if currentStatus == StepExecuteFieldError {
			status = StepExecuteFieldError
		}
		if !completionReady {
			continue
		}
		if i.bindIterationResponse(fieldResponse, iterationParamContext.parentResponse, completed, rundata, responsePath) {
			// 绑定失败的结果无法参与后续参数解析和结果组装，回滚本occurrence刚写入的数据。
			rollbackFieldResponse(fieldResponse, responseRawCount, responsePathCount, occurrenceCount)
			status = StepExecuteFieldError
		}
	}
	if fieldResponse != nil {
		rundata.setFieldResponse(i.fieldPlan.fieldId, fieldResponse)
	}
	return status
}

func fieldResponseCheckpoint(fieldResponse *FieldResponse) (responseRawCount int, responsePathCount int, occurrenceCount int) {
	if fieldResponse == nil {
		return 0, 0, 0
	}
	responseRawCount = len(fieldResponse.responseRaws)
	responsePathCount = len(fieldResponse.responsePaths)
	if fieldResponse.bulkState != nil {
		fieldResponse.bulkState.mu.Lock()
		occurrenceCount = len(fieldResponse.bulkState.iterationResponses)
		fieldResponse.bulkState.mu.Unlock()
	}
	return responseRawCount, responsePathCount, occurrenceCount
}

func rollbackFieldResponse(fieldResponse *FieldResponse, responseRawCount int, responsePathCount int, occurrenceCount int) {
	if fieldResponse == nil {
		return
	}
	if responseRawCount <= len(fieldResponse.responseRaws) {
		clear(fieldResponse.responseRaws[responseRawCount:])
		fieldResponse.responseRaws = fieldResponse.responseRaws[:responseRawCount]
	}
	if responsePathCount <= len(fieldResponse.responsePaths) {
		clear(fieldResponse.responsePaths[responsePathCount:])
		fieldResponse.responsePaths = fieldResponse.responsePaths[:responsePathCount]
	}
	if fieldResponse.bulkState != nil {
		fieldResponse.bulkState.mu.Lock()
		if occurrenceCount <= len(fieldResponse.bulkState.iterationResponses) {
			clear(fieldResponse.bulkState.iterationResponses[occurrenceCount:])
			fieldResponse.bulkState.iterationResponses = fieldResponse.bulkState.iterationResponses[:occurrenceCount]
		}
		fieldResponse.bulkState.mu.Unlock()
	}
}

func (i *IterationCallStep) bindIterationResponse(fieldResponse *FieldResponse, parentResponse any, completed any, rundata *Rundata, responsePath *ResponsePath) bool {
	if fieldResponse == nil {
		rundata.addFieldErrorAtResponsePath(i.fieldPlan.fieldId, FieldErrorTypeField, fmt.Errorf("field response is nil"), responsePath)
		return true
	}
	// __typename 由当前父元素的实际类型直接完成，不需要按业务字段的 composite key 建立父子映射。
	if i.fieldPlan != nil && i.fieldPlan.isIntrospectionTypeNameField() {
		return false
	}

	keyFieldName := i.fieldPlan.parentKeyFieldName
	if keyFieldName == "" {
		//responsePath包含当前字段名，Prev才是resolver所属的父occurrence。
		if responsePath == nil || responsePath.Prev == nil {
			rundata.addFieldErrorAtResponsePath(i.fieldPlan.fieldId, FieldErrorTypeField, fmt.Errorf("parent occurrence path is missing for field %s", i.fieldPlan.fieldName), responsePath)
			return true
		}

		bindingKey, err := responsePathBindingKey(responsePath.Prev)
		if err != nil {
			rundata.addFieldErrorAtResponsePath(i.fieldPlan.fieldId, FieldErrorTypeField, err, responsePath)
			return true
		}

		fieldResponse.bindParentResponse(fieldResponseBindingResponsePath, bindingKey, completed)
		return false
	}

	//parentKey存在时保持原有compositeKey语义，不能退回path
	parentMap, ok := parentResponse.(map[string]any)
	if !ok {
		rundata.addFieldErrorAtResponsePath(i.fieldPlan.fieldId, FieldErrorTypeField, fmt.Errorf("parent response for field %s does not support composite key mapping", i.fieldPlan.fieldName), responsePath)
		return true
	}
	if _, exist := parentMap[keyFieldName]; !exist {
		rundata.addFieldErrorAtResponsePath(i.fieldPlan.fieldId, FieldErrorTypeField, fmt.Errorf("parent key field %q is missing for field %s", keyFieldName, i.fieldPlan.fieldName), responsePath)
		return true
	}
	compositeKey := generateCompositeKey([]string{keyFieldName}, parentMap)
	if _, exists := fieldResponse.lookParentResponse(compositeKey); exists {
		rundata.addFieldErrorAtResponsePath(i.fieldPlan.fieldId, FieldErrorTypeField, fmt.Errorf("duplicate parent binding key %q for field %s", compositeKey, i.fieldPlan.fieldName), responsePath)
		return true
	}
	fieldResponse.bindParentResponse(fieldResponseBindingCompositeKey, compositeKey, completed)
	return false
}

func (i *IterationCallStep) bindBulkResponse2ParentResponseWithCompositeKey(fieldResponse *FieldResponse, completed any, rundata *Rundata) bool {
	if fieldResponse == nil {
		rundata.addFieldError(i.fieldPlan.fieldId, FieldErrorTypeField, errors.New("field response is nil"), i.fieldPlan.paths)
		return true
	}
	// Bulk即使返回空集合也进入按父key组装模式，避免把批量传输切片当成某个父元素的字段值。
	fieldResponse.parentBindingMode = fieldResponseBindingCompositeKey
	if i.fieldPlan.needsOccurrencePath {
		state := fieldResponse.ensureBulkState(i.fieldPlan)
		state.iterationState = bulkIterationPending
	}
	responses, ok := asListValue(completed)
	if !ok {
		rundata.addFieldError(i.fieldPlan.fieldId, FieldErrorTypeField, fmt.Errorf("bulk resolver for field %s must return an iterable result", i.fieldPlan.fieldName), i.fieldPlan.paths)
		return true
	}
	hasError := false
	for _, response := range responses {
		if group, grouped := asListValue(response); grouped {
			for _, groupedResponse := range group {
				compositeKey, hasCompositeKey, listIndex, err := i.bindBulkResponseItem(fieldResponse, groupedResponse)
				if err != nil {
					i.recordBulkBindingError(fieldResponse, compositeKey, hasCompositeKey, listIndex, err, rundata)
					hasError = true
				}
			}
			continue
		}
		compositeKey, hasCompositeKey, listIndex, err := i.bindBulkResponseItem(fieldResponse, response)
		if err != nil {
			i.recordBulkBindingError(fieldResponse, compositeKey, hasCompositeKey, listIndex, err, rundata)
			hasError = true
		}
	}
	return hasError
}

func (i *IterationCallStep) recordBulkBindingError(fieldResponse *FieldResponse, compositeKey any, hasCompositeKey bool, listIndex int, err error, rundata *Rundata) {
	if !hasCompositeKey {
		rundata.addFieldError(i.fieldPlan.fieldId, FieldErrorTypeField, err, i.fieldPlan.paths)
		return
	}
	state := fieldResponse.ensureBulkState(i.fieldPlan)
	if state.bindingErrors == nil {
		state.bindingErrors = make(map[any][]bulkBindingError)
	}
	state.bindingErrors[compositeKey] = append(state.bindingErrors[compositeKey], bulkBindingError{
		err:          err,
		listIndex:    listIndex,
		hasListIndex: listIndex >= 0,
	})
	// 只设置布尔快路径；具体父occurrence要在结果组装阶段按composite key定位。
	rundata.hasPendingBulkBindingErrors.Store(true)
}

func (i *IterationCallStep) bindBulkResponseItem(fieldResponse *FieldResponse, response any) (any, bool, int, error) {
	responseMap, ok := response.(map[string]any)
	if !ok {
		return nil, false, -1, fmt.Errorf("bulk resolver for field %s returned an item of unsupported type %T", i.fieldPlan.fieldName, response)
	}
	keyFieldName := i.fieldPlan.resultParentKeyName
	if keyFieldName == "" {
		return nil, false, -1, fmt.Errorf("bulk result key field name is empty for field %s", i.fieldPlan.fieldName)
	}
	if _, exists := responseMap[keyFieldName]; !exists {
		return nil, false, -1, fmt.Errorf("bulk result key field %q is missing for field %s", keyFieldName, i.fieldPlan.fieldName)
	}
	compositeKey := generateCompositeKey([]string{keyFieldName}, responseMap)
	bound, exists := fieldResponse.lookParentResponse(compositeKey)
	if !i.fieldPlan.fieldWrapperTypeInfo.isList {
		if exists {
			return compositeKey, true, -1, fmt.Errorf("bulk resolver for non-list field %s returned duplicate key %q", i.fieldPlan.fieldName, compositeKey)
		}
		fieldResponse.bindParentResponse(fieldResponseBindingCompositeKey, compositeKey, responseMap)
		return compositeKey, true, -1, nil
	}
	if !exists {
		fieldResponse.bindParentResponse(fieldResponseBindingCompositeKey, compositeKey, []any{responseMap})
		return compositeKey, true, 0, nil
	}
	boundList, ok := bound.([]any)
	if !ok {
		return compositeKey, true, -1, fmt.Errorf("bulk result binding for list field %s is not a list", i.fieldPlan.fieldName)
	}
	fieldResponse.bindParentResponse(fieldResponseBindingCompositeKey, compositeKey, append(boundList, responseMap))
	return compositeKey, true, len(boundList), nil
}

func innerStepCalling(fieldPlan *FieldPlan, paramContext ParamContext, resolverFn ResolverFunc, currentFieldResponse *FieldResponse, rundata *Rundata, ctx context.Context, responsePath *ResponsePath, writeOccurrencePaths bool) (fieldResponse *FieldResponse, completed any, completionReady bool, status StepExecuteStatus) {
	fieldResponse = currentFieldResponse
	if fieldPlan == nil || paramContext == nil || rundata == nil {
		if rundata != nil {
			rundata.addFieldError(0, FieldErrorTypeTree, errors.New("inner step execution state is invalid"), nil)
		}
		return fieldResponse, nil, false, StepExecuteTreeError
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			if fieldResponse == nil {
				fieldResponse = acquireFieldResponse()
			}
			completionReady = true
			completed = nil
			rundata.addFieldErrorAtResponsePath(fieldPlan.fieldId, FieldErrorTypeField, recoveredValueAsError(recovered), responsePath)
			status = StepExecuteFieldError
		}
	}()
	// 父对象为null时GraphQL不会执行其子选择集。内部物化Step必须直接跳过，
	// 避免为原本不会执行的non-null子字段额外记录完成错误。
	if fieldPlan.materializeFromParentSource && isNilInterfaceValue(paramContext.getParentResponse()) {
		return fieldResponse, nil, false, StepExecuteSuccess
	}
	//判断其他指令的计算结果当前字段是否执行
	shouldExecute := true
	if !fieldPlan.materializeFromParentSource {
		var directiveEvaluateErr error
		shouldExecute, directiveEvaluateErr = evaluateCommonDirectivesShouldExecuteField(fieldPlan, paramContext, rundata, ctx)
		if directiveEvaluateErr != nil {
			if fieldResponse == nil {
				fieldResponse = acquireFieldResponse()
			}
			rundata.addFieldErrorAtResponsePath(fieldPlan.fieldId, FieldErrorTypeField, directiveEvaluateErr, responsePath)
			return fieldResponse, nil, true, StepExecuteFieldError
		}
		if !shouldExecute {
			return fieldResponse, nil, false, StepExecuteSuccess
		}
	}

	//动态类型判定
	typeInfo := buildSGraphResolveInfo(rundata, fieldPlan, responsePath)
	shouldExecute = evaluateTypeShouldExecuteField(fieldPlan, paramContext.getParentResponse(), typeInfo, ctx)
	if !shouldExecute {
		return fieldResponse, nil, false, StepExecuteSuccess
	}

	//FieldResponse
	if fieldResponse == nil {
		fieldResponse = acquireFieldResponse()
	}

	//执行BeforeResolve指令
	if !fieldPlan.materializeFromParentSource {
		if beforeResolvedParams, beforeResolvedParamsErr := applyBeforeResolveDirectives(fieldPlan, paramContext, rundata, ctx); beforeResolvedParamsErr != nil {
			rundata.addFieldErrorAtResponsePath(fieldPlan.fieldId, FieldErrorTypeField, beforeResolvedParamsErr, responsePath)
			return fieldResponse, nil, true, StepExecuteFieldError
		} else {
			paramContext.setParams(beforeResolvedParams)
		}
	}

	//调用resolver前出发extension hook
	fieldCtx, info, fieldHook := startSGraphResolveFieldHook(rundata, fieldPlan, ctx, responsePath)

	//resolver方法调用
	res, err := execResolveProcess(fieldPlan, paramContext.getParentResponse(), paramContext.getParams(), resolverFn, info, fieldCtx)

	//执行完不论成功与否，调用extension hook
	finishSGraphResolveFieldHook(rundata, fieldHook, res, err)

	if err != nil {
		rundata.addFieldErrorAtResponsePath(fieldPlan.fieldId, FieldErrorTypeField, err, responsePath)
		return fieldResponse, nil, true, StepExecuteFieldError
	}

	//执行AfterResolve指令
	if !fieldPlan.materializeFromParentSource {
		afterResolvedResponse, afterResolvedErr := applyAfterResolveDirectives(fieldPlan, paramContext, res, rundata, ctx)
		if afterResolvedErr != nil {
			rundata.addFieldErrorAtResponsePath(fieldPlan.fieldId, FieldErrorTypeField, afterResolvedErr, responsePath)
			return fieldResponse, nil, true, StepExecuteFieldError
		}
		res = afterResolvedResponse
	}

	// 字段坐标只在真正产生完成错误时生成，避免正常执行路径为每个Step分配字符串。
	nilBubbled, completionErrors := processNullValueBubbling(fieldPlan, fieldPlan.fieldWrapperTypeInfo, res)
	if nilBubbled != nil {
		if fieldPlan.fieldWrapperTypeInfo.isList {
			fieldResponse.responseRaws = append(fieldResponse.responseRaws, nilBubbled...)
			if writeOccurrencePaths && fieldPlan.needsOccurrencePath {
				if fieldPlan.fieldWrapperTypeInfo.elementWrapperTypeInfo != nil && fieldPlan.fieldWrapperTypeInfo.elementWrapperTypeInfo.isList {
					state := fieldResponse.ensureBulkState(nil)
					state.iterationState = bulkIterationReady
					appendFieldResponseOccurrences(&state.iterationResponses, &fieldPlan.fieldWrapperTypeInfo, nilBubbled, responsePath)
				} else {
					for index := range nilBubbled {
						fieldResponse.responsePaths = append(fieldResponse.responsePaths, responsePath.WithKey(index))
					}
				}
			}
			completed = nilBubbled
		} else {
			if len(nilBubbled) > 0 {
				fieldResponse.responseRaws = append(fieldResponse.responseRaws, nilBubbled[0])
				if writeOccurrencePaths && fieldPlan.needsOccurrencePath {
					fieldResponse.responsePaths = append(fieldResponse.responsePaths, responsePath)
				}
				completed = nilBubbled[0]
			}
		}
	}
	for _, completionError := range completionErrors {
		completionPath := responsePath
		for _, index := range completionError.indexes {
			completionPath = completionPath.WithKey(index)
		}
		rundata.addFieldErrorAtResponsePath(fieldPlan.fieldId, FieldErrorTypeField, completionError.err, completionPath)
	}
	if len(completionErrors) != 0 {
		return fieldResponse, completed, true, StepExecuteFieldError
	}
	return fieldResponse, completed, true, StepExecuteSuccess
}

func applyBeforeResolveDirectives(fieldPlan *FieldPlan, paramContext ParamContext, rundata *Rundata, ctx context.Context) (map[string]any, error) {
	fieldArgs := paramContext.getParams()
	argsCopied := false
	for _, dp := range fieldPlan.directivePlans {
		if dp == nil {
			return nil, errors.New("directive plan is nil")
		}
		if dp.stage == DIRECTIVE_STAGE_BEFORE_RESOLVE {
			if !argsCopied {
				fieldArgs = make(map[string]any, len(paramContext.getParams()))
				maps.Copy(fieldArgs, paramContext.getParams())
				argsCopied = true
			}

			args, argsErr := materializeDirectiveArgs(dp, fieldPlan, paramContext, rundata, ctx)
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
		if dp == nil {
			return nil, errors.New("directive plan is nil")
		}
		if dp.stage == DIRECTIVE_STAGE_AFTER_RESOLVE {
			args, argsErr := materializeDirectiveArgs(dp, fieldPlan, paramContext, rundata, ctx)
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

func execResolveProcess(fieldPlan *FieldPlan, source any, params map[string]any, resolverFn ResolverFunc, info ResolveInfo, ctx context.Context) (result any, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			result = nil
			err = recoveredValueAsError(recovered)
		}
	}()

	if fieldPlan == nil {
		return nil, errors.New("field plan is nil")
	}
	if fieldPlan.materializeFromParentSource {
		if resolverFn == nil {
			return nil, fmt.Errorf("materialized source resolver is nil for field %s", fieldPlan.fieldName)
		}
		// 只有内部物化Step接收父字段原始值；用户resolver仍不兼容graphql-go Source。
		return resolverFn(source, nil, info, ctx)
	}
	//__typename特殊处理
	if fieldPlan.isIntrospectionTypeNameField() {
		if fieldPlan.fieldTypeScope != nil && fieldPlan.fieldTypeScope.dynamicTypeResolver != nil {
			typeName := fieldPlan.fieldTypeScope.dynamicTypeResolver(source, info, &ctx)
			if typeName == "" {
				err := errors.New("__typename resolved failed, value is empty")
				return nil, err
			}
			return typeName, nil
		}

		if resolverFn == nil {
			resolverFn = fieldPlan.resolverFunc
		}
		if resolverFn == nil {
			return nil, errors.New("resolver is nil for __typename field ")
		}
		return resolverFn(source, params, info, ctx)
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
func evaluateTypeShouldExecuteField(fieldPlan *FieldPlan, parentFieldResponseRaw any, info ResolveInfo, ctx context.Context) bool {
	shouldExecute := false
	if isFieldPlanTypeCompiled(fieldPlan) {
		shouldExecute = evaluateCompiledTypeShouldExecuteField(fieldPlan, ctx)
	} else {
		if parentFieldResponseRaw == nil {
			return shouldExecute
		}
		shouldExecute = evaluateDynamicTypeShouldExecuteField(fieldPlan, parentFieldResponseRaw, info, ctx)
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
			directiveArgs, argsErr := materializeDirectiveArgs(directive, fieldPlan, nil, rundata, ctx)
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
			return false, errors.New("directive plan is nil")
		}
		//筛选出类型为should execute的指令
		if directive.stage != DIRECTIVE_STAGE_SHOULD_EXECUTE {
			continue
		}
		if directive.runtimeHandler == nil {
			return false, fmt.Errorf("directive %s has no runtimeHandler ", directive.name)
		}
		directiveArgs, argsErr := materializeDirectiveArgs(directive, fieldPlan, paramContext, rundata, ctx)
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
func evaluateDynamicTypeShouldExecuteField(fp *FieldPlan, parentResponse any, info ResolveInfo, ctx context.Context) bool {
	if fp == nil || fp.fieldTypeScope == nil || len(fp.fieldTypeScope.allowedDynamicTypes) == 0 {
		return true
	}
	if fp.fieldTypeScope.dynamicTypeResolver != nil {
		typename := fp.fieldTypeScope.dynamicTypeResolver(parentResponse, info, &ctx)
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
func materializeDirectiveArgs(directivePlan *DirectivePlan, fieldPlan *FieldPlan, paramContext ParamContext, rundata *Rundata, ctx context.Context) (map[string]any, error) {
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
			switch argPlan.paramType {
			case PARAM_TYPE_ENUM_INPUT:
				continue
			case PARAM_TYPE_ENUM_FIELD_RESPONSE_ATTRIBUTE:
				val, err = resolveDirectiveFieldResponseParam(fieldPlan, argPlan, paramContext, rundata, ctx)
				if err != nil {
					return nil, err
				}
			default:
				return nil, fmt.Errorf("directive args plan has invalid param type %d", argPlan.paramType)
			}
		}

		args[argPlan.paramKey] = val
	}
	return args, nil
}

func resolveDirectiveFieldResponseParam(fieldPlan *FieldPlan, paramPlan *ParamPlan, paramContext ParamContext, rundata *Rundata, ctx context.Context) (any, error) {
	if fieldPlan == nil || paramContext == nil {
		return nil, fmt.Errorf("FIELD_RESPONSE directive parameter requires an executing field")
	}
	switch typedContext := paramContext.(type) {
	case *IterationCallParamContext:
		var parentResponsePath *ResponsePath
		if typedContext.responsePath != nil {
			parentResponsePath = typedContext.responsePath.Prev
		}
		return resolveIterationFieldResponseAttributeParam(fieldPlan, paramPlan, typedContext.parentResponse, parentResponsePath, fieldPlan.parentFieldId, rundata)
	case *BulkCallParamContext:
		return resolveBulkDirectiveFieldResponseParam(fieldPlan, paramPlan, rundata, ctx)
	case *SingleCallStepParamContext:
		return resolveSingleFieldResponseAttributeParam(paramPlan, rundata)
	default:
		return nil, fmt.Errorf("unsupported directive parameter context %T", paramContext)
	}
}

type listCompletionError struct {
	err     error
	indexes []int
}

func processNullValueBubbling(fieldPlan *FieldPlan, typeInfo FieldWrapperTypeInfo, fieldResponse any) ([]any, []listCompletionError) {
	value, _, completionErrors := innerProcessNullValueBubbling(fieldPlan, typeInfo, fieldResponse)
	return value, completionErrors
}

// TODO 检查逻辑是否正确
// 仅在元素需要规范化或发生null值冒泡时复制list， 正常的[]any返回原切片，避免每个list字段都重新分配并复制全部元素。
// 处理null值冒泡，错误保存相对当前字段的完整List下标路径。
// 只在真正产生完成错误的分支读取FieldPlan并生成字段坐标，
// 使错误文案与 graphql-go 原生链路一致，同时保持成功路径无额外字符串分配。
func innerProcessNullValueBubbling(fieldPlan *FieldPlan, typeInfo FieldWrapperTypeInfo, fieldResponse any) ([]any, bool, []listCompletionError) {
	//对于nil和typed nil的response进行处理
	if isNilInterfaceValue(fieldResponse) {
		//schema非空却为nil，报错
		if typeInfo.notNil {
			return nil, true, []listCompletionError{{err: fmt.Errorf("Cannot return null for non-nullable field %s.", fieldErrorCoordinate(fieldPlan.parentType, fieldPlan.fieldName))}}
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
		return nil, true, []listCompletionError{{err: fmt.Errorf("User Error: expected iterable, but did not find one for field %s.", fieldErrorCoordinate(fieldPlan.parentType, fieldPlan.fieldName))}}
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
		return nil, true, []listCompletionError{{err: fmt.Errorf("element type info is nil")}}
	}
	if !elementTypeInfo.isList {
		var completionErrors []listCompletionError
		listMustBubble := false
		for i, el := range list {
			if isNilInterfaceValue(el) {
				// 继续扫描同层其他元素，确保同一List内的多个错误都被保留。
				if elementTypeInfo.notNil {
					completionErrors = append(completionErrors, listCompletionError{
						err:     fmt.Errorf("Cannot return null for non-nullable field %s.", fieldErrorCoordinate(fieldPlan.parentType, fieldPlan.fieldName)),
						indexes: []int{i},
					})
					listMustBubble = true
					continue
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
		if listMustBubble {
			return nil, true, completionErrors
		}
		if result == nil {
			result = list
		}
		return result, responseChanged, completionErrors
	}

	//如果List中元素的封装类型还是List，要递归处理
	var completionErrors []listCompletionError
	listMustBubble := false
	for i, el := range list {
		processedElement, elChanged, elementErrors := innerProcessNullValueBubbling(fieldPlan, *elementTypeInfo, el)
		if len(elementErrors) != 0 {
			for errorIndex := range elementErrors {
				indexes := make([]int, len(elementErrors[errorIndex].indexes)+1)
				indexes[0] = i
				copy(indexes[1:], elementErrors[errorIndex].indexes)
				elementErrors[errorIndex].indexes = indexes
			}
			completionErrors = append(completionErrors, elementErrors...)
			// 当前元素是Non-Null时整个当前List冒泡，但仍继续扫描兄弟元素收集错误。
			if elementTypeInfo.notNil {
				listMustBubble = true
				continue
			}
			//schema允许空则只把当前嵌套List置为nil。
			processedElement = nil
			elChanged = true
		}

		if elChanged {
			if result == nil {
				result = make([]any, len(list))
				copy(result, list)
			}
			result[i] = processedElement
			responseChanged = true
		}
	}
	if listMustBubble {
		return nil, true, completionErrors
	}
	if result == nil {
		result = list
	}
	return result, responseChanged, completionErrors
}

func fieldDependenciesAvailable(fieldPlan *FieldPlan, rundata *Rundata) bool {
	if fieldPlan == nil || rundata == nil {
		return false
	}

	dependencyParamPlans := [3][]*ParamPlan{
		fieldPlan.paramPlans,
		fieldPlan.bulkParamPlans,
		nil,
	}
	if !fieldPlan.materializeFromParentSource {
		dependencyParamPlans[2] = fieldPlan.directiveParamPlans
	}
	for _, paramPlans := range dependencyParamPlans {
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
			value, valueErr := resolveSingleFieldResponseAttributeParam(pp, rundata)
			if valueErr != nil {
				return nil, valueErr
			}
			params[pp.paramKey] = value
		case PARAM_TYPE_ENUM_FIELD_RESPONSE_RAW:
			if uint64(pp.dependentFieldId) >= uint64(len(rundata.fieldResponses)) {
				return nil, fmt.Errorf("dependent field id %d is out of range", pp.dependentFieldId)
			}
			parentResponseRaw := rundata.getFieldResponseByFieldId(pp.dependentFieldId)
			if parentResponseRaw == nil || len(parentResponseRaw.responseRaws) == 0 {
				continue
			}
			if len(parentResponseRaw.responseRaws) != 1 {
				return nil, fmt.Errorf("parent response has more than one response for field %d", pp.dependentFieldId)
			}
			result.parentResponse = parentResponseRaw.responseRaws[0]
		default:
			return nil, fmt.Errorf("single call param plan has invalid param type %d", pp.paramType)
		}
	}
	result.params = params
	return result, nil
}

type IterationCallParamContext struct {
	params         map[string]any
	parentResponse any
	index          int
	responsePath   *ResponsePath
	prepareErr     error
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
				return nil, fmt.Errorf("iteration step response raw dependency %d does not match parent field %d", paramPlan.dependentFieldId, fieldPlan.parentFieldId)
			}
		default:
			return nil, fmt.Errorf("iteration step param plan has invalid param type %d", paramPlan.paramType)
		}
	}

	parentResponse := rundata.getFieldResponseByFieldId(fieldPlan.parentFieldId)
	if parentResponse == nil || len(parentResponse.responseRaws) == 0 {
		return []IterationCallParamContext{}, nil
	}
	parentRaws, parentPaths, parentOccurrences, iterationResponsesErr := parentResponse.iterationResponseData(rundata)
	if iterationResponsesErr != nil {
		return nil, iterationResponsesErr
	}
	parentCount := len(parentOccurrences)
	if parentOccurrences == nil {
		if len(parentRaws) != len(parentPaths) {
			return nil, fmt.Errorf("parent field %d response paths are not aligned with response values", fieldPlan.parentFieldId)
		}
		parentCount = len(parentRaws)
	}

	result := make([]IterationCallParamContext, 0, parentCount)
	for index := 0; index < parentCount; index++ {
		var responseRaw any
		var parentPath *ResponsePath
		if parentOccurrences != nil {
			responseRaw = parentOccurrences[index].responseRaw
			parentPath = parentOccurrences[index].responsePath
		} else {
			responseRaw = parentRaws[index]
			parentPath = parentPaths[index]
		}
		if isNilInterfaceValue(responseRaw) {
			continue
		}
		if parentPath == nil {
			return nil, fmt.Errorf("parent field %d response path %d is nil", fieldPlan.parentFieldId, index)
		}
		responsePath := parentPath.WithKey(fieldPlan.responseName)

		params := make(map[string]any, len(fieldPlan.paramPlans))
		var prepareErr error
		for _, paramPlan := range fieldPlan.paramPlans {
			val, handled, err := paramPlan.resolveFromInputs(rundata.originalParams)
			if err != nil {
				prepareErr = err
				break
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
				val, err = resolveIterationFieldResponseAttributeParam(fieldPlan, paramPlan, responseRaw, parentPath, fieldPlan.parentFieldId, rundata)
				if err != nil {
					prepareErr = err
					break
				}
				params[paramPlan.paramKey] = val
			default:
				prepareErr = fmt.Errorf("iteration step param plan has invalid param type %d", paramPlan.paramType)
			}
			if prepareErr != nil {
				break
			}
		}

		result = append(result, IterationCallParamContext{
			parentResponse: responseRaw,
			params:         params,
			index:          index,
			responsePath:   responsePath,
			prepareErr:     prepareErr,
		})
	}
	return result, nil
}

// 根据fieldPlan和paramPlan动态判定参数值从父节点的结果取还是从dependentFieldId对应的节点结果取
func resolveIterationFieldResponseAttributeParam(fieldPlan *FieldPlan, paramPlan *ParamPlan, parentResponse any, parentResponsePath *ResponsePath, parentFieldId uint32, rundata *Rundata) (any, error) {
	dependentFieldId := paramPlan.dependentFieldId
	//默认dependencySource就是parentResponse，从父节点结果取值
	dependencySource := parentResponse
	if dependentFieldId != parentFieldId {
		//如果dependentFieldId不等于parentFieldId，从dependentFieldId对应的FieldResponse里找参数值
		dependencyResponse := rundata.getFieldResponseByFieldId(dependentFieldId)
		if dependencyResponse == nil || len(dependencyResponse.responseRaws) == 0 {
			return nil, fmt.Errorf("dependent field id %d is out of range", dependentFieldId)
		}
		switch dependencyResponse.parentBindingMode {
		case fieldResponseBindingCompositeKey:
			// 生产者使用业务key绑定时，继续用当前父结果中的同名key查询，保持原有跨分支关联能力。
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

			dependencySource, exists = dependencyResponse.lookParentResponse(compositeKey)
			if !exists {
				return nil, fmt.Errorf("dependent field has no response for composite key %q", compositeKey)
			}
		case fieldResponseBindingResponsePath:
			// 无业务key时只能关联同一父occurrence，不能用顺序猜测跨分支结果关系。
			bindingKey, bindingKeyErr := responsePathBindingKey(parentResponsePath)
			if bindingKeyErr != nil {
				return nil, bindingKeyErr
			}
			var exists bool
			dependencySource, exists = dependencyResponse.lookParentResponse(bindingKey)
			if !exists {
				return nil, fmt.Errorf("dependent field %d has no response for parent occurrence path %v", dependentFieldId, parentResponsePath.AsArray())
			}
		case fieldResponseBindingNone:
			//没有批量数据，直接取dependencyResponse作为dependencySource
			if len(dependencyResponse.responseRaws) != 1 {
				return nil, fmt.Errorf("too many unbound responses for %d", dependentFieldId)
			}
			dependencySource = dependencyResponse.responseRaws[0]
		default:
			return nil, fmt.Errorf("dependent field %d has invalid parent binding mode", dependentFieldId)
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

	// 只有显式依赖父字段结果时才读取并筛选父结果，避免引入参数依赖图之外的隐藏父子依赖。
	for _, pp := range fieldPlan.bulkParamPlans {
		if pp == nil || pp.dependentFieldId != fieldPlan.parentFieldId {
			continue
		}
		if pp.paramType != PARAM_TYPE_ENUM_FIELD_RESPONSE_ATTRIBUTE && pp.paramType != PARAM_TYPE_ENUM_FIELD_RESPONSE_RAW {
			continue
		}
		if pp.dependentFieldId == 0 || uint64(pp.dependentFieldId) >= uint64(len(rundata.fieldResponses)) {
			return nil, fmt.Errorf("dependent field id %d is out of range", pp.dependentFieldId)
		}
		parentResponse := rundata.getFieldResponseByFieldId(pp.dependentFieldId)
		if parentResponse == nil || len(parentResponse.responseRaws) == 0 {
			return nil, fmt.Errorf("parent response is nil for field %d", pp.dependentFieldId)
		}
		result.parentResponseList = make([]any, 0, len(parentResponse.responseRaws))
		typeInfo := buildSGraphResolveInfo(rundata, fieldPlan, responsePathForFieldOccurrence(fieldPlan, nil))
		for _, raw := range parentResponse.responseRaws {
			if isNilInterfaceValue(raw) || !evaluateTypeShouldExecuteField(fieldPlan, raw, typeInfo, ctx) {
				continue
			}
			result.parentResponseList = append(result.parentResponseList, raw)
		}
		break
	}

	params := result.params
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
				return nil, fmt.Errorf("parent response is nil for field %d", pp.dependentFieldId)
			}

			if pp.dependentFieldId != fieldPlan.parentFieldId {
				//依赖参数来源是其他祖先或独立节点时，保持原有语义：读取该节点的全部结果。
				bulkVals := make([]any, 0, len(dependentResponse.responseRaws))
				for _, raw := range dependentResponse.responseRaws {
					bulkVals = append(bulkVals, getValueFromMapByPaths(raw, pp.fieldResponsePaths))
				}
				result.params[pp.paramKey] = bulkVals
				continue
			}

			values := make([]any, 0, len(result.parentResponseList))
			for _, parentResponse := range result.parentResponseList {
				values = append(values, getValueFromMapByPaths(parentResponse, pp.fieldResponsePaths))
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

func resolveSingleFieldResponseAttributeParam(paramPlan *ParamPlan, rundata *Rundata) (any, error) {
	if paramPlan == nil || rundata == nil {
		return nil, errors.New("single call FIELD_RESPONSE parameter is invalid")
	}
	if paramPlan.dependentFieldId == 0 || uint64(paramPlan.dependentFieldId) >= uint64(len(rundata.fieldResponses)) {
		return nil, fmt.Errorf("dependent field id %d is out of range", paramPlan.dependentFieldId)
	}
	dependentResponse := rundata.getFieldResponseByFieldId(paramPlan.dependentFieldId)
	if dependentResponse == nil || len(dependentResponse.responseRaws) == 0 {
		return nil, fmt.Errorf("dependent response is nil for field %d", paramPlan.dependentFieldId)
	}
	if len(dependentResponse.responseRaws) != 1 {
		return nil, fmt.Errorf("dependent field %d has more than one response", paramPlan.dependentFieldId)
	}
	value := dependentResponse.responseRaws[0]
	if len(paramPlan.fieldResponsePaths) == 0 {
		return value, nil
	}

	return getValueFromMapByPaths(value, paramPlan.fieldResponsePaths), nil
}

func resolveBulkDirectiveFieldResponseParam(fieldPlan *FieldPlan, paramPlan *ParamPlan, rundata *Rundata, ctx context.Context) ([]any, error) {
	if paramPlan == nil || paramPlan.dependentFieldId == 0 || uint64(paramPlan.dependentFieldId) >= uint64(len(rundata.fieldResponses)) {
		return nil, fmt.Errorf("dependent field id %d is out of range", paramPlan.dependentFieldId)
	}

	dependentResponse := rundata.getFieldResponseByFieldId(paramPlan.dependentFieldId)
	if dependentResponse == nil || len(dependentResponse.responseRaws) == 0 {
		return nil, fmt.Errorf("dependent field %d has no response", paramPlan.dependentFieldId)
	}

	values := make([]any, 0, len(dependentResponse.responseRaws))
	typeInfo := buildSGraphResolveInfo(rundata, fieldPlan, responsePathForFieldOccurrence(fieldPlan, nil))
	for _, response := range dependentResponse.responseRaws {
		if isNilInterfaceValue(response) {
			continue
		}
		if paramPlan.dependentFieldId == fieldPlan.parentFieldId && !evaluateTypeShouldExecuteField(fieldPlan, response, typeInfo, ctx) {
			continue
		}

		if len(paramPlan.fieldResponsePaths) == 0 {
			values = append(values, response)
			continue
		}

		values = append(values, getValueFromMapByPaths(response, paramPlan.fieldResponsePaths))
	}
	return values, nil
}
