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
func (s *Server) Run(ctx context.Context) error {
	t := time.NewTicker(s.cfg.CommitEvery)
	defer t.Stop()
	s.Step(ctx)
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
	now := s.cfg.Clock()
	if err := s.Commit(ctx, now); err != nil && !errors.Is(err, context.Canceled) {
		s.cfg.Logf("commit: %v", err)
	}
	s.flushDirty(ctx)
	s.afterCommit(ctx)
}

// Commit commits terrain at now-TerrainGrace, updates the pyramid and commits movers at
// now-MoverGrace.
func (s *Server) Commit(ctx context.Context, now core.TimeMs) error {
	var errs []error
	if err := s.cfg.Terrain.Commit(ctx, now-s.cfg.TerrainGrace); err != nil {
		errs = append(errs, err)
	} else if _, err := s.cfg.Tiles.Update(ctx); err != nil {
		errs = append(errs, err)
	}
	if err := s.cfg.Movers.Commit(ctx, now-s.cfg.MoverGrace); err != nil {
		errs = append(errs, err)
	}
	s.metrics.commits.Add(1)
	return errors.Join(errs...)
}
