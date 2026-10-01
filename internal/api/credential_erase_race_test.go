// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// eraseDoor is a second server over the SAME secret store, wired to erase the
// named principal (the door an admin reaches), so a test can run an erase while
// the first server's refresh or stamp is mid-flight.
func eraseDoor(t *testing.T, sec secretstore.Store, principal string) *Server {
	t.Helper()
	h := newHarness(t)
	h.srv.cfg.OIDC = &oidc.Authenticator{}
	h.srv.cfg.Secrets = sec
	h.srv.cfg.Store = secretOwnerDirectory{toks: []types.APIToken{{ID: uuid.New(), Principal: principal, Email: "member@corp.example"}}}
	h.srv.router = h.srv.routes()
	return h.srv
}

// eraseAsAdmin runs the erase in the background and closes the channel when it
// returns, so a test can let it queue behind a lock before it releases the
// writer that holds it.
func eraseAsAdmin(t *testing.T, srv *Server, principal string) <-chan struct{} {
	t.Helper()
	admin := ssoSession(t, "security-review", "security@corp.example", oidc.RoleSecurityAdmin)
	done := make(chan struct{})
	go func() {
		defer close(done)
		w := doSSO(t, srv, http.MethodDelete, "/api/v1/people/"+principal+"/credentials", admin, "")
		if w.Code != http.StatusOK {
			t.Errorf("admin erase = %d %s", w.Code, w.Body)
		}
	}()
	return done
}

// A successful admin erase is final (#1478): the refusal stamp reads the stored
// token, the erase runs, and the stamp then writes. Without the erase taking the
// stamp's lock, the stamp wrote the credential back after the erase reported
// success. The erase now queues behind the stamp and removes what it wrote.
func TestReviewCredentials_AdminEraseMustSurviveOwnPATRefusalStamp(t *testing.T) {
	f := newOwnPATRun(t)
	inner := f.srv.cfg.Secrets
	var hook func(string)
	f.srv.cfg.Secrets = hookedSecrets{Store: inner, afterGet: &hook}
	f.token(t, ownPATToken, "contoso", f.now.Add(30*24*time.Hour))
	door := eraseDoor(t, inner, capSub)
	ownPATConfirm(t, http.StatusUnauthorized)
	gets := 0
	var erased <-chan struct{}
	hook = func(string) {
		if gets++; gets != 2 { // the resolve's own read is #1; the stamp's, under its lock, is #2
			return
		}
		erased = eraseAsAdmin(t, door, capSub)
		select {
		case <-erased:
		case <-time.After(200 * time.Millisecond): // queued behind the stamp, as it must be
		}
	}
	if w := f.resolve(t, capSub, "dev.azure.com", "?stale_jti=synthetic-old-jti"); w.Code != http.StatusOK {
		t.Fatalf("resolve = %d", w.Code)
	}
	hook = nil
	if erased == nil {
		t.Fatal("fixture never entered the stamp read/write interval")
	}
	<-erased
	if names := namesOf(t, inner, capSub); len(names) != 0 {
		t.Fatalf("an admin erase that reported success left %v behind", names)
	}
	if _, found, err := f.srv.readADOOwnPAT(secretstore.WithPurpose(context.Background(), secretstore.PurposeStatus), capSub, ownPATRowID); err != nil || found {
		t.Fatalf("credential resurrected after successful admin erase: found=%v err=%v", found, err)
	}
}

// The same promise against an AWS SSO refresh in flight: the erase waits for
// the owner's refresh lock, so the rotated pair the refresh persists is erased
// with the rest rather than written back behind it.
func TestReviewCredentials_AdminEraseMustSurviveAWSRefresh(t *testing.T) {
	h := newHarness(t)
	s := h.srv
	s.cfg.OIDC = &oidc.Authenticator{}
	s.cfg.Secrets = &memSecrets{m: map[string][]byte{}}
	s.cfg.Now = func() time.Time { return awsSSOTestFixedNow }
	s.cfg.Store = secretOwnerDirectory{toks: []types.APIToken{{ID: uuid.New(), Principal: awsSSOTestOwner, Email: "member@corp.example"}}}
	s.router = s.routes()
	putAWSSSOBlob(t, s, awsSSOTestFixedNow.Add(-time.Minute))
	started, resume, done := make(chan struct{}), make(chan struct{}), make(chan string, 1)
	fakeOIDC(t, func(w http.ResponseWriter, _ map[string]string, _ int) {
		close(started)
		select {
		case <-resume:
		case <-time.After(5 * time.Second):
			t.Error("refresh was not resumed")
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"accessToken": "review-synthetic-fresh-access", "expiresIn": 3600, "refreshToken": "review-synthetic-rotated-refresh"})
	})
	go func() { _, failure := ssoDispatch(s); done <- failure }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("refresh did not start")
	}
	erased := eraseAsAdmin(t, s, awsSSOTestOwner)
	select {
	case <-erased:
	case <-time.After(200 * time.Millisecond): // queued behind the refresh, as it must be
	}
	close(resume)
	select {
	case failure := <-done:
		if failure != "" {
			t.Fatalf("refresh failed: %s", failure)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("refresh did not complete")
	}
	select {
	case <-erased:
	case <-time.After(5 * time.Second):
		t.Fatal("erase did not complete")
	}
	if names := namesOf(t, s.cfg.Secrets, awsSSOTestOwner); len(names) != 0 {
		t.Fatalf("an admin erase that reported success left %v behind", names)
	}
	if blob, found, err := s.readAWSSSOBlob(context.Background(), awsSSOTestScope()); err != nil || found {
		t.Fatalf("credential resurrected after successful admin erase: found=%v err=%v rotated=%v", found, err, blob.RefreshToken == "review-synthetic-rotated-refresh")
	}
}

// An Azure DevOps sign-in configuration that cannot be read refuses the erase:
// it could not take the sign-in's redemption lock, so it must not proceed.
// Nothing is erased and the failure is audited, never a success.
func TestErasePersonCredentials_SignInConfigUnreadableRefuses(t *testing.T) {
	sec := &memSecrets{m: map[string][]byte{}}
	h, srv := eraseFixture(t, sec)
	srv.cfg.ADOEntra = func(context.Context) (ADOEntraConfig, bool, error) {
		return ADOEntraConfig{}, false, errors.New("site config unreadable")
	}
	admin := ssoSession(t, "admin-1", "admin@corp.example", oidc.RoleAdmin)
	w := doSSO(t, srv, http.MethodDelete, "/api/v1/people/bob/credentials", admin, "")
	if w.Code != http.StatusServiceUnavailable || errorReason(w) != "credential_erase_signin_config_unreadable" {
		t.Fatalf("erase = %d %s, want 503 credential_erase_signin_config_unreadable", w.Code, w.Body)
	}
	if got := namesOf(t, sec, "bob"); len(got) != 2 {
		t.Errorf("bob holds %v, want both credentials untouched", got)
	}
	ev := lastAuditEvent(t, h.audit.events, "credential.erase")
	if ev.Outcome != "failure" {
		t.Errorf("credential.erase row = %+v, want a failure row", ev)
	}
	for _, e := range h.audit.events {
		if e.Action == "credential.erase" && e.Outcome == "success" {
			t.Fatalf("a refused erase audited success: %s", e.Data)
		}
	}
}

// refreshFixture is a server holding the test owner's expired AWS SSO blob and
// a counter of the CreateToken calls the fake authority gets.
func refreshFixture(t *testing.T, expiresAt time.Time) (*Server, awsSSOBlob, *atomic.Int32) {
	t.Helper()
	s, _, _ := ssoRefreshServer(t)
	blob := putAWSSSOBlob(t, s, expiresAt)
	calls := fakeOIDC(t, func(w http.ResponseWriter, _ map[string]string, _ int) {
		_ = json.NewEncoder(w).Encode(map[string]any{"accessToken": "fresh-access-token-abcdefghij", "expiresIn": 3600, "refreshToken": "rotated-refresh-token-abcdefghij"})
	})
	return s, blob, calls
}

// secretsFailingReads answers every read with an error once armed, over a real
// store.
type secretsFailingReads struct {
	secretstore.Store
	armed *atomic.Bool
}

func (f secretsFailingReads) For(owner string) secretstore.Store {
	return secretsFailingReads{Store: f.Store.For(owner), armed: f.armed}
}
func (f secretsFailingReads) List(ctx context.Context) ([]string, error) {
	if f.armed.Load() {
		return nil, errors.New("store unreadable")
	}
	return f.Store.List(ctx)
}
func (f secretsFailingReads) Get(ctx context.Context, name string) ([]byte, error) {
	if f.armed.Load() {
		return nil, errors.New("store unreadable")
	}
	return f.Store.Get(ctx, name)
}

// A renewal whose re-read under the lock fails never reaches the store: it
// spends nothing at the authority and writes nothing, because the copy in hand
// may predate an erase. A token still valid is served from memory; an expired
// one is refused.
func TestAWSRefresh_FailedReReadPersistsNothing(t *testing.T) {
	for _, c := range []struct {
		name      string
		expiresIn time.Duration
		wantMsg   string
	}{
		{"still valid: served from memory", 5 * time.Minute, ""},
		{"expired: refused", -time.Minute, awsSSORefreshUnavailableSentence},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, blob, calls := refreshFixture(t, awsSSOTestFixedNow.Add(c.expiresIn))
			before := storedSSOBlob(t, s)
			var armed atomic.Bool
			armed.Store(true)
			s.cfg.Secrets = secretsFailingReads{Store: s.cfg.Secrets, armed: &armed}
			got, msg := s.refreshAWSSSOBlob(context.Background(), awsSSOTestScope(), blob)
			if msg != c.wantMsg {
				t.Errorf("refusal = %q, want %q", msg, c.wantMsg)
			}
			if c.wantMsg == "" && got.AccessToken != blob.AccessToken {
				t.Errorf("served %q, want the blob in hand", got.AccessToken)
			}
			if n := calls.Load(); n != 0 {
				t.Errorf("%d CreateToken calls, want none: a renewal that cannot persist must not spend the refresh token", n)
			}
			armed.Store(false)
			if after := storedSSOBlob(t, s); after != before {
				t.Errorf("the store changed: %+v -> %+v", before, after)
			}
		})
	}
}

// A refresh that arrives while the owner's lock is held (the erase holds it)
// and still has a servable token serves it from memory and writes nothing.
func TestAWSRefresh_TryLockDuringEraseServesInMemory(t *testing.T) {
	s, blob, calls := refreshFixture(t, awsSSOTestFixedNow.Add(8*time.Minute)) // inside the skew, above the serve floor
	unlock := s.lockAWSSSOOwner(awsSSOTestOwner)
	got, msg := s.refreshAWSSSOBlob(context.Background(), awsSSOTestScope(), blob)
	unlock()
	if msg != "" || got.AccessToken != blob.AccessToken {
		t.Fatalf("refresh under a held lock = %+v %q, want the token in hand", got, msg)
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("%d CreateToken calls under a held lock, want none", n)
	}
	if after := storedSSOBlob(t, s); after != blob {
		t.Errorf("the store changed under a held lock: %+v", after)
	}
}

// After an erase the person reconnects: the locks are free, a new own token is
// stored by the paste and a new AWS credential by the capture, and neither is
// touched by anything the erase left running.
func TestEraseThenReconnectKeepsTheCredential(t *testing.T) {
	d := newOwnPATDoor(t, adoSite(ownPATTestRow()), nil)
	d.putBlob(t, capSub, ownPATRowID, ownPATToken)
	var rep secretstore.EraseReport
	if err := d.srv.eraseLocked(context.Background(), capSub, "", &rep); err != nil || rep.Count != 1 {
		t.Fatalf("erase = %+v, %v", rep, err)
	}
	if code, body := d.put(t, ownPATOrgKey, ownPATToken, days(10)); code != http.StatusOK {
		t.Fatalf("paste after the erase = %d %s", code, body)
	}
	if _, found := d.stored(t); !found {
		t.Fatal("the pasted token is not stored")
	}
	s, _, blob := ssoRefreshServer(t)
	var awsRep secretstore.EraseReport
	if err := s.eraseLocked(context.Background(), awsSSOTestOwner, "", &awsRep); err != nil || awsRep.Count != 1 {
		t.Fatalf("aws erase = %+v, %v", awsRep, err)
	}
	if err := s.storeAWSSSOBlob(context.Background(), awsSSOTestScope(), blob); err != nil {
		t.Fatal(err)
	}
	if got := storedSSOBlob(t, s); got != blob {
		t.Errorf("the recaptured credential reads back %+v", got)
	}
}

// secretsRacingWrite writes one more name into the namespace as its last
// delete lands: a writer outside every lock, which the re-list must catch.
type secretsRacingWrite struct {
	secretstore.Store
	owner string
}

func (r secretsRacingWrite) For(owner string) secretstore.Store {
	return secretsRacingWrite{Store: r.Store.For(owner), owner: owner}
}
func (r secretsRacingWrite) Delete(ctx context.Context, name string) error {
	if err := r.Store.Delete(ctx, name); err != nil {
		return err
	}
	return r.Store.Put(ctx, "late-writer", []byte("written-while-the-erase-ran"))
}

// A residue the re-list finds is a failure row and a 500, never a success.
func TestErasePersonCredentials_ResidueOnReListIsNotSuccess(t *testing.T) {
	sec := secretsRacingWrite{Store: &memSecrets{m: map[string][]byte{}, owned: map[string]map[string][]byte{}}}
	h, srv := eraseFixture(t, sec)
	admin := ssoSession(t, "admin-1", "admin@corp.example", oidc.RoleAdmin)
	if w := doSSO(t, srv, http.MethodDelete, "/api/v1/people/bob/credentials", admin, ""); w.Code != http.StatusInternalServerError {
		t.Fatalf("erase with a late writer = %d %s, want 500", w.Code, w.Body)
	}
	for _, e := range h.audit.events {
		if e.Action == "credential.erase" && e.Outcome == "success" {
			t.Fatalf("a residue audited success: %s", e.Data)
		}
	}
	if ev := lastAuditEvent(t, h.audit.events, "credential.erase"); ev.Outcome != "failure" {
		t.Errorf("credential.erase row = %+v, want a failure row", ev)
	}
}

// Erase, AWS renewal, refusal stamp, own-token paste and an Entra redemption's
// store, all on one person at once, under -race: nothing deadlocks, and the
// erase's success leaves nothing behind that was written before it returned.
func TestEraseRace_FourWayNoDeadlock(t *testing.T) {
	s, _, blob := ssoRefreshServer(t)
	const rowID = ownPATRowID
	fakeOIDC(t, func(w http.ResponseWriter, _ map[string]string, _ int) {
		_ = json.NewEncoder(w).Encode(map[string]any{"accessToken": "fresh-access-token-abcdefghij", "expiresIn": 3600, "refreshToken": "rotated-refresh-token-abcdefghij"})
	})
	refuse := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) }))
	defer refuse.Close()
	owner := awsSSOTestOwner
	putOwn := func(token string) {
		raw, _ := json.Marshal(adoOwnPATBlob{Token: token, Org: "contoso", ExpiresOn: awsSSOTestFixedNow.AddDate(0, 0, 10), StoredAt: awsSSOTestFixedNow})
		adoOwnPATWriteMu.Lock()
		defer adoOwnPATWriteMu.Unlock()
		_ = s.cfg.Secrets.For(owner).Put(context.Background(), adoOwnPATSecretName(rowID), raw)
	}
	putOwn("stamp-target-token")
	claims := &identity.Claims{RunID: uuid.New(), SPIFFEID: "spiffe://wardyn.local/run"}
	sn := adoEntraScopeSnapshot{OwnerSubject: owner, ProviderRowID: rowID, Organisation: "contoso"}

	var wg sync.WaitGroup
	run := func(f func()) { wg.Add(1); go func() { defer wg.Done(); f() }() }
	for range 8 {
		run(func() { _, _ = s.refreshAWSSSOBlob(context.Background(), awsSSOTestScope(), blob) })
		run(func() {
			s.stampADOOwnPATRefused(context.Background(), claims, sn, adoOwnPATBlob{Token: "stamp-target-token", Org: "contoso",
				ExpiresOn: awsSSOTestFixedNow.AddDate(0, 0, 10)}, refuse.URL+"/x")
		})
		run(func() { putOwn("pasted-token") })
		run(func() { // an Entra redemption storing its rotated refresh token
			unlock := s.adoEntra.lock(owner, "entra-row")
			defer unlock()
			_ = s.cfg.Secrets.For(owner).Put(context.Background(), adoEntraSecretName("entra-row"), []byte("rotated"))
		})
		run(func() {
			var rep secretstore.EraseReport
			_ = s.eraseLocked(context.Background(), owner, "entra-row", &rep)
		})
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("erase, renewal, stamp, paste and redemption deadlocked")
	}
	// One more erase with nothing else running: it must leave the namespace empty.
	var rep secretstore.EraseReport
	if err := s.eraseLocked(context.Background(), owner, "entra-row", &rep); err != nil {
		t.Fatal(err)
	}
	if names := namesOf(t, s.cfg.Secrets, owner); len(names) != 0 {
		t.Fatalf("an erase with no concurrent writer left %v", names)
	}
}
