//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package graphql

import (
	"syscall"
	"time"
)

// benchmarkNativeProcessCPUTime 返回当前进程累计消耗的用户态与内核态 CPU 时间。
func benchmarkNativeProcessCPUTime() (time.Duration, bool) {
	var usage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
		return 0, false
	}
	return time.Duration(usage.Utime.Nano() + usage.Stime.Nano()), true
}
