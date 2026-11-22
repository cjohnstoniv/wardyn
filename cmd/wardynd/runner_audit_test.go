// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/google/uuid"
	"strings"
	"testing"
	"time"
)

func TestRunnerAuditMasksBeforeAtomicStoreAndRefusesPendingSeal(t *testing.T) {
	id := uuid.New()
	reg := secretmask.NewRegistry()
	secret := "runner-secret-value-12345"
	reg.Add(id, []byte(secret))
	ev := decideRow(secret)
	ev.RunID = &id
	ev.Target = secret
	got, err := prepareRunnerAudit(t.Context(), ev, maskingRecorder{reg: reg}, nil)
	if err != nil || strings.Contains(string(got.Data), secret) || strings.Contains(got.Target, secret) {
		t.Fatalf("unmasked replay: %+v %v", got, err)
	}
	src := newAuditSealSource(audit.SealFields)
	if _, err := prepareRunnerAudit(t.Context(), ev, maskingRecorder{}, src); err == nil {
		t.Fatal("unarmed sealing accepted")
	}
	src.arm(&audit.Sealer{Keys: downKeys{}, Pending: func() []byte { return make([]byte, 32) }})
	if _, err := prepareRunnerAudit(t.Context(), ev, maskingRecorder{}, src); err == nil {
		t.Fatal("pending subject-key row acknowledged")
	}
	ordinary := types.AuditEvent{Action: "egress.deny", ActorType: types.ActorAgent, Actor: "runner:" + uuid.NewString()}
	if _, err := prepareRunnerAudit(t.Context(), ordinary, maskingRecorder{}, src); err != nil {
		t.Fatal(err)
	}
}

// A run-less event, as a service grant mint row is, is masked against the global corpus in both Target and Data,
// and its target is capped first: the preparation the in-memory and Postgres test fixtures stand in for.
func TestPrepareRunnerAudit_MasksRunlessTarget(t *testing.T) {
	reg := secretmask.NewRegistry()
	marker := "global-marker-0123456789"
	if err := reg.AddGlobal(0, "", "a-global", time.Now(), []byte(marker)); err != nil {
		t.Fatal(err)
	}
	ev := types.AuditEvent{Action: "model_provider.service_grant.mint", ActorType: types.ActorSystem, Actor: "wardyn/service-grant",
		Target: "provider-" + marker, Data: []byte(`{"provider":"provider-` + marker + `"}`)}
	got, err := prepareRunnerAudit(t.Context(), ev, maskingRecorder{reg: reg}, nil)
	if err != nil || strings.Contains(got.Target, marker) || strings.Contains(string(got.Data), marker) {
		t.Fatalf("run-less preparation = %+v, %v; want Target and Data masked", got, err)
	}
	long := types.AuditEvent{Action: ev.Action, ActorType: types.ActorSystem, Actor: ev.Actor,
		Target: strings.Repeat("t", 2*store.MaxAuditTargetLen)}
	got, err = prepareRunnerAudit(t.Context(), long, maskingRecorder{reg: reg}, nil)
	if err != nil || got.Target != store.CapAuditTarget(long.Target) || len(got.Target) >= len(long.Target) {
		t.Fatalf("a long target came back %d bytes, %v; want it capped", len(got.Target), err)
	}
}
