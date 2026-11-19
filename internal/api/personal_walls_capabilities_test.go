// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/ghscope"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A person's capability walls narrow the provider's default too, where the run's policy names no list:
// the brokered set is the default meet the walls, never more than either.

const wallsCapsOwner = "sub-walls-caps"

// adoWallsStore is adoTestStore plus the principal_prefs table; err fails every walls read.
type adoWallsStore struct {
	*adoTestStore
	prefs map[string]json.RawMessage
	err   error
}

func (s *adoWallsStore) GetPrincipalPref(_ context.Context, principal, key string) (json.RawMessage, error) {
	if s.err != nil {
		return nil, s.err
	}
	if v, ok := s.prefs[principal+"|"+key]; ok {
		return v, nil
	}
	return nil, store.ErrNotFound
}

func (s *adoWallsStore) PutPrincipalPref(_ context.Context, principal, key string, value json.RawMessage) error {
	s.prefs[principal+"|"+key] = value
	return nil
}

// adoLaneUnderWalls dispatches the fixture row (default profile code_read+code_write, ceiling adds pr)
// for a member whose walls are walls (nil for none), under a policy naming policyCaps (nil for none).
func adoLaneUnderWalls(t *testing.T, walls *types.PersonalWallsSpec, readErr error, policyCaps []adoscope.Capability,
) (adoEntraLane, bool, types.RunPolicySpec, *adoTestStore, *memAudit) {
	t.Helper()
	st := &adoWallsStore{adoTestStore: &adoTestStore{}, prefs: map[string]json.RawMessage{}}
	if walls != nil {
		saveWalls(t, st, wallsCapsOwner, *walls)
	}
	st.err = readErr
	audit := &memAudit{}
	s := &Server{cfg: Config{Store: st, Audit: audit, Now: time.Now}}
	policy := types.RunPolicySpec{AllowedDomains: []string{"api.anthropic.com"}, AzureDevOpsCapabilities: policyCaps}
	lane, ok := s.authorADOEntraLane(context.Background(), types.AgentRun{ID: uuid.New(), CreatedBy: wallsCapsOwner}, adoTestRun(t), true,
		adoEntraUngraded(), adoStandingBound{}, dispatchLLMPlan{mitmCACertPEM: "CERT", mitmCAKeyPEM: "KEY"}, &policy, map[string]string{}, nil)
	return lane, ok, policy, st.adoTestStore, audit
}

// assertADOLaneCaps checks the REST gate and every grant snapshot the resolver and each re-mint read.
func assertADOLaneCaps(t *testing.T, name string, lane adoEntraLane, ok bool, st *adoTestStore, want []adoscope.Capability) {
	t.Helper()
	if !ok || lane.gate == nil {
		t.Fatalf("%s: dispatch refused (ok=%v)", name, ok)
	}
	if !slices.Equal(lane.gate.Capabilities, want) {
		t.Errorf("%s: gate capabilities = %v, want %v", name, lane.gate.Capabilities, want)
	}
	snaps := grantSnapshotCaps(t, st)
	if len(snaps) == 0 {
		t.Fatalf("%s: no grant authored", name)
	}
	for i, got := range snaps {
		if !slices.Equal(got, want) {
			t.Errorf("%s: grant[%d] snapshot capabilities = %v, want %v", name, i, got, want)
		}
	}
}

func assertADOWallsRefused(t *testing.T, name string, lane adoEntraLane, ok bool, st *adoTestStore, audit *memAudit, reason, says string) {
	t.Helper()
	if ok || lane.gate != nil || len(st.grants) != 0 {
		t.Fatalf("%s: ok=%v gate=%+v grants=%d, want a refusal with nothing authored", name, ok, lane.gate, len(st.grants))
	}
	if !slices.Equal(st.failTo, []types.RunState{types.RunFailed}) {
		t.Errorf("%s: run transitions = %v, want one to FAILED", name, st.failTo)
	}
	rows := audit.find("run.create")
	if len(rows) != 1 {
		t.Fatalf("%s: run.create audit rows = %d, want 1", name, len(rows))
	}
	data := string(rows[0].Data)
	if !strings.Contains(data, `"reason":"`+reason+`"`) || !strings.Contains(data, says) {
		t.Errorf("%s: refusal %s, want reason %s saying %q", name, data, reason, says)
	}
}

func TestWallsNarrowTheADODefaultProfile(t *testing.T) {
	read, write, pr := adoscope.CapCodeRead, adoscope.CapCodeWrite, adoscope.CapPR
	readOnly := &types.PersonalWallsSpec{AzureDevOpsCapabilities: &[]adoscope.Capability{read}}

	lane, ok, policy, st, _ := adoLaneUnderWalls(t, readOnly, nil, nil)
	assertADOLaneCaps(t, "a read-only wall over a writable default", lane, ok, st, []adoscope.Capability{read})
	if !slices.Equal(policy.AzureDevOpsCapabilities, []adoscope.Capability{read}) {
		t.Errorf("the audited policy's list = %v, want the met [code_read]", policy.AzureDevOpsCapabilities)
	}

	// No walls: the default profile, byte for byte, and no list recorded.
	lane, ok, policy, st, _ = adoLaneUnderWalls(t, nil, nil, nil)
	assertADOLaneCaps(t, "no walls", lane, ok, st, []adoscope.Capability{read, write})
	if len(policy.AzureDevOpsCapabilities) != 0 {
		t.Errorf("no walls: the policy's list = %v, want none", policy.AzureDevOpsCapabilities)
	}

	// A wall wider than the default never adds what the default does not give.
	lane, ok, _, st, _ = adoLaneUnderWalls(t, &types.PersonalWallsSpec{AzureDevOpsCapabilities: &[]adoscope.Capability{read, write, pr}}, nil, nil)
	assertADOLaneCaps(t, "a wider wall", lane, ok, st, []adoscope.Capability{read, write})

	// A policy that names a list met the walls at create (personalWallsRunStep); dispatch keeps it.
	lane, ok, _, st, _ = adoLaneUnderWalls(t, &types.PersonalWallsSpec{AzureDevOpsCapabilities: &[]adoscope.Capability{read, write}}, nil, []adoscope.Capability{read})
	assertADOLaneCaps(t, "a policy list", lane, ok, st, []adoscope.Capability{read})
}

func TestWallsADODefaultProfileRefusals(t *testing.T) {
	lane, ok, _, st, audit := adoLaneUnderWalls(t, &types.PersonalWallsSpec{AzureDevOpsCapabilities: &[]adoscope.Capability{adoscope.CapPR}}, nil, nil)
	assertADOWallsRefused(t, "a wall with nothing in common", lane, ok, st, audit, reasonGovernanceOverlayUnsatisfiable,
		"Your walls and this run's provider default leave azure_devops_capabilities nothing in common")

	lane, ok, _, st, audit = adoLaneUnderWalls(t, nil, errors.New("principal_prefs unreachable"), nil)
	assertADOWallsRefused(t, "walls that cannot be read", lane, ok, st, audit, reasonGrantWithheldUnread,
		"Your walls could not be read")
}

// ghWallsStore is componentTestStore plus the principal_prefs table, for a launch through the doors.
type ghWallsStore struct {
	*componentTestStore
	prefs map[string]json.RawMessage
}

func (s *ghWallsStore) GetPrincipalPref(_ context.Context, principal, key string) (json.RawMessage, error) {
	if v, ok := s.prefs[principal+"|"+key]; ok {
		return v, nil
	}
	return nil, store.ErrNotFound
}

func (s *ghWallsStore) PutPrincipalPref(_ context.Context, principal, key string, value json.RawMessage) error {
	s.prefs[principal+"|"+key] = value
	return nil
}

// githubCLIWallsFixture is githubCLIFixture with a writable GitHub CLI row (code_read+code_write), a
// writable git lane, and the launching member's walls (nil for none).
func githubCLIWallsFixture(t *testing.T, walls *types.PersonalWallsSpec) (*componentFixture, map[string]any) {
	t.Helper()
	return githubCLIWallsFixtureWith(t, walls, nil)
}

// githubCLIWallsFixtureWith is githubCLIWallsFixture with the deployment's policy, the member's
// ceiling, edited first (nil for none).
func githubCLIWallsFixtureWith(t *testing.T, walls *types.PersonalWallsSpec, edit func(*types.RunPolicySpec)) (*componentFixture, map[string]any) {
	t.Helper()
	cs := &capStore{}
	h := newHarness(t)
	st := &componentTestStore{govEscapeStore: newGovEscapeStore(cs), snapshots: map[uuid.UUID][]types.RunComponent{}}
	wst := &ghWallsStore{componentTestStore: st, prefs: map[string]json.RawMessage{}}
	if walls != nil {
		saveWalls(t, wst, capSub, *walls)
	}
	rec := &recRecorder{}
	cfg := baseTestConfig(h, wst)
	cfg.Audit, cfg.Broker, cfg.Runner = rec, h.broker, &fakeRunner{}
	cfg.Secrets = &memSecrets{m: map[string][]byte{}}
	cfg.OIDC = &oidc.Authenticator{}
	cfg.DefaultPolicy = govDeployment()
	cfg.DefaultPolicy.EligibleGrants = []types.GrantSpec{{Kind: types.GrantGitHubToken, Scope: json.RawMessage(`{"repos":["acme/app"],"permissions":{"contents":"write"}}`)}}
	if edit != nil {
		edit(&cfg.DefaultPolicy)
	}
	f := &componentFixture{srv: New(cfg), st: st, cs: cs, rec: rec}
	ref := f.org(ghCLIRowID, types.ComponentDefinition{CLI: &types.ComponentCLI{Kind: types.CLIKindGitHub,
		GitHub: &types.CLIGitHub{Capabilities: []ghscope.Capability{ghscope.CapCodeRead, ghscope.CapCodeWrite}}}})
	return f, ref
}

// launchGitHubCLIUnderWalls creates a run with the writable row on acme/app and answers the door's
// capabilities and its grant row's scope.
func launchGitHubCLIUnderWalls(t *testing.T, walls *types.PersonalWallsSpec) ([]ghscope.Capability, string) {
	t.Helper()
	f, ref := githubCLIWallsFixture(t, walls)
	fr := f.srv.cfg.Runner.(*fakeRunner)
	body := componentBody(ref)
	body["repo"] = "acme/app"
	if w := f.ask(t, componentDoors[0], body); w.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", w.Code, w.Body)
	}
	fr.waitForSandbox(t)
	fr.mu.Lock()
	door := fr.lastSpec.ProxyConfig.GitHubAPI
	fr.mu.Unlock()
	if door == nil || len(door.Grants) != 1 {
		t.Fatalf("GitHubAPI = %+v, want the door with one grant", door)
	}
	for _, g := range f.st.govEscapeStore.grants {
		if g.ID == door.Grants["acme"] {
			return door.Capabilities, string(g.Spec.Scope)
		}
	}
	t.Fatal("the door's grant row was not written")
	return nil, ""
}

func TestWallsNarrowTheGitHubCLIDefault(t *testing.T) {
	read, write := ghscope.CapCodeRead, ghscope.CapCodeWrite
	caps, scope := launchGitHubCLIUnderWalls(t, &types.PersonalWallsSpec{GitHubCapabilities: &[]ghscope.Capability{read}})
	if !slices.Equal(caps, []ghscope.Capability{read}) || scope != `{"api":true,"capabilities":["code_read"],"repos":["acme/app"]}` {
		t.Errorf("a read-only wall: door %v grant scope %s, want code_read alone in both", caps, scope)
	}

	caps, scope = launchGitHubCLIUnderWalls(t, nil)
	if !slices.Equal(caps, []ghscope.Capability{read, write}) || scope != `{"api":true,"capabilities":["code_read","code_write"],"repos":["acme/app"]}` {
		t.Errorf("no walls: door %v grant scope %s, want the row's code_read and code_write", caps, scope)
	}
}

func TestWallsGitHubCLIDisjointIsRefused(t *testing.T) {
	f, ref := githubCLIWallsFixture(t, &types.PersonalWallsSpec{GitHubCapabilities: &[]ghscope.Capability{ghscope.CapIssuesWrite}})
	body := componentBody(ref)
	body["repo"] = "acme/app"
	w := f.ask(t, componentDoors[0], body)
	got := decodeErrorBody(t, w)
	if w.Code != http.StatusForbidden || got.Reason != reasonGovernanceOverlayUnsatisfiable || !strings.Contains(got.Error, "github_capabilities") {
		t.Fatalf("create = %d %+v, want 403 %s naming github_capabilities", w.Code, got, reasonGovernanceOverlayUnsatisfiable)
	}
}
