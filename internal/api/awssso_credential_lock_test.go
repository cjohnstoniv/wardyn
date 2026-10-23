// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// otherCredentialOf is a second AWS SSO credential of owner's: the AWS CLI
// session, beside the bedrock_sso provider's.
func otherCredentialOf(owner string) awsSSOScope {
	return awsSSOScope{perUser: true, owner: owner, cli: "cli-row-other"}
}

// #2137: a renewal of one of a person's credentials does not make a create
// renewing another of theirs wait: the create renews its own at once.
func TestAWSSSOLock_TwoCredentialsRenewWithoutWaiting(t *testing.T) {
	shortCreateWait(t, 3*time.Second)
	srv := createRenewalFixture(t)
	srv.cfg.Secrets = revisionedSecrets{srv.cfg.Secrets}
	storeSSOBlobFor(t, srv, createRenewalOwner, createWaitBlob())
	calls := renewedOIDC(t)
	_, unlock, err := srv.lockAWSSSOCredential(context.Background(), otherCredentialOf(createRenewalOwner))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(unlock)

	start := time.Now()
	w := do(t, srv, http.MethodPost, "/api/v1/runs", providerAdminToken(srv, createRenewalOwner), createRenewalBody)
	took := time.Since(start)
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d %s, want 201", w.Code, w.Body.String())
	}
	if took > time.Second {
		t.Errorf("create took %v while another credential was renewing, want no wait", took)
	}
	if got, _ := createRenewalStored(t, srv); calls.Load() != 1 || got.AccessToken != "fresh-access-token-abcdefghij" {
		t.Errorf("CreateToken calls = %d, stored token %q, want this credential renewed", calls.Load(), got.AccessToken)
	}
}

// A capture of a credential holds that credential's lock: a renewal of the
// same credential does not run beside it, and serves the token in hand.
func TestAWSSSOLock_CaptureAndRenewOfOneCredentialSerialise(t *testing.T) {
	s, blob, calls := refreshFixture(t, awsSSOTestFixedNow.Add(8*time.Minute)) // inside the skew, above the serve floor
	_, unlock, err := s.lockAWSSSOCapture(context.Background(), awsSSOTestScope())
	if err != nil {
		t.Fatal(err)
	}
	got, msg := s.refreshAWSSSOBlob(context.Background(), awsSSOTestScope(), blob)
	unlock()
	if msg != "" || got.AccessToken != blob.AccessToken || calls.Load() != 0 {
		t.Fatalf("renewal during a capture of the same credential = %q %q with %d CreateToken calls, want the token in hand and none",
			got.AccessToken, msg, calls.Load())
	}
}

// The whole-person erase excludes a renewal of each of the person's
// credentials: a renewal that must wait (its token expired) starts only after
// the erase releases.
func TestAWSSSOLock_WholePersonEraseBlocksARenewal(t *testing.T) {
	s, blob, calls := refreshFixture(t, awsSSOTestFixedNow.Add(-time.Minute)) // expired: the renewal waits for the lock
	_, unlock, err := s.lockAWSSSOEveryCredential(context.Background(), awsSSOTestOwner)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.refreshAWSSSOBlob(context.Background(), awsSSOTestScope(), blob)
	}()
	select {
	case <-done:
		unlock()
		t.Fatalf("a renewal ran while the whole-person erase held its locks (%d CreateToken calls)", calls.Load())
	case <-time.After(200 * time.Millisecond):
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("%d CreateToken calls during the erase, want none", n)
	}
	unlock()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the renewal did not run after the erase released")
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("%d CreateToken calls after the erase, want 1", n)
	}
}

// Captures, renewals of two credentials and whole-person erases of one person,
// all at once, under the daemon's own lock helpers: every lock is taken (no
// order refusal, no deadlock), one credential is never in two critical sections
// at once, and nothing runs inside an erase. Run under -race with -count.
func TestAWSSSOLock_ConcurrentCaptureEraseRenewKeepTheOrder(t *testing.T) {
	s, _, _ := ssoRefreshServer(t)
	a, b := awsSSOTestScope(), otherCredentialOf(awsSSOTestOwner)
	if err := s.storeAWSSSOBlob(context.Background(), b, putAWSSSOBlob(t, s, awsSSOTestFixedNow.Add(time.Hour))); err != nil {
		t.Fatal(err)
	}
	var inA, inB, inErase atomic.Int32
	section := func(own *atomic.Int32, what string) {
		if inErase.Load() != 0 {
			t.Errorf("%s ran inside a whole-person erase", what)
		}
		if own != nil && own.Add(1) != 1 {
			t.Errorf("%s overlapped another holder of the same credential", what)
		}
		time.Sleep(time.Millisecond)
		if own != nil {
			own.Add(-1)
		}
	}
	held := func(what string, lock func() (func(), error), body func()) {
		unlock, err := lock()
		if err != nil {
			t.Errorf("%s: %v", what, err)
			return
		}
		defer unlock()
		body()
	}
	ctx := context.Background()
	strip := func(_ context.Context, u func(), err error) (func(), error) { return u, err }
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			held("renew a", func() (func(), error) { return strip(s.lockAWSSSOCredential(ctx, a)) }, func() { section(&inA, "renew a") })
		})
		wg.Go(func() {
			held("renew b", func() (func(), error) { return strip(s.lockAWSSSOCredential(ctx, b)) }, func() { section(&inB, "renew b") })
		})
		wg.Go(func() {
			held("capture a", func() (func(), error) { return strip(s.lockAWSSSOCapture(ctx, a)) }, func() { section(&inA, "capture a") })
		})
		wg.Go(func() {
			held("erase", func() (func(), error) { return strip(s.lockAWSSSOEveryCredential(ctx, awsSSOTestOwner)) }, func() {
				if inA.Load() != 0 || inB.Load() != 0 {
					t.Error("a whole-person erase began while a credential was held")
				}
				inErase.Add(1)
				time.Sleep(time.Millisecond)
				inErase.Add(-1)
			})
		})
	}
	wg.Wait()
}
