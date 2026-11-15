// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func (m *memProxyStarts) RecordProxyContainment(_ context.Context, runID uuid.UUID, gen int, stopErr string) error {
	now := m.tick()
	m.pmu.Lock()
	defer m.pmu.Unlock()
	p := m.starts[runID][gen]
	if p == nil || p.State != types.ProxyStartFailed || p.ContainedAt != nil {
		return nil
	}
	p.ContainmentError = stopErr
	if stopErr == "" {
		p.ContainedAt = &now
	}
	return nil
}

func (f *readbackFixture) setProxyStopErr(err error) {
	f.lr.mu.Lock()
	f.lr.proxyErr = err
	f.lr.mu.Unlock()
}

// failWithUnstoppableProxy revives the run, makes the proxy stop fail, and posts a report of other
// bytes: the start fails with its proxy stop unresolved. It answers the start's generation.
func (f *readbackFixture) failWithUnstoppableProxy(t *testing.T) int {
	t.Helper()
	if code := f.revive(t); code != http.StatusOK {
		t.Fatalf("revive: code %d, want 200", code)
	}
	_, _, jti := f.lastHandover(t)
	f.setProxyStopErr(errors.New("daemon down"))
	if code, state := postLoaded(t, f.srv, f.run.ID, jti, hashHex([]byte("other bytes"))); code != http.StatusOK || state != string(types.ProxyStartFailed) {
		t.Fatalf("mismatched report: %d %q, want 200 failed", code, state)
	}
	p := f.active(t)
	if p.State != types.ProxyStartFailed || p.ContainedAt != nil || !strings.Contains(p.ContainmentError, "daemon down") {
		t.Fatalf("start = %s (contained %v, error %q), want failed and unresolved", p.State, p.ContainedAt, p.ContainmentError)
	}
	return p.Generation
}

// A mismatch whose proxy stop fails stays unresolved, on every retry that fails too, until a stop
// really succeeds; then it is contained, and asking again stops nothing more.
func TestReadback_MismatchContainmentUnresolvedUntilTheStopSucceeds(t *testing.T) {
	f := newReadbackFixture(t)
	gen := f.failWithUnstoppableProxy(t)
	if c := f.srv.containProxyStart(context.Background(), f.run.ID, gen); c.Contained || c.Superseded || c.Err == nil {
		t.Fatalf("retry with the stop still failing = %+v, want unresolved", c)
	}
	if p := f.active(t); p.ContainedAt != nil {
		t.Fatal("a failed stop must not record the start contained")
	}
	f.setProxyStopErr(nil)
	if err := f.srv.sweepOverdueProxyStarts(context.Background()); err != nil {
		t.Fatal(err)
	}
	if p := f.active(t); p.ContainedAt == nil || p.ContainmentError != "" {
		t.Fatalf("after a stop that succeeds: contained %v, error %q; want contained", p.ContainedAt, p.ContainmentError)
	}
	stops := f.lr.proxyStopCount()
	if c := f.srv.containProxyStart(context.Background(), f.run.ID, gen); !c.Contained || c.Err != nil {
		t.Fatalf("a contained start answers %+v, want contained", c)
	}
	if f.lr.proxyStopCount() != stops {
		t.Error("a contained start must not be stopped again")
	}
}

// Containment is fenced on its generation: once a newer start is the run's active one, retrying the
// older failed start's stop stops nothing, from a direct call or the sweep.
func TestReadback_ContainmentNeverStopsANewerStart(t *testing.T) {
	f := newReadbackFixture(t)
	gen := f.failWithUnstoppableProxy(t)
	if _, err := f.rb.BeginProxyStart(context.Background(), f.run.ID, gen+1, "newer-jti", store.ProxyStartExpected, false); err != nil {
		t.Fatal(err)
	}
	f.setProxyStopErr(nil)
	stops, sandboxStops := f.lr.proxyStopCount(), f.lr.stopCount()
	if c := f.srv.containProxyStart(context.Background(), f.run.ID, gen); !c.Superseded || c.Contained {
		t.Fatalf("an older start's containment = %+v, want superseded", c)
	}
	if err := f.srv.sweepOverdueProxyStarts(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.lr.proxyStopCount() != stops || f.lr.stopCount() != sandboxStops {
		t.Error("an older start's containment must never stop the newer start's proxy")
	}
	if p := f.rb.get(f.run.ID, gen); p.ContainedAt != nil {
		t.Error("an older start whose stop was never confirmed must not be recorded contained")
	}
}

// A revive that arrives while containment of the failed start N is paused between its generation read
// and its stop must not have its new proxy (N+1) stopped: containment holds the run's lock across the
// read and the stop, so the revive waits for it and replaces the proxy after the stop. Containment
// asked again then answers Superseded and stops nothing.
func TestReadback_ContainmentPausedAcrossARevive(t *testing.T) {
	f := newReadbackFixture(t)
	gen := f.failWithUnstoppableProxy(t)
	f.setProxyStopErr(nil)
	f.srv.liveRevived.Delete(f.run.ID) // the live-revive bound is not what this test is about
	stopsAtReplace := -1
	f.rr.onReplace = func([]byte) { stopsAtReplace = f.lr.proxyStopCount() }
	revived := make(chan int, 1)
	old := proxyContainPaused
	proxyContainPaused = func() {
		proxyContainPaused = func() {}
		// The proof: containment holds the run's lock across its read and its stop.
		if _, unlock, ok := f.srv.tryLockRunOp(context.Background(), f.run.ID); ok {
			unlock()
			t.Error("the run's lock is free between containment's generation read and its stop")
		}
		go func() { revived <- f.revive(t) }()
		select { // without the lock the revive would complete here, before the stop
		case code := <-revived:
			revived <- code
		case <-time.After(500 * time.Millisecond):
		}
	}
	t.Cleanup(func() { proxyContainPaused = old })
	c := f.srv.containProxyStart(context.Background(), f.run.ID, gen)
	var code int
	select {
	case code = <-revived:
	case <-time.After(10 * time.Second):
		t.Fatal("the revive never finished")
	}
	if code != http.StatusOK {
		t.Fatalf("revive: code %d, want 200", code)
	}
	if !c.Contained || c.Err != nil {
		t.Errorf("containment of start %d = %+v, want contained before the revive", gen, c)
	}
	if p := f.active(t); p.Generation <= gen {
		t.Fatalf("active start %d after the revive, want newer than %d", p.Generation, gen)
	}
	if stops := f.lr.proxyStopCount(); stopsAtReplace < 0 || stops != stopsAtReplace {
		t.Fatalf("proxy stops = %d, %d at the revive's replace: the newer start's proxy was stopped", stops, stopsAtReplace)
	}
	if c := f.srv.containProxyStart(context.Background(), f.run.ID, gen); !c.Superseded || c.Contained {
		t.Errorf("containment of start %d after the revive = %+v, want superseded", gen, c)
	}
	if f.lr.proxyStopCount() != stopsAtReplace {
		t.Error("containment of an older start stopped the newer start's proxy")
	}
}

// lostWriteFailStore is readbackStore whose lost marks cannot be written, as during a database blip.
type lostWriteFailStore struct{ *readbackStore }

func (lostWriteFailStore) MarkRunLost(context.Context, uuid.UUID, types.LostReason, time.Time, time.Duration) (bool, error) {
	return false, errors.New("database down")
}

func (lostWriteFailStore) MarkRunLostAtGeneration(context.Context, uuid.UUID, int, types.LostReason, time.Time, time.Duration) (bool, error) {
	return false, errors.New("database down")
}

// A revive whose new proxy sends no hash, when neither the lost mark nor the proxy stop lands: the
// answer keeps 503 revive_recovery_unresolved but never says the proxy was stopped, and the start
// stays unresolved.
func TestReadback_ReviveUnresolvedNeverClaimsAStopThatFailed(t *testing.T) {
	f := newReadbackFixture(t)
	f.srv.cfg.Store = lostWriteFailStore{f.rb}
	f.hr.hash = func([]byte) string { return "" }
	f.setProxyStopErr(errors.New("daemon down"))
	rec := do(t, f.srv, http.MethodPost, "/api/v1/runs/"+f.run.ID.String()+"/revive", adminToken, "")
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), reasonReviveRecoveryUnresolved) {
		t.Fatalf("revive = %d %s, want 503 %s", rec.Code, rec.Body.String(), reasonReviveRecoveryUnresolved)
	}
	if body := rec.Body.String(); strings.Contains(body, "was stopped") || !strings.Contains(body, "not confirmed") {
		t.Errorf("the answer claims a stop that failed: %s", body)
	}
	if p := f.active(t); p.State != types.ProxyStartFailed || p.ContainedAt != nil || p.ContainmentError == "" {
		t.Errorf("start = %s (contained %v, error %q), want failed and unresolved", p.State, p.ContainedAt, p.ContainmentError)
	}
}

// unconfirmedFailureRow is the run's one run.policy.unconfirmed failure row, decoded.
func unconfirmedFailureRow(t *testing.T, f *readbackFixture) map[string]any {
	t.Helper()
	var out map[string]any
	for _, ev := range f.audit.eventsFor(f.run.ID, "run.policy.unconfirmed") {
		if ev.Outcome == "failure" {
			if out != nil {
				t.Fatal("want one run.policy.unconfirmed failure row")
			}
			if err := json.Unmarshal(ev.Data, &out); err != nil {
				t.Fatal(err)
			}
		}
	}
	if out == nil {
		t.Fatal("no run.policy.unconfirmed failure row")
	}
	return out
}

// The failure row's stopped follows the stop: false, with the stop's error, when it failed.
func TestReadback_FailureRowStoppedFalseWhenTheStopFails(t *testing.T) {
	f := newReadbackFixture(t)
	f.failWithUnstoppableProxy(t)
	row := unconfirmedFailureRow(t, f)
	if row["stopped"] != false || !strings.Contains(fmt.Sprint(row["containment_error"]), "daemon down") {
		t.Errorf("failure row = %v, want stopped false with the stop's error", row)
	}
}

// The failure row's stopped is false, naming the lock, when another operation held the run's lock and
// containment was left to the sweep.
func TestReadback_FailureRowStoppedFalseWhenTheLockIsBusy(t *testing.T) {
	f := newReadbackFixture(t)
	if code := f.revive(t); code != http.StatusOK {
		t.Fatalf("revive: code %d, want 200", code)
	}
	_, _, jti := f.lastHandover(t)
	_, unlock, ok := f.srv.tryLockRunOp(context.Background(), f.run.ID)
	if !ok {
		t.Fatal("take the run's lock")
	}
	code, state := postLoaded(t, f.srv, f.run.ID, jti, hashHex([]byte("other bytes")))
	unlock()
	if code != http.StatusOK || state != string(types.ProxyStartFailed) {
		t.Fatalf("mismatched report: %d %q, want 200 failed", code, state)
	}
	row := unconfirmedFailureRow(t, f)
	if row["stopped"] != false || row["containment_error"] != errRunOpBusy.Error() {
		t.Errorf("failure row = %v, want stopped false with the busy lock", row)
	}
}

// A run whose lock another operation holds is not contained now: containProxyStart answers
// errRunOpBusy, stops nothing, and writes nothing on the start, for the sweep to retry.
func TestReadback_BusyLockLeavesContainmentToTheSweep(t *testing.T) {
	f := newReadbackFixture(t)
	gen := f.failWithUnstoppableProxy(t)
	f.setProxyStopErr(nil)
	before, stops := f.active(t), f.lr.proxyStopCount()
	_, unlock, ok := f.srv.tryLockRunOp(context.Background(), f.run.ID)
	if !ok {
		t.Fatal("take the run's lock")
	}
	c := f.srv.containProxyStart(context.Background(), f.run.ID, gen)
	unlock()
	if !errors.Is(c.Err, errRunOpBusy) || c.Contained || c.Superseded {
		t.Fatalf("containment under a busy lock = %+v, want errRunOpBusy", c)
	}
	if after := f.active(t); f.lr.proxyStopCount() != stops || after.ContainedAt != nil || after.ContainmentError != before.ContainmentError {
		t.Errorf("a busy lock stopped or recorded something: stops %d -> %d, start %+v", stops, f.lr.proxyStopCount(), after)
	}
}

// A plain revive whose new proxy sends no hash and whose stop fails answers 502
// revive_policy_unconfirmed_lost without saying the proxy was stopped.
func TestReadback_RevivePolicyUnconfirmedNeverClaimsAStopThatFailed(t *testing.T) {
	f := newReadbackFixture(t)
	f.hr.hash = func([]byte) string { return "" }
	f.setProxyStopErr(errors.New("daemon down"))
	rec := do(t, f.srv, http.MethodPost, "/api/v1/runs/"+f.run.ID.String()+"/revive", adminToken, "")
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), reasonRevivePolicyUnconfirmedLost) {
		t.Fatalf("revive = %d %s, want 502 %s", rec.Code, rec.Body.String(), reasonRevivePolicyUnconfirmedLost)
	}
	if body := rec.Body.String(); strings.Contains(body, "so it was stopped") || !strings.Contains(body, "not confirmed") {
		t.Errorf("the answer claims a stop that failed: %s", body)
	}
}

// A create whose answer cannot be recorded fails the run, and its start is failed by a second answer
// whose deadline already passed, so the stop is recorded and the sweep could find it.
func TestReadback_UnrecordedAnswerFailsTheStartForTheSweep(t *testing.T) {
	d := newDispatchReadback(t, hashHex([]byte("bytes")), false)
	d.st.failAnswers = 1
	d.dispatch()
	if got := d.st.State(); got != types.RunFailed {
		t.Fatalf("run state = %s, want FAILED", got)
	}
	if p := d.st.get(d.run.ID, 1); p.State != types.ProxyStartFailed || p.ContainedAt == nil {
		t.Errorf("start = %s (contained %v), want failed and contained", p.State, p.ContainedAt)
	}
}

// An answer that committed (a report already held, the hashes equal) but whose write returned an
// error is read back, not inferred: the start is enforced and the revive succeeds, with no stop.
func TestReadback_AnswerCommittedDespiteItsErrorStands(t *testing.T) {
	f := newReadbackFixture(t)
	f.rr.onReplace = func(cfg []byte) {
		var c struct {
			RunToken string `json:"run_token"`
		}
		_ = json.Unmarshal(cfg, &c)
		claims, err := f.srv.cfg.Identity.Verify(context.Background(), c.RunToken, internalAudience)
		if err != nil {
			t.Errorf("verify the new token: %v", err)
			return
		}
		postLoaded(t, f.srv, f.run.ID, claims.JTI, hashHex(cfg))
	}
	f.rb.commitThenFailAnswers = 1
	f.setProxyStopErr(errors.New("daemon down"))
	stops := f.lr.proxyStopCount()
	if code := f.revive(t); code != http.StatusOK {
		t.Fatalf("revive: code %d, want 200 (the answer committed)", code)
	}
	if p := f.active(t); p.State != types.ProxyStartEnforced {
		t.Errorf("start = %s, want enforced", p.State)
	}
	if f.lr.proxyStopCount() != stops {
		t.Error("a committed matching answer must not stop the proxy")
	}
}

// An answer refused twice (the answer, then the failing retry) leaves the start pending, and the
// revive's answer promises no sweep for it.
func TestReadback_AnswerRefusedTwicePromisesNoSweep(t *testing.T) {
	f := newReadbackFixture(t)
	f.rb.failAnswers = 2
	f.setProxyStopErr(errors.New("daemon down"))
	rec := do(t, f.srv, http.MethodPost, "/api/v1/runs/"+f.run.ID.String()+"/revive", adminToken, "")
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), reasonRevivePolicyUnconfirmedLost) {
		t.Fatalf("revive = %d %s, want 502 %s", rec.Code, rec.Body.String(), reasonRevivePolicyUnconfirmedLost)
	}
	if body := rec.Body.String(); !strings.Contains(body, "not confirmed yet") || strings.Contains(body, "sweep") {
		t.Errorf("the answer promises a sweep for a start that stays pending: %s", body)
	}
	if p := f.active(t); p.State != types.ProxyStartPending || p.Deadline != nil {
		t.Errorf("start = %s (deadline %v), want pending with no deadline", p.State, p.Deadline)
	}
}
