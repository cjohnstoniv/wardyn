// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// residentMintProxy is a proxy whose clock the test sets, holding mints, with a
// control plane that counts the mints relayed to it.
func residentMintProxy(t *testing.T, now *atomic.Int64, mints []ResidentMint, brokered []uuid.UUID) (*Proxy, *bytes.Buffer, *atomic.Int32) {
	t.Helper()
	var cpCalls atomic.Int32
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		cpCalls.Add(1)
		_, _ = w.Write([]byte(`{"kind":"git_pat","token":"relayed"}`))
	}))
	t.Cleanup(cp.Close)
	buf := &bytes.Buffer{}
	p := newProxy(Options{
		RunID:               uuid.New(),
		Policy:              CompilePolicy(types.RunPolicySpec{}),
		Sink:                &decisionSink{out: buf, ch: make(chan egress.DecisionLog, 64)},
		Resolver:            publicResolver{},
		Dial:                redirectDial(upstreamAddr(cp)),
		ControlPlaneURL:     "http://wardynd.test:8080",
		RunToken:            newTokenSource("RUNTOK"),
		BrokeredPATGrantIDs: brokered,
		Now:                 func() time.Time { return time.Unix(0, now.Load()).UTC() },
	})
	p.residentMints = residentMintsByGrant(mints)
	return p, buf, &cpCalls
}

func residentMintCall(t *testing.T, p *Proxy, grant uuid.UUID) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, mustLocalReq(t, http.MethodPost, routeMint, strings.NewReader(`{"grant_id":"`+grant.String()+`"}`)))
	return rec
}

// A runner_resident ssh_key or git_pat mint is answered by the proxy from the
// runner-held value, never relayed, and only before its ExpiresAt: at it, after
// it, or with no ExpiresAt at all, the answer is a 403 and a mint deny. The
// answer carries ExpiresAt, so a helper's cache lapses with it.
func TestBrokerMint_ResidentAnswerRefusedAtExpiry(t *testing.T) {
	var now atomic.Int64
	start := time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC)
	now.Store(start.UnixNano())
	ssh, pat, zero := uuid.New(), uuid.New(), uuid.New()
	expires := start.Add(2 * time.Hour)
	p, buf, cpCalls := residentMintProxy(t, &now, []ResidentMint{
		{GrantID: ssh, Kind: types.GrantSSHKey, Username: "git", KnownHosts: "ssh.example.test ssh-ed25519 AAAA", Token: "ssh-key-value", ExpiresAt: expires},
		{GrantID: pat, Kind: types.GrantGitPAT, Username: "pat", Token: "pat-value", ExpiresAt: expires},
		{GrantID: zero, Kind: types.GrantGitPAT, Username: "oauth2", Token: "no-expiry-value"},
	}, nil)

	for _, tc := range []struct {
		grant              uuid.UUID
		kind               types.GrantKind
		token, user, hosts string
	}{
		{ssh, types.GrantSSHKey, "ssh-key-value", "git", "ssh.example.test ssh-ed25519 AAAA"},
		{pat, types.GrantGitPAT, "pat-value", "pat", ""},
	} {
		rec := residentMintCall(t, p, tc.grant)
		var got struct {
			Kind       types.GrantKind `json:"kind"`
			Token      string          `json:"token"`
			Username   string          `json:"username"`
			ExpiresAt  string          `json:"expires_at"`
			KnownHosts string          `json:"known_hosts"`
		}
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &got) != nil {
			t.Fatalf("%s before expiry: status %d body %q, want a local 200", tc.kind, rec.Code, rec.Body.String())
		}
		if got.Kind != tc.kind || got.Token != tc.token || got.Username != tc.user || got.KnownHosts != tc.hosts || got.ExpiresAt != expires.Format(time.RFC3339) {
			t.Fatalf("%s answer = %+v", tc.kind, got)
		}
		if d := lastDecision(t, buf); d.RuleSource != ruleSourceMint || d.Decision != egress.Allow {
			t.Fatalf("%s decision = %+v, want a mint allow", tc.kind, d)
		}
	}

	refused := func(when string, grant uuid.UUID) {
		t.Helper()
		rec := residentMintCall(t, p, grant)
		if rec.Code != http.StatusForbidden || strings.Contains(rec.Body.String(), "value") {
			t.Fatalf("%s: status %d body %q, want a 403 with no value", when, rec.Code, rec.Body.String())
		}
		if d := lastDecision(t, buf); d.RuleSource != ruleSourceMint || d.Decision != egress.Deny {
			t.Fatalf("%s: decision = %+v, want a mint deny", when, d)
		}
	}
	refused("no ExpiresAt", zero)
	now.Store(expires.Add(-time.Nanosecond).UnixNano())
	if rec := residentMintCall(t, p, pat); rec.Code != http.StatusOK {
		t.Fatalf("a nanosecond before expiry: status %d, want 200", rec.Code)
	}
	now.Store(expires.UnixNano())
	refused("at expiry", pat)
	refused("at expiry", ssh)
	now.Store(expires.Add(time.Hour).UnixNano())
	refused("after expiry", pat)
	if n := cpCalls.Load(); n != 0 {
		t.Fatalf("%d resident mints reached the control plane, want none", n)
	}
	if rec := residentMintCall(t, p, uuid.New()); rec.Code != http.StatusOK || cpCalls.Load() != 1 {
		t.Fatalf("a grant outside the resident set: status %d, relays %d, want one relay", rec.Code, cpCalls.Load())
	}
}

// The brokered refusals stay first: a grant the PAT broker holds is refused as
// brokered even when a resident answer for it is configured.
func TestBrokerMint_BrokeredRefusalPrecedesResidentAnswer(t *testing.T) {
	var now atomic.Int64
	now.Store(time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC).UnixNano())
	g := uuid.New()
	p, buf, cpCalls := residentMintProxy(t, &now, []ResidentMint{
		{GrantID: g, Kind: types.GrantGitPAT, Token: "pat-value", ExpiresAt: time.Unix(0, now.Load()).Add(time.Hour)},
	}, []uuid.UUID{g})
	rec := residentMintCall(t, p, g)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), routePATBroker) || strings.Contains(rec.Body.String(), "pat-value") {
		t.Fatalf("status %d body %q, want the brokered-PAT refusal", rec.Code, rec.Body.String())
	}
	if d := lastDecision(t, buf); d.Decision != egress.Deny || cpCalls.Load() != 0 {
		t.Fatalf("decision %+v, relays %d, want a deny and no relay", d, cpCalls.Load())
	}
}

// A resident_mints entry needs a grant, a kind the proxy answers, and a grant
// named once.
func TestValidResidentMints(t *testing.T) {
	g := uuid.New()
	for _, tc := range []struct {
		name string
		ms   []ResidentMint
		ok   bool
	}{
		{"ssh and pat", []ResidentMint{{GrantID: g, Kind: types.GrantSSHKey}, {GrantID: uuid.New(), Kind: types.GrantGitPAT}}, true},
		{"no grant", []ResidentMint{{Kind: types.GrantSSHKey}}, false},
		{"an env_secret", []ResidentMint{{GrantID: g, Kind: types.GrantEnvSecret}}, false},
		{"a grant twice", []ResidentMint{{GrantID: g, Kind: types.GrantSSHKey}, {GrantID: g, Kind: types.GrantGitPAT}}, false},
	} {
		if err := validResidentMints(tc.ms); (err == nil) != tc.ok {
			t.Errorf("%s: err = %v, want ok=%v", tc.name, err, tc.ok)
		}
	}
}
