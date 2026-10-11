// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/google/uuid"
	"strings"
	"testing"
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
