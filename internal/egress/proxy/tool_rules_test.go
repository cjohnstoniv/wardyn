// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// tool_rules narrows an autonomous run's tool use from a binary into a policy.
// These pin the matching semantics, which are deliberately dull: exact name,
// then "*", then no match.
func TestToolEffectFor(t *testing.T) {
	spec := types.RunPolicySpec{ToolRules: []types.ToolRule{
		{Tool: "Read", Effect: types.ToolAllow},
		{Tool: "Bash", Effect: types.ToolHold},
		{Tool: "WebFetch", Effect: types.ToolDeny},
	}}
	p := CompilePolicy(spec)

	for _, tc := range []struct {
		tool string
		want types.ToolEffect
		ok   bool
	}{
		{"Read", types.ToolAllow, true},
		{"Bash", types.ToolHold, true},
		{"WebFetch", types.ToolDeny, true},
		// No rule and no "*" default: the caller must fall back to asking a
		// human. This is the case that keeps the field additive.
		{"Edit", "", false},
	} {
		got, ok := p.ToolEffectFor(tc.tool)
		if ok != tc.ok || got != tc.want {
			t.Errorf("ToolEffectFor(%q) = (%q, %v), want (%q, %v)", tc.tool, got, ok, tc.want, tc.ok)
		}
	}

	// Case-sensitive on purpose: "bash" is not "Bash". A case-insensitive match
	// would make a rule fire on a tool the operator did not name.
	if _, ok := p.ToolEffectFor("bash"); ok {
		t.Error(`ToolEffectFor("bash") matched the rule for "Bash" — matching must be case-sensitive, or a rule fires on a tool nobody named`)
	}
}

func TestToolEffectFor_WildcardIsTheDefault(t *testing.T) {
	p := CompilePolicy(types.RunPolicySpec{ToolRules: []types.ToolRule{
		{Tool: "Read", Effect: types.ToolAllow},
		{Tool: "*", Effect: types.ToolDeny},
	}})

	// An exact rule beats the default...
	if got, ok := p.ToolEffectFor("Read"); !ok || got != types.ToolAllow {
		t.Errorf(`ToolEffectFor("Read") = (%q, %v), want (allow, true) — an exact rule must beat "*"`, got, ok)
	}
	// ...and everything else takes it.
	if got, ok := p.ToolEffectFor("Bash"); !ok || got != types.ToolDeny {
		t.Errorf(`ToolEffectFor("Bash") = (%q, %v), want (deny, true) via "*"`, got, ok)
	}
}

// The property that makes this field safe to add: a run with NO rules behaves
// exactly as it did before the field existed. If this regresses, every policy
// written before 0.7 changes meaning silently.
func TestToolEffectFor_NoRulesChangesNothing(t *testing.T) {
	for _, name := range []string{"no rules", "nil policy"} {
		t.Run(name, func(t *testing.T) {
			var p *Policy
			if name == "no rules" {
				p = CompilePolicy(types.RunPolicySpec{})
			}
			if _, ok := p.ToolEffectFor("Bash"); ok {
				t.Error("a policy with no tool_rules reported a rule — every call must still go to a human")
			}
		})
	}
}

// Clone is deep for every slice field, and a miss aliases the backing array
// across concurrent runs — the exact bug its own doc warns about. Nothing else
// would catch a new field being left out.
func TestClone_DeepCopiesToolRules(t *testing.T) {
	orig := types.RunPolicySpec{ToolRules: []types.ToolRule{{Tool: "Bash", Effect: types.ToolHold}}}
	cp := orig.Clone()
	cp.ToolRules[0].Effect = types.ToolAllow

	if orig.ToolRules[0].Effect != types.ToolHold {
		t.Fatal("mutating the CLONE's tool_rules changed the ORIGINAL — Clone is aliasing the slice, so one run's policy edit would leak into another's")
	}
}

// toolgateContractDir holds the proxy's recorded answers to the tool-approval
// route. cmd/wardyn-toolgate's contract test feeds these same bytes to the gate,
// so a drift on either side of the seam fails a test instead of shipping.
const toolgateContractDir = "testdata/toolgate-contract"

// TestToolRulesAtTheLocalRoute drives the run's tool_rules through the real
// tool-approval route: `allow` and `deny` are answered by the proxy itself —
// no approval row, no control-plane call — and logged under their own rule
// source; `hold` (here via "*") still raises a human approval.
func TestToolRulesAtTheLocalRoute(t *testing.T) {
	cases := []struct {
		tool       string
		wantStatus int
		golden     string
		wantCP     bool
		wantSource string
		wantDec    egress.Decision
	}{
		{"Read", http.StatusOK, "approved.json", false, ruleSourceToolAllow, egress.Allow},
		{"WebFetch", http.StatusOK, "denied.json", false, ruleSourceToolDeny, egress.Deny},
		{"Bash", http.StatusCreated, "held.json", true, ruleSourceApprovals, egress.Allow},
	}
	for _, tc := range cases {
		t.Run(tc.tool, func(t *testing.T) {
			var cpCalls int
			cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				cpCalls++
				_, _ = io.Copy(io.Discard, r.Body)
				// The control plane's answer to a raise: the created row, as
				// handleInternalRequestApproval writes it.
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusCreated)
				_ = json.NewEncoder(w).Encode(types.ApprovalRequest{
					ID:             uuid.MustParse("11111111-2222-3333-4444-555555555555"),
					RunID:          uuid.MustParse("66666666-7777-8888-9999-000000000000"),
					Kind:           types.ApprovalToolCall,
					RequestedScope: json.RawMessage(`{"tool":"Bash","cmd":"make test"}`),
					State:          types.ApprovalPending,
					RequestedAt:    time.Now(),
				})
			}))
			defer cp.Close()

			p, buf := newLocalRouteProxy(t, "http://wardynd.test:8080", "RUNTOK", upstreamAddr(cp), nil, nil)
			p.policy = CompilePolicy(types.RunPolicySpec{ToolRules: []types.ToolRule{
				{Tool: "Read", Effect: types.ToolAllow},
				{Tool: "WebFetch", Effect: types.ToolDeny},
				{Tool: "*", Effect: types.ToolHold},
			}})

			body := `{"kind":"tool_call","payload":{"tool":"` + tc.tool + `","cmd":"make test"}}`
			rec := httptest.NewRecorder()
			p.ServeHTTP(rec, mustLocalReq(t, http.MethodPost, routeApprovalsCreate, strings.NewReader(body)))

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if (cpCalls > 0) != tc.wantCP {
				t.Fatalf("control-plane calls = %d, want contacted=%v — a policy decision must not create an approval row, and a hold must", cpCalls, tc.wantCP)
			}
			d := lastDecision(t, buf)
			if d.RuleSource != tc.wantSource || d.Decision != tc.wantDec {
				t.Fatalf("decision = %s/%s, want %s/%s", d.RuleSource, d.Decision, tc.wantSource, tc.wantDec)
			}
			want, err := os.ReadFile(filepath.Join(toolgateContractDir, tc.golden))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(rec.Body.Bytes(), want) {
				t.Fatalf("response drifted from %s (wardyn-toolgate's contract test reads that file):\n got %s\nwant %s",
					tc.golden, rec.Body.Bytes(), want)
			}
		})
	}
}
