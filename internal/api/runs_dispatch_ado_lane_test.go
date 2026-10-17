// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// adoLaneDispatch is one dispatch of a run on site, after mut edits the run and
// params, with what the runner was handed and every audit row.
type adoLaneDispatch struct {
	spec   runner.SandboxSpec
	st     *dispatchTestStore
	events []types.AuditEvent
	run    types.AgentRun
	runner *fakeRunner
}

func dispatchADOLane(t *testing.T, site types.SiteConfig, mutRun func(*types.AgentRun), p dispatchParams) adoLaneDispatch {
	t.Helper()
	return dispatchADOLaneRead(t, site, nil, mutRun, p)
}

// dispatchADOLaneRead is dispatchADOLane with the site config read failing with siteErr when it is set.
func dispatchADOLaneRead(t *testing.T, site types.SiteConfig, siteErr error, mutRun func(*types.AgentRun), p dispatchParams) adoLaneDispatch {
	t.Helper()
	fr := &fakeRunner{}
	srv, st, audit, run := dispatchTeardownFixture(t, fr, types.RunPending)
	if mutRun != nil {
		mutRun(&run)
		st.run = run
	}
	srv.cfg.Store = &laneDispatchStore{siteDispatchStore: &siteDispatchStore{dispatchTestStore: st, site: site, siteErr: siteErr}}
	p.RunToken, p.Image = "run-token", "wardyn/claude-code:latest"
	p.Policy.MinConfinementClass = types.CC1
	p.Policy.AllowedDomains = append(p.Policy.AllowedDomains, "api.anthropic.com")
	srv.dispatchRun(context.Background(), run, ceilingForDispatch(governanceCeiling{}, adoEntraUngraded(), bedrockCredUngraded()), p)
	return adoLaneDispatch{spec: fr.lastSpec, st: st, events: audit.snapshot(), run: run, runner: fr}
}

// skipRows is the dispatch's run.ado_lane.skip rows.
func (d adoLaneDispatch) skipRows() []types.AuditEvent {
	var out []types.AuditEvent
	for _, ev := range d.events {
		if ev.Action == "run.ado_lane.skip" {
			out = append(out, ev)
		}
	}
	return out
}

// T6 (H2 core): the lane follows dispatchParams.ADOLaneRepo, never run.Repo.
func TestDispatch_ADOLaneFollowsTheLaneRepo(t *testing.T) {
	d := dispatchADOLane(t, adoSite(adoEntraTestRow()), nil, dispatchParams{ADOLaneRepo: adoTestRepo})
	if d.st.State() == types.RunFailed {
		t.Fatalf("the run failed: %q", d.st.FailureHint())
	}
	if g := d.spec.ProxyConfig.ADOGrant; g == nil || g.Organization != "contoso" {
		t.Fatalf("ADOGrant = %+v, want contoso's", g)
	}
	if hosts := d.spec.Env["WARDYN_GIT_PAT_BROKER_HOSTS"]; !strings.Contains(hosts, "dev.azure.com") {
		t.Errorf("WARDYN_GIT_PAT_BROKER_HOSTS = %q, want contoso's git hosts", hosts)
	}
	if rows := d.skipRows(); len(rows) != 0 {
		t.Errorf("skip rows %+v, want none", rows)
	}

	// The same run with only run.Repo set: a server-authored lane. No lane.
	d = dispatchADOLane(t, adoSite(adoEntraTestRow()), func(r *types.AgentRun) { r.Repo = adoTestRepo }, dispatchParams{})
	if d.spec.ProxyConfig.ADOGrant != nil || len(d.skipRows()) != 0 || d.spec.Env[adoLaneDeclinedEnv] != "" {
		t.Fatalf("run.Repo alone: ADOGrant=%+v skip=%+v env=%q, want nothing", d.spec.ProxyConfig.ADOGrant, d.skipRows(), d.spec.Env[adoLaneDeclinedEnv])
	}
}

// T7: a decline starts the run without the credential and says so, once.
func TestDispatch_ADOLaneDeclineIsAnnouncedNotRefused(t *testing.T) {
	shared := adoEntraTestRow()
	shared.CredentialSource = ""
	wide := adoEntraTestRow()
	wide.BaseURLs = []string{"https://dev.azure.com"}
	minted := adoEntraTestRow()
	minted.Entra.TokenMode = types.ADOTokenModeMintedPAT
	for name, tc := range map[string]struct {
		site types.SiteConfig
		mut  func(*types.AgentRun)
		p    dispatchParams
		want adoLaneDecline
	}{
		"shared source":   {adoSite(shared), nil, dispatchParams{ADOLaneRepo: adoTestRepo}, adoDeclineCredentialSource},
		"two orgs":        {adoSite(wide), nil, dispatchParams{ADOLaneRepo: "https://dev.azure.com/fabrikam/p/_git/r", Policy: types.RunPolicySpec{WorkspaceRepos: []types.WorkspaceRepo{{Repo: adoTestRepo}}}}, adoDeclineCrossRepoConflict},
		"operator-owned":  {adoSite(minted), func(r *types.AgentRun) { r.OperatorOwned = true }, dispatchParams{ADOLaneRepo: adoTestRepo}, adoDeclineNoPerson},
		"service creator": {adoSite(minted), func(r *types.AgentRun) { r.CreatedBy = "service:ci" }, dispatchParams{ADOLaneRepo: adoTestRepo}, adoDeclineNoPerson},
	} {
		t.Run(name, func(t *testing.T) {
			d := dispatchADOLane(t, tc.site, tc.mut, tc.p)
			if d.st.State() == types.RunFailed || d.runner.createCalls != 1 {
				t.Fatalf("state %s (%q), CreateSandbox calls %d; want the run started", d.st.State(), d.st.FailureHint(), d.runner.createCalls)
			}
			rows := d.skipRows()
			if len(rows) != 1 || rows[0].Outcome != "denied" ||
				!strings.Contains(string(rows[0].Data), `"reason":"`+string(tc.want)+`"`) ||
				!strings.Contains(string(rows[0].Data), adoLaneDeclineClauses[tc.want]) {
				t.Fatalf("skip rows = %+v, want one denied %s row with its sentence", rows, tc.want)
			}
			if got := d.spec.Env[adoLaneDeclinedEnv]; got != string(tc.want) {
				t.Errorf("%s = %q, want %q", adoLaneDeclinedEnv, got, tc.want)
			}
			if d.spec.ProxyConfig.ADOGrant != nil {
				t.Errorf("ADOGrant = %+v on a declined run", d.spec.ProxyConfig.ADOGrant)
			}
		})
	}
	d := dispatchADOLane(t, adoSite(adoEntraTestRow()), nil, dispatchParams{ADOLaneRepo: "https://github.com/acme/r"})
	if len(d.skipRows()) != 0 || d.spec.Env[adoLaneDeclinedEnv] != "" {
		t.Errorf("a GitHub-only run wrote skip=%+v env=%q, want neither", d.skipRows(), d.spec.Env[adoLaneDeclinedEnv])
	}
}

// A run the dispatch then refuses was never dispatched without the credential, so a double site-config
// read failure on a model run writes no decline row; with no Azure DevOps row there is nothing to claim.
func TestDispatch_ADOLaneDeclineIsSilentOnARefusedRunAndWithNoRow(t *testing.T) {
	d := dispatchADOLaneRead(t, types.SiteConfig{}, errors.New("db down"), nil, dispatchParams{ADOLaneRepo: adoTestRepo})
	if d.st.State() != types.RunFailed || d.runner.createCalls != 0 {
		t.Fatalf("state %s, CreateSandbox calls %d; want the model run refused on the unreadable config", d.st.State(), d.runner.createCalls)
	}
	if rows := d.skipRows(); len(rows) != 0 {
		t.Errorf("a refused run wrote skip rows %+v, want none", rows)
	}
	d = dispatchADOLane(t, types.SiteConfig{}, nil, dispatchParams{ADOLaneRepo: adoTestRepo})
	if len(d.skipRows()) != 0 || d.spec.Env[adoLaneDeclinedEnv] != "" || d.spec.Env[adoLaneDeclinedHostsEnv] != "" {
		t.Errorf("no azure_devops row: skip=%+v env=%q hosts=%q, want silence", d.skipRows(), d.spec.Env[adoLaneDeclinedEnv], d.spec.Env[adoLaneDeclinedHostsEnv])
	}
}

// The decline names the hosts that went without, and only hosts: not the other repositories' hosts, and no address.
func TestDispatch_ADOLaneDeclineNamesOnlyTheDecliningHosts(t *testing.T) {
	shared := adoEntraTestRow()
	shared.CredentialSource = ""
	p := dispatchParams{ADOLaneRepo: adoTestRepo, Policy: types.RunPolicySpec{WorkspaceRepos: []types.WorkspaceRepo{
		{Repo: adoTestRepo}, {Repo: "https://github.com/acme/r"}}}}
	d := dispatchADOLane(t, adoSite(shared), nil, p)
	if got := d.spec.Env[adoLaneDeclinedHostsEnv]; got != "dev.azure.com" {
		t.Errorf("%s = %q, want dev.azure.com alone", adoLaneDeclinedHostsEnv, got)
	}
	if got := d.spec.Env[adoLaneDeclinedEnv]; got != string(adoDeclineCredentialSource) {
		t.Errorf("%s = %q, want credential_source", adoLaneDeclinedEnv, got)
	}
}

// T8 (H2): a source scan of an Azure DevOps repository never takes the lane:
// it sets run.Repo and dispatches with no ADOLaneRepo.
func TestSourceScan_NeverTakesTheADOLane(t *testing.T) {
	srv, st, audit := govEscapeFixture(t, &capStore{})
	fr := &fakeRunner{}
	srv.cfg.Runner = fr
	srv.cfg.Store = &scanLaneStore{govEscapeStore: st}
	row := adoEntraTestRow()
	row.Entra.TokenMode = types.ADOTokenModeMintedPAT
	st.siteConfig = adoSite(row)
	ctx := govMemberCtx([]string{"eng"}, false)
	src := types.Source{ID: uuid.New(), Kind: types.SourceRepo, Locator: adoTestRepo}
	run, err := srv.launchSourceScanRun(ctx, "bob@corp.example", src, nil)
	if err != nil {
		t.Fatalf("launchSourceScanRun: %v", err)
	}
	if fr.createCalls != 1 {
		t.Fatalf("the scan reached CreateSandbox %d times, want once", fr.createCalls)
	}
	if fr.lastSpec.ProxyConfig.ADOGrant != nil {
		t.Errorf("the scan carries an ADO grant: %+v", fr.lastSpec.ProxyConfig.ADOGrant)
	}
	for _, ev := range audit.events {
		if ev.RunID != nil && *ev.RunID == run.ID && (strings.HasPrefix(ev.Action, "ado_pat.") || ev.Action == "run.ado_lane.skip") {
			t.Errorf("the scan wrote %s/%s", ev.Action, ev.Outcome)
		}
	}
}

// T9 (characterization): a workspace-repository run resolves from
// WorkspaceRepos as it always did, whether or not ADOLaneRepo repeats it.
func TestADOWorkspaceRunsAreUnchanged(t *testing.T) {
	ws := types.RunPolicySpec{WorkspaceRepos: []types.WorkspaceRepo{{Repo: adoTestRepo}}}
	for name, p := range map[string]dispatchParams{
		"POST /runs seeded from the workspace": {Policy: ws, ADOLaneRepo: adoTestRepo},
		"a workspace step run":                 {Policy: ws},
	} {
		d := dispatchADOLane(t, adoSite(adoEntraTestRow()), nil, p)
		g := d.spec.ProxyConfig.ADOGrant
		if g == nil || g.Organization != "contoso" || len(g.Capabilities) != 2 {
			t.Errorf("%s: ADOGrant = %+v, want contoso at the row default", name, g)
		}
		if len(d.skipRows()) != 0 || d.spec.Env[adoLaneDeclinedEnv] != "" {
			t.Errorf("%s: skip=%+v env=%q, want neither", name, d.skipRows(), d.spec.Env[adoLaneDeclinedEnv])
		}
	}
}

// T10: the grade a door takes for a repo-only run is the lane dispatch
// resolves, so dispatch authors it without autonomy_grade_drift.
func TestDispatch_ADOGradeParityForTheRunsRepo(t *testing.T) {
	site := adoSite(adoEntraTestRow())
	lane, on := resolveADOEntraRun(site, nil, adoTestRepo, adoLaneOwner("t@example.com", false))
	fr := &fakeRunner{}
	srv, st, audit, run := dispatchTeardownFixture(t, fr, types.RunPending)
	srv.cfg.Store = &laneDispatchStore{siteDispatchStore: &siteDispatchStore{dispatchTestStore: st, site: site}}
	srv.dispatchRun(context.Background(), run, ceilingForDispatch(governanceCeiling{}, adoEntraGradedAs(lane, on), bedrockCredUngraded()), dispatchParams{
		RunToken: "run-token", Image: "wardyn/claude-code:latest", ADOLaneRepo: adoTestRepo,
		Policy: types.RunPolicySpec{MinConfinementClass: types.CC1, AllowedDomains: []string{"api.anthropic.com"}},
	})
	if !on || st.State() == types.RunFailed || fr.lastSpec.ProxyConfig.ADOGrant == nil {
		t.Fatalf("graded on=%v; state %s (%q); grant %+v — want the graded lane authored", on, st.State(), st.FailureHint(), fr.lastSpec.ProxyConfig.ADOGrant)
	}
	if findAudit(audit.snapshot(), run.ID, "run.create", "failure") != nil {
		t.Fatal("dispatch refused the graded run")
	}
}

// T20 (shell): agent-run's clone failure line points at the decline when
// dispatch set WARDYN_ADO_LANE_DECLINED, naming the repository, and says nothing of it otherwise.
func TestAgentRunCloneFailureNamesTheADODecline(t *testing.T) {
	lib, err := filepath.Abs("../../deploy/images/common/agent-run-lib.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, code := range []string{"row_not_admitted", ""} {
		cmd := exec.Command("bash", "-c", `source "$1"; clone_one file:///nonexistent "$2/x"`, "bash", lib, t.TempDir())
		cmd.Env = append(os.Environ(), "WARDYN_ADO_LANE_DECLINED="+code, "WARDYN_RUN_ID=run-1", "GIT_TERMINAL_PROMPT=0")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("clone_one: %v\n%s", err, out)
		}
		hint := "cloning file:///nonexistent: this run was not given an Azure DevOps credential (row_not_admitted); the run's Credentials panel and 'wardyn run get run-1' say why"
		if got := strings.Contains(string(out), hint); got != (code != "") || !strings.Contains(string(out), "clone FAILED") {
			t.Errorf("code %q: hint shown = %v, output:\n%s", code, got, out)
		}
	}
}
