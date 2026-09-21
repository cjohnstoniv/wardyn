// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package federation

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const (
	tickInterval = 15 * time.Second
	// batchSize is the organisation's per-push row cap (maxDeviceIngestRows).
	batchSize  = 500
	maxBackoff = 5 * time.Minute
)

// AuditActor names this package on the local rows it writes.
const AuditActor = "wardyn/federation"

// Store is the local table the forwarder reads and the durable org_federation
// row it keeps: the cursor and the revoked mark (store.PG).
type Store interface {
	ListAuditEventsAfterSeq(ctx context.Context, seq int64, limit int) ([]types.FederatedAuditEvent, error)
	GetFederationCursor(ctx context.Context) (int64, error)
	SetFederationCursor(ctx context.Context, seq int64) error
	AuditHeadSeq(ctx context.Context) (int64, error)
	FederationRevoked(ctx context.Context) (bool, error)
	MarkFederationRevoked(ctx context.Context) error
	ResetFederation(ctx context.Context) error
}

// Status is the forwarder's published state. DeviceID is for this daemon's
// own logs and operators; /healthz, which is anonymous, never carries it.
type Status struct {
	DeviceID  uuid.UUID
	AckedSeq  int64
	HeadSeq   int64
	LastOK    time.Time
	Revoked   bool
	LastError string
}

// Lag is how many local rows the organisation does not yet hold.
func (s Status) Lag() int64 { return max(s.HeadSeq-s.AckedSeq, 0) }

// Forwarder pushes this daemon's chained audit rows to the organisation from
// the durable cursor in org_federation, one batch at a time — at-least-once:
// the cursor moves only after the organisation acknowledged, and the
// organisation deduplicates a re-sent row by its seq.
type Forwarder struct {
	client   *Client
	store    Store
	cred     Credential
	audit    audit.Recorder
	interval time.Duration

	mu      sync.Mutex
	status  Status
	loaded  bool
	halted  bool
	backoff time.Duration
}

// NewForwarder returns a forwarder for cred. Nothing is read until Load or Run.
func NewForwarder(c *Client, st Store, cred Credential, rec audit.Recorder) *Forwarder {
	return &Forwarder{client: c, store: st, cred: cred, audit: rec, interval: tickInterval,
		status: Status{DeviceID: cred.DeviceID}}
}

// Status returns a copy of the current state.
func (f *Forwarder) Status() Status {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.status
}

func (f *Forwarder) update(fn func(*Status)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(&f.status)
}

// Load reads the durable cursor and revoked mark. bootHybrid calls it before
// the server starts, so a laptop revoked before a restart refuses new runs from
// its first request, whether or not the organisation is reachable.
func (f *Forwarder) Load(ctx context.Context) error {
	acked, err := f.store.GetFederationCursor(ctx)
	if err != nil {
		return err
	}
	revoked, err := f.store.FederationRevoked(ctx)
	if err != nil {
		return err
	}
	f.update(func(s *Status) {
		s.AckedSeq, s.Revoked = acked, revoked
		if revoked {
			s.LastError = "the organisation revoked this device before this start"
		}
	})
	f.loaded = true
	return nil
}

// Run forwards until ctx ends or the organisation revokes this device. A
// revoked forwarder stops calling the organisation for the rest of the
// process: its credential is dead, and re-enrolment is a boot-time act.
func (f *Forwarder) Run(ctx context.Context) {
	for {
		wait, stop := f.step(ctx)
		if stop {
			return
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}

// step does one unit of work — push the next batch, or heartbeat when there is
// none — and says how long to wait before the next.
//
// The head is read before anything can fail upstream, so an unreachable
// organisation shows as lag that grows. Rows with no row_hash predate the
// chain (migration 0047) and cannot be verified upstream; they are skipped,
// never sent, and the cursor moves past them.
func (f *Forwarder) step(ctx context.Context) (time.Duration, bool) {
	if !f.loaded {
		if err := f.Load(ctx); err != nil {
			return f.retry(err, 0), false
		}
	}
	if f.Status().Revoked {
		return 0, true
	}
	head, err := f.store.AuditHeadSeq(ctx)
	if err != nil {
		return f.retry(err, 0), false
	}
	f.update(func(s *Status) { s.HeadSeq = head })
	acked := f.Status().AckedSeq
	if head < acked {
		// The local table was truncated or restored under a kept cursor: its rows
		// would never be read again. Start over; the organisation skips what it
		// already holds and records a genesis row as a chain reset.
		slog.Error("federation: the local audit head is below the forwarding cursor; resending from the start",
			"device_id", f.cred.DeviceID, "head_seq", head, "acked_seq", acked)
		if err := f.store.SetFederationCursor(ctx, 0); err != nil {
			return f.retry(err, 0), false
		}
		acked = 0
		f.update(func(s *Status) { s.AckedSeq = 0 })
	}

	var rows []types.FederatedAuditEvent
	if !f.halted && head > acked {
		if rows, err = f.store.ListAuditEventsAfterSeq(ctx, acked, batchSize); err != nil {
			return f.retry(err, 0), false
		}
	}
	chained := slices.DeleteFunc(slices.Clone(rows), func(e types.FederatedAuditEvent) bool { return e.RowHash == "" })

	next := acked
	switch {
	case len(chained) > 0:
		ack, perr := f.client.Push(ctx, f.cred, chained)
		if perr != nil {
			return f.refused(ctx, perr)
		}
		// Never past what was read, never backwards.
		next = max(acked, min(ack, rows[len(rows)-1].Seq))
	case len(rows) > 0:
		next = rows[len(rows)-1].Seq
	default:
		if _, herr := f.client.Heartbeat(ctx, f.cred); herr != nil {
			return f.refused(ctx, herr)
		}
	}
	if next != acked {
		if err := f.store.SetFederationCursor(ctx, next); err != nil {
			return f.retry(err, 0), false
		}
	}
	f.backoff = 0
	f.update(func(s *Status) {
		s.AckedSeq, s.LastOK = next, time.Now().UTC()
		if !f.halted {
			s.LastError = ""
		}
	})
	if len(rows) == batchSize {
		return 0, false
	}
	return f.interval, false
}

// refused maps an organisation answer to what the forwarder does next:
// 401/410 revoke; 429 waits as its Retry-After says, or backs off when it says
// nothing usable; any other 4xx is definitive — the batch
// will never be accepted as sent, so it is not retried (the forwarder halts
// pushing, keeps heart-beating so revocation is still noticed, and says so on
// its status until a restart); everything else backs off and retries.
func (f *Forwarder) refused(ctx context.Context, err error) (time.Duration, bool) {
	var se *StatusError
	if !errors.As(err, &se) {
		return f.retry(err, 0), false
	}
	switch {
	case se.Revoked():
		f.revoke(ctx, se)
		return 0, true
	case se.Code == 429 && se.RetryAfter > 0:
		f.update(func(s *Status) { s.LastError = se.Error() })
		return min(se.RetryAfter, maxBackoff), false
	case se.Code >= 400 && se.Code < 500 && se.Code != 408 && se.Code != 429:
		f.halted = true
		slog.Error("federation: the organisation refused an audit batch definitively; forwarding is halted until wardynd restarts",
			"device_id", f.cred.DeviceID, "acked_seq", f.Status().AckedSeq, "error", se)
		f.update(func(s *Status) { s.LastError = se.Error() })
		return f.interval, false
	}
	return f.retry(se, se.RetryAfter), false
}

// retry records err and returns the next backoff: doubling from the tick
// interval, capped at maxBackoff, never shorter than a Retry-After.
func (f *Forwarder) retry(err error, retryAfter time.Duration) time.Duration {
	f.backoff = min(max(2*f.backoff, f.interval), maxBackoff)
	slog.Warn("federation: audit forward failed; retrying", "error", err, "retry_in", f.backoff)
	f.update(func(s *Status) { s.LastError = err.Error() })
	return min(max(f.backoff, retryAfter), maxBackoff)
}

func (f *Forwarder) revoke(ctx context.Context, se *StatusError) {
	st := f.Status()
	slog.Error("federation: the organisation revoked this device; new runs are refused until it is re-enrolled",
		"device_id", f.cred.DeviceID, "status", se.Code, "acked_seq", st.AckedSeq)
	f.update(func(s *Status) { s.Revoked, s.LastError = true, se.Error() })
	if err := f.store.MarkFederationRevoked(ctx); err != nil {
		slog.Error("federation: recording the revocation durably failed; a restart will not remember it", "error", err)
	}
	data, _ := json.Marshal(map[string]any{"status": se.Code, "acked_seq": st.AckedSeq})
	if err := f.audit.Record(ctx, types.AuditEvent{
		ID: uuid.New(), Time: time.Now().UTC(), ActorType: types.ActorSystem, Actor: AuditActor,
		Action: "device.local.revoke", Target: f.cred.DeviceID.String(), Outcome: "denied", Data: data,
	}); err != nil {
		slog.Error("federation: recording device.local.revoke failed", "error", err)
	}
}
