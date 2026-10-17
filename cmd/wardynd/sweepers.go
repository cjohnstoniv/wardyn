// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/approval"
	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/sweephealth"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The periodic sweepers startBackgroundWorkers launches (approval expiry, run
// secrets, stored credentials, recording retention). Each records its ticks in
// the shared sweep health record.

// runApprovalSweeper periodically transitions PENDING approvals older than
// `after` to EXPIRED via approval.ExpireStale, until ctx is cancelled. It mirrors
// the lifecycle reaper's goroutine shape; the first sweep runs after one tick.
// reauthExpiryCounter is the ONE thing the sweeper reports upward: how many
// credential_reauth rows it aged out. An interface rather than *api.Server so
// the sweeper keeps taking only what it needs, and nil is a no-op.
type reauthExpiryCounter interface {
	RecordCredentialReauthExpired(n int)
}

// st is the INTERFACE the sweep already uses, not the concrete adapter: the
// body only ever hands it to approval.ExpireStaleByKind. Widening it is what
// lets the credential re-auth expiry counter be pinned against this loop with a
// fake store instead of a live Postgres (round-2 F2) — the wiring it counts is
// three lines here, and a metric nobody can test is a metric nobody can trust.
//
// ticks records each tick as the approval_expiry sweep (nil records nothing):
// a tick whose sweep returned an error is an attempt without a success.
func runApprovalSweeper(ctx context.Context, st approval.Store, interval, after time.Duration, m reauthExpiryCounter, ticks *sweephealth.Tracker) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = ticks.Tick(ctx, sweephealth.ApprovalExpiry, func(ctx context.Context) error {
				return approvalSweepTick(ctx, st, after, m)
			})
		}
	}
}

// approvalSweepTick is one approval expiry sweep, returning the sweep's error.
func approvalSweepTick(ctx context.Context, st approval.Store, after time.Duration, m reauthExpiryCounter) error {
	n, byKind, err := approval.ExpireStaleByKind(ctx, st, after)
	if err != nil {
		// NOT a return: since the sweep collects per-row failures
		// instead of aborting on the first one, a non-nil error and a
		// non-zero count are both true on the same tick, and skipping
		// the count here would hide the work the sweep DID do behind
		// one wedged row.
		slog.ErrorContext(ctx, "wardynd: approval sweep error", slog.Any("err", err))
	}
	// At the transition: this is where a credential re-auth request
	// actually becomes EXPIRED, and the only place that can count it
	// honestly — the sidecar holding for it has long since given up, so
	// no later resolve will ever meet the row. AWS SSO re-auth rows
	// only (approval.TallyReauthAWSSSO), the series' HELP: an Azure
	// DevOps sign-in or consent row is credential_reauth too, and is
	// not — the same split CancelForRun's own tally already makes for
	// outcome="cancelled".
	if m != nil && byKind[approval.TallyReauthAWSSSO] > 0 {
		m.RecordCredentialReauthExpired(byKind[approval.TallyReauthAWSSSO])
	}
	if n > 0 {
		slog.InfoContext(ctx, "wardynd: approval sweep expired stale PENDING approvals",
			slog.Int("expired", n),
		)
	}
	return err
}

// runSecretSweeperServer and credentialSweeperServer are the *api.Server
// surfaces the two sweepers need, narrowed so a test can drive a failing sweep
// without a store behind a real server.
type runSecretSweeperServer interface {
	SweepRunSecrets(context.Context) (int, error)
}

type credentialSweeperServer interface {
	SweepExpiredCredentials(context.Context) (int, error)
}

// runSecretSweepInterval is how often the run-secret eviction lane ticks. Well
// under api.RunSecretGrace so a cold run's corpus is dropped promptly once it
// qualifies, and cheap enough to leave unconfigured: one run listing per tick,
// and none at all while the registry holds nothing.
const runSecretSweepInterval = 15 * time.Minute

// runSecretSweeper periodically evicts the plaintext masking corpus of runs
// that have been terminal past api.RunSecretGrace, until ctx is cancelled. Same
// goroutine shape as runApprovalSweeper; the first sweep runs after one tick.
// Every replica runs it, so every replica records its ticks as run_secret.
func runSecretSweeper(ctx context.Context, srv runSecretSweeperServer, interval time.Duration, ticks *sweephealth.Tracker) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = ticks.Tick(ctx, sweephealth.RunSecret, func(ctx context.Context) error {
				n, err := srv.SweepRunSecrets(ctx)
				if n > 0 {
					slog.InfoContext(ctx, "wardynd: evicted masking secrets for cold terminal runs",
						slog.Int("runs", n),
					)
				}
				return err
			})
		}
	}
}

// credentialSweepInterval is how often expired stored credentials are deleted
// (credential-storage design §2.7): daily, so a lapsed sign-in outlives its
// expiry by at most a day.
const credentialSweepInterval = 24 * time.Hour

// runCredentialSweeper deletes expired stored credentials once at start and
// then every interval, until ctx is cancelled. Unlike the sweepers above the
// first sweep does not wait a tick: a daemon restarted daily would otherwise
// never sweep. Several replicas may sweep at once; each row is deleted, and
// audited, by the one that wins its lock.
func runCredentialSweeper(ctx context.Context, srv credentialSweeperServer, interval time.Duration, ticks *sweephealth.Tracker) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		_ = ticks.Tick(ctx, sweephealth.CredentialExpiry, func(ctx context.Context) error {
			n, err := srv.SweepExpiredCredentials(ctx)
			if n > 0 {
				slog.InfoContext(ctx, "wardynd: deleted expired stored credentials", slog.Int("deleted", n))
			}
			return err
		})
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// recordingSweepable is satisfied structurally by BOTH recording.FSStore and
// recording.PGStore. Sweep is deliberately NOT on recording.Store itself (see
// the package doc on internal/recording/store.go): retention is a
// storage-backend concern, and a future object-storage backend would use its
// bucket's own lifecycle rules instead of an app-level sweep. This unexported
// interface — rather than promoting Sweep to recording.Store, or duplicating
// the goroutine-launch code below per concrete type — is the smaller diff for
// the ONE call site (startBackgroundWorkers' type-assert in boot_serve.go)
// that needs to sweep whichever concrete store is selected.
type recordingSweepable interface {
	Sweep(olderThan time.Duration) (int, error)
}

// runRecordingSweeper periodically deletes stored session recordings older
// than `after`, until ctx is cancelled. Only started when the operator sets a
// retention window (WARDYN_RECORDING_RETENTION_DAYS); unset = keep forever,
// because a recording is governance evidence and deleting one is an operator
// decision, not a default.
//
// Deletions are audited: a sweep that removed anything emits one
// recording.retention.sweep event, so the disappearance of evidence is itself
// evidence. The first sweep runs after one tick, mirroring the other sweepers.
//
// ticks records each tick as the recording_retention sweep (nil records
// nothing); a Sweep error is an attempt without a success.
func runRecordingSweeper(ctx context.Context, s recordingSweepable, rec audit.Recorder, interval, after time.Duration, ticks *sweephealth.Tracker) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = ticks.Tick(ctx, sweephealth.RecordingRetention, func(ctx context.Context) error {
				return recordingSweepTick(ctx, s, rec, after)
			})
		}
	}
}

// recordingSweepTick is one retention sweep, returning the sweep's error.
func recordingSweepTick(ctx context.Context, s recordingSweepable, rec audit.Recorder, after time.Duration) error {
	n, err := s.Sweep(after)
	if err != nil {
		slog.ErrorContext(ctx, "wardynd: recording sweep error", slog.Any("err", err))
	}
	if n == 0 {
		return err
	}
	slog.InfoContext(ctx, "wardynd: recording sweep deleted expired recordings", slog.Int("deleted", n))
	data, _ := json.Marshal(map[string]any{"deleted": n, "retention_sec": int64(after.Seconds())})
	ev := types.AuditEvent{
		ID:        uuid.New(),
		Time:      time.Now().UTC(),
		ActorType: types.ActorSystem,
		Actor:     "wardyn/recording-sweeper",
		Action:    "recording.retention.sweep",
		Target:    "recordings",
		Outcome:   "success",
		Data:      json.RawMessage(data),
	}
	if rerr := rec.Record(ctx, ev); rerr != nil {
		audit.LogWriteFailure(ctx, ev, rerr)
	}
	return err
}
