// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Acknowledged audit delivery (#1513): the opt-in mode where a leader-only
// loop reads the already-masked, hash-chained rows from audit_events and
// delivers them to a webhook collector from a durable checkpoint, advancing
// only on the collector's acceptance. At-least-once: a collector deduplicates
// on id (or seq + row_hash), and a 2xx means accepted, not stored.
//
// Nothing here runs on a write path. A stalled collector grows lag and
// backoff; it never delays an audit row, a kill or a revoke.
package sinks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/ackcursor"
	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// AckedDestination is the one checkpoint destination: there is one webhook per
// sinks config, so a second row could only name a second config.
const AckedDestination = "webhook"

// deliveryMaxBackoff caps both the doubling backoff and a Retry-After the
// collector asks for, so a collector can never park the loop for good.
const deliveryMaxBackoff = 5 * time.Minute

// DeliveryStore is the durable checkpoint and the audit table the loop reads
// (store.PG). None of it is on the Store interface: like the federation
// cursor, these methods serve one background loop.
type DeliveryStore interface {
	ListAuditEventsAfterSeq(ctx context.Context, seq int64, limit int) ([]types.FederatedAuditEvent, error)
	EnsureAuditDeliveryCursor(ctx context.Context, dest string) error
	GetAuditDeliveryCursor(ctx context.Context, dest string) (int64, string, error)
	SetAuditDeliveryCursor(ctx context.Context, dest string, seq int64, rowHash string) error
	SetAuditDeliveryError(ctx context.Context, dest, msg string, halted bool) error
}

// AckedWebhook delivers the stored audit trail to one acknowledged collector.
type AckedWebhook struct {
	cfg      WebhookConfig
	client   *http.Client
	interval time.Duration
	base     time.Duration
	st       DeliveryStore
	rec      audit.Recorder

	cursor  *ackcursor.Cursor[types.FederatedAuditEvent]
	loaded  bool
	backoff time.Duration
	// benignReset is set by reset when retention removed only rows at or before
	// the checkpoint, so ackedStore.Set(zero) keeps the durable position (and
	// the resets counter) as it is: the next accepted batch moves it anyway.
	benignReset bool
	// benignAt is the checkpoint a benign reset classified, held until the
	// replacement batch is in hand: reset and the batch are two reads, and
	// retention can drop the first unacknowledged row between them.
	benignAt ackcursor.Pos
	// benignPending is true from a skipped durable reset until the next accepted
	// batch: the cursor's memory is at zero while the durable checkpoint is
	// still p, so a failed delivery must re-read p and re-run the gap check,
	// not keep resending from zero over rows retention may drop meanwhile.
	benignPending bool
	// stale is true while the durable last_error/halted may disagree with the
	// loop: after a load (a previous run may have left a halt or an error) and
	// after any recorded error. The first accepted batch clears it.
	stale bool
}

// NewAckedWebhook validates cfg and wires the cursor over st. BatchSize is
// the cursor's per-step row count, the same knob the fan-out sink batches on.
func NewAckedWebhook(cfg WebhookConfig, st DeliveryStore, rec audit.Recorder) (*AckedWebhook, error) {
	cfg = cfg.withDefaults()
	interval, base, timeout, err := cfg.validate()
	if err != nil {
		return nil, err
	}
	a := &AckedWebhook{
		// A redirect is never followed: net/http would turn the POST into a
		// bodyless GET, and the collector's 200 for that GET would advance the
		// checkpoint over rows it never received.
		cfg: cfg, client: &http.Client{
			Timeout:       timeout,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		interval: interval, base: base, st: st, rec: rec,
	}
	a.cursor = &ackcursor.Cursor[types.FederatedAuditEvent]{
		Source: ackedSource{st}, Sink: ackedSink{a}, Store: ackedStore{st, a}, Batch: cfg.BatchSize,
		Key: func(e types.FederatedAuditEvent) ackcursor.Pos { return ackcursor.Pos{Seq: e.Seq, Hash: e.RowHash} },
		// Wired here so EVERY path that steps the cursor reports the gap; Run
		// rebinds it to its own ctx, which scopes the recording to the loop's
		// lifetime.
		OnReset: func(p ackcursor.Pos) { a.reset(context.Background(), p) },
	}
	return a, nil
}

// AckedTerms validates cfg and returns the function to run on the sweeper
// leader: it builds a fresh AckedWebhook per call, so every leader term loads
// the durable checkpoint instead of resuming a stale in-memory one that
// another replica has since moved past.
func AckedTerms(cfg WebhookConfig, st DeliveryStore, rec audit.Recorder) (func(context.Context), error) {
	if _, err := NewAckedWebhook(cfg, st, rec); err != nil {
		return nil, err
	}
	return func(ctx context.Context) {
		a, err := NewAckedWebhook(cfg, st, rec)
		if err != nil {
			slog.Error("sinks.acked: acknowledged audit delivery not started", "error", err)
			return
		}
		a.Run(ctx)
	}, nil
}

// Run delivers until ctx ends or the collector refuses definitively (a
// terminal 4xx or an unfollowed redirect halts until wardynd restarts or another replica takes over delivery). It
// must be called at most once per AckedWebhook; AckedTerms gives each leader
// term its own.
func (a *AckedWebhook) Run(ctx context.Context) {
	// Rebind the reset hook to this Run's ctx so the recording is scoped to
	// the loop's lifetime, as federation.Forwarder.Run does.
	a.cursor.OnReset = func(p ackcursor.Pos) { a.reset(ctx, p) }
	for {
		wait, stop := a.step(ctx)
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

// step delivers the next batch and says how long to wait before the next, the
// shape federation.Forwarder.step uses: the first step also loads the
// checkpoint, so a cold start and a retry after a failed load are the same
// code.
func (a *AckedWebhook) step(ctx context.Context) (time.Duration, bool) {
	if !a.loaded {
		if err := a.load(ctx); err != nil {
			return a.retry(ctx, nil, 0), false
		}
		a.backoff = 0
	}
	more, err := a.cursor.Step(ctx)
	if err != nil && a.benignPending {
		a.benignPending = false
		if a.cursor.Load(ctx) != nil {
			a.loaded = false
		}
	}
	switch {
	case err == nil:
		a.benignPending = false
		if a.stale {
			a.recordError(ctx, "", false)
		}
		a.backoff = 0
		if more {
			return 0, false
		}
		return a.interval, false
	case terminal(err):
		a.halt(ctx, err)
		return 0, true
	default:
		return a.retry(ctx, err, retryAfterOf(err)), false
	}
}

// load makes the checkpoint row exist — first enable starts it at the current
// head, so only new events are sent — and reads it. A database that cannot
// answer yet is waited out rather than treated as a collector failure.
func (a *AckedWebhook) load(ctx context.Context) error {
	if err := a.st.EnsureAuditDeliveryCursor(ctx, AckedDestination); err != nil {
		return err
	}
	if err := a.cursor.Load(ctx); err != nil {
		return err
	}
	a.loaded, a.stale = true, true
	return nil
}

// retry records err and returns the next wait: the backoff doubled from the
// configured base, capped at deliveryMaxBackoff and never shorter than a
// Retry-After the collector asked for.
func (a *AckedWebhook) retry(ctx context.Context, err error, retryAfter time.Duration) time.Duration {
	a.backoff = min(max(2*a.backoff, a.base), deliveryMaxBackoff)
	wait := min(max(a.backoff, retryAfter), deliveryMaxBackoff)
	msg := "loading the audit delivery checkpoint failed"
	if err != nil {
		msg = errorText(err)
	}
	a.recordError(ctx, msg, false)
	slog.Warn("sinks.acked: audit delivery failed; retrying",
		"destination", AckedDestination, "error", msg, "retry_in", wait)
	return wait
}

// halt records a terminal refusal and stops the loop: the collector answered a
// 4xx it will answer the same way on every retry, so retrying is noise. The
// flag is durable, so the gauge keeps reading halted until a later run (after
// a wardynd restart, or another replica's leader term) delivers a batch and
// clears it.
func (a *AckedWebhook) halt(ctx context.Context, err error) {
	msg := errorText(err)
	a.recordError(ctx, msg, true)
	slog.Error("sinks.acked: the collector refused an audit batch definitively; delivery is halted until wardynd restarts or another replica takes over delivery",
		"destination", AckedDestination, "error", msg)
}

// recordError persists the collector's answer on the checkpoint row, where the
// delivery metrics read it. A store that refuses the write fails the next
// Step anyway, so this only logs.
func (a *AckedWebhook) recordError(ctx context.Context, msg string, halted bool) {
	if err := a.st.SetAuditDeliveryError(ctx, AckedDestination, msg, halted); err != nil {
		a.stale = true
		slog.Warn("sinks.acked: recording the delivery state failed",
			"destination", AckedDestination, "error", err)
		return
	}
	a.stale = msg != "" || halted
}

// reset runs when the row at the checkpoint is gone or changed: retention
// moved past the checkpoint, or the table was reset. The gap is reported at
// ERROR and recorded as an audit event, so the collector itself learns of it;
// ackcursor.Cursor.Step then clears the position, so the next batch is resent
// from the oldest retained row. The resets counter is bumped by the store's
// Set of the zero position (store.SetAuditDeliveryCursor).
//
// A gap that cost nothing is not reported: when the checkpoint row is gone but
// the next retained row is the very next seq, retention removed only rows the
// collector had already accepted, and the resend starts at the first
// unacknowledged row anyway.
func (a *AckedWebhook) reset(ctx context.Context, p ackcursor.Pos) {
	// A verdict left by a step whose batch read failed is judged afresh here;
	// keeping it would report a loss found now a second time in Deliver.
	a.benignAt = ackcursor.Pos{}
	if rows, err := a.st.ListAuditEventsAfterSeq(ctx, p.Seq-1, 1); err == nil &&
		len(rows) == 1 && rows[0].Seq == p.Seq+1 {
		a.benignReset, a.benignAt = true, p
		slog.Info("sinks.acked: retention removed the audit row at the delivery checkpoint; nothing unacknowledged was lost",
			"destination", AckedDestination, "acked_seq", p.Seq)
		return
	}
	a.reportGap(ctx, p)
}

// reportGap logs a delivery gap at ERROR and records it as an audit event.
func (a *AckedWebhook) reportGap(ctx context.Context, p ackcursor.Pos) {
	slog.Error("sinks.acked: the audit row at the delivery checkpoint is gone or changed; resending from the oldest retained row",
		"destination", AckedDestination, "acked_seq", p.Seq, "acked_row_hash", p.Hash)
	data, err := json.Marshal(map[string]any{"destination": AckedDestination, "acked_seq": p.Seq})
	if err != nil {
		slog.Error("sinks.acked: encoding the reset datum failed", "error", err)
		return
	}
	if err := a.rec.Record(ctx, types.AuditEvent{
		ID: uuid.New(), Time: time.Now().UTC(), ActorType: types.ActorSystem, Actor: "wardynd",
		Action: "audit.delivery.reset", Target: AckedDestination, Outcome: "failure", Data: data,
	}); err != nil {
		slog.Error("sinks.acked: recording audit.delivery.reset failed", "error", err)
	}
}

// deliveryError is a collector answer the loop classified: the HTTP status (0
// for a transport failure), the Retry-After it asked for, and the underlying
// error.
type deliveryError struct {
	status     int
	retryAfter time.Duration
	err        error
}

func (e *deliveryError) Error() string { return e.err.Error() }
func (e *deliveryError) Unwrap() error { return e.err }

// terminal reports whether err is a definitive refusal: a 3xx (the client does
// not follow redirects, and the configured URL will redirect again) or a 4xx
// the collector will answer the same way on every retry, except 408 and 429,
// which ask to be retried. federation.Forwarder.refused applies the 4xx rule.
func terminal(err error) bool {
	var de *deliveryError
	if !errors.As(err, &de) {
		return false
	}
	return de.status >= 300 && de.status < 500 &&
		de.status != http.StatusRequestTimeout && de.status != http.StatusTooManyRequests
}

// retryAfterOf is the Retry-After a classified error carried, 0 otherwise.
func retryAfterOf(err error) time.Duration {
	var de *deliveryError
	if errors.As(err, &de) {
		return de.retryAfter
	}
	return 0
}

// errorText renders err for the checkpoint row and the log. The bearer token
// and the URL (password, path, query) never appear: postNDJSON strips the URL
// net/http embeds in its errors, and a status renders as its number alone.
func errorText(err error) string {
	var de *deliveryError
	if !errors.As(err, &de) {
		return err.Error()
	}
	if de.status >= 300 && de.status < 400 {
		return fmt.Sprintf("collector answered HTTP %d, a redirect; set url to the collector's final address", de.status)
	}
	if de.status > 0 {
		return fmt.Sprintf("collector answered HTTP %d", de.status)
	}
	return "collector unreachable: " + de.err.Error()
}

// ackedSource adapts DeliveryStore to ackcursor.Source.
type ackedSource struct{ st DeliveryStore }

func (s ackedSource) After(ctx context.Context, seq int64, limit int) ([]types.FederatedAuditEvent, error) {
	return s.st.ListAuditEventsAfterSeq(ctx, seq, limit)
}

// ackedStore adapts DeliveryStore to ackcursor.Store, always on the one
// webhook destination.
type ackedStore struct {
	st DeliveryStore
	a  *AckedWebhook
}

func (s ackedStore) Get(ctx context.Context) (ackcursor.Pos, error) {
	seq, hash, err := s.st.GetAuditDeliveryCursor(ctx, AckedDestination)
	return ackcursor.Pos{Seq: seq, Hash: hash}, err
}

func (s ackedStore) Set(ctx context.Context, p ackcursor.Pos) error {
	if s.a.benignReset && p == (ackcursor.Pos{}) {
		s.a.benignReset, s.a.benignPending = false, true
		return nil
	}
	return s.st.SetAuditDeliveryCursor(ctx, AckedDestination, p.Seq, p.Hash)
}

// ackedSink delivers one batch as JSON lines. An empty batch sends nothing —
// the cursor's liveness signal is its own read, not a POST — and only an
// accepted batch reports its last seq, so the checkpoint cannot move past
// rows the collector never held.
type ackedSink struct{ a *AckedWebhook }

func (s ackedSink) Deliver(ctx context.Context, rows []types.FederatedAuditEvent) (int64, error) {
	if p := s.a.benignAt; p.Seq != 0 {
		// The replacement batch is the read that gets delivered: if its first
		// row past the checkpoint is not the very next seq, the benign verdict
		// is stale and rows were lost after all, so count and report the reset.
		// A retained row older than the checkpoint (retention drops time
		// partitions, and seq order can cross them) is not a gap. A full batch
		// of such rows holds no row past the checkpoint and cannot be judged:
		// the verdict is kept for the next batch.
		i := slices.IndexFunc(rows, func(r types.FederatedAuditEvent) bool { return r.Seq > p.Seq })
		if i >= 0 || len(rows) < s.a.cursor.Batch {
			s.a.benignAt = ackcursor.Pos{}
			if i < 0 || rows[i].Seq != p.Seq+1 {
				if err := s.a.st.SetAuditDeliveryCursor(ctx, AckedDestination, 0, ""); err != nil {
					return 0, err
				}
				s.a.reportGap(ctx, p)
			}
		}
	}
	if len(rows) == 0 {
		return 0, nil
	}
	body, err := encodeAckedBatch(rows)
	if err != nil {
		return 0, err
	}
	status, retryAfter, err := postNDJSON(ctx, s.a.client, s.a.cfg, body)
	if err != nil {
		return 0, &deliveryError{status: status, retryAfter: retryAfter, err: err}
	}
	return rows[len(rows)-1].Seq, nil
}

// encodeAckedBatch is the acknowledged wire format: one JSON line per row,
// the federated event as a collector deduplicates on it (id, seq, row_hash)
// plus the Source stamp when one is configured.
func encodeAckedBatch(rows []types.FederatedAuditEvent) ([]byte, error) {
	var buf bytes.Buffer
	for i := range rows {
		b, err := json.Marshal(struct {
			types.FederatedAuditEvent
			Source string `json:"source,omitempty"`
		}{rows[i], Source})
		if err != nil {
			return nil, fmt.Errorf("encode audit event at %d: %w", i, err)
		}
		buf.Write(b)
		buf.WriteByte('\n')
	}
	return buf.Bytes(), nil
}
