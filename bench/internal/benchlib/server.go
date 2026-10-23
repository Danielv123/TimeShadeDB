package benchlib

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// MemInfo is a process's memory use in bytes.
type MemInfo struct {
	RSS     uint64 `json:"rss"`
	Private uint64 `json:"private"`
	PeakRSS uint64 `json:"peak_rss"`
}

// MiB converts bytes to MiB.
func MiB(b uint64) float64 { return float64(b) / (1 << 20) }

// Ports the harness must never use (shared with the Clusterio test suites and the
// default TimeShadeDB port).
var forbiddenPorts = map[int]bool{8880: true, 4443: true, 8080: true}

// FreePort returns a free loopback TCP port that is not one of the forbidden ones.
func FreePort() (int, error) {
	for i := 0; i < 20; i++ {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return 0, err
		}
		port := l.Addr().(*net.TCPAddr).Port
		l.Close()
		if !forbiddenPorts[port] {
			return port, nil
		}
	}
	return 0, errors.New("no free port")
}

// ServerSpec says how to start a server under test.
//
// Args may contain these placeholders:
//   - {addr}: replaced by 127.0.0.1:<free port> chosen by the harness (v2 `serve --addr`);
//   - {listen}: replaced by 127.0.0.1:0; the server must print "listening on <addr>" as
//     its first stdout line (v3 contract, docs/README.md);
//   - {db}: replaced by DB.
type ServerSpec struct {
	Binary string
	Args   []string
	DB     string
	// ReadyPath is polled (GET, expecting 200) until the server answers. Default
	// "/healthz" for v3 and "/api/chunk/saves" for v2.
	ReadyPath string
	// Timeout for the server to become ready. Default 120 s.
	Timeout time.Duration
	// Log receives the server's stdout and stderr (after the listening line). Nil
	// discards them.
	Log io.Writer
}

// Server is a running server under test.
type Server struct {
	Spec    ServerSpec
	BaseURL string
	PID     int
	// Startup is the time from process start until ReadyPath first answered 200.
	Startup time.Duration
	cmd     *exec.Cmd
	done    chan struct{}
	waitErr error
}

// StartServer starts the server and waits until it is ready.
func StartServer(spec ServerSpec) (*Server, error) {
	if spec.Timeout == 0 {
		spec.Timeout = 120 * time.Second
	}
	args := make([]string, len(spec.Args))
	var fixedAddr string
	announce := false
	for i, a := range spec.Args {
		if strings.Contains(a, "{addr}") {
			if fixedAddr == "" {
				port, err := FreePort()
				if err != nil {
					return nil, err
				}
				fixedAddr = fmt.Sprintf("127.0.0.1:%d", port)
			}
			a = strings.ReplaceAll(a, "{addr}", fixedAddr)
		}
		if strings.Contains(a, "{listen}") {
			announce = true
			a = strings.ReplaceAll(a, "{listen}", "127.0.0.1:0")
		}
		a = strings.ReplaceAll(a, "{db}", spec.DB)
		args[i] = a
	}
	if !announce && fixedAddr == "" {
		return nil, errors.New("server args need {addr} or {listen}")
	}
	cmd := exec.Command(spec.Binary, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	logw := spec.Log
	if logw == nil {
		logw = io.Discard
	}
	cmd.Stderr = logw
	start := time.Now()
	if err := StartTied(cmd); err != nil {
		return nil, err
	}
	s := &Server{Spec: spec, PID: cmd.Process.Pid, cmd: cmd, done: make(chan struct{})}
	track(s)
	addrCh := make(chan string, 1)
	go func() {
		r := bufio.NewReader(stdout)
		first := true
		for {
			line, err := r.ReadString('\n')
			if line != "" {
				if first && announce {
					first = false
					if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "listening on "); ok {
						addrCh <- rest
						continue
					}
					addrCh <- ""
				}
				io.WriteString(logw, line)
			}
			if err != nil {
				if first && announce {
					addrCh <- ""
				}
				return
			}
		}
	}()
	go func() {
		s.waitErr = cmd.Wait()
		close(s.done)
	}()
	addr := fixedAddr
	if announce {
		select {
		case addr = <-addrCh:
		case <-s.done:
			return nil, fmt.Errorf("server exited before listening: %v", s.waitErr)
		case <-time.After(spec.Timeout):
			s.Stop()
			return nil, errors.New("server did not print its listening line in time")
		}
		if addr == "" {
			s.Stop()
			return nil, errors.New(`server's first stdout line was not "listening on <addr>"`)
		}
		if host, port, err := net.SplitHostPort(addr); err == nil {
			if host == "" || host == "0.0.0.0" || host == "::" {
				addr = net.JoinHostPort("127.0.0.1", port)
			}
		}
	}
	s.BaseURL = "http://" + addr
	ready := spec.ReadyPath
	if ready == "" {
		if announce {
			ready = "/healthz"
		} else {
			ready = "/api/chunk/saves"
		}
	}
	client := &http.Client{Timeout: 5 * time.Second}
	deadline := start.Add(spec.Timeout)
	for {
		resp, err := client.Get(s.BaseURL + ready)
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		select {
		case <-s.done:
			return nil, fmt.Errorf("server exited during startup: %v", s.waitErr)
		default:
		}
		if time.Now().After(deadline) {
			s.Stop()
			return nil, fmt.Errorf("server not ready at %s after %s", ready, spec.Timeout)
		}
		time.Sleep(5 * time.Millisecond)
	}
	s.Startup = time.Since(start)
	return s, nil
}

// Memory samples the server's memory.
func (s *Server) Memory() (MemInfo, error) { return ProcessMemory(s.PID) }

// Exited reports whether the process has ended.
func (s *Server) Exited() bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}

// Stop kills the server and waits for it to exit. Windows has no SIGTERM for console
// children, so this is a hard kill on every platform; stores must survive it (the v3
// crash-safety requirement), and the v2 store is only reopened after ingest requests
// have returned.
func (s *Server) Stop() error {
	if s == nil || s.cmd == nil || s.cmd.Process == nil {
		return nil
	}
	defer untrack(s)
	if !s.Exited() {
		_ = s.cmd.Process.Kill()
	}
	select {
	case <-s.done:
	case <-time.After(30 * time.Second):
		return errors.New("server did not exit after kill")
	}
	return nil
}

// Every server StartServer started and Stop has not stopped yet, so that Fatal and an
// interrupt can stop them: deferred Stop calls do not run on os.Exit.
var (
	liveMu  sync.Mutex
	live    = map[*Server]bool{}
	sigOnce sync.Once
)

func track(s *Server) {
	liveMu.Lock()
	live[s] = true
	liveMu.Unlock()
	sigOnce.Do(func() {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
		go func() {
			sig := <-ch
			fmt.Fprintf(os.Stderr, "%v: stopping servers\n", sig)
			StopAll()
			os.Exit(130)
		}()
	})
}

func untrack(s *Server) {
	liveMu.Lock()
	delete(live, s)
	liveMu.Unlock()
}

// StopAll stops every server this process started that is still running.
func StopAll() {
	liveMu.Lock()
	all := make([]*Server, 0, len(live))
	for s := range live {
		all = append(all, s)
	}
	liveMu.Unlock()
	for _, s := range all {
		s.Stop()
	}
}

// StartTied starts cmd so that it cannot outlive this process: on Windows the child joins
// a kill-on-close Job Object, on Linux it gets SIGKILL when this process dies. If tying
// fails the child still runs (the caller stops it) and a warning is printed.
func StartTied(cmd *exec.Cmd) error {
	prepareChild(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	if err := adoptChild(cmd); err != nil {
		fmt.Fprintf(os.Stderr, "warning: child %d is not tied to this process: %v\n", cmd.Process.Pid, err)
	}
	return nil
}

// MemSampler samples a process's memory at a fixed interval in the background.
type MemSampler struct {
	mu      sync.Mutex
	samples []MemSample
	cancel  context.CancelFunc
	done    chan struct{}
}

// MemSample is one memory reading at T seconds after the sampler started.
type MemSample struct {
	T   float64
	Mem MemInfo
}

// SampleMemory starts sampling pid every interval.
func SampleMemory(pid int, interval time.Duration) *MemSampler {
	ctx, cancel := context.WithCancel(context.Background())
	m := &MemSampler{cancel: cancel, done: make(chan struct{})}
	start := time.Now()
	go func() {
		defer close(m.done)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			if mi, err := ProcessMemory(pid); err == nil {
				m.mu.Lock()
				m.samples = append(m.samples, MemSample{T: time.Since(start).Seconds(), Mem: mi})
				m.mu.Unlock()
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
	return m
}

// Stop ends sampling and returns the samples.
func (m *MemSampler) Stop() []MemSample {
	m.cancel()
	<-m.done
	return m.Samples()
}

// Samples returns a copy of the samples so far.
func (m *MemSampler) Samples() []MemSample {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]MemSample(nil), m.samples...)
}

// MaxRSS returns the largest RSS among samples.
func MaxRSS(samples []MemSample) uint64 {
	var m uint64
	for _, s := range samples {
		if s.Mem.RSS > m {
			m = s.Mem.RSS
		}
	}
	return m
}

// DirSize sums the sizes of all regular files under dir, and also per first-level
// entry (subdirectory or file name).
func DirSize(dir string) (total int64, byTop map[string]int64, err error) {
	byTop = map[string]int64{}
	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		top := strings.SplitN(filepath.ToSlash(rel), "/", 2)[0]
		byTop[top] += info.Size()
		total += info.Size()
		return nil
	})
	return total, byTop, err
}

// TempDir makes a fresh directory under base (or the OS temp dir) for a benchmark DB.
func TempDir(base, prefix string) (string, error) {
	if base != "" {
		if err := os.MkdirAll(base, 0o755); err != nil {
			return "", err
		}
	}
	return os.MkdirTemp(base, prefix)
}
