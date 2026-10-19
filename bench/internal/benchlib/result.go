// Package benchlib holds what the WP11 benchmark tools share: the result file format,
// statistics, process memory sampling, server launching, the v2 and v3 HTTP targets, and
// the synthetic workload generator.
package benchlib

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

// Result is one tool run. The report generator (bench/cmd/report) reads these files and
// matches Metrics keys against bench/targets.json.
type Result struct {
	// Tool is the program that produced the file (ingestbench, tilebench, ...).
	Tool string `json:"tool"`
	// Label names the system under test, for example "tsdb-master", "illisis" or "v3".
	Label string `json:"label"`
	// Target is the API flavour: "v2" (TimeShadeDB master), "v3", or "illisis".
	Target string `json:"target"`
	// Scenario names the workload ("testdata-nauvis", "synth", ...); targets.json
	// matches on it.
	Scenario string                  `json:"scenario,omitempty"`
	Started  time.Time               `json:"started"`
	Seconds  float64                 `json:"seconds"`
	Machine  Machine                 `json:"machine"`
	Params   map[string]any          `json:"params"`
	Metrics  map[string]float64      `json:"metrics"`
	Notes    []string                `json:"notes,omitempty"`
	Series   map[string][][2]float64 `json:"series,omitempty"`
	finished bool
}

// NewResult starts a result and records the machine.
func NewResult(tool, label, target string) *Result {
	return &Result{
		Tool:    tool,
		Label:   label,
		Target:  target,
		Started: time.Now().UTC(),
		Machine: DetectMachine(),
		Params:  map[string]any{},
		Metrics: map[string]float64{},
		Series:  map[string][][2]float64{},
	}
}

// Set records a metric.
func (r *Result) Set(key string, v float64) { r.Metrics[key] = v }

// Param records an input parameter.
func (r *Result) Param(key string, v any) { r.Params[key] = v }

// Note records a free-text remark (caveats, extrapolations, skipped parts).
func (r *Result) Note(format string, args ...any) {
	r.Notes = append(r.Notes, fmt.Sprintf(format, args...))
}

// Point appends (x, y) to a named series, for example RSS over time.
func (r *Result) Point(series string, x, y float64) {
	r.Series[series] = append(r.Series[series], [2]float64{x, y})
}

// SetSummary records the usual percentiles of a sample under prefix: prefix.p50, .p90,
// .p95, .p99, .max, .mean and .n.
func (r *Result) SetSummary(prefix string, s Summary) {
	r.Set(prefix+".n", float64(s.N))
	if s.N == 0 {
		return
	}
	r.Set(prefix+".p50", s.P50)
	r.Set(prefix+".p90", s.P90)
	r.Set(prefix+".p95", s.P95)
	r.Set(prefix+".p99", s.P99)
	r.Set(prefix+".max", s.Max)
	r.Set(prefix+".mean", s.Mean)
}

// Write finishes the result and writes it as indented JSON to path (directories are
// created). It also prints the metrics to stdout, sorted by key.
func (r *Result) Write(path string) error {
	if !r.finished {
		r.Seconds = time.Since(r.Started).Seconds()
		r.finished = true
	}
	r.Print()
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

// Print writes the metrics and notes to stdout.
func (r *Result) Print() {
	keys := make([]string, 0, len(r.Metrics))
	for k := range r.Metrics {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	fmt.Printf("== %s [%s, %s] %.1fs\n", r.Tool, r.Label, r.Target, r.Seconds)
	for _, k := range keys {
		fmt.Printf("  %-44s %s\n", k, FormatFloat(r.Metrics[k]))
	}
	for _, n := range r.Notes {
		fmt.Printf("  note: %s\n", n)
	}
}

// ReadResult loads a result file.
func ReadResult(path string) (*Result, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var r Result
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if r.Metrics == nil {
		r.Metrics = map[string]float64{}
	}
	r.finished = true
	return &r, nil
}

// FormatFloat prints integers without decimals and other values with up to 3 significant
// decimals.
func FormatFloat(v float64) string {
	if v == float64(int64(v)) && v < 1e15 && v > -1e15 {
		return fmt.Sprintf("%d", int64(v))
	}
	s := fmt.Sprintf("%.3f", v)
	if strings.Contains(s, ".") {
		s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	}
	return s
}

// Machine describes where a benchmark ran. Every report records it (PLAN.md §5).
type Machine struct {
	OS        string `json:"os"`
	Arch      string `json:"arch"`
	CPU       string `json:"cpu"`
	CPUs      int    `json:"cpus"`
	GoVersion string `json:"go"`
}

var (
	machineOnce sync.Once
	machine     Machine
)

// DetectMachine fills Machine from the runtime and the OS (once per process).
func DetectMachine() Machine {
	machineOnce.Do(func() {
		machine = Machine{
			OS:        osDescription(),
			Arch:      runtime.GOARCH,
			CPU:       cpuName(),
			CPUs:      runtime.NumCPU(),
			GoVersion: runtime.Version(),
		}
	})
	return machine
}
