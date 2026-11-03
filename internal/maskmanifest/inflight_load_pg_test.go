// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package maskmanifest_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/audit/sinks"
	"github.com/cjohnstoniv/wardyn/internal/maskmanifest"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/kek"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/subjectkey/subjectkeytest"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

type lastSink struct {
	mu sync.Mutex
	ev types.AuditEvent
}

func (*lastSink) Name() string { return "last" }
func (l *lastSink) Emit(_ context.Context, ev types.AuditEvent) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ev = ev
	return nil
}

// TestPG_ALoadInFlightWhenTheCorpusIsDroppedCannotVouchForItAgain: replica B's load of a run's manifest has
// registered the value and is about to install its entry when the fence lands and the value is dropped, by
// a run-value drop or by a process-wide one. The load keeps nothing, B never holds the run, and the audit
// sink gets neither the value nor the id.
func TestPG_ALoadInFlightWhenTheCorpusIsDroppedCannotVouchForItAgain(t *testing.T) {
	const secret = "zq-throwaway-inflight-10"
	for _, global := range []bool{false, true} {
		name := map[bool]string{false: "a run value drop", true: "a process-wide value drop"}[global]
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			migrated := subjectkeytest.ThrowawayDB(t)
			id, err := age.GenerateX25519Identity()
			if err != nil {
				t.Fatal(err)
			}
			k, err := kek.NewLocalPurpose(id, kek.PurposeCred)
			if err != nil {
				t.Fatal(err)
			}
			keys := subjectkeytest.Manager(migrated, k)
			run := uuid.New()

			mA := maskmanifest.New(migrated, keys, secretmask.NewRegistry())
			if err := mA.Start(ctx, run, "alice"); err != nil {
				t.Fatal(err)
			}
			if err := mA.Append(ctx, run, []byte(secret)); err != nil {
				t.Fatal(err)
			}
			if err := mA.Complete(ctx, run); err != nil {
				t.Fatal(err)
			}

			regB := secretmask.NewRegistry()
			mB := maskmanifest.New(migrated, keys, regB)
			rowID := uuid.New()
			if global {
				regB.ApplyGlobal(rowID, "alice", "sso", []byte(secret), time.Time{}, time.Time{})
			}
			paused, resume := make(chan struct{}), make(chan struct{})
			mB.SetLoadPause(func(stage string, _ uuid.UUID) {
				if stage == "after-register" { // the value is registered here, and the drop below removes it
					close(paused)
					<-resume
				}
			})
			covered := make(chan bool, 1)
			go func() { covered <- mB.Covered(ctx, run) }()
			<-paused // the value is registered, the entry is not installed

			if _, err := mA.FenceSubject(ctx, "alice"); err != nil { // the erasure, durable first
				t.Fatal(err)
			}
			if global {
				regB.DropGlobalValue(rowID) // the tombstone this replica applies
			} else {
				regB.DropRunValue(run, []byte(secret))
			}
			close(resume)
			if <-covered {
				t.Error("the load that was in flight reported the run covered")
			}
			if mB.Held(run) {
				t.Fatal("replica B holds the run again from a load that began before the drop")
			}

			sink := &lastSink{}
			fan := sinks.NewFanout(sink)
			fan.EnrichRuns(func(context.Context, uuid.UUID) (string, bool, error) { return secret, true, nil },
				func(rid uuid.UUID, b []byte) []byte { return regB.JSONVariantMasker(rid).Mask(b) }, mB.Held)
			if err := fan.Emit(ctx, types.AuditEvent{ID: uuid.New(), RunID: &run, Action: "run.kill", Outcome: "success", Data: json.RawMessage(`{"k":1}`)}); err != nil {
				t.Fatal(err)
			}
			sink.mu.Lock()
			defer sink.mu.Unlock()
			if strings.Contains(string(sink.ev.Data), secret) || strings.Contains(string(sink.ev.Data), sinks.CorrelationKey) {
				t.Errorf("the sink got %s", sink.ev.Data)
			}
		})
	}
}

// TestPG_ALoadVoidedByAForgetTakesBackWhatItRegistered: a load that finds the run forgotten before it
// registers, or after, leaves the registry without the values it read; values someone else registered stay.
func TestPG_ALoadVoidedByAForgetTakesBackWhatItRegistered(t *testing.T) {
	const secret, injected = "zq-throwaway-rollback-11", "zq-throwaway-injected-12"
	for _, stage := range []string{"before-register", "after-register"} {
		t.Run(stage, func(t *testing.T) {
			ctx := t.Context()
			migrated := subjectkeytest.ThrowawayDB(t)
			id, err := age.GenerateX25519Identity()
			if err != nil {
				t.Fatal(err)
			}
			k, err := kek.NewLocalPurpose(id, kek.PurposeCred)
			if err != nil {
				t.Fatal(err)
			}
			keys := subjectkeytest.Manager(migrated, k)
			run := uuid.New()
			mA := maskmanifest.New(migrated, keys, secretmask.NewRegistry())
			if err := mA.Start(ctx, run, "alice"); err != nil {
				t.Fatal(err)
			}
			if err := mA.Append(ctx, run, []byte(secret)); err != nil {
				t.Fatal(err)
			}
			if err := mA.Complete(ctx, run); err != nil {
				t.Fatal(err)
			}

			regB := secretmask.NewRegistry()
			mB := maskmanifest.New(migrated, keys, regB)
			if err := regB.Add(run, []byte(injected)); err != nil { // not this load's to take back
				t.Fatal(err)
			}
			paused, resume := make(chan struct{}), make(chan struct{})
			mB.SetLoadPause(func(s string, _ uuid.UUID) {
				if s == stage {
					close(paused)
					<-resume
				}
			})
			covered := make(chan bool, 1)
			go func() { covered <- mB.Covered(ctx, run) }()
			<-paused
			mB.ForgetCache(run) // an erase's local step, short of the eviction
			close(resume)
			if <-covered || mB.Held(run) {
				t.Fatal("the voided load vouched for the run")
			}
			got := regB.Snapshot(run)
			if len(got) != 1 || string(got[0]) != injected {
				t.Errorf("the registry holds %d values after the load, want only the one registered elsewhere", len(got))
			}
		})
	}
}
