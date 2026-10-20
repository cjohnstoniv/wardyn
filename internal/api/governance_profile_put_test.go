// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// With WARDYN_GOVERNANCE_PROFILE_PUT_EXISTING_ONLY on, PUT updates a profile
// that exists and answers 404 for an id no row holds; off, it still creates.
func TestGovernanceProfilePut_CreatesOnlyWithTheSwitchOff(t *testing.T) {
	existing := types.GovernanceProfile{ID: uuid.New(), Name: "interns"}
	for _, on := range []bool{false, true} {
		st := &govCrudStore{profiles: []types.GovernanceProfile{existing}}
		srv, _ := govCrudServer(t, st)
		srv.cfg.GovernanceProfilePutExistingOnly = on

		absent := uuid.New()
		w := do(t, srv, http.MethodPut, "/api/v1/governance/profiles/"+absent.String(), adminToken, `{"name":"new","ceiling":{"min_confinement_class":"CC2"}}`)
		switch {
		case on && (w.Code != http.StatusNotFound || errorReason(w) != reasonGovernanceProfileNotFoundByID || st.lastProfile.ID == absent):
			t.Errorf("switch on: PUT to an absent id = %d %s (stored %s), want 404 %s and no row", w.Code, w.Body.String(), st.lastProfile.ID, reasonGovernanceProfileNotFoundByID)
		case !on && (w.Code != http.StatusOK || st.lastProfile.ID != absent):
			t.Errorf("switch off: PUT to an absent id = %d %s, want 200 and a row at that id", w.Code, w.Body.String())
		}
		if w := do(t, srv, http.MethodPut, "/api/v1/governance/profiles/"+existing.ID.String(), adminToken, `{"name":"interns-2","ceiling":{"min_confinement_class":"CC2"}}`); w.Code != http.StatusOK || st.lastProfile.ID != existing.ID {
			t.Errorf("switch %v: PUT to an existing id = %d %s, want 200", on, w.Code, w.Body.String())
		}
	}
}

// A PUT that creates a profile is logged once the write has stored it, not before: a write the store fails
// created nothing, and an update to an existing id was never a create.
func TestGovernanceProfilePut_CreateIsLoggedOnlyOnceStored(t *testing.T) {
	const logLine = "a PUT created a governance profile"
	existing := types.GovernanceProfile{ID: uuid.New(), Name: "interns"}
	body := `{"name":"new","ceiling":{"min_confinement_class":"CC2"}}`
	run := func(t *testing.T, id uuid.UUID, failStore bool) (int, string) {
		t.Helper()
		st := &govCrudStore{profiles: []types.GovernanceProfile{existing}}
		if failStore {
			st.upsertProfileErr = errors.New("store: down")
		}
		srv, _ := govCrudServer(t, st)
		var logged lockedBuffer
		prev := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&logged, nil)))
		t.Cleanup(func() { slog.SetDefault(prev) })
		w := do(t, srv, http.MethodPut, "/api/v1/governance/profiles/"+id.String(), adminToken, body)
		return w.Code, logged.String()
	}

	if code, logged := run(t, uuid.New(), false); code != http.StatusOK || strings.Count(logged, logLine) != 1 {
		t.Errorf("a stored create: code %d, logged %d times, want 200 and once", code, strings.Count(logged, logLine))
	}
	if code, logged := run(t, uuid.New(), true); code == http.StatusOK || strings.Contains(logged, logLine) {
		t.Errorf("a create the store failed: code %d, logged %v, want a failure and no log line", code, strings.Contains(logged, logLine))
	}
	if code, logged := run(t, existing.ID, false); code != http.StatusOK || strings.Contains(logged, logLine) {
		t.Errorf("an update to an existing id: code %d, logged %v, want 200 and no log line", code, strings.Contains(logged, logLine))
	}
}
