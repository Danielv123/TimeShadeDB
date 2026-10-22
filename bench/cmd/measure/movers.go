package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/klauspost/compress/zstd"

	"timeshadedb/bench/internal/benchlib"
	"timeshadedb/internal/v3/core"
	"timeshadedb/internal/v3/core/wire"
)

// moversExport is the file `measure movers` writes and the train evaluator reads with
// --journeys-json (bench/trains/src/v3_data.ts): the train journeys of one surface as
// stored in a v3 database, so the evaluator can run on the imported (re-timed, deduped)
// rows instead of the illisis windows.
type moversExport struct {
	Source  string          `json:"source"`
	Surface string          `json:"surface"`
	Windows int             `json:"windows"`
	Rows    int             `json:"rows"`
	Journey []exportJourney `json:"journeys"`
}

type exportJourney struct {
	ID    string `json:"id"`
	Force string `json:"force"`
	Name  string `json:"name"`
	// Metas lists [carriages, kinds, colour] variants; each row's last element indexes it.
	Metas [][3]any `json:"metas"`
	// Rows are [t, tick or null, x, y, orientation or null, flags, meta, carriages or
	// null] in time order, and for rail path rows (core.FlagRail) also [speed, rail pieces
	// as [x, y, direction, kind] or null, rail start]; flags are core.MoverFlags. With
	// -bodies a rail path row also has its body: the pieces under the train that earlier rows
	// listed, as a keyframe or the live payload gives them in front of the row's own
	// (core.RailBody), or null.
	Rows [][]any `json:"rows"`
}

// cmdMovers exports the train journeys of one surface of a v3 database.
func cmdMovers(args []string) {
	fs := flag.NewFlagSet("movers", flag.ExitOnError)
	db := fs.String("db", "", "v3 database directory")
	surface := fs.String("surface", "gw-bc41437d~2E1.nauvis", "surface directory under movers/")
	kind := fs.String("kind", "t", "mover kind to export (t trains, v vehicles, p players)")
	out := fs.String("out", "", "output JSON file")
	bodies := fs.Bool("bodies", false, "add each rail path row's body as the store gives it (trainBodies)")
	fs.Parse(args)
	if *db == "" || *out == "" {
		benchlib.Fatal(fmt.Errorf("measure movers: -db and -out are required"))
	}
	ex, err := exportMovers(*db, *surface, core.MoverKind(*kind))
	if err != nil {
		benchlib.Fatal(err)
	}
	if *bodies {
		b, err := trainBodies(*db, *surface)
		if err != nil {
			benchlib.Fatal(err)
		}
		addBodies(ex, b)
	}
	b, err := json.Marshal(ex)
	if err != nil {
		benchlib.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		benchlib.Fatal(err)
	}
	if err := os.WriteFile(*out, b, 0o644); err != nil {
		benchlib.Fatal(err)
	}
	fmt.Printf("%d journeys, %d rows from %d windows -> %s\n", len(ex.Journey), ex.Rows, ex.Windows, *out)
}

func exportMovers(db, surface string, kind core.MoverKind) (*moversExport, error) {
	dec, err := zstd.NewReader(nil)
	if err != nil {
		return nil, err
	}
	defer dec.Close()
	root := filepath.Join(db, "movers", surface, "windows")
	var files []string
	err = filepath.WalkDir(root, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !e.IsDir() && strings.HasSuffix(p, ".mvw") {
			files = append(files, p)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	ex := &moversExport{Source: "v3", Surface: surface, Windows: len(files)}
	type jstate struct {
		j     *exportJourney
		metas map[[3]any]int
	}
	byID := map[string]*jstate{}
	for _, p := range files {
		z, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		b, err := dec.DecodeAll(z, nil)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		w, err := wire.DecodeMoverWindow(b, core.SurfaceKey{})
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		for _, r := range w.Rows {
			if r.Mover < 0 || r.Mover >= len(w.Movers) {
				return nil, fmt.Errorf("%s: row mover %d outside the table", p, r.Mover)
			}
			m := w.Movers[r.Mover]
			if m.Kind != kind {
				continue
			}
			st := byID[m.ID]
			if st == nil {
				st = &jstate{j: &exportJourney{ID: m.ID, Force: m.Force, Name: m.Name}, metas: map[[3]any]int{}}
				byID[m.ID] = st
			}
			key := [3]any{m.Carriages, m.Kinds, m.Color}
			mi, ok := st.metas[key]
			if !ok {
				mi = len(st.j.Metas)
				st.metas[key] = mi
				st.j.Metas = append(st.j.Metas, key)
			}
			var tick, orient, cars any
			if r.Flags&core.FlagNoTick == 0 {
				tick = r.Tick
			}
			if r.Flags&(core.FlagNoHeading|core.FlagGone) == 0 {
				orient = r.Orientation
			}
			if len(r.Carriages) > 0 {
				cs := make([][3]float64, len(r.Carriages))
				for i, c := range r.Carriages {
					cs[i] = [3]float64{c.X, c.Y, c.Orientation}
				}
				cars = cs
			}
			row := []any{int64(r.T), tick, r.X, r.Y, orient, int(r.Flags), mi, cars}
			if r.Flags&core.FlagRail != 0 {
				var rail any
				if len(r.Rail) > 0 {
					ps := make([][4]int, len(r.Rail))
					for i, p := range r.Rail {
						ps[i] = [4]int{int(p.X), int(p.Y), int(p.Dir), int(p.Kind)}
					}
					rail = ps
				}
				row = append(row, r.Speed, rail, r.RailStart)
			}
			st.j.Rows = append(st.j.Rows, row)
			ex.Rows++
		}
	}
	for _, st := range byID {
		rows := st.j.Rows
		sort.SliceStable(rows, func(a, b int) bool { return rows[a][0].(int64) < rows[b][0].(int64) })
		ex.Journey = append(ex.Journey, *st.j)
	}
	sort.Slice(ex.Journey, func(a, b int) bool { return ex.Journey[a].ID < ex.Journey[b].ID })
	return ex, nil
}

// addBodies appends to every rail path row its body (trainBodies), as pieces [x, y,
// direction, kind] or null.
func addBodies(ex *moversExport, bodies map[string]map[core.TimeMs][]core.RailPiece) {
	for ji := range ex.Journey {
		j := &ex.Journey[ji]
		for ri, row := range j.Rows {
			if len(row) <= 8 {
				continue
			}
			var out any
			if body := bodies[j.ID][core.TimeMs(row[0].(int64))]; len(body) > 0 {
				ps := make([][4]int, len(body))
				for i, p := range body {
					ps[i] = [4]int{int(p.X), int(p.Y), int(p.Dir), int(p.Kind)}
				}
				out = ps
			}
			j.Rows[ri] = append(row, out)
		}
	}
}

// readMoverFiles decodes every .mvw file under dir (windows/ or keyframes/ of a surface),
// in file name order.
func readMoverFiles(dir string) ([]*core.MoverWindow, error) {
	dec, err := zstd.NewReader(nil)
	if err != nil {
		return nil, err
	}
	defer dec.Close()
	var files []string
	err = filepath.WalkDir(dir, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return filepath.SkipDir
			}
			return err
		}
		if !e.IsDir() && strings.HasSuffix(p, ".mvw") {
			files = append(files, p)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	var out []*core.MoverWindow
	for _, p := range files {
		z, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		b, err := dec.DecodeAll(z, nil)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		w, err := wire.DecodeMoverWindow(b, core.SurfaceKey{})
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		out = append(out, w)
	}
	return out, nil
}

// trainRow is a train row of a closed window with its journey's carriage count.
type trainRow struct {
	id   string
	n    int
	meta core.MoverMeta
	row  core.MoverRow
}

func (r *trainRow) railRow() core.RailRow {
	return core.RailRow{T: r.row.T, X: r.row.X, Y: r.row.Y, Orientation: r.row.Orientation, Flags: r.row.Flags,
		Rail: r.row.Rail, RailStart: r.row.RailStart, Carriages: r.n}
}

// trainRows returns the train rows of windows sorted by time (and journey).
func trainRows(windows []*core.MoverWindow) []trainRow {
	var rows []trainRow
	for _, w := range windows {
		for _, r := range w.Rows {
			if m := w.Movers[r.Mover]; m.Kind == core.KindTrain {
				rows = append(rows, trainRow{m.ID, m.Carriages, m, r})
			}
		}
	}
	sort.SliceStable(rows, func(a, b int) bool {
		if rows[a].row.T != rows[b].row.T {
			return rows[a].row.T < rows[b].row.T
		}
		return rows[a].id < rows[b].id
	})
	return rows
}

// trainBodies returns the body of every train row of a surface's closed windows as the
// mover store gives it (core.RailBody over each journey's rows in time order, begun again at
// each keyframe from the keyframe's row, internal/v3/movers): keyed by journey and time.
func trainBodies(db, surface string) (map[string]map[core.TimeMs][]core.RailPiece, error) {
	sdir := filepath.Join(db, "movers", surface)
	windows, err := readMoverFiles(filepath.Join(sdir, "windows"))
	if err != nil {
		return nil, err
	}
	keyframes, err := readMoverFiles(filepath.Join(sdir, "keyframes"))
	if err != nil {
		return nil, err
	}
	out := map[string]map[core.TimeMs][]core.RailPiece{}
	rbs := map[string]*core.RailBody{}
	k := 0
	for _, r := range trainRows(windows) {
		for k < len(keyframes) && keyframes[k].Start <= r.row.T {
			rbs = map[string]*core.RailBody{}
			kf := keyframes[k]
			for _, x := range kf.Rows {
				if m := kf.Movers[x.Mover]; m.Kind == core.KindTrain {
					tr := trainRow{m.ID, m.Carriages, m, x}
					rbs[m.ID] = new(core.RailBody)
					rbs[m.ID].Seed(tr.railRow())
				}
			}
			k++
		}
		rb := rbs[r.id]
		if rb == nil {
			rb = new(core.RailBody)
			rbs[r.id] = rb
		}
		if out[r.id] == nil {
			out[r.id] = map[core.TimeMs][]core.RailPiece{}
		}
		if body := rb.Add(r.railRow()); len(body) > 0 {
			out[r.id][r.row.T] = body
		}
	}
	return out, nil
}
