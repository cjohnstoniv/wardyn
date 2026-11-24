// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"log/slog"
	"slices"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// memberDriveByRetrySweep is the "by" of a member drive delete the retry sweep finishes (#4061).
const memberDriveByRetrySweep = "retry_sweep"

// memberDriveRetryBatch bounds the held deletes one sweep retries, so one tick's substrate calls stay bounded.
const memberDriveRetryBatch = 50

// heldDrivesListed runs between the sweep's list and its first takeover. Tests set it to hold two sweeps that both
// listed the same row, so they race at the takeover (the memberDriveCounted precedent, store/member_drives.go).
var heldDrivesListed = func() {}

// heldMemberDriveDeleteStore lists the held member drive deletes the retry sweep retries (store.PG).
type heldMemberDriveDeleteStore interface {
	ListHeldMemberDriveDeletes(ctx context.Context, backends []string, limit int) ([]types.UserDrive, error)
}

// heldDriveBackends is the drive backends a server of runner target deletes: those whose RunnerTarget is target. An
// empty target (and so a Mac disk, whose RunnerTarget is "") gives none.
func heldDriveBackends(target string) []string {
	var out []string
	for _, b := range types.DriveBackends {
		if target != "" && b.RunnerTarget() == target {
			out = append(out, string(b))
		}
	}
	return out
}

// sweepHeldMemberDriveDeletes retries each member drive delete left held (a call whose outcome is not known, a time-out,
// a refusal after the issue) on a Docker volume or a Kubernetes claim. It runs on the leader only, as the substrate is
// reachable from every replica. Each row is taken over as the Mac sweep takes its own (finishMacDiskDeletes) and deleted
// under the sweep's rule: a failure keeps the mark for the next pass, and one claim makes at most one call.
func (s *Server) sweepHeldMemberDriveDeletes(ctx context.Context) error {
	ms, ok := s.cfg.Store.(store.MemberDriveStore)
	ls, lok := s.cfg.Store.(heldMemberDriveDeleteStore)
	if !ok || !lok {
		return nil
	}
	ctx, end, leader := s.beginLeaderSweep(ctx)
	if !leader {
		return nil
	}
	defer end()
	// The attempt is the server's own, as other sweeps' rows are: actor system, wardynd.
	ctx = withActor(ctx, types.ActorSystem, "wardynd")
	// The pass is bounded: a slow substrate must not hold the tick, and a lost lease ends it too. Rows not taken keep
	// their mark and come first on the next pass. A delete already begun finishes on its own clock (deleteMemberDrive).
	ctx, cancel := context.WithTimeout(ctx, reconcileFinalizeTimeout)
	defer cancel()
	if !s.memberDrivesDeletable(ctx) {
		return nil
	}
	backends := heldDriveBackends(s.cfg.RunnerTarget)
	if len(backends) == 0 {
		return nil
	}
	held, err := ls.ListHeldMemberDriveDeletes(ctx, backends, memberDriveRetryBatch)
	if err != nil {
		return err
	}
	heldDrivesListed()
	var errs []error
	for _, h := range held {
		if ctx.Err() != nil {
			break
		}
		d, started, err := ms.BeginMemberDriveTakeover(ctx, h.ID)
		switch {
		case errors.Is(err, store.ErrNotFound), errors.Is(err, store.ErrMemberDriveBusy):
			continue
		case err != nil:
			errs = append(errs, err)
			continue
		}
		if res := s.deleteMemberDrive(ctx, ms, d, started, memberDriveByRetrySweep); res.err != nil {
			errs = append(errs, res.err)
		}
	}
	return errors.Join(errs...)
}

// meDeletingDrives is the caller's own drives whose delete has not finished, listed so the owner sees them and can
// press Delete again (#4061). They are added after the reachable set is built, so no run attaches one. A drive already
// in have is not listed twice.
func (s *Server) meDeletingDrives(ctx context.Context, have []client.MeDrive) []client.MeDrive {
	ms, ok := s.cfg.Store.(store.MemberDriveStore)
	owner := types.CanonicalUserSubject(oidcHumanFromContext(ctx))
	if _, actor := actorFromContext(ctx, ""); !ok || owner == "" || types.CanonicalUserSubject(actor) != owner {
		return nil
	}
	drives, err := ms.ListMemberDrivesOf(ctx, owner)
	if err != nil {
		slog.WarnContext(ctx, "api: list the caller's deleting drives", slog.String("error", err.Error()))
		return nil
	}
	var out []client.MeDrive
	for _, d := range drives {
		if !d.Deleting() || slices.ContainsFunc(have, func(h client.MeDrive) bool { return h.ID == d.ID.String() }) {
			continue
		}
		row := memberMeDrive(d)
		row.UnavailableReason = client.MeDriveUnavailableDeleting
		out = append(out, row)
	}
	return out
}
