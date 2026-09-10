//go:build !race

package graphql

// raceDetectorEnabled 标识当前是否为 -race 构建，说明见 sgraph_race_detector_on_test.go。
const raceDetectorEnabled = false
