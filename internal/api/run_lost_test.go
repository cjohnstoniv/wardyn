// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// lostStore is leaseStore plus store.RunLoser and the watcher claim, with the
// PG conditions of MarkRunLost and StampRunTokenRenewed.
type lostStore struct {
	*leaseStore
	lapsed  bool
	claimed bool
}

func (s *lostStore) StampRunTokenRenewed(context.Context, uuid.UUID) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.run.LostAt != nil {
		return false, nil
	}
	s.lapsed = false
	return true, nil
}

func (s *lostStore) ListLapsedTokenRuns(ctx context.Context, _ time.Duration) ([]types.AgentRun, error) {
	run, _ := s.GetRun(ctx, s.run.ID)
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.lapsed || run.State != types.RunRunning || run.LostAt != nil {
		return nil, nil
	}
	return []types.AgentRun{run}, nil
}

func (s *lostStore) MarkRunLost(_ context.Context, _ uuid.UUID, reason types.LostReason, now time.Time, tokenLife time.Duration) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state != types.RunRunning || s.run.LostAt != nil || (tokenLife > 0 && !s.lapsed) {
		return false, nil
	}
	s.run.LostAt, s.run.LostReason = &now, reason
	return true, nil
}

func (s *lostStore) ClaimStaleRunWatchers(ctx context.Context, _ string, _ time.Duration) ([]types.AgentRun, error) {
	run, _ := s.GetRun(ctx, s.run.ID)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.claimed || run.LostAt != nil || run.State.IsTerminal() {
		return nil, nil
	}
	s.claimed = true
	return []types.AgentRun{run}, nil
}

func (s *lostStore) HeartbeatRunWatcher(context.Context, uuid.UUID, string) error { return nil }

func (s *lostStore) RunWatcherFresh(context.Context, uuid.UUID, time.Duration) (bool, error) {
	return false, nil
}

// lostRunner is leaseRunner that can also remove a proxy alone, and reports
// the agent as exited: with an exit code (the container is still there) or
// without one (the container is gone).
type lostRunner struct {
	*leaseRunner
	proxyErr   error
	proxyStops int
	exitCode   *int
}

func (r *lostRunner) StopProxy(context.Context, string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.proxyStops++
	return r.proxyErr
}

func (r *lostRunner) proxyStopCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.proxyStops
}

func (r *lostRunner) AgentStatus(context.Context, string, string) (runner.Status, error) {
	return runner.Status{State: types.RunStopped, ExitCode: r.exitCode}, nil
}

type lostFixture struct {
	*leaseFixture
	ls *lostStore
	lr *lostRunner
}

// newLostFixture is newLeaseFixture's run made interactive, with no end, whose
// container exited with code 137 but is still there.
func newLostFixture(t *testing.T) *lostFixture {
	t.Helper()
	f := &lostFixture{leaseFixture: newLeaseFixture(t, time.Hour)}
	f.st.run.Interactive = true
	f.st.run.EndsAt = nil
	f.run = f.st.run
	code := 137
	f.ls = &lostStore{leaseStore: f.st}
	f.lr = &lostRunner{leaseRunner: f.rn, exitCode: &code}
	f.srv.cfg.Store = f.ls
	f.srv.cfg.Runner = f.lr
	return f
}

func (f *lostFixture) sweepTokens(t *testing.T) {
	t.Helper()
	if err := f.srv.sweepLapsedRunTokens(context.Background()); err != nil {
		t.Fatalf("sweepLapsedRunTokens: %v", err)
	}
}

func (f *lostFixture) sweepWatchers(t *testing.T) {
	t.Helper()
	if err := f.srv.sweepRunWatchers(context.Background()); err != nil {
		t.Fatalf("sweepRunWatchers: %v", err)
	}
}

// TestLostRun_ALapsedTokenCutsTheProxy is RL-9's security fix. A run whose
// token lapsed (the control plane was down past the proxy's renew window) has
// a proxy that forwards allowlisted egress on a dead identity, audit-dark. The
// sweep marks it lost (outage) and removes that proxy, so it has no egress,
// while its agent keeps running and the run is kept. Every later pass
// re-asserts the removal.
func TestLostRun_ALapsedTokenCutsTheProxy(t *testing.T) {
	f := newLostFixture(t)
	f.sweepTokens(t)
	if f.lr.proxyStopCount() != 0 {
		t.Fatal("a run whose token is live lost its proxy")
	}

	f.ls.lapsed = true
	f.sweepTokens(t)
	if lostAt, reason := f.st.lost(); lostAt == nil || reason != types.LostOutage {
		t.Fatalf("lost = %v %q, want the run marked lost (outage)", lostAt, reason)
	}
	if f.lr.proxyStopCount() != 1 {
		t.Fatalf("StopProxy = %d, want 1: a run with a dead identity must lose its egress", f.lr.proxyStopCount())
	}
	if f.rn.endCount() != 0 || f.rn.stopCount() != 0 || f.st.State() != types.RunRunning {
		t.Errorf("EndSandbox %d, StopSandbox %d, state %s; want 0, 0, RUNNING — the agent keeps running and the run is kept",
			f.rn.endCount(), f.rn.stopCount(), f.st.State())
	}
	if f.brk.count(f.run.ID) != 1 || f.idp.count() != 0 {
		t.Errorf("broker revocations %d, identity revocations %d; want 1 and 0", f.brk.count(f.run.ID), f.idp.count())
	}
	if calls := f.fa.cancelledCalls(); len(calls) != 1 || calls[0].Reason != "run_lost" {
		t.Errorf("approval cancels = %+v, want one with reason run_lost", calls)
	}
	lost := f.audit.eventsFor(f.run.ID, "run.lost")
	if len(lost) != 1 || leaseAuditData(t, lost[0])["kept"] != true || leaseAuditData(t, lost[0])["reason"] != "outage" {
		t.Fatalf("run.lost events = %+v, want one with kept:true, reason outage", lost)
	}

	f.sweepTokens(t)
	f.sweep(t)
	if f.lr.proxyStopCount() != 2 || f.rn.endCount() != 0 || f.brk.count(f.run.ID) != 1 {
		t.Errorf("later passes: StopProxy %d, EndSandbox %d, broker revocations %d; want the proxy removal re-asserted once, the agent left, one revoke",
			f.lr.proxyStopCount(), f.rn.endCount(), f.brk.count(f.run.ID))
	}
	if len(f.audit.eventsFor(f.run.ID, "run.lost")) != 1 {
		t.Error("a later pass audited run.lost again")
	}
}

// TestLostRun_ALapsedTokenFailsClosed: a run with a dead identity that cannot
// be kept is torn down (FAILED, the full revoke cascade), never left running
// with its proxy up.
func TestLostRun_ALapsedTokenFailsClosed(t *testing.T) {
	cases := map[string]func(f *lostFixture){
		"a headless run":            func(f *lostFixture) { f.st.run.Interactive = false },
		"a substrate that can't":    func(f *lostFixture) { f.lr.proxyErr = runner.ErrEndUnsupported },
		"a runner without the stop": func(f *lostFixture) { f.srv.cfg.Runner = f.rn },
		"a run past its end and grace": func(f *lostFixture) {
			end := f.now.Add(-8 * 24 * time.Hour)
			f.st.run.EndsAt = &end
		},
	}
	for name, arrange := range cases {
		t.Run(name, func(t *testing.T) {
			f := newLostFixture(t)
			f.ls.lapsed = true
			arrange(f)
			f.sweepTokens(t)
			if f.st.State() != types.RunFailed || f.rn.stopCount() != 1 {
				t.Fatalf("state %s, StopSandbox %d; want FAILED, 1 — a dead identity must not keep its proxy",
					f.st.State(), f.rn.stopCount())
			}
			if f.idp.count() == 0 {
				t.Error("the run identity was not revoked on the fail-closed teardown")
			}
		})
	}
}

// TestLostRun_ARebootKeepsTheRun: the watcher finds an interactive run's agent
// container exited but still there (a reboot). Instead of finalizing it and
// deleting its files, the run is marked lost (reboot), its agent kept stopped
// and its proxy removed. A headless run, or a container that is gone, is
// finalized as before.
func TestLostRun_ARebootKeepsTheRun(t *testing.T) {
	f := newLostFixture(t)
	f.sweepWatchers(t)
	if lostAt, reason := f.st.lost(); lostAt == nil || reason != types.LostReboot {
		t.Fatalf("lost = %v %q, want the run marked lost (reboot)", lostAt, reason)
	}
	if f.rn.endCount() != 1 || f.rn.stopCount() != 0 || f.st.State() != types.RunRunning {
		t.Fatalf("EndSandbox %d, StopSandbox %d, state %s; want 1, 0, RUNNING — the files must survive the reboot",
			f.rn.endCount(), f.rn.stopCount(), f.st.State())
	}
	if lost := f.audit.eventsFor(f.run.ID, "run.lost"); len(lost) != 1 || leaseAuditData(t, lost[0])["reason"] != "reboot" {
		t.Errorf("run.lost events = %+v, want one with reason reboot", lost)
	}

	t.Run("a headless run", func(t *testing.T) {
		f := newLostFixture(t)
		f.st.run.Interactive = false
		f.st.run.AgentExecID = "exec-1"
		f.sweepWatchers(t)
		if f.st.State() != types.RunFailed || f.rn.stopCount() != 1 || f.rn.endCount() != 0 {
			t.Errorf("state %s, StopSandbox %d, EndSandbox %d; want FAILED, 1, 0", f.st.State(), f.rn.stopCount(), f.rn.endCount())
		}
	})
	t.Run("a container that is gone", func(t *testing.T) {
		f := newLostFixture(t)
		f.lr.exitCode = nil
		f.sweepWatchers(t)
		if f.st.State() != types.RunFailed || f.rn.stopCount() != 1 || f.rn.endCount() != 0 {
			t.Errorf("state %s, StopSandbox %d, EndSandbox %d; want FAILED, 1, 0", f.st.State(), f.rn.stopCount(), f.rn.endCount())
		}
	})
}

// TestLostRun_KeptUntilItsEndAndGrace: a lost run is kept while its lease or
// grace is live. An outage run's agent is stopped once its end passes, and the
// run is torn down at its end plus the grace; a lost run with no end stays kept.
func TestLostRun_KeptUntilItsEndAndGrace(t *testing.T) {
	f := newLostFixture(t)
	end := f.now.Add(time.Hour)
	f.st.run.EndsAt = &end
	f.ls.lapsed = true
	f.sweepTokens(t)
	if f.lr.proxyStopCount() != 1 || f.rn.endCount() != 0 {
		t.Fatalf("inside the lease: StopProxy %d, EndSandbox %d; want 1, 0", f.lr.proxyStopCount(), f.rn.endCount())
	}

	f.now = end.Add(time.Minute)
	f.sweep(t)
	if f.rn.endCount() != 1 || f.st.State() != types.RunRunning {
		t.Fatalf("past the end: EndSandbox %d, state %s; want the agent stopped and the run kept", f.rn.endCount(), f.st.State())
	}

	f.now = end.Add(7 * 24 * time.Hour)
	f.sweep(t)
	if f.st.State() != types.RunStopped || f.rn.stopCount() != 1 {
		t.Fatalf("past the end and grace: state %s, StopSandbox %d; want STOPPED, 1", f.st.State(), f.rn.stopCount())
	}
	if len(f.audit.eventsFor(f.run.ID, "run.lost.expired")) != 1 {
		t.Error("no run.lost.expired audit row")
	}

	noEnd := newLostFixture(t)
	noEnd.sweepWatchers(t)
	noEnd.now = noEnd.now.Add(365 * 24 * time.Hour)
	noEnd.sweep(t)
	if noEnd.st.State() != types.RunRunning || noEnd.rn.stopCount() != 0 {
		t.Errorf("a lost run with no end: state %s, StopSandbox %d; want kept until someone kills it",
			noEnd.st.State(), noEnd.rn.stopCount())
	}
}

// renewStampStore is renewStore plus the token stamp.
type renewStampStore struct {
	*renewStore
	stamps   int
	stampErr error
}

func (s *renewStampStore) StampRunTokenRenewed(context.Context, uuid.UUID) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stampErr != nil {
		return false, s.stampErr
	}
	if s.run.LostAt != nil {
		return false, nil
	}
	s.stamps++
	return true, nil
}

func (s *renewStampStore) ListLapsedTokenRuns(context.Context, time.Duration) ([]types.AgentRun, error) {
	return nil, nil
}

func (s *renewStampStore) MarkRunLost(context.Context, uuid.UUID, types.LostReason, time.Time, time.Duration) (bool, error) {
	return false, nil
}

// TestLostRun_RenewStampsAndRefusesAKeptRun: every renew stamps the run, which
// is what the lapsed-token sweep reads; a stamp that cannot be written hands
// out no token (so a stamp is never older than the token the proxy holds);
// and a kept run, ended or lost, never renews.
func TestLostRun_RenewStampsAndRefusesAKeptRun(t *testing.T) {
	h, _, rs, runID := newRenewHarness(t)
	st := &renewStampStore{renewStore: rs}
	srv := New(baseTestConfig(h, st))
	tok := h.mintRunToken(t, runID)

	if w := do(t, srv, "POST", "/api/v1/internal/token/renew", tok, ""); w.Code != 200 || st.stamps != 1 {
		t.Fatalf("renew: code %d, stamps %d; want 200 and 1", w.Code, st.stamps)
	}

	st.stampErr = errors.New("pg: connection refused")
	w := do(t, srv, "POST", "/api/v1/internal/token/renew", tok, "")
	if w.Code != 503 {
		t.Fatalf("renew with a failing stamp: code %d, want 503", w.Code)
	}
	if body := w.Body.String(); strings.Contains(body, `"token"`) {
		t.Fatalf("a renew whose stamp failed handed out a token: %s", body)
	}
	st.stampErr = nil

	lostAt := time.Now()
	st.mu.Lock()
	st.run.LostAt, st.run.LostReason = &lostAt, types.LostOutage
	st.mu.Unlock()
	if w := do(t, srv, "POST", "/api/v1/internal/token/renew", tok, ""); w.Code != 403 {
		t.Fatalf("renew of a lost run: code %d, want 403", w.Code)
	}
	if !hasAudit(h, "identity.renew", "denied") {
		t.Error("no identity.renew/denied audit row for a lost run's renew")
	}
}

// TestLostRun_ATransientStopProxyErrorKeepsTheRun is #1060 (0.8 review F06):
// a proxy stop that fails with anything but ErrEndUnsupported must not tear
// the run down, which would remove the agent container and its files on the
// same failing daemon. The run stays kept and RUNNING, its containment
// unresolved (audited, containment_error set); every pass retries the stop
// without a teardown, and the pass that lands it clears the error and audits
// the resolution once.
func TestLostRun_ATransientStopProxyErrorKeepsTheRun(t *testing.T) {
	f := newLostFixture(t)
	f.ls.lapsed = true
	f.lr.proxyErr = errors.New("docker: stop proxy: context deadline exceeded")
	f.sweepTokens(t)

	if f.st.State() != types.RunRunning || f.rn.stopCount() != 0 {
		t.Fatalf("state %s, StopSandbox %d; want RUNNING, 0 — a transient stop error must not tear the run down",
			f.st.State(), f.rn.stopCount())
	}
	lost := f.audit.eventsFor(f.run.ID, "run.lost")
	if len(lost) != 1 {
		t.Fatalf("run.lost events = %+v, want one", lost)
	}
	if d := leaseAuditData(t, lost[0]); d["kept"] != true || d["containment"] != "unresolved" || d["lost_error"] == nil {
		t.Errorf("run.lost data = %v, want kept:true, containment:unresolved and lost_error", d)
	}
	if f.st.containmentError() == "" {
		t.Error("containment_error not set on a run whose proxy stop failed")
	}
	if f.brk.count(f.run.ID) != 1 {
		t.Errorf("broker revocations = %d, want 1 — the credentials go even when the stop fails", f.brk.count(f.run.ID))
	}

	for range 3 {
		f.now = f.now.Add(time.Minute)
		f.sweep(t)
	}
	if f.st.State() != types.RunRunning || f.rn.stopCount() != 0 || f.lr.proxyStopCount() != 4 || f.st.containmentError() == "" {
		t.Fatalf("a persistent error: state %s, StopSandbox %d, StopProxy %d, containment_error %q; want RUNNING, 0, 4, still set",
			f.st.State(), f.rn.stopCount(), f.lr.proxyStopCount(), f.st.containmentError())
	}
	if got := f.audit.eventsFor(f.run.ID, "run.containment.reassert"); len(got) != 0 {
		t.Errorf("run.containment.reassert events = %+v; want none — run.lost already audited this failure in this process", got)
	}

	f.lr.proxyErr = nil
	f.sweep(t)
	f.sweep(t)
	if f.lr.proxyStopCount() != 6 || f.st.containmentError() != "" {
		t.Fatalf("StopProxy %d, containment_error %q; want the stop re-asserted and the error cleared",
			f.lr.proxyStopCount(), f.st.containmentError())
	}
	resolved := f.audit.eventsFor(f.run.ID, "run.containment.reassert")
	if len(resolved) != 1 || resolved[0].Outcome != "success" || leaseAuditData(t, resolved[0])["containment"] != "resolved" {
		t.Errorf("run.containment.reassert events = %+v, want one success with containment:resolved", resolved)
	}
	if f.st.State() != types.RunRunning || f.rn.stopCount() != 0 {
		t.Errorf("after the resolution: state %s, StopSandbox %d; want RUNNING, 0", f.st.State(), f.rn.stopCount())
	}
}
