// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// accessAnswer is one dry door's access block and component facts.
type accessAnswer struct {
	Access     *accessBlock           `json:"access"`
	Components []client.ComponentFact `json:"components"`
}

func decodeAccess(t *testing.T, door string, w *httptest.ResponseRecorder) accessAnswer {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("%s = %d %s", door, w.Code, w.Body.String())
	}
	var out accessAnswer
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Access == nil {
		t.Fatalf("%s answered no access block: %s", door, w.Body.String())
	}
	return out
}

func accessRow(a *accessBlock, id string) (client.AccessRow, bool) {
	i := slices.IndexFunc(a.Rows, func(r client.AccessRow) bool { return r.ID == id })
	if i < 0 {
		return client.AccessRow{}, false
	}
	return a.Rows[i], true
}

func accessHost(a *accessBlock, host string) (client.AccessHost, bool) {
	i := slices.IndexFunc(a.Hosts, func(h client.AccessHost) bool { return h.Host == host })
	if i < 0 {
		return client.AccessHost{}, false
	}
	return a.Hosts[i], true
}

// accessSourceKnown is the closed vocabulary an access source may carry.
func accessSourceKnown(kind string) bool {
	return slices.Contains([]string{provKindPolicy, provKindCeiling, provKindWorkspace, provKindComponent,
		provKindModelProvider, provKindPerson, provKindHarness, provKindPosture}, kind)
}

// Both dry doors answer one projection of one fold: the same rows, hosts,
// credentials, unlisted line and setup for the same request. A person's keyed
// push rule reaches the repository row's own component fact at both doors.
func TestAccessFacts_PreviewEqualsPreflight(t *testing.T) {
	var answers []accessAnswer
	for _, door := range dryDoors {
		f := newComponentFixture(t)
		f.st.siteConfig = types.SiteConfig{WorkspaceProviders: &types.WorkspaceProviders{Git: []types.GitProvider{githubRow("github-row", false, "https://github.com")}}}
		f.srv.cfg.DefaultPolicy = types.RunPolicySpec{MinConfinementClass: types.CC2, AllowedDomains: []string{"api.anthropic.com", "github.com", "*.githubusercontent.com"},
			FirstUseApproval: types.FirstUseDenyWithReview, PushRules: &types.PushRulesSpec{DenyPaths: []string{"private/**"}}}
		id := uuid.MustParse("6f1c2c4e-2d1b-4c47-9a53-3a8f1f1d2e10")
		f.st.ws = &types.Workspace{ID: id, Name: "Approved workspace", Status: types.WorkspaceScanned, ApprovedEgress: []string{"registry.example"},
			Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeRepo, Source: "https://github.com/acme/one", Target: "/home/agent/one"}}}
		f.st.workspaces = []types.Workspace{*f.st.ws}
		body := map[string]any{"agent": "claude-code", "task": "access", "workspace_id": id.String(),
			"components": []any{map[string]any{"name": "Person Tool", "inline": map[string]any{"hosts": []string{"person-api.example"}}}},
			"overrides":  map[string]any{"push_rules": []any{map[string]any{"provider": "github", "org": "acme", "deny_paths": []string{"acme-only/**"}}}}}
		answers = append(answers, decodeAccess(t, door, f.ask(t, door, body)))
	}
	preview, preflight := answers[0], answers[1]
	a, b := mustJSON(preview.Access), mustJSON(preflight.Access)
	if string(a) != string(b) {
		t.Fatalf("the doors disagree:\npreview   %s\npreflight %s", a, b)
	}
	access := preflight.Access
	for _, w := range []struct{ id, kind string }{{"agent:claude-code", accessKindHarness}, {"inline:0", accessKindCustom}} {
		if r, ok := accessRow(access, w.id); !ok || r.Kind != w.kind {
			t.Errorf("row %s = %+v, want kind %s in %+v", w.id, r, w.kind, access.Rows)
		}
	}
	if h, ok := accessHost(access, "person-api.example"); !ok || h.Verdict != verdictAllowed || !slices.Equal(h.Rows, []string{"inline:0"}) {
		t.Errorf("component host = %+v", h)
	}
	for _, h := range access.Hosts {
		if !accessSourceKnown(h.Source.Kind) {
			t.Errorf("host %s carries a source outside the closed set: %+v", h.Host, h.Source)
		}
	}
	pushRules := map[string]*types.PushRulesSpec{}
	for door, ans := range map[string]accessAnswer{"preview": preview, "preflight": preflight} {
		var repo *client.AccessRow
		for i, r := range ans.Access.Rows {
			if r.Kind == accessKindRepository {
				repo = &ans.Access.Rows[i]
			}
		}
		if repo == nil {
			t.Fatalf("%s: no repository row in %+v", door, ans.Access.Rows)
		}
		i := slices.IndexFunc(ans.Components, func(c client.ComponentFact) bool { return c.ID == repo.ID })
		if i < 0 || ans.Components[i].PushRules == nil || !slices.Contains(ans.Components[i].PushRules.DenyPaths, "acme-only/**") {
			t.Fatalf("%s: the repository row's fact lacks the person's keyed push rule: %+v", door, ans.Components)
		}
		pushRules[door] = ans.Components[i].PushRules
	}
	compareOrUpdateGolden(t, "testdata/access_facts_golden.json", map[string]any{"access": access, "repository_push_rules": pushRules})
}

// A host is allowed only when the resolved, clamped spec allows it: one the
// person asked for and the ceiling took away is never shown allowed.
func TestAccessHost_RequestedNeverAllowedBeforeClamp(t *testing.T) {
	cs := &capStore{}
	cs.govProfile = govProfile("walled")
	cs.govTier, cs.govHasGroupTier = types.CapabilitySubjectGroup, true
	srv, _, _ := govEscapeFixture(t, cs)
	cookie := govSession(t, govMemberSub, []string{"eng"}, false)
	body := `{"agent":"claude-code","task":"t","overrides":{"hosts":{"add_allowed":["pastebin.com"]}}}`
	for _, door := range dryDoors {
		access := decodeAccess(t, door, doSSO(t, srv, http.MethodPost, door, cookie, body)).Access
		h, ok := accessHost(access, "pastebin.com")
		if !ok || h.Verdict == verdictAllowed || h.Source.Kind != provKindPerson || !slices.Equal(h.Rows, []string{"host:pastebin.com"}) {
			t.Errorf("%s: the clamped request = %+v (%v)", door, h, ok)
		}
		if r, _ := accessRow(access, "host:pastebin.com"); r.Status != accessRefused {
			t.Errorf("%s: the clamped host's row = %+v", door, r)
		}
		if h, ok := accessHost(access, "api.anthropic.com"); !ok || h.Verdict != verdictAllowed {
			t.Errorf("%s: a host the ceiling keeps = %+v (%v)", door, h, ok)
		}
	}
}

// A clamped host waits on the effective first-use mode: a request under a
// review mode, refused and named clamped otherwise.
func TestAccessHost_ClampedVerdictFollowsFirstUse(t *testing.T) {
	for _, tc := range []struct {
		mode            types.FirstUseMode
		verdict, reason string
	}{
		{types.FirstUseAlwaysDeny, verdictNotAllowed, hostReasonClamped},
		{types.FirstUseDenyWithReview, verdictNeedsApproval, ""},
		{types.FirstUseWaitForReview, verdictNeedsApproval, ""},
	} {
		t.Run(string(tc.mode), func(t *testing.T) {
			cs := &capStore{}
			cs.govProfile = govProfile("walled")
			cs.govProfile.Ceiling.FirstUseApproval = tc.mode
			cs.govTier, cs.govHasGroupTier = types.CapabilitySubjectGroup, true
			srv, _, _ := govEscapeFixture(t, cs)
			srv.cfg.DefaultPolicy.FirstUseApproval = tc.mode
			cookie := govSession(t, govMemberSub, []string{"eng"}, false)
			body := `{"agent":"claude-code","task":"t","overrides":{"hosts":{"add_allowed":["pastebin.com"]}}}`
			for _, door := range dryDoors {
				access := decodeAccess(t, door, doSSO(t, srv, http.MethodPost, door, cookie, body)).Access
				if access.Unlisted.Mode != tc.mode {
					t.Fatalf("%s: effective mode %q, want %q", door, access.Unlisted.Mode, tc.mode)
				}
				if h, _ := accessHost(access, "pastebin.com"); h.Verdict != tc.verdict || h.Reason != tc.reason {
					t.Errorf("%s: clamped host = %+v, want %s/%s", door, h, tc.verdict, tc.reason)
				}
			}
		})
	}
}

// The verdict rules on their own: a deny wins, a lane host is allowed once its
// lane is admitted, and an unlisted host follows the mode.
func TestAccessHostVerdict(t *testing.T) {
	spec := types.RunPolicySpec{AllowedDomains: []string{"listed.example", "*.wild.example", "ssh.github.com:443"}, DeniedDomains: []string{"blocked.example"}}
	for _, tc := range []struct {
		host, via       string
		spec            func(types.RunPolicySpec) types.RunPolicySpec
		clamped         bool
		verdict, reason string
	}{
		{"listed.example", viaEgress, nil, false, verdictAllowed, ""},
		{"a.wild.example", viaEgress, nil, false, verdictAllowed, ""},
		{"ssh.github.com:443", viaEgress, nil, false, verdictAllowed, ""},
		{"blocked.example", viaModelLane, nil, false, verdictNotAllowed, hostReasonDeniedList},
		{"api.provider.example", viaModelLane, nil, false, verdictAllowed, ""},
		{"other.example", viaEgress, nil, false, verdictNotAllowed, hostReasonAlwaysDeny},
		{"other.example", viaEgress, nil, true, verdictNotAllowed, hostReasonClamped},
		{"other.example", viaEgress, func(s types.RunPolicySpec) types.RunPolicySpec { s.AllowAllEgress = true; return s }, true, verdictAllowed, ""},
		{"blocked.example", viaEgress, func(s types.RunPolicySpec) types.RunPolicySpec { s.AllowAllEgress = true; return s }, false, verdictNotAllowed, hostReasonDeniedList},
	} {
		sp := spec
		if tc.spec != nil {
			sp = tc.spec(spec.Clone())
		}
		v, r := accessHostVerdict(client.AccessHost{Host: tc.host, Via: tc.via}, sp, tc.clamped)
		if v != tc.verdict || r != tc.reason {
			t.Errorf("%s via %s (clamped %v) = %s/%s, want %s/%s", tc.host, tc.via, tc.clamped, v, r, tc.verdict, tc.reason)
		}
	}
}

// The broker's repository brings no raw host: the row reads "via Wardyn's git
// broker" and github.com stays the policy's alone.
func TestAccessRepo_BrokerLaneHasNoRawHosts(t *testing.T) {
	req := createRunRequest{Agent: "claude-code", Task: "t", Repo: "acme/one"}
	spec := types.RunPolicySpec{AllowedDomains: []string{"api.anthropic.com"}, EligibleGrants: []types.GrantSpec{githubTokenGrant()}}
	facts := componentFacts(componentFactInputs{req: &req, spec: spec}, nil)
	access := accessFacts(accessInputs{req: &req, spec: spec, facts: facts, decider: deciderYou, providerStored: true})
	repo, ok := accessRow(&access, "git_provider:github:app:acme")
	if !ok {
		t.Fatalf("no broker repository row in %+v", access.Rows)
	}
	if len(repo.Hosts) != 0 {
		t.Errorf("the broker row lists raw hosts %v", repo.Hosts)
	}
	i := slices.IndexFunc(access.Credentials, func(c client.AccessCredential) bool { return c.Row == repo.ID })
	if i < 0 || access.Credentials[i].Delivery != deliveryGitBroker || access.Credentials[i].Owner != credOwnerOrgApp {
		t.Errorf("broker credential = %+v", access.Credentials)
	}
	for _, h := range access.Hosts {
		if slices.Contains(h.Rows, repo.ID) {
			t.Errorf("host %s is brought by the broker row", h.Host)
		}
	}
}

// An AWS sign-in's credential facts follow the dispatch-time proxy-inject
// switch: on, only the role credentials are in the sandbox; off, the session's
// access token is there too, as a file, and neither ends with the run.
func TestAccessCredential_BedrockSSOFollowsInjectSwitch(t *testing.T) {
	req := createRunRequest{Agent: "claude-code", Task: "t"}
	choice := runProviderChoice{chosen: true, provider: types.ModelProvider{ID: "bedrock", Name: "Bedrock", Kind: types.ModelProviderBedrockSSO}}
	for _, tc := range []struct {
		inject bool
		want   []client.AccessCredential
	}{
		{true, []client.AccessCredential{{ID: "agent:claude-code:model", Row: "agent:claude-code", Kind: credKindModel, Owner: credOwnerOwn,
			Delivery: deliveryShortLived, Renewed: true, AfterEnd: afterEndExpires}}},
		{false, []client.AccessCredential{
			{ID: "agent:claude-code:model", Row: "agent:claude-code", Kind: credKindModel, Owner: credOwnerOwn, Delivery: deliveryShortLived, AfterEnd: afterEndExpires},
			{ID: "agent:claude-code:sso_token", Row: "agent:claude-code", Kind: credKindModel, Owner: credOwnerOwn, Delivery: deliverySandboxFile, AfterEnd: afterEndExpires},
		}},
	} {
		srv := &Server{cfg: Config{AWSSSOProxyInject: tc.inject}}
		facts := componentFacts(componentFactInputs{req: &req, mpChoice: choice}, nil)
		r := httptest.NewRequest(http.MethodPost, preflightDoor, nil)
		access, err := srv.accessFor(r, accessInputs{req: &req, mpChoice: choice, facts: facts})
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(access.Credentials, tc.want) {
			t.Errorf("inject %v: credentials %+v, want %+v", tc.inject, access.Credentials, tc.want)
		}
	}
}

// The unlisted line reads the effective spec: unset is always_deny, a
// wait_for_review hold defaults to the proxy's window, allow-all is reported.
func TestAccessUnlisted(t *testing.T) {
	if u := accessUnlisted(types.RunPolicySpec{}, deciderAdmin); u.Mode != types.FirstUseAlwaysDeny || u.HoldSec != 0 || u.Decider != deciderAdmin {
		t.Errorf("unset = %+v", u)
	}
	if u := accessUnlisted(types.RunPolicySpec{FirstUseApproval: types.FirstUseWaitForReview}, deciderYou); u.HoldSec != 30 {
		t.Errorf("wait_for_review default hold = %+v", u)
	}
	if u := accessUnlisted(types.RunPolicySpec{FirstUseApproval: types.FirstUseWaitForReview, FirstUseHoldSeconds: 90, AllowAllEgress: true}, deciderYou); u.HoldSec != 90 || !u.AllowAll {
		t.Errorf("wait_for_review 90s, allow-all = %+v", u)
	}
}

// The sentences behind every closed code step 5 shows are the console's table.
func TestAccessReasonSentencesMatchGolden(t *testing.T) {
	type entry struct {
		Key  string `json:"key"`
		Text string `json:"text"`
	}
	got := make([]entry, 0, len(accessSentences))
	for k, v := range accessSentences {
		got = append(got, entry{k, v})
	}
	sort.Slice(got, func(i, j int) bool { return got[i].Key < got[j].Key })
	compareOrUpdateGolden(t, "../../ui/src/app/lib/access-reasons.golden.json", got)
}

// A CLI component's row says whether its tool is in the image the run starts
// from: the convention table for a harness's own image, unknown for an image
// the run names or the operator pinned.
func TestAccessRow_CLIInImageFromTheRunsImage(t *testing.T) {
	id := uuid.MustParse("0b5c3a52-77e1-4c1e-9d0a-1d4f3c2b1a00")
	comps := runComponents{attached: []attachedComponent{{source: componentSourceOrg, snapshot: types.RunComponent{
		ComponentID: &id, Name: "GitHub CLI", Definition: types.ComponentDefinition{Hosts: []string{"api.github.com"},
			CLI: &types.ComponentCLI{Kind: types.CLIKindGitHub, GitHub: &types.CLIGitHub{}}}}}}}
	for _, tc := range []struct {
		name   string
		image  string
		pinned map[string]string
		want   string
	}{
		{"the harness's convention image", "", nil, cliInImageYes},
		{"an image the run names", "registry.example/mine:1", nil, cliInImageUnknown},
		{"an image the operator pinned", "", map[string]string{"claude-code": "registry.example/pinned:1"}, cliInImageUnknown},
	} {
		req := createRunRequest{Agent: "claude-code", Task: "t", Image: tc.image}
		facts := componentFacts(componentFactInputs{req: &req, comps: comps}, nil)
		srv := &Server{cfg: Config{AgentImages: tc.pinned}}
		access, err := srv.accessFor(httptest.NewRequest(http.MethodPost, preflightDoor, nil), accessInputs{req: &req, comps: comps, facts: facts})
		if err != nil {
			t.Fatal(err)
		}
		if r, ok := accessRow(access, id.String()); !ok || r.Kind != accessKindCLI || r.InImage != tc.want {
			t.Errorf("%s: CLI row = %+v (%v), want kind cli in_image %s", tc.name, r, ok, tc.want)
		}
	}
}

// An ssh_key on an agent with no SSH lane brings neither the lane's host nor
// the key at either dry door, as create drops it; the same request on
// claude-code brings both.
func TestAccessRepo_UnwiredSSHLaneBringsNoHostOrCredential(t *testing.T) {
	sshEndpoint, _ := sshOver443Endpoint("github.com")
	for _, tc := range []struct {
		agent string
		wired bool
	}{{"claude-code", true}, {"codex-cli", false}} {
		id := uuid.New()
		body := `{"agent":"` + tc.agent + `","task":"t","workspace_id":"` + id.String() + `",` +
			`"inline_policy":{"min_confinement_class":"CC2","allowed_domains":["api.anthropic.com"],` +
			`"eligible_grants":[{"kind":"ssh_key","scope":{"host":"github.com","key_secret_ref":"` + compOperatorSecret + `"}}]}}`
		for _, door := range dryDoors {
			f := newComponentFixture(t)
			f.st.ws = &types.Workspace{ID: id, Name: "ssh", Status: types.WorkspaceScanned, Sources: []types.WorkspaceSource{
				{Type: types.WorkspaceSourceTypeRepo, Source: "git@github.com:acme/one.git", Target: "/home/agent/one"}}}
			f.st.workspaces = []types.Workspace{*f.st.ws}
			access := decodeAccess(t, door, do(t, f.srv, http.MethodPost, door, adminToken, body)).Access
			// The workspace's SSH clone host is the workspace's either way; the
			// lane's own host and key are what create drops.
			h, _ := accessHost(access, sshEndpoint)
			hasHost := slices.Contains(h.Rows, "git_provider:github:ssh:acme")
			hasKey := slices.ContainsFunc(access.Credentials, func(c client.AccessCredential) bool { return c.Kind == credKindSSHKey })
			if hasHost != tc.wired || hasKey != tc.wired {
				t.Errorf("%s %s: lane host %v, ssh key %v, want %v: %+v", tc.agent, door, hasHost, hasKey, tc.wired, access)
			}
		}
	}
}

// A secret added by header is ready only when its host is allowed: refused
// when the host is not, needing setup while the host waits on approval.
func TestAccessSecretRow_StatusFollowsItsHost(t *testing.T) {
	for _, tc := range []struct {
		name string
		spec types.RunPolicySpec
		want string
	}{
		{"listed", types.RunPolicySpec{AllowedDomains: []string{"api.example"}}, accessReady},
		{"refused", types.RunPolicySpec{FirstUseApproval: types.FirstUseAlwaysDeny}, accessRefused},
		{"waits on approval", types.RunPolicySpec{FirstUseApproval: types.FirstUseDenyWithReview}, accessNeedsSetup},
	} {
		req := createRunRequest{Agent: "claude-code", Task: "t", Overrides: &client.RunOverrides{Agent: &client.AgentOverrides{
			AddSecrets: []client.SecretOverride{{SecretName: "my-key", Host: "api.example"}}}}}
		access := accessFacts(accessInputs{req: &req, spec: tc.spec, decider: deciderYou, providerStored: true})
		if r, ok := accessRow(&access, "secret:my-key"); !ok || r.Status != tc.want {
			t.Errorf("%s: secret row = %+v (%v), want %s", tc.name, r, ok, tc.want)
		}
	}
}

// A person's own Azure DevOps lane with no Entra sign-in stored brings one
// sign-in item that targets the existing connect door; connected, never graded,
// or missing for a cause the popup cannot fix, it brings none.
func TestAccessRepo_ADOSignInSetupItem(t *testing.T) {
	row := adoEntra(adoRow("ado-row", false, "https://dev.azure.com/acme"))
	req := createRunRequest{Agent: "claude-code", Task: "t", Repo: "https://dev.azure.com/acme/project/_git/one"}
	site := types.SiteConfig{WorkspaceProviders: &types.WorkspaceProviders{Git: []types.GitProvider{row}}}
	for _, tc := range []struct {
		name string
		scm  *SCMAccess
		want bool
	}{
		{"not connected", &SCMAccess{Kind: "azure_devops", Org: "https://dev.azure.com/acme", State: modelAccessNotConfigured}, true},
		{"sign-in ended", &SCMAccess{Kind: "azure_devops", Org: "https://dev.azure.com/acme", State: modelAccessExpiredSignin, Cause: scmAccessCauseEnded}, false},
		{"consent missing", &SCMAccess{Kind: "azure_devops", Org: "https://dev.azure.com/acme", State: modelAccessExpiredSignin, Cause: scmAccessCauseConsentNeeded}, false},
		{"console app missing", &SCMAccess{Kind: "azure_devops", Org: "https://dev.azure.com/acme", State: modelAccessExpiredSignin, Cause: ReasonADOPATNeedsConsoleApp}, false},
		{"own token, none added", &SCMAccess{Kind: "azure_devops", Org: "https://dev.azure.com/acme", State: modelAccessNotConfigured, TokenMode: string(types.ADOTokenModeOwnPAT)}, false},
		{"connected", &SCMAccess{Kind: "azure_devops", Org: "https://dev.azure.com/acme", State: modelAccessLive}, false},
		{"not graded", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			facts := componentFacts(componentFactInputs{req: &req, scmSite: site}, tc.scm)
			access := accessFacts(accessInputs{req: &req, scmSite: site, facts: facts, decider: deciderYou, providerStored: true})
			var got []SetupItem
			for _, it := range access.Setup {
				if it.Kind == "repo_credential" {
					got = append(got, it)
				}
			}
			if !tc.want {
				if len(got) != 0 {
					t.Fatalf("setup = %+v, want none", got)
				}
				return
			}
			if len(got) != 1 || got[0].Status != "missing" || got[0].Fix == nil || got[0].Fix.Action != fixSignIn || got[0].Fix.Target != signInAzureDevOps {
				t.Fatalf("setup = %+v, want one missing azure_devops sign-in", got)
			}
			if r, ok := accessRow(&access, facts[0].ID); !ok || r.Status != accessNeedsSetup {
				t.Errorf("repository row = %+v", r)
			}
		})
	}
}

// Both dry doors offer the Azure DevOps sign-in item only for the cause the
// connect popup fixes: no Entra sign-in stored. The preview grades that by
// PRESENCE alone (a names-only list of the caller's own namespace, never a Get),
// so a stored sign-in, even one Review grades as ended, brings none there; an own
// token and a minted_pat row without its console app bring none at either door.
func TestAccessFacts_ADOSignInItemByCause(t *testing.T) {
	entraRow := types.GitProvider{ID: scmTestRowID, Kind: types.GitProviderAzureDevOps, BaseURLs: []string{scmTestADOOrg},
		Lanes: []types.GitLane{types.GitLaneEntra}, CredentialSource: types.CredentialSourcePerUser,
		Entra: &types.ADOEntraConfig{TenantID: "tenant-1", ClientID: "client-1"}}
	blob := func(dead bool) *adoEntraBlob {
		b := &adoEntraBlob{RefreshToken: "rt", Scopes: scmTestBaseline(t), TenantID: "tenant-1", ClientID: "client-1", Subject: "sub-ado-op", Source: adoEntraSourceSignIn}
		if dead {
			b.DeadAt = time.Now()
		}
		return b
	}
	console := func(ctx context.Context) (ADOEntraConfig, bool, error) {
		cfg, _, _ := adoTestEntraSource(ctx)
		cfg.TokenMode, cfg.Scopes = types.ADOTokenModeMintedPAT, adoscope.MintScopes()
		return cfg, true, nil
	}
	for _, tc := range []struct {
		name            string
		row             types.GitProvider
		source          func(context.Context) (ADOEntraConfig, bool, error)
		blob            *adoEntraBlob
		preview, review bool
		cause           string // of Review's git_credential fact, when it names one
	}{
		{name: "no sign-in stored", row: entraRow, preview: true, review: true},
		{name: "sign-in stored and live", row: entraRow, blob: blob(false)},
		{name: "sign-in stored and ended", row: entraRow, blob: blob(true), cause: scmAccessCauseEnded},
		{name: "own token, none added", row: ownPATTestRow()},
		{name: "minted_pat without the console app", row: entraRow, source: console, cause: ReasonADOPATNeedsConsoleApp},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, st, _ := adoRunHarness(t, true)
			st.siteConfig = types.SiteConfig{WorkspaceProviders: &types.WorkspaceProviders{Git: []types.GitProvider{tc.row}}}
			if tc.source != nil {
				srv.cfg.ADOEntra = tc.source
			}
			srv.cfg.DefaultPolicy = types.RunPolicySpec{MinConfinementClass: types.CC2, AllowedDomains: []string{"api.anthropic.com"},
				FirstUseApproval: types.FirstUseDenyWithReview}
			if tc.blob != nil {
				if err := srv.storeADOEntraBlob(context.Background(), "sub-ado-op", scmTestRowID, *tc.blob); err != nil {
					t.Fatal(err)
				}
			}
			body := `{"agent":"claude-code","task":"access","repo":"` + scmTestADORepo + `"}`
			for door, want := range map[string]bool{previewDoor: tc.preview, preflightDoor: tc.review} {
				w := do(t, srv, http.MethodPost, door, adoOperatorToken(st), body)
				access := decodeAccess(t, door, w).Access
				i := slices.IndexFunc(access.Setup, func(it SetupItem) bool { return it.Kind == "repo_credential" })
				if !want {
					if i >= 0 {
						t.Errorf("%s setup = %+v, want no sign-in item", door, access.Setup)
					}
				} else if i < 0 || access.Setup[i].Status != "missing" || access.Setup[i].Fix == nil || access.Setup[i].Fix.Target != signInAzureDevOps {
					t.Errorf("%s setup = %+v, want the azure_devops sign-in item", door, access.Setup)
				}
				if door == preflightDoor && tc.cause != "" && !strings.Contains(w.Body.String(), `"cause":"`+tc.cause+`"`) {
					t.Errorf("preflight does not grade the cause %q: %s", tc.cause, w.Body.String())
				}
			}
		})
	}
}
