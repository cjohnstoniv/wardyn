// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/subjectkey"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

type captureRecorder struct{ evs []types.AuditEvent }

func (c *captureRecorder) Record(_ context.Context, ev types.AuditEvent) error {
	c.evs = append(c.evs, ev)
	return nil
}

func decideRow(reason string) types.AuditEvent {
	data, _ := json.Marshal(map[string]any{"approval_id": "a1", "reason": reason})
	return types.AuditEvent{ID: uuid.New(), Time: time.Now().UTC(), ActorType: types.ActorHuman, Actor: "alice",
		Action: "approval.decide", Target: "a1", Outcome: "success", Data: data}
}

func TestAuditSealModeServesAllThreeAndRefusesAMisspelling(t *testing.T) {
	for in, wantErr := range map[string]string{"": "", "off": "", "fields": "", "full": "", "fileds": "want off, fields or full"} {
		f := &bootFlags{auditSeal: &in}
		_, err := auditSealMode(f)
		if (err == nil) != (wantErr == "") || (err != nil && !strings.Contains(err.Error(), wantErr)) {
			t.Errorf("WARDYN_AUDIT_SEAL=%q: err = %v, want containing %q", in, err, wantErr)
		}
	}
}

// With sealing off every row passes through; a row that has no sealed field
// passes through whatever the mode; and a row that needs sealing is refused,
// never written in the clear, while the keys are not armed.
func TestSealingRecorderNeverWritesAPersonalFieldInTheClear(t *testing.T) {
	inner := &captureRecorder{}
	off := sealingRecorder{inner: inner, src: newAuditSealSource(audit.SealOff)}
	if err := off.Record(t.Context(), decideRow("words")); err != nil || len(inner.evs) != 1 || !strings.Contains(string(inner.evs[0].Data), "words") {
		t.Fatalf("mode off: %v, rows %d; want the row as it was", err, len(inner.evs))
	}

	inner = &captureRecorder{}
	on := sealingRecorder{inner: inner, src: newAuditSealSource(audit.SealFields)}
	if err := on.Record(t.Context(), types.AuditEvent{Action: "run.kill", Data: json.RawMessage(`{"reason":"sweep_recovered_kill_tail"}`)}); err != nil || len(inner.evs) != 1 {
		t.Fatalf("a row with no sealed field: %v, rows %d; want it passed through", err, len(inner.evs))
	}
	if err := on.Record(t.Context(), decideRow("words")); err == nil {
		t.Fatal("a row with a personal field was recorded before the keys were armed")
	}
	if len(inner.evs) != 1 {
		t.Fatalf("the refused row reached the recorder below: %d rows", len(inner.evs))
	}

	// A nil source (a maintenance mode) records as it always did.
	inner = &captureRecorder{}
	if err := (sealingRecorder{inner: inner}).Record(t.Context(), decideRow("words")); err != nil || len(inner.evs) != 1 {
		t.Fatalf("nil source: %v", err)
	}
}

// Under full a person's row needs the Sealer whatever its action, and so is
// refused, never written under the person's name, before the keys are armed; an
// agent's row and every row under fields still pass through.
func TestSealingRecorderUnderFullNeedsTheSealerForAnyHumanRow(t *testing.T) {
	human := types.AuditEvent{ActorType: types.ActorHuman, Actor: "alice", Action: "run.kill"}
	agent := types.AuditEvent{ActorType: types.ActorAgent, Actor: "spiffe://x/run/1", Action: "run.start"}

	inner := &captureRecorder{}
	full := sealingRecorder{inner: inner, src: newAuditSealSource(audit.SealFull)}
	if err := full.Record(t.Context(), human); err == nil || len(inner.evs) != 0 {
		t.Fatalf("a human row under full before the keys were armed: err %v, rows %d; want a refusal", err, len(inner.evs))
	}
	if err := full.Record(t.Context(), agent); err != nil || len(inner.evs) != 1 {
		t.Fatalf("an agent row under full: %v, rows %d; want it passed through", err, len(inner.evs))
	}
	inner = &captureRecorder{}
	fields := sealingRecorder{inner: inner, src: newAuditSealSource(audit.SealFields)}
	if err := fields.Record(t.Context(), human); err != nil || len(inner.evs) != 1 || inner.evs[0].Actor != "alice" {
		t.Fatalf("a human row under fields: %v, rows %v; want it unchanged", err, inner.evs)
	}
}

type downKeys struct{}

func (downKeys) Current(context.Context, string, string) (int, []byte, error) {
	return 0, nil, errors.New("key store down")
}
func (downKeys) Key(context.Context, string, string, int) ([]byte, error) {
	return nil, errors.New("key store down")
}

// A pending row needs a spool to wait in; with none it is refused rather than
// written to the store or dropped silently.
func TestSealingRecorderRefusesAPendingRowWithNoSpool(t *testing.T) {
	inner := &captureRecorder{}
	src := newAuditSealSource(audit.SealFields)
	pending := make([]byte, 32)
	src.arm(&audit.Sealer{Keys: downKeys{}, Pending: func() []byte { return pending }})
	err := sealingRecorder{inner: inner, src: src}.Record(t.Context(), decideRow("words"))
	if err == nil || len(inner.evs) != 0 {
		t.Fatalf("pending row with no spool: err %v, rows below %d; want a refusal and nothing stored", err, len(inner.evs))
	}
}

// In replay mode a row that is not pending goes straight to the store, and one
// that cannot be re-sealed yet is a deferred error the drain never counts toward quarantine.
func TestSealingRecorderReplayDefersWhatItCannotReseal(t *testing.T) {
	inner := &captureRecorder{}
	r := sealingRecorder{inner: inner, replay: true}
	if err := r.Record(t.Context(), decideRow("words")); err != nil || len(inner.evs) != 1 {
		t.Fatalf("a settled row on replay: %v", err)
	}
	src := newAuditSealSource(audit.SealFields)
	pending := make([]byte, 32)
	sealer := &audit.Sealer{Keys: downKeys{}, Pending: func() []byte { return pending }}
	pendingRow, isPending, err := sealer.Seal(t.Context(), decideRow("words"))
	if err != nil || !isPending {
		t.Fatalf("seal with the keys down: %v, pending %v", err, isPending)
	}
	r = sealingRecorder{inner: inner, replay: true, src: src}
	if err := r.Record(t.Context(), pendingRow); !errors.Is(err, audit.ErrReplayDeferred) {
		t.Errorf("unarmed: err = %v, want ErrReplayDeferred", err)
	}
	src.arm(sealer)
	if err := r.Record(t.Context(), pendingRow); !errors.Is(err, audit.ErrReplayDeferred) {
		t.Errorf("keys down: err = %v, want ErrReplayDeferred", err)
	}
	if len(inner.evs) != 1 {
		t.Errorf("a pending row reached the store: %d rows", len(inner.evs))
	}
}

// A pending field that does not open under the pending key never heals by
// waiting: it is not a deferred error, so the drain strikes and quarantines it
// rather than wedging the spool behind it.
func TestSealingRecorderReplayDoesNotDeferAnUnopenablePendingRow(t *testing.T) {
	inner := &captureRecorder{}
	oldKey, newKey := make([]byte, 32), make([]byte, 32)
	newKey[0] = 1
	pendingRow, isPending, err := (&audit.Sealer{Keys: downKeys{}, Pending: func() []byte { return oldKey }}).Seal(t.Context(), decideRow("words"))
	if err != nil || !isPending {
		t.Fatalf("seal with the keys down: %v, pending %v", err, isPending)
	}
	src := newAuditSealSource(audit.SealFields)
	src.arm(&audit.Sealer{Keys: downKeys{}, Pending: func() []byte { return newKey }})
	err = sealingRecorder{inner: inner, replay: true, src: src}.Record(t.Context(), pendingRow)
	if err == nil || errors.Is(err, audit.ErrReplayDeferred) || !errors.Is(err, audit.ErrPendingUnopenable) {
		t.Errorf("pending row under another pending key: err = %v, want ErrPendingUnopenable and not ErrReplayDeferred", err)
	}
	if len(inner.evs) != 0 {
		t.Errorf("an unopenable row reached the store: %d rows", len(inner.evs))
	}
}

func TestAuditSealPurposeIsTheSubjectKeyPurpose(t *testing.T) {
	if audit.SealPurpose != subjectkey.PurposeAuditSeal {
		t.Fatalf("audit.SealPurpose = %q, subjectkey.PurposeAuditSeal = %q: sealed rows would be read under another purpose's key", audit.SealPurpose, subjectkey.PurposeAuditSeal)
	}
}
