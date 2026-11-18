// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A helper who meets a REST door again after their help ended, naming no grant, is told the help ended
// (support_grant_ended) rather than refused as a super admin who was never let in (run_owner_only,
// supportHelperEnded). The doors are the four a super admin enters a person's run through without a ticket
// in hand: the attach-ticket mint for a terminal and for a UI app, POST /exec and the Files panel.

// restDoors is each REST door the helper asks with no grant named.
func (rig *supportTermRig) restDoors() map[string]func() *httptest.ResponseRecorder {
	return map[string]func() *httptest.ResponseRecorder{
		"attach-ticket mint": func() *httptest.ResponseRecorder { return rig.mint(rig.helper, "") },
		"UI app ticket mint": func() *httptest.ResponseRecorder { return rig.mint(rig.helper, "?purpose=ui") },
		"POST /exec":         func() *httptest.ResponseRecorder { return rig.exec(rig.helper, "true") },
		"Files panel (listed)": func() *httptest.ResponseRecorder {
			return doSSO(rig.t, rig.srv, http.MethodGet, rig.path("/desktop/files"), rig.helper, "")
		},
	}
}

// wantEveryDoorRefused asserts each REST door answers 403 with reason and mints no ticket.
func (rig *supportTermRig) wantEveryDoorRefused(reason string) {
	rig.t.Helper()
	for name, door := range rig.restDoors() {
		if w := door(); w.Code != http.StatusForbidden || wireReason(rig.t, w) != reason || rig.ticketCount() != 0 {
			rig.t.Errorf("%s = %d %s with %d tickets, want 403 %s and none", name, w.Code, w.Body, rig.ticketCount(), reason)
		}
	}
}

// grantOn requests a grant for the helper on run and, when doors are given, has the owner accept it.
func (rig *supportTermRig) grantOn(run types.AgentRun, doors ...string) store.SupportGrant {
	rig.t.Helper()
	ctx := context.Background()
	g, err := rig.st.RequestSupportGrant(ctx, store.SupportGrant{RunID: run.ID, Helper: supportHelperSub,
		HelperEmail: supportHelperSub + "@example.test", Reason: "r", DoorsAsked: allSupportDoors})
	if err != nil {
		rig.t.Fatal(err)
	}
	if len(doors) == 0 {
		return g
	}
	if g, err = rig.st.AcceptSupportGrant(ctx, run.ID, g.ID, supportOwnerSub, supportOwnerSub+"@example.test", doors, 30, 0); err != nil {
		rig.t.Fatal(err)
	}
	return g
}

func TestSupportEntryNamesAnEndedGrant(t *testing.T) {
	t.Run("ended by the owner", func(t *testing.T) {
		rig := newSupportTermRig(t, "terminal", "commands", "files", "web_apps")
		rig.endGrant("revoked")
		rig.wantEveryDoorRefused(string(authz.ReasonSupportGrantEnded))
	})
	t.Run("expired", func(t *testing.T) {
		rig := newSupportTermRig(t, "terminal", "commands", "files", "web_apps")
		rig.st.tick(31 * time.Minute)
		rig.wantEveryDoorRefused(string(authz.ReasonSupportGrantEnded))
	})
}

func TestSupportEntryKeepsRunOwnerOnlyForACallerWithNoGrantOnTheRun(t *testing.T) {
	t.Run("no grant row", func(t *testing.T) {
		newSupportTermRig(t).wantEveryDoorRefused(runOwnerOnlyReason)
	})
	t.Run("a request that lapsed unanswered", func(t *testing.T) {
		rig := newSupportTermRig(t)
		rig.grantOn(rig.run)
		rig.st.tick(16 * time.Minute)
		rig.wantEveryDoorRefused(runOwnerOnlyReason)
	})
	t.Run("a request still open, unanswered", func(t *testing.T) {
		rig := newSupportTermRig(t)
		rig.grantOn(rig.run)
		rig.wantEveryDoorRefused(runOwnerOnlyReason)
	})
	t.Run("a grant on another run that ended", func(t *testing.T) {
		rig := newSupportTermRig(t)
		other := types.AgentRun{ID: uuid.New(), CreatedBy: supportOwnerSub, State: types.RunRunning, SandboxRef: "sbx-2"}
		rig.st.putRun(other)
		g := rig.grantOn(other, "terminal", "commands", "files", "web_apps")
		if _, err := rig.st.EndSupportGrant(context.Background(), other.ID, g.ID, "revoked", true); err != nil {
			t.Fatal(err)
		}
		rig.wantEveryDoorRefused(runOwnerOnlyReason)
	})
}

func TestSupportEntryAdmitsALiveGrantAsBefore(t *testing.T) {
	rig := newSupportTermRig(t, "terminal", "commands")
	if tk := rig.stored(rig.mint(rig.helper, "")); tk.SupportGrant != rig.grant.ID {
		t.Fatalf("terminal ticket = %+v, want one carrying the live grant", tk)
	}
	if w := rig.exec(rig.helper, "true"); w.Code != http.StatusOK {
		t.Fatalf("POST /exec under a live grant = %d %s, want 200", w.Code, w.Body)
	}
}

// supportMayEnter's own answer for each caller, with the grant named and not.
func TestSupportMayEnterEndedUnnamed(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name         string
		prepare      func(rig *supportTermRig)
		unnamed, set supportVerdict
	}{
		{"ended grant on this run", func(rig *supportTermRig) { rig.endGrant("revoked") }, supportEndedUnnamed, supportEnded},
		{"no grant row", func(*supportTermRig) {}, supportNone, supportEnded},
		{"live grant", func(*supportTermRig) {}, supportOK, supportOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doors := []string{"terminal"}
			if tc.name == "no grant row" {
				doors = nil
			}
			rig := newSupportTermRig(t, doors...)
			tc.prepare(rig)
			if _, v := rig.srv.supportMayEnter(ctx, rig.run, supportHelperSub, uuid.Nil, supportDoorTerminal); v != tc.unnamed {
				t.Errorf("no grant named: verdict %d, want %d", v, tc.unnamed)
			}
			named := rig.grant.ID
			if named == uuid.Nil {
				named = uuid.New()
			}
			if _, v := rig.srv.supportMayEnter(ctx, rig.run, supportHelperSub, named, supportDoorTerminal); v != tc.set {
				t.Errorf("grant named: verdict %d, want %d", v, tc.set)
			}
		})
	}
	t.Run("the helper read failing refuses", func(t *testing.T) {
		rig := newSupportTermRig(t)
		rig.st.failOn("SupportHelperSince", errors.New("grants unavailable"))
		if _, v := rig.srv.supportMayEnter(ctx, rig.run, supportHelperSub, uuid.Nil, supportDoorTerminal); v != supportUnavailable {
			t.Errorf("verdict %d, want supportUnavailable", v)
		}
	})
}
