// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
)

// fakeWorkloadVerifier stands in for the issuer verifier: it refuses every portal token, counts the workload
// verifications it is asked for, and answers them with what the test set.
type fakeWorkloadVerifier struct {
	calls   atomic.Int64
	mu      sync.Mutex
	refusal oidc.PortalRefusal
	expiry  time.Time // zero: now + 5 min
}

func (f *fakeWorkloadVerifier) VerifyDetail(context.Context, oidc.PortalRegistration, string, string) (oidc.PortalSubject, oidc.PortalRefusal) {
	return oidc.PortalSubject{}, oidc.PortalRefusal{Reason: oidc.SubjectTokenInvalid, Class: oidc.PortalRefusedToken}
}

func (f *fakeWorkloadVerifier) VerifyWorkload(_ context.Context, reg oidc.WorkloadRegistration, _ string) (oidc.WorkloadSubject, oidc.PortalRefusal) {
	f.calls.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.refusal.OK() {
		return oidc.WorkloadSubject{}, f.refusal
	}
	exp := f.expiry
	if exp.IsZero() {
		exp = time.Now().Add(5 * time.Minute)
	}
	return oidc.WorkloadSubject{Issuer: reg.Issuer, Subject: reg.Subject, IssuedAt: time.Now(), Expiry: exp}, oidc.PortalRefusal{}
}

func (f *fakeWorkloadVerifier) set(refusal oidc.PortalRefusal, expiry time.Time) {
	f.mu.Lock()
	f.refusal, f.expiry = refusal, expiry
	f.mu.Unlock()
}

var (
	workloadTestKeyOnce sync.Once
	workloadTestKey     *rsa.PrivateKey
)

// workloadToken is a compact RS256 token carrying iss, aud and sub: what oidc.WorkloadCandidate reads. The fake
// verifier decides the rest, so the signature is never checked.
func workloadToken(t *testing.T, iss, aud, sub string) string {
	t.Helper()
	workloadTestKeyOnce.Do(func() {
		k, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			panic(err)
		}
		workloadTestKey = k
	})
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: workloadTestKey}, (&jose.SignerOptions{}).WithType("JWT"))
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(map[string]any{"iss": iss, "aud": aud, "sub": sub, "iat": time.Now().Unix(),
		"exp": time.Now().Add(5 * time.Minute).Unix()})
	sig, err := signer.Sign(payload)
	if err != nil {
		t.Fatal(err)
	}
	out, err := sig.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// exchangeFrom posts body to the exchange from the TCP peer peer.
func exchangeFrom(t *testing.T, srv *Server, peer, body string) *httptest.ResponseRecorder {
	t.Helper()
	return doFrom(t, srv, peer, http.MethodPost, "/api/v1/inbound/exchange", "", body)
}

// TestInboundExchange_OffByDefault: on a server with no trust store the exchange is the off answer, with no
// verification, no audit row and no way to a token row.
func TestInboundExchange_OffByDefault(t *testing.T) {
	h := newHarness(t)
	fake := &fakeWorkloadVerifier{}
	h.srv.cfg.PortalIssuers = fake
	tok := workloadToken(t, "https://idp.example.com", inboundTestOrigin, "repo:org/app")
	w := exchangeFrom(t, h.srv, "198.51.100.1:1000", `{"subject_token":"`+tok+`"}`)
	if w.Code != http.StatusNotFound || respReason(w) != reasonInboundFederationOff {
		t.Fatalf("exchange on a default server: %d %s, want 404 %s", w.Code, w.Body.String(), reasonInboundFederationOff)
	}
	if n := fake.calls.Load(); n != 0 {
		t.Errorf("verifier calls = %d, want 0", n)
	}
	for _, ev := range h.audit.snapshot() {
		if ev.Action == "inbound.exchange" {
			t.Errorf("audit row %+v, want none", ev)
		}
	}
	if _, ok := inboundTrustStore(h.srv); ok {
		t.Error("the default store holds trusts, so an exchange could write a token row")
	}
	if strings.Contains(w.Body.String(), "token\"") {
		t.Errorf("off answer %s carries a token", w.Body.String())
	}
}

// countingBody counts the reads of a request body.
type countingBody struct {
	reads atomic.Int64
	r     *strings.Reader
}

func (c *countingBody) Read(p []byte) (int, error) {
	c.reads.Add(1)
	return c.r.Read(p)
}

// TestInboundExchange_RateLimitedBeforeParse: the limiter runs before anything else, so the attempt past the burst
// is refused without its body being read; another address keeps its own bucket.
func TestInboundExchange_RateLimitedBeforeParse(t *testing.T) {
	h := newHarness(t)
	const peer = "198.51.100.7:1000"
	for i := range 10 {
		if w := exchangeFrom(t, h.srv, peer, `garbage`); w.Code == http.StatusTooManyRequests {
			t.Fatalf("attempt %d: 429 inside the burst", i+1)
		}
	}
	body := &countingBody{r: strings.NewReader(`garbage`)}
	r := httptest.NewRequest(http.MethodPost, "/api/v1/inbound/exchange", body)
	r.RemoteAddr = peer
	w := httptest.NewRecorder()
	h.srv.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") != "10" || respReason(w) != reasonInboundExchangeRateLimited {
		t.Fatalf("11th attempt: %d Retry-After %q %s, want 429 with Retry-After 10", w.Code, w.Header().Get("Retry-After"), w.Body.String())
	}
	if n := body.reads.Load(); n != 0 {
		t.Errorf("the refused attempt's body was read %d times, want 0", n)
	}
	if w := exchangeFrom(t, h.srv, "198.51.100.8:1000", `garbage`); w.Code == http.StatusTooManyRequests {
		t.Errorf("another address: 429, want its own bucket")
	}
}
