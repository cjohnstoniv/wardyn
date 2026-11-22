// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Settling the person's drives that existed when migration 0299 ran (user_drives.legacy_unsettled): a Mac disk
// is released by its own runner's evidence, a Docker volume by an administrator's call, which also leaves a record
// that is replayed to the audit log until the audit table holds it (drive_legacy_releases).
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ReleaseLegacyMacDrives clears legacy_unsettled on the Mac disks kept on runnerID, and on no other row: the
// runner's evidence (a new process, which no earlier delete of this Mac can outlive) is about its own Mac. It
// answers how many rows it cleared; calling it again is a no-op.
func (s PG) ReleaseLegacyMacDrives(ctx context.Context, runnerID uuid.UUID) (int64, error) {
	tag, err := s.Pool.Exec(ctx, `UPDATE user_drives SET legacy_unsettled = false
		WHERE backend = 'mac_disk' AND home_runner_id = $1 AND legacy_unsettled`, runnerID)
	if err != nil {
		return 0, fmt.Errorf("store: release legacy Mac drives: %w", err)
	}
	return tag.RowsAffected(), nil
}

// LegacyDockerRelease is the durable record of one release of the legacy Docker drives (drive_legacy_releases):
// the audit event's fixed id and time, who released, and how many drives. It is committed in the statement that
// clears the marks and is replayed to the audit log until the audit table holds it.
type LegacyDockerRelease struct {
	ID        uuid.UUID
	EventAt   time.Time
	ActorType string
	Actor     string
	Released  int
	AuditedAt *time.Time
}

// LegacyDockerReleaseStore is the release of the legacy Docker drives and its audit delivery (store.PG).
type LegacyDockerReleaseStore interface {
	ReleaseLegacyDockerDrives(ctx context.Context, actorType, actor string) (LegacyDockerRelease, error)
	PendingLegacyDockerReleases(ctx context.Context) ([]LegacyDockerRelease, error)
	SettleLegacyDockerRelease(ctx context.Context, id uuid.UUID) (bool, error)
	AuditEventsStored(ctx context.Context, want []AuditEventKey) (map[AuditEventKey]bool, error)
}

var _ LegacyDockerReleaseStore = PG{}

// ReleaseLegacyDockerDrives clears legacy_unsettled on every person's Docker volume that has it and, in the same
// statement, records the release with the audit event's id and time fixed (the time to the microsecond, as the
// audit table stores it). Nothing is released and nothing recorded when no drive is marked: the answer then has a
// zero Released. A second call finds nothing marked, so a retry never releases or records twice.
func (s PG) ReleaseLegacyDockerDrives(ctx context.Context, actorType, actor string) (LegacyDockerRelease, error) {
	rec := LegacyDockerRelease{ID: uuid.New(), EventAt: time.Now().UTC().Truncate(time.Microsecond), ActorType: actorType, Actor: actor}
	err := s.Pool.QueryRow(ctx, `
		WITH rel AS (UPDATE user_drives SET legacy_unsettled = false WHERE backend = 'docker_volume' AND legacy_unsettled RETURNING id)
		INSERT INTO drive_legacy_releases (id, event_at, actor_type, actor, released, audited_at)
		SELECT $1::uuid, $2::timestamptz, $3::text, $4::text, count(*)::int, NULL::timestamptz FROM rel HAVING count(*) > 0
		RETURNING released`, rec.ID, rec.EventAt, rec.ActorType, rec.Actor).Scan(&rec.Released)
	if errors.Is(err, pgx.ErrNoRows) {
		return LegacyDockerRelease{}, nil
	}
	if err != nil {
		return LegacyDockerRelease{}, fmt.Errorf("store: release legacy Docker drives: %w", err)
	}
	return rec, nil
}

// PendingLegacyDockerReleases is the committed releases whose audit event is not yet settled, oldest first.
func (s PG) PendingLegacyDockerReleases(ctx context.Context) ([]LegacyDockerRelease, error) {
	return collect(ctx, s.Pool, "list", "legacy Docker releases", `
		SELECT id, event_at, actor_type, actor, released FROM drive_legacy_releases WHERE audited_at IS NULL ORDER BY event_at, id`,
		nil, func(r pgx.Row) (LegacyDockerRelease, error) {
			var rec LegacyDockerRelease
			err := r.Scan(&rec.ID, &rec.EventAt, &rec.ActorType, &rec.Actor, &rec.Released)
			rec.EventAt = rec.EventAt.UTC().Truncate(time.Microsecond)
			return rec, err
		})
}

// SettleLegacyDockerRelease settles the release whose audit event the shared audit table holds, with the id and
// time the record fixed, and reports whether this call did. The actor leaves the record in the same statement, so
// nothing in it can attribute the audit row once its subject's key is erased.
func (s PG) SettleLegacyDockerRelease(ctx context.Context, id uuid.UUID) (bool, error) {
	tag, err := s.Pool.Exec(ctx, `
		UPDATE drive_legacy_releases r SET audited_at = clock_timestamp(), actor = ''
		 WHERE r.id = $1 AND r.audited_at IS NULL
		   AND EXISTS (SELECT 1 FROM audit_events e WHERE e.id = r.id AND e."time" = r.event_at AND e.action = 'drive.legacy_docker.release')`, id)
	if err != nil {
		return false, fmt.Errorf("store: settle legacy Docker release: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}
