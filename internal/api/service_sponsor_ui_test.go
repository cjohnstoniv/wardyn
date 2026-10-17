// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// sponsorUIStore answers the one question the sponsor arm asks of the service store.
type sponsorUIStore struct {
	*uiMemStore
	store.ServiceIdentityStore // never called: only ServiceStillSponsored is asked
	still                      atomic.Bool
	sponsorSince               atomic.Int64 // unix seconds the current sponsorship began; 0 = always
	sponsorSinceNs             atomic.Int64 // when set, unix nanoseconds it began, compared exactly as the store does
}

// ServiceStillSponsored answers as the real store does: the sponsor is current, and the sponsorship
// did not begin after openedAt, so a re-sponsor A to B to A ends a session opened before it.
func (s *sponsorUIStore) ServiceStillSponsored(_ context.Context, _, _ string, openedAt time.Time) (bool, error) {
	if ns := s.sponsorSinceNs.Load(); ns != 0 {
		return s.still.Load() && !time.Unix(0, ns).After(openedAt), nil
	}
	return s.still.Load() && s.sponsorSince.Load() <= openedAt.Unix(), nil
}

// A service sponsored at T.200 and a ticket admitted at T.500 in the same second: the session's first relay
// is allowed, as it was at entry. The cookie carries the authority time to the nanosecond, so the re-check
// compares like the store does. A ticket admitted before the sponsorship still ends a session at its re-check.
func TestServiceIdentity_SponsorUISessionAdmittedInTheSponsorshipSecond(t *testing.T) {
	h := newUIHarness(t, okBackend())
	st := &sponsorUIStore{uiMemStore: h.store}
	st.still.Store(true)
	h.srv.cfg.Store = st
	h.run.CreatedBy = "service:ci-bot"
	h.store.putRun(h.run)
	sec := time.Now().Truncate(time.Second)
	st.sponsorSinceNs.Store(sec.Add(200 * time.Millisecond).UnixNano())
	path := uiRelayPrefix(h.run.ID, "code") + "/ide"
	enterAt := func(at time.Time) *http.Cookie {
		t.Helper()
		tok, err := mintAttachTicket(context.Background(), h.store, h.run.ID, types.ActorHuman, "sponsor-sub", oidc.RoleUser, oidc.Issue{At: at})
		if err != nil {
			t.Fatalf("mint ticket: %v", err)
		}
		rec := h.enter(url.Values{"run": {h.run.ID.String()}, "app": {"code"}, "ticket": {tok}})
		for _, c := range rec.Result().Cookies() {
			if c.Name == uiCookieName {
				return c
			}
		}
		t.Fatalf("enter at %v: %d %s, no relay cookie", at, rec.Code, rec.Body.String())
		return nil
	}
	cookie := enterAt(sec.Add(500 * time.Millisecond))
	if rec := uiGet(h, path, cookie); rec.Code != http.StatusOK {
		t.Errorf("first relay of a session admitted after sponsor_since in the same second: %d %s, want 200", rec.Code, rec.Body.String())
	}
	// Admitted at T.100, before sponsor_since T.200: refused at the entry check, so no cookie can exist; a
	// sponsorship that moves past an open session's authority time ends it at its next re-check.
	st.sponsorSinceNs.Store(sec.Add(900 * time.Millisecond).UnixNano())
	h.clock.advance(uiReassertInterval + time.Second)
	if rec := uiGet(h, path, cookie); rec.Code != http.StatusForbidden {
		t.Errorf("a session authorized before sponsor_since: %d %s, want 403", rec.Code, rec.Body.String())
	}
}

// TestServiceIdentity_SponsorUIEntry: a sponsor's member-stamped ticket opens a UI app of the service's run;
// ui.open names the sponsor as the human actor with the service identity and entry=sponsor; the session ends at
// the next re-check once the sponsor is no longer entitled; and a ticket redeemed by someone no longer entitled
// is refused outright.
func TestServiceIdentity_SponsorUIEntry(t *testing.T) {
	h := newUIHarness(t, okBackend())
	st := &sponsorUIStore{uiMemStore: h.store}
	st.still.Store(true)
	h.srv.cfg.Store = st
	h.run.CreatedBy = "service:ci-bot"
	h.store.putRun(h.run)
	enter := func() *http.Cookie {
		t.Helper()
		rec := h.enter(url.Values{"run": {h.run.ID.String()}, "app": {"code"}, "ticket": {h.ticket(h.run.ID, "sponsor-sub", oidc.RoleUser)}})
		if rec.Code != http.StatusFound {
			t.Fatalf("enter with a sponsor's ticket: %d %s, want 302", rec.Code, rec.Body.String())
		}
		for _, c := range rec.Result().Cookies() {
			if c.Name == uiCookieName {
				return c
			}
		}
		t.Fatal("no relay cookie")
		return nil
	}
	cookie := enter()
	path := uiRelayPrefix(h.run.ID, "code") + "/ide"
	h.audit.mu.Lock()
	var authorized *types.AuditEvent
	for i := range h.audit.events {
		if h.audit.events[i].Action == "ui.authorize" && h.audit.events[i].Outcome == "success" {
			authorized = &h.audit.events[i]
		}
	}
	h.audit.mu.Unlock()
	if authorized == nil || !strings.Contains(string(authorized.Data), `"entry":"sponsor"`) || !strings.Contains(string(authorized.Data), `"service_identity":"ci-bot"`) {
		t.Errorf("the entry's ui.authorize success row = %+v, want the sponsor marks", authorized)
	}
	if rec := uiGet(h, path, cookie); rec.Code != http.StatusOK {
		t.Fatalf("relay: %d %s, want 200", rec.Code, rec.Body.String())
	}

	h.audit.mu.Lock()
	var open *types.AuditEvent
	for i := range h.audit.events {
		if h.audit.events[i].Action == "ui.open" {
			open = &h.audit.events[i]
		}
	}
	h.audit.mu.Unlock()
	if open == nil {
		t.Fatalf("no ui.open row; rows = %v", h.audit.actions())
	}
	var d struct {
		ServiceIdentity string `json:"service_identity"`
		Entry           string `json:"entry"`
	}
	_ = json.Unmarshal(open.Data, &d)
	if open.ActorType != types.ActorHuman || open.Actor != "sponsor-sub" || d.ServiceIdentity != "ci-bot" || d.Entry != "sponsor" {
		t.Errorf("ui.open row actor_type=%q actor=%q service_identity=%q entry=%q, want human, sponsor-sub, ci-bot, sponsor",
			open.ActorType, open.Actor, d.ServiceIdentity, d.Entry)
	}

	// The sponsorship ends: the open session is refused at its next re-check, and a fresh ticket is no longer redeemable.
	st.still.Store(false)
	h.clock.advance(uiReassertInterval + time.Second)
	if rec := uiGet(h, path, cookie); rec.Code != http.StatusForbidden {
		t.Errorf("a sponsor's session after the sponsorship ended: %d %s, want 403", rec.Code, rec.Body.String())
	}
	if rec := h.enter(url.Values{"run": {h.run.ID.String()}, "app": {"code"}, "ticket": {h.ticket(h.run.ID, "sponsor-sub", oidc.RoleUser)}}); rec.Code != http.StatusForbidden {
		t.Errorf("a sponsor's ticket after the sponsorship ended: %d %s, want 403", rec.Code, rec.Body.String())
	}
	// A sponsorship that began after the ticket was admitted (A to B to A) refuses the redemption, and ends
	// an open session at its next re-check: the authority time is the ticket's, passed through both.
	st.still.Store(true)
	later := time.Now().Add(time.Hour)
	st.sponsorSince.Store(later.Unix())
	if rec := h.enter(url.Values{"run": {h.run.ID.String()}, "app": {"code"}, "ticket": {h.ticket(h.run.ID, "sponsor-sub", oidc.RoleUser)}}); rec.Code != http.StatusForbidden {
		t.Errorf("a ticket admitted before the sponsorship began: %d %s, want 403", rec.Code, rec.Body.String())
	}
	st.sponsorSince.Store(0)
	again := enter()
	st.sponsorSince.Store(time.Now().Add(2 * time.Second).Unix())
	h.clock.advance(uiReassertInterval + time.Second)
	if rec := uiGet(h, path, again); rec.Code != http.StatusForbidden {
		t.Errorf("a session opened before a re-sponsor, at its re-check: %d %s, want 403", rec.Code, rec.Body.String())
	}
	st.sponsorSince.Store(0)
	// A super admin's stamp never takes the sponsor arm.
	if rec := h.enter(url.Values{"run": {h.run.ID.String()}, "app": {"code"}, "ticket": {h.ticket(h.run.ID, "sponsor-sub", oidc.RoleAdmin)}}); rec.Code != http.StatusForbidden {
		t.Errorf("an admin-stamped ticket on a service run: %d %s, want 403", rec.Code, rec.Body.String())
	}
}
