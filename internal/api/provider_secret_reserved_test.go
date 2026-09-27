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

// providerSecretNames is every per-person model-provider credential name an
// env_secret or llm_inspection grant must never read: the three suffixes that
// exist, plus one that does not yet, since the reservation is by prefix (#1035).
var providerSecretNames = []string{
	providerSecretPrefix + "0c1f7a2e-key",
	providerSecretPrefix + "0c1f7a2e-oauth",
	providerSecretPrefix + "0c1f7a2e-sso",
	providerSecretPrefix + "0c1f7a2e-future",
}

func envSecretInline(secretName string) string {
	return `"min_confinement_class":"CC1","eligible_grants":[{"kind":"env_secret","scope":{"name":"CORP_TOKEN","secret_name":"` + secretName + `"}}]`
}

func llmInspectionInline(secretName string) string {
	return `"min_confinement_class":"CC1","llm_inspection":{"mode":"alert","detect_secrets":true,"workspace_secret_names":["` + secretName + `"]}`
}

// gitPATInline and sshKeyInline are #1048's added lanes: both return the raw
// secret VALUE into the sandbox (unlike env_secret's proxy-side env-var
// delivery and llm_inspection's proxy-side scan copy, both return the same
// way), so both must refuse a wardyn-provider-*-key name at write time too —
// they used to check only the narrower sinkReservedSecret, which does not
// cover a provider -key name (the provider arm legitimately names one at the
// api_key sink, so that guard stays narrow there on purpose).
func gitPATInline(secretName string) string {
	return `"min_confinement_class":"CC1","eligible_grants":[{"kind":"git_pat","scope":{"host":"github.com","secret_name":"` + secretName + `"}}]`
}

func sshKeyInline(secretName string) string {
	return `"min_confinement_class":"CC1","eligible_grants":[{"kind":"ssh_key","scope":{"host":"github.com","key_secret_ref":"` + secretName + `"}}]`
}

// TestProviderSecretNames_RefusedAtWrite: each suffix, every lane, both doors a
// run policy is written through — create (POST /runs) and Review (POST
// /runs/preflight) — and an ordinary name still passes both.
func TestProviderSecretNames_RefusedAtWrite(t *testing.T) {
	h := newHarness(t)
	h.srv.cfg.Store = createRunUnconfiguredStore{}
	// git_pat/ssh_key (unlike env_secret/llm_inspection) refuse write with 422
	// "requires a secret store" the moment ANY grant needs one at all
	// (validateInlineSecretRefs) — before this even reaches the ordinary-name
	// case below, so a secrets store with the ordinary name already Put is
	// needed to exercise their write-time path past that gate.
	h.srv.cfg.Secrets = &memSecrets{m: map[string][]byte{"corp-api-token": []byte("ordinary-value-0123456789")}}
	h.srv.router = h.srv.routes()

	lanes := map[string]func(string) string{
		"env_secret":     envSecretInline,
		"llm_inspection": llmInspectionInline,
		"git_pat":        gitPATInline,
		"ssh_key":        sshKeyInline,
	}
	// ssh_key's write-time refusal deliberately does not name which of
	// key_secret_ref/known_hosts_secret_ref was reserved (policy.go), unlike
	// every other lane here — so its body assertion below is 400 alone.
	namesTheOffender := map[string]bool{"env_secret": true, "llm_inspection": true, "git_pat": true, "ssh_key": false}
	doors := map[string]string{"create": "/api/v1/runs", "review": "/api/v1/runs/preflight"}
	body := func(inline string) string {
		return `{"agent":"claude-code","repo":"ephemeral","task":"echo hi","task_mode":"exec","inline_policy":{` + inline + `}}`
	}
	for lane, inline := range lanes {
		for door, path := range doors {
			for _, name := range providerSecretNames {
				w := do(t, h.srv, http.MethodPost, path, adminToken, body(inline(name)))
				if w.Code != http.StatusBadRequest || (namesTheOffender[lane] && !strings.Contains(w.Body.String(), name)) {
					t.Errorf("%s via %s naming %q: code=%d body=%s, want a 400", lane, door, name, w.Code, w.Body.String())
				}
			}
			// The ordinary name gets past validation: Review answers 200, and
			// create reaches CreateRun (createRunUnconfiguredStore's 500 sentinel).
			w := do(t, h.srv, http.MethodPost, path, adminToken, body(inline("corp-api-token")))
			want := map[string]int{"create": http.StatusInternalServerError, "review": http.StatusOK}[door]
			if w.Code != want {
				t.Errorf("%s via %s naming an ordinary secret: code=%d body=%s, want %d", lane, door, w.Code, w.Body.String(), want)
			}
		}
	}
}

// TestProviderSecretNames_RefusedAtDispatch is the defence-in-depth half: a
// spec that never met the write-time validators (a stored row written before
// it, or WARDYN_DEFAULT_POLICY) still resolves no provider name — neither the
// member's own row nor the operator's, which the owner-then-operator read
// would otherwise fall back to.
func TestProviderSecretNames_RefusedAtDispatch(t *testing.T) {
	const member = "sub-member@corp.example"
	sec := &memSecrets{m: map[string][]byte{"corp-api-token": []byte("ordinary-value-0123456789")}, owned: map[string]map[string][]byte{}}
	// The member holds only their own -key row; every other name falls back to
	// the operator's row, which is the read this reservation must also stop.
	for _, name := range providerSecretNames {
		sec.m[name] = []byte("OPERATOR-MODEL-KEY-" + name)
	}
	if err := sec.For(member).Put(context.Background(), providerSecretNames[0], []byte("MEMBER-MODEL-KEY")); err != nil {
		t.Fatal(err)
	}
	h := newHarness(t)
	h.srv.cfg.Secrets = sec
	run := types.AgentRun{ID: uuid.New(), CreatedBy: member}

	var grants []types.GrantSpec
	for i, name := range providerSecretNames {
		grants = append(grants, envSecretGrant("PROVIDER_"+string(rune('A'+i)), name))
	}
	grants = append(grants, envSecretGrant("CORP_TOKEN", "corp-api-token"))
	env := map[string]string{}
	resolved := h.srv.resolveEnvSecretGrants(context.Background(), run, types.RunPolicySpec{EligibleGrants: grants}, env)
	if len(resolved) != 1 || resolved[0] != "CORP_TOKEN" || len(env) != 1 || env["CORP_TOKEN"] != "ordinary-value-0123456789" {
		t.Fatalf("env_secret resolved %v into env %v, want only CORP_TOKEN", resolved, env)
	}
	refused := 0
	for _, ev := range h.audit.events {
		if ev.Action == "run.env_secret.resolve" && ev.Outcome == "failure" &&
			strings.Contains(string(ev.Data), "reserved platform-internal secret name") {
			refused++
		}
	}
	if refused != len(providerSecretNames) {
		t.Errorf("audited reserved-name refusals = %d, want %d", refused, len(providerSecretNames))
	}

	spec := &types.RunPolicySpec{LLMInspection: &types.LLMInspectionSpec{
		Mode: "alert", DetectSecrets: true, WorkspaceSecretNames: append(append([]string(nil), providerSecretNames...), "corp-api-token"),
	}}
	h.srv.resolveLLMInspectionSecrets(context.Background(), run, spec)
	if got := spec.LLMInspection.WorkspaceSecretValues; len(got) != 1 || got[0] != "ordinary-value-0123456789" {
		t.Fatalf("llm_inspection resolved %v, want only the ordinary secret's value", got)
	}
}

// TestSecretNameFormat_RefusedAtWrite (#1048): env_secret's secret_name and
// llm_inspection's workspace_secret_names never went through secretNameRE, so
// an impossible name passed write time and just covered one fewer value at
// dispatch — silently, with no reason surfaced to the author. Both now refuse
// 400 at write, naming the bad value, for the same four malformed shapes
// secretNameRE (`^[a-z0-9]([a-z0-9._-]{0,126}[a-z0-9])?$`) was always meant to
// exclude: upper-case, a leading space, a unicode hyphen (not ASCII `-`), and a
// path.
func TestSecretNameFormat_RefusedAtWrite(t *testing.T) {
	h := newHarness(t)
	h.srv.cfg.Store = createRunUnconfiguredStore{}
	h.srv.router = h.srv.routes()

	badNames := []string{"UPPER-CASE", " leading-space", "a‐b", "a/b/c"}
	lanes := map[string]func(string) string{"env_secret": envSecretInline, "llm_inspection": llmInspectionInline}
	doors := map[string]string{"create": "/api/v1/runs", "review": "/api/v1/runs/preflight"}
	body := func(inline string) string {
		return `{"agent":"claude-code","repo":"ephemeral","task":"echo hi","task_mode":"exec","inline_policy":{` + inline + `}}`
	}
	for lane, inline := range lanes {
		for door, path := range doors {
			for _, name := range badNames {
				w := do(t, h.srv, http.MethodPost, path, adminToken, body(inline(name)))
				if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "not a valid secret name") {
					t.Errorf("%s via %s naming %q: code=%d body=%s, want a 400 refusing the format", lane, door, name, w.Code, w.Body.String())
				}
			}
		}
	}
}

// TestSecretRefsOf_RefusesProviderKeyDirectly pins inline_policy.go's
// secretRefsOf guard (git_pat/ssh_key -> nameSinkReservedSecret) ON ITS OWN,
// bypassing every HTTP door and policy.go's validatePolicySpec — the review
// round for PR #1248 (finding F6) found that guard was, in every real call
// path, unreachable-as-primary-defense: validatePolicySpec is the single
// chokepoint every door (POST /policies, POST /runs, POST /runs/preflight,
// presets, profiles, governance ceilings, LoadPolicySpec at boot) reaches
// FIRST, so reverting only inline_policy.go's guard left the HTTP-level test
// suite green. That makes it defense in depth, not dead code: a future call
// path that reaches secretRefsOf before validatePolicySpec (or bypasses it
// entirely) would still be caught here. Calling the method directly, with no
// HTTP layer in between, is what actually exercises that on its own.
func TestSecretRefsOf_RefusesProviderKeyDirectly(t *testing.T) {
	s := &Server{}
	for _, tc := range []struct {
		name       string
		spec       types.RunPolicySpec
		errNamesIt bool // ssh_key's message deliberately doesn't name the offender (policy.go)
	}{
		{"git_pat", types.RunPolicySpec{EligibleGrants: []types.GrantSpec{{
			Kind:  types.GrantGitPAT,
			Scope: json.RawMessage(`{"host":"github.com","secret_name":"` + providerSecretNames[0] + `"}`),
		}}}, true},
		{"ssh_key", types.RunPolicySpec{EligibleGrants: []types.GrantSpec{{
			Kind:  types.GrantSSHKey,
			Scope: json.RawMessage(`{"host":"github.com","key_secret_ref":"` + providerSecretNames[0] + `"}`),
		}}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := func() error { _, err := s.secretRefsOf(tc.spec); return err }()
			if err == nil {
				t.Fatalf("secretRefsOf(%s naming %q) = nil, want an error", tc.name, providerSecretNames[0])
			}
			if tc.errNamesIt && !strings.Contains(err.Error(), providerSecretNames[0]) {
				t.Fatalf("secretRefsOf(%s naming %q) = %v, want an error naming it", tc.name, providerSecretNames[0], err)
			}
		})
	}
}
