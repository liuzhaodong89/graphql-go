> **状态：已被取代（历史记录，保留原文）**
>
> 本文件记录的是早期一轮 graphql-go 原生链路的规范测试结果。此后测试资产已重建，
> 当前有效的原生链路结论请看 `SPEC2025_NATIVE_REPORT.md`。
>
> 与当前代码不符之处：
>
> | 本文原结论 | 当前实际 |
> |---|---|
> | `graphqlgo_native_matrix_test.go` 新增 20 个 `GraphQLGoNative` + 11 个 `GraphQLGoStrict` | 当前是 **32** 个 `GraphQLGoNative` + 11 个 `GraphQLGoStrict` |
> | 原有测试保留 4 个 `Skip` | 仓库中**没有任何 `t.Skip`**；高风险用例改为子进程隔离执行，失败仍按真实失败上报 |
> | `go test -vet=off ./...` 会被 `examples/hello-world/main.go` 引用已删除的 `plan` 包阻断 | 已不再阻断，`go build ./...` 与 `go test ./...` 均可通过 |
>
> 仍然成立的结论：11 处 `fmt` 格式符类型错误导致必须加 `-vet=off`；
> `extensions.go` 对共享 `executionContext.Context` 的并发读写在 `-race` 下复现。

---

# graphql-go 原生执行链路测试报告

## 1. 测试目标

- 公开 `Do -> Execute` 恢复为 graphql-go 原生执行链路。
- 规范断言以 [GraphQL September 2025](https://spec.graphql.org/September2025/) 为准，不按当前实现缺陷降低预期。
- 复用原有 conformance schema，并补充原生 executor、严格规范、组合、边界、并发和 subscription 场景。

## 2. 本次代码

- `executor.go`：公开 `Execute` 直接调用 `ExecuteGraphQLGo`；SGraph engine 不参与本轮执行基线。
- `graphql.go`：保留原 parse、validate 流程，通过公开 `Execute` 进入原生 executor。
- `graphqlgo_native_matrix_test.go`：新增 20 个 `GraphQLGoNative` 顶层测试和 11 个 `GraphQLGoStrict` 顶层测试。

新增矩阵覆盖：公开入口、directive 真值表、重复 response name、参数/变量默认值、变量输入强制转换、operation 选择、field collection 去重、默认 resolver、query 并发、context、`ResolveInfo`、自定义 scalar、subscription、错误 path/extensions、partial data、typed list、非法 list、宽查询/大列表、并发请求、schema 构建、strict validation、leaf completion、响应 JSON、可执行定义 description、specified directives 和 September 2025 introspection。

## 3. 执行结果

### 根包完整矩阵

命令：

```bash
GOCACHE=/tmp/graphql-go-build-cache go test -vet=off -count=1 .
```

| 分组 | 通过 | 失败 | 跳过 | 合计 |
|---|---:|---:|---:|---:|
| 新增 `GraphQLGoNative` | 18 | 2 | 0 | 20 |
| 新增 `GraphQLGoStrict` | 1 | 10 | 0 | 11 |
| 原有 `GraphQLGoSpec` | 23 | 0 | 4 | 27 |
| **根包合计** | **42** | **12** | **4** | **58** |

### 语言及辅助包

```bash
GOCACHE=/tmp/graphql-go-build-cache go test -vet=off -count=1 ./language/...
GOCACHE=/tmp/graphql-go-build-cache go test -vet=off -count=1 ./gqlerrors ./testutil
```

结果：全部通过。lexer、parser、printer、visitor 和 testutil 未因 executor 切换产生回归。

### Race 检查

```bash
GOCACHE=/tmp/graphql-go-race-cache go test -vet=off -race -count=1 \
  -run '^TestGraphQLGoNative_(ConcurrentRequestsDoNotLeakVariables|QueryRootFieldsCanExecuteConcurrently)$' .
```

- 64 个并发独立请求通过，没有变量串请求。
- 同一 query 的并发根字段失败：`extensions.go:196-197` 对共享 `executionContext.Context` 并发读写，即使 schema 没有 extension 也会写回 context。

## 4. 规范失败归因

| 问题 | 实际行为 | 影响 |
|---|---|---|
| `null` literal | parser 报 `Unexpected Name "null"` | 合法 nullable literal 无法执行 |
| 显式 null 与变量默认值 | runtime 显式传 `null` 后仍使用变量默认值 | 无法区分 omitted 和 explicit null |
| String 输入强制转换过宽 | 数字 `3` 被接受并转换成字符串 | 非法变量未产生 request error |
| directive 唯一性 | 同一位置重复 `@skip` 通过验证 | 缺少 non-repeatable directive 校验 |
| subscription 单根字段规则 | 多根字段、别名重复根字段、`__typename` 根字段均通过验证 | 不满足 Single Root Field |
| leaf result coercion | scalar/enum 序列化失败只返回 null，不记录 field error | 丢失错误及 path |
| request error JSON | 输出 `"data": null` | request error result 应省略 `data` |
| response field 顺序 | JSON 按 map key 输出 `a,m,z` | 未保留 query 中 `z,a,m` 的顺序 |
| September 2025 introspection | 缺少 `__Schema.description`、`specifiedByURL`、`isOneOf`、`isRepeatable`、input value deprecation | 新版内省查询验证失败 |
| 可执行定义 description | variable definition 和 fragment definition 前的 description 均解析失败 | 不支持 September 2025 可执行文档语法 |
| specified directives | 缺少 `specifiedBy/oneOf`，且 `deprecated` 缺少 argument/input field locations | 指令集合和内省结果不完整 |
| variable definition directive | variable definition 上出现 directive 时 parser 报错 | 不支持 `VARIABLE_DEFINITION` 指令位置 |
| query field context 竞态 | 并发字段共享写 `executionContext.Context` | race detector 失败，存在并发安全风险 |

## 5. 已隔离的高风险缺口

原有测试继续保留 4 个 `Skip`，没有伪造通过：

- 嵌套 non-null 错误冒泡可能跨 goroutine panic。
- `null` literal parser 缺口。
- fragment cycle 可能在字段合并验证时无限递归并 stack overflow。
- response field order 缺口。

## 6. 仓库级工具链问题

- 不加 `-vet=off` 时，`plan.go`、`plan_compiler.go`、`plan_coordinator.go` 共 11 处 `fmt` 格式符类型错误会阻断 `go test`。
- `go test -vet=off ./...` 还会被 `examples/hello-world/main.go` 引用已不存在的 `github.com/graphql-go/graphql/plan` 包阻断。
- 上述问题与本次 executor 路由切换无关，本次未修改对应业务代码。
