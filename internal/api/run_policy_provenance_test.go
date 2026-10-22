// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/policyref"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// provenanceRun creates one run as the given caller and returns its id, its
// run.policy.resolve datum (raw) and GET /runs/{id}/policy as that caller.
func provenanceRun(t *testing.T, srv *Server, st *pvStore, rd pvReader, body string) (resolve string, view *runPolicyResponse, raw string) {
	t.Helper()
	var w *httptest.ResponseRecorder
	if rd.cookie == nil {
		w = do(t, srv, http.MethodPost, "/api/v1/runs", adminToken, body)
	} else {
		w = doSSO(t, srv, http.MethodPost, "/api/v1/runs", rd.cookie, body)
	}
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d %s, want 201", w.Code, w.Body.String())
	}
	var created struct {
		ID uuid.UUID `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	ev := waitForRecAudit(t, st.audit, created.ID, "run.policy.resolve", "success")
	rec, resp := rd.get(t, srv, created.ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET policy = %d %s", rec.Code, rec.Body.String())
	}
	return string(ev.Data), &resp, rec.Body.String()
}

// provenanceJSON is the wire form of one knob's record in a raw body.
func provenanceJSON(t *testing.T, raw, knob string) string {
	t.Helper()
	var body struct {
		Provenance map[string]json.RawMessage `json:"provenance"`
	}
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatal(err)
	}
	return string(body.Provenance[knob])
}

// TestRunPolicyProvenanceNamesTheLeafThatNarrowed: a member's inline policy
// under a composed profile. The knob the clamp narrowed is the leaf's, locked,
// with the leaf's contact on the read and none in the audit; a knob the member
// set and the clamp kept is the member's; nothing names the base (#2951).
func TestRunPolicyProvenanceNamesTheLeafThatNarrowed(t *testing.T) {
	base, leaf := disclosureGraph()
	leaf.Contact = &policyref.Contact{Owner: "Team owners", RequestURL: "https://team.example/ask"}
	cs := assignedStore(&leaf)
	cs.govGraph = []types.GovernanceUnit{base, leaf}
	srv, st := pvFixture(t, cs)
	st.profiles = cs.govGraph
	const owner = "sub-walled"
	member := pvReader{govSession(t, owner, []string{"eng"}, false)}

	resolve, view, raw := provenanceRun(t, srv, st, member, `{"agent":"claude-code","task":"t","inline_policy":{`+
		`"allowed_domains":["api.anthropic.com","off-wall.example"],"min_confinement_class":"CC3","first_use_approval":"always_deny"}}`)

	// The golden wire records.
	const leafRec = `{"layer":"unit","name":"team","contact":{"owner":"Team owners","request_url":"https://team.example/ask"}}`
	const memberRec = `{"layer":"member"}`
	if got := provenanceJSON(t, raw, "allowed_domains"); got != leafRec {
		t.Errorf("allowed_domains = %s, want %s", got, leafRec)
	}
	if got := provenanceJSON(t, raw, "denied_domains"); got != leafRec {
		t.Errorf("denied_domains = %s, want %s (the ceiling's deny list was unioned in)", got, leafRec)
	}
	if got := provenanceJSON(t, raw, "min_confinement_class"); got != memberRec {
		t.Errorf("min_confinement_class = %s, want %s", got, memberRec)
	}
	if len(view.Provenance) != len(policyKnobFields) {
		t.Errorf("provenance has %d knobs, want every one of the %d policy fields", len(view.Provenance), len(policyKnobFields))
	}
	assertNoBase(t, "GET /runs/{id}/policy", raw)
	assertNoLock(t, raw)

	// The audit row: the same layers, the leaf's name, never a contact or the base.
	var datum struct {
		Provenance map[string]policyref.Provenance `json:"provenance"`
	}
	if err := json.Unmarshal([]byte(resolve), &datum); err != nil {
		t.Fatal(err)
	}
	if want := (policyref.Provenance{Layer: "unit", Name: "team"}); datum.Provenance["allowed_domains"] != want {
		t.Errorf("run.policy.resolve allowed_domains = %+v, want %+v", datum.Provenance["allowed_domains"], want)
	}
	for _, secret := range []string{"Team owners", "team.example", `"contact"`} {
		if strings.Contains(resolve, secret) {
			t.Errorf("run.policy.resolve carries %q: %s", secret, resolve)
		}
	}
	assertNoBase(t, "run.policy.resolve", resolve)
}

// TestRunPolicyProvenanceDeploymentAndOperator: a member no profile binds who
// sends no policy runs the deployment ceiling itself, so every knob is the
// deployment's, locked, with its policy_help; an operator's run is bound by no
// ceiling, so every knob is the operator's and open.
func TestRunPolicyProvenanceDeploymentAndOperator(t *testing.T) {
	t.Run("deployment", func(t *testing.T) {
		srv, st := pvFixture(t, &capStore{})
		st.siteConfig = types.SiteConfig{PolicyHelp: &policyref.Contact{RequestText: "ask platform"}}
		_, view, raw := provenanceRun(t, srv, st, pvMember(t, "sub-free"), `{"agent":"claude-code","task":"t"}`)
		const want = `{"layer":"deployment","name":"deployment","contact":{"request_text":"ask platform"}}`
		for _, knob := range policyKnobFields {
			if got := provenanceJSON(t, raw, knob); got != want {
				t.Errorf("%s = %s, want %s", knob, got, want)
			}
		}
		if len(view.Provenance) == 0 {
			t.Fatal("no provenance served")
		}
	})
	t.Run("operator", func(t *testing.T) {
		srv, st := pvFixture(t, &capStore{})
		_, _, raw := provenanceRun(t, srv, st, pvReader{}, `{"agent":"claude-code","task":"t"}`)
		const want = `{"layer":"operator"}`
		for _, knob := range policyKnobFields {
			if got := provenanceJSON(t, raw, knob); got != want {
				t.Errorf("%s = %s, want %s", knob, got, want)
			}
		}
	})
}

// assertNoLock: no layer can lock a knob yet (a lock is lower_may_change
// false), so no record carries locked, ceiling-set or not.
func assertNoLock(t *testing.T, raw string) {
	t.Helper()
	if strings.Contains(raw, `"locked"`) {
		t.Errorf("a provenance record is locked, but no layer can lock a knob yet: %s", raw)
	}
}

// TestRunPolicyProvenanceSplitsAnOverlay: a member's policy_overlay laid on
// their profile's default. A key the overlay set and the bound kept is the
// member's; a key the overlay set and the bound narrowed back is the profile's;
// a key the overlay left alone stays the profile's.
func TestRunPolicyProvenanceSplitsAnOverlay(t *testing.T) {
	p := govProfile("team")
	p.Limits = types.GovernanceLimits{}
	cs := assignedStore(p)
	srv, st := pvFixture(t, cs)
	_, _, raw := provenanceRun(t, srv, st, pvReader{govSession(t, "sub-walled", []string{"eng"}, false)},
		`{"agent":"claude-code","task":"t","policy_overlay":{"denied_domains":["corp.internal","extra.example"],"min_confinement_class":"CC1"}}`)
	const profileRec, memberRec = `{"layer":"unit","name":"team"}`, `{"layer":"member"}`
	for knob, want := range map[string]string{
		"denied_domains":        memberRec,  // the overlay's, kept: the ceiling's deny was already in it
		"min_confinement_class": profileRec, // the overlay's CC1, raised back to the profile's CC2
		"allowed_domains":       profileRec, // not in the overlay: the default's
	} {
		if got := provenanceJSON(t, raw, knob); got != want {
			t.Errorf("%s = %s, want %s", knob, got, want)
		}
	}
	assertNoLock(t, raw)
}

// TestProvenanceIgnoresACanonicalizingStep: a step that only re-cases,
// reorders or dedupes a list (composer's union and unionPaths do) changed no
// value, so the knob keeps the layer that set it; a real change still moves it.
func TestProvenanceIgnoresACanonicalizingStep(t *testing.T) {
	before := types.RunPolicySpec{
		AllowedDomains: []string{"B.example", "a.example"}, DeniedDomains: []string{"x.example", "x.example "},
		PushRules: &types.PushRulesSpec{DenyPaths: []string{"b/", "a/"}},
	}
	for _, tc := range []struct {
		name  string
		after func(*types.RunPolicySpec)
		moved string
	}{
		{"re-cased, reordered and deduped", func(s *types.RunPolicySpec) {
			s.AllowedDomains, s.DeniedDomains, s.PushRules.DenyPaths = []string{"a.example", "b.example"}, []string{"x.example"}, []string{"a/", "b/"}
		}, ""},
		{"a host dropped", func(s *types.RunPolicySpec) { s.AllowedDomains = []string{"a.example"} }, "allowed_domains"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			after := before.Clone()
			tc.after(&after)
			k := startProvenance(memberProvenance)
			k.setBy(deploymentProvenance, before, after)
			for knob, rec := range k {
				if want := map[bool]policyref.Provenance{true: deploymentProvenance, false: memberProvenance}[knob == tc.moved]; rec != want {
					t.Errorf("%s = %+v, want %+v", knob, rec, want)
				}
			}
		})
	}
}
