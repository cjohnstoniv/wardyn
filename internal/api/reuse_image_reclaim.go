// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const (
	// reuseImageMaxAge is how long a reuse image stays after it was built and after the newest run that used it was
	// created. Removing one costs the next run a wrap of a base the host already holds.
	reuseImageMaxAge = 7 * 24 * time.Hour
	// reuseImageReclaimInterval spaces the passes after the one at server start.
	reuseImageReclaimInterval = 6 * time.Hour
	// reuseImageReclaimReadBound bounds a pass's daemon list and store reads. Each removal is bounded by
	// runner.RemoveUnlessHeld instead, so one queued behind a launch's hold is not spent while it waits.
	reuseImageReclaimReadBound = time.Minute
	// reuseImageReclaimActor is the image.reuse.reclaim row's actor.
	reuseImageReclaimActor = "wardyn/reuse-image-sweeper"
)

// ReuseImage is one tag a reusable per-run build left on the image builder's daemon.
type ReuseImage struct {
	Ref     string
	Created time.Time
}

// ReuseImageReclaimer is an OPTIONAL capability an ImageBuilder may implement, checked by type assertion as
// ImageBuildSweeper is: it lists the tags its reusable builds left (ReusingImageBuilder) and removes one, never
// forced. A builder without it leaves them.
type ReuseImageReclaimer interface {
	ListReuseImages(ctx context.Context) ([]ReuseImage, error)
	RemoveReuseImage(ctx context.Context, ref string) error
}

// startReuseImageReclaim runs the reuse image reclaim once now and then every reuseImageReclaimInterval, for the life
// of the daemon, when the image builder can list and remove its reuse images.
func (s *Server) startReuseImageReclaim() {
	rc, ok := s.cfg.ImageBuilder.(ReuseImageReclaimer)
	base := s.watcherBaseCtx()
	if !ok || base == nil {
		return
	}
	s.goBackground(func() { s.reuseImageReclaimLoop(base, rc, reuseImageReclaimInterval) })
}

// reuseImageReclaimLoop is one pass now, then one each interval, until ctx ends.
func (s *Server) reuseImageReclaimLoop(ctx context.Context, rc ReuseImageReclaimer, every time.Duration) {
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		s.reuseImageReclaimTick(ctx, rc)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// reuseImageReclaimTick is one pass. A panic is contained to it, so the next pass still runs.
func (s *Server) reuseImageReclaimTick(ctx context.Context, rc ReuseImageReclaimer) {
	defer func() {
		if r := recover(); r != nil {
			slog.ErrorContext(ctx, "wardynd: PANIC in reuse image reclaim (contained; the next pass runs)", slog.Any("panic", r))
		}
	}()
	if err := s.reclaimReuseImages(ctx, rc, s.cfg.Now()); err != nil {
		slog.WarnContext(ctx, "wardynd: reuse image reclaim", slog.Any("err", err))
	}
}

// reclaimReuseImages removes the reuse images this deployment's runs have finished with: a tag whose image was built
// more than reuseImageMaxAge before now, that at least one run in this database recorded, whose every such run has
// ended without keeping its sandbox, whose newest such run was created more than reuseImageMaxAge before now, and
// that no workspace row records. A run launch holding the tag keeps it (runner.RemoveUnlessHeld), and the daemon keeps
// an image a container still uses. A tag no run here recorded is another deployment's on a shared daemon, or a build
// whose launch failed before it recorded its image, and is left. Any read that fails removes nothing. A pass that
// removed a tag writes one image.reuse.reclaim row.
func (s *Server) reclaimReuseImages(ctx context.Context, rc ReuseImageReclaimer, now time.Time) error {
	uses, ok := s.cfg.Store.(store.RunImageUseReader)
	if !ok {
		return nil
	}
	rctx, cancel := context.WithTimeout(ctx, reuseImageReclaimReadBound)
	defer cancel()
	imgs, err := rc.ListReuseImages(rctx)
	if err != nil {
		return fmt.Errorf("list reuse images: %w", err)
	}
	cutoff := now.Add(-reuseImageMaxAge)
	var old []string
	for _, im := range imgs {
		if im.Created.Before(cutoff) {
			old = append(old, im.Ref)
		}
	}
	if len(old) == 0 {
		return nil
	}
	rows, err := uses.RunImageUse(rctx, old)
	if err != nil {
		return fmt.Errorf("read the runs that used reuse images: %w", err)
	}
	wss, err := s.cfg.Store.ListWorkspaces(rctx)
	if err != nil {
		return fmt.Errorf("list workspaces: %w", err)
	}
	use := make(map[string]store.RunImageUse, len(rows))
	for _, u := range rows {
		use[u.Ref] = u
	}
	var removed []string
	failed := 0
	for _, ref := range old {
		u, recorded := use[ref]
		if !recorded || u.InUse || !u.LastCreated.Before(cutoff) ||
			slices.ContainsFunc(wss, func(ws types.Workspace) bool { return ws.ImageRef == ref }) {
			continue
		}
		held, rerr := runner.RemoveUnlessHeld(ctx, ref, rc.RemoveReuseImage)
		switch {
		case held:
			slog.InfoContext(ctx, "wardynd: kept a reuse image a run launch is using", slog.String("image", ref))
		case rerr != nil:
			failed++
			slog.WarnContext(ctx, "wardynd: could not remove a reuse image", slog.String("image", ref), slog.Any("err", rerr))
		default:
			removed = append(removed, ref)
		}
	}
	if len(removed) > 0 {
		s.recordAudit(ctx, s.auditEvent(nil, types.ActorSystem, reuseImageReclaimActor, "image.reuse.reclaim", "reuse_images", "success",
			mustJSON(map[string]any{"removed": removed, "failed": failed, "max_age_days": int(reuseImageMaxAge / (24 * time.Hour))})))
	}
	return nil
}
