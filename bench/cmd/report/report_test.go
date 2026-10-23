package main

import (
	"strings"
	"testing"
	"time"

	"timeshadedb/bench/internal/benchlib"
)

func TestRenderStatuses(t *testing.T) {
	targets := []Target{
		{ID: "a", Group: "G", Title: "rate", Tool: "ingestbench", Scenario: "synth", Metric: "ingest.chunk_rows_per_s", Op: ">=", Value: 100000, Unit: "updates/s"},
		{ID: "b", Group: "G", Title: "disk", Tool: "*", Scenario: "testdata", Metric: "disk.b_per_chunk_hour_z0", Op: "<=", Value: 4, Unit: "B"},
		{ID: "c", Group: "H", Title: "missing", Tool: "trains", Metric: "trains.loo_p90", Op: "<=", Value: 0.6, Unit: "tiles"},
	}
	mk := func(label, tool, scen string, m map[string]float64, age time.Duration) *benchlib.Result {
		r := benchlib.NewResult(tool, label, label)
		r.Scenario = scen
		r.Metrics = m
		r.Started = time.Now().Add(-age)
		return r
	}
	all := []*benchlib.Result{
		mk("tsdb-master", "ingestbench", "synth", map[string]float64{"ingest.chunk_rows_per_s": 59000}, 0),
		mk("v3", "ingestbench", "synth", map[string]float64{"ingest.chunk_rows_per_s": 90000}, time.Hour),
		mk("v3", "ingestbench", "synth", map[string]float64{"ingest.chunk_rows_per_s": 150000}, 0), // newest wins
		mk("v3", "measure-v3disk", "testdata", map[string]float64{"disk.b_per_chunk_hour_z0": 5.2}, 0),
		mk("illisis", "measure-illisis", "testdata", map[string]float64{"disk.b_per_chunk_hour_z0": 69}, 0),
		mk("v3", "ingestbench", "other", map[string]float64{"disk.b_per_chunk_hour_z0": 1}, 0), // wrong scenario
	}
	md, failed := render(targets, all, "v3")
	for _, want := range []string{"| 150000 updates/s | >= 100000 updates/s | PASS |", "**FAIL**", "NOT RUN", "| 69.0 B |", "59000 updates/s", "PASS 1, FAIL 1, NOT RUN 1 of 3"} {
		if !strings.Contains(md, want) {
			t.Errorf("report lacks %q:\n%s", want, md)
		}
	}
	if failed != 2 {
		t.Fatalf("failed = %d, want 2 (one FAIL, one NOT RUN)", failed)
	}
}

func TestToolPriority(t *testing.T) {
	disk := Target{ID: "d", Group: "G", Title: "disk", Tool: "measure-v3disk|measure-illisis|ingestbench", Scenario: "testdata", Metric: "disk.b_per_chunk_hour_z0", Op: "<=", Value: 4, Unit: "B"}
	mk := func(label, tool string, v float64, age time.Duration) *benchlib.Result {
		r := benchlib.NewResult(tool, label, label)
		r.Scenario = "testdata"
		r.Metrics = map[string]float64{"disk.b_per_chunk_hour_z0": v}
		r.Started = time.Now().Add(-age)
		return r
	}
	all := []*benchlib.Result{
		mk("v3", "measure-v3disk", 3.1, time.Hour), // the imported database, measured first
		mk("v3", "ingestbench", 9.9, 0),            // a later re-ingest over HTTP must not win
		mk("tsdb-master", "ingestbench", 8.9, 0),   // the only source for v2
	}
	if v, r, ok := pick(disk, all, "v3"); !ok || v != 3.1 || r.Tool != "measure-v3disk" {
		t.Fatalf("v3 picked %v from %v", v, r)
	}
	if v, _, ok := pick(disk, all, "tsdb-master"); !ok || v != 8.9 {
		t.Fatalf("master picked %v", v)
	}
	any := disk
	any.Tool = "*"
	if v, _, _ := pick(any, all, "v3"); v != 9.9 {
		t.Fatalf("'*' should take the newest, got %v", v)
	}
}
