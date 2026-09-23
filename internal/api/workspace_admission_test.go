// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// PROVIDER ADMISSION (0.7.2) at every door a repository can enter through, and
// the lane veto at every site that mints a git credential.
//
// A table over SITES rather than a test per site, for the reason the capability
// table next door has one: admission is worth nothing if one door answers
// differently, and the doors are heterogeneous on purpose (two of them are
// server-side launchers that pass through neither decodeAndValidateCreateRun nor
// validateWorkspaceSources).

const (
	admitRowID = "corp-github"
	// admitOnRow is admitted by the fixture row; admitOffRow is on a host the row
	// CLAIMS kind-wide and outside its base path — the case org-path scoping
	// exists for, and the ordinary operator refusal.
	admitOnRow  = "acme/app"  // -> https://github.com/acme/app.git
	admitOffRow = "other/app" // -> https://github.com/other/app.git
	// admitUnclaimed is on a host NO row claims.
	admitUnclaimed      = "https://gitlab.example.com/team/app.git"
	admitBaseURL        = "https://github.com/acme"
	admitLegacyHostName = "gitlab.example.com"
)

func admitSite() types.SiteConfig {
	return providersConfig([]types.GitProvider{githubRow(admitRowID, false, admitBaseURL)})
}

func admitDisabledSite() types.SiteConfig {
	return providersConfig([]types.GitProvider{githubRow(admitRowID, true, admitBaseURL)})
}

// admitLegacySite keeps the unclaimed host on the legacy scm_hosts list — the
// one-release grace an onboarded GitLab repo gets.
func admitLegacySite() types.SiteConfig {
	return providersConfig([]types.GitProvider{githubRow(admitRowID, false, admitBaseURL)}, admitLegacyHostName)
}

// admitClaimedLegacySite is the CLIFF GUARD: github.com is on the legacy list AND
// a row claims it kind-wide, so a repo outside the row's base path is refused by
// the claim — and the operator's refusal names the claiming row's addresses, not
// the union, because that is the list they would have to widen.
func admitClaimedLegacySite() types.SiteConfig {
	return providersConfig([]types.GitProvider{githubRow(admitRowID, false, admitBaseURL)}, "github.com")
}

// assertions

// assertOperatorRefused is the operator half of the DISCLOSURE rule: 422, and the
// allowed addresses are named, because the operator is the person who can widen
// them and can already read them off the providers surface.
func assertOperatorRefused(t *testing.T, w *httptest.ResponseRecorder, wantAddress string) {
	t.Helper()
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422: %s", w.Code, w.Body.String())
	}
	if body := w.Body.String(); !strings.Contains(body, wantAddress) {
		t.Errorf("body = %s, want the allowed address %q named for an operator", body, wantAddress)
	}
}

// assertMemberRefused is the member half: 403, the frozen sentence verbatim, and
// NEITHER a base URL nor a row id — GET /workspace-providers is operator-only
// precisely because base URLs name corporate topology, so a refusal that listed
// them would hand the tier that door refuses the same document.
func assertMemberRefused(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, admitMember) {
		t.Errorf("body = %s, want the frozen ADMIT_MEMBER sentence %q", body, admitMember)
	}
	for _, leak := range []string{admitBaseURL, "github.com/acme", admitRowID} {
		if strings.Contains(body, leak) {
			t.Errorf("body = %s, must not disclose %q to a member", body, leak)
		}
	}
}

// admissionMarkers are the substrings that mean "provider admission refused
// this". Asserting their ABSENCE (rather than a status code) is what makes the
// admitted legs immune to a thin fixture failing later for an unrelated reason.
var admissionMarkers = []string{
	"outside every enabled provider",
	"turned off for this deployment",
	"claimed by the",
	admitMember,
}

func assertNotAdmissionRefused(t *testing.T, w *httptest.ResponseRecorder, why string) {
	t.Helper()
	body := w.Body.String()
	for _, marker := range admissionMarkers {
		if strings.Contains(body, marker) {
			t.Fatalf("admission refused (%d %s); %s", w.Code, body, why)
		}
	}
}

// the doors

// admitDoor is one way a repository reaches a clone. fire drives ONE request
// through it with the given provider policy, naming the given repo, as an
// operator or as a member.
type admitDoor struct {
	name string
	// memberReachable is false for a door only an operator can open — POST
	// /sources is operatorOnly, and devcontainer_repo is refused from a member by
	// denyMemberRequest before admission is reached.
	memberReachable bool
	fire            func(t *testing.T, sc types.SiteConfig, repo string, operator bool) (*Server, *httptest.ResponseRecorder)
}

func admitAdminSession(t *testing.T) *http.Cookie {
	t.Helper()
	return ssoSession(t, "sub-admit-admin", "admin@corp.example", oidc.RoleAdmin)
}

// admitRunDoor drives POST /runs with a body built from the repo under test. The
// repo is ALSO onboarded, because the resolved-spec door sits behind
// validateWorkspaceSources — an un-onboarded repo would be refused one line
// earlier, for the wrong reason.
func admitRunDoor(name string, memberReachable bool, body func(repo string) string) admitDoor {
	return admitDoor{
		name:            name,
		memberReachable: memberReachable,
		fire: func(t *testing.T, sc types.SiteConfig, repo string, operator bool) (*Server, *httptest.ResponseRecorder) {
			t.Helper()
			srv, st, _ := govEscapeFixture(t, &capStore{})
			st.siteConfig = sc
			st.workspaces = []types.Workspace{{
				ID: uuid.New(), Name: "app", OwnedBy: govMemberSub,
				Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeRepo, Source: repo}},
			}}
			session := govSession(t, govMemberSub, []string{"eng"}, false)
			if operator {
				session = admitAdminSession(t)
			}
			return srv, doSSO(t, srv, http.MethodPost, "/api/v1/runs", session, body(repo))
		},
	}
}

func admitWorkspaceDoor(name string, fire func(t *testing.T, srv *Server, st *ownerStore, session *http.Cookie, repo string) *httptest.ResponseRecorder) admitDoor {
	return admitDoor{
		name:            name,
		memberReachable: true,
		fire: func(t *testing.T, sc types.SiteConfig, repo string, operator bool) (*Server, *httptest.ResponseRecorder) {
			t.Helper()
			srv, st, _ := ownerHarness(t, runner.MemberMountPolicy{})
			st.siteConfig = sc
			session := ssoSession(t, ownerMemberSub, "member@corp.example", oidc.RoleMember)
			if operator {
				session = admitAdminSession(t)
			}
			return srv, fire(t, srv, st, session, repo)
		},
	}
}

// admitSourcesStore is the smallest store the library door and the scan launcher
// need: a provider policy to read, and an upsert to count (so a refusal can be
// shown to have written nothing).
type admitSourcesStore struct {
	store.Store
	siteConfig types.SiteConfig
	upserts    int
}

func (s *admitSourcesStore) GetSiteConfig(context.Context) (types.SiteConfig, error) {
	return s.siteConfig, nil
}

func (s *admitSourcesStore) UpsertSource(_ context.Context, src types.Source) (types.Source, error) {
	s.upserts++
	return src, nil
}

// errAdmitFixtureReached is how the launcher legs below prove admission did NOT
// refuse: the next thing launchSourceScanRun does is claim the source's fence,
// and this fixture answers that with a sentinel instead of a nil-method panic.
var errAdmitFixtureReached = errors.New("fixture: reached the fence claim")

func (s *admitSourcesStore) ClaimSourceActiveRun(context.Context, uuid.UUID, uuid.UUID) error {
	return errAdmitFixtureReached
}

func admitDoors() []admitDoor {
	const wsBody = `{"name":"app","sources":[{"type":"repo","source":%q}]}`
	return []admitDoor{
		// The LEGACY single `repo` field: cloned by the sandbox, broker-minted for
		// and unioned into the run's egress, and reaching neither the capability
		// door nor the resolved-spec gate.
		admitRunDoor("POST /runs (legacy repo field)", true, func(repo string) string {
			return `{"agent":"claude-code","task":"t","repo":` + quote(repo) + `}`
		}),
		// The RESOLVED spec — the un-bypassable gate, reached alike by
		// workspace_id, a stored policy and a hand-authored inline policy.
		admitRunDoor("POST /runs (resolved spec)", true, func(repo string) string {
			return `{"agent":"claude-code","task":"t","inline_policy":{"min_confinement_class":"CC2",` +
				`"workspace_repos":[{"repo":` + quote(repo) + `,"target":"/home/agent/work/app"}]}}`
		}),
		// devcontainer_repo: cloned SERVER-SIDE by the image builder, and
		// unconditional on req.image — before 0.7.2 nothing validated it at all.
		admitRunDoor("POST /runs (devcontainer_repo)", false, func(repo string) string {
			return `{"agent":"claude-code","task":"t","devcontainer_repo":` + quote(repo) + `}`
		}),
		admitWorkspaceDoor("POST /workspaces", func(t *testing.T, srv *Server, _ *ownerStore, session *http.Cookie, repo string) *httptest.ResponseRecorder {
			return doSSO(t, srv, http.MethodPost, "/api/v1/workspaces", session, sprintfBody(wsBody, repo))
		}),
		// An EDIT is how a workspace moves onto a repository nothing admits.
		admitWorkspaceDoor("PUT /workspaces/{id}", func(t *testing.T, srv *Server, st *ownerStore, session *http.Cookie, repo string) *httptest.ResponseRecorder {
			id := st.put(types.Workspace{OwnedBy: ownerMemberSub})
			return doSSO(t, srv, http.MethodPut, "/api/v1/workspaces/"+id.String(), session, sprintfBody(wsBody, repo))
		}),
		// A scan is a SERVER-SIDE clone (launchSourceScanRun).
		admitWorkspaceDoor("POST /workspaces/{id}/scan", func(t *testing.T, srv *Server, st *ownerStore, session *http.Cookie, repo string) *httptest.ResponseRecorder {
			id := st.put(types.Workspace{OwnedBy: ownerMemberSub,
				Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeRepo, Source: repo}}})
			return doSSO(t, srv, http.MethodPost, "/api/v1/workspaces/"+id.String()+"/scan", session, "")
		}),
		// So is the wizard's Build step (resolveWorkspaceImage ->
		// repoOwnDevcontainerURL -> envbuilder).
		admitWorkspaceDoor("POST /workspaces/{id}/build", func(t *testing.T, srv *Server, st *ownerStore, session *http.Cookie, repo string) *httptest.ResponseRecorder {
			id := st.put(types.Workspace{OwnedBy: ownerMemberSub,
				Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeRepo, Source: repo}}})
			return doSSO(t, srv, http.MethodPost, "/api/v1/workspaces/"+id.String()+"/build", session, "")
		}),
		// The LIBRARY door: a repo locator onboarded outside the workspace table.
		{
			name:            "POST /sources",
			memberReachable: false,
			fire: func(t *testing.T, sc types.SiteConfig, repo string, _ bool) (*Server, *httptest.ResponseRecorder) {
				t.Helper()
				h := newHarness(t)
				st := &admitSourcesStore{siteConfig: sc}
				srv := New(baseTestConfig(h, st))
				return srv, do(t, srv, http.MethodPost, "/api/v1/sources", adminToken,
					`{"kind":"repo","locator":`+quote(repo)+`}`)
			},
		},
	}
}

func quote(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func sprintfBody(tmpl, repo string) string {
	return strings.Replace(tmpl, "%q", quote(repo), 1)
}

// TestAdmissionAtEveryRequestDoor is admission's whole contract at the eight
// doors a request can reach.
func TestAdmissionAtEveryRequestDoor(t *testing.T) {
	for _, door := range admitDoors() {
		t.Run(door.name, func(t *testing.T) {
			t.Run("operator: 422 listing the allowed addresses", func(t *testing.T) {
				_, w := door.fire(t, admitSite(), admitOffRow, true)
				assertOperatorRefused(t, w, admitBaseURL)
			})

			t.Run("a DISABLED row refuses, and says so", func(t *testing.T) {
				_, w := door.fire(t, admitDisabledSite(), admitOnRow, true)
				if w.Code != http.StatusUnprocessableEntity {
					t.Fatalf("status = %d, want 422: %s", w.Code, w.Body.String())
				}
				// "off" is a switch to flip, not an address to widen — telling an
				// admin the row's paths here sends them the wrong way.
				if body := w.Body.String(); !strings.Contains(body, "turned off for this deployment") {
					t.Errorf("body = %s, want the disabled-row sentence", body)
				}
			})

			t.Run("on the row: admitted", func(t *testing.T) {
				_, w := door.fire(t, admitSite(), admitOnRow, true)
				assertNotAdmissionRefused(t, w, "a repository inside the row's base URL is admitted")
			})

			// The one-release grace: nothing claims this host, and the legacy
			// scm_hosts list still names it.
			t.Run("unclaimed host on the legacy scm_hosts list: admitted", func(t *testing.T) {
				_, w := door.fire(t, admitLegacySite(), admitUnclaimed, true)
				assertNotAdmissionRefused(t, w, "an unclaimed legacy host stays launchable for one release")
			})

			// The upgrade pin.
			t.Run("no provider rows: admission is a no-op", func(t *testing.T) {
				_, w := door.fire(t, types.SiteConfig{}, admitOffRow, true)
				assertNotAdmissionRefused(t, w, "legacy open mode must behave exactly as 0.7.1 did")
			})

			if !door.memberReachable {
				return
			}
			t.Run("member: 403 naming the kind and nothing else", func(t *testing.T) {
				_, w := door.fire(t, admitSite(), admitOffRow, false)
				assertMemberRefused(t, w)
			})
		})
	}
}

// TestAdmissionClaimedLegacyHostNamesTheClaimingRow is the cliff guard's own
// disclosure: github.com is on the legacy list AND claimed kind-wide, so the
// refusal points at the row that claimed it rather than at the union — the list
// an admin would actually have to widen.
func TestAdmissionClaimedLegacyHostNamesTheClaimingRow(t *testing.T) {
	sc := admitClaimedLegacySite()
	msg := admissionRefusal(sc, admitOffRow, true)
	if msg == "" {
		t.Fatal("a claimed legacy host outside the row's base path must be refused")
	}
	for _, want := range []string{"claimed by the", admitRowID, admitBaseURL} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal = %q, want %q named", msg, want)
		}
	}
	// And the member never sees any of it.
	if got := admissionRefusal(sc, admitOffRow, false); got != admitMember {
		t.Errorf("member refusal = %q, want the frozen sentence %q", got, admitMember)
	}
}

// TestAdmissionRefusalStringsAreTheDraftConstants pins every refusal to its
// frozen constant rather than to a copy of the sentence: the M2 canon swap has to
// be a one-file diff, and a test carrying its own copy of the string is what
// turns that into a hunt.
func TestAdmissionRefusalStringsAreTheDraftConstants(t *testing.T) {
	for _, tc := range []struct {
		name string
		sc   types.SiteConfig
		repo string
		want string
	}{
		{"no row claims it", admitSite(), admitUnclaimed, admitOperator},
		{"claimed, outside the base path", admitSite(), admitOffRow, admitOperator},
		{"a disabled row", admitDisabledSite(), admitOnRow, admitDisabledRow},
		{"claimed legacy host", admitClaimedLegacySite(), admitOffRow, admitOperatorClaimed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := admissionRefusal(tc.sc, tc.repo, true)
			// The constant is a format string; compare on its fixed prefix, which
			// is what a frozen-string swap actually changes.
			prefix := tc.want
			if i := strings.Index(prefix, "%"); i > 0 {
				prefix = prefix[:i]
			}
			if !strings.HasPrefix(got, prefix) {
				t.Errorf("refusal = %q, want it formatted from %q", got, tc.want)
			}
		})
	}
}

// the two launchers

// TestAdmissionAtTheScanLauncher: launchSourceScanRun creates a run without
// passing decodeAndValidateCreateRun or validateWorkspaceSources, so the request
// doors above it cannot cover it. The refusal lands BEFORE the fence claim — a
// store with nothing but a site config proves it, because anything past the claim
// would panic on the absent methods.
func TestAdmissionAtTheScanLauncher(t *testing.T) {
	fire := func(sc types.SiteConfig, locator string) error {
		h := newHarness(t)
		srv := New(baseTestConfig(h, &admitSourcesStore{siteConfig: sc}))
		_, err := srv.launchSourceScanRun(context.Background(), "admin",
			types.Source{ID: uuid.New(), Kind: types.SourceRepo, Locator: locator})
		return err
	}

	err := fire(admitSite(), admitOffRow)
	if !errors.Is(err, errRepoNotAdmitted) {
		t.Fatalf("err = %v, want errRepoNotAdmitted", err)
	}
	if !strings.Contains(err.Error(), "outside every enabled provider") {
		t.Errorf("err = %v, want the operator refusal sentence", err)
	}
	// The upgrade pin at the launcher too: legacy open mode gets past admission
	// and reaches the fence claim, which is exactly how we know admission did not
	// refuse it — and, read the other way, that the refusal above happened BEFORE
	// the claim, so it cost no state and needed no release().
	if err := fire(types.SiteConfig{}, admitOffRow); !errors.Is(err, errAdmitFixtureReached) {
		t.Errorf("legacy open mode: err = %v, want the launch to reach the fence claim", err)
	}
}

// TestAdmissionAtTheRecordLauncher: the record session's clone URLs are derived
// by wireWorkspaceSource, a PURE function with no site config in scope, so the
// check lives at the launcher's call site — beside the agent-roster refusal, and
// before the CAS claim. recordLaunchRefusals is that pair.
func TestAdmissionAtTheRecordLauncher(t *testing.T) {
	h := newHarness(t)
	srv := New(baseTestConfig(h, &admitSourcesStore{siteConfig: admitSite()}))
	ws := types.Workspace{ID: uuid.New(),
		Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeRepo, Source: admitOffRow}}}

	err := srv.recordLaunchRefusals(context.Background(), ws, stepRunAgent)
	if !errors.Is(err, errRepoNotAdmitted) {
		t.Fatalf("err = %v, want errRepoNotAdmitted", err)
	}

	ws.Sources[0].Source = admitOnRow
	if err := srv.recordLaunchRefusals(context.Background(), ws, stepRunAgent); err != nil {
		t.Fatalf("a repository on the row must record: %v", err)
	}
}

// TestAdmissionLaunchRefusalStatusMatchesTheRequestDoors: a launcher's refusal is
// the policy answering, not a daemon fault, so it must not reach the 500 every
// other launcher error maps to — and the status has to be the SAME one the
// request doors write, or the two disagree about what the refusal costs.
func TestAdmissionLaunchRefusalStatusMatchesTheRequestDoors(t *testing.T) {
	// The disclosure split itself, on the pure function both halves key on.
	if got := admissionRefusalStatus(true); got != http.StatusUnprocessableEntity {
		t.Errorf("operator status = %d, want 422", got)
	}
	if got := admissionRefusalStatus(false); got != http.StatusForbidden {
		t.Errorf("member status = %d, want 403", got)
	}

	h := newHarness(t)
	srv := New(baseTestConfig(h, &admitSourcesStore{siteConfig: admitSite()}))
	err := srv.admitLauncherRepo(context.Background(), admitOffRow)
	if err == nil {
		t.Fatal("want a refusal")
	}
	w := httptest.NewRecorder()
	if !srv.writeAdmissionLaunchRefusal(w, requestWithToken(t, adminToken), err) {
		t.Fatal("writeAdmissionLaunchRefusal did not claim the error")
	}
	if w.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422: %s", w.Code, w.Body.String())
	}
	// The sentinel prefix is INTERNAL: it must never reach the body (the roster
	// refusal learned this one the hard way).
	if strings.Contains(w.Body.String(), errRepoNotAdmitted.Error()+":") {
		t.Errorf("body = %s, carries the internal sentinel prefix", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "outside every enabled provider") {
		t.Errorf("body = %s, want the refusal sentence verbatim", w.Body.String())
	}
	// Any other launcher error stays the caller's to map.
	if srv.writeAdmissionLaunchRefusal(httptest.NewRecorder(), requestWithToken(t, adminToken), errors.New("boom")) {
		t.Error("an unrelated error must not be claimed as an admission refusal")
	}
}

func requestWithToken(t *testing.T, token string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	return r
}

// TestStoreCreateRunCallerCensus is the pin that no launcher was missed. Every
// lane that creates a run either routes through a request door that admits, or
// admits at its own clone-URL derivation — and the only way to keep that true as
// lanes are added is to fail when the caller set changes.
//
// harnesscred.go (the managed-harness login capture) and site_config_probe.go
// (the upstream-proxy probe) clone NOTHING: both run a Wardyn-authored step with
// no workspace and no repo, which is why they carry no admission call.
func TestStoreCreateRunCallerCensus(t *testing.T) {
	want := []string{
		"harnesscred.go",          // clones nothing — the login capture
		"runs.go",                 // POST /runs (three admission sites above it)
		"site_config_probe.go",    // clones nothing — the proxy probe
		"source_scan.go",          // launchSourceScanRun — admits at its clone URL
		"workspace_run_launch.go", // launchRecordRun — admits at its call site
	}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read internal/api: %v", err)
	}
	var got []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, rerr := os.ReadFile(filepath.Join(".", name))
		if rerr != nil {
			t.Fatalf("read %s: %v", name, rerr)
		}
		if strings.Contains(string(src), "Store.CreateRun(") {
			got = append(got, name)
		}
	}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Errorf("Store.CreateRun callers = %v, want %v\n"+
			"a NEW lane that creates a run must admit the repository it is about to clone "+
			"(admitRepoSources at a request door, or admitLauncherRepo at its own clone-URL "+
			"derivation) — then add it here with a note saying which", got, want)
	}
}

// the lane veto

// laneVetoSite returns a policy whose row admits the host but permits only the
// ONE lane named — so every other lane is vetoed.
func laneVetoSite(kind types.GitProviderKind, baseURL string, permitted types.GitLane) types.SiteConfig {
	return providersConfig([]types.GitProvider{{
		ID: admitRowID, Kind: kind, BaseURLs: []string{baseURL}, Lanes: []types.GitLane{permitted},
	}})
}

func laneVetoAudit(t *testing.T, srv *Server) []map[string]any {
	t.Helper()
	rec, ok := srv.cfg.Audit.(*recRecorder)
	if !ok {
		t.Fatalf("audit recorder = %T, want *recRecorder", srv.cfg.Audit)
	}
	var out []map[string]any
	for _, ev := range rec.events {
		if ev.Action != "run.provider.lane_dropped" {
			continue
		}
		var d map[string]any
		if err := json.Unmarshal(ev.Data, &d); err != nil {
			t.Fatalf("unmarshal lane_dropped data: %v", err)
		}
		out = append(out, d)
	}
	return out
}

// TestProviderLaneVetoAtTheThreeGrantArms: providers mint nothing — they veto.
// Each arm drops its own wiring, warns on the response and audits; the grant ROW
// stays, because it is a record of what the policy asked for.
func TestProviderLaneVetoAtTheThreeGrantArms(t *testing.T) {
	ghScope := mustJSON(map[string]any{
		"repos": []string{admitOnRow}, "permissions": map[string]string{"contents": "read"},
	})
	for _, tc := range []struct {
		name   string
		sc     types.SiteConfig
		grant  types.GrantSpec
		lane   types.GitLane
		assert func(t *testing.T, gw grantWiring)
	}{
		{
			name:  "app",
			sc:    laneVetoSite(types.GitProviderGitHub, admitBaseURL, types.GitLanePAT),
			grant: types.GrantSpec{Kind: types.GrantGitHubToken, Scope: ghScope},
			lane:  types.GitLaneApp,
			assert: func(t *testing.T, gw grantWiring) {
				if gw.firstGitHubGrantID != nil || len(gw.gitGrants) != 0 {
					t.Errorf("github wiring = %v/%v, want none", gw.firstGitHubGrantID, gw.gitGrants)
				}
			},
		},
		{
			name:  "pat",
			sc:    laneVetoSite(types.GitProviderAzureDevOps, "https://dev.azure.com/acme", types.GitLaneSSH),
			grant: types.GrantSpec{Kind: types.GrantGitPAT, Scope: mustJSON(map[string]any{"host": "dev.azure.com", "secret_name": "ado-pat"})},
			lane:  types.GitLanePAT,
			assert: func(t *testing.T, gw grantWiring) {
				if len(gw.gitPATGrants) != 0 {
					t.Errorf("git_pat wiring = %v, want none", gw.gitPATGrants)
				}
				// The half a veto written anywhere else would have left behind:
				// nothing but that arm adds the ADO bundle.
				if len(gw.gitPATEgress) != 0 {
					t.Errorf("ADO egress = %v, want none — a dropped PAT lane drops its bundle with it", gw.gitPATEgress)
				}
			},
		},
		{
			name:  "ssh",
			sc:    laneVetoSite(types.GitProviderGitHub, admitBaseURL, types.GitLaneApp),
			grant: types.GrantSpec{Kind: types.GrantSSHKey, Scope: mustJSON(map[string]any{"host": "github.com", "key_secret_ref": "ssh-key-github.com"})},
			lane:  types.GitLaneSSH,
			assert: func(t *testing.T, gw grantWiring) {
				if len(gw.sshGrants) != 0 || len(gw.sshEgress) != 0 {
					t.Errorf("ssh wiring = %v/%v, want none", gw.sshGrants, gw.sshEgress)
				}
			},
		},
	} {
		t.Run(string(tc.lane), func(t *testing.T) {
			srv, st, _ := govEscapeFixture(t, &capStore{})
			st.siteConfig = tc.sc
			runID := uuid.New()
			gw, ok := srv.persistRunGrants(context.Background(), httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil), runID, time.Now().UTC(),
				types.RunPolicySpec{EligibleGrants: []types.GrantSpec{tc.grant}})
			if !ok {
				t.Fatal("persistRunGrants failed")
			}
			tc.assert(t, gw)
			if len(gw.warnings) != 1 || !strings.Contains(gw.warnings[0], string(tc.lane)) {
				t.Fatalf("warnings = %v, want one naming the %s lane", gw.warnings, tc.lane)
			}
			if !strings.Contains(gw.warnings[0], "was not wired") {
				t.Errorf("warning = %q, want the frozen ADMIT_LANE_DROPPED shape", gw.warnings[0])
			}
			rows := laneVetoAudit(t, srv)
			if len(rows) != 1 {
				t.Fatalf("audit rows = %v, want exactly one", rows)
			}
			if rows[0]["lane"] != string(tc.lane) || rows[0]["kind"] == "" {
				t.Errorf("audit row = %v, want lane %s named with its provider KIND", rows[0], tc.lane)
			}
			// A member can read their own run's audit trail, so the ROW ID is on the
			// far side of the disclosure rule even here.
			for _, field := range []string{"provider", "row", "id"} {
				if got, ok := rows[0][field]; ok && got == admitRowID {
					t.Errorf("audit row = %v, discloses the provider row id", rows[0])
				}
			}
		})
	}
}

// TestProviderLaneVetoPermitsWhatTheRowPermits: the veto narrows and never
// widens, so an EMPTY Lanes list (today's every deployment) and a row that names
// the lane both wire exactly what 0.7.1 wired.
func TestProviderLaneVetoPermitsWhatTheRowPermits(t *testing.T) {
	scope := mustJSON(map[string]any{"host": "dev.azure.com", "secret_name": "ado-pat"})
	for _, tc := range []struct {
		name string
		sc   types.SiteConfig
	}{
		{"no provider rows (the upgrade pin)", types.SiteConfig{}},
		{"a row with no lanes named", providersConfig([]types.GitProvider{
			adoRow(admitRowID, false, "https://dev.azure.com/acme")})},
		{"a row naming the lane", laneVetoSite(types.GitProviderAzureDevOps, "https://dev.azure.com/acme", types.GitLanePAT)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, st, _ := govEscapeFixture(t, &capStore{})
			st.siteConfig = tc.sc
			gw, ok := srv.persistRunGrants(context.Background(), httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil), uuid.New(), time.Now().UTC(),
				types.RunPolicySpec{EligibleGrants: []types.GrantSpec{{Kind: types.GrantGitPAT, Scope: scope}}})
			if !ok {
				t.Fatal("persistRunGrants failed")
			}
			if len(gw.gitPATGrants) != 1 {
				t.Errorf("git_pat wiring = %v, want the grant wired", gw.gitPATGrants)
			}
			if len(gw.warnings) != 0 {
				t.Errorf("warnings = %v, want none", gw.warnings)
			}
		})
	}
}

// TestProviderLaneVetoAtTheTwoLauncherGrantSites: the record/scan launchers mint
// their own clone credentials, outside persistRunGrants entirely. A veto there
// writes NO grant at all — an eligibility record nothing may mint would still set
// WARDYN_GITHUB_GRANT_ID and point the in-sandbox helper at a mint that can only
// fail.
func TestProviderLaneVetoAtTheTwoLauncherGrantSites(t *testing.T) {
	t.Run("app", func(t *testing.T) {
		srv, st, _ := govEscapeFixture(t, &capStore{})
		st.siteConfig = laneVetoSite(types.GitProviderGitHub, admitBaseURL, types.GitLanePAT)
		gid, err := srv.maybeGitHubReadGrant(context.Background(), uuid.New(), time.Now().UTC(),
			"https://github.com/acme/app.git")
		if err != nil || gid != nil {
			t.Fatalf("grant = %v, err = %v; want no grant and no error", gid, err)
		}
		if rows := laneVetoAudit(t, srv); len(rows) != 1 {
			t.Errorf("audit rows = %v, want exactly one — with no warnings channel, the row IS the record", rows)
		}
	})

	t.Run("ssh", func(t *testing.T) {
		srv, st, _ := govEscapeFixture(t, &capStore{})
		st.siteConfig = laneVetoSite(types.GitProviderGitHub, admitBaseURL, types.GitLaneApp)
		grants, err := srv.maybeSSHKeyGrant(context.Background(), uuid.New(), time.Now().UTC(),
			"git@github.com:acme/app.git")
		if err != nil || len(grants) != 0 {
			t.Fatalf("grants = %v, err = %v; want none", grants, err)
		}
		if rows := laneVetoAudit(t, srv); len(rows) != 1 {
			t.Errorf("audit rows = %v, want exactly one", rows)
		}
	})
}

// operatorContext / memberContext are the two tiers the disclosure rule splits
// on, as isOperator reads them: no OIDC human at all is the admin token (an
// operator), and an OIDC human whose role is not admin is a member.
func operatorContext() context.Context { return context.Background() }

func memberContext() context.Context {
	return withOIDCRole(withOIDCHuman(context.Background(), "sub-admit-member"), oidc.RoleMember)
}

// laneRow is a provider row with an explicit lane list.
func laneRow(id string, kind types.GitProviderKind, baseURL string, lanes ...types.GitLane) types.GitProvider {
	return types.GitProvider{ID: id, Kind: kind, BaseURLs: []string{baseURL}, Lanes: lanes}
}

// TestLaneVetoAsksTheROWThatAdmitted is the two-row case, in BOTH directions.
//
// Two rows of one kind are a supported configuration — a GHES row and a
// github.com row are both `kind: github`, and a strict org beside an open one on
// a single host is how an admin runs exactly this feature. Asking "which row
// claims this host" answers from whichever is listed first, which is wrong twice:
// it UNDER-vetoes (an open row permits a lane the admitting strict row forbids)
// and it OVER-vetoes (a GHES row decides github.com's lanes) — and at the two
// launcher sites the over-veto is invisible, because a record or scan clone
// simply runs without the credential it should have had.
func TestLaneVetoAsksTheROWThatAdmitted(t *testing.T) {
	// gh-strict admits github.com/acme and permits `app` only; gh-open admits the
	// rest of github.com and permits everything. Each case runs both orderings,
	// because "first match" must not be the answer either way round.
	strict := laneRow("gh-strict", types.GitProviderGitHub, "https://github.com/acme", types.GitLaneApp)
	open := laneRow("gh-open", types.GitProviderGitHub, "https://github.com")
	ghes := laneRow("ghes", types.GitProviderGitHub, "https://git.corp.example", types.GitLanePAT)

	t.Run("under-veto: a row that does not ADMIT must not permit the lane", func(t *testing.T) {
		// The GHES row CLAIMS github.com — rowClaimsHost is kind-wide on the two
		// well-known hosts, because a kind cannot be derived for a self-hosted
		// forge — but it admits nothing there. gh-strict is what admits acme/app,
		// and gh-strict permits `app` alone.
		for _, order := range [][]types.GitProvider{{ghes, strict}, {strict, ghes}} {
			srv, st, _ := govEscapeFixture(t, &capStore{})
			st.siteConfig = providersConfig(order)
			// The git_pat scope names only the host, so the decider is the row that
			// admitted THIS RUN's repository on it.
			gw, ok := srv.persistRunGrants(context.Background(), httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil), uuid.New(), time.Now().UTC(),
				types.RunPolicySpec{
					WorkspaceRepos: []types.WorkspaceRepo{{Repo: admitOnRow}},
					EligibleGrants: []types.GrantSpec{{Kind: types.GrantGitPAT,
						Scope: mustJSON(map[string]any{"host": "github.com", "secret_name": "gh-pat"})}},
				})
			if !ok {
				t.Fatal("persistRunGrants failed")
			}
			if len(gw.gitPATGrants) != 0 {
				t.Errorf("rows %s/%s: git_pat wiring = %v, want none — gh-strict admitted the repo and forbids `pat`",
					order[0].ID, order[1].ID, gw.gitPATGrants)
			}
		}
	})

	// And the same pair the other way up: when the OPEN row is what admits, its
	// answer is the right one. The lane tracks the admitting row — it is not a
	// second, stricter policy layered on top of admission.
	t.Run("the admitting row's answer is the answer", func(t *testing.T) {
		srv, st, _ := govEscapeFixture(t, &capStore{})
		st.siteConfig = providersConfig([]types.GitProvider{open, strict})
		gw, ok := srv.persistRunGrants(context.Background(), httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil), uuid.New(), time.Now().UTC(),
			types.RunPolicySpec{
				WorkspaceRepos: []types.WorkspaceRepo{{Repo: admitOnRow}},
				EligibleGrants: []types.GrantSpec{{Kind: types.GrantGitPAT,
					Scope: mustJSON(map[string]any{"host": "github.com", "secret_name": "gh-pat"})}},
			})
		if !ok {
			t.Fatal("persistRunGrants failed")
		}
		if len(gw.gitPATGrants) != 1 {
			t.Errorf("git_pat wiring = %v, want it wired — gh-open admits acme/app here and permits every lane", gw.gitPATGrants)
		}
	})

	t.Run("over-veto: a GHES row must not decide github.com's lanes", func(t *testing.T) {
		for _, order := range [][]types.GitProvider{{ghes, open}, {open, ghes}} {
			srv, st, _ := govEscapeFixture(t, &capStore{})
			st.siteConfig = providersConfig(order)
			// gh-open admits this clone URL and permits every lane; the GHES row
			// claims a different host entirely and must not be consulted.
			gid, err := srv.maybeGitHubReadGrant(context.Background(), uuid.New(), time.Now().UTC(),
				"https://github.com/other/app.git")
			if err != nil {
				t.Fatalf("rows %s/%s: %v", order[0].ID, order[1].ID, err)
			}
			if gid == nil {
				t.Errorf("rows %s/%s: no github grant — a GHES row vetoed a lane on a host it does not admit",
					order[0].ID, order[1].ID)
			}
		}
	})

	// The host-only fallback, for a grant naming no repository at all: permit what
	// ANY claiming row permits, never the first row's answer alone.
	t.Run("host-only fallback permits the union", func(t *testing.T) {
		sc := providersConfig([]types.GitProvider{strict, open})
		if rows := laneRowsForGrantHost(sc, "github.com", nil); len(rows) != 2 {
			t.Fatalf("rows = %v, want both claiming rows", rows)
		}
		srv, st, _ := govEscapeFixture(t, &capStore{})
		st.siteConfig = sc
		gw, ok := srv.persistRunGrants(context.Background(), httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil), uuid.New(), time.Now().UTC(),
			types.RunPolicySpec{EligibleGrants: []types.GrantSpec{{Kind: types.GrantGitPAT,
				Scope: mustJSON(map[string]any{"host": "github.com", "secret_name": "gh-pat"})}}})
		if !ok {
			t.Fatal("persistRunGrants failed")
		}
		if len(gw.gitPATGrants) != 1 {
			t.Errorf("git_pat wiring = %v, want it wired — gh-open permits `pat` and the run names no repo", gw.gitPATGrants)
		}
	})

	t.Run("a host no row claims, and a disabled row, decide nothing", func(t *testing.T) {
		sc := laneVetoSite(types.GitProviderGitHub, admitBaseURL, types.GitLanePAT)
		if rows := claimingRows(sc, "gitlab.example.com"); len(rows) != 0 {
			t.Errorf("rows = %v, want none for a host no row claims", rows)
		}
		// A disabled row refuses ADMISSION outright, so no run reaches a lane
		// question through one.
		if rows := claimingRows(admitDisabledSite(), "github.com"); len(rows) != 0 {
			t.Errorf("rows = %v, want none — a disabled row must not answer a lane question", rows)
		}
	})
}

// TestLaneDropWarningNamesTheRowForOperatorsOnly: the 201 warning reaches
// whoever created the run, so it is held to the same disclosure rule the
// refusals are — an operator is told which row to edit, a member the kind.
func TestLaneDropWarningNamesTheRowForOperatorsOnly(t *testing.T) {
	rows := claimingRows(laneVetoSite(types.GitProviderGitHub, admitBaseURL, types.GitLanePAT), "github.com")
	srv, _, _ := govEscapeFixture(t, &capStore{})

	msg, vetoed := srv.laneVetoed(operatorContext(), uuid.New(), types.GitLaneApp, types.GrantGitHubToken, rows)
	if !vetoed {
		t.Fatal("want a veto")
	}
	if !strings.Contains(msg, admitRowID) {
		t.Errorf("operator warning = %q, want the row %q named", msg, admitRowID)
	}
	memberMsg, vetoed := srv.laneVetoed(memberContext(), uuid.New(), types.GitLaneApp, types.GrantGitHubToken, rows)
	if !vetoed {
		t.Fatal("want a veto")
	}
	if strings.Contains(memberMsg, admitRowID) {
		t.Errorf("member warning = %q, must not name the provider row", memberMsg)
	}
	if !strings.Contains(memberMsg, "github") {
		t.Errorf("member warning = %q, want the provider kind named", memberMsg)
	}
}

// TestLegacyHostAdmissionIsAuditedAtEveryDoor: admitting on the one-release
// grace is neither a refusal nor ordinary, and only run create's 201 can say so
// out loud — so every door records it. Without this, onboarding a GitLab
// workspace through the wizard succeeds in complete silence and the admin first
// hears about the host when 0.8 stops admitting it.
func TestLegacyHostAdmissionIsAuditedAtEveryDoor(t *testing.T) {
	legacyHostRows := func(srv *Server) []string {
		rec, ok := srv.cfg.Audit.(*recRecorder)
		if !ok {
			t.Fatalf("audit recorder = %T, want *recRecorder", srv.cfg.Audit)
		}
		var out []string
		for _, ev := range rec.events {
			if ev.Action == "workspace.provider.legacy_host" {
				out = append(out, ev.Target)
			}
		}
		return out
	}

	t.Run("POST /workspaces", func(t *testing.T) {
		srv, st, _ := ownerHarness(t, runner.MemberMountPolicy{})
		st.siteConfig = admitLegacySite()
		w := doSSO(t, srv, http.MethodPost, "/api/v1/workspaces", admitAdminSession(t),
			`{"name":"app","sources":[{"type":"repo","source":`+quote(admitUnclaimed)+`}]}`)
		assertNotAdmissionRefused(t, w, "an unclaimed legacy host stays launchable for one release")
		if got := legacyHostRows(srv); len(got) != 1 || got[0] != admitLegacyHostName {
			t.Errorf("legacy_host rows = %v, want one naming %q", got, admitLegacyHostName)
		}
		// …AND the 201 says it out loud (V2/F1). The audit row alone told the
		// trail and nobody else: the admin who can enable a provider row before
		// 0.8 is the person standing at THIS door, and until this the sentence
		// reached only whoever launched the first run against the workspace.
		assertLegacyHostWarned(t, w, `"name":"app"`)
	})

	t.Run("PUT /workspaces/{id} — an edit is how a source MOVES onto a legacy host", func(t *testing.T) {
		srv, st, _ := ownerHarness(t, runner.MemberMountPolicy{})
		st.siteConfig = admitLegacySite()
		sess := admitAdminSession(t)
		created := doSSO(t, srv, http.MethodPost, "/api/v1/workspaces", sess,
			`{"name":"app","sources":[{"type":"repo","source":`+quote(admitOnRow)+`}]}`)
		var row struct {
			ID       string   `json:"id"`
			Warnings []string `json:"warnings"`
		}
		if err := json.Unmarshal(created.Body.Bytes(), &row); err != nil || row.ID == "" {
			t.Fatalf("create: err=%v body=%s", err, created.Body.String())
		}
		if len(row.Warnings) != 0 {
			t.Errorf("a repo ON a provider row earned warnings %v, want none", row.Warnings)
		}
		w := doSSO(t, srv, http.MethodPut, "/api/v1/workspaces/"+row.ID, sess,
			`{"name":"app","sources":[{"type":"repo","source":`+quote(admitUnclaimed)+`}]}`)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
		}
		assertLegacyHostWarned(t, w, `"name":"app"`)
	})

	t.Run("POST /sources", func(t *testing.T) {
		srv, st, _ := ownerHarness(t, runner.MemberMountPolicy{})
		st.siteConfig = admitLegacySite()
		w := doSSO(t, srv, http.MethodPost, "/api/v1/sources", admitAdminSession(t),
			`{"kind":"repo","locator":`+quote(admitUnclaimed)+`}`)
		assertNotAdmissionRefused(t, w, "the library door admits the legacy host too")
		assertLegacyHostWarned(t, w, `"kind":"repo"`)
	})

	t.Run("the scan launcher", func(t *testing.T) {
		h := newHarness(t)
		st := &admitSourcesStore{siteConfig: admitLegacySite()}
		srv := New(baseTestConfig(h, st))
		_, err := srv.launchSourceScanRun(context.Background(), "admin",
			types.Source{ID: uuid.New(), Kind: types.SourceRepo, Locator: admitUnclaimed})
		if !errors.Is(err, errAdmitFixtureReached) {
			t.Fatalf("err = %v, want the launch to reach the fence claim", err)
		}
		if got := legacyHostRows(srv); len(got) != 1 {
			t.Errorf("legacy_host rows = %v, want one", got)
		}
	})

	// ORDINARY admission says nothing: a repo on a provider row is not news, and
	// a trail that records every clone is a trail nobody reads (B5).
	t.Run("a repo on a row is not audited", func(t *testing.T) {
		srv, st, _ := ownerHarness(t, runner.MemberMountPolicy{})
		st.siteConfig = admitLegacySite()
		doSSO(t, srv, http.MethodPost, "/api/v1/workspaces", admitAdminSession(t),
			`{"name":"app","sources":[{"type":"repo","source":`+quote(admitOnRow)+`}]}`)
		if got := legacyHostRows(srv); len(got) != 0 {
			t.Errorf("legacy_host rows = %v, want none", got)
		}
	})
}

// assertLegacyHostWarned is the F1 assertion: the frozen ADMIT_LEGACY_HOST
// sentence rides this door's success body, and the row it wraps is still spelled
// at the TOP LEVEL — the envelope embeds the row precisely so every existing
// Workspace/Source decoder is unaffected, and a nested one would be a silent
// wire break no warning assertion would catch.
func assertLegacyHostWarned(t *testing.T, w *httptest.ResponseRecorder, wantFlattened string) {
	t.Helper()
	body := w.Body.String()
	if want := fmt.Sprintf(admitLegacyHost, admitLegacyHostName); !strings.Contains(body, want) {
		t.Errorf("body = %s, want the ADMIT_LEGACY_HOST warning %q", body, want)
	}
	if !strings.Contains(body, wantFlattened) {
		t.Errorf("body = %s, want the row's own field %s still at the top level", body, wantFlattened)
	}
}

// the `admitted` projection

func admittedFlags(t *testing.T, body string) []*bool {
	t.Helper()
	var one struct {
		Sources []types.WorkspaceSource `json:"sources"`
	}
	if err := json.Unmarshal([]byte(body), &one); err == nil && one.Sources != nil {
		return flagsOf(one.Sources)
	}
	var page []struct {
		Sources []types.WorkspaceSource `json:"sources"`
	}
	if err := json.Unmarshal([]byte(body), &page); err != nil {
		t.Fatalf("unmarshal %s: %v", body, err)
	}
	var out []*bool
	for _, ws := range page {
		out = append(out, flagsOf(ws.Sources)...)
	}
	return out
}

func flagsOf(sources []types.WorkspaceSource) []*bool {
	out := make([]*bool, 0, len(sources))
	for _, src := range sources {
		out = append(out, src.Admitted)
	}
	return out
}

// TestAdmittedProjectionOnBothReads: the console renders "not an enabled git
// provider" from the SERVER's verdict, on the list and the single row alike —
// one document, one projection.
func TestAdmittedProjectionOnBothReads(t *testing.T) {
	srv, st, _ := ownerHarness(t, runner.MemberMountPolicy{})
	st.siteConfig = admitSite()
	admin := admitAdminSession(t)
	id := st.put(types.Workspace{Sources: []types.WorkspaceSource{
		{Type: types.WorkspaceSourceTypeRepo, Source: admitOnRow},
		{Type: types.WorkspaceSourceTypeRepo, Source: admitOffRow},
		{Type: types.WorkspaceSourceTypeEphemeral, Target: "/home/agent/work"},
	}})

	for _, path := range []string{"/api/v1/workspaces", "/api/v1/workspaces/" + id.String()} {
		t.Run(path, func(t *testing.T) {
			w := doSSO(t, srv, http.MethodGet, path, admin, "")
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d: %s", w.Code, w.Body.String())
			}
			flags := admittedFlags(t, w.Body.String())
			if len(flags) != 3 {
				t.Fatalf("flags = %v, want one per source", flags)
			}
			if flags[0] == nil || !*flags[0] {
				t.Errorf("source on the row: admitted = %v, want true", flags[0])
			}
			if flags[1] == nil || *flags[1] {
				t.Errorf("source outside the row: admitted = %v, want false", flags[1])
			}
			// An ephemeral source clones nothing; a flag on it would invite the
			// console to render a provider state for a scratch dir.
			if flags[2] != nil {
				t.Errorf("ephemeral source: admitted = %v, want absent", *flags[2])
			}
		})
	}
}

// TestAdmittedProjectionAbsentInLegacyOpenMode is the byte-identical upgrade pin
// on the READ side: with no provider rows the key is absent from every response,
// asserted by EQUALITY against the pre-feature document rather than by eyeballing
// a missing field.
func TestAdmittedProjectionAbsentInLegacyOpenMode(t *testing.T) {
	srv, st, _ := ownerHarness(t, runner.MemberMountPolicy{})
	admin := admitAdminSession(t)
	id := st.put(types.Workspace{Sources: []types.WorkspaceSource{
		{Type: types.WorkspaceSourceTypeRepo, Source: admitOnRow},
	}})

	for _, path := range []string{"/api/v1/workspaces", "/api/v1/workspaces/" + id.String()} {
		got := doSSO(t, srv, http.MethodGet, path, admin, "").Body.String()
		if strings.Contains(got, `"admitted"`) {
			t.Errorf("%s = %s, must carry no admitted key in legacy open mode", path, got)
		}
	}
}

// TestAdmittedIsNeverTakenFromAWriteBody: the projection is the server's, so a
// client round-tripping a GET cannot persist a provider verdict.
func TestAdmittedIsNeverTakenFromAWriteBody(t *testing.T) {
	srv, st, _ := ownerHarness(t, runner.MemberMountPolicy{})
	st.siteConfig = admitSite()
	w := doSSO(t, srv, http.MethodPost, "/api/v1/workspaces", admitAdminSession(t),
		`{"name":"app","sources":[{"type":"repo","source":"`+admitOffRow+`","admitted":true}]}`)
	// The body's claim changes nothing: admission still refuses the repo.
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422: %s", w.Code, w.Body.String())
	}
	all, err := st.ListWorkspaces(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 0 {
		t.Errorf("stored %d workspaces, want none", len(all))
	}
}

// TestSSHHostLevelAdmissionIsNeverSilent pins the MEDIUM of V1 lens A. The
// host-level SSH ceiling STAYS (an SSH clone URL carries no path to compare), so
// what is pinned here is that it is said out loud rather than closed: a run whose
// SSH repository slipped a path-scoped row's org bound earns the 201 warning and
// the run.provider.ssh_host_level audit row — the one admission outcome that is
// WIDER than the policy an admin wrote.
func TestSSHHostLevelAdmissionIsNeverSilent(t *testing.T) {
	runID := uuid.New()
	fire := func(t *testing.T, sc types.SiteConfig, repo string) ([]string, *recRecorder) {
		t.Helper()
		srv, st, audit := govEscapeFixture(t, &capStore{})
		st.siteConfig = sc
		return srv.sshHostLevelWarnings(t.Context(), runID, repo), audit
	}

	t.Run("another org over SSH under a path-scoped row: warned and audited", func(t *testing.T) {
		warnings, audit := fire(t, admitSite(), "git@github.com:other/app.git")
		if len(warnings) != 1 || !strings.Contains(warnings[0], "SSH at the host level") {
			t.Fatalf("warnings = %#v, want the ADMIT.SSH_HOST_LEVEL sentence", warnings)
		}
		// The disclosure rule: the KIND, never the row id (a member reads their
		// own run's warnings and its audit rows alike).
		if strings.Contains(warnings[0], admitRowID) {
			t.Errorf("warning = %q, must not name the row id", warnings[0])
		}
		var found bool
		for _, ev := range audit.events {
			if ev.Action == "run.provider.ssh_host_level" {
				found = true
				if ev.Target != string(types.GitProviderGitHub) {
					t.Errorf("audit target = %q, want the provider KIND", ev.Target)
				}
			}
		}
		if !found {
			t.Errorf("events = %#v, want a run.provider.ssh_host_level row", audit.events)
		}
	})

	t.Run("an HTTPS clone of the same repo is refused, not warned", func(t *testing.T) {
		// The ceiling is SSH-only: over https the org path binds, so this repo
		// never reaches a warning — it is refused by admission upstream.
		if _, ok := providerFor(admitSite(), repoCloneURL("other/app")); ok {
			t.Fatal("an https clone outside the org path must not be admitted at all")
		}
		warnings, _ := fire(t, admitSite(), "other/app")
		if len(warnings) != 0 {
			t.Errorf("warnings = %#v, want none for an https target", warnings)
		}
	})

	t.Run("a bare-host row widened nothing, so it says nothing", func(t *testing.T) {
		sc := providersConfig([]types.GitProvider{githubRow(admitRowID, false, "https://github.com")})
		if warnings, _ := fire(t, sc, "git@github.com:other/app.git"); len(warnings) != 0 {
			t.Errorf("warnings = %#v, want none — the row bounds the whole host anyway", warnings)
		}
	})

	t.Run("legacy open mode says nothing", func(t *testing.T) {
		if warnings, _ := fire(t, types.SiteConfig{}, "git@github.com:other/app.git"); len(warnings) != 0 {
			t.Errorf("warnings = %#v, want none with no provider rows", warnings)
		}
	})
}
