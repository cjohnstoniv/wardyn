// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/recordmode"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The control plane's write doors for a host the proxy's config load reads refuse the spellings that load
// refuses (proxy.ValidConfigHost): a model provider's base URL, a site host, URL or redirect target, a
// git_pat host and an api_key host. Each other spelling passes as before.
func TestConfigHostWriteDoorsRefuseASpellingTheProxyRefuses(t *testing.T) {
	const why = `"134744072" writes an IP address in a non-standard spelling, which the proxy refuses in every request; its standard form is 8.8.8.8`
	gitPAT := func(h string) types.GrantSpec {
		return types.GrantSpec{Kind: types.GrantGitPAT, Scope: json.RawMessage(`{"host":"` + h + `","secret_name":"k","access":"read","repos":["acme/one"]}`)}
	}
	apiKey := func(h string) types.GrantSpec {
		return types.GrantSpec{Kind: types.GrantAPIKey, Scope: mustJSON(map[string]any{"host": h, "secret_name": "k"})}
	}
	if err := validateProviderGateway("api.anthropic.com", "https://134744072/v1", false); err == nil || err.Error() != why {
		t.Errorf("a model provider's base URL: %v\nwant %s", err, why)
	}
	if err := validateEligibleGrantMode(0, gitPAT("134744072"), true); err == nil || err.Error() != "eligible_grants[0]: git_pat host "+why {
		t.Errorf("a git_pat host: %v", err)
	}
	if err := validateEligibleGrantMode(0, apiKey("134744072"), true); err == nil || err.Error() != "eligible_grants[0]: api_key scope invalid: host "+why {
		t.Errorf("an api_key host: %v", err)
	}
	for _, h := range []string{"134744072", "010.0.0.1", "0x7f.1", "bücher.de", "８.８.８.８"} {
		if validateProviderGateway("api.anthropic.com", "https://"+h+"/v1", false) == nil {
			t.Errorf("a model provider's base URL at %q passes", h)
		}
		if validSiteHost(h) || validSiteURL("https://"+h+"/") || validSiteURLOrHost(h+":8443") {
			t.Errorf("a site host, URL or redirect target at %q passes", h)
		}
		if validateEligibleGrantMode(0, gitPAT(h), true) == nil || validateEligibleGrantMode(0, apiKey(h), true) == nil {
			t.Errorf("a git_pat or api_key host %q passes", h)
		}
	}
	for _, h := range []string{"git.corp.example", "10.40.2.11"} {
		if err := validateProviderGateway("api.anthropic.com", "https://"+h+"/v1", false); err != nil {
			t.Errorf("a model provider's base URL at %q: %v", h, err)
		}
		if !validSiteHost(h) || !validSiteURL("https://"+h+"/") || !validSiteURLOrHost(h+":8443") {
			t.Errorf("a site host, URL or redirect target at %q is refused", h)
		}
		if err := validateEligibleGrantMode(0, gitPAT(h), true); err != nil {
			t.Errorf("a git_pat host %q: %v", h, err)
		}
		if err := validateEligibleGrantMode(0, apiKey(h), true); err != nil {
			t.Errorf("an api_key host %q: %v", h, err)
		}
	}
}

// A live edit on a run whose stored config does not load is answered as the revive answers one: 409, with
// the load error that names the entry, and nothing changed. A store read that fails stays a 503.
func TestNarrowEdit_StoredConfigThatDoesNotLoadIs409(t *testing.T) {
	f := newNarrowFixture(t)
	var m map[string]any
	if err := json.Unmarshal(f.rs.cfg, &m); err != nil {
		t.Fatal(err)
	}
	m["live_denies"] = []string{"134744072"}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	f.rs.mu.Lock()
	f.rs.cfg = b
	f.rs.mu.Unlock()
	f.orig = b
	w := f.edit(t, `{"deny_hosts":["evil.example"]}`, http.StatusConflict, reasonReviveConfigDoesNotLoad)
	if want := `the run's proxy config does not load: config: live_denies[0]: \"134744072\" writes an IP address in a non-standard spelling`; !strings.Contains(w.Body.String(), want) {
		t.Errorf("answer %s, want it to name the entry", w.Body.String())
	}
	f.nothingChanged(t)
}

// The workspace and approval host doors refuse a host in a spelling the proxy refuses in every request, and
// say why, as the other write doors do: a workspace's approved and denied egress, an approval rule's hosts,
// an egress requirement, and the egress baseline's two lists. A bypass entry in such a spelling says why
// too. Only a dotted spelling reaches these doors: each one's shape check refuses every other first.
func TestApprovedHostDoorsRefuseASpellingTheProxyRefuses(t *testing.T) {
	const why = `"010.0.0.1" writes an IP address in a non-standard spelling, which the proxy refuses in every request; its standard form is 8.0.0.1 or 10.0.0.1, as programs differ on which address it names`
	h := newHarness(t)
	wsID := uuid.New()
	// A list that passes validation reaches the store, which answers stale: writes counts what got that far.
	put := func(route, host string) (code int, b errorBody, writes int) {
		fake := &staleEgressStore{approvedEgressStore: approvedEgressStore{ws: types.Workspace{ID: wsID, Name: "w", Status: types.WorkspaceScanned}}}
		w := do(t, New(baseTestConfig(h, fake)), http.MethodPut, "/api/v1/workspaces/"+wsID.String()+"/"+route, adminToken, `{"domains":["`+host+`"]}`)
		if w.Code != http.StatusOK {
			b = decodeErrorBody(t, w)
		}
		return w.Code, b, fake.writes
	}
	approve := func(host string) error {
		return validateApprovalRuleApprove([]types.ApprovalKind{types.ApprovalEgressDomain}, types.ApprovalRuleApprove{Hosts: []string{host}})
	}
	require := func(host string) string {
		return validateWorkspaceRequirement("egress:"+host, types.WorkspaceRequirement{Level: "required", Provenance: "operator_set"})
	}
	baseline := func(hosts, suffixes []string) error {
		_, err := normalizeEgressBaselineBody(egressBaselineBody{BaselineHosts: hosts, InternalHostSuffixes: suffixes})
		return err
	}
	for route, reason := range map[string]string{"approved-egress": reasonWorkspaceApprovedEgressInvalid, "denied-egress": reasonWorkspaceDeniedEgressInvalid} {
		if code, b, writes := put(route, "010.0.0.1"); code != http.StatusBadRequest || b.Reason != reason || b.Error != "domains: "+why || writes != 0 {
			t.Errorf("PUT %s 010.0.0.1: %d %s %q (%d writes), want 400 %s %q and no write", route, code, b.Reason, b.Error, writes, reason, "domains: "+why)
		}
	}
	for _, c := range []struct {
		door string
		err  error
		want string
	}{
		{"an approval rule's hosts", approve("010.0.0.1"), "approve.hosts: " + why},
		{"an egress requirement", errors.New(require("010.0.0.1")), `requirement "egress:010.0.0.1": ` + why},
		{"the egress baseline's hosts", baseline([]string{"010.0.0.1"}, nil), "baseline_hosts[0]: " + why},
		{"the egress baseline's internal host suffixes", baseline(nil, []string{"010.0.0.1"}), "internal_host_suffixes[0]: " + why},
		{"a bypass entry", validateUpstreamProxyNoProxy([]string{"010.0.0.1"}), "upstream_proxy_no_proxy[0]: " + why},
	} {
		if c.err == nil || c.err.Error() != c.want {
			t.Errorf("%s at 010.0.0.1: %v\nwant %s", c.door, c.err, c.want)
		}
	}
	for _, s := range []string{"0x7f.0.0.1", "127.1", "1.2.3.077", "0x7f.1"} {
		for _, route := range []string{"approved-egress", "denied-egress"} {
			if code, _, writes := put(route, s); code != http.StatusBadRequest || writes != 0 {
				t.Errorf("PUT %s %q: %d (%d writes), want 400 and no write", route, s, code, writes)
			}
		}
		if approve(s) == nil || require(s) == "" || baseline([]string{s}, nil) == nil || baseline(nil, []string{s}) == nil {
			t.Errorf("an approval rule, requirement or baseline at %q passes", s)
		}
		if err := validateUpstreamProxyNoProxy([]string{s}); err == nil || !strings.Contains(err.Error(), "which the proxy refuses in every request") {
			t.Errorf("a bypass entry %q: %v, want it to say why", s, err)
		}
	}
	// A name passes each door, and a bypass entry that is no host keeps the text it had.
	for _, route := range []string{"approved-egress", "denied-egress"} {
		if code, b, writes := put(route, "git.corp.example"); writes != 1 {
			t.Errorf("PUT %s git.corp.example: %d %q, never reached the store", route, code, b.Error)
		}
	}
	if err, msg, berr := approve("git.corp.example"), require("git.corp.example"), baseline([]string{"git.corp.example"}, []string{"git.corp.example"}); err != nil || msg != "" || berr != nil {
		t.Errorf("git.corp.example: %v, %q, %v", err, msg, berr)
	}
	if err := validateUpstreamProxyNoProxy([]string{"vpce.amazonaws.com", "fc00::/7", "100.64.0.0/10"}); err != nil {
		t.Errorf("a host and two CIDRs in the bypass list: %v", err)
	}
	for _, e := range []string{"fc00::/129", "*.corp.internal", "proxy.corp:8080"} {
		if err := validateUpstreamProxyNoProxy([]string{e}); err == nil || !strings.Contains(err.Error(), "must be a CIDR") {
			t.Errorf("bypass entry %q: %v, want the CIDR-or-host text", e, err)
		}
	}
}

// A host a sidecar named reaches a workspace by four paths, and none of them stores one in a spelling the
// proxy refuses in every request: an operator's always decision is refused and says why, and each automatic
// path (the record write-back, a recording's promotion offer, the boot heal of always decisions) skips it
// and leaves a record of why, so no run is broken by it. A sidecar that lies, or one older than step 0, is
// the only source of such a host. Each path takes a name as before.
func TestSidecarNamedHostPathsDoNotStoreASpellingTheProxyRefuses(t *testing.T) {
	const why = `"010.0.0.1" writes an IP address in a non-standard spelling, which the proxy refuses in every request; its standard form is 8.0.0.1 or 10.0.0.1, as programs differ on which address it names`

	t.Run("an always decision is refused, and says why", func(t *testing.T) {
		f := newScopeFixture(t)
		admin := ssoSession(t, "sub-admin-spelling", "admin@corp.example", oidc.RoleAdmin)
		wsID := f.seedWorkspace(t, nil, nil)
		for _, verb := range []string{"approve", "deny"} {
			id := f.seedEgress(t, "010.0.0.1")
			w := doSSO(t, f.srv, http.MethodPost, "/api/v1/approvals/"+id.String()+"/"+verb, admin, decideBody(t, types.ScopeAlways, nil))
			want := "host " + why + "; decide it run or once instead"
			if b := decodeErrorBody(t, w); w.Code != http.StatusBadRequest || b.Reason != reasonDecisionAlwaysInvalidHost || b.Error != want {
				t.Errorf("%s always on 010.0.0.1: %d %s %q, want 400 %s %q", verb, w.Code, b.Reason, b.Error, reasonDecisionAlwaysInvalidHost, want)
			}
			f.approval.mu.Lock()
			state := f.approval.byID[id].State
			f.approval.mu.Unlock()
			if state != types.ApprovalPending {
				t.Errorf("%s always on 010.0.0.1 left the approval %s, want PENDING", verb, state)
			}
		}
		if approved, denied := f.egressLists(t, wsID); len(approved)+len(denied) != 0 {
			t.Errorf("a refused always wrote the workspace: approved=%v denied=%v", approved, denied)
		}
		id := f.seedEgress(t, "registry.npmjs.org")
		if w := doSSO(t, f.srv, http.MethodPost, "/api/v1/approvals/"+id.String()+"/approve", admin, decideBody(t, types.ScopeAlways, nil)); w.Code != http.StatusOK {
			t.Errorf("approve always on registry.npmjs.org: %d %s", w.Code, w.Body.String())
		}
	})

	t.Run("the record write-back skips it, audited with why", func(t *testing.T) {
		h := newHarness(t)
		runID, wsID := uuid.New(), uuid.New()
		fake := &recordStore{
			run:             types.AgentRun{ID: runID, WorkspaceID: &wsID, Task: "workspace record", State: types.RunRunning},
			importStateFake: importStateFake{ws: types.Workspace{ID: wsID}},
		}
		cfg := baseTestConfig(h, fake)
		cfg.Approvals = h.approvals
		srv := New(cfg)
		decided := func(host string) types.ApprovalRequest {
			return types.ApprovalRequest{ID: uuid.New(), RunID: runID, Kind: types.ApprovalEgressDomain, State: types.ApprovalApproved,
				RequestedScope: mustJSON(map[string]string{"host": host, "mode": "wait_for_review"})}
		}
		srv.learnVerifyEgress(context.Background(), decided("010.0.0.1"), types.ActorHuman, "admin@corp.example")
		if len(fake.ws.Requirements) != 0 {
			t.Errorf("requirements = %+v, want nothing written", fake.ws.Requirements)
		}
		if fails := r3bRequirementWriteFailures(h.audit.snapshot()); len(fails) != 1 || r3bAuditDetail(t, fails[0]) != why {
			t.Errorf("failure rows = %+v, want one whose detail is %q", fails, why)
		}
		srv.learnVerifyEgress(context.Background(), decided("registry.npmjs.org"), types.ActorHuman, "admin@corp.example")
		if _, ok := fake.ws.Requirements["egress:registry.npmjs.org"]; !ok || len(fake.ws.Requirements) != 1 {
			t.Errorf("requirements = %+v, want only egress:registry.npmjs.org", fake.ws.Requirements)
		}
	})

	t.Run("a recording never offers it, logged, and a promote naming it says why", func(t *testing.T) {
		logs := captureSlog(t)
		obs := recordmode.Observations{Domains: []recordmode.DomainObservation{
			{Host: "010.0.0.1", AllowCount: 1},
			{Host: "api.stripe.com", AllowCount: 1},
		}}
		if got := promotableHosts(&obs, "", nil); len(got) != 1 {
			t.Errorf("promotableHosts = %v, want only api.stripe.com", got)
		} else if _, ok := got["api.stripe.com"]; !ok {
			t.Errorf("promotableHosts = %v, want api.stripe.com", got)
		}
		if l := logs.String(); !strings.Contains(l, "api: recorded host not offered for promotion") || !strings.Contains(l, "host=010.0.0.1") {
			t.Errorf("log = %q, want the skipped host named", l)
		}
		if got := notPromotableText("010.0.0.1"); got != "host "+why {
			t.Errorf("a promote naming 010.0.0.1: %q", got)
		}
		if got := notPromotableText("api.stripe.com"); got != "host api.stripe.com was not observed+allowed in this recording — cannot promote" {
			t.Errorf("a promote naming an unrecorded host: %q", got)
		}
	})

	t.Run("the boot heal skips an always decision on it, logged", func(t *testing.T) {
		logs := captureSlog(t)
		f := newScopeFixture(t)
		ws := f.seedWorkspace(t, nil, nil)
		at := time.Now().UTC().Add(-time.Hour)
		rcSeedDecided(f, "010.0.0.1", types.ApprovalApproved, at)
		rcSeedDecided(f, "registry.npmjs.org", types.ApprovalApproved, at)
		if n, err := f.srv.ReconcileWorkspaceEgressDecisions(t.Context()); err != nil || n != 1 {
			t.Errorf("reconcile = %d/%v, want 1 decision re-applied", n, err)
		}
		if approved, _ := f.egressLists(t, ws); slices.Contains(approved, "010.0.0.1") || !slices.Contains(approved, "registry.npmjs.org") {
			t.Errorf("approved = %v, want registry.npmjs.org and not 010.0.0.1", approved)
		}
		if l := logs.String(); !strings.Contains(l, "api: always egress decision not re-applied") || !strings.Contains(l, "host=010.0.0.1") {
			t.Errorf("log = %q, want the skipped decision named", l)
		}
	})
}

// An upstream proxy URL refused for its host's spelling alone, stored before the write refused it or held
// in a secret, names a real proxy: it is delivered, with a failure row naming why, so the run's proxy
// refuses its config and the run stops instead of going around that proxy. Each other refusal is dropped.
func TestAnUpstreamProxyURLInARefusedSpellingIsDeliveredSoTheRunStops(t *testing.T) {
	const raw = "http://user:pass@010.0.0.1:3128"
	for _, sc := range []types.SiteConfig{{UpstreamProxyURL: raw}, {UpstreamProxySecretRef: "corp-proxy-url"}} {
		h := newHarness(t)
		h.srv.cfg.Secrets = &memSecrets{m: map[string][]byte{"corp-proxy-url": []byte(raw)}}
		runID := uuid.New()
		if got := h.srv.resolveRunUpstreamProxy(context.Background(), runID, sc, nil, ""); got != raw {
			t.Errorf("site config %+v: resolveRunUpstreamProxy = %q, want the URL delivered", sc, got)
		}
		ev := findAudit(h.audit.snapshot(), runID, "run.upstream_proxy.resolve", "failure")
		var data struct {
			InEffect *bool  `json:"in_effect"`
			Reason   string `json:"reason"`
		}
		if ev == nil || json.Unmarshal(ev.Data, &data) != nil || data.InEffect == nil || *data.InEffect || data.Reason != upstreamURLHostRefused {
			t.Errorf("site config %+v: the resolve row = %+v, want a failure with in_effect false and reason %s", sc, ev, upstreamURLHostRefused)
		} else if strings.Contains(string(ev.Data), "user:pass") {
			t.Errorf("the resolve row carries the URL's credential: %s", ev.Data)
		}
	}
	h := newHarness(t)
	if got := h.srv.resolveRunUpstreamProxy(context.Background(), uuid.New(), types.SiteConfig{UpstreamProxyURL: "http://proxy.corp:0"}, nil, ""); got != "" {
		t.Errorf("a URL whose port the proxy refuses: %q, want it dropped", got)
	}
}

// wardynd's boot files are held to the write rule: a default policy file or a site config seed that holds a
// host in a spelling the proxy refuses in every request stops wardynd at start, and the error names the
// file, the field and the entry.
func TestABootFileWithAHostTheProxyRefusesStopsWardyndNamingTheFileFieldAndEntry(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "examples", "policies", "default.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ field, key, entry string }{
		{"allowed_domains[0]", "allowed_domains", "134744072"},
		{"denied_domains[0]", "denied_domains", "010.0.0.1"},
	} {
		var spec map[string]any
		if err := json.Unmarshal(b, &spec); err != nil {
			t.Fatal(err)
		}
		spec[c.key] = []string{c.entry}
		path := filepath.Join(t.TempDir(), "default.json")
		if err := os.WriteFile(path, mustJSON(spec), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := LoadPolicySpec(path)
		if err == nil || !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), c.field+": ") || !strings.Contains(err.Error(), `"`+c.entry+`"`) {
			t.Errorf("a default policy file with %s %q: %v, want the file, the field and the entry named", c.key, c.entry, err)
		}
	}
	if _, err := LoadPolicySpec(filepath.Join("..", "..", "examples", "policies", "default.json")); err != nil {
		t.Errorf("the shipped default policy: %v", err)
	}
	for _, c := range []struct{ body, field, entry string }{
		{`{"internal_hosts":[{"host_suffix":"010.0.0.1","cidrs":["10.0.0.0/8"]}]}`, "internal_hosts[0].host_suffix", "010.0.0.1"},
		{`{"upstream_proxy_no_proxy":["134744072"]}`, "upstream_proxy_no_proxy[0]", "134744072"},
		{`{"upstream_proxy_url":"http://010.0.0.1:3128"}`, "upstream_proxy_url", "http://010.0.0.1:3128"},
	} {
		path := writeSeedFile(t, c.body)
		_, err := LoadSiteConfigSeed(path)
		if err == nil || !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), c.field+": ") || !strings.Contains(err.Error(), `"`+c.entry+`"`) {
			t.Errorf("a seed %s: %v, want the file, the field and the entry named", c.body, err)
		}
	}
}

// A repository source's clone host joins its runs' allowed_domains, so a workspace write refuses one in a
// spelling the proxy refuses in every request, with the rule's own text. Each other source passes as before.
func TestARepositorySourceHostInARefusedSpellingIsRefusedAtWrite(t *testing.T) {
	repo := func(src string) types.WorkspaceSource {
		return types.WorkspaceSource{Type: types.WorkspaceSourceTypeRepo, Source: src}
	}
	const why = `source host "010.0.0.1" writes an IP address in a non-standard spelling, which the proxy refuses in every request; ` +
		`its standard form is 8.0.0.1 or 10.0.0.1, as programs differ on which address it names`
	if got := validateWorkspaceSource(repo("https://010.0.0.1/org/app.git"), nil); got != why {
		t.Errorf("a source at 010.0.0.1:\n got %q\nwant %q", got, why)
	}
	for src, prefix := range map[string]string{
		"https://134744072/org/app.git":       `source host "134744072" writes an IP address in a non-standard spelling`,
		"https://bücher.de/org/app.git":       `source host "bücher.de" has a character outside ASCII`,
		"https://0x7f.0.0.1:8443/org/app.git": `source host "0x7f.0.0.1" writes an IP address in a non-standard spelling`,
	} {
		if got := validateWorkspaceSource(repo(src), nil); !strings.HasPrefix(got, prefix) {
			t.Errorf("a source %q: %q, want %s…", src, got, prefix)
		}
	}
	for _, src := range []string{"acme/app", "https://github.com/acme/app.git", "https://git.corp.example/org/app.git", "https://10.0.0.1/org/app.git"} {
		if got := validateWorkspaceSource(repo(src), nil); got != "" {
			t.Errorf("a source %q: %q, want it to pass", src, got)
		}
	}
}

// Two git_pat grants for one host written two ways are refused at write, naming both: dispatch keys them
// apart, and the run's proxy refuses two entries with one host key at load. A host written once, or twice
// alike, keeps today's behaviour.
func TestTwoGitPATGrantsForOneHostWrittenTwoWaysAreRefused(t *testing.T) {
	pat := func(h string) types.GrantSpec {
		return types.GrantSpec{Kind: types.GrantGitPAT, Scope: json.RawMessage(`{"host":"` + h + `","secret_name":"k"}`)}
	}
	err := validatePATNarrowedDuplicates([]types.GrantSpec{pat("git.example.com"), pat("git.example.com.")})
	if want := `eligible_grants[0] and eligible_grants[1]: git_pat hosts "git.example.com" and "git.example.com." name one host, ` +
		`which the run's proxy refuses at load: write it one way`; err == nil || err.Error() != want {
		t.Errorf("git.example.com and git.example.com.:\n got %v\nwant %s", err, want)
	}
	for _, pair := range [][2]string{{"git.example.com", "git.example.com"}, {"Git.Example.com", "git.example.com"}, {"git.a.example", "git.b.example"}} {
		if err := validatePATNarrowedDuplicates([]types.GrantSpec{pat(pair[0]), pat(pair[1])}); err != nil {
			t.Errorf("%q and %q: %v", pair[0], pair[1], err)
		}
	}
}

// A workspace's observed egress does not offer a host in a spelling the proxy refuses in every request,
// step 0's own denials among them: the approved-egress write would refuse it.
func TestObservedEgressDoesNotOfferAHostTheWriteRefuses(t *testing.T) {
	h := newHarness(t)
	wsID, runID := uuid.New(), uuid.New()
	fake := &observedEgressStore{
		ws: types.Workspace{
			ID: wsID, Name: "w", Status: types.WorkspaceScanned,
			Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeLocalDir, Path: "/home/u/repo", Target: "/home/agent/work"}},
		},
		runs: []types.AgentRun{{ID: runID, WorkspacePath: "/home/u/repo"}},
		events: map[uuid.UUID][]types.AuditEvent{runID: {
			{Action: "egress.deny", Target: "010.0.0.1", Data: []byte(`{"rule_source":"builtin:ip-spelling"}`)},
			{Action: "egress.deny", Target: "0x7f.0.0.1", Data: []byte(`{"rule_source":"builtin:ip-spelling"}`)},
			{Action: "egress.deny", Target: "feeds.example.com"},
		}},
	}
	w := do(t, New(baseTestConfig(h, fake)), http.MethodGet, "/api/v1/workspaces/"+wsID.String()+"/observed-egress", adminToken, "")
	var got struct {
		Denied []string `json:"denied"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &got) != nil || strings.Join(got.Denied, ",") != "feeds.example.com" {
		t.Errorf("observed egress: %d %s, want only feeds.example.com", w.Code, w.Body.String())
	}
}
