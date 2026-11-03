// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A stored endpoint sign-in is presented only to the authority, and for the address, it was captured for. Each
// race below holds a redemption at its authority's discovery (after it has read the row, before it takes the
// sign-in lock), changes the row and signs in again, then lets it go.

const oidcDiscoveryPath = "/.well-known/openid-configuration"

// oidcPauseFirst holds the first request whose path ends in suffix until release is closed, closing reached
// when it arrives.
func oidcPauseFirst(suffix string) (gate func(*http.Request), reached, release chan struct{}) {
	reached, release = make(chan struct{}), make(chan struct{})
	var once sync.Once
	gate = func(r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, suffix) {
			return
		}
		first := false
		once.Do(func() { first = true })
		if first {
			close(reached)
			<-release
		}
	}
	return gate, reached, release
}

func oidcWait(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// oidcOtherAuthority is a second authority: discovery (held by gate when set) and a token endpoint that
// records every request and answers invalid_grant, so a token wrongly sent there would also be deleted.
type oidcOtherAuthority struct {
	srv    *httptest.Server
	mu     sync.Mutex
	bodies []string
}

func newOIDCOtherAuthority(t *testing.T, gate func(*http.Request)) *oidcOtherAuthority {
	t.Helper()
	a := &oidcOtherAuthority{}
	mux := http.NewServeMux()
	mux.HandleFunc(oidcDiscoveryPath, func(w http.ResponseWriter, r *http.Request) {
		if gate != nil {
			gate(r)
		}
		base := a.srv.URL
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer": base, "authorization_endpoint": base + "/authorize", "token_endpoint": base + "/token",
			"jwks_uri": base + "/keys", "id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		a.mu.Lock()
		a.bodies = append(a.bodies, r.PostForm.Encode())
		a.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
	})
	a.srv = httptest.NewServer(mux)
	t.Cleanup(a.srv.Close)
	return a
}

func (a *oidcOtherAuthority) posts() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.bodies...)
}

// rawSignIn is the person's stored -oidc row byte for byte, nil when there is none.
func (f *oidcFlow) rawSignIn(t *testing.T, owner string) []byte {
	t.Helper()
	raw, err := f.srv.cfg.Secrets.For(owner).Get(context.Background(), providerSecretName(f.row.UID, providerOIDCPart))
	if err != nil {
		return nil
	}
	return raw
}

func (f *oidcFlow) mustSignIn(t *testing.T, owner string) {
	t.Helper()
	if _, w := f.capture(t, owner); w.Header().Get("Location") != f.doneRedirect() {
		t.Fatalf("sign-in: %d %s", w.Code, w.Header().Get("Location"))
	}
}

func withOIDC(p types.ModelProvider, edit func(*types.ProviderOIDC)) types.ModelProvider {
	o := *p.OIDC
	edit(&o)
	p.OIDC = &o
	return p
}

// A redemption that read the row at authority A and stalled is resumed after the admin moved the row to B and
// the person signed in there: A receives nothing, B's new sign-in is untouched and nothing is refreshed at B.
func TestEndpointOIDCRedeem_AStaleRedemptionPostsNothingToItsOldAuthority(t *testing.T) {
	f := newOIDCFlow(t, "")
	gate, reached, release := oidcPauseFirst(oidcDiscoveryPath)
	a := newOIDCOtherAuthority(t, gate)
	rowA := withOIDC(f.row, func(o *types.ProviderOIDC) { o.Issuer = a.srv.URL })
	f.site.set(func(b *types.ModelProviders) { b.Providers[0] = rowA })

	done := make(chan error, 1)
	go func() {
		_, err := f.srv.RedeemEndpointOIDCAccess(context.Background(), oidcPersonSub, rowA)
		done <- err
	}()
	oidcWait(t, reached, "the redemption to reach authority A")

	purgeChangedProvider(t, f.srv, rowA, f.row)
	f.site.set(func(b *types.ModelProviders) { b.Providers[0] = f.row })
	f.mustSignIn(t, oidcPersonSub)
	before := f.rawSignIn(t, oidcPersonSub)
	if before == nil {
		t.Fatal("setup: the sign-in at B was not stored")
	}

	close(release)
	var err error
	select {
	case err = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the redemption did not finish")
	}
	if !errors.Is(err, ErrADOEntraNotCaptured) {
		t.Fatalf("stale redemption = %v, want ErrADOEntraNotCaptured (sign in again)", err)
	}
	if got := a.posts(); len(got) != 0 {
		t.Fatalf("authority A received %d token requests, want none: %q", len(got), got)
	}
	if got := f.fake.RefreshRequestScopes(); len(got) != 0 {
		t.Errorf("authority B was refreshed %d times by a redemption resolved at A", len(got))
	}
	if after := f.rawSignIn(t, oidcPersonSub); string(after) != string(before) {
		t.Error("B's stored sign-in changed")
	}
}

// The token URL alone moving, on the same issuer and client, is a different authority: nothing is posted to
// either URL and the stored sign-in is kept. Discovery naming another token URL with no row change moves the
// digest the same way.
func TestEndpointOIDCRedeem_ASameIssuerTokenURLChangeMismatches(t *testing.T) {
	f := newOIDCFlow(t, "")
	f.mustSignIn(t, oidcPersonSub)
	before := f.rawSignIn(t, oidcPersonSub)
	a := newOIDCOtherAuthority(t, nil)
	moved := withOIDC(f.row, func(o *types.ProviderOIDC) { o.TokenURL = a.srv.URL + "/token" })
	f.site.set(func(b *types.ModelProviders) { b.Providers[0] = moved })

	if _, err := f.srv.RedeemEndpointOIDCAccess(context.Background(), oidcPersonSub, moved); !errors.Is(err, ErrADOEntraNotCaptured) {
		t.Fatalf("redeem at a moved token URL = %v, want ErrADOEntraNotCaptured", err)
	}
	if got := a.posts(); len(got) != 0 {
		t.Fatalf("the new token URL received %d requests, want none", len(got))
	}
	if got := f.fake.RefreshRequestScopes(); len(got) != 0 {
		t.Errorf("the old token URL was refreshed %d times", len(got))
	}
	if after := f.rawSignIn(t, oidcPersonSub); string(after) != string(before) {
		t.Error("the stored sign-in changed")
	}
	if endpointOIDCCaptureDigest(f.row, "https://a.example/token") == endpointOIDCCaptureDigest(f.row, "https://b.example/token") {
		t.Error("the capture digest does not cover the discovered token URL")
	}
}

// A sign-in stored before the binding carries no digest: it reads as no sign-in, is never posted, and is kept.
func TestEndpointOIDCRedeem_ASignInWithNoDigestIsNotUsed(t *testing.T) {
	f := newOIDCFlow(t, "")
	f.mustSignIn(t, oidcPersonSub)
	var m map[string]any
	if err := json.Unmarshal(f.rawSignIn(t, oidcPersonSub), &m); err != nil || m["capture_digest"] == nil {
		t.Fatalf("setup: stored sign-in %v has no capture digest (%v)", m, err)
	}
	delete(m, "capture_digest")
	old, _ := json.Marshal(m)
	if err := f.srv.cfg.Secrets.For(oidcPersonSub).Put(context.Background(), providerSecretName(f.row.UID, providerOIDCPart), old); err != nil {
		t.Fatal(err)
	}

	if _, err := f.srv.RedeemEndpointOIDCAccess(context.Background(), oidcPersonSub, f.row); !errors.Is(err, ErrADOEntraNotCaptured) {
		t.Fatalf("redeem an undigested sign-in = %v, want ErrADOEntraNotCaptured", err)
	}
	if got := f.fake.RefreshRequestScopes(); len(got) != 0 {
		t.Errorf("an undigested sign-in was presented %d times", len(got))
	}
	if after := f.rawSignIn(t, oidcPersonSub); string(after) != string(old) {
		t.Error("the undigested sign-in was changed or deleted")
	}
	if _, found := f.stored(t, oidcPersonSub); found {
		t.Error("the status lanes read an undigested sign-in as signed in")
	}
}

// The normal path: the capture stores the digest of the row and the token URL it exchanged at, and a
// redemption with the same row posts once, to that authority.
func TestEndpointOIDCRedeem_AMatchingDigestPostsToItsAuthority(t *testing.T) {
	f := newOIDCFlow(t, "")
	f.mustSignIn(t, oidcPersonSub)
	blob, found := f.stored(t, oidcPersonSub)
	ta, _, err := f.srv.endpointOIDCAuthority(context.Background(), f.row.OIDC)
	if err != nil || !found || blob.CaptureDigest != endpointOIDCCaptureDigest(f.row, ta.tokenURL) {
		t.Fatalf("stored digest %q (found=%v, %v), want the row's at %s", blob.CaptureDigest, found, err, ta.tokenURL)
	}
	access, err := f.srv.RedeemEndpointOIDCAccess(context.Background(), oidcPersonSub, f.row)
	if err != nil || access.AccessToken == "" {
		t.Fatalf("redeem = %+v, %v", access, err)
	}
	if got := f.fake.RefreshRequestScopes(); len(got) != 1 {
		t.Fatalf("the authority was refreshed %d times, want once", len(got))
	}
	if after, _ := f.stored(t, oidcPersonSub); after.CaptureDigest != blob.CaptureDigest {
		t.Error("the rotation dropped the capture digest")
	}
}

// Only the base URL moves (same issuer, token URL and client): a resolve that checked the old row and stalled
// neither refreshes the new sign-in nor injects its token into the old endpoint.
func TestEndpointOIDCSink_AStaleResolveAfterABaseURLChangeInjectsNothing(t *testing.T) {
	f := newOIDCRunFixture(t)
	f.signIn(t)
	plan, _, _, ok := f.dispatch(t)
	if !ok || len(plan.injections) != 1 {
		t.Fatalf("dispatch ok=%v injections=%d", ok, len(plan.injections))
	}
	gate, reached, release := oidcPauseFirst(oidcDiscoveryPath)
	f.gate.Store(&gate)

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- f.resolve(plan, "?"+adoResolvePhase+"="+adoResolvePhaseBoot) }()
	oidcWait(t, reached, "the resolve to reach discovery")

	old := f.liveRow(t)
	moved := old
	moved.BaseURL = old.BaseURL + "/v2"
	purgeChangedProvider(t, f.srv, old, moved)
	f.site.set(func(b *types.ModelProviders) { b.Providers[0] = moved })
	f.signIn(t)
	before := f.rawSignIn(t, oidcPersonSub)
	if before == nil {
		t.Fatal("setup: the new sign-in was not stored")
	}

	close(release)
	var w *httptest.ResponseRecorder
	select {
	case w = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the resolve did not finish")
	}
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), string(ADOEntraFailureNotCaptured)) {
		t.Fatalf("stale resolve = %d %s, want 403 %s", w.Code, w.Body.String(), ADOEntraFailureNotCaptured)
	}
	for _, tok := range f.wire.all() {
		if strings.Contains(w.Body.String(), tok) {
			t.Fatal("the stale resolve answered with a token")
		}
	}
	if got := f.fake.RefreshRequestScopes(); len(got) != 0 {
		t.Errorf("the new sign-in was refreshed %d times by a resolve for the old address", len(got))
	}
	if after := f.rawSignIn(t, oidcPersonSub); string(after) != string(before) {
		t.Error("the new sign-in changed")
	}
}
