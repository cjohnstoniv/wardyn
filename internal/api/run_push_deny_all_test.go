// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// denyAllPolicy is a run policy that refuses every push.
func denyAllPolicy() types.RunPolicySpec {
	return types.RunPolicySpec{PushRules: &types.PushRulesSpec{DenyAll: true}}
}

// pushDenyAllSentence is the sentence a resident git token refusal carries.
const pushDenyAllSentence = "This run refuses every push, but this install gives runs their git token directly, so pushes can't be stopped. Ask your admin to turn the git token broker on."

// TestPushDenyAllRefusal: a run that refuses every push is refused when a credential would push around the
// broker: a git_pat with the broker off (a GitHub host, an Azure DevOps host, a runner_resident row), or an
// ssh_key. A git_pat with the broker on, and a credential withheld because its forge is GitHub-brokered,
// are not refused, and nothing is refused without deny_all.
func TestPushDenyAllRefusal(t *testing.T) {
	runID := uuid.New()
	spec := func(rows ...types.CredentialGrant) []types.GrantSpec { return patGrantSpecs(rows) }
	resident := patRowWith(uuid.New(), runID, "gitlab.corp.io", nil)
	resident.Delivery = types.GrantDeliveryResident
	for _, tc := range []struct {
		name               string
		denyAll            bool
		grants             []types.GrantSpec
		brokerOn, brokered bool
		want               string
	}{
		{"github.com PAT, broker off", true, spec(patRowWith(uuid.New(), runID, "github.com", nil)), false, false, reasonPushDenyAllResidentPAT},
		{"Azure DevOps PAT, broker off", true, spec(patRowWith(uuid.New(), runID, "dev.azure.com", nil)), false, false, reasonPushDenyAllResidentPAT},
		{"runner_resident PAT, broker off", true, spec(resident), false, false, reasonPushDenyAllResidentPAT},
		{"PAT, broker on", true, spec(patRowWith(uuid.New(), runID, "github.com", nil)), true, false, ""},
		{"withheld github.com PAT on a brokered run", true, spec(patRowWith(uuid.New(), runID, "github.com", nil)), false, true, ""},
		{"ssh_key", true, spec(sshKeyRowFor(runID, "gitlab.corp.io")), true, false, reasonPushDenyAllSSHKey},
		{"withheld github.com ssh_key on a brokered run", true, spec(sshKeyRowFor(runID, "github.com")), true, true, ""},
		{"no deny_all", false, spec(patRowWith(uuid.New(), runID, "github.com", nil), sshKeyRowFor(runID, "gitlab.corp.io")), false, false, ""},
	} {
		reason, detail := pushDenyAllRefusal(tc.denyAll, tc.grants, tc.brokerOn, tc.brokered)
		if reason != tc.want {
			t.Errorf("%s: reason = %q, want %q", tc.name, reason, tc.want)
		}
		if reason == reasonPushDenyAllResidentPAT && detail != pushDenyAllSentence {
			t.Errorf("%s: detail = %q, want %q", tc.name, detail, pushDenyAllSentence)
		}
	}
}

// TestRefusesEveryPushReadsEverySource: the policy's block, the base of an entry with no set, and any
// entry's set each carry deny_all.
func TestRefusesEveryPushReadsEverySource(t *testing.T) {
	on := &types.PushRulesSpec{DenyAll: true}
	if !refusesEveryPush(denyAllPolicy(), nil, nil) || !refusesEveryPush(types.RunPolicySpec{}, on, nil) ||
		!refusesEveryPush(types.RunPolicySpec{}, nil, []types.PushRuleSet{{Rules: types.PushRulesSpec{DenyPaths: []string{"a"}}}, {Rules: *on}}) {
		t.Error("a source carrying deny_all was missed")
	}
	if refusesEveryPush(types.RunPolicySpec{}, &types.PushRulesSpec{DenyPaths: []string{"a"}}, nil) {
		t.Error("deny_all read where no source sets it")
	}
}

// TestDispatchRefusesDenyAllWithAResidentPAT is the create and queue-start door: both launch through
// dispatchRun, whose enforceablePATNarrowing refuses an operator's run (which the member-run broker-off
// refusal lets through) that refuses every push while its git_pat would be resident, fails it with the
// sentence and audits the reason on run.create. With the broker on the same run is dispatched.
func TestDispatchRefusesDenyAllWithAResidentPAT(t *testing.T) {
	dispatch := func(t *testing.T, brokerOff bool, rows func(uuid.UUID) []types.CredentialGrant) (*fakeRunner, *dispatchTestStore, *recRecorder, types.AgentRun) {
		t.Helper()
		fr := &fakeRunner{}
		srv, st, audit, run := dispatchTeardownFixture(t, fr, types.RunPending)
		run.Task = ""
		srv.cfg.DisableGitPATBroker = brokerOff
		st.grants = rows(run.ID)
		srv.dispatchRun(context.Background(), run, ceilingForDispatch(governanceCeiling{Operator: true}, adoEntraUngraded(), bedrockCredUngraded()), dispatchParams{
			RunToken: "run-token", Image: "wardyn/claude-code:latest", Policy: denyAllPolicy(),
			GitPATGrants: winningByHost(st.grants),
		})
		return fr, st, audit, run
	}
	for _, host := range []string{"github.com", "dev.azure.com"} {
		t.Run(host+" broker off", func(t *testing.T) {
			fr, st, audit, run := dispatch(t, true, func(id uuid.UUID) []types.CredentialGrant {
				return []types.CredentialGrant{patRowWith(uuid.New(), id, host, nil)}
			})
			requireRefusedWith(t, fr, st, audit, run, reasonPushDenyAllResidentPAT)
			if ev := findAudit(audit.events, run.ID, "run.create", "failure"); ev == nil || !strings.Contains(string(ev.Data), "pushes can't be stopped") {
				t.Errorf("run.create failure does not carry the sentence: %v", ev)
			}
		})
	}
	t.Run("ssh_key", func(t *testing.T) {
		fr, st, audit, run := dispatch(t, false, func(id uuid.UUID) []types.CredentialGrant {
			return []types.CredentialGrant{sshKeyRowFor(id, "gitlab.corp.io")}
		})
		requireRefusedWith(t, fr, st, audit, run, reasonPushDenyAllSSHKey)
	})
	t.Run("broker on", func(t *testing.T) {
		fr, _, _, _ := dispatch(t, false, func(id uuid.UUID) []types.CredentialGrant {
			return []types.CredentialGrant{patRowWith(uuid.New(), id, "github.com", nil)}
		})
		if fr.createCalls != 1 {
			t.Fatalf("createCalls = %d, want the run dispatched", fr.createCalls)
		}
	})
}

// TestPATNarrowingAtDoorRefusesDenyAllWithAResidentPAT is Review's door (POST /runs/preflight and
// policy-preview): an operator's run, pooled or not, hears the refusal before the click.
func TestPATNarrowingAtDoorRefusesDenyAllWithAResidentPAT(t *testing.T) {
	srv := &Server{cfg: Config{DisableGitPATBroker: true}}
	spec := denyAllPolicy()
	spec.EligibleGrants = []types.GrantSpec{patGrantSpec("")}
	r := httptest.NewRequest(http.MethodPost, "/api/v1/runs/preflight", nil)
	for _, pooled := range []bool{false, true} {
		reason, detail := srv.patNarrowingAtDoor(r, createRunRequest{}, spec, types.SiteConfig{}, true, pooled)
		if reason != reasonPushDenyAllResidentPAT || detail != pushDenyAllSentence {
			t.Errorf("pooled=%v: %q %q, want %s and the sentence", pooled, reason, detail, reasonPushDenyAllResidentPAT)
		}
	}
	srv.cfg.DisableGitPATBroker = false
	if reason, _ := srv.patNarrowingAtDoor(r, createRunRequest{}, spec, types.SiteConfig{}, true, false); reason != "" {
		t.Errorf("broker on: refused %q, want accepted", reason)
	}
}

// TestRevivePATNarrowingRefusesDenyAllWithAResidentPAT is the revive door: a run whose stored proxy config
// refuses every push, from the policy, the base rules or an entry's set, is refused 409 and audited on
// run.revive when the broker is now off.
func TestRevivePATNarrowingRefusesDenyAllWithAResidentPAT(t *testing.T) {
	for name, cfg := range map[string]*proxy.Config{
		"policy":   {Policy: denyAllPolicy()},
		"base":     {PushRuleBase: &types.PushRulesSpec{DenyAll: true}},
		"an entry": {PushRuleSets: []types.PushRuleSet{{Provider: "github", Host: "github.com", Org: "acme", Rules: types.PushRulesSpec{DenyAll: true}}}},
	} {
		t.Run(name, func(t *testing.T) {
			audit := &memAudit{}
			srv := &Server{cfg: Config{DisableGitPATBroker: true, Audit: audit, Now: time.Now}}
			run := types.AgentRun{ID: uuid.New(), CreatedBy: "sub-owner", OperatorOwned: true}
			grants := []types.CredentialGrant{patRowWith(uuid.New(), run.ID, "github.com", nil)}
			rerr := srv.revivePATNarrowing(context.Background(), run, cfg, grants, types.ActorHuman, "sub-owner")
			if rerr == nil || rerr.status != http.StatusConflict || rerr.reason != reasonPushDenyAllResidentPAT || !strings.Contains(rerr.msg, pushDenyAllSentence) {
				t.Fatalf("revive = %+v, want 409 %s with the sentence", rerr, reasonPushDenyAllResidentPAT)
			}
			if rows := audit.find("run.revive"); len(rows) != 1 || !strings.Contains(string(rows[0].Data), reasonPushDenyAllResidentPAT) {
				t.Errorf("run.revive rows = %+v, want one naming %s", rows, reasonPushDenyAllResidentPAT)
			}
		})
	}
}
