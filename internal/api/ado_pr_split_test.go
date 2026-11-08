// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/ghscope"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The pull-request split: Azure DevOps pr is pr_create, pr_vote and pr_complete, GitHub pr is
// pr_write and pr_review, and pr stays an alias for its parts wherever a list is compared.

// newADOCapFixtureRow is newADOCapFixture on a row with this ceiling and default profile. pre, when
// set, is the capability list a run dispatched before the split carried (its snapshot and ceiling).
func newADOCapFixtureRow(t *testing.T, ceiling, profile, pre []adoscope.Capability) *adoCapFixture {
	t.Helper()
	f := newADOFixture(t)
	subject := f.fake.Subject()
	if w := f.capture(t, subject); w.Code != http.StatusFound {
		t.Fatalf("capture: status %d body %q", w.Code, w.Body.String())
	}
	row := adoEntraTestRow()
	row.ID = f.cfg.RowID
	row.Entra.TenantID, row.Entra.ClientID = f.cfg.TenantID, f.cfg.ClientID
	row.Entra.CapabilityCeiling, row.Entra.DefaultProfile = ceiling, profile
	st := &adoTestStore{site: adoSite(row)}
	f.srv.cfg.Store = st
	rf := &adoResolveFixture{adoFixture: f, st: st, runID: uuid.New(), subject: subject}
	ado, ok := resolveADOEntraRun(st.site, nil, adoTestRepo, subject)
	if !ok {
		t.Fatal("fixture resolved no lane")
	}
	if pre != nil {
		ado.caps, ado.ceiling = slices.Clone(pre), slices.Clone(pre)
	}
	if _, _, ok := f.srv.authorADOEntraInjection(context.Background(), types.AgentRun{ID: rf.runID}, ado,
		"CERT", "KEY", &types.RunPolicySpec{}, map[string]string{}, nil); !ok {
		t.Fatal("fixture dispatch refused")
	}
	rf.grantID = st.grants[0].ID
	f.audit.rows = nil
	fa := newFakeApprovals()
	rf.srv.cfg.Approvals = fa
	rf.srv.cfg.Store = &adoCapStore{adoTestStore: rf.st, approvals: fa}
	return &adoCapFixture{adoResolveFixture: rf, approvals: fa}
}

var (
	adoRead, adoCreate, adoVote = adoscope.CapCodeRead, adoscope.CapPRCreate, adoscope.CapPRVote
	adoCaps                     = func(c ...adoscope.Capability) []adoscope.Capability { return c }
)

// Inside the ceiling and outside the default profile, a vote is held for a person, while opening a
// pull request is standing.
func TestADOPRVoteHeldInsideCeiling(t *testing.T) {
	f := newADOCapFixtureRow(t, adoCaps(adoRead, adoCreate, adoVote), adoCaps(adoRead, adoCreate), nil)
	if got := granted(t, f.ask(t, adoCreate, types.FirstUseWaitForReview, uuid.Nil, prPath)); !adoscope.HasName(got, adoCreate) {
		t.Errorf("pr_create answered %v, want it standing", got)
	}
	pendingID(t, f.ask(t, adoVote, types.FirstUseWaitForReview, uuid.Nil, prPath+"/7/reviewers/a1"), adoCapabilityPendingState)
	if len(f.approvals.requested) != 1 {
		t.Errorf("rows raised = %d, want one escalation for the vote", len(f.approvals.requested))
	}
}

// Outside the ceiling a vote is refused, at the ask and at the decide door.
func TestADOPRVoteAboveCeilingRefused(t *testing.T) {
	f := newADOCapFixtureRow(t, adoCaps(adoRead, adoCreate), adoCaps(adoRead, adoCreate), nil)
	w := f.ask(t, adoVote, types.FirstUseWaitForReview, uuid.Nil, prPath+"/7/reviewers/a1")
	if w.Code != http.StatusForbidden || f.failureReasonOf(t)["reason"] != "capability_above_ceiling" {
		t.Fatalf("status %d body %s, want 403 capability_above_ceiling", w.Code, w.Body.String())
	}
	if len(f.approvals.requested) != 0 {
		t.Errorf("rows raised = %d, want none", len(f.approvals.requested))
	}
	rowID := f.st.site.WorkspaceProviders.Git[0].ID
	if f.srv.adoEscalationWithinCeiling(context.Background(), adoCapabilityScope{ProviderID: rowID, Capability: string(adoVote)}) {
		t.Error("the decide door admits pr_vote outside the ceiling")
	}
	if !f.srv.adoEscalationWithinCeiling(context.Background(), adoCapabilityScope{ProviderID: rowID, Capability: string(adoCreate)}) {
		t.Error("the decide door refuses pr_create inside the ceiling")
	}
}

// A run dispatched before the split recorded pr; a vote is standing for it, and the row's ceiling of
// pr still covers its snapshot.
func TestADOSnapshotNamingPRAdmitsVote(t *testing.T) {
	pre := adoCaps(adoRead, adoscope.CapCodeWrite, adoscope.CapPR)
	f := newADOCapFixtureRow(t, pre, adoCaps(adoRead, adoscope.CapCodeWrite), pre)
	if got := granted(t, f.ask(t, adoVote, types.FirstUseWaitForReview, uuid.Nil, prPath+"/7/reviewers/a1")); !adoscope.HasName(got, adoVote) {
		t.Errorf("pr_vote answered %v, want it standing under a snapshot naming pr", got)
	}
	if len(f.approvals.requested) != 0 {
		t.Errorf("rows raised = %d, want none", len(f.approvals.requested))
	}
}

// A default profile naming a part stores under a ceiling naming pr.
func TestADORowPRAliasValidates(t *testing.T) {
	row := entraRow(func(r *types.GitProvider) {
		r.Entra.CapabilityCeiling = adoCaps(adoscope.CapPR)
		r.Entra.DefaultProfile = adoCaps(adoVote)
	})
	if err := validateProviderEntra(0, row); err != nil {
		t.Fatalf("default profile [pr_vote] under ceiling [pr] refused: %v", err)
	}
	row = entraRow(func(r *types.GitProvider) {
		r.Entra.CapabilityCeiling = adoCaps(adoCreate)
		r.Entra.DefaultProfile = adoCaps(adoscope.CapPR)
	})
	if err := validateProviderEntra(0, row); err == nil {
		t.Fatal("default profile [pr] under ceiling [pr_create] stored")
	}
}

// A new run records the parts, never pr: from the row's default profile and from a policy's list.
func TestADODispatchRecordsSplitNames(t *testing.T) {
	parts := adoCaps(adoscope.CapPRCreate, adoscope.CapPRVote, adoscope.CapPRComplete)
	row := adoEntraTestRow()
	row.Entra.DefaultProfile = adoCaps(adoscope.CapPR)
	ado, ok := resolveADOEntraRun(adoSite(row), nil, adoTestRepo, "sub-1")
	if !ok || !slices.Equal(ado.caps, parts) {
		t.Fatalf("dispatch caps = %v (ok %v), want %v", ado.caps, ok, parts)
	}
	if slices.Contains(ado.ceiling, adoscope.CapPR) || !slices.Equal(row.Entra.CapabilityCeiling, adoCaps(adoRead, adoscope.CapCodeWrite, adoscope.CapPR)) {
		t.Errorf("ceiling = %v, row %v; want the parts read, the row as stored", ado.ceiling, row.Entra.CapabilityCeiling)
	}
	got, ok := ado.withPolicyCapabilities(adoCaps(adoRead, adoscope.CapPR), adoStandingBound{operator: true})
	if !ok || !slices.Equal(got.caps, append(adoCaps(adoRead), parts...)) {
		t.Errorf("policy [code_read pr] = %v (ok %v), want code_read and the parts", got.caps, ok)
	}
	got, ok = ado.withPolicyCapabilities(adoCaps(adoVote), adoStandingBound{governance: adoCaps(adoscope.CapPR)})
	if !ok || !slices.Equal(got.caps, adoCaps(adoVote)) {
		t.Errorf("member [pr_vote] under a governance list of [pr] = %v (ok %v), want pr_vote", got.caps, ok)
	}
}

// githubCapabilitiesDisjoint reads pr as its parts on both sides.
func TestGitHubCapabilitiesDisjointPRAlias(t *testing.T) {
	gh := func(c ...ghscope.Capability) []ghscope.Capability { return c }
	if ref := githubCapabilitiesDisjoint(gh(ghscope.CapPRWrite), gh(ghscope.CapPR)); ref != nil {
		t.Errorf("[pr_write] under [pr] refused: %+v", ref)
	}
	if ref := githubCapabilitiesDisjoint(gh(ghscope.CapPR), gh(ghscope.CapPRWrite, ghscope.CapPRReview)); ref != nil {
		t.Errorf("[pr] under [pr_write pr_review] refused: %+v", ref)
	}
	if ref := githubCapabilitiesDisjoint(gh(ghscope.CapCodeRead), gh(ghscope.CapPR)); ref == nil || ref.status != http.StatusForbidden {
		t.Errorf("[code_read] under [pr] = %+v, want 403", ref)
	}
}

// prRuleStore answers the run's recorded envelope.
type prRuleStore struct {
	*authzStore
	events []types.AuditEvent
}

func (s *prRuleStore) QueryAuditEvents(context.Context, uuid.UUID, int) ([]types.AuditEvent, error) {
	return s.events, nil
}

// An approval rule's capabilities_within reads pr as its parts, on the rule and on the run.
func TestApprovalRuleCapabilitiesWithinPRAlias(t *testing.T) {
	for _, tc := range []struct {
		name   string
		within []string
		ado    []adoscope.Capability
		gh     []ghscope.Capability
		want   bool
	}{
		{"a rule naming pr matches a new run's part", []string{"code_read", "code_write", "pr"}, adoCaps(adoRead, adoscope.CapCodeWrite, adoCreate), nil, true},
		{"a rule naming pr matches a GitHub part", []string{"code_read", "pr"}, nil, []ghscope.Capability{ghscope.CapCodeRead, ghscope.CapPRReview}, true},
		{"a rule naming pr matches a run recording pr", []string{"code_read", "pr"}, adoCaps(adoRead, adoscope.CapPR), nil, true},
		{"a pre-split run's pr holds the vote", []string{"code_read", "pr_create"}, adoCaps(adoRead, adoscope.CapPR), nil, false},
		{"a GitHub pr holds the review", []string{"code_read", "pr_write"}, nil, []ghscope.Capability{ghscope.CapPR}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data, err := json.Marshal(effectivePolicyDatum{RunPolicySpec: types.RunPolicySpec{AzureDevOpsCapabilities: tc.ado, GitHubCapabilities: tc.gh}})
			if err != nil {
				t.Fatal(err)
			}
			h := newHarness(t)
			run := types.AgentRun{ID: uuid.New(), CreatedBy: "service:ci", State: types.RunRunning, Repo: "org/repo"}
			st := &prRuleStore{authzStore: newAuthzStore(), events: []types.AuditEvent{{ID: uuid.New(), RunID: &run.ID, Action: "run.policy.resolve", Data: data}}}
			cfg := baseTestConfig(h, st)
			cfg.OIDC = &oidc.Authenticator{}
			srv := New(cfg)
			rule := testApprovalRule()
			rule.Match.CapabilitiesWithin = slices.Clone(tc.within)
			ap := types.ApprovalRequest{ID: uuid.New(), RunID: run.ID, Kind: types.ApprovalToolCall, RequestedScope: json.RawMessage(ruleToolScope)}
			_, ok, err := srv.matchApprovalRule(context.Background(), rule, ap, run)
			if err != nil || ok != tc.want {
				t.Fatalf("match = %v, %v; want %v", ok, err, tc.want)
			}
			if !slices.Equal(rule.Match.CapabilitiesWithin, tc.within) {
				t.Errorf("the rule's list changed to %v", rule.Match.CapabilitiesWithin)
			}
		})
	}
}

// The GitHub CLI: a policy naming pr over a row granting pr_write keeps pr_write.
func TestGitHubCLIPolicyPRAlias(t *testing.T) {
	f, ref := githubCLIWallsFixtureWith(t, nil, func(p *types.RunPolicySpec) {
		p.GitHubCapabilities = []ghscope.Capability{ghscope.CapPR}
	})
	ref = f.org("6f0c1d2e-0000-4000-8000-0000000c05a6", types.ComponentDefinition{CLI: &types.ComponentCLI{Kind: types.CLIKindGitHub,
		GitHub: &types.CLIGitHub{Capabilities: []ghscope.Capability{ghscope.CapPRWrite}}}})
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
	if door == nil || !slices.Equal(door.Capabilities, []ghscope.Capability{ghscope.CapPRWrite}) {
		t.Fatalf("GitHubAPI = %+v, want the door with pr_write alone", door)
	}
}
