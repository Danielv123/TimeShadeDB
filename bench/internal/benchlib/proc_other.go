//go:build !windows

package benchlib

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
)

// ProcessMemory reads a process's memory use from /proc (Linux). RSS is VmRSS, Private
// is RssAnon and PeakRSS is VmHWM.
func ProcessMemory(pid int) (MemInfo, error) {
	f, err := os.Open(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return MemInfo{}, err
	}
	defer f.Close()
	var m MemInfo
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		fields := strings.Fields(v)
		if len(fields) == 0 {
			continue
		}
		n, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			continue
		}
		switch k {
		case "VmRSS":
			m.RSS = n * 1024
		case "RssAnon":
			m.Private = n * 1024
		case "VmHWM":
			m.PeakRSS = n * 1024
		}
	}
	return m, sc.Err()
}

func osDescription() string {
	if out, err := exec.Command("uname", "-srm").Output(); err == nil {
		return strings.TrimSpace(string(out))
	}
	return runtime.GOOS
}

func cpuName() string {
	b, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		if k, v, ok := strings.Cut(line, ":"); ok && strings.TrimSpace(k) == "model name" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
