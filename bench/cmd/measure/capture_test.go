package main

import (
	"testing"

	"timeshadedb/bench/internal/benchlib"
)

// Lines in the format capture/test/run_game.mjs --mode bench prints.
const captureOut = "Factorio 2.1.19 at C:/Factorio/bin/x64/factorio.exe; sandbox C:/tmp/x\r\n" +
	"== bench: 100 moving trains, 50,176 virtual charted chunks, 4800 ticks (first 600 skipped), a resync at 3600\r\n" +
	"  bench pair 0: capture Lua cost 0.1950 ms/tick (baseline Lua 0.0300 ms, whole tick 0.6000 ms)\r\n" +
	"  bench pair 1: capture Lua cost 0.2100 ms/tick (baseline Lua 0.0300 ms, whole tick 0.6100 ms)\r\n" +
	"  WARN bench: the baseline's whole tick moved from 0.500 to 0.700 ms between pairs: the machine was busy, the costs are inflated\r\n" +
	"  bench: capture costs 0.1570 ms/tick of Lua time by the medians\r\n" +
	"  bench: capture costs 0.2025 ms/tick of Lua time (scriptUpdate + luaGarbageIncremental, mean), 0.2100 ms/tick of the whole tick; budget 0.3 ms\r\n" +
	"  bench: during a resync (400 ticks from tick 3600): Lua mean 0.4 ms with, mean 0.01 ms without; capture costs 0.3900 ms/tick\r\n" +
	"PASS (12 notes)\r\n"

func TestParseCaptureBench(t *testing.T) {
	res := benchlib.NewResult("capture", "v3", "v3")
	if err := parseCaptureBench(captureOut, res); err != nil {
		t.Fatal(err)
	}
	want := map[string]float64{
		"capture.ms_per_tick":          0.2025,
		"capture.whole_ms_per_tick":    0.21,
		"capture.ms_per_tick_p50":      0.157,
		"capture.resync_ms_per_tick":   0.39,
		"capture.pairs":                2,
		"capture.pair_min_ms_per_tick": 0.195,
		"capture.pair_max_ms_per_tick": 0.21,
	}
	for k, v := range want {
		if got, ok := res.Metrics[k]; !ok || got != v {
			t.Errorf("%s = %v (present %v), want %v", k, got, ok, v)
		}
	}
	if res.Params["factorio"] != "2.1.19" {
		t.Errorf("factorio param %v", res.Params["factorio"])
	}
	if len(res.Notes) != 1 {
		t.Errorf("notes %q, want the WARN line", res.Notes)
	}
	if err := parseCaptureBench("FAIL: 1 checks failed\n", benchlib.NewResult("capture", "v3", "v3")); err == nil {
		t.Error("no error for output without the summary line")
	}
}
