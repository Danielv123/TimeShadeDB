package terrain

import (
	"syscall"
	"time"
	"unsafe"
)

// On Windows time.Now advances in coarse steps (about 0.5 ms), too coarse for per-read
// latencies; the benchmarks use QueryPerformanceCounter instead.
var (
	qpc     = syscall.NewLazyDLL("kernel32.dll").NewProc("QueryPerformanceCounter")
	qpf     = syscall.NewLazyDLL("kernel32.dll").NewProc("QueryPerformanceFrequency")
	qpcFreq = func() int64 {
		var f int64
		qpf.Call(uintptr(unsafe.Pointer(&f)))
		return f
	}()
)

type hrStamp int64

func hrNow() hrStamp {
	var c int64
	qpc.Call(uintptr(unsafe.Pointer(&c)))
	return hrStamp(c)
}

func hrSince(s hrStamp) time.Duration {
	return time.Duration((int64(hrNow()) - int64(s)) * int64(time.Second) / qpcFreq)
}
