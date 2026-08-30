// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// createFailStore is the smallest store.Store whose CreateRun fails: the
// embedded interface is nil, so any other call panics and the test stays honest
// about which seam it exercises.
type createFailStore struct{ store.Store }

func (createFailStore) CreateRun(context.Context, types.AgentRun) (types.AgentRun, error) {
	return types.AgentRun{}, errors.New("db down")
}

// revokeSpy records RevokeRun calls on top of a real embedded provider.
type revokeSpy struct {
	identity.Provider
	revoked []uuid.UUID
}

func (r *revokeSpy) RevokeRun(_ context.Context, id uuid.UUID) error {
	r.revoked = append(r.revoked, id)
	return nil
}

// A harness-login run mints its identity BEFORE the row exists; if CreateRun
// then fails, the minted token must be revoked or it outlives a run that never
// was. The site-config probe already does this; this pins the login lane to
// the same rule.
func TestLaunchHarnessLoginRun_CreateRunFailureRevokesIdentity(t *testing.T) {
	spy := &revokeSpy{Provider: mustIDP(t)}
	s := &Server{cfg: Config{Runner: &probeFakeRunner{}, Identity: spy, Store: createFailStore{}, Now: time.Now}}
	hl, ok := agentHarnessLogin(awsSSOAgent)
	if !ok {
		t.Fatal("aws-sso harness login convention missing")
	}
	if _, err := s.launchHarnessLoginRun(context.Background(), "operator", hl, ""); err == nil {
		t.Fatal("expected CreateRun failure to surface")
	}
	if len(spy.revoked) != 1 {
		t.Fatalf("minted run identity not revoked after CreateRun failure: revoked=%v", spy.revoked)
	}
}
