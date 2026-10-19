//go:build windows

package benchlib

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"unsafe"
)

var (
	kernel32                 = syscall.NewLazyDLL("kernel32.dll")
	procGetProcessMemoryInfo = kernel32.NewProc("K32GetProcessMemoryInfo")
)

// processMemoryCounters is PROCESS_MEMORY_COUNTERS from psapi.h.
type processMemoryCounters struct {
	CB                         uint32
	PageFaultCount             uint32
	PeakWorkingSetSize         uintptr
	WorkingSetSize             uintptr
	QuotaPeakPagedPoolUsage    uintptr
	QuotaPagedPoolUsage        uintptr
	QuotaPeakNonPagedPoolUsage uintptr
	QuotaNonPagedPoolUsage     uintptr
	PagefileUsage              uintptr
	PeakPagefileUsage          uintptr
}

// ProcessMemory reads a process's memory use. On Windows RSS is the working set,
// Private is the commit charge (PagefileUsage) and PeakRSS the peak working set.
func ProcessMemory(pid int) (MemInfo, error) {
	const access = 0x1000 | 0x0010 // PROCESS_QUERY_LIMITED_INFORMATION | PROCESS_VM_READ
	h, err := syscall.OpenProcess(access, false, uint32(pid))
	if err != nil {
		return MemInfo{}, fmt.Errorf("OpenProcess %d: %w", pid, err)
	}
	defer syscall.CloseHandle(h)
	var c processMemoryCounters
	c.CB = uint32(unsafe.Sizeof(c))
	r, _, e := procGetProcessMemoryInfo.Call(uintptr(h), uintptr(unsafe.Pointer(&c)), uintptr(c.CB))
	if r == 0 {
		return MemInfo{}, fmt.Errorf("GetProcessMemoryInfo %d: %w", pid, e)
	}
	return MemInfo{RSS: uint64(c.WorkingSetSize), Private: uint64(c.PagefileUsage), PeakRSS: uint64(c.PeakWorkingSetSize)}, nil
}

func osDescription() string {
	out, err := exec.Command("cmd", "/c", "ver").Output()
	if err == nil {
		if s := strings.TrimSpace(string(out)); s != "" {
			return s
		}
	}
	return "windows"
}

func cpuName() string {
	out, err := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command",
		"(Get-CimInstance Win32_Processor | Select-Object -First 1).Name").Output()
	if err == nil {
		if s := strings.TrimSpace(string(out)); s != "" {
			return s
		}
	}
	return os.Getenv("PROCESSOR_IDENTIFIER")
}
