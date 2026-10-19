// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// grantRecStore records the grants dispatch authors.
type grantRecStore struct {
	*dispatchTestStore
	mu      sync.Mutex
	created []types.CredentialGrant
}

func (s *grantRecStore) CreateGrant(_ context.Context, g types.CredentialGrant) (types.CredentialGrant, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.created = append(s.created, g)
	return g, nil
}

const placeholderHost = "api.corp.test"

// hostScoped is g with the scope's optional host set.
func hostScoped(t *testing.T, g types.GrantSpec) types.GrantSpec {
	t.Helper()
	var sc map[string]any
	if err := json.Unmarshal(g.Scope, &sc); err != nil {
		t.Fatal(err)
	}
	sc["host"] = placeholderHost
	g.Scope = mustJSON(sc)
	return g
}

// placeholderDispatch dispatches policy with both placeholder switches set to
// on, returning the sandbox spec and the grants dispatch authored.
func placeholderDispatch(t *testing.T, on bool, grants ...types.GrantSpec) (runner.SandboxSpec, []types.CredentialGrant, *recRecorder) {
	t.Helper()
	rn := &fakeRunner{}
	srv, st, audit, run := dispatchTeardownFixture(t, rn, types.RunPending)
	run.Task, run.CreatedBy = "", fileOwner
	st.run = run
	rec := &grantRecStore{dispatchTestStore: st}
	_, sec, _ := fileSecretServer(t)
	srv.cfg.Store, srv.cfg.Secrets, srv.cfg.MaskRegistry = rec, sec, secretmask.NewRegistry()
	srv.cfg.EnvSecretPlaceholders, srv.cfg.FileSecretPlaceholders = on, on
	dispatchOf(srv, run, types.RunPolicySpec{AllowedDomains: []string{placeholderHost}, EligibleGrants: grants})
	if rn.createCalls != 1 {
		t.Fatalf("the sandbox was not created; events: %s", auditDump(audit.events, run.ID))
	}
	return rn.lastSpec, rec.created, audit
}

func agentFile(spec runner.SandboxSpec, file string) string {
	for _, f := range spec.ManagedFiles {
		if f.Path == runner.ComponentSecretDir+"/"+file {
			return string(f.Content)
		}
	}
	return "<absent>"
}

// Off, a host on the scope changes nothing: the sandbox gets the values, the
// proxy no rule and no interception, exactly as a scope without one.
func TestSecretPlaceholders_OffDeliversTheValueAsBefore(t *testing.T) {
	env, file := envSecretGrant("CORP_TOKEN", "corp-token"), fileSecretGrant("api-token", "deploy-token", true)
	plain, _, _ := placeholderDispatch(t, false, env, file)
	hosted, created, _ := placeholderDispatch(t, false, hostScoped(t, env), hostScoped(t, file))
	if plain.SecretEnv["CORP_TOKEN"] != fileOperValue || agentFile(plain, "api-token") != fileOwnValue {
		t.Fatalf("baseline: CORP_TOKEN %q, api-token %q; want the values", plain.SecretEnv["CORP_TOKEN"], agentFile(plain, "api-token"))
	}
	if hosted.SecretEnv["CORP_TOKEN"] != plain.SecretEnv["CORP_TOKEN"] || agentFile(hosted, "api-token") != agentFile(plain, "api-token") {
		t.Errorf("off with a host: CORP_TOKEN %q, api-token %q; want the baseline's", hosted.SecretEnv["CORP_TOKEN"], agentFile(hosted, "api-token"))
	}
	if len(created) != 0 || len(hosted.ProxyConfig.Injection) != len(plain.ProxyConfig.Injection) ||
		!slices.Equal(hosted.ProxyConfig.MITMHosts, plain.ProxyConfig.MITMHosts) || hosted.ProxyConfig.MITMCACertPEM != "" {
		t.Errorf("off with a host authored %d grants, injections %+v, MITM %v; want none", len(created), hosted.ProxyConfig.Injection, hosted.ProxyConfig.MITMHosts)
	}
}

// On, each host-scoped value is replaced by a placeholder that the run's
// proxy fills on requests to the host, and the value is in neither the env
// nor the file; a grant with no host is delivered as before.
func TestSecretPlaceholders_OnKeepsTheValueAtTheProxy(t *testing.T) {
	spec, created, audit := placeholderDispatch(t, true,
		hostScoped(t, envSecretGrant("CORP_TOKEN", "operator-only")),
		hostScoped(t, fileSecretGrant("api-token", "deploy-token", true)),
		envSecretGrant("PLAIN_TOKEN", "corp-token"),
		hostScoped(t, envSecretGrant("MULTI_TOKEN", "corp-token")))
	if spec.SecretEnv["MULTI_TOKEN"] != fileOperValue {
		t.Errorf("MULTI_TOKEN = %q, want the value: one with a line break cannot ride a header", spec.SecretEnv["MULTI_TOKEN"])
	}
	if spec.SecretEnv["PLAIN_TOKEN"] != fileOperValue {
		t.Errorf("PLAIN_TOKEN = %q, want the value: a grant with no host is untouched", spec.SecretEnv["PLAIN_TOKEN"])
	}
	// The env grant is placed first and takes the host; the file grant finds
	// it carrying a credential and is delivered as before (one per host).
	ph := spec.SecretEnv["CORP_TOKEN"]
	if !strings.HasPrefix(ph, secretPlaceholderPrefix) {
		t.Fatalf("CORP_TOKEN = %q, want a placeholder", ph)
	}
	if got := agentFile(spec, "api-token"); got != fileOwnValue {
		t.Errorf("api-token = %q, want the value: its host already carries the env grant's credential", got)
	}
	i := slices.IndexFunc(spec.ProxyConfig.Injection, func(in runner.InjectionGrant) bool { return in.Rule.Placeholder == ph })
	if i < 0 {
		t.Fatalf("no injection fills %q: %+v", ph, spec.ProxyConfig.Injection)
	}
	in := spec.ProxyConfig.Injection[i]
	if in.Rule.Host != placeholderHost || in.Rule.SecretName != "operator-only" || in.Rule.Format != "%s" || !in.Rule.RequireTLS {
		t.Errorf("rule = %+v, want the host, the secret, the raw value and TLS only", in.Rule)
	}
	if !slices.Contains(spec.ProxyConfig.MITMHosts, placeholderHost+":443") || spec.ProxyConfig.MITMCACertPEM == "" {
		t.Errorf("MITM hosts %v (CA set %v), want the host intercepted", spec.ProxyConfig.MITMHosts, spec.ProxyConfig.MITMCACertPEM != "")
	}
	if len(created) != 1 || created[0].ID != in.GrantID || created[0].Spec.Kind != types.GrantAPIKey || created[0].Spec.OwnerOnly {
		t.Errorf("authored grants %+v, want the one api_key grant, read from the env grant's namespace", created)
	}
	for _, v := range spec.Env {
		if strings.Contains(v, fileOtherValue) {
			t.Error("Env carries the placed value")
		}
	}
	var outcomes []string
	for _, ev := range audit.events {
		if ev.Action == "run.secret.placeholder" {
			outcomes = append(outcomes, ev.Outcome)
		}
	}
	if !slices.Equal(outcomes, []string{"success", "failure", "failure"}) {
		t.Errorf("run.secret.placeholder outcomes %v, want CORP_TOKEN placed and the other two fallbacks recorded", outcomes)
	}
}

// The file arm on its own host: the file holds the placeholder.
func TestSecretPlaceholders_OnPlacesAFile(t *testing.T) {
	spec, created, _ := placeholderDispatch(t, true, hostScoped(t, fileSecretGrant("api-token", "deploy-token", true)))
	ph := agentFile(spec, "api-token")
	if !strings.HasPrefix(ph, secretPlaceholderPrefix) || len(created) != 1 || !created[0].Spec.OwnerOnly {
		t.Fatalf("api-token = %q with grants %+v; want a placeholder filled from the owner's own row", ph, created)
	}
	if !slices.ContainsFunc(spec.ProxyConfig.Injection, func(in runner.InjectionGrant) bool { return in.Rule.Placeholder == ph }) ||
		!slices.Contains(spec.ProxyConfig.MITMHosts, placeholderHost+":443") {
		t.Errorf("injections %+v, MITM %v; want the file's placeholder filled on the host", spec.ProxyConfig.Injection, spec.ProxyConfig.MITMHosts)
	}
}

// An authored api_key scope may not set placeholder, and a secret's host
// must be a bare host.
func TestSecretPlaceholders_WriteTimeRules(t *testing.T) {
	for name, g := range map[string]types.GrantSpec{
		"api_key placeholder": {Kind: types.GrantAPIKey, Scope: mustJSON(map[string]any{"host": "a.test", "secret_name": "k", "placeholder": "x"})},
		"env host with port":  {Kind: types.GrantEnvSecret, Scope: mustJSON(map[string]any{"name": "T", "secret_name": "k", "host": "a.test:8443"})},
		"file host wildcard":  {Kind: types.GrantFileSecret, Scope: mustJSON(map[string]any{"file": "t", "secret_name": "k", "host": "*.a.test"})},
	} {
		if err := validateEligibleGrantMode(0, g, true); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	for name, g := range map[string]types.GrantSpec{
		"env host":  {Kind: types.GrantEnvSecret, Scope: mustJSON(map[string]any{"name": "T", "secret_name": "k", "host": "a.test"})},
		"file host": {Kind: types.GrantFileSecret, Scope: mustJSON(map[string]any{"file": "t", "secret_name": "k", "host": "a.test"})},
	} {
		if err := validateEligibleGrantMode(0, g, true); err != nil {
			t.Errorf("%s: refused: %v", name, err)
		}
	}
}
