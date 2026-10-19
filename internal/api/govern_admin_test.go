// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/ghscope"
	"github.com/cjohnstoniv/wardyn/internal/runnerpool"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Constrained-admin mode (WARDYN_GOVERN_ADMIN_RUNS): every run seam that used to
// ask isOperator asks runUngoverned, so with the switch on an SSO admin session
// and an admin-role personal token get the member outcome, while the admin
// token and local mode (no person) stay ungoverned.

// governAdminSub is the admin's sub, and also the owner of the end-wait
// fixture's run, so one identity can drive every probe below.
const governAdminSub = endWaitOwner

// governAdminSessionCtx is what humanOrAdminAuth publishes for an admin's SSO
// session.
func governAdminSessionCtx() context.Context {
	return withOIDCGroupsTruncated(withOIDCGroups(
		operatorCtx(governAdminSub, "owner@corp.example", oidc.RoleAdmin), []string{"eng"}), false)
}

// governAdminTokenCtx is what apiTokenAuth publishes for an admin-role personal
// token, captured from the real middleware rather than rebuilt by hand.
func governAdminTokenCtx(t *testing.T) context.Context {
	t.Helper()
	srv, st, _ := apiTokenTestServer(t)
	const raw = apiTokenPrefix + "govern-admin"
	if _, err := st.CreateAPIToken(context.Background(), types.APIToken{
		ID: uuid.New(), Principal: governAdminSub, Email: "owner@corp.example", Role: oidc.RoleAdmin,
		UserType: types.UserTypeStandard, Groups: []string{"eng"}, GroupsTruncated: new(bool), Name: "ci",
	}, raw); err != nil {
		t.Fatalf("seed token: %v", err)
	}
	var got context.Context
	capture := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { got = r.Context() })
	deny := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("token fell through to the admin path") })
	r := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	r.Header.Set("Authorization", "Bearer "+raw)
	srv.apiTokenAuth(capture, deny).ServeHTTP(httptest.NewRecorder(), r)
	if got == nil {
		t.Fatal("token auth never reached the next handler")
	}
	return got
}

func governReq(ctx context.Context) *http.Request {
	return httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil).WithContext(ctx)
}

// governActiveStore answers the one read personAllowance makes: the caller's active-run count.
type governActiveStore struct{ *capStore }

func (governActiveStore) CountActiveRunsBy(context.Context, string) (int, error) { return 2, nil }

// governSite is one swap site: build makes its server, and governed reports
// whether the caller got the MEMBER outcome there.
type governSite struct {
	name     string
	build    func(t *testing.T) *Server
	governed func(t *testing.T, s *Server, ctx context.Context) bool
}

func governSites() []governSite {
	walled := func(l types.GovernanceLimits) governanceCeiling {
		return governanceCeiling{Limits: l, Profile: &ResolvedProfile{ID: uuid.New(), Name: "walled", Limits: l}}
	}
	wide := types.RunPolicySpec{
		MinConfinementClass: types.CC1,
		AllowedDomains:      []string{"api.anthropic.com", "evil.example"},
	}
	capSrv := func(cs *capStore) func(*testing.T) *Server {
		return func(*testing.T) *Server { return New(Config{Store: cs, Audit: &recRecorder{}}) }
	}
	boundSrv := func(t *testing.T) *Server { s, _, _ := memberBoundFixture(t, wide); return s }
	plain := func(*testing.T) *Server { return New(Config{Audit: &recRecorder{}}) }
	admitSrv := func(*testing.T) *Server {
		return New(Config{Store: &admitSourcesStore{siteConfig: admitSite()}, Audit: &recRecorder{}})
	}
	activeSrv := func(*testing.T) *Server {
		return New(Config{Store: governActiveStore{&capStore{}}, Audit: &recRecorder{}})
	}
	driveProfile := &types.GovernanceProfile{ID: uuid.New(), Name: "walled", Limits: types.GovernanceLimits{DenyUserDrive: true}}
	ownerLaunch := func(s *Server, ctx context.Context) bool {
		run := types.AgentRun{ID: uuid.New(), CreatedBy: governAdminSub, Agent: "claude-code"}
		ref, err := s.ownerCapabilityRefusal(ctx, run, true, nil)
		return err == nil && ref != nil
	}

	return []governSite{
		{"governance.go resolveEffectiveCeiling", capSrv(&capStore{govProfile: govProfile("walled"), govTier: types.CapabilitySubjectUser}),
			func(t *testing.T, s *Server, ctx context.Context) bool {
				got, err := s.effectiveCeiling(ctx)
				return err == nil && got.Profile != nil
			}},
		{"runs_create_validate.go denyUserRequest", capSrv(&capStore{enf: map[string]bool{capAgent: true}}),
			func(t *testing.T, s *Server, ctx context.Context) bool {
				_, refused := s.denyUserRequest(httptest.NewRecorder(), governReq(ctx), createRunRequest{Agent: "claude-code"})
				return refused
			}},
		{"inline_policy.go resolveRunPolicy inline clamp", boundSrv,
			func(t *testing.T, s *Server, ctx context.Context) bool {
				in := wide
				spec, _, _, _, ok := s.resolveRunPolicy(ctx, httptest.NewRecorder(), governReq(ctx),
					&createRunRequest{Agent: "claude-code", Repo: "acme/widgets", InlinePolicy: &in}, false)
				return ok && spec.MinConfinementClass == types.CC2
			}},
		{"inline_policy.go resolveRunPolicy stored bound", plain,
			func(t *testing.T, s *Server, ctx context.Context) bool {
				// The stored row's id is only known to the fixture that made it.
				srv, _, id := memberBoundFixture(t, wide)
				srv.cfg.GovernAdminRuns = s.cfg.GovernAdminRuns
				spec, _, _, _, ok := srv.resolveRunPolicy(ctx, httptest.NewRecorder(), governReq(ctx),
					&createRunRequest{Agent: "claude-code", Repo: "acme/widgets", PolicyID: &id}, false)
				return ok && spec.MinConfinementClass == types.CC2
			}},
		{"runs_dispatch_ceiling.go boundResources", plain,
			func(t *testing.T, s *Server, ctx context.Context) bool {
				c := walled(types.GovernanceLimits{})
				// Above the deployment's default: the member cap is never below it (#1949).
				c.Limits.MaxCPUMillis = 3000
				spec := types.RunPolicySpec{Resources: &types.ResourceLimits{CPUMillis: 4000}}
				// The cut is the outcome; stepResources, not this bound, says it was cut.
				s.boundResources(ctx, governReq(ctx), &spec, c)
				return spec.Resources.CPUMillis == 3000
			}},
		{"runs_policy.go resolvePolicy", plain,
			func(t *testing.T, s *Server, ctx context.Context) bool {
				c := governanceCeiling{Spec: types.RunPolicySpec{AzureDevOpsCapabilities: []adoscope.Capability{"code_read"},
					GitHubCapabilities: []ghscope.Capability{"code_read"}}}
				spec, _, _, err := s.resolvePolicy(ctx, nil, c)
				return err == nil && len(spec.AzureDevOpsCapabilities) == 0 && len(spec.GitHubCapabilities) == 0
			}},
		{"governance_run_doors.go boundUIApps", plain,
			func(t *testing.T, s *Server, ctx context.Context) bool {
				spec := types.RunPolicySpec{UIApps: []types.UIApp{{Name: "code", Port: 8080}}}
				warns := s.boundUIApps(ctx, governReq(ctx), &spec, walled(types.GovernanceLimits{DenyUIApps: true}), false)
				return len(warns) > 0 && len(spec.UIApps) == 0
			}},
		{"capabilities.go newCapBatch", capSrv(&capStore{enf: map[string]bool{capAgent: true}}),
			func(t *testing.T, s *Server, ctx context.Context) bool {
				ok, err := s.newCapBatch(ctx).allowed(ctx, capAgent, "claude-code")
				return err == nil && !ok
			}},
		{"capabilities.go capAllowed (no store)", plain,
			func(t *testing.T, s *Server, ctx context.Context) bool {
				_, err := s.capAllowed(ctx, capAgent, "claude-code")
				return err != nil
			}},
		{"user_drives_run.go driveDoorProfile", plain,
			func(t *testing.T, s *Server, ctx context.Context) bool {
				_, shut := s.driveDoorProfile(ctx, walled(types.GovernanceLimits{DenyUserDrive: true}))
				return shut
			}},
		{"user_drives_me.go userDriveDeniedByProfile", capSrv(&capStore{govProfile: driveProfile, govTier: types.CapabilitySubjectUser}),
			func(t *testing.T, s *Server, ctx context.Context) bool {
				name, _ := s.userDriveDeniedByProfile(governReq(ctx))
				return name != ""
			}},
		{"workspace_providers.go denyUserWorkspaceProviders", capSrv(&capStore{site: capProviderSite(), enf: capProviderEnforced()}),
			func(t *testing.T, s *Server, ctx context.Context) bool {
				return s.denyUserWorkspaceProviders(httptest.NewRecorder(), governReq(ctx), "runs.workspace_provider", capProviderRepo)
			}},
		// workspace_admission.go changes wording and status, never the decision:
		// every probe below also asserts the repository is refused either way.
		{"workspace_admission.go admitRepoSources", admitSrv,
			func(t *testing.T, s *Server, ctx context.Context) bool {
				w := httptest.NewRecorder()
				if !s.admitRepoSources(w, governReq(ctx), admitOffRow) {
					t.Fatal("the repository was admitted: the decision must not change")
				}
				return w.Code == http.StatusForbidden
			}},
		{"workspace_admission.go admitLauncherRepo", admitSrv,
			func(t *testing.T, s *Server, ctx context.Context) bool {
				err := s.admitLauncherRepo(ctx, admitOffRow)
				if err == nil {
					t.Fatal("the repository was admitted: the decision must not change")
				}
				return strings.Contains(err.Error(), admitMember)
			}},
		{"workspace_admission.go writeAdmissionLaunchRefusal", plain,
			func(t *testing.T, s *Server, ctx context.Context) bool {
				w := httptest.NewRecorder()
				if !s.writeAdmissionLaunchRefusal(w, governReq(ctx), fmt.Errorf("%w: x", errRepoNotAdmitted)) {
					t.Fatal("the refusal was not claimed")
				}
				return w.Code == http.StatusForbidden
			}},
		{"workspace_admission.go laneVetoedBy", func(t *testing.T) *Server { s, _, _ := govEscapeFixture(t, &capStore{}); return s },
			func(t *testing.T, s *Server, ctx context.Context) bool {
				rows := claimingRows(laneVetoSite(types.GitProviderGitHub, admitBaseURL, types.GitLanePAT), "github.com")
				msg, vetoed := s.laneVetoed(ctx, uuid.New(), types.GitLaneApp, types.GrantGitHubToken, rows)
				if !vetoed {
					t.Fatal("the lane was not vetoed: the decision must not change")
				}
				return !strings.Contains(msg, admitRowID)
			}},
		{"run_end_wait.go handleSetRunEndAndWait (the end bound)", nil,
			func(t *testing.T, s *Server, ctx context.Context) bool {
				f := newEndWaitFixture(t, types.RunLimits{MaxEndAheadSec: 30 * 86400})
				f.srv.cfg.GovernAdminRuns = s.cfg.GovernAdminRuns
				code, out := governPatchEnd(t, f, ctx, f.now.Add(90*24*time.Hour))
				return code == http.StatusOK && slices.Equal(out.Capped, []string{"ends_at"})
			}},
		{"run_end_wait.go -> run_owner_authority.go extension re-check", nil,
			func(t *testing.T, s *Server, ctx context.Context) bool {
				f := newEndWaitFixture(t, types.RunLimits{MaxEndAheadSec: 30 * 86400})
				f.srv.cfg.GovernAdminRuns = s.cfg.GovernAdminRuns
				f.st.enf = map[string]bool{capAgent: true}
				code, _ := governPatchEnd(t, f, ctx, f.now.Add(7*24*time.Hour))
				return code == http.StatusForbidden
			}},
		{"run_owner_authority.go ownerCapabilityRefusal (revive, restart)", capSrv(&capStore{enf: map[string]bool{capAgent: true}}),
			func(t *testing.T, s *Server, ctx context.Context) bool { return ownerLaunch(s, ctx) }},
		{"presets.go presetLaunchOpenTo", plain,
			func(t *testing.T, s *Server, ctx context.Context) bool {
				p := types.LaunchPreset{Name: "other", UserTypes: []string{"some-other-type"}}
				return !s.presetLaunchOpenTo(governReq(ctx), p)
			}},
		// The governed outcome is a clamp: the built-in's write access is lowered by a read-only
		// ceiling, so the grant is refused rather than issued with less than was asked.
		{"components_builtin.go builtinGitHubGrant", plain,
			func(t *testing.T, s *Server, ctx context.Context) bool {
				ceiling := governanceCeiling{Spec: types.RunPolicySpec{EligibleGrants: []types.GrantSpec{{
					Kind: types.GrantGitHubToken, Scope: json.RawMessage(`{"repos":[],"permissions":{"contents":"read"}}`)}}}}
				_, ok := s.builtinGitHubGrant(governReq(ctx), ceiling, []string{"acme/api"}, "write")
				return !ok
			}},
		// Governance joins the folded sources only for a caller whose runs it governs.
		{"run_limit_sources.go limitSources", plain,
			func(t *testing.T, s *Server, ctx context.Context) bool {
				sources, refusal, err := s.limitSources(ctx, types.RunnerPoolRunBackground, nil,
					walled(types.GovernanceLimits{MaxConcurrentRuns: 1}), types.ResourceLimits{}, nil)
				if refusal != nil || err != nil {
					t.Fatalf("limitSources = %v, %v", refusal, err)
				}
				return slices.ContainsFunc(sources, func(l runnerpool.SourceLimits) bool { return l.Source == types.LimitSourceGovernance })
			}},
		{"runner_pools_catalogue.go personAllowance", activeSrv,
			func(t *testing.T, s *Server, ctx context.Context) bool {
				allowance, refusal := s.personAllowance(governReq(ctx), walled(types.GovernanceLimits{MaxConcurrentRuns: 3}))
				if refusal != nil {
					t.Fatalf("personAllowance refused: %+v", refusal)
				}
				return allowance != nil
			}},
		{"run_policy_overlay.go applyPolicyOverlay", boundSrv,
			func(t *testing.T, s *Server, ctx context.Context) bool {
				ceiling, err := s.effectiveCeiling(ctx)
				if err != nil {
					t.Fatalf("effectiveCeiling: %v", err)
				}
				req := &createRunRequest{Agent: "claude-code", PolicyOverlay: json.RawMessage(`{"allowed_domains":["api.anthropic.com","evil.example"]}`)}
				// A valid base: the merged spec is validated, and an empty one is refused for every caller.
				base := types.RunPolicySpec{MinConfinementClass: types.CC1}
				spec, _, refusal := s.applyPolicyOverlay(ctx, governReq(ctx), base, req, ceiling, true, &policySourceRecord{})
				if refusal != nil {
					t.Fatalf("applyPolicyOverlay refused: status %d %+v", refusal.status, refusal.body)
				}
				return !slices.Contains(spec.AllowedDomains, "evil.example")
			}},
		{"run_end_create.go resolveCreateEnd", plain,
			func(t *testing.T, s *Server, ctx context.Context) bool {
				asked := 7200
				end, refusal := s.resolveCreateEnd(governReq(ctx), createRunRequest{EndsAfterSec: &asked},
					walled(types.GovernanceLimits{RunLimits: types.RunLimits{MaxEndAheadSec: 3600}}), nil)
				return refusal == nil && end.capped
			}},
		{"member_drives.go memberDriveDoorRefusal", plain,
			func(t *testing.T, s *Server, ctx context.Context) bool {
				return s.memberDriveDoorRefusal(governReq(ctx), walled(types.GovernanceLimits{DenyMemberDrives: true})) != nil
			}},
		// repoAdmissionRefusal changes the status only, never the decision.
		{"run_workspace_authorization.go repoAdmissionRefusal", admitSrv,
			func(t *testing.T, s *Server, ctx context.Context) bool {
				refusal := s.repoAdmissionRefusal(governReq(ctx), admitOffRow)
				if refusal == nil {
					t.Fatal("the repository was admitted: the decision must not change")
				}
				return refusal.status == http.StatusForbidden
			}},
		{"run_workspace_authorization.go workspaceProvidersRefusal", capSrv(&capStore{site: capProviderSite(), enf: capProviderEnforced()}),
			func(t *testing.T, s *Server, ctx context.Context) bool {
				return s.workspaceProvidersRefusal(governReq(ctx), "runs.workspace_provider", capProviderRepo) != nil
			}},
		{"run_request_governance.go runRequestGovernance", plain,
			func(t *testing.T, s *Server, ctx context.Context) bool {
				_, refusal := s.runRequestGovernance(governReq(ctx), createRunRequest{DevcontainerRepo: "acme/devcontainer"})
				return refusal != nil
			}},
	}
}

// governSiteElsewhere names how each runUngoverned caller that no governSites row is named after is
// covered: "row: <name>" a governSites row that reaches it through its caller, "test: <file>" a
// file of switch-on tests that asserts it, or "none: <why>" a site no test pins yet. TestGovernAdminRuns_
// EverySiteIsListed holds the table to the call sites, so a new site is a failure until it is here or
// has a row, and an entry whose function no longer asks runUngoverned is a failure too.
var governSiteElsewhere = map[string]string{
	"resolveRunPolicyFacts":   "row: inline_policy.go resolveRunPolicy inline clamp",
	"resolveChainCeiling":     "row: governance.go resolveEffectiveCeiling",
	"wallsEndPlan":            "row: run_end_wait.go handleSetRunEndAndWait (the end bound)",
	"memberResources":         "row: runs_dispatch_ceiling.go boundResources",
	"recordLaneVeto":          "row: workspace_admission.go laneVetoedBy",
	"deniedPlacements":        "test: run_placement_capabilities_test.go",
	"handleRecordGoverned":    "test: record_govern_admin_test.go",
	"markGovernanceExempt":    "test: govern_admin_doors_test.go",
	"meGovernanceContact":     "none: /me display read, no switch-on test",
	"refuseNoTicketAttach":    "none: attach entry door, no switch-on test",
	"unionDirectGitHubEgress": "none: direct GitHub egress union, no switch-on test",
}

// runUngovernedCallers is every function in dir's non-test sources that calls
// runUngoverned, found by walking the syntax so a call spelled any way is counted.
func runUngovernedCallers(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	fset := token.NewFileSet()
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(dir, e.Name()), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", e.Name(), err)
		}
		for _, d := range file.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil || fd.Name.Name == "runUngoverned" {
				continue
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				if call, ok := n.(*ast.CallExpr); ok {
					if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "runUngoverned" {
						seen[fd.Name.Name] = true
					}
				}
				return true
			})
		}
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// governSiteGaps is every way callers (the functions that ask runUngoverned) and the coverage
// (rowNames: the governSites rows; elsewhere: governSiteElsewhere) disagree.
func governSiteGaps(callers, rowNames []string, elsewhere map[string]string) []string {
	rows := map[string]bool{}
	for _, n := range rowNames {
		rows[n] = true
	}
	joined := strings.Join(rowNames, "\n")
	isCaller := map[string]bool{}
	var gaps []string
	for _, fn := range callers {
		isCaller[fn] = true
		how, ok := elsewhere[fn]
		if !ok {
			if !regexp.MustCompile(`\b` + regexp.QuoteMeta(fn) + `\b`).MatchString(joined) {
				gaps = append(gaps, fn+" asks runUngoverned, but no governSites row is named for it and governSiteElsewhere does not cover it: "+
					"add a row to governSites (the table TestGovernAdminRuns_EverySwapSite runs), or an entry in governSiteElsewhere")
			}
			continue
		}
		kind, ref, _ := strings.Cut(how, ": ")
		switch kind {
		case "row":
			if !rows[ref] {
				gaps = append(gaps, fn+": governSiteElsewhere names row "+ref+", which is not in governSites")
			}
		case "test":
			src, err := os.ReadFile(ref)
			if err != nil || !strings.Contains(string(src), "GovernAdminRuns") {
				gaps = append(gaps, fn+": governSiteElsewhere names "+ref+", which is missing or never turns the switch on")
			}
		case "none":
		default:
			gaps = append(gaps, fn+": governSiteElsewhere entry "+how+" is not row:, test: or none:")
		}
	}
	for fn := range elsewhere {
		if !isCaller[fn] {
			gaps = append(gaps, "governSiteElsewhere lists "+fn+", which no longer asks runUngoverned: remove the entry")
		}
	}
	sort.Strings(gaps)
	return gaps
}

// TestGovernAdminRuns_EverySiteIsListed is what makes the table above a proof of every site: a
// function that asks runUngoverned must be named by a governSites row, or be in governSiteElsewhere
// with the row or test that reaches it.
func TestGovernAdminRuns_EverySiteIsListed(t *testing.T) {
	var names []string
	for _, site := range governSites() {
		names = append(names, site.name)
	}
	callers := runUngovernedCallers(t, ".")
	if len(callers) < 25 {
		t.Fatalf("found only %d runUngoverned callers: the walk stopped seeing the call sites", len(callers))
	}
	for _, gap := range governSiteGaps(callers, names, governSiteElsewhere) {
		t.Error(gap)
	}
}

// TestGovernAdminRuns_ANewSiteFailsTheListing is the guard's counterfactual: a new function that asks
// runUngoverned, found in a scratch source dir, is a gap until a row or an entry covers it.
func TestGovernAdminRuns_ANewSiteFailsTheListing(t *testing.T) {
	dir := t.TempDir()
	const src = `package api

func (s *Server) brandNewGovernedDoor(ctx context.Context) bool { return !s.runUngoverned(ctx) }
`
	if err := os.WriteFile(filepath.Join(dir, "new_door.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	callers := runUngovernedCallers(t, dir)
	if !slices.Equal(callers, []string{"brandNewGovernedDoor"}) {
		t.Fatalf("callers = %v, want the new door found", callers)
	}
	if gaps := governSiteGaps(callers, []string{"x.go someOtherSite"}, nil); len(gaps) != 1 || !strings.Contains(gaps[0], "brandNewGovernedDoor") {
		t.Errorf("gaps = %v, want the unlisted new door flagged", gaps)
	}
	if gaps := governSiteGaps(callers, []string{"new_door.go brandNewGovernedDoor"}, nil); len(gaps) != 0 {
		t.Errorf("a row named for the door still reports %v", gaps)
	}
}

// governPatchEnd drives PATCH /runs/{id} as ctx's caller, straight into the
// handler so the caller's context is exactly the one under test.
func governPatchEnd(t *testing.T, f *endWaitFixture, ctx context.Context, to time.Time) (int, runEndWaitResponse) {
	t.Helper()
	rc := chi.NewRouteContext()
	rc.URLParams.Add("id", f.st.run.ID.String())
	r := httptest.NewRequest(http.MethodPatch, "/api/v1/runs/"+f.st.run.ID.String(), strings.NewReader(endsAtBody(to))).
		WithContext(context.WithValue(ctx, chi.RouteCtxKey, rc))
	w := httptest.NewRecorder()
	f.srv.handleSetRunEndAndWait(w, r)
	var out runEndWaitResponse
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
	}
	return w.Code, out
}

// TestGovernAdminRuns_EverySwapSite runs every site that asks runUngoverned as
// each kind of caller, with the switch on and off. With it on, an SSO admin's
// session and an admin-role personal token get the member outcome at every site
// and the admin token and local mode (no person) do not; with it off every
// caller gets today's admin outcome.
func TestGovernAdminRuns_EverySwapSite(t *testing.T) {
	token := governAdminTokenCtx(t)
	callers := []struct {
		name string
		ctx  context.Context
		// governedWhenOn: the member outcome with the switch on.
		governedWhenOn bool
	}{
		{"SSO admin session", governAdminSessionCtx(), true},
		{"admin-role personal token", token, true},
		// No person on the context: what the admin token and local mode publish.
		{"admin token", context.Background(), false},
		{"local mode", withLocalPrincipal(context.Background(), "local:dev"), false},
	}
	// The extension re-check asks about the run's OWNER. The admin token and
	// local mode are not that person, so they are held to the owner's own rows
	// whatever the switch says, and those rows say nothing about this seam.
	const ownerRecheck = "run_end_wait.go -> run_owner_authority.go extension re-check"
	for _, site := range governSites() {
		for _, on := range []bool{true, false} {
			for _, c := range callers {
				if site.name == ownerRecheck && !c.governedWhenOn {
					continue
				}
				want := on && c.governedWhenOn
				t.Run(fmt.Sprintf("%s/switch %v/%s", site.name, on, c.name), func(t *testing.T) {
					var s *Server
					if site.build != nil {
						s = site.build(t)
					} else {
						s = New(Config{})
					}
					s.cfg.GovernAdminRuns = on
					if got := site.governed(t, s, c.ctx); got != want {
						t.Errorf("member outcome = %v, want %v", got, want)
					}
				})
			}
		}
	}
	// A member is governed everywhere whatever the switch says: the proof the
	// probes above detect the member outcome at all.
	member := withOIDCGroupsTruncated(withOIDCGroups(operatorCtx(governAdminSub, "owner@corp.example", oidc.RoleUser), []string{"eng"}), false)
	for _, site := range governSites() {
		t.Run("member control/"+site.name, func(t *testing.T) {
			s := New(Config{})
			if site.build != nil {
				s = site.build(t)
			}
			if !site.governed(t, s, member) {
				t.Error("a member did not get the member outcome: the probe proves nothing")
			}
		})
	}
}

// TestGovernAdminRuns_RouteAdmissionUnchanged: the preset editor lists every
// preset for a governed admin (presetOpenTo is not swapped), and the switch
// leaves isOperator, the route tier, alone.
func TestGovernAdminRuns_RouteAdmissionUnchanged(t *testing.T) {
	s := New(Config{Audit: &recRecorder{}, GovernAdminRuns: true})
	p := types.LaunchPreset{Name: "other", UserTypes: []string{"some-other-type"}}
	for name, ctx := range map[string]context.Context{
		"session": governAdminSessionCtx(), "token": governAdminTokenCtx(t),
	} {
		if !s.isOperator(ctx) {
			t.Errorf("%s: isOperator = false, want true: the switch must not change the route tier", name)
		}
		if !s.presetOpenTo(governReq(ctx), p) {
			t.Errorf("%s: presetOpenTo = false, want the editor's list left whole", name)
		}
		if s.presetLaunchOpenTo(governReq(ctx), p) {
			t.Errorf("%s: presetLaunchOpenTo = true, want a governed admin refused a preset closed to their type", name)
		}
	}
}

func TestGovernAdminRuns_ExemptMarkerIsBreakGlass(t *testing.T) {
	s := New(Config{GovernAdminRuns: true})
	exempt := context.WithValue(governAdminSessionCtx(), governanceExemptKey{}, true)
	if !s.runUngoverned(exempt) {
		t.Error("an admin session carrying the exempt marker must stay ungoverned")
	}
	if s.runUngoverned(governAdminSessionCtx()) {
		t.Error("an admin session without the marker must be governed")
	}
	member := operatorCtx(governAdminSub, "owner@corp.example", oidc.RoleUser)
	if s.runUngoverned(context.WithValue(member, governanceExemptKey{}, true)) {
		t.Error("the exempt marker must not turn a member into an operator")
	}
}
