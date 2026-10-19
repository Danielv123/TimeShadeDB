package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"timeshadedb/internal/v3/core"
	"timeshadedb/internal/v3/movers"
	"timeshadedb/internal/v3/overlays"
	"timeshadedb/internal/v3/pyramid"
	"timeshadedb/internal/v3/terrain"
)

// This file wires the file-backed v3 stores (terrain and series from WP2, the pyramid
// from WP3, movers and overlays from WP4) into the offline commands: import-minimap
// --db and migrate-v3 --db. The server (WP6) opens the same stores with its own options.

func init() {
	v3TerrainStoreOpener = func(ctx context.Context, dir string) (core.TerrainStore, error) {
		return openV3BulkTerrain(dir)
	}
	v3TerrainFinisher = func(ctx context.Context, store core.TerrainStore) (any, error) {
		ter, ok := store.(*terrain.Store)
		if !ok {
			return nil, nil
		}
		st, err := buildV3Pyramid(ctx, ter)
		return st, err
	}
}

// openV3BulkTerrain opens the terrain store under a database root for an offline bulk
// write (import or migration): no fsync and no WAL, because an interrupted run is redone
// from its source, and no open-window budget, because the writer commits on its own
// schedule and has nothing to retry with. The writer must Commit past its last window
// before Close, since windows still open at Close are lost without a WAL.
func openV3BulkTerrain(dir string) (*terrain.Store, error) {
	return terrain.Open(dir, terrain.Options{NoSync: true, NoWAL: true, OpenWindowBytes: -1})
}

// openV3ImportStores opens every file-backed store under dir for a bulk import. The
// terrain store is returned too, so the caller can build the pyramid after the import.
func openV3ImportStores(dir string) (importStores, *terrain.Store, error) {
	ter, err := openV3BulkTerrain(dir)
	if err != nil {
		return importStores{}, nil, err
	}
	// The importer loads one surface after another, so the reference-clock bounds must
	// be off (movers.Options.MaxAhead/MaxBehind), and it commits at the end, so no WAL.
	mov, err := movers.Open(dir, movers.Options{SyncInterval: -1, NoWAL: true, MaxAhead: -1, MaxBehind: -1})
	if err != nil {
		return importStores{}, nil, errors.Join(err, ter.Close())
	}
	ovl, err := overlays.Open(dir, overlays.Options{NoSync: true})
	if err != nil {
		return importStores{}, nil, errors.Join(err, ter.Close(), mov.Close())
	}
	return importStores{Terrain: ter, Movers: mov, Overlays: ovl, Close: func() error {
		return errors.Join(ovl.Close(), mov.Close(), ter.Close())
	}}, ter, nil
}

// buildV3Pyramid builds the coarse levels of every datastore after an offline write:
// Rebuild replays the whole z0 history for a datastore without coarse records, and a
// datastore that already has some (an import into an existing database) gets a Flush at
// the horizon instead.
func buildV3Pyramid(ctx context.Context, ter *terrain.Store) (pyramid.BuildStats, error) {
	var total pyramid.BuildStats
	p, err := pyramid.New(ctx, ter, ter.SeriesStore(), pyramid.Options{})
	if err != nil {
		return total, err
	}
	infos, err := ter.Datastores(ctx)
	if err != nil {
		return total, errors.Join(err, p.Close())
	}
	flush := false
	for _, info := range infos {
		st, err := p.Rebuild(ctx, info.Key)
		if errors.Is(err, pyramid.ErrAlreadyBuilt) {
			flush = true
			continue
		}
		if err != nil {
			return total, errors.Join(fmt.Errorf("pyramid %v: %w", info.Key, err), p.Close())
		}
		addBuildStats(&total, st)
	}
	if flush {
		st, err := p.Flush(ctx)
		if err != nil {
			return total, errors.Join(err, p.Close())
		}
		addBuildStats(&total, st)
	}
	return total, p.Close()
}

func addBuildStats(t *pyramid.BuildStats, s pyramid.BuildStats) {
	t.Passes += s.Passes
	t.Built += s.Built
	t.Versions += s.Versions
	t.Snapshots += s.Snapshots
	t.Deltas += s.Deltas
	t.Unchanged += s.Unchanged
	t.Syncs += s.Syncs
	t.Bytes += s.Bytes
	t.SnapshotBytes += s.SnapshotBytes
}

// writeV3Maps stores the map metadata of an import in <db>/maps.json (docs/v3/FORMAT.md
// "On-disk layout"), as the core.MapInfo list the catalog serves.
func writeV3Maps(dir string, maps []core.MapInfo) error {
	if maps == nil {
		maps = []core.MapInfo{}
	}
	return writeJSONFile(filepath.Join(dir, "maps.json"), maps)
}

// dirBytes is the total size of the regular files under dir.
func dirBytes(dir string) (int64, error) {
	var n int64
	err := filepath.Walk(dir, func(_ string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if fi.Mode().IsRegular() {
			n += fi.Size()
		}
		return nil
	})
	return n, err
}
