package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"

	"timeshadedb"
	"timeshadedb/internal/v3/core"
	"timeshadedb/internal/v3/core/memstore"
	"timeshadedb/internal/v3/migrate"
)

func init() {
	register(Command{Name: "migrate-v3", Summary: "copy a v2 chunk database into a v3 terrain store and verify it", Group: GroupV3, Run: cmdMigrateV3})
}

// v3TerrainStoreOpener opens, creating it if needed, the v3 terrain store under a
// database root (docs/v3/FORMAT.md "On-disk layout"). The v3 terrain engine sets it
// from its own command file when it is built in; until then migrate-v3 supports only
// --dry-run, which migrates into the in-memory reference store.
var v3TerrainStoreOpener func(ctx context.Context, dir string) (core.TerrainStore, error)

type migrateV3Output struct {
	Target    string                `json:"target"`
	Migration migrate.Stats         `json:"migration"`
	Verify    *migrate.VerifyReport `json:"verify,omitempty"`
}

func cmdMigrateV3(ctx context.Context, args []string) error {
	return runMigrateV3(ctx, args, os.Stdout)
}

func runMigrateV3(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("migrate-v3", flag.ContinueOnError)
	from := fs.String("from", "", "v2 chunk database directory (opened read-only)")
	dbDir := fs.String("db", "", "v3 database root to write")
	dryRun := fs.Bool("dry-run", false, "migrate into an in-memory store and verify it, writing nothing")
	mapName := fs.String("map", "", "v3 map name (default: the v2 savefile UUID; only valid for a single savefile)")
	epoch := fs.String("epoch", "auto", `map-clock time of tick 0: "auto" (anchor each savefile's latest tick at its progress.json time), RFC 3339, or Unix milliseconds`)
	ups := fs.Uint64("ups", migrate.DefaultUPS, "game ticks per second used to convert ticks to milliseconds")
	window := fs.Duration("window", time.Duration(core.DefaultTerrainWindow)*time.Millisecond, "terrain window of the created v3 datastores")
	verify := fs.Int("verify", 20, "random ticks per datastore to compare pixel by pixel after migrating (0 disables)")
	seed := fs.Int64("seed", 1, "seed for the verification ticks")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *from == "" {
		return errors.New("migrate-v3 requires --from")
	}
	if (*dbDir == "") == !*dryRun {
		return errors.New("migrate-v3 requires exactly one of --db and --dry-run")
	}
	if *ups == 0 {
		return errors.New("migrate-v3: --ups must be positive")
	}
	if *window < time.Millisecond {
		return errors.New("migrate-v3: --window must be at least 1ms")
	}
	src, err := timeshadedb.Open(timeshadedb.OpenOptions{Path: *from, ReadOnly: true, Format: timeshadedb.FormatChunks})
	if err != nil {
		return err
	}
	defer src.Close()
	if src.Format() != timeshadedb.FormatChunks {
		return fmt.Errorf("migrate-v3: %s is not a chunk database", *from)
	}

	opts := migrate.Options{Window: core.TimeMs(window.Milliseconds())}
	if *epoch == "auto" {
		opts.Clock, err = migrate.AutoClocks(src, *ups)
		if err != nil {
			return err
		}
	} else {
		epochMs, err := parseEpoch(*epoch)
		if err != nil {
			return err
		}
		opts.Clock = migrate.FixedClock(migrate.Clock{EpochMs: epochMs, UPS: *ups})
	}
	if *mapName != "" {
		keys, err := migrate.Datastores(src)
		if err != nil {
			return err
		}
		saves := map[string]bool{}
		for _, k := range keys {
			saves[k.SavefileUUID] = true
		}
		if len(saves) > 1 {
			return fmt.Errorf("migrate-v3: --map needs a database with one savefile, %s has %d", *from, len(saves))
		}
		name := *mapName
		opts.MapKey = func(k timeshadedb.DatastoreKey) core.DatastoreKey {
			return core.DatastoreKey{Map: name, Surface: k.Surface, Force: k.Force}
		}
	}

	var dst core.TerrainStore
	out := migrateV3Output{}
	if *dryRun {
		dst = memstore.NewTerrain()
		out.Target = "memory (dry run)"
	} else {
		if v3TerrainStoreOpener == nil {
			return errors.New("migrate-v3: this binary has no v3 terrain store built in yet; use --dry-run to migrate into memory and verify")
		}
		dst, err = v3TerrainStoreOpener(ctx, *dbDir)
		if err != nil {
			return err
		}
		out.Target = *dbDir
	}
	defer dst.Close()

	out.Migration, err = migrate.Migrate(ctx, src, dst, opts)
	if err != nil {
		return err
	}
	if *verify > 0 {
		report, err := migrate.Verify(ctx, src, dst, opts, migrate.VerifyOptions{Samples: *verify, Seed: *seed, Window: opts.Window})
		if err != nil {
			return err
		}
		out.Verify = &report
	}
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		return err
	}
	if out.Verify != nil && !out.Verify.OK() {
		return fmt.Errorf("migrate-v3: verification found %d differing pixels", out.Verify.BadPixels)
	}
	return nil
}

func parseEpoch(s string) (core.TimeMs, error) {
	if ms, err := strconv.ParseInt(s, 10, 64); err == nil {
		return core.TimeMs(ms), nil
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return 0, fmt.Errorf("migrate-v3: --epoch must be auto, RFC 3339 or Unix milliseconds: %w", err)
	}
	return core.FromTime(t), nil
}
