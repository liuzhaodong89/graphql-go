# 两条执行链路性能对比报告

## 1. 报告信息

- 基准代码：`benchmark_chain_compare_test.go`（本次新增，独立于仓库既有 benchmark）
- 被测代码：`bb75dea alpha1.1` + 工作副本对 `executor.go` 的串行化改动
- 平台：darwin/arm64，`GOMAXPROCS=10`，Go 1.26.2
- 采样：主矩阵 23 用例 × `-benchtime 300ms -count 5` 取中位数；并发组 5 用例 × `-count 3`
- 单个延迟 resolver 耗时：2ms

## 2. 基线说明：本轮已把原生链路改回串行

本报告第一版曾发现，本仓库的 `executeSubFields` 被改造成「每字段一个 goroutine + 每对象一个分片 concurrent map」，与上游 graphql-go 不同。**本轮该改造已被回退为上游形态的串行循环**，因此报告采集了三组数据：

| 代号 | 含义 | 状态 |
| --- | --- | --- |
| **上游原版** | `4ebf270` 的 graphql-go（独立 worktree 采集） | 参照 |
| **串行原生** | 当前 `ExecuteGraphQLGo`（已回退为串行） | **本轮主基线** |
| **SGraph** | 当前 `Execute` → `executeSGraph` | 被测对象 |
| 并发版原生 | 回退前的实现 | 仅用于量化那次改造的代价 |

### 交叉验证：串行原生 ≈ 上游原版

| 用例 | 上游 allocs | 串行原生 allocs | 上游 ns | 串行原生 ns |
| --- | ---: | ---: | ---: | ---: |
| RespSize/Rows10 | 508 | **508** | 23.8us | 25.4us |
| RespSize/Rows100 | 4,741 | **4,741** | 166.2us | 183.1us |
| RespSize/Rows1000 | 48,537 | **48,537** | 1.61ms | 1.81ms |
| RespSize/Rows5000 | 244,551 | **244,563** | 7.98ms | 8.75ms |
| NoResolverMid/NestedList20x20 | 12,385 | **12,386** | 430.6us | 477.0us |

allocs 逐用例几乎逐字相同，ns/op 相差 1.00–1.12x（串行原生略慢 7–12%，来自本 fork 的其它改动与测量噪声）。这一致性说明上游基线的采集是可靠的，也说明**回退是彻底的**。

同时确认：`go test -race ./` 现在 **0 DATA RACE**。`CONFORMANCE_MATRIX.md:142` 记录的「原生 `-race` 报 `extensions.go` 对共享 `executionContext.Context` 的并发读写」随串行化一并消失，证实那个竞态的根因就是 `executeSubFields` 的并发化，而非 `extensions.go` 本身。

## 3. 公平性保障

1. 三条链路使用**逐字节相同**的 schema 结构、数据集、查询文本与变量。schema 按普通 graphql-go 写法构造：不为 SGraph 补 resolver、不注册 `ParamRegistry`、不改写查询。
2. parse 与 validate 由两条链路共用（`graphql.go` 中位于链路路由之前），故基准只测执行段，三侧跑同一份已解析并校验的 AST。
3. 三侧都在计时前预热一次。SGraph 借此完成 Plan 编译与 BatchPlan 协调，使其不计入稳态；原生无对应缓存，预热对其是纯空转。
4. **结果一致性经逐用例校验：23/23 用例三方响应体大小完全一致**，SGraph 侧另有 data 深度比较全部通过。
5. **不跳过任何用例。** 仓库既有 benchmark 在结果不一致时 `b.Skip`，会让 SGraph 只跑自己能跑的子集；本基准改为「标记但照常计时」，实测无一例需要标记。
6. **mutation 不纳入对比**：`executor.go` 对 mutation 直接回退 `ExecuteGraphQLGo`，两侧测同一段代码。既有 benchmark 的 Mutation 数据存在这一误读风险。

## 4. 主矩阵结果（ns/op 中位数）

| 组 | 用例 | 响应B | 上游 | 串行原生 | SGraph | SG/串行原生 | （并发版原生） |
|---|---|---:|---:|---:|---:|---:|---:|
| Baseline | SingleScalar | 14 | 3.1us | 3.2us | 3.3us | 1.03x | 18.5us |
| RespSize | Rows10 | 632 | 23.8us | 25.4us | 7.5us | **0.30x** | 238.5us |
| RespSize | Rows100 | 6303 | 166.2us | 183.1us | 39.2us | **0.21x** | 2.20ms |
| RespSize | Rows1000 | 63904 | 1.61ms | 1.81ms | 331.4us | **0.18x** | 22.31ms |
| RespSize | Rows5000 | 323904 | 7.98ms | 8.75ms | 1.61ms | **0.18x** | 107.88ms |
| ReqSize | Wide50 | 701 | 22.9us | 24.2us | 34.0us | **1.41x** | 60.8us |
| ReqSize | Wide500 | 7001 | 190.1us | 204.0us | 247.6us | **1.21x** | 358.6us |
| ReqSize | Wide2000 | 28001 | 768.1us | 824.5us | 963.7us | **1.17x** | 1.31ms |
| PerElemResolver | Rows100Upper | 3213 | 106.1us | 117.0us | 56.3us | **0.48x** | 2.10ms |
| PerElemResolver | Rows1000Upper | 32014 | 976.1us | 1.10ms | 501.4us | **0.46x** | 20.90ms |
| NoResolverMid | WrapperItems | 2033 | 60.4us | 67.1us | 15.5us | **0.23x** | 1.09ms |
| NoResolverMid | WrapperItemsTypename | 2083 | 61.5us | 68.9us | 34.6us | **0.50x** | 1.12ms |
| NoResolverMid | WrapperItemsResolver | 1633 | 59.6us | 66.0us | 32.8us | **0.50x** | 1.10ms |
| NoResolverMid | NestedList20x20 | 16532 | 430.6us | 477.0us | 95.7us | **0.20x** | 8.74ms |
| Abstract | Interface100 | 4811 | 171.3us | 188.7us | 95.5us | **0.51x** | 2.27ms |
| Abstract | Union100 | 4812 | 171.2us | 190.9us | 95.9us | **0.50x** | 2.25ms |
| Language | AliasFragmentDirective | 218 | 14.6us | 15.9us | 12.3us | 0.77x | 206.0us |
| Language | VariableArg | 16 | 3.7us | 4.0us | 3.5us | 0.89x | 18.9us |
| Latency | Parallel1 | 11 | 2.30ms | 2.29ms | 2.29ms | 1.00x | 2.35ms |
| Latency | Parallel4 | 41 | 9.11ms | 9.12ms | 2.32ms | **0.25x** | 2.38ms |
| Latency | Parallel16 | 167 | 36.39ms | 36.39ms | 2.36ms | **0.06x** | 2.41ms |
| Latency | Chain3Levels | 35 | 9.14ms | 9.13ms | 2.33ms | **0.26x** | 9.39ms |
| Latency | Mixed | 55 | 13.71ms | 13.68ms | 2.32ms | **0.17x** | 9.42ms |

（比值 < 1 表示 SGraph 更快。）

### allocs/op 中位数

| 用例 | 上游 | 串行原生 | SGraph | （并发版原生） |
|---|---:|---:|---:|---:|
| RespSize/Rows5000 | 244,551 | 244,563 | **44,773** | 1,479,945 |
| RespSize/Rows1000 | 48,537 | 48,537 | **8,772** | 295,806 |
| NoResolverMid/NestedList20x20 | 12,385 | 12,386 | **2,649** | 113,856 |
| PerElemResolver/Rows1000Upper | 29,790 | 29,791 | **14,774** | 271,061 |
| Abstract/Interface100 | 4,746 | 4,746 | **2,730** | 29,387 |
| Language/AliasFragmentDirective | 252 | 252 | **98** | 2,876 |
| ReqSize/Wide2000 | **18,056** | 18,057 | 20,043 | 20,664 |
| Baseline/SingleScalar | 27 | 27 | **25** | 265 |

## 5. 并发吞吐（`RunParallel`）

| 用例 | 串行原生 | SGraph | SG/串行原生 | （并发版原生） | 原生 allocs | SG allocs |
|---|---:|---:|---:|---:|---:|---:|
| SingleScalar | 879ns | 730ns | 0.83x | 7.3us | 27 | 25 |
| Rows100 | 126.5us | 15.0us | **0.12x** | 1.09ms | 4,742 | 828 |
| Rows1000 | 854.5us | 110.0us | **0.13x** | 9.03ms | 48,539 | 8,773 |
| Interface100 | 111.4us | 43.5us | **0.39x** | 1.03ms | 4,747 | 2,732 |
| Parallel4Delay | 922.7us | 230.3us | **0.25x** | 233.1us | 59 | 67 |

## 6. 结论

### 6.1 回退是正确的，但它交出了同层 IO 并发能力

把回退前后的原生数据并排看，那次并发化改造是一个**极不对称的交换**：

| 场景 | 并发版原生 | 串行原生 | 差异 |
| --- | ---: | ---: | --- |
| Rows1000（CPU 密集） | 22.31ms | 1.81ms | 串行快 **12.3x** |
| Rows1000Upper（N+1） | 20.90ms | 1.10ms | 串行快 **19.0x** |
| NestedList20x20 | 8.74ms | 477.0us | 串行快 **18.3x** |
| SingleScalar（固定开销） | 18.5us | 3.2us | 串行快 **5.8x** |
| **Parallel16（同层 IO）** | **2.41ms** | **36.39ms** | 并发版快 **15.1x** |
| **Parallel4（同层 IO）** | **2.38ms** | **9.12ms** | 并发版快 **3.8x** |

回退换回了 CPU 密集场景 5–19 倍的性能和 5–10 倍的 allocs，代价是**失去同层多字段的 IO 并发**：16 个各 2ms 的独立根字段，从 2.41ms 退回 36.39ms。

如果业务查询里存在「同层多个慢字段」的形态，这个退化是真实可感的。但注意 SGraph 在同一场景是 **2.36ms** —— 也就是说，同层 IO 并发这个能力**不必靠改造原生 executor 来获得**。

### 6.2 SGraph 的 batch 编排在两个方向上都优于「无条件并发」和「完全串行」

| 场景 | 串行原生 | 并发版原生 | SGraph |
| --- | ---: | ---: | ---: |
| Rows5000（CPU 密集） | 8.75ms | 107.88ms | **1.61ms** |
| Parallel16（同层 IO） | 36.39ms | 2.41ms | **2.36ms** |
| Chain3Levels（跨层 IO） | 9.13ms | 9.39ms | **2.33ms** |

两种原生形态各占一头、各失一头；SGraph 三项都是最优。原因是它**按参数依赖分层后再在层内并发**，而不是无条件对每个字段起 goroutine：

- CPU 密集时依赖图退化为少数几个 batch，不产生 goroutine 风暴
- 同层无依赖字段自然落进同一 batch 并发
- 跨层无依赖字段被折叠进同一 batch（见 §6.4 的前提说明）

这是本次基准中 SGraph 架构层面最站得住的结论。

### 6.3 与串行原生（≈上游）相比，SGraph 的收益与劣势

| 维度 | SGraph 相对串行原生 |
| --- | --- |
| 响应体规模（10→5000 行） | 快 **3.3–5.6x**，倍率随规模上升后稳定在 5.4x |
| 无 resolver 中间层 | 快 **2.0–5.0x** |
| 每元素带 resolver（N+1 形态） | 快 **2.1–2.2x** |
| 抽象类型运行时判定 | 快 **2.0x** |
| 同层高延迟并发（4/16 字段） | 快 **3.9–15.4x** |
| 跨层高延迟（3 级链） | 快 **3.9x** |
| alias/fragment/`@skip` | 快 1.3x |
| 单标量基线 | 持平（1.03x，SGraph 略慢） |
| **请求体规模（50→2000 别名字段）** | **慢 1.17–1.41x** |

allocs 侧普遍少 **2–5.5x**，这是大响应体场景领先的主因：编译期一次性产出 Plan，运行期靠对象池复用 `FieldResponse`；原生每次执行都要重建 execution context 与字段元数据。

**唯一劣势是请求体维度。** `Wide2000`（2000 个别名字段、32KB 查询）慢 1.17x，allocs 反而略多（20,043 vs 18,057）。此时每个别名都是独立 FieldPlan 与 Step，编译产物规模与 Step 调度开销随请求体线性增长，而单字段实际工作量极小，收益无从摊薄。**字段数极多、每字段计算量极小的查询是 SGraph 目前不占优的形态**，且倍率随字段数增加而收敛（1.41x → 1.17x），说明是固定的单字段调度成本而非规模问题。

### 6.4 跨层延迟优势有明确语义前提，不能无条件引用

`Chain3Levels`（`deep { b { c { value } } }`，四级各 sleep 2ms）：

```
上游原版   9.14ms  ≈ 4 × 2ms   四级串行
串行原生   9.13ms  ≈ 4 × 2ms   四级串行
并发版原生 9.39ms  ≈ 4 × 2ms   同样串行——并发化只作用于同层，不跨层
SGraph     2.33ms  ≈ 1 × 2ms   四级折叠进同一 batch 并发
```

SGraph 快 3.9 倍，但这个数字**建立在一个语义前提上**：它把父子字段编排进同一 batch 并发执行，因此子 resolver 拿不到父结果（`p.Source` 恒为 nil）。本基准的 `deepA/deepB/deepC` resolver 都不读 `Source`，折叠才合法。

**真实业务中若子 resolver 需要父数据**，必须通过 `ParamRegistry` 声明 `FIELD_RESPONSE` 依赖，SGraph 随即按依赖分层，退化为与原生同样的串行 —— 3.9 倍优势归零。

正确的表述是：**SGraph 用「不自动传递父结果」换取了跨层并发的可能性**，收益取决于业务 schema 中「父子无数据依赖」的比例，而非一个普适倍率。同层并发（`Parallel4/16`）不受此前提约束，那部分优势是无条件的。

### 6.5 并发吞吐结论与单请求一致

`RunParallel` 下 SGraph 相对串行原生的优势（Rows1000 快 7.8x、Interface100 快 2.6x）与单请求同量级，说明差异来自每请求内部的分配与元数据重建，而非调度器竞争。`Parallel4Delay` 一项 SGraph 快 4x，也与单请求一致。

## 7. 覆盖范围与局限

已覆盖：响应体 14B→324KB（5 档）、请求体 10B→32KB（4 档）、resolver 延迟 0/2ms × 同层 1/4/16 并发 × 跨层 3 级、无 resolver 中间层（含 `__typename` 与带 resolver 子字段两种形态）、嵌套 list、interface/union 运行时判定、alias/fragment/`@skip`/变量、并发吞吐。

未覆盖及原因：

1. **mutation** —— 两侧回退同一实现，无信息量（§3.6）。
2. **bulk resolver** —— `Field` 公开结构体没有 `BulkResolve`，只能改内部 `FieldDefinition`。SGraph 独有能力，原生无对应实现，无法构成对比；应作为「SGraph 特性收益」单独测量。
3. **ParamRegistry 参数依赖** —— 同上，SGraph 独有。如 §6.4 所述它会取消跨层并发优势，建议后续单独测「声明依赖后 SGraph 相对自身的退化幅度」。
4. **subscription** —— SGraph 不支持。
5. 延迟统一为 2ms 且用 `time.Sleep` 模拟。真实 IO 有连接池、超时、抖动，绝对值不可外推；本报告只用它比较**调度能力**。
6. 单机采样（`count=5` 取中位数），未做多机复现。倍率 ≥2x 的结论稳健；`Baseline`（1.03x）、`ReqSize`（1.17x）这类接近 1 的比值受噪声影响相对更大。
7. 上游基线在独立 worktree 采集，进程/环境与主矩阵不同批次。§2 的 allocs 逐用例吻合可作为该基线可信的旁证。

## 8. 建议

1. **同层 IO 并发不必靠改造原生 executor 获得。** 回退后串行原生在 `Parallel16` 退到 36.39ms，而 SGraph 是 2.36ms。若业务存在同层多慢字段形态，走 SGraph 比重新并发化 `executeSubFields` 更划算——后者在 CPU 密集场景要付 5–19 倍代价。
2. 若迁移动因是**大响应体、高扇出、抽象类型密集**，SGraph 有 2.0–5.6 倍稳定收益，allocs 降低 2–5.5 倍，收益可信。
3. 若迁移动因是**跨层 IO 并发**，先统计业务中「子 resolver 不需要父数据」的比例——它决定 §6.4 的 3.9 倍能实现多少。需要父数据的链路声明依赖后不获得该收益。
4. **字段数极多、每字段计算量极小**的查询（大量别名、宽扁平投影）SGraph 慢 1.17–1.41x，迁移前应针对真实查询形态复测。
5. `executor.go` 当前保留了大段被注释的并发实现残留（`signal`、`mutex`、`mapSetFunc`、`cmap`）。若确认不再走回并发路线，建议清理，避免后续读者误判当前实现形态。
6. 仓库既有 benchmark 建议调整两处：Mutation 组标注「两侧同实现」，以及把结果不一致时的 `b.Skip` 改为标记后照常计时。

## 9. 复现方式

```bash
# 主矩阵（串行原生 + SGraph）
go test -run '^$' -bench 'BenchmarkChain(Native|SGraph)$' -benchtime 300ms -count 5 -benchmem .

# 并发组
go test -run '^$' -bench 'BenchmarkChain(Native|SGraph)Concurrent' -benchtime 300ms -count 3 -benchmem .

# 一致性校验（三方等价性的前提）
go test -run '^TestChainCompareConsistency$' -count=1 -v .

# 上游原版基线：检出 4ebf270 到独立 worktree，放入等价 benchmark 后运行
git worktree add /tmp/upstream 4ebf270
```
