// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func TestPortalExchange_RefreshFailureNamesPortalsOnSharedIssuer(t *testing.T) {
	a, b, other := uuid.NewString(), uuid.NewString(), uuid.NewString()
	counter := &PortalRefreshFailures{Portals: func(issuer string) []string {
		if issuer == portalTestIssuer {
			return []string{a, b}
		}
		return []string{other}
	}}
	counter.Record(oidc.PortalRefreshFailure{Issuer: portalTestIssuer, Source: "scheduled", Cause: "keys_unreachable"})
	counter.Record(oidc.PortalRefreshFailure{Issuer: portalTestIssuer, Source: "scheduled", Cause: "keys_unreachable"})
	counter.Record(oidc.PortalRefreshFailure{Issuer: "https://elsewhere.example", Source: "unknown_kid", Cause: "address_refused"})
	var w strings.Builder
	counter.write(&w)
	key := oidc.PortalIssuerKey(portalTestIssuer)
	for _, id := range []string{a, b} {
		want := `wardyn_portal_issuer_refresh_failures_total{issuer_key="` + key + `",portal="` + id + `",source="scheduled",cause="keys_unreachable"} 2`
		if !strings.Contains(w.String(), want) {
			t.Errorf("no series for portal %s on the shared issuer; want %s in:\n%s", id, want, w.String())
		}
	}
	if !strings.Contains(w.String(), `portal="`+other+`",source="unknown_kid",cause="address_refused"} 1`) {
		t.Errorf("the other issuer's failure is not attributed to its own portal:\n%s", w.String())
	}
	// The labels are the issuer key and delegate ids: no address, no person, no token.
	if strings.Contains(w.String(), "https://") || strings.Contains(w.String(), "delegate:person") {
		t.Errorf("the metric carries an address or a person:\n%s", w.String())
	}
}

func TestPortalPerson_ErasureProtectsConfiguredOperatorFirst(t *testing.T) {
	h := newExchHarness(t)
	person := oidc.PortalPrincipal(portalTestIssuer, "00u-person-1")
	// A seat configured with a name of the portal-person format stays reserved; a real, distinct portal person
	// is erasable.
	h.srv.cfg.LocalOperator = person
	r := httptest.NewRequest(http.MethodPost, "/api/v1/people/x/erase", nil)
	w := httptest.NewRecorder()
	if !h.srv.refuseReservedErasure(w, r, "person.erase", person) || w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), reasonReservedPrincipal) {
		t.Fatalf("the configured seat %q: refused=%d %s, want 422 reserved_principal", person, w.Code, w.Body.String())
	}
	other := oidc.PortalPrincipal(portalTestIssuer, "00u-person-2")
	w = httptest.NewRecorder()
	if h.srv.refuseReservedErasure(w, r, "person.erase", other) {
		t.Fatalf("a distinct portal person was refused erasure: %d %s", w.Code, w.Body.String())
	}
	// And the exchange never admits a person under the seat's name.
	d, cred := h.seedPortal(nil)
	h.ver.subj = h.subject("00u-person-1")
	we := h.exchange(t, d.ID, cred, "subject-token", nil)
	if we.Code != http.StatusBadRequest || h.rowCount("grants") != 0 {
		t.Fatalf("an exchange for the operator's own name = %d %s, want 400 and no grant", we.Code, we.Body.String())
	}
}

func (h *exchHarness) rowCount(kind string) int {
	h.ps.pmu.Lock()
	defer h.ps.pmu.Unlock()
	if kind == "grants" {
		return len(h.ps.grants)
	}
	return len(h.ps.idents)
}

// retiredBindingStore is a store whose capability-grant write is refused as the store refuses it when the
// person's issuer binding was retired after the handler's own checks.
type retiredBindingStore struct {
	*portalStore
	err    error
	writes int
}

func (s *retiredBindingStore) UpsertCapabilityGrant(context.Context, types.CapabilityGrant) (types.CapabilityGrant, error) {
	s.writes++
	return types.CapabilityGrant{}, s.err
}

func TestPortalRuling_StoreRefusalIs422WithItsRegisteredReason(t *testing.T) {
	person := oidc.PortalPrincipal(portalTestIssuer, "00u-person-1")
	for _, c := range []struct {
		name   string
		err    error
		reason string
	}{
		{"a retirement that committed after the check", store.ErrDelegatePersonUnknownPortal, reasonDelegatePersonUnknownPortal},
		{"a role a portal person cannot hold", store.ErrDelegatePersonNotAllowed, reasonDelegatePersonNotAllowed},
	} {
		h := newExchHarness(t)
		rs := &retiredBindingStore{portalStore: h.ps, err: c.err}
		h.srv.cfg.Store = rs
		body := `{"subject_type":"user","subject":"` + person + `","capability":"` + string(capWorkspaceProvider) + `","value":"` + uuid.NewString() + `","effect":"deny"}`
		w := doSSO(t, h.srv, http.MethodPost, "/api/v1/permissions/grants", ssoSession(t, "sec", "sec@corp.example", oidc.RoleSecurityAdmin), body)
		if w.Code != http.StatusUnprocessableEntity || errorReason(w) != c.reason {
			t.Errorf("%s: %d %s, want 422 %s", c.name, w.Code, w.Body.String(), c.reason)
		}
		if len(h.ps.grants) != 0 || len(h.ps.idents) != 0 {
			t.Errorf("%s: the refused write mutated state", c.name)
		}
		if rs.writes != 1 {
			t.Errorf("%s: the store write was asked %d times, want once (the refusal is the store's)", c.name, rs.writes)
		}
	}
}

func TestPortalIntroduction_BacklogBeyondBatchStillDeliversOwn(t *testing.T) {
	h := newExchHarness(t)
	d, cred := h.seedPortal(nil)
	// More owed introductions than one batch, all older than this person's.
	old := time.Now().Add(-time.Hour)
	for i := 0; i < portalIntroductionBatch+50; i++ {
		p := oidc.PortalPrincipal(portalTestIssuer, "backlog-"+uuid.NewString())
		h.ps.idents[p] = &portalIdent{id: uuid.New(), principal: p, introBy: d.ID, realIssuer: portalTestIssuer, key: d.IssuerKey,
			eventID: uuid.New(), eventAt: old.Add(time.Duration(i) * time.Second)}
	}
	portalToken(t, h.exchange(t, d.ID, cred, "subject-token", nil))
	own := h.ps.idents[h.ver.subj.Principal]
	if own == nil || !own.audited {
		t.Fatalf("the person's own introduction was not delivered behind a backlog: %+v", own)
	}
	rows := h.introductions()
	if len(rows) != 1 || rows[0].ID != own.eventID || rows[0].Target != h.ver.subj.Principal {
		t.Fatalf("person.create rows = %+v, want the person's own event only", rows)
	}
	for p, i := range h.ps.idents {
		if p != h.ver.subj.Principal && i.audited {
			t.Fatalf("the exchange delivered another person's introduction %s", p)
		}
	}
}
