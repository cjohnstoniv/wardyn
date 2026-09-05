// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// staleAuditStore answers the two reads ceilingWithUnusableGroups makes. The
// group-tier flag is what decides whether an unanswerable snapshot is a refusal
// (PF-21: nothing an unknown group could have been hiding means nothing to
// refuse), so it is the control's own switch rather than a second fixture.
type staleAuditStore struct {
	store.Store
	hasGroupTier bool
}

func (staleAuditStore) ResolveGovernanceProfile(context.Context, []string, []string) (*types.GovernanceProfile, types.CapabilitySubjectType, error) {
	return nil, "", store.ErrNotFound
}
func (s staleAuditStore) HasGroupTierAssignments(context.Context) (bool, error) {
	return s.hasGroupTier, nil
}

// TestStaleSnapshotRefusalIsAudited is F227.
//
// docs/OPERATIONS.md's "Every denial that isn't a 404" makes authz.denied the
// record of every member denial that is not a plain foreign-resource 404. The
// groups_snapshot_stale 403 is member-reachable from six seams and produced
// ZERO audit rows: an operator reading the denial stream saw nothing at all for
// a member who cannot use the product, and the reason was not in the closed
// enum either.
func TestStaleSnapshotRefusalIsAudited(t *testing.T) {
	newSrv := func(t *testing.T, hasGroupTier bool) (*Server, *recRecorder) {
		h := newHarness(t)
		cfg := baseTestConfig(h, staleAuditStore{hasGroupTier: hasGroupTier})
		cfg.OIDC = &oidc.Authenticator{}
		return New(cfg), h.audit
	}

	t.Run("the refusal writes authz.denied with the documented reason", func(t *testing.T) {
		srv, rec := newSrv(t, true)
		// A member whose group snapshot is NIL — the launch-side spelling of
		// "the group tier was not evaluated".
		member := ssoSession(t, "sub-stale-bob", "bob@corp.example", oidc.RoleMember)
		w := doSSO(t, srv, http.MethodGet, "/api/v1/policies/default", member, "")
		if w.Code != http.StatusForbidden {
			t.Fatalf("GET /policies/default = %d, want 403; body=%s", w.Code, w.Body.String())
		}

		var found int
		for _, ev := range rec.events {
			if ev.Action != "authz.denied" {
				continue
			}
			var data map[string]any
			if err := json.Unmarshal(ev.Data, &data); err != nil {
				t.Fatal(err)
			}
			if data["reason"] != "groups_snapshot_stale" {
				continue
			}
			found++
			if ev.Target != "governance.ceiling" {
				t.Errorf("target = %q, want %q", ev.Target, "governance.ceiling")
			}
			if ev.Actor != "sub-stale-bob" {
				t.Errorf("actor = %q, want the refused principal", ev.Actor)
			}
		}
		if found == 0 {
			t.Fatalf("a member-reachable 403 produced NO authz.denied row (%d events recorded) — the denial "+
				"stream is the operator's only view of who cannot use the product", len(rec.events))
		}
		// ONCE PER REQUEST, not once per seam: effectiveCeiling memoizes, so a
		// create that asks three times is one denial, which is what an operator
		// counting denials means.
		if found != 1 {
			t.Errorf("one request produced %d denial rows, want 1 — the memo is what makes the count mean "+
				"'denials', not 'ceiling reads'", found)
		}
	})

	// The control: the SAME unanswerable snapshot on a deployment that has
	// authored no group-tier assignment is not a refusal at all (PF-21 — nothing
	// an unknown group could have been hiding), so it must write no denial
	// either. Without this arm the new reason could be noise on every
	// pre-upgrade session rather than signal.
	t.Run("a deployment with no group-tier assignment writes no denial", func(t *testing.T) {
		srv, rec := newSrv(t, false)
		member := ssoSession(t, "sub-ok-bob", "ok@corp.example", oidc.RoleMember)
		before := len(rec.events)
		if w := doSSO(t, srv, http.MethodGet, "/api/v1/policies/default", member, ""); w.Code == http.StatusForbidden {
			t.Fatalf("a member on a deployment with no group-tier assignment was refused; body=%s", w.Body.String())
		}
		for _, ev := range rec.events[before:] {
			if ev.Action == "authz.denied" {
				t.Errorf("no refusal happened, but an authz.denied row was written: %+v", ev)
			}
		}
	})
}
