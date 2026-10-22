package api

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"time"

	"timeshadedb/internal/v3/core"
)

// Run drives the commit loop until ctx ends: every CommitEvery it commits terrain up to
// the clock minus TerrainGrace, updates the pyramid, commits movers up to the clock
// minus MoverGrace, then publishes live events for what changed. It also steps at once
// when ingest has filled a quarter of the mover store's open rows (a fast replay), so
// those rows are committed before the store refuses batches. It returns ctx's error.
// The first step runs one period after the start, so a freshly started server serves its
// first requests without competing with the catch-up commit and pyramid build.
func (s *Server) Run(ctx context.Context) error {
	t := time.NewTicker(s.cfg.CommitEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			s.Step(ctx)
			s.releaseIfIdle()
		case <-s.kick:
			s.Step(ctx)
		}
	}
}

// Step runs one iteration of the commit loop. Tests call it directly.
func (s *Server) Step(ctx context.Context) {
	t0 := time.Now()
	now := s.cfg.Clock()
	if err := s.Commit(ctx, now); err != nil && !errors.Is(err, context.Canceled) {
		s.cfg.Logf("commit: %v", err)
	}
	t1 := time.Now()
	s.flushDirty(ctx)
	s.afterCommit(ctx)
	if d := time.Since(t0); d > 500*time.Millisecond {
		s.cfg.Logf("slow commit step: %v (commit %v, live events %v)", d.Round(time.Millisecond), t1.Sub(t0).Round(time.Millisecond), time.Since(t1).Round(time.Millisecond))
	}
}

// SourceHolds is the per-source ingest state the commit loop asks for its targets
// (ingest.Sources).
type SourceHolds interface {
	// Holds returns, per map key, the lowest newest time of the sources holding it.
	Holds() map[string]core.TimeMs
	// Save persists the state.
	Save() error
}

// terrainCommitter and moverCommitter are stores that take a target per datastore or
// surface (terrain.Store, movers.Store).
type terrainCommitter interface {
	CommitBy(ctx context.Context, upTo func(core.DatastoreKey) core.TimeMs) error
}

type moverCommitter interface {
	CommitBy(ctx context.Context, upTo func(core.SurfaceKey) core.TimeMs) error
}

// Commit commits terrain at now-TerrainGrace, updates the pyramid and commits movers at
// now-MoverGrace. A map held by a source (Config.Sources) is committed only up to that
// source's newest time minus the grace.
func (s *Server) Commit(ctx context.Context, now core.TimeMs) error {
	var errs []error
	var holds map[string]core.TimeMs
	if s.cfg.Sources != nil {
		holds = s.cfg.Sources.Holds()
		s.noteHolds(holds, now)
	}
	target := func(mapKey string, grace core.TimeMs) core.TimeMs {
		t := now - grace
		if h, ok := holds[mapKey]; ok && h-grace < t {
			t = h - grace
		}
		return t
	}
	t0 := time.Now()
	if tc, ok := s.cfg.Terrain.(terrainCommitter); ok {
		if err := tc.CommitBy(ctx, func(k core.DatastoreKey) core.TimeMs { return target(k.Map, s.cfg.TerrainGrace) }); err != nil {
			errs = append(errs, err)
		}
	} else if err := s.cfg.Terrain.Commit(ctx, now-s.cfg.TerrainGrace); err != nil {
		errs = append(errs, err)
	}
	t1 := time.Now()
	if len(errs) == 0 {
		bs, err := s.cfg.Tiles.Update(ctx)
		if err != nil {
			errs = append(errs, err)
		}
		if bs.Built > 0 || bs.Versions > 0 {
			s.builds.Add(1)
		}
	}
	t2 := time.Now()
	if mc, ok := s.cfg.Movers.(moverCommitter); ok {
		if err := mc.CommitBy(ctx, func(k core.SurfaceKey) core.TimeMs { return target(k.Map, s.cfg.MoverGrace) }); err != nil {
			errs = append(errs, err)
		}
	} else if err := s.cfg.Movers.Commit(ctx, now-s.cfg.MoverGrace); err != nil {
		errs = append(errs, err)
	}
	if s.cfg.Sources != nil {
		if err := s.cfg.Sources.Save(); err != nil {
			errs = append(errs, fmt.Errorf("sources: %w", err))
		}
	}
	if d := time.Since(t0); d > 500*time.Millisecond {
		s.cfg.Logf("slow commit: terrain %v, pyramid %v, movers %v", t1.Sub(t0).Round(time.Millisecond), t2.Sub(t1).Round(time.Millisecond), time.Since(t2).Round(time.Millisecond))
	}
	s.metrics.commits.Add(1)
	return errors.Join(errs...)
}

// holdLog is how far behind the clock a map's commits must be held before the loop
// logs it (a live source's newest time trails the clock by a few seconds).
const holdLog = core.Minute

// noteHolds logs when a map starts or stops being held more than holdLog behind the
// clock, and records the held maps and the largest hold for /metrics.
func (s *Server) noteHolds(holds map[string]core.TimeMs, now core.TimeMs) {
	s.holdMu.Lock()
	defer s.holdMu.Unlock()
	if s.held == nil {
		s.held = map[string]bool{}
	}
	var lag core.TimeMs
	n := 0
	for mk, h := range holds {
		behind := now - h
		if behind > holdLog {
			n++
			lag = max(lag, behind)
			if !s.held[mk] {
				s.held[mk] = true
				s.cfg.Logf("holding the horizon of map %q at %s (%v behind the clock): a source is still sending older data",
					mk, h.Time().UTC().Format(time.RFC3339), time.Duration(behind)*time.Millisecond)
			}
		}
	}
	for mk := range s.held {
		if h, ok := holds[mk]; !ok || now-h <= holdLog {
			delete(s.held, mk)
			s.cfg.Logf("released the horizon of map %q", mk)
		}
	}
	s.metrics.heldMaps.Store(int64(n))
	s.metrics.holdLag.Store(int64(lag))
}

// pressured is implemented by stores that bound their open rows (movers.Store).
type pressured interface{ Pressure() float64 }

// checkPressure kicks the commit loop when the mover store's open rows are a quarter
// full.
func (s *Server) checkPressure() {
	if p, ok := s.cfg.Movers.(pressured); ok && p.Pressure() >= 0.25 {
		select {
		case s.kick <- struct{}{}:
		default:
		}
	}
}

// releaseIfIdle returns freed memory to the operating system once after IdleRelease
// without requests (Config.IdleRelease).
func (s *Server) releaseIfIdle() {
	if s.cfg.IdleRelease < 0 || s.released.Load() {
		return
	}
	last := s.lastReq.Load()
	if last == 0 || time.Since(time.Unix(0, last)) < s.cfg.IdleRelease {
		return
	}
	s.released.Store(true)
	debug.FreeOSMemory()
	s.metrics.idleReleases.Add(1)
}
