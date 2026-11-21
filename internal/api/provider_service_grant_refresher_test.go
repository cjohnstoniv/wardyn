// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The refresher and the clean-up worker, driven one pass at a time on the fixture's fake clock.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// addRow appends a copy of the grant row under a new id and uid, with its own token URL and a written secret.
func (f *svcFix) addRow(id, tokenURL string) types.ModelProvider {
	f.t.Helper()
	f.site.mu.Lock()
	base := f.site.sc.ModelProviders.Providers[0]
	f.site.mu.Unlock()
	p := base
	p.ID, p.UID, p.BaseURL = id, uuid.NewString(), "https://"+id+".corp.example"
	o := *base.OIDC
	g := *base.OIDC.ServiceGrant
	g.TokenURL = tokenURL
	o.ServiceGrant = &g
	p.OIDC = &o
	f.site.mu.Lock()
	f.site.sc.ModelProviders = &types.ModelProviders{Providers: append(append([]types.ModelProvider(nil), f.site.sc.ModelProviders.Providers...), p)}
	f.site.mu.Unlock()
	raw, _ := json.Marshal(serviceGrantRecord{V: 1, TokenURL: tokenURL, ClientID: g.ClientID, BaseURL: p.BaseURL, ClientSecret: svcClientSecret})
	f.sec.set(serviceGrantSecretName(p.UID), raw)
	return p
}

// addClaim stores a clean-up claim on a retired uid whose record is in the secret store.
func (f *svcFix) addClaim(i int) string {
	f.t.Helper()
	uid := uuid.NewString()
	raw, _ := json.Marshal(serviceGrantRecord{V: 1, TokenURL: svcTokenURL, ClientID: svcClientID, BaseURL: "https://old.corp.example", ClientSecret: svcClientSecret})
	f.sec.set(serviceGrantSecretName(uid), raw)
	f.sec.mu.Lock()
	rev := f.sec.revOf(serviceGrantSecretName(uid))
	f.sec.mu.Unlock()
	f.st.gmu.Lock()
	if f.st.cleanups == nil {
		f.st.cleanups = map[string]store.ServiceGrantCleanup{}
	}
	f.st.cleanups[uid] = store.ServiceGrantCleanup{ID: uuid.NewString(), ProviderID: fmt.Sprintf("gone-%d", i), Reason: cleanupReasonGrantRemoved, Revision: rev,
		Since: f.clock.Now().Add(time.Duration(i) * time.Second)}
	f.st.gmu.Unlock()
	return uid
}

func (f *svcFix) claims() map[string]store.ServiceGrantCleanup {
	f.st.gmu.Lock()
	defer f.st.gmu.Unlock()
	out := map[string]store.ServiceGrantCleanup{}
	for k, v := range f.st.cleanups {
		out[k] = v
	}
	return out
}

func (f *svcFix) retry(uid string) *svcHTTP {
	w := f.serve(http.MethodPost, "/service-grant-cleanups/"+uid+"/retry", nil)
	return &svcHTTP{Code: w.Code, Body: w.Body.String()}
}

type svcHTTP struct {
	Code int
	Body string
}

func TestServiceGrantRefresher_FailedRowAttemptedEveryPass(t *testing.T) {
	f := newSvcFixWithSecret(t)
	f.idp.respond = func(n int, _ svcReq) svcAnswer {
		if n == 1 {
			f.clock.Advance(10 * time.Second) // the first attempt ends 10 s into its pass
		}
		return svcOAuthError(401, "invalid_client")
	}
	ctx := context.Background()
	f.srv.serviceGrantPass(ctx)
	f.srv.serviceGrantPass(ctx)
	f.srv.serviceGrantPass(ctx)
	if f.idp.count() != 3 {
		t.Fatalf("3 passes sent %d requests, want a failed row attempted at every pass", f.idp.count())
	}
}

func TestServiceGrantRefresher_WarmIdleRowSeesRotation(t *testing.T) {
	f := newSvcFixWithSecret(t)
	ctx := context.Background()
	f.srv.serviceGrantPass(ctx)
	f.srv.serviceGrantPass(ctx) // warm and idle: nothing is due
	if f.idp.count() != 1 {
		t.Fatalf("a warm row was attempted again: %d requests", f.idp.count())
	}
	rotated := "rotated-client-secret-0002-abcdefgh"
	f.writeSecret(rotated)
	f.srv.serviceGrantPass(ctx)
	if f.idp.count() != 2 {
		t.Fatalf("requests = %d, want one more after the rotation", f.idp.count())
	}
	if _, secret := svcBasic(t, f.idp.requests()[1].Header.Get("Authorization")); secret != rotated {
		t.Fatalf("the new request used %q, want the rotated secret", secret)
	}
}

func TestServiceGrantRefresher_SlowProvidersDoNotDelayHealthyRow(t *testing.T) {
	f := newSvcFixWithSecret(t)
	release := make(chan struct{})
	done := make(chan struct{})
	t.Cleanup(func() { close(release); <-done })
	f.idp.respond = func(n int, r svcReq) svcAnswer {
		if strings.Contains(r.URL, "hang") {
			return svcAnswer{Status: 200, Block: release}
		}
		return svcOK(n, 3600)
	}
	var hung []types.ModelProvider
	for i := range 3 {
		hung = append(hung, f.addRow(fmt.Sprintf("slow-%d", i), fmt.Sprintf("https://hang-%d.example.test/token", i)))
	}
	go func() { defer close(done); f.srv.serviceGrantPass(context.Background()) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if row, ok := f.st.statusOf(f.uid, f.srv.svcGrant.replica()); ok && row.Outcome == "ok" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the healthy row's status was not written while three providers hung")
		}
		time.Sleep(5 * time.Millisecond)
	}
	select {
	case <-done:
		t.Fatal("the pass ended while its providers were still hanging")
	default:
	}
	_ = hung
}

func TestServiceGrant_HeadlessReplicaRefreshes(t *testing.T) {
	f := newSvcFixWithSecret(t)
	if f.srv.cfg.Runner != nil {
		t.Fatal("the fixture's server has a runner")
	}
	f.srv.serviceGrantPass(context.Background())
	if f.idp.count() != 1 {
		t.Fatalf("a server with no runner sent %d requests in a pass, want 1", f.idp.count())
	}
	if row, ok := f.st.statusOf(f.uid, f.srv.svcGrant.replica()); !ok || row.Outcome != "ok" {
		t.Fatalf("status = %+v, %v", row, ok)
	}
}

func TestServiceGrant_SlowProviderDoesNotDelayRunWatcher(t *testing.T) {
	f := newSvcFixWithSecret(t)
	release := make(chan struct{})
	f.idp.respond = func(int, svcReq) svcAnswer { return svcAnswer{Status: 200, Block: release} }
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { close(release); cancel(); f.srv.WaitBackground() })
	start := time.Now()
	f.srv.StartServiceGrantRefresher(ctx)
	if d := time.Since(start); d > time.Second {
		t.Fatalf("StartServiceGrantRefresher took %v with a hanging token endpoint, want it to return at once", d)
	}
	// Its work is its own goroutines: a lock-taking pass of any other loop is unaffected.
	lctx, unlock, err := f.srv.lock(context.Background(), db.SiteConfigLockClass)
	if err != nil {
		t.Fatalf("a lock taken while the refresher hangs: %v", err)
	}
	_ = lctx
	unlock()
}

func TestServiceGrantRefresher_StorelessPassIsQuiet(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelError})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	srv := &Server{cfg: Config{Now: time.Now}}
	srv.serviceGrantPassSafely(context.Background())
	srv.serviceGrantHeartbeat(context.Background())
	srv.serviceGrantCleanupPassSafely(context.Background())
	if buf.Len() != 0 {
		t.Fatalf("a pass on a server with no store logged at Error: %s", buf.String())
	}
}

func TestServiceGrantCleanup_BoundedPassAndBackoff(t *testing.T) {
	f := newSvcFix(t)
	var cur, peak atomic.Int32
	f.sec.hold = func(op, _ string) {
		if op != "delete" {
			return
		}
		n := cur.Add(1)
		for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
		}
		time.Sleep(2 * time.Millisecond)
		cur.Add(-1)
	}
	f.sec.delErr = errSvcDown
	var uids []string
	for i := range 20 {
		uids = append(uids, f.addClaim(i))
	}
	ctx := context.Background()
	f.srv.serviceGrantCleanupPass(ctx)
	if n := f.sec.callsOf("delete"); n != 8 || peak.Load() > 1 {
		t.Fatalf("first pass: %d delete attempts, %d at once, want 8 and 1", n, peak.Load())
	}
	f.srv.serviceGrantCleanupPass(ctx) // the 8 are in backoff; the next 8 go
	if n := f.sec.callsOf("delete"); n != 16 {
		t.Fatalf("second pass: %d attempts in all, want 16", n)
	}
	f.srv.serviceGrantCleanupPass(ctx)
	f.srv.serviceGrantCleanupPass(ctx)
	if n := f.sec.callsOf("delete"); n != 20 {
		t.Fatalf("all claims attempted once and none twice before the backoff: %d, want 20", n)
	}
	f.clock.Advance(61 * time.Second)
	f.srv.serviceGrantCleanupPass(ctx)
	if n := f.sec.callsOf("delete"); n != 28 {
		t.Fatalf("after the first backoff: %d attempts, want 28", n)
	}
	if rows := f.audit.find("model_provider.service_grant.secret_delete"); len(rows) != 0 {
		t.Fatalf("failed attempts wrote %d audit rows, want none", len(rows))
	}
	// The route attempts at once, backoff or not.
	before := f.sec.callsOf("delete")
	if res := f.retry(uids[0]); res.Code != http.StatusServiceUnavailable || f.sec.callsOf("delete") != before+1 {
		t.Fatalf("route retry = %d with %d new attempts, want an attempt at once", res.Code, f.sec.callsOf("delete")-before)
	}
}

func TestServiceGrantCleanup_EachRevisionAnswer_PG(t *testing.T) {
	// Absence settles with no delete.
	f := newSvcFix(t)
	uid := f.addClaim(0)
	name := serviceGrantSecretName(uid)
	f.sec.mu.Lock()
	delete(f.sec.m, name)
	f.sec.rev[name]++
	f.sec.mu.Unlock()
	if res := f.retry(uid); res.Code != http.StatusNoContent || f.sec.callsOf("delete") != 0 || len(f.claims()) != 0 {
		t.Fatalf("absence: %d, %d deletes, %d claims; want 204, none, none", res.Code, f.sec.callsOf("delete"), len(f.claims()))
	}
	// The claim's own revision deletes, guarded, and settles.
	uid = f.addClaim(1)
	if res := f.retry(uid); res.Code != http.StatusNoContent || f.sec.has(serviceGrantSecretName(uid)) || len(f.claims()) != 0 {
		t.Fatalf("own revision: %d, record kept %v, %d claims", res.Code, f.sec.has(serviceGrantSecretName(uid)), len(f.claims()))
	}
	// Another revision keeps the claim, deletes nothing and answers 409 revision_changed.
	uid = f.addClaim(2)
	f.sec.set(serviceGrantSecretName(uid), []byte(`{"v":1,"written":"outside"}`))
	before := f.sec.callsOf("delete")
	res := f.retry(uid)
	if res.Code != http.StatusConflict || !strings.Contains(res.Body, reasonServiceGrantCleanupRevisionChanged) || f.sec.callsOf("delete") != before || len(f.claims()) != 1 {
		t.Fatalf("another revision: %d %s, %d deletes, %d claims", res.Code, res.Body, f.sec.callsOf("delete")-before, len(f.claims()))
	}
	// A read error keeps the claim.
	f.sec.revErr = errSvcDown
	if res = f.retry(uid); res.Code != http.StatusServiceUnavailable || len(f.claims()) != 1 || f.sec.callsOf("delete") != before {
		t.Fatalf("read error: %d, %d claims, %d deletes", res.Code, len(f.claims()), f.sec.callsOf("delete")-before)
	}
	// A store that keeps no revision (r16 L1): zero Delete calls, the claim kept, 409 cleanup_unsupported.
	f.sec.revErr = nil
	f.sec.noRev = true
	if res = f.retry(uid); res.Code != http.StatusConflict || !strings.Contains(res.Body, reasonServiceGrantCleanupUnsupported) ||
		f.sec.callsOf("delete") != before || len(f.claims()) != 1 {
		t.Fatalf("no revision: %d %s, %d deletes, %d claims; want 409, zero Delete calls, the claim kept", res.Code, res.Body, f.sec.callsOf("delete")-before, len(f.claims()))
	}
	// The worker does the same.
	f.srv.serviceGrantCleanupPass(context.Background())
	if f.sec.callsOf("delete") != before || len(f.claims()) != 1 {
		t.Fatalf("the worker deleted on a store with no revision: %d deletes", f.sec.callsOf("delete")-before)
	}
}

func TestServiceGrant_OverlappingRetriesSettleOnce(t *testing.T) {
	f := newSvcFix(t)
	uid := f.addClaim(0)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() { defer wg.Done(); f.retry(uid) }()
	}
	wg.Wait()
	f.srv.serviceGrantCleanupPass(context.Background())
	if rows := f.audit.find("model_provider.service_grant.secret_delete"); len(rows) != 1 {
		t.Fatalf("settlement audit rows = %d, want exactly one", len(rows))
	}
	if f.st.settles != 1 || len(f.claims()) != 0 {
		t.Fatalf("settles = %d, claims = %d, want 1 and 0", f.st.settles, len(f.claims()))
	}
}
