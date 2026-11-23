// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package oidc_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc/oidctest"
	jose "github.com/go-jose/go-jose/v4"

	writoidc "github.com/cjohnstoniv/wardyn/internal/auth/oidc"
)

func isReserved(sub string) bool {
	s := strings.ToLower(strings.TrimSpace(sub))
	return strings.HasPrefix(s, "subject:") || strings.HasPrefix(s, "local:")
}

// forgedToken signs a well-formed logout token with a key the provider does not publish.
func (e *idpEnv) forgedToken(t *testing.T, key *rsa.PrivateKey, kid, sub, jti string) string {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{
		"iss": e.httpSrv.URL, "aud": e.clientID, "sub": sub, "jti": jti,
		"iat": time.Now().Unix(), "exp": time.Now().Add(2 * time.Minute).Unix(),
		"events": map[string]any{logoutEvent: map[string]any{}},
	})
	return oidctest.SignIDToken(key, kid, "RS256", string(raw))
}

func (e *idpEnv) tokenWithJTI(t *testing.T, sub, jti string) string {
	return e.logoutToken(t, sub, func(c map[string]any) { c["jti"] = jti })
}

func holdToken(t *testing.T, h http.HandlerFunc, token string) *httptest.ResponseRecorder {
	t.Helper()
	return postLogout(h, token)
}

func wantStatus(t *testing.T, what string, w *httptest.ResponseRecorder, code int) {
	t.Helper()
	if w.Code != code {
		t.Errorf("%s = %d %s, want %d", what, w.Code, w.Body.String(), code)
	}
}

// P3: a logout received while pending is queued with a 503 and, once the provider answers, cuts that sub's
// sessions: a cookie that was live before is rejected after.
func TestPending_BackchannelQueuedThenApplied(t *testing.T) {
	env := newIdPEnv(t)
	rev := &cutLedger{seq: 10}
	disc := env.newRoleMappingAuth(t, nil, "", nil, nil, func(cfg *writoidc.Config) { cfg.Revocations = rev })
	cookie := signedInCookie(t, env, disc)
	p := newPendingEnvFor(t, env, rev)
	h := p.auth.BackchannelLogoutHandler(isReserved, nil)

	w := holdToken(t, h, env.logoutToken(t, "pairwise-1", nil))
	wantStatus(t, "a logout while pending", w, http.StatusServiceUnavailable)
	if !strings.Contains(w.Body.String(), "temporarily_unavailable") {
		t.Errorf("body = %s, want temporarily_unavailable", w.Body.String())
	}
	if got := p.sink.states(); len(got) != 1 || got[0] != writoidc.LogoutQueued {
		t.Fatalf("events = %v, want [queued]", got)
	}
	if live, _ := replay(p.auth, cookie); !live {
		t.Error("the held logout cut the session before the provider was reached")
	}
	p.recover()
	if err := p.auth.Discover(p.ctx); err != nil {
		t.Fatalf("Discover after recovery: %v", err)
	}
	if p.auth.Pending() {
		t.Error("still pending after the provider answered")
	}
	if live, reason := replay(p.auth, cookie); live || reason != "revoked_session" {
		t.Errorf("the cookie after the held logout was applied = live %v, %q; want revoked_session", live, reason)
	}
	if st := p.auth.LogoutQueueStats(); st.Held != 0 || st.Queued != 1 || st.Applied != 1 {
		t.Errorf("stats = %+v, want Held 0, Queued 1, Applied 1", st)
	}
	if p.sink.count(writoidc.LogoutApplied) != 1 || len(p.sink.applied) != 1 {
		t.Errorf("applied events %d, applies %v; want one", p.sink.count(writoidc.LogoutApplied), p.sink.applied)
	}
}

// P4: the bound, the duplicate rule, a token that is not a logout token, and a forged token that claims a real
// token's jti.
func TestLogoutQueue_BoundDedupAndPrecheck(t *testing.T) {
	p := newPendingEnv(t)
	h := p.auth.BackchannelLogoutHandler(isReserved, nil)
	env := p.env
	for i := 0; i < writoidc.LogoutQueueCap; i++ {
		wantStatus(t, fmt.Sprintf("token %d", i), holdToken(t, h, env.tokenWithJTI(t, fmt.Sprintf("u%d", i), fmt.Sprintf("j%d", i))), http.StatusServiceUnavailable)
	}
	if st := p.auth.LogoutQueueStats(); st.Held != writoidc.LogoutQueueCap {
		t.Fatalf("held = %d, want %d", st.Held, writoidc.LogoutQueueCap)
	}
	over := env.tokenWithJTI(t, "overflow", "jo")
	wantStatus(t, "the 257th token", holdToken(t, h, over), http.StatusServiceUnavailable)
	if p.sink.count(writoidc.LogoutRefusedQueueFull) != 1 || p.auth.LogoutQueueStats().RefusedQueueFull != 1 {
		t.Errorf("a full queue gave %d refused_queue_full events, stats %+v", p.sink.count(writoidc.LogoutRefusedQueueFull), p.auth.LogoutQueueStats())
	}
	if ev := p.sink.events[len(p.sink.events)-1]; ev.TokenSHA256 == "" || ev.SubUnverified != "overflow" {
		t.Errorf("the refused_queue_full event = %+v, want the token hash and the unverified sub", ev)
	}
	if st := p.auth.LogoutQueueStats(); st.Held != writoidc.LogoutQueueCap {
		t.Errorf("held = %d after the overflow, want %d", st.Held, writoidc.LogoutQueueCap)
	}
}

// P4: the same raw token twice is one entry and one event.
func TestLogoutQueue_DuplicateRawTokenAddsNothing(t *testing.T) {
	p := newPendingEnv(t)
	h := p.auth.BackchannelLogoutHandler(isReserved, nil)
	tok := p.env.logoutToken(t, "pairwise-1", nil)
	wantStatus(t, "the first post", holdToken(t, h, tok), http.StatusServiceUnavailable)
	wantStatus(t, "the repeat", holdToken(t, h, tok), http.StatusServiceUnavailable)
	if st := p.auth.LogoutQueueStats(); st.Held != 1 || st.Queued != 1 || p.sink.count(writoidc.LogoutQueued) != 1 {
		t.Errorf("stats %+v, queued events %d; want one entry and one event", st, p.sink.count(writoidc.LogoutQueued))
	}
}

// P4: a forged token that claims a real token's jti does not displace the real one, and is dropped unapplied.
func TestLogoutQueue_ForgedJTIDoesNotDisplaceTheReal(t *testing.T) {
	p := newPendingEnv(t)
	h := p.auth.BackchannelLogoutHandler(isReserved, nil)
	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	forged := p.env.forgedToken(t, other, "test-key", "victim", "shared-jti")
	real := p.env.tokenWithJTI(t, "pairwise-1", "shared-jti")
	wantStatus(t, "the forged token", holdToken(t, h, forged), http.StatusServiceUnavailable)
	wantStatus(t, "the real token", holdToken(t, h, real), http.StatusServiceUnavailable)
	if p.auth.LogoutQueueStats().Held != 2 {
		t.Fatalf("held = %d, want both entries (dedup is on the raw token's hash, not the jti)", p.auth.LogoutQueueStats().Held)
	}
	p.recover()
	if err := p.auth.Discover(p.ctx); err != nil {
		t.Fatal(err)
	}
	if p.rev.cutCalls != 1 || p.rev.cuts["pairwise-1"] == 0 || p.rev.cuts["victim"] != 0 {
		t.Errorf("cuts %d %v; want only pairwise-1 cut", p.rev.cutCalls, p.rev.cuts)
	}
	if st := p.auth.LogoutQueueStats(); st.Unverifiable != 1 || st.Applied != 1 {
		t.Errorf("stats %+v, want one unverifiable and one applied", st)
	}
}

// P4: a token that cannot be a logout token for this client gets the discovered server's 400 and is not queued.
func TestLogoutQueue_PrecheckRefusals(t *testing.T) {
	p := newPendingEnv(t)
	h := p.auth.BackchannelLogoutHandler(isReserved, nil)
	env := p.env
	long := strings.Repeat("s", 257)
	for name, token := range map[string]string{
		"no token":         "",
		"not a jwt":        "garbage",
		"another audience": env.logoutToken(t, "pairwise-1", func(c map[string]any) { c["aud"] = "someone-else" }),
		"another issuer":   env.logoutToken(t, "pairwise-1", func(c map[string]any) { c["iss"] = "https://evil.example" }),
		"stale iat":        env.logoutToken(t, "pairwise-1", func(c map[string]any) { c["iat"] = time.Now().Add(-time.Hour).Unix() }),
		"no event":         env.logoutToken(t, "pairwise-1", func(c map[string]any) { c["events"] = map[string]any{} }),
		"a null event":     env.logoutToken(t, "pairwise-1", func(c map[string]any) { c["events"] = map[string]any{logoutEvent: nil} }),
		"a nonce":          env.logoutToken(t, "pairwise-1", func(c map[string]any) { c["nonce"] = "n" }),
		"no jti":           env.logoutToken(t, "pairwise-1", func(c map[string]any) { delete(c, "jti") }),
		"a sid and no sub": env.logoutToken(t, "", func(c map[string]any) { delete(c, "sub"); c["sid"] = "s1" }),
		"a sub too long":   env.logoutToken(t, long, nil),
	} {
		w := holdToken(t, h, token)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "invalid_request") {
			t.Errorf("%s = %d %s, want 400 invalid_request", name, w.Code, w.Body.String())
		}
	}
	if st := p.auth.LogoutQueueStats(); st.Held != 0 {
		t.Errorf("a refused token was held: %+v", st)
	}
	if n := p.sink.count(writoidc.LogoutRefusedUnreadable); n != 11 {
		t.Errorf("refused_unreadable events = %d, want 11", n)
	}
	for _, ev := range p.sink.events {
		if ev.SubUnverified != "" {
			t.Errorf("a refused_unreadable event carries a sub: %+v", ev)
		}
	}
}

// A reserved sub is refused at receipt, takes no slot, and carries no sub in its event.
func TestLogoutQueue_ReservedRefusedAtReceipt(t *testing.T) {
	p := newPendingEnv(t)
	h := p.auth.BackchannelLogoutHandler(isReserved, nil)
	for _, sub := range []string{"subject:x", " Subject:x", "local:x"} {
		w := holdToken(t, h, p.env.tokenWithJTI(t, sub, "j-"+sub))
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "the sub names a reserved principal") {
			t.Errorf("sub %q = %d %s, want 400 reserved", sub, w.Code, w.Body.String())
		}
	}
	if st := p.auth.LogoutQueueStats(); st.Held != 0 {
		t.Errorf("a reserved sub took a slot: %+v", st)
	}
	if n := p.sink.count(writoidc.LogoutRefusedReserved); n != 3 {
		t.Errorf("refused_reserved events = %d, want 3", n)
	}
	for _, ev := range p.sink.events {
		if ev.SubUnverified != "" {
			t.Errorf("a refused_reserved event carries the sub: %+v", ev)
		}
	}
}

// P5a: a held token that does not verify after discovery cuts nobody, is dropped, is recorded with its hash,
// and neither the raw token nor its sub-less log line leaks the token.
func TestLogoutQueue_UnverifiableNotApplied(t *testing.T) {
	logs := captureLogs(t)
	p := newPendingEnv(t)
	h := p.auth.BackchannelLogoutHandler(isReserved, nil)
	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	env := p.env
	tokens := map[string]string{
		"an unknown key and kid": env.forgedToken(t, other, "no-such-kid", "u1", "j1"),
		"a bad signature":        env.forgedToken(t, other, "test-key", "u2", "j2"),
		"expired at receipt":     env.logoutToken(t, "u3", func(c map[string]any) { c["exp"] = time.Now().Add(-time.Minute).Unix() }),
		"an issuer that differs": env.logoutToken(t, "u4", func(c map[string]any) { c["iss"] = env.httpSrv.URL + "/" }),
	}
	for name, tok := range tokens {
		wantStatus(t, name, holdToken(t, h, tok), http.StatusServiceUnavailable)
	}
	p.recover()
	if err := p.auth.Discover(p.ctx); err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if p.rev.cutCalls != 0 {
		t.Errorf("an unverifiable token cut %d times", p.rev.cutCalls)
	}
	if st := p.auth.LogoutQueueStats(); st.Unverifiable != 4 || st.Held != 0 || st.Applied != 0 {
		t.Errorf("stats %+v, want 4 unverifiable, none held or applied", st)
	}
	for _, ev := range p.sink.events {
		if ev.State == writoidc.LogoutUnverifiable && (ev.TokenSHA256 == "" || ev.SubUnverified == "" || ev.Problem == "") {
			t.Errorf("an unverifiable event is incomplete: %+v", ev)
		}
		for name, tok := range tokens {
			if strings.Contains(fmt.Sprintf("%+v", ev), tok) {
				t.Errorf("an event carries the raw token of %s", name)
			}
		}
	}
	for name, tok := range tokens {
		if strings.Contains(logs.String(), tok) {
			t.Errorf("a log line carries the raw token of %s", name)
		}
	}
	if !strings.Contains(logs.String(), "failed verification and was not applied") {
		t.Error("no ERROR line for the unverifiable logouts")
	}
}

// P5a: the verifier's clock is the token's receipt time, so a token that expired while the provider was away
// still verifies; one already expired at receipt does not.
func TestLogoutQueue_VerifiedAtReceiptTime(t *testing.T) {
	p := newPendingEnv(t)
	h := p.auth.BackchannelLogoutHandler(isReserved, nil)
	received := time.Now().Add(-10 * time.Minute)
	writoidc.SetLogoutClockForTest(p.auth, func() time.Time { return received })
	tok := p.env.logoutToken(t, "pairwise-1", func(c map[string]any) {
		c["iat"] = received.Unix()
		c["exp"] = received.Add(2 * time.Minute).Unix() // past by the time the provider returns
	})
	wantStatus(t, "the logout", holdToken(t, h, tok), http.StatusServiceUnavailable)
	p.recover()
	if err := p.auth.Discover(p.ctx); err != nil {
		t.Fatal(err)
	}
	if p.rev.cuts["pairwise-1"] == 0 || p.auth.LogoutQueueStats().Applied != 1 {
		t.Errorf("a token valid at receipt was not applied: cuts %v, stats %+v", p.rev.cuts, p.auth.LogoutQueueStats())
	}
}

func queueTwo(t *testing.T, p *pendingEnv) {
	t.Helper()
	h := p.auth.BackchannelLogoutHandler(isReserved, nil)
	for i := 0; i < 2; i++ {
		wantStatus(t, "queue", holdToken(t, h, p.env.tokenWithJTI(t, fmt.Sprintf("u%d", i), fmt.Sprintf("j%d", i))), http.StatusServiceUnavailable)
	}
}

func wantKept(t *testing.T, p *pendingEnv, err, want error, held int) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Errorf("Discover = %v, want %v", err, want)
	}
	if st := p.auth.LogoutQueueStats(); st.Held != held || st.Unverifiable != 0 {
		t.Errorf("stats %+v, want %d held and none unverifiable", st, held)
	}
	if !p.auth.Pending() || !writoidc.AuthenticatorClosedForTest(p.auth) {
		t.Error("the state was published although held logouts were not checked")
	}
	if n := p.sink.count(writoidc.LogoutUnverifiable); n != 0 {
		t.Errorf("%d unverifiable events for entries that were never checked", n)
	}
}

// P5b: a key endpoint that answers 503 or 429 leaves every entry queued and unchecked; the next attempt after
// it recovers applies them and publishes.
func TestLogoutQueue_JWKS503KeepsEntries(t *testing.T) {
	for _, code := range []int{503, 429} {
		p := newPendingEnv(t)
		queueTwo(t, p)
		p.recover()
		p.chaos.set(keysPath, statusRule(code))
		wantKept(t, p, p.auth.Discover(p.ctx), writoidc.ErrLogoutKeysUnavailable, 2)
		p.chaos.set(keysPath, nil)
		if err := p.auth.Discover(p.ctx); err != nil {
			t.Fatalf("%d: Discover after the keys recovered: %v", code, err)
		}
		if st := p.auth.LogoutQueueStats(); st.Applied != 2 || p.auth.Pending() {
			t.Errorf("%d: stats %+v, pending %v; want both applied and published", code, st, p.auth.Pending())
		}
	}
}

// P5c: a key endpoint that refuses the connection, and one whose 200 body does not decode, keep the entries.
func TestLogoutQueue_JWKSUnreachableKeepsEntries(t *testing.T) {
	for name, rule := range map[string]chaosRule{"refused": refuseRule, "not json": bodyRule("{not json")} {
		p := newPendingEnv(t)
		queueTwo(t, p)
		p.recover()
		p.chaos.set(keysPath, rule)
		t.Run(name, func(t *testing.T) { wantKept(t, p, p.auth.Discover(p.ctx), writoidc.ErrLogoutKeysUnavailable, 2) })
	}
}

// P5d: a drain context whose deadline passes between two entries keeps the unchecked one and takes the error
// that wraps the deadline.
func TestLogoutQueue_DeadlineKeepsEntries(t *testing.T) {
	p := newPendingEnv(t)
	queueTwo(t, p)
	p.recover()
	ctx, cancel := context.WithTimeout(p.ctx, 600*time.Millisecond)
	defer cancel()
	p.sink.onApply = func() { <-ctx.Done() } // the first cut outlasts the attempt
	err := p.auth.Discover(ctx)
	if !errors.Is(err, writoidc.ErrLogoutDrainIncomplete) || !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Discover = %v, want ErrLogoutDrainIncomplete wrapping DeadlineExceeded", err)
	}
	if st := p.auth.LogoutQueueStats(); st.Held != 1 || st.Applied != 1 || !p.auth.Pending() {
		t.Errorf("stats %+v, pending %v; want the first applied and the second kept", st, p.auth.Pending())
	}
}

// P5e: a context cancelled mid-drain keeps the unchecked entries and publishes nothing.
func TestLogoutQueue_CancelKeepsEntries(t *testing.T) {
	p := newPendingEnv(t)
	queueTwo(t, p)
	p.recover()
	ctx, cancel := context.WithCancel(p.ctx)
	p.sink.onApply = cancel
	err := p.auth.Discover(ctx)
	if !errors.Is(err, writoidc.ErrLogoutDrainIncomplete) || !errors.Is(err, context.Canceled) {
		t.Errorf("Discover = %v, want ErrLogoutDrainIncomplete wrapping Canceled", err)
	}
	if st := p.auth.LogoutQueueStats(); st.Held != 1 || !p.auth.Pending() || !writoidc.AuthenticatorClosedForTest(p.auth) {
		t.Errorf("stats %+v; want one entry kept and nothing published", st)
	}
}

// P5f: one key request per attempt, however many entries are held; junk is verified invalid and dropped.
func TestLogoutQueue_OneKeyFetchPerAttempt(t *testing.T) {
	p := newPendingEnv(t)
	h := p.auth.BackchannelLogoutHandler(isReserved, nil)
	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	for i := 0; i < writoidc.LogoutQueueCap-1; i++ {
		wantStatus(t, "junk", holdToken(t, h, p.env.forgedToken(t, other, "test-key", fmt.Sprintf("junk%d", i), fmt.Sprintf("jj%d", i))), http.StatusServiceUnavailable)
	}
	wantStatus(t, "genuine", holdToken(t, h, p.env.tokenWithJTI(t, "pairwise-1", "jg")), http.StatusServiceUnavailable)
	p.recover()
	p.sink.mu.Lock()
	p.sink.applyErr = errors.New("db down")
	p.sink.mu.Unlock()
	if err := p.auth.Discover(p.ctx); !errors.Is(err, writoidc.ErrLogoutCutFailed) {
		t.Fatalf("Discover = %v, want ErrLogoutCutFailed", err)
	}
	if n := p.chaos.count(keysPath); n != 1 {
		t.Errorf("key requests in the first attempt = %d, want exactly 1", n)
	}
	p.sink.mu.Lock()
	p.sink.applyErr = nil
	p.sink.mu.Unlock()
	if err := p.auth.Discover(p.ctx); err != nil {
		t.Fatal(err)
	}
	if n := p.chaos.count(keysPath); n != 2 {
		t.Errorf("key requests after the second attempt = %d, want 2", n)
	}
	if st := p.auth.LogoutQueueStats(); st.Unverifiable != writoidc.LogoutQueueCap-1 || st.Applied != 1 {
		t.Errorf("stats %+v, want %d unverifiable and one applied", st, writoidc.LogoutQueueCap-1)
	}
}

func keySetJSON(t *testing.T, keys ...jose.JSONWebKey) string {
	t.Helper()
	b, err := json.Marshal(jose.JSONWebKeySet{Keys: keys})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// P5g: a key of a type the static set cannot use, ahead of the real one, is skipped with a WARN naming it.
func TestLogoutQueue_UnsupportedKeySkipped(t *testing.T) {
	logs := captureLogs(t)
	p := newPendingEnv(t)
	queueTwo(t, p)
	p.recover()
	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	p.chaos.set(keysPath, bodyRule(keySetJSON(t,
		jose.JSONWebKey{Key: []byte("0123456789abcdef0123456789abcdef"), KeyID: "oct-1", Algorithm: "HS256"},
		jose.JSONWebKey{Key: &other.PublicKey, KeyID: "enc-1", Use: "enc", Algorithm: "RS256"},
		jose.JSONWebKey{Key: p.env.priv.Public(), KeyID: "test-key", Algorithm: "RS256", Use: "sig"},
	)))
	if err := p.auth.Discover(p.ctx); err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if st := p.auth.LogoutQueueStats(); st.Applied != 2 || st.Unverifiable != 0 {
		t.Errorf("stats %+v, want both applied", st)
	}
	for _, kid := range []string{"oct-1", "enc-1"} {
		if !strings.Contains(logs.String(), kid) {
			t.Errorf("no WARN naming the skipped key %q", kid)
		}
	}
}

// P5h: a key set with no usable key is "could not check", not "verified invalid".
func TestLogoutQueue_NoUsableKeyKeepsEntries(t *testing.T) {
	for name, body := range map[string]string{
		"empty":            `{"keys":[]}`,
		"only unsupported": keySetJSON(t, jose.JSONWebKey{Key: []byte("0123456789abcdef0123456789abcdef"), KeyID: "oct-1", Algorithm: "HS256"}),
	} {
		p := newPendingEnv(t)
		queueTwo(t, p)
		p.recover()
		p.chaos.set(keysPath, bodyRule(body))
		t.Run(name, func(t *testing.T) { wantKept(t, p, p.auth.Discover(p.ctx), writoidc.ErrLogoutKeysUnavailable, 2) })
	}
}

// P6: a cut that cannot be written keeps the state pending and the entry queued; the next Discover applies it.
func TestLogoutQueue_CutFailureKeepsPending(t *testing.T) {
	p := newPendingEnv(t)
	queueTwo(t, p)
	p.recover()
	p.sink.mu.Lock()
	p.sink.applyErr = errors.New("db down")
	p.sink.mu.Unlock()
	wantKept(t, p, p.auth.Discover(p.ctx), writoidc.ErrLogoutCutFailed, 2)
	p.sink.mu.Lock()
	p.sink.applyErr = nil
	p.sink.mu.Unlock()
	if err := p.auth.Discover(p.ctx); err != nil {
		t.Fatalf("the retry: %v", err)
	}
	if st := p.auth.LogoutQueueStats(); st.Applied != 2 || st.Held != 0 || p.auth.Pending() {
		t.Errorf("stats %+v, pending %v; want both applied and published", st, p.auth.Pending())
	}
}

type safeRev struct {
	mu   sync.Mutex
	cuts map[string]int
}

func (s *safeRev) IsSessionRevoked(context.Context, string, string, time.Time) (bool, error) {
	return false, nil
}
func (s *safeRev) RevokeSub(context.Context, string) error { return nil }
func (s *safeRev) RevokeAll(context.Context) error         { return nil }
func (s *safeRev) CutSessions(_ context.Context, sub string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cuts == nil {
		s.cuts = map[string]int{}
	}
	s.cuts[sub]++
	return nil
}

// P7: concurrent back-channel POSTs and Discover under -race: each POST is either held and later applied, or
// answered by the discovered handler, and none is lost.
func TestLogoutQueue_DrainPublishRace(t *testing.T) {
	env := newIdPEnv(t)
	chaos := newChaos(env)
	chaos.set(discoveryPath, refuseRule)
	ctx := gooidcCtx(chaos)
	rev := &safeRev{}
	a, err := writoidc.NewPending(writoidc.Config{
		IssuerURL: env.httpSrv.URL, ClientID: env.clientID, RedirectURL: "http://localhost/auth/callback",
		Revocations: rev, BackchannelLogout: true,
	}, testHMACKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Discover(ctx); !errors.Is(err, writoidc.ErrProviderUnreachable) {
		t.Fatal(err)
	}
	h := a.BackchannelLogoutHandler(isReserved, nil)
	const n = 40
	tokens := make([]string, n)
	for i := range tokens {
		tokens[i] = env.tokenWithJTI(t, fmt.Sprintf("racer%d", i), fmt.Sprintf("jr%d", i))
	}
	codes := make([]int, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range tokens {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			codes[i] = postLogout(h, tokens[i]).Code
		}()
	}
	chaos.set(discoveryPath, nil)
	close(start)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for a.Discover(ctx) != nil {
			time.Sleep(time.Millisecond)
		}
	}()
	wg.Wait()
	if err := a.Discover(ctx); err != nil {
		t.Fatal(err)
	}
	for i, code := range codes {
		sub := fmt.Sprintf("racer%d", i)
		if code != http.StatusOK && code != http.StatusServiceUnavailable {
			t.Errorf("token %d answered %d", i, code)
		}
		if rev.cuts[sub] != 1 {
			t.Errorf("token %d (answered %d) cut %d times, want exactly once", i, code, rev.cuts[sub])
		}
	}
	if st := a.LogoutQueueStats(); st.Held != 0 {
		t.Errorf("stats %+v, want an empty queue", st)
	}
}

// P7: a handler that read "not published", then lost the race to the final drain and publish, serves
// its token on the discovered path instead of queueing it behind a drain that is over.
func TestLogoutQueue_EnqueueRechecksUnderMutex(t *testing.T) {
	p := newPendingEnv(t)
	h := p.auth.BackchannelLogoutHandler(isReserved, nil)
	p.recover()
	writoidc.SetBeforeHoldHookForTest(p.auth, func() {
		if err := p.auth.Discover(p.ctx); err != nil {
			t.Errorf("Discover in the hook: %v", err)
		}
	})
	w := holdToken(t, h, p.env.logoutToken(t, "pairwise-1", nil))
	wantStatus(t, "a token that lost the race to the publish", w, http.StatusOK)
	if p.rev.cuts["pairwise-1"] == 0 || p.auth.LogoutQueueStats().Held != 0 {
		t.Errorf("cuts %v, stats %+v; want the discovered path to have cut and nothing queued", p.rev.cuts, p.auth.LogoutQueueStats())
	}
}

// P4b (unit half): while the limiter refuses, the count and the once-a-minute summary: 50 refusals in a minute
// give no summary until the minute passes, one then, and the rest at the publish.
func TestLogoutQueue_RateLimitedSummaryOncePerMinute(t *testing.T) {
	logs := captureLogs(t)
	p := newPendingEnv(t)
	now := time.Now()
	writoidc.SetLogoutClockForTest(p.auth, func() time.Time { return now })
	for i := 0; i < 50; i++ {
		p.auth.NoteLogoutRateLimited(context.Background())
	}
	// The first refusal is summarised at once; the other 49 wait for the minute.
	if p.sink.count(writoidc.LogoutRateLimitedSummary) != 1 || p.sink.events[0].Count != 1 {
		t.Fatalf("events %+v, want one summary of 1 inside the first minute", p.sink.events)
	}
	now = now.Add(61 * time.Second)
	p.auth.NoteLogoutRateLimited(context.Background())
	if p.sink.count(writoidc.LogoutRateLimitedSummary) != 2 || p.sink.events[1].Count != 50 {
		t.Fatalf("events %+v, want a second summary of 50", p.sink.events)
	}
	p.auth.NoteLogoutRateLimited(context.Background())
	p.recover()
	if err := p.auth.Discover(p.ctx); err != nil {
		t.Fatal(err)
	}
	if p.sink.count(writoidc.LogoutRateLimitedSummary) != 3 || p.sink.events[2].Count != 1 {
		t.Errorf("events %+v, want a third summary of 1 at the publish", p.sink.events)
	}
	if got := strings.Count(logs.String(), "refused as rate limited"); got != 3 {
		t.Errorf("ERROR lines = %d, want one per summary row (3)", got)
	}
	if st := p.auth.LogoutQueueStats(); st.RateLimited != 52 {
		t.Errorf("RateLimited = %d, want 52", st.RateLimited)
	}
}

// pendingInflow posts one more genuine logout through h for each logout the drain applies while the state is
// still unpublished, as a steady inflow of well-formed tokens would; it returns how many it posted.
func pendingInflow(t *testing.T, p *pendingEnv, h http.HandlerFunc, perApply int) (applied *int, posted *int) {
	t.Helper()
	applied, posted = new(int), new(int)
	p.sink.onApply = func() {
		if !p.auth.Pending() {
			return
		}
		*applied++
		for range perApply {
			*posted++
			wantStatus(t, "an arrival during the drain", holdToken(t, h,
				p.env.tokenWithJTI(t, fmt.Sprintf("late-%d", *posted), fmt.Sprintf("jl%d", *posted))), http.StatusServiceUnavailable)
		}
	}
	return applied, posted
}

// Logouts that keep arriving during the drain do not hold up the publish: the drain checks the entries held
// when it fetched the keys, publishes, and then checks and applies the late ones.
func TestLogoutQueue_InflowDuringDrainPublishes(t *testing.T) {
	p := newPendingEnv(t)
	h := p.auth.BackchannelLogoutHandler(isReserved, nil)
	wantStatus(t, "a logout while pending", holdToken(t, h, p.env.tokenWithJTI(t, "pairwise-1", "j0")), http.StatusServiceUnavailable)
	applied, posted := pendingInflow(t, p, h, 3)
	p.recover()
	if err := p.auth.Discover(p.ctx); err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if p.auth.Pending() {
		t.Fatal("still pending after the drain: the inflow kept sign-in closed")
	}
	if *applied != 1 || *posted != 3 {
		t.Errorf("applied while pending %d, posted %d; want the snapshot of 1 applied and 3 arrivals", *applied, *posted)
	}
	st := p.auth.LogoutQueueStats()
	if st.Held != 0 || st.Applied != 4 {
		t.Errorf("stats = %+v, want the queue empty and 4 applied (1 snapshot, 3 late)", st)
	}
	p.sink.mu.Lock()
	late := p.sink.lateCharges
	p.sink.mu.Unlock()
	if late != 0 {
		t.Errorf("late charges = %d, want 0: a late logout that verifies is applied uncharged", late)
	}
}

// A late logout that fails verification is charged, and counted as rate limited when the limiter refuses; the
// publish and the genuine late logouts are unaffected.
func TestLogoutQueue_LateForgedEntryChargedAndRefused(t *testing.T) {
	p := newPendingEnv(t)
	h := p.auth.BackchannelLogoutHandler(isReserved, nil)
	wantStatus(t, "a logout while pending", holdToken(t, h, p.env.tokenWithJTI(t, "pairwise-1", "j0")), http.StatusServiceUnavailable)
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	p.sink.denyCharge = true
	n := 0
	p.sink.onApply = func() {
		if !p.auth.Pending() {
			return
		}
		for range 2 {
			n++
			wantStatus(t, "a forged arrival", holdToken(t, h, p.env.forgedToken(t, other, "test-key", fmt.Sprintf("x%d", n), fmt.Sprintf("jf%d", n))), http.StatusServiceUnavailable)
		}
		n++
		wantStatus(t, "a genuine arrival", holdToken(t, h, p.env.tokenWithJTI(t, "late-genuine", "jg")), http.StatusServiceUnavailable)
	}
	p.recover()
	if err := p.auth.Discover(p.ctx); err != nil {
		t.Fatalf("Discover: %v", err)
	}
	st := p.auth.LogoutQueueStats()
	if p.auth.Pending() || st.Held != 0 || st.Applied != 2 || st.RateLimited != 2 || st.Unverifiable != 0 {
		t.Errorf("pending %v, stats %+v; want published, empty, 2 applied (snapshot and the genuine late one), 2 forged rate limited", p.auth.Pending(), st)
	}
}

// A burst of genuine logouts that arrive during the drain is applied whole, even when the limiter would refuse
// every charge: none is charged.
func TestLogoutQueue_TwentyGenuineLateAllApplied(t *testing.T) {
	p := newPendingEnv(t)
	h := p.auth.BackchannelLogoutHandler(isReserved, nil)
	wantStatus(t, "a logout while pending", holdToken(t, h, p.env.tokenWithJTI(t, "pairwise-1", "j0")), http.StatusServiceUnavailable)
	p.sink.denyCharge = true
	pendingInflow(t, p, h, 20)
	p.recover()
	if err := p.auth.Discover(p.ctx); err != nil {
		t.Fatalf("Discover: %v", err)
	}
	st := p.auth.LogoutQueueStats()
	if p.auth.Pending() || st.Held != 0 || st.Applied != 21 || st.RateLimited != 0 {
		t.Errorf("pending %v, stats %+v; want published, empty, 21 applied and none rate limited", p.auth.Pending(), st)
	}
}

// A late logout whose cut fails once is kept after the publish and applied when Discover runs again.
func TestLogoutQueue_LateCutFailsThenApplied(t *testing.T) {
	p := newPendingEnv(t)
	h := p.auth.BackchannelLogoutHandler(isReserved, nil)
	wantStatus(t, "a logout while pending", holdToken(t, h, p.env.tokenWithJTI(t, "pairwise-1", "j0")), http.StatusServiceUnavailable)
	injected := false
	p.sink.onApply = func() {
		if injected || !p.auth.Pending() {
			return
		}
		injected = true
		wantStatus(t, "an arrival during the drain", holdToken(t, h, p.env.tokenWithJTI(t, "late-1", "jl1")), http.StatusServiceUnavailable)
		p.sink.mu.Lock()
		p.sink.applyErr = errors.New("database unavailable")
		p.sink.mu.Unlock()
	}
	p.recover()
	err := p.auth.Discover(p.ctx)
	if !errors.Is(err, writoidc.ErrLogoutCutFailed) {
		t.Fatalf("Discover = %v, want ErrLogoutCutFailed for the late cut", err)
	}
	if st := p.auth.LogoutQueueStats(); p.auth.Pending() || st.Held != 1 || st.Applied != 1 {
		t.Fatalf("pending %v, stats %+v; want published with the failed late logout still held", p.auth.Pending(), st)
	}
	p.sink.mu.Lock()
	p.sink.applyErr = nil
	p.sink.mu.Unlock()
	if err := p.auth.Discover(p.ctx); err != nil {
		t.Fatalf("Discover again: %v", err)
	}
	if st := p.auth.LogoutQueueStats(); st.Held != 0 || st.Applied != 2 {
		t.Errorf("stats = %+v, want the late logout applied on the retry", st)
	}
}

// A stop during the late loop leaves the unfinished logouts held and reports each as lost at shutdown.
func TestLogoutQueue_StopDuringLateLoopReportsLost(t *testing.T) {
	logs := captureLogs(t)
	p := newPendingEnv(t)
	h := p.auth.BackchannelLogoutHandler(isReserved, nil)
	wantStatus(t, "a logout while pending", holdToken(t, h, p.env.tokenWithJTI(t, "pairwise-1", "j0")), http.StatusServiceUnavailable)
	ctx, cancel := context.WithCancel(p.ctx)
	defer cancel()
	applies := 0
	p.sink.onApply = func() {
		applies++
		switch applies {
		case 1: // the snapshot entry: two arrive during the drain
			for i := range 2 {
				wantStatus(t, "an arrival during the drain", holdToken(t, h, p.env.tokenWithJTI(t, fmt.Sprintf("late-%d", i), fmt.Sprintf("jl%d", i))), http.StatusServiceUnavailable)
			}
		case 2: // the first late entry: the process is stopping
			cancel()
		}
	}
	p.recover()
	err := p.auth.Discover(ctx)
	if !errors.Is(err, writoidc.ErrLogoutDrainIncomplete) {
		t.Fatalf("Discover = %v, want ErrLogoutDrainIncomplete", err)
	}
	if st := p.auth.LogoutQueueStats(); st.Held != 1 || st.Applied != 2 {
		t.Fatalf("stats = %+v, want the first late logout applied despite the stop and one still held", st)
	}
	writoidc.ReportLogoutsLostAtShutdown(p.auth)
	if p.sink.count(writoidc.LogoutLostAtShutdown) != 1 || !strings.Contains(logs.String(), "was never applied") {
		t.Errorf("events %v, want one lost_at_shutdown for the unfinished late logout", p.sink.states())
	}
}

// A request that reaches the discovered path is charged through the sink, and answered 429 when it refuses.
func TestBackchannelHandler_DiscoveredPathCharges(t *testing.T) {
	p := newPendingEnv(t)
	h := p.auth.BackchannelLogoutHandler(isReserved, nil)
	p.recover()
	if err := p.auth.Discover(p.ctx); err != nil {
		t.Fatalf("Discover: %v", err)
	}
	wantStatus(t, "a logout once discovered", holdToken(t, h, p.env.tokenWithJTI(t, "pairwise-1", "j1")), http.StatusOK)
	if p.sink.charges != 1 {
		t.Errorf("charges = %d, want 1", p.sink.charges)
	}
	p.sink.denyCharge = true
	w := holdToken(t, h, p.env.tokenWithJTI(t, "pairwise-2", "j2"))
	wantStatus(t, "a refused charge", w, http.StatusTooManyRequests)
	if w.Header().Get("Retry-After") == "" {
		t.Error("the 429 carries no Retry-After")
	}
}

// A nil reserved func is an accepted argument: draining a genuine held logout through it does not panic.
func TestLogoutQueue_NilReservedFuncDrainsWithoutPanic(t *testing.T) {
	p := newPendingEnv(t)
	h := p.auth.BackchannelLogoutHandler(nil, nil)
	wantStatus(t, "a logout while pending", holdToken(t, h, p.env.logoutToken(t, "pairwise-1", nil)), http.StatusServiceUnavailable)
	p.recover()
	if err := p.auth.Discover(p.ctx); err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if st := p.auth.LogoutQueueStats(); st.Applied != 1 || st.Held != 0 {
		t.Errorf("stats = %+v, want the logout applied", st)
	}
}

// Under a split-horizon issuer the internal discovery document may name another issuer than the public one the
// provider puts in its tokens: a genuine held logout is still applied, not dropped as unverifiable.
func TestLogoutQueue_SplitHorizonIssuerApplied(t *testing.T) {
	const public = "https://idp.public.example"
	p := newPendingEnv(t, func(c *writoidc.Config) {
		c.InternalIssuerURL = c.IssuerURL // the test provider's own URL; its document names that issuer
		c.IssuerURL = public
	})
	h := p.auth.BackchannelLogoutHandler(isReserved, nil)
	tok := p.env.logoutToken(t, "pairwise-1", func(c map[string]any) { c["iss"] = public })
	wantStatus(t, "a logout while pending", holdToken(t, h, tok), http.StatusServiceUnavailable)
	p.recover()
	if err := p.auth.Discover(p.ctx); err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if st := p.auth.LogoutQueueStats(); st.Applied != 1 || st.Unverifiable != 0 || st.Held != 0 {
		t.Errorf("stats = %+v, want the genuine logout applied and none unverifiable", st)
	}
	if len(p.sink.applied) != 1 || !strings.HasPrefix(p.sink.applied[0], "pairwise-1:") {
		t.Errorf("applied %v, want [pairwise-1]", p.sink.applied)
	}
}

// A clean stop with logouts still held records each one best effort, and the remaining rate-limit count.
func TestLogoutQueue_LostAtShutdownRecorded(t *testing.T) {
	logs := captureLogs(t)
	p := newPendingEnv(t)
	queueTwo(t, p)
	p.auth.NoteLogoutRateLimited(context.Background())
	writoidc.ReportLogoutsLostAtShutdown(p.auth)
	if p.sink.count(writoidc.LogoutLostAtShutdown) != 2 || p.sink.count(writoidc.LogoutRateLimitedSummary) != 1 {
		t.Errorf("events %v, want 2 lost_at_shutdown and the remaining summary", p.sink.states())
	}
	if strings.Count(logs.String(), "was never applied") != 2 {
		t.Errorf("want one ERROR line per held logout:\n%s", logs.String())
	}
}
