// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func sshKeyPolicy(host, keyRef string) types.RunPolicySpec {
	return types.RunPolicySpec{
		MinConfinementClass: types.CC2,
		EligibleGrants: []types.GrantSpec{{
			Kind:  types.GrantSSHKey,
			Scope: mustJSON(map[string]any{"host": host, "key_secret_ref": keyRef}),
		}},
	}
}

// TestSSHOver443Endpoint asserts the clone-URL rewrite mapping: the supported SCM
// hosts rewrite to their SSH-over-443 endpoint (PORT-QUALIFIED :443, so the
// egress entry matches ONLY :443), and unsupported hosts are refused.
func TestSSHOver443Endpoint(t *testing.T) {
	cases := []struct {
		host   string
		wantEP string
		wantOK bool
	}{
		{"github.com", "ssh.github.com:443", true},
		{"ssh.github.com", "ssh.github.com:443", true},
		{"GitHub.com", "ssh.github.com:443", true},  // case-insensitive
		{"github.com.", "ssh.github.com:443", true}, // trailing dot tolerated
		{"dev.azure.com", "ssh.dev.azure.com:443", true},
		{"ssh.dev.azure.com", "ssh.dev.azure.com:443", true},
		{"gitlab.com", "", false},         // unsupported (no published :443 SSH)
		{"ghes.corp.internal", "", false}, // custom GHES: out of scope for SSH
		{"", "", false},
	}
	for _, c := range cases {
		ep, ok := sshOver443Endpoint(c.host)
		if ok != c.wantOK || ep != c.wantEP {
			t.Errorf("sshOver443Endpoint(%q) = (%q,%v), want (%q,%v)", c.host, ep, ok, c.wantEP, c.wantOK)
		}
	}
}

// TestSSHKeyScopeFields asserts the scope decoder returns the fields and fails
// closed on a missing host or key_secret_ref.
func TestSSHKeyScopeFields(t *testing.T) {
	host, keyRef, user, khRef, err := sshKeyScopeFields(mustJSON(map[string]any{
		"host": "github.com", "key_secret_ref": "gh-key", "username": "git", "known_hosts_secret_ref": "kh",
	}))
	if err != nil || host != "github.com" || keyRef != "gh-key" || user != "git" || khRef != "kh" {
		t.Fatalf("decode = (%q,%q,%q,%q,%v), want github.com/gh-key/git/kh/nil", host, keyRef, user, khRef, err)
	}
	if _, _, _, _, err := sshKeyScopeFields(mustJSON(map[string]any{"host": "github.com"})); err == nil {
		t.Fatal("missing key_secret_ref: expected error")
	}
	if _, _, _, _, err := sshKeyScopeFields(mustJSON(map[string]any{"key_secret_ref": "k"})); err == nil {
		t.Fatal("missing host: expected error")
	}
}

// TestValidatePolicySpec_SSHKey asserts the write-time invariants for ssh_key: a
// valid grant passes; empty host, empty key_secret_ref, an unsupported host, and
// a reserved secret name (key or known_hosts) are rejected (fail closed).
func TestValidatePolicySpec_SSHKey(t *testing.T) {
	if err := validatePolicySpec(sshKeyPolicy("github.com", "gh-ssh-key")); err != nil {
		t.Fatalf("valid ssh_key grant rejected: %v", err)
	}

	khReserved := types.RunPolicySpec{
		MinConfinementClass: types.CC2,
		EligibleGrants: []types.GrantSpec{{
			Kind: types.GrantSSHKey,
			Scope: mustJSON(map[string]any{
				"host": "github.com", "key_secret_ref": "gh-key", "known_hosts_secret_ref": "wardyn-signing-key",
			}),
		}},
	}

	bad := []struct {
		name string
		spec types.RunPolicySpec
	}{
		{"empty-host", sshKeyPolicy("", "gh-ssh-key")},
		{"empty-key-ref", sshKeyPolicy("github.com", "")},
		{"unsupported-host", sshKeyPolicy("gitlab.com", "gl-key")},
		{"reserved-key-secret", sshKeyPolicy("github.com", "wardyn-signing-key")},
		{"reserved-known-hosts", khReserved},
	}
	for _, c := range bad {
		t.Run(c.name, func(t *testing.T) {
			if err := validatePolicySpec(c.spec); err == nil {
				t.Fatalf("%s: expected validatePolicySpec to reject, got nil", c.name)
			}
		})
	}
}

// TestValidatePolicySpec_BrokeredForgeIsSingleLane pins the write-time half of
// "brokered means single-lane": a policy may not declare BOTH a github_token
// grant (the brokered, branch-namespace-confined lane) and an ssh_key grant for
// the same forge (an unparseable second push path). Either alone is fine, an
// ssh_key for a DIFFERENT forge is fine, and git_pat is deliberately NOT refused.
func TestValidatePolicySpec_BrokeredForgeIsSingleLane(t *testing.T) {
	ghToken := types.GrantSpec{
		Kind:  types.GrantGitHubToken,
		Scope: mustJSON(map[string]any{"repos": []string{"acme/widgets"}, "permissions": map[string]string{"contents": "write"}}),
	}
	sshFor := func(host string) types.GrantSpec {
		return types.GrantSpec{Kind: types.GrantSSHKey, Scope: mustJSON(map[string]any{"host": host, "key_secret_ref": "gh-ssh-key"})}
	}
	withGrants := func(gs ...types.GrantSpec) types.RunPolicySpec {
		return types.RunPolicySpec{MinConfinementClass: types.CC2, EligibleGrants: gs}
	}

	// REFUSED: both lanes to github.com, in either grant order and under either
	// spelling of the host (sshOver443Endpoint folds ssh.github.com onto the forge).
	for _, c := range []struct {
		name string
		spec types.RunPolicySpec
	}{
		{"token-then-ssh", withGrants(ghToken, sshFor("github.com"))},
		{"ssh-then-token", withGrants(sshFor("github.com"), ghToken)},
		{"ssh-host-alias", withGrants(ghToken, sshFor("ssh.github.com"))},
		{"host-case-and-dot", withGrants(ghToken, sshFor("GitHub.com."))},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := validatePolicySpec(c.spec)
			if err == nil {
				t.Fatal("expected the github_token + ssh_key combination to be refused")
			}
			// The message must name both kinds and the host, not just "invalid".
			for _, want := range []string{"github_token", "ssh_key", "single-lane"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error must mention %q, got: %v", want, err)
				}
			}
		})
	}

	// ACCEPTED: each lane alone, a different forge, and git_pat alongside the
	// broker. git_pat is not refused on purpose — on a brokered run it is already
	// dead twice (wardyn-git-helper refuses on isGitHubHost before the PAT
	// fallback when WARDYN_GIT_BROKER_REPOS is set, and github.com is one of the
	// four broker denies), so refusing it would change behaviour for no gain.
	gitPAT := types.GrantSpec{
		Kind:  types.GrantGitPAT,
		Scope: mustJSON(map[string]any{"host": "github.com", "secret_name": "gh-pat"}),
	}
	for _, c := range []struct {
		name string
		spec types.RunPolicySpec
	}{
		{"github_token-alone", withGrants(ghToken)},
		{"ssh_key-alone", withGrants(sshFor("github.com"))},
		{"ssh_key-other-forge", withGrants(ghToken, sshFor("dev.azure.com"))},
		{"git_pat-with-broker", withGrants(ghToken, gitPAT)},
	} {
		t.Run(c.name, func(t *testing.T) {
			if err := validatePolicySpec(c.spec); err != nil {
				t.Fatalf("%s must be accepted, got: %v", c.name, err)
			}
		})
	}
}

// TestBrokeredRunSSHLaneEvaluatesDeny reproduces the review probe end to end,
// through the REAL evaluator: build the effective policy of a brokered run that
// also holds an ssh_key grant for github.com — create time widens the allowlist
// with the SSH-over-443 endpoint (unionRunEgress), dispatch confines it
// (confineGitBrokerEgress) — then compile it and ask the proxy. github.com:443
// was denied and ssh.github.com:443 ALLOWED; both are deny now. It also probes
// the case the deny's BARE-host spelling exists for — allow_all_egress at port
// 22, where a ":443"-spelled deny measurably lets ssh.github.com:22 through. The
// non-brokered control keeps its SSH lane, on both ports.
func TestBrokeredRunSSHLaneEvaluatesDeny(t *testing.T) {
	ep, ok := sshOver443Endpoint("github.com")
	if !ok {
		t.Fatal("github.com must have an SSH-over-443 endpoint")
	}
	effective := func(gitGrants map[string]uuid.UUID) types.RunPolicySpec {
		spec := types.RunPolicySpec{
			MinConfinementClass: types.CC2,
			// "*.github.com" is NOT subtracted (it is not a broker-managed entry) —
			// the deny is what has to beat it, which is why the deny is the bare host.
			AllowedDomains: []string{"api.anthropic.com", "github.com", "*.github.com"},
		}
		unionAllowedDomains(&spec, []string{ep}) // create time: the ssh_key grant's lane
		confineGitBrokerEgress(&spec, gitGrants) // dispatch: LAST policy phase
		return spec
	}
	probe := func(t *testing.T, spec types.RunPolicySpec, host string, port int, want egress.HostVerdict) {
		t.Helper()
		v, err := proxy.NewBuiltinEvaluator(spec).EvaluateHost(context.Background(), egress.Request{Host: host, Port: port})
		if err != nil {
			t.Fatalf("evaluate %s:%d: %v", host, port, err)
		}
		if v != want {
			t.Errorf("%s:%d = %v, want %v (allowed=%v denied=%v allow_all=%v)",
				host, port, v, want, spec.AllowedDomains, spec.DeniedDomains, spec.AllowAllEgress)
		}
	}

	brokeredGrants := map[string]uuid.UUID{"acme/widgets": uuid.New()}
	brokered := effective(brokeredGrants)
	probe(t, brokered, "github.com", 443, egress.VerdictDeny)
	probe(t, brokered, "ssh.github.com", 443, egress.VerdictDeny)
	probe(t, brokered, "api.anthropic.com", 443, egress.VerdictAllow)

	// THE PROBE THE BARE-HOST DENY EXISTS FOR — and the only one that fails if the
	// deny is spelled ":443". Two things have to be true at once: allow_all_egress
	// (so the allowlist subtraction is worth nothing — everything not denied is
	// allowed, and the deny is the ONLY remaining half) and port 22 (plain
	// git-over-SSH, which no ":443" entry matches). Measured on this evaluator:
	// a ":443"-spelled deny gives ssh.github.com:22 = ALLOW, a bare-host deny gives
	// deny. Without this case the confineGitBrokerEgress deviation is pinned only
	// by a string-equality assertion on the deny's spelling, which any rewrite of
	// that assertion silently discards.
	openBrokered := effective(brokeredGrants)
	openBrokered.AllowAllEgress = true
	probe(t, openBrokered, "ssh.github.com", 22, egress.VerdictDeny)
	probe(t, openBrokered, "ssh.github.com", 443, egress.VerdictDeny)
	probe(t, openBrokered, "github.com", 22, egress.VerdictDeny)

	// NOT brokered (no broker map): the operator's SSH lane is untouched, on both
	// ports and under allow_all_egress too — the confinement must be scoped to
	// actual brokered-ness, never a blanket ban on SSH to a forge.
	plain := effective(nil)
	probe(t, plain, "ssh.github.com", 443, egress.VerdictAllow)
	probe(t, plain, "github.com", 443, egress.VerdictAllow)
	openPlain := effective(nil)
	openPlain.AllowAllEgress = true
	probe(t, openPlain, "ssh.github.com", 22, egress.VerdictAllow)
}

// TestBrokeredRunWithholdsSSHGrantEnv pins the CREDENTIAL half of "brokered means
// single-lane". Closing the network lane is not enough on its own: a PRE-EXISTING
// stored policy can still hold both grants (validateGrantLaneExclusivity refuses
// new writes only, and resolveRunPolicy deliberately does not re-validate stored
// specs), and until this drop existed such a run still exported WARDYN_SSH_GRANTS
// — so agent-run minted the key and wrote it 0400 into a sandbox it could not use
// it from, leaving it re-mintable and exfiltratable for the run's lifetime.
//
// The drop is scoped, not a blanket: only a BROKERED forge's host, only on a run
// that is actually brokered, and the caller's map is never mutated.
func TestBrokeredRunWithholdsSSHGrantEnv(t *testing.T) {
	run := types.AgentRun{ID: uuid.New()}
	brokered := map[string]uuid.UUID{"acme/widgets": uuid.New()}
	apply := func(sshGrants map[string]string, gitGrants map[string]uuid.UUID) (map[string]string, []string) {
		env := map[string]string{}
		dropped := applyDispatchModeEnv(env, run, nil, false, "", nil, nil, sshGrants, gitGrants)
		return env, dropped
	}

	// Brokered run, ssh_key for the brokered forge + one for another forge: the
	// brokered host is withheld and REPORTED (the caller warns + audits on it);
	// the unrelated forge's key still ships.
	mixed := map[string]string{"github.com": uuid.NewString(), "dev.azure.com": uuid.NewString()}
	env, dropped := apply(mixed, brokered)
	if !slices.Equal(dropped, []string{"github.com"}) {
		t.Errorf("dropped = %v, want [github.com] — a silent drop leaves the operator with no explanation", dropped)
	}
	if got := env["WARDYN_SSH_GRANTS"]; strings.Contains(got, "github.com") || !strings.Contains(got, "dev.azure.com") {
		t.Errorf("WARDYN_SSH_GRANTS = %q, want the brokered forge withheld and dev.azure.com kept", got)
	}
	if len(mixed) != 2 {
		t.Errorf("the caller's ssh grant map was mutated: %v", mixed)
	}

	// Brokered run whose ONLY ssh_key is the brokered forge's (under either host
	// spelling): the env var must be ABSENT, not an empty map — agent-run branches
	// on the key existing.
	for _, host := range []string{"github.com", "ssh.github.com"} {
		env, dropped := apply(map[string]string{host: uuid.NewString()}, brokered)
		if _, ok := env["WARDYN_SSH_GRANTS"]; ok {
			t.Errorf("%s: WARDYN_SSH_GRANTS = %q, want it unset entirely", host, env["WARDYN_SSH_GRANTS"])
		}
		if !slices.Equal(dropped, []string{host}) {
			t.Errorf("%s: dropped = %v, want [%s]", host, dropped, host)
		}
	}

	// NOT brokered: the operator's ssh_key is untouched, nothing to report.
	env, dropped = apply(map[string]string{"github.com": uuid.NewString()}, nil)
	if !strings.Contains(env["WARDYN_SSH_GRANTS"], "github.com") || dropped != nil {
		t.Errorf("non-brokered run lost its SSH grant: env=%q dropped=%v", env["WARDYN_SSH_GRANTS"], dropped)
	}
}

// TestDispatch_BrokeredSSHDropIsAuditedAndNeverReachesTheSandbox runs the drop
// through a REAL dispatch, so it pins the two things the unit test above cannot:
// the env the runner is actually handed (the key's grant id must not be in the
// SandboxSpec at all — agent-run reads it from there) and the audit event that
// makes the drop non-silent. A withheld credential the operator is never told
// about is the failure mode this whole class of drop has (see the codex-cli
// precedent, applySSHLaneWarnings).
func TestDispatch_BrokeredSSHDropIsAuditedAndNeverReachesTheSandbox(t *testing.T) {
	fr := &fakeRunner{}
	srv, _, audit, run := dispatchTeardownFixture(t, fr, types.RunPending)
	run.Task = "" // no agent exec / completion watcher; this test is about dispatch

	srv.dispatchRun(context.Background(), run, dispatchParams{
		RunToken: "run-token", Image: "wardyn/claude-code:latest",
		Policy:    types.RunPolicySpec{AllowedDomains: []string{"api.anthropic.com"}, MinConfinementClass: types.CC1},
		GitGrants: map[string]uuid.UUID{"acme/widgets": uuid.New()},
		SSHGrants: map[string]string{"github.com": uuid.NewString()},
	})

	if v, ok := fr.lastSpec.Env["WARDYN_SSH_GRANTS"]; ok {
		t.Errorf("the sandbox was handed WARDYN_SSH_GRANTS=%q on a brokered run — agent-run will mint the key and write it 0400", v)
	}
	ev := findAudit(audit.events, run.ID, "run.ssh.brokered_forge", "failure")
	if ev == nil {
		t.Fatalf("the ssh_key drop was SILENT: no run.ssh.brokered_forge event; events=%s", auditDump(audit.events, run.ID))
	}
	if !strings.Contains(string(ev.Data), "github.com") {
		t.Errorf("audit event does not name the dropped host: %s", ev.Data)
	}
}

// TestValidateInlineSecretRefs_SSHKey asserts an ssh_key grant referencing an
// unknown key secret is rejected at create time (422), and a present one passes.
func TestValidateInlineSecretRefs_SSHKey(t *testing.T) {
	h, _ := newSecretsHarness(t) // memSecrets seeded with "anthropic-api-key"
	ctx := context.Background()

	present := sshKeyPolicy("github.com", "anthropic-api-key") // reuse the seeded name
	if code, err := h.srv.validateInlineSecretRefs(ctx, present); err != nil || code != 0 {
		t.Fatalf("present ssh_key secret: code=%d err=%v, want (0,nil)", code, err)
	}

	missing := sshKeyPolicy("github.com", "no-such-key")
	if code, err := h.srv.validateInlineSecretRefs(ctx, missing); err == nil || code != http.StatusUnprocessableEntity {
		t.Fatalf("missing ssh_key secret: code=%d err=%v, want (422,err)", code, err)
	}
}

// grantsStore serves ListGrantsByRun from a fixed slice; every other method
// panics if reached, which is the point — the mint guard must read nothing else.
// (Embedding convention: notFoundStore, scanRunStore.)
type grantsStore struct {
	store.Store
	grants []types.CredentialGrant
}

func (s grantsStore) ListGrantsByRun(context.Context, uuid.UUID) ([]types.CredentialGrant, error) {
	return s.grants, nil
}

// TestInternalMint_BrokeredForgeRefusesSSHKey is the MINT-time half of the
// single-lane rule (policy-write: TestValidateGrantLaneExclusivity; dispatch:
// TestDispatch_BrokeredSSHDropIsAuditedAndNeverReachesTheSandbox). It covers the
// residual a policy STORED BEFORE that rule leaves behind: the ssh_key grant row
// still exists, so a caller holding its id could still ask the control plane to
// mint it. The refusal must land BEFORE the broker — that is what proves no
// transaction opened and no approval-gated grant's single-use slot was consumed.
func TestInternalMint_BrokeredForgeRefusesSSHKey(t *testing.T) {
	sshGrant := func(runID, id uuid.UUID, host string) types.CredentialGrant {
		return types.CredentialGrant{ID: id, RunID: runID, Spec: types.GrantSpec{
			Kind:  types.GrantSSHKey,
			Scope: mustJSON(map[string]any{"host": host, "key_secret_ref": "deploy-key"}),
		}}
	}
	ghGrant := func(runID uuid.UUID) types.CredentialGrant {
		return types.CredentialGrant{ID: uuid.New(), RunID: runID, Spec: types.GrantSpec{
			Kind:  types.GrantGitHubToken,
			Scope: mustJSON(map[string]any{"repos": []string{"acme/widgets"}}),
		}}
	}

	cases := []struct {
		name     string
		host     string
		withGH   bool
		noStore  bool
		wantCode int
	}{
		{"github ssh_key co-granted with a github_token is refused", "github.com", true, false, http.StatusForbidden},
		{"an ssh_key for another forge is untouched by a github_token", "dev.azure.com", true, false, http.StatusOK},
		{"a github ssh_key with no github_token is the unbrokered lane", "github.com", false, false, http.StatusOK},
		{"no Store fails OPEN — defence in depth, not a gate", "github.com", true, true, http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			runID, sshID := uuid.New(), uuid.New()
			grants := []types.CredentialGrant{sshGrant(runID, sshID, tc.host)}
			if tc.withGH {
				grants = append(grants, ghGrant(runID))
			}
			if !tc.noStore {
				h.srv.cfg.Store = grantsStore{grants: grants}
			}
			tok := h.mintRunToken(t, runID)
			w := do(t, h.srv, http.MethodPost, "/api/v1/internal/credentials/mint", tok,
				`{"grant_id":"`+sshID.String()+`"}`)
			if w.Code != tc.wantCode {
				t.Fatalf("mint code = %d, want %d; body=%s", w.Code, tc.wantCode, w.Body.String())
			}
			if tc.wantCode != http.StatusForbidden {
				if h.broker.lastCall == nil {
					t.Fatalf("the mint never reached the broker — the guard over-refused")
				}
				return
			}
			if h.broker.lastCall != nil {
				t.Errorf("a REFUSED mint still called the broker: an approval-gated grant's single-use slot could have been burned")
			}
			ev := findAudit(h.audit.events, runID, "credential.mint", "denied")
			if ev == nil {
				t.Fatalf("the refusal was SILENT: no credential.mint/denied event; events=%s", auditDump(h.audit.events, runID))
			}
			if ev.ActorType != types.ActorAgent || ev.Actor == "" {
				t.Errorf("audit attribution = %s/%q, want agent + the caller's SPIFFE id", ev.ActorType, ev.Actor)
			}
			if !strings.Contains(string(ev.Data), tc.host) || !strings.Contains(string(ev.Data), sshID.String()) {
				t.Errorf("audit event names neither the host nor the grant: %s", ev.Data)
			}
			if !strings.Contains(w.Body.String(), "github_token") {
				t.Errorf("refusal body does not name the reason/remedy: %s", w.Body.String())
			}
		})
	}
}
