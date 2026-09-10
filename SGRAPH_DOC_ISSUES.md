# 文档问题清单：SGRAPH_USER_MANUAL.md 与 sgraph_执行引擎技术方案.docx

## 0. 说明

- 审查对象：`SGRAPH_USER_MANUAL.md`（1294 行）、`sgraph_执行引擎技术方案.docx`
- 代码基准：`bb75dea alpha1.1` + 工作副本对 `executor.go` 的串行化改动
- 本清单**只列文档自身的问题**（事实错误、已过时、缺失）。代码缺陷本身记录在 `opus_bug_advice.md`，此处只在文档未覆盖时作为「缺失项」出现。
- 每条都注明核对方式。未经实测的推断不写入。

### 已核对通过、无需改动的部分

| 项 | 结论 |
|---|---|
| 技术方案全部技术断言 | 逐条对照源码核对，未发现错误 |
| 手册 §14 列出的 20 条错误文案 | **20/20** 均存在于当前源码 |
| 两份文档的 topic 边界 | 清晰：手册＝调用方视角，技术方案＝维护者视角；少量重叠（能力边界）是必要的 |
| 两份文档的冗余 | 未发现偏离 topic 的干扰信息 |
| 手册章节权重 | 合理：ParamRegistry 190 行、Bulk 203 行、resolver 契约 150 行，正是迁移最难的三块 |

---

## 1. 事实错误与已过时（优先级：高）

### 1.1 手册 §2 速查表 #13：原生 non-null 冒泡「panic 逃逸可能终止进程」——已过时

**文档现状**

| # | 维度 | 原生 | sgraph | 影响 |
|---|---|---|---|---|
| 13 | non-null 冒泡跨 goroutine | panic 逃逸可能终止进程 | recover 成字段错误 | ✅（更稳） |

**实测（子进程隔离，`ExecuteGraphQLGo` 直调）**

```
{ v }   v: String! 且 resolver 返回 nil
→ {"data":null,"errors":[{"message":"Cannot return null for non-nullable field Query.v.",
                          "locations":[{"line":1,"column":3}],"path":["v"]}]}
→ 未 panic，正常返回
```

**原因**：`executor.go` 的 `executeSubFields` 已回退为串行循环。panic 不再跨 goroutine，沿调用栈被上层 recover 捕获。

**处理**：删除该行，或改写为「两条链路均返回字段错误，行为一致 ✅」。

---

### 1.2 手册 §2 速查表 #14：原生 `-race` 有数据竞争——已过时

**文档现状**

| # | 维度 | 原生 | sgraph | 影响 |
|---|---|---|---|---|
| 14 | `-race` 并发检查 | 原生 `extensions.go` 有数据竞争 | 通过 | ✅ |

**实测**：`go test -race -count=1 .` → `WARNING: DATA RACE` 出现 **0** 次。

**原因**：该竞态的根因是 `executeSubFields` 对每个字段无条件 `go func()`，导致并发写共享 `executionContext`。串行化后竞态消失。这也修正了 `CONFORMANCE_MATRIX.md:142` 把根因归给 `extensions.go` 的表述。

**处理**：删除该行，或改写为「两条链路 `-race` 均通过 ✅」。

**连带影响**：`CONFORMANCE_MATRIX.md` 中「sgraph 相对原生的 31 条改善」里，non-null 冒泡 3 条与 `-race` 一条同样失效，需一并复核（该文档不在本次审查范围内，此处仅提示）。

---

### 1.3 手册 §13.2：P0 条数「8 条」错误，且与本章标题自相矛盾

**文档现状**

```
### 13.2 已知缺陷（sgraph 独有，14 条叶子用例）

| 严重度 | 问题 | 条数 |
| P0   | 根选择集被字面量 @skip/@include 全部裁剪时返回 request error | 8 |
| 行为差异 | 不执行 map 中的函数属性                                   | 2 |
| 能力边界 | subscription 不支持                                       | 1 |
```

8 + 2 + 1 = 11 ≠ 标题里的 14。

**实测**（`go test -run TestSpec2025 -v`，统计错误文案命中次数）

| 文案 | 实际条数 |
|---|---:|
| `no roots found` | **11** |
| `resolves to a function value` | 2 |
| `subscription is not supported yet` | 1 |
| 合计 | **14** ✓ |

`SPEC2025_CASE_MATRIX.csv` 中 `SGRAPH_REGRESSED` 亦为 14 条，其中 `Cross_ConditionalDirectiveMatrix` 占 9 条（`SPEC2025_SGRAPH_REPORT.md` §3.1 写的「6 条」同样偏低）。

**处理**：P0 一行的条数改为 **11**。同时修正 `SPEC2025_SGRAPH_REPORT.md` §3.1 的「6 条」为 9 条。

---

### 1.4 手册 §14：`does not produce a FieldResponse` 的触发条件描述已过时

**文档现状**

| 错误文案 | 阶段 | 原因 | 处置 |
|---|---|---|---|
| `field %d depends on field %d which does not produce a FieldResponse` | 编排 | 来源字段没有 Step | 来源必须是有 resolver 的字段（§8.3） |

**实测**

| 查询 | 结果 |
|---|---|
| `{ w { items { __typename id } } }`，`items` 无 resolver | **正常返回** `{"w":{"items":[{"__typename":"DCLeaf","id":"a"}]}}` |
| `{ w { items(n:1) { __typename id } } }`，`items` 无 resolver 但有参数 | 报 `field 2 has param plan but no resolver`（不是本条） |

**原因**：内部物化（`materializeFromParentSource`）落地后，无 resolver 中间层在下游需要其运行时值时会自动生成物化 Step，常规路径不再触发本条。文案本身仍在源码中（`plan_coordinator.go`），但当前描述会让读者误以为「无 resolver 中间层不可用」。

**处理**：把「原因」改为「ParamRegistry 指向的来源字段无法产出 FieldResponse，或依赖边指向了不会生成 Step 的字段」，并补一句「无 resolver 的中间层字段已由编译器按需物化，不再触发本条」。

---

## 2. 缺失：与 topic 直接相关但未覆盖（优先级：中）

### 2.1 手册 §14 排错手册缺整个执行期 / 组装期错误族

现有 20 条覆盖启动、编译、编排、bulk 运行期。**生产中最常遇到的执行期与组装期错误一条都没有**，而这些文案上一轮刚统一对齐 graphql-go 原生，正是补入的时机：

| 错误文案 | 阶段 | 触发场景 |
|---|---|---|
| `Cannot return null for non-nullable field <Parent>.<field>.` | 执行/组装 | non-null 字段或 non-null list 元素为 null |
| `User Error: expected iterable, but did not find one for field <Parent>.<field>.` | 执行/组装 | list 字段的值不是切片 |
| `Abstract type X must resolve to an Object type at runtime for field P.f with value "...", received "<nil>".` | 组装 | `ResolveType` / `IsTypeOf` 无法确定运行时类型 |
| `Runtime Object type "X" is not a possible type for "Y".` | 组装 | `ResolveType` 返回的 Object 不在 possible types 内 |
| `cannot serialize leaf value for <field>:<Type>` | 组装 | Scalar/Enum 序列化返回 nullish（含枚举未知值、Int 超范围） |
| `parent response for field X does not support composite key mapping` | 执行 | 启用业务 key 绑定，但父元素不是 `map[string]any`（struct、命名 map） |
| `Variable "$x" of required type "Int!" was not provided.` | 变量 | non-null 变量未提供，或显式传 null |
| `Variable "$x" got invalid value <JSON>.⏎<原因>` | 变量 | 变量值不合法，`<原因>` 为下列之一 |
| ├ `Expected type "Int", found "abc".` | 变量 | 标量/枚举解析失败 |
| ├ `Expected "Int!", found null.` | 变量 | non-null 位置收到 null |
| ├ `In field "f": Unknown field.` | 变量 | 输入对象含未声明字段（多个时逐条一行） |
| ├ `In field "f": <嵌套原因>` | 变量 | 输入对象字段错误，可多层嵌套 |
| ├ `In element #N: <原因>` | 变量 | 输入 list 第 N 个元素错误（N 从 1 起） |
| └ `Expected "T", found not an object.` | 变量 | 输入对象位置收到非对象 |

> 补充说明可一并写入：这些文案已与 graphql-go 原生逐字对齐，唯一有意保留的差异是 `In element #N` 的序号取真实元素下标——原生该处存在 off-by-one（把消息下标当成元素下标），未予复制。

---

### 2.2 手册 §5.4 遗漏三个盲区

§5.4 本身写得充分（有实测、有对照实验、指出了「给类型加 ID 字段」的隐蔽风险）。缺三点，均与本节 topic 直接相关：

**（1）父元素不是 `map[string]any` 时直接失败**

全节假设父元素是 `map[string]any`。实际上命中业务 key 绑定后，父元素是 struct 或命名 map 类型时会报：

```
parent response for field <字段> does not support composite key mapping
```

同一 schema 下，不启用业务 key 绑定（父类型无 ID 字段）时 struct 父元素工作正常——这是引擎内部的不一致，接入方需要知道。

**（2）绑定失败时 list 子字段返回 `[]` 而非 `null`**

§5.4「副作用一」的实测只展示了标量子字段 `label: null`。同一失败原因下 list 子字段返回的是**空数组**：

```json
{"data":{"items":[{"name":"a","tags":[],"label":null}, ...]},
 "errors":[{"message":"parent key field \"id\" is missing for field tags", ...}]}
```

`tags: []` 与「确实是空集合」不可区分，比 `null` 更难被客户端发现。建议在副作用一里补一行对照。

**（3）「值逐元素唯一」的判定是字符串比较**

§5.4 要求「值必须逐父元素唯一」，但未说明唯一性如何判定。`generateCompositeKey` 经 `valueToString` 生成 key，因此：

- `int(1)` 与 `string("1")` 折叠成同一个 key —— 而 `ID` 类型恰好两者都接受
- 多个 `nil` 值折叠成 `"null"`

即「业务上唯一」不等于「key 唯一」。

---

### 2.3 技术方案 §10.1「当前已确认限制」漏 5 类仍存在的缺陷

以下均经实测复现（详见 `opus_bug_advice.md` 与 `sgraph_parent_data_matrix_test.go`），当前仍存在，但 §10.1 表格未覆盖：

| 缺陷 | 现象 | 与现有表格的关系 |
|---|---|---|
| composite key 绑定要求 `map[string]any` | struct / 命名 map 父元素报 `does not support composite key mapping` | 表中「Bulk」行只提到 bulk item 要求 map；**普通逐元素绑定同样受限**，未覆盖 |
| 绑定失败时 list 与标量补全不一致 | 标量正确变 `null`，list 变 `[]` | 未覆盖 |
| 抽象类型按声明类型解析 field definition | 实现 Object 上配置的 `Resolve` 不生效；同名字段结果依赖书写顺序 | 未覆盖，另见 §2.4 |
| null / 报错的父仍调用子 resolver | 非 list 具体 Object 父下的子字段进 batch 0 无条件执行；`errors[].path` 指向 `data` 中不存在的位置 | 未覆盖 |
| composite key 跨类型折叠 | `valueToString` 使 `int(1)` 与 `"1"` 折叠 | 未覆盖 |

---

### 2.4 两份文档均未记录：抽象类型字段按声明类型解析 field definition

**现象**：字段返回类型声明为 interface/union 时，编译期按**声明类型**取 field definition（`plan_compiler.go` 的 `getFieldDefinition(parentTypeScope.declaredType, ...)`）。接口上的字段定义天生没有 `Resolve`，因此**实现 Object 上配置的 `Resolve` 不会被挂载**。

**实测**（接口声明计算字段 `displayName`，实现类各给算法，父数据里无该 key）：

```
NATIVE| { actor { id displayName } }                    {"displayName":"Ada Lovelace","id":"p1"}
SGRAPH| { actor { id displayName } }                    {"displayName":null,"id":"p1"}     无错误
SGRAPH| { actor { id ... on Person { displayName } } }  {"displayName":"Ada Lovelace","id":"p1"}
```

**衍生问题**：同一 responseName 分别出现在无条件选择与内联片段中时，编译出两个 FieldPlan，写入响应后写覆盖，结果依赖书写顺序：

```
SGRAPH| { me { id phone ... on User { phone } } }   → 走声明类型（无 resolver）
SGRAPH| { me { id ... on User { phone } phone } }   → 走内联片段（有 resolver）
```

两种写法在 §6.3.2 CollectFields 下应合并为同一字段，结果必须相同；原生两种写法一致。

**严重程度评估（刻意压低）**：

- 迁到 sgraph 后，实现类 resolver 因 `p.Source` 恒为 nil 本来就要改造，多数会在改造期暴露
- 有现成规避：改用内联片段即可，而抽象类型下本就倾向这种写法
- 后果是 `null` 而非错值，通常会被前端或测试发现

**处理**：**不进 §2 速查表**（该表是「会让现有代码出错」的清单，17 条密度已足够）。建议在手册 §13.1 设计边界与技术方案 §10.1 各加一行，措辞如：

> 抽象类型字段按声明类型解析 field definition，实现 Object 上配置的 `Resolve` 不生效；需要按运行时类型分派时使用内联片段。同名字段同时出现在无条件选择与内联片段中时，结果取决于书写顺序。

---

## 3. 缺失：优先级低

### 3.1 技术方案 §9 未说明 planCache 无淘汰

§9.3 详细描述了对象池的内存边界，但 §9.1 缓存表中 `engine.planCache` 一行只写「Load/LoadOrStore；冷并发可重复编译但只保留一个；错误不缓存」，**未说明它无容量上限、无 TTL、无淘汰**。

**核对**：`sgraph_engine.go` 中对 `planCache` 只有 `Load` 与 `LoadOrStore`，无 `Delete`／`Range`（grep 计数为 0）。

**影响**：每个不同的 query 文本永久驻留一个 `*SGraphExecutionPlan`（含其 `batches`、全部 `FieldPlan`；`__type`/`__schema` 查询还会闭包持有 `PlanCompiler`）。仅靠 alias 变体（`{a:f b:f c:f}`）即可无限增长。原生链路不缓存任何东西，这是新增的内存暴露面。

**处理**：在 §9.1 的 `engine.planCache` 行补「无容量上限与淘汰，缓存条目数等于进程内出现过的不同 query 文本数」，或在 §9.3 内存边界中单列一段。

---

## 4. 修订优先级汇总

| 优先级 | 编号 | 文档 | 项 | 性质 |
|---|---|---|---|---|
| 高 | 1.1 | 手册 §2 #13 | 原生 panic 逃逸已不成立 | 事实错误（已过时） |
| 高 | 1.2 | 手册 §2 #14 | 原生 `-race` 竞态已不成立 | 事实错误（已过时） |
| 高 | 1.3 | 手册 §13.2 | P0 条数 8 → 11，与标题自相矛盾 | 事实错误 |
| 高 | 1.4 | 手册 §14 | `does not produce a FieldResponse` 触发条件 | 描述已过时 |
| 中 | 2.1 | 手册 §14 | 补执行期/组装期错误族（约 14 条） | 缺失 |
| 中 | 2.2 | 手册 §5.4 | 补 struct 父元素、list 返回 `[]`、key 跨类型折叠 | 缺失 |
| 中 | 2.3 | 技术方案 §10.1 | 补 5 类仍存在的缺陷 | 缺失 |
| 低 | 2.4 | 手册 §13.1 + 技术方案 §10.1 | 抽象类型按声明类型解析 | 缺失（边界未记录） |
| 低 | 3.1 | 技术方案 §9.1 | planCache 无淘汰 | 缺失 |

**连带项**（不在本次审查范围，仅提示）：`CONFORMANCE_MATRIX.md` 的「31 条改善」中 non-null 冒泡 3 条与 `-race` 一条随串行化失效；`SPEC2025_SGRAPH_REPORT.md` §3.1 的「6 条」应为 9 条。

## 5. 核对方式

```bash
# 1.2 / 1.1：原生竞态与 panic
go test -race -count=1 .
# non-null panic 需子进程隔离直调 ExecuteGraphQLGo

# 1.3：spec2025 独有失败构成
go test -run 'TestSpec2025' -count=1 -v . 2>&1 | grep -c 'no roots found'

# 1.4 / 2.4：物化与抽象类型行为，构造最小 schema 后对比 Do 与 ExecuteGraphQLGo

# 2.1：文案与源码一致性
grep -rlF '<文案片段>' *.go

# 3.1：planCache 淘汰逻辑
grep -E 'planCache\.(Delete|Range)' sgraph_engine.go
```
