// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// bindBeforeSuspend runs hook once, after the suspension read the identity and before it takes the lock.
type bindBeforeSuspend struct {
	store.PG
	hook func()
}

func (s *bindBeforeSuspend) SuspendIdentity(ctx context.Context, p store.SuspendPlan) (store.SuspendResult, error) {
	if hook := s.hook; hook != nil {
		s.hook = nil
		hook()
	}
	return s.PG.SuspendIdentity(ctx, p)
}

// A SCIM-created identity whose first sign-in binds it while the suspension is between reading it and
// locking it: the newly bound principal is still cut, swept and has its run killed.
func TestSCIMSuspendCatchesAFirstBindingDuringTheSuspension(t *testing.T) {
	e := newSCIMEnv(t)
	const sub, email = "late-bound-sub", "late-bound@corp.example"
	id := e.postUserID(e.a, oidPat, email, email)
	var run uuid.UUID
	var raw string
	wrapped := &bindBeforeSuspend{PG: e.st, hook: func() {
		if _, err := e.st.IssueLoginIdentity(context.Background(), store.LoginIdentity{Principal: sub, Issuer: e.issuer, TenantID: e.tenant, ObjectID: oidPat, Email: email}, time.Now()); err != nil {
			t.Fatal(err)
		}
		run = e.seedRun(sub, types.RunRunning)
		_, raw = e.seedToken(sub, email)
	}}
	n := e.node(func(c *Config) { c.Store = wrapped })

	if w := e.patch(n, id, patchOf(`false`)); w.Code != http.StatusOK {
		t.Fatalf("suspend = %d %s", w.Code, w.Body.String())
	}
	row := e.identity(id)
	if row.Principal != sub || row.DeactivatedAt == nil {
		t.Fatalf("identity = principal %q deactivated %v, want %q deactivated", row.Principal, row.DeactivatedAt, sub)
	}
	if got := e.runState(run); got != types.RunKilled {
		t.Errorf("the newly bound principal's run = %s, want KILLED", got)
	}
	for name, node := range map[string]*scimNode{"a": e.a, "b": e.b} {
		if e.tokenWorks(node, raw) {
			t.Errorf("instance %s: the newly bound principal's token still works", name)
		}
	}
	if !e.allJobsDone(id) {
		t.Error("the ledger is not done")
	}
}
