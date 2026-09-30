// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const (
	pvHostPath   = "/srv/team-data"
	pvSecretName = "operator-key"
)

// pvWide is a policy that carries everything the source record must not keep
// verbatim (a host path, a secret name) plus a host the member's ceiling drops.
func pvWide() types.RunPolicySpec {
	return types.RunPolicySpec{
		MinConfinementClass: types.CC1,
		AllowedDomains:      []string{"api.anthropic.com", "evil.example"},
		WorkspaceMounts:     []types.WorkspaceMount{{Source: pvHostPath, Target: "/home/agent/data"}},
		EligibleGrants: []types.GrantSpec{{
			Kind:  types.GrantAPIKey,
			Scope: mustJSON(map[string]any{"host": "evil.example", "secret_name": pvSecretName}),
		}},
	}
}

// resolveSource runs resolveRunPolicy for one arm and returns the recorded
// source, as the run.create row would carry it (marshalled).
func resolveSource(t *testing.T, role, arm string) (policySourceRecord, types.RunPolicySpec, string) {
	t.Helper()
	srv, _, policyID := memberBoundFixture(t, pvWide())
	ctx := operatorCtx("sub-member", "member@corp.example", role)
	r := httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil).WithContext(ctx)
	req := &createRunRequest{Agent: "claude-code", Repo: "acme/widgets"}
	switch arm {
	case "inline":
		wide := pvWide()
		req.InlinePolicy = &wide
	case "stored":
		req.PolicyID = &policyID
	}
	srv.cfg.Secrets = &memSecrets{m: map[string][]byte{pvSecretName: []byte("v")}}
	w := httptest.NewRecorder()
	spec, _, _, source, ok := srv.resolveRunPolicy(ctx, w, r, req, false)
	if !ok {
		t.Fatalf("resolveRunPolicy refused (%s, %s): %s", role, arm, w.Body.String())
	}
	b, err := json.Marshal(source)
	if err != nil {
		t.Fatal(err)
	}
	return source, spec, string(b)
}

// TestPolicySource_IsRecordedRedacted is the security pin: the run.create
// row is readable raw by the run's creator through the audit API, and the
// starting policy is the pre-clamp, admin-authored source. Stamped raw, a member
// who selected a stored policy would read the host paths and secret names their
// clamp removed. Counterfactual: stamp `src` instead of
// redactSpecForUser(auditablePolicy(src)) and both arms fail here.
func TestPolicySource_IsRecordedRedacted(t *testing.T) {
	for _, role := range []string{oidc.RoleUser, oidc.RoleAdmin} {
		for _, arm := range []string{"inline", "stored"} {
			t.Run(role+"/"+arm, func(t *testing.T) {
				source, _, body := resolveSource(t, role, arm)
				for _, secret := range []string{pvHostPath, pvSecretName, "secret_name"} {
					if strings.Contains(body, secret) {
						t.Errorf("the recorded source carries %q: %s", secret, body)
					}
				}
				if len(source.Spec.WorkspaceMounts) != 1 || source.Spec.WorkspaceMounts[0].Source != "<redacted>" {
					t.Errorf("mount sources = %+v, want only <redacted>", source.Spec.WorkspaceMounts)
				}
				if len(source.Spec.EligibleGrants) != 1 || source.Spec.EligibleGrants[0].Kind != types.GrantAPIKey {
					t.Errorf("grants = %+v, want the api_key grant kept without its secret name", source.Spec.EligibleGrants)
				}
			})
		}
	}
}

func TestPolicySource_BaseIsPreClampAndBoundedIsRight(t *testing.T) {
	for _, arm := range []string{"inline", "stored"} {
		t.Run(arm, func(t *testing.T) {
			source, member, _ := resolveSource(t, oidc.RoleUser, arm)
			if !source.Bounded {
				t.Error("bounded = false for a member")
			}
			if !slices.Contains(source.Spec.AllowedDomains, "evil.example") {
				t.Errorf("the base %v lacks the host the clamp dropped: it is not pre-clamp", source.Spec.AllowedDomains)
			}
			if slices.Contains(member.AllowedDomains, "evil.example") {
				t.Errorf("the resolved spec %v kept evil.example: the fixture no longer clamps", member.AllowedDomains)
			}
			operator, op, _ := resolveSource(t, oidc.RoleAdmin, arm)
			if operator.Bounded || !slices.Contains(op.AllowedDomains, "evil.example") {
				t.Errorf("operator: bounded=%v allowed=%v, want unbounded and unclamped", operator.Bounded, op.AllowedDomains)
			}
		})
	}
}

func TestPolicySource_KindsAndStoredIdentity(t *testing.T) {
	srv, _, policyID := memberBoundFixture(t, pvWide())
	srv.cfg.Secrets = &memSecrets{m: map[string][]byte{pvSecretName: []byte("v")}}
	resolve := func(ctx context.Context, req *createRunRequest) policySourceRecord {
		r := httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil).WithContext(ctx)
		w := httptest.NewRecorder()
		_, _, _, src, ok := srv.resolveRunPolicy(ctx, w, r, req, false)
		if !ok {
			t.Fatalf("resolveRunPolicy refused: %s", w.Body.String())
		}
		return src
	}
	member := operatorCtx("sub-member", "member@corp.example", oidc.RoleUser)
	if s := resolve(member, &createRunRequest{Agent: "claude-code", PolicyID: &policyID}); s.Kind != policyKindStored ||
		s.PolicyID == nil || *s.PolicyID != policyID || s.Name != "wide" || s.UpdatedAt == nil {
		t.Errorf("stored source = %+v, want kind stored, the id, the name at launch and updated_at", s)
	}
	if s := resolve(member, &createRunRequest{Agent: "claude-code"}); s.Kind != policyKindProfile || s.Name != "walled" || s.PolicyID != nil {
		t.Errorf("no-policy member under a profile = %+v, want kind profile named walled", s)
	}
	admin := operatorCtx("sub-admin", "admin@corp.example", oidc.RoleAdmin)
	if s := resolve(admin, &createRunRequest{Agent: "claude-code"}); s.Kind != policyKindDefault || s.Bounded {
		t.Errorf("no-policy operator = %+v, want kind default, unbounded", s)
	}
}

// TestCreateRun_RecordsPolicySourceOnRunCreate drives the real route: the row
// carries the redacted base, and the run's own resolved envelope is what
// dispatch wrote — the two differ by exactly what the clamp took.
func TestCreateRun_RecordsPolicySourceOnRunCreate(t *testing.T) {
	profile := govProfile("walled")
	srv, st, audit := govEscapeFixture(t, &capStore{govProfile: profile, govTier: types.CapabilitySubjectGroup, govHasGroupTier: true})
	member := govSession(t, "sub-walled", []string{"eng"}, false)
	body := `{"agent":"claude-code","task":"t","inline_policy":{"min_confinement_class":"CC2","allowed_domains":["api.anthropic.com","wide.example"],` +
		`"eligible_grants":[{"kind":"api_key","scope":{"host":"api.anthropic.com","header":"Authorization","secret_name":"` + govCorpSecret + `"}}]}}`
	resolved := govCreateAndDispatch(t, srv, st, audit, member, body)
	st.mu.Lock()
	var runID uuid.UUID
	for id := range st.runs {
		runID = id
	}
	st.mu.Unlock()
	ev := findAudit(audit.snapshot(), runID, "run.create", "success")
	if ev == nil {
		t.Fatal("no run.create row")
	}
	if strings.Contains(string(ev.Data), govCorpSecret) {
		t.Errorf("run.create carries the operator secret name: %s", ev.Data)
	}
	var got struct {
		Source policySourceRecord `json:"policy_source"`
	}
	if err := json.Unmarshal(ev.Data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Source.Kind != policyKindInline || !got.Source.Bounded || !slices.Contains(got.Source.Spec.AllowedDomains, "wide.example") {
		t.Errorf("policy_source = %+v, want the inline, bound, pre-clamp base", got.Source)
	}
	if slices.Contains(resolved.AllowedDomains, "wide.example") {
		t.Errorf("the resolved envelope %v kept the clamped host", resolved.AllowedDomains)
	}
}

func TestPreflightRun_WritesNoPolicySource(t *testing.T) {
	srv, _, audit := govEscapeFixture(t, &capStore{})
	w := do(t, srv, http.MethodPost, "/api/v1/runs/preflight", adminToken,
		`{"agent":"claude-code","task":"t","inline_policy":{"min_confinement_class":"CC2","allowed_domains":["api.anthropic.com"]}}`)
	if w.Code != http.StatusOK {
		t.Fatalf("preflight = %d: %s", w.Code, w.Body.String())
	}
	for _, ev := range audit.snapshot() {
		if ev.Action == "run.create" || ev.Action == "policy.inline.apply" {
			t.Errorf("a preflight wrote %s: %s", ev.Action, ev.Data)
		}
	}
}

// TestDispatch_AuditsGitBrokerConfinement: confineGitBrokerEgress used to say
// what it removed only in a log line, so the policy view could not state why
// github.com was denied. The row is written inside the log's own branch.
func TestDispatch_AuditsGitBrokerConfinement(t *testing.T) {
	policy := types.RunPolicySpec{AllowedDomains: []string{"api.anthropic.com", "github.com"}, MinConfinementClass: types.CC1}
	t.Run("a run with a git grant", func(t *testing.T) {
		_, _, events, runID := runWalledDispatch(t, walledDispatch{policy: policy, gitGrants: map[string]uuid.UUID{"o/r": uuid.New()}})
		ev := findAudit(events, runID, "run.egress.confine", "success")
		if ev == nil {
			t.Fatalf("no run.egress.confine row; events=%s", auditDump(events, runID))
		}
		var data struct {
			Hosts []string `json:"hosts"`
		}
		if err := json.Unmarshal(ev.Data, &data); err != nil || !slices.Contains(data.Hosts, "github.com") {
			t.Errorf("hosts = %v (err %v), want the confined github.com", data.Hosts, err)
		}
	})
	t.Run("a run without one", func(t *testing.T) {
		_, _, events, runID := runWalledDispatch(t, walledDispatch{policy: policy})
		if ev := findAudit(events, runID, "run.egress.confine", "success"); ev != nil {
			t.Errorf("run.egress.confine written for a run with no git grant: %s", ev.Data)
		}
	})
}
