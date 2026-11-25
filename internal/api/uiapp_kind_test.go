// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// kindHarness is the UI harness with the run declaring apps, the desktop kind under a name that is not novnc among them.
func kindHarness(t *testing.T, backend http.Handler, apps ...types.UIApp) *uiHarness {
	t.Helper()
	h := newUIHarness(t, backend)
	h.store.putEffectivePolicy(h.run.ID, types.RunPolicySpec{MinConfinementClass: types.CC2, UIApps: apps})
	return h
}

// An app declared with kind desktop is relayed as the desktop is whatever its name: no extension offer reaches the
// sandbox, the desktop's copy out is dropped under the default rules, and the transfer is audited for the app.
func TestUIGateway_ClipboardFiltersADesktopKindAppWhateverItsName(t *testing.T) {
	offered, pasted := make(chan string, 1), make(chan []byte, 1)
	h := kindHarness(t, rfbDesktopBackend(t, offered, pasted, false),
		types.UIApp{Name: "code", Port: uiTestPort, Path: "/ide"},
		types.UIApp{Name: "desk", Port: uiTestPort, Kind: types.UIAppKindDesktop})
	ds := openDesktop(t, h, "desk")
	if got := <-offered; got != "" {
		t.Fatalf("the sandbox saw a WebSocket extension offer %q", got)
	}
	if next, err := readWSFrame(ds.br); err != nil || len(next) != 1 || next[0] != 2 {
		t.Fatalf("after ServerInit the browser got %q (err %v), want only the Bell: the copy out must be dropped", next, err)
	}
	rows := clipboardRows(t, h, 1)
	if len(rows) == 0 {
		t.Fatal("no ui.clipboard.transfer row for the app")
	}
	for _, row := range rows {
		var d struct {
			App string `json:"app"`
		}
		_ = json.Unmarshal(row.Data, &d)
		if d.App != "desk" {
			t.Errorf("ui.clipboard.transfer row names app %q, want desk: %s", d.App, row.Data)
		}
	}
}

// The negative control: the kind, not the name or the port, switches the filter on.
func TestUIGateway_ClipboardLeavesADeskWithoutTheKindUnfiltered(t *testing.T) {
	offered, pasted := make(chan string, 1), make(chan []byte, 1)
	h := kindHarness(t, rfbDesktopBackend(t, offered, pasted, false),
		types.UIApp{Name: "code", Port: uiTestPort, Path: "/ide"},
		types.UIApp{Name: "desk", Port: uiTestPort})
	ds := openDesktop(t, h, "desk")
	if got := <-offered; got == "" {
		t.Fatal("the relay stripped the extension offer of an app that is not a desktop")
	}
	if next, err := readWSFrame(ds.br); err != nil || !strings.Contains(string(next), copySentinel) {
		t.Fatalf("an app without the kind was filtered: %q %v", next, err)
	}
	if rows := clipboardRows(t, h, 0); len(rows) != 0 {
		t.Fatalf("an app without the kind wrote ui.clipboard.transfer rows: %+v", rows)
	}
}

// kindSupportRig is a support rig whose run declares a web app ("code") and a desktop-kind app ("desk"), with a
// helper's grant for doors.
func kindSupportRig(t *testing.T, doors ...string) *supportUIRig {
	t.Helper()
	rig := newSupportUIRig(t, doors...)
	rig.store.events = nil
	rig.store.putEffectivePolicy(rig.run.ID, types.RunPolicySpec{MinConfinementClass: types.CC2,
		UIApps: []types.UIApp{{Name: "code", Port: uiTestPort, Path: "/ide"}, {Name: "desk", Port: uiTestPort, Kind: types.UIAppKindDesktop}}})
	return rig
}

// setDenyInteractive puts the run under a governance profile that denies interactive doors.
func setDenyInteractive(rig *supportUIRig) {
	unit := types.GovernanceUnit{ID: uuid.New(), Name: "ci", Limits: types.GovernanceLimits{DenyInteractive: true}}
	rig.store.profiles = []types.GovernanceUnit{unit}
	rig.run.UnitID = &unit.ID
	rig.store.putRun(rig.run)
}

// A helper opens an app of kind desktop only through both doors: uiTicketMayEnter asks the web_apps door by name, and
// the gateway asks the desktop door once the policy has named the app. deny_interactive refuses it as it does the desktop.
func TestSupportUI_DesktopKindAppNeedsBothDoors(t *testing.T) {
	mismatch := reasonUIGatewayTicketRunMismatch
	for _, tc := range []struct {
		name  string
		doors []string
		want  int
	}{
		{"web apps alone", []string{"web_apps"}, http.StatusForbidden},
		{"desktop alone", []string{"desktop"}, http.StatusForbidden},
		{"both", []string{"web_apps", "desktop"}, http.StatusFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := kindSupportRig(t, tc.doors...)
			rec := rig.enterApp(rig.helperTicket(), "desk")
			if rec.Code != tc.want || (tc.want == http.StatusForbidden && respondedReason(t, rec) != mismatch) {
				t.Errorf("enter desk = %d %s, want %d %s", rec.Code, rec.Body, tc.want, mismatch)
			}
		})
	}
	t.Run("deny_interactive", func(t *testing.T) {
		rig := kindSupportRig(t, "web_apps", "desktop")
		setDenyInteractive(rig)
		governance := string(authz.ReasonGovernanceUnit)
		if rec := rig.enterApp(rig.helperTicket(), "desk"); rec.Code != http.StatusForbidden || respondedReason(t, rec) != governance {
			t.Errorf("enter desk under deny_interactive = %d %s, want 403 %s", rec.Code, rec.Body, governance)
		}
		if rec := rig.enterApp(rig.helperTicket(), "code"); rec.Code != http.StatusFound {
			t.Errorf("enter code under deny_interactive = %d %s, want %d", rec.Code, rec.Body, http.StatusFound)
		}
	})
}

// A session's three questions about its app answer for a declared desktop kind as they do for novnc, and the re-check
// ends the session when deny_interactive is set after it opened.
func TestUISession_DesktopKindMethods(t *testing.T) {
	for _, tc := range []struct {
		sess uiSession
		want bool
	}{
		{uiSession{App: "desk", DesktopKind: true}, true},
		{uiSession{App: "desk"}, false},
		{uiSession{App: "novnc"}, true},
	} {
		door := supportDoorWebApps
		if tc.want {
			door = supportDoorDesktop
		}
		if got := tc.sess.desktopApp(); got != tc.want {
			t.Errorf("%+v desktopApp = %v, want %v", tc.sess, got, tc.want)
		}
		if got := tc.sess.desktopSession(); got != tc.want {
			t.Errorf("%+v desktopSession = %v, want %v", tc.sess, got, tc.want)
		}
		if got := tc.sess.supportDoor(); got != door {
			t.Errorf("%+v supportDoor = %q, want %q", tc.sess, got, door)
		}
	}

	rig := kindSupportRig(t, "web_apps", "desktop")
	cookie := rig.helperSession("desk")
	if rec := rig.get("desk", "/", cookie); rec.Code != http.StatusOK {
		t.Fatalf("first request = %d %s", rec.Code, rec.Body)
	}
	setDenyInteractive(rig)
	rig.clock.advance(uiReassertInterval + time.Second)
	governance := string(authz.ReasonGovernanceUnit)
	if rec := rig.get("desk", "/", cookie); rec.Code != http.StatusForbidden || respondedReason(t, rec) != governance {
		t.Fatalf("re-checked under deny_interactive = %d %s, want 403 %s", rec.Code, rec.Body, governance)
	}
}

// A declared desktop never renews through an issuer portal's chain: its session ends with the portal grant, and any
// other app keeps the ticket's chain. The kind survives the signed cookie.
func TestNewDeclaredUISession_DesktopKindNeverRenews(t *testing.T) {
	h := kindHarness(t, http.NotFoundHandler(), types.UIApp{Name: "code", Port: uiTestPort})
	now := h.clock.now()
	chain := uuid.New()
	ta := ticketActor{
		principal: h.owner, role: "user", authorizedAt: now, renewChain: chain,
		via: &types.DelegationVia{Delegate: uuid.New(), Grant: uuid.New()}, grantExpires: now.Add(time.Minute),
	}
	desk := h.srv.newDeclaredUISession(h.run, ta, types.UIApp{Name: "desk", Port: 5900, Kind: types.UIAppKindDesktop})
	if desk.Chain != uuid.Nil || desk.Expires != ta.grantExpires.Unix() || !desk.DesktopKind {
		t.Errorf("a desktop kind session: chain %v, expires %d (want the grant's %d), DesktopKind %v", desk.Chain, desk.Expires, ta.grantExpires.Unix(), desk.DesktopKind)
	}
	code := h.srv.newDeclaredUISession(h.run, ta, types.UIApp{Name: "code", Port: uiTestPort})
	if code.Chain != chain || code.DesktopKind {
		t.Errorf("a plain app's session: chain %v (want the ticket's %v), DesktopKind %v", code.Chain, chain, code.DesktopKind)
	}

	plain := ticketActor{principal: h.owner, role: "user", authorizedAt: now}
	sess := h.srv.newDeclaredUISession(h.run, plain, types.UIApp{Name: "desk", Port: 5900, Kind: types.UIAppKindDesktop})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: uiCookieName, Value: h.srv.encodeUISession(sess)})
	got, ok := h.srv.decodeUISession(req, now)
	if !ok || !got.DesktopKind {
		t.Fatalf("the session did not survive its cookie with DesktopKind: ok=%v %+v", ok, got)
	}
}

// A desktop kind app is recorded as the desktop is, and an inbound call to it carries the kind into its session.
func TestDesktopKind_RecordedAndInbound(t *testing.T) {
	srv := New(Config{Audit: &recRecorder{}, AdminToken: adminToken, RecordingStore: fsRecordingStore(t)})
	run := types.AgentRun{ID: uuid.New(), RecordingLevel: types.RecordingScreenActions}
	novnc := srv.recordsDesktopApp(run, uiSession{App: desktopApp})
	if !novnc {
		t.Fatal("the novnc app is not recorded on a run the server records")
	}
	if got := srv.recordsDesktopApp(run, uiSession{App: "desk", DesktopKind: true}); got != novnc {
		t.Errorf("a desktop kind app is recorded = %v, want %v as novnc", got, novnc)
	}
	if srv.recordsDesktopApp(run, uiSession{App: "desk"}) {
		t.Error("an app without the kind is recorded as a desktop")
	}

	h, _ := inboundHarness(t)
	h.store.putEffectivePolicy(h.run.ID, types.RunPolicySpec{MinConfinementClass: types.CC2, DataOut: types.TransferAllowed,
		UIApps: []types.UIApp{
			{Name: "desk", Port: uiTestPort, Inbound: true, Kind: types.UIAppKindDesktop},
			{Name: "api", Port: uiTestPort + 1, Inbound: true},
		}})
	token := verifiedAPIToken{APIToken: types.APIToken{ID: uuid.New(), Principal: h.owner, Email: h.owner + "@example.com",
		Kind: types.APITokenKindInbound, CreatedAt: time.Now().Add(-time.Hour)}}
	for app, want := range map[string]bool{"desk": true, "api": false} {
		sess, deny, _ := h.srv.inboundSessionFor(context.Background(), h.run.ID, app, token)
		if deny != "" || sess.DesktopKind != want {
			t.Errorf("inbound %s: deny %q, DesktopKind %v, want none and %v", app, deny, sess.DesktopKind, want)
		}
	}
}

// The kind adds no data_out exemption: only the novnc name is exempt, so a desktop kind app under another name is
// refused like any other web app.
func TestDesktopKind_NoDataOutExemption(t *testing.T) {
	h, _ := inboundHarness(t)
	token := verifiedAPIToken{APIToken: types.APIToken{ID: uuid.New(), Principal: h.owner, Email: h.owner + "@example.com",
		Kind: types.APITokenKindInbound, CreatedAt: time.Now().Add(-time.Hour)}}
	for _, rule := range []types.TransferRule{types.TransferReview, types.TransferBlocked} {
		h.store.putEffectivePolicy(h.run.ID, types.RunPolicySpec{MinConfinementClass: types.CC2, DataOut: rule,
			UIApps: []types.UIApp{{Name: "desk", Port: uiTestPort, Inbound: true, Kind: types.UIAppKindDesktop}}})
		if _, deny, got := h.srv.inboundSessionFor(context.Background(), h.run.ID, "desk", token); deny != inboundDenyDataOut || got != rule {
			t.Errorf("inbound desk under %s: deny %q, rule %q, want %q", rule, deny, got, inboundDenyDataOut)
		}
	}
}
