package timeshadedb

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestNewDatastoresWriteDatastoreVersion(t *testing.T) {
	for _, format := range []string{FormatLegacyTiles, FormatChunks} {
		t.Run(format, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "db.tshd")
			db, err := Open(OpenOptions{Path: path, Format: format})
			if err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			m, err := readManifest(path)
			if err != nil {
				t.Fatal(err)
			}
			version, err := datastoreVersion(m)
			if err != nil {
				t.Fatal(err)
			}
			if version != currentDatastoreVersion {
				t.Fatalf("datastore version = %d, want %d", version, currentDatastoreVersion)
			}
		})
	}
}

func TestMissingDatastoreVersionUsesBaseline(t *testing.T) {
	m, err := decodeDataManifest([]byte(`{"format":"timeShadeDB"}`))
	if err != nil {
		t.Fatal(err)
	}
	version, err := datastoreVersion(m)
	if err != nil {
		t.Fatal(err)
	}
	if version != baselineDatastoreVersion {
		t.Fatalf("missing datastore version = %d, want %d", version, baselineDatastoreVersion)
	}
}

func TestGenerationNameValidation(t *testing.T) {
	for name, want := range map[string]bool{
		"v000002-abc123": true,
		"../outside":     false,
		`C:stream`:       false,
		"":               false,
	} {
		if got := validGenerationName(name); got != want {
			t.Errorf("validGenerationName(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestMigrationValidation(t *testing.T) {
	noop := func(string) error { return nil }
	tests := []struct {
		name       string
		migrations []datastoreMigration
		ok         bool
	}{
		{name: "none", ok: true},
		{name: "valid", migrations: []datastoreMigration{{name: "one", run: noop}}, ok: true},
		{name: "missing name", migrations: []datastoreMigration{{run: noop}}},
		{name: "missing run", migrations: []datastoreMigration{{name: "one"}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateMigrations(test.migrations)
			if (err == nil) != test.ok {
				t.Fatalf("validateMigrations() error = %v, want success %v", err, test.ok)
			}
		})
	}
}

func TestIncrementalMigrationPublishesOneGeneration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db.tshd")
	key := DatastoreKey{SavefileUUID: "save-1", Surface: "nauvis", Force: "player"}
	coord := ChunkCoord{X: 2, Y: -3}
	pixels := make([]uint16, ChunkPixelCount)
	pixels[7] = 0x2462
	db, err := Open(OpenOptions{Path: path, Format: FormatChunks})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.IngestChunk(context.Background(), ChunkIngest{Key: key, Tick: 10, Chunk: coord, Pixels: pixels}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	var calls []string
	migrations := []datastoreMigration{
		{
			name: "one-to-two",
			run: func(stage string) error {
				calls = append(calls, "run-1")
				return os.WriteFile(filepath.Join(stage, "v2.marker"), []byte("ok"), 0o644)
			},
		},
		{
			name: "two-to-three",
			run: func(stage string) error {
				calls = append(calls, "run-2")
				if _, err := os.Stat(filepath.Join(stage, "v2.marker")); err != nil {
					return err
				}
				m, err := readDataManifest(stage)
				if err != nil {
					return err
				}
				version, err := datastoreVersion(m)
				if err != nil {
					return err
				}
				if version != 2 {
					return errors.New("first migration version was not published")
				}
				return nil
			},
		},
	}

	db, err = openExistingDBWithMigrations(OpenOptions{Path: path}, migrations)
	if err != nil {
		t.Fatal(err)
	}
	result, err := db.ChunkAt(context.Background(), ChunkAtOptions{Key: key, Tick: 10, Chunk: coord})
	if err != nil {
		t.Fatal(err)
	}
	if result.Pixels[7] != 0x2462 {
		t.Fatalf("migrated pixel = %#04x, want 0x2462", result.Pixels[7])
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	dataPath, m, err := resolveDatastore(path)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Clean(dataPath) == filepath.Clean(path) {
		t.Fatal("migration did not publish a managed generation")
	}
	version, err := datastoreVersion(m)
	if err != nil {
		t.Fatal(err)
	}
	if version != 3 {
		t.Fatalf("selected version = %d, want 3", version)
	}

	db, err = openExistingDBWithMigrations(OpenOptions{Path: path, ReadOnly: true}, migrations)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if want := []string{"run-1", "run-2"}; !slices.Equal(calls, want) {
		t.Fatalf("migration calls = %v, want %v", calls, want)
	}
}

func TestFailedMigrationLeavesFlatDatastoreSelected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db.tshd")
	db, err := Open(OpenOptions{Path: path, Format: FormatChunks})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("stop")
	migrations := []datastoreMigration{{
		name: "fail",
		run: func(stage string) error {
			if err := os.WriteFile(filepath.Join(stage, "partial"), []byte("partial"), 0o644); err != nil {
				return err
			}
			return failure
		},
	}}
	if _, err := openExistingDBWithMigrations(OpenOptions{Path: path}, migrations); !errors.Is(err, failure) {
		t.Fatalf("migration error = %v, want %v", err, failure)
	}
	dataPath, m, err := resolveDatastore(path)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Clean(dataPath) != filepath.Clean(path) {
		t.Fatal("failed migration selected a generation")
	}
	version, err := datastoreVersion(m)
	if err != nil || version != 1 {
		t.Fatalf("active datastore version = %d, error %v", version, err)
	}
}

func TestVersionChecksDoNotModifyDatastore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db.tshd")
	db, err := Open(OpenOptions{Path: path, Format: FormatChunks})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(path, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	noop := func(string) error { return nil }
	migrations := []datastoreMigration{{name: "one", run: noop}}
	if _, err := openExistingDBWithMigrations(OpenOptions{Path: path, ReadOnly: true}, migrations); err == nil || !strings.Contains(err.Error(), "requires migration") {
		t.Fatalf("read-only old version error = %v", err)
	}
	after, err := os.ReadFile(filepath.Join(path, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("read-only version check modified the manifest")
	}

	m, err := readDataManifest(path)
	if err != nil {
		t.Fatal(err)
	}
	m.DatastoreVersion = currentDatastoreVersion + 1
	if err := writeDataManifest(path, m); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(OpenOptions{Path: path}); err == nil || !strings.Contains(err.Error(), "newer") {
		t.Fatalf("future version error = %v", err)
	}
	m.DatastoreVersion = 0
	if err := writeDataManifest(path, m); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(OpenOptions{Path: path}); err == nil || !strings.Contains(err.Error(), "invalid datastore version") {
		t.Fatalf("invalid version error = %v", err)
	}
}

func TestLegacyTileMigrationPreservesHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db.tshd")
	ts := time.Unix(123, 0).UTC()
	db, err := Open(OpenOptions{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.IngestPlacement(ts, 4, 5, RGB{R: 1, G: 2, B: 3}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	noop := func(string) error { return nil }
	migrations := []datastoreMigration{{name: "one", run: noop}}
	db, err = openExistingDBWithMigrations(OpenOptions{Path: path}, migrations)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	result, err := db.TileAt(context.Background(), TileAtOptions{Timestamp: ts, Tile: TileCoord{X: 0, Y: 0}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Pixels[5*result.Width+4] != 1 {
		t.Fatalf("migrated legacy pixel = %d, want 1", result.Pixels[5*result.Width+4])
	}
}
