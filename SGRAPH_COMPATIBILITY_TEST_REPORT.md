> **状态：已被取代（历史记录，保留原文）**
>
> 本文件记录的是早期一轮 SGraph 与 graphql-go 原生链路的对比结果。此后代码已发生变化，
> 其中多条结论**与当前代码不符**。当前有效的对比结论请看 `SPEC2025_COMPARISON_REPORT.md`
> 与 `SPEC2025_CASE_MATRIX.csv`。
>
> 已失效的结论如下（依据最近一轮 526 个叶子用例的双链路实测）：
>
> | 本文原结论 | 当前实际 |
> |---|---|
> | request error 暴露 typed-nil `Data` | **已修复**。`toGraphQLResult` 显式判空后才赋值，request error 的 `Result.Data == nil` |
> | 抽象类型列表 `__typename` 绑定失败（`parent key field name is empty`） | **已修复**。interface / union 列表的 `__typename` 与内联片段分发全部通过 |
> | list item 错误 path 缺少下标 | **已修复**。`FieldError.responsePath` 是 `[]any`，产出 `["people", 1, "failForBob"]` |
> | typed pointer list（`*[]string`）不被识别 | **已修复**。`asListValue` 会解引用一层 |
> | resolver error 的 extensions 丢失 | **已修复**。`storeFieldError` 通过 `OriginalError` 保留 `ExtendedError.Extensions()` |
> | execution error 缺少 field location | **已修复**。错误发布前用 `FieldASTsToNodeASTs` 补齐 AST 位置 |
> | 小 batch 被强制串行，阈值 `sGraphConcurrentStepMin = 8` | **不存在**。该常量在仓库中查无此物；`BatchPlan.execute` 的并发条件是 `b.concurrent && len(b.steps) > 1`，且 query 的每个 batch 的 `concurrent` 恒为 true。实测同 batch 内父子 step 真并发 |
>
> 仍然成立的结论：`Source == nil` 的设计边界、根字段必须有 resolver、subscription 不支持、
> mutation 回退原生链路。
>
> 此外，本文提到的两个 sgraph 专项测试文件在当前仓库中不存在。

---

# SGraph 与 graphql-go 原生执行链路兼容性报告

## 1. 测试方式

- 规范目标：[GraphQL September 2025](https://spec.graphql.org/September2025/)。
- 原生基线：`parse -> validate -> ExecuteGraphQLGo`。
- SGraph 链路：`parse -> validate -> Execute -> SGraphEngine`。
- mutation 仍由公开 `Execute` 转交 `ExecuteGraphQLGo`；subscription 按当前能力边界返回不支持错误。
- 两轮执行使用同一套查询、变量、schema 和断言。SGraph 轮只把测试入口切换到公开 `Execute`，并在 map 断言处把 `SGraphResponseOrderedMap` 转为普通 map；JSON 字段顺序测试仍直接序列化原始结果，没有降低断言。

根包命令：

```bash
GOCACHE=/tmp/graphql-go-build-cache go test -vet=off -count=1 .
```

并发检查：

```bash
GOCACHE=/tmp/graphql-go-race-cache go test -vet=off -race -count=1 \
  -run '^TestGraphQLGoNative_(ConcurrentRequestsDoNotLeakVariables|QueryRootFieldsCanExecuteConcurrently)$' .
```

## 2. 总体结果

| 分组 | graphql-go 通过 | SGraph 通过 | graphql-go 失败 | SGraph 失败 | 跳过 |
|---|---:|---:|---:|---:|---:|
| `GraphQLGoNative` | 18 | 12 | 2 | 8 | 0 |
| `GraphQLGoStrict` | 1 | 3 | 10 | 8 | 0 |
| `GraphQLGoSpec` | 23 | 16 | 0 | 7 | 4 |
| **合计** | **42** | **31** | **12** | **23** | **4** |

状态变化：

- 13 个顶层用例由原生通过变为 SGraph 失败。
- 2 个顶层用例由原生失败变为 SGraph 通过。
- `explicit_null_bypasses_defaults` 子用例也由原生失败变为 SGraph 通过。
- 原生已有的 12 个顶层规范失败没有因切换执行器而消失；其中部分发生在共享的 parse/validate 阶段。

## 3. SGraph 新增兼容性不足

### 3.1 确认的实现问题

| 问题 | 失败场景 | 实际结果 | 影响 |
|---|---|---|---|
| request error 暴露 typed-nil data | required variable 缺失/null、未知 input field、non-null list item 为 null、非法 enum | `Result.Data` 是 `(*SGraphResponseOrderedMap)(nil)`，接口本身不为 nil | 调用方误判为已产生 execution data；变量错误与嵌套 input coercion 用例失败 |
| 抽象类型列表的 `__typename` 绑定依赖不存在的 parent key | interface/union 列表，包含显式 `ResolveType` 和 `IsTypeOf` fallback | `parent key field name is empty for field __typename` | interface/union 的合法列表查询无法组装 |
| list item 错误 path 缺少下标 | `[T!]` 中第 2 个元素为 null | path 为 `field`，期望 `field, 1` | 客户端无法定位出错元素；不满足 list completion 的路径语义 |
| typed pointer list 不被识别 | resolver 返回 `*[]string` | `list value is not a list` | 与原生 executor 的 Go 返回值兼容性不足；typed slice 和 array 已通过 |
| resolver error 的 extensions 丢失 | resolver 返回实现扩展错误接口的错误 | alias path 正确，但 `extensions.code` 为空 | 错误元数据不能传给客户端 |
| execution error 缺少 field location | 普通 resolver/completion 错误 | `Locations` 为空 | 与原生响应错误形状不一致，客户端无法定位 query 字段 |
| 小 batch 被强制串行 | 两个独立 query 根字段位于同一 batch | 2 个 step 不并发，测试等待同时启动后超时 | 不违反 query 结果语义，但不符合“同 batch 允许并发”的引擎目标，并会使有协作等待的 resolver 卡住 |

小 batch 串行的直接原因是 `plan.go` 只在 `b.concurrent && len(b.steps) >= sGraphConcurrentStepMin` 时并发，而当前阈值是 8；测试只有两个互不依赖的根 step。

### 3.2 已确认的设计边界

这些场景相对 graphql-go 不兼容，但符合当前已确认的 SGraph 设计，不应误归因为本轮新增实现 bug：

| 设计边界 | 受影响用例 | 说明 |
|---|---|---|
| 显式 resolver 不兼容 graphql-go `Source` | field collection、list sibling error、`ResolveInfo`、execution error location 等 | SGraph 调用 resolver 时显式传入 nil source；读取 `p.Source` 的 resolver 会返回 nil 或 panic |
| 根字段不允许无 resolver | `DefaultResolverBranches` | SGraph 构建 plan 时返回 `field 1 has no resolver function`；嵌套无 resolver 字段从父结果组装的独立用例仍通过 |
| subscription 暂不支持 | `SubscriptionMapsEachEventThroughExecutor` | 公开 `Execute` 返回 `subscription is not supported yet`；这是当前明确能力边界 |
| mutation 不由 SGraph 执行 | mutation 串行用例仍通过 | 公开 `Execute` 按当前方案回退原生 executor，不代表 SGraph batch 链路已覆盖 mutation |

由 `Source == nil` 直接导致或遮挡的顶层失败包括：

- `TestGraphQLGoSpec_FieldCollectionFragmentsDirectivesAndAliases`
- `TestGraphQLGoSpec_ListItemErrorPathKeepsSiblingData`
- `TestGraphQLGoSpec_ResponseParseErrorShapeAndExecutionErrorLocations`
- `TestGraphQLGoNative_ResolveInfoContainsExecutionMetadata`

## 4. SGraph 相对原生的改善

| 改善项 | 对比结果 |
|---|---|
| 非法 leaf 序列化产生 field error | `TestGraphQLGoStrict_InvalidLeafSerializationProducesFieldError`：原生失败，SGraph 通过 |
| response JSON 保留 query 字段顺序 | `TestGraphQLGoStrict_ResponseSerializationPreservesQueryFieldOrder`：原生失败，SGraph 通过 |
| 显式 null 不再错误套用变量默认值 | `explicit_null_bypasses_defaults`：原生失败，SGraph 通过 |

并发请求隔离在 SGraph 下通过 race 检查，没有发现变量或请求数据串扰。原生 executor 的并行根字段测试虽然语义断言通过，但 race detector 报告 `extensions.go` 对共享 execution context 的并发读写；SGraph 没有复现该竞态，不过当前小 batch 串行意味着它没有实际并发执行这两个字段。

## 5. 两条链路共有的问题

以下不是 SGraph 新增回归，原生基线同样失败：

- parser 不接受合法 `null` literal。
- 数字变量被错误接受为 String。
- 缺少 non-repeatable directive 重复使用校验。
- subscription 单根字段规则未验证。
- request error JSON 仍包含 `"data": null`。
- September 2025 introspection 字段不完整。
- 不支持 executable definition description。
- specified directives 及其 locations 不完整。
- 不支持 variable definition directives。

4 个高风险场景继续保持 `Skip`，没有伪造通过：嵌套 non-null panic、null literal parser、fragment cycle stack overflow、原生 response field order 缺口。

## 6. 结论与优先级

若目标是兼容 graphql-go 的 query 执行结果，同时保留 SGraph 的依赖编排设计，建议修复顺序为：

1. **P0**：抽象类型列表 `__typename` 绑定；这是合法 interface/union 查询的执行阻断。
2. **P0**：request error 的 typed-nil `Data`；这是公开 Result API 的错误状态表达。
3. **P1**：list error path 加入元素下标，并补齐 execution error location。
4. **P1**：保留 resolver error extensions。
5. **P1**：typed pointer list completion。
6. **P1**：重新评估小 batch 串行阈值；它与当前 batch 并发设计目标冲突，也会影响查询折叠收益。

`Source`、根字段默认 resolver、subscription 和 mutation 路由属于产品能力决策。只有决定扩大兼容边界时才应修改，不能为让测试通过而隐式改变现有设计。

## 7. 仓库级工具链状态

- `language/...`、`gqlerrors`、`testutil` 在 SGraph 切换后全部通过。
- 不加 `-vet=off` 时，现有 `fmt` 格式符问题仍会阻断根包测试。
- `go test -vet=off ./...` 仍会被 `examples/hello-world` 引用已删除的 `plan` 包阻断。
- 这两项与本次 SGraph 执行结果对比无关。
