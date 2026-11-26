// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The release of the person's Docker volumes that existed when migration 0299 ran. An older server may still be
// deleting one of them, which nothing the new server can see rules out, so each stays listed but not attachable
// until an administrator who has stopped every older server releases them. There is no command-line or console
// form: the route is the whole interface.
const (
	// legacyDockerReleaseRetryEvery is how often every replica reads the owed release records and sends the audit
	// row of each it cannot see in the shared audit table (RunLegacyDockerReleaseRetry).
	legacyDockerReleaseRetryEvery = time.Minute
	// pgDeadlockDetected is PostgreSQL's SQLSTATE for the transaction a deadlock chose to end; the statement has
	// not taken effect and is run again as it was.
	pgDeadlockDetected = "40P01"
)

// handleReleaseLegacyDockerDrives is POST /api/v1/admin/drives/legacy-docker/release: it clears the hold on every
// person's Docker volume from before the upgrade and records the release (the release statement commits both).
// Owed records of earlier calls are delivered first, so a call after one that was interrupted replays that call's
// event with its own id, exactly once. The answer is {released, event_id} for a release made now and
// {released: 0} when none was left, with replayed ids of the earlier events this call delivered.
func (s *Server) handleReleaseLegacyDockerDrives(w http.ResponseWriter, r *http.Request) {
	ls, ok := s.cfg.Store.(store.LegacyDockerReleaseStore)
	if !ok {
		writeErrorReason(w, http.StatusNotImplemented, reasonDelegationStoreUnavailable, "drive release requires the Postgres store backend")
		return
	}
	replayed := s.deliverLegacyDockerReleases(r.Context(), ls)
	out := map[string]any{"released": 0}
	// Only the Docker deployment's drives are held for this release; any other target releases nothing, but still
	// answers the earlier events it delivered, as the Docker target's zero release does.
	if s.cfg.RunnerTarget == types.DriveBackendDockerVolume.RunnerTarget() {
		actorType, actor := actorFromRequest(r)
		rec, err := ls.ReleaseLegacyDockerDrives(r.Context(), string(actorType), actor)
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgDeadlockDetected {
			rec, err = ls.ReleaseLegacyDockerDrives(r.Context(), string(actorType), actor) // nothing committed: the same call again
		}
		if err != nil {
			writeServerError(w, r, "release the legacy Docker drives", err)
			return
		}
		out["released"] = rec.Released
		if rec.Released > 0 {
			out["event_id"] = rec.ID
			// This release's own event is delivered now; only the earlier ones are replays.
			for _, id := range s.deliverLegacyDockerReleases(r.Context(), ls) {
				if id != rec.ID {
					replayed = append(replayed, id)
				}
			}
		}
	}
	if len(replayed) > 0 {
		out["replayed"] = replayed
	}
	writeJSON(w, http.StatusOK, out)
}

// RunLegacyDockerReleaseRetry delivers the owed release records until ctx ends: at once, then every
// legacyDockerReleaseRetryEvery. It is its own loop, so it neither waits for the portal monitor's timer (an hour
// once portals are on) nor runs behind its sampling, and each pass reads the owed rows from the database, so a
// record committed by a replica that then stopped is found by every other. Boot starts it on every replica.
func (s *Server) RunLegacyDockerReleaseRetry(ctx context.Context) {
	s.runLegacyDockerReleaseRetry(ctx, legacyDockerReleaseRetryEvery)
}

func (s *Server) runLegacyDockerReleaseRetry(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if ls, ok := s.cfg.Store.(store.LegacyDockerReleaseStore); ok {
			s.deliverLegacyDockerReleases(ctx, ls)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// deliverLegacyDockerReleases sends the audit event of every committed release not yet settled and settles each
// the audit table then holds, and returns the ids it settled. Every delivery is a replay of the same event
// (store.WithAuditReplay) with the id and time the record fixed, so the audit table keeps one row each however
// often, and from whichever replica, it runs. Delivery is not settlement: a recorder that says it accepted the row
// may hold it only in a pod's local spool, so a record is settled only when the store reads the row, with its
// fixed time, in the shared audit table. A record that stays owed is sent again by the next call or tick.
func (s *Server) deliverLegacyDockerReleases(ctx context.Context, ls store.LegacyDockerReleaseStore) []uuid.UUID {
	recs, err := ls.PendingLegacyDockerReleases(ctx)
	if err != nil {
		slog.WarnContext(ctx, "drive release: read the owed audit rows", slog.Any("err", err))
		return nil
	}
	var settled []uuid.UUID
	for _, rec := range recs {
		key := store.AuditEventKey{ID: rec.ID, At: rec.EventAt}
		stored := func() bool {
			held, err := ls.AuditEventsStored(ctx, []store.AuditEventKey{key})
			if err != nil {
				slog.WarnContext(ctx, "drive release: read the audit table", slog.Any("err", err))
				return false
			}
			return held[store.AuditEventKey{ID: rec.ID, At: rec.EventAt.UTC().Truncate(time.Microsecond)}]
		}
		if !stored() {
			ev := s.auditEvent(nil, types.ActorType(rec.ActorType), rec.Actor, "drive.legacy_docker.release", "drives", "success",
				mustJSON(map[string]any{"released": rec.Released}))
			ev.ID, ev.Time = rec.ID, rec.EventAt
			s.recordAuditDurably(store.WithAuditReplay(ctx), ev)
		}
		if !stored() {
			slog.WarnContext(ctx, "drive release: the audit row is not in the shared audit table yet, it stays owed")
			continue
		}
		ok, err := ls.SettleLegacyDockerRelease(ctx, rec.ID)
		switch {
		case err != nil:
			slog.WarnContext(ctx, "drive release: settle the audit row", slog.Any("err", err))
		case !ok:
			slog.WarnContext(ctx, "drive release: the record was not settled, it stays owed")
		default:
			settled = append(settled, rec.ID)
		}
	}
	return settled
}
