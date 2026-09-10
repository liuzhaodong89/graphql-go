//go:build race

package graphql

// raceDetectorEnabled 标识当前是否为 -race 构建。
//
// 用途：部分用例以原生链路（ExecuteGraphQLGo）作为 oracle，而原生 executor 的
// handleExtensionsResolveFieldDidStart 对共享 executionContext.Context 有并发读写
// （extensions.go:196/197，经 executor.go:400 executeSubFields 的并发子字段触发），
// 这是 CONFORMANCE_MATRIX.md 已记录的原生链路缺陷，与被测的 sgraph 行为无关。
// 实测只有内省查询（结果列表长、并发子字段多）会稳定命中它，因此仅这类用例
// 在 -race 下改为只断言 sgraph 侧行为，避免把原生的已知竞态记到 sgraph 账上。
const raceDetectorEnabled = true
