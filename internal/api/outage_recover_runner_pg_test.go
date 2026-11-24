// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// orcRecoveryIdentity is the fixture's identity provider with an expired token to present. The embedded provider's
// clock is private to its package, so a test here cannot mint a token that has already expired: this double knows
// one real token and answers its claims, expiry moved back, as identity.RecoveryVerifier. Everything else, the mint
// of the recovered run's fresh token among it, is the embedded provider's.
type orcRecoveryIdentity struct {
	identity.Provider
	token  string
	claims identity.Claims
}

var _ identity.RecoveryVerifier = (*orcRecoveryIdentity)(nil)

func (o *orcRecoveryIdentity) VerifyExpired(_ context.Context, token, _ string, _ time.Duration) (*identity.Claims, error) {
	if token != o.token {
		return nil, errors.New("orc: not the token this double expires")
	}
	claims := o.claims
	return &claims, nil
}

// orcExpireToken mints a real token for run, makes it the run's current token, and has the server's identity provider
// answer it as expired one hour after the run's last token stamp, as it would once the proxy had not renewed for that
// long. It returns the token.
func orcExpireToken(t *testing.T, f *runnerLeaseFixture, run types.AgentRun) string {
	t.Helper()
	id, err := f.h.idp.MintRunIdentity(f.ctx, run.ID, run.CreatedBy, "", internalAudience, false)
	if err != nil {
		t.Fatalf("mint the run's token: %v", err)
	}
	claims, err := f.h.idp.Verify(f.ctx, id.Token, internalAudience)
	if err != nil {
		t.Fatalf("verify the run's token: %v", err)
	}
	var stamped time.Time
	if err := f.st.Pool.QueryRow(f.ctx, `SELECT token_renewed_at FROM agent_runs WHERE id = $1`, run.ID).Scan(&stamped); err != nil {
		t.Fatal(err)
	}
	claims.Expiry = stamped.Add(identity.RunTokenTTL)
	ohExec(t, f.st, `UPDATE agent_runs SET token_jti = $2 WHERE id = $1`, run.ID, id.JTI)
	f.srv.cfg.Identity = &orcRecoveryIdentity{Provider: f.h.idp, token: id.Token, claims: *claims}
	return id.Token
}

// orcRecover posts the recover door's request for token over the run's own runner relay, as a run on a runner's always
// arrives.
func orcRecover(t *testing.T, f *runnerLeaseFixture, token string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/internal/token/recover", nil)
	r = r.WithContext(context.WithValue(audit.WithRelay(r.Context(), f.rn.ID), relayRunnerKey{}, f.rn.ID))
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	panicFails(t, f.srv.Handler()).ServeHTTP(w, r)
	return w
}

// A runner whose link dropped while its run's token expired reconnects, and its run recovers: the window the drop
// recorded covers the expiry, so the STATE judge holds the run instead of asking for its loss, the runner is ready,
// and the proxy's relayed recover is answered with a fresh token. The run is never lost.
func TestOutageRecover_RunnerCutReconnectsReady_PG(t *testing.T) {
	f := ohRunnerFixture(t)
	run := ohRunnerRun(t, f, 70*time.Minute, ohPauses)
	ohOpenWindow(t, f.st, store.OutageKindRunnerLink, &f.rn.ID, 70*time.Minute)
	token := orcExpireToken(t, f, run)

	// The runner is back: its STATE page reports the sandbox present and the proxy up, and it is ready.
	ohRequireReady(t, f, run, false, "the runner reconnected")

	w := orcRecover(t, f, token)
	if w.Code != http.StatusOK {
		t.Fatalf("relayed recover = %d %s, want 200", w.Code, w.Body)
	}
	var resp tokenRenewResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil || resp.Token == "" {
		t.Fatalf("recover answer %q: %v, want a fresh token", w.Body, err)
	}
	fresh, err := f.h.idp.Verify(f.ctx, resp.Token, internalAudience)
	if err != nil || fresh.RunID != run.ID {
		t.Fatalf("the fresh token = %+v, %v; want a valid token of run %s", fresh, err, run.ID)
	}
	lineage, err := f.st.RunRecoveryLineage(f.ctx, run.ID)
	if err != nil || lineage.JTI != fresh.JTI || time.Since(lineage.RenewedAt) > time.Minute {
		t.Fatalf("lineage after the recover = %+v, %v; want the fresh token's jti %s stamped just now", lineage, err, fresh.JTI)
	}
	ohRequireUnmarked(t, f.st, f.srv.cfg.Audit.(*syncAudit), run, "the recovered run")
}

// The same run once its window ended more than the grace ago is not held: the STATE judge asks for its loss, as it
// does without the recovery, and the offline sweep then marks it. Inside the grace it is held and ready.
func TestOutageRecover_RunnerHoldEndedNeedsLoss_PG(t *testing.T) {
	f := ohRunnerFixture(t)
	run := ohRunnerRun(t, f, 2*time.Hour, ohPauses) // its token expired an hour ago
	ohClosedWindow(t, f.st, store.OutageKindRunnerLink, &f.rn.ID, 3*time.Hour, 24*time.Minute)
	ohRequireReady(t, f, run, false, "the window ended 24 minutes ago")

	ohExec(t, f.st, `UPDATE outage_windows SET ended_at = now() - interval '26 minutes' WHERE runner_id = $1`, f.rn.ID)

	ohRequireNeedsLoss(t, f, run, false, "the window ended 26 minutes ago")
	ohRequireUnmarked(t, f.st, f.srv.cfg.Audit.(*syncAudit), run, "the run after the refused page")
	ohOfflineSweep(t, f)
	ohRequireMarked(t, f.st, run, "the run after the offline sweep")
	if n := ohQueued(t, f, run, types.RunnerActionStopProxy); n != 1 {
		t.Errorf("stop_proxy actions queued = %d, want 1", n)
	}
}

// A held run is a fault again as soon as its runner reports its proxy stopped, or holds it contained pending its
// completion evidence: nothing is waiting for the proxy's recovery then, and the judge asks for the run's loss.
func TestRunnerState_HeldRunWithStoppedProxyNeedsLoss_PG(t *testing.T) {
	f := ohRunnerFixture(t)
	run := ohRunnerRun(t, f, 70*time.Minute, ohPauses)
	ohOpenWindow(t, f.st, store.OutageKindRunnerLink, &f.rn.ID, 70*time.Minute)
	ohRequireReady(t, f, run, false, "the proxy up")

	ohRequireNeedsLoss(t, f, run, true, "the proxy stopped")

	pending := runnerwire.RunState{RunID: run.ID, Ref: run.SandboxRef, SandboxPresent: true,
		ContainmentPending: &runnerwire.ContainmentPending{Kind: "agent_only", Since: time.Now().Add(-time.Minute)}}
	if err := finalStatePage(t, f.srv, f.rn.ID, pending); !errors.Is(err, store.ErrRunnerReconciliationRequired) {
		t.Fatalf("a page with containment pending = %v, want reconciliation required", err)
	}
	if needs, _ := ohObservation(t, f, run); !needs {
		t.Fatalf("containment pending: the judge did not ask for the run's loss, want it to")
	}
	ohRequireUnmarked(t, f.st, f.srv.cfg.Audit.(*syncAudit), run, "the judge marks nothing")
}

// A lapsed runner run is a fault for the STATE judge until a window holds it: with no window the judge asks for its
// loss, and once a window covering its expiry is on record, with its proxy up, it does not.
func TestRunnerState_HeldRunDoesNotNeedLoss_PG(t *testing.T) {
	f := ohRunnerFixture(t)
	run := ohRunnerRun(t, f, 70*time.Minute, ohPauses)
	ohRequireNeedsLoss(t, f, run, false, "no window on record")

	ohOpenWindow(t, f.st, store.OutageKindRunnerLink, &f.rn.ID, 90*time.Minute)

	ohRequireReady(t, f, run, false, "held by its runner's window")
	ohRequireUnmarked(t, f.st, f.srv.cfg.Audit.(*syncAudit), run, "the held run")
}

// A held run whose runner reports its proxy stopped is held no longer, by the sweeps as by the STATE judge: the
// offline sweep marks it lost, and the runner's next page is READY. Nothing waits for the hold to run out, with the
// runner not ready and its other held runs refused with it.
func TestOfflineFromRunner_HeldRunWithStoppedProxyIsMarkedLost_PG(t *testing.T) {
	f := ohRunnerFixture(t)
	run := ohRunnerRun(t, f, 70*time.Minute, ohPauses)
	ohOpenWindow(t, f.st, store.OutageKindRunnerLink, &f.rn.ID, 70*time.Minute)
	ohRequireReady(t, f, run, false, "the proxy up")

	ohRequireNeedsLoss(t, f, run, true, "the proxy stopped")
	if rc := ohCheck(t, f.st, run.ID); rc.Held {
		t.Fatal("the sweeps still hold a run whose runner reports its proxy stopped")
	}
	ohOfflineSweep(t, f)
	ohRequireMarked(t, f.st, run, "the held run with its proxy stopped")
	ohRequireReady(t, f, run, true, "after the loss mark")
}

// The STATE judge and the sweeps read one hold: for each report of a held run, the judge asks for the run's loss
// exactly when the sweeps' recovery check no longer holds it.
func TestRunnerState_JudgeAndSweepsAgreeOnTheHold_PG(t *testing.T) {
	stopped := time.Now().Add(-time.Second)
	for _, tc := range []struct {
		name string
		held bool
		row  func(run types.AgentRun) runnerwire.RunState
	}{
		{"the proxy up", true, func(run types.AgentRun) runnerwire.RunState {
			return runnerwire.RunState{RunID: run.ID, Ref: run.SandboxRef, SandboxPresent: true}
		}},
		{"the proxy stopped", false, func(run types.AgentRun) runnerwire.RunState {
			return runnerwire.RunState{RunID: run.ID, Ref: run.SandboxRef, SandboxPresent: true, ProxyStoppedAt: &stopped}
		}},
		{"containment pending", false, func(run types.AgentRun) runnerwire.RunState {
			return runnerwire.RunState{RunID: run.ID, Ref: run.SandboxRef, SandboxPresent: true,
				ContainmentPending: &runnerwire.ContainmentPending{Kind: "agent_only", Since: time.Now().Add(-time.Minute)}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := ohRunnerFixture(t)
			run := ohRunnerRun(t, f, 70*time.Minute, ohPauses)
			ohOpenWindow(t, f.st, store.OutageKindRunnerLink, &f.rn.ID, 70*time.Minute)
			if err := finalStatePage(t, f.srv, f.rn.ID, tc.row(run)); err != nil && !errors.Is(err, store.ErrRunnerReconciliationRequired) {
				t.Fatalf("STATE page = %v", err)
			}
			needs, _ := ohObservation(t, f, run)
			held := ohCheck(t, f.st, run.ID).Held
			if held != tc.held || needs == held {
				t.Fatalf("the sweeps hold the run: %v (want %v); the judge asks for its loss: %v (want %v)", held, tc.held, needs, !tc.held)
			}
		})
	}
}
