// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// B5, control-plane half. Quieting the sidecar's renew loop must not hide the
// case underneath it: a HEALTHY run whose renews were refused through a
// control-plane outage (a wardynd rollout longer than the renewer's half-life)
// holds a dead identity for the rest of its life, and every /internal/* call it
// makes 401s. Before this that fact existed in the trail only as an
// undifferentiated pile of auth.fail rows. It is one row now, keyed to the run,
// emitted ONCE — because a row per request is the flood B5 exists to stop.

// expiredIdentity is an identity.Provider that answers Verify with the typed
// expiry refusal for ONE token and a flat error for anything else — the two
// shapes internal/api has to tell apart. The provider side (that a real expired
// token verifies this way, and that no OTHER failure does) is pinned where it
// lives, internal/identity/embedded's TestB5_ExpiredVerifyCarriesTheRunID.
type expiredIdentity struct {
	token string
	runID uuid.UUID
}

const expiredIdentityToken = "expired-run-token"

func (e *expiredIdentity) Name() string { return "expired-test" }

func (e *expiredIdentity) MintRunIdentity(context.Context, uuid.UUID, string, string, string) (identity.RunIdentity, error) {
	return identity.RunIdentity{}, errors.New("not used")
}

func (e *expiredIdentity) Verify(_ context.Context, token, _ string) (*identity.Claims, error) {
	if token != e.token {
		// Forged / unknown: names no run of ours, stays flat.
		return nil, errors.New("embedded identity: verify signature: bad signature")
	}
	return nil, &identity.ExpiredTokenError{RunID: e.runID, Err: errors.New("validate claims: go-jose/go-jose/jwt: validation failed, token is expired (exp)")}
}

func (e *expiredIdentity) RevokeRun(context.Context, uuid.UUID) error { return nil }

var _ identity.Provider = (*expiredIdentity)(nil)

// countingRunStore counts GetRun calls, which is the cost the once-guard's
// ORDERING is about: the internal lane has no rate limiter, so a replayed dead
// token can arrive as often as it likes and must not buy a store read each time.
type countingRunStore struct {
	*dispatchTestStore
	getRuns atomic.Int32
}

func (c *countingRunStore) GetRun(ctx context.Context, id uuid.UUID) (types.AgentRun, error) {
	c.getRuns.Add(1)
	return c.dispatchTestStore.GetRun(ctx, id)
}

// expiredIdentityServer wires a server whose identity provider reports the run's
// token as EXPIRED.
func expiredIdentityServer(t *testing.T, st store.Store, audit *syncAudit, runID uuid.UUID) (*Server, string) {
	t.Helper()
	srv := New(Config{
		Identity:        &expiredIdentity{token: expiredIdentityToken, runID: runID},
		Store:           st,
		Audit:           audit,
		AdminToken:      adminToken,
		TrustDomain:     "wardyn.local",
		ControlPlaneURL: "http://wardynd:8080",
	})
	return srv, expiredIdentityToken
}

// TestRunIdentityExpired_EmittedOncePerRunningRun: a non-terminal run presenting
// an expired identity gets ONE run.identity.expire row however many calls it
// makes, and is left running (mid-flight work is the owner's; the remedy is kill
// + start a new run).
func TestRunIdentityExpired_EmittedOncePerRunningRun(t *testing.T) {
	runID := uuid.New()
	st := &dispatchTestStore{run: types.AgentRun{ID: runID, CreatedBy: "t@example.com"}, state: types.RunRunning}
	counting := &countingRunStore{dispatchTestStore: st}
	audit := &syncAudit{}
	srv, tok := expiredIdentityServer(t, counting, audit, runID)

	for range 20 {
		w := do(t, srv, http.MethodPost, "/api/v1/internal/approvals", tok, `{"kind":"egress_domain","requested_scope":{"host":"x"}}`)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("expired token: code = %d, want 401 — expiry must still fail closed", w.Code)
		}
	}

	rows := audit.eventsFor(runID, "run.identity.expire")
	if len(rows) != 1 {
		t.Fatalf("run.identity.expire rows = %d for 20 refused calls, want exactly 1 — a row per request is the "+
			"flood this exists to stop", len(rows))
	}
	// AND the repeats cost nothing. The guard is consulted BEFORE the run read, so
	// a replayed dead token is refused without touching Postgres; with the read
	// first this was one GetRun per refused request, forever, on a lane that has
	// no rate limiter in front of it (routes.go).
	if got := counting.getRuns.Load(); got != 1 {
		t.Errorf("GetRun calls = %d for 20 refused requests, want 1 — the once-guard must short-circuit BEFORE "+
			"the store read, or a token replay is a free DB read amplifier", got)
	}
	if rows[0].Outcome != "failure" || rows[0].ActorType != types.ActorSystem {
		t.Errorf("row outcome/actor_type = %q/%q, want failure/system", rows[0].Outcome, rows[0].ActorType)
	}
	if got := st.State(); got != types.RunRunning {
		t.Errorf("the run was moved to %q; an expired identity does not end a run — the owner decides", got)
	}
}

// TestRunIdentityExpired_AFailedRunReadDoesNotSpendTheRow: claiming before the
// read has one hazard — a transient store failure could consume the run's single
// row and emit nothing. The claim is released on exactly that path, so the next
// refusal still records it.
func TestRunIdentityExpired_AFailedRunReadDoesNotSpendTheRow(t *testing.T) {
	runID := uuid.New()
	st := &dispatchTestStore{run: types.AgentRun{ID: runID, CreatedBy: "t@example.com"}, state: types.RunRunning}
	failing := &failThenCountStore{countingRunStore: countingRunStore{dispatchTestStore: st}, failFirst: 1}
	audit := &syncAudit{}
	srv, tok := expiredIdentityServer(t, failing, audit, runID)

	do(t, srv, http.MethodPost, "/api/v1/internal/approvals", tok, `{"kind":"egress_domain","requested_scope":{"host":"x"}}`)
	if rows := audit.eventsFor(runID, "run.identity.expire"); len(rows) != 0 {
		t.Fatalf("a failed run read still wrote %d rows; it cannot know the run is non-terminal", len(rows))
	}
	do(t, srv, http.MethodPost, "/api/v1/internal/approvals", tok, `{"kind":"egress_domain","requested_scope":{"host":"x"}}`)
	if rows := audit.eventsFor(runID, "run.identity.expire"); len(rows) != 1 {
		t.Errorf("rows after the store recovered = %d, want 1 — a blip must not silently spend the run's one row "+
			"(that is exactly when this evidence matters)", len(rows))
	}
}

// failThenCountStore fails the first N GetRun calls, then behaves.
type failThenCountStore struct {
	countingRunStore
	failFirst int32
}

func (f *failThenCountStore) GetRun(ctx context.Context, id uuid.UUID) (types.AgentRun, error) {
	if f.countingRunStore.getRuns.Add(1) <= f.failFirst {
		return types.AgentRun{}, errStoreNotFound
	}
	return f.countingRunStore.dispatchTestStore.GetRun(ctx, id)
}

// TestRunIdentityExpired_TerminalRunEmitsNothing: a terminal run's token is
// SUPPOSED to be dead, and a sidecar the teardown has not reaped yet is not news
// — that population is exactly where a flood would come from.
func TestRunIdentityExpired_TerminalRunEmitsNothing(t *testing.T) {
	for _, state := range []types.RunState{types.RunKilled, types.RunCompleted, types.RunFailed} {
		runID := uuid.New()
		st := &dispatchTestStore{run: types.AgentRun{ID: runID, CreatedBy: "t@example.com"}, state: state}
		audit := &syncAudit{}
		srv, tok := expiredIdentityServer(t, st, audit, runID)

		do(t, srv, http.MethodPost, "/api/v1/internal/approvals", tok, `{"kind":"egress_domain","requested_scope":{"host":"x"}}`)

		if rows := audit.eventsFor(runID, "run.identity.expire"); len(rows) != 0 {
			t.Errorf("%s run emitted %d run.identity.expire rows; its token is meant to be dead", state, len(rows))
		}
	}
}

// TestRunIdentityExpired_ForgedTokenEmitsNothing: only an expiry names a run. A
// forged or unparseable token names nobody, so it must stay as coarse as the
// auth.fail row beside it — otherwise a prober picks the run id the row is
// written against.
func TestRunIdentityExpired_ForgedTokenEmitsNothing(t *testing.T) {
	runID := uuid.New()
	st := &dispatchTestStore{run: types.AgentRun{ID: runID, CreatedBy: "t@example.com"}, state: types.RunRunning}
	audit := &syncAudit{}
	srv, _ := expiredIdentityServer(t, st, audit, runID)

	do(t, srv, http.MethodPost, "/api/v1/internal/approvals", "not-a-real-token", "")

	for _, ev := range audit.events {
		if ev.Action == "run.identity.expire" {
			t.Fatalf("a forged token produced a run.identity.expire row (%+v); only a token that names one of "+
				"our runs may", ev)
		}
	}
}
