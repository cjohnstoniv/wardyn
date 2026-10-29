// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// appTicketAs is POST /runs/{id}/apps/{app}/ticket on h's run as the signed-in person sub, in groups.
func (h *uiHarness) appTicketAs(app, sub string, groups []string) *httptest.ResponseRecorder {
	h.t.Helper()
	r := chi.NewRouter()
	h.srv.mountInboundTicketRoutes(r)
	req := httptest.NewRequest(http.MethodPost, "/runs/"+h.run.ID.String()+"/apps/"+app+"/ticket", nil)
	req = req.WithContext(withHumanIdentity(req.Context(), sub, sub+"@example.com", oidc.RoleUser, types.UserTypeStandard, groups, false))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

// enterOnRunHost is enter served on the run's own host, as host mode requires.
func (h *uiHarness) enterOnRunHost(app, ticket string) *httptest.ResponseRecorder {
	h.t.Helper()
	q := url.Values{"run": {h.run.ID.String()}, "app": {app}, "ticket": {ticket}}
	req := h.bound(httptest.NewRequest(http.MethodGet, uiEnterPath+"?"+q.Encode(), nil), q)
	req.Host = h.run.ID.String() + ".ui.example.com"
	rec := httptest.NewRecorder()
	h.gateway.ServeHTTP(rec, req)
	return rec
}

// inboundTicketHarness is newUIHarness in host mode whose "code" app is inbound with callers, beside "ide",
// which is not.
func inboundTicketHarness(t *testing.T, callers ...string) *uiHarness {
	t.Helper()
	h := newUIHarness(t, okBackend())
	h.srv.cfg.UIOriginTemplate = "https://{run}.ui.example.com"
	h.store.putEffectivePolicy(h.run.ID, types.RunPolicySpec{MinConfinementClass: types.CC2, UIApps: []types.UIApp{
		{Name: "code", Port: uiTestPort, Inbound: true, Callers: callers}, {Name: "ide", Port: uiTestPort + 1}}})
	return h
}

// TestAppTicket_EnterOpensOnlyItsApp: a caller's ticket opens the app it was minted for as that caller (the
// relay names them on inbound.allow), never another app of the run, and not once the app stops naming them.
func TestAppTicket_EnterOpensOnlyItsApp(t *testing.T) {
	h := inboundTicketHarness(t, "user:bob", "group:ops")
	mint := func(sub string, groups ...string) string {
		t.Helper()
		return ticketOf(t, h.appTicketAs("code", sub, groups))
	}

	if rec := h.enterOnRunHost("ide", mint("bob")); rec.Code != http.StatusForbidden || len(setUICookies(rec)) != 0 {
		t.Fatalf("a caller's ticket for code, spent on ide: %d %s, want 403 and no cookie", rec.Code, rec.Body.String())
	}
	rec := h.enterOnRunHost("code", mint("bob"))
	if rec.Code != http.StatusFound {
		t.Fatalf("a caller's ticket for code: %d %s, want 302", rec.Code, rec.Body.String())
	}
	var cookie *http.Cookie
	for _, c := range setUICookies(rec) {
		if c.Name == uiCookieName {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("no relay cookie set")
	}
	relay := h.relay("/", cookie, func(r *http.Request) { r.Host = h.run.ID.String() + ".ui.example.com" })
	if relay.Code != http.StatusOK {
		t.Fatalf("relay: %d %s, want 200", relay.Code, relay.Body.String())
	}
	if rows := h.audit.auditRows("inbound.allow", "success"); len(rows) != 1 || !strings.Contains(string(rows[0].Data), `"caller":"user:bob"`) {
		t.Errorf("inbound.allow rows = %+v, want one naming caller user:bob", rows)
	}
	if rec := h.enterOnRunHost("code", mint("gail", "OPS")); rec.Code != http.StatusFound {
		t.Errorf("a caller by group: %d %s, want 302", rec.Code, rec.Body.String())
	}

	// The callers entry the ticket was minted under is gone from the app: the ticket opens nothing.
	tok := mint("bob")
	h.store.putEffectivePolicy(h.run.ID, types.RunPolicySpec{MinConfinementClass: types.CC2, UIApps: []types.UIApp{
		{Name: "code", Port: uiTestPort, Inbound: true, Callers: []string{"group:ops"}}}})
	if rec := h.enterOnRunHost("code", tok); rec.Code != http.StatusForbidden {
		t.Errorf("a ticket whose callers entry is gone: %d %s, want 403", rec.Code, rec.Body.String())
	}
}
