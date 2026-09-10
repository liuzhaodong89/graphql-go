# 第一轮：graphql-go 原生执行链路 GraphQL September 2025 规范测试报告

规范基准：https://spec.graphql.org/September2025/
执行链路：`Do → parse → validate → Execute → ExecuteGraphQLGo`
代码改动：`executor.go:32` 由 `return executeSGraph(p)` 改为 `return ExecuteGraphQLGo(p)`（1 行）。

> **历史快照说明**：本报告的 410/116 统计产生于原生 `executeSubFields` 并发字段执行时期。当前实现已改为串行循环：定向复核确认 non-null 冒泡不再跨 goroutine 终止进程，旧版 `extensions.go` 共享 executionContext 竞态也不再触发。由于串行化会改变根字段调度，本报告保留当时完整跑数，不把局部复核结果拼成新的总通过数。

---

## 1. 测试资产

新增 9 个测试文件，全部位于 `package graphql`：

| 文件 | 覆盖 | 顶层 Test |
|---|---|---:|
| `spec2025_corpus_test.go` | 执行入口、断言 helper、参数注入 | — |
| `spec2025_schema_test.go` | 全部 schema 构造器 | — |
| `spec2025_language_test.go` | §2 Language | 13 |
| `spec2025_typesystem_test.go` | §3 Type System | 12 |
| `spec2025_introspection_test.go` | §4 Introspection | 7 |
| `spec2025_validation_test.go` | §5 Validation（32 条规则） | 13 |
| `spec2025_execution_test.go` | §6 Execution | 15 |
| `spec2025_response_test.go` | §7 Response | 7 |
| `spec2025_cross_test.go` | 场景交叉矩阵 | 9 |
| `spec2025_boundary_test.go` | 边界值与极限值 | 12 |
| `spec2025_sgraph_ext_test.go` | SGraph 专项（不进对比基线） | 7 |

对比基线共 **93 个顶层 Test / 526 个叶子用例**。

## 2. 执行方式

每个顶层 Test 单独启动一次 `go test`，避免任一用例的进程级崩溃吞掉后续结果：

```bash
for name in $(grep -ho '^func \(TestSpec2025_[A-Za-z0-9_]*\)' spec2025_*_test.go | sed 's/^func //'); do
  go test -vet=off -count=1 -timeout 240s -json -run "^${name}\$" .
done
```

已知会使进程崩溃的 4 个用例用子进程隔离（`s25Isolated`），子进程失败仍按真实失败上报，**不转为 skip**：

- `TestSpec2025_Language_Fragments/fragment_cycles_are_invalid`
- `TestSpec2025_Validation_FragmentCyclesAreRejected`
- `TestSpec2025_Execution_NullBubbling`、`TestSpec2025_Execution_ErrorHandling`
- `TestSpec2025_Response_DataEntry`、`TestSpec2025_TypeSystem_ListAndNonNullCombinations`

## 3. 结果总览

| 章节 | 用例数 | 通过 | 失败 |
|---|---:|---:|---:|
| §2 Language | 130 | 109 | 21 |
| §3 Type System | 88 | 60 | 28 |
| §4 Introspection | 31 | 23 | 8 |
| §5 Validation | 43 | 28 | 15 |
| §6 Execution | 77 | 55 | 22 |
| §7 Response | 27 | 15 | 12 |
| 场景交叉 | 60 | 58 | 2 |
| 边界与极限 | 70 | 62 | 8 |
| **合计** | **526** | **410** | **116** |

通过率 78.0%。

## 4. 失败归因

### 4.1 P0 — 会导致进程崩溃

| 问题 | 证据 |
|---|---|
| **non-null 冒泡跨 goroutine panic 终止进程（历史行为，当前已消失）** | 本轮快照中 `executeSubFields` 为每个字段启动 goroutine，NonNull panic 可能逃逸并终止进程。当前实现改为串行循环后，panic 沿同一调用栈被 `resolveField` 的 recover 转为字段错误。 |
| **fragment 环导致栈溢出** | `ValidateDocument` 在 `A → B → A` 上无限递归，`overlappingFieldsCanBeMergedRule.findConflict` 栈溢出。规范 §5.5.2.2 要求校验期拒绝，实际是进程死亡。 |

### 4.2 P0 — 并发数据竞争（历史行为，当前串行实现不再触发）

`go test -race -run 'TestSpec2025_(Boundary_ConcurrentRequestsDoNotLeakVariables|Execution_QueryRootFieldsMayExecuteConcurrently)'` 报告 **DATA RACE**：

```
extensions.go:196  p.Context = ctx        // 写
extensions.go:197                          // 读
executor.go:746    handleExtensionsResolveFieldDidStart(eCtx.Schema.extensions, eCtx, &info)
executor.go:400    executeSubFields 的并发 goroutine
```

本轮快照中，`executeSubFields` 为每个 response name 起一个 goroutine，全部共享同一个 `*executionContext`，而 `handleExtensionsResolveFieldDidStart` 无条件写回 `p.Context`。当前实现已改为串行循环，写点仍在，但不再被多个字段 goroutine 并发访问；定向 `-race` 复核通过。

### 4.3 P1 — 输入强制转换过宽（规范 §3.5 / §6.1.2）

以下变量输入按规范必须产生 request error，实际全部被接受：

| 字段类型 | 传入值 | 实际行为 |
|---|---|---|
| `Int!` | `true` | 转成 `1` |
| `Int!` | `"5"` | 转成 `5` |
| `Int!` | `1.5` | 截断为 `1` |
| `Float` | `"1.5"` | 转成 `1.5` |
| `Boolean` | `1` / `0` | 转成 `true` / `false` |
| `String` | `3` | 转成 `"3"` |
| `ID` | `true` | 转成 `"true"` |

根因：`scalars.go` 的 `coerceInt`/`coerceFloat`/`coerceBool` 做宽松转换，`coerceString` 用 `fmt.Sprintf("%v")` 兜底，从不返回错误。

### 4.4 P1 — 结果强制转换失败被静默吞掉（规范 §6.4.3）

`executor.go:971 completeLeafValue` 在 `Serialize` 返回 nullish 时**直接返回 nil，不写入任何 error**。受影响：

- `Int` 收到 `int64(3000000000)` → `null`，无 error
- `Enum` 收到未声明的内部值 → `null`，无 error
- 自定义 Scalar 的 `Serialize` 返回 nil → `null`，无 error
- `Float` 收到 `NaN` / `±Inf` / 字符串 → `null` 或原样输出，无 error
- `String` 收到 `42` / struct → 输出 `"42"`，无 error
- `Boolean` 收到 `1` / `"yes"` → 输出 `true`，无 error

客户端拿不到任何失败信号。

### 4.5 P1 — 显式 null 与"未提供"不可区分（规范 §6.1.2 / §6.4.1）

| 场景 | 规范 | 实际 |
|---|---|---|
| `$v: String` 运行时传 `null` | 参数为 null | 参数缺省（`<missing>`） |
| `$v: String = "d"` 运行时传 `null` | 参数为 null | 用变量默认值 `"d"` |
| input object 字段显式传 `null` | 保持 null | 用字段默认值 |
| 嵌套 input object 字段显式传 `null` | 保持 null | 用字段默认值 |

根因：`values.go:60` 用 `isNullish(tmp)` 判断后回落到 `argDef.DefaultValue`，无法区分"没给"和"给了 null"。

### 4.6 P1 — 响应字段顺序丢失（规范 §7.1.4 / §7.2.2）

`Result.Data` 是 `map[string]interface{}`，JSON 序列化按 Go map key 排序。查询 `{ z a m }` 输出 `{"a":..,"m":..,"z":..}`。根字段、嵌套字段、list 元素内字段、别名全部受影响（7 个用例）。

### 4.7 P1 — parser 缺失（规范 §2）

| 缺口 | 表现 |
|---|---|
| **`null` 字面量** | `Syntax Error ... Unexpected Name "null"`；合法的 `NullValue` 无法书写（5 个用例） |
| **可执行定义 description**（September 2025 新增） | operation / fragment / variable definition 前的 `"..."` 与 `"""..."""` 全部解析失败（8 个用例） |
| **VARIABLE_DEFINITION 指令位置** | `query Q($a: Int @tag)` 解析失败；`directives.go` 也没有该常量 |
| **`\u{1F600}` 花括号转义** | `Invalid character escape sequence` |
| **代理对 `😀`** | 解码成两个独立的替换字符，未合成 U+1F600 |
| **IntValue 后紧跟 NameStart** | `[1abc]` 被接受，规范要求词法错误 |

### 4.8 P1 — 校验规则缺失（规范 §5）

| 规则 | 表现 |
|---|---|
| 5.1.1 Executable Definitions | 文档中混入 `type Foo { a: Int }` 被接受 |
| 5.2.1.1 Operation Type Existence | query-only schema 上 `mutation M { ... }` 通过校验 |
| 5.2.4.1 Single Root Field | subscription 多根字段、别名重复根字段、`__typename` 唯一根字段全部通过 |
| 5.6.3 Input Object Field Uniqueness | `{text: "a", text: "b"}` 通过。根因：`rules.go:1495` 把 `ObjectField` handler 注册在 `Kind:` 键下而不是 `Enter:`，规则从未触发 |
| 5.7.3 Directives Are Unique per Location | 同一字段上两个 `@skip` 通过校验 |
| 变量非空 + 默认值 | `$text: String! = "hi"` 被**错误拒绝**（规范允许），错误文案是多年前已移除的规则 |

### 4.9 P1 — request error 响应仍带 `data` 键（规范 §7.1.3）

`types.go:11` 的 `Data interface{}` 带 `json:"data"` 但没有 `omitempty`，parse error / validation error / 未知 operationName / 变量协变失败四种 request error 的序列化结果都包含 `"data": null`。规范明确要求 **不得出现 data 条目**。

### 4.10 P2 — September 2025 内省能力缺失

当前 schema 模型无法表达，内省查询直接校验失败：

`__Schema.description`、`__Type.specifiedByURL`、`__Type.isOneOf`、`__Type.inputFields(includeDeprecated:)`、`__Field.args(includeDeprecated:)`、`__InputValue.isDeprecated`、`__InputValue.deprecationReason`、`__Directive.isRepeatable`、`__Directive.args(includeDeprecated:)`、`__Type.interfaces` 在 INTERFACE 上返回 null（interface 实现 interface 不可内省）。

内置指令 `@specifiedBy`、`@oneOf` 完全不存在；`@deprecated` 缺少 `ARGUMENT_DEFINITION` 和 `INPUT_FIELD_DEFINITION` 位置；OneOf 输入对象无法声明也无法校验（4 个用例）。

## 5. 通过的关键能力

以下规范条款在原生链路上完整通过，作为 sgraph 轮的对照基准：

- §2.1 忽略符（BOM / 空白 / 换行 / 注释 / 逗号）与紧凑写法字节一致
- §2.8 别名、§2.9 片段（命名 / 内联 / 无类型条件 / 三层嵌套 / 重复展开）
- §5.3.2 字段合并的全部正反例，含互斥类型下同名别名可合并
- §5.5.2.3.1–4 四类片段可能性、§5.8.x 全部变量规则
- §6.2.1 query 根字段真并发（仅适用于本轮历史快照；当前原生实现已改为串行）
- §6.2.2 mutation 根字段严格串行，且一个根字段的嵌套子字段全部完成后才执行下一个
- §6.3.2 字段收集去重、resolver 只调用一次、跨片段合并
- §6.4.2 默认 resolver 四分支（map / map 中的函数属性 / struct tag / `FieldResolver`）
- §6.4.3 抽象类型 `ResolveType` 与 `IsTypeOf` 回退、list 各种 Go 载体（`[]string` / `[3]int` / `*[]string` / typed nil）
- §6.4.4 错误 path 含 list 下标与别名、兄弟字段数据保留、`extensions` 透传
- 边界：Int/Float/String/ID/Enum 合法边界值、10240 元素列表、4 层嵌套列表、64 层选择集深度、512 个兄弟字段、256 个变量、8 层嵌套 input object、128 个片段、约 250 KiB 单文档、128 并发请求

## 6. 仓库级工具链问题（与本轮路由切换无关）

1. `plan.go` / `plan_compiler.go` / `plan_coordinator.go` 共 11 处 `fmt` 格式符类型错误（`%s` 用于 `uint32` / `ParamTypeEnum`），不加 `-vet=off` 会阻断 `go test`。
2. ~~`util4sgraph.go` 的 unreachable code~~ —— **已修复**。
3. `graphqlgo_native_matrix_test.go`、`sgraph_occurrence_error_test.go`、`sgraph_query_folding_test.go` 原本 `import "github.com/graphql-go/graphql/sgraph"`，但 `sgraph/` 目录已删除，根包测试无法编译。本轮已做纯符号迁移收尾（删除 import、去掉 `sgraph.` 前缀），未改动任何逻辑。
4. `benchmark_sgraph_query_folding_test.go` 同样引用 `sgraph` 包，本轮已做同样的符号迁移收尾（删除 import、`sgraph.toPlainValue` → `toPlainValue`，共 3 行），未改动任何 benchmark 逻辑。修复后 `go build ./...` 与 `go test ./...` 全部通过。
