//go:build windows

package benchlib

import (
	"fmt"
	"os/exec"
	"sync"
	"syscall"
	"unsafe"
)

// Windows does not end child processes when their parent exits. Every process the
// harness starts is put in one Job Object created with JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE:
// the only handle to the job is held by this process, so when it exits for any reason
// (os.Exit, a panic, being killed) the system closes the handle and kills the children.

var (
	procCreateJobObjectW         = kernel32.NewProc("CreateJobObjectW")
	procSetInformationJobObject  = kernel32.NewProc("SetInformationJobObject")
	procAssignProcessToJobObject = kernel32.NewProc("AssignProcessToJobObject")
	procGetExitCodeProcess       = kernel32.NewProc("GetExitCodeProcess")

	jobOnce   sync.Once
	jobHandle syscall.Handle
	jobErr    error
)

const (
	jobObjectExtendedLimitInformation = 9
	jobObjectLimitKillOnJobClose      = 0x2000
	processSetQuota                   = 0x0100
	processTerminate                  = 0x0001
	processQueryLimitedInformation    = 0x1000
	stillActive                       = 259
)

type ioCounters struct {
	ReadOperationCount, WriteOperationCount, OtherOperationCount uint64
	ReadTransferCount, WriteTransferCount, OtherTransferCount    uint64
}

type jobBasicLimit struct {
	PerProcessUserTimeLimit int64
	PerJobUserTimeLimit     int64
	LimitFlags              uint32
	MinimumWorkingSetSize   uintptr
	MaximumWorkingSetSize   uintptr
	ActiveProcessLimit      uint32
	Affinity                uintptr
	PriorityClass           uint32
	SchedulingClass         uint32
}

type jobExtendedLimit struct {
	Basic                 jobBasicLimit
	IO                    ioCounters
	ProcessMemoryLimit    uintptr
	JobMemoryLimit        uintptr
	PeakProcessMemoryUsed uintptr
	PeakJobMemoryUsed     uintptr
}

func killJob() (syscall.Handle, error) {
	jobOnce.Do(func() {
		h, _, e := procCreateJobObjectW.Call(0, 0)
		if h == 0 {
			jobErr = fmt.Errorf("CreateJobObject: %w", e)
			return
		}
		var info jobExtendedLimit
		info.Basic.LimitFlags = jobObjectLimitKillOnJobClose
		r, _, e := procSetInformationJobObject.Call(h, jobObjectExtendedLimitInformation,
			uintptr(unsafe.Pointer(&info)), unsafe.Sizeof(info))
		if r == 0 {
			syscall.CloseHandle(syscall.Handle(h))
			jobErr = fmt.Errorf("SetInformationJobObject: %w", e)
			return
		}
		jobHandle = syscall.Handle(h)
	})
	return jobHandle, jobErr
}

// prepareChild is called before cmd.Start (nothing to do on Windows).
func prepareChild(cmd *exec.Cmd) {}

// adoptChild ties a started child's lifetime to this process (see the comment above).
// A failure is returned but the child keeps running; the caller still stops it.
func adoptChild(cmd *exec.Cmd) error {
	job, err := killJob()
	if err != nil {
		return err
	}
	h, err := syscall.OpenProcess(processSetQuota|processTerminate, false, uint32(cmd.Process.Pid))
	if err != nil {
		return fmt.Errorf("OpenProcess: %w", err)
	}
	defer syscall.CloseHandle(h)
	if r, _, e := procAssignProcessToJobObject.Call(uintptr(job), uintptr(h)); r == 0 {
		return fmt.Errorf("AssignProcessToJobObject: %w", e)
	}
	return nil
}

// ProcessAlive reports whether a process with this pid is still running.
func ProcessAlive(pid int) bool {
	h, err := syscall.OpenProcess(processQueryLimitedInformation, false, uint32(pid))
	if err != nil {
		return false
	}
	defer syscall.CloseHandle(h)
	var code uint32
	if r, _, _ := procGetExitCodeProcess.Call(uintptr(h), uintptr(unsafe.Pointer(&code))); r == 0 {
		return false
	}
	return code == stillActive
}
