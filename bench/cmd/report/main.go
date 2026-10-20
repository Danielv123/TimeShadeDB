// Command report turns result files into the PLAN.md §5 acceptance table
// (bench/REPORT.md): for every target in bench/targets.json, the plan's baseline, the
// measured baselines (TimeShadeDB master, illisis), the candidate's value, the target and
// PASS / FAIL / NOT RUN.
//
//	report -targets bench/targets.json -results bench/baselines,bench/results -candidate v3 -out bench/REPORT.md
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"timeshadedb/bench/internal/benchlib"
)

// Target is one row of bench/targets.json.
type Target struct {
	ID           string  `json:"id"`
	Group        string  `json:"group"`
	Title        string  `json:"title"`
	Tool         string  `json:"tool"`
	Scenario     string  `json:"scenario"`
	Metric       string  `json:"metric"`
	Op           string  `json:"op"`
	Value        float64 `json:"value"`
	Unit         string  `json:"unit"`
	PlanBaseline string  `json:"plan_baseline"`
}

type targetsFile struct {
	Targets []Target `json:"targets"`
}

func main() {
	targetsPath := flag.String("targets", "bench/targets.json", "targets file")
	results := flag.String("results", "bench/baselines,bench/results", "comma-separated directories (searched recursively) or files with result JSON")
	candidate := flag.String("candidate", "v3", "label of the system that must meet the targets")
	out := flag.String("out", "bench/REPORT.md", "report file ('-' for stdout)")
	strict := flag.Bool("strict", false, "exit 1 when a target fails or was not run")
	notes := flag.String("notes", "bench/baselines/NOTES.md,bench/results/NOTES.md", "comma-separated Markdown files appended to the report (how to read the baselines and the candidate run); missing ones are skipped")
	flag.Parse()

	b, err := os.ReadFile(*targetsPath)
	if err != nil {
		benchlib.Fatal(err)
	}
	var tf targetsFile
	if err := json.Unmarshal(b, &tf); err != nil {
		benchlib.Fatal(fmt.Errorf("%s: %w", *targetsPath, err))
	}
	all, err := loadResults(strings.Split(*results, ","))
	if err != nil {
		benchlib.Fatal(err)
	}
	md, failed := render(tf.Targets, all, *candidate)
	for _, n := range strings.Split(*notes, ",") {
		if n = strings.TrimSpace(n); n == "" {
			continue
		}
		if b, err := os.ReadFile(n); err == nil {
			md += "\n" + strings.TrimSpace(string(b)) + "\n"
		}
	}
	if *out == "-" {
		fmt.Print(md)
	} else {
		if err := os.WriteFile(*out, []byte(md), 0o644); err != nil {
			benchlib.Fatal(err)
		}
		fmt.Printf("wrote %s (%d results, %d targets)\n", *out, len(all), len(tf.Targets))
	}
	if *strict && failed > 0 {
		os.Exit(1)
	}
}

func loadResults(paths []string) ([]*benchlib.Result, error) {
	var out []*benchlib.Result
	for _, p := range paths {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		info, err := os.Stat(p)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			r, err := benchlib.ReadResult(p)
			if err != nil {
				return nil, err
			}
			out = append(out, r)
			continue
		}
		err = filepath.WalkDir(p, func(fp string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(fp, ".json") {
				return err
			}
			r, err := benchlib.ReadResult(fp)
			if err != nil || r.Tool == "" {
				return nil // not a result file
			}
			out = append(out, r)
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// pick returns the result value for the target and label. Tool is one tool name, "*"
// (any tool), or "a|b|c": the first of those tools that has the metric wins, so a metric
// two tools record in one scenario is read from the intended one. Scenario may list
// alternatives the same way (tried in order for each tool). Among results of the same
// tool and scenario the newest wins.
func pick(t Target, all []*benchlib.Result, label string) (float64, *benchlib.Result, bool) {
	for _, tool := range strings.Split(t.Tool, "|") {
		for _, scen := range strings.Split(t.Scenario, "|") {
			if v, r, ok := pickOne(t.Metric, tool, scen, all, label); ok {
				return v, r, true
			}
		}
	}
	return 0, nil, false
}

func pickOne(metric, tool, scenario string, all []*benchlib.Result, label string) (float64, *benchlib.Result, bool) {
	var best *benchlib.Result
	var v float64
	for _, r := range all {
		if r.Label != label || (tool != "*" && r.Tool != tool) || (scenario != "" && r.Scenario != scenario) {
			continue
		}
		x, ok := r.Metrics[metric]
		if !ok || math.IsNaN(x) {
			continue
		}
		if best == nil || r.Started.After(best.Started) {
			best, v = r, x
		}
	}
	return v, best, best != nil
}

func pass(t Target, v float64) bool {
	switch t.Op {
	case "<=":
		return v <= t.Value
	case "<":
		return v < t.Value
	case ">=":
		return v >= t.Value
	case ">":
		return v > t.Value
	case "==":
		return v == t.Value
	}
	return false
}

func render(targets []Target, all []*benchlib.Result, candidate string) (string, int) {
	labels := map[string]bool{}
	for _, r := range all {
		if r.Label != candidate {
			labels[r.Label] = true
		}
	}
	var baseLabels []string
	for l := range labels {
		baseLabels = append(baseLabels, l)
	}
	sort.Strings(baseLabels)

	var sb strings.Builder
	sb.WriteString("# TimeShadeDB v3 acceptance report\n\n")
	sb.WriteString("Generated by `bench/cmd/report` from `bench/targets.json` (PLAN.md section 5) on " +
		time.Now().UTC().Format("2006-01-02 15:04 UTC") + ". Do not edit by hand; rerun `bench/run_acceptance`.\n\n")
	fmt.Fprintf(&sb, "Candidate: `%s`. Measured baselines: %s.\n\n", candidate, joinCode(baseLabels))
	sb.WriteString("Status: PASS or FAIL for the candidate; NOT RUN when no result file has the metric. ")
	sb.WriteString("Baseline columns show measured values only; the plan's own baseline text is quoted in its column.\n\n")

	counts := map[string]int{}
	group := ""
	header := "| Target | Plan baseline |"
	sep := "|---|---|"
	for _, l := range baseLabels {
		header += " " + l + " |"
		sep += "---:|"
	}
	header += " " + candidate + " | Goal | Status |\n"
	sep += "---:|---|---|\n"
	failed := 0
	for _, t := range targets {
		if t.Group != group {
			group = t.Group
			fmt.Fprintf(&sb, "\n## %s\n\n%s%s", group, header, sep)
		}
		row := fmt.Sprintf("| %s<br>`%s` | %s |", t.Title, t.Metric, t.PlanBaseline)
		for _, l := range baseLabels {
			if v, _, ok := pick(t, all, l); ok {
				row += " " + fmtVal(v, t.Unit) + " |"
			} else {
				row += " |"
			}
		}
		status := "NOT RUN"
		if v, _, ok := pick(t, all, candidate); ok {
			row += " " + fmtVal(v, t.Unit) + " |"
			if pass(t, v) {
				status = "PASS"
			} else {
				status = "**FAIL**"
				failed++
			}
		} else {
			row += " |"
			failed++
		}
		counts[strings.Trim(status, "*")]++
		row += fmt.Sprintf(" %s %s %s | %s |\n", t.Op, benchlib.FormatFloat(t.Value), t.Unit, status)
		sb.WriteString(row)
	}
	fmt.Fprintf(&sb, "\n## Summary\n\nPASS %d, FAIL %d, NOT RUN %d of %d targets.\n", counts["PASS"], counts["FAIL"], counts["NOT RUN"], len(targets))

	// Provenance.
	sb.WriteString("\n## Result files\n\n| Label | Tool | Scenario | Started (UTC) | Machine | Notes |\n|---|---|---|---|---|---|\n")
	sorted := append([]*benchlib.Result(nil), all...)
	sort.Slice(sorted, func(i, j int) bool {
		a, b := sorted[i], sorted[j]
		if a.Label != b.Label {
			return a.Label < b.Label
		}
		if a.Tool != b.Tool {
			return a.Tool < b.Tool
		}
		return a.Started.Before(b.Started)
	})
	for _, r := range sorted {
		m := fmt.Sprintf("%s, %d CPUs, %s", r.Machine.CPU, r.Machine.CPUs, r.Machine.OS)
		notes := strings.ReplaceAll(strings.Join(r.Notes, "; "), "|", "/")
		fmt.Fprintf(&sb, "| %s | %s | %s | %s | %s | %s |\n", r.Label, r.Tool, r.Scenario, r.Started.Format("2006-01-02 15:04"), m, notes)
	}
	return sb.String(), failed
}

func joinCode(xs []string) string {
	if len(xs) == 0 {
		return "none"
	}
	return "`" + strings.Join(xs, "`, `") + "`"
}

func fmtVal(v float64, unit string) string {
	a := math.Abs(v)
	var s string
	switch {
	case a >= 1e6:
		s = fmt.Sprintf("%.2f M", v/1e6)
	case a >= 1e4:
		s = fmt.Sprintf("%.0f", v)
	case a >= 100:
		s = fmt.Sprintf("%.0f", v)
	case a >= 10:
		s = fmt.Sprintf("%.1f", v)
	default:
		s = fmt.Sprintf("%.3g", v)
	}
	return s + " " + unit
}
