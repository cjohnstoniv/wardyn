// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// ─── runMarker: deterministic per-run revocation sentinel jti ────────────────────

// TestRunMarker pins the run-level revocation key shape. identity_revocations.jti
// is the PRIMARY KEY, so the run-marker must be unique per run (not a shared
// empty string) and deterministic so IsRevoked/RevokeRun agree on the same key.
func TestRunMarker(t *testing.T) {
	a := uuid.New()
	b := uuid.New()

	if got, want := runMarker(a), "run:"+a.String(); got != want {
		t.Fatalf("runMarker(%s) = %q, want %q", a, got, want)
	}
	// Deterministic: the same run id always yields the same marker (else
	// RevokeRun would write a row IsRevoked could never find).
	if runMarker(a) != runMarker(a) {
		t.Fatal("runMarker must be deterministic for a given run id")
	}
	// Unique per run: distinct run ids must not collide on the jti PK.
	if runMarker(a) == runMarker(b) {
		t.Fatalf("runMarker collided for distinct run ids %s and %s", a, b)
	}
	// The "run:" prefix distinguishes a run-level marker from a real token jti.
	if !strings.HasPrefix(runMarker(a), "run:") {
		t.Fatalf("runMarker %q lacks the run: prefix", runMarker(a))
	}
}

// ─── FIX #5: approval recorder is the masked + fanout recorder ───────────────────

// TestApprovalRecorderIsMaskedFanout is the regression guard for FIX #5: the
// approval FSM + sweeper were constructed with the PLAIN store.Recorder (Postgres
// only), so approval.decide / approval.expire audit events bypassed masking AND
// the SIEM fanout that idp/broker already used (maskedRec). The recorder seam must
// therefore hold an audit.Recorder (the masked+fanout one), not a concrete
// store.Recorder, and events must flow through it MASKED.
//
// A fakeAuditRecorder is an audit.Recorder but NOT a store.Recorder — assigning it
// (and a maskingRecorder) into approvalStore.rec / approvalService.rec only
// compiles because the fields are audit.Recorder. If either regressed to
// store.Recorder this test would fail to compile.
func TestApprovalRecorderIsMaskedFanout(t *testing.T) {
	reg := secretmask.NewRegistry()
	runID := uuid.New()
	const secret = "ghp_supersecrettoken123" // >= secretmask.MinLen
	reg.Add(runID, []byte(secret))

	inner := &fakeAuditRecorder{}
	// maskedRec's exact type in main.go: masking in front of the (fanout) recorder.
	masked := maskingRecorder{inner: inner, reg: reg}

	// The approval FSM store + the service both carry the masked/fanout recorder.
	as := approvalStore{rec: masked}
	svc := &approvalService{rec: masked}
	if svc.st().rec == nil {
		t.Fatal("approvalService.st() dropped the recorder (approval audit would vanish)")
	}

	ev := types.AuditEvent{
		RunID:   &runID,
		Action:  "approval.decide",
		Target:  "approval-token=" + secret,
		Outcome: "success",
		Data:    json.RawMessage(`{"decision":"APPROVED","secret":"` + secret + `"}`),
	}
	if err := as.Record(context.Background(), ev); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if inner.calls != 1 {
		t.Fatalf("approval.decide event did not reach the masked/fanout recorder (calls=%d)", inner.calls)
	}
	if inner.last.Action != "approval.decide" {
		t.Fatalf("forwarded action = %q, want approval.decide", inner.last.Action)
	}
	// The event was MASKED on its way through — proof it went through maskedRec, not
	// the raw store recorder (which never masks).
	if strings.Contains(inner.last.Target, secret) || strings.Contains(string(inner.last.Data), secret) {
		t.Fatalf("approval audit not masked: target=%q data=%s", inner.last.Target, inner.last.Data)
	}
}

// ─── maskingRecorder: verbatim secret masking + delegation ──────────────────────

// fakeAuditRecorder is a hand-rolled audit.Recorder capturing the (possibly
// masked) event the maskingRecorder forwards, plus an optional error to assert
// that delegation errors propagate.
type fakeAuditRecorder struct {
	last   types.AuditEvent
	calls  int
	retErr error
}

func (f *fakeAuditRecorder) Record(_ context.Context, ev types.AuditEvent) error {
	f.calls++
	f.last = ev
	return f.retErr
}

// TestMaskingRecorder_MasksRegisteredSecret verifies the masking recorder
// scrubs a verbatim registered secret from BOTH ev.Data and ev.Target before
// the event reaches the inner recorder.
func TestMaskingRecorder_MasksRegisteredSecret(t *testing.T) {
	reg := secretmask.NewRegistry()
	runID := uuid.New()
	const secret = "ghp_supersecrettoken123" // >= secretmask.MinLen
	reg.Add(runID, []byte(secret))

	inner := &fakeAuditRecorder{}
	rec := maskingRecorder{inner: inner, reg: reg}

	ev := types.AuditEvent{
		RunID:  &runID,
		Action: "credential.mint",
		Target: "token=" + secret,
		Data:   json.RawMessage(`{"token":"` + secret + `"}`),
	}
	if err := rec.Record(context.Background(), ev); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if inner.calls != 1 {
		t.Fatalf("inner recorder called %d times, want 1", inner.calls)
	}
	if strings.Contains(string(inner.last.Data), secret) {
		t.Fatalf("secret leaked in masked Data: %s", inner.last.Data)
	}
	if strings.Contains(inner.last.Target, secret) {
		t.Fatalf("secret leaked in masked Target: %s", inner.last.Target)
	}
	if !strings.Contains(string(inner.last.Data), "<secret-hidden>") {
		t.Fatalf("expected placeholder in Data, got %s", inner.last.Data)
	}
	if !strings.Contains(inner.last.Target, "<secret-hidden>") {
		t.Fatalf("expected placeholder in Target, got %s", inner.last.Target)
	}
}

// TestMaskingRecorder_NoRegisteredSecretsPassThrough verifies that with nothing
// registered for the run the event is forwarded byte-for-byte (the masker is a
// pass-through when the snapshot is empty).
func TestMaskingRecorder_NoRegisteredSecretsPassThrough(t *testing.T) {
	reg := secretmask.NewRegistry()
	runID := uuid.New()
	inner := &fakeAuditRecorder{}
	rec := maskingRecorder{inner: inner, reg: reg}

	const target = "token=not-a-registered-secret"
	data := json.RawMessage(`{"k":"v"}`)
	ev := types.AuditEvent{RunID: &runID, Target: target, Data: data}
	if err := rec.Record(context.Background(), ev); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if inner.last.Target != target {
		t.Fatalf("Target altered with no secrets registered: %q", inner.last.Target)
	}
	if string(inner.last.Data) != string(data) {
		t.Fatalf("Data altered with no secrets registered: %s", inner.last.Data)
	}
}

// TestMaskingRecorder_NilRegistryIsNoOp verifies a nil Registry is a safe no-op
// (the event is forwarded unchanged) — the documented contract for components
// wired without a mask registry.
func TestMaskingRecorder_NilRegistryIsNoOp(t *testing.T) {
	runID := uuid.New()
	inner := &fakeAuditRecorder{}
	rec := maskingRecorder{inner: inner, reg: nil}

	const target = "token=ghp_supersecrettoken123"
	ev := types.AuditEvent{RunID: &runID, Target: target}
	if err := rec.Record(context.Background(), ev); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if inner.last.Target != target {
		t.Fatalf("nil registry should not alter Target, got %q", inner.last.Target)
	}
}

// TestMaskingRecorder_NilRunIDIsNoOp verifies an event with no RunID is
// forwarded unchanged: masking is per-run, so without a run id there is no
// snapshot to apply (and a registered secret for some other run must not leak
// into a runless event's masking — it simply isn't masked here).
func TestMaskingRecorder_NilRunIDIsNoOp(t *testing.T) {
	reg := secretmask.NewRegistry()
	other := uuid.New()
	const secret = "ghp_supersecrettoken123"
	reg.Add(other, []byte(secret))

	inner := &fakeAuditRecorder{}
	rec := maskingRecorder{inner: inner, reg: reg}

	target := "token=" + secret
	ev := types.AuditEvent{RunID: nil, Target: target} // no run id
	if err := rec.Record(context.Background(), ev); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if inner.last.Target != target {
		t.Fatalf("runless event should pass through unchanged, got %q", inner.last.Target)
	}
}

// TestMaskingRecorder_DelegationErrorPropagates verifies the masking recorder
// returns the inner recorder's error verbatim — masking must never swallow a
// failed durable write (the store write is authoritative).
func TestMaskingRecorder_DelegationErrorPropagates(t *testing.T) {
	reg := secretmask.NewRegistry()
	runID := uuid.New()
	wantErr := errors.New("inner record failed")
	inner := &fakeAuditRecorder{retErr: wantErr}
	rec := maskingRecorder{inner: inner, reg: reg}

	err := rec.Record(context.Background(), types.AuditEvent{RunID: &runID})
	if !errors.Is(err, wantErr) {
		t.Fatalf("Record error = %v, want %v", err, wantErr)
	}
}
