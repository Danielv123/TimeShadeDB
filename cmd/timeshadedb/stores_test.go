package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"timeshadedb/internal/import/minimap/minimaptest"
	"timeshadedb/internal/core"
	"timeshadedb/internal/core/coretest"
	"timeshadedb/internal/movers"
	"timeshadedb/internal/overlays"
	"timeshadedb/internal/pyramid"
	"timeshadedb/internal/terrain"
)

// importIntoDB runs import-minimap --db --verify and returns its output.
func importIntoDB(t *testing.T, root, db string, extra ...string) string {
	t.Helper()
	var buf bytes.Buffer
	args := append([]string{"--db", db, "--verify", "--report", filepath.Join(t.TempDir(), "report.json")}, extra...)
	args = append(args, root)
	if err := runImportMinimap(context.Background(), &buf, args); err != nil {
		t.Fatalf("import-minimap --db: %v\n%s", err, buf.String())
	}
	return buf.String()
}

// checkReopenedDB reopens the stores written by import-minimap --db with their default
// (server) options and checks what a server would read: every datastore has z0 tiles
// and a pyramid whose latest coarse state is the z0 average, every surface has movers,
// and maps.json holds the map metadata. It returns the number of datastores.
func checkReopenedDB(t *testing.T, db string) int {
	t.Helper()
	ctx := context.Background()
	t0 := time.Now()
	ter, err := terrain.Open(db, terrain.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer ter.Close()
	p, err := pyramid.New(ctx, ter, ter.SeriesStore(), pyramid.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	t.Logf("terrain and pyramid opened in %v", time.Since(t0))
	infos, err := ter.Datastores(ctx)
	if err != nil || len(infos) == 0 {
		t.Fatalf("datastores: %v %v", infos, err)
	}
	for _, info := range infos {
		if info.Tiles == 0 || info.Horizon == 0 {
			t.Errorf("datastore %v: %+v", info.Key, info)
			continue
		}
		if _, ok := p.BuiltUpTo(info.Key); !ok {
			t.Errorf("datastore %v has no pyramid build", info.Key)
		}
		coarse, err := p.ListTiles(ctx, info.Key, -1)
		if err != nil || len(coarse) == 0 {
			t.Errorf("datastore %v: %d z-1 tiles (%v)", info.Key, len(coarse), err)
		}
		coretest.PyramidLatest(t, ter, p, info.Key, core.MinZ)
	}

	t0 = time.Now()
	mov, err := movers.Open(db, movers.Options{MaxAhead: -1, MinTime: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer mov.Close()
	t.Logf("movers opened in %v", time.Since(t0))
	sfcs, err := mov.Surfaces(ctx)
	if err != nil || len(sfcs) == 0 {
		t.Fatalf("mover surfaces: %v %v", sfcs, err)
	}

	t0 = time.Now()
	ovl, err := overlays.Open(db, overlays.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer ovl.Close()
	t.Logf("overlays opened in %v", time.Since(t0))

	var maps []core.MapInfo
	b, err := os.ReadFile(filepath.Join(db, "maps.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &maps); err != nil || len(maps) == 0 {
		t.Fatalf("maps.json: %s (%v)", b, err)
	}
	return len(infos)
}

func TestImportMinimapIntoDB(t *testing.T) {
	exp := minimaptest.Write(t, t.TempDir())
	db := filepath.Join(t.TempDir(), "db")
	out := importIntoDB(t, exp.Root, db, "--verify-history", "-1")
	for _, want := range []string{
		"verify terrain: final 2/2 areas equal",
		"verify movers gw-test~2E1.nauvis: match=true",
		"verify overlays gw-test~2E1.nauvis.player: match=true intervals=3",
		"pyramid: ",
		"bytes on disk",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	checkReopenedDB(t, db)

	// A second import into the same database would drop everything before the horizon
	// as late, so it is refused and the database is left as it was.
	err := runImportMinimap(context.Background(), &bytes.Buffer{}, []string{"--db", db, "--no-movers", "--no-overlays", exp.Root})
	if err == nil || !strings.Contains(err.Error(), "already holds") {
		t.Fatalf("second import: %v, want a refusal", err)
	}
	checkReopenedDB(t, db)
}

// TestImportMinimapTestdataIntoDB imports the illisis test data (TSDB_TESTDATA) into the
// file-backed stores, verifies it, and reopens the database as a server would.
func TestImportMinimapTestdataIntoDB(t *testing.T) {
	root := os.Getenv("TSDB_TESTDATA")
	if root == "" {
		t.Skip("TSDB_TESTDATA not set")
	}
	db := filepath.Join(t.TempDir(), "db")
	t0 := time.Now()
	out := importIntoDB(t, root, db)
	t.Logf("import with verification: %v", time.Since(t0))
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "import ") || strings.HasPrefix(line, "pyramid:") || strings.HasPrefix(line, "database ") || strings.HasPrefix(line, "verify terrain") {
			t.Log(line)
		}
	}
	t0 = time.Now()
	n := checkReopenedDB(t, db)
	t.Logf("%d datastores reopened and checked in %v", n, time.Since(t0))
}
