// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// reviewFor's caller, one case per row of the value table: the person's tier as the tool context caps it (the
// effective role), and the caller that context yields. super is never true: the cap leaves isOperator false.
func TestAssistantReviewCallerValueTable(t *testing.T) {
	s := &Server{}
	cases := []struct {
		name        string
		snap, fresh string
		want        reviewCaller
	}{
		// A unit admin of the target is a member here too; their row runs on the store (TestAssistantUnitAdminHeldWithTheSwitchOff_PG).
		{"member", oidc.RoleUser, oidc.RoleUser, reviewCaller{unitAdmin: true}},
		{"security admin", oidc.RoleSecurityAdmin, oidc.RoleSecurityAdmin, reviewCaller{}},
		{"super admin, capped", oidc.RoleAdmin, oidc.RoleAdmin, reviewCaller{}},
		// A cli_access token publishes the member tier; the user view publishes the viewed role.
		{"security admin through a cli_access token", oidc.RoleUser, oidc.RoleSecurityAdmin, reviewCaller{unitAdmin: true}},
		{"security admin in the user view", oidc.RoleUser, oidc.RoleSecurityAdmin, reviewCaller{unitAdmin: true}},
	}
	for _, c := range cases {
		role, denied := s.assistantEffectiveRole(c.snap, c.fresh, false)
		if denied {
			t.Fatalf("%s: denied", c.name)
		}
		ctx := withHumanIdentity(context.Background(), "sub-"+c.name, "p@corp.example", role, "", nil, false)
		if got := s.assistantReviewCaller(ctx); got != c.want {
			t.Errorf("%s: caller = %+v, want %+v", c.name, got, c.want)
		}
	}
}

// The second-human switch, mirrored from governanceSaveHeld and unitWriteMode: on, every governance save is
// judged; off, only a unit admin's save of a kind unitWriteMode decides is, and every other one applies with no review and no approvals.
func TestAssistantSaveJudgedFollowsTheSwitch(t *testing.T) {
	unitAdmin, security := reviewCaller{unitAdmin: true}, reviewCaller{}
	t.Setenv(envGovernanceSecondHuman, "true")
	if !assistantSaveJudged(govKindRestriction, security) || !assistantSaveJudged(govKindUnit, security) {
		t.Error("switch on: a security admin's save is judged")
	}
	t.Setenv(envGovernanceSecondHuman, "false")
	if assistantSaveJudged(govKindUnit, security) || assistantSaveJudged(govKindRestriction, unitAdmin) {
		t.Error("switch off: a security admin's save, and a unit admin's non-unit save, apply directly")
	}
	for _, k := range []string{govKindUnit, types.GovernanceTargetMembership, types.GovernanceTargetUnitAdmin, types.GovernanceTargetUnitDeciders} {
		if !assistantSaveJudged(k, unitAdmin) {
			t.Errorf("switch off: a unit admin's %s save goes through unitWriteMode, held whatever the switch", k)
		}
	}
	rv, n, err := (&Server{}).assistantReview(context.Background(), assistantSubject{govKind: govKindUnit, key: uuid.NewString(),
		draft: json.RawMessage(`{}`)}, security)
	if err != nil || rv != nil || n != 0 {
		t.Errorf("switch off, security admin: review %+v, approvals %d, err %v; want applied: no review, 0", rv, n, err)
	}
}

type assistantFakeEvaluator struct {
	v   egress.HostVerdict
	err error
}

func (assistantFakeEvaluator) Name() string { return "fake" }

func (assistantFakeEvaluator) MethodAllowed(string) bool { return true }

func (e assistantFakeEvaluator) EvaluateHost(context.Context, egress.Request) (egress.HostVerdict, error) {
	return e.v, e.err
}

// Only an allow verdict counts as allowed: deny, unknown and an error do not, and neither does an attempt with
// no recorded port.
func TestAssistantSimulateAllowedUnder(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name string
		ev   egress.Evaluator
		port int
		want bool
	}{
		{"allowed", assistantFakeEvaluator{v: egress.VerdictAllow}, 443, true},
		{"denied", assistantFakeEvaluator{v: egress.VerdictDeny}, 443, false},
		{"unknown", assistantFakeEvaluator{v: egress.VerdictUnknown}, 443, false},
		{"evaluator error", assistantFakeEvaluator{v: egress.VerdictAllow, err: errors.New("boom")}, 443, false},
		{"no port", assistantFakeEvaluator{v: egress.VerdictAllow}, 0, false},
	} {
		if got := assistantAllowedUnder(ctx, c.ev)("h.example", c.port); got != c.want {
			t.Errorf("%s: allowed = %v, want %v", c.name, got, c.want)
		}
	}
}

// Every counted run is judged by the proposed rules alone, in both directions: a host the run's other layers
// allowed but the draft does not counts as refused, and a host they refused but the draft allows counts as
// allowed.
func TestAssistantSimulateUsesDraftRulesOnly(t *testing.T) {
	ctx := context.Background()
	a, b := uuid.New(), uuid.New()
	rows := []store.RunHostDecision{
		replayRowAt(a, "layers-allow.example", 443, "allow", egress.RuleSourcePolicyAllowed),
		replayRowAt(b, "draft-allows.example", 443, "deny", egress.RuleSourcePolicyDenied),
	}
	allowed := assistantAllowedUnder(ctx, proxy.NewBuiltinEvaluator(types.RunPolicySpec{AllowedDomains: []string{"draft-allows.example"}}))
	got := assistantSimulateOf(replayCounts(rows, store.ReplayTotals{RunsInScope: 2, RunsCounted: 2}, allowed))
	if got.WouldRefuse != 1 || got.WouldAllow != 1 || got.Basis != assistantReplayBasis {
		t.Errorf("got %+v, want 1 refused, 1 allowed, basis %q", got, assistantReplayBasis)
	}
}

// simulate's and impact's answers carry counts only: no run id, address or person label.
func TestAssistantSimulateAndImpactCarryCountsOnly(t *testing.T) {
	run := uuid.New()
	rows := []store.RunHostDecision{replayRowAt(run, "h.example", 443, "allow", egress.RuleSourcePolicyAllowed)}
	res := replayCounts(rows, store.ReplayTotals{RunsInScope: 1, RunsCounted: 1}, func(string, int) bool { return false })
	if len(res.Runs) == 0 {
		t.Fatal("the replay listed no run to leave out")
	}
	sim, _ := json.Marshal(assistantSimulateOf(res))
	imp, _ := json.Marshal(impactCounts{Computed: "full", People: 2, LiveRunsTotal: 3})
	leak := regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}|@|Person `)
	for name, raw := range map[string][]byte{"simulate": sim, "impact": imp} {
		if leak.Match(raw) {
			t.Errorf("%s = %s, want counts only", name, raw)
		}
	}
}

// The relay's tool list keeps draft out of a review session's list, and both lists are non-empty.
func TestAssistantToolListsAreNeverEmpty(t *testing.T) {
	for _, p := range []types.AssistantPurpose{types.AssistantCompose, types.AssistantReview} {
		list := assistantToolList(p)
		if len(list) == 0 {
			t.Errorf("%s: empty tool list", p)
		}
		for _, tool := range list {
			if p == types.AssistantReview && tool.Name == types.AssistantToolDraft {
				t.Error("a review session is offered draft")
			}
			if _, ok := assistantToolRegistry()[tool.Name]; !ok {
				t.Errorf("listed tool %s has no handler", tool.Name)
			}
		}
	}
}

// assistantFailingRestrictions is a store whose restriction read fails, for the validator's failure case.
type assistantFailingRestrictions struct {
	store.Store
	store.RestrictionStore
}

func (assistantFailingRestrictions) GetRestriction(context.Context, uuid.UUID) (types.Restriction, error) {
	return types.Restriction{}, errors.New("pq: connection reset SECRET-READ-TEXT")
}

// On a restriction change, validate keeps the route validator's refusal as a message, but a failed read is an
// error (a 500), never the raw text handed to the model as a field error.
func TestAssistantValidateRestrictionReadFailureIsAnError(t *testing.T) {
	s := &Server{cfg: Config{Store: assistantFailingRestrictions{}}}
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	sub := assistantSubject{govKind: govKindRestriction, key: uuid.NewString()}

	sub.draft = json.RawMessage(`{"name":"","spec":{},"limits":{},"members":[]}`)
	errs, err := s.assistantValidate(r, sub, reviewCaller{})
	if err != nil || len(errs) != 1 || errs[0].Message == "" {
		t.Fatalf("invalid body: errs=%v err=%v, want one validator message", errs, err)
	}

	sub.draft = json.RawMessage(`{"name":"tighten","spec":{},"limits":{},"members":[]}`)
	errs, err = s.assistantValidate(r, sub, reviewCaller{})
	if err == nil || errs != nil {
		t.Fatalf("failed read: errs=%v err=%v, want an error and no message", errs, err)
	}
	ref := assistantCheckFailed(err)
	if ref == nil || ref.status != http.StatusInternalServerError || regexp.MustCompile(`SECRET-READ-TEXT|pq:`).MatchString(ref.msg) {
		t.Errorf("failed read refusal = %+v, want a 500 without the store's text", ref)
	}
}
