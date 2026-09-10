# GraphQL Conformance Matrix

> 本文件描述一致性测试组织方式，并保留最近一次 **526 用例双链路完整跑数快照**。
> 该快照生成后，原生 `executeSubFields` 已改为串行执行；因此下方总数、四象限与 `SPEC2025_CASE_MATRIX.csv` 是历史快照，不应直接当作当前代码的完整重跑结果。已确认受影响的并发与 non-null 结论在对应位置单独校正。

## Scope

- 规范基准：https://spec.graphql.org/September2025/
- 原生基线：`Do → parse → validate → Execute → ExecuteGraphQLGo`
- sgraph：`Do → parse → validate → Execute → executeSGraph → SGraphEngine`
- 两条链路的切换点是 `executor.go` 中 `Execute` 的**一行**：`ExecuteGraphQLGo(p)` ↔ `executeSGraph(p)`。
- `sgraph/` 目录已不存在，其内容已迁入根包（`plan*.go`、`rundata.go`、`result_assembler.go`、`sgraph_engine.go`、`param_registry.go`、`directive_registry.go`、`util4sgraph.go`）。
- mutation 由 `executeSGraph` 回退到 `ExecuteGraphQLGo`；subscription 在 SGraph 链路返回 `subscription is not supported yet`。

## 测试文件

### September 2025 规范语料（两条链路共用，对比基线）

| 文件 | 覆盖 | 顶层 Test |
|---|---|---:|
| `spec2025_corpus_test.go` | 执行入口、断言 helper、父结果注入 | — |
| `spec2025_schema_test.go` | 全部 schema 构造器 | — |
| `spec2025_language_test.go` | §2 Language | 13 |
| `spec2025_typesystem_test.go` | §3 Type System | 12 |
| `spec2025_introspection_test.go` | §4 Introspection | 7 |
| `spec2025_validation_test.go` | §5 Validation（32 条规则正反用例） | 13 |
| `spec2025_execution_test.go` | §6 Execution | 15 |
| `spec2025_response_test.go` | §7 Response | 7 |
| `spec2025_cross_test.go` | 场景交叉矩阵 | 9 |
| `spec2025_boundary_test.go` | 边界值与极限值 | 12 |

合计 **93 个顶层 Test / 526 个叶子用例**，两条链路各跑一遍，用例源码逐字节相同。

### SGraph 专项（只在 SGraph 链路有意义，不进对比基线）

| 文件 | 覆盖 | 顶层 Test |
|---|---|---:|
| `spec2025_sgraph_ext_test.go` | 批次拓扑、同 batch 并发、参数物化、非法依赖图、Plan cache、池化清理 | 7 |
| `sgraph_parent_key_test.go` | 父侧关联 key 推断规则、推断确定性、bulk 歧义报错、迭代回退 occurrence 路径、并发编译一致性 | 9 |
| `sgraph_query_folding_test.go` | 查询折叠、依赖编排、指令参数依赖、bulk 映射 | 12 |
| `sgraph_occurrence_error_test.go` | occurrence 路径、并发错误、tree 错误、池化状态 | 9 |

### 既有矩阵

| 文件 | 覆盖 | 顶层 Test |
|---|---|---:|
| `graphqlgo_spec_conformance_test.go` | 早期一致性基线 | 25 |
| `graphqlgo_native_matrix_test.go` | `TestGraphQLGoNative_` 行为矩阵 32 + `TestGraphQLGoStrict_` 严格规范 11 | 43 |

**注意：仓库中没有任何 `t.Skip`。** fragment 环栈溢出等进程级失败通过 `s25Isolated` 在子进程中隔离执行，子进程失败仍按真实失败上报。原生 non-null 冒泡曾因跨 goroutine 逃逸而需要隔离；当前字段串行执行后已能正常返回字段错误。

## Commands

不存在 `SGRAPH_SPEC_CONFORMANCE` 之类的环境变量开关；链路由 `executor.go` 的一行决定。仓库中唯一被读取的测试环境变量是 `GRAPHQLGO_CONFORMANCE_CHILD` 与 `SPEC2025_ISOLATED_CASE`，二者都只用于子进程隔离，不需要手工设置。

规范语料（当前 `executor.go` 路由决定跑的是哪条链路）：

```bash
GOCACHE=/tmp/gc go test -count=1 -timeout 900s -run '^TestSpec2025' .
```

逐用例独立进程（推荐：任一用例的进程级崩溃不会吞掉后续结果）：

```bash
for name in $(grep -ho '^func \(TestSpec2025_[A-Za-z0-9_]*\)' spec2025_*_test.go | sed 's/^func //'); do
  go test -count=1 -timeout 240s -json -run "^${name}\$" .
done
```

SGraph 专项：

```bash
go test -count=1 -run '^TestSGraph' .
```

并发隔离：

```bash
go test -race -count=1 \
  -run '^(TestSpec2025_Boundary_ConcurrentRequestsDoNotLeakVariables|TestSpec2025_Execution_QueryRootFieldsMayExecuteConcurrently|TestSGraphExt)' .
```

静态检查：

```bash
go vet ./...
```

> `go vet ./...` 当前无输出，`go test` 不再需要 `-vet=off`。

## 最近一次完整快照（526 个叶子用例）

> 以下数字产生于原生字段并发执行时期。当前原生链路已改为串行：定向复核确认原生 non-null 冒泡不再终止进程，`-race` 也不再报告共享 `executionContext` 竞态。由于串行化还会改变根字段调度语义，在完整双链路矩阵重跑前，不根据局部结果重算总通过数和四象限。

| | 原生 | sgraph |
|---|---:|---:|
| 通过 | 410 | 427 |
| 失败 | 116 | 99 |
| `-race` | **历史快照：FAIL**；当前定向复核 PASS | PASS |
| 进程崩溃用例 | 历史快照：包含 non-null 跨 goroutine 失败；当前该类已消失 | 0 |

| 象限 | 数量 |
|---|---:|
| 两轮均通过 | 396 |
| 两轮均失败（共有缺口） | 85 |
| 原生失败 → sgraph 通过 | 31 |
| 原生通过 → sgraph 失败 | 14 |

分章节明细、逐用例状态与四类归因见：

- `SPEC2025_NATIVE_REPORT.md` —— 原生链路结果与失败归因
- `SPEC2025_SGRAPH_REPORT.md` —— sgraph 链路结果与独有失败
- `SPEC2025_COMPARISON_REPORT.md` —— 四象限对比与修复优先级
- `SPEC2025_CASE_MATRIX.csv` —— 526 行逐用例状态

面向使用者与维护者的文档：

- `SGRAPH_USER_MANUAL.md` —— 调用方使用手册：两条链路差异、迁移改造清单、查询折叠 / 参数依赖 / bulk / 自定义指令的配置方法
- `SGRAPH_USAGE_NOTES.md` —— 维护约束：Engine 生命周期、并发边界、父子关联 key 推断契约、对象池规则

## 两条链路共有的缺口（85 条，摘要）

绝大部分位于**共用的 parse / validate 阶段**，或属于当前 schema 模型无法表达的能力：

- parse：`null` 字面量、可执行定义 description、`VARIABLE_DEFINITION` 指令位置、`\u{...}` 转义、代理对合成、IntValue 后紧跟 NameStart
- validate：Executable Definitions、Operation Type Existence、Single Root Field、fragment 环（**栈溢出**而非报错）、Input Object Field Uniqueness（`rules.go` 把 `ObjectField` handler 注册在 `Kind:` 而非 `Enter:`，规则从未触发）、Directives Are Unique per Location、非空变量带默认值被错误拒绝
- 输入强制转换过宽：`Int!` 接受 `true`/`"5"`/`1.5`；`Float` 接受 `"1.5"`；`Boolean` 接受 `0`/`1`；`String` 接受 `3`；`ID` 接受 `true`
- 结果强制转换过宽：`String`/`Boolean`/`Float` 对非法内部值不报错
- request error 响应仍含 `data` 键（`types.go` 的 `Data` 无 `omitempty`）
- September 2025 内省与类型系统新增能力缺失：`__Schema.description`、`__Type.specifiedByURL`、`__Type.isOneOf`、`includeDeprecated` 系列参数、`__InputValue` 的 deprecation 字段、`__Directive.isRepeatable`、interface 实现 interface 的内省、内置 `@specifiedBy` / `@oneOf`、`@deprecated` 的 argument / input field 位置、OneOf 输入对象

## sgraph 相对原生的改善（历史快照 31 条）

| 类别 | 条数 | 依据 |
|---|---:|---|
| 叶子结果强制转换失败产生 field error | 8 | `result_assembler.go` `serializeLeafValue` 在序列化结果为 nullish 时写入错误；原生 `completeLeafValue` 静默返回 nil |
| 响应字段严格保序 | 9 | `SGraphResponseOrderedMap` + 自定义 `MarshalJSON`，满足 §7.1.4 / §7.2.2 |
| 显式 null 与"未提供"可区分 | 6 | `completeOperationVariables` / `parseInputValue` 显式区分 `provided` 与 `value == nil` |
| non-null 冒泡不终止进程 | 3 | **仅适用于历史快照**。当前原生字段串行执行后同样会返回字段错误，这 3 条不再构成 SGraph 相对改善 |
| 抽象类型列表 `__typename`、typed pointer list、error extensions、error locations | 5 | 均为早期报告记录的 sgraph 缺陷，现已修复 |

> 另有一项不改变用例状态的一致性修复：`result_assembler.go` 的 `rejectDeferredFunctionProperty` 在 `extractFieldResponse` 的 4 个取值出口按 `reflect.Kind() == Func` 拦截函数值，写字段错误并返回 `null`。依据 §6.4.3 `CoerceResult`——结果强制转换必须产出该类型的有效值，否则必须抛执行错误；此前把函数指针序列化成 `"0x…"` 当作 `String` 返回违反该条。§6.4.2 把取值方式留给实现，因此"不执行惰性属性"本身不违规。

并发方面：历史竞态的直接写点在 `extensions.go`，根因是旧版 `executeSubFields` 并发执行字段并共享 `executionContext`。当前原生字段串行执行后，两条链路定向 `-race` 复核均通过；SGraph 仍保持同 Batch Step 并发，并使用请求私有 Rundata 与显式 ctx 传递保护自身链路。

## sgraph 独有失败（14 条）

| 严重度 | 问题 | 条数 |
|---|---|---:|
| P0 | 根选择集被字面量 `@skip`/`@include` 全部裁剪时返回 request error，而非 `{"data":{}}`（`plan_coordinator.go` 的 `len(roots)==0` 分支） | 11 |
| 行为差异 | 结果组装不执行 map 中的函数属性（`result_assembler.go`）。**静默部分已修复**：命中函数值时该字段返回 `null` 并写入一条 field error，不再输出 `"0x…"` 指针字符串。用例断言的是"函数被调用"，因此仍计为失败 | 2 |
| 能力边界 | subscription 不支持 | 1 |

> 空列表在冷池下被补全为 `null` 的非确定性缺陷（`acquireFieldResponse` 的 `responseRaws` nil 性）**已修复**：`acquireFieldResponse` 与 `acquireBulkFieldResponseState` 现在保证 `responseRaws` / `iterationResponses` 非 nil。修复后逐用例独立进程与单进程全量运行的结果差异为 0。

> **用例矩阵之外**另有一条非确定性缺陷已修复：父侧关联 key（`checkAndCompileParentKeyFieldNames`）原本在父类型声明多个 `ID` 字段时按 map 遍历顺序任选一个，推断结果随 Plan 进入缓存，因而进程内稳定、跨进程启动才变化。现改为候选唯一才推断；歧义时普通迭代回退 occurrence 路径绑定，bulk 在编译期报错并列出候选字段名。本仓库语料中没有声明 ≥2 个 `ID` 字段的 list 元素类型，四象限计数不受影响。配套用例见 `sgraph_parent_key_test.go`，完整分析见 `SPEC2025_SGRAPH_REPORT.md` §7。

## 已确认的设计边界（不计为缺陷）

- 业务 resolver 的 `Source` 恒为 nil（`plan.go` 的 `execResolveProcess` 普通字段分支），父结果需通过 `ParamRegistry` 的 `FIELD_RESPONSE` 参数注入。
- 文档中出现的自定义指令必须先登记到 `DirectiveRegistry`（有语义的注册 compiler / handler，纯标注型用 `RegisterMetadataOnly`），否则 Plan 编译阶段整体失败。`SchemaConfig.Directives` 与 `DirectiveRegistry` 是两份独立配置，框架不做对账。
- 带参数的字段必须配置 resolver（内省字段豁免）。
- 第一条请求必须晚于 `RegisterSGraphEngine`：Engine 与 Schema 的绑定按设计一次性、进程内不可变，未注册时兜底的默认 Engine 会被写入全局缓存且无解绑接口。详见 `SGRAPH_USAGE_NOTES.md` 的接入检查清单。
- ParamRegistry 的 `FIELD_RESPONSE` 来源字段必须有显式 resolver；内部按需物化的无 resolver 中间字段可以生成内部 Step，但不能作为外部 ParamRegistry producer。
- list 元素类型上的 `ID` 字段会启用业务 key 绑定，影响该父类型下的**全部**子字段（不限 bulk）：父 resolver 必须在返回值中带上该字段，且值逐父元素唯一，否则报 `parent key field %q is missing` 或 `duplicate parent binding key %q`。父类型不含 `ID` 字段时走 occurrence 路径绑定，不依赖任何业务字段。多个 `ID` 字段又没有 `id` 时 bulk 编译期报歧义错误。
- 根字段必须有 resolver（`plan_coordinator.go`）。
- mutation 不进入 SGraph batch 链路；subscription 不支持。
- `Field` 公开配置没有 `BulkResolve` / `BulkArgs` / `BulkResultMappedFieldName`，bulk 只能在 schema 构建后改内部 `FieldDefinition`。

## Coverage Boundary

本矩阵覆盖当前**可执行 API 表面**，不是规范每一句规范性语句的形式化证明。以下记为不适用：

- §2.14 Schema Coordinates、§3 各类 Type System Extension —— 属于 SDL 定义语法，本仓库使用编程式 schema 构建，无对应入口
- §6.2.3.3 Unsubscribe —— 无公开 API
- bulk resolver 的完整绑定错误路径 —— 无公开配置入口，仅在 SGraph 专项中覆盖编排与参数物化
