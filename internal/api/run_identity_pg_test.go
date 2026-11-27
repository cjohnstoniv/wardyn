// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/egress"
)

// identityDoor is pgDoor with the run identity recorder at the head of the chain, as cmd/wardynd builds it, and
// a run token minted for owner (and sponsor, which only a service owner keeps).
func identityDoor(t *testing.T, chain *pgBatchChain, owner, sponsor string) (*Server, string, uuid.UUID) {
	t.Helper()
	h := newHarness(t)
	runID := uuid.New()
	srv, _ := newBatchServer(h, &batchDoorStore{})
	srv.cfg.Audit = audit.RunIdentityRecorder{Inner: chain}
	id, err := h.idp.MintRunIdentity(context.Background(), runID, owner, sponsor, internalAudience, false)
	if err != nil {
		t.Fatal(err)
	}
	return srv, id.Token, runID
}

func storedDecisionData(t *testing.T, chain *pgBatchChain, id uuid.UUID) (map[string]any, string) {
	t.Helper()
	var raw []byte
	if err := chain.pool.QueryRow(t.Context(), `SELECT data FROM audit_events WHERE id = $1`, id).Scan(&raw); err != nil {
		t.Fatalf("read the stored row: %v", err)
	}
	var data map[string]any
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatal(err)
	}
	return data, string(raw)
}

// TestAudit_AgentRowCarriesOwnerSponsorWorkload: a decision row written under a verified run token is stored
// with the token's owner, its sponsor when the owner is a service identity, and its SPIFFE ID as the workload.
func TestAudit_AgentRowCarriesOwnerSponsorWorkload(t *testing.T) {
	for name, tc := range map[string]struct{ owner, sponsor, wantSponsor string }{
		"person":  {"alice@example.com", "alice@example.com", ""},
		"service": {"service:ci", "boss@example.com", "boss@example.com"},
	} {
		t.Run(name, func(t *testing.T) {
			chain := &pgBatchChain{pool: throwawayBarePGPool(t)}
			srv, tok, runID := identityDoor(t, chain, tc.owner, tc.sponsor)
			body, ids := batchBody(t, time.Now().UTC(), egress.DecisionLog{Request: egress.Request{Host: "a.example.com", Method: "GET"}, Decision: egress.Allow, RuleSource: "policy:allowlist"})
			if code := postDecisionBatch(t, srv, tok, body, false); code != http.StatusAccepted {
				t.Fatalf("post = %d", code)
			}
			data, raw := storedDecisionData(t, chain, ids[0])
			if data["run_owner"] != tc.owner {
				t.Errorf("run_owner = %v, want %q (data %s)", data["run_owner"], tc.owner, raw)
			}
			if w, _ := data["run_workload"].(string); !strings.Contains(w, runID.String()) {
				t.Errorf("run_workload = %q, want the token's SPIFFE ID naming run %s", w, runID)
			}
			if got, _ := data["sponsor"].(string); got != tc.wantSponsor {
				t.Errorf("sponsor = %q, want %q (data %s)", got, tc.wantSponsor, raw)
			}
		})
	}
}

// TestAudit_ProxyPostedIdentityKeysIgnored_PG: identity keys a proxy puts in a record, at the top or in a
// "data" object, never reach the stored row; the verified token's values do.
func TestAudit_ProxyPostedIdentityKeysIgnored_PG(t *testing.T) {
	chain := &pgBatchChain{pool: throwawayBarePGPool(t)}
	srv, tok, _ := identityDoor(t, chain, "alice@example.com", "alice@example.com")
	body, ids := batchBody(t, time.Now().UTC(), egress.DecisionLog{Request: egress.Request{Host: "a.example.com", Method: "GET"}, Decision: egress.Allow, RuleSource: "policy:allowlist"})
	var b map[string]any
	if err := json.Unmarshal([]byte(body), &b); err != nil {
		t.Fatal(err)
	}
	forged := map[string]any{"run_owner": "mallory", "run_workload": "spiffe://evil/x", "sponsor": "mallory"}
	rec := b["records"].([]any)[0].(map[string]any)
	for k, v := range forged {
		rec[k] = v
	}
	rec["data"] = forged
	rec["decision"].(map[string]any)["data"] = forged
	forgedBody, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	if code := postDecisionBatch(t, srv, tok, string(forgedBody), false); code != http.StatusAccepted {
		t.Fatalf("post = %d", code)
	}
	data, raw := storedDecisionData(t, chain, ids[0])
	if strings.Contains(raw, "mallory") || strings.Contains(raw, "evil") {
		t.Errorf("a posted identity reached the stored row: %s", raw)
	}
	if data["run_owner"] != "alice@example.com" {
		t.Errorf("run_owner = %v, want the token's owner (data %s)", data["run_owner"], raw)
	}
	if _, ok := data["sponsor"]; ok {
		t.Errorf("a person's row carries a sponsor: %s", raw)
	}
}
