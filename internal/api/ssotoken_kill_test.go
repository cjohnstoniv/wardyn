// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// killableLoginRunStore is the login-run store with a real state CAS, so the
// kill cascade the capture schedules can actually land and be read back.
type killableLoginRunStore struct {
	ssoLoginRunStore
	mu    sync.Mutex
	state types.RunState
}

func (s *killableLoginRunStore) GetRun(context.Context, uuid.UUID) (types.AgentRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	run := s.run
	run.State = s.state
	return run, nil
}

func (s *killableLoginRunStore) UpdateRunStateIf(_ context.Context, _ uuid.UUID, from, to types.RunState) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state != from {
		return false, nil
	}
	s.state = to
	return true, nil
}

func (s *killableLoginRunStore) stateNow() types.RunState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

// TestUploadSSOToken_KillsSignInRunAfterResponse is #151's server belt: a
// capture that landed ends its own sign-in sandbox on the server, so a closed
// console tab no longer leaves it running to its idle cap. A capture that did
// NOT land leaves the run alone — the person is still signing in there.
func TestUploadSSOToken_KillsSignInRunAfterResponse(t *testing.T) {
	newSrv := func(t *testing.T) (*Server, *memAudit, *killableLoginRunStore, string, uuid.UUID) {
		t.Helper()
		h := newHarness(t)
		runID := uuid.New()
		st := &killableLoginRunStore{
			ssoLoginRunStore: ssoLoginRunStore{
				run: types.AgentRun{
					ID: runID, Task: harnessLoginTask, Agent: awsSSOAgent,
					CreatedBy: "alice@example.com", UpdatedAt: time.Now().UTC(),
				},
				events: ssoLoginStartedEvents(runID, "https://my-sso.awsapps.com/start"),
			},
			state: types.RunRunning,
		}
		audit := &memAudit{}
		cfg := baseTestConfig(h, st)
		cfg.Audit = audit
		cfg.Secrets = &memSecrets{m: map[string][]byte{}}
		cfg.BedrockRegion = "us-west-2"
		srv := New(cfg)
		srv.signInCaptureKillGrace = 0
		return srv, audit, st, h.mintRunToken(t, runID), runID
	}

	t.Run("a stored capture kills the sign-in run", func(t *testing.T) {
		srv, audit, st, tok, runID := newSrv(t)
		w := do(t, srv, http.MethodPut, "/api/v1/internal/sso-token/"+runID.String(), tok, validSSOBody)
		if w.Code != http.StatusNoContent {
			t.Fatalf("upload: code = %d, want 204; body=%s", w.Code, w.Body.String())
		}
		srv.WaitBackground()
		if got := st.stateNow(); got != types.RunKilled {
			t.Fatalf("sign-in run state = %s after a stored capture, want KILLED", got)
		}
		row := killRow(t, audit, runID.String())
		data := killData(t, row)
		if data["reason"] != signInCapturedReason {
			t.Errorf("run.kill reason = %v, want %q", data["reason"], signInCapturedReason)
		}
		if row.ActorType != types.ActorSystem || row.Actor != "wardynd" {
			t.Errorf("run.kill actor = %s/%s, want system/wardynd", row.ActorType, row.Actor)
		}
		if row.Outcome != "success" {
			t.Errorf("run.kill outcome = %q, want success", row.Outcome)
		}
	})

	t.Run("a failed store leaves the run running", func(t *testing.T) {
		srv, audit, st, tok, runID := newSrv(t)
		srv.cfg.Secrets = &failingPutSecrets{memSecrets: &memSecrets{m: map[string][]byte{}}}
		w := do(t, srv, http.MethodPut, "/api/v1/internal/sso-token/"+runID.String(), tok, validSSOBody)
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("upload: code = %d, want 500; body=%s", w.Code, w.Body.String())
		}
		srv.WaitBackground()
		if got := st.stateNow(); got != types.RunRunning {
			t.Fatalf("sign-in run state = %s after a failed store, want RUNNING — nothing landed, "+
				"so the person is still signing in there", got)
		}
		if rows := audit.find("run.kill"); len(rows) != 0 {
			t.Errorf("a failed store wrote %d run.kill rows, want 0", len(rows))
		}
	})

	// The console's own killRun (or the banner door) may end the run inside the
	// grace. The upload route refuses only KILLED, so a STOPPED run still
	// captures, and the post-grace re-read must leave it alone: a cascade here
	// would CAS it to KILLED and write a run.kill row for a run already over.
	t.Run("an already-ended run is left alone", func(t *testing.T) {
		srv, audit, st, tok, runID := newSrv(t)
		st.state = types.RunStopped
		w := do(t, srv, http.MethodPut, "/api/v1/internal/sso-token/"+runID.String(), tok, validSSOBody)
		if w.Code != http.StatusNoContent {
			t.Fatalf("upload: code = %d, want 204; body=%s", w.Code, w.Body.String())
		}
		srv.WaitBackground()
		if got := st.stateNow(); got != types.RunStopped {
			t.Fatalf("sign-in run state = %s, want STOPPED — an ended run is not re-killed", got)
		}
		if rows := audit.find("run.kill"); len(rows) != 0 {
			t.Errorf("an already-ended run got %d run.kill rows, want 0", len(rows))
		}
	})

	// Shutdown inside the grace: the wait ends at once rather than holding
	// WaitBackground for the grace, and no kill is attempted.
	t.Run("shutdown inside the grace neither waits nor kills", func(t *testing.T) {
		srv, audit, st, tok, runID := newSrv(t)
		srv.signInCaptureKillGrace = time.Hour
		baseCtx, shutdown := context.WithCancel(context.Background())
		srv.cfg.BaseCtx = baseCtx
		w := do(t, srv, http.MethodPut, "/api/v1/internal/sso-token/"+runID.String(), tok, validSSOBody)
		if w.Code != http.StatusNoContent {
			t.Fatalf("upload: code = %d, want 204; body=%s", w.Code, w.Body.String())
		}
		shutdown()
		start := time.Now()
		srv.WaitBackground()
		if waited := time.Since(start); waited > 5*time.Second {
			t.Fatalf("WaitBackground took %s after shutdown; the grace must not hold it", waited)
		}
		if got := st.stateNow(); got != types.RunRunning {
			t.Fatalf("sign-in run state = %s, want RUNNING — a shutdown inside the grace kills nothing", got)
		}
		if rows := audit.find("run.kill"); len(rows) != 0 {
			t.Errorf("a shutdown inside the grace wrote %d run.kill rows, want 0", len(rows))
		}
	})

	t.Run("a refused capture leaves the run running", func(t *testing.T) {
		srv, _, st, tok, runID := newSrv(t)
		// Foreign IdP: refused by the start_url binding, before any store.
		body := strings.Replace(validSSOBody, "https://my-sso.awsapps.com/start", "https://evil.awsapps.com/start", 1)
		w := do(t, srv, http.MethodPut, "/api/v1/internal/sso-token/"+runID.String(), tok, body)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("upload: code = %d, want 400; body=%s", w.Code, w.Body.String())
		}
		srv.WaitBackground()
		if got := st.stateNow(); got != types.RunRunning {
			t.Fatalf("sign-in run state = %s after a refused capture, want RUNNING", got)
		}
	})
}

// blockingGetRunStore's GetRun never returns on its own — only when its
// context is cancelled — standing in for a wedged read (a stalled connection
// pool, a saturated primary) that killSignInRunAfterCapture's post-grace read
// must not be allowed to ride out for as long as the kill cascade that would
// follow it.
type blockingGetRunStore struct {
	ssoLoginRunStore
}

func (s blockingGetRunStore) GetRun(ctx context.Context, _ uuid.UUID) (types.AgentRun, error) {
	<-ctx.Done()
	return types.AgentRun{}, ctx.Err()
}

// TestKillSignInRunAfterCapture_ReadIsBoundedShortOfTheCascade pins #971's
// background-budget fix. Before it, the post-grace run read shared
// killCascadeTimeout's own 30s bound with the kill cascade that may follow it,
// so together they could take up to 60s against backgroundShutdownBudget's
// 35s ceiling — a shutdown landing between the read and the cascade would
// abandon a live kill cascade mid-flight instead of getting the bounded
// window it asked for. The read alone must give up at signInCaptureReadTimeout
// (5s), well short of killCascadeTimeout (30s).
func TestKillSignInRunAfterCapture_ReadIsBoundedShortOfTheCascade(t *testing.T) {
	h := newHarness(t)
	runID := uuid.New()
	cfg := baseTestConfig(h, blockingGetRunStore{})
	srv := New(cfg)
	srv.signInCaptureKillGrace = 0

	start := time.Now()
	srv.killSignInRunAfterCapture(context.Background(), runID, "alice@example.com")
	srv.WaitBackground()
	elapsed := time.Since(start)

	if elapsed >= killCascadeTimeout {
		t.Fatalf("the post-capture read took %s — at or past killCascadeTimeout (%s); it must give up "+
			"at signInCaptureReadTimeout (%s) instead, or a shutdown mid-cascade can abandon this "+
			"goroutine past backgroundShutdownBudget", elapsed, killCascadeTimeout, signInCaptureReadTimeout)
	}
	if elapsed < signInCaptureReadTimeout {
		t.Fatalf("the post-capture read returned after %s, before its own %s timeout could even "+
			"elapse — the fixture is not exercising a blocked read", elapsed, signInCaptureReadTimeout)
	}
}
