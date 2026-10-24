// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// countedGets counts the value reads (the decrypts) that reach the store. Its
// revision reads go to the store underneath and are not counted.
type countedGets struct {
	revisionedSecrets
	n *atomic.Int32
}

func (c countedGets) Get(ctx context.Context, name string) ([]byte, error) {
	c.n.Add(1)
	return c.revisionedSecrets.Get(ctx, name)
}

func (c countedGets) For(owner string) secretstore.Store {
	return countedGets{revisionedSecrets{c.Store.For(owner)}, c.n}
}

// cacheFirstFixture is a resolve fixture whose store keeps revisions, as the
// Postgres store does, and audits every value read, as the daemon's does.
func cacheFirstFixture(t *testing.T) (*adoResolveFixture, *atomic.Int32) {
	t.Helper()
	rf := newADOResolveFixture(t)
	gets := new(atomic.Int32)
	rf.srv.cfg.Secrets = secretstore.Audited(countedGets{revisionedSecrets{rf.srv.cfg.Secrets}, gets}, rf.audit)
	return rf, gets
}

// signInReads are the secret.read rows for the person's stored sign-in.
func (rf *adoResolveFixture) signInReads() []types.AuditEvent {
	var out []types.AuditEvent
	for _, ev := range rf.audit.find("secret.read") {
		if ev.Target == adoCapture(rf.cfg).secretName {
			out = append(out, ev)
		}
	}
	return out
}

// #2140: a resolve served from the cache neither decrypts the stored sign-in
// nor writes a secret.read row for it.
func TestADOResolve_CacheHitReadsNoStoredSignIn(t *testing.T) {
	rf, gets := cacheFirstFixture(t)
	if w := rf.resolve(t, rf.subject, "dev.azure.com"); w.Code != http.StatusOK {
		t.Fatalf("warm: status %d body %q", w.Code, w.Body.String())
	}
	gets.Store(0)
	rf.audit.rows = nil
	if w := rf.resolve(t, rf.subject, "dev.azure.com"); w.Code != http.StatusOK {
		t.Fatalf("second resolve: status %d body %q", w.Code, w.Body.String())
	}
	if n := gets.Load(); n != 0 {
		t.Errorf("a cache hit read the stored sign-in %d times; want none", n)
	}
	if rows := rf.signInReads(); len(rows) != 0 {
		t.Errorf("a cache hit wrote %d secret.read rows for the stored sign-in; want none", len(rows))
	}
}

// A miss reads the stored sign-in as before the cache moved first: once for
// the request scopes, once for the redemption, each an audited refresh read.
func TestADOResolve_CacheMissReadsAndAuditsTheStoredSignIn(t *testing.T) {
	rf, gets := cacheFirstFixture(t)
	if w := rf.resolve(t, rf.subject, "dev.azure.com"); w.Code != http.StatusOK {
		t.Fatalf("resolve: status %d body %q", w.Code, w.Body.String())
	}
	if n := gets.Load(); n != 2 {
		t.Errorf("a cache miss read the stored sign-in %d times, want 2 (request scopes, redemption)", n)
	}
	rows := rf.signInReads()
	if len(rows) != 2 {
		t.Fatalf("a cache miss wrote %d secret.read rows for the stored sign-in, want 2", len(rows))
	}
	for _, ev := range rows {
		if !strings.Contains(string(ev.Data), `"purpose":"`+string(secretstore.PurposeADORefresh)+`"`) {
			t.Errorf("secret.read row data = %s, want purpose %s", ev.Data, secretstore.PurposeADORefresh)
		}
	}
}

// A resolve after the person's stored sign-in ended misses, finds nothing
// stored and serves no token. The store keeps no revisions here, so the
// forget at the end is the only thing that can stop the hit.
func requireNextResolveMisses(t *testing.T, rf *adoResolveFixture, end func() error) {
	t.Helper()
	if w := rf.resolve(t, rf.subject, "dev.azure.com"); w.Code != http.StatusOK {
		t.Fatalf("warm: status %d body %q", w.Code, w.Body.String())
	}
	if err := end(); err != nil {
		t.Fatal(err)
	}
	w := rf.resolve(t, rf.subject, "dev.azure.com")
	if w.Code == http.StatusOK || strings.Contains(w.Body.String(), "fake-entra-access-") {
		t.Fatalf("after the sign-in ended: status %d body %q; want a refusal with no token: the cached token outlived it", w.Code, w.Body.String())
	}
}

// The person's credential erase evicts the token cached under the row's ceiling.
func TestADOResolve_EraseMakesTheNextResolveMiss(t *testing.T) {
	rf := newADOResolveFixture(t)
	requireNextResolveMisses(t, rf, func() error {
		var rep secretstore.EraseReport
		return rf.srv.eraseLocked(context.Background(), rf.subject, rf.cfg.RowID, nil, &rep)
	})
}

// Disconnecting the Azure DevOps sign-in evicts it too.
func TestADOResolve_DisconnectMakesTheNextResolveMiss(t *testing.T) {
	rf := newADOResolveFixture(t)
	requireNextResolveMisses(t, rf, func() error {
		removed, err := rf.srv.forgetADOSignIn(context.Background(), rf.subject, rf.cfg.RowID)
		if err == nil && !removed {
			t.Fatal("the disconnect removed no stored sign-in")
		}
		return err
	})
}

// A sign-in another writer replaced (a fresh sign-in on another replica, a
// rotation stored there) retires the cached token: the next resolve misses and
// redeems from the stored sign-in as it stands, reading and auditing it.
func TestADOResolve_RotatedSignInMakesTheNextResolveMiss(t *testing.T) {
	rf, gets := cacheFirstFixture(t)
	if w := rf.resolve(t, rf.subject, "dev.azure.com"); w.Code != http.StatusOK {
		t.Fatalf("warm: status %d body %q", w.Code, w.Body.String())
	}
	ctx := secretstore.WithPurpose(context.Background(), secretstore.PurposeADORefresh)
	st, name := rf.srv.cfg.Secrets.For(rf.subject), adoCapture(rf.cfg).secretName
	raw, err := st.Get(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Put(ctx, name, append(raw, ' ')); err != nil {
		t.Fatal(err)
	}
	gets.Store(0)
	rf.audit.rows = nil
	if w := rf.resolve(t, rf.subject, "dev.azure.com"); w.Code != http.StatusOK {
		t.Fatalf("after the rotation: status %d body %q", w.Code, w.Body.String())
	}
	if n := gets.Load(); n == 0 || len(rf.signInReads()) == 0 {
		t.Errorf("after another writer replaced the sign-in, the resolve read it %d times with %d secret.read rows; want a miss that reads it",
			n, len(rf.signInReads()))
	}
}

// A cached token is handed out for at most adoEntraAccessCacheTTL after the
// fill, however long the authority let it live; the bound is the documented
// one hour.
func TestADOEntraAccessCache_EntryLivesAtMostItsTTL(t *testing.T) {
	if adoEntraAccessCacheTTL != time.Hour {
		t.Errorf("adoEntraAccessCacheTTL = %v; docs/AZURE-DEVOPS.md and docs/AUDIT-ACTIONS.md say one hour", adoEntraAccessCacheTTL)
	}
	var c adoEntraAccessCache
	c.put("k", adoEntraCached{ADOEntraAccess: ADOEntraAccess{AccessToken: "t", ExpiresAt: adoTestNow.Add(24 * time.Hour)}, filled: adoTestNow})
	if _, ok := c.get("k", adoTestNow.Add(adoEntraAccessCacheTTL-time.Second)); !ok {
		t.Error("a token inside its bound was not served")
	}
	if _, ok := c.get("k", adoTestNow.Add(adoEntraAccessCacheTTL)); ok {
		t.Error("a token was served past adoEntraAccessCacheTTL after its fill")
	}
}

// Entries are keyed by person and credential (the provider row): another
// person, or the same person's sign-in for another row, misses on this one's
// entry and redeems nothing. The store keeps no revisions, so the key alone
// decides.
func TestADOResolve_AnotherPersonNeverHitsTheCache(t *testing.T) {
	rf := newADOResolveFixture(t)
	if w := rf.resolve(t, rf.subject, "dev.azure.com"); w.Code != http.StatusOK {
		t.Fatalf("warm: status %d body %q", w.Code, w.Body.String())
	}
	got, err := rf.srv.adoEntraAccessFor(context.Background(), rf.cfg, "someone-else@example.com", nil, false)
	if err == nil || got.AccessToken != "" {
		t.Errorf("another person's lookup = %q, %v; want no token and an error", got.AccessToken, err)
	}
	other := rf.cfg
	other.RowID = "ado-row-2"
	got, err = rf.srv.adoEntraAccessFor(context.Background(), other, rf.subject, nil, false)
	if err == nil || got.AccessToken != "" {
		t.Errorf("the same person's lookup for another row = %q, %v; want no token and an error", got.AccessToken, err)
	}
}
