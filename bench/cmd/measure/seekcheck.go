package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"

	"github.com/klauspost/compress/zstd"

	"timeshadedb/bench/internal/benchlib"
	"timeshadedb/internal/v3/core"
	"timeshadedb/internal/v3/core/wire"
	"timeshadedb/internal/v3/movers"
)

// seekCheck is what `measure seekcheck` writes: for each source of first rows (the stored
// keyframes, the live payload rebuilt from disk, the live payload from memory of a store
// fed the rows again), how many journeys' first rows were checked, how many carried a body,
// and how many differ from the body trainBodies gives.
type seekCheck struct {
	Surface string               `json:"surface"`
	Checks  map[string]*checkSum `json:"checks"`
	Errors  []string             `json:"errors,omitempty"`
	// Keyframes as the store would write them at every -every ms (each journey's last train
	// row before the instant, unless a gone), zstd as stored: their count and bytes with the
	// bodies and without.
	SimKeyframes      int   `json:"sim_keyframes"`
	SimBytesWithBody  int64 `json:"sim_keyframe_bytes"`
	SimBytesOwnPieces int64 `json:"sim_keyframe_bytes_without_bodies"`
}

type checkSum struct {
	Payloads, Rows, WithBody, Pieces, Differ int
}

// cmdSeekCheck checks the bodies a v3 database's mover store serves for one surface (the
// rail pieces under a moving train in front of a keyframe row's or a live payload's first
// row's own) against trainBodies over its closed windows:
//
//   - every stored keyframe;
//   - Store.Live every -every ms of map time before the horizon (rebuilt from disk);
//   - Store.Live from memory of a fresh store fed the surface's train rows again in time
//     order, in 1 s batches, committing -grace behind the newest row, every -every ms.
func cmdSeekCheck(args []string) {
	fs := flag.NewFlagSet("seekcheck", flag.ExitOnError)
	db := fs.String("db", "", "v3 database directory")
	surface := fs.String("surface", "gw-bc41437d~2E1.nauvis", "surface directory under movers/")
	every := fs.Int64("every", 60000, "map ms between live payloads")
	grace := fs.Int64("grace", 15000, "commit this far behind the newest row (serve-v3 --mover-grace)")
	out := fs.String("out", "", "output JSON file (default stdout)")
	fs.Parse(args)
	if *db == "" {
		benchlib.Fatal(fmt.Errorf("measure seekcheck: -db is required"))
	}
	res, err := seekcheck(*db, *surface, core.TimeMs(*every), core.TimeMs(*grace))
	if err != nil {
		benchlib.Fatal(err)
	}
	b, _ := json.MarshalIndent(res, "", "  ")
	if *out == "" {
		fmt.Println(string(b))
		return
	}
	if err := os.WriteFile(*out, b, 0o644); err != nil {
		benchlib.Fatal(err)
	}
	for k, c := range res.Checks {
		fmt.Printf("%s: %d payloads, %d first rows, %d with a body (%d pieces), %d differ\n", k, c.Payloads, c.Rows, c.WithBody, c.Pieces, c.Differ)
	}
}

func seekcheck(db, surface string, every, grace core.TimeMs) (*seekCheck, error) {
	ctx := context.Background()
	key, err := core.ParseSurfaceID(surface)
	if err != nil {
		return nil, err
	}
	bodies, err := trainBodies(db, surface)
	if err != nil {
		return nil, err
	}
	windows, err := readMoverFiles(filepath.Join(db, "movers", surface, "windows"))
	if err != nil {
		return nil, err
	}
	rows := trainRows(windows)
	if len(rows) == 0 {
		return nil, fmt.Errorf("%s: no train rows", surface)
	}
	own := map[string]map[core.TimeMs]core.MoverRow{}
	for _, r := range rows {
		if own[r.id] == nil {
			own[r.id] = map[core.TimeMs]core.MoverRow{}
		}
		own[r.id][r.row.T] = r.row
	}
	res := &seekCheck{Surface: surface, Checks: map[string]*checkSum{}}
	check := func(name string, w *core.MoverWindow, firstOnly bool) {
		c := res.Checks[name]
		if c == nil {
			c = &checkSum{}
			res.Checks[name] = c
		}
		c.Payloads++
		seen := map[string]bool{}
		for _, r := range w.Rows {
			m := w.Movers[r.Mover]
			if m.Kind != core.KindTrain || (firstOnly && seen[m.ID]) {
				continue
			}
			seen[m.ID] = true
			o, ok := own[m.ID][r.T]
			if !ok {
				continue // a row only the payload has (none expected)
			}
			c.Rows++
			body := bodies[m.ID][r.T]
			if len(body) > 0 {
				c.WithBody++
				c.Pieces += len(body)
			}
			want := core.RailWithBody(body, o.Rail)
			if len(want) != len(r.Rail) || (len(want) > 0 && !reflect.DeepEqual(want, r.Rail)) {
				c.Differ++
				if len(res.Errors) < 20 {
					res.Errors = append(res.Errors, fmt.Sprintf("%s: %s at %d: %d pieces, want %d", name, m.ID, r.T, len(r.Rail), len(want)))
				}
			}
		}
	}
	// the stored keyframes and the live payload from disk
	st, err := movers.Open(db, movers.Options{NoWAL: true, SyncInterval: -1, MaxAhead: -1, MinTime: -1, Logf: func(string, ...any) {}})
	if err != nil {
		return nil, err
	}
	kfs, err := readMoverFiles(filepath.Join(db, "movers", surface, "keyframes"))
	if err != nil {
		st.Close()
		return nil, err
	}
	for _, kf := range kfs {
		check("keyframes", kf, false)
	}
	t0, t1 := rows[0].row.T, rows[len(rows)-1].row.T
	for now := t0 + every; now <= t1; now += every {
		w, err := st.Live(ctx, key, now)
		if err != nil {
			st.Close()
			return nil, err
		}
		check("live from disk", w, true)
	}
	st.Close()
	// keyframes at every instant, with and without the bodies
	enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedBestCompression), zstd.WithEncoderConcurrency(1),
		zstd.WithLowerEncoderMem(true), zstd.WithWindowSize(1<<22)) // as the store writes them
	if err != nil {
		return nil, err
	}
	defer enc.Close()
	size := func(w *core.MoverWindow) (int64, error) {
		b, err := wire.EncodeMoverWindow(w)
		if err != nil {
			return 0, err
		}
		return int64(len(enc.EncodeAll(b, nil))), nil
	}
	for now := t0 + every; now <= t1; now += every {
		last := map[string]trainRow{}
		for _, r := range rows {
			if r.row.T >= now {
				break
			}
			last[r.id] = r
		}
		with := &core.MoverWindow{Surface: key, Kind: core.WindowKeyframe, Start: now}
		without := &core.MoverWindow{Surface: key, Kind: core.WindowKeyframe, Start: now}
		ids := make([]string, 0, len(last))
		for id, r := range last {
			if r.row.Flags&core.FlagGone == 0 {
				ids = append(ids, id)
			}
		}
		sort.Strings(ids)
		for i, id := range ids {
			r := last[id]
			with.Movers = append(with.Movers, r.meta)
			without.Movers = append(without.Movers, r.meta)
			row := r.row
			row.Mover = i
			row.Flags &^= core.FlagHandoff
			without.Rows = append(without.Rows, row)
			row.Rail = core.RailWithBody(bodies[id][r.row.T], r.row.Rail)
			with.Rows = append(with.Rows, row)
		}
		a, err := size(with)
		if err != nil {
			return nil, err
		}
		b, err := size(without)
		if err != nil {
			return nil, err
		}
		res.SimKeyframes++
		res.SimBytesWithBody += a
		res.SimBytesOwnPieces += b
	}
	// a fresh store fed the train rows again: the live payload from memory
	tmp, err := os.MkdirTemp("", "seekcheck-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	fresh, err := movers.Open(tmp, movers.Options{NoWAL: true, SyncInterval: -1, MaxAhead: -1, MinTime: -1})
	if err != nil {
		return nil, err
	}
	defer fresh.Close()
	next := t0 + every
	for i := 0; i < len(rows); {
		batchEnd := core.WindowStart(rows[i].row.T, core.Second) + core.Second
		var batch []core.MoverSample
		for ; i < len(rows) && rows[i].row.T < batchEnd; i++ {
			batch = append(batch, sampleOf(key, rows[i]))
		}
		if _, err := fresh.Append(ctx, batch); err != nil {
			return nil, err
		}
		for ; next <= batchEnd && next <= t1; next += every {
			if err := fresh.Commit(ctx, next-grace); err != nil {
				return nil, err
			}
			w, err := fresh.Live(ctx, key, next)
			if err != nil {
				return nil, err
			}
			check("live from memory", w, true)
		}
	}
	return res, nil
}

// sampleOf turns a stored train row back into the sample that made it.
func sampleOf(key core.SurfaceKey, r trainRow) core.MoverSample {
	s := core.MoverSample{Surface: key, Meta: r.meta, T: r.row.T, Tick: r.row.Tick, X: r.row.X, Y: r.row.Y,
		Orientation: r.row.Orientation, Flags: r.row.Flags &^ core.FlagHandoff, Carriages: r.row.Carriages,
		Speed: r.row.Speed, Rail: r.row.Rail, RailStart: r.row.RailStart}
	return s
}
