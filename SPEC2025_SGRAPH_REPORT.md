# 第二轮：sgraph 执行链路 GraphQL September 2025 规范测试报告

规范基准：https://spec.graphql.org/September2025/
执行链路：`Do → parse → validate → Execute → executeSGraph → SGraphEngine`
代码改动：`executor.go:32` 还原为 `return executeSGraph(p)`（1 行）。**测试文件与第一轮完全一致，未做任何改动。**

---

## 1. 两轮可比性说明

- 用例源码、schema、query、变量、断言与第一轮**逐字节相同**；唯一变量是 `executor.go` 那一行路由。
- 唯一的适配是 `parentRef` 参数注入：sgraph 给业务 resolver 传 `Source = nil`（`plan.go:977`），因此需要父对象的 resolver 用 `s25Parent(p)` 双读——原生读 `p.Source`，sgraph 读由 `ParamRegistry` 注入的 `parentRef` 参数。该参数在 schema 中可空、无默认值，原生链路的 `values.go:getArgumentValues`（`values.go:60-65`）不会把它写进 `p.Args`，因此对原生完全惰性；`FIELD_RESPONSE` 参数在 `plan_compiler_param_registry.go:180-186` 构造时不读 `argDef.Type`，父结果原样透传，不做任何协变。
- 注入只搬运父对象，不预计算结果、不跳过 resolver、不改变可空性 / 顺序 / 错误语义。
- 为防止注入掩盖真实差异，另有三条**不注入**的对照用例保留在语料中（默认 resolver 四分支、根字段无 resolver、无 registry 时的父结果传递）。

## 1.1 执行方式与顺序依赖

每个顶层 Test 单独启动一次 `go test`（与第一轮完全相同的 runner），这样任一用例的进程级崩溃不会吞掉后续结果，并且每个用例都在**干净的进程状态**下执行。

本轮（`acquireFieldResponse` / `acquireBulkFieldResponseState` 的池 nil 性修复之后）**逐用例独立进程与单进程全量运行的结果差异为 0 条**，不再存在顺序依赖。

## 2. 结果总览

| 章节 | 用例数 | 通过 | 失败 |
|---|---:|---:|---:|
| §2 Language | 130 | 110 | 20 |
| §3 Type System | 88 | 65 | 23 |
| §4 Introspection | 31 | 23 | 8 |
| §5 Validation | 43 | 28 | 15 |
| §6 Execution | 77 | 65 | 12 |
| §7 Response | 27 | 23 | 4 |
| 场景交叉 | 60 | 49 | 11 |
| 边界与极限 | 70 | 64 | 6 |
| **合计** | **526** | **427** | **99** |

通过率 81.2%（原生 78.0%）。

> 另有一条**用例矩阵未覆盖**的缺陷已在本轮之后发现并修复：父侧关联 key 在父类型声明多个 `ID` 字段时随机选取。本轮语料中没有这种类型，四象限计数不受影响，详见 §7。

## 3. sgraph 独有失败（原生通过 → sgraph 失败，共 14 条）

### 3.1 P0 — 根选择集被编译期全部裁剪时执行失败（11 条）

**现象**：`{ greeting @skip(if: true) }`、`{ ... on Query @include(if: false) { greeting } }`、`{ ...F @skip(if: true) }` 等所有"根字段被字面量条件指令静态排除"的查询返回：

```
no roots found for %!s(*ast.Name=<nil>)
```

**根因**：`SkipDirectiveCompiler.Compile`（`directive_registry.go:199`）在字面量为 `true` 时返回 `IncludeDecision = &false`，`flattenOneSelection` 在编译期直接删掉该 selection（`plan_compiler.go:697/711/733/749`）；根选择集清空后 `coordinateBatches` 在 `plan_coordinator.go:24` 判定 `len(roots) == 0` 并报错。

**规范要求**：§6.3.2 CollectFields 对被排除的字段不产出条目，§7.1.5 要求 `data` 是一个（可能为空的）map。正确结果是 `{"data":{}}`，不是 request error。

**附带**：错误文案 `no roots found for %s` 把 `operationDef.Name`（`*ast.Name`）用 `%s` 格式化，输出 `%!s(*ast.Name=<nil>)`。

受影响用例：`Cross_ConditionalDirectiveMatrix` 9 条，以及另外 2 条全裁剪场景；以 `SPEC2025_CASE_MATRIX.csv` 中 `SGRAPH_REGRESSED` 且错误包含 `no roots found` 的 11 条记录为准。

### 3.2 调用方约束（非缺陷）— 自定义指令必须先注册到 DirectiveRegistry

早前一版把这条记成了 P0 缺陷，**判断有误，已更正**。

`SGRAPH_USAGE_NOTES.md` 的 Engine 生命周期第 2 步已明确要求：

> 通过 `NewDirectiveRegistry()` 创建包含 `@skip`、`@include` 的默认指令注册表，**再完成自定义指令和 `ParamRegistry` 的全部注册**

也就是说，SGraph 的设计前提就是"文档中出现的每个指令都要在 `DirectiveRegistry` 里有归属"——有执行语义的注册 compiler / runtime handler（写法与 `SkipDirectiveCompiler` / `IncludeDirectiveRuntimeHandler` 相同），纯标注型的用 `RegisterMetadataOnly`。`plan_compiler.go` 的 `no directive compiler found for %s` 是这条契约的强制点，不是遗漏。

问题出在本测试语料：`s25NewIntrospectSchema` 在 `SchemaConfig.Directives` 里声明了 `@s25Tag`，却没有登记到 `DirectiveRegistry`。已在 `s25Request` 上新增 `MetadataDirectives` 字段，按契约在建 Engine 前调用 `RegisterMetadataOnly`；该字段对原生链路完全惰性（原生不读 `DirectiveRegistry`）。修正后 5 条用例全部通过。

仍需注意的**迁移成本**（不是缺陷，是接入约束）：从 graphql-go 迁过来时，原本"schema 声明了但执行期被忽略"的标注型指令，必须逐个补 `RegisterMetadataOnly`，漏一个则用到它的请求整体失败。建议在应用启动阶段用 `schema.Directives()` 对账一遍 registry，把失败前移到启动期。

### 3.3 空列表在冷池下被补全为 null（**已修复**，本轮不再复现）

上一轮发现：`[]any{}` 的列表字段在进程内第一次列表请求返回 `null`，预热后同一查询返回 `[]`。

根因是 `acquireFieldResponse()` 从 `sync.Pool` 取到**新建**对象时 `responseRaws` 是 nil slice、取到**复用**对象时是 `[:0]` 的非 nil 空 slice；`extractFieldResponse` 对 list 字段直接把它返回给结果组装（`result_assembler.go` 的"没有父级绑定时，直接返回整个字段结果"分支），nil 性决定了是 `null` 还是 `[]`。

**当前代码已在池的出口统一消除 nil 性**：

```go
if frVal.responseRaws == nil {
    frVal.responseRaws = make([]any, 0)
} else {
    frVal.responseRaws = frVal.responseRaws[:0]
}
```

`acquireBulkFieldResponseState` 对 `iterationResponses` 做了同样处理——这一条是必要的配套修复，因为它的消费者用 `parentOccurrences == nil` 区分走 occurrence 分支还是 raws/paths 分支，nil 性直接决定分支走向。

实测确认：

```
COLD  { empty } -> map[string]any{"empty": []any{}}
WARM  { full }  -> map[string]any{"full": ["a"]}
AFTER { empty } -> map[string]any{"empty": []any{}}
```

`Boundary_ListSizes/size_0` 与 `Execution_ValueCompletionList/collections_of_every_shape_complete_element_wise` 两条用例现已通过，逐用例独立进程与单进程全量运行的结果差异降为 0。

**遗留注意**：`FieldResponse.responsePaths` 仍是 `frVal.responsePaths[:0]`，没有做同样处理。经逐点核查，它的全部读取点（checkpoint / rollback / append / `iterationResponseData` 的对齐检查）都是 `len()` 驱动，并且**从不逃逸到响应**，因此当前不可观测——已用 IterationCallStep 的冷/热对比实测确认结果一致。但它与 `responseRaws` 共用同一套 acquire/release 逻辑，一旦新增依赖 nil 性的读取点就会复现同类问题。

### 3.4 结果组装不执行 map 中的函数属性（2 条，**静默错误已修复**，行为差异保留）

**原始现象**：无 resolver 的字段，父结果是 `map[string]any{"thunk": func() any { return "thunkValue" }}`，sgraph 把函数指针交给叶子序列化，输出字符串 `"0x68cea0"` 且**不产生任何错误**；原生调用该函数，输出 `"thunkValue"`。

**规范定位**：§6.4.2 Value Resolution 把 `ResolveFieldValue` 如何从 `objectValue` 取值留给实现自行决定，因此"不执行函数形式的延迟属性"本身**不违反规范**。违规的是后半段——§6.4.3 Value Completion 的 `CoerceResult` 要求结果强制转换必须为该类型产出有效值，否则必须抛执行错误；把函数指针格式化成 `"0x68cea0"` 当作 `String` 返回，既不是有效值也没有错误。

**已实施的修复（方案 A）**：`result_assembler.go` 新增 `rejectDeferredFunctionProperty`，在 `extractFieldResponse` 的 4 个取值出口（map 快路径无 extensions / map 快路径带 extensions / reflect 字符串键 map 路径 / `DefaultResolveFn` 兜底路径）用 `reflect.TypeOf(v).Kind() == reflect.Func` 判定，命中即写字段错误并让该字段返回 null：

```
{"data":{"fromMap":{"plain":"plainValue","thunk":null}},
 "errors":[{"message":"field thunk resolves to a function value; the result assembler does not evaluate deferred properties, configure a resolver for this field instead",
            "locations":[{"line":1,"column":19}],"path":["fromMap","thunk"]}]}
```

修复后的行为要点：

- 命中函数值的字段返回 null，**兄弟字段与其余数据不受影响**；字段处于 non-null 位置时按既有规则向上冒泡，只产生一条错误。
- 列表元素逐个判定，5 元素列表产出 5 条错误，路径分别是 `[items 0 thunk]` … `[items 4 thunk]`（与 sgraph 的 occurrence 路径模型一致）。
- 覆盖全部函数形态：map 值、命名 map 类型、结构体字段、签名不匹配的函数、typed-nil 函数值。
- 正常值与"属性不存在"两种情况的行为完全不变；`-race` 无新增竞争。

**仍然记为差异的原因**：修复只把"静默数据错误"变成"显式字段错误"，并没有让 sgraph 支持惰性属性。上述 2 条用例断言的是"函数属性被调用、得到 `thunkValue`"，因此**仍然失败**，只是失败点从值不符变成了 `unexpected errors`。sgraph 独有失败仍为 14 条，四象限计数不变。

**迁移影响**：依赖 graphql-go `DefaultResolveFn` 惰性属性的既有 schema，迁移到 sgraph 后不再拿到指针字符串，而是拿到 null + 一条指名字段的错误。适配方式是给该字段配置 resolver。

受影响用例：`Cross_DefaultResolverBranches`、`Execution_ValueResolution/default_resolution_branches`。

### 3.5 能力边界 — subscription 不支持（1 条）

`executor.go:51` 直接返回 `subscription is not supported yet`。这是 `SGRAPH_USAGE_NOTES.md` 明确记录的当前能力边界，不是回归。

### 3.6 已确认的其他调用方约束

以下两条在本轮排查中确认，均属接入约束而非缺陷，但**当前文档未覆盖**，已补入 `SGRAPH_USAGE_NOTES.md`：

**（1）带参数的字段必须配置 resolver。**
`plan_coordinator.go` 对"无 Step 却存在 `paramPlans`"的字段报 `field %d has param plan but no resolver`（内省字段豁免）。原生链路允许字段声明参数却不配 resolver——默认 resolver 直接从父对象取属性、忽略参数。本轮语料里 `S25IntroItem.withArgs` 就踩了这条，已补 resolver。

**（2）第一条请求必须晚于 `RegisterSGraphEngine`。**
Engine 与 Schema 的绑定按设计是一次性、进程内不可变的，Plan 缓存与两个 Registry 快照因此在运行期无需任何同步或失效逻辑。未注册时 `getSGraphEngineForSchema` 会创建一个只含 `@skip`/`@include` 的默认 Engine 并写入全局缓存；之后再调用 `RegisterSGraphEngine` 返回 `another sgraph engine is already registered for this schema`，框架不提供解绑接口。实测：

```
注册前的请求: errors=[] data=map[greeting:hello]
随后 RegisterSGraphEngine -> another sgraph engine is already registered for this schema
```

原文档只写了"在任何请求执行前调用 `RegisterSGraphEngine`"，没有展开这条顺序要求的推导和后果。已在 `SGRAPH_USAGE_NOTES.md` 中补成完整的设计说明：绑定一次性 → 启动顺序 → 接入检查清单（路由注册、健康检查、预热请求都算"请求"；`RegisterSGraphEngine` 返回错误应视为启动失败）。

## 4. sgraph 相对原生的改善（完整双链路历史快照为 31 条）

> 该 31 条来自原生字段并发执行时期的完整对比快照。当前原生 `executeSubFields` 已串行化，下面第 4 类的 3 条 non-null 进程崩溃差异已消失；完整总数需在当前两条链路重新跑完 526 用例后更新。

按根因归为 5 类：

1. **叶子结果强制转换失败会产生 field error**（8 条）—— `result_assembler.go:774 serializeLeafValue` 在序列化结果为 nullish 时写入 `cannot serialize leaf value for <field>:<Type>`，而原生 `completeLeafValue` 静默返回 nil。
2. **响应字段严格保序**（9 条）—— `SGraphResponseOrderedMap`（`rundata.go:635`）+ 自定义 `MarshalJSON`（`rundata.go:719`），满足 §7.1.4 / §7.2.2。
3. **显式 null 与"未提供"可区分**（6 条）—— `completeOperationVariables`（`sgraph_engine.go:230-238`）显式区分 `provided` 与 `value == nil`。
4. **non-null 冒泡不会 panic 出进程**（历史快照 3 条）—— SGraph 的 recover 仍有效；当前原生字段串行执行后也会正常返回字段错误，本项不再是当前差异。
5. **SGraph 并发链路无数据竞争**（race 检查）—— sgraph 把 ctx 按值参数传入、按返回值传出（`extensions.go:245/263`），从不写共享结构；`-race` 下 128 并发请求 + 并发根字段全部通过。当前原生改为字段串行后定向 `-race` 也通过。

## 5. SGraph 专项结果（不进对比基线，7 个 Test 全部通过）

| 项 | 结论 |
|---|---|
| **查询折叠生效** | 无依赖声明时，`root → next → value` 三级链的三个 resolver step 全部折叠进 **batch 0** |
| **同 batch 真并发** | 父子 step 互相等待的用例通过（3s 超时），证明同 batch 内**真正并发执行**，不是串行 |
| **并发阈值** | `plan.go:388` 的条件是 `b.concurrent && len(b.steps) > 1`，**不存在**旧报告所述的 `sGraphConcurrentStepMin = 8` 阈值；"小 batch 被强制串行"在当前代码中已不复存在 |
| **依赖链编排** | 全链声明依赖 → 3 个批次，每批 1 个 step；混合依赖 → 2 个批次（2+1） |
| **batch 并发标记** | query 的每个 batch 的 `concurrent` 均为 true |
| **参数物化** | CONST 覆盖、query 字面量、FIELD_RESPONSE 深路径取值三种来源均正确 |
| **非法依赖图** | 自依赖 / 环 / 未知源均在编译期被拒，错误文案分别含 `cannot depend on itself` / `cycle` / `unknown FIELD_RESPONSE source` |
| **Plan cache 隔离** | 同一 plan 上交替执行 `a/b/a/c` 四组变量，结果互不污染 |
| **并发请求隔离** | 单 Engine 上 64 并发请求，`-race` 通过，无变量串扰 |
| **对象池清理** | `releaseRundata` 后字段错误、请求入参、schema / plan / operation 引用、FieldResponse 槽位全部清空 |

## 6. 与既有文档的偏差（需要更新文档）

| 文档 | 记载 | 实测 |
|---|---|---|
| `SGRAPH_USAGE_NOTES.md`「字段错误存储限制」 | 同一 fieldId 的多条错误会被后写覆盖 | **不符**。当前 `rundata.go:170-185` 使用 CAS 单向链表，同一字段的多条错误全部保留；`matrixWithNull` 的两个元素错误都被正确返回 |
| `SGRAPH_USAGE_NOTES.md`「错误路径不能表示 list index」 | `fieldPath []string` 无法表达下标 | **不符**。当前 `FieldError.responsePath` 是 `[]any`，`materializeFieldPlanPath`（`rundata.go:238`）产出 `["people", 1, "failForBob"]` 形式，下标正确 |
| `SGRAPH_COMPATIBILITY_TEST_REPORT.md`「小 batch 被强制串行，阈值 8」 | `len(b.steps) >= sGraphConcurrentStepMin` | **不符**。该常量在仓库中不存在；当前条件是 `len(b.steps) > 1` |
| `CONFORMANCE_MATRIX.md` | 依赖 `SGRAPH_SPEC_CONFORMANCE=1` 环境变量、列举 `TestSGraphSpec_` 等测试 | **不符**。该环境变量无任何代码读取，列举的测试名全部不存在 |
| `SGRAPH_COMPATIBILITY_TEST_REPORT.md`「request error 暴露 typed-nil data」 | `Result.Data` 是 `(*SGraphResponseOrderedMap)(nil)` | **已修复**。`rundata.go:783-785` 现在显式判空后才赋值，本轮 request error 用例的 `result.Data == nil` 全部通过 |
| `SGRAPH_COMPATIBILITY_TEST_REPORT.md`「抽象类型列表 `__typename` 绑定失败」 | `parent key field name is empty for field __typename` | **已修复**。interface / union 列表的 `__typename` 与内联片段分发全部通过 |
| `SGRAPH_COMPATIBILITY_TEST_REPORT.md`「typed pointer list 不被识别」 | `*[]string` 报 `list value is not a list` | **已修复**。`pointerList` 用例通过 |
| `SGRAPH_COMPATIBILITY_TEST_REPORT.md`「resolver error 的 extensions 丢失」 | `extensions.code` 为空 | **已修复**。`boomExtended` 用例的 `extensions.code == "S25_CODE"` 通过 |
| `SGRAPH_COMPATIBILITY_TEST_REPORT.md`「execution error 缺少 location」 | `Locations` 为空 | **已修复**。`rundata.go:161-167` 在错误发布前补齐 AST 位置，本轮 locations 断言通过 |

## 7. 用例矩阵之外发现并修复的缺陷：父侧关联 key 在多 ID 字段时随机选取

**这条不在 526 用例矩阵内，四象限计数不受影响**——本轮语料中没有任何 list 元素类型声明了 ≥2 个 `ID` 字段（静态扫描 200 个类型定义块 + 动态扫描 7 个语料 schema 的 `TypeMap()`，命中数均为 0），因此矩阵从未触发该分支。它是在复核用户手册第 9 章 bulk 配置时，顺着"父类型有多个 `ID` 字段会怎样"这个问题查出来的。

### 7.1 缺陷

`checkAndCompileParentKeyFieldNames` 推断父子结果关联 key，原实现的兜底分支是"遍历父类型全部字段，取**第一个**基础类型为 `ID` 的字段"。`Fields()` 返回 `map[string]*FieldDefinition`，Go 的 map 迭代顺序未指定且被运行时随机化，因此父类型有 ≥2 个 `ID` 字段又没有 `id` 时，选中哪个字段是随机的。

GraphQL 规范对一个对象能声明几个 `ID` 字段没有限制（§3.6 的 Type Validation 只要求字段名在类型内唯一），外键、多租户、Relay `Node.id` + 业务 id 都是常见形态，因此这是合法 schema 上的非确定性行为。

**时间粒度是这条缺陷最难查的地方**：推断发生在 Plan 编译期，结果随 Plan 进入缓存。实测同一 Engine 连续 60 次执行结果完全一致，而每轮重建 schema + Engine（等价于进程重启）时才会变化。线上表现为"测试环境反复跑都正常，某次发布重启后整片接口报错，回滚重启又好了"。

### 7.2 实测数据（修复前）

同一 scope 调用推断函数 200 次，4 个候选字段上的分布：

```
alphaId:127   betaId:29   gammaId:25   deltaId:19
```

给父类型补上 `id: ID!` 后：`id:200`，完全稳定。

每轮重建 schema + Engine 各跑 60 次的两组端到端实测：

```
父 resolver 只返回 alphaId、未返回 betaId：
   53 次 OK
    7 次 parent key field "betaId" is missing for field label

betaId 在两个父元素上重复、alphaId 不重复：
   51 次 OK
    9 次 duplicate parent binding key "shared" for field label
```

### 7.3 影响面比"bulk 专属"更大

`parentKeyFieldName` 一旦推断出非空值，`bindIterationResponse` 会把该 list 父类型下**所有子字段**的绑定方式从 occurrence 路径切换成业务 key 绑定，普通逐元素 resolver 同样受影响。四个消费点：

| # | 位置 | 用途 | 值为空时 |
|---|---|---|---|
| 1 | `plan_compiler.go` bulk 分支 | bulk 硬性要求非空 | 编译期报错 |
| 2 | `plan.go bindIterationResponse` | 选择绑定模式 | 回退 responsePath 绑定 |
| 3 | `plan.go resolveIterationFieldResponseAttributeParam` | 跨分支读 bulk 生产者结果 | 运行期报错 |
| 4 | `result_assembler.go extractFieldResponse` | 组装期查父绑定 | 返回 nil（实际不可达，由消费点 1 的编译期检查兜底） |

**bulk 场景选错字段的后果是静默数据丢失**：父侧 key 的值要与 `BulkResultMappedFieldName` 的值语义对齐，选错会让映射整体落空。实测父 key 推断为 `betaId`、bulk 结果按 `alphaId` 的值回填时：

```json
{"data":{"groups":[{"alphaId":"A1","betaId":"B1","users":[]},
                   {"alphaId":"A2","betaId":"B2","users":[]}]}}
errors 数量 = 0
```

因此"把兜底改成排序后取首位"并不足够——它只是把随机选错变成稳定选错，静默数据丢失依然存在。

### 7.4 修复

`plan_compiler.go`（1 个文件，2 个函数，2 处调用点）：

- `checkAndCompileParentKeyFieldNames` 签名改为 `(string, []string)`。R1（名为 `id` 且基础类型是内置 `ID` scalar 的字段）分支逐字保留原实现；兜底分支改为收集全部 `ID` 候选，**恰好 1 个才采用**，0 个或 ≥2 个返回空名并回传排序后的候选列表。
- 业务字段调用点：bulk 分支在候选 >1 时报歧义错误并列出候选字段名。
- `__typename` 调用点：只跟签名，丢弃候选列表，行为不变（`bindIterationResponse` 对 `__typename` 提前返回，该取值不参与绑定）。

返回的候选列表只用于拼装错误文案，不写入 `FieldPlan`，因此不进入可缓存的 Plan，也不被多请求共享。

新的错误文案：

```
parent key field name for bulk resolver users result binding is ambiguous:
parent type SPKBulkGroup declares multiple ID fields [alphaId betaId],
declare an id: ID! field on the parent type to disambiguate
```

### 7.5 修复后的行为矩阵

| 父类型形态 | 修复前 | 修复后 |
|---|---|---|
| 有 `id: ID!` | 取 `id` | 不变 |
| 恰好 1 个 `ID` 字段 | 取它 | 不变 |
| ≥2 个 `ID`、无 `id`、**普通迭代** | 随机取一个，可能报字段缺失 / key 重复 | 回退 responsePath 绑定，逐元素映射仍正确 |
| ≥2 个 `ID`、无 `id`、**bulk** | 随机取一个，可能静默返回空列表 | 编译期报错并列出候选 |
| 0 个 `ID` 字段 | `""` | 不变 |

### 7.6 验证

**用例有牙**：把实现临时还原成"兜底任选"后跑新增用例，9 个顶层 Test 全部失败，其中确定性用例在同一进程内抓到 `gammaId vs alphaId`，迭代用例抓到 `parent key field "betaId" is missing for field label`。还原修复后全绿。

**零回归**：全量测试套件改动前后逐用例对照——

```
改动前用例数: 922   改动后用例数: 937
新增用例: 15        消失用例: 0
状态发生变化的既有用例: 0
```

历史分链路核对：spec2025 / sgraph 链路 526 叶子用例 427 通过 / 99 失败，逐用例对照 `SPEC2025_CASE_MATRIX.csv` 后 **0 条状态不同**；当时临时把 `executor.go` 路由翻到原生链路复跑，526 叶子用例 410 通过 / 116 失败，同样 **0 条不同**。四象限 396 / 85 / 31 / 14 属于该轮快照。此后原生 `executeSubFields` 已串行化，当前原生总数需重新完整跑数，不能继续引用该四象限作为当前结果。

**并发**：新增用例含 32 goroutine × 16 轮的并发编译一致性验证，`-race` 通过无 DATA RACE。

### 7.7 配套用例

新增 `sgraph_parent_key_test.go`（9 个顶层 Test）：推断规则的六种父类型形态、500 次重复推断的确定性、bulk 歧义的编译期报错与文案稳定性、补 `id: ID!` 后恢复正常、普通迭代回退 responsePath 的行为固化（含父 key 值重复的容忍性）、消费点 3 的可达性验证、并发编译一致性。

### 7.8 遗留决策

框架目前**没有显式指定父侧 key 的配置入口**：`FieldDefinition` 和公开的 `Field` 都没有对应字段，`parentKeyFieldName` 只能靠推断。因此"`id` 名字被非 `ID` 类型字段占用、同时又有多个 `ID` 字段"的 schema 在修复后用不了 bulk。当前采取的是约定方案（list 元素类型显式声明 `id: ID!`，新错误文案直接指向该做法）；是否新增 `ObjectConfig.KeyFieldName` 或 `FieldDefinition.ParentKeyFieldName` 尚未决定，待出现真实受阻 schema 后再评估。

> 附带观察：`generateCompositeKey(fieldNames []string, ...)` 内部已支持多字段组合 key，只是配置侧始终只传一个。若将来新增配置入口，把配置项设计成 `[]string` 可顺带打开复合业务主键能力，边际成本接近零。
