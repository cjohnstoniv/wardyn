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
	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// secretPlaceholderPrefix starts every env or file secret placeholder; the
// rest is the id of the injection grant that fills it.
const secretPlaceholderPrefix = placement.FillPlaceholderPrefix

// placeholdersWanted reports whether dispatch may author a placeholder for
// this policy: a switch is on (the runner one only for a local run) and a
// grant of its kind names a host. It is
// asked before the run's interception CA is decided, since the proxy can fill
// a placeholder only inside a connection it terminates.
func (s *Server) placeholdersWanted(run types.AgentRun, policy types.RunPolicySpec) bool {
	onRunner := s.cfg.RunnerSecretPlaceholders && run.Placement == types.PlacementLocal
	return slices.ContainsFunc(policy.EligibleGrants, func(g types.GrantSpec) bool {
		if !(g.Kind == types.GrantEnvSecret && (s.cfg.EnvSecretPlaceholders || onRunner)) && !(g.Kind == types.GrantFileSecret && (s.cfg.FileSecretPlaceholders || onRunner)) {
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
// allowlist entry with no other credential, and the value one line. resident
// is a runner_resident grant's fill (placeResidentEnvSecrets): its grant is
// recorded via_org and marked shared, so the organisation's egress reads the
// operator's row, the one readResidentValue read, and the runner gets the
// placeholder and a via_org route only.
func (s *Server) authorSecretPlaceholder(ctx context.Context, run types.AgentRun, g types.GrantSpec, host, secretName, value string,
	allowed func(string) bool, taken []runner.InjectionGrant, resident bool) (ph string, inj runner.InjectionGrant, mitm, why string) {
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
	fillScope := map[string]any{"host": host, "secret_name": secretName, "format": "%s", "require_tls": true, "placeholder": ph}
	if resident {
		fillScope["shared"] = true
	}
	scope, _ := json.Marshal(fillScope)
	grant := types.CredentialGrant{
		ID: grantID, RunID: run.ID, CreatedAt: s.cfg.Now().UTC(),
		Spec: types.GrantSpec{Kind: types.GrantAPIKey, Scope: scope, TTLSeconds: 3600, OwnerOnly: g.OwnerOnly},
	}
	var err error
	if resident {
		grant.Delivery = types.GrantDeliveryViaOrg
		_, err = s.cfg.Store.CreateGrant(ctx, grant)
	} else {
		_, err = s.createDispatchGrant(ctx, run, grant, !g.OwnerOnly)
	}
	if err != nil {
		return "", inj, "", "its injection grant could not be recorded"
	}
	rule, err := injectionRuleFromScope(scope)
	if err != nil {
		return "", inj, "", "its injection rule did not decode"
	}
	return ph, runner.InjectionGrant{GrantID: grantID, Rule: rule}, net.JoinHostPort(host, "443"), ""
}

// recordPlaceholder writes run.secret_placeholder.resolve for one grant with a
// host; a runner_resident grant's row says delivery via_org.
func (s *Server) recordPlaceholder(ctx context.Context, run types.AgentRun, kind types.GrantKind, secretName, host, why string, resident bool) {
	data := map[string]any{"kind": kind, "secret_name": secretName, "host": host}
	if resident {
		data["delivery"] = types.GrantDeliveryViaOrg
	}
	outcome := "success"
	if why != "" {
		outcome, data["reason"] = "failure", why
	}
	s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.secret_placeholder.resolve",
		run.ID.String(), outcome, mustJSON(data)))
}

// placeEnvSecrets is WARDYN_ENV_SECRET_PLACEHOLDERS: each env_secret grant
// with a host whose value resolveEnvSecretValues set (keys) gets a
// placeholder in its variable instead. A runner-resident grant keeps the
// runner's own placeholder. Returns the hosts to intercept; nil, and nothing
// touched, with the switch off.
func (s *Server) placeEnvSecrets(ctx context.Context, run types.AgentRun, policy types.RunPolicySpec, p dispatchParams, rows []types.CredentialGrant,
	keys []string, sandboxEnv map[string]string, injections *[]runner.InjectionGrant) []string {
	if !s.cfg.EnvSecretPlaceholders {
		return nil
	}
	// The gate buildInjector applies, so an authored rule never fails the proxy's boot.
	allowed := proxy.CompilePolicy(sidecarPolicy(policy, p)).AllowedExactHost
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
		ph, inj, h, why := s.authorSecretPlaceholder(ctx, run, g, host, secretName, sandboxEnv[name], allowed, *injections, false)
		if ph != "" {
			sandboxEnv[name] = ph
			*injections = append(*injections, inj)
			mitm = append(mitm, h)
		}
		s.recordPlaceholder(ctx, run, g.Kind, secretName, host, why, false)
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
		ph, inj, h, why := s.authorSecretPlaceholder(ctx, run, g, host, secretName, string(files[i].Content), allowed, spec.ProxyConfig.Injection, false)
		if ph != "" {
			files[i].Content = []byte(ph)
			spec.ProxyConfig.Injection = append(spec.ProxyConfig.Injection, inj)
			spec.ProxyConfig.MITMHosts = append(spec.ProxyConfig.MITMHosts, h)
		}
		s.recordPlaceholder(ctx, run, g.Kind, secretName, host, why, false)
	}
}

// placeResidentEnvSecrets is WARDYN_RUNNER_SECRET_PLACEHOLDERS for env_secret:
// each runner_resident value resolveEnvSecretValues read whose grant names a
// host gets a fill placeholder in its variable instead, and leaves resident,
// so the runner never receives it. Returns resident without those values and
// the hosts to intercept; resident as it came, and nothing touched, with the
// switch off or on a remote run.
func (s *Server) placeResidentEnvSecrets(ctx context.Context, run types.AgentRun, policy types.RunPolicySpec, p dispatchParams,
	sandboxEnv map[string]string, injections *[]runner.InjectionGrant, resident []residentValue) ([]residentValue, []string) {
	if !s.cfg.RunnerSecretPlaceholders || run.Placement != types.PlacementLocal {
		return resident, nil
	}
	allowed := proxy.CompilePolicy(sidecarPolicy(policy, p)).AllowedExactHost
	var mitm []string
	kept := []residentValue{}
	for _, v := range resident {
		name, secretName, err := envSecretScopeFields(v.grant.Spec.Scope)
		if v.class != placement.ClassEnvSecret || err != nil {
			kept = append(kept, v)
			continue
		}
		ph, inj, h := s.fillResident(ctx, run, v, secretName, allowed, *injections)
		if ph == "" {
			kept = append(kept, v)
			continue
		}
		sandboxEnv[name] = ph
		*injections = append(*injections, inj)
		mitm = append(mitm, h)
	}
	return kept, mitm
}

// placeResidentFileSecrets is placeResidentEnvSecrets for the runner_resident
// file_secret values resolveFileSecretValues read: the file holds the fill
// placeholder. Returns resident without those values.
func (s *Server) placeResidentFileSecrets(ctx context.Context, run types.AgentRun, files []runner.ManagedFile, spec *runner.SandboxSpec,
	resident []residentValue) []residentValue {
	if !s.cfg.RunnerSecretPlaceholders || run.Placement != types.PlacementLocal {
		return resident
	}
	allowed := proxy.CompilePolicy(spec.ProxyConfig.Policy).AllowedExactHost
	kept := []residentValue{}
	for _, v := range resident {
		file, secretName, err := fileSecretScopeFields(v.grant.Spec.Scope)
		i := slices.IndexFunc(files, func(f runner.ManagedFile) bool { return f.Path == runner.ComponentSecretDir+"/"+file })
		if v.class != placement.ClassFileSecret || err != nil || i < 0 {
			kept = append(kept, v)
			continue
		}
		ph, inj, h := s.fillResident(ctx, run, v, secretName, allowed, spec.ProxyConfig.Injection)
		if ph == "" {
			kept = append(kept, v)
			continue
		}
		files[i].Content = []byte(ph)
		spec.ProxyConfig.Injection = append(spec.ProxyConfig.Injection, inj)
		spec.ProxyConfig.MITMHosts = append(spec.ProxyConfig.MITMHosts, h)
	}
	return kept
}

// fillResident authors the via_org fill of one runner_resident value whose
// grant names a host, and clears the value, which the runner will not get. An
// empty placeholder means the value is delivered runner_resident as before:
// the grant names no host, or the audited reason.
func (s *Server) fillResident(ctx context.Context, run types.AgentRun, v residentValue, secretName string,
	allowed func(string) bool, taken []runner.InjectionGrant) (string, runner.InjectionGrant, string) {
	host, err := secretScopeHost(v.grant.Spec.Scope)
	if err != nil || host == "" {
		return "", runner.InjectionGrant{}, ""
	}
	ph, inj, mitm, why := s.authorSecretPlaceholder(ctx, run, v.grant.Spec, host, secretName, string(v.value), allowed, taken, true)
	s.recordPlaceholder(ctx, run, v.grant.Spec.Kind, secretName, host, why, true)
	if ph != "" {
		clear(v.value)
	}
	return ph, inj, mitm
}

// residentFillClass is the delivery class of the runner_resident env_secret or
// file_secret grant whose placeholder g fills via_org (placeResidentEnvSecrets):
// g is an api_key grant recorded via_org and shared, and its placeholder is
// spec's value of that grant's variable, or its file's content, with the same
// secret and host. false for any other grant.
func residentFillClass(spec runner.SandboxSpec, rows []types.CredentialGrant, g types.CredentialGrant) (string, bool) {
	if g.Spec.Kind != types.GrantAPIKey || g.Delivery != types.GrantDeliveryViaOrg || !apiKeyScopeShared(g.Spec.Scope) {
		return "", false
	}
	rule, err := injectionRuleFromScope(g.Spec.Scope)
	if err != nil || rule.Placeholder == "" {
		return "", false
	}
	for _, row := range rows {
		host, herr := secretScopeHost(row.Spec.Scope)
		if row.Delivery != types.GrantDeliveryResident || herr != nil || !hostEqual(host, rule.Host) {
			continue
		}
		switch row.Spec.Kind {
		case types.GrantEnvSecret:
			if name, secret, err := envSecretScopeFields(row.Spec.Scope); err == nil && secret == rule.SecretName && spec.SecretEnv[name] == rule.Placeholder {
				return placement.ClassEnvSecret, true
			}
		case types.GrantFileSecret:
			file, secret, err := fileSecretScopeFields(row.Spec.Scope)
			if err == nil && secret == rule.SecretName && slices.ContainsFunc(spec.ManagedFiles, func(f runner.ManagedFile) bool {
				return f.AgentOwned && f.Path == runner.ComponentSecretDir+"/"+file && string(f.Content) == rule.Placeholder
			}) {
				return placement.ClassFileSecret, true
			}
		}
	}
	return "", false
}
