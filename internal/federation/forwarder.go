// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package federation

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
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
	GetFederationCursor(ctx context.Context) (seq int64, rowHash string, err error)
	SetFederationCursor(ctx context.Context, seq int64, rowHash string) error
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
// the cursor moves only after acknowledgement, and the organisation
// recognises a re-sent row by seq + row hash (the cursor keeps both, since a
// local table reset can reuse the seq).
type Forwarder struct {
	client   *Client
	store    Store
	cred     Credential
	audit    audit.Recorder
	interval time.Duration

	ackedHash string // row_hash of the local row at status.AckedSeq when it was acknowledged

	mu      sync.Mutex
	status  Status
	loaded  bool
	halted  bool
	backoff time.Duration

	// markPending is true from a revoke whose durable write failed, until step
	// retries it. Status().Revoked closes the instant revoke runs regardless —
	// this only tracks the durable write.
	markPending bool

	// done closes once Run returns, so a caller on its own goroutine can join it.
	done chan struct{}
}

// NewForwarder returns a forwarder for cred. Nothing is read until Load or Run.
func NewForwarder(c *Client, st Store, cred Credential, rec audit.Recorder) *Forwarder {
	return &Forwarder{client: c, store: st, cred: cred, audit: rec, interval: tickInterval,
		status: Status{DeviceID: cred.DeviceID}, done: make(chan struct{})}
}

// Done closes once Run has returned — after ctx ends or the organisation
// revokes this device. A caller running Run on its own goroutine must join it
// (cancel, then <-Done()) before treating shutdown as complete. Run must be
// called at most once per Forwarder; a second call double-closes done and panics.
func (f *Forwarder) Done() <-chan struct{} { return f.done }

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

// Load reads the durable cursor and revoked mark, called before the server
// starts so a laptop revoked before a restart refuses new runs from its first
// request, regardless of organisation reachability.
func (f *Forwarder) Load(ctx context.Context) error {
	acked, ackedHash, err := f.store.GetFederationCursor(ctx)
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
	f.ackedHash, f.loaded = ackedHash, true
	return nil
}

// Run forwards until ctx ends or the organisation revokes this device. A
// revoked forwarder stops calling the organisation for the rest of the
// process: its credential is dead, and re-enrolment is a boot-time act.
func (f *Forwarder) Run(ctx context.Context) {
	defer close(f.done)
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
// Head is read before anything can fail upstream, so an unreachable
// organisation shows as growing lag. Rows with no row_hash predate the chain
// and can't be verified upstream; skipped, never sent, cursor moves past them.
func (f *Forwarder) step(ctx context.Context) (time.Duration, bool) {
	if !f.loaded {
		if err := f.Load(ctx); err != nil {
			return f.retry(err, 0), false
		}
	}
	if f.Status().Revoked {
		if f.markPending {
			f.markRevokedDurable(ctx)
			if f.markPending {
				return f.interval, false
			}
		}
		return 0, true
	}
	head, err := f.store.AuditHeadSeq(ctx)
	if err != nil {
		return f.retry(err, 0), false
	}
	f.update(func(s *Status) { s.HeadSeq = head })
	acked := f.Status().AckedSeq

	var rows []types.FederatedAuditEvent
	if acked > 0 {
		// One read, one snapshot: the row at the cursor, then the batch after
		// it. It must still be the row the organisation acknowledged; if gone
		// or carrying another hash, the local chain was reset — rows after it
		// would link to nothing the organisation holds, so start over from
		// genesis (it skips what it already holds and records a chain reset).
		if rows, err = f.store.ListAuditEventsAfterSeq(ctx, acked-1, batchSize+1); err != nil {
			return f.retry(err, 0), false
		}
		if len(rows) > 0 && rows[0].Seq == acked && rows[0].RowHash == f.ackedHash {
			rows = rows[1:]
		} else {
			slog.Error("federation: the local audit row at the forwarding cursor is gone or changed; the local table was reset, resending from the start",
				"device_id", f.cred.DeviceID, "head_seq", head, "acked_seq", acked)
			if err := f.store.SetFederationCursor(ctx, 0, ""); err != nil {
				return f.retry(err, 0), false
			}
			acked, f.ackedHash, rows = 0, "", nil
			f.update(func(s *Status) { s.AckedSeq = 0 })
		}
	}
	if acked == 0 && !f.halted {
		if rows, err = f.store.ListAuditEventsAfterSeq(ctx, 0, batchSize); err != nil {
			return f.retry(err, 0), false
		}
	}
	if f.halted {
		rows = nil
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
		// The hash of the row at next, as read; a next that is not a row read
		// here keeps "", which the next tick reads as a reset and resends from
		// the start — the safe direction.
		hash := ""
		if i := slices.IndexFunc(rows, func(e types.FederatedAuditEvent) bool { return e.Seq == next }); i >= 0 {
			hash = rows[i].RowHash
		}
		if err := f.store.SetFederationCursor(ctx, next, hash); err != nil {
			return f.retry(err, 0), false
		}
		f.ackedHash = hash
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
// 401/410 revoke; 429 waits per Retry-After or backs off if it's unusable;
// any other 4xx is definitive and not retried (halts pushing, keeps
// heart-beating so revocation is still noticed, reports on status until a
// restart); everything else backs off and retries.
func (f *Forwarder) refused(ctx context.Context, err error) (time.Duration, bool) {
	var se *StatusError
	if !errors.As(err, &se) {
		return f.retry(err, 0), false
	}
	switch {
	case se.Revoked():
		f.revoke(ctx, se)
		// Status().Revoked is already closed; only stop calling the organisation
		// once the mark is durable too, else step retries the write next tick
		// without contacting it again.
		if f.markPending {
			return f.interval, false
		}
		return 0, true
	case se.Code == http.StatusUnauthorized:
		// A 401 without deviceAuth's own realm: something on the path answered,
		// not the organisation. Halt like any definitive refusal, but say what
		// actually happened rather than logging a bland "refused".
		f.halted = true
		slog.Error("federation: 401 from something that is not the organisation; forwarding is halted until wardynd restarts",
			"device_id", f.cred.DeviceID, "acked_seq", f.Status().AckedSeq)
		f.update(func(s *Status) { s.LastError = "401 from something that is not the organisation" })
		return f.interval, false
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
	f.markRevokedDurable(ctx)
	data, _ := json.Marshal(map[string]any{"status": se.Code, "acked_seq": st.AckedSeq})
	if err := f.audit.Record(ctx, types.AuditEvent{
		ID: uuid.New(), Time: time.Now().UTC(), ActorType: types.ActorSystem, Actor: AuditActor,
		Action: "device.local.revoke", Target: f.cred.DeviceID.String(), Outcome: "denied", Data: data,
	}); err != nil {
		slog.Error("federation: recording device.local.revoke failed", "error", err)
	}
}

// markRevokedDurable tries once to persist the revoked mark, clearing
// markPending on success or setting it on failure so step retries next tick.
// Status().Revoked is already closed either way — the credential is never
// trusted again on the strength of an unwritten mark, but a restart before
// the write lands would forget it.
func (f *Forwarder) markRevokedDurable(ctx context.Context) {
	if err := f.store.MarkFederationRevoked(ctx); err != nil {
		slog.Error("federation: recording the revocation durably failed; retrying on the tick loop", "error", err)
		f.markPending = true
		return
	}
	f.markPending = false
}
