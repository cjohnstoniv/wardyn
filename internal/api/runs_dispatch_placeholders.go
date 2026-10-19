// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// secretPlaceholderPrefix starts every env or file secret placeholder; the
// rest is the id of the injection grant that fills it.
const secretPlaceholderPrefix = "wardyn-placeholder:"

// placeholdersWanted reports whether dispatch may author a placeholder for
// this policy: a switch is on and a grant of its kind names a host. It is
// asked before the run's interception CA is decided, since the proxy can fill
// a placeholder only inside a connection it terminates.
func (s *Server) placeholdersWanted(policy types.RunPolicySpec) bool {
	return slices.ContainsFunc(policy.EligibleGrants, func(g types.GrantSpec) bool {
		if !(g.Kind == types.GrantEnvSecret && s.cfg.EnvSecretPlaceholders) && !(g.Kind == types.GrantFileSecret && s.cfg.FileSecretPlaceholders) {
			return false
		}
		host, err := secretScopeHost(g.Scope)
		return err == nil && host != ""
	})
}

// authorSecretPlaceholder keeps a delivered env or file secret's value out of
// the sandbox: it authors an injection grant for the scope's host whose rule
// fills a placeholder with the value, and returns that placeholder, the grant
// and the host to intercept. An empty placeholder means the value goes in as
// before, and why says why. Narrowest: the host must already be an exact
// allowlist entry with no other credential, and the value one line.
func (s *Server) authorSecretPlaceholder(ctx context.Context, run types.AgentRun, g types.GrantSpec, host, secretName, value string,
	allowed func(string) bool, taken []runner.InjectionGrant) (ph string, inj runner.InjectionGrant, mitm, why string) {
	switch {
	case strings.ContainsAny(value, "\r\n"):
		return "", inj, "", "the value spans more than one line, so it cannot ride a request header"
	case !allowed(host):
		return "", inj, "", "the host is not an exact entry of the run's allowlist"
	case slices.ContainsFunc(taken, func(in runner.InjectionGrant) bool { return hostEqual(in.Rule.Host, host) }):
		return "", inj, "", "the host already carries another credential"
	}
	grantID := uuid.New()
	ph = secretPlaceholderPrefix + grantID.String()
	scope, _ := json.Marshal(map[string]any{
		"host": host, "secret_name": secretName, "format": "%s", "require_tls": true, "placeholder": ph,
	})
	if _, err := s.createDispatchGrant(ctx, run, types.CredentialGrant{
		ID: grantID, RunID: run.ID, CreatedAt: s.cfg.Now().UTC(),
		Spec: types.GrantSpec{Kind: types.GrantAPIKey, Scope: scope, TTLSeconds: 3600, OwnerOnly: g.OwnerOnly},
	}, !g.OwnerOnly); err != nil {
		return "", inj, "", "its injection grant could not be recorded"
	}
	rule, err := injectionRuleFromScope(scope)
	if err != nil {
		return "", inj, "", "its injection rule did not decode"
	}
	return ph, runner.InjectionGrant{GrantID: grantID, Rule: rule}, net.JoinHostPort(host, "443"), ""
}

// recordPlaceholder writes run.secret.placeholder for one grant with a host.
func (s *Server) recordPlaceholder(ctx context.Context, run types.AgentRun, kind types.GrantKind, secretName, host, why string) {
	data := map[string]any{"kind": kind, "secret_name": secretName, "host": host}
	outcome := "success"
	if why != "" {
		outcome, data["reason"] = "failure", why
	}
	s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.secret.placeholder",
		run.ID.String(), outcome, mustJSON(data)))
}

// placeEnvSecrets is WARDYN_ENV_SECRET_PLACEHOLDERS: each env_secret grant
// with a host whose value resolveEnvSecretValues set (keys) gets a
// placeholder in its variable instead. A runner-resident grant keeps the
// runner's own placeholder. Returns the hosts to intercept.
func (s *Server) placeEnvSecrets(ctx context.Context, run types.AgentRun, policy types.RunPolicySpec, rows []types.CredentialGrant,
	keys []string, sandboxEnv map[string]string, allowed func(string) bool, injections *[]runner.InjectionGrant) []string {
	var mitm []string
	seen := map[string]bool{}
	for _, g := range policy.EligibleGrants {
		if g.Kind != types.GrantEnvSecret {
			continue
		}
		name, secretName, err := envSecretScopeFields(g.Scope)
		if err != nil || seen[name] {
			continue
		}
		seen[name] = true // the first grant naming a variable is the one that set it
		host, herr := secretScopeHost(g.Scope)
		if _, resident := residentGrantRow(rows, g); herr != nil || host == "" || resident || !slices.Contains(keys, name) {
			continue
		}
		ph, inj, h, why := s.authorSecretPlaceholder(ctx, run, g, host, secretName, sandboxEnv[name], allowed, *injections)
		if ph != "" {
			sandboxEnv[name] = ph
			*injections = append(*injections, inj)
			mitm = append(mitm, h)
		}
		s.recordPlaceholder(ctx, run, g.Kind, secretName, host, why)
	}
	return mitm
}

// placeFileSecrets is WARDYN_FILE_SECRET_PLACEHOLDERS: placeEnvSecrets for
// the file_secret grants whose file resolveFileSecretValues delivered.
func (s *Server) placeFileSecrets(ctx context.Context, run types.AgentRun, policy types.RunPolicySpec, rows []types.CredentialGrant,
	files []runner.ManagedFile, spec *runner.SandboxSpec) {
	allowed := proxy.CompilePolicy(spec.ProxyConfig.Policy).AllowedExactHost
	for _, g := range policy.EligibleGrants {
		if g.Kind != types.GrantFileSecret {
			continue
		}
		file, secretName, err := fileSecretScopeFields(g.Scope)
		host, herr := secretScopeHost(g.Scope)
		if _, resident := residentGrantRow(rows, g); err != nil || herr != nil || host == "" || resident {
			continue
		}
		i := slices.IndexFunc(files, func(f runner.ManagedFile) bool { return f.Path == runner.ComponentSecretDir+"/"+file })
		if i < 0 {
			continue
		}
		ph, inj, h, why := s.authorSecretPlaceholder(ctx, run, g, host, secretName, string(files[i].Content), allowed, spec.ProxyConfig.Injection)
		if ph != "" {
			files[i].Content = []byte(ph)
			spec.ProxyConfig.Injection = append(spec.ProxyConfig.Injection, inj)
			spec.ProxyConfig.MITMHosts = append(spec.ProxyConfig.MITMHosts, h)
		}
		s.recordPlaceholder(ctx, run, g.Kind, secretName, host, why)
	}
}
