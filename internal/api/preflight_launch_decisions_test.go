// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestPreflightDecisionsEqualLaunch is the BEHAVIOURAL half of preflight
// parity (authorization-kernel design G3); TestPreflightMirrorsLaunchGates is
// the structural half. That one proves Review calls the gates launch calls, in
// launch's order. This one proves the gates, called that way, DECIDE the same:
// both doors are asked the same request over one store (one snapshot of grants,
// switches and restrictions), and the decisions each makes — every refusal,
// and every entry the member's inline policy loses — must match. A Review that
// hands the gates a different request, a different principal or a policy
// resolved some other way previews a launch that will not happen, and only a
// diff of the decisions themselves can see that.
func TestPreflightDecisionsEqualLaunch(t *testing.T) {
	cs := &capStore{}
	srv, st, rec := govEscapeFixture(t, cs)
	st.workspaces = []types.Workspace{{ID: kernelWorkspaceID, Name: "kernel"}}
	st.policies[kernelPolicyID] = types.RunPolicy{ID: kernelPolicyID, Name: "kernel", Spec: govDeployment()}
	// The deployment lets the api_key grant through its own stage, so the
	// member's secret capability is what decides it.
	grant := map[string]any{"kind": "api_key", "scope": map[string]string{
		"host": "api.anthropic.com", "header": "Authorization", "secret_name": govCorpSecret}}
	var eligible types.GrantSpec
	raw, _ := json.Marshal(grant)
	if err := json.Unmarshal(raw, &eligible); err != nil {
		t.Fatal(err)
	}
	srv.cfg.DefaultPolicy.EligibleGrants = []types.GrantSpec{eligible}

	// Each request, and the fields whose kinds its grant state applies to.
	type request struct {
		body   map[string]any
		fields []kernelLaunchField
	}
	requests := map[string]request{}
	every := request{body: map[string]any{"agent": "claude-code", "task": "decisions"}}
	for _, f := range kernelLaunchFields {
		one := map[string]any{"agent": "claude-code", "task": "decisions", f.field: f.value}
		requests[f.kind] = request{one, []kernelLaunchField{f}}
		every.body[f.field] = f.value
		every.fields = append(every.fields, f)
	}
	requests["every field"] = every
	requests["inline policy"] = request{
		body: map[string]any{"agent": "claude-code", "task": "decisions", "inline_policy": map[string]any{
			"min_confinement_class": "CC2",
			"allowed_domains":       []string{"api.anthropic.com", "wide.example"},
			"eligible_grants":       []any{grant},
		}},
		fields: []kernelLaunchField{{kind: capEgressHost, value: "wide.example"}, {kind: capSecret, value: govCorpSecret}},
	}

	user := kernelTiers[2]
	ask := func(path string, groups []string, body []byte) doorDecisions {
		before := len(rec.snapshot())
		w := doSSO(t, srv, http.MethodPost, path, user.session(t, groups), string(body))
		return decisionsOf(t, rec.snapshot()[before:], w)
	}
	refusals, drops := 0, 0
	for _, name := range sortedKeys(requests) {
		req := requests[name]
		body, _ := json.Marshal(req.body)
		for _, c := range resolverCases() {
			if c.tier != user.oracle || c.noStore {
				continue
			}
			// One snapshot: every kind the request names is in the same grant state.
			cs.grants, cs.enf, cs.restricted = nil, map[string]bool{}, map[string]map[string]bool{}
			for _, f := range req.fields {
				cs.grants = append(cs.grants, kernelGrants(c, f)...)
				cs.enf[f.kind] = c.enforced
				if c.restricted {
					cs.restricted[f.kind] = map[string]bool{f.value: true}
				}
			}
			var groups []string
			if !c.stale {
				groups = []string{"eng"}
			}
			pre := ask("/api/v1/runs/preflight", groups, body)
			launch := ask("/api/v1/runs", groups, body)
			at := fmt.Sprintf("%s %v", name, c)

			if !slices.Equal(pre.refused, launch.refused) {
				t.Errorf("%s: refused differently\npreflight: %v\nlaunch:    %v", at, pre.refused, launch.refused)
			}
			if launch.code == http.StatusForbidden || pre.code == http.StatusForbidden {
				if pre.code != launch.code || pre.body != launch.body {
					t.Errorf("%s: preflight %d %s, launch %d %s", at, pre.code, pre.body, launch.code, launch.body)
				}
			}
			// Review warns what launch will do to the policy, in launch's words
			// and order; launch then adds warnings of its own after them.
			if pre.code < 300 && launch.code < 300 &&
				(len(pre.warned) > len(launch.warned) || !slices.Equal(pre.warned, launch.warned[:len(pre.warned)])) {
				t.Errorf("%s: preflight warned %q, launch %q", at, pre.warned, launch.warned)
			}
			// And every entry launch took away, Review said it would.
			for _, d := range launch.dropped {
				if !slices.ContainsFunc(pre.warned, func(w string) bool { return strings.Contains(w, d) }) {
					t.Errorf("%s: launch dropped %q, which preflight never warned of (%q)", at, d, pre.warned)
				}
			}
			refusals += len(launch.refused)
			drops += len(launch.dropped)
		}
	}
	// Non-vacuous: the states above refuse and drop at both doors.
	if refusals == 0 || drops == 0 {
		t.Fatalf("refusals = %d, drops = %d: a comparison with nothing to compare proves nothing", refusals, drops)
	}
}

// doorDecisions is what one door decided about one request.
type doorDecisions struct {
	code int
	body string
	// refused is each refusal's authz.denied row: target, then its data. A dry
	// run has no run to name, so the run id is left out.
	refused []string
	// dropped is what launch's drop rows name. Review does not audit a drop
	// (boundUserSpec's dryRun); it warns of it instead.
	dropped []string
	warned  []string
}

// decisionsOf reads one door's answer w and the audit rows events it wrote.
func decisionsOf(t *testing.T, events []types.AuditEvent, w *httptest.ResponseRecorder) doorDecisions {
	t.Helper()
	d := doorDecisions{code: w.Code}
	if w.Code >= 400 {
		d.body = refusalBody(t, w)
	}
	for _, ev := range events {
		if ev.Action != "authz.denied" {
			continue
		}
		var data struct {
			Dropped []string `json:"dropped"`
		}
		if err := json.Unmarshal(ev.Data, &data); err != nil {
			t.Fatalf("authz.denied data: %v", err)
		}
		if data.Dropped != nil {
			d.dropped = append(d.dropped, data.Dropped...)
		} else {
			d.refused = append(d.refused, ev.Target+" "+string(ev.Data))
		}
	}
	if w.Code < 300 {
		var resp struct {
			Warnings []string `json:"warnings"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("%d body: %v", w.Code, err)
		}
		d.warned = resp.Warnings
	}
	return d
}
