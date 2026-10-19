package api

import (
	"context"
	"errors"
	"time"

	"timeshadedb/internal/v3/core"
)

// Run drives the commit loop until ctx ends: every CommitEvery it commits terrain up to
// the clock minus TerrainGrace, updates the pyramid, commits movers up to the clock
// minus MoverGrace, then publishes live events for what changed. It returns ctx's error.
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

// Commit commits terrain at now-TerrainGrace, updates the pyramid and commits movers at
// now-MoverGrace.
func (s *Server) Commit(ctx context.Context, now core.TimeMs) error {
	var errs []error
	t0 := time.Now()
	if err := s.cfg.Terrain.Commit(ctx, now-s.cfg.TerrainGrace); err != nil {
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
	if err := s.cfg.Movers.Commit(ctx, now-s.cfg.MoverGrace); err != nil {
		errs = append(errs, err)
	}
	if d := time.Since(t0); d > 500*time.Millisecond {
		s.cfg.Logf("slow commit: terrain %v, pyramid %v, movers %v", t1.Sub(t0).Round(time.Millisecond), t2.Sub(t1).Round(time.Millisecond), time.Since(t2).Round(time.Millisecond))
	}
	s.metrics.commits.Add(1)
	return errors.Join(errs...)
}
