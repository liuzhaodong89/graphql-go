# graphql-go 原生链路 vs sgraph 链路：September 2025 规范对比报告

规范基准：https://spec.graphql.org/September2025/
两轮唯一差异：`executor.go:32` 的一行路由（`ExecuteGraphQLGo` ↔ `executeSGraph`）。
测试源码、schema、query、变量、断言两轮**完全一致，未做任何调整**。

逐用例明细见 `SPEC2025_CASE_MATRIX.csv`（526 行）。

> **快照边界**：本报告的 410/427、四象限 396/85/31/14 来自原生字段并发执行时期。当前原生 `executeSubFields` 已串行化；定向复核确认原生 non-null 冒泡不再终止进程，旧版共享 `executionContext` 竞态也不再触发。串行化同时可能改变根字段调度，因此在完整双链路重跑前，本报告保留历史数字，不把局部变化拼成新的总数。

---

## 0. 一句话结论

在该轮完整快照中，sgraph 修复了原生链路 **31 条**规范违规（其中 3 条当时是进程崩溃级），同时引入 **13 条**新的不兼容或缺陷（其中 1 类是 P0 阻断），另有 **85 条**两条链路共有的规范缺口。当前原生串行化后，3 条 non-null 差异已消失；其余总数需以新的完整双链路跑数为准。

> 本版包含两处更正：
> 1. 上一版的 P0-3「空列表在冷池下被补全为 null」已由 `acquireFieldResponse` / `acquireBulkFieldResponseState` 保证切片非 nil 修复，相关 2 条用例转为通过，顺序依赖消除。
> 2. 上一版的 P0-2「自定义指令阻断执行」**判断有误**：SGraph 的设计契约就是自定义指令必须先注册到 `DirectiveRegistry`（`SGRAPH_USAGE_NOTES.md` 生命周期第 2 步已写明），是本测试语料漏了这一步。已按契约补上 `RegisterMetadataOnly`，5 条用例转为通过，该条从"新框架引入的问题"移入"设计边界 / 接入约束"。
>
> 另有两处**代码变更**，两者都**不改变任何用例的通过/失败状态**，上表全部计数在修复前后完全一致，并已复跑双轮与 `-race` 确认：
>
> - P1-1「函数属性输出指针字符串且无错误」已按方案 A 修复（`result_assembler.go` 在组装阶段拦截函数值并写 field error）。这条消除的是 §6.4.3 的一致性违规；相关 2 条用例断言的是"函数被调用"，仍然失败。
> - **用例矩阵之外**发现并修复了一条非确定性缺陷：父侧关联 key 在父类型声明多个 `ID` 字段时随机选取（`plan_compiler.go` 的 `checkAndCompileParentKeyFieldNames`）。本轮语料中没有任何 list 元素类型声明 ≥2 个 `ID` 字段，因此 526 用例从未触发该分支，四象限计数不受影响。详见 `SPEC2025_SGRAPH_REPORT.md` §7。

## 1. 总体数据

| | 原生 | sgraph |
|---|---:|---:|
| 叶子用例总数 | 526 | 526 |
| 通过 | 410 | 427 |
| 失败 | 116 | 99 |
| 通过率 | 78.0% | 81.2% |
| `-race` 并发检查 | 历史快照 FAIL；当前串行实现定向复核 PASS | PASS |
| 进程崩溃用例 | 4 | 0 |

四象限分布：

| 象限 | 数量 | 含义 |
|---|---:|---|
| 两轮均通过 | 396 | 兼容且合规 |
| 两轮均失败 | 85 | **① 原框架的问题（sgraph 未修复）** |
| 原生失败 → sgraph 通过 | 31 | **③ 原框架有、新框架修复** |
| 原生通过 → sgraph 失败 | 14 | **④ 新框架引入的问题 + 能力边界** |

**② 新旧框架不兼容**的总集合是后两类之和 **45 条**（行为在两条链路上不同）。

分章节：

| 章节 | 用例数 | 原生通过 | 原生失败 | sgraph通过 | sgraph失败 |
|---|---:|---:|---:|---:|---:|
| §2 Language | 130 | 109 | 21 | 110 | 20 |
| §3 Type System | 88 | 60 | 28 | 65 | 23 |
| §4 Introspection | 31 | 23 | 8 | 23 | 8 |
| §5 Validation | 43 | 28 | 15 | 28 | 15 |
| §6 Execution | 77 | 55 | 22 | 65 | 12 |
| §7 Response | 27 | 15 | 12 | 23 | 4 |
| 场景交叉 | 60 | 58 | 2 | 49 | 11 |
| 边界与极限 | 70 | 62 | 8 | 64 | 6 |
| **合计** | **526** | **410** | **116** | **427** | **99** |

---

## 2. ① 原框架的问题（85 条两条链路共有）

这些缺口 sgraph 没有修复，因为它们位于**共用的 parse / validate 阶段**，或属于当前 schema 模型无法表达的能力。

### 2.1 位于共用 parse 阶段（19 条）

| 缺口 | 规范 | 影响 |
|---|---|---|
| **`null` 字面量不被解析** | §2.10.5 | `Syntax Error ... Unexpected Name "null"`。合法的 NullValue 完全无法书写，连带 §6.4.1「显式 null 参数不使用默认值」无法测试 |
| **可执行定义 description 不支持**（Sept 2025 新增） | §2.2 / §2.4 / §2.9 / §2.11 | operation / fragment / variable definition 前的 `"..."` 与 `"""..."""` 全部解析失败 |
| **VARIABLE_DEFINITION 指令位置不支持** | §2.13 | `query Q($a: Int @tag)` 解析失败；`directives.go` 也无该常量 |
| **`\u{...}` 花括号转义不支持** | §2.10.4 | `Invalid character escape sequence` |
| **代理对未合成** | §2.10.4 | `😀` 解码成两个替换字符而非 U+1F600 |
| **IntValue 后紧跟 NameStart 未报错** | §2.10.1 | `[1abc]` 被接受 |

### 2.2 位于共用 validate 阶段（15 条）

| 规则 | 表现 |
|---|---|
| 5.1.1 Executable Definitions | 文档中混入 `type Foo { a: Int }` 被接受 |
| 5.2.1.1 Operation Type Existence | query-only schema 上 `mutation M { ... }` 通过校验 |
| 5.2.4.1 Single Root Field | subscription 多根字段 / 别名重复根字段 / `__typename` 唯一根字段全部通过 |
| 5.5.2.2 No Fragment Cycles | **进程栈溢出**，而非返回校验错误 |
| 5.6.3 Input Object Field Uniqueness | `{text:"a", text:"b"}` 通过。根因是 `rules.go:1495` 把 `ObjectField` handler 注册在 `Kind:` 键而非 `Enter:`，规则从未触发 |
| 5.7.3 Directives Are Unique per Location | 同一字段两个 `@skip` 通过校验 |
| 非空变量带默认值被错误拒绝 | `$text: String! = "hi"` 被拒，错误文案是多年前已从规范移除的规则 |

### 2.3 输入强制转换过宽（13 条，共用 `scalars.go`）

`Int!` 接受 `true` / `"5"` / `1.5`；`Float` 接受 `"1.5"`；`Boolean` 接受 `0` / `1`；`String` 接受 `3`；`ID` 接受 `true`。全部应为 request error。

### 2.4 String / Boolean / Float 结果强制转换过宽（8 条）

`coerceString` 用 `fmt.Sprintf("%v")` 兜底，任何值都能变成字符串；`coerceBool` 未知类型返回 `false` 而非 nil；`Float` 的 `NaN` / `±Inf` / 字符串被原样或静默接受。两条链路都不报错 —— **注意这与 §3.1 中 sgraph 修复的 Int/Enum/自定义 Scalar 情形不同**：sgraph 的 `serializeLeafValue` 只在 `Serialize` 返回 nullish 时报错，而这三类根本不会返回 nullish。

### 2.5 request error 响应仍带 `data` 键（4 条）

`types.go:11` 的 `Data interface{}` 带 `json:"data"` 而无 `omitempty`，四种 request error 的序列化结果都含 `"data": null`。规范 §7.1.3 明确要求不得出现 data 条目。

### 2.6 September 2025 内省与类型系统能力缺失（26 条）

当前 schema 模型无法表达，内省查询在校验期即失败：

- `__Schema.description`（`SchemaConfig` 无 `Description` 字段）
- `__Type.specifiedByURL`（`ScalarConfig` 无 `SpecifiedByURL`）
- `__Type.isOneOf` / OneOf 输入对象（`InputObjectConfig` 无 `IsOneOf`），4 条 oneOf 语义用例全灭
- `__Type.inputFields(includeDeprecated:)`、`__Field.args(includeDeprecated:)`、`__Directive.args(includeDeprecated:)`
- `__InputValue.isDeprecated` / `deprecationReason`（`Argument` 与 `InputObjectFieldConfig` 均无 `DeprecationReason`）
- `__Directive.isRepeatable`（`Directive` 结构体无该字段）
- INTERFACE 上的 `__Type.interfaces` 返回 null（interface 实现 interface 不可内省）
- 内置 `@specifiedBy` / `@oneOf` 完全不存在；`@deprecated` 缺少 `ARGUMENT_DEFINITION` 与 `INPUT_FIELD_DEFINITION` 位置

---

## 3. ③ 原框架有、新框架已修复（历史快照 31 条）

| 类别 | 条数 | 规范 | sgraph 做对了什么 |
|---|---:|---|---|
| **叶子结果强制转换失败产生 field error** | 8 | §6.4.3 | `result_assembler.go:774 serializeLeafValue` 在序列化结果为 nullish 时写入 `cannot serialize leaf value for <field>:<Type>` 并置 null；原生 `executor.go:971 completeLeafValue` 静默返回 nil。覆盖：Int 越界、未知 Enum 内部值、自定义 Scalar 返回 nil |
| **响应字段严格保序** | 9 | §7.1.4 / §7.2.2 | `SGraphResponseOrderedMap`（`rundata.go:635`）+ 自定义 `MarshalJSON`（`rundata.go:719`）。根字段、嵌套字段、list 元素内字段、别名、片段展开后的首次出现位置全部正确；原生因 `map[string]interface{}` 全部丢序 |
| **显式 null 与"未提供"可区分** | 6 | §6.1.2 / §6.4.1 | `completeOperationVariables`（`sgraph_engine.go:230-238`）显式区分 `provided` 与 `value == nil`，`parseInputValue`（`:347-348`）对 input object 字段同样保留显式 null；原生 `values.go:60` 的 `isNullish` 判断把两者混为一谈 |
| **non-null 冒泡不会终止进程** | 3 | §6.4.3 | **仅适用于历史快照**。当前原生字段串行执行后同样把 panic 沿调用栈转成字段错误，不再构成 SGraph 相对改善 |
| **抽象类型列表 / typed pointer list / error extensions / error locations** | 5 | §6.4.3 / §7.1.6 | 这四项在 `SGRAPH_COMPATIBILITY_TEST_REPORT.md`（8 月 14 日）中还是 sgraph 的缺陷，本轮实测**已全部修复** |

### 并发安全（不计入用例数，但同等重要）

`go test -race`：

- **原生历史快照 FAIL，当前定向复核 PASS** —— 旧版直接写点位于 `extensions.go:196-197`，根因是 `executeSubFields` 为每个 response name 起 goroutine 并共享同一个 `*executionContext`。当前字段串行执行后不再并发访问这些写点。
- **sgraph PASS** —— ctx 按值参数传入、按返回值传出（`extensions.go:245/263`），从不写共享结构；错误用 `atomic.Pointer` CAS 链表 + mutex。128 并发请求 + 并发根字段 + 单 Engine 64 并发请求全部通过。

---

## 4. ④ 新框架引入的问题（13 条 + 1 条能力边界）

按严重度排序。

### P0-1 根选择集被编译期全部裁剪时整个请求失败（11 条）

| 项 | 内容 |
|---|---|
| 触发 | `{ greeting @skip(if: true) }`、`{ ... on Query @include(if: false) { greeting } }`、`{ ...F @skip(if: true) }` —— 所有根字段被**字面量**条件指令静态排除的查询 |
| sgraph | request error：`no roots found for %!s(*ast.Name=<nil>)` |
| 原生 | `{"data":{}}` |
| 规范 | §6.3.2 + §7.1.5：被排除的字段不产出条目，`data` 是一个可能为空的 map |
| 根因 | `SkipDirectiveCompiler.Compile`（`directive_registry.go:199`）返回 `IncludeDecision=&false` → `flattenOneSelection`（`plan_compiler.go:697/711/733/749`）编译期删除 selection → `coordinateBatches`（`plan_coordinator.go:24`）判定 `len(roots)==0` 报错 |
| 附带 | 错误文案把 `*ast.Name` 用 `%s` 格式化，输出 `%!s(...)` |
| 修复方向 | `coordinateBatches` 在 roots 为空时返回空 batch 列表而非错误，结果组装产出空 map。注意区分"编译期全部裁剪"与"plan 结构损坏"两种 roots 为空的原因 |

### 已修复：空列表在冷池下被补全为 null（**本轮不再复现**）

上一版记为 P0-3。根因是 `acquireFieldResponse()` 取到**新建**对象时 `responseRaws` 是 nil slice、取到**复用**对象时是 `[:0]` 的非 nil 空 slice，而 `extractFieldResponse` 对 list 字段直接把它返回给结果组装，nil 性决定了 `null` 还是 `[]`。表现为"同一请求在同一进程的不同时刻返回不同结果"。

当前代码已在**池的出口**统一消除 nil 性（`acquireFieldResponse` 的 `responseRaws`、`acquireBulkFieldResponseState` 的 `iterationResponses`），实测 `COLD { empty } -> []`，且逐用例独立进程与单进程全量运行的结果差异降为 **0 条**，顺序依赖消除。

`FieldResponse.responsePaths` 未做同样处理，经逐点核查其全部读取点都是 `len()` 驱动且不逃逸到响应，当前不可观测（已用 IterationCallStep 冷/热对比实测确认）；但它与 `responseRaws` 共用同一套 acquire/release 逻辑，属于同类风险点，新增读取点时需要一并处理。

### 已修复：父侧关联 key 在多 `ID` 字段时随机选取（**用例矩阵未覆盖**）

不在 526 用例内，因此不占任何象限计数——本轮语料中没有任何 list 元素类型声明 ≥2 个 `ID` 字段（静态扫 200 个类型定义块 + 动态扫 7 个语料 schema，命中数均为 0）。

`checkAndCompileParentKeyFieldNames` 的兜底分支原本"遍历父类型全部字段取第一个 `ID` 字段"，而 `Fields()` 是 map、遍历顺序被运行时随机化。规范对一个对象声明几个 `ID` 字段没有限制（§3.6 只要求字段名唯一），外键 / 多租户 / Relay 场景下多 `ID` 字段很常见，因此这是合法 schema 上的非确定性行为。

影响面不限于 bulk：`parentKeyFieldName` 非空会把该 list 父类型下**所有子字段**的绑定从 occurrence 路径切成业务 key 绑定。实测（每轮重建 schema+Engine 各 60 次）：普通迭代 7/60 次报 `parent key field "betaId" is missing`、9/60 次报 `duplicate parent binding key`；bulk 选错字段则**静默返回空列表且 errors 为 0**。

推断在编译期发生并进入 Plan 缓存，因此**进程内稳定、跨进程启动才变化**——线上表现为"测试反复跑都正常，某次重启后整片报错，回滚重启又好了"。

已改为候选唯一才推断：歧义时普通迭代回退 occurrence 路径绑定，bulk 编译期报错并列出候选字段名。全量套件改动前后 922 个既有用例**零状态变化**，双链路 526 用例各自与基线 0 条不同。详见 `SPEC2025_SGRAPH_REPORT.md` §7。

### P1-1 结果组装不执行 map 中的函数属性（2 条，**静默错误已修复**，行为差异保留）

| 项 | 内容 |
|---|---|
| 触发 | 无 resolver 的字段，父结果 map 中该属性是 `func() any` |
| sgraph（修复前） | 输出指针字符串 `"0x68cea0"`，**无任何错误** |
| sgraph（修复后） | 该字段返回 `null` + 一条指名字段与路径的 field error：`field thunk resolves to a function value; the result assembler does not evaluate deferred properties, configure a resolver for this field instead`，`path: ["fromMap","thunk"]` |
| 原生 | 调用函数，输出 `"thunkValue"` |
| 根因 | `result_assembler.go` 的设计声明："map 中保存的是已经完成的属性值，不执行函数形式的延迟属性" |
| 规范定位 | §6.4.2 把 `ResolveFieldValue` 的取值方式留给实现，**不执行函数属性本身不违规**；违规的是把函数指针格式化成 `"0x…"` 当作 `String` 返回——§6.4.3 `CoerceResult` 要求产出该类型的有效值，否则必须抛执行错误。因此本项是**一致性修复**，不是兼容性妥协 |
| 已实施修复 | `result_assembler.go` 新增 `rejectDeferredFunctionProperty`，在 `extractFieldResponse` 的 4 个取值出口按 `reflect.Kind() == Func` 拦截。列表元素逐个判定（5 元素列表产出 5 条错误，路径带 occurrence 下标）；非 null 位置按既有规则冒泡且只产生一条错误；正常值与"属性不存在"路径行为不变；`-race` 无新增竞争 |
| 用例状态 | 这 2 条用例断言"函数属性被调用并得到 `thunkValue`"，修复后**仍然失败**，失败点从值不符变为 `unexpected errors`。四象限计数与 sgraph 独有失败条数（14）均不变 |
| 迁移适配 | 给该字段配置 resolver。行为从"静默拿到指针字符串"变成"拿到 null 并被明确告知哪个字段有问题" |

### 能力边界：subscription 不支持（1 条）

`executor.go:51` 直接返回 `subscription is not supported yet`。这是 `SGRAPH_USAGE_NOTES.md` 明确记录的能力边界，不计为回归。

### 已知设计边界（本轮通过参数注入适配，未计入失败）

- **业务 resolver 的 `Source` 恒为 nil**（`plan.go:977`）。本轮用 `ParamRegistry` 的 `FIELD_RESPONSE` 参数把父结果注入 resolver 参数，两轮结果一致。但这意味着**任何读 `p.Source` 的既有 resolver 迁移到 sgraph 都会拿到 nil**，且必须为每个 (query 原文, operationName) 注册绑定 —— `buildDocumentOperationKey`（`param_registry.go:275`）按文档字节哈希，改一个空格就命中不上，且未命中的绑定会在 `finalizeParamRegistry` 报错。
- **FIELD_RESPONSE 来源字段必须有 resolver**（`plan_compiler_param_registry.go:308`）。中间层是"无 resolver、纯从祖父结果组装"的对象时，无法作为参数来源，父结果传不下去。
- **根字段必须有 resolver**（`plan_coordinator.go`）。原生可用默认 resolver 从 RootObject 取值，sgraph 直接报 `field N has no resolver function`。
- **带参数的字段必须有 resolver**（`plan_coordinator.go` 的 `field %d has param plan but no resolver`，内省字段豁免）。原生允许字段声明参数却不配 resolver，默认 resolver 直接取父对象属性、忽略参数。
- **自定义指令必须先注册到 `DirectiveRegistry`**（`plan_compiler.go` 的 `no directive compiler found for %s`）。有执行语义的注册 compiler / runtime handler，纯标注型用 `RegisterMetadataOnly`。这是 `SGRAPH_USAGE_NOTES.md` 生命周期第 2 步的既定契约。迁移成本：原生对"声明了但执行期无语义"的指令是零成本忽略，迁移时必须逐个补登记，漏一个则用到它的请求整体失败。建议启动期用 `schema.Directives()` 对账 registry，把失败前移。
- **第一条请求必须晚于 `RegisterSGraphEngine`**。Engine 与 Schema 的绑定按设计是一次性、进程内不可变的：未注册时 `getSGraphEngineForSchema` 会创建只含 `@skip`/`@include` 的默认 Engine 并写入全局缓存，之后再注册返回 `another sgraph engine is already registered for this schema`，无解绑接口。启动顺序错误的后果是确定性的，只能重启进程纠正。已写入 `SGRAPH_USAGE_NOTES.md` 的接入检查清单。

---

## 5. ② 新旧框架不兼容问题汇总（45 条）

"不兼容"= 同一请求在两条链路上行为不同。它是 ③（31 条）与 ④（14 条）的并集。按是否违反规范划分：

| 分档 | 条数 | 内容 |
|---|---:|---|
| **sgraph 合规、原生违规** | 历史快照 31 | 见 §3；其中 3 条 non-null 进程崩溃差异已随原生串行化消失。当前总数待完整重跑 |
| **原生合规、sgraph 违规** | 13 | 见 §4。P0-1 会让原本成功的请求直接失败 |
| **无规范约束的能力差异** | 1 | subscription |

**迁移风险清单（按客户端可感知程度排序）：**

1. 原本返回 `{"data":{}}` 的全裁剪查询变成 request error（P0-1）
2. 函数属性不再被执行：该字段返回 null 并附一条 field error（P1-1，已从"静默变指针字符串"改为显式报错）
3. 读 `p.Source` 的 resolver 全部拿到 nil（设计边界）
4. 原本被忽略的自定义指令必须逐个补 `RegisterMetadataOnly`，漏一个则该指令的全部请求失败（接入约束）
6. 响应字段顺序改变（合规改善，但客户端若依赖旧顺序会受影响）
7. 原本静默为 null 的字段现在多出 error 条目（合规改善，但 `errors` 长度变化）
8. 显式 null 不再套用默认值（合规改善，但业务默认值行为改变）

---

## 6. 修复优先级建议

### sgraph 侧

| 优先级 | 项 | 理由 |
|---|---|---|
| **P0** | 根选择集被编译期全部裁剪 → 返回空 data 而非报错 | 合法查询被整体拒绝，且 `@skip(if: true)` 是极常见写法 |
| ~~P0~~ | ~~空列表补全依赖对象池状态~~ | **已修复**（acquire 出口保证切片非 nil）。建议把"池化切片必须非 nil""读点用 len 而非 nil 判空"写进维护约束，`responsePaths` 是同类残留风险点 |
| ~~P1~~ | ~~map 中的函数属性：要么执行，要么报错~~ | **已修复**（方案 A：组装阶段按 `reflect.Kind() == Func` 拦截并写 field error）。若后续决定支持惰性属性，应在执行阶段而非组装阶段调用，以免函数在组装阶段被并发调用 |
| ~~P0~~ | ~~父侧关联 key 在多 `ID` 字段时随机选取~~ | **已修复**（候选唯一才推断；歧义时普通迭代回退 occurrence 路径，bulk 编译期报错）。用例矩阵未覆盖，见 sgraph 报告 §7 |
| **P1** | 把"池化切片 acquire 时必须非 nil""读点用 `len()` 而非 `== nil` 判空"写进维护约束 | 本轮 P0-3 已按此修复；`FieldResponse.responsePaths` 是同类残留风险点 |
| **P1** | 评估是否新增显式指定父侧关联 key 的配置入口（`ObjectConfig.KeyFieldName` 或 `FieldDefinition.ParentKeyFieldName`） | 当前只能靠推断。`id` 名字被非 `ID` 类型占用、同时又有多个 `ID` 字段的 schema 用不了 bulk。若新增，配置项建议设计成 `[]string`，可顺带打开复合业务主键（`generateCompositeKey` 内部已支持） |
| **P2** | 修正 `%s` 用于 `uint32` / `ParamTypeEnum` / `*ast.Name` 的 11+1 处格式符 | 错误信息不可读，也阻断 `go vet` |
| **P2** | 更新 `SGRAPH_USAGE_NOTES.md` / `SGRAPH_COMPATIBILITY_TEST_REPORT.md` / `CONFORMANCE_MATRIX.md` | 三份文档均与当前代码不符，详见 sgraph 报告 §6 |

### 两条链路共有（原生侧）

| 优先级 | 项 |
|---|---|
| ~~P0~~ | ~~`executionContext.Context` 数据竞争~~：当前原生字段串行执行后定向 `-race` 通过；写点仍存在，但不再被字段 goroutine 并发访问 |
| ~~P0~~ | ~~non-null 冒泡跨 goroutine panic 终止进程~~：当前原生字段串行执行后已能返回字段错误 |
| **P0** | fragment 环导致校验期栈溢出（两条链路共有，共用 validate） |
| **P1** | `rules.go:1495` UniqueInputFieldNames 规则注册键错误，规则从未生效 |
| **P1** | 输入强制转换过宽（13 条） |
| **P1** | request error 响应应省略 `data` 条目（`types.go:11` 加 `omitempty` 不够，需要区分"无 data"与"data 为 null"） |
| **P1** | `null` 字面量 parser 支持 |
| **P2** | 非空变量带默认值不应被拒绝 |
| **P2** | September 2025 内省与类型系统能力补齐（26 条，工作量大，属于版本升级） |

---

## 7. 方法学说明与剩余风险

**已验证的可比性保障：**

- 两轮用例源码逐字节相同，唯一变量是一行路由。
- `parentRef` 注入对原生完全惰性（`values.go:60-65` 不会把未提供且无默认值的参数写进 `p.Args`），实测两条链路对同一注入查询产出完全一致的 data。
- 断言不依赖错误顺序（集合语义），不依赖 map 顺序（`toPlainValue` 归一化），顺序断言单独走 `json.Marshal` 字节序。
- 无任何 `t.Skip`；会导致进程崩溃的 6 个用例走子进程隔离，子进程失败仍按真实失败上报。
- 两轮各 526 个叶子用例全部产出确定结果，无 UNKNOWN。

**剩余风险（明确标注，不做无依据的推断）：**

1. 本报告覆盖的是**当前可执行 API 表面**，不是规范每一句规范性语句的形式化证明。§2.14 Schema Coordinates 与 §3 各类 Type System Extension 属于 SDL 定义语法，本仓库用编程式 schema 构建，无对应入口，记为 **OUT-OF-SCOPE**；§6.2.3.3 Unsubscribe 无公开 API，记为 **BOUNDARY**。
2. `parentRef` 注入把"sgraph 需要外部配置才能传递父结果"这一差异从数据断言中移开了。该差异在 §4「已知设计边界」中单独记录，但它的**实际迁移成本**（每个 query 原文都要注册绑定）没有量化用例。
3. mutation 在两条链路上都由 `ExecuteGraphQLGo` 执行，因此 mutation 相关用例的一致性**不能证明 sgraph 的 batch 链路支持 mutation**，只能证明路由未回归。
4. 并发用例通过 channel 互等证明真并发，但没有度量吞吐或延迟；查询折叠的**性能收益**本轮未测量。
5. 顺序依赖已消除：池 nil 性修复后，逐用例独立进程与单进程全量运行的结果差异为 0 条。本报告仍统一采用**每个顶层 Test 单独起进程**的结果，因为它能防止任一用例的进程级崩溃吞掉后续结果。
6. bulk resolver 无公开配置入口（`Field` 结构体没有 `BulkResolve` / `BulkArgs` / `BulkResultMappedFieldName`，只有内部 `FieldDefinition` 有），因此 bulk 路径未纳入对比基线，SGraph 专项中也只覆盖了编排与参数物化，未覆盖 bulk 绑定错误的完整路径。
