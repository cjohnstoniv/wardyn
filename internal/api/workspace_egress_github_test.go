// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

var directGitHubHosts = []string{"github.com", "*.githubusercontent.com"}

func directGitHubFixture(t *testing.T, repos ...string) (*Server, *govEscapeStore, *recRecorder, createRunRequest) {
	t.Helper()
	srv, st, audit := govEscapeFixture(t, &capStore{})
	srv.cfg.DefaultPolicy = types.RunPolicySpec{MinConfinementClass: types.CC2, AllowedDomains: slices.Clone(directGitHubHosts)}
	req := createRunRequest{Agent: "claude-code", Interactive: true,
		InlinePolicy: &types.RunPolicySpec{MinConfinementClass: types.CC2}}
	ws := types.Workspace{ID: uuid.New(), Name: "sources", Status: types.WorkspaceScanned}
	for _, repo := range repos {
		ws.Sources = append(ws.Sources, types.WorkspaceSource{Type: types.WorkspaceSourceTypeRepo, Source: repo})
		req.InlinePolicy.WorkspaceRepos = append(req.InlinePolicy.WorkspaceRepos, types.WorkspaceRepo{Repo: repo})
	}
	if len(repos) > 0 {
		st.workspaces = []types.Workspace{ws}
	}
	return srv, st, audit, req
}

func checkDirectGitHubDoors(t *testing.T, srv *Server, st *govEscapeStore, audit *recRecorder, req createRunRequest, want, added []string) types.RunPolicySpec {
	t.Helper()
	body := string(mustJSON(req))
	member := ssoSession(t, capSub, capEmail, oidc.RoleUser)
	preview := previewResult(t, doSSO(t, srv, http.MethodPost, policyPreviewPath, member, body))
	check := func(door string, got []string) {
		t.Helper()
		if !slices.Equal(sortedDomains(got), sortedDomains(want)) {
			t.Fatalf("%s domains = %v, want %v", door, got, want)
		}
	}
	check("preview", preview.Spec.AllowedDomains)
	if !slices.Contains(preview.Pending, previewEgress) {
		t.Fatal("preview lost pending dispatch egress")
	}
	w := doSSO(t, srv, http.MethodPost, "/api/v1/runs/preflight", member, body)
	if w.Code != http.StatusOK {
		t.Fatalf("preflight = %d %s", w.Code, w.Body.String())
	}
	var preflight preflightResponse
	if err := json.Unmarshal(w.Body.Bytes(), &preflight); err != nil {
		t.Fatal(err)
	}
	egressRisk := func(items []composer.RiskItem) []composer.RiskItem {
		return slices.DeleteFunc(items, func(item composer.RiskItem) bool { return item.Field != "allowed_domains" })
	}
	expected := composer.Grade(composer.RunInput{}, preview.Spec)
	if !reflect.DeepEqual(egressRisk(preflight.RiskAssessment), egressRisk(expected)) {
		t.Fatalf("preflight graded different egress: %+v, preview %+v", preflight.RiskAssessment, expected)
	}
	if len(audit.snapshot()) != 0 {
		t.Fatalf("dry doors wrote audit: %+v", audit.snapshot())
	}
	launched := govCreateAndDispatch(t, srv, st, audit, member, body)
	check("run policy", launched.AllowedDomains)
	fr := srv.cfg.Runner.(*fakeRunner)
	fr.waitForCreates(t, 1)
	fr.mu.Lock()
	actual := slices.Clone(fr.lastSpec.ProxyConfig.Policy.AllowedDomains)
	fr.mu.Unlock()
	check("proxy", actual)
	var rows int
	for _, event := range audit.snapshot() {
		if event.Action != "run.egress.add" {
			continue
		}
		var data struct {
			Kind    string   `json:"kind"`
			Domains []string `json:"added_domains"`
		}
		if err := json.Unmarshal(event.Data, &data); err != nil {
			t.Fatal(err)
		}
		if data.Kind != "github_direct" {
			continue
		}
		rows++
		if event.RunID == nil || !slices.Equal(data.Domains, added) {
			t.Fatalf("direct audit = %+v, want added %v", event, added)
		}
	}
	if (len(added) == 0 && rows != 0) || (len(added) > 0 && rows != 1) {
		t.Fatalf("direct audit rows = %d, added %v", rows, added)
	}
	return launched
}

func sortedDomains(domains []string) []string {
	out := slices.Clone(domains)
	slices.Sort(out)
	return out
}

func TestDirectGitHubEgressSourcesAtAllDoors(t *testing.T) {
	for _, tc := range []struct {
		name, legacy string
		repos, want  []string
		direct       bool
	}{
		{name: "slug", repos: []string{"acme/one"}, want: directGitHubHosts, direct: true},
		{name: "https", repos: []string{"https://github.com/acme/one.git"}, want: directGitHubHosts, direct: true},
		{name: "host case and terminal dot", repos: []string{"https://GitHub.Com./acme/one.git"}, want: directGitHubHosts, direct: true},
		{name: "legacy slug", legacy: "acme/one", want: directGitHubHosts, direct: true},
		{name: "legacy https", legacy: "https://github.com/acme/one.git", want: directGitHubHosts, direct: true},
		{name: "multiple github", repos: []string{"acme/one", "acme/two"}, want: directGitHubHosts, direct: true},
		{name: "github second", repos: []string{"https://gitlab.example/acme/one", "acme/two"}, want: append([]string{"gitlab.example"}, directGitHubHosts...), direct: true},
		{name: "github first", repos: []string{"acme/two", "https://gitlab.example/acme/one"}, want: append([]string{"gitlab.example"}, directGitHubHosts...), direct: true},
		{name: "no clone"},
		{name: "non github", repos: []string{"https://gitlab.example/acme/one"}, want: []string{"gitlab.example"}},
		{name: "ghes", repos: []string{"https://github.corp.example/acme/one"}, want: []string{"github.corp.example"}},
		{name: "host suffix spoof", repos: []string{"https://github.com.example/acme/one"}, want: []string{"github.com.example"}},
		{name: "github subdomain", repos: []string{"https://api.github.com/acme/one"}},
		{name: "plain http", repos: []string{"http://github.com/acme/one"}},
		{name: "ssh url", repos: []string{"ssh://git@github.com/acme/one.git"}, want: []string{"ssh.github.com:443"}},
		{name: "scp", repos: []string{"git@github.com:acme/one.git"}, want: []string{"ssh.github.com:443"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, st, audit, req := directGitHubFixture(t, tc.repos...)
			req.Repo = tc.legacy
			var added []string
			if tc.direct {
				added = directGitHubHosts
			}
			checkDirectGitHubDoors(t, srv, st, audit, req, tc.want, added)
		})
	}
}

func TestDirectGitHubEgressGrantAndDomainBounds(t *testing.T) {
	githubGrant := types.GrantSpec{Kind: types.GrantGitHubToken,
		Scope: json.RawMessage(`{"repos":["acme/one"],"permissions":{"contents":"read"}}`)}
	for _, tc := range []struct {
		name         string
		requestGrant bool
		ceilingGrant bool
		ceiling      []string
		allowAll     bool
		enforce      bool
		personal     []types.CapabilityGrant
		want         []string
	}{
		{name: "absent grant", ceiling: directGitHubHosts, want: directGitHubHosts},
		{name: "dropped grant", requestGrant: true, ceiling: directGitHubHosts, want: directGitHubHosts},
		{name: "retained grant", requestGrant: true, ceilingGrant: true, ceiling: directGitHubHosts},
		{name: "ceiling excludes both"},
		{name: "ceiling includes github only", ceiling: []string{"github.com"}, want: []string{"github.com"}},
		{name: "ceiling includes content only", ceiling: []string{"*.githubusercontent.com"}, want: []string{"*.githubusercontent.com"}},
		{name: "ceiling allow all", allowAll: true, want: directGitHubHosts},
		{name: "personal excludes both", ceiling: directGitHubHosts, enforce: true},
		{name: "personal includes github only", ceiling: directGitHubHosts, enforce: true,
			personal: []types.CapabilityGrant{grant(types.CapabilitySubjectUser, capSub, capEgressHost, "github.com", types.CapabilityAllow)}, want: []string{"github.com"}},
		{name: "personal deny wins", ceiling: directGitHubHosts, enforce: true,
			personal: []types.CapabilityGrant{
				grant(types.CapabilitySubjectUser, capSub, capEgressHost, "*", types.CapabilityAllow),
				grant(types.CapabilitySubjectUser, capSub, capEgressHost, "github.com", types.CapabilityDeny)}, want: []string{"*.githubusercontent.com"}},
		{name: "ceiling allow all still personal bounded", allowAll: true, enforce: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, st, audit, req := directGitHubFixture(t, "acme/one")
			srv.cfg.DefaultPolicy.AllowedDomains = tc.ceiling
			srv.cfg.DefaultPolicy.AllowAllEgress = tc.allowAll
			st.enf = map[string]bool{capEgressHost: tc.enforce}
			st.capStore.grants = tc.personal
			if tc.requestGrant {
				req.InlinePolicy.EligibleGrants = []types.GrantSpec{githubGrant}
			}
			if tc.ceilingGrant {
				srv.cfg.DefaultPolicy.EligibleGrants = []types.GrantSpec{githubGrant}
			}
			checkDirectGitHubDoors(t, srv, st, audit, req, tc.want, tc.want)
		})
	}
}

func TestDirectGitHubEgressPreservesPolicySourcesAndManualDomains(t *testing.T) {
	for _, mode := range []string{"inline", "stored", "default"} {
		t.Run(mode, func(t *testing.T) {
			srv, st, audit, req := directGitHubFixture(t, "acme/one")
			srv.cfg.DefaultPolicy.AllowedDomains = []string{"api.anthropic.com"}
			req.InlinePolicy.AllowedDomains = []string{"api.anthropic.com"}
			switch mode {
			case "inline":
				srv.cfg.DefaultPolicy.AllowedDomains = append(srv.cfg.DefaultPolicy.AllowedDomains, directGitHubHosts...)
			case "stored":
				id := uuid.New()
				st.policies[id] = types.RunPolicy{ID: id, Name: "saved", Spec: *req.InlinePolicy}
				req.InlinePolicy, req.PolicyID = nil, &id
			case "default":
				req.InlinePolicy = nil
				req.WorkspaceID = &st.workspaces[0].ID
			}
			want := []string{"api.anthropic.com"}
			var added []string
			if mode == "inline" {
				want = append(want, directGitHubHosts...)
				added = directGitHubHosts
			}
			checkDirectGitHubDoors(t, srv, st, audit, req, want, added)
		})
	}
	t.Run("authored domains dedupe and deny priority", func(t *testing.T) {
		srv, st, audit, req := directGitHubFixture(t, "acme/one")
		req.InlinePolicy.AllowedDomains = []string{"github.com", "api.anthropic.com"}
		req.InlinePolicy.DeniedDomains = []string{"github.com"}
		srv.cfg.DefaultPolicy.AllowedDomains = append(srv.cfg.DefaultPolicy.AllowedDomains, "api.anthropic.com")
		srv.cfg.DefaultPolicy.DeniedDomains = []string{"*.githubusercontent.com"}
		want := []string{"github.com", "api.anthropic.com", "*.githubusercontent.com"}
		got := checkDirectGitHubDoors(t, srv, st, audit, req, want, []string{"*.githubusercontent.com"})
		if !slices.Equal(got.AllowedDomains, want) || !slices.Contains(got.DeniedDomains, "github.com") || !slices.Contains(got.DeniedDomains, "*.githubusercontent.com") {
			t.Fatalf("authored policy changed: %+v", got)
		}
		p := proxy.NewBuiltinEvaluator(got)
		for _, host := range []string{"github.com", "raw.githubusercontent.com"} {
			if decision, err := p.EvaluateHost(context.Background(), egress.Request{Host: host, Port: 443}); err != nil || decision != egress.VerdictDeny {
				t.Fatalf("denied host %q became reachable: %s, %v", host, decision, err)
			}
		}
	})
}

func TestDirectGitHubEgressOnlyActualAttachedSources(t *testing.T) {
	for _, primary := range []bool{false, true} {
		t.Run(map[bool]string{false: "selected source only", true: "primary seeds every source"}[primary], func(t *testing.T) {
			srv, st, audit, req := directGitHubFixture(t, "https://gitlab.example/acme/one", "acme/two")
			req.InlinePolicy.WorkspaceRepos = req.InlinePolicy.WorkspaceRepos[:1]
			want := []string{"gitlab.example"}
			var added []string
			if primary {
				req.WorkspaceID = &st.workspaces[0].ID
				want = append(want, directGitHubHosts...)
				added = directGitHubHosts
			}
			checkDirectGitHubDoors(t, srv, st, audit, req, want, added)
		})
	}
}

func TestDirectGitHubEgressPreservesWorkspaceFold(t *testing.T) {
	for _, host := range []string{"trusted.example", "github.com"} {
		t.Run(host, func(t *testing.T) {
			srv, st, audit, req := directGitHubFixture(t, "acme/one")
			st.workspaces[0].Requirements = map[string]types.WorkspaceRequirement{
				"egress:" + host: {Level: "required", Provenance: "operator_set"},
			}
			st.enf = map[string]bool{capEgressHost: true}
			st.capStore.grants = []types.CapabilityGrant{
				grant(types.CapabilitySubjectUser, capSub, capEgressHost, "github.com", types.CapabilityAllow),
			}
			want := []string{host}
			var added []string
			if host != "github.com" {
				want = append(want, "github.com")
				added = []string{"github.com"}
			}
			checkDirectGitHubDoors(t, srv, st, audit, req, want, added)
		})
	}
}

func TestDirectGitHubEgressCandidateNarrowingDoesNotReclamp(t *testing.T) {
	srv, st, _, req := directGitHubFixture(t, "acme/one")
	st.enf = map[string]bool{capEgressHost: true}
	// A trusted fold can legitimately supply hosts, mounts and grants outside
	// the member's authored-policy ceiling; only the new candidates are bounded.
	spec := types.RunPolicySpec{AllowedDomains: []string{"trusted.example", "github.com"},
		DeniedDomains: []string{"denied.example"}, MinConfinementClass: types.CC3,
		WorkspaceRepos:  req.InlinePolicy.WorkspaceRepos,
		WorkspaceMounts: []types.WorkspaceMount{{Source: "/trusted", Target: "/work"}},
		EligibleGrants:  []types.GrantSpec{{Kind: types.GrantAPIKey, Scope: json.RawMessage(`{"host":"trusted.example","secret_name":"operator-name"}`)}}}
	before := string(mustJSON(spec))
	ceiling := governanceCeiling{Spec: srv.cfg.DefaultPolicy}
	added, refusal := srv.unionDirectGitHubEgress(memberRequest(t), req, &spec, ceiling)
	if refusal != nil || len(added) != 0 || string(mustJSON(spec)) != before {
		t.Fatalf("candidate narrowing altered resolved policy: added %v, refusal %+v, spec %+v", added, refusal, spec)
	}
	st.err = errors.New("capability store unavailable")
	if _, refusal = srv.unionDirectGitHubEgress(memberRequest(t), req, &spec, ceiling); refusal == nil || string(mustJSON(spec)) != before {
		t.Fatalf("capability error did not fail closed without mutation: refusal %+v", refusal)
	}
}

func TestDirectGitHubPreviewKeepsConfiguredHostsPending(t *testing.T) {
	srv, st, audit, req := directGitHubFixture(t, "acme/one")
	st.siteConfig.ScmHosts = []string{"private-configured-host.invalid"}
	forbidPreviewSideEffects(t, srv)
	w := doSSO(t, srv, http.MethodPost, policyPreviewPath, ssoSession(t, capSub, capEmail, oidc.RoleUser), string(mustJSON(req)))
	got := previewResult(t, w)
	if !slices.Equal(got.Spec.AllowedDomains, directGitHubHosts) || !slices.Contains(got.Pending, previewEgress) || strings.Contains(w.Body.String(), "private-configured-host") {
		t.Fatalf("unsafe preview: %s", w.Body.String())
	}
	if len(audit.snapshot()) != 0 {
		t.Fatalf("preview wrote audit: %+v", audit.snapshot())
	}
}

func TestDirectGitHubEgressAuthorizationPrecedesFacts(t *testing.T) {
	for _, gate := range []string{"workspace", "workspace source", "provider", "foreign source"} {
		t.Run(gate, func(t *testing.T) {
			srv, st, audit, req := directGitHubFixture(t, "acme/one")
			status := http.StatusForbidden
			switch gate {
			case "workspace", "workspace source":
				st.enf = map[string]bool{capWorkspace: true}
				if gate == "workspace" {
					req.WorkspaceID = &st.workspaces[0].ID
				}
			case "provider":
				st.siteConfig.WorkspaceProviders = &types.WorkspaceProviders{
					Git: []types.GitProvider{githubRow("private-github-row", false, "https://github.com")}}
				st.enf = map[string]bool{capWorkspaceProvider: true}
			case "foreign source":
				st.workspaces[0].OwnedBy = "another-member"
				status = http.StatusUnprocessableEntity
			}
			body := string(mustJSON(req))
			var previous string
			for _, path := range []string{policyPreviewPath, "/api/v1/runs/preflight", "/api/v1/runs"} {
				w := doSSO(t, srv, http.MethodPost, path, ssoSession(t, capSub, capEmail, oidc.RoleUser), body)
				if w.Code != status || strings.Contains(w.Body.String(), `"spec"`) || strings.Contains(w.Body.String(), "githubusercontent.com") {
					t.Fatalf("%s bypassed authorization: %d %s", path, w.Code, w.Body.String())
				}
				if previous != "" && previous != w.Body.String() {
					t.Fatalf("refusal differs at %s: %s vs %s", path, previous, w.Body.String())
				}
				previous = w.Body.String()
			}
			if len(st.runs) != 0 {
				t.Fatal("refused request created a run")
			}
			for _, row := range audit.snapshot() {
				if row.Action == "run.egress.add" {
					t.Fatalf("refused request audited an addition: %+v", row)
				}
			}
		})
	}
}
