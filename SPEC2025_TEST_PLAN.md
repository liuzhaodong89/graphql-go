# GraphQL September 2025 规范测试方案（graphql-go 原生链路 / sgraph 链路 双轮对比）

状态：**待确认，尚未修改任何项目文件**。
基准规范：https://spec.graphql.org/September2025/ （下称"规范"，章节号均引自该版本目录）。

---

## 0. 已完成的代码通读结论（作为方案前提）

以下事实来自本轮对当前工作区代码的完整通读，是方案设计的依据；如与你的预期不符，请先纠正，我再调整方案。

### 0.1 两条链路的切换点

`executor.go:30` 的公开 `Execute` 目前无条件调用 `executeSGraph`；`executeSGraph` 内部按 operation 类型分流：

| operation | 当前实际执行者 |
|---|---|
| query | SGraphEngine（`engine.executeWithCache` → `coordinateBatches` → `BatchPlan.execute` → `assembleGraphResult`） |
| mutation | `ExecuteGraphQLGo`（原生串行链路） |
| subscription | 直接返回 `subscription is not supported yet` |

因此**"切回原生链路"只需把 `Execute` 改为 `return ExecuteGraphQLGo(p)`**，是 1 处 1 行改动，parse/validate 完全共用，不影响 §2/§5 的行为。

### 0.2 sgraph 与原生的结构性差异（会直接决定测试如何写）

| 差异 | 事实依据 | 对测试的影响 |
|---|---|---|
| **业务 resolver 的 `Source` 恒为 nil** | `plan.go:977` `return resolverFn(nil, params, info, ctx)`；只有 `__typename` 例外 | 读 `p.Source` 的 resolver 在 sgraph 下拿到 nil。必须用"双读"resolver（见 §2.2），否则两轮断言不可比 |
| **根字段必须有 resolver** | `plan_coordinator.go:195` `field %d has no resolver function` | 共享语料的根字段一律显式配 resolver；默认 resolver 语义只在**非根**字段上测 |
| **mutation 不走 sgraph** | `executor.go:47`；`plan_coordinator.go:18` 也会拒绝非 query | mutation 用例两轮结果理论上应完全一致，作为"路由未回归"的对照组 |
| **subscription 不支持** | `executor.go:51` | 记为能力边界（BOUNDARY），不伪造通过，也不算 sgraph 新引入缺陷 |
| **同 batch 并发条件是 `concurrent && len(steps) > 1`** | `plan.go:388`；`ensureBatch` 对 query 恒置 `concurrent=true`（`plan_coordinator.go:297`） | 旧报告里"阈值 8 导致小 batch 串行"的现象在当前代码中**已不存在**，需重新实测 |
| **响应保序** | `SGraphResponseOrderedMap`（`rundata.go:635`）+ `MarshalJSON`（`rundata.go:719`） | §7.2.2 用例在 sgraph 下可能通过、原生下失败 |
| **叶子序列化失败会记错误** | `result_assembler.go:774` `serializeLeafValue`；原生 `executor.go:971` `completeLeafValue` 静默返回 nil | §6.4.3 用例两轮结果不同 |
| **错误 path 含 list 下标** | `materializeFieldPlanPath`（`rundata.go:238`）产出 `[]any{string\|int}` | §7.1.6 path 断言两轮均可施加同一强断言 |
| **同一 fieldId 多错误用 CAS 链表保留** | `rundata.go:170-185` | 旧文档 `SGRAPH_USAGE_NOTES.md` 第"字段错误存储限制"一节描述的"后写覆盖先写"**与当前代码不符**，当前已改为链表；方案按当前代码写强断言 |
| **bulk resolver 无公开配置入口** | `Field` 结构体（`definition.go`）没有 `BulkResolve` / `BulkArgs` / `BulkResultMappedFieldName`，只有内部 `FieldDefinition` 有 | bulk 用例只能在 schema 构建后改内部 `FieldDefinition`，属 sgraph 专项，不进入对比基线 |

### 0.3 当前工作区已存在的、与本次测试无关的问题（先行报告，不擅自修改）

1. `benchmark_sgraph_query_folding_test.go`、`graphqlgo_native_matrix_test.go`、`sgraph_occurrence_error_test.go`、`sgraph_query_folding_test.go` 仍 `import "github.com/graphql-go/graphql/sgraph"`，但 `sgraph/` 目录已被删除（`git status` 显示 `D sgraph/*.go`）。**根包当前无法编译测试**。
2. `plan.go`、`plan_compiler.go`、`plan_coordinator.go` 共 11 处 `fmt` 格式符类型错误（`%s` 用于 `uint32` / `ParamTypeEnum`），不加 `-vet=off` 时 `go test` 直接失败。
3. `examples/hello-world/main.go` 引用已删除的 `plan` 包，`go test ./...` 会被它阻断。
4. 你的项目目录下有我为了把代码同步到云端编译环境而生成的临时文件 `_stage_sgraph_src.tgz`（本机未安装 Go，测试必须在云端容器执行）。**需要你授权后我才会删除或移动它**。

上述 1~3 会阻断测试执行。第 1 项是必须先修复的前置条件；第 2、3 项可用 `-vet=off` 和限定包路径绕过，我倾向**不改**。

### 0.4 现有测试资产（可复用，避免重复造轮子）

- `graphqlgo_spec_conformance_test.go`：25 个 `TestGraphQLGoSpec_`，含 `newSpecConformanceSchema`、`executeGraphQLGoSpec`、`executeSGraphSpecWithParamRegistry`、`assertGraphQLData`、`runGraphQLGoConformanceSubprocess`（子进程隔离 panic/栈溢出）等 helper。
- `graphqlgo_native_matrix_test.go`：32 个 `TestGraphQLGoNative_` + 11 个 `TestGraphQLGoStrict_`。
- `sgraph_query_folding_test.go` / `sgraph_occurrence_error_test.go`：21 个 sgraph 内部行为用例。
- 注意：`package graphql` 的测试**不能** import `testutil`（会形成 import cycle）。
- 文档 `CONFORMANCE_MATRIX.md` 提到的 `SGRAPH_SPEC_CONFORMANCE` 环境变量**在代码中不存在**，其列举的 `TestSGraphSpec_` 等测试名也全部不存在，该文档已与代码脱节。

---

## 1. 本次方案的目标与判定基准

1. **断言以规范为准，不以任一实现的现状为准。** 任何用例的期望值都从规范条文推导，不允许因为某条链路做不到就降低期望。
2. **两轮使用完全相同的用例源码、相同的 schema、相同的 query、相同的变量、相同的断言。** 唯一允许的差异是 §2.2 描述的"参数依赖注入"，且该注入对两轮**同时生效**（schema 相同、resolver 相同），只是原生链路走 `p.Source`、sgraph 走 `p.Args`，最终值相同。
3. **不为让某条链路通过而改写期望、加 `t.Skip`、加 if-else 分支。** 无法测试的项显式标记为 `BOUNDARY`（能力边界）或 `UNREPRESENTABLE`（当前 schema 模型无法表达），并在报告中单列。
4. **结果可机器比对。** 两轮都用 `go test -json` 采集，逐用例（含子用例）落成 `PASS/FAIL/BOUNDARY`，再机械推导四类结论。

---

## 2. 可比性设计（方案的核心，请重点确认）

### 2.1 执行入口

新增测试统一通过公开入口 `graphql.Do(Params{...})` 执行 —— 这是唯一同时覆盖 parse → validate → execute 的公开链路，也是两轮唯一的差异点所在。不直接调 `engine.Execute`，避免绕过 `Do` 的 extension/validate 阶段造成两轮语义不等价。

### 2.2 `Source == nil` 的处理：双读 resolver + ParamRegistry 注入

sgraph 给业务 resolver 传 `Source = nil` 是**当前实现的既定事实**（`plan.go:977`），不是 bug 也不是可配置项。为了让同一份断言在两轮都成立，采用如下写法（这是你允许的"增加必要的参数依赖设置和 resolver 入参调整"）：

```go
// 语料公共 helper，两轮完全相同
func parentOf(p ResolveParams) map[string]any {
    if p.Source != nil {              // 原生链路
        if m, ok := p.Source.(map[string]any); ok { return m }
    }
    if v, ok := p.Args["parentRef"]; ok {   // sgraph 链路：由 ParamRegistry 注入
        if m, ok := v.(map[string]any); ok { return m }
    }
    return nil
}
```

配套约束（用于保证这不是"迎合框架"）：

- `parentRef` 在 schema 中声明为**可空、无默认值**的参数。原生链路下 query 不传它、注册表也不生效，`compileParamPlansByArgDefs` 对"未提供且可空"直接跳过，`p.Args` 中不会出现该 key —— 原生行为不受任何影响。
- 两轮**使用同一个 ParamRegistry 配置**。原生链路根本不读 registry，配置存在与否对它无副作用。
- `parentRef` 只用于把父对象**原样**递给子 resolver，不改变子 resolver 的业务逻辑、不预先算好答案、不绕过任何被测行为。
- 引入 `parentRef` 会改变 introspection 中该字段的 `args` 形状。因此 **§4 内省用例使用独立的、不含任何注入参数的 schema**，避免注入污染内省断言。
- 需要证明"注入本身没有放水"的地方，额外加一条对照用例：同一字段在**不配置**任何 registry 绑定时执行，断言 sgraph 下 `parentRef` 缺失、子字段取值为 nil —— 用来暴露"sgraph 无法在无外部配置时传递父结果"这一真实差异，而不是把它藏起来。

### 2.3 其余适配点

| 适配 | 做法 | 为什么不算放水 |
|---|---|---|
| 根字段必须有 resolver | 共享语料所有根字段显式配 resolver | 原生链路对显式 resolver 的行为与默认 resolver 完全一致；默认 resolver 语义另在非根字段上单独测（含 map/struct tag/func 属性/`FieldResolver` 四分支） |
| `Result.Data` 类型不同 | 断言前统一 `toPlainValue(result.Data)` | 仅做容器归一化，不改内容；字段**顺序**断言另走 `json.Marshal(result.Data)` 原始序列化，不归一化 |
| 错误顺序不同 | 错误断言按 **集合语义**（存在性 + 数量 + path/message/locations/extensions 逐项匹配），不按数组下标 | 规范 §7.1.6 未规定 errors 顺序 |
| 可能栈溢出/panic 的用例 | 复用现有 `runGraphQLGoConformanceSubprocess` 子进程隔离模式 | 子进程失败仍按真实失败上报，不转 skip |

### 2.4 两轮之间的代码改动清单（获授权后执行）

**第一轮（原生）**
- `executor.go`：`Execute` 改为 `return ExecuteGraphQLGo(p)`（1 处）。
- 前置修复：3 个测试文件删除 `"github.com/graphql-go/graphql/sgraph"` import 并去掉 `sgraph.` 前缀（纯符号迁移遗留，不改任何逻辑）。

**第二轮（sgraph）**
- `executor.go`：`Execute` 还原为 `return executeSGraph(p)`（1 处）。
- 其余文件**零改动**。

新增测试文件在两轮之间**完全不变**。

---

## 3. 测试资产结构

| 文件 | 内容 |
|---|---|
| `spec2025_corpus_test.go` | 共享 schema 集合、双读 resolver、ParamRegistry 绑定表、执行/断言 helper、用例状态记录 |
| `spec2025_language_test.go` | §2 语言层 |
| `spec2025_typesystem_test.go` | §3 类型系统 |
| `spec2025_introspection_test.go` | §4 内省（独立 schema） |
| `spec2025_validation_test.go` | §5 校验（全部 32 条规则正反用例） |
| `spec2025_execution_test.go` | §6 执行 |
| `spec2025_response_test.go` | §7 响应 |
| `spec2025_cross_test.go` | 场景交叉矩阵 |
| `spec2025_boundary_test.go` | 边界值与极限值 |
| `spec2025_sgraph_ext_test.go` | sgraph 专项（**不进入对比基线**，仅在第二轮有意义） |

命名约定沿用现有习惯：`TestSpec2025_<章节>_<行为断言句>`，子用例用 snake_case。所有新增类型名统一加 `S25` 前缀，避免与现有测试的 schema 类型重名（现有 `TestGraphQLGoNative_SchemaConstructionBoundaries/duplicate_type_name` 正是在验证同包重名会报错）。

### 3.1 Schema 集合（5 个，职责分离）

| schema | 用途 | 关键内容 |
|---|---|---|
| `S25CoreSchema` | §2/§5/§6/§7/交叉/边界主语料 | Scalar(Int/Float/String/Boolean/ID/自定义 `S25Odd`/`S25DateTime`)、Enum(`S25Mode{A,B,DEPRECATED_C}`)、Interface(`S25Node`)、Object(`S25User`/`S25Robot`)、Union(`S25Search`)、InputObject(`S25Filter` 含默认值/嵌套/list)、全部 wrapping 组合字段、故意报错字段、所有根字段显式 resolver、子字段带 `parentRef` |
| `S25IntrospectSchema` | §3/§4 | **无任何注入参数**，含 deprecated 字段/enum 值、描述、interface 实现 interface、多层 wrapping、自定义 directive（含 `VARIABLE_DEFINITION` 位置） |
| `S25MutationSchema` | §6.2.2 | 串行 mutation、含可空失败字段、含嵌套子字段 |
| `S25SubscriptionSchema` | §6.2.3 | 单/多根字段、`Subscribe` |
| `S25DefaultResolverSchema` | §6.4.2 默认 resolver | 非根字段的 map / struct(json+graphql tag) / map 中的 func / `FieldResolver` 四分支 |

---

## 4. 用例矩阵（按规范章节）

下表 "#" 为顶层 Test 函数数 / 子用例数（估算）。**判定列**说明该用例期望值的规范依据。

### §2 Language（12 / 58）

| ID | 规范 | 用例 | 判定 |
|---|---|---|---|
| L-01 | 2.1.1–2.1.6 | 忽略符交叉：BOM + 空格 + Tab + LF/CR/CRLF + 注释 + 逗号 在同一 query 中混排 | 与紧凑写法结果逐字节相同 |
| L-02 | 2.1.8 | Name 合法性：`_a`、`a1`、`A_1`；非法 `1a`、`a-b`、`$`、空名 | 合法执行，非法为 request error 且无 `data` 键 |
| L-03 | 2.1.7 | 全部 Punctuator 出现在一个 query 中（`! $ & ( ) ... : = @ [ ] { | }`） | 解析成功 |
| L-04 | 2.2 | 描述出现在 OperationDefinition / FragmentDefinition / VariableDefinition 前（单行串 + block string） | 解析成功且**不影响**执行与响应 |
| L-05 | 2.3 | 单文档多 operation、operation 与 fragment 混排、只有 fragment 的文档 | 前两者按 operationName 选择；第三者 request error |
| L-06 | 2.4 | 匿名 query 简写形式 `{ a }`；显式 `query`；`mutation`；`subscription` | 各自路由正确 |
| L-07 | 2.5–2.6 | 嵌套 selection set、同名字段重复出现、字段无 selection set | 合并语义见 §6.3.2 |
| L-08 | 2.7 | 参数：字面量 / 变量 / 省略用默认值 / 显式 null / 参数顺序无关 | 见 §6.4.1 |
| L-09 | 2.8 | 别名：同字段不同别名不同参数、别名与字段名冲突、别名为保留字样式 | 响应 key 用别名 |
| L-10 | 2.9.1–2.9.2 | 命名片段 / 内联片段 / 无 type condition 的内联片段 / 片段嵌套 3 层 / 同一片段被 spread 两次 | 展开结果与手写展开相同 |
| L-11 | 2.10.1–2.10.8 | 全部 8 种输入字面量，含 `null` 字面量、空 list `[]`、空 input object `{}`、enum 字面量、block string、`\u` 转义与代理对 | 逐值回显比对 |
| L-12 | 2.11–2.13 | 变量定义（默认值 / non-null / list 类型引用 / 变量上带 directive）、类型引用嵌套 `[[Int!]!]!`、directive 出现在全部可执行位置 | 解析 + 执行正确 |

### §3 Type System（9 / 41）

| ID | 规范 | 用例 | 判定 |
|---|---|---|---|
| T-01 | 3.3.1 | query-only schema 上执行 mutation / subscription | request error |
| T-02 | 3.5.1 | Int 序列化与输入强制：`0`、`±2147483647`、`±2147483648`、小数、数字字符串、bool | 越界/非整数必须是错误，不能静默 null |
| T-03 | 3.5.2 | Float：整数提升、`1.0`、`1e10`、`-0.0`、`NaN`/`+Inf`（Go 侧）、数字字符串 | 非法必须报错 |
| T-04 | 3.5.3 | String：空串、超长(64KiB)、含 ` `、含代理对、非字符串输入 | 非字符串输入必须是错误（当前 `coerceString` 用 `%v` 兜底，预期失败） |
| T-05 | 3.5.4 | Boolean：`true/false`、数字输入、字符串输入 | 非 bool 输入必须报错 |
| T-06 | 3.5.5 | ID：字符串 / 整数输入互通，序列化为字符串 | |
| T-07 | 3.6.2 / 3.9 / 3.10 | 字段/enum 值 deprecation；enum 未知内部值序列化；input object 默认值与显式 null 的区别 | 未知 enum 值必须产生 field error |
| T-08 | 3.10.1 | **@oneOf input object**：恰好一个字段、零个字段、两个字段、字段值为 null | 期望：合法/非法按规范；当前 `InputObjectConfig` 无 `IsOneOf` → 预期 UNREPRESENTABLE |
| T-09 | 3.11–3.12.1 | List/Non-Null 全组合：`T`,`T!`,`[T]`,`[T]!`,`[T!]`,`[T!]!`,`[[T!]!]!` × 值为 null / 空 list / 含 null 元素 | 冒泡范围严格按 §6.4.3 |

### §4 Introspection（8 / 46）

| ID | 规范 | 用例 |
|---|---|---|
| I-01 | 4.1 | `__typename` 在 Object / Interface / Union / list 元素 / 别名 / 片段内 |
| I-02 | 4.2.1 | `__schema` 全字段，含 **`description`**（Sept2025）、`types`、`queryType`、`mutationType`、`subscriptionType`、`directives` |
| I-03 | 4.2.2 | `__Type` 全字段，含 **`specifiedByURL`**、**`isOneOf`**、`inputFields(includeDeprecated:)`、`interfaces`（含 interface 实现 interface）、`possibleTypes`、`ofType` 链 |
| I-04 | 4.2.3 | `__Field.args(includeDeprecated:)`、`isDeprecated`、`deprecationReason`、`description` 非空 |
| I-05 | 4.2.4 | `__InputValue.isDeprecated` / `deprecationReason` / `defaultValue` 以 **GraphQL 字面量**打印（enum 不带引号、list 用 `[...]`、input object 用 `{...}`） |
| I-06 | 4.2.5 | `__EnumValue` 全字段 + `enumValues(includeDeprecated:)` 开关 |
| I-07 | 4.2.6 | `__Directive.isRepeatable`、`args(includeDeprecated:)`、`locations` 完整性；内置 `@skip/@include/@deprecated/@specifiedBy/@oneOf` 全部存在且 locations 与规范一致 |
| I-08 | 4.2 | 内省 × 片段 / 别名 / 变量 / `@skip` 交叉；未知 `__type(name:)` 返回 null；内省字段与业务字段同层混排 |

> 预判：`SchemaConfig` 无 `Description`、`ScalarConfig` 无 `SpecifiedByURL`、`InputObjectConfig` 无 `IsOneOf`、`Argument`/`InputObjectFieldConfig` 无 `DeprecationReason`、`Directive` 无 `IsRepeatable` —— I-02/03/04/05/07 的对应子用例大概率两轮同时失败，归入"原框架问题（两条链路共有）"。**不因此降低断言**。

### §5 Validation（32 / 96）

规范 §5 共 32 条具名规则。每条至少 1 正 1 反，共 96 个子用例：

5.1.1 Executable Definitions｜5.2.1.1 Operation Type Existence｜5.2.2.1 Operation Name Uniqueness｜5.2.3.1 Lone Anonymous Operation｜5.2.4.1 Single Root Field｜5.3.1 Field Selections｜5.3.2 Field Selection Merging｜5.3.3 Leaf Field Selections｜5.4.1 Argument Names｜5.4.2 Argument Uniqueness｜5.4.3 Required Arguments｜5.5.1.1 Fragment Name Uniqueness｜5.5.1.2 Fragment Spread Type Existence｜5.5.1.3 Fragments on Object/Interface/Union｜5.5.1.4 Fragments Must Be Used｜5.5.2.1 Fragment Spread Target Defined｜5.5.2.2 No Cycles｜5.5.2.3.1–4 Fragment Spread Is Possible（4 个子场景各一组）｜5.6.1 Values of Correct Type｜5.6.2 Input Object Field Names｜5.6.3 Input Object Field Uniqueness｜5.6.4 Input Object Required Fields｜5.7.1 Directives Are Defined｜5.7.2 Valid Locations｜5.7.3 **Unique per Location**｜5.8.1 Variable Uniqueness｜5.8.2 Variables Are Input Types｜5.8.3 All Variable Uses Defined｜5.8.4 All Variables Used｜5.8.5 All Variable Usages Are Allowed

重点/交叉补充：
- 5.3.2 加强：互斥类型下的同名别名**可以**合并（正例）、同名不同参数**不可**合并（反例）、同名不同返回类型（反例）、别名指向不同字段（反例）、跨嵌套片段的深层冲突（反例）。
- 5.5.2.2 片段环：自引用、二元环、三元环 —— **子进程隔离执行**（已知可能栈溢出）。
- 5.8.5 变量位置：`$a: Int` 用在 `Int!` 位（反例）、`$a: Int = 1` 用在 `Int!` 位（正例）、`$a: Int!` 用在 `[Int]` 位（反例）、`$a: [Int!]` 用在 `[Int]` 位（正例）。
- 校验错误必须带 `locations`，且 request error 响应**不含 `data` 键**（§7.1.3）。

### §6 Execution（26 / 118）

| ID | 规范 | 用例要点 |
|---|---|---|
| E-01 | 6.1.1 | 校验失败时**不得执行任何 resolver**（用调用计数器断言） |
| E-02 | 6.1.2 | 变量强制转换全矩阵：缺失+有默认 / 缺失+无默认+可空 / 缺失+non-null（错误）/ 显式 null+可空 / 显式 null+non-null（错误）/ 显式 null 覆盖默认值 / 类型不符（错误）/ 单值提升为 list / 嵌套 input object 中的缺失与显式 null / 未知 input 字段（错误）/ 非法 enum（错误） |
| E-03 | 6.2.1 | query 根字段**可并行**（用 channel 互相等待证明真并发，而非耗时推断） |
| E-04 | 6.2.2 | mutation 根字段**必须串行**；前一个根字段的**全部嵌套子字段**完成后才执行下一个；可空根字段报错后继续串行 |
| E-05 | 6.2.3 | subscription：单根字段规则、`Subscribe` 每事件过一遍 executor、`__typename` 作为唯一根字段（规范禁止） |
| E-06 | 6.3.1–6.3.2 | 字段收集：重复 response name 合并且 resolver 只调 1 次、跨片段合并、`@skip/@include` 参与收集、collect 顺序决定响应顺序 |
| E-07 | 6.3.3 | 执行顺序与响应位置（§7.1.4）：先出现的 response name 占位在前，即使该次出现被运行期跳过 |
| E-08 | 6.4.1 | 字段参数强制转换：字面量 / 变量 / 参数默认值 / 变量默认值 / 显式 null 的**优先级**全矩阵（6 组） |
| E-09 | 6.4.2 | 值解析：显式 resolver、默认 resolver 四分支、resolver 返回 error、resolver panic、resolver 返回 `nil`、返回 typed-nil 指针、返回函数（thunk） |
| E-10 | 6.4.3 | 值补全全矩阵：Scalar/Enum 序列化失败、List 非可迭代、typed list（`[]string`/`[3]int`/`*[]string`）、嵌套 list、抽象类型 `ResolveType` / `IsTypeOf` fallback / 运行期类型非法 / 运行期类型不在 possibleTypes、Object 上 `IsTypeOf` 校验 |
| E-11 | 6.4.3 | **null 冒泡**：non-null 叶子为 null → 父对象 null；non-null 对象 → 继续上冒；`[T!]` 含 null → 整 list null；`[T]!` 为 null → 父 null；冒泡至根 → `data: null`；冒泡时兄弟字段数据保留 |
| E-12 | 6.4.4 | 错误处理：单字段多错误、兄弟字段各自报错、list 中多个元素分别报错**全部保留**、错误不阻断其他字段、`extensions` 透传、`locations` 存在、`path` 含 list 下标与别名 |
| E-13 | 6.2 | 执行期 context 传播、取消（`ctx.Done()`）、超时 |

### §7 Response（9 / 34）

| ID | 规范 | 用例 |
|---|---|---|
| R-01 | 7.1.1 | 成功响应含 `data`，可含 `errors`；`errors` 非空时不得为空数组 |
| R-02 | 7.1.3 | **request error 响应不得包含 `data` 键**（JSON 层断言，非 `data:null`） |
| R-03 | 7.1.4 | Response Position：响应中字段顺序 = query 中 response name 首次出现顺序（含片段展开、含被跳过的出现位） |
| R-04 | 7.1.5 | `data` 为 null 的合法场景（根 non-null 冒泡） |
| R-05 | 7.1.6 | 错误对象形状：`message` 必填、`locations` 为 `{line,column}` 数组、`path` 为字符串/整数混合数组、`extensions` 为 map |
| R-06 | 7.1.7 | `extensions` 顶层键；extension 自身错误进入 `errors` 但不参与 null 冒泡 |
| R-07 | 7.1.8 | 顶层不得出现 `data`/`errors`/`extensions` 之外的键 |
| R-08 | 7.2.1 | JSON 序列化：整数不带小数点、字符串转义、null 表示 |
| R-09 | 7.2.2 | **Serialized Map Ordering**：`json.Marshal(result)` 的字节顺序与 query 顺序一致（`z,a,m` 用例） |

---

## 5. 场景交叉矩阵（10 / 96）

每个交叉用例都是一次完整请求，同时激活多个维度，用于发现"单维度都对、组合起来错"的问题。

| ID | 交叉维度 | 组合数 |
|---|---|---|
| X-01 | `@skip`/`@include` × (字面量/变量) × (true/false) × (字段/内联片段/命名片段/片段定义) | 4×2×2×... 取 24 组 |
| X-02 | 同一 response name 多次出现 × 每次带不同 skip/include 组合（OR 语义）× 别名 | 8 组 |
| X-03 | 抽象类型 × list × non-null 元素 × 元素报错 × `__typename` × 内联片段类型分发 | 12 组 |
| X-04 | 错误 × list 下标 × 别名 × 嵌套层级（3 层）× 兄弟字段保留 | 8 组 |
| X-05 | 变量 × input object 嵌套 × list 提升 × 默认值 × 显式 null × enum | 12 组 |
| X-06 | 内省字段 × 业务字段同层 × 片段 × 别名 × `@skip` | 6 组 |
| X-07 | 自定义 scalar × 变量输入 × 字面量输入 × 序列化失败 × list 中 | 6 组 |
| X-08 | 默认 resolver 四分支 × non-null × list × 报错 | 8 组 |
| X-09 | Extension 钩子 × 被 skip 的字段 × `__typename` × 报错字段 × 并发根字段 | 6 组 |
| X-10 | mutation 串行 × 嵌套子字段 × 可空报错 × 后续根字段仍执行 | 6 组 |

## 6. 边界值与极限值矩阵（12 / 62）

| ID | 维度 | 取值 |
|---|---|---|
| B-01 | Int | `0`, `1`, `-1`, `2147483647`, `-2147483648`, `2147483648`(越界), `-2147483649`(越界), `1.0`(非整), `"5"`(字符串), `true` |
| B-02 | Float | `0.0`, `-0.0`, `1e-308`, `1.7976931348623157e308`, `1e309`(溢出), `"1.5"`, 整数提升 |
| B-03 | String | `""`, 1 字符, 64 KiB, 含 ` `, 含代理对 `😀`, block string 缩进边界 |
| B-04 | ID | `""`, 纯数字字符串, 整数, 超长 |
| B-05 | Enum | 全部值、deprecated 值、未定义值、大小写变体 |
| B-06 | List | `[]`, 1 元素, 1024 元素, 10240 元素, 含全 null, 首/尾为 null |
| B-07 | 嵌套 list | `[[T]]` 2 层 → `[[[[T]]]]` 4 层；每层空/含 null |
| B-08 | 选择集深度 | 1 / 8 / 32 / 64 层（自引用类型 `FieldsThunk`） |
| B-09 | 选择集广度 | 1 / 64 / 512 个兄弟别名字段 |
| B-10 | 变量 | 0 / 1 / 64 / 256 个变量；input object 嵌套 8 层 |
| B-11 | 片段 | 0 / 1 / 128 个片段 spread；同一片段 spread 64 次 |
| B-12 | 并发与文档规模 | 128 并发请求共享同一 `*ast.Document`（`-race`）；单文档约 1 MiB；plan cache 命中/未命中交替 |

> B-08 的 64 层与 B-12 的 `-race` 用例采用子进程隔离，避免栈溢出/竞态导致整轮测试丢失结果。

## 7. sgraph 专项（8 / 30，**不进入两轮对比基线**）

仅在第二轮有意义，用于评估新引擎自身设计目标是否达成，报告中单列一节：

- S-01 批次拓扑：无依赖全折叠进 batch 0；链式依赖生成 N 批；混合依赖分批正确。
- S-02 同 batch 真并发（channel 互等证明），验证旧报告"小 batch 串行"是否仍存在。
- S-03 参数依赖 CONST / INPUT / FIELD_RESPONSE 三种来源的物化正确性与 `fieldResultPaths` 深路径取值。
- S-04 依赖图非法配置：自依赖、环、未知源、指向无 resolver 字段、指向多结果字段被单值消费。
- S-05 IterationStep 逐元素参数与 occurrence path；父列表为空/含 nil 元素。
- S-06 BulkStep 复合 key 回绑、乱序结果保序、重复 key 报错、绑定错误延迟上报路径。
- S-07 Plan cache：同文本命中、不同变量不串、并发首次编译、operationName 影响 key。
- S-08 Rundata / FieldResponse 池化后无请求间数据残留（`-race` + 顺序复用双验证）。

---

## 8. 自检结果（覆盖度与有效性）

### 8.1 覆盖度自检

| 规范章节 | 条目总数 | 已覆盖 | 未覆盖及原因 |
|---|---|---|---|
| §2 Language | 14 节 | 13 | §2.14 Schema Coordinates 是工具/文档语法，不参与执行与响应，且当前 parser 无对应 API → 记 **OUT-OF-SCOPE** |
| §3 Type System | 13 节 | 11 | §3.1 Type System Extensions、§3.4.3/3.6.3/… 各类 Extension 属 SDL 定义语法；本仓库用编程式 schema 构建，无 SDL 解析入口 → 记 **OUT-OF-SCOPE**（会在报告中明确写出，不隐藏） |
| §4 Introspection | 6 节 | 6 | — |
| §5 Validation | 32 条规则 | 32 | — |
| §6 Execution | 4 大节 15 小节 | 15 | §6.2.3.3 Unsubscribe 无公开 API → BOUNDARY |
| §7 Response | 8+2 小节 | 10 | — |

合计新增：**约 126 个顶层 Test，约 581 个子用例**（不含 sgraph 专项 30 个）。

### 8.2 有效性自检（我对方案做的反向质疑）

1. **"双读 resolver 是不是在帮 sgraph 作弊？"**
   不是，但**它确实掩盖了一个真实差异**。所以额外加了 §2.2 最后一条对照用例：不配置 registry 时断言 sgraph 拿不到父结果。这条对照用例会明确产出一个"sgraph 需要外部配置才能传递父结果"的结论，写进报告的"不兼容"一类，而不是被注入抹平。
2. **"根字段都配 resolver 是不是回避了 sgraph 的限制？"**
   是一种回避，因此单独保留一条用例：根字段不配 resolver（原生走默认 resolver 应成功）。该用例在 sgraph 下必然失败，明确归入"不兼容/设计边界"，不用注入绕过。
3. **"用 `toPlainValue` 归一化会不会丢掉顺序问题？"**
   会。因此 §7.1.4 / §7.2.2 的顺序断言**不经过** `toPlainValue`，直接对 `result.Data` 和整个 `result` 做 `json.Marshal` 后比对字节序。
4. **"错误按集合断言会不会漏掉'只报了一条'的缺陷？"**
   不会 —— 集合断言同时约束**数量**与**每条内容**，比按下标更严格（下标断言在顺序不定时反而只能放宽）。
5. **"两轮之间 plan cache / engine 全局缓存会不会串味？"**
   `defaultSGraphEngineCache` 按 `schema.typeMap` 指针缓存且无淘汰；每个 Test 各自新建 schema 实例，因此不会跨用例串。但**同一 schema 实例注册两个不同 engine 会报错**，语料 helper 里必须保证一个 schema 只注册一次 —— 已列为实现约束。
6. **"126 个 Test 会不会因为一个 panic 全丢？"**
   高风险用例（片段环、64 层嵌套、并发 race）走子进程隔离；其余按 `go test -json` 逐用例采集，单个 panic 只影响所在包的后续用例 —— 因此把高风险用例集中在**最后**执行并加 `-timeout 600s`。
7. **"`SGRAPH_USAGE_NOTES.md` 说错误会被覆盖，方案却写强断言，会不会断言错？"**
   该文档与当前代码不符（当前是 CAS 链表，`rundata.go:170-185`）。方案按**规范**写断言（同一 list 中多个元素报错必须全部保留），当前代码应当能通过；若不通过说明文档描述的旧缺陷仍在，属于真实发现。

### 8.3 已知会同时失败的项（预判，不降低断言）

Sept2025 新增能力在当前 schema 模型中无法表达，预计两轮同时失败并归入"原框架问题"：`__Schema.description`、`__Type.specifiedByURL`、`__Type.isOneOf`、`__Type.inputFields(includeDeprecated:)`、`__Field.args(includeDeprecated:)`、`__InputValue.isDeprecated/deprecationReason`、`__Directive.isRepeatable/args(includeDeprecated:)`、内置 `@specifiedBy`/`@oneOf`、`@deprecated` 在 argument/input field 上的位置、OneOf 输入强制、repeatable directive。

---

## 9. 执行与报告方式

### 9.1 命令

```bash
# 第一轮（原生）
GOCACHE=/tmp/gc go test -vet=off -count=1 -timeout 900s -json -run '^TestSpec2025' . > native.jsonl
GOCACHE=/tmp/gc go test -vet=off -count=1 -race -run '^TestSpec2025_Boundary_Concurrent' . 

# 第二轮（sgraph）：同样两条命令 → sgraph.jsonl
```

两轮各产出逐用例结果，再机械比对。

### 9.2 四类结论的判定规则（对应你的四个问题）

设每个用例（含子用例）在两轮的状态 ∈ {PASS, FAIL, BOUNDARY, UNREPRESENTABLE}：

| 类别 | 判定 |
|---|---|
| **① 原框架的问题** | native = FAIL（无论 sgraph 如何）。再按 sgraph 结果细分为"共有"与"已修复" |
| **② 新框架与原框架不兼容** | native ≠ sgraph 的**全部**用例，按"是否违反规范"再分两档：违反规范的进 ④，不违反规范的（如响应保序、错误文案、`Source` 语义）留在 ② 作为兼容性差异 |
| **③ 原框架有但新框架修复** | native = FAIL 且 sgraph = PASS |
| **④ 新框架引入的问题** | native = PASS 且 sgraph = FAIL |

另外单列：两轮同为 BOUNDARY/UNREPRESENTABLE 的项、sgraph 专项结果、`-race` 结果、性能观察（若并发用例暴露串行化）。

### 9.3 报告产出

- `SPEC2025_NATIVE_REPORT.md`（第一轮）
- `SPEC2025_SGRAPH_REPORT.md`（第二轮）
- `SPEC2025_COMPARISON_REPORT.md`（四分类对比 + 逐用例状态表 + 优先级建议）

---

## 10. 需要你确认的事项

1. **是否授权我修改代码并新增测试文件**（改动清单见 §2.4 与 §3；除 `executor.go` 1 行外，其余均为新增 `spec2025_*_test.go`）。
2. **前置修复是否授权**：3 个现有测试文件的 `sgraph` import 残留必须清理，否则根包测试无法编译。这是逻辑无关的符号迁移收尾。是否同意由我修复？
3. **`_stage_sgraph_src.tgz`**（我在你项目根目录留下的同步临时文件）是否授权删除。
4. **测试规模**：约 126 个顶层 Test / 581 个子用例，是否接受；若希望缩减，请指明优先保留的章节。
5. **`p.Source` 注入方案**（§2.2）是否认可为"必要的参数依赖设置与 resolver 入参调整"，以及其中三条对照用例（不注入时的真实差异、根字段无 resolver、默认 resolver）是否保留。
