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
	// null] in time order; flags are core.MoverFlags.
	Rows [][]any `json:"rows"`
}

// cmdMovers exports the train journeys of one surface of a v3 database.
func cmdMovers(args []string) {
	fs := flag.NewFlagSet("movers", flag.ExitOnError)
	db := fs.String("db", "", "v3 database directory")
	surface := fs.String("surface", "gw-bc41437d~2E1.nauvis", "surface directory under movers/")
	kind := fs.String("kind", "t", "mover kind to export (t trains, v vehicles, p players)")
	out := fs.String("out", "", "output JSON file")
	fs.Parse(args)
	if *db == "" || *out == "" {
		benchlib.Fatal(fmt.Errorf("measure movers: -db and -out are required"))
	}
	ex, err := exportMovers(*db, *surface, core.MoverKind(*kind))
	if err != nil {
		benchlib.Fatal(err)
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
			st.j.Rows = append(st.j.Rows, []any{int64(r.T), tick, r.X, r.Y, orient, int(r.Flags), mi, cars})
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
