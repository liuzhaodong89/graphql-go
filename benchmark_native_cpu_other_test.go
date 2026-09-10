//go:build !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris

package graphql

import "time"

// 不支持 getrusage 的平台保留其他 benchmark 指标，但不报告 CPU 时间。
func benchmarkNativeProcessCPUTime() (time.Duration, bool) {
	return 0, false
}
