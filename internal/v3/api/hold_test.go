package api

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"timeshadedb/internal/v3/core"
	"timeshadedb/internal/v3/ingest"
)

// backlogBody is a source's backlog of n envelopes, one a second from start: a chunk
// update and a train sample each, for map gw-back.1.
func backlogBody(t *testing.T, src string, start core.TimeMs, from, n int) string {
	t.Helper()
	d := core.EncodeChart(chart(0x4444))
	var b strings.Builder
	for k := from; k < from+n; k++ {
		env := map[string]any{"v": 1, "tick": 60 * (k + 1), "src": src, "map": "gw-back.1", "recv_ms": int64(start) + int64(k)*1000, "seq": k + 1,
			"msgs": []any{
				map[string]any{"type": "chunk", "s": "nauvis", "f": "player", "x": k % 8, "y": 0, "d": d},
				map[string]any{"type": "move", "s": "nauvis", "m": []any{map[string]any{"j": "9.9.9", "k": "t", "f": "player", "name": "locomotive", "x": float64(k), "y": 0, "h": 0.25, "n": 1}}},
			}}
		raw, err := json.Marshal(env)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(raw)
		b.WriteByte('\n')
	}
	return b.String()
}

// TestSourceHoldsHorizon: a source posts a backlog an hour behind the wall clock in two
// parts (a forwarder catching up). Between them the commit loop runs; the horizon of the
// source's map stays behind its newest data, so the second part is not late. A caught_up
// envelope then releases the map and the loop closes everything. Without the hold the
// second part is late (the old behaviour, also kept with a hold of 0).
func TestSourceHoldsHorizon(t *testing.T) {
	for _, hold := range []time.Duration{time.Hour, 0} {
		t.Run(fmt.Sprint(hold), func(t *testing.T) {
			srcs, err := ingest.OpenSources("", hold, nil)
			if err != nil {
				t.Fatal(err)
			}
			f := newFixture(t, fixtureOpt{sources: srcs})
			ctx := context.Background()
			start := f.clock() - core.Hour
			first, err := f.in.Ingest(ctx, strings.NewReader(backlogBody(t, "standalone:back", start, 0, 200)))
			if err != nil || first.Samples.Accepted != 200 || first.Chunks.Accepted != 200 {
				t.Fatalf("first part: %+v, %v", first, err)
			}
			f.srv.Step(ctx)
			second, err := f.in.Ingest(ctx, strings.NewReader(backlogBody(t, "standalone:back", start, 200, 400)))
			if err != nil {
				t.Fatal(err)
			}
			sfc := core.SurfaceKey{Map: "gw-back.1", Surface: "nauvis"}
			if hold == 0 {
				if second.Samples.Late == 0 || second.Chunks.Late == 0 {
					t.Fatalf("without a hold the second part was not late: %+v", second)
				}
				return
			}
			if second.Samples.Accepted != 400 || second.Chunks.Accepted != 400 || second.Samples.Late+second.Chunks.Late != 0 {
				t.Fatalf("second part: %+v", second)
			}
			f.srv.Step(ctx)
			info := surfaceInfo(t, f, sfc)
			if info.Horizon > start+600*core.Second {
				t.Fatalf("horizon %d passed the source's data (last at %d)", info.Horizon, start+599*core.Second)
			}
			// Caught up: the map is released and the loop closes up to the clock.
			cu := fmt.Sprintf(`{"v":1,"tick":0,"src":"standalone:back","map":"gw-back.1","recv_ms":%d,"seq":601,"caught_up":true,"msgs":[]}`+"\n", f.clock())
			if _, err := f.in.Ingest(ctx, strings.NewReader(cu)); err != nil {
				t.Fatal(err)
			}
			f.srv.Step(ctx)
			info = surfaceInfo(t, f, sfc)
			if info.Horizon <= start+600*core.Second {
				t.Fatalf("horizon %d still held after caught_up", info.Horizon)
			}
			w, err := f.mov.Window(ctx, sfc, core.WindowStart(start+300*core.Second, core.MoverWindowLen))
			if err != nil || len(w.Rows) == 0 {
				t.Fatalf("window: %v", err)
			}
			stored := 0
			days, _ := f.mov.Surfaces(ctx)
			for _, si := range days {
				if si.Key != sfc {
					continue
				}
				for _, day := range si.Days {
					d, _ := f.mov.Day(ctx, sfc, day)
					for _, wr := range d.Windows {
						stored += wr.Rows
					}
				}
			}
			if stored != 600 {
				t.Fatalf("stored %d rows of 600 samples", stored)
			}
		})
	}
}

func surfaceInfo(t *testing.T, f *fixture, sfc core.SurfaceKey) core.SurfaceInfo {
	t.Helper()
	infos, err := f.mov.Surfaces(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, si := range infos {
		if si.Key == sfc {
			return si
		}
	}
	t.Fatalf("no surface %v", sfc)
	return core.SurfaceInfo{}
}
