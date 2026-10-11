// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// A writer that reads a credential and writes a changed copy back must begin
// its fenced write (beginCredentialWrite) BEFORE the read. The erase generation
// then refuses its Put when an erase lands between the read and the write; a
// begin placed after the read would read the advanced generation and write the
// erased credential back. Each test erases the person the moment the writer's
// read returns, and moves the begin's meaning into the outcome: the namespace
// is empty afterwards. The control run of each, with no erase, proves the
// writer reaches its Put.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
)

// genSecrets is memSecrets with an erase generation (secretstore.Generational)
// and a fenced Put, and a hook that erases an owner right after the first read
// of that owner's namespace returns.
type genSecrets struct {
	secretstore.Store
	owner string
	st    *genState
}

type genState struct {
	mu        sync.Mutex
	gens      map[string]int64
	hookOwner string
	hook      func()
	putOwner  string
	beforePut func()
}

func newGenSecrets(inner secretstore.Store) genSecrets {
	return genSecrets{Store: inner, st: &genState{gens: map[string]int64{}}}
}

// eraseAfterFirstRead arms the hook: the first Get in owner's namespace is
// followed by an erase of it, as an admin's erase landing mid-write.
func (g genSecrets) eraseAfterFirstRead(owner string) {
	g.st.mu.Lock()
	defer g.st.mu.Unlock()
	g.st.hookOwner = owner
	g.st.hook = func() {
		if _, err := secretstore.EraseOwner(context.Background(), g, owner); err != nil {
			panic(err)
		}
	}
}

// eraseBeforeFirstPut erases owner just before the first Put into its
// namespace reaches the store: an erase landing after a read-free capture began.
func (g genSecrets) eraseBeforeFirstPut(owner string) {
	g.st.mu.Lock()
	defer g.st.mu.Unlock()
	g.st.putOwner = owner
	g.st.beforePut = func() {
		if _, err := secretstore.EraseOwner(context.Background(), g, owner); err != nil {
			panic(err)
		}
	}
}

func (g genSecrets) For(owner string) secretstore.Store {
	g.Store = g.Store.For(owner)
	g.owner = owner
	return g
}

func (g genSecrets) Generation(context.Context) (int64, error) {
	g.st.mu.Lock()
	defer g.st.mu.Unlock()
	return g.st.gens[g.owner], nil
}

func (g genSecrets) AdvanceGeneration(context.Context) (int64, error) {
	g.st.mu.Lock()
	defer g.st.mu.Unlock()
	g.st.gens[g.owner]++
	return g.st.gens[g.owner], nil
}

func (g genSecrets) Put(ctx context.Context, name string, v []byte) error {
	g.st.mu.Lock()
	var run func()
	if g.owner != "" && g.owner == g.st.putOwner {
		run, g.st.beforePut, g.st.putOwner = g.st.beforePut, nil, ""
	}
	g.st.mu.Unlock()
	if run != nil {
		run()
	}
	if want, ok := secretstore.IfGenerationFrom(ctx, g.owner); ok && g.owner != "" {
		g.st.mu.Lock()
		cur := g.st.gens[g.owner]
		g.st.mu.Unlock()
		if cur != want {
			return secretstore.ErrGenerationChanged
		}
	}
	return g.Store.Put(ctx, name, v)
}

func (g genSecrets) Get(ctx context.Context, name string) ([]byte, error) {
	v, err := g.Store.Get(ctx, name)
	g.st.mu.Lock()
	var run func()
	if g.owner != "" && g.owner == g.st.hookOwner {
		run, g.st.hook, g.st.hookOwner = g.st.hook, nil, ""
	}
	g.st.mu.Unlock()
	if run != nil {
		run()
	}
	return v, err
}

// The refusal stamp (stampADOOwnPATRefused) reads the stored token, then
// rewrites it with RefusedAt.
func TestCredentialWriteBeginsBeforeItsRead_OwnPATStamp(t *testing.T) {
	refuse := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) }))
	defer refuse.Close()
	claims := &identity.Claims{RunID: uuid.New(), SPIFFEID: "spiffe://wardyn.local/run"}
	owner := awsSSOTestOwner
	sn := adoEntraScopeSnapshot{OwnerSubject: owner, ProviderRowID: ownPATRowID, Organisation: "contoso"}
	blob := adoOwnPATBlob{Token: "stamp-target-token", Org: "contoso", ExpiresOn: awsSSOTestFixedNow.AddDate(0, 0, 10), StoredAt: awsSSOTestFixedNow}

	for _, erase := range []bool{false, true} {
		sec := newGenSecrets(&memSecrets{m: map[string][]byte{}, owned: map[string]map[string][]byte{}})
		s := &Server{cfg: Config{Secrets: sec, Now: func() time.Time { return awsSSOTestFixedNow }, Audit: &memAudit{}}}
		raw, _ := json.Marshal(blob)
		if err := sec.For(owner).Put(context.Background(), adoOwnPATSecretName(ownPATRowID), raw); err != nil {
			t.Fatal(err)
		}
		if erase {
			sec.eraseAfterFirstRead(owner)
		}
		s.stampADOOwnPATRefused(context.Background(), claims, sn, blob, refuse.URL+"/x")
		got := namesOf(t, sec, owner)
		switch {
		case erase && len(got) != 0:
			t.Fatalf("the stamp wrote the erased token back: namespace %v", got)
		case !erase:
			cur, found, err := s.readADOOwnPAT(context.Background(), owner, ownPATRowID)
			if err != nil || !found || cur.RefusedAt == nil {
				t.Fatalf("control: the stamp did not land (found %v, err %v)", found, err)
			}
		}
	}
}

// The AWS SSO renewal (refreshAWSSSOBlob) re-reads the stored pair under its
// lock, redeems the refresh token, and stores the renewed pair.
func TestCredentialWriteBeginsBeforeItsRead_AWSSSORenewal(t *testing.T) {
	for _, erase := range []bool{false, true} {
		sec := newGenSecrets(&memSecrets{m: map[string][]byte{}, owned: map[string]map[string][]byte{}})
		s := &Server{cfg: Config{Secrets: sec, MaskRegistry: secretmask.NewRegistry(), Now: func() time.Time { return awsSSOTestFixedNow }, Audit: &memAudit{}}}
		blob := putAWSSSOBlob(t, s, awsSSOTestFixedNow.Add(-time.Minute))
		fakeOIDC(t, func(w http.ResponseWriter, _ map[string]string, _ int) {
			_ = json.NewEncoder(w).Encode(map[string]any{"accessToken": "fresh-access-token-abcdefghij", "expiresIn": 3600, "refreshToken": "rotated-refresh-token-abcdefghij"})
		})
		if erase {
			sec.eraseAfterFirstRead(awsSSOTestOwner)
		}
		s.refreshAWSSSOBlob(context.Background(), awsSSOTestScope(), blob)
		got := namesOf(t, sec, awsSSOTestOwner)
		switch {
		case erase && len(got) != 0:
			t.Fatalf("the renewal wrote the erased pair back: namespace %v", got)
		case !erase && storedSSOBlob(t, s).RefreshToken != "rotated-refresh-token-abcdefghij":
			t.Fatal("control: the renewal did not land")
		}
	}
}

// An Entra redemption (redeemEntraAccessLocked) reads the stored sign-in and
// stores the rotated refresh token.
func TestCredentialWriteBeginsBeforeItsRead_EntraRotation(t *testing.T) {
	for _, erase := range []bool{false, true} {
		f := newADOFixture(t)
		subject := f.fake.Subject()
		if w := f.capture(t, subject); w.Code != http.StatusFound {
			t.Fatalf("capture: %d %s", w.Code, w.Body)
		}
		before, _ := f.stored(t, subject)
		sec := newGenSecrets(f.srv.cfg.Secrets)
		f.srv.cfg.Secrets = sec
		if erase {
			sec.eraseAfterFirstRead(subject)
		}
		_, _ = f.srv.RedeemADOEntraAccess(context.Background(), f.cfg, subject, f.cfg.Scopes)
		got := namesOf(t, sec, subject)
		switch {
		case erase && len(got) != 0:
			t.Fatalf("the rotation wrote the erased sign-in back: namespace %v", got)
		case !erase:
			if after, found := f.stored(t, subject); !found || after.RefreshToken == before.RefreshToken {
				t.Fatal("control: the rotation did not land")
			}
		}
	}
}

// A capture reads nothing before it writes, so the begin putOwned makes itself
// is the only fence it has: an erase landing after the capture began and before
// its Put refuses the Put. Without that begin the Put reads the advanced
// generation and writes the erased person's new sign-in behind the erase.
func TestCredentialWriteBeginsBeforeItsRead_ACaptureBeginsInPutOwned(t *testing.T) {
	for _, erase := range []bool{false, true} {
		f := newADOFixture(t)
		subject := f.fake.Subject()
		sec := newGenSecrets(f.srv.cfg.Secrets)
		f.srv.cfg.Secrets = sec
		if erase {
			sec.eraseBeforeFirstPut(subject)
		}
		f.capture(t, subject)
		got := namesOf(t, sec, subject)
		switch {
		case erase && len(got) != 0:
			t.Fatalf("a capture that began before the erase wrote %v behind it", got)
		case !erase && len(got) == 0:
			t.Fatal("control: the capture stored nothing")
		}
	}
}
