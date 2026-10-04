// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// cancelAwareStore honours the context on the two writes a revive's recovery
// depends on, as a Postgres write does, and can be taken down.
type cancelAwareStore struct {
	*reviveStore
	wmu          sync.Mutex
	down         error // MarkRunLost fails with this whatever the context
	lostCanceled int
	lostBounded  bool // a MarkRunLost that landed ran under a deadline
}

func (s *cancelAwareStore) MarkRunRevived(ctx context.Context, id uuid.UUID, from types.LostReason, ended *store.EndedKept, limit int) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	return s.reviveStore.MarkRunRevived(ctx, id, from, ended, limit)
}

func (s *cancelAwareStore) MarkRunLost(ctx context.Context, id uuid.UUID, why types.LostReason, at time.Time, life time.Duration) (bool, error) {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	if err := ctx.Err(); err != nil {
		s.lostCanceled++
		return false, err
	}
	if s.down != nil {
		return false, s.down
	}
	_, s.lostBounded = ctx.Deadline()
	return s.reviveStore.MarkRunLost(ctx, id, why, at, life)
}

// cancelRunner cancels the request at one boundary of the revive.
type cancelRunner struct {
	*startingRunner
	cancel context.CancelFunc
	// at is "replace-fails" (the replace fails once the request is gone),
	// "replace-ok" (it lands, then the request goes) or "start-fails" (the
	// agent start fails as the request goes).
	at string
	// replaceBounded is whether ReplaceProxy ran under a deadline.
	replaceBounded bool
}

func (r *cancelRunner) ReplaceProxy(ctx context.Context, ref string, cfg []byte) error {
	_, r.replaceBounded = ctx.Deadline()
	switch r.at {
	case "replace-fails":
		r.cancel()
		return errors.Join(runner.ErrProxyReplaceFailed, ctx.Err())
	case "replace-ok":
		err := r.startingRunner.ReplaceProxy(ctx, ref, cfg)
		r.cancel()
		return err
	}
	return r.startingRunner.ReplaceProxy(ctx, ref, cfg)
}

func (r *cancelRunner) StartSandbox(ctx context.Context, ref string) error {
	if r.at == "start-fails" {
		r.cancel()
		return errors.Join(errors.New("docker: start agent"), ctx.Err())
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return r.startingRunner.StartSandbox(ctx, ref)
}

var testVia = types.DelegationVia{Delegate: uuid.New(), Grant: uuid.New()}

// cancelRevive runs a revive of the fixture's run for a delegated owner whose
// request is cancelled at the boundary at names. The returned func reports the
// retiring token, a real one spliced into the run's stored config.
type cancelCase struct {
	f        *reviveFixture
	st       *cancelAwareStore
	rn       *cancelRunner
	ctx      context.Context
	cancel   context.CancelFunc
	retiring string
}

func newCancelCase(t *testing.T, f *reviveFixture, sr *startingRunner, at string) *cancelCase {
	t.Helper()
	c := &cancelCase{f: f}
	c.ctx, c.cancel = context.WithCancel(audit.WithDelegation(context.Background(), testVia))
	t.Cleanup(c.cancel)
	c.st = &cancelAwareStore{reviveStore: f.rs}
	if sr == nil {
		sr = &startingRunner{reviveRunner: f.rr}
	}
	c.rn = &cancelRunner{startingRunner: sr, cancel: c.cancel, at: at}
	f.srv.cfg.Store, f.srv.cfg.Runner = c.st, c.rn
	old, err := f.srv.cfg.Identity.MintRunIdentity(context.Background(), f.run.ID, f.run.CreatedBy, f.run.CreatedBy, internalAudience, false)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := proxy.LoadConfigBytes(f.rs.cfg)
	if err != nil {
		t.Fatal(err)
	}
	cfg.RunToken = old.Token
	if f.rs.cfg, err = json.Marshal(cfg); err != nil {
		t.Fatal(err)
	}
	c.retiring = old.Token
	seedPendingApproval(t, f.fa, f.run.ID) // the fixture's first loss cancelled the seeded one
	return c
}

func (c *cancelCase) revive() (reviveResult, *reviveError) {
	return c.f.srv.reviveRunProxy(c.ctx, c.f.run, types.ActorHuman, "synthetic-owner", true)
}

func (c *cancelCase) retiringLive() bool {
	_, err := c.f.srv.cfg.Identity.Verify(context.Background(), c.retiring, internalAudience)
	return err == nil
}

// assertLostAgain is what a revive cancelled after its claim must leave: the
// lost mark written, the proxy stopped, the retiring token revoked, approvals
// cancelled, and the audit rows carrying the delegation the request came in on.
func (c *cancelCase) assertLostAgain(t *testing.T, rerr *reviveError, want types.LostReason, ends, cancels int) {
	t.Helper()
	f := c.f
	if rerr == nil || !rerr.lost {
		t.Fatalf("revive = %+v; want a refusal that says lost, because lost was written", rerr)
	}
	lostAt, reason := f.st.lost()
	if lostAt == nil || reason != want || f.st.State() != types.RunRunning {
		t.Errorf("stored lost = %v %q state %s; want lost (%s) and RUNNING", lostAt, reason, f.st.State(), want)
	}
	if got := f.rn.endCount() - ends; got != 1 {
		t.Errorf("EndSandbox +%d; want the agent contained once", got)
	}
	if c.retiringLive() {
		t.Error("the retiring token still verifies; want it revoked")
	}
	if got := len(f.fa.cancelledCalls()) - cancels; got != 1 {
		t.Errorf("approval cancels +%d; want 1", got)
	}
	for _, action := range []string{"run.revive", "run.lost"} {
		evs := f.audit.eventsFor(f.run.ID, action)
		if len(evs) == 0 {
			t.Fatalf("no %s row", action)
		}
		ev := evs[len(evs)-1]
		if _, ok := leaseAuditData(t, ev)["via"]; !ok {
			t.Errorf("%s row lost the delegation of the original request: %s", action, ev.Data)
		}
		if action == "run.revive" && (ev.Outcome != "failure" || ev.Actor != "synthetic-owner" || ev.ActorType != types.ActorHuman) {
			t.Errorf("run.revive row = %s %s/%s; want a failure by the original actor", ev.Outcome, ev.ActorType, ev.Actor)
		}
	}
	if c.st.lostCanceled != 0 {
		t.Errorf("%d recovery write(s) ran on the cancelled request context", c.st.lostCanceled)
	}
	if !c.st.lostBounded {
		t.Error("the lost mark was written with no deadline; recovery must be bounded")
	}
}

// TestReviveRun_CancelAfterClaimStillPersistsLost (#1481): the request is
// cancelled after the revive claimed the run and its proxy replace failed. The
// recovery writes the lost mark on its own bounded context, and the response
// says "lost" only because that was persisted.
func TestReviveRun_CancelAfterClaimStillPersistsLost(t *testing.T) {
	f, sr := newRebootFixture(t)
	c := newCancelCase(t, f, sr, "replace-fails")
	ends, cancels := f.rn.endCount(), len(f.fa.cancelledCalls())
	_, rerr := c.revive()
	c.assertLostAgain(t, rerr, types.LostReboot, ends, cancels)
	if rerr.reason != reasonReviveProxyReplaceFailedLost {
		t.Errorf("reason %q; want %q", rerr.reason, reasonReviveProxyReplaceFailedLost)
	}
	if !c.rn.replaceBounded {
		t.Error("the replace after the claim ran with no deadline")
	}
}

// TestReviveRun_CancelAtAgentStartStillPersistsLost: the same, when the agent
// start is the step that fails as the request goes.
func TestReviveRun_CancelAtAgentStartStillPersistsLost(t *testing.T) {
	f, sr := newRebootFixture(t)
	c := newCancelCase(t, f, sr, "start-fails")
	ends, cancels := f.rn.endCount(), len(f.fa.cancelledCalls())
	_, rerr := c.revive()
	c.assertLostAgain(t, rerr, types.LostReboot, ends, cancels)
	if rerr.reason != reasonReviveAgentStartFailedLost {
		t.Errorf("reason %q; want %q", rerr.reason, reasonReviveAgentStartFailedLost)
	}
}

// TestReviveRun_CancelAfterASuccessfulReplaceFinishesTheRevive: a request
// cancelled once the new proxy is up still retires the old token and starts
// the agent behind it.
func TestReviveRun_CancelAfterASuccessfulReplaceFinishesTheRevive(t *testing.T) {
	f, sr := newRebootFixture(t)
	c := newCancelCase(t, f, sr, "replace-ok")
	_, rerr := c.revive()
	if rerr != nil {
		t.Fatalf("revive: %+v", rerr)
	}
	if lostAt, _ := f.st.lost(); lostAt != nil || len(sr.starts()) != 1 {
		t.Errorf("lost %v, agent starts %v; want the run live with its agent started", lostAt, sr.starts())
	}
	if c.retiringLive() {
		t.Error("the retiring token still verifies after the replace landed")
	}
}

// TestReviveRun_CancelBeforeTheClaimChangesNothing: cancellation may abort the
// work up to the claim; the run, its proxy and its old token are untouched.
func TestReviveRun_CancelBeforeTheClaimChangesNothing(t *testing.T) {
	f := newReviveFixture(t)
	c := newCancelCase(t, f, nil, "")
	f.rr.onEnsure = c.cancel
	stops := f.lr.proxyStopCount()
	_, rerr := c.revive()
	if rerr == nil || rerr.status != http.StatusServiceUnavailable || rerr.reason != reasonReviveClaimFailed || rerr.lost {
		t.Fatalf("revive = %+v; want 503 %s, not lost", rerr, reasonReviveClaimFailed)
	}
	if lostAt, _ := f.st.lost(); lostAt == nil || len(f.rr.replaced) != 0 || f.lr.proxyStopCount() != stops || !c.retiringLive() {
		t.Errorf("lost %v, replaces %d, StopProxy +%d, retiring live %v; want nothing changed",
			lostAt, len(f.rr.replaced), f.lr.proxyStopCount()-stops, c.retiringLive())
	}
}

// TestReviveRun_StoreDownDuringRecoveryIsUnresolved: when the lost mark cannot
// be written the response must not say lost. The proxy is stopped and the broker
// revoked anyway, and the sweep recovers the run.
func TestReviveRun_StoreDownDuringRecoveryIsUnresolved(t *testing.T) {
	f := newReviveFixture(t)
	c := newCancelCase(t, f, nil, "")
	c.st.down = errors.New("store: connection refused")
	f.rr.replaceErr = errors.Join(runner.ErrProxyReplaceFailed, errors.New("docker: start proxy: boom"))
	stops, revokes := f.lr.proxyStopCount(), f.brk.count(f.run.ID)
	_, rerr := c.revive()
	if rerr == nil || rerr.status != http.StatusServiceUnavailable || rerr.reason != "revive_recovery_unresolved" || rerr.lost {
		t.Fatalf("revive = %+v; want 503 revive_recovery_unresolved with lost false", rerr)
	}
	if f.lr.proxyStopCount() != stops+1 || f.brk.count(f.run.ID) != revokes+1 {
		t.Errorf("StopProxy +%d, broker revokes +%d; want the proxy stopped and the broker revoked once each",
			f.lr.proxyStopCount()-stops, f.brk.count(f.run.ID)-revokes)
	}
	if lostAt, _ := f.st.lost(); lostAt != nil {
		t.Error("a lost mark is stored although the write failed")
	}
}

// TestRestartRuns_ACancelledBatchRefusesTheRestBeforeTheirClaim (#1481): once
// the request is cancelled, the runs still to come are refused, each before its
// claim, and the one in flight is settled.
func TestRestartRuns_ACancelledBatchRefusesTheRestBeforeTheirClaim(t *testing.T) {
	f := newReviveFixture(t)
	f.st.run.LostAt, f.st.run.LostReason = nil, ""
	f.run = f.st.run
	c := newCancelCase(t, f, nil, "replace-fails")
	body := `{"run_ids":["` + f.run.ID.String() + `","` + f.run.ID.String() + `"]}`
	r := httptest.NewRequest(http.MethodPost, "/api/v1/admin/runs/restart", strings.NewReader(body)).WithContext(c.ctx)
	w := httptest.NewRecorder()
	f.srv.handleAdminRestartRuns(w, r)
	var out struct {
		Results []adminRestartResult `json:"results"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || len(out.Results) != 2 {
		t.Fatalf("restart = %d %s (%v); want 200 with two results", w.Code, w.Body.String(), err)
	}
	first, second := out.Results[0], out.Results[1]
	lostAt, _ := f.st.lost()
	if !first.LostAgain || lostAt == nil {
		t.Errorf("first = %+v, stored lost %v; want lost_again mirroring the stored state", first, lostAt)
	}
	if second.OK || second.LostAgain || second.Reason == reasonReviveLiveTooSoon || second.Error == "" {
		t.Errorf("second = %+v; want refused for the cancelled request, before any claim", second)
	}
}
