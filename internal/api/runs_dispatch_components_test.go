// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Dispatch of a run's components: the interception entries, the config
// environment, the ceiling's prune and the credential-per-host re-check.

func componentRule(host string) runner.InjectionGrant {
	return runner.InjectionGrant{GrantID: uuid.New(), Rule: egress.InjectionRule{
		Host: host, Header: "Authorization", SecretName: "person-secret", Format: "Bearer %s", RequireTLS: true}}
}

// dispatchComponents runs dispatchRun for a pending run with p, and returns the
// store, the audit recorder, the runner and the run.
func dispatchComponents(t *testing.T, p dispatchParams) (*dispatchTestStore, *recRecorder, *fakeRunner, types.AgentRun) {
	t.Helper()
	fr := &fakeRunner{}
	srv, st, audit, run := dispatchTeardownFixture(t, fr, types.RunPending)
	p.RunToken, p.Image = "run-token", "wardyn/claude-code:latest"
	p.Policy.MinConfinementClass = types.CC1
	srv.dispatchRun(context.Background(), run, ceilingForDispatch(governanceCeiling{}, adoEntraUngraded(), bedrockCredUngraded()), p)
	return st, audit, fr, run
}

// Through the create door, as a member: a component's header host is
// intercepted on :443 and only there, the run gets its CA, a header an
// organisation's component delivers over plain HTTP is not intercepted, and
// the config reaches the sandbox environment without replacing a platform
// variable.
func TestDispatch_ComponentHeaderHostIsInterceptedOn443(t *testing.T) {
	f := newComponentFixture(t)
	fr := f.srv.cfg.Runner.(*fakeRunner)
	orgRef := f.org(compOrgID, types.ComponentDefinition{
		Hosts: []string{"org-plain.example"},
		Secrets: []types.ComponentSecret{{SecretName: compOperatorSecret, Shared: true,
			Delivery: types.ComponentDelivery{Mode: types.ComponentDeliveryHeader, Host: "org-plain.example", PlainHTTP: true}}},
	})
	person := map[string]any{"inline": map[string]any{
		"hosts":   []string{"person-api.example", "person-api.example:8443"},
		"secrets": []any{headerSecret(compOwnSecret, "person-api.example")},
		// GIT_AUTHOR_NAME is a variable the platform sets and the validator does
		// not reserve: the one kind of key dispatch's own drop still meets.
		"config": map[string]string{"PERSON_REGION": "person-eu", "GIT_AUTHOR_NAME": "someone-else"},
	}}
	if w := f.ask(t, componentDoors[0], componentBody(person, orgRef)); w.Code != http.StatusCreated {
		t.Fatalf("create = %d %s, want 201", w.Code, w.Body.String())
	}
	fr.waitForSandbox(t)
	fr.mu.Lock()
	spec := fr.lastSpec
	fr.mu.Unlock()
	pc := spec.ProxyConfig

	if !slices.Equal(pc.MITMHosts, []string{"person-api.example:443"}) {
		t.Errorf("MITMHosts = %v, want exactly the person's header host on :443", pc.MITMHosts)
	}
	assertNoDuplicateBareMITMHost(t, pc.MITMHosts)
	if pc.MITMCACertPEM == "" || pc.MITMCAKeyPEM == "" {
		t.Error("no per-run CA was minted: the proxy cannot terminate the header host's TLS")
	}
	if pc.MITMLLM {
		t.Error("a component's CA turned on interception of the model hosts")
	}
	rules := map[string]bool{}
	for _, in := range pc.Injection {
		rules[in.Rule.Host] = in.Rule.RequireTLS
	}
	if tls, ok := rules["person-api.example"]; !ok || !tls {
		t.Errorf("injection rules = %v, want the person's host with require_tls", rules)
	}
	if tls, ok := rules["org-plain.example"]; !ok || tls {
		t.Errorf("injection rules = %v, want the organisation's plain-HTTP host without require_tls", rules)
	}
	for _, want := range []string{"person-api.example", "person-api.example:8443", "org-plain.example"} {
		if !slices.Contains(pc.Policy.AllowedDomains, want) {
			t.Errorf("allowed domains %v lack %q", pc.Policy.AllowedDomains, want)
		}
	}
	if got := spec.Env["PERSON_REGION"]; got != "person-eu" {
		t.Errorf("Env[PERSON_REGION] = %q, want the component's config", got)
	}
	if got := spec.Env["GIT_AUTHOR_NAME"]; got == "someone-else" || got == "" {
		t.Errorf("Env[GIT_AUTHOR_NAME] = %q: a component's config replaced a platform variable", got)
	}
	for k, v := range spec.Env {
		if strings.Contains(v, "own-value") || strings.Contains(v, "operator-value") {
			t.Errorf("Env[%s] carries a header secret's value", k)
		}
	}
	// The variable the person did not get is on record, by count: the key is
	// what they typed.
	var drops []string
	for _, ev := range f.rec.snapshot() {
		if ev.Action == "run.component_config.drop" {
			drops = append(drops, string(ev.Data))
		}
	}
	if len(drops) != 1 || !strings.Contains(drops[0], `"dropped":1`) || !strings.Contains(drops[0], `"source":"inline"`) ||
		strings.Contains(drops[0], "GIT_AUTHOR_NAME") || strings.Contains(drops[0], "keys") {
		t.Errorf("run.component_config.drop rows = %v, want one counting the person's dropped variable without naming it", drops)
	}
}

// A run without components composes exactly what it did: no interception
// entry, no CA, no extra variable.
func TestDispatch_NoComponentsAddsNoInterceptionAndNoCA(t *testing.T) {
	_, _, fr, _ := dispatchComponents(t, dispatchParams{})
	if fr.createCalls != 1 {
		t.Fatalf("CreateSandbox calls = %d, want 1", fr.createCalls)
	}
	pc := fr.lastSpec.ProxyConfig
	if len(pc.MITMHosts) != 0 || pc.MITMCACertPEM != "" || pc.MITMCAKeyPEM != "" {
		t.Errorf("a run without components got MITMHosts %v and a CA %t", pc.MITMHosts, pc.MITMCACertPEM != "")
	}
	raw, err := runner.BuildProxyConfig(fr.lastSpec.RunID, pc, runner.ProxyListenPort)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	if _, present := fields["mitm_hosts"]; present {
		t.Errorf("the rendered proxy config names mitm_hosts for a run that has none: %s", fields["mitm_hosts"])
	}
}

// dispatch() turns the gate's decision into interception entries: one per
// header delivery that requires TLS, on the standard TLS port.
func TestDispatch_ComponentInterceptionEntriesAreHostColon443(t *testing.T) {
	header := func(host string, plain bool) types.ComponentSecret {
		return types.ComponentSecret{SecretName: "s", Delivery: types.ComponentDelivery{Mode: types.ComponentDeliveryHeader, Host: host, PlainHTTP: plain}}
	}
	comps := runComponents{
		attached: []attachedComponent{
			{snapshot: types.RunComponent{SelfDefined: true, Definition: types.ComponentDefinition{Secrets: []types.ComponentSecret{
				header("a.example", false),
				{SecretName: "s", Delivery: types.ComponentDelivery{Mode: types.ComponentDeliveryEnv, Var: "A_TOKEN"}},
				{SecretName: "s", Delivery: types.ComponentDelivery{Mode: types.ComponentDeliveryFile, File: "a-token"}},
			}}}},
			{snapshot: types.RunComponent{Definition: types.ComponentDefinition{Secrets: []types.ComponentSecret{
				header("b.example", true), header("c.example", false),
			}}}},
		},
		mitmHosts: []string{"a.example", "b.example", "c.example"},
	}
	comps.attached[1].source = componentSourceOrg
	comps.attached[1].snapshot.Definition.Config = map[string]string{"K": "v"}
	d := comps.dispatch()
	if !slices.Equal(d.MITMHosts, []string{"a.example:443", "c.example:443"}) {
		t.Errorf("MITMHosts = %v, want the two TLS header hosts on :443 and no plain-HTTP one", d.MITMHosts)
	}
	if !slices.Equal(d.HeaderHosts, comps.mitmHosts) {
		t.Errorf("HeaderHosts = %v, want every header host %v, plain-HTTP included", d.HeaderHosts, comps.mitmHosts)
	}
	if len(d.Config) != 1 || d.Config[0].ordinal != 1 || d.Config[0].selfDefined || d.Config[0].source != componentSourceOrg || d.Config[0].env["K"] != "v" {
		t.Errorf("Config = %+v, want the second component's, marked as the organisation's", d.Config)
	}
	if zero := (runComponents{}).dispatch(); zero.MITMHosts != nil || zero.HeaderHosts != nil || zero.Config != nil {
		t.Errorf("no components dispatch as %+v, want the zero value", zero)
	}
	// The proxy reads the entry the way dispatch wrote it.
	for _, entry := range d.MITMHosts {
		if got := componentMITMHost(entry); got+":443" != entry {
			t.Errorf("componentMITMHost(%q) = %q", entry, got)
		}
	}
}

// Config is written under the env_secret lane's rules: never over a variable
// the platform set, never a WARDYN_* or model-provider variable, never a name
// that is not an environment variable. Each component that lost a key gets
// one audit row: an organisation's names the keys, a person's counts them.
func TestDispatch_ComponentConfigNeverReplacesPlatformEnv(t *testing.T) {
	srv, _, audit, run := dispatchTeardownFixture(t, &fakeRunner{}, types.RunPending)
	env := map[string]string{"HTTPS_PROXY": "http://wardyn-proxy:3128", "SSL_CERT_FILE": "/etc/wardyn/ca.pem", "EMPTY_PLACEHOLDER": ""}
	srv.applyComponentConfigEnv(context.Background(), run, []componentConfig{
		{ordinal: 0, source: componentSourceInline, selfDefined: true, env: map[string]string{
			"HTTPS_PROXY":        "http://elsewhere.example:1",
			"WARDYN_TASK_MODE":   "exec",
			"ANTHROPIC_BASE_URL": "https://elsewhere.example",
			"lower_case":         "x",
			"BAD-NAME":           "x",
			"EMPTY_PLACEHOLDER":  "filled",
			"REGION":             "eu-west-1",
		}},
		{ordinal: 1, source: componentSourceOrg, env: map[string]string{"SSL_CERT_FILE": "/tmp/other.pem", "ORG_MODE": "on"}},
		{ordinal: 2, source: componentSourceSelf, selfDefined: true, env: map[string]string{"KEPT": "yes"}},
	}, env)
	want := map[string]string{"HTTPS_PROXY": "http://wardyn-proxy:3128", "SSL_CERT_FILE": "/etc/wardyn/ca.pem",
		"EMPTY_PLACEHOLDER": "filled", "REGION": "eu-west-1", "ORG_MODE": "on", "KEPT": "yes"}
	if len(env) != len(want) {
		t.Fatalf("env = %v, want %v", env, want)
	}
	for k, v := range want {
		if env[k] != v {
			t.Errorf("env[%s] = %q, want %q", k, env[k], v)
		}
	}
	var rows []string
	for _, ev := range audit.snapshot() {
		if ev.Action == "run.component_config.drop" && ev.RunID != nil && *ev.RunID == run.ID {
			rows = append(rows, string(ev.Data))
		}
	}
	if len(rows) != 2 {
		t.Fatalf("run.component_config.drop rows = %v, want one per component that lost a key", rows)
	}
	if !strings.Contains(rows[0], `"dropped":5`) || !strings.Contains(rows[0], `"ordinal":0`) || strings.Contains(rows[0], "keys") ||
		strings.Contains(rows[0], "HTTPS_PROXY") || strings.Contains(rows[0], "lower_case") {
		t.Errorf("the person's row = %s, want a count and no key", rows[0])
	}
	if !strings.Contains(rows[1], `"keys":["SSL_CERT_FILE"]`) || !strings.Contains(rows[1], `"dropped":1`) || !strings.Contains(rows[1], `"source":"org"`) {
		t.Errorf("the organisation's row = %s, want the dropped key named", rows[1])
	}
	for _, r := range rows {
		if strings.Contains(r, "elsewhere") || strings.Contains(r, "other.pem") {
			t.Errorf("a row carries a config value: %s", r)
		}
	}
}

// An interception entry is kept only while its host still has a rule: one a
// phase dropped the credential for is dropped with it.
func TestDispatch_ComponentInterceptionFollowsItsInjection(t *testing.T) {
	_, _, fr, _ := dispatchComponents(t, dispatchParams{
		Policy:     types.RunPolicySpec{AllowedDomains: []string{"kept.example", "dropped.example"}},
		Injections: []runner.InjectionGrant{componentRule("Kept.Example.")},
		Components: componentDispatch{MITMHosts: []string{"kept.example:443", "dropped.example:443"}},
	})
	if fr.createCalls != 1 {
		t.Fatalf("CreateSandbox calls = %d, want 1", fr.createCalls)
	}
	if got := fr.lastSpec.ProxyConfig.MITMHosts; !slices.Equal(got, []string{"kept.example:443"}) {
		t.Errorf("MITMHosts = %v, want only the host that still has an injection rule", got)
	}
	if fr.lastSpec.ProxyConfig.MITMCACertPEM == "" {
		t.Error("no per-run CA for a run with a component header host")
	}
}

// The profile's deny removes a component's host, its injection and its
// interception entry together.
func TestCeilingReassert_DeniedComponentHostLosesInjectionAndInterceptionTogether(t *testing.T) {
	fr := &fakeRunner{}
	srv, _, audit, run := dispatchTeardownFixture(t, fr, types.RunPending)
	policy := types.RunPolicySpec{AllowedDomains: []string{"walled.example", "open.example"}}
	injections := []runner.InjectionGrant{componentRule("walled.example"), componentRule("open.example")}
	p := dispatchParams{Components: componentDispatch{MITMHosts: []string{"walled.example:443", "open.example:443"}}}
	c := dispatchCeiling{resolved: true, profile: "contractors", deny: []string{"*.unrelated.example", "walled.example:443"}}
	var llm llmTransport
	var bedrock []string
	caller := p.Components.MITMHosts

	srv.reassertCeilingDenies(context.Background(), run, &policy, &injections, c, &p, map[string]string{}, &llm, &bedrock, &p.Components.MITMHosts)

	if !slices.Contains(policy.DeniedDomains, "walled.example:443") {
		t.Errorf("denied domains = %v, want the profile's entry unioned in", policy.DeniedDomains)
	}
	if len(injections) != 1 || injections[0].Rule.Host != "open.example" {
		t.Errorf("injections = %+v, want only the host the profile leaves open", injections)
	}
	if !slices.Equal(p.Components.MITMHosts, []string{"open.example:443"}) {
		t.Errorf("component MITMHosts = %v, want the denied host's entry gone", p.Components.MITMHosts)
	}
	if !slices.Equal(caller, []string{"walled.example:443", "open.example:443"}) {
		t.Errorf("the caller's slice was edited in place: %v", caller)
	}
	ev := findAudit(audit.snapshot(), run.ID, "run.ceiling.reassert", "success")
	if ev == nil || !strings.Contains(string(ev.Data), `"dropped_injection_hosts":["walled.example"]`) {
		t.Errorf("run.ceiling.reassert = %v, want the dropped injection host named", ev)
	}

	// No profile: the phase stays a no-op for components too.
	q := dispatchParams{Components: componentDispatch{MITMHosts: []string{"walled.example:443"}}}
	inj := []runner.InjectionGrant{componentRule("walled.example")}
	srv.reassertCeilingDenies(context.Background(), run, &policy, &inj, dispatchCeiling{resolved: true}, &q, map[string]string{}, &llm, &bedrock, &q.Components.MITMHosts)
	if len(inj) != 1 || len(q.Components.MITMHosts) != 1 {
		t.Errorf("with no profile the phase changed %+v / %v", inj, q.Components.MITMHosts)
	}
}

// Two credentials for one host fail the run closed before a sandbox exists,
// whatever authored them and however the host is spelled; the failure is
// audited with the reason and the two grants, and names the host nowhere.
func TestDispatch_CredentialHostCollisionFailsTheRunClosed(t *testing.T) {
	const host = "collide.example"
	for name, second := range map[string]string{
		"the same host":         host,
		"another spelling":      "Collide.Example.",
		"a port on the host":    host + ":8443",
		"a wildcard over it":    "*.example",
		"the same host, padded": " " + host,
	} {
		t.Run(name, func(t *testing.T) {
			first, other := componentRule(host), componentRule(second)
			st, audit, fr, run := dispatchComponents(t, dispatchParams{
				Policy:     types.RunPolicySpec{AllowedDomains: []string{host}},
				Injections: []runner.InjectionGrant{first, componentRule("unrelated.example"), other},
				Components: componentDispatch{MITMHosts: []string{host + ":443"}},
			})
			if fr.createCalls != 0 {
				t.Fatalf("CreateSandbox was called %d times for a run with two credentials on one host", fr.createCalls)
			}
			if got := st.State(); got != types.RunFailed {
				t.Fatalf("run state = %q, want FAILED", got)
			}
			if hint := st.FailureHint(); hint != credentialHostCollisionHint || strings.Contains(hint, "collide") {
				t.Errorf("failure hint = %q", hint)
			}
			ev := findAudit(audit.snapshot(), run.ID, "run.create", "failure")
			if ev == nil {
				t.Fatal("no run.create failure row")
			}
			var data struct {
				Reason   string      `json:"reason"`
				GrantIDs []uuid.UUID `json:"grant_ids"`
			}
			if err := json.Unmarshal(ev.Data, &data); err != nil {
				t.Fatal(err)
			}
			if data.Reason != reasonCredentialHostCollision || !slices.Equal(data.GrantIDs, []uuid.UUID{first.GrantID, other.GrantID}) {
				t.Errorf("run.create failure = %s, want reason %s and the two colliding grants", ev.Data, reasonCredentialHostCollision)
			}
			if strings.Contains(strings.ToLower(string(ev.Data)), "collide") {
				t.Errorf("the audit row names the host: %s", ev.Data)
			}
		})
	}
}

// The comparison covers everything the proxy credentials a host with: every
// injection rule, whoever authored it, and a git_pat grant's forge API door.
func TestDispatch_CredentialHostCollisionCoversEveryAuthor(t *testing.T) {
	component := componentRule("portal.sso.us-east-1.amazonaws.com")
	portal := runner.InjectionGrant{GrantID: uuid.New(), Rule: egress.InjectionRule{Host: "portal.sso.us-east-1.amazonaws.com", Header: "x-amz-sso_bearer_token"}}
	api := uuid.New()
	for _, tc := range []struct {
		name       string
		injections []runner.InjectionGrant
		pat        map[string]proxy.PATGrant
		collide    bool
	}{
		{"nothing", nil, nil, false},
		{"distinct hosts", []runner.InjectionGrant{componentRule("a.example"), componentRule("b.example"), componentRule("a.example.org")}, nil, false},
		{"a component header on the access-portal host of a Bedrock sign-in", []runner.InjectionGrant{portal, component}, nil, true},
		{"a redirect's token on a policy credential's host", []runner.InjectionGrant{componentRule("mirror.corp"), componentRule("mirror.corp")}, nil, true},
		{"a header on a forge whose API door carries the PAT", []runner.InjectionGrant{componentRule("gitlab.corp")},
			map[string]proxy.PATGrant{"gitlab.corp": {GrantID: api, API: true}}, true},
		{"a header on a forge whose PAT stays on the git route", []runner.InjectionGrant{componentRule("gitlab.corp")},
			map[string]proxy.PATGrant{"gitlab.corp": {GrantID: api}}, false},
		{"hosts that do not parse are compared as written", []runner.InjectionGrant{componentRule("bad host"), componentRule("BAD HOST")}, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			grants, found := credentialHostCollision(tc.injections, tc.pat)
			if found != tc.collide {
				t.Fatalf("collision = %v (%v), want %v", found, grants, tc.collide)
			}
			if found && (len(grants) != 2 || grants[0] == uuid.Nil || grants[1] == uuid.Nil || grants[0] == grants[1]) {
				t.Errorf("colliding grants = %v; want two distinct grants", grants)
			}
		})
	}
}

// siteDispatchStore is dispatchTestStore with a site config.
type siteDispatchStore struct {
	*dispatchTestStore
	site types.SiteConfig
}

func (s *siteDispatchStore) GetSiteConfig(context.Context) (types.SiteConfig, error) {
	return s.site, nil
}

// The gate's own collision set is asked again at dispatch, over the site
// config dispatch reads: a redirect an admin added after the run was admitted
// — with no token, so no injection rule of its own — still refuses a
// component's header on its target, and on a public host it stands in for.
func TestDispatch_ComponentHeaderOnARedirectAddedSinceTheGateFailsTheRunClosed(t *testing.T) {
	redirect := types.EgressRedirect{From: "https://registry.npmjs.org/", To: "https://mirror.corp/npm", Ecosystem: "npm"}
	for name, host := range map[string]string{"the redirect's target": "mirror.corp", "the public host it fronts": "registry.npmjs.org"} {
		t.Run(name, func(t *testing.T) {
			fr := &fakeRunner{}
			srv, st, audit, run := dispatchTeardownFixture(t, fr, types.RunPending)
			srv.cfg.Store = &siteDispatchStore{dispatchTestStore: st, site: types.SiteConfig{EgressRedirects: []types.EgressRedirect{redirect}}}
			rule := componentRule(host)
			scope := mustJSON(map[string]any{"host": host, "secret_name": "person-secret", "require_tls": true})
			srv.dispatchRun(context.Background(), run, ceilingForDispatch(governanceCeiling{}, adoEntraUngraded(), bedrockCredUngraded()), dispatchParams{
				RunToken: "run-token", Image: "wardyn/claude-code:latest",
				Policy: types.RunPolicySpec{MinConfinementClass: types.CC1, AllowedDomains: []string{host},
					EligibleGrants: []types.GrantSpec{{Kind: types.GrantAPIKey, Scope: scope, OwnerOnly: true}}},
				Injections: []runner.InjectionGrant{rule},
				Components: componentDispatch{HeaderHosts: []string{host}, MITMHosts: []string{host + ":443"}},
			})
			if fr.createCalls != 0 || st.State() != types.RunFailed {
				t.Fatalf("CreateSandbox calls = %d, state = %q; want the run failed before a sandbox exists", fr.createCalls, st.State())
			}
			ev := findAudit(audit.snapshot(), run.ID, "run.create", "failure")
			if ev == nil || !strings.Contains(string(ev.Data), `"reason":"`+reasonCredentialHostCollision+`"`) ||
				!strings.Contains(string(ev.Data), rule.GrantID.String()) || strings.Contains(string(ev.Data), host) {
				t.Fatalf("run.create failure = %v, want the reason and the component's grant, never the host", ev)
			}
		})
	}
	// The same run with no redirect launches: its own grant is not a collision.
	_, _, fr, _ := dispatchComponents(t, dispatchParams{
		Policy: types.RunPolicySpec{AllowedDomains: []string{"mirror.corp"}, EligibleGrants: []types.GrantSpec{{Kind: types.GrantAPIKey, OwnerOnly: true,
			Scope: mustJSON(map[string]any{"host": "mirror.corp", "secret_name": "person-secret", "require_tls": true})}}},
		Injections: []runner.InjectionGrant{componentRule("mirror.corp")},
		Components: componentDispatch{HeaderHosts: []string{"mirror.corp"}, MITMHosts: []string{"mirror.corp:443"}},
	})
	if fr.createCalls != 1 {
		t.Fatalf("CreateSandbox calls = %d, want 1: a component's own grant is not a second credential", fr.createCalls)
	}
}

// componentHostCollision is credentialedDestinations asked for the run's owner.
func TestDispatch_ComponentHostCollisionReusesTheGatesSet(t *testing.T) {
	own := func(host string) types.GrantSpec {
		return types.GrantSpec{Kind: types.GrantAPIKey, Scope: mustJSON(map[string]any{"host": host, "secret_name": "s"})}
	}
	site := types.SiteConfig{EgressRedirects: []types.EgressRedirect{{From: "https://pypi.org/", To: "https://mirror.corp:8443/pypi", Ecosystem: "pip"}}}
	for _, tc := range []struct {
		name    string
		headers []string
		grants  []types.GrantSpec
		site    types.SiteConfig
		collide bool
	}{
		{"no header hosts", nil, []types.GrantSpec{own("a.example")}, site, false},
		{"only its own grant", []string{"a.example"}, []types.GrantSpec{own("a.example")}, types.SiteConfig{}, false},
		{"a policy credential on another host", []string{"a.example"}, []types.GrantSpec{own("a.example"), own("b.example")}, types.SiteConfig{}, false},
		{"a redirect target on another port", []string{"mirror.corp"}, []types.GrantSpec{own("mirror.corp")}, site, true},
		{"a redirect's public host", []string{"pypi.org"}, []types.GrantSpec{own("pypi.org")}, site, true},
		{"a host that does not parse is refused, never skipped", []string{"*.*.example"}, nil, types.SiteConfig{}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			policy := types.RunPolicySpec{EligibleGrants: tc.grants}
			_, found := componentHostCollision(componentDispatch{HeaderHosts: tc.headers}, tc.site, policy, "sub-owner", nil)
			if found != tc.collide {
				t.Fatalf("collision = %v, want %v", found, tc.collide)
			}
			if len(policy.EligibleGrants) != len(tc.grants) {
				t.Error("the caller's policy was edited")
			}
		})
	}
}

// laneDispatchStore is siteDispatchStore that keeps the grants dispatch writes.
type laneDispatchStore struct {
	*siteDispatchStore
	lmu     sync.Mutex
	written []types.CredentialGrant
}

func (s *laneDispatchStore) CreateGrant(_ context.Context, g types.CredentialGrant) (types.CredentialGrant, error) {
	s.lmu.Lock()
	defer s.lmu.Unlock()
	s.written = append(s.written, g)
	return g, nil
}

func (s *laneDispatchStore) ListGrantsByRun(context.Context, uuid.UUID) ([]types.CredentialGrant, error) {
	s.lmu.Lock()
	defer s.lmu.Unlock()
	return slices.Clone(s.written), nil
}

// dispatchWithFeedRedirect dispatches a run that reaches the public npm
// registry on a deployment whose npm redirect targets a package feed on an
// Azure DevOps host, with a token. repos decides whether the per-person Azure
// DevOps lane resolves for the run.
func dispatchWithFeedRedirect(t *testing.T, repos []types.WorkspaceRepo) (runner.SandboxSpec, *dispatchTestStore, *laneDispatchStore, []types.AuditEvent, types.AgentRun) {
	t.Helper()
	fr := &fakeRunner{}
	srv, st, audit, run := dispatchTeardownFixture(t, fr, types.RunPending)
	ls := &laneDispatchStore{siteDispatchStore: &siteDispatchStore{dispatchTestStore: st, site: feedSite()}}
	srv.cfg.Store = ls
	srv.cfg.Secrets = &memSecrets{m: map[string][]byte{feedToken: []byte("feed-token-value")}}
	srv.dispatchRun(context.Background(), run, ceilingForDispatch(governanceCeiling{}, adoEntraUngraded(), bedrockCredUngraded()), dispatchParams{
		RunToken: "run-token", Image: "wardyn/claude-code:latest",
		Policy: types.RunPolicySpec{MinConfinementClass: types.CC1, AllowedDomains: []string{"registry.npmjs.org"}, WorkspaceRepos: repos},
	})
	return fr.lastSpec, st, ls, audit.snapshot(), run
}

// A run on the per-person Azure DevOps lane, with a token-bearing redirect
// whose target is a lane host: the lane carries that host's credential. The
// redirect still applies — the run's allowlist swaps the public registry for
// the feed — but dispatch authors no grant, no injection rule and no
// interception entry for the redirect's token, and says so on the audit row.
// The proxy is handed one rule for the host, the lane's.
func TestDispatch_ALaneRunLeavesARedirectOntoALaneHostWithoutItsToken(t *testing.T) {
	const feedHost = "pkgs.dev.azure.com"
	spec, st, ls, events, run := dispatchWithFeedRedirect(t, []types.WorkspaceRepo{{Repo: adoTestRepo}})
	if got := st.State(); got == types.RunFailed {
		t.Fatalf("the run failed at dispatch (%q); want it launched with the lane's credential on the feed host", st.FailureHint())
	}
	pc := spec.ProxyConfig
	var onFeed []egress.InjectionRule
	for _, in := range pc.Injection {
		if in.Rule.SecretName == feedToken {
			t.Errorf("the redirect's token is on the run: rule %+v", in.Rule)
		}
		if hostEqual(in.Rule.Host, feedHost) {
			onFeed = append(onFeed, in.Rule)
		}
	}
	if len(onFeed) != 1 || onFeed[0].SecretName != types.ADOEntraAccessTokenSecret || !onFeed[0].RequireTLS {
		t.Fatalf("rules for %s = %+v, want exactly the lane's", feedHost, onFeed)
	}
	if _, found := credentialHostCollision(pc.Injection, pc.PATGrants); found {
		t.Error("the composed proxy config still carries two credentials for one host")
	}
	for _, g := range ls.written {
		if strings.Contains(string(g.Spec.Scope), feedToken) {
			t.Errorf("a grant row was written for the redirect's token: %s", g.Spec.Scope)
		}
	}
	assertNoDuplicateBareMITMHost(t, pc.MITMHosts)
	// The allowlist swap is kept: the feed is reachable, the public registry is not listed.
	if slices.Contains(pc.Policy.AllowedDomains, "registry.npmjs.org") ||
		!slices.ContainsFunc(pc.Policy.AllowedDomains, func(d string) bool { return hostEqual(egressEntryHost(d), feedHost) }) {
		t.Errorf("allowed domains = %v, want the public registry swapped for the feed host", pc.Policy.AllowedDomains)
	}
	ev := findAudit(events, run.ID, "run.artifact.redirect", "warn")
	if ev == nil || !strings.Contains(string(ev.Data), "the per-person Azure DevOps lane carries this host's credential") ||
		!strings.Contains(string(ev.Data), "redirect applied without token injection") {
		t.Errorf("run.artifact.redirect warn = %v, want the row that says the lane carries the host", ev)
	}
	if ok := findAudit(events, run.ID, "run.artifact.redirect", "success"); ok != nil {
		t.Errorf("a token injection was audited for the redirect: %s", ok.Data)
	}
}

// The same redirect on a run the lane does not resolve for keeps its token, as
// it always did.
func TestDispatch_ARunOffTheLaneKeepsTheRedirectsToken(t *testing.T) {
	spec, st, _, events, run := dispatchWithFeedRedirect(t, nil)
	if got := st.State(); got == types.RunFailed {
		t.Fatalf("the run failed at dispatch: %q", st.FailureHint())
	}
	pc := spec.ProxyConfig
	if len(pc.Injection) != 1 || pc.Injection[0].Rule.SecretName != feedToken || !hostEqual(pc.Injection[0].Rule.Host, "pkgs.dev.azure.com") {
		t.Fatalf("injection rules = %+v, want the redirect's token on the feed host", pc.Injection)
	}
	if len(pc.MITMHosts) != 1 || componentMITMHost(pc.MITMHosts[0]) != "pkgs.dev.azure.com" {
		t.Errorf("MITMHosts = %v, want the feed host", pc.MITMHosts)
	}
	if findAudit(events, run.ID, "run.artifact.redirect", "success") == nil || findAudit(events, run.ID, "run.artifact.redirect", "warn") != nil {
		t.Error("want the redirect's token injection audited as a success, and no lane row")
	}
}
