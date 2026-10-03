// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

func patRowWith(id, runID uuid.UUID, host string, axes map[string]any) types.CredentialGrant {
	scope := map[string]any{"host": host, "secret_name": "pat-" + host}
	for k, v := range axes {
		scope[k] = v
	}
	return types.CredentialGrant{ID: id, RunID: runID, Spec: types.GrantSpec{Kind: types.GrantGitPAT, Scope: mustJSON(scope)}}
}

func sshKeyRowFor(runID uuid.UUID, host string) types.CredentialGrant {
	return types.CredentialGrant{ID: uuid.New(), RunID: runID, Spec: types.GrantSpec{
		Kind:  types.GrantSSHKey,
		Scope: mustJSON(map[string]any{"host": host, "key_secret_ref": "key-" + host}),
	}}
}

// dispatchWithPATRows dispatches one run whose stored grants are rows(run) and
// whose winning git_pat grants are winning(rows).
func dispatchWithPATRows(t *testing.T, brokerOff bool, gitGrants map[string]uuid.UUID, rows func(runID uuid.UUID) []types.CredentialGrant,
	winning func(rows []types.CredentialGrant) map[string]string,
) (*fakeRunner, *dispatchTestStore, *recRecorder, types.AgentRun) {
	t.Helper()
	fr := &fakeRunner{}
	srv, st, audit, run := dispatchTeardownFixture(t, fr, types.RunPending)
	run.Task = "" // composition only: no agent exec, no completion watcher
	srv.cfg.DisableGitPATBroker = brokerOff
	st.grants = rows(run.ID)
	srv.dispatchRun(context.Background(), run, ceilingForDispatch(governanceCeiling{}, adoEntraUngraded(), bedrockCredUngraded()), dispatchParams{
		RunToken: "run-token", Image: "wardyn/claude-code:latest",
		GitGrants: gitGrants, GitPATGrants: winning(st.grants),
	})
	return fr, st, audit, run
}

// winningByHost is the {host: id} a dispatch is handed for the git_pat rows.
func winningByHost(rows []types.CredentialGrant) map[string]string {
	out := map[string]string{}
	for _, r := range rows {
		if r.Spec.Kind != types.GrantGitPAT {
			continue
		}
		var sc struct {
			Host string `json:"host"`
		}
		_ = json.Unmarshal(r.Spec.Scope, &sc)
		out[sc.Host] = r.ID.String()
	}
	return out
}

// requireRefusedWith asserts the dispatch failed the run with reason, started no
// sandbox, and audited the reason on run.create.
func requireRefusedWith(t *testing.T, fr *fakeRunner, st *dispatchTestStore, audit *recRecorder, run types.AgentRun, reason string) {
	t.Helper()
	if fr.createCalls != 0 {
		t.Fatalf("createCalls = %d, want no sandbox for a refused run", fr.createCalls)
	}
	if got := st.State(); got != types.RunFailed {
		t.Fatalf("run state = %s, want %s", got, types.RunFailed)
	}
	ev := findAudit(audit.events, run.ID, "run.create", "failure")
	if ev == nil {
		t.Fatalf("no run.create failure audited; events=%s", auditDump(audit.events, run.ID))
	}
	if want := `"reason":"` + reason + `"`; !strings.Contains(string(ev.Data), want) {
		t.Fatalf("audit data %s does not carry %s", ev.Data, want)
	}
}

// The PAT is resident with the broker off, so a narrowing would be fiction: the
// run is refused rather than started.
func TestDispatchRefusesANarrowedGrantWithTheBrokerOff(t *testing.T) {
	for _, axes := range []map[string]any{
		{"repos": []string{"team/app"}},
		{"access": "read"},
		{"repos": []string{}},
	} {
		fr, st, audit, run := dispatchWithPATRows(t, true, nil, func(runID uuid.UUID) []types.CredentialGrant {
			return []types.CredentialGrant{patRowWith(uuid.New(), runID, "gitlab.corp.io", axes)}
		}, winningByHost)
		requireRefusedWith(t, fr, st, audit, run, reasonGitPATNarrowingNeedsBroker)
	}
}

// An unnarrowed grant with the broker off still launches: the refusal is about a
// narrowing that cannot bind, not about the escape hatch.
func TestDispatchStillLaunchesAnUnnarrowedGrantWithTheBrokerOff(t *testing.T) {
	fr, _, _, _ := dispatchWithPATRows(t, true, nil, func(runID uuid.UUID) []types.CredentialGrant {
		return []types.CredentialGrant{patRowWith(uuid.New(), runID, "gitlab.corp.io", nil)}
	}, winningByHost)
	if fr.createCalls != 1 {
		t.Fatalf("createCalls = %d, want the run dispatched", fr.createCalls)
	}
}

// SSH is a second push path the broker cannot see. ssh.github.com folds onto
// github.com through sshOver443Endpoint, so both spellings conflict. No
// github_token grant is held, so the run is not GitHub-brokered and the ssh
// conflict, not the unsupported host, is what refuses it.
func TestDispatchRefusesANarrowedGrantBesideASameForgeSSHKey(t *testing.T) {
	for _, sshHost := range []string{"github.com", "ssh.github.com"} {
		t.Run(sshHost, func(t *testing.T) {
			fr, st, audit, run := dispatchWithPATRows(t, false, nil, func(runID uuid.UUID) []types.CredentialGrant {
				return []types.CredentialGrant{
					patRowWith(uuid.New(), runID, "github.com", map[string]any{"repos": []string{"org/repo"}}),
					sshKeyRowFor(runID, sshHost),
				}
			}, winningByHost)
			requireRefusedWith(t, fr, st, audit, run, reasonGitPATNarrowingSSHConflict)
		})
	}
	t.Run("an ssh_key for another forge is not a conflict", func(t *testing.T) {
		fr, _, _, _ := dispatchWithPATRows(t, false, nil, func(runID uuid.UUID) []types.CredentialGrant {
			return []types.CredentialGrant{
				patRowWith(uuid.New(), runID, "gitlab.corp.io", map[string]any{"repos": []string{"team/app"}}),
				sshKeyRowFor(runID, "github.com"),
			}
		}, winningByHost)
		if fr.createCalls != 1 {
			t.Fatalf("createCalls = %d, want the run dispatched", fr.createCalls)
		}
	})
}

// A host another lane serves would never apply the narrowing: dev.azure.com is
// served by the Azure DevOps lane, and a GitHub-brokered forge's PAT is withheld.
func TestDispatchRefusesANarrowedGrantOnAHostAnotherLaneServes(t *testing.T) {
	t.Run("dev.azure.com", func(t *testing.T) {
		fr, st, audit, run := dispatchWithPATRows(t, false, nil, func(runID uuid.UUID) []types.CredentialGrant {
			return []types.CredentialGrant{patRowWith(uuid.New(), runID, "dev.azure.com", map[string]any{"access": "read"})}
		}, winningByHost)
		requireRefusedWith(t, fr, st, audit, run, reasonGitPATNarrowingUnsupportedHost)
	})
	t.Run("a GitHub-brokered forge", func(t *testing.T) {
		fr, st, audit, run := dispatchWithPATRows(t, false, map[string]uuid.UUID{"org/repo": uuid.New()},
			func(runID uuid.UUID) []types.CredentialGrant {
				return []types.CredentialGrant{patRowWith(uuid.New(), runID, "github.com", map[string]any{"repos": []string{"org/repo"}})}
			}, winningByHost)
		requireRefusedWith(t, fr, st, audit, run, reasonGitPATNarrowingUnsupportedHost)
	})
}

// patNarrowingRefusal over a host the run's Azure DevOps gate covers, which a site
// config row can name and no address betrays.
func TestPATNarrowingRefusalCoversTheRunsADOGateHosts(t *testing.T) {
	grants := []types.GrantSpec{patRowWith(uuid.New(), uuid.New(), "tfs.corp.example", map[string]any{"repos": []string{"proj/repo"}}).Spec}
	env := patNarrowingEnv{brokerOn: true}
	if reason, _ := patNarrowingRefusal(grants, env); reason != "" {
		t.Fatalf("refused %q with no Azure DevOps gate, want it accepted", reason)
	}
	env.adoHosts = []string{"TFS.corp.example"}
	if reason, detail := patNarrowingRefusal(grants, env); reason != reasonGitPATNarrowingUnsupportedHost {
		t.Fatalf("reason = %q (%s), want %q for a host the gate covers", reason, detail, reasonGitPATNarrowingUnsupportedHost)
	}
}

// A winning grant id with no stored row has an unknown scope, and an unknown
// scope must not read as an unnarrowed one.
func TestDispatchFailsClosedOnAWinningGrantWithNoRow(t *testing.T) {
	fr, st, audit, run := dispatchWithPATRows(t, false, nil, func(uuid.UUID) []types.CredentialGrant { return nil },
		func([]types.CredentialGrant) map[string]string {
			return map[string]string{"gitlab.corp.io": uuid.NewString()}
		})
	if fr.createCalls != 0 {
		t.Fatalf("createCalls = %d, want no sandbox", fr.createCalls)
	}
	if got := st.State(); got != types.RunFailed {
		t.Fatalf("run state = %s, want %s", got, types.RunFailed)
	}
	ev := findAudit(audit.events, run.ID, "run.create", "failure")
	if ev == nil || !strings.Contains(string(ev.Data), "has no stored row") {
		t.Fatalf("audit = %s, want a failure naming the missing row", auditDump(audit.events, run.ID))
	}
}

// The scope reaches the proxy for a narrowed grant, from the stored row and
// normalised, and an unnarrowed grant carries none of it.
func TestDispatchCarriesAGrantsScopeToTheProxy(t *testing.T) {
	fr, _, _, _ := dispatchWithPATRows(t, false, nil, func(runID uuid.UUID) []types.CredentialGrant {
		return []types.CredentialGrant{
			patRowWith(uuid.New(), runID, "gitlab.corp.io", map[string]any{
				"repos": []string{"team/app", "group/*"}, "access": "read", "forge": "gitlab"}),
			patRowWith(uuid.New(), runID, "gitea.corp.io", map[string]any{"repos": []string{}}),
			patRowWith(uuid.New(), runID, "plain.corp.io", nil),
			patRowWith(uuid.New(), runID, "forge-only.corp.io", map[string]any{"forge": "gitea"}),
		}
	}, winningByHost)
	if fr.createCalls != 1 {
		t.Fatalf("createCalls = %d, want the run dispatched", fr.createCalls)
	}
	got := fr.lastSpec.ProxyConfig.PATGrants

	narrowed := got["gitlab.corp.io"]
	if narrowed.Repos == nil || strings.Join(*narrowed.Repos, ",") != "team/app,group/*" || narrowed.Access != "read" || narrowed.Forge != "gitlab" {
		t.Errorf("gitlab grant = %+v, want repos, read and gitlab", narrowed)
	}
	if empty := got["gitea.corp.io"]; empty.Repos == nil || len(*empty.Repos) != 0 {
		t.Errorf("empty-repos grant = %+v, want a non-nil empty list: none, never all", empty)
	}
	for _, host := range []string{"plain.corp.io", "forge-only.corp.io"} {
		g := got[host]
		if g.Repos != nil || g.Access != "" || g.Forge != "" || g.API {
			t.Errorf("%s grant = %+v, want no scope carried for an unnarrowed grant", host, g)
		}
	}
}

// A denied host's lane, dropped by the governance ceiling's re-assertion, stays
// dropped: scope is added to what survives, never to the map before it is
// narrowed.
func TestDispatchScopesOnlyTheGrantsTheCeilingKept(t *testing.T) {
	const denied = "denied.corp.example"
	_, spec, _, _ := runWalledDispatch(t, walledDispatch{
		deny:   []string{denied},
		policy: types.RunPolicySpec{AllowedDomains: []string{"api.anthropic.com", denied, "gitlab.com"}, MinConfinementClass: types.CC2},
		patGrants: map[string]string{
			denied:       uuid.NewString(),
			"gitlab.com": uuid.NewString(),
		},
	})
	if _, ok := spec.ProxyConfig.PATGrants[denied]; ok {
		t.Fatalf("PATGrants = %v, want the denied host's lane dropped", spec.ProxyConfig.PATGrants)
	}
	if _, ok := spec.ProxyConfig.PATGrants["gitlab.com"]; !ok {
		t.Fatalf("PATGrants = %v, want the other host kept", spec.ProxyConfig.PATGrants)
	}
}

// Review answers the same refusals launch gives at dispatch.
func TestPreflightMirrorsThePATNarrowingRefusals(t *testing.T) {
	narrowed := patGrantSpec(`"repos":["team/app"]`)
	githubNarrowed := types.GrantSpec{Kind: types.GrantGitPAT, Scope: json.RawMessage(`{"host":"github.com","secret_name":"pat","repos":["org/repo"]}`)}
	ssh := types.GrantSpec{Kind: types.GrantSSHKey, Scope: json.RawMessage(`{"host":"ssh.github.com","key_secret_ref":"pat"}`)}
	adoNarrowed := types.GrantSpec{Kind: types.GrantGitPAT, Scope: json.RawMessage(`{"host":"dev.azure.com","secret_name":"pat","access":"read"}`)}
	githubToken := types.GrantSpec{Kind: types.GrantGitHubToken, Scope: json.RawMessage(`{"repos":["org/repo"]}`)}

	for _, tc := range []struct {
		name      string
		brokerOff bool
		grants    []types.GrantSpec
		want      string // "" = Review passes
	}{
		{"broker off", true, []types.GrantSpec{narrowed}, reasonGitPATNarrowingNeedsBroker},
		{"ssh conflict", false, []types.GrantSpec{githubNarrowed, ssh}, reasonGitPATNarrowingSSHConflict},
		{"azure devops host", false, []types.GrantSpec{adoNarrowed}, reasonGitPATNarrowingUnsupportedHost},
		{"brokered forge", false, []types.GrantSpec{githubNarrowed, githubToken}, reasonGitPATNarrowingUnsupportedHost},
		{"narrowed grant, broker on", false, []types.GrantSpec{narrowed}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, st, _ := govEscapeFixture(t, &capStore{})
			srv.cfg.Secrets = &memSecrets{m: map[string][]byte{"pat": []byte("v"), "key-github.com": []byte("k")}}
			srv.cfg.DisableGitPATBroker = tc.brokerOff
			id := uuid.New()
			st.policies[id] = types.RunPolicy{ID: id, Name: id.String(), Spec: types.RunPolicySpec{
				MinConfinementClass: types.CC1, EligibleGrants: tc.grants}}
			body := `{"agent":"claude-code","task":"t","policy_id":"` + id.String() + `"}`
			w := do(t, srv, http.MethodPost, "/api/v1/runs/preflight", adminToken, body)
			if tc.want == "" {
				if w.Code != http.StatusOK {
					t.Fatalf("preflight = %d %s, want 200", w.Code, w.Body.String())
				}
				return
			}
			if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), `"reason":"`+tc.want+`"`) {
				t.Fatalf("preflight = %d %s, want 422 with reason %s", w.Code, w.Body.String(), tc.want)
			}
		})
	}
}

// Policy write refuses a narrowed git_pat beside a same-forge ssh_key, naming the
// rule, for both spellings of the forge; the wire reason is the rule's own.
func TestPolicyWriteRefusesANarrowedGitPATBesideASameForgeSSHKey(t *testing.T) {
	for _, sshHost := range []string{"github.com", "ssh.github.com"} {
		t.Run(sshHost, func(t *testing.T) {
			spec := types.RunPolicySpec{MinConfinementClass: types.CC2, EligibleGrants: []types.GrantSpec{
				{Kind: types.GrantGitPAT, Scope: json.RawMessage(`{"host":"github.com","secret_name":"pat","repos":["org/repo"]}`)},
				{Kind: types.GrantSSHKey, Scope: mustJSON(map[string]any{"host": sshHost, "key_secret_ref": "key"})},
			}}
			err := validatePolicySpec(spec)
			if err == nil || !strings.Contains(err.Error(), reasonGitPATNarrowingSSHConflict) {
				t.Fatalf("validatePolicySpec = %v, want a refusal naming %s", err, reasonGitPATNarrowingSSHConflict)
			}
			if got := specRefusalReason(err, "bucket"); got != reasonGitPATNarrowingSSHConflict {
				t.Fatalf("specRefusalReason = %q, want %q", got, reasonGitPATNarrowingSSHConflict)
			}

			h := newHarness(t)
			w := do(t, h.srv, http.MethodPost, "/api/v1/policies", adminToken,
				`{"name":"narrow-ssh-`+sshHost+`","spec":`+string(mustJSON(spec))+`}`)
			if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), `"reason":"`+reasonGitPATNarrowingSSHConflict+`"`) ||
				!strings.Contains(w.Body.String(), reasonGitPATNarrowingSSHConflict+":") {
				t.Fatalf("POST /api/v1/policies = %d %s, want 400 naming %s", w.Code, w.Body.String(), reasonGitPATNarrowingSSHConflict)
			}
		})
	}
	t.Run("an unnarrowed git_pat beside an ssh_key is accepted", func(t *testing.T) {
		spec := types.RunPolicySpec{MinConfinementClass: types.CC2, EligibleGrants: []types.GrantSpec{
			{Kind: types.GrantGitPAT, Scope: json.RawMessage(`{"host":"github.com","secret_name":"pat"}`)},
			{Kind: types.GrantSSHKey, Scope: mustJSON(map[string]any{"host": "github.com", "key_secret_ref": "key"})},
		}}
		if err := validatePolicySpec(spec); err != nil {
			t.Fatalf("validatePolicySpec = %v, want it accepted", err)
		}
	})
}
