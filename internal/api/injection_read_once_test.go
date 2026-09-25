// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/broker"
	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestInternalInjection_RecordsTheStoredReadOnce: behind the Audited store
// wardynd runs, the injection sink's one read of a stored key is ONE
// secret.read — the sink's own, with grant_id and jti, naming the row it read —
// not a second one from the decorator.
func TestInternalInjection_RecordsTheStoredReadOnce(t *testing.T) {
	h, sec := newSecretsHarness(t)
	h.srv.cfg.Secrets = secretstore.Audited(sec, h.audit)
	h.srv.router = h.srv.routes()
	runID := uuid.New()
	token := h.mintRunToken(t, runID)
	h.broker.minted = broker.Minted{
		Kind: types.GrantAPIKey, JTI: "jti-once",
		Injection: &egress.InjectionRule{
			Host: "api.anthropic.com", Header: "x-api-key",
			SecretName: "anthropic-api-key", Format: "%s",
		},
	}

	rr := do(t, h.srv, http.MethodGet, "/api/v1/internal/injection/"+uuid.NewString(), token, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	var reads []types.AuditEvent
	for _, ev := range h.audit.snapshot() {
		if ev.Action == "secret.read" && ev.Target == "anthropic-api-key" {
			reads = append(reads, ev)
		}
	}
	if len(reads) != 1 {
		t.Fatalf("one resolve recorded %d secret.read events, want exactly 1: %+v", len(reads), reads)
	}
	var d map[string]any
	if err := json.Unmarshal(reads[0].Data, &d); err != nil {
		t.Fatal(err)
	}
	if d["jti"] != "jti-once" || d["purpose"] != "proxy-injection" || d["store"] != "mem" || d["ref"] != "mem:/anthropic-api-key" {
		t.Errorf("the read is recorded as %v; want the sink's event naming the row it read", d)
	}
}
