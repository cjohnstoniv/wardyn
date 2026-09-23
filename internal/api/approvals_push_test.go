// Copyright 2025 The Wardyn Authors
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

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// pushContentScopeJSON is a well-formed push_content requested_scope.
func pushContentScopeJSON(t *testing.T) string {
	t.Helper()
	b, err := json.Marshal(types.PushContentScope{
		Repo:        "github.com/octocat/hello-world",
		Branch:      "refs/heads/wardyn/run/work",
		ActsAs:      "github_token:" + uuid.NewString(),
		Paths:       []string{".github/workflows/ci.yml"},
		PathsTotal:  1,
		Commits:     []string{strings.Repeat("a", 40)},
		PathsDigest: strings.Repeat("0", 64),
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// seedPushContent adds a PENDING push_content row on the fixture's run.
func (f *scopeFixture) seedPushContent(t *testing.T) uuid.UUID {
	t.Helper()
	id := uuid.New()
	f.approval.mu.Lock()
	f.approval.byID[id] = types.ApprovalRequest{
		ID: id, RunID: f.runID, Kind: types.ApprovalPushContent,
		RequestedScope: json.RawMessage(pushContentScopeJSON(t)),
		State:          types.ApprovalPending, RequestedAt: time.Now().UTC(),
	}
	f.approval.mu.Unlock()
	return id
}

// TestPushContentMemberCannotDecide: push_content is admin-decidable only. A
// member — the run's own owner included — gets the member gate's existing
// byte-identical 404, on approve and on deny, and the row stays PENDING. A
// member approving their own workflow-file edit is the exfiltration the rule
// stops.
func TestPushContentMemberCannotDecide(t *testing.T) {
	f := newScopeFixture(t)
	owner := ssoSession(t, f.memberID, "member@corp.example", oidc.RoleMember)

	for _, verb := range []string{"approve", "deny"} {
		t.Run(verb, func(t *testing.T) {
			id := f.seedPushContent(t)
			w := doSSO(t, f.srv, http.MethodPost, "/api/v1/approvals/"+id.String()+"/"+verb, owner, "")
			if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "approval not found") {
				t.Fatalf("run owner (member) %s on push_content: status = %d body=%s, want 404 approval not found",
					verb, w.Code, w.Body.String())
			}
			f.approval.mu.Lock()
			got := f.approval.byID[id].State
			f.approval.mu.Unlock()
			if got != types.ApprovalPending {
				t.Errorf("state after a refused member decision = %q, want PENDING", got)
			}
		})
	}
}

// TestPushContentAdminDecides: an admin decides one bodyless, and rule 4
// refuses a decision_scope on it — the sidecar reads an approval as covering
// exactly the commits the scope names, so a scope would mean nothing.
func TestPushContentAdminDecides(t *testing.T) {
	f := newScopeFixture(t)
	admin := ssoSession(t, "sub-admin-push", "admin@corp.example", oidc.RoleAdmin)

	id := f.seedPushContent(t)
	if w := doSSO(t, f.srv, http.MethodPost, "/api/v1/approvals/"+id.String()+"/approve",
		admin, decideBody(t, types.ScopeRun, nil)); w.Code != http.StatusBadRequest {
		t.Fatalf("scope on push_content: status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
	if w := doSSO(t, f.srv, http.MethodPost, "/api/v1/approvals/"+id.String()+"/approve", admin, ""); w.Code != http.StatusOK {
		t.Fatalf("admin approve: status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
}

// grantStore is authzStore with a run's grants, which authzStore leaves empty.
type grantStore struct {
	*authzStore
	grants []types.CredentialGrant
}

func (g grantStore) ListGrantsByRun(_ context.Context, runID uuid.UUID) ([]types.CredentialGrant, error) {
	var out []types.CredentialGrant
	for _, gr := range g.grants {
		if gr.RunID == runID {
			out = append(out, gr)
		}
	}
	return out, nil
}

// withActsAs is pushContentScopeJSON naming a different credential.
func withActsAs(t *testing.T, actsAs string) string {
	t.Helper()
	var sc map[string]any
	if err := json.Unmarshal([]byte(pushContentScopeJSON(t)), &sc); err != nil {
		t.Fatal(err)
	}
	sc["acts_as"] = actsAs
	return string(mustJSON(sc))
}

// TestInternalPushContentRaise drives the sidecar's raise: an attended run's
// well-formed scope becomes a row, stamped server-side with who the push acts
// as; an unattended run's, a malformed one, one naming a grant the run does not
// hold, or one carrying its own label, does not.
func TestInternalPushContentRaise(t *testing.T) {
	h := newHarness(t)
	ast := newAuthzStore()
	attended, unattended, other := uuid.New(), uuid.New(), uuid.New()
	const owner = "alice@example.com" // mintRunToken's subject
	app, ownPAT, sharedPAT, foreign := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	patScope := func(secret string) json.RawMessage {
		return json.RawMessage(`{"host":"gitlab.com","secret_name":"` + secret + `"}`)
	}
	st := grantStore{authzStore: ast, grants: []types.CredentialGrant{
		{ID: app, RunID: attended, Spec: types.GrantSpec{Kind: types.GrantGitHubToken}},
		{ID: ownPAT, RunID: attended, Spec: types.GrantSpec{Kind: types.GrantGitPAT, Scope: patScope("alice-pat")}},
		{ID: sharedPAT, RunID: attended, Spec: types.GrantSpec{Kind: types.GrantGitPAT, Scope: patScope("team-pat")}},
		{ID: foreign, RunID: other, Spec: types.GrantSpec{Kind: types.GrantGitHubToken}},
	}}
	secrets := &memSecrets{m: map[string][]byte{"team-pat": []byte("x"), "alice-pat": []byte("x")}}
	if err := secrets.For(owner).Put(context.Background(), "alice-pat", []byte("y")); err != nil {
		t.Fatal(err)
	}
	aap := newAuthzApprovals(ast)
	cfg := baseTestConfig(h, st)
	cfg.Approvals = aap
	cfg.Secrets = secrets
	srv := New(cfg)

	ast.mu.Lock()
	ast.runs[attended] = types.AgentRun{ID: attended, State: types.RunRunning, Interactive: true, CreatedBy: owner}
	ast.runs[unattended] = types.AgentRun{ID: unattended, State: types.RunRunning, CreatedBy: owner}
	ast.mu.Unlock()
	raise := func(runID uuid.UUID, scope string) int {
		w := do(t, srv, http.MethodPost, "/api/v1/internal/approvals", h.mintRunToken(t, runID),
			`{"kind":"push_content","requested_scope":`+scope+`}`)
		return w.Code
	}
	stored := func() map[string]types.PushContentScope {
		aap.mu.Lock()
		defer aap.mu.Unlock()
		out := map[string]types.PushContentScope{}
		for _, ap := range aap.byID {
			var sc types.PushContentScope
			if err := json.Unmarshal(ap.RequestedScope, &sc); err != nil {
				t.Fatal(err)
			}
			out[sc.ActsAs] = sc
		}
		return out
	}

	want := map[string][2]string{
		"github_token:" + app.String():  {types.PushActsAsGitHubApp, owner},
		"git_pat:" + ownPAT.String():    {types.PushActsAsGitPAT, owner},
		"git_pat:" + sharedPAT.String(): {types.PushActsAsGitPAT, types.PushActsAsOperator},
	}
	for actsAs := range want {
		if got := raise(attended, withActsAs(t, actsAs)); got != http.StatusCreated {
			t.Fatalf("attended raise acting as %s: status = %d, want 201", actsAs, got)
		}
	}
	rows := stored()
	for actsAs, kl := range want {
		if sc := rows[actsAs]; sc.ActsAsKind != kl[0] || sc.ActsAsLabel != kl[1] {
			t.Errorf("%s stored as kind %q label %q, want %q %q", actsAs, sc.ActsAsKind, sc.ActsAsLabel, kl[0], kl[1])
		}
	}

	for name, c := range map[string]struct {
		run   uuid.UUID
		scope string
		want  int
	}{
		"unattended run":      {unattended, withActsAs(t, "github_token:"+app.String()), http.StatusForbidden},
		"unknown field":       {attended, strings.Replace(pushContentScopeJSON(t), `{`, `{"host":"x",`, 1), http.StatusBadRequest},
		"eleven paths":        {attended, strings.Replace(pushContentScopeJSON(t), `"paths":[".github/workflows/ci.yml"]`, `"paths":["a","b","c","d","e","f","g","h","i","j","k"]`, 1), http.StatusBadRequest},
		"not an object id":    {attended, strings.Replace(pushContentScopeJSON(t), strings.Repeat("a", 40), "HEAD", 1), http.StatusBadRequest},
		"another run's grant": {attended, withActsAs(t, "github_token:"+foreign.String()), http.StatusBadRequest},
		"grant kind mismatch": {attended, withActsAs(t, "git_pat:"+app.String()), http.StatusBadRequest},
		"sidecar-sent label":  {attended, strings.Replace(withActsAs(t, "github_token:"+app.String()), `{`, `{"acts_as_label":"root@evil",`, 1), http.StatusBadRequest},
		"sidecar-sent kind":   {attended, strings.Replace(withActsAs(t, "github_token:"+app.String()), `{`, `{"acts_as_kind":"git_pat",`, 1), http.StatusBadRequest},
	} {
		if got := raise(c.run, c.scope); got != c.want {
			t.Errorf("%s: status = %d, want %d", name, got, c.want)
		}
	}
	if n := len(stored()); n != len(want) {
		t.Errorf("rows = %d, want still %d: a refused raise must write nothing", n, len(want))
	}
}

// TestDispatchStampsUnattended: the sidecar learns a run is unattended from
// dispatch, from the same flag that decides whether anyone drives it.
func TestDispatchStampsUnattended(t *testing.T) {
	for _, interactive := range []bool{true, false} {
		fr := &fakeRunner{}
		srv, _, _, run := dispatchTeardownFixture(t, fr, types.RunPending)
		run.Task = ""
		srv.dispatchRun(context.Background(), run, ceilingForDispatch(governanceCeiling{}), dispatchParams{
			RunToken: "run-token", Image: "wardyn/claude-code:latest", Interactive: interactive,
		})
		if got := fr.lastSpec.ProxyConfig.Unattended; got == interactive {
			t.Errorf("Interactive=%v dispatched ProxyConfig.Unattended=%v", interactive, got)
		}
	}
}
