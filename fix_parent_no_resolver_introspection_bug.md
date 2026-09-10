# 修复无 Resolver 父字段下 __typename 构建失败

## 1. 文档状态

- 状态：待确认的代码修改方案
- 本文档只描述方案，不代表源码已经修改
- 目标问题：无 resolver 的 List、Interface 或 Union 父字段下包含 __typename 时，Batch 依赖构建失败
- 设计约束：继续按照参数来源构建 Step 依赖图，不按照 GraphQL 父子层级强行串行化

### 1.1 Opus 审阅结论

| 编号 | 判断 | 结论与方案调整 |
| --- | --- | --- |
| P0 | 成立，但根因分析不完整 | `len(paramPlans) == 0` 确实会挡住 `fields(includeDeprecated: ...)` 等内省字段；更根本的是这些字段走 `compileCommonIntrospectionField()`，原方案只改 `compileFieldPlans()`，连无参数内省中间层也没有被物化。本版同时修正两条编译链路。 |
| P1 | 成立 | 内省中间结果按 responseName 写 map，原方案却用 `ResolveInfo.FieldName` 读取。改为结果组装和内部物化共用同一 property key 规则。未采用“新增隐藏 CONST 参数”的建议，避免污染参数模型和增加每个 occurrence 的参数 map 写入。 |
| P2 | 成立 | 普通业务字段的内部物化结果与父 occurrence 一一对应，不应要求父对象携带推断出的 ID。物化时强制清空 `parentKeyFieldName`，使用现有 responsePath 绑定。现有读取、组装和参数解析代码均已具备 responsePath 分支，无需再新增分支。 |
| P3 | “判断对象错误”不成立；可读性建议采纳 | `FieldPlan.fieldTypeScope` 保存的是字段所属父对象范围，因此 `child.fieldTypeScope` 实际就是当前字段的返回类型范围，原判断与 RAW 注入条件一致，并不依赖抽象类型 resolver 缺陷。为消除误读，本版改为显式传入当前字段返回类型范围，不再从 child 反推。 |
| P4 | 成立，并纳入本次修复 | nullable 父对象为 null 时，内部物化 Step 不应执行，也不应产生子字段 non-null 错误。在 `innerStepCalling()` 中对内部物化字段提前返回 `completionReady=false`。 |

Opus 关于 `__Type.inputFields` 也带 `includeDeprecated` 默认参数的描述与当前源码不一致：当前 `introspection.go` 仅为 `__Type.fields` 和 `__Type.enumValues` 定义该参数。这个事实错误不影响 P0 对 `fields`、`enumValues` 的结论。

## 2. 问题定义

当前 compileIntrospectionTypenameField() 在以下任一条件成立时，会为 __typename 注入一个指向逻辑父字段的 FIELD_RESPONSE_RAW 依赖：

1. 父字段是 List；
2. 父字段的返回类型是 Interface 或 Union，需要运行时动态类型推断。

例如：

~~~graphql
{
  page {
    holder {
      items {
        __typename
        id
      }
    }
  }
}
~~~

假设：

- page 配置了 resolver；
- holder 没有 resolver，只从 page 的结果读取；
- items 没有 resolver，只从 holder 的结果读取；
- items 是 List。

当前生成的依赖关系为：

~~~text
items.__typename Step
        |
        | FIELD_RESPONSE_RAW(items.fieldId)
        v
items FieldPlan
~~~

但是 items 没有 resolver，因此：

- 不会生成 Step；
- 不会在 Rundata 中产生独立 FieldResponse；
- coordinateBatches() 无法为这条依赖找到 producer。

最终返回类似错误：

~~~text
field 4 depends on field 3 which does not produce a FieldResponse
~~~

该错误发生在 Plan/Batch 构建阶段，因此公开执行入口最终表现为 data: null。

## 3. 修复原则

本方案采用“内部按需物化”：

1. 无 resolver 字段默认仍不生成 Step，继续在结果组装阶段从父对象读取属性；
2. 只有当下游 Step 必须使用该字段的运行时完整值时，编译器才为它安装内部物化 resolver；
3. 内部物化 resolver 从父字段已经完成的结果中读取当前属性，并写入 Rundata；
4. 多层无 resolver 字段会从下向上逐层物化，直到连接到真正拥有 resolver 的 producer；
5. 用户 resolver 仍然接收 nil Source，不恢复 graphql-go 的 Source fallback；
6. ParamRegistry 仍不能把无 resolver 字段配置为 FIELD_RESPONSE 来源；
7. 无 resolver 字段原本不执行的普通 runtime directive，不会因为内部物化而被启用；
8. @skip、@include、extension hook、动态类型判断、错误路径和 null bubbling 继续保留。

修复后的执行关系示例：

~~~text
Batch 0: page resolver
             |
             | internal FIELD_RESPONSE_RAW
             v
Batch 1: holder internal materialization
             |
             | internal FIELD_RESPONSE_RAW
             v
Batch 2: items internal materialization
             |
             | internal FIELD_RESPONSE_RAW
             v
Batch 3: items.__typename
~~~

这些依赖来自实际运行时数据来源，不是根据字段父子关系无条件增加。

## 4. 改动范围

必须修改 5 个源码文件：

| 文件 | 用途 |
| --- | --- |
| plan.go | 标记内部物化字段，控制执行语义 |
| plan_compiler.go | 识别并生成内部物化计划 |
| result_assembler.go | 提供与现有组装逻辑一致的属性读取方法 |
| plan_coordinator.go | 避免内部物化字段引入无效 directive 依赖 |
| plan_compiler_param_registry.go | 保持 ParamRegistry 的无 resolver 来源限制 |

还需要：

- 增加针对性功能测试；
- 更新 SGRAPH_USAGE_NOTES.md 中“无 resolver 字段永远不产生 Step”的绝对描述。

明确不需要修改：

- compileIntrospectionTypenameField()；
- Rundata；
- BatchPlan；
- SGraphEngine；
- 用户 resolver 的方法签名；
- 公开 Execute/Do 调用接口。

## 5. 逐文件代码方案

### 5.1 plan.go

#### 5.1.1 FieldPlan 增加内部物化标记

在 resolver 字段后增加：

~~~go
materializeFromParentSource bool // schema字段无resolver但下游Step依赖其结果时，从父结果读取属性并写入Rundata
~~~

该字段属于可缓存 Plan 的静态编译信息，不保存请求数据。

#### 5.1.2 限制普通 runtime directive

innerStepCalling() 中的普通 ShouldExecute、BeforeResolve 和 AfterResolve 逻辑分别使用以下条件包裹：

~~~go
if !fieldPlan.materializeFromParentSource {
	// 保留当前对应的directive执行逻辑
}
~~~

ShouldExecute 部分替换为：

~~~go
shouldExecute := true
if !fieldPlan.materializeFromParentSource {
	// 内部物化Step只补齐原本由组装阶段读取的属性，不扩大无resolver字段的自定义directive能力。
	var directiveEvaluateErr error
	shouldExecute, directiveEvaluateErr =
		evaluateCommonDirectivesShouldExecuteField(
			fieldPlan,
			paramContext,
			rundata,
			ctx,
		)
	if directiveEvaluateErr != nil {
		if fieldResponse == nil {
			fieldResponse = acquireFieldResponse()
		}
		rundata.addFieldErrorAtResponsePath(
			fieldPlan.fieldId,
			FieldErrorTypeField,
			directiveEvaluateErr,
			responsePath,
		)
		return fieldResponse, nil, true, StepExecuteFieldError
	}
	if !shouldExecute {
		return fieldResponse, nil, false, StepExecuteSuccess
	}
}
~~~

BeforeResolve 部分替换为：

~~~go
if !fieldPlan.materializeFromParentSource {
	if beforeResolvedParams, beforeResolvedParamsErr :=
		applyBeforeResolveDirectives(
			fieldPlan,
			paramContext,
			rundata,
			ctx,
		); beforeResolvedParamsErr != nil {
		rundata.addFieldErrorAtResponsePath(
			fieldPlan.fieldId,
			FieldErrorTypeField,
			beforeResolvedParamsErr,
			responsePath,
		)
		return fieldResponse, nil, true, StepExecuteFieldError
	} else {
		paramContext.setParams(beforeResolvedParams)
	}
}
~~~

AfterResolve 部分替换为：

~~~go
if !fieldPlan.materializeFromParentSource {
	afterResolvedResponse, afterResolvedErr :=
		applyAfterResolveDirectives(
			fieldPlan,
			paramContext,
			res,
			rundata,
			ctx,
		)
	if afterResolvedErr != nil {
		rundata.addFieldErrorAtResponsePath(
			fieldPlan.fieldId,
			FieldErrorTypeField,
			afterResolvedErr,
			responsePath,
		)
		return fieldResponse, nil, true, StepExecuteFieldError
	}
	res = afterResolvedResponse
}
~~~

@skip 和 @include 仍由 SingleCallStep.Execute()、IterationCallStep.Execute() 的外层逻辑执行。

在 `innerStepCalling()` 的 panic recover 建立后、普通 directive 判断前增加：

~~~go
// 父对象为null时GraphQL不会执行其子选择集。内部物化Step必须直接跳过，
// 避免为原本不会执行的non-null子字段额外记录完成错误。
if fieldPlan.materializeFromParentSource &&
	isNilInterfaceValue(paramContext.getParentResponse()) {
	return fieldResponse, nil, false, StepExecuteSuccess
}
~~~

这里返回 `completionReady=false`，因此 `SingleCallStep` 不发布 FieldResponse，`IterationCallStep` 不建立父 occurrence 绑定，下游依赖也会因 producer 没有结果而静默跳过。父对象本身的 null bubbling 仍由父字段处理。

#### 5.1.3 内部物化 resolver 显式接收父结果

在 execResolveProcess() 的 fieldPlan == nil 检查后增加：

~~~go
if fieldPlan.materializeFromParentSource {
	if resolverFn == nil {
		return nil, fmt.Errorf(
			"materialized source resolver is nil for field %s",
			fieldPlan.fieldName,
		)
	}
	// 只有内部物化Step接收父字段原始值；用户resolver仍不兼容graphql-go Source。
	return resolverFn(source, nil, info, ctx)
}
~~~

普通用户 resolver 的现有调用保持不变：

~~~go
return resolverFn(nil, params, info, ctx)
~~~

#### 5.1.4 内部物化字段忽略普通 directive 的 FIELD_RESPONSE 依赖

将 fieldDependenciesAvailable() 中参数组遍历入口替换为：

~~~go
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
		if paramType != PARAM_TYPE_ENUM_FIELD_RESPONSE_ATTRIBUTE &&
			paramType != PARAM_TYPE_ENUM_FIELD_RESPONSE_RAW {
			continue
		}

		dependentFieldId := paramPlan.dependentFieldId
		if uint64(dependentFieldId) >=
			uint64(len(rundata.fieldResponses)) {
			return false
		}

		fieldResponse :=
			rundata.getFieldResponseByFieldId(dependentFieldId)
		if fieldResponse == nil ||
			len(fieldResponse.responseRaws) == 0 {
			return false
		}
	}
}
return true
~~~

### 5.2 plan_compiler.go

#### 5.2.1 精确补充父字段 RAW 依赖

在 generateFieldId() 后增加：

~~~go
func ensureFieldResponseRawDependency(
	paramPlans []*ParamPlan,
	dependentFieldId uint32,
) []*ParamPlan {
	for _, paramPlan := range paramPlans {
		if paramPlan != nil &&
			paramPlan.paramType ==
				PARAM_TYPE_ENUM_FIELD_RESPONSE_RAW &&
			paramPlan.dependentFieldId == dependentFieldId {
			return paramPlans
		}
	}
	return append(
		paramPlans,
		newFieldResponseRawParamPlan(dependentFieldId),
	)
}
~~~

必须同时比较 paramType 和 dependentFieldId。仅判断已经存在某个 RAW 参数不够，因为该 RAW 可能依赖其他 producer。

#### 5.2.2 判断子字段是否需要当前字段的运行时值

增加：

~~~go
func childrenRequireParentRuntimeValue(
	children []*FieldPlan,
	parentIsList bool,
	parentTypeScope *FieldTypeScope,
) bool {
	parentRequiresRuntimeValue :=
		parentIsList ||
			(parentTypeScope != nil &&
				parentTypeScope.dynamicTypeResolver != nil)

	for _, child := range children {
		if child == nil {
			continue
		}

		// 已提升的子字段始终需要当前字段的原始值，逐层向上补齐物化链路。
		if child.materializeFromParentSource {
			return true
		}

		// 只识别编译器自身为逐元素执行或动态类型判定生成的父结果依赖。
		// ParamRegistry显式依赖无resolver字段仍由finalizeParamRegistry拒绝。
		if parentRequiresRuntimeValue &&
			child.resolverFunc != nil &&
			child.bulkResolverFunc == nil {
			return true
		}
	}
	return false
}
~~~

这里的 `parentTypeScope` 必须传当前字段返回类型编译出的 `fieldTypeScope`，不是当前字段所属父对象的 `parentTypeScope`。这样判断条件与子字段自动注入父 RAW 依赖时使用的范围完全一致，也不依赖 `child.fieldTypeScope` 的命名语义。

#### 5.2.3 判断内省参数是否允许内部物化

增加：

~~~go
func introspectionParamsAllowInternalMaterialization(
	paramPlans []*ParamPlan,
) bool {
	for _, paramPlan := range paramPlans {
		if paramPlan == nil {
			return false
		}
		switch paramPlan.paramType {
		case PARAM_TYPE_ENUM_CONST,
			PARAM_TYPE_ENUM_INPUT,
			PARAM_TYPE_ENUM_VAR_TEMPLATE:
			continue
		default:
			return false
		}
	}
	return true
}
~~~

内省中间层字段的参数已经由 `GenerateTypeMetaResult()` 等父级生成器通过 `argValue()` 读取并应用；内部物化 Step 只负责从已经生成的 map 中取属性，因此允许 CONST、INPUT 和变量模板。FIELD_RESPONSE 依赖和未知参数类型仍拒绝，不能借内省豁免扩大能力。

#### 5.2.4 compileFieldPlans 使用封装后的 resolver

参数计划完成后增加：

~~~go
resolverFunc := wrapFieldResolverFunc(fieldDefinition.Resolve)
bulkResolverFunc :=
	wrapFieldResolverFunc(fieldDefinition.BulkResolve)

usesNormalResolver :=
	resolverFunc != nil &&
		(!parentFieldIsList || bulkResolverFunc == nil)

needParentFieldResponseRaw :=
	parentFieldId > 0 &&
		usesNormalResolver &&
		(parentFieldIsList ||
			(parentTypeScope != nil &&
				parentTypeScope.dynamicTypeResolver != nil))

if needParentFieldResponseRaw {
	paramPlans = ensureFieldResponseRawDependency(
		paramPlans,
		parentFieldId,
	)
}
~~~

#### 5.2.5 编译完 children 后按需物化普通字段

在 childrenFieldsErr 检查后、构造 FieldPlan 前增加：

~~~go
materializeFromParentSource := false
if parentFieldId > 0 &&
	resolverFunc == nil &&
	bulkResolverFunc == nil &&
	len(paramPlans) == 0 &&
	childrenRequireParentRuntimeValue(
		childrenFields,
		fieldWrapperTypeInfo.isList,
		fieldTypeScope,
	) {
	// 无resolver字段只有在必须向下游Step提供原始值时才进入执行阶段，
	// 其他字段继续走结果组装阶段的默认属性读取路径。
	materializeFromParentSource = true
	// 物化值与父occurrence天然一一对应，不要求父对象携带业务ID。
	parentKeyFieldName = ""
	resolverFunc = newMaterializedSourceResolver(
		fieldValuePropertyKey(
			fieldName,
			responseName,
			getParentCompositeFromScope(parentTypeScope),
		),
	)
	paramPlans = ensureFieldResponseRawDependency(
		paramPlans,
		parentFieldId,
	)
}
~~~

len(paramPlans) == 0 用于保持现有能力边界：

- 字段存在实际参数计划但没有 schema resolver 时，仍然报错；
- 不会因为内部物化而静默忽略字段参数；
- ParamRegistry 向该字段注入参数后，也不会把它自动提升成可执行字段。

该限制只用于普通业务字段。内省字段使用上面的白名单判断，因为其 Query 参数已经在内省结果生成阶段生效。

清空 `parentKeyFieldName` 后不需要补新模型或新分支，现有链路已经闭合：

1. `IterationCallStep.bindIterationResponse()` 在 key 为空时用 `responsePathBindingKey(responsePath.Prev)` 写入父 occurrence 绑定；
2. `extractFieldResponse()` 的 `fieldResponseBindingResponsePath` 分支用当前组装路径的 `Prev` 读取同一绑定；
3. `resolveIterationFieldResponseAttributeParam()` 已支持 `fieldResponseBindingResponsePath`；
4. ParamRegistry 又明确拒绝内部物化字段作为外部 producer，因此不会新增跨分支语义。

这不是把普通 resolver 的 composite-key 规则全局改成 responsePath，只对内部物化字段生效；普通逐元素 resolver 和 Bulk resolver 的现有绑定规则不变。

FieldPlan 初始化中的相关字段改为：

~~~go
paramPlans:                  paramPlans,
resolverFunc:                resolverFunc,
bulkParamPlans:              arrParamPlans,
bulkResolverFunc:            bulkResolverFunc,
materializeFromParentSource: materializeFromParentSource,
~~~

#### 5.2.6 compileCommonIntrospectionField 同步按需物化

这是原方案遗漏的必要修改。`compileFieldPlansFromEntries()` 在 `isIntrospection=true` 时调用的是 `compileCommonIntrospectionField()`，不会进入 `compileFieldPlans()`。

在 `childrenFieldsErr` 检查后、返回 FieldPlan 前增加：

~~~go
materializeFromParentSource := false
var resolverFunc ResolverFunc
if parentFieldId > 0 &&
	introspectionParamsAllowInternalMaterialization(paramPlans) &&
	childrenRequireParentRuntimeValue(
		childrenFields,
		fieldWrapperTypeInfo.isList,
		fieldTypeScope,
	) {
	// 内省参数已在父级内省结果生成器中生效；
	// 物化Step只读取生成后的responseName属性。
	materializeFromParentSource = true
	resolverFunc = newMaterializedSourceResolver(
		fieldValuePropertyKey(
			fieldName,
			responseName,
			getParentCompositeFromScope(parentTypeScope),
		),
	)
	paramPlans = ensureFieldResponseRawDependency(
		paramPlans,
		parentFieldId,
	)
}
~~~

FieldPlan 初始化中增加：

~~~go
resolverFunc:                  resolverFunc,
materializeFromParentSource: materializeFromParentSource,
~~~

该改动覆盖：

- `__schema.types { __typename }`；
- `__type.fields { __typename }`；
- `fields(includeDeprecated: true)`；
- `fields(includeDeprecated: $variable)`；
- `enumValues(includeDeprecated: ...)`；
- 内省多层 List 中间字段。

### 5.3 result_assembler.go

增加统一属性 key 规则：

~~~go
// fieldValuePropertyKey统一结果组装和内部物化Step的属性取值规则。
// 普通业务对象使用schema fieldName；内省中间结果由生成器按responseName写入。
func fieldValuePropertyKey(
	fieldName string,
	responseName string,
	parentType Composite,
) string {
	if responseName == "" ||
		responseName == fieldName ||
		parentType == nil {
		return fieldName
	}
	switch parentType.Name() {
	case "__Schema", "__Type", "__Field",
		"__InputValue", "__EnumValue", "__Directive":
		return responseName
	default:
		return fieldName
	}
}
~~~

`extractFieldResponse()` 中现有 propertyKey 判断替换为：

~~~go
propertyKey := fieldValuePropertyKey(
	fieldPlan.fieldName,
	fieldPlan.responseName,
	fieldPlan.parentType,
)
~~~

在 `callDefaultResolveFn()` 后增加内部物化 resolver 工厂和读取逻辑：

~~~go
// newMaterializedSourceResolver只捕获编译期确定的属性key，
// 不保存任何请求级数据。
func newMaterializedSourceResolver(propertyKey string) ResolverFunc {
	return func(
		source any,
		_ map[string]any,
		info ResolveInfo,
		ctx context.Context,
	) (any, error) {
		return resolveMaterializedSourceField(
			source,
			propertyKey,
			info,
			ctx,
		)
	}
}

// resolveMaterializedSourceField用于按需提升的无resolver字段。
// 它只读取父对象属性，不改变用户resolver不接收graphql-go Source的既有约束。
func resolveMaterializedSourceField(
	source any,
	propertyKey string,
	info ResolveInfo,
	ctx context.Context,
) (any, error) {
	if isNilInterfaceValue(source) {
		return nil, nil
	}

	if sourceMap, ok := source.(map[string]any); ok {
		result := sourceMap[propertyKey]
		return result,
			deferredFunctionPropertyError(propertyKey, result)
	}

	// FieldResolver优先级与当前组装阶段一致，不能被反射map快路径绕过。
	if _, isFieldResolver := source.(FieldResolver);
		!isFieldResolver {
		sourceValue := reflect.ValueOf(source)
		if sourceValue.IsValid() &&
			sourceValue.Kind() == reflect.Map &&
			sourceValue.Type().Key().Kind() == reflect.String {
			mapKey :=
				reflect.New(sourceValue.Type().Key()).Elem()
			mapKey.SetString(propertyKey)

			mapValue := sourceValue.MapIndex(mapKey)
			if !mapValue.IsValid() {
				return nil, nil
			}
			result := mapValue.Interface()
			return result,
				deferredFunctionPropertyError(
					propertyKey,
					result,
				)
		}
	}

	result, err := callDefaultResolveFn(ResolveParams{
		Source:  source,
		Info:    info,
		Context: ctx,
	})
	if err != nil {
		return nil, err
	}
	return result,
		deferredFunctionPropertyError(propertyKey, result)
}

func deferredFunctionPropertyError(
	fieldName string,
	value any,
) error {
	if value == nil {
		return nil
	}
	valueType := reflect.TypeOf(value)
	if valueType == nil || valueType.Kind() != reflect.Func {
		return nil
	}
	return fmt.Errorf(
		"field %s resolves to a function value; "+
			"the result assembler does not evaluate deferred properties, "+
			"configure a resolver for this field instead",
		fieldName,
	)
}
~~~

这里不采用 Opus 建议的隐藏 CONST 参数。propertyKey 是 Plan 编译期常量，resolver 工厂只捕获一个不可变字符串；不会保存请求数据，也不会产生并发写。相比把内部 key 写入 ParamPlan，它不会：

- 混入 schema 参数和 ParamRegistry 参数模型；
- 与用户同名参数冲突；
- 在每个 List occurrence 的参数 map 中增加一次写入；
- 让属性 key 参与本不需要的参数解析和依赖扫描。

将 rejectDeferredFunctionProperty() 替换为：

~~~go
func rejectDeferredFunctionProperty(
	fieldPlan *FieldPlan,
	value any,
	rundata *Rundata,
) error {
	if fieldPlan == nil {
		return nil
	}
	err := deferredFunctionPropertyError(
		fieldPlan.fieldName,
		value,
	)
	if err == nil {
		return nil
	}
	if rundata != nil {
		rundata.addFieldErrorAtPlanPath(
			fieldPlan,
			FieldErrorTypeField,
			err,
			rundata.assemblyListIndexes,
		)
	}
	return err
}
~~~

该读取逻辑保留：

- map[string]any 快路径；
- 命名 string-key map 和 map[string]T；
- struct 字段；
- json、graphql tag；
- FieldResolver；
- typed nil；
- 函数属性仍返回错误，不执行延迟函数。

### 5.4 plan_coordinator.go

coordinateBatches() 构建依赖图时，将参数组入口替换为：

~~~go
dependencyParamPlans := [3][]*ParamPlan{
	fieldPlan.paramPlans,
	fieldPlan.bulkParamPlans,
	nil,
}
if !fieldPlan.materializeFromParentSource {
	dependencyParamPlans[2] =
		fieldPlan.directiveParamPlans
}

for _, paramPlans := range dependencyParamPlans {
	// 保留现有依赖校验和DAG建边逻辑
}
~~~

内部物化字段原本没有 resolver，也不会执行普通 runtime directive，因此不能仅因内部增加了 Step，就把这些 directive 的 FIELD_RESPONSE 依赖加入 Batch 图。

### 5.5 plan_compiler_param_registry.go

finalizeParamRegistry() 中的 producer 校验替换为：

~~~go
producer := fieldByID[sourceFieldIDs[0]]
if producer == nil ||
	producer.materializeFromParentSource ||
	(producer.resolverFunc == nil &&
		producer.bulkResolverFunc == nil) {
	return fmt.Errorf(
		"%s FIELD_RESPONSE source field %d has no resolver",
		dependency.target,
		sourceFieldIDs[0],
	)
}
~~~

该判断保证：

- 内部物化 resolver 只是引擎实现细节；
- ParamRegistry 不能把内部物化字段当成业务 resolver producer；
- 既有 dependentFieldId 只能指向 schema resolver 字段的规则保持不变。

## 6. Directive 和 Extension 语义

保持执行的能力：

- @skip；
- @include；
- extension 的 ResolveFieldDidStart/finish hook；
- 动态类型过滤；
- null bubbling；
- 字段错误和动态 List 下标路径。

不新增的能力：

- 普通 ShouldExecute runtime directive；
- BeforeResolve；
- AfterResolve。

这与当前结果组装阶段读取无 resolver 字段的行为一致。

## 7. ParamRegistry 能力边界

本方案只处理编译器内部为以下功能生成的父结果依赖：

- List 逐元素 Step；
- Interface/Union 动态类型推断；
- __typename。

外部 ParamRegistry 仍不得把无 resolver 字段配置为 FIELD_RESPONSE 来源。

如果某字段同时满足以下条件：

- schema 没有 resolver；
- 存在 ParamPlan；
- 下游又包含 __typename；

普通业务字段不会被内部物化，仍按现有规则返回“有参数计划但没有 resolver”的构建错误，避免静默忽略参数。

内省字段是受控例外：只允许 CONST、INPUT、VAR_TEMPLATE 参数，因为这些参数已由父级内省结果生成器应用；FIELD_RESPONSE 和未知参数类型不享受豁免。

## 8. 并发和缓存安全

materializeFromParentSource 是 Plan 编译期写入、运行期只读的布尔值。

内部物化 resolver：

- resolver 工厂只捕获编译期确定的 propertyKey 字符串；
- 不保存请求参数；
- 不保存父对象；
- 不保存 Rundata；
- 不修改缓存 Plan；
- 每次请求仍使用独立 Rundata 和 FieldResponse。

因此不会新增：

- 跨请求数据泄漏；
- Plan cache 请求数据污染；
- resolver 闭包持有请求对象；
- 并发读写竞态；
- FieldPlan 与 Rundata 的循环引用。

闭包不捕获 FieldPlan、Schema、Rundata、source、请求变量或 context；propertyKey 在 Plan 发布后不可变。它只在 Plan 编译时创建一次，不产生每请求闭包分配。

## 9. 性能影响

不受影响的字段：

- 已配置 resolver 的字段；
- 没有下游运行时依赖的无 resolver 字段；
- 静态、非 List 父类型下可以直接并发执行的 resolver；
- Bulk resolver。

受影响的字段会增加必要的内部 Step：

~~~text
父resolver
  -> 无resolver对象物化
  -> 无resolver List/Abstract字段物化
  -> __typename或普通逐元素resolver
~~~

新增成本包括一次父属性读取、一个 FieldResponse、对应依赖图的一条边，以及必要时增加一个 Batch 层级。这些成本只发生在原实现无法构建和执行的场景中。

## 10. 测试方案

必须新增：

1. 多层无 resolver Object；
2. 无 resolver List 下的 __typename；
3. 无 resolver Interface/Union 下的 __typename；
4. alias、inline fragment、named fragment；
5. @skip 和 @include；
6. typed slice；
7. struct、命名 map 和 FieldResolver 父结果。
8. `__schema { types { __typename name } }`；
9. `__type(name:"Query") { fields { __typename name } }`，覆盖默认参数；
10. `fields(includeDeprecated:true)` 和 `enumValues(includeDeprecated:true)`；
11. `fields(includeDeprecated:$include)`，覆盖请求变量且验证 Plan cache 不保存变量值；
12. `__schema { myTypes: types { __typename name } }`，验证内省 alias 不静默丢值；
13. 父 List 类型声明 `id: ID!`、实际父 map 不返回 id，验证内部物化仍按 responsePath 绑定；
14. nullable 父对象返回 null、其下物化字段为 non-null，验证不新增子字段错误；
15. GraphiQL/codegen 使用的完整标准内省查询。

必须保持：

1. 用户 resolver 的 ResolveParams.Source 仍为 nil；
2. ParamRegistry 依赖内部物化字段仍报无 resolver；
3. 无 resolver 字段的普通 runtime directive 不执行；
4. 无 resolver 字段有实际 ParamPlan 时仍报错；
5. root 字段无 resolver 时仍报错；
6. 函数属性仍返回字段错误；
7. extension hook 每个字段 occurrence 只执行一次；
8. null bubbling 的错误路径包含完整 List 下标。

第 4 项限定为普通业务字段；内省 Query 参数按上面的受控白名单处理。

不采纳“644 条矩阵中必须恰好 11 条由 FAIL 变 PASS”的固定数字门槛。原因是当前矩阵并没有稳定标注这 11 条与该缺陷的一一映射，而且后续新增用例会改变总数。正确门槛是：

1. 上述新增用例全部通过；
2. 修改前已经通过且与本次链路相关的用例不得退化；
3. 对修改前后矩阵按 case ID 做差异，逐条解释每一个状态变化；
4. 聚焦用例运行 `go test -race`，全仓执行仅在既有失败有基线分类时判断是否新增失败。

## 11. 隔离验证结果

修正版候选代码只应用在 `/tmp` 隔离副本，项目源码未修改。

本轮新增并执行的聚焦场景：

- `__schema.types` 下 `__typename`；
- `__type.fields` 默认参数、显式参数和变量参数；
- 内省 `types` alias 并断言 alias 对应列表非空；
- 父类型声明 `id: ID!` 但父结果没有 id；
- nullable 父对象为 null 且下级物化字段 non-null。

结果：

~~~text
go test -run '^TestOpusCandidate' -count=1
PASS

go test -race -run '^TestOpusCandidate' -count=1
PASS

go test ./... -run '^$'
PASS

go test -run '^TestGraphQLGoSpec_Introspection' -count=1
PASS
~~~

原问题的未修复表现仍为：

未修复代码：

~~~text
field 4 depends on field 3 which does not produce a FieldResponse
~~~

同时通过的相关回归范围还包括：

- 原有 __typename 测试；
- Interface/Union 动态类型；
- Query folding 抽象类型与 fragment；
- nested List occurrence path；
- 全仓仅编译检查：go test ./... -run '^$'。

`TestSpec2025_Introspection_*` 全组仍存在仓库既有的 September 2025 能力缺失，例如 `specifiedByURL`、`isOneOf`、`isRepeatable` 和部分新内省参数。这些失败与本修复无关，不能据此宣称候选方案全量通过，也不能为了本修复调整其预期。

## 12. SGRAPH_USAGE_NOTES.md 调整

应补充：

> schema 中没有 resolver 的字段通常不生成 Step，也不能作为 ParamRegistry 的 FIELD_RESPONSE 来源。唯一例外是编译器为了支持 List 逐元素执行或 Abstract/__typename 运行时类型判定，可以按需生成内部物化 Step。该 Step 仅用于把父对象中已经完成的属性值写入 Rundata，不属于用户 resolver，不开放为 ParamRegistry 数据源，也不会启用无 resolver 字段的普通 runtime directive。

## 13. 最终汇总

实际需要修改：

- plan.go
- plan_compiler.go
- result_assembler.go
- plan_coordinator.go
- plan_compiler_param_registry.go
- 对应测试文件
- SGRAPH_USAGE_NOTES.md

明确无需修改：

- compileIntrospectionTypenameField()
- Rundata
- BatchPlan
- SGraphEngine
- graphql-go 公开入口
- 用户 resolver 签名和 Source 策略
