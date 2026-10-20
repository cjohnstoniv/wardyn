// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/broker"
	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/policyref"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The credential paths the Wardyn boundary withholds (DESIGN-TREE §4.4): each brokered lane, each
// injection and each grant the outer layers keep from a run, at dispatch, at revive and live.

var (
	anthropicKey = types.GrantSpec{Kind: types.GrantAPIKey, Scope: json.RawMessage(`{"host":"api.anthropic.com","header":"x-api-key","secret_name":"anthropic"}`)}
	otherKey     = types.GrantSpec{Kind: types.GrantAPIKey, Scope: json.RawMessage(`{"host":"api.other.test","header":"x-api-key","secret_name":"other"}`)}
)

// boundaryOnly is the outer layers of a hard_limits boundary that allows only allow.
func boundaryOnly(allow ...string) []sealLayer { return []sealLayer{listLayer("boundary", allow)} }

// denyOnlyLayer is a layer that only denies, as a restriction does: the stand-in for the
// restriction layer until that lane plants one.
func denyOnlyLayer(deny ...string) sealLayer {
	return sealLayer{rec: policyref.Provenance{Layer: "restriction"}, egress: types.CeilingOverlay{DeniedDomains: &deny}}
}

// TestWithheldPredicate: a host is withheld when the ceiling denies it, a layer denies it, or a
// layer names an allowlist that does not cover it; an open layer with no list withholds nothing.
func TestWithheldPredicate(t *testing.T) {
	on := true
	open := sealLayer{egress: types.CeilingOverlay{AllowAllEgress: &on}}
	for _, tc := range []struct {
		name string
		w    withholding
		host string
		want bool
	}{
		{"nothing binds", withholding{}, "github.com", false},
		{"the ceiling denies", withholding{deny: []string{"github.com"}}, "github.com", true},
		{"a layer denies", withholding{layers: []sealLayer{denyOnlyLayer("*.evil.example")}}, "a.evil.example", true},
		{"a deny-only layer leaves other hosts", withholding{layers: []sealLayer{denyOnlyLayer("*.evil.example")}}, "github.com", false},
		{"a layer allowlist covers", withholding{layers: boundaryOnly("github.com")}, "github.com", false},
		{"a layer allowlist omits", withholding{layers: boundaryOnly("pypi.org")}, "github.com", true},
		{"an empty layer allowlist", withholding{layers: boundaryOnly()}, "pypi.org", true},
		{"an open layer", withholding{layers: []sealLayer{open}}, "github.com", false},
	} {
		if got := tc.w.withheld(tc.host); got != tc.want {
			t.Errorf("%s: withheld(%s) = %v, want %v", tc.name, tc.host, got, tc.want)
		}
	}
	if (withholding{layers: []sealLayer{open}}).binds() {
		t.Error("an open layer with no list or grants binds")
	}
	if !(withholding{layers: []sealLayer{{grants: &[]types.GrantSpec{}}}}).binds() {
		t.Error("a layer naming its eligible grants does not bind")
	}
	w := withholding{layers: []sealLayer{{grants: &[]types.GrantSpec{anthropicKey}}}}
	if w.grantWithheld(anthropicKey) || !w.grantWithheld(otherKey) {
		t.Error("eligible grants: want the named grant kept and the other withheld")
	}
	if !(withholding{layers: boundaryOnly("pypi.org")}).credentialWithheld(anthropicKey) {
		t.Error("a grant whose host the layer does not allow is not withheld")
	}
}

// TestBrokeredLanesWithheldByBoundary: a run no profile applies to, under a boundary that allows only
// the model provider, loses at dispatch every lane that reaches another host — the git broker (and
// its grant id in the env), the PAT broker, the GitHub CLI door, a header injection and its
// interception entry — and the row names the boundary.
func TestBrokeredLanesWithheldByBoundary(t *testing.T) {
	srv, _, audit, run := dispatchTeardownFixture(t, &fakeRunner{}, types.RunPending)
	policy := types.RunPolicySpec{AllowedDomains: []string{"api.anthropic.com"}}
	gitID, patID, doorID := uuid.New(), uuid.New(), uuid.New()
	injections := []runner.InjectionGrant{componentRule("api.anthropic.com"), componentRule("api.other.test")}
	p := dispatchParams{
		GitGrants:    map[string]uuid.UUID{"acme/app": gitID},
		GitPATGrants: map[string]string{"gitlab.example": patID.String()},
		Components: componentDispatch{MITMHosts: []string{"api.other.test:443"},
			GitHubAPI: &proxy.GitHubAPIGrant{Repos: []string{"acme/app"}, Grants: map[string]uuid.UUID{"acme/app": doorID}}},
	}
	env := map[string]string{"WARDYN_GITHUB_GRANT_ID": gitID.String()}
	var llm llmTransport
	var bedrock []string

	srv.reassertCeilingDenies(context.Background(), run, &policy, &injections, dispatchCeiling{resolved: true}, boundaryOnly("api.anthropic.com"),
		nil, &p, env, &llm, &bedrock, &p.Components.MITMHosts)

	if len(p.GitGrants) != 0 || env["WARDYN_GITHUB_GRANT_ID"] != "" {
		t.Errorf("git broker = %v, env grant id %q: want both gone", p.GitGrants, env["WARDYN_GITHUB_GRANT_ID"])
	}
	if len(p.GitPATGrants) != 0 {
		t.Errorf("PAT broker = %v, want gitlab.example withheld", p.GitPATGrants)
	}
	if p.Components.GitHubAPI != nil {
		t.Error("the GitHub CLI door survived a boundary that does not allow GitHub")
	}
	if len(injections) != 1 || injections[0].Rule.Host != "api.anthropic.com" {
		t.Errorf("injections = %+v, want only the model provider's", injections)
	}
	if len(p.Components.MITMHosts) != 0 {
		t.Errorf("component MITMHosts = %v, want the withheld host's entry gone", p.Components.MITMHosts)
	}
	ev := findAudit(audit.snapshot(), run.ID, "run.ceiling.reassert", "success")
	if ev == nil || !strings.Contains(string(ev.Data), `"boundary":true`) || !strings.Contains(string(ev.Data), `"profile":""`) {
		t.Fatalf("run.ceiling.reassert = %v, want a row naming the boundary for a run no profile applies to", ev)
	}
}

// TestBrokeredLanesWithheldByARestrictionLayer: a layer that only denies withholds the lanes for the
// hosts it names (an injection and a PAT host here) and keeps the rest, with no allowlist of its own.
func TestBrokeredLanesWithheldByARestrictionLayer(t *testing.T) {
	srv, _, _, run := dispatchTeardownFixture(t, &fakeRunner{}, types.RunPending)
	policy := types.RunPolicySpec{AllowAllEgress: true}
	gitID, patID := uuid.New(), uuid.New()
	injections := []runner.InjectionGrant{componentRule("api.anthropic.com"), componentRule("gitlab.example")}
	p := dispatchParams{GitGrants: map[string]uuid.UUID{"acme/app": gitID}, GitPATGrants: map[string]string{"gitlab.example": patID.String()}}
	env := map[string]string{"WARDYN_GITHUB_GRANT_ID": gitID.String()}
	var llm llmTransport
	var bedrock []string

	srv.reassertCeilingDenies(context.Background(), run, &policy, &injections, dispatchCeiling{resolved: true}, []sealLayer{denyOnlyLayer("gitlab.example")},
		nil, &p, env, &llm, &bedrock, nil)

	if len(p.GitPATGrants) != 0 {
		t.Errorf("PAT broker = %v, want the denied host withheld", p.GitPATGrants)
	}
	if len(injections) != 1 || injections[0].Rule.Host != "api.anthropic.com" {
		t.Errorf("injections = %+v, want the denied host's injection gone and the other kept", injections)
	}
	if len(p.GitGrants) != 1 {
		t.Errorf("git broker %v: a deny-only layer withheld a host it does not name", p.GitGrants)
	}
}

// TestBrokeredGrantEligibilityStrip: the boundary's eligible grants bind every grant the run holds,
// a launch-added one included — a grant outside them is dropped with its injection and listed.
func TestBrokeredGrantEligibilityStrip(t *testing.T) {
	srv, _, audit, run := dispatchTeardownFixture(t, &fakeRunner{}, types.RunPending)
	policy := types.RunPolicySpec{AllowAllEgress: true}
	kept, gone := types.CredentialGrant{ID: uuid.New(), Spec: anthropicKey}, types.CredentialGrant{ID: uuid.New(), Spec: otherKey}
	injections := []runner.InjectionGrant{
		{GrantID: kept.ID, Rule: egress.InjectionRule{Host: "api.anthropic.com"}},
		{GrantID: gone.ID, Rule: egress.InjectionRule{Host: "api.other.test"}},
	}
	var p dispatchParams
	var llm llmTransport
	var bedrock []string
	layers := []sealLayer{{rec: boundaryProvenance, grants: &[]types.GrantSpec{anthropicKey}}}

	rows := srv.reassertCeilingDenies(context.Background(), run, &policy, &injections, dispatchCeiling{resolved: true}, layers,
		[]types.CredentialGrant{kept, gone}, &p, map[string]string{}, &llm, &bedrock, nil)

	if len(rows) != 1 || rows[0].ID != kept.ID {
		t.Errorf("grant rows = %+v, want only the eligible grant", rows)
	}
	if len(injections) != 1 || injections[0].GrantID != kept.ID {
		t.Errorf("injections = %+v, want the ineligible grant's gone", injections)
	}
	ev := findAudit(audit.snapshot(), run.ID, "run.ceiling.reassert", "success")
	if ev == nil || !strings.Contains(string(ev.Data), `"withheld_grants":["`+gone.ID.String()+`"]`) {
		t.Errorf("run.ceiling.reassert = %v, want the withheld grant listed", ev)
	}
}

// boundaryGrantRecStore is grantRecStore with a Wardyn boundary.
type boundaryGrantRecStore struct {
	*grantRecStore
	b types.Boundary
}

func (s boundaryGrantRecStore) GetBoundary(context.Context) (types.Boundary, error) { return s.b, nil }

func (s boundaryGrantRecStore) PutBoundary(context.Context, types.BoundaryWrite, string) (types.Boundary, error) {
	return s.b, nil
}

// boundaryPlaceholderDispatch is placeholderDispatch, switches on, under the boundary b.
func boundaryPlaceholderDispatch(t *testing.T, b types.Boundary, grants ...types.GrantSpec) runner.SandboxSpec {
	t.Helper()
	rn := &fakeRunner{}
	srv, st, audit, run := dispatchTeardownFixture(t, rn, types.RunPending)
	run.Task, run.CreatedBy = "", fileOwner
	st.run = run
	_, sec, _ := fileSecretServer(t)
	srv.cfg.Store = boundaryGrantRecStore{grantRecStore: &grantRecStore{dispatchTestStore: st}, b: b}
	srv.cfg.Secrets, srv.cfg.MaskRegistry = sec, secretmask.NewRegistry()
	srv.cfg.EnvSecretPlaceholders, srv.cfg.FileSecretPlaceholders = true, true
	dispatchOf(srv, run, types.RunPolicySpec{AllowedDomains: []string{placeholderHost}, EligibleGrants: grants})
	if rn.createCalls != 1 {
		t.Fatalf("the sandbox was not created; events: %s", auditDump(audit.events, run.ID))
	}
	return rn.lastSpec
}

// TestBrokeredEnvSecretWithheld: an env secret the boundary does not list as eligible is never read
// into the sandbox, and a boundary deny of its host leaves no placeholder injection for that host.
func TestBrokeredEnvSecretWithheld(t *testing.T) {
	grant := hostScoped(t, envSecretGrant("CORP_TOKEN", "operator-only"))
	if spec := boundaryPlaceholderDispatch(t, types.Boundary{Mode: types.BoundaryNoLimits}, grant); !strings.HasPrefix(spec.SecretEnv["CORP_TOKEN"], secretPlaceholderPrefix) {
		t.Fatalf("control: CORP_TOKEN = %q, want a placeholder", spec.SecretEnv["CORP_TOKEN"])
	}
	spec := boundaryPlaceholderDispatch(t, types.Boundary{Mode: types.BoundaryHardLimits, Spec: types.BoundarySpec{EligibleGrants: &[]types.GrantSpec{}}}, grant)
	if v, ok := spec.SecretEnv["CORP_TOKEN"]; ok {
		t.Errorf("CORP_TOKEN = %q under a boundary with no eligible grants, want it absent", v)
	}
	denied := []string{placeholderHost}
	spec = boundaryPlaceholderDispatch(t, types.Boundary{Mode: types.BoundaryHardLimits, Spec: types.BoundarySpec{DeniedDomains: &denied}}, grant)
	if slices.ContainsFunc(spec.ProxyConfig.Injection, func(in runner.InjectionGrant) bool { return hostEqual(in.Rule.Host, placeholderHost) }) {
		t.Errorf("injections %+v: the boundary denies %s and a placeholder injection was authored for it", spec.ProxyConfig.Injection, placeholderHost)
	}
}

// TestReassertAfterSeal: on both sealed paths the ceiling re-assertion runs after the seal, so the
// lanes it drops are read against the sealed policy and the boundary's layers; a re-assertion
// planted before the seal is flagged.
func TestReassertAfterSeal(t *testing.T) {
	fset, files := parsePackageSources(t, ".")
	for _, v := range runSealCensus(fset, files).violations {
		if strings.Contains(v, "before its seal") {
			t.Error(v)
		}
	}
	src := "package api\nfunc (s *Server) dispatchRun() { s.reassertCeilingDenies(); s.sealRunPolicy() }\n" +
		"func (s *Server) reviveRunProxy() { s.sealRunPolicy(); reassertProxyCeiling() }\n"
	planted := token.NewFileSet()
	f, err := parser.ParseFile(planted, "planted.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	c := runSealCensus(planted, map[string]*ast.File{"planted.go": f})
	if !slices.ContainsFunc(c.violations, func(v string) bool { return strings.Contains(v, "before its seal") }) {
		t.Errorf("a re-assertion before the seal was not flagged: %v", c.violations)
	}
}

// TestBoundaryMCPWithheld: an MCP server on a host the boundary does not allow is not brokered at
// dispatch, and is dropped with its credential and bindings at revive.
func TestBoundaryMCPWithheld(t *testing.T) {
	grant := uuid.New()
	injections := []runner.InjectionGrant{{GrantID: grant, Rule: egress.InjectionRule{Host: "mcp.linear.test"}}}
	servers := []componentMCP{{Key: "linear", URL: "https://mcp.linear.test/mcp", Host: "mcp.linear.test", Harnesses: []string{"claude-code"}, Credentialed: true}}
	if out := dispatchMCP(servers, withholding{layers: boundaryOnly("api.anthropic.com")}, &injections, "http://p:3128", false, map[string]string{}); len(out.servers) != 0 {
		t.Errorf("dispatch brokered %+v on a host the boundary does not allow", out.servers)
	}

	run := types.AgentRun{ID: uuid.New()}
	cfg := &proxy.Config{RunID: run.ID,
		MCPServers:  []proxy.MCPServerConfig{{Key: "linear", URL: "https://mcp.linear.test/mcp", GrantID: &grant}},
		MCPBindings: []proxy.MCPBinding{{Harness: "claude-code", Servers: []string{"linear"}}},
	}
	re, rerr := reassertProxyCeiling(run, cfg, ownerCeiling{}, boundaryOnly("api.anthropic.com"))
	if rerr != nil {
		t.Fatalf("refused: %+v", rerr)
	}
	if len(cfg.MCPServers) != 0 || len(cfg.MCPBindings[0].Servers) != 0 || !slices.Equal(re.droppedInjection, []string{"mcp.linear.test"}) {
		t.Errorf("revive kept %+v / %+v (dropped %v)", cfg.MCPServers, cfg.MCPBindings, re.droppedInjection)
	}
}

// TestReviveWithheldByBoundary: a revive under a boundary that allows only the model provider drops
// the PAT broker host, the other injection and its interception entry, and refuses a run whose git
// broker or Azure CLI the boundary no longer allows.
func TestReviveWithheldByBoundary(t *testing.T) {
	run := types.AgentRun{ID: uuid.New()}
	layers := boundaryOnly("api.anthropic.com")
	cfg := &proxy.Config{RunID: run.ID,
		Injection: []proxy.InjectionConfig{
			{InjectionRule: egress.InjectionRule{Host: "api.anthropic.com"}, GrantID: uuid.New()},
			{InjectionRule: egress.InjectionRule{Host: "api.other.test"}, GrantID: uuid.New()},
		},
		PATGrants: map[string]proxy.PATGrant{"gitlab.example": {GrantID: uuid.New()}},
		MITMHosts: []string{"api.other.test:443"},
	}
	re, rerr := reassertProxyCeiling(run, cfg, ownerCeiling{}, layers)
	if rerr != nil {
		t.Fatalf("refused: %+v", rerr)
	}
	if len(cfg.Injection) != 1 || cfg.Injection[0].Host != "api.anthropic.com" {
		t.Errorf("injections = %+v, want only the model provider's", cfg.Injection)
	}
	if len(cfg.PATGrants) != 0 || !slices.Equal(re.droppedLane, []string{"gitlab.example"}) {
		t.Errorf("PAT broker = %v (dropped %v), want gitlab.example withheld", cfg.PATGrants, re.droppedLane)
	}
	if len(cfg.MITMHosts) != 0 {
		t.Errorf("MITMHosts = %v, want the withheld host's entry gone", cfg.MITMHosts)
	}

	git := &proxy.Config{RunID: run.ID, GitGrants: map[string]uuid.UUID{"acme/app": uuid.New()}}
	if _, rerr := reassertProxyCeiling(run, git, ownerCeiling{}, layers); rerr == nil || rerr.reason != reasonReviveCeilingDeniesGitBroker {
		t.Errorf("git broker revive = %+v, want %s", rerr, reasonReviveCeilingDeniesGitBroker)
	}
	az := &proxy.Config{RunID: run.ID, AzureARM: &proxy.AzureARMGrant{TenantID: "t"}}
	if _, rerr := reassertProxyCeiling(run, az, ownerCeiling{}, layers); rerr == nil || rerr.reason != reasonReviveCeilingDeniesAzureCLI {
		t.Errorf("Azure CLI revive = %+v, want %s", rerr, reasonReviveCeilingDeniesAzureCLI)
	}
	open := &proxy.Config{RunID: run.ID, GitGrants: map[string]uuid.UUID{"acme/app": uuid.New()}}
	if _, rerr := reassertProxyCeiling(run, open, ownerCeiling{}, nil); rerr != nil {
		t.Errorf("no layers refused the revive: %+v", rerr)
	}
}

// mintBoundaryStore is grantStore with a Wardyn boundary, or a boundary that cannot be read.
type mintBoundaryStore struct {
	grantStore
	b   types.Boundary
	err error
}

func (s mintBoundaryStore) GetBoundary(context.Context) (types.Boundary, error) { return s.b, s.err }

func (s mintBoundaryStore) PutBoundary(context.Context, types.BoundaryWrite, string) (types.Boundary, error) {
	return s.b, s.err
}

// TestBoundaryMintRefusesWithheldGrants: the mint and injection doors refuse, live and before the
// broker is called, a grant the boundary now withholds — by its host or by eligible grants — and
// answer 503 when the boundary cannot be read; a grant the boundary keeps is minted.
func TestBoundaryMintRefusesWithheldGrants(t *testing.T) {
	allowed := []string{"api.anthropic.com"}
	elsewhere := []string{"pypi.org"}
	for _, tc := range []struct {
		name   string
		b      types.Boundary
		err    error
		code   int
		reason string
	}{
		{"no_limits", types.Boundary{Mode: types.BoundaryNoLimits}, nil, http.StatusOK, ""},
		{"host allowed", types.Boundary{Mode: types.BoundaryHardLimits, Spec: types.BoundarySpec{AllowedDomains: &allowed}}, nil, http.StatusOK, ""},
		{"host not allowed", types.Boundary{Mode: types.BoundaryHardLimits, Spec: types.BoundarySpec{AllowedDomains: &elsewhere}}, nil, http.StatusForbidden, reasonGrantWithheld},
		{"not an eligible grant", types.Boundary{Mode: types.BoundaryHardLimits, Spec: types.BoundarySpec{EligibleGrants: &[]types.GrantSpec{otherKey}}}, nil, http.StatusForbidden, reasonGrantWithheld},
		{"boundary unread", types.Boundary{}, errors.New("down"), http.StatusServiceUnavailable, reasonGrantWithheldUnread},
	} {
		for _, door := range []string{"mint", "injection"} {
			t.Run(tc.name+" "+door, func(t *testing.T) {
				h, _ := newSecretsHarness(t)
				run := types.AgentRun{ID: uuid.New()}
				grant := types.CredentialGrant{ID: uuid.New(), RunID: run.ID, Spec: anthropicKey}
				ast := newAuthzStore()
				ast.runs[run.ID] = run
				h.srv.cfg.Store = mintBoundaryStore{grantStore: grantStore{authzStore: ast, grants: []types.CredentialGrant{grant}}, b: tc.b, err: tc.err}
				h.broker.minted = broker.Minted{Kind: types.GrantAPIKey, JTI: "jti-1",
					Injection: &egress.InjectionRule{Host: "api.anthropic.com", Header: "x-api-key", SecretName: "anthropic-api-key", Format: "%s"}}
				tok := h.mintRunToken(t, run.ID)
				var code int
				var body string
				if door == "mint" {
					rr := do(t, h.srv, http.MethodPost, "/api/v1/internal/credentials/mint", tok, `{"grant_id":"`+grant.ID.String()+`"}`)
					code, body = rr.Code, rr.Body.String()
				} else {
					rr := do(t, h.srv, http.MethodGet, "/api/v1/internal/injection/"+grant.ID.String(), tok, "")
					code, body = rr.Code, rr.Body.String()
				}
				if tc.code == http.StatusOK {
					if code == http.StatusForbidden || code == http.StatusServiceUnavailable {
						t.Fatalf("code %d body %s: a grant the boundary keeps was refused", code, body)
					}
					return
				}
				reason := tc.reason
				if door == "injection" && reason == reasonGrantWithheldUnread {
					reason = reasonSinkStoreUnavailable // the sink answers its own unread reason
				}
				if code != tc.code || !strings.Contains(body, reason) {
					t.Fatalf("code %d body %s, want %d %s", code, body, tc.code, reason)
				}
				if h.broker.lastCall != nil {
					t.Error("the broker was called for a refused grant")
				}
			})
		}
	}
}
