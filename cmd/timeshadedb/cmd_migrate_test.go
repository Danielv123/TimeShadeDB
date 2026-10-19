package main

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"timeshadedb"
	"timeshadedb/internal/v3/core"
	"timeshadedb/internal/v3/core/memstore"
)

func writeSmallChunkDB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "v2.tshd")
	db, err := timeshadedb.Open(timeshadedb.OpenOptions{Path: path, Format: timeshadedb.FormatChunks})
	if err != nil {
		t.Fatal(err)
	}
	key := timeshadedb.DatastoreKey{SavefileUUID: "save-1", Surface: "nauvis", Force: "player"}
	var px [timeshadedb.ChunkPixelCount]uint16
	for tick := uint64(0); tick < 60*60*5; tick += 600 {
		for i := range px {
			px[i] = uint16(0x0841 * (1 + tick/600%7))
		}
		px[int(tick/600)%timeshadedb.ChunkPixelCount] = 0xf800
		rows := []timeshadedb.ParsedChunkRow{
			{Key: key, Tick: tick, Chunk: timeshadedb.ChunkCoord{X: -9, Y: 0}, Pixels: px},
			{Key: key, Tick: tick, Chunk: timeshadedb.ChunkCoord{X: 8, Y: 8}, Pixels: px},
		}
		if _, err := db.IngestChunkRows(context.Background(), rows); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestMigrateV3DryRun(t *testing.T) {
	path := writeSmallChunkDB(t)
	var out bytes.Buffer
	if err := runMigrateV3(context.Background(), []string{"--from", path, "--dry-run", "--map", "gw-test", "--epoch", "2026-10-01T00:00:00Z", "--verify", "10"}, &out); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	var res migrateV3Output
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if res.Migration.Rows != 60 || res.Migration.Applied.Accepted != 60 {
		t.Fatalf("migration = %+v", res.Migration)
	}
	if len(res.Migration.Datastores) != 1 || res.Migration.Datastores[0].To != (core.DatastoreKey{Map: "gw-test", Surface: "nauvis", Force: "player"}) {
		t.Fatalf("datastores = %+v", res.Migration.Datastores)
	}
	if got, want := res.Migration.Datastores[0].FirstT, core.TimeMs(1790812800000); got != want {
		t.Fatalf("first time = %d, want %d", got, want)
	}
	if res.Verify == nil || !res.Verify.OK() || len(res.Verify.Checks) != 10 {
		t.Fatalf("verify = %+v", res.Verify)
	}
}

func TestMigrateV3Targets(t *testing.T) {
	path := writeSmallChunkDB(t)
	var out bytes.Buffer
	err := runMigrateV3(context.Background(), []string{"--from", path, "--db", filepath.Join(t.TempDir(), "v3")}, &out)
	if err == nil || !strings.Contains(err.Error(), "no v3 terrain store") {
		t.Fatalf("missing store error = %v", err)
	}
	if err := runMigrateV3(context.Background(), []string{"--from", path}, &out); err == nil {
		t.Fatal("neither --db nor --dry-run accepted")
	}

	store := memstore.NewTerrain()
	opened := ""
	v3TerrainStoreOpener = func(ctx context.Context, dir string) (core.TerrainStore, error) {
		opened = dir
		return keepOpen{store}, nil
	}
	defer func() { v3TerrainStoreOpener = nil }()
	dir := filepath.Join(t.TempDir(), "v3")
	out.Reset()
	if err := runMigrateV3(context.Background(), []string{"--from", path, "--db", dir, "--window", "5m"}, &out); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if opened != dir {
		t.Fatalf("opened %q, want %q", opened, dir)
	}
	infos, err := store.Datastores(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 1 || infos[0].Key.Map != "save-1" || infos[0].Window != 5*core.Minute {
		t.Fatalf("v3 datastores = %+v", infos)
	}
}

// keepOpen ignores Close, so the test can inspect the store after the command.
type keepOpen struct{ core.TerrainStore }

func (keepOpen) Close() error { return nil }
