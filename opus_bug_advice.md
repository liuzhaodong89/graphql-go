# 对 fix_parent_no_resolver_introspection_bug.md 的审查结论

## 1. 审查信息

- 审查对象：`fix_parent_no_resolver_introspection_bug.md`（待确认的代码修改方案，744 行）
- 审查对象要解决的问题：无 resolver 的 List / Interface / Union 父字段下包含 `__typename` 时，Batch 依赖构建失败，返回 `data: null`
- 审查方式：逐节比对方案与当前 `master` 工作副本源码；对关键判断做实测复现（测试文件已在验证后删除，工作树未被本次审查修改）
- 审查结论：**方案骨架可用，但落地前必须修掉 1 个阻断性缺陷、2 个实质缺陷**（初版另报的 P3 已撤回，见 §6）

## 2. 结论摘要

| 编号 | 严重度 | 问题 | 位置 |
|---|---|---|---|
| P0 | **阻断** | 内省中间层字段无法物化，方案修不掉内省 | §5.2.4 |
| P1 | 实质 | `info.FieldName` 丢掉了内省的 responseName 取值规则，alias 场景静默取空 | §5.3 |
| P2 | 实质 | 未处理 `parentKeyFieldName`，物化字段会被强加业务 key 绑定要求 | §5.2.4 |
| ~~P3~~ | **已撤回** | 原判断「dynamic 判断对象错误」不成立，见 §6 | §5.2.2 |
| P4 | 次要 | 父对象为 null 时会多产出一条原生不存在的 non-null 错误 | §5.3 |

> **本文修订说明**：初版对 P0 的归因不完整、P3 判断错误，另有两处事实错误（`inputFields` 的参数、§9 的验收门槛）。
> 均已在下文对应位置更正，更正内容以本版为准。

方案中 §5.1.2、§5.1.4、§5.4、§5.5 四处设计完备，建议原样保留，理由见第 8 节。

## 3. P0（阻断）—— 方案修不掉内省

### 3.1 缺陷

§5.2.4 把 `len(paramPlans) == 0` 作为物化前提：

```go
materializeFromParentSource := false
if parentFieldId > 0 &&
	resolverFunc == nil &&
	bulkResolverFunc == nil &&
	len(paramPlans) == 0 &&          // ← 问题所在
	childrenRequireParentRuntimeValue(
		childrenFields,
		fieldWrapperTypeInfo.isList,
	) {
```

`compileParamPlansByArgDefs`（`plan_compiler.go`）对**有默认值但未在查询中提供**的参数同样会产出 CONST paramPlan：

```go
if !provided {
	if argDef.DefaultValue != nil {
		result = append(result, newConstParamPlan(argDef.PrivateName, argDef.DefaultValue))
		continue
	}
	...
}
```

而内省的两个关键中间层字段带默认值参数。`introspection.go:522-529`：

```go
TypeType.AddFieldConfig("fields", &Field{
	Type: NewList(NewNonNull(FieldType)),
	Args: FieldConfigArgument{
		"includeDeprecated": &ArgumentConfig{
			Type:         Boolean,
			DefaultValue: false,      // ← 查询不写也会生成 CONST paramPlan
		},
	},
```

`__Type.fields` 与 `__Type.enumValues` 是这一形态。

> **更正**：初版称 `__Type.inputFields` 也带 `includeDeprecated`，与源码不符——`introspection.go:613` 定义的 `inputFields` 没有任何参数。
> 但 `inputFields` 同样是无 resolver 的内省中间层，仍然触发本缺陷，只是原因属于下面补充的那条主因，而非默认值参数。

### 3.2 实测

当前代码下三个查询的实际返回：

```
{ __schema { types { __typename name } } }
  => {"data":null,"errors":[{"message":"field 3 depends on field 2 which does not produce a FieldResponse"}]}
{ __type(name:"Query") { fields { __typename name } } }
  => {"data":null,"errors":[{"message":"field 3 depends on field 2 which does not produce a FieldResponse"}]}
{ __type(name:"Query") { fields(includeDeprecated: true) { __typename name } } }
  => {"data":null,"errors":[{"message":"field 3 depends on field 2 which does not produce a FieldResponse"}]}
```

按方案应用后的预期结果：

| 查询 | `fields`/`types` 的 paramPlans | 能否物化 |
|---|---|---|
| `{ __schema { types { __typename name } } }` | 空（`types` 无参数定义） | 能修 |
| `{ __type(name:"Query"){ fields { __typename name } } }` | `[CONST(includeDeprecated,false)]` | **修不掉** |
| `{ __type(name:"Query"){ fields(includeDeprecated:true){ __typename } } }` | 同上 | **修不掉** |

GraphiQL、graphql-codegen、Apollo Client 的标准内省查询必然包含 `fields(includeDeprecated: true)` 与 `enumValues(includeDeprecated: true)`，因此真实内省流量落在"修不掉"一栏。

### 3.3 旁证：方案与验证都没有覆盖内省

- §2 的问题定义使用自定义 schema（`page` / `holder` / `items`），不是内省
- §10 的测试方案共 7 项，**没有任何一项是内省用例**
- §11 的"隔离验证 PASS"仅针对 §2 那个自定义用例

文档文件名为 `fix_parent_no_resolver_introspection_bug.md`，但方案设计与验收证据都未真正触及内省路径。

### 3.3.1 补充：更根本的主因是编译链路没有覆盖

初版把 `len(paramPlans) == 0` 当作 P0 的根因，这只是次要原因。更根本的是：
内省字段走的是**独立的编译链路**。`compileFieldPlansFromEntries()` 在 `isIntrospection=true` 时
调用 `compileCommonIntrospectionField()`（`plan_compiler.go:826`，`default` 分支），
根本不进入 `compileFieldPlans()`。

而原方案的物化逻辑只加在 `compileFieldPlans()` 里，因此**所有**内省中间层字段
（无论有无参数，包括无参数的 `__schema.types`）都不会被物化。
`len(paramPlans) == 0` 只是在此之上又多挡了一层。

结论「方案修不掉内省」成立，但必须同时修正两条编译链路才能真正解决。

### 3.4 修正建议

`len(paramPlans) == 0` 的本意是保住"有参数计划却没有 resolver 就报错"这条既有边界（`plan_coordinator.go:263-274` 的 `field %d has param plan but no resolver`）。

但那条检查本身已经对内省坐标做了豁免，`plan_coordinator.go:262`：

```go
isIntrospectionResultField := fieldPlan.parentType != nil &&
	isIntrospectionCoordinate(fieldPlan.parentType.Name(), fieldPlan.fieldName)
if step == nil && !isIntrospectionResultField {
	// 报 has param plan but no resolver
}
```

因此物化判断应当采用**与之一致的豁免口径**（复用 `isIntrospectionCoordinate`），而不是一刀切的 `len(paramPlans) == 0`。否则同一个"无 resolver 但有参数"的字段，在两处判断中被赋予互相矛盾的语义。

若希望更保守，可将条件收紧为"除 CONST 之外没有其它类型的 paramPlan"，这样既排除了真正的业务参数与 ParamRegistry 注入，又不会被默认值 CONST 误伤。

## 4. P1（实质）—— 丢掉内省的 responseName 取值规则

### 4.1 缺陷

§5.3 的物化 resolver 用 `info.FieldName` 作为属性名：

```go
propertyKey := info.FieldName
```

而 `info.FieldName` 填的是 schema 字段名，`extensions.go:279-283`：

```go
func buildSGraphResolveInfo(rundata *Rundata, fieldPlan *FieldPlan, path *ResponsePath) ResolveInfo {
	info := ResolveInfo{
		FieldName:      fieldPlan.fieldName,     // ← schema 字段名，不是 responseName
```

但内省中间结果是按 **responseName** 生成的。`GenerateTypeMetaResult`（`plan_compiler.go:1434`）通篇使用 `child.getResponseName()` 作为 map key：

```go
result[child.getResponseName()] = GenerateFieldsMetaResult(...)
result[child.getResponseName()] = GenerateEnumValuesMetaResult(...)
result[child.getResponseName()] = GenerateInputFieldsMetaResult(...)
```

现有的 `extractFieldResponse`（`result_assembler.go`）**正确处理了**这个差异：

```go
// 普通业务对象使用 schema fieldName；内省中间结果当前已经按照 responseName 生成。
propertyKey := fieldPlan.fieldName
if fieldPlan.responseName != propertyKey && fieldPlan.parentType != nil {
	switch fieldPlan.parentType.Name() {
	case "__Schema", "__Type", "__Field", "__InputValue", "__EnumValue", "__Directive":
		propertyKey = fieldPlan.responseName
	}
}
```

§5.3 的说明声称"该读取逻辑保留：map 快路径、命名 map、struct、json/graphql tag、FieldResolver、typed nil、函数属性"——这些分支确实都保留了，但**漏掉了 propertyKey 的计算规则本身**，而这恰好是内省场景的关键。

### 4.2 后果

```graphql
{ __schema { myTypes: types { __typename name } } }
```

`types` 无参数定义，不受 P0 影响，会被正常物化。但物化 resolver 用 `"types"` 去父 map 取值，而 `GenerateTypeMetaResult` 写入的 key 是 `"myTypes"`，取到 nil，导致 `__typename` 迭代 0 次、字段静默缺失。**无错误、无报警，是静默数据缺失。**

### 4.3 修正建议

propertyKey 必须与 `extractFieldResponse` 使用同一份计算。建议抽取为共享函数，例如：

```go
// valueExtractPropertyKey 计算从父对象读取属性时使用的 key。
// 普通业务对象使用 schema fieldName；内省中间结果由 GenerateTypeMetaResult 按 responseName 生成。
// 结果组装阶段与内部物化 Step 必须使用同一份规则。
func valueExtractPropertyKey(fieldPlan *FieldPlan) string
```

由 `extractFieldResponse` 与物化 resolver 共同调用。物化 resolver 拿不到 `fieldPlan`，因此需要在编译期把算好的 propertyKey 通过 CONST 参数传入（与 `__typename` 用 `DefaultFieldKeyTypename` 传类型名的既有模式一致，见 `plan_compiler.go:1018`），而不是在运行期依赖 `info.FieldName`。

两处各写一份取值规则，日后必然漂移，这是长期维护成本最高的一种写法。

## 5. P2（实质）—— 未处理 parentKeyFieldName

### 5.1 缺陷

物化字段若**父字段是 List**，其 `parentKeyFieldName` 仍由 `checkAndCompileParentKeyFieldNames` 推断（`plan_compiler.go:883`）。只要父 List 的元素类型声明了 `id: ID`（或恰好只有一个 ID 类型字段），推断结果即为非空。

非空的 `parentKeyFieldName` 会让 `bindIterationResponse` 走 composite key 分支（`plan.go:651-685`），**硬性要求父元素是 `map[string]any` 且携带该 key**：

```go
parentMap, ok := parentResponse.(map[string]any)
if !ok {
	// parent response for field %s dose not support composite key mapping
}
if _, exist := parentMap[keyFieldName]; !exist {
	// parent key field %q is missing for field %s
}
```

### 5.2 后果

物化字段做的是纯属性提取，其值天然按 occurrence 对应父元素，不需要业务 key 关联。保留业务 key 会让中间层凭空多出"父 resolver 必须返回 id 且逐元素唯一"的要求；父未返回时报 `parent key field "id" is missing`，即把已知缺陷 P1-4 那一类失败模式引入物化字段。

场景示例：父 List 元素类型声明 `id: ID!`，但父 resolver 未返回 `id`，中间层 `holder` 无 resolver 且下游有 `__typename`。

### 5.3 修正建议

物化时显式置空：

```go
// 属性提取的结果天然按 occurrence 对应父元素，不需要业务 key 关联。
// 保留业务 key 会让中间层额外要求父元素携带 ID 字段，与取值语义无关，故强制走 responsePath 绑定。
field.parentKeyFieldName = ""
```

### 5.4 附带待查项

置空后走 responsePath 绑定模式。`resolveIterationFieldResponseAttributeParam`（`plan.go:1541` 起）中的 `switch dependencyResponse.parentBindingMode`，本次审查只确认了 `fieldResponseBindingCompositeKey` 分支，**responsePath 模式的分支是否齐备未确认**。若调用方通过 ParamRegistry 让其它字段依赖一个物化字段，需要先补齐该分支。场景边缘，但应在实现时验证。

## 6. ~~P3~~（已撤回）—— dynamic 判断对象错误的判断不成立

**初版结论错误，此处撤回。**

初版认为 `childrenRequireParentRuntimeValue` 用 `child.fieldTypeScope` 判断 dynamic 与
RAW 注入条件（用 `parentTypeScope`）不对应，并推断其正确性依赖 P1-5 缺陷。该推断基于对字段名的误读。

`FieldPlan.fieldTypeScope` 保存的**不是**字段自身的返回类型范围，而是**字段所属父对象的范围**。
`plan_compiler.go:970-971` 有明确注释：

```go
// FieldPlan保存字段所属父对象的类型范围；字段返回类型范围只用于递归编译childrenFields。
fieldTypeScope:             parentTypeScope,
```

第 933 行由 `wrapTypeDefinition2Scope(fieldWrapperTypeInfo.baseType, compiler)` 算出的同名局部变量
只用于递归编译 childrenFields，不写入 FieldPlan。

因此 `child.fieldTypeScope` 恰好就是当前字段的返回类型范围，原判断与 RAW 注入条件一致，
`plan_coordinator.go:242` 用 `fieldPlan.fieldTypeScope` 与 `plan_compiler.go:910` 用 `parentTypeScope`
指的是同一个东西，不存在不一致，也不依赖 P1-5。

初版引用的实测（`{ wrap { pet { label } } }` 返回 `FROM-MAP` 而非 resolver 的值）事实本身成立，
那是 P1-5 的独立表现，但不能用来支撑本条结论。

**唯一仍然成立的部分是可读性**：`fieldTypeScope` 这个字段名与其实际语义不一致，容易误读
（本次即因此误判）。建议在判断函数中显式传入当前字段的返回类型范围，不从 `child` 反推。

## 7. P4（次要）—— 父对象为 null 时多一条错误

§5.3：

```go
if isNilInterfaceValue(source) {
	return nil, nil
}
```

父对象为 null 时，若物化字段的返回类型是静态类型，`evaluateTypeShouldExecuteField` 会走 `evaluateCompiledTypeShouldExecuteField`（`plan.go:981-992`），该分支**不检查 parentResponse**：

```go
func evaluateTypeShouldExecuteField(fieldPlan *FieldPlan, parentFieldResponseRaw any, info ResolveInfo, ctx context.Context) bool {
	shouldExecute := false
	if isFieldPlanTypeCompiled(fieldPlan) {
		shouldExecute = evaluateCompiledTypeShouldExecuteField(fieldPlan, ctx)   // ← 不看 parentResponse
	} else {
		if parentFieldResponseRaw == nil {
			return shouldExecute
		}
		...
```

因此物化 Step 仍会执行并返回 nil；若该字段是 non-null，`processNullValueBubbling`（`plan.go:851`）会产生一条 completion error。原生链路在父为 null 时不进入子选择集，没有这条错误。

修正需要在 Step 层让物化字段在 source 为 nil 时不产出 FieldResponse（`completionReady = false`），resolver 内部无法解决。

优先级低：当前该场景直接返回 request error，任何行为都是改进，可作为后续项。

## 8. 建议原样保留的部分

以下四处设计完备，是方案中价值最高的部分：

| 节 | 内容 | 为什么必要 |
|---|---|---|
| §5.1.2 | 用 `if !materializeFromParentSource` 关闭普通 runtime directive 的 ShouldExecute / BeforeResolve / AfterResolve | 无 resolver 字段原本不执行这些 directive；物化若不加限制会静默启用一项新能力，属于行为变更 |
| §5.1.4 | `fieldDependenciesAvailable` 对物化字段排除 `directiveParamPlans` | 与 §5.1.2 配套：directive 既然不执行，就不应参与依赖可用性判断 |
| §5.4 | `coordinateBatches` 对物化字段排除 `directiveParamPlans` | 同上，避免仅因内部新增 Step 就把 directive 的 FIELD_RESPONSE 依赖引入 DAG |
| §5.5 | `finalizeParamRegistry` 显式排除 `materializeFromParentSource` | 物化字段的 resolverFunc 非 nil，若不显式排除会被 ParamRegistry 当作合法业务 producer，突破既有边界 |

另外 §5.2.1 的 `ensureFieldResponseRawDependency` 同时比较 `paramType` 与 `dependentFieldId` 是正确的，注释中指出的"仅判断存在某个 RAW 参数不够"这一点成立。

`materializeFromParentSource` 作为编译期写入、运行期只读的布尔值，以及物化 resolver 为无闭包包级函数，这两点符合"Plan 编译完成后冻结"的既有约定（`sgraph_engine.go:146`），§8 的并发与缓存分析没有问题。

## 9. 验收补充建议

§10 的测试清单需要补入以下用例，它们正是当前缺失的部分：

1. `{ __schema { types { __typename name } } }`
2. `{ __type(name:"X") { fields { __typename name } } }` —— 覆盖 P0，带默认值参数
3. `{ __type(name:"X") { fields(includeDeprecated: true) { __typename name } } }` —— 覆盖 P0，显式传参
4. `{ __type(name:"X") { enumValues(includeDeprecated: true) { __typename name } } }`
5. `{ __schema { myTypes: types { __typename name } } }` —— 覆盖 P1，内省 alias
6. 父 List 元素类型声明 `id: ID!` 但 resolver 未返回该字段，中间层无 resolver —— 覆盖 P2
7. GraphiQL / codegen 的完整标准内省查询作为端到端用例

关于回归门槛——**初版在此处写错，现更正**：

初版要求「`SPEC2025_CASE_MATRIX.csv` 的 644 条只允许 P0-1 相关的 11 条由 FAIL 转 PASS」。
这是错的：那 11 条的失败原因是 `unexpected errors: [no roots found]`，属于**另一个** P0 缺陷
（根选择集被字面量 `@skip`/`@include` 全部裁剪）。

矩阵中 `does not produce a FieldResponse` 的出现次数为 **0**——本缺陷在 spec2025 的 526 条用例中
**完全没有覆盖**，这正是该矩阵的盲区所在。

因此正确的门槛是：修复本缺陷后 `SPEC2025_CASE_MATRIX.csv` 应当 **0 条状态变化**；
本缺陷的验收必须依赖新增的专项用例，而不是既有矩阵。`go test -race` 无 DATA RACE 的要求不变。

§11 关于"全量功能测试包含仓库既有的规范能力失败，不能以全量通过作为验收结论"的说明是准确的，逐用例对照矩阵是正确的替代方案。

## 10. 处理顺序建议

1. 先修 P0，否则方案无法达成其命名所声称的目标
2. P1 与 P2 与 P0 同批修正，三者都在同一段编译期代码内，分批改动反而增加回归面
3. ~~P3~~ 已撤回，无需处理；仅建议顺带采纳 §6 末尾的可读性改进（显式传入当前字段返回类型范围）
4. P4 作为独立后续项，与"null 父仍调用子 resolver"（已知缺陷 P1-6）一并处理更合适
