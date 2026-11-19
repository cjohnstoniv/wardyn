// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// resumeRecheckStore is pauseStore with the reads a resume's re-check makes: the owner's identity rows,
// the captured profile's chain and memberships, and the capability rows.
type resumeRecheckStore struct {
	*pauseStore
	profiles []types.GovernanceUnit
	blocked  bool
	caps     []types.CapabilityGrant
	enforced map[string]bool
}

func (s *resumeRecheckStore) ListCapabilityGrants(context.Context) ([]types.CapabilityGrant, error) {
	return s.caps, nil
}

func (s *resumeRecheckStore) ListCapabilityGrantsFor(context.Context, []string, []string, string) ([]types.CapabilityGrant, error) {
	return s.caps, nil
}

func (s *resumeRecheckStore) GetCapabilityEnforcement(context.Context) (map[string]bool, error) {
	return s.enforced, nil
}

func (s *resumeRecheckStore) IdentityBlocked(context.Context, string, int64) (bool, error) {
	return s.blocked, nil
}

func (s *resumeRecheckStore) GetGovernanceUnitChain(_ context.Context, id uuid.UUID) ([]types.GovernanceUnit, error) {
	return unitChainOf(s.profiles, nil, id)
}

func (s *resumeRecheckStore) ListGovernanceMemberships(context.Context) ([]types.GovernanceMembership, error) {
	return allRowsFor(s.profiles), nil
}

// newResumeRecheckFixture is newPauseFixture with its run paused idle under profile, on a
// resumeRecheckStore.
func newResumeRecheckFixture(t *testing.T, profile types.GovernanceUnit, blocked bool) (*pauseFixture, *resumeRecheckStore) {
	t.Helper()
	var rs *resumeRecheckStore
	f := newPauseFixture(t, time.Hour, func(c *Config) {
		rs = &resumeRecheckStore{pauseStore: c.Store.(*pauseStore), profiles: []types.GovernanceUnit{profile}, blocked: blocked}
		c.Store = rs
	})
	f.srv.cfg.Broker, f.srv.cfg.Approvals = f.h.broker, newFakeApprovals()
	paused := time.Now().UTC()
	f.st.run.PausedAt, f.st.run.PausedReason = &paused, types.PauseIdle
	f.st.run.UnitID, f.st.run.Placement = &profile.ID, types.PlacementLocal
	return f, rs
}

// TestResumeRun_RefusedForASuspendedOwner: a person's Resume of a run whose owner was suspended
// while it was paused thaws nothing, answers 403 identity_deactivated, writes a run.resume denied
// row, and ends the run through the kill cascade.
func TestResumeRun_RefusedForASuspendedOwner(t *testing.T) {
	f, _ := newResumeRecheckFixture(t, types.GovernanceUnit{ID: uuid.New(), Name: "contractors"}, true)
	f.st.run.UnitID = nil // the identity refuses first; no profile is read
	owner := ssoSession(t, pauseOwner, "owner@corp.example", oidc.RoleUser)

	w := doSSO(t, f.srv, http.MethodPost, "/api/v1/runs/"+f.run.ID.String()+"/resume", owner, "")
	if w.Code != http.StatusForbidden || reasonOf(t, w) != reasonIdentityDeactivated {
		t.Fatalf("resume for a suspended owner = %d %s, want 403 %s", w.Code, w.Body.String(), reasonIdentityDeactivated)
	}
	if _, thaws := f.rn.counts(); thaws != 0 {
		t.Errorf("thaws = %d, want 0: a refused resume never thaws the agent", thaws)
	}
	if got := f.st.State(); got != types.RunKilled {
		t.Errorf("run state = %s, want KILLED: an owner who lost access ends the run", got)
	}
	rows := f.rows("run.resume", "denied")
	if len(rows) != 1 || leaseAuditData(t, rows[0])["refusal"] != reasonIdentityDeactivated {
		t.Errorf("run.resume denied rows = %+v, want one with refusal %s", rows, reasonIdentityDeactivated)
	}
	killed := false
	for _, ev := range f.audit.snapshot() {
		killed = killed || ev.Action == "run.kill" && leaseAuditData(t, ev)["reason"] == reasonIdentityDeactivated
	}
	if !killed {
		t.Error("no run.kill row with reason identity_deactivated")
	}
}

// TestResumeRun_RefusedOutsideANarrowedCeiling: a run whose profile came to deny its placement
// while it was paused stays paused and frozen; the refusal is the extension's placement_denied.
func TestResumeRun_RefusedOutsideANarrowedCeiling(t *testing.T) {
	profile := types.GovernanceUnit{ID: uuid.New(), Name: "contractors",
		Limits: types.GovernanceLimits{DenyPlacements: []types.Placement{types.PlacementLocal}}}
	f, rs := newResumeRecheckFixture(t, profile, false)

	err := f.srv.resumeRun(context.Background(), rs, f.st.run, types.ActorSystem, "wardynd", "request_closed")
	var refused *errResumeRefused
	if !errors.As(err, &refused) || refused.ref.status != http.StatusForbidden || refused.ref.reason != "placement_denied" {
		t.Fatalf("resume outside the narrowed profile = %v, want a 403 placement_denied refusal", err)
	}
	if _, thaws := f.rn.counts(); thaws != 0 {
		t.Errorf("thaws = %d, want 0", thaws)
	}
	if pausedAt, _ := f.st.paused(); pausedAt == nil {
		t.Error("the pause was cleared by a refused resume")
	}
	if got := f.st.State(); got != types.RunRunning {
		t.Errorf("run state = %s, want RUNNING: a narrowed profile refuses the resume, it does not end the run", got)
	}
	rows := f.rows("run.resume", "denied")
	if len(rows) != 1 || leaseAuditData(t, rows[0])["refusal"] != "placement_denied" || leaseAuditData(t, rows[0])["subject"] != pauseOwner {
		t.Errorf("run.resume denied rows = %+v, want one naming the owner with refusal placement_denied", rows)
	}
}

// TestResumeRun_AllowedWhileStillValid: an active owner whose profile still admits the run resumes it.
func TestResumeRun_AllowedWhileStillValid(t *testing.T) {
	f, rs := newResumeRecheckFixture(t, types.GovernanceUnit{ID: uuid.New(), Name: "contractors"}, false)

	if err := f.srv.resumeRun(context.Background(), rs, f.st.run, types.ActorSystem, "wardynd", "request_closed"); err != nil {
		t.Fatalf("resume = %v, want nil", err)
	}
	if _, thaws := f.rn.counts(); thaws != 1 {
		t.Errorf("thaws = %d, want 1", thaws)
	}
	if pausedAt, _ := f.st.paused(); pausedAt != nil {
		t.Errorf("still paused at %v", pausedAt)
	}
	if rows := f.rows("run.resume", "denied"); len(rows) != 0 {
		t.Errorf("run.resume denied rows = %+v, want none", rows)
	}
}

// TestResumeRun_RefusedForARevokedCapability: a deny on the run's agent written while it was paused
// refuses the owner's own Resume with the extension's capability_agent, and the run stays paused and
// RUNNING.
func TestResumeRun_RefusedForARevokedCapability(t *testing.T) {
	f, rs := newResumeRecheckFixture(t, types.GovernanceUnit{ID: uuid.New(), Name: "contractors"}, false)
	f.st.run.UnitID = nil // the capability refuses; no profile is needed for it
	rs.caps = []types.CapabilityGrant{grant(types.CapabilitySubjectAll, "", capAgent, f.st.run.Agent, types.CapabilityDeny)}
	owner := ssoSession(t, pauseOwner, "owner@corp.example", oidc.RoleUser)

	w := doSSO(t, f.srv, http.MethodPost, "/api/v1/runs/"+f.run.ID.String()+"/resume", owner, "")
	if w.Code != http.StatusForbidden || reasonOf(t, w) != reasonOwnerCapabilityAgent {
		t.Fatalf("owner's resume after the agent's grant was revoked = %d %s, want 403 %s", w.Code, w.Body.String(), reasonOwnerCapabilityAgent)
	}
	if _, thaws := f.rn.counts(); thaws != 0 {
		t.Errorf("thaws = %d, want 0", thaws)
	}
	if pausedAt, _ := f.st.paused(); pausedAt == nil || f.st.State() != types.RunRunning {
		t.Errorf("paused at %v, state %s; want the run left paused and RUNNING", pausedAt, f.st.State())
	}
	rows := f.rows("run.resume", "denied")
	if len(rows) != 1 || leaseAuditData(t, rows[0])["refusal"] != reasonOwnerCapabilityAgent {
		t.Errorf("run.resume denied rows = %+v, want one with refusal %s", rows, reasonOwnerCapabilityAgent)
	}
}

// TestResumeRun_GroupGrantedOwnerResumesWithoutTheirSession: under an enforced agent kind whose allow
// names the owner's group, a resume that does not carry the owner's session (an approver's decision,
// the sweep) cannot see that group, so it does not ask the capabilities: the run resumes.
func TestResumeRun_GroupGrantedOwnerResumesWithoutTheirSession(t *testing.T) {
	f, rs := newResumeRecheckFixture(t, types.GovernanceUnit{ID: uuid.New(), Name: "contractors"}, false)
	rs.enforced = map[string]bool{capAgent: true}
	rs.caps = []types.CapabilityGrant{grant(types.CapabilitySubjectGroup, "engineering", capAgent, f.st.run.Agent, types.CapabilityAllow)}

	if err := f.srv.resumeRun(context.Background(), rs, f.st.run, types.ActorSystem, "wardynd", "request_closed"); err != nil {
		t.Fatalf("system resume of a group-granted owner's run = %v, want nil", err)
	}
	if _, thaws := f.rn.counts(); thaws != 1 {
		t.Errorf("thaws = %d, want 1", thaws)
	}
	if rows := f.rows("run.resume", "denied"); len(rows) != 0 {
		t.Errorf("run.resume denied rows = %+v, want none", rows)
	}
}

// TestResumeRun_RepeatedRefusalsAuditOncePerPause: the sweep retries a refused resume every pass; the
// pause writes one denied row, and a new pause writes its own.
func TestResumeRun_RepeatedRefusalsAuditOncePerPause(t *testing.T) {
	profile := types.GovernanceUnit{ID: uuid.New(), Name: "contractors",
		Limits: types.GovernanceLimits{DenyPlacements: []types.Placement{types.PlacementLocal}}}
	f, rs := newResumeRecheckFixture(t, profile, false)

	for range 3 {
		if err := f.srv.resumeRun(context.Background(), rs, f.st.run, types.ActorSystem, "wardynd", "request_closed"); err == nil {
			t.Fatal("resume outside the narrowed profile succeeded")
		}
	}
	if rows := f.rows("run.resume", "denied"); len(rows) != 1 {
		t.Fatalf("run.resume denied rows after 3 refusals of one pause = %d, want 1", len(rows))
	}
	later := f.st.run.PausedAt.Add(time.Minute)
	f.st.run.PausedAt = &later
	if err := f.srv.resumeRun(context.Background(), rs, f.st.run, types.ActorSystem, "wardynd", "request_closed"); err == nil {
		t.Fatal("resume outside the narrowed profile succeeded")
	}
	if rows := f.rows("run.resume", "denied"); len(rows) != 2 {
		t.Errorf("run.resume denied rows after a new pause = %d, want 2", len(rows))
	}
}
