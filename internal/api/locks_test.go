// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The six locks that used to be in-process mutexes refuse when they cannot be
// taken; none proceeds unlocked. refusingLocker is a lock pool with no
// connection to spare, so every lock is a refusal and anything that still ran
// ran unlocked.

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// refusingLocker refuses every lock the way an exhausted pool does.
type refusingLocker struct{ err error }

func (r refusingLocker) Lock(ctx context.Context, _ db.LockKey, _ time.Duration) (context.Context, func(), error) {
	return ctx, nil, r.err
}

func (r refusingLocker) TryLock(ctx context.Context, _ db.LockKey) (context.Context, func(), bool, error) {
	return ctx, nil, false, r.err
}

func exhaustedPool() refusingLocker { return refusingLocker{err: db.ErrLockNoCapacity} }

func requireLockUnavailable(t *testing.T, code int, body string) {
	t.Helper()
	if code != http.StatusServiceUnavailable || !strings.Contains(body, reasonLockUnavailable) {
		t.Fatalf("answered %d %s, want 503 %s", code, body, reasonLockUnavailable)
	}
}

// A revive of one run takes the run's operation lock; with the pool exhausted it
// refuses, and nothing is replaced, claimed or stopped.
func TestReviveRun_ExhaustedLockPoolRefusesInsteadOfProceedingUnlocked(t *testing.T) {
	f := newReviveFixture(t)
	f.srv.locks.override = exhaustedPool()
	w := do(t, f.srv, http.MethodPost, "/api/v1/runs/"+f.run.ID.String()+"/revive", adminToken, "")
	requireLockUnavailable(t, w.Code, w.Body.String())
	if n := len(f.rr.replaced); n != 0 {
		t.Errorf("ReplaceProxy ran %d time(s) on a revive that could not take the run's lock", n)
	}
	if lostAt, _ := f.st.lost(); lostAt == nil {
		t.Error("a refused revive cleared the run's lost mark")
	}
}

// The sweep side keeps tryLockRunOp's semantics: a run whose lock cannot be
// taken, for any reason, is skipped and retried next pass.
func TestLeaseRun_ExhaustedLockPoolSkipsTheRun(t *testing.T) {
	f := newReviveFixture(t)
	stops, revokes := f.lr.proxyStopCount(), f.brk.count(f.run.ID)
	f.srv.locks.override = exhaustedPool()
	f.srv.leaseRun(context.Background(), f.rs, f.run)
	if f.lr.proxyStopCount() != stops || f.brk.count(f.run.ID) != revokes {
		t.Error("a lease pass acted on a run whose lock it could not take")
	}
}

// An expired AWS SSO session cannot be renewed without its lock: the refresh
// token is NOT redeemed unlocked, and the caller is told to retry.
func TestAWSRefresh_ExhaustedLockPoolRedeemsNothing(t *testing.T) {
	for _, c := range []struct {
		name      string
		expiresIn time.Duration
		wantMsg   string
	}{
		{"expired token", -time.Minute, awsSSORefreshUnavailableSentence},
		{"token still servable", 8 * time.Minute, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, blob, calls := refreshFixture(t, awsSSOTestFixedNow.Add(c.expiresIn))
			s.locks.override = exhaustedPool()
			got, msg := s.refreshAWSSSOBlob(context.Background(), awsSSOTestScope(), blob)
			if msg != c.wantMsg {
				t.Errorf("sentence = %q, want %q", msg, c.wantMsg)
			}
			if got.AccessToken != blob.AccessToken {
				t.Errorf("served a different token %q than the one in hand", got.AccessToken)
			}
			if n := calls.Load(); n != 0 {
				t.Errorf("%d CreateToken calls without the lock, want none: the refresh token was spent unlocked", n)
			}
			if after := storedSSOBlob(t, s); after != blob {
				t.Errorf("the store changed without the lock: %+v", after)
			}
		})
	}
}

// The AWS SSO capture takes the credential's lock (the once-only guard is a
// read-then-put over the same row a renewal rewrites): with it unavailable the
// capture is refused 503 and stores nothing.
func TestUploadSSOToken_ExhaustedLockPoolStoresNothing(t *testing.T) {
	h := newHarness(t)
	runID := uuid.New()
	st := ssoLoginRunStore{
		run: types.AgentRun{
			ID: runID, Task: harnessLoginTask, Agent: awsSSOAgent, State: types.RunRunning,
			CreatedBy: "member@corp.example", UpdatedAt: time.Now().UTC(),
		},
		events: ssoLoginStartedEvents(runID, "https://my-sso.awsapps.com/start"),
	}
	sec := &memSecrets{m: map[string][]byte{}}
	cfg := baseTestConfig(h, st)
	cfg.Secrets = sec
	srv := New(cfg)
	srv.locks.override = exhaustedPool()
	tok := h.mintRunToken(t, runID)

	w := do(t, srv, http.MethodPut, "/api/v1/internal/sso-token/"+runID.String(), tok, validSSOBody)
	requireLockUnavailable(t, w.Code, w.Body.String())
	if _, stored := uploadedBlob(sec, uploadOwner); stored {
		t.Error("the capture was stored without the credential's lock")
	}
}

// The document writers answer 503 and write nothing.
func TestDocumentWritersRefuseWhenTheirLockCannotBeTaken(t *testing.T) {
	t.Run("site config", func(t *testing.T) {
		fake := &fakeSiteConfigStore{}
		srv, _ := newSiteConfigHarness(t, fake)
		srv.locks.override = exhaustedPool()
		w := do(t, srv, http.MethodPost, "/api/v1/setup/onboarding-complete", adminToken, "")
		requireLockUnavailable(t, w.Code, w.Body.String())
		if fake.putSeen != nil {
			t.Errorf("wrote the site config without its lock: %+v", fake.putSeen)
		}
	})
	t.Run("capability enforcement", func(t *testing.T) {
		srv, st := permServer(t)
		srv.locks.override = exhaustedPool()
		w := doSSO(t, srv, http.MethodPut, "/api/v1/permissions/enforcement", permAdmin(t), `{"egress_host":true}`)
		requireLockUnavailable(t, w.Code, w.Body.String())
		if st.enf[capEgressHost] {
			t.Error("wrote the capability enforcement without its lock")
		}
	})
	t.Run("audit chain verify", func(t *testing.T) {
		srv := chainServer(t, &chainStore{})
		srv.locks.override = exhaustedPool()
		w := doSSO(t, srv, http.MethodGet, "/api/v1/audit/chain/verify", permAdmin(t), "")
		requireLockUnavailable(t, w.Code, w.Body.String())
	})
}
