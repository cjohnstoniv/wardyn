// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/identity"
)

// TestRunIdentity_GatewayMintRowKeepsWorkload: through the run identity recorder, the gateway's first-mint row
// keeps the key "workload" for the workload the token was signed for (none on an ordinary run), and carries the
// run's owner and the token's SPIFFE ID under run_owner and run_workload.
func TestRunIdentity_GatewayMintRowKeepsWorkload(t *testing.T) {
	f := newGatewayFixture(t)
	f.srv.cfg.Audit = audit.RunIdentityRecorder{Inner: f.srv.cfg.Audit}
	if w := f.mint(t); w.Code != http.StatusOK {
		t.Fatalf("mint = %d %s", w.Code, w.Body)
	}
	evs := f.mintEvents()
	if len(evs) != 1 {
		t.Fatalf("rows = %d, want one first-mint row", len(evs))
	}
	var data map[string]any
	if err := json.Unmarshal(evs[0].Data, &data); err != nil {
		t.Fatal(err)
	}
	if data["run_owner"] != f.run.CreatedBy {
		t.Errorf("run_owner = %v, want %q (data %s)", data["run_owner"], f.run.CreatedBy, evs[0].Data)
	}
	spiffe, _ := data["run_workload"].(string)
	if !strings.Contains(spiffe, f.run.ID.String()) {
		t.Errorf("run_workload = %q, want the token's SPIFFE ID naming run %s", spiffe, f.run.ID)
	}
	if w, ok := data["workload"]; ok && w == spiffe {
		t.Errorf("workload = %v: the stamp overwrote the signed workload's key", w)
	}
}

// TestRunIdentity_ContextWithClaimsSetsIdentity: the verified claims, and nothing else, make the identity a row
// recorded under the request's context carries; a sponsor rides only on a service owner.
func TestRunIdentity_ContextWithClaimsSetsIdentity(t *testing.T) {
	for name, tc := range map[string]struct {
		c    identity.Claims
		want audit.RunIdentity
	}{
		"person":  {identity.Claims{Sub: "alice@example.com", Sponsor: "alice@example.com", SPIFFEID: "spiffe://t/agent-run/1"}, audit.RunIdentity{Owner: "alice@example.com", Workload: "spiffe://t/agent-run/1"}},
		"service": {identity.Claims{Sub: "service:ci", Sponsor: "boss@example.com", SPIFFEID: "spiffe://t/agent-run/2"}, audit.RunIdentity{Owner: "service:ci", Sponsor: "boss@example.com", Workload: "spiffe://t/agent-run/2"}},
	} {
		got, ok := audit.RunIdentityFrom(contextWithClaims(t.Context(), &tc.c))
		if !ok || got != tc.want {
			t.Errorf("%s: identity = %+v ok=%v, want %+v", name, got, ok, tc.want)
		}
	}
}
