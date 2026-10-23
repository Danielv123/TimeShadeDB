//go:build windows

package benchlib

import (
	"syscall"
	"time"
	"unsafe"
)

// On Windows the Go monotonic clock advances in steps of about 0.5 ms, too coarse for
// the sub-millisecond targets, so latencies use QueryPerformanceCounter.
var (
	procQPC  = kernel32.NewProc("QueryPerformanceCounter")
	procQPF  = kernel32.NewProc("QueryPerformanceFrequency")
	qpcFreq  int64
	qpcStart int64
)

func init() {
	procQPF.Call(uintptr(unsafe.Pointer(&qpcFreq)))
	procQPC.Call(uintptr(unsafe.Pointer(&qpcStart)))
	_ = syscall.EINVAL
}

// Tick is a high-resolution timestamp.
type Tick int64

// Now returns the current high-resolution timestamp.
func Now() Tick {
	var c int64
	procQPC.Call(uintptr(unsafe.Pointer(&c)))
	return Tick(c)
}

// Since returns the time elapsed since t.
func Since(t Tick) time.Duration {
	if qpcFreq == 0 {
		return 0
	}
	d := int64(Now() - t)
	return time.Duration(d/qpcFreq*int64(time.Second) + d%qpcFreq*int64(time.Second)/qpcFreq)
}
