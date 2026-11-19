// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"net/url"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// signinDoorWall puts f's run, as task, under a profile that denies nothing yet, and returns what sets its limits.
func signinDoorWall(f *signinFixture, task string) func(types.GovernanceLimits) {
	unit := types.GovernanceUnit{ID: uuid.New(), Name: "walled"}
	set := func(l types.GovernanceLimits) {
		unit.Limits = l
		f.st.sshMemStore.mu.Lock()
		defer f.st.sshMemStore.mu.Unlock()
		f.st.profiles = []types.GovernanceUnit{unit}
	}
	set(types.GovernanceLimits{})
	f.run.UnitID, f.run.Task = &unit.ID, task
	f.st.putRun(f.run)
	return set
}

// signinDoorRows is how many sign-in rows the store holds.
func signinDoorRows(f *signinFixture) int {
	f.st.smu.Lock()
	defer f.st.smu.Unlock()
	return len(f.st.signins)
}

// Under either limit a begin is refused before anything happens: the agent was not stopped, no helper ran, no sign-in row exists.
func TestSigninDoor_BeginRefusedBeforeTheAgentStops(t *testing.T) {
	for what, limits := range map[string]types.GovernanceLimits{
		"deny_interactive": {DenyInteractive: true},
		"deny_ui_apps":     {DenyUIApps: true},
	} {
		t.Run(what, func(t *testing.T) {
			f := newSigninFixture(t)
			signinDoorWall(f, "")(limits)
			w := doSSO(t, f.srv, http.MethodPost, "/api/v1/runs/"+f.run.ID.String()+"/desktop-signin", f.cookie, "")
			if w.Code != http.StatusForbidden || reasonOf(t, w) != string(authz.ReasonGovernanceUnit) {
				t.Fatalf("begin = %d %s, want 403 %s", w.Code, w.Body.String(), authz.ReasonGovernanceUnit)
			}
			if got := f.rn.seen(); len(got) != 0 {
				t.Fatalf("calls = %v, want none: the agent was stopped", got)
			}
			if n := signinDoorRows(f); n != 0 {
				t.Fatalf("%d sign-in rows, want none", n)
			}
		})
	}
}

// The harness sign-in run keeps its exemption from deny_interactive at begin, and share runs the same door.
func TestSigninDoor_HarnessLoginRunKeepsItsExemption(t *testing.T) {
	f := newSigninFixture(t)
	signinDoorWall(f, harnessLoginTask)(types.GovernanceLimits{DenyInteractive: true})
	f.begin(t)
	f.rn.stdout["export"] = `[{"name":"sid","value":"v","domain":".site.test"}]`
	if code, body := f.post(t, "/desktop-signin/shares", `{"site":"site.test"}`); code != http.StatusOK {
		t.Fatalf("share = %d %s, want 200", code, body)
	}
}

// A limit set during a sign-in refuses the next share: no cookie is read or written. End stays open, so the person
// can still give the agent back, in order: the display goes, the agent resumes, the row closes.
func TestSigninDoor_LimitMidSigninRefusesShareAndEndStillWorks(t *testing.T) {
	f := newSigninFixture(t)
	set := signinDoorWall(f, "")
	f.begin(t)
	before := len(f.rn.seen())
	set(types.GovernanceLimits{DenyInteractive: true})
	w := doSSO(t, f.srv, http.MethodPost, "/api/v1/runs/"+f.run.ID.String()+"/desktop-signin/shares", f.cookie, `{"site":"site.test"}`)
	if w.Code != http.StatusForbidden || reasonOf(t, w) != string(authz.ReasonGovernanceUnit) {
		t.Fatalf("share = %d %s, want 403 %s", w.Code, w.Body.String(), authz.ReasonGovernanceUnit)
	}
	if got := f.rn.seen()[before:]; len(got) != 0 {
		t.Fatalf("calls after a refused share = %v, want none", got)
	}
	if code, body := f.post(t, "/desktop-signin/end", ""); code != http.StatusNoContent {
		t.Fatalf("end under the limit = %d %s, want 204", code, body)
	}
	if got, want := f.rn.seen()[before:], []string{"person:stop", "agent:resume-agent", "signal:SIGCONT"}; !slices.Equal(got, want) {
		t.Fatalf("calls = %v, want %v", got, want)
	}
	if si := f.st.only(t); si.EndedAt == nil || si.EndReason != signinEndDone {
		t.Fatalf("row = %+v, want ended %s", si, signinEndDone)
	}
}

// signinDoorRunner relays what the gateway dials to the harness's backend and leaves the helper to signinFakeRunner.
type signinDoorRunner struct {
	*signinFakeRunner
	relay func(runner.ExecSpec) (*runner.ExecSession, error)
}

func (r *signinDoorRunner) ExecStream(ctx context.Context, ref string, spec runner.ExecSpec) (*runner.ExecSession, error) {
	if spec.Argv[0] == signinBinary {
		return r.signinFakeRunner.ExecStream(ctx, ref, spec)
	}
	return r.relay(spec)
}

func (r *signinDoorRunner) ExecStreamAs(ctx context.Context, ref, user string, spec runner.ExecSpec) (*runner.ExecSession, error) {
	if spec.Argv[0] == signinBinary {
		return r.signinFakeRunner.ExecStreamAs(ctx, ref, user, spec)
	}
	return r.relay(spec)
}

// signinDoorGateway is a UI gateway on a run of task, under a profile that denies nothing yet, declaring "code" and
// "novnc", with a sign-in open that its owner holds. It returns
// the harness, the helper's runner, the store and what sets the limits.
func signinDoorGateway(t *testing.T, task string) (*uiHarness, *signinFakeRunner, *signinTestStore, func(types.GovernanceLimits)) {
	t.Helper()
	h := newUIHarness(t, okBackend())
	h.store.putEffectivePolicy(h.run.ID, types.RunPolicySpec{MinConfinementClass: types.CC2, UIApps: []types.UIApp{
		{Name: "code", Port: uiTestPort, Path: "/ide"}, {Name: desktopUIApp, Port: uiTestPort},
	}})
	h.run.Task = task
	set := walledRun(h)
	st := &signinTestStore{uiMemStore: h.store, signins: map[uuid.UUID]types.DesktopSignin{}, dispatched: map[uuid.UUID]string{h.run.ID: ""}}
	rn := newSigninFakeRunner()
	rn.sshFakeRunner = h.runner
	h.srv.cfg.Store, h.srv.cfg.DesktopSignins = st, true
	h.srv.cfg.Runner = &signinDoorRunner{signinFakeRunner: rn, relay: h.runner.execFn}
	now := h.clock.now()
	if err := st.BeginDesktopSignin(context.Background(), types.DesktopSignin{ID: uuid.New(), RunID: h.run.ID, Holder: h.owner,
		BeganAt: now, ExpiresAt: now.Add(desktopSigninWindow)}); err != nil {
		t.Fatal(err)
	}
	return h, rn, st, set
}

// The sign-in display and a declared novnc desktop are refused at entry under deny_interactive, with no cookie; another
// declared app is deny_ui_apps' alone and still enters; a harness login run keeps its exemption.
func TestSigninDoor_EntryUnderDenyInteractive(t *testing.T) {
	for _, c := range []struct {
		what, app, task string
		refused         bool
	}{
		{"the sign-in display", desktopSigninApp, "", true},
		{"a declared novnc desktop", desktopUIApp, "", true},
		{"another declared app", "code", "", false},
		{"novnc on a harness login run", desktopUIApp, harnessLoginTask, false},
		{"the sign-in display on a harness login run", desktopSigninApp, harnessLoginTask, false},
	} {
		t.Run(c.what, func(t *testing.T) {
			h, _, _, set := signinDoorGateway(t, c.task)
			set(types.GovernanceLimits{DenyInteractive: true})
			rec := h.enter(url.Values{"run": {h.run.ID.String()}, "app": {c.app}, "ticket": {h.ticket(h.run.ID, h.owner, oidc.RoleUser)}})
			if c.refused {
				if rec.Code != http.StatusForbidden || respondedReason(t, rec) != string(authz.ReasonGovernanceUnit) || len(setUICookies(rec)) != 0 {
					t.Fatalf("enter = %d %s, %d cookies; want 403 %s and none", rec.Code, rec.Body.String(), len(setUICookies(rec)), authz.ReasonGovernanceUnit)
				}
				return
			}
			if rec.Code != http.StatusFound || len(setUICookies(rec)) != 1 {
				t.Fatalf("enter = %d %s, %d cookies; want a redirect and the cookie", rec.Code, rec.Body.String(), len(setUICookies(rec)))
			}
		})
	}
}

// A session opened before the limit answers to it at its next re-check: the display and novnc are refused, another
// declared app goes on, and a harness login run keeps its exemption.
func TestSigninDoor_ReCheckUnderDenyInteractive(t *testing.T) {
	for _, c := range []struct {
		what, app, task string
		refused         bool
	}{
		{"the sign-in display", desktopSigninApp, "", true},
		{"a declared novnc desktop", desktopUIApp, "", true},
		{"another declared app", "code", "", false},
		{"novnc on a harness login run", desktopUIApp, harnessLoginTask, false},
		{"the sign-in display on a harness login run", desktopSigninApp, harnessLoginTask, false},
	} {
		t.Run(c.what, func(t *testing.T) {
			h, _, _, set := signinDoorGateway(t, c.task)
			cookie := h.openAppSession(c.app)
			path := uiRelayPrefix(h.run.ID, c.app) + "/"
			if rec := uiGet(h, path, cookie); rec.Code != http.StatusOK {
				t.Fatalf("first request: %d %s", rec.Code, rec.Body.String())
			}
			set(types.GovernanceLimits{DenyInteractive: true})
			h.clock.advance(uiReassertInterval + time.Second)
			rec := uiGet(h, path, cookie)
			if c.refused && (rec.Code != http.StatusForbidden || respondedReason(t, rec) != string(authz.ReasonGovernanceUnit)) {
				t.Fatalf("re-checked: %d %s, want 403 %s", rec.Code, rec.Body.String(), authz.ReasonGovernanceUnit)
			}
			if !c.refused && rec.Code != http.StatusOK {
				t.Fatalf("re-checked: %d %s, want 200", rec.Code, rec.Body.String())
			}
		})
	}
}

// A limit set during a sign-in refuses the display, and the window's end still gives the agent back with no one
// asking: the sweep closes the person's side, resumes the agent and closes the row. (End under the limit is above.)
func TestSigninDoor_LimitMidSigninRefusesTheDisplayAndTheAgentComesBack(t *testing.T) {
	h, rn, st, set := signinDoorGateway(t, "")
	cookie := h.openAppSession(desktopSigninApp)
	path := uiRelayPrefix(h.run.ID, desktopSigninApp) + "/"
	set(types.GovernanceLimits{DenyInteractive: true})
	h.clock.advance(uiReassertInterval + time.Second)
	if rec := uiGet(h, path, cookie); rec.Code != http.StatusForbidden || respondedReason(t, rec) != string(authz.ReasonGovernanceUnit) {
		t.Fatalf("display under the limit: %d %s, want 403 %s", rec.Code, rec.Body.String(), authz.ReasonGovernanceUnit)
	}
	h.clock.advance(desktopSigninWindow)
	if err := h.srv.sweepDesktopSignins(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got, want := rn.seen(), []string{"person:stop", "agent:resume-agent", "signal:SIGCONT"}; !slices.Equal(got, want) {
		t.Fatalf("calls = %v, want %v", got, want)
	}
	if si := st.only(t); si.EndedAt == nil || si.EndReason != signinEndExpired {
		t.Fatalf("row = %+v, want ended %s", si, signinEndExpired)
	}
}
