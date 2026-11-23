// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/api"
	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/audit/sinks"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// batchCapture is a BatchRecorder below the stage under test: it keeps every batch it is handed and notes
// each event appended.
type batchCapture struct {
	batches [][]types.AuditEvent
	singles []types.AuditEvent
}

func (c *batchCapture) Record(_ context.Context, ev types.AuditEvent) error {
	c.singles = append(c.singles, ev)
	return nil
}

func (c *batchCapture) RecordBatch(ctx context.Context, evs []types.AuditEvent) error {
	c.batches = append(c.batches, evs)
	for _, ev := range evs {
		audit.NoteBatch(ctx, ev.ID, audit.BatchAppended)
	}
	return nil
}

// recordedSink keeps every event the sinks were sent.
type recordedSink struct {
	mu  sync.Mutex
	evs []types.AuditEvent
}

func (*recordedSink) Name() string { return "recorded" }

func (s *recordedSink) Emit(_ context.Context, ev types.AuditEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.evs = append(s.evs, ev)
	return nil
}

func (s *recordedSink) got() []types.AuditEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.evs)
}

// deadPool is a pool whose every connection attempt is refused: a store write through it fails.
func deadPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(t.Context(), "postgres://u:p@127.0.0.1:1/db?connect_timeout=2")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

const (
	plantedToken  = "tok-planted-run-token-7f3a9c"
	plantedSecret = "ghp_plantedsecretvalue98765"
)

func plantedRows(n int) []types.AuditEvent {
	run := uuid.New()
	out := make([]types.AuditEvent, n)
	for i := range out {
		data, _ := json.Marshal(map[string]any{"host": "api.example", "authorization": plantedToken, "body": plantedSecret})
		out[i] = types.AuditEvent{ID: uuid.New(), Time: time.Now().UTC(), RunID: &run, ActorType: types.ActorAgent,
			Actor: "spiffe://x/run/" + run.String(), Action: "egress.allow", Target: "api.example", Outcome: "success", Data: data}
	}
	return out
}

// TestAuditChainEveryStageBatches walks the serving chain buildAuditChain builds, through every stage's
// Inner, inner and primary field: each recorder on it takes a batch, and no stage this lane did not name is on
// it. The strict chain and the spool drain's recorder stay per row and are not walked.
func TestAuditChainEveryStageBatches(t *testing.T) {
	dir := t.TempDir()
	sinksJSON, _ := json.Marshal(map[string]any{"file": map[string]any{"path": filepath.Join(dir, "sink.jsonl")}})
	head, _, _, _, err := buildAuditChain(t.Context(), string(sinksJSON), filepath.Join(dir, "spool.jsonl"), "", deadPool(t),
		secretmask.NewRegistry(), newAuditSealSource(audit.SealFields), serveChain{denials: &audit.DenialCoalescer{}})
	if err != nil {
		t.Fatal(err)
	}
	recType := reflect.TypeFor[audit.Recorder]()
	batchType := reflect.TypeFor[audit.BatchRecorder]()
	var seen []string
	var walk func(v reflect.Value)
	walk = func(v reflect.Value) {
		for v.Kind() == reflect.Interface {
			if v.IsNil() {
				return
			}
			v = v.Elem()
		}
		if !v.Type().Implements(recType) {
			return
		}
		seen = append(seen, v.Type().String())
		if !v.Type().Implements(batchType) {
			t.Errorf("stage %s of the serving chain does not take a batch", v.Type())
		}
		s := v
		if s.Kind() == reflect.Pointer {
			if s.IsNil() {
				return
			}
			s = s.Elem()
		}
		if s.Kind() != reflect.Struct {
			return
		}
		for _, name := range []string{"Inner", "inner", "primary"} {
			if f := s.FieldByName(name); f.IsValid() {
				walk(f)
			}
		}
	}
	walk(reflect.ValueOf(&head).Elem())
	want := []string{"audit.DelegationRecorder", "audit.DryRunRecorder", "*audit.DenialCoalescer", "audit.PolicyStampRecorder",
		"main.maskingRecorder", "main.sealingRecorder", "main.spoolingRecorder", "main.fanoutRecorder", "store.Recorder"}
	if !slices.Equal(seen, want) {
		t.Fatalf("serving chain stages = %v, want %v", seen, want)
	}
}

// TestSpoolingRecorderBatch_SpoolsAllAndLogsNoData: a batch the store refuses is spooled whole, logged once
// without its data, and sent to the sinks once per event, without hashes, only when the door releases it.
func TestSpoolingRecorderBatch_SpoolsAllAndLogsNoData(t *testing.T) {
	logs := captureSlog(t)
	spoolPath := filepath.Join(t.TempDir(), "audit-spool.jsonl")
	spool, err := api.NewAuditSpool(spoolPath)
	if err != nil {
		t.Fatal(err)
	}
	sink := &recordedSink{}
	rec := spoolingRecorder{inner: fanoutRecorder{primary: store.Recorder{Pool: deadPool(t)}, fanout: sinks.NewFanout(sink)}, spool: spool}
	evs := plantedRows(3)

	ctx, out := audit.WithBatchOutcome(t.Context())
	if err := rec.RecordBatch(ctx, evs); !errors.Is(err, audit.ErrSpooled) {
		t.Fatalf("RecordBatch = %v, want ErrSpooled", err)
	}
	if n := strings.Count(logs.String(), "AUDIT WRITE FAILED"); n != 1 {
		t.Fatalf("%d failure lines, want one for the batch: %s", n, logs)
	}
	if strings.Contains(logs.String(), plantedToken) || strings.Contains(logs.String(), plantedSecret) {
		t.Fatalf("the log carries the rows' data: %s", logs)
	}
	raw, err := os.ReadFile(spoolPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range evs {
		if !bytes.Contains(raw, []byte(ev.ID.String())) {
			t.Errorf("event %s is not in the spool", ev.ID)
		}
		if st := out.Status(ev.ID); st != audit.BatchSpooled {
			t.Errorf("event %s noted %d, want spooled", ev.ID, st)
		}
	}
	if spool.Lines() != len(evs) {
		t.Fatalf("spool holds %d lines, want %d", spool.Lines(), len(evs))
	}
	if got := sink.got(); len(got) != 0 {
		t.Fatalf("the sinks got %d rows before the door released them", len(got))
	}
	out.Release(ctx, true)
	got := sink.got()
	if len(got) != len(evs) {
		t.Fatalf("the sinks got %d rows after release, want %d", len(got), len(evs))
	}
	for i, ev := range got {
		if ev.ID != evs[i].ID || ev.RowHash != "" || ev.PrevHash != "" {
			t.Errorf("sink row %d = %s with hashes (%q,%q), want %s without hashes", i, ev.ID, ev.PrevHash, ev.RowHash, evs[i].ID)
		}
	}

	ctx, out = audit.WithBatchOutcome(t.Context())
	_ = rec.RecordBatch(ctx, plantedRows(2))
	out.Release(ctx, false)
	if n := len(sink.got()); n != len(evs) {
		t.Fatalf("the sinks got %d rows after a dropped release, want still %d", n, len(evs))
	}
}

// failOwnerKeys seals every subject but one, whose key cannot be had: that subject's rows are pending.
type failOwnerKeys struct {
	key  []byte
	down string
}

func (k failOwnerKeys) Current(_ context.Context, owner, _ string) (uuid.UUID, []byte, error) {
	if owner == k.down {
		return uuid.Nil, nil, errors.New("key store down for this subject")
	}
	return uuid.Nil, append([]byte(nil), k.key...), nil
}

func (k failOwnerKeys) Key(context.Context, string, uuid.UUID) ([]byte, error) {
	return append([]byte(nil), k.key...), nil
}

// TestMaskingAndSealingRecorderBatch: in a batch, a registered secret is masked, a sealed field reaches the store
// as ciphertext, and a pending row goes to the spool and not to the store.
func TestMaskingAndSealingRecorderBatch(t *testing.T) {
	reg := secretmask.NewRegistry()
	run := uuid.New()
	if err := reg.Add(run, []byte(plantedSecret)); err != nil {
		t.Fatal(err)
	}
	spool, err := api.NewAuditSpool(filepath.Join(t.TempDir(), "audit-spool.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	src := newAuditSealSource(audit.SealFields)
	pendingKey := make([]byte, 32)
	src.arm(&audit.Sealer{Keys: failOwnerKeys{key: make([]byte, 32), down: "bob"}, Pending: func() []byte { return pendingKey }})
	inner := &batchCapture{}
	rec := maskingRecorder{inner: sealingRecorder{inner: inner, src: src, spool: spool}, reg: reg}

	masked := plantedRows(1)[0]
	masked.RunID = &run
	sealed := decideRow("a private reason")
	pending := decideRow("bob's private reason")
	pending.Actor = "bob"
	ctx, out := audit.WithBatchOutcome(t.Context())
	if err := rec.RecordBatch(ctx, []types.AuditEvent{masked, sealed, pending}); err != nil {
		t.Fatalf("RecordBatch = %v", err)
	}
	if len(inner.batches) != 1 || len(inner.batches[0]) != 2 || len(inner.singles) != 0 {
		t.Fatalf("below the seal: %d batches, %d singles; want one batch of the two stored rows", len(inner.batches), len(inner.singles))
	}
	got := inner.batches[0]
	if got[0].ID != masked.ID || strings.Contains(string(got[0].Data), plantedSecret) {
		t.Fatalf("the registered secret reached the store: %s", got[0].Data)
	}
	if got[1].ID != sealed.ID || strings.Contains(string(got[1].Data), "a private reason") || audit.IsPending(got[1]) {
		t.Fatalf("the sealed field reached the store in the clear or pending: %s", got[1].Data)
	}
	if spool.Lines() != 1 || out.Status(pending.ID) != audit.BatchSpooled {
		t.Fatalf("pending row: spool lines %d, noted %d; want it in the spool", spool.Lines(), out.Status(pending.ID))
	}
	for _, ev := range got {
		if ev.ID == pending.ID {
			t.Fatal("the pending row reached the store")
		}
	}
}

// TestSealingRecorderBatch_OneRefusedOthersStored: a row the sealer refuses inside a batch is noted refused and
// is the result, and the batch's other rows are still stored.
func TestSealingRecorderBatch_OneRefusedOthersStored(t *testing.T) {
	src := newAuditSealSource(audit.SealFields)
	src.arm(&audit.Sealer{Keys: miscCovFixedKeys{key: make([]byte, 32)}})
	inner := &batchCapture{}
	rec := sealingRecorder{inner: inner, src: src}
	plain := plantedRows(1)[0]
	sealed := decideRow("words")
	forged := types.AuditEvent{ID: uuid.New(), Time: time.Now().UTC(), ActorType: types.ActorHuman, Actor: "admin",
		Action: "person.token.create", Target: audit.SubjectActorPrefix + uuid.NewString(), Outcome: "success"}
	ctx, out := audit.WithBatchOutcome(t.Context())
	err := rec.RecordBatch(ctx, []types.AuditEvent{plain, forged, sealed})
	if err == nil || errors.Is(err, audit.ErrSpooled) {
		t.Fatalf("RecordBatch = %v, want the seal refusal", err)
	}
	if len(inner.batches) != 1 || len(inner.batches[0]) != 2 || inner.batches[0][0].ID != plain.ID || inner.batches[0][1].ID != sealed.ID {
		t.Fatalf("below the seal: %v, want the two other rows in one batch", inner.batches)
	}
	if out.Status(forged.ID) != audit.BatchRefused || out.Status(plain.ID) != audit.BatchAppended || out.Status(sealed.ID) != audit.BatchAppended {
		t.Fatalf("notes = %d %d %d, want appended, refused, appended", out.Status(plain.ID), out.Status(forged.ID), out.Status(sealed.ID))
	}
}

// TestAuditBatchStages_LogNoData: the batch stages' log lines, for a store failure and a seal refusal, carry
// no row data, token or secret.
func TestAuditBatchStages_LogNoData(t *testing.T) {
	logs := captureSlog(t)
	spool, err := api.NewAuditSpool(filepath.Join(t.TempDir(), "audit-spool.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	src := newAuditSealSource(audit.SealFull)
	src.arm(&audit.Sealer{Keys: miscCovFixedKeys{key: make([]byte, 32)}})
	chain := sealingRecorder{inner: spoolingRecorder{inner: fanoutRecorder{primary: store.Recorder{Pool: deadPool(t)}}, spool: spool}, src: src, spool: spool}
	evs := plantedRows(2)
	forged := evs[1]
	forged.ID, forged.ActorType, forged.Target = uuid.New(), types.ActorHuman, audit.SubjectActorPrefix+uuid.NewString()
	evs = append(evs, forged)
	ctx, out := audit.WithBatchOutcome(t.Context())
	_ = chain.RecordBatch(ctx, evs)
	out.Release(ctx, true)
	if !strings.Contains(logs.String(), "AUDIT WRITE FAILED") || !strings.Contains(logs.String(), "seal_refused") {
		t.Fatalf("log = %q, want the store failure and the seal refusal", logs)
	}
	for _, planted := range []string{plantedToken, plantedSecret, "api.example"} {
		if strings.Contains(logs.String(), planted) {
			t.Fatalf("the log carries %q: %s", planted, logs)
		}
	}
}
