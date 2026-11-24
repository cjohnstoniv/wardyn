// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A limit set during a desktop sign-in ends it at the next sweep when the profile its run was launched under now
// denies interactive sessions (signinGovernanceEnd). The person's own End and the window's expiry keep their reasons.

// sweepSoon runs one sweep a second from now, inside the window.
func sweepSoon(t *testing.T, f *signinFixture) {
	t.Helper()
	f.srv.cfg.Now = func() time.Time { return time.Now().Add(time.Second) }
	if err := f.srv.sweepDesktopSignins(context.Background()); err != nil {
		t.Fatalf("sweep: %v", err)
	}
}

// Rows 4 and 5: the sweep ends an open sign-in under deny_interactive and resumes the agent.
func TestDesktopSigninSweep_GovernanceEndsTheSignin(t *testing.T) {
	f := newSigninFixture(t)
	set := signinDoorWall(f, "")
	f.begin(t)
	set(types.GovernanceLimits{DenyInteractive: true})
	sweepSoon(t, f)
	si := f.st.only(t)
	if si.EndedAt == nil || si.EndReason != signinEndGovernance {
		t.Fatalf("row = %+v, want ended %s", si, signinEndGovernance)
	}
	ev := findAudit(f.h.audit.snapshot(), f.run.ID, "desktop_signin.close", "success")
	if ev == nil || !strings.Contains(string(ev.Data), `"reason":"governance_unit"`) || !strings.Contains(string(ev.Data), `"agent_resumed":true`) {
		t.Fatalf("close row = %+v, want success with reason governance_unit and agent_resumed true", ev)
	}
	if got := signinCloseRows(f); !slices.Equal(got, []string{"success true"}) {
		t.Fatalf("desktop_signin.close rows = %v, want one", got)
	}
}

// A profile that cannot be read ends nothing; the next sweep, with the read healed, ends it.
func TestDesktopSigninSweep_UnreadProfileEndsNothing(t *testing.T) {
	f := newSigninFixture(t)
	set := signinDoorWall(f, "")
	f.begin(t)
	f.st.sshMemStore.mu.Lock()
	f.st.profilesErr = errors.New("db down")
	f.st.sshMemStore.mu.Unlock()
	sweepSoon(t, f)
	if si := f.st.only(t); si.EndedAt != nil {
		t.Fatalf("row = %+v, want still open on an unreadable profile", si)
	}
	if got := signinCloseRows(f); len(got) != 0 {
		t.Fatalf("desktop_signin.close rows = %v, want none", got)
	}
	f.st.sshMemStore.mu.Lock()
	f.st.profilesErr = nil
	f.st.sshMemStore.mu.Unlock()
	set(types.GovernanceLimits{DenyInteractive: true})
	sweepSoon(t, f)
	if si := f.st.only(t); si.EndedAt == nil || si.EndReason != signinEndGovernance {
		t.Fatalf("row = %+v, want ended %s once the read healed", si, signinEndGovernance)
	}
}

// Row 18: a profile composition nothing satisfies is a definite answer.
func TestDesktopSigninSweep_UnsatisfiableEnds(t *testing.T) {
	f := newSigninFixture(t)
	f.begin(t)
	base := types.GovernanceUnit{ID: uuid.New(), Name: "baseline", Ceiling: types.RunPolicySpec{AllowedMethods: []string{"GET"}}}
	post := []string{"POST"}
	child := types.GovernanceUnit{ID: uuid.New(), Name: "team", ParentID: &base.ID, Overlay: &types.CeilingOverlay{AllowedMethods: &post}}
	f.st.sshMemStore.mu.Lock()
	f.st.profiles = []types.GovernanceUnit{base, child}
	f.st.sshMemStore.mu.Unlock()
	f.run.UnitID = &child.ID
	f.st.putRun(f.run)
	sweepSoon(t, f)
	if si := f.st.only(t); si.EndedAt == nil || si.EndReason != signinEndGovernance {
		t.Fatalf("row = %+v, want ended %s", si, signinEndGovernance)
	}
}

// Row 17: the person's own End and the window's expiry keep their reasons; the harness sign-in run is exempt.
func TestDesktopSigninSweep_OwnReasonsWin(t *testing.T) {
	deny := types.GovernanceLimits{DenyInteractive: true}
	t.Run("an End already asked ends done", func(t *testing.T) {
		f := newSigninFixture(t)
		set := signinDoorWall(f, "")
		f.begin(t)
		f.rn.exit["stop"] = 1 // the End cannot finish its first try: it is left for the sweep
		if code, body := f.post(t, "/desktop-signin/end", ""); code != http.StatusBadGateway {
			t.Fatalf("end = %d %s, want 502", code, body)
		}
		f.rn.exit["stop"] = 0
		set(deny)
		sweepSoon(t, f)
		if si := f.st.only(t); si.EndedAt == nil || si.EndReason != signinEndDone {
			t.Fatalf("row = %+v, want ended %s", si, signinEndDone)
		}
	})
	t.Run("an expired window ends expired", func(t *testing.T) {
		f := newSigninFixture(t)
		set := signinDoorWall(f, "")
		f.begin(t)
		set(deny)
		f.srv.cfg.Now = func() time.Time { return time.Now().Add(31 * time.Minute) }
		if err := f.srv.sweepDesktopSignins(context.Background()); err != nil {
			t.Fatal(err)
		}
		if si := f.st.only(t); si.EndedAt == nil || si.EndReason != signinEndExpired {
			t.Fatalf("row = %+v, want ended %s", si, signinEndExpired)
		}
	})
	t.Run("the harness sign-in run is not ended for governance", func(t *testing.T) {
		f := newSigninFixture(t)
		set := signinDoorWall(f, harnessLoginTask)
		f.begin(t)
		set(deny)
		sweepSoon(t, f)
		if si := f.st.only(t); si.EndedAt != nil {
			t.Fatalf("row = %+v, want the harness sign-in left open", si)
		}
		if got := signinCloseRows(f); len(got) != 0 {
			t.Fatalf("desktop_signin.close rows = %v, want none", got)
		}
	})
}
