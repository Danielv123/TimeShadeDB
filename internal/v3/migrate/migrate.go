// Package migrate copies a TimeShadeDB v2 chunk database into a v3 terrain store
// (PLAN.md §6 WP1, `timeshadedb migrate-v3`).
//
// v2 keys chunk history by game tick and datastore (savefile, surface, force); v3 keys
// it by map-clock milliseconds and (map, surface, force). The migration
//
//   - maps each v2 datastore to a v3 key (by default the savefile UUID becomes the map
//     name; see Options.MapKey),
//   - converts ticks to milliseconds with a Clock (an epoch plus ticks at a fixed UPS),
//   - streams every stored v2 row, oldest first, into TerrainStore.Apply as a full
//     chunk update, and commits closed windows as it goes.
//
// v3 keeps only the net change per chunk and window, so intermediate states inside a
// window are dropped by design. At every window boundary B the migrated store equals
// the v2 database at the last tick before B; Verify checks exactly that.
package migrate

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"timeshadedb"
	"timeshadedb/internal/v3/core"
)

// Clock converts v2 game ticks to v3 map-clock milliseconds:
// t = EpochMs + floor(tick * 1000 / UPS).
type Clock struct {
	// EpochMs is the map-clock time of tick 0.
	EpochMs core.TimeMs
	// UPS is the game speed in ticks per second (Factorio: 60).
	UPS uint64
}

// DefaultUPS is Factorio's normal game speed.
const DefaultUPS = 60

func (c Clock) ups() uint64 {
	if c.UPS == 0 {
		return DefaultUPS
	}
	return c.UPS
}

// TimeOf returns the map-clock time of a tick.
func (c Clock) TimeOf(tick uint64) core.TimeMs {
	return c.EpochMs + core.TimeMs(tick*1000/c.ups())
}

// LastTickBefore returns the greatest tick whose time is before t, and false when no
// tick is (t <= TimeOf(0)).
func (c Clock) LastTickBefore(t core.TimeMs) (uint64, bool) {
	d := int64(t - c.EpochMs)
	if d <= 0 {
		return 0, false
	}
	// floor(tick*1000/ups) < d  <=>  tick*1000 < d*ups  <=>  tick < d*ups/1000.
	ups := c.ups()
	return (uint64(d)*ups+999)/1000 - 1, true
}

// AnchorClock returns a clock that puts latestTick at wall-clock time at: the v2
// database stores no wall time, but its progress file was written when the latest row
// arrived.
func AnchorClock(latestTick uint64, at time.Time, ups uint64) Clock {
	c := Clock{UPS: ups}
	c.EpochMs = core.FromTime(at) - core.TimeMs(latestTick*1000/c.ups())
	return c
}

// Options control a migration.
type Options struct {
	// MapKey maps a v2 datastore to its v3 key. Nil uses DefaultMapKey.
	MapKey func(timeshadedb.DatastoreKey) core.DatastoreKey
	// Clock converts ticks for one savefile. Nil is an error.
	Clock func(savefileUUID string) (Clock, error)
	// Window is the terrain window of the created datastores (0: core.DefaultTerrainWindow).
	Window core.TimeMs
	// BatchSize is the number of updates per Apply call (0: 4096).
	BatchSize int
	// CommitEvery is how many windows of history may stay open before a commit
	// (0: 16). Commits only ever close windows whose rows have all been applied.
	CommitEvery int
	// AllowExisting lets the migration write into v3 datastores that already exist.
	// Rows older than such a datastore's horizon are then dropped as late.
	AllowExisting bool
	// Progress, if set, is called after each datastore.
	Progress func(DatastoreStats)
}

// DefaultMapKey uses the savefile UUID as the v3 map name.
func DefaultMapKey(k timeshadedb.DatastoreKey) core.DatastoreKey {
	return core.DatastoreKey{Map: k.SavefileUUID, Surface: k.Surface, Force: k.Force}
}

// DatastoreStats describes the migration of one datastore.
type DatastoreStats struct {
	From      timeshadedb.DatastoreKey `json:"from"`
	To        core.DatastoreKey        `json:"to"`
	Clock     Clock                    `json:"clock"`
	Rows      int                      `json:"rows"`
	Chunks    int                      `json:"chunks"`
	FirstTick uint64                   `json:"first_tick"`
	LastTick  uint64                   `json:"last_tick"`
	FirstT    core.TimeMs              `json:"first_t"`
	LastT     core.TimeMs              `json:"last_t"`
	Applied   core.ApplyResult         `json:"applied"`
	Commits   int                      `json:"commits"`
	Duration  time.Duration            `json:"duration_ns"`
}

// Stats sums a migration.
type Stats struct {
	Datastores []DatastoreStats `json:"datastores"`
	Rows       int              `json:"rows"`
	Applied    core.ApplyResult `json:"applied"`
	Duration   time.Duration    `json:"duration_ns"`
}

// Datastores lists the v2 datastores of src, sorted by (savefile, force, surface).
func Datastores(src *timeshadedb.DB) ([]timeshadedb.DatastoreKey, error) {
	catalog, err := src.ChunkSaveCatalog()
	if err != nil {
		return nil, err
	}
	var out []timeshadedb.DatastoreKey
	for _, save := range catalog.Saves {
		for _, ds := range save.Datastores {
			out = append(out, timeshadedb.DatastoreKey{SavefileUUID: save.SavefileUUID, Surface: ds.Surface, Force: ds.Force})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.SavefileUUID != b.SavefileUUID {
			return a.SavefileUUID < b.SavefileUUID
		}
		if a.Force != b.Force {
			return a.Force < b.Force
		}
		return a.Surface < b.Surface
	})
	return out, nil
}

// AutoClocks anchors each savefile's clock at the progress file of its datastore with
// the greatest latest tick (see AnchorClock). All datastores of a savefile share one
// game clock.
func AutoClocks(src *timeshadedb.DB, ups uint64) (func(string) (Clock, error), error) {
	catalog, err := src.ChunkSaveCatalog()
	if err != nil {
		return nil, err
	}
	clocks := map[string]Clock{}
	for _, save := range catalog.Saves {
		var best *timeshadedb.ChunkDatastoreSummary
		for i := range save.Datastores {
			if best == nil || save.Datastores[i].LatestTick > best.LatestTick {
				best = &save.Datastores[i]
			}
		}
		if best == nil {
			continue
		}
		key := timeshadedb.DatastoreKey{SavefileUUID: save.SavefileUUID, Surface: best.Surface, Force: best.Force}
		at, err := src.ChunkDatastoreProgressTime(key)
		if err != nil {
			return nil, fmt.Errorf("anchor clock for %s: %w", save.SavefileUUID, err)
		}
		clocks[save.SavefileUUID] = AnchorClock(best.LatestTick, at, ups)
	}
	return func(save string) (Clock, error) {
		c, ok := clocks[save]
		if !ok {
			return Clock{}, fmt.Errorf("no clock for savefile %q", save)
		}
		return c, nil
	}, nil
}

// FixedClock returns the same clock for every savefile.
func FixedClock(c Clock) func(string) (Clock, error) {
	return func(string) (Clock, error) { return c, nil }
}

// Migrate copies every datastore of src into dst. It fails before writing anything
// when two v2 datastores map to the same v3 key, or (unless AllowExisting) when a
// target datastore already exists.
func Migrate(ctx context.Context, src *timeshadedb.DB, dst core.TerrainStore, opts Options) (Stats, error) {
	start := time.Now()
	var stats Stats
	if opts.Clock == nil {
		return stats, errors.New("migrate: Options.Clock is required")
	}
	mapKey := opts.MapKey
	if mapKey == nil {
		mapKey = DefaultMapKey
	}
	keys, err := Datastores(src)
	if err != nil {
		return stats, err
	}
	existing := map[core.DatastoreKey]bool{}
	infos, err := dst.Datastores(ctx)
	if err != nil {
		return stats, err
	}
	for _, info := range infos {
		existing[info.Key] = true
	}
	targets := map[core.DatastoreKey]timeshadedb.DatastoreKey{}
	for _, k := range keys {
		to := mapKey(k)
		if err := to.Validate(); err != nil {
			return stats, fmt.Errorf("migrate: v2 datastore %s/%s/%s: %w", k.SavefileUUID, k.Surface, k.Force, err)
		}
		if prev, dup := targets[to]; dup {
			return stats, fmt.Errorf("migrate: v2 datastores %s/%s/%s and %s/%s/%s both map to %s", prev.SavefileUUID, prev.Surface, prev.Force, k.SavefileUUID, k.Surface, k.Force, to.ID())
		}
		if existing[to] && !opts.AllowExisting {
			return stats, fmt.Errorf("migrate: v3 datastore %s already exists", to.ID())
		}
		if _, err := opts.Clock(k.SavefileUUID); err != nil {
			return stats, err
		}
		targets[to] = k
	}
	for _, k := range keys {
		ds, err := migrateDatastore(ctx, src, dst, k, mapKey(k), opts)
		if err != nil {
			return stats, err
		}
		stats.Datastores = append(stats.Datastores, ds)
		stats.Rows += ds.Rows
		stats.Applied.Add(ds.Applied)
		if opts.Progress != nil {
			opts.Progress(ds)
		}
	}
	stats.Duration = time.Since(start)
	return stats, nil
}

func migrateDatastore(ctx context.Context, src *timeshadedb.DB, dst core.TerrainStore, from timeshadedb.DatastoreKey, to core.DatastoreKey, opts Options) (DatastoreStats, error) {
	start := time.Now()
	clock, err := opts.Clock(from.SavefileUUID)
	if err != nil {
		return DatastoreStats{}, err
	}
	st := DatastoreStats{From: from, To: to, Clock: clock}
	window := opts.Window
	if window <= 0 {
		window = core.DefaultTerrainWindow
	}
	batchSize := opts.BatchSize
	if batchSize <= 0 {
		batchSize = 4096
	}
	commitEvery := opts.CommitEvery
	if commitEvery <= 0 {
		commitEvery = 16
	}
	// EnsureDatastore right before the scan: Commit raises the horizon of every
	// datastore that exists, so creating them all up front would make the later ones
	// drop their history as late.
	if err := dst.EnsureDatastore(ctx, to, window); err != nil {
		return st, err
	}
	pixels := make([]core.ChunkPixels, batchSize)
	batch := make([]core.ChunkUpdate, 0, batchSize)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		res, err := dst.Apply(ctx, batch)
		if err != nil {
			return err
		}
		st.Applied.Add(res)
		batch = batch[:0]
		return nil
	}
	chunks := map[timeshadedb.ChunkCoord]struct{}{}
	committed := core.TimeMs(0)
	haveCommit := false
	err = src.ScanChunkHistory(ctx, from, func(row timeshadedb.ChunkHistoryRow) error {
		t := clock.TimeOf(row.Tick)
		if st.Rows == 0 {
			st.FirstTick, st.FirstT = row.Tick, t
		}
		if row.Tick > st.LastTick || st.Rows == 0 {
			st.LastTick, st.LastT = row.Tick, t
		}
		st.Rows++
		chunks[row.Chunk] = struct{}{}
		// Every row so far is older than t (rows arrive in tick order), so windows
		// ending at or before WindowStart(t) can be closed once they are applied.
		ws := core.WindowStart(t, window)
		if !haveCommit {
			committed, haveCommit = ws, true
		} else if ws-committed >= core.TimeMs(commitEvery)*window {
			if err := flush(); err != nil {
				return err
			}
			if err := dst.Commit(ctx, ws); err != nil {
				return err
			}
			committed = ws
			st.Commits++
		}
		i := len(batch)
		pixels[i] = core.ChunkPixels(*row.Pixels)
		batch = append(batch, core.ChunkUpdate{
			DS:     to,
			Chunk:  core.ChunkCoord{X: row.Chunk.X, Y: row.Chunk.Y},
			T:      t,
			Pixels: &pixels[i],
		})
		if len(batch) == batchSize {
			return flush()
		}
		return nil
	})
	if err != nil {
		return st, err
	}
	if err := flush(); err != nil {
		return st, err
	}
	if st.Rows > 0 {
		if err := dst.Commit(ctx, core.WindowEnd(st.LastT, window)); err != nil {
			return st, err
		}
		st.Commits++
	}
	st.Chunks = len(chunks)
	st.Duration = time.Since(start)
	return st, nil
}
