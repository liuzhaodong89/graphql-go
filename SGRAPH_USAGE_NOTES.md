# sgraph 新执行引擎使用注意事项

本文档记录 sgraph 新执行引擎在使用和维护过程中的必要约束。使用者在配置参数依赖、维护计划构建逻辑或扩展依赖解析能力前，应先确认相关改动没有违反本文档中的约束。

## Engine 生命周期与并发使用约束

`SGraphEngine` 绑定一个固定的 `Schema`、`DirectiveRegistry` 快照和 `ParamRegistry` 快照，并持有该组合对应的 Plan 缓存。完成初始化后，同一个 Engine 允许被多个请求并发调用，但必须遵守以下生命周期：

1. 创建并完整初始化 Schema，包括类型、字段和 resolver；
2. 通过 `NewDirectiveRegistry()` 创建包含 `@skip`、`@include` 的默认指令注册表，再完成自定义指令和 `ParamRegistry` 的全部注册；
3. 调用 `NewSGraphEngine` 创建 Engine；
4. 在任何请求执行前调用 `RegisterSGraphEngine`，将 Engine 与 Schema 绑定；
5. Engine 创建后，Schema 以及已注册的 compiler、runtime handler、resolver 等共享对象保持只读；
6. 请求仍使用 graphql-go 原始 `Params` 调用 `graphql.Do`，执行层根据 Schema 复用已绑定的 Engine。

`SGraphEngine` 是 Schema 级对象，不是请求参数。同一 Schema 只允许绑定一个 Engine；重复绑定其他 Engine 会返回错误。未显式注册时，公开 Query 执行链路会为该 Schema 创建不包含自定义 Registry 元数据的默认 Engine；因此使用自定义 Registry 时必须先完成注册，再开始接收请求。

### Engine 注册必须在第一条请求到达之前完成

这是 sgraph 的既定设计：**Engine 与 Schema 的绑定是一次性的、进程内不可变的**。绑定关系一旦建立就不再改变，Plan 缓存、两个 Registry 快照和 Schema 的对应关系在整个进程生命周期内保持稳定，运行期不需要任何同步或失效逻辑。

由此推出的启动顺序要求：

```
建 Schema
  → NewDirectiveRegistry() + 注册全部自定义指令
  → NewParamRegistry() + 注册全部参数依赖
  → NewSGraphEngine(schema, directiveRegistry, paramRegistry)
  → RegisterSGraphEngine(engine)
  → 开始接收流量        ← 第一条请求必须晚于这一步
```

`getSGraphEngineForSchema` 提供的兜底逻辑只覆盖"完全不使用自定义 Registry"的场景：未注册时它会为该 Schema 创建一个只含 `@skip`、`@include` 的默认 Engine 并写入全局缓存。这个缓存写入同样遵循"绑定一次性"的设计——之后再调用 `RegisterSGraphEngine` 会返回 `another sgraph engine is already registered for this schema`，框架不提供解绑或替换接口。

因此，如果应用使用了自定义指令或参数依赖，却让请求早于注册到达，该 Schema 在本进程内会一直使用那个不含自定义配置的默认 Engine，且无法纠正——只能重启进程。**这不是可恢复的错误状态，而是启动顺序错误的确定性后果。**

接入检查清单：

- HTTP 路由 / RPC 服务的注册必须晚于 `RegisterSGraphEngine`；
- 健康检查、预热请求、启动自检同样算"请求"，不能早于注册；
- 多 Schema 场景下每个 Schema 各自完成一遍上述顺序；
- `RegisterSGraphEngine` 的返回值必须检查，返回错误说明该 Schema 已被绑定，应当视为启动失败而不是忽略。

### 自定义指令必须全部登记

文档中出现的每一个指令都必须在 `DirectiveRegistry` 里有归属，否则 Plan 编译阶段返回 `no directive compiler found for <name>`，整个请求变成 request error：

- 有执行语义的指令：`Register(name, compiler, handler)`，写法与内置的 `SkipDirectiveCompiler` / `IncludeDirectiveRuntimeHandler` 一致；
- 纯标注型、执行期无语义的指令：`RegisterMetadataOnly(name)`。

注意 `SchemaConfig.Directives` 与 `DirectiveRegistry` 是两份独立配置，框架不做交叉对账。从 graphql-go 迁移时尤其要留意：原生链路对"schema 声明了但执行期无语义"的指令是零成本忽略的，迁到 sgraph 后必须逐个补登记。建议在启动阶段遍历 `schema.Directives()` 与 Registry 对账一次，把配置缺失暴露在启动期而不是线上。

### Schema 和 Registry 创建后不得热更新

Engine 创建时会克隆并冻结两个 Registry，但不会克隆 Schema：

- 后续修改传入的原始 `DirectiveRegistry` 或 `ParamRegistry`，不会影响已经创建的 Engine；
- 新配置需要生效时，必须使用完整的新配置创建新的 Engine；
- 不应使用零值 `&DirectiveRegistry{}` 代替 `NewDirectiveRegistry()`，否则注册表不会自动包含 `@skip`、`@include` 的默认 compiler 和 runtime handler；
- Engine 创建后不得继续修改其绑定 Schema 的类型、字段、resolver、directive 或 extension；
- 不同 Schema 或不同 Registry 配置必须使用不同 Engine，不能共享同一个 Plan 缓存。

在 Engine 执行期间修改 Schema，不仅可能与 Plan 编译形成并发读写，还会使已缓存 Plan 与当前 Schema 不一致。

### 自定义扩展对象必须保证并发安全

Registry 的冻结只保证“指令名称到对象”的映射不再变化，不会复制或保护 compiler、runtime handler 自身的内部状态。同一个注册对象可能被多个请求共享：

- 不同未缓存 Query 的 Plan 可能同时调用同一个 `DirectiveCompiler`；
- 同一 batch 中的多个 Step 以及多个并发请求可能同时调用同一个 `DirectiveRuntimeHandler`；
- resolver、array resolver、动态类型解析函数以及其他运行期回调也可能被多个 Step 或请求并发调用。

因此这些对象和函数应保持无请求状态。请求参数、ResolveInfo 和 extension 状态必须通过方法参数或 `Rundata` 显式传递，不能写入共享对象字段，也不能新增私有 context key 隐式传递；`context.Context` 只用于取消、超时和调用链上下文。确实需要维护共享状态时，实现方必须自行使用锁、原子变量或并发安全的数据结构。

Registry 自身的互斥锁只保护注册表中的 map，不保护注册进去的 compiler 或 handler。

### ParamRegistry 常量值必须视为不可变数据

`ParamRegistry` 会复制外部参数配置，JSON 解码产生的标量、map 和 slice 可以作为 `CONST` 使用。对于自定义 scalar 或调用方直接构造的 Go 值，如果 struct 内部仍引用 map、slice、pointer 或其他可变对象，不能依赖 Registry 自动隔离其全部内部状态。

配置注册完成后，调用方不得继续修改 `CONST` 引用的可变数据；compiler、resolver 和 directive handler 也不得修改缓存于 `ParamPlan` 中的常量值。否则可能造成跨请求数据污染，并在并发执行时形成数据竞态。

### Engine 并发安全边界

满足上述只读和无状态约束时：

- 同一个 Engine 可以并发执行多个请求；
- Plan、BatchPlan 和 Registry 快照在运行期只读；
- 每个请求使用独立的 `Rundata`，请求参数和执行结果不会写入缓存 Plan；
- 并发安全不包含调用方传入后仍被其他 goroutine 修改的 `args`、`root`、`CONST` 内部对象或自定义回调状态。

## 字段必须配置 resolver 的两种情形

编排阶段（`plan_coordinator.go` 的 `appendBatches`）对以下两种情形直接返回错误，请求整体失败：

1. **根字段没有 resolver** —— `field %d has no resolver function`。graphql-go 原生允许根字段不配 resolver、由默认 resolver 从 `RootObject` 取值；sgraph 不支持。
2. **字段声明了参数但没有 resolver** —— `field %d has param plan but no resolver`（`bulkParamPlans` 对应 `field %d has bulk param plan but no bulk resolver`）。内省字段豁免这条检查，因为 `__Type.fields(includeDeprecated:)` 一类字段确实有参数但由结果生成逻辑读取。

原生链路允许"字段声明参数、不配 resolver"——默认 resolver 直接从父对象读同名属性并忽略参数。迁移时这类字段需要补上 resolver，或者去掉不再使用的参数声明。

### 属性值不得是函数（不支持惰性属性）

graphql-go 原生的 `DefaultResolveFn` 支持把 `map[string]any` 或结构体字段的值写成函数，取值时调用它拿返回值（惰性属性 / thunk）。**sgraph 不支持这种写法**：结果组装阶段处理的是"已经完成的属性值"，不执行任何函数形式的延迟属性。

命中函数值时，`extractFieldResponse`（`result_assembler.go` 的 `rejectDeferredFunctionProperty`）在全部 4 个取值出口拦截，该字段返回 `null` 并写入一条字段错误：

```
field <name> resolves to a function value; the result assembler does not evaluate
deferred properties, configure a resolver for this field instead
```

- 错误路径指向该字段本身；列表元素逐个判定，因此 N 个元素命中会产出 N 条错误，路径各自带 occurrence 下标。
- 字段处于 non-null 位置时按既有规则向上冒泡，兄弟字段的数据不受影响。
- map 值、命名 map 类型、结构体字段、签名不匹配的函数、typed-nil 函数值，全部按同一规则处理。

**依据**：规范 §6.4.2 把 `ResolveFieldValue` 如何取值留给实现，不执行惰性属性本身不违规；但 §6.4.3 `CoerceResult` 要求结果强制转换必须产出该类型的有效值，否则必须抛执行错误——因此不能把函数指针序列化成 `"0x…"` 交给 `String` 字段。

**迁移做法**：把惰性属性改成该字段的 resolver。若需要保留延迟求值的语义，求值必须发生在执行阶段（resolver 内），不能寄望于组装阶段。

## 父子结果关联 key 的推断契约

父字段是 list 时，`checkAndCompileParentKeyFieldNames`（`plan_compiler.go`）推断一个"父侧关联 key 字段名"，写入 `FieldPlan.parentKeyFieldName`。它有四个消费点：

| # | 位置 | 用途 | 值为空时 |
|---|---|---|---|
| 1 | `plan_compiler.go` bulk 分支 | bulk 硬性要求非空 | 编译期报错 |
| 2 | `plan.go bindIterationResponse` | 选择绑定模式 | 回退 responsePath 绑定 |
| 3 | `plan.go resolveIterationFieldResponseAttributeParam` | 跨分支读 bulk 生产者结果 | 运行期报错（由消费点 1 的编译期检查兜底，实际不可达） |
| 4 | `result_assembler.go extractFieldResponse` | 组装期查父绑定 | 返回 nil（同上，不可达） |

**注意消费点 2**：推断出非空值会把该 list 父类型下**全部**子字段的绑定方式从 occurrence 路径切成业务 key 绑定，普通逐元素 resolver 同样受影响，不是 bulk 专属。

### 推断规则

1. 父类型有名为 `id` 且基础类型是内置 `ID` scalar 的字段 → 取 `id`；
2. 否则收集全部基础类型为 `ID` 的字段作为候选，**恰好 1 个**才采用；
3. 候选为 0 个或 ≥2 个 → 返回空字段名，并回传排序后的候选列表供调用方拼装错误信息。

### 维护约束：候选 ≥2 时不得任选一个

这条曾经是一个非确定性缺陷。`Fields()` 返回 `map[string]*FieldDefinition`，Go 的 map 遍历顺序未指定且被运行时随机化；而推断在**编译期一次性完成并随 Plan 进入缓存**，因此表现为进程内稳定、跨进程启动才变化——测试环境很难复现，线上表现为"某次重启后整片接口报错，回滚重启又好了"。

**把兜底改成"排序后取首位"同样不可接受**：bulk 场景下父侧 key 的值必须与 `BulkResultMappedFieldName` 的值语义对齐，选错字段会让映射整体落空、每个父元素返回空列表且 `errors` 为 0。稳定地选错比随机选错更难发现。

因此维护时须保持：

- 候选不唯一时**返回空名**，由调用方决定报错还是回退绑定方式，不得引入任何"取首位""取最短名""按声明顺序"之类的启发式；
- 回传的候选列表只用于拼装错误文案，**不得写入 `FieldPlan`**——它不能进入可缓存的 Plan，也不能被多请求共享；
- 新增消费点时，须同时明确"值为空"的行为，不得默认非空。

### 尚未提供的能力

框架目前没有显式指定父侧 key 的配置入口：`FieldDefinition` 与公开的 `Field` 都没有对应字段。因此"`id` 名字被非 `ID` 类型字段占用、同时又声明了多个 `ID` 字段"的 schema 用不了 bulk，当前只能靠接入约定（list 元素类型显式声明 `id: ID!`）规避。

若将来新增配置入口，建议把配置项设计成 `[]string` 而不是 `string`：`generateCompositeKey(fieldNames []string, ...)` 内部已支持多字段组合 key，只是配置侧始终只传一个，改成切片可顺带打开复合业务主键能力。

## 参数依赖字段限制

`ParamPlan.dependentFieldId` 只允许指向满足以下条件的 `FieldPlan`：

- 拥有 resolver；
- 执行阶段会生成独立的 `Step`；
- resolver 执行完成后会在 `Rundata` 中写入独立的 `FieldResponse`。

不允许将 `dependentFieldId` 指向没有 resolver、仅在结果组装阶段从父对象中读取值的 `FieldPlan`。

### 原因

sgraph 根据参数来源计算 Step 之间的执行依赖，并据此将 Step 编排到不同 batch。运行期参数组装还会通过 `dependentFieldId` 从 `Rundata` 中读取依赖字段的 `FieldResponse`。

没有业务 resolver 的字段默认不会生成独立 Step，也不会在 `Rundata` 中产生独立 `FieldResponse`。当下游 Step 必须读取该字段的运行时结果时，编译器可以为其生成内部物化 Step；该 Step 只服务于引擎内部依赖，不会使字段成为 ParamRegistry 可引用的业务 producer。外部配置如果仍将该字段指定为 `dependentFieldId`：

- Plan 编译阶段会把它判定为没有可供外部配置使用的 resolver，并直接返回错误；
- 该外部依赖不会进入 Batch DAG，也不会进入请求执行阶段；
- 不能依赖查询选择集是否恰好触发内部物化，来改变外部参数配置是否合法。

### 正确配置

如果需要读取某个 resolver 返回对象中的嵌套字段，`dependentFieldId` 应指向实际执行 resolver 并产生 `FieldResponse` 的字段，`fieldResultPaths` 则描述从该字段结果到目标值的完整路径。

例如，field 1 的 resolver 返回：

```go
map[string]any{
	"profile": map[string]any{
		"id": "P1",
	},
}
```

参数需要读取 `profile.id` 时，应配置为：

```go
dependentFieldId = 1
fieldResultPaths = []string{"profile", "id"}
```

即使 `profile` 或 `id` 在 GraphQL FieldPlan 树中有各自的 fieldId，只要它们没有 resolver，就不能将这些 fieldId 配置为 `dependentFieldId`。

### 维护要求

- 外部依赖解析器必须拒绝指向无 resolver FieldPlan 的 `dependentFieldId`。
- Plan 构建或依赖校验逻辑应在执行前发现此类非法配置，不能依赖运行期报错。
- 不得仅因字段生成了内部物化 Step，就允许 ParamRegistry 将它配置为外部 `dependentFieldId`；内部物化与外部参数依赖是两个独立能力边界。

## Mutation 执行链路限制

sgraph 新执行引擎当前严格按照参数依赖关系构建 Step 依赖图和 batch，不为 mutation 根字段额外增加基于字段层级或根字段完成顺序的执行约束。

由于该执行模型当前不保证一个 mutation 根字段及其全部子字段完整执行后再执行下一个 mutation 根字段，因此 mutation 请求暂时不允许进入 sgraph 新执行链路。

当前使用要求如下：

- query 请求可以进入 sgraph 新执行链路；
- mutation 请求必须使用原有 graphql-go Execute 链路；
- sgraph 的 batch 编排逻辑继续只由参数依赖关系决定；
- 不允许为了支持 mutation 串行完成而在 sgraph 参数依赖图中隐式增加父子依赖或根字段顺序依赖。

后续如果需要让 mutation 使用 sgraph 新执行链路，必须单独设计符合 GraphQL mutation 串行完成语义的 operation 级执行策略。在该方案确定前，不能直接将 mutation 请求切换到 sgraph。

## 无 resolver 字段的 Directive 限制

没有业务 resolver 的 `FieldPlan` 默认不会生成 `SingleCallStep` 或 `IterationCallStep`，字段值在结果组装阶段通过默认字段读取逻辑从父对象中取得。仅当下游 Step 必须读取该字段的运行时结果时，编译器才会生成内部物化 Step；内部物化不改变下面的 Directive 能力边界。

结果组装阶段支持标准条件指令：

- `@skip(if: ...)`；
- `@include(if: ...)`。

这两个指令会在组装字段前进行判断，参数可以使用字面量或本次请求变量。

结果组装阶段不会执行以下自定义 runtime directive 阶段：

- `DirectiveStageShouldExecute`；
- `DirectiveStageBeforeResolve`；
- `DirectiveStageAfterResolve`。

因此，即使无 resolver 的 `FieldPlan` 中存在 `directiveParamPlans`，结果组装阶段也不会物化或消费这些参数，自定义 directive handler 不会被调用。`directiveParamPlans` 的存在不代表该指令会在无 resolver 字段上生效。

使用和维护时必须遵守以下约束：

- `coordinateBatches` / `appendBatches` 不得仅因无 resolver 的 `FieldPlan` 存在 `directiveParamPlans` 而返回构建错误（当前实现只对 `paramPlans` 和 `bulkParamPlans` 报错，内省字段另有豁免）；
- 不得依赖自定义 runtime directive 改变无 resolver 字段是否执行、解析参数或最终字段值；
- 需要执行自定义 runtime directive 的字段必须配置 resolver，使其生成 Step 并进入完整的 directive 执行链路；
- 不得把标准 `@skip`、`@include` 与自定义 runtime directive 混为一谈，前者仍然支持无 resolver 字段；
- 后续如果需要补齐该能力，必须单独设计结果组装阶段的 directive 参数物化、错误处理及 `ShouldExecute`、`BeforeResolve`、`AfterResolve` 的明确语义。

## ParamRegistry 覆盖限制

ParamRegistry 用于在 Plan 编译阶段覆盖 Query AST 生成的参数来源，但不能突破 GraphQL 标准指令和内省字段的既有执行边界。

### `@skip` 和 `@include` 不支持字段结果来源

标准 `@skip`、`@include` 指令的 `if` 参数只允许使用以下来源：

- `CONST`：Query 字面量或外部配置提供的常量；
- `INPUT`：本次请求中已经完成 GraphQL 输入协变的变量。

不允许把 `@skip` 或 `@include` 的参数来源配置为 `FIELD_RESPONSE`，包括当前字段的父元素结果、祖先字段结果以及其他 resolver 的执行结果。

graphql-go 原生链路在字段收集阶段计算 `@skip` 和 `@include`，只读取字面量和请求变量；此时目标 resolver 及其依赖 resolver 不保证已经执行。允许读取 `FIELD_RESPONSE` 会引入非标准的执行期条件语义，并且在 list 父节点下还会产生“每个父元素分别判断”的额外执行模型。

Plan 编译阶段发现 `@skip` 或 `@include` 的外部参数配置使用 `FIELD_RESPONSE` 时，必须返回错误并停止构建，不能静默忽略、降级为 `nil` 或推迟到运行期处理。

### 内省字段不允许外部覆盖

ParamRegistry 不允许覆盖任何 GraphQL 内省字段的参数或以该内省字段为目标的 Directive 参数配置，包括但不限于：

- `__schema`；
- `__type(name: ...)`；
- `__typename`；
- 内省对象上的 `fields(includeDeprecated: ...)`、`enumValues(includeDeprecated: ...)` 等字段。

Plan 编译阶段只要发现外部配置的目标字段属于内省字段或内省类型，就必须返回错误并停止构建。不能按照普通 `FieldPlan` 参数覆盖逻辑处理，也不能通过 Directive 参数配置间接改变内省字段的执行行为。

该限制只约束 ParamRegistry 的外部覆盖能力，不改变 Query AST 中原有合法内省参数和标准 `@skip`、`@include` 的执行语义。

## 字段错误存储模型

sgraph 在 `Rundata` 中按 `fieldId` 保存字段错误：

```go
fieldErrors     []atomic.Pointer[FieldError]
fieldErrorCount atomic.Int32
```

每个 `fieldId` 对应一个槽位，槽位里挂的是一条**不可变单向链表**。`storeFieldError` 用 CAS 头插，因此并发 Step 同时向同一个字段写错误不会互相覆盖：

```go
fieldErrorSlot := &rundata.fieldErrors[fieldId]
for {
    currentHead := fieldErrorSlot.Load()
    fieldError.next = currentHead
    if fieldErrorSlot.CompareAndSwap(currentHead, fieldError) { break }
}
```

`getAllFieldErrors` 遍历时按 `fieldId` 升序输出，并把同一 `fieldId` 区间反转，恢复该字段内部的发生顺序。

### 同一字段的多条错误全部保留

以下场景会让同一个 `FieldPlan` 在一次请求中产生多个错误 occurrence，它们**都会出现在最终响应中**：

- 同一个字段在多个 list item 上执行，多个 item 分别报错；
- list 的多个元素在结果完成阶段分别发生抽象类型解析错误；
- 同一字段在执行阶段和结果组装阶段先后产生错误。

例如 `[[String!]]` 类型的字段返回 `[["a", nil], [nil, "d"]]` 时，两个非法 null 元素各产生一条错误，路径分别是 `["matrix", 0, 1]` 和 `["matrix", 1, 0]`。

`fieldErrorCount` 是 `storeFieldError` 的调用次数，用于给 `getAllFieldErrors` 预分配容量；它与最终错误数量一致，但不应作为响应语义依赖。

### 错误路径包含 list index

`FieldError` 使用以下字段保存响应路径：

```go
responsePath []any //包含responseName和List整数下标的请求级动态响应路径
```

元素只可能是 `string`（responseName / alias）或 `int`（list 下标），与 GraphQL 规范 §7.1.6 的 `path` 形状一致：

```go
[]any{"people", 1, "failForBob"}
```

路径由两条互补的链路产出，最终形状一致：

- 执行阶段：基于 `*ResponsePath` 链表逐级 `WithKey`，见 `addFieldErrorAtResponsePath`；
- 结果组装阶段：基于 `FieldPlan.paths` + `pathListDepths` + `Rundata.assemblyListIndexes` 物化，见 `materializeFieldPlanPath`。

因为两者形状一致，`hasFieldErrorAtPath` 的去重可以跨阶段生效，避免同一位置既记录执行错误又补一条通用的 Non-Null 错误。

### 使用要求

- 不要假设某个字段最多只有一条错误；
- 错误在响应中的整体顺序是"按 fieldId 升序"，不是按发生时间，客户端不应依赖跨字段的错误顺序；
- Bulk 绑定错误在命中父 occurrence 之前不写入 `Rundata`，而是暂存在 `bulkState.bindingErrors` 中，由结果组装阶段的 `reportBulkBindingErrors` 补齐完整 list 路径后写入；请求结束前 `flushPendingBulkBindingErrors` 会把未命中的残留错误按静态字段路径落库，因此错误不会丢失，但那部分错误的 path 不含 list 下标。

## 对象池与请求间隔离

`Rundata`、`FieldResponse`、`bulkFieldResponseState` 都走 `sync.Pool`。请求级引用必须在 `releaseRundata` / `releaseFieldResponse` 中清空，否则会跨请求泄漏 schema、plan、root、fragments、extensions 和上一次的字段结果。

### 池化对象不得把"新建"与"复用"的差异暴露到响应

`sync.Pool` 新建的对象里，切片字段是 nil；复用的对象里，同一字段是长度为 0 但**非 nil** 的切片。只要有任何一个读取点依赖切片的 nil 性，同一个请求就会因为池是否被预热而返回不同结果。

`FieldResponse.responseRaws` 会被 `extractFieldResponse` 对 list 字段直接返回给结果组装：

```go
if fieldPlan.fieldWrapperTypeInfo.isList {
    //没有父级绑定时，直接返回整个字段结果
    return fieldResult.responseRaws, nil
}
```

nil 切片会被 `isNilInterfaceValue` 判成 null，非 nil 空切片则补全为 `[]`。因此 `acquireFieldResponse` 与 `acquireBulkFieldResponseState` 必须保证取出的对象里这些切片非 nil：

```go
if frVal.responseRaws == nil {
    frVal.responseRaws = make([]any, 0)
} else {
    frVal.responseRaws = frVal.responseRaws[:0]
}
```

`bulkFieldResponseState.iterationResponses` 同理，且它的消费者用 `parentOccurrences == nil` 区分"走 occurrence 分支"还是"走 raws/paths 分支"，nil 性直接决定分支走向。

维护要求：

- 新增池化切片字段时，acquire 必须保证非 nil，不能只写 `x = x[:0]`；
- 新增读取点时，优先用 `len()` 判空，不要用 `== nil` 表达"没有结果"；确实需要区分"未产出"与"产出空集合"时，应当用显式的布尔或状态字段，而不是切片的 nil 性；
- `FieldResponse.responsePaths` 目前所有读取点都是 `len()` 驱动且不逃逸到响应，因此暂未表现出该问题；但它与 `responseRaws` 走的是同一套 acquire/release 逻辑，一旦新增依赖 nil 性的读取点就会复现，改动时需要一并处理。
