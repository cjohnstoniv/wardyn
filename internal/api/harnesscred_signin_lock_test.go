// Copyright 2025 The Wardyn Authors
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

// The per-person sign-in lock used to fail OPEN on every arm, a 5s wait
// included — exactly under the concurrent-sign-in burst it exists to
// serialize — leaving two live credential-bearing login sandboxes behind one
// WARN line (#505 F5). A wait that expires now refuses; only the structural
// no-capacity arm proceeds, and that one is written to the audit trail.

// errLockWaitExpired stands in for AdvisoryLockKeyed's wait arms (the
// in-process slot, lock_timeout): anything that is not ErrLoginLockNoCapacity.
var errLockWaitExpired = errors.New("db: advisory lock: context deadline exceeded")

// lockingSupersedeStore is the supersede fixture's store with the lock seam,
// answering every LockLoginSupersede with lockErr.
type lockingSupersedeStore struct {
	*supersedeStore
	lockErr error
}

func (s *lockingSupersedeStore) LockLoginSupersede(context.Context, string) (func(), error) {
	if s.lockErr != nil {
		return nil, s.lockErr
	}
	return func() {}, nil
}

func withLoginLock(f supersedeFixture, lockErr error) *lockingSupersedeStore {
	st := &lockingSupersedeStore{supersedeStore: f.store, lockErr: lockErr}
	f.srv.cfg.Store = st
	return st
}

func TestHarnessLogin_ALockWaitThatExpiresRefusesBeforeAnythingIsStarted(t *testing.T) {
	f := newSupersedeFixture(t, nil, nil)
	sess := memberLoginSession(t)
	first := launchLoginRun(t, f.srv, sess)
	waitRunState(t, f.store, first, types.RunRunning)

	withLoginLock(f, errLockWaitExpired)
	f.store.mu.Lock()
	before := len(f.store.runs)
	f.store.mu.Unlock()

	w := doSSO(t, f.srv, http.MethodPost, "/api/v1/setup/harness-login", sess, `{"provider":"aws"}`)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("code = %d, want 503 — an unserialized sign-in must not proceed; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), signInBusyRefusal) {
		t.Errorf("body = %s, want the fixed sentence %q", w.Body.String(), signInBusyRefusal)
	}
	if strings.Contains(w.Body.String(), "deadline") {
		t.Errorf("body carries the lock error's own text: %s", w.Body.String())
	}
	f.store.mu.Lock()
	after := len(f.store.runs)
	f.store.mu.Unlock()
	if after != before {
		t.Errorf("runs = %d, want %d — a refused sign-in created a sandbox anyway", after, before)
	}
	if got := f.store.stateOf(t, first); got != types.RunRunning {
		t.Errorf("the existing sign-in is %s, want RUNNING — a refused launch must not supersede anything", got)
	}
}

func TestHarnessLogin_NoPoolCapacityProceedsAndIsAudited(t *testing.T) {
	f := newSupersedeFixture(t, nil, nil)
	withLoginLock(f, fmt.Errorf("%w (max_conns 2, acquired 1, need 2 free)", store.ErrLoginLockNoCapacity))

	runID := launchLoginRun(t, f.srv, memberLoginSession(t))

	var row *types.AuditEvent
	for _, ev := range f.audit.find("auth.signin_unserialized") {
		row = &ev
	}
	if row == nil {
		t.Fatal("no auth.signin_unserialized row — the unserialized pass left only a log line")
	}
	if row.RunID == nil || row.RunID.String() != runID {
		t.Errorf("row run_id = %v, want the launched run %s", row.RunID, runID)
	}
	if row.Target != "sub-member" {
		t.Errorf("row target = %q, want the person whose sign-ins went unserialized", row.Target)
	}
	var data map[string]any
	if err := json.Unmarshal(row.Data, &data); err != nil {
		t.Fatalf("decode row data: %v", err)
	}
	if data["reason"] != signInUnserializedReasonNoCapacity {
		t.Errorf("reason = %v, want %q", data["reason"], signInUnserializedReasonNoCapacity)
	}
}

// lockingLoginRunStore is the capture route's login-run store with the lock
// seam, the run carrying a creator so the lock is actually asked for.
type lockingLoginRunStore struct {
	ssoLoginRunStore
	lockErr error
}

func (s lockingLoginRunStore) LockLoginSupersede(context.Context, string) (func(), error) {
	return nil, s.lockErr
}

func TestUploadSSOToken_ALockWaitThatExpiresStoresNothing(t *testing.T) {
	h := newHarness(t)
	runID := uuid.New()
	st := lockingLoginRunStore{
		ssoLoginRunStore: ssoLoginRunStore{
			run: types.AgentRun{
				ID: runID, Task: harnessLoginTask, Agent: awsSSOAgent, State: types.RunRunning,
				CreatedBy: "member@corp.example", UpdatedAt: time.Now().UTC(),
			},
			events: ssoLoginStartedEvents(runID, "https://my-sso.awsapps.com/start"),
		},
		lockErr: errLockWaitExpired,
	}
	sec := &memSecrets{m: map[string][]byte{}}
	cfg := baseTestConfig(h, st)
	cfg.Secrets = sec
	cfg.BedrockRegion = "us-west-2"
	srv := New(cfg)
	h.srv = srv
	tok := h.mintRunToken(t, runID)

	w := do(t, srv, http.MethodPut, "/api/v1/internal/sso-token/"+runID.String(), tok, validSSOBody)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("code = %d, want 503 — an unserialized capture must not store; body=%s", w.Code, w.Body.String())
	}
	if _, stored := sec.m[harnessCredSecretName(awsSSOProvider)]; stored {
		t.Error("the capture was stored without the per-person lock")
	}
	var refused *types.AuditEvent
	for _, ev := range h.audit.events {
		if ev.Action == "harness.credential.refused" {
			refused = &ev
		}
	}
	if refused == nil {
		t.Fatal("no harness.credential.refused row for a refused capture")
	}
	if data := killData(t, *refused); data["reason"] != refuseReasonSignInBusy {
		t.Errorf("refusal reason = %v, want %q", data["reason"], refuseReasonSignInBusy)
	}
}
