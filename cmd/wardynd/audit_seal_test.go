// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/api"
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

func (downKeys) Current(context.Context, string, string) (uuid.UUID, []byte, error) {
	return uuid.Nil, nil, errors.New("key store down")
}
func (downKeys) Key(context.Context, string, uuid.UUID) ([]byte, error) {
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

// oneSubject is an audit.SubjectDirectory that knows one person.
type oneSubject struct{ principal, id string }

func (d oneSubject) AuditSubjectFor(_ context.Context, p string) (string, bool, error) {
	return d.id, p == d.principal, nil
}
func (d oneSubject) AuditPrincipalOf(_ context.Context, id string) (string, bool, error) {
	return d.principal, id == d.id, nil
}
func (oneSubject) AuditKeyDestroyedAt(context.Context, string) (time.Time, bool, error) {
	return time.Time{}, false, nil
}

// fullSealer is a SealFull Sealer over alice, its keys down, holding pending
// rows under pending.
func fullSealer(pending []byte) *audit.Sealer {
	return &audit.Sealer{Keys: downKeys{}, Pending: func() []byte { return pending },
		Subjects: oneSubject{principal: "alice", id: uuid.NewString()}, SealActor: true}
}

// pendingActorRow is a row with no sealed field whose human actor waits under
// pending: written under SealFull while the keys were down.
func pendingActorRow(t *testing.T, pending []byte) types.AuditEvent {
	t.Helper()
	row, isPending, err := fullSealer(pending).Seal(t.Context(), types.AuditEvent{ID: uuid.New(), Time: time.Now().UTC(),
		ActorType: types.ActorHuman, Actor: "alice", Action: "run.kill", Target: "r1", Outcome: "success", Data: json.RawMessage(`{}`)})
	if err != nil || !isPending || row.Actor != audit.PendingActor {
		t.Fatalf("seal with the keys down: %v, pending %v, actor %q", err, isPending, row.Actor)
	}
	return row
}

// The actor twin of the test above: a pending actor that does not open under
// the pending key, or is malformed, never heals by waiting either.
func TestSealingRecorderReplayDoesNotDeferAnUnopenablePendingActor(t *testing.T) {
	oldKey, newKey := make([]byte, 32), make([]byte, 32)
	newKey[0] = 1
	malformed := pendingActorRow(t, newKey)
	malformed.Data = json.RawMessage(`{"` + audit.PendingMarker + `":true,"` + audit.PendingActorKey + `":"seal2p.!!"}`)
	for name, row := range map[string]types.AuditEvent{"under another pending key": pendingActorRow(t, oldKey), "malformed": malformed} {
		inner := &captureRecorder{}
		src := newAuditSealSource(audit.SealFull)
		src.arm(fullSealer(newKey))
		err := sealingRecorder{inner: inner, replay: true, src: src}.Record(t.Context(), row)
		if errors.Is(err, audit.ErrReplayDeferred) || !errors.Is(err, audit.ErrPendingUnopenable) {
			t.Errorf("%s: err = %v, want ErrPendingUnopenable and not ErrReplayDeferred", name, err)
		}
		if len(inner.evs) != 0 {
			t.Errorf("%s: an unopenable row reached the store: %d rows", name, len(inner.evs))
		}
	}
}

// One unopenable pending actor at the head of the spool does not hold the
// lines behind it: the drain strikes it, quarantines it once a line behind it
// lands, and the rest drains.
func TestAuditSpoolDrainsPastAnUnopenablePendingActor(t *testing.T) {
	spoolPath := filepath.Join(t.TempDir(), "audit-spool.jsonl")
	spool, err := api.NewAuditSpool(spoolPath)
	if err != nil {
		t.Fatal(err)
	}
	oldKey, newKey := make([]byte, 32), make([]byte, 32)
	newKey[0] = 1
	bad := pendingActorRow(t, oldKey)
	good := types.AuditEvent{ID: uuid.New(), Time: time.Now().UTC(), ActorType: types.ActorSystem, Actor: "wardynd",
		Action: "run.kill", Target: "r2", Outcome: "success", Data: json.RawMessage(`{"reason":"superseded_by_new_login"}`)}
	for _, ev := range []types.AuditEvent{bad, good} {
		if err := spool.Append(ev); err != nil {
			t.Fatal(err)
		}
	}
	inner := &captureRecorder{}
	src := newAuditSealSource(audit.SealFull)
	src.arm(fullSealer(newKey))
	drain := sealingRecorder{inner: inner, replay: true, src: src}
	for range 6 {
		if _, err := spool.Drain(t.Context(), drain, 10); err == nil && spool.Lines() == 0 {
			break
		}
	}
	if len(inner.evs) != 1 || inner.evs[0].ID != good.ID {
		t.Fatalf("the store got %d rows %v, want only the line behind the unopenable one", len(inner.evs), inner.evs)
	}
	if spool.Lines() != 0 {
		t.Errorf("the spool still holds %d lines", spool.Lines())
	}
	if q := readFileOrEmpty(spoolPath + ".quarantine"); !strings.Contains(q, bad.ID.String()) {
		t.Errorf("the quarantine sidecar = %q, want the unopenable line", q)
	}
}

func TestAuditSealPurposeIsTheSubjectKeyPurpose(t *testing.T) {
	if audit.SealPurpose != subjectkey.PurposeAuditSeal {
		t.Fatalf("audit.SealPurpose = %q, subjectkey.PurposeAuditSeal = %q: sealed rows would be read under another purpose's key", audit.SealPurpose, subjectkey.PurposeAuditSeal)
	}
}
