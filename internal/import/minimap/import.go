package minimap

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"time"

	"timeshadedb/internal/v3/core"
)

// Options configure an import. Nil stores are skipped.
type Options struct {
	Terrain  core.TerrainStore
	Movers   core.MoverStore
	Overlays core.OverlayStore
	// Window is the terrain window for the datastores the import creates (default
	// core.DefaultTerrainWindow).
	Window core.TimeMs
	// CommitEvery is how much map time of terrain is fed between TerrainStore.Commit
	// calls (default 1 h).
	CommitEvery core.TimeMs
	// MoverCommitEvery is how much map time of movers is fed between MoverStore.Commit
	// calls (default 1 h). Each commit trails the feed by more than core.HandoffWindow.
	MoverCommitEvery core.TimeMs
	// Workers bounds parallel decoding (default GOMAXPROCS).
	Workers int
	// Maps, when non-empty, restricts the import to these raw map keys.
	Maps []string
	// Surfaces, when non-empty, restricts the import to these raw surface names.
	Surfaces []string
}

func (o *Options) defaults() {
	if o.Window <= 0 {
		o.Window = core.DefaultTerrainWindow
	}
	if o.CommitEvery <= 0 {
		o.CommitEvery = core.Hour
	}
	if o.MoverCommitEvery <= 0 {
		o.MoverCommitEvery = core.Hour
	}
	if o.Workers <= 0 {
		o.Workers = runtime.GOMAXPROCS(0)
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func (o *Options) wantSurface(k core.SurfaceKey) bool {
	return (len(o.Maps) == 0 || contains(o.Maps, k.Map)) && (len(o.Surfaces) == 0 || contains(o.Surfaces, k.Surface))
}

func (o *Options) wantDS(k core.DatastoreKey) bool { return o.wantSurface(k.SurfaceKey()) }

// Report is what an import read, fixed and wrote.
type Report struct {
	Root     string         `json:"root"`
	WindowMs core.TimeMs    `json:"window_ms"`
	Maps     []core.MapInfo `json:"maps"`
	// Icons are the catalog icon files, relative to Root, when present.
	Icons    *core.CatalogIcons `json:"icons,omitempty"`
	Warnings []string           `json:"warnings,omitempty"`

	Terrain  []*TerrainReport `json:"terrain"`
	Movers   []*MoverReport   `json:"movers"`
	Overlays []*OverlayReport `json:"overlays"`

	// Store results: what the stores accepted, found late or invalid.
	TerrainApply core.ApplyResult `json:"terrain_apply"`
	MoverApply   core.ApplyResult `json:"mover_apply"`
	OverlayApply core.ApplyResult `json:"overlay_apply"`

	// Wall-clock durations in milliseconds. The three parts run in parallel.
	TerrainMs  int64 `json:"terrain_ms"`
	MoversMs   int64 `json:"movers_ms"`
	OverlaysMs int64 `json:"overlays_ms"`
	TotalMs    int64 `json:"total_ms"`
}

// Import reads the dataset and feeds it into the stores in opt. Terrain, movers and
// overlays are imported concurrently (they go to separate stores).
func Import(ctx context.Context, d *Dataset, opt Options) (*Report, error) {
	opt.defaults()
	start := time.Now()
	rep := &Report{Root: d.Root, WindowMs: opt.Window, Icons: d.Icons, Warnings: d.Warnings}
	for _, m := range d.Maps {
		if len(opt.Maps) == 0 || contains(opt.Maps, m.Key) {
			rep.Maps = append(rep.Maps, m)
		}
	}
	var wg sync.WaitGroup
	var errT, errM, errO error
	if opt.Terrain != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			t0 := time.Now()
			rep.Terrain, rep.TerrainApply, errT = importTerrain(ctx, d, &opt, opt.Terrain)
			rep.TerrainMs = time.Since(t0).Milliseconds()
		}()
	}
	if opt.Movers != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			t0 := time.Now()
			rep.Movers, rep.MoverApply, errM = importMovers(ctx, d, &opt, opt.Movers)
			rep.MoversMs = time.Since(t0).Milliseconds()
		}()
	}
	if opt.Overlays != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			t0 := time.Now()
			rep.Overlays, rep.OverlayApply, errO = importOverlays(ctx, d, &opt, opt.Overlays)
			rep.OverlaysMs = time.Since(t0).Milliseconds()
		}()
	}
	wg.Wait()
	rep.TotalMs = time.Since(start).Milliseconds()
	return rep, errors.Join(errT, errM, errO)
}
