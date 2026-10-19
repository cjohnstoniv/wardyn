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
	cs.govGraph = []types.GovernanceProfile{base, leaf}
	srv, st := pvFixture(t, cs)
	st.profiles = cs.govGraph
	const owner = "sub-walled"
	member := pvReader{govSession(t, owner, []string{"eng"}, false)}

	resolve, view, raw := provenanceRun(t, srv, st, member, `{"agent":"claude-code","task":"t","inline_policy":{`+
		`"allowed_domains":["api.anthropic.com","off-wall.example"],"min_confinement_class":"CC3","first_use_approval":"always_deny"}}`)

	// The golden wire records.
	const leafRec = `{"layer":"profile","name":"team","contact":{"owner":"Team owners","request_url":"https://team.example/ask"},"locked":true}`
	const memberRec = `{"layer":"member","locked":false}`
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

	// The audit row: the same layers, the leaf's name, never a contact or the base.
	var datum struct {
		Provenance map[string]policyref.Provenance `json:"provenance"`
	}
	if err := json.Unmarshal([]byte(resolve), &datum); err != nil {
		t.Fatal(err)
	}
	if want := (policyref.Provenance{Layer: "profile", Name: "team", Locked: true}); datum.Provenance["allowed_domains"] != want {
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
		const want = `{"layer":"deployment","name":"deployment","contact":{"request_text":"ask platform"},"locked":true}`
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
		const want = `{"layer":"operator","locked":false}`
		for _, knob := range policyKnobFields {
			if got := provenanceJSON(t, raw, knob); got != want {
				t.Errorf("%s = %s, want %s", knob, got, want)
			}
		}
	})
}
