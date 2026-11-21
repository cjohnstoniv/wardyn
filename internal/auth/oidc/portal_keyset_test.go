// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package oidc

import (
	"strings"
	"sync"
	"testing"
	"time"
)

// warmPortal admits one token so discovery and the first key fetch have happened, and returns the fetch counts.
func warmPortal(h *portalHarness) (disc, keys int) {
	h.t.Helper()
	h.mustAdmit(h.token(h.accessClaims()))
	return h.idp.hits()
}

func (h *portalHarness) kidToken(kid string, idx int) string {
	return h.mint(kid, idx, h.accessClaims(), nil)
}

func (h *portalHarness) wantKeyHits(want int) {
	h.t.Helper()
	if _, got := h.idp.hits(); got != want {
		h.t.Fatalf("%d key fetches, want %d", got, want)
	}
}

func TestPortalKeySet_OutcomeTable(t *testing.T) {
	cooldown := portalKeyCooldown + time.Second
	t.Run("known kid, good signature: verify", func(t *testing.T) {
		h := newPortalHarness(t, "example.com", true, "/oauth2/a")
		h.mustAdmit(h.token(h.accessClaims()))
		h.mustAdmit(h.token(h.accessClaims()))
		h.wantKeyHits(1)
	})
	t.Run("known kid, bad signature: 400 and no fetch", func(t *testing.T) {
		h := newPortalHarness(t, "example.com", true, "/oauth2/a")
		_, keys := warmPortal(h)
		h.clock.Advance(cooldown)
		r := h.wantRefusal(h.kidToken("k1", 1), SubjectTokenInvalid)
		if r.Class != PortalRefusedToken {
			t.Fatalf("class = %v", r.Class)
		}
		h.wantKeyHits(keys)
	})
	t.Run("unknown kid, fetch succeeds, kid found: verify", func(t *testing.T) {
		h := newPortalHarness(t, "example.com", true, "/oauth2/a")
		_, keys := warmPortal(h)
		h.idp.publish(t, "k2", 1)
		h.clock.Advance(cooldown)
		h.mustAdmit(h.kidToken("k2", 1))
		h.wantKeyHits(keys + 1)
	})
	t.Run("unknown kid, fetch succeeds, kid absent: 400", func(t *testing.T) {
		h := newPortalHarness(t, "example.com", true, "/oauth2/a")
		_, keys := warmPortal(h)
		h.clock.Advance(cooldown)
		r := h.wantRefusal(h.kidToken("k9", 1), SubjectTokenInvalid)
		if r.Class != PortalRefusedToken {
			t.Fatalf("an absent kid after a successful fetch answered class %v", r.Class)
		}
		h.wantKeyHits(keys + 1)
	})
	t.Run("kid removed by a successful fetch: 400 at once, inside the cooldown", func(t *testing.T) {
		h := newPortalHarness(t, "example.com", true, "/oauth2/a")
		h.idp.publish(t, "k2", 1)
		warmPortal(h)
		h.mustAdmit(h.kidToken("k2", 1))
		h.idp.unpublish("k2")
		h.clock.Advance(cooldown)
		h.wantRefusal(h.kidToken("k9", 1), SubjectTokenInvalid) // this fetch sees k2 gone
		_, keys := h.idp.hits()
		h.wantRefusal(h.kidToken("k2", 1), SubjectTokenInvalid) // no grace, no second fetch
		h.wantKeyHits(keys)
	})
	t.Run("never-seen kid inside the cooldown: 503 with Retry-After", func(t *testing.T) {
		h := newPortalHarness(t, "example.com", true, "/oauth2/a")
		_, keys := warmPortal(h)
		h.clock.Advance(10 * time.Second)
		r := h.wantUnavailable(h.kidToken("k9", 1), portalCauseKeysCooldown)
		if r.RetryAfter != 20*time.Second {
			t.Fatalf("Retry-After = %v, want the rest of the cooldown, 20s", r.RetryAfter)
		}
		h.wantKeyHits(keys)
	})
	t.Run("key fetch fails: 503, then every token 503", func(t *testing.T) {
		h := newPortalHarness(t, "example.com", true, "/oauth2/a")
		warmPortal(h)
		h.clock.Advance(cooldown)
		h.idp.set(func(f *fakeIdP) { f.keyStatus = 500 })
		h.wantUnavailable(h.kidToken("k9", 1), portalCauseKeysUnreachable)
		h.wantUnavailable(h.token(h.accessClaims()), portalCauseIssuerDown) // a known kid too
	})
	t.Run("discovery fails at build: 503, negative-cached 30 s", func(t *testing.T) {
		h := newPortalHarness(t, "example.com", true, "/oauth2/a")
		h.idp.set(func(f *fakeIdP) { f.discStatus = 500 })
		h.wantUnavailable(h.token(h.accessClaims()), portalCauseUnreachable)
		h.clock.Advance(10 * time.Second)
		h.wantUnavailable(h.token(h.accessClaims()), portalCauseUnreachable)
		if disc, _ := h.idp.hits(); disc != 1 {
			t.Fatalf("%d discovery fetches inside the negative cache, want 1", disc)
		}
		h.idp.set(func(f *fakeIdP) { f.discStatus = 0 })
		h.clock.Advance(cooldown)
		h.mustAdmit(h.token(h.accessClaims()))
	})
}

func TestPortalKeySet_RotationByUnknownKid(t *testing.T) {
	h := newPortalHarness(t, "example.com", true, "/oauth2/a")
	_, keys := warmPortal(h)
	h.idp.publish(t, "k2", 1)
	h.clock.Advance(portalKeyCooldown + time.Second)
	h.mustAdmit(h.kidToken("k2", 1))
	h.mustAdmit(h.kidToken("k2", 1))
	h.mustAdmit(h.kidToken("k1", 0))
	h.wantKeyHits(keys + 1) // one fetch for the new kid, none for the next uses
	// The provider drops k1 and adds k3: the fetch for k3 replaces the set, and k1 is refused from then on.
	h.idp.unpublish("k1")
	h.idp.publish(t, "k3", 2)
	h.clock.Advance(portalKeyCooldown + time.Second)
	h.mustAdmit(h.kidToken("k3", 2))
	h.wantRefusal(h.kidToken("k1", 0), SubjectTokenInvalid)
	h.wantKeyHits(keys + 2)
}

func TestPortalKeySet_BadSignatureNeverFetches(t *testing.T) {
	h := newPortalHarness(t, "example.com", true, "/oauth2/a")
	disc, keys := warmPortal(h)
	good := h.token(h.accessClaims())
	parts := strings.Split(good, ".")
	flipped := parts[2][:len(parts[2])-4] + "AAAA"
	other := h.accessClaims()
	other["uid"] = "00uOTHER"
	otherPayload := strings.Split(h.mint("k1", 1, other, nil), ".")[1]
	for name, tok := range map[string]string{
		"another key under the known kid": h.kidToken("k1", 1),
		"tampered signature":              parts[0] + "." + parts[1] + "." + flipped,
		"payload swapped":                 parts[0] + "." + otherPayload + "." + parts[2],
	} {
		t.Run(name, func(t *testing.T) {
			h.wantRefusal(tok, SubjectTokenInvalid)
			h.clock.Advance(time.Minute) // far past any cooldown: still no fetch
		})
	}
	if d, k := h.idp.hits(); d != disc || k != keys {
		t.Fatalf("fetches went from (%d,%d) to (%d,%d) on bad signatures", disc, keys, d, k)
	}
}

func TestPortalKeySet_OneFetchPer30sUnderConcurrency(t *testing.T) {
	h := newPortalHarness(t, "example.com", true, "/oauth2/a")
	_, keys := warmPortal(h)
	run := func(kid string, idx, n int) []PortalRefusal {
		toks := make([]string, n)
		for i := range toks {
			toks[i] = h.kidToken(kid, idx)
		}
		out := make([]PortalRefusal, n)
		var wg sync.WaitGroup
		for i := range toks {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, out[i] = h.verify(toks[i])
			}()
		}
		wg.Wait()
		return out
	}
	h.idp.set(func(f *fakeIdP) { f.keyDelay = 150 * time.Millisecond })
	// A burst for a key the provider has just published: one fetch, every request verified.
	h.idp.publish(t, "k2", 1)
	h.clock.Advance(portalKeyCooldown + time.Second)
	for i, r := range run("k2", 1, 25) {
		if !r.OK() {
			t.Fatalf("request %d: %+v", i, r)
		}
	}
	h.wantKeyHits(keys + 1)
	// A burst for a kid that does not exist: one fetch, and every request waits for it and gets its answer.
	h.clock.Advance(portalKeyCooldown + time.Second)
	for i, r := range run("k9", 1, 25) {
		if r.Reason != SubjectTokenInvalid {
			t.Fatalf("request %d: %+v, want the shared fetch's answer", i, r)
		}
	}
	h.wantKeyHits(keys + 2)
	// Inside the cooldown a burst makes no fetch at all.
	for i, r := range run("k8", 1, 25) {
		if r.Reason != PortalIssuerUnavailable || r.Cause != portalCauseKeysCooldown {
			t.Fatalf("request %d: %+v", i, r)
		}
	}
	h.wantKeyHits(keys + 2)
}

func TestPortalKeySet_ClockIntervals(t *testing.T) {
	h := newPortalHarness(t, "example.com", true, "/oauth2/a")
	disc, keys := warmPortal(h)
	v, _ := h.pi.verifierFor(h.reg)
	okAt, _, _ := v.ks.state()

	// Inside the hour nothing is fetched.
	h.clock.Advance(59 * time.Minute)
	h.mustAdmit(h.token(h.accessClaims()))
	if d, k := h.idp.hits(); d != disc || k != keys {
		t.Fatalf("fetched inside the hour: (%d,%d)", d, k)
	}
	// At the hour, discovery and keys are re-read together.
	h.clock.Advance(time.Minute)
	h.mustAdmit(h.token(h.accessClaims()))
	if d, k := h.idp.hits(); d != disc+1 || k != keys+1 {
		t.Fatalf("hourly refresh made (%d,%d) fetches, want (%d,%d)", d, k, disc+1, keys+1)
	}
	disc, keys = disc+1, keys+1
	okAt, _, _ = v.ks.state()

	// A failed discovery at the next hour turns a known kid into 503 until a fetch succeeds.
	h.clock.Advance(time.Hour)
	h.idp.set(func(f *fakeIdP) { f.discStatus = 500 })
	h.wantUnavailable(h.token(h.accessClaims()), portalCauseUnreachable)
	if got, _, _ := v.ks.state(); !got.Equal(okAt) {
		t.Fatal("a failed fetch moved keys_ok_at")
	}
	h.clock.Advance(10 * time.Second)
	h.wantUnavailable(h.token(h.accessClaims()), portalCauseIssuerDown) // no retry sooner than 30 s
	if d, _ := h.idp.hits(); d != disc+1 {
		t.Fatalf("%d discovery fetches, want the one failed attempt", d-disc)
	}
	h.clock.Advance(25 * time.Second)
	h.wantUnavailable(h.token(h.accessClaims()), portalCauseUnreachable) // the retry fails too
	if d, _ := h.idp.hits(); d != disc+2 {
		t.Fatalf("%d discovery fetches, want a retry after 30 s", d-disc)
	}
	h.idp.set(func(f *fakeIdP) { f.discStatus = 0 })
	h.clock.Advance(31 * time.Second)
	h.mustAdmit(h.token(h.accessClaims()))
	if _, k := h.idp.hits(); k != keys+1 {
		t.Fatalf("%d key fetches after recovery, want the one that came with discovery", k-keys)
	}
}

func TestPortalKeySet_FailClosedAfterFailedRefresh(t *testing.T) {
	// A failed hourly key fetch turns a known kid into 503 until the next success; discovery alone failing does
	// the same (the clock test); neither leaves a grace period.
	h := newPortalHarness(t, "example.com", true, "/oauth2/a")
	warmPortal(h)
	h.clock.Advance(time.Hour)
	h.idp.set(func(f *fakeIdP) { f.keyStatus = 500 })
	h.wantUnavailable(h.token(h.accessClaims()), portalCauseKeysUnreachable)
	for _, d := range []time.Duration{5 * time.Second, 10 * time.Second} {
		h.clock.Advance(d)
		h.wantUnavailable(h.token(h.accessClaims()), portalCauseIssuerDown)
	}
	h.clock.Advance(time.Hour * 23) // a day later there is still no grace
	h.wantUnavailable(h.token(h.accessClaims()), portalCauseKeysUnreachable)
	h.idp.set(func(f *fakeIdP) { f.keyStatus = 0 })
	h.clock.Advance(31 * time.Second)
	h.mustAdmit(h.token(h.accessClaims()))
}

func TestPortalKeySet_RemovedVersusNeverSeenInCooldown(t *testing.T) {
	h := newPortalHarness(t, "example.com", true, "/oauth2/a")
	h.idp.publish(t, "k2", 1)
	warmPortal(h)
	h.mustAdmit(h.kidToken("k2", 1))
	h.idp.unpublish("k2")
	h.clock.Advance(portalKeyCooldown + time.Second)
	h.mustAdmit(h.token(h.accessClaims())) // not an unknown kid: no fetch yet, k2 still cached
	h.wantRefusal(h.kidToken("k9", 1), SubjectTokenInvalid)
	_, keys := h.idp.hits()
	h.clock.Advance(10 * time.Second)
	// k2 was removed by a successful fetch: 400, even inside the cooldown. k8 was never seen: 503.
	h.wantRefusal(h.kidToken("k2", 1), SubjectTokenInvalid)
	r := h.wantUnavailable(h.kidToken("k8", 1), portalCauseKeysCooldown)
	if r.RetryAfter != 20*time.Second {
		t.Fatalf("Retry-After = %v", r.RetryAfter)
	}
	h.wantKeyHits(keys)
	// Once the cooldown ends, the never-seen kid is looked for, and not found.
	h.clock.Advance(21 * time.Second)
	h.wantRefusal(h.kidToken("k8", 1), SubjectTokenInvalid)
	h.wantKeyHits(keys + 1)
}
