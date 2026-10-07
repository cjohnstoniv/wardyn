// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// Run create while another flight is already renewing the same AWS sign-in:
// create only tries the owner lock, and then watches the store for that
// flight's result, so a renewal that ends invalid_grant refuses the launch
// before any run row exists. Two replicas over one Postgres; needs
// WARDYN_TEST_PG (skipped cleanly without it).

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
)

// createWaitBlob is a session create would serve as it is (more than the
// serve floor left) but inside the renewal skew, so create only tries the lock.
func createWaitBlob() awsSSOBlob {
	b := createRenewalBlob()
	b.ExpiresAt = time.Now().Add(5 * time.Minute)
	return b
}

// createWaitFixture is a create server on replica A's secret store, with a
// two-slot lock pool, and replica B, whose renewal is the flight in progress.
func createWaitFixture(t *testing.T) (*Server, *replicaPair) {
	t.Helper()
	p := newReplicaPair(t)
	srv := createRenewalFixture(t)
	srv.cfg.Secrets = p.a.cfg.Secrets
	srv.locks.override = db.NewPGLocker(p.poolA, 2)
	p.b.cfg.Now = time.Now
	storeSSOBlobFor(t, srv, createRenewalOwner, createWaitBlob())
	return srv, p
}

// heldOIDC is a token endpoint whose first call blocks until release is
// called, then answers with answer; later calls answer at once. entered
// closes when the first call arrives.
func heldOIDC(t *testing.T, answer func(w http.ResponseWriter)) (calls *atomic.Int32, entered chan struct{}, release func()) {
	t.Helper()
	entered, gate := make(chan struct{}), make(chan struct{})
	var once sync.Once
	release = func() { once.Do(func() { close(gate) }) }
	t.Cleanup(release)
	calls = fakeOIDC(t, func(w http.ResponseWriter, _ map[string]string, call int) {
		if call == 1 {
			close(entered)
			<-gate
		}
		answer(w)
	})
	return calls, entered, release
}

func invalidGrant(w http.ResponseWriter) {
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": "invalid_grant"})
}

func renewedPair(w http.ResponseWriter) {
	_ = json.NewEncoder(w).Encode(map[string]any{
		"accessToken": "fresh-access-token-abcdefghij", "expiresIn": 3600,
		"refreshToken": "rotated-refresh-token-abcdefghij",
	})
}

// startHolder runs replica B's renewal of the stored session, the flight
// create finds in progress, and returns once B is inside CreateToken.
func startHolder(t *testing.T, p *replicaPair, entered chan struct{}) (done chan struct{}) {
	t.Helper()
	done = make(chan struct{})
	go func() {
		defer close(done)
		ctx := secretstore.WithPurpose(context.Background(), secretstore.PurposeSSORefresh)
		p.b.refreshAWSSSOBlob(ctx, createRenewalScope(), createWaitBlob())
	}()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("replica B never reached CreateToken")
	}
	return done
}

// A launch that arrives while another flight's renewal ends invalid_grant
// inside the budget is refused with the sign-in door, and no run row exists.
func TestPG_CreateWait_HeldRenewalEndingInvalidGrantRefusesWithNoRow(t *testing.T) {
	srv, p := createWaitFixture(t)
	_, entered, release := heldOIDC(t, invalidGrant)
	holder := startHolder(t, p, entered)

	res := make(chan *httpResult, 1)
	go func() { res <- postCreate(t, srv) }()
	time.Sleep(500 * time.Millisecond) // create is in flight beside B's renewal
	release()
	got := <-res
	<-holder

	var body errorBody
	_ = json.Unmarshal([]byte(got.body), &body)
	if got.code != http.StatusUnprocessableEntity || body.Reason != llmRefusalAuditReason || body.Provider != "bedrock-sso" {
		t.Errorf("create = %d %s, want the 422 sign-in door: the launch was not refused although the renewal in flight ended invalid_grant", got.code, got.body)
	}
	if n := runRowCount(srv); n != 0 {
		t.Errorf("run rows = %d, want none: a run row exists for a sign-in that cannot be renewed", n)
	}
}

// Ten creates for one person at once, on a lock pool of two: one renews, the
// rest wait on the store rather than on the lock, so none is refused for want
// of a lock connection, and a lock of another class is still taken meanwhile.
func TestPG_CreateWait_TenConcurrentCreatesOnATwoSlotLocker(t *testing.T) {
	srv, _ := createWaitFixture(t)
	calls := fakeOIDC(t, func(w http.ResponseWriter, _ map[string]string, _ int) {
		time.Sleep(500 * time.Millisecond)
		renewedPair(w)
	})

	var wg sync.WaitGroup
	codes := make([]int, 10)
	bodies := make([]string, 10)
	bearer := providerAdminToken(srv, createRenewalOwner) // minted once: it writes the store
	for i := range codes {
		wg.Go(func() {
			w := do(t, srv, http.MethodPost, "/api/v1/runs", bearer, createRenewalBody)
			codes[i], bodies[i] = w.Code, w.Body.String()
		})
	}
	time.Sleep(200 * time.Millisecond)
	_, unlock, err := srv.lock(context.Background(), db.RunOpLockClass, uuid.NewString())
	if err != nil {
		t.Errorf("a run-op lock taken beside the contended creates = %v, want it taken", err)
	} else {
		unlock()
	}
	wg.Wait()

	for i, c := range codes {
		if c != http.StatusCreated {
			t.Errorf("create %d = %d %s, want 201", i, c, bodies[i])
		}
	}
	if calls.Load() != 1 {
		t.Errorf("CreateToken calls = %d, want 1: one flight renews for all", calls.Load())
	}
	if n := runRowCount(srv); n != 10 {
		t.Errorf("run rows = %d, want 10", n)
	}
}

// A create that waits reads the stored pair once, when it changed: never a
// decrypting, audited read per tick.
func TestPG_CreateWait_ContendedCreateReadsThePairOnce(t *testing.T) {
	srv, p := createWaitFixture(t)
	rec := &memAudit{}
	srv.cfg.Secrets = secretstore.Audited(p.a.cfg.Secrets, rec)
	_, entered, release := heldOIDC(t, renewedPair)
	holder := startHolder(t, p, entered)

	res := make(chan *httpResult, 1)
	go func() { res <- postCreate(t, srv) }()
	time.Sleep(2 * time.Second) // eight ticks of waiting
	release()
	got := <-res
	<-holder
	if got.code != http.StatusCreated {
		t.Fatalf("create = %d %s, want 201 on the renewed pair", got.code, got.body)
	}
	reads := 0
	rec.mu.Lock()
	defer rec.mu.Unlock()
	for _, ev := range rec.rows {
		if ev.Action == "secret.read" && ev.Target == createRenewalScope().ssoSecret() {
			reads++
		}
	}
	if reads > 2 {
		t.Errorf("secret.read rows for the sign-in = %d, want at most 2 (the door's read, and one after the pair changed)", reads)
	}
}
