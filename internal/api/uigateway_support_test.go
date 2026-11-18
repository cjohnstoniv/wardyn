// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A support helper at the UI gateway (uigateway_support.go) and at the Files panel (desktopRun): the ticket the
// helper redeems, the re-check of the session it opens, the context watch that ends a request already being
// relayed, and the Files panel's entry. The grant record is fakeSupportGrants, so the clock is the test's.

// supportGrantedStore is a test server's store with the support grant record on top.
type supportGrantedStore struct {
	store.Store
	*fakeSupportGrants
}

// grantSupport gives srv the grant record over the store it has, switches support sessions on, and accepts a
// grant for helper on run with doors (owner is the run's creator).
func grantSupport(t *testing.T, srv *Server, run types.AgentRun, owner, helper string, doors ...string) (*fakeSupportGrants, store.SupportGrant) {
	t.Helper()
	t.Setenv(envSupportSessions, "1")
	base := srv.cfg.Store
	grants := newFakeSupportGrants(func(id uuid.UUID) (types.AgentRun, bool) {
		r, err := base.GetRun(context.Background(), id)
		return r, err == nil
	})
	srv.cfg.Store = &supportGrantedStore{Store: base, fakeSupportGrants: grants}
	ctx := context.Background()
	g, err := grants.RequestSupportGrant(ctx, store.SupportGrant{RunID: run.ID, Helper: helper, HelperEmail: helper + "@example.test", Reason: "r", DoorsAsked: allSupportDoors})
	if err != nil {
		t.Fatal(err)
	}
	if g, err = grants.AcceptSupportGrant(ctx, run.ID, g.ID, owner, owner+"@example.test", doors, 30, 0); err != nil {
		t.Fatal(err)
	}
	return grants, g
}

// setSupportWatch makes the context watch tick every every and bound a check by wait, for one test.
func setSupportWatch(t *testing.T, every, wait time.Duration) {
	t.Helper()
	supportWatchEveryNS.Store(int64(every))
	supportWatchWaitNS.Store(int64(wait))
	t.Cleanup(func() { supportWatchEveryNS.Store(0); supportWatchWaitNS.Store(0) })
}

// supportUIRig is the UI gateway in host mode serving a person's run, with the helper's grant when doors are
// given. The run declares a web app ("code") and the noVNC desktop ("novnc").
type supportUIRig struct {
	*uiHarness
	grants *fakeSupportGrants
	grant  store.SupportGrant
}

// supportUIBackend is the sandbox's side: a websockify (a framed RFB hello, then held) on /websockify, an event stream
// on /events, an echoing socket for any other upgrade and a short page for any other request, each held until held closes or its peer leaves.
func supportUIBackend(t *testing.T, held <-chan struct{}) http.Handler {
	t.Helper()
	echo := echoAfterUpgradeBackend(t, held)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/events":
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			for {
				_, _ = io.WriteString(w, "data: tick\n\n")
				w.(http.Flusher).Flush()
				select {
				case <-held:
					return
				case <-r.Context().Done():
					return
				case <-time.After(10 * time.Millisecond):
				}
			}
		case "/websockify":
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				return
			}
			defer conn.Close()
			head := "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n"
			if _, err := conn.Write(append([]byte(head), wsFrame([]byte("RFB 003.008\n"), false)...)); err != nil {
				return
			}
			gone := make(chan struct{})
			go func() { defer close(gone); _, _ = io.Copy(io.Discard, conn) }()
			select {
			case <-held:
			case <-gone:
			}
		default:
			if r.Header.Get("Upgrade") == "" {
				_, _ = io.WriteString(w, "sandbox app")
				return
			}
			echo.ServeHTTP(w, r)
		}
	})
}

func newSupportUIRig(t *testing.T, doors ...string) *supportUIRig {
	t.Helper()
	held := make(chan struct{})
	h := newUIHarness(t, supportUIBackend(t, held))
	t.Cleanup(func() { close(held) }) // registered after the harness's backend, so it runs before the backend's Close waits on its handlers
	h.srv.cfg.UIOriginTemplate = "https://run-{run}.ui.example.com"
	h.store.events = nil // the harness seeded the web app alone; this run declares the desktop too
	h.store.putEffectivePolicy(h.run.ID, types.RunPolicySpec{MinConfinementClass: types.CC2,
		UIApps: []types.UIApp{{Name: "code", Port: uiTestPort, Path: "/ide"}, {Name: "novnc", Port: uiTestPort}}})
	rig := &supportUIRig{uiHarness: h}
	rig.grants, rig.grant = grantSupport(t, h.srv, h.run, h.owner, supportHelperSub, doors...)
	return rig
}

func (rig *supportUIRig) host() string { return "run-" + rig.run.ID.String() + ".ui.example.com" }

// helperTicket is the ticket the mint hands a helper under the rig's grant, put straight into the store.
func (rig *supportUIRig) helperTicket() string {
	rig.t.Helper()
	tk := store.AttachTicket{RunID: rig.run.ID, ActorType: types.ActorHuman, Principal: supportHelperSub, Role: oidc.RoleUser}
	tk.SupportGrant, tk.SupportExpires = supportTicketGrant(rig.grant)
	tok, err := storeAttachTicket(context.Background(), rig.store, tk, oidc.Issue{At: time.Now()})
	if err != nil {
		rig.t.Fatalf("mint a helper's ticket: %v", err)
	}
	return tok
}

// enterApp redeems ticket for app on the run's own host, as the browser that bound it.
func (rig *supportUIRig) enterApp(ticket, app string) *httptest.ResponseRecorder {
	rig.t.Helper()
	q := url.Values{"run": {rig.run.ID.String()}, "app": {app}, "ticket": {ticket}}
	req := rig.bound(httptest.NewRequest(http.MethodGet, uiEnterPath+"?"+q.Encode(), nil), q)
	req.Host = rig.host()
	rec := httptest.NewRecorder()
	rig.gateway.ServeHTTP(rec, req)
	return rec
}

// session enters app and returns the relay cookie, failing unless the enter was admitted.
func (rig *supportUIRig) session(ticket, app string) *http.Cookie {
	rig.t.Helper()
	rec := rig.enterApp(ticket, app)
	if rec.Code != http.StatusFound {
		rig.t.Fatalf("enter %s: %d %s, want it admitted", app, rec.Code, rec.Body)
	}
	cookies := setUICookies(rec)
	if len(cookies) == 0 {
		rig.t.Fatalf("enter %s set no relay cookie", app)
	}
	return cookies[0]
}

func (rig *supportUIRig) helperSession(app string) *http.Cookie {
	rig.t.Helper()
	return rig.session(rig.helperTicket(), app)
}

// get is one relay request on the run's host at path of app.
func (rig *supportUIRig) get(app, path string, cookie *http.Cookie) *httptest.ResponseRecorder {
	rig.t.Helper()
	req := httptest.NewRequest(http.MethodGet, uiRelayPrefix(rig.run.ID, app)+path, nil)
	req.Host = rig.host()
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	rig.gateway.ServeHTTP(rec, req)
	return rec
}

// requireStreamOpen fails when the stream ended inside a few watch ticks: a cut that follows means the cut did it.
func requireStreamOpen(t *testing.T, ended <-chan error) {
	t.Helper()
	select {
	case err := <-ended:
		t.Fatalf("the stream ended before anything cut it: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
}

// openStream opens a long-lived request of kind through the gateway under cookie and answers a channel that
// receives once the stream has ended: "socket" an app's WebSocket, "novnc" the declared desktop's, "events" a
// plain response that never completes.
func (rig *supportUIRig) openStream(kind string, cookie *http.Cookie) <-chan error {
	rig.t.Helper()
	gw := httptest.NewServer(rig.gateway)
	rig.t.Cleanup(gw.Close)
	spec := map[string]struct{ app, path string }{
		"socket": {"code", "/ide"}, "novnc": {"novnc", "/websockify"}, "events": {"code", "/events"}}[kind]
	app, path := spec.app, spec.path
	req, err := http.NewRequest(http.MethodGet, gw.URL+uiRelayPrefix(rig.run.ID, app)+path, nil)
	if err != nil {
		rig.t.Fatal(err)
	}
	req.Host = rig.host()
	req.AddCookie(cookie)
	ended := make(chan error, 1)
	if kind == "events" {
		client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
		resp, err := client.Do(req)
		if err != nil {
			rig.t.Fatalf("open the event stream: %v", err)
		}
		rig.t.Cleanup(func() { _ = resp.Body.Close() })
		go func() {
			_, err := io.Copy(io.Discard, resp.Body)
			ended <- err
		}()
		return ended
	}
	conn, err := net.Dial("tcp", gw.Listener.Addr().String())
	if err != nil {
		rig.t.Fatalf("dial the gateway: %v", err)
	}
	rig.t.Cleanup(func() { _ = conn.Close() })
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	if err := req.Write(conn); err != nil {
		rig.t.Fatalf("write the upgrade: %v", err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil || resp.StatusCode != http.StatusSwitchingProtocols {
		rig.t.Fatalf("relayed upgrade = %v %v, want 101", resp, err)
	}
	go func() {
		_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
		_, err := io.Copy(io.Discard, br)
		ended <- err
	}()
	return ended
}

// wantStreamEnded waits for the stream to end the way a cut does, not by timing out, and answers how it ended.
func wantStreamEnded(t *testing.T, ended <-chan error) error {
	t.Helper()
	select {
	case err := <-ended:
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			t.Fatal("the stream was still open when the client gave up waiting")
		}
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("the stream is still open")
		return nil
	}
}

func uiRowData(t *testing.T, ev types.AuditEvent) map[string]any {
	t.Helper()
	var data map[string]any
	if err := json.Unmarshal(ev.Data, &data); err != nil {
		t.Fatalf("audit row %s: %v", ev.Action, err)
	}
	return data
}

// (a) the door follows the app: a web app opens under web_apps alone, the noVNC desktop under desktop alone.
func TestSupportUI_EnterFollowsTheTickedDoor(t *testing.T) {
	for _, tc := range []struct {
		name        string
		doors       []string
		code, novnc int
	}{
		{"web_apps only", []string{"web_apps"}, http.StatusFound, http.StatusForbidden},
		{"desktop only", []string{"desktop"}, http.StatusForbidden, http.StatusFound},
		{"both", []string{"web_apps", "desktop"}, http.StatusFound, http.StatusFound},
		{"neither", []string{"terminal", "files"}, http.StatusForbidden, http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newSupportUIRig(t, tc.doors...)
			admitted := 0
			for app, want := range map[string]int{"code": tc.code, "novnc": tc.novnc} {
				if rec := rig.enterApp(rig.helperTicket(), app); rec.Code != want {
					t.Errorf("enter %s = %d %s, want %d", app, rec.Code, rec.Body, want)
				} else if want == http.StatusFound {
					admitted++
				}
			}
			rows := rig.audit.auditRows("ui.authorize", "success")
			if len(rows) != admitted {
				t.Fatalf("%d ui.authorize success rows, want %d (one per admitted enter)", len(rows), admitted)
			}
			for _, ev := range rows {
				d := uiRowData(t, ev)
				if d["support_grant"] != rig.grant.ID.String() || d["entry"] != supportEntryHelper || ev.Actor != supportHelperSub {
					t.Errorf("success row %v by %s, want the grant %s, entry support and the helper as actor", d, ev.Actor, rig.grant.ID)
				}
			}
		})
	}
}

// (a) no grant, an ended one, one that ran out and the switch off all leave the owner rule's refusal at the enter.
func TestSupportUI_NoLiveGrantIsTheOwnerRulesRefusal(t *testing.T) {
	for _, tc := range []struct {
		name   string
		ticket func(*supportUIRig) string
		cut    func(*supportUIRig)
	}{
		{"a ticket with no grant", func(r *supportUIRig) string { return r.ticket(r.run.ID, supportHelperSub, oidc.RoleUser) }, func(*supportUIRig) {}},
		{"the grant ended after the mint", (*supportUIRig).helperTicket, func(r *supportUIRig) { r.endGrant("revoked") }},
		{"the grant ran out after the mint", (*supportUIRig).helperTicket, func(r *supportUIRig) { r.grants.tick(31 * time.Minute) }},
		{"the switch went off after the mint", (*supportUIRig).helperTicket, func(r *supportUIRig) { r.t.Setenv(envSupportSessions, "") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newSupportUIRig(t, "web_apps", "desktop")
			ticket := tc.ticket(rig)
			tc.cut(rig)
			if rec := rig.enterApp(ticket, "code"); rec.Code != http.StatusForbidden {
				t.Fatalf("enter = %d %s, want 403", rec.Code, rec.Body)
			}
			if rows := rig.audit.auditRows("ui.authorize", "success"); len(rows) != 0 {
				t.Fatalf("an authorize success row was written: %+v", rows)
			}
		})
	}
}

func (rig *supportUIRig) endGrant(reason string) {
	rig.t.Helper()
	if _, err := rig.grants.EndSupportGrant(context.Background(), rig.run.ID, rig.grant.ID, reason, true); err != nil {
		rig.t.Fatal(err)
	}
}

// (b) belt: a helper's ticket is refused at the enter on a deployment serving apps from one shared host, even
// though the mint (support_terminal.go, covered by TestSupportTerminalMintRefusals) refuses it first.
func TestSupportUI_PathModeRefusesTheEnter(t *testing.T) {
	rig := newSupportUIRig(t, "web_apps", "desktop")
	rig.srv.cfg.UIOriginTemplate = ""
	for _, app := range []string{"code", "novnc"} {
		q := url.Values{"run": {rig.run.ID.String()}, "app": {app}, "ticket": {rig.helperTicket()}}
		if rec := rig.enter(q); rec.Code != http.StatusForbidden {
			t.Fatalf("enter %s in path mode = %d %s, want 403", app, rec.Code, rec.Body)
		}
	}
}

// (c) H1: the helper is governed whatever GovernAdminRuns says (off here): deny_ui_apps refuses a web app,
// deny_interactive the desktop and not a web app, at the enter and at the next re-check.
func TestSupportUI_GovernanceHasNoAdminExemption(t *testing.T) {
	setLimits := func(rig *supportUIRig, limits types.GovernanceLimits) {
		unit := types.GovernanceUnit{ID: uuid.New(), Name: "ci", Limits: limits}
		rig.store.profiles = []types.GovernanceUnit{unit}
		rig.run.UnitID = &unit.ID
		rig.store.putRun(rig.run)
	}
	governance := string(authz.ReasonGovernanceUnit)
	for _, tc := range []struct {
		name        string
		limits      types.GovernanceLimits
		code, novnc int
	}{
		{"deny_ui_apps", types.GovernanceLimits{DenyUIApps: true}, http.StatusForbidden, http.StatusForbidden},
		{"deny_interactive", types.GovernanceLimits{DenyInteractive: true}, http.StatusFound, http.StatusForbidden},
		{"neither", types.GovernanceLimits{}, http.StatusFound, http.StatusFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newSupportUIRig(t, "web_apps", "desktop")
			setLimits(rig, tc.limits)
			for app, want := range map[string]int{"code": tc.code, "novnc": tc.novnc} {
				rec := rig.enterApp(rig.helperTicket(), app)
				if rec.Code != want || (want == http.StatusForbidden && respondedReason(t, rec) != governance) {
					t.Errorf("enter %s = %d %s, want %d %s", app, rec.Code, rec.Body, want, governance)
				}
			}
		})
	}
	t.Run("a profile edit ends an open session", func(t *testing.T) {
		rig := newSupportUIRig(t, "web_apps")
		cookie := rig.helperSession("code")
		if rec := rig.get("code", "/ide", cookie); rec.Code != http.StatusOK {
			t.Fatalf("first request = %d %s", rec.Code, rec.Body)
		}
		setLimits(rig, types.GovernanceLimits{DenyUIApps: true})
		rig.clock.advance(uiReassertInterval + time.Second)
		if rec := rig.get("code", "/ide", cookie); rec.Code != http.StatusForbidden || respondedReason(t, rec) != governance {
			t.Fatalf("re-checked under deny_ui_apps = %d %s, want 403 %s", rec.Code, rec.Body, governance)
		}
	})
}

// The session's rows name the grant, and the owner's own rows do not.
func TestSupportUI_RowsNameTheGrant(t *testing.T) {
	rig := newSupportUIRig(t, "web_apps")
	cookie := rig.helperSession("code")
	if rec := rig.get("code", "/ide", cookie); rec.Code != http.StatusOK {
		t.Fatalf("relay = %d %s", rec.Code, rec.Body)
	}
	owner := rig.session(rig.ticket(rig.run.ID, rig.owner, oidc.RoleUser), "code")
	if rec := rig.get("code", "/ide", owner); rec.Code != http.StatusOK {
		t.Fatalf("owner's relay = %d %s", rec.Code, rec.Body)
	}
	named := map[string]int{}
	for _, ev := range rig.audit.snapshot() {
		if !strings.HasPrefix(ev.Action, "ui.") {
			continue
		}
		d := uiRowData(t, ev)
		if _, has := d["support_grant"]; has != (ev.Actor == supportHelperSub) {
			t.Errorf("%s by %s: support_grant present = %v", ev.Action, ev.Actor, has)
		}
		if has := d["entry"] == supportEntryHelper; has != (ev.Actor == supportHelperSub) {
			t.Errorf("%s by %s: entry support = %v", ev.Action, ev.Actor, has)
		}
		if ev.Actor == supportHelperSub {
			named[ev.Action]++
		}
	}
	if named["ui.authorize"] == 0 || named["ui.open"] == 0 {
		t.Fatalf("helper rows = %v, want a ui.authorize and a ui.open row naming the grant", named)
	}
}

// The re-check of an open session asks the arm again: a revoke or an expiry is a 403, a grant that cannot be read
// is a 503 (fail closed), and each writes a ui.authorize denial naming the grant.
func TestSupportUI_RecheckEndsASessionWhenTheGrantStops(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cut    func(*supportUIRig)
		status int
		reason string
	}{
		{"revoked", func(r *supportUIRig) { r.endGrant("revoked") }, http.StatusForbidden, uiDeniedReasonNotAuthorized},
		{"expired", func(r *supportUIRig) { r.grants.tick(31 * time.Minute) }, http.StatusForbidden, uiDeniedReasonNotAuthorized},
		{"unreadable", func(r *supportUIRig) { r.grants.failOn("SupportGrantLive", errors.New("db down")) }, http.StatusServiceUnavailable, uiDeniedReasonRevocationUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newSupportUIRig(t, "web_apps")
			cookie := rig.helperSession("code")
			if rec := rig.get("code", "/ide", cookie); rec.Code != http.StatusOK {
				t.Fatalf("first request = %d %s", rec.Code, rec.Body)
			}
			tc.cut(rig)
			rig.clock.advance(uiReassertInterval + time.Second)
			if rec := rig.get("code", "/ide", cookie); rec.Code != tc.status || respondedReason(t, rec) != tc.reason {
				t.Fatalf("re-checked request = %d %s, want %d %s", rec.Code, rec.Body, tc.status, tc.reason)
			}
			rows := rig.audit.auditRows("ui.authorize", "denied")
			if len(rows) != 1 || uiRowData(t, rows[0])["support_grant"] != rig.grant.ID.String() {
				t.Fatalf("denied rows = %+v, want one naming the grant", rows)
			}
		})
	}
}

// (d) (e) H3: a relayed request already being served ends within a watch tick of a revoke, of an expiry and of
// a grant that cannot be read: an app's WebSocket, the declared noVNC desktop's socket and a plain response
// that never completes (check-2 M7).
func TestSupportUI_WatchEndsRelayedStreams(t *testing.T) {
	for _, kind := range []string{"socket", "novnc", "events"} {
		for _, cut := range []struct {
			name string
			do   func(*supportUIRig)
		}{
			{"revoke", func(r *supportUIRig) { r.endGrant("revoked") }},
			{"expiry", func(r *supportUIRig) { r.grants.tick(31 * time.Minute) }},
			{"unreadable grant", func(r *supportUIRig) { r.grants.failOn("SupportGrantLive", errors.New("db down")) }},
		} {
			t.Run(kind+"/"+cut.name, func(t *testing.T) {
				setSupportWatch(t, 20*time.Millisecond, 2*time.Second)
				rig := newSupportUIRig(t, "web_apps", "desktop")
				app := map[string]string{"socket": "code", "novnc": "novnc", "events": "code"}[kind]
				ended := rig.openStream(kind, rig.helperSession(app))
				requireStreamOpen(t, ended)
				cut.do(rig)
				if err := wantStreamEnded(t, ended); kind == "events" && err == nil {
					t.Fatal("the event stream ended as if it had completed; a cut must abort it so the client sees a broken stream")
				}
			})
		}
	}
	t.Run("a run that cannot be read ends the stream", func(t *testing.T) {
		setSupportWatch(t, 20*time.Millisecond, 2*time.Second)
		rig := newSupportUIRig(t, "web_apps")
		ended := rig.openStream("events", rig.helperSession("code"))
		requireStreamOpen(t, ended)
		rig.store.dropRun(rig.run.ID)
		_ = wantStreamEnded(t, ended)
	})
}

// check-2 M7: an owner's relayed request is not watched, and neither is any session without a grant: the
// context comes back as it went in and the grant record is never read.
func TestSupportUI_OwnersStreamIsNotWatched(t *testing.T) {
	setSupportWatch(t, 20*time.Millisecond, 2*time.Second)
	rig := newSupportUIRig(t, "web_apps")
	parent := context.Background()
	if ctx, stop := rig.srv.supportWatchCtx(parent, uiSession{}); ctx != parent {
		t.Fatal("a session with no grant got a derived context")
	} else {
		stop()
	}
	owner := rig.session(rig.ticket(rig.run.ID, rig.owner, oidc.RoleUser), "code")
	ended := rig.openStream("events", owner)
	time.Sleep(300 * time.Millisecond) // fifteen ticks, had it been watched
	select {
	case err := <-ended:
		t.Fatalf("the owner's stream ended: %v", err)
	default:
	}
	rig.grants.mu.Lock()
	defer rig.grants.mu.Unlock()
	if len(rig.grants.lives) != 0 {
		t.Fatalf("the grant record was read %d times for an owner's stream", len(rig.grants.lives))
	}
}

// (f) the relay cookie stops at the grant's expiry when that is sooner than the session TTL, and at the TTL when
// that is sooner.
func TestSupportUI_CookieExpiresWithTheGrant(t *testing.T) {
	rig := newSupportUIRig(t, "web_apps")
	cookie := rig.helperSession("code")
	if got, want := cookie.Expires.Unix(), rig.grant.ExpiresAt.Unix(); got != want {
		t.Fatalf("cookie Expires = %d, want the grant's expires_at %d", got, want)
	}
	rig.srv.cfg.UISessionTTL = 10 * time.Minute
	cookie = rig.helperSession("code")
	if got, want := cookie.Expires.Unix(), rig.clock.now().Add(10*time.Minute).Unix(); got != want {
		t.Fatalf("cookie Expires = %d under a 10 minute TTL, want %d", got, want)
	}
}

// (g) the VM desktop's stream asks desktopRunCheck every tick, which asks uiSessionStillAuthorized: a revoke, an
// expiry or a grant without the desktop door closes it as not authorized.
func TestSupportUI_VMDesktopClosesThroughItsRecheck(t *testing.T) {
	for _, tc := range []struct {
		name  string
		doors []string
		cut   func(*supportUIRig)
	}{
		{"revoke", []string{"desktop"}, func(r *supportUIRig) { r.endGrant("revoked") }},
		{"expiry", []string{"desktop"}, func(r *supportUIRig) { r.grants.tick(31 * time.Minute) }},
		{"the desktop door is not ticked", []string{"web_apps"}, func(*supportUIRig) {}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newSupportUIRig(t, tc.doors...)
			now := rig.clock.now().Unix()
			sess := uiSession{Run: rig.run.ID, App: uiDesktopAppName, Principal: supportHelperSub, Role: oidc.RoleUser, Desktop: true,
				IssuedAt: now, AuthorizedAt: now, SupportGrant: rig.grant.ID}
			_, code, why := rig.srv.desktopRunCheck(context.Background(), sess)
			if (tc.name == "the desktop door is not ticked") != (code == desktopCloseNotAuthorized) {
				t.Fatalf("before the cut: %d %s", code, why)
			}
			tc.cut(rig)
			if _, code, why = rig.srv.desktopRunCheck(context.Background(), sess); code != desktopCloseNotAuthorized {
				t.Fatalf("after the cut: %d %s, want %d", code, why, desktopCloseNotAuthorized)
			}
		})
	}
}

// uiSupportDoor names the door by app kind: the desktop for the VM's display, a declared noVNC and the
// sign-in display; web_apps for the rest.
func TestSupportUI_DoorByApp(t *testing.T) {
	for app, want := range map[string]supportDoor{
		uiDesktopAppName: supportDoorDesktop, "novnc": supportDoorDesktop, desktopSigninApp: supportDoorDesktop,
		"code": supportDoorWebApps, "": supportDoorWebApps,
	} {
		if got := uiSupportDoor(app); got != want {
			t.Errorf("uiSupportDoor(%q) = %q, want %q", app, got, want)
		}
	}
}

// supportFilesEnv is the Files panel over a person's run with the helper's grant: the env's super admin is the
// helper. limits, when set, is the governance profile the run was launched under.
func supportFilesEnv(t *testing.T, desktop *types.DesktopControls, limits *types.GovernanceLimits, doors ...string) (*desktopFilesEnv, *fakeSupportGrants, store.SupportGrant) {
	t.Helper()
	e := newDesktopFilesEnv(t, desktop)
	if limits != nil {
		unit := types.GovernanceUnit{ID: uuid.New(), Name: "ci", Limits: *limits}
		e.srv.cfg.Store = &desktopDoorUnitStore{envelopeStore: e.srv.cfg.Store.(*envelopeStore), unit: unit}
	}
	grants, g := grantSupport(t, e.srv, e.run, entryOwnerSub, "sub-admin", doors...)
	return e, grants, g
}

// (c) (h) the Files panel under a grant: the files door admits listing and upload, any other door refuses, no
// grant, an ended one or the switch off keep the owner rule's refusal, and a profile that denies interactive
// sessions or UI apps closes it for the helper (no exemption, GovernAdminRuns off).
func TestSupportFiles_DoorAndGovernance(t *testing.T) {
	allowed := &types.DesktopControls{Upload: types.TransferAllowed, Download: types.TransferAllowed}
	for _, tc := range []struct {
		name   string
		doors  []string
		limits *types.GovernanceLimits
		setup  func(*desktopFilesEnv, *fakeSupportGrants, store.SupportGrant, *testing.T)
		status int
		reason string
	}{
		{"files ticked", []string{"files"}, nil, nil, http.StatusOK, ""},
		{"files not ticked", []string{"terminal", "desktop"}, nil, nil, http.StatusForbidden, string(authz.ReasonSupportDoorNotGranted)},
		{"the grant ended", []string{"files"}, nil, func(e *desktopFilesEnv, g *fakeSupportGrants, gr store.SupportGrant, t *testing.T) {
			if _, err := g.EndSupportGrant(context.Background(), e.run.ID, gr.ID, "revoked", true); err != nil {
				t.Fatal(err)
			}
		}, http.StatusForbidden, runOwnerOnlyReason},
		{"the grant ran out", []string{"files"}, nil, func(_ *desktopFilesEnv, g *fakeSupportGrants, _ store.SupportGrant, _ *testing.T) {
			g.tick(31 * time.Minute)
		}, http.StatusForbidden, runOwnerOnlyReason},
		{"the switch is off", []string{"files"}, nil, func(_ *desktopFilesEnv, _ *fakeSupportGrants, _ store.SupportGrant, t *testing.T) {
			t.Setenv(envSupportSessions, "")
		}, http.StatusForbidden, runOwnerOnlyReason},
		{"the grant cannot be read", []string{"files"}, nil, func(_ *desktopFilesEnv, g *fakeSupportGrants, _ store.SupportGrant, _ *testing.T) {
			g.failOn("SupportGrantLive", errors.New("db down"))
		}, http.StatusServiceUnavailable, reasonSupportUnavailable},
		{"deny_interactive", []string{"files"}, &types.GovernanceLimits{DenyInteractive: true}, nil, http.StatusForbidden, string(authz.ReasonGovernanceUnit)},
		{"deny_ui_apps", []string{"files"}, &types.GovernanceLimits{DenyUIApps: true}, nil, http.StatusForbidden, string(authz.ReasonGovernanceUnit)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, grants, grant := supportFilesEnv(t, allowed, tc.limits, tc.doors...)
			if tc.setup != nil {
				tc.setup(e, grants, grant, t)
			}
			helper := e.ses["admin"]
			for _, r := range []struct{ method, name, body string }{{http.MethodGet, "", ""}, {http.MethodPut, "in.txt", "x"}} {
				w := doSSO(t, e.srv, r.method, e.path(r.name), helper, r.body)
				if w.Code != tc.status || (tc.reason != "" && reasonOf(t, w) != tc.reason) {
					t.Errorf("%s %q = %d %s, want %d %s", r.method, r.name, w.Code, w.Body, tc.status, tc.reason)
				}
			}
			if tc.status != http.StatusOK && e.folder.execs != 0 {
				t.Errorf("a refused request reached the sandbox (%d execs)", e.folder.execs)
			}
		})
	}
}

// (h) a download under a reviewed rule still asks for review when a helper holds the files door: the helper's
// plain GET is refused with nothing sent, and asking raises the one approval, under the helper's name. Deciding
// it is the review lane's.
func TestSupportFiles_ReviewedDownloadStillAsksForReview(t *testing.T) {
	e := newReviewEnv(t, types.TransferReview)
	_, _ = grantSupport(t, e.srv, e.run, entryOwnerSub, "sub-admin", "files")
	helper := e.ses["admin"]
	content := string(e.folder.files[reviewFile])
	w := doSSO(t, e.srv, http.MethodGet, e.path(reviewFile), helper, "")
	if w.Code != http.StatusForbidden || wireReason(t, w) != reasonDesktopReviewRequired {
		t.Fatalf("a helper's download with no review: %d %s", w.Code, w.Body)
	}
	wantNothingSent(t, "helper's download", w, content)
	w = doSSO(t, e.srv, http.MethodPost, e.path(reviewFile)+"/review", helper, "")
	if w.Code != http.StatusAccepted {
		t.Fatalf("a helper's review request: %d %s", w.Code, w.Body)
	}
	var ap types.ApprovalRequest
	if err := json.Unmarshal(w.Body.Bytes(), &ap); err != nil {
		t.Fatal(err)
	}
	row := e.row(t, ap.ID)
	sc, ok := types.ParseFileDownloadScope(row.RequestedScope)
	if row.Kind != types.ApprovalFileDownload || row.State != types.ApprovalPending || !ok || sc.RequestedBy != "sub-admin" {
		t.Fatalf("the raised row = %+v (scope %s), want a pending file_download asked by the helper", row, row.RequestedScope)
	}
	w = doSSO(t, e.srv, http.MethodGet, e.path(reviewFile)+"?approval="+ap.ID.String(), helper, "")
	if w.Code != http.StatusForbidden || wireReason(t, w) != reasonDesktopReviewNotApproved {
		t.Fatalf("a release before the approval: %d %s", w.Code, w.Body)
	}
	wantNothingSent(t, "pending release", w, content)
}
