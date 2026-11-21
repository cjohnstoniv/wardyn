// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// activationStore is the activation's store surface over a map-free double.
type activationStore struct {
	store.Store
	on       bool
	lose     bool // another caller's write set the mark first
	sessions []types.ClientSession
}

func (s *activationStore) PortalIssuersActivated(context.Context) (bool, error) { return s.on, nil }
func (s *activationStore) ActivatePortalIssuers(context.Context, string, []string) (bool, error) {
	if s.lose {
		return false, nil
	}
	s.on = true
	return true, nil
}
func (s *activationStore) UntaggedClientSessions(context.Context) ([]types.ClientSession, error) {
	return s.sessions, nil
}

// The activation needs the operator's statement that older servers are stopped: a body without it, or with it
// false, is 422 and switches nothing on.
func TestPortalIssuersActivate_NeedsOldServersStoppedStatement(t *testing.T) {
	fake := &activationStore{}
	h := newHarness(t)
	srv := New(baseTestConfig(h, fake))
	for _, body := range []string{`{}`, `{"old_servers_stopped":false}`, `{"confirm_listed_sessions":true}`} {
		w := do(t, srv, http.MethodPost, "/api/v1/admin/portal-issuers/activate", adminToken, body)
		if w.Code != http.StatusUnprocessableEntity || errorReason(w) != reasonPortalIssuersStatementRequired {
			t.Errorf("activate with %s = %d %s, want 422 %s", body, w.Code, w.Body, reasonPortalIssuersStatementRequired)
		}
	}
	if fake.on {
		t.Error("a refused activation switched issuer portals on")
	}
}

// Two operators activating at once: the caller whose write did not set the mark is told it is already on, and
// no activation or override row names it.
func TestPortalIssuersActivate_LoserOfARaceIsAlreadyActivated(t *testing.T) {
	fake := &activationStore{lose: true, sessions: []types.ClientSession{{Label: "old-wardynd", ClientAddr: "10.0.0.9"}}}
	h := newHarness(t)
	srv := New(baseTestConfig(h, fake))
	for i := 0; i < 14; i++ {
		srv.portalMonitor().record(time.Now().Add(time.Duration(i-14)*portalSampleEvery), fake.sessions)
	}
	w := do(t, srv, http.MethodPost, "/api/v1/admin/portal-issuers/activate", adminToken, `{"old_servers_stopped":true,"confirm_listed_sessions":true}`)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"already_activated":true`) {
		t.Fatalf("the losing call = %d %s, want 200 already_activated", w.Code, w.Body)
	}
	for _, ev := range h.audit.snapshot() {
		if strings.HasPrefix(ev.Action, "portal_issuers.activate") {
			t.Errorf("the loser wrote %s", ev.Action)
		}
	}
}

// The mark is a store-only key: a PUT /site-config body that names it is 400 and changes nothing.
func TestPortalIssuersActivate_BodyNamingMarkRefused(t *testing.T) {
	fake := &fakeSiteConfigStore{}
	srv, audit := newSiteConfigHarness(t, fake)
	w := do(t, srv, http.MethodPut, "/api/v1/site-config", adminToken, `{"`+store.PortalIssuersActivatedKey+`":"2026-01-01T00:00:00Z"}`)
	if w.Code != http.StatusBadRequest || errorReason(w) != reasonSiteConfigRequestInvalid {
		t.Fatalf("a body naming the mark = %d %s, want 400 %s", w.Code, w.Body, reasonSiteConfigRequestInvalid)
	}
	if fake.putSeen != nil || len(audit.events) != 0 {
		t.Errorf("the refused body reached the store (%+v) or the audit (%d rows)", fake.putSeen, len(audit.events))
	}
}
