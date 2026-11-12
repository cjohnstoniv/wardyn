// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// touchStore records TouchRun calls. Every other store.Store method is nil, so
// the handler panics if it reaches one — the decision path must not, beyond the
// GetRun internalAuth's liveness gate asks of every /internal/* call
// (refuseTerminalRun), which anyRunLive answers.
type touchStore struct {
	anyRunLive
	touched []uuid.UUID
}

func (s *touchStore) TouchRun(_ context.Context, id uuid.UUID) error {
	s.touched = append(s.touched, id)
	return nil
}

// TestInternalDecisionTouchesRun pins the idle clock to real agent activity.
// The reaper measures idleness as the age of agent_runs.updated_at, so agent
// activity must bump it, not only the interactive attach keepalive — otherwise
// a non-interactive run is hard-killed at its auto_stop_after_sec cap while
// actively working, under an audit event claiming it was idle
// (internal/lifecycle). The blind case is asserted too because it returns
// early, before the egress audit write.
func TestInternalDecisionTouchesRun(t *testing.T) {
	h := newHarness(t)
	st := &touchStore{}
	srv := New(baseTestConfig(h, st))
	runID := uuid.New()
	tok := h.mintRunToken(t, runID)
	const path = "/api/v1/internal/decisions"

	body := `{"request":{"host":"api.anthropic.com","method":"POST"},"decision":"allow"}`
	if w := do(t, srv, http.MethodPost, path, tok, body); w.Code != http.StatusAccepted {
		t.Fatalf("decision code = %d, want 202", w.Code)
	}
	// The blind case must also route through the touch (it returns early, before
	// the egress audit write) — but a burst inside touchDebounce coalesces to ONE
	// UPDATE on the hot agent_runs row, so the second decision here is debounced.
	blind := `{"request":{"host":"api.anthropic.com","method":"CONNECT"},"decision":"allow",` +
		`"scan":{"scanned":false,"coverage":"tunneled-opaque","action":"bypass"}}`
	if w := do(t, srv, http.MethodPost, path, tok, blind); w.Code != http.StatusAccepted {
		t.Fatalf("blind decision code = %d, want 202", w.Code)
	}
	if len(st.touched) != 1 || st.touched[0] != runID {
		t.Fatalf("TouchRun calls = %v, want exactly one (debounced) for %s", st.touched, runID)
	}

	// Aging the run's entry past the window touches again — via the BLIND
	// payload, proving the touch happens before that path's early return — and a
	// DIFFERENT run is never debounced by this one's entry.
	srv.lastTouchMu.Lock()
	srv.lastTouch[runID] = srv.lastTouch[runID].Add(-2 * touchDebounce)
	srv.lastTouchMu.Unlock()
	if w := do(t, srv, http.MethodPost, path, tok, blind); w.Code != http.StatusAccepted {
		t.Fatalf("aged blind decision code = %d, want 202", w.Code)
	}
	otherID := uuid.New()
	otherTok := h.mintRunToken(t, otherID)
	if w := do(t, srv, http.MethodPost, path, otherTok, body); w.Code != http.StatusAccepted {
		t.Fatalf("other-run decision code = %d, want 202", w.Code)
	}
	if len(st.touched) != 3 || st.touched[1] != runID || st.touched[2] != otherID {
		t.Fatalf("TouchRun calls = %v, want aged re-touch then the other run", st.touched)
	}

	// credential:reauth-timeout is the proxy's OWN signal that a re-auth hold's
	// wait ran out with nobody there — it reports that nobody answered,
	// not real agent activity, so it must never touch, even well past the
	// debounce window. (Every refused request is excluded the same way: see
	// TestInternalDecision_DenyDoesNotTouchRun.)
	srv.lastTouchMu.Lock()
	srv.lastTouch[runID] = srv.lastTouch[runID].Add(-2 * touchDebounce)
	srv.lastTouchMu.Unlock()
	timeout := `{"request":{"host":"portal.sso.us-east-1.amazonaws.com","method":"CONNECT"},` +
		`"decision":"deny","rule_source":"credential:reauth-timeout"}`
	if w := do(t, srv, http.MethodPost, path, tok, timeout); w.Code != http.StatusAccepted {
		t.Fatalf("reauth-timeout decision code = %d, want 202", w.Code)
	}
	if len(st.touched) != 3 {
		t.Fatalf("TouchRun calls after a credential:reauth-timeout decision = %v, want no new touch (still 3)", st.touched)
	}
}

// The toolgate polls the approval it is blocked on every 2s, and the proxy relays each
// poll as a decision. That is waiting, not agent activity: counting it kept a run
// whose tool call nobody answered from ever idling out. A refused request is not the
// run doing something either. shouldTouch uses egress.CountsAsActivity, the predicate
// the pause clock and the runner-side Activity flag share.
func TestShouldTouch_ApprovalPollAndReauthTimeoutAreNotActivity(t *testing.T) {
	srv := &Server{}
	for _, dl := range []egress.DecisionLog{
		{Decision: egress.Allow, RuleSource: egress.RuleSourceApprovals},
		{Decision: egress.Deny, RuleSource: ruleSourceCredentialReauthTimeout},
		{Decision: egress.Deny, RuleSource: "policy:denied"},
		{Decision: egress.Pending, RuleSource: "policy:first-use"},
	} {
		if srv.shouldTouch(uuid.New(), "", dl) {
			t.Errorf("shouldTouch(%s %q) = true; it is not agent activity", dl.Decision, dl.RuleSource)
		}
	}
	if !srv.shouldTouch(uuid.New(), "", egress.DecisionLog{Decision: egress.Allow, RuleSource: "allow-list"}) {
		t.Error("shouldTouch(allowed decision) = false; an allowed decision must restart the idle clock")
	}
}

// A macOS guest calls Apple's boot hosts in the background. A run that allows all egress, or whose allow
// covers one, gets those calls allowed, so the idle clock must not read them as the run's own activity.
func TestShouldTouch_MacOSBootHostAllowedDoesNotTouch(t *testing.T) {
	srv := &Server{}
	host := macOSBootHosts()[0]
	boot := egress.DecisionLog{Request: egress.Request{Host: host}, Decision: egress.Allow, RuleSource: "policy:allowlist"}
	if srv.shouldTouch(uuid.New(), types.MachineMacOSVM, boot) {
		t.Errorf("shouldTouch(boot host %s, macOS VM) = true; Apple's background traffic is not the run's activity", host)
	}
	other := egress.DecisionLog{Request: egress.Request{Host: "github.com"}, Decision: egress.Allow, RuleSource: "policy:allowlist"}
	if !srv.shouldTouch(uuid.New(), types.MachineMacOSVM, other) {
		t.Error("shouldTouch(github.com, macOS VM) = false; the run's own allowed traffic still counts")
	}
}

// The boot-host list is a macOS rule: on any other kind of run the same host is ordinary allowed traffic.
func TestShouldTouch_BootHostOnLinuxRunTouches(t *testing.T) {
	srv := &Server{}
	boot := egress.DecisionLog{Request: egress.Request{Host: macOSBootHosts()[0]}, Decision: egress.Allow, RuleSource: "policy:allowlist"}
	for _, kind := range []types.MachineKind{"", types.MachineContainer, types.MachineLinuxVM} {
		if !srv.shouldTouch(uuid.New(), kind, boot) {
			t.Errorf("shouldTouch(boot host, kind %q) = false; only a macOS VM run ignores them", kind)
		}
	}
}

// A refused request is the run being told no, not the run working: it must not reset the idle clock, on any
// run kind. An allow still does, and a refusal in the same debounce window cannot spend the allow's touch.
func TestInternalDecision_DenyDoesNotTouchRun(t *testing.T) {
	h := newHarness(t)
	st := &touchStore{}
	srv := New(baseTestConfig(h, st))
	runID := uuid.New()
	tok := h.mintRunToken(t, runID)
	const path = "/api/v1/internal/decisions"

	for _, body := range []string{
		`{"request":{"host":"evil.example","method":"GET"},"decision":"deny","rule_source":"policy:denied"}`,
		`{"request":{"host":"10.0.0.1","method":"CONNECT"},"decision":"deny","rule_source":"builtin:private-ip"}`,
		`{"request":{"host":"new.example","method":"GET"},"decision":"hold","rule_source":"policy:first-use"}`,
	} {
		if w := do(t, srv, http.MethodPost, path, tok, body); w.Code != http.StatusAccepted {
			t.Fatalf("decision code = %d, want 202 (%s)", w.Code, body)
		}
	}
	if len(st.touched) != 0 {
		t.Fatalf("TouchRun calls after refusals = %v, want none", st.touched)
	}
	allow := `{"request":{"host":"api.anthropic.com","method":"POST"},"decision":"allow","rule_source":"policy:allowlist"}`
	if w := do(t, srv, http.MethodPost, path, tok, allow); w.Code != http.StatusAccepted {
		t.Fatalf("allow code = %d, want 202", w.Code)
	}
	if len(st.touched) != 1 || st.touched[0] != runID {
		t.Fatalf("TouchRun calls after an allow = %v, want exactly one for %s", st.touched, runID)
	}
}

// macTouchStore reports the run as a macOS VM, as the liveness gate reads it for every /internal call.
type macTouchStore struct{ touchStore }

func (s *macTouchStore) GetRun(_ context.Context, id uuid.UUID) (types.AgentRun, error) {
	return types.AgentRun{ID: id, State: types.RunRunning, MachineKind: types.MachineMacOSVM, UpdatedAt: time.Now().UTC()}, nil
}

// The handler reads the run's kind from the liveness gate's read: an allowed boot-host request on a macOS VM
// run does not touch it, and the same run's own allowed traffic does.
func TestInternalDecision_MacOSBootHostDoesNotTouchRun(t *testing.T) {
	h := newHarness(t)
	st := &macTouchStore{}
	srv := New(baseTestConfig(h, st))
	runID := uuid.New()
	tok := h.mintRunToken(t, runID)
	const path = "/api/v1/internal/decisions"

	boot := `{"request":{"host":"` + macOSBootHosts()[0] + `","method":"CONNECT"},"decision":"allow","rule_source":"policy:allowlist"}`
	if w := do(t, srv, http.MethodPost, path, tok, boot); w.Code != http.StatusAccepted {
		t.Fatalf("boot-host decision code = %d, want 202", w.Code)
	}
	if len(st.touched) != 0 {
		t.Fatalf("TouchRun calls after a boot-host allow = %v, want none", st.touched)
	}
	own := `{"request":{"host":"github.com","method":"GET"},"decision":"allow","rule_source":"policy:allowlist"}`
	if w := do(t, srv, http.MethodPost, path, tok, own); w.Code != http.StatusAccepted {
		t.Fatalf("own decision code = %d, want 202", w.Code)
	}
	if len(st.touched) != 1 {
		t.Fatalf("TouchRun calls after the run's own allow = %v, want one", st.touched)
	}
}

// presenceStore adds TouchRun to the pause fixture's store.
type presenceStore struct {
	*pauseStore
	touched int
}

func (s *presenceStore) TouchRun(context.Context, uuid.UUID) error { s.touched++; return nil }

var _ store.RunPauser = (*presenceStore)(nil)

// The pause's presence clock reads the same predicate: a refused request neither touches the run nor stamps
// the agent-activity clock, so a run that is only being refused still reaches its idle pause.
func TestInternalDecision_DenyDoesNotStampPresence(t *testing.T) {
	var ps *presenceStore
	f := newPauseFixture(t, time.Hour, func(c *Config) {
		ps = &presenceStore{pauseStore: c.Store.(*pauseStore)}
		c.Store = ps
	})
	tok := f.h.mintRunToken(t, f.run.ID)
	const path = "/api/v1/internal/decisions"

	deny := `{"request":{"host":"evil.example","method":"GET"},"decision":"deny","rule_source":"policy:denied"}`
	if w := do(t, f.srv, http.MethodPost, path, tok, deny); w.Code != http.StatusAccepted {
		t.Fatalf("deny code = %d, want 202 (%s)", w.Code, w.Body.String())
	}
	if f.st.stamps != 0 || ps.touched != 0 {
		t.Fatalf("after a refusal: stamps %d touches %d, want none", f.st.stamps, ps.touched)
	}
	allow := `{"request":{"host":"github.com","method":"GET"},"decision":"allow","rule_source":"policy:allowlist"}`
	if w := do(t, f.srv, http.MethodPost, path, tok, allow); w.Code != http.StatusAccepted {
		t.Fatalf("allow code = %d, want 202 (%s)", w.Code, w.Body.String())
	}
	if f.st.stamps != 1 || ps.touched != 1 {
		t.Fatalf("after an allow: stamps %d touches %d, want one each", f.st.stamps, ps.touched)
	}
}

// With the expiry sweeper off for a non-positive age, the ceiling handed to the
// sandbox stays the 24h default (toolgate's own), so it never waits on nothing.
func TestApprovalExpiryCeiling_NonPositiveFallsBackToTheDefault(t *testing.T) {
	for _, in := range []time.Duration{0, -time.Hour} {
		if got := approvalExpiryCeiling(in); got != 24*time.Hour {
			t.Errorf("approvalExpiryCeiling(%v) = %v, want 24h", in, got)
		}
	}
	if got := approvalExpiryCeiling(2 * time.Hour); got != 2*time.Hour {
		t.Errorf("approvalExpiryCeiling(2h) = %v", got)
	}
}
