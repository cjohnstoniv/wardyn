// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/runner/remote"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// bgWatchPutRunner puts h's run on a new runner, connected or not, whose caps carry mac.
func bgWatchPutRunner(h *uiHarness, online bool, mac *runnerwire.MacVMCaps) {
	h.t.Helper()
	caps := laptopCaps(types.CC1)
	caps.MacOSVM = mac
	data, err := json.Marshal(caps)
	if err != nil {
		h.t.Fatal(err)
	}
	id := uuid.New()
	sub := remote.New(id.String(), &capsTransport{online: online}, remote.Options{})
	sub.HandleEvent(runnerwire.Event{Kind: runnerwire.EventCaps, Data: data})
	h.srv.runnerStreams.subsMu.Lock()
	if h.srv.runnerStreams.subs == nil {
		h.srv.runnerStreams.subs = map[uuid.UUID]runnerEndpoint{}
	}
	h.srv.runnerStreams.subs[id] = runnerEndpoint{sub: sub, link: &remote.Link{}}
	h.srv.runnerStreams.subsMu.Unlock()
	h.run.RunnerID = &id
	h.store.putRun(h.run)
}

func bgWatchMac(desktop string) *runnerwire.MacVMCaps {
	m := readyMac()
	m.Desktop = desktop
	return m
}

// bgWatchMakeWatchable makes h's run a background one whose helper reported this boot forced.
func bgWatchMakeWatchable(h *uiHarness) {
	h.run.Experience = types.ExperienceBackground
	h.run.VMMachine = &types.VMMachine{DesktopViewOnlyAsked: true, DesktopForcedViewOnly: true}
	h.store.putRun(h.run)
}

// bgWatchHarness is a background macOS VM run that may be watched: forced in this boot, on a connected Mac
// whose caps say observe, with no profile and a default site.
func bgWatchHarness(t *testing.T) (*uiHarness, *desktopSiteStore) {
	t.Helper()
	h, _ := newDesktopHarness(t)
	st := &desktopSiteStore{desktopClosedStore: h.srv.cfg.Store.(*desktopClosedStore)}
	h.srv.cfg.Store = st
	h.run.Agent, h.run.MachineKind = "claude-code", types.MachineMacOSVM
	bgWatchMakeWatchable(h)
	bgWatchPutRunner(h, true, bgWatchMac(runnerwire.MacVMDesktopObserve))
	return h, st
}

// bgWatchMint is POST /runs/{id}/desktop/watch-ticket as the signed-in person sub.
func bgWatchMint(h *uiHarness, sub, role string) *httptest.ResponseRecorder {
	h.t.Helper()
	r := chi.NewRouter()
	h.srv.mountBackgroundWatchRoutes(r)
	req := httptest.NewRequest(http.MethodPost, "/runs/"+h.run.ID.String()+"/desktop/watch-ticket", nil)
	req = req.WithContext(withHumanIdentity(req.Context(), sub, sub+"@example.com", role, types.UserTypeStandard, nil, false))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

// bgWatchTicket stores a ticket with the given stamp, as no route may mint it.
func bgWatchTicket(h *uiHarness, principal, role, app, caller string) string {
	h.t.Helper()
	tok, err := storeAttachTicket(context.Background(), h.store, store.AttachTicket{RunID: h.run.ID, ActorType: types.ActorHuman,
		Principal: principal, Role: role, App: app, Caller: caller}, oidc.Issue{At: time.Now()})
	if err != nil {
		h.t.Fatal(err)
	}
	return tok
}

// bgWatchSession is the session the door's ticket opens on h's run.
func bgWatchSession(h *uiHarness) uiSession {
	now := h.clock.now().Unix()
	return uiSession{Run: h.run.ID, App: uiDesktopAppName, Principal: h.owner, Role: oidc.RoleUser, Desktop: true, ViewOnly: true,
		IssuedAt: now, AuthorizedAt: now}
}

// bgWatchRig is rfbClientNewRig entered through the door, its cookie the watch's.
func bgWatchRig(t *testing.T) *rfbClientRig {
	t.Helper()
	r := rfbClientNewRig(t, runnerwire.MacVMDesktopObserve)
	bgWatchMakeWatchable(r.h)
	r.cookie = desktopCookie(t, r.h.enterDesktop(ticketOf(t, bgWatchMint(r.h, r.h.owner, oidc.RoleUser))))
	return r
}

// bgWatchUnitUnreadable can't read a run's profile.
type bgWatchUnitUnreadable struct{ *desktopSiteStore }

func (bgWatchUnitUnreadable) GetGovernanceUnitChain(context.Context, uuid.UUID) ([]types.GovernanceUnit, error) {
	return nil, errors.New("conn closed by peer")
}

// bgWatchLiveSite is a site config a test changes while a stream reads it.
type bgWatchLiveSite struct {
	*desktopSiteStore
	mu   sync.Mutex
	live types.SiteConfig
}

func (s *bgWatchLiveSite) GetSiteConfig(context.Context) (types.SiteConfig, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.live, nil
}

func (s *bgWatchLiveSite) set(sc types.SiteConfig) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.live = sc
}

// bgWatchArms are every way the door's function refuses a run that was watchable, with the reason each gives.
var bgWatchArms = []struct {
	name   string
	arm    func(h *uiHarness, st *desktopSiteStore)
	reason string
	state  string
}{
	{"the install's switch is off", func(_ *uiHarness, st *desktopSiteStore) {
		off := false
		st.sc = types.SiteConfig{MacOSVM: &types.SiteMacOSVM{AllowBackgroundWatch: &off}}
	}, reasonRunBackgroundWatchSiteOff, backgroundWatchSiteOff},
	{"the site config can't be read", func(_ *uiHarness, st *desktopSiteStore) {
		st.err = errors.New("pool closed")
	}, reasonRunUnreadable, backgroundWatchUnconfirmed},
	{"the limit is set", func(h *uiHarness, _ *desktopSiteStore) {
		walledRun(h)(types.GovernanceLimits{DenyBackgroundDesktopWatch: true})
	}, string(authz.ReasonGovernanceUnit), backgroundWatchGovernance},
	{"deny_ui_apps is set", func(h *uiHarness, _ *desktopSiteStore) {
		walledRun(h)(types.GovernanceLimits{DenyUIApps: true})
	}, string(authz.ReasonGovernanceUnit), backgroundWatchGovernance},
	{"deny_interactive is set", func(h *uiHarness, _ *desktopSiteStore) {
		walledRun(h)(types.GovernanceLimits{DenyInteractive: true})
	}, string(authz.ReasonGovernanceUnit), backgroundWatchGovernance},
	{"the agent's sign-ins in a desktop are off", func(_ *uiHarness, st *desktopSiteStore) {
		st.sc = types.SiteConfig{AgentProviders: &types.AgentProviders{Agents: []types.AgentProvider{{ID: "claude-code", DesktopSignInsOff: true}}}}
	}, reasonDesktopSignInsOff, backgroundWatchSignInsOff},
	{"a boot from before the ask", func(h *uiHarness, _ *desktopSiteStore) {
		h.run.VMMachine = &types.VMMachine{}
		h.store.putRun(h.run)
	}, reasonRunBackgroundWatchOldBoot, backgroundWatchOldBoot},
	{"no machine recorded", func(h *uiHarness, _ *desktopSiteStore) {
		h.run.VMMachine = nil
		h.store.putRun(h.run)
	}, reasonRunBackgroundWatchUnconfirmed, backgroundWatchUnconfirmed},
	{"asked, and the helper did not report forced", func(h *uiHarness, _ *desktopSiteStore) {
		h.run.VMMachine = &types.VMMachine{DesktopViewOnlyAsked: true}
		h.store.putRun(h.run)
	}, reasonRunBackgroundWatchUnconfirmed, backgroundWatchUnconfirmed},
	{"on no runner", func(h *uiHarness, _ *desktopSiteStore) {
		h.run.RunnerID = nil
		h.store.putRun(h.run)
	}, reasonRunBackgroundWatchOffline, backgroundWatchOffline},
	{"a runner this replica holds no session for", func(h *uiHarness, _ *desktopSiteStore) {
		id := uuid.New()
		h.run.RunnerID = &id
		h.store.putRun(h.run)
	}, reasonRunBackgroundWatchOffline, backgroundWatchOffline},
	{"a runner offline, its remembered caps saying observe", func(h *uiHarness, _ *desktopSiteStore) {
		bgWatchPutRunner(h, false, bgWatchMac(runnerwire.MacVMDesktopObserve))
	}, reasonRunBackgroundWatchOffline, backgroundWatchOffline},
	{"caps say control", func(h *uiHarness, _ *desktopSiteStore) {
		bgWatchPutRunner(h, true, bgWatchMac(runnerwire.MacVMDesktopControl))
	}, reasonRunBackgroundWatchControl, backgroundWatchControl},
	{"caps say none", func(h *uiHarness, _ *desktopSiteStore) {
		bgWatchPutRunner(h, true, bgWatchMac(runnerwire.MacVMDesktopNone))
	}, reasonRunBackgroundWatchUnconfirmed, backgroundWatchUnconfirmed},
	{"caps say nothing of the desktop", func(h *uiHarness, _ *desktopSiteStore) {
		bgWatchPutRunner(h, true, bgWatchMac(""))
	}, reasonRunBackgroundWatchUnconfirmed, backgroundWatchUnconfirmed},
	{"caps say what this server doesn't know", func(h *uiHarness, _ *desktopSiteStore) {
		bgWatchPutRunner(h, true, bgWatchMac("later"))
	}, reasonRunBackgroundWatchUnconfirmed, backgroundWatchUnconfirmed},
	{"caps name no macOS VM", func(h *uiHarness, _ *desktopSiteStore) {
		bgWatchPutRunner(h, true, nil)
	}, reasonRunBackgroundWatchUnconfirmed, backgroundWatchUnconfirmed},
	{"paused", func(h *uiHarness, _ *desktopSiteStore) {
		now := h.clock.now()
		h.run.PausedAt = &now
		h.store.putRun(h.run)
	}, reasonRunBackgroundWatchPaused, backgroundWatchPaused},
}

// Every way the door refuses, and the one way it opens. The read says the same as the door, with its sentence.
func TestBgWatchRefusalAndTheRunRead(t *testing.T) {
	t.Run("a watchable run opens", func(t *testing.T) {
		h, _ := bgWatchHarness(t)
		if rf := h.srv.backgroundWatchRefusal(context.Background(), h.run); rf != nil {
			t.Fatalf("refusal = %+v, want it open", rf)
		}
		v := h.srv.backgroundWatchViewFor(context.Background(), h.run, h.owner)
		if v == nil || v.State != backgroundWatchOpen || v.Message != "" {
			t.Fatalf("view = %+v, want open with no sentence", v)
		}
	})
	for _, c := range bgWatchArms {
		t.Run(c.name, func(t *testing.T) {
			h, st := bgWatchHarness(t)
			c.arm(h, st)
			rf := h.srv.backgroundWatchRefusal(context.Background(), h.run)
			if rf == nil || backgroundWatchReasonOf(rf) != c.reason {
				t.Fatalf("refusal = %+v, want %s", rf, c.reason)
			}
			v := h.srv.backgroundWatchViewFor(context.Background(), h.run, h.owner)
			if v == nil || v.State != c.state || v.Message == "" {
				t.Fatalf("view = %+v, want %s with a sentence", v, c.state)
			}
		})
	}
	for _, exp := range doorExperiences {
		t.Run("not a background run: "+exp.name, func(t *testing.T) {
			h, _ := bgWatchHarness(t)
			h.run.Experience = exp.exp
			h.store.putRun(h.run)
			rf := h.srv.backgroundWatchRefusal(context.Background(), h.run)
			if rf == nil || rf.body.Reason != reasonRunBackgroundWatchNotBackground {
				t.Fatalf("refusal = %+v, want %s", rf, reasonRunBackgroundWatchNotBackground)
			}
			if v := h.srv.backgroundWatchViewFor(context.Background(), h.run, h.owner); v != nil {
				t.Fatalf("view = %+v, want none", v)
			}
		})
	}
	for _, kind := range []types.MachineKind{types.MachineLinuxVM, ""} {
		t.Run("not a macOS VM: "+string(kind), func(t *testing.T) {
			h, _ := bgWatchHarness(t)
			h.run.MachineKind = kind
			h.store.putRun(h.run)
			rf := h.srv.backgroundWatchRefusal(context.Background(), h.run)
			if rf == nil || rf.body.Reason != reasonRunBackgroundOnly {
				t.Fatalf("refusal = %+v, want %s", rf, reasonRunBackgroundOnly)
			}
			if v := h.srv.backgroundWatchViewFor(context.Background(), h.run, h.owner); v != nil {
				t.Fatalf("view = %+v, want none", v)
			}
		})
	}
	t.Run("the read says only that it is not this caller's", func(t *testing.T) {
		h, _ := bgWatchHarness(t)
		v := h.srv.backgroundWatchViewFor(context.Background(), h.run, "someone-else")
		if v == nil || v.State != backgroundWatchNotRunOwner || v.Message != "" {
			t.Fatalf("view = %+v, want %s with no sentence", v, backgroundWatchNotRunOwner)
		}
	})
	t.Run("the read is silent on a run that isn't going", func(t *testing.T) {
		h, _ := bgWatchHarness(t)
		h.run.State = types.RunStopped
		if v := h.srv.backgroundWatchViewFor(context.Background(), h.run, h.owner); v != nil {
			t.Fatalf("view of a stopped run = %+v, want none", v)
		}
		now := h.clock.now()
		h.run.State, h.run.LostAt = types.RunRunning, &now
		if v := h.srv.backgroundWatchViewFor(context.Background(), h.run, h.owner); v != nil {
			t.Fatalf("view of a kept run = %+v, want none", v)
		}
	})
}

// Each value of the two predicates that mark a watch.
func TestBgWatchPredicates(t *testing.T) {
	watch := ticketActor{role: oidc.RoleUser, app: uiDesktopAppName}
	for _, c := range []struct {
		name string
		ta   ticketActor
		app  string
		want bool
	}{
		{"the door's ticket on the desktop", watch, uiDesktopAppName, true},
		{"the door's ticket on another app", watch, "novnc", false},
		{"an unbound ticket", ticketActor{role: oidc.RoleUser}, uiDesktopAppName, false},
		{"bound to another app", ticketActor{role: oidc.RoleUser, app: "novnc"}, uiDesktopAppName, false},
		{"with a caller", ticketActor{role: oidc.RoleUser, app: uiDesktopAppName, caller: "user:bob"}, uiDesktopAppName, false},
		{"admin-stamped", ticketActor{role: oidc.RoleAdmin, app: uiDesktopAppName}, uiDesktopAppName, false},
		{"unstamped", ticketActor{app: uiDesktopAppName}, uiDesktopAppName, false},
	} {
		if got := ticketOpensBackgroundWatch(c.ta, c.app); got != c.want {
			t.Errorf("ticketOpensBackgroundWatch, %s = %v, want %v", c.name, got, c.want)
		}
	}
	whole := uiSession{App: uiDesktopAppName, Desktop: true, ViewOnly: true}
	for _, c := range []struct {
		name string
		edit func(*uiSession)
		want bool
	}{
		{"a watch", func(*uiSession) {}, true},
		{"not pinned", func(s *uiSession) { s.ViewOnly = false }, false},
		{"not a VM desktop", func(s *uiSession) { s.Desktop = false }, false},
		{"another app", func(s *uiSession) { s.App = "novnc" }, false},
		{"a caller's", func(s *uiSession) { s.Caller = "user:bob" }, false},
		{"inbound", func(s *uiSession) { s.Inbound = true }, false},
	} {
		sess := whole
		c.edit(&sess)
		if got := sess.backgroundWatch(); got != c.want {
			t.Errorf("backgroundWatch, %s = %v, want %v", c.name, got, c.want)
		}
	}
}

// Who gets a ticket: the run's owner alone, under the run's own limits, whatever their role.
func TestBgWatchTicketWho(t *testing.T) {
	t.Run("the owner, stamped member and bound to the desktop", func(t *testing.T) {
		h, _ := bgWatchHarness(t)
		tok := ticketOf(t, bgWatchMint(h, h.owner, oidc.RoleUser))
		ta, ok, err := consumeAttachTicket(context.Background(), h.store, tok, h.run.ID, h.clock.now())
		if err != nil || !ok {
			t.Fatalf("consume = %v %v", ok, err)
		}
		if ta.role != oidc.RoleUser || ta.app != uiDesktopAppName || ta.caller != "" || ta.principal != h.owner {
			t.Fatalf("ticket = %+v, want the owner's, member-stamped, on the desktop", ta)
		}
	})
	t.Run("a super admin who owns the run is member-stamped too", func(t *testing.T) {
		h, _ := bgWatchHarness(t)
		tok := ticketOf(t, bgWatchMint(h, h.owner, oidc.RoleAdmin))
		ta, _, _ := consumeAttachTicket(context.Background(), h.store, tok, h.run.ID, h.clock.now())
		if ta.role != oidc.RoleUser {
			t.Fatalf("role = %q, want member", ta.role)
		}
	})
	t.Run("a super admin's own run obeys deny_interactive", func(t *testing.T) {
		h, _ := bgWatchHarness(t)
		walledRun(h)(types.GovernanceLimits{DenyInteractive: true})
		rec := bgWatchMint(h, h.owner, oidc.RoleAdmin)
		if rec.Code != http.StatusForbidden || respondedReason(t, rec) != string(authz.ReasonGovernanceUnit) {
			t.Fatalf("mint = %d %s, want 403 %s", rec.Code, rec.Body.String(), authz.ReasonGovernanceUnit)
		}
	})
	t.Run("a member on another's run gets the foreign-run 404", func(t *testing.T) {
		h, _ := bgWatchHarness(t)
		if rec := bgWatchMint(h, "stranger", oidc.RoleUser); rec.Code != http.StatusNotFound {
			t.Fatalf("mint = %d %s, want 404", rec.Code, rec.Body.String())
		}
	})
	t.Run("a super admin on another's run is refused", func(t *testing.T) {
		h, _ := bgWatchHarness(t)
		rec := bgWatchMint(h, "root", oidc.RoleAdmin)
		if rec.Code != http.StatusForbidden || respondedReason(t, rec) != string(authz.ReasonRunOwnerOnly) {
			t.Fatalf("mint = %d %s, want 403 %s", rec.Code, rec.Body.String(), authz.ReasonRunOwnerOnly)
		}
	})
	t.Run("a super admin on an operator-owned run gets no watch", func(t *testing.T) {
		h, _ := bgWatchHarness(t)
		h.run.CreatedBy, h.run.OperatorOwned = "", true
		h.store.putRun(h.run)
		rec := bgWatchMint(h, "root", oidc.RoleAdmin)
		if rec.Code != http.StatusForbidden || respondedReason(t, rec) != string(authz.ReasonRunOwnerOnly) {
			t.Fatalf("mint = %d %s, want 403 %s", rec.Code, rec.Body.String(), authz.ReasonRunOwnerOnly)
		}
	})
	t.Run("a run that isn't RUNNING, and a kept one", func(t *testing.T) {
		h, _ := bgWatchHarness(t)
		h.run.State = types.RunStopped
		h.store.putRun(h.run)
		rec := bgWatchMint(h, h.owner, oidc.RoleUser)
		if rec.Code != http.StatusConflict || respondedReason(t, rec) != reasonAttachNotRunning {
			t.Fatalf("mint = %d %s, want 409 %s", rec.Code, rec.Body.String(), reasonAttachNotRunning)
		}
		now := h.clock.now()
		h.run.State, h.run.LostAt = types.RunRunning, &now
		h.store.putRun(h.run)
		rec = bgWatchMint(h, h.owner, oidc.RoleUser)
		if rec.Code != http.StatusConflict || respondedReason(t, rec) != reasonAttachRunKept {
			t.Fatalf("mint = %d %s, want 409 %s", rec.Code, rec.Body.String(), reasonAttachRunKept)
		}
	})
	for _, c := range bgWatchArms {
		t.Run("refused: "+c.name, func(t *testing.T) {
			h, st := bgWatchHarness(t)
			c.arm(h, st)
			if rec := bgWatchMint(h, h.owner, oidc.RoleUser); rec.Code < 400 {
				t.Fatalf("mint = %d %s, want a refusal", rec.Code, rec.Body.String())
			}
		})
	}
}

// The door's ticket opens the built-in desktop alone, and every other ticket on a background run still refuses.
func TestBgWatchEnterOpensOnlyTheDesktop(t *testing.T) {
	t.Run("the door's ticket opens it, pinned and marked", func(t *testing.T) {
		h, _ := bgWatchHarness(t)
		rec := h.enterDesktop(ticketOf(t, bgWatchMint(h, h.owner, oidc.RoleUser)))
		if rec.Code != http.StatusFound {
			t.Fatalf("enter = %d %s, want 302", rec.Code, rec.Body.String())
		}
		sess, ok := h.srv.decodeUISession(bgWatchCookieRequest(desktopCookie(t, rec)), h.clock.now())
		if !ok || !sess.backgroundWatch() || sess.Role != oidc.RoleUser {
			t.Fatalf("session = %+v (%v), want a member's watch", sess, ok)
		}
		rows := h.audit.auditRows("ui.authorize", "success")
		if len(rows) != 1 || !strings.Contains(string(rows[0].Data), `"view_only":true`) || !strings.Contains(string(rows[0].Data), `"background":true`) {
			t.Fatalf("ui.authorize rows = %+v, want view_only and background", rows)
		}
		page := uiGet(h, desktopPath(h, "/"), desktopCookie(t, rec)).Body.String()
		if !strings.Contains(page, `&#34;background&#34;:true`) || !strings.Contains(page, `&#34;view_only&#34;:true`) {
			t.Fatalf("viewer = %s, want background and view_only in its boot data", page)
		}
	})
	t.Run("not another app", func(t *testing.T) {
		h, _ := bgWatchHarness(t)
		rec := h.enter(url.Values{"run": {h.run.ID.String()}, "app": {"code"}, "ticket": {bgWatchTicket(h, h.owner, oidc.RoleUser, uiDesktopAppName, "")}})
		if rec.Code != http.StatusForbidden {
			t.Fatalf("enter = %d %s, want 403", rec.Code, rec.Body.String())
		}
	})
	for _, c := range []struct {
		name, role, app, caller string
		want                    int
	}{
		{"an unbound ticket", oidc.RoleUser, "", "", http.StatusConflict},
		{"an admin-stamped one", oidc.RoleAdmin, uiDesktopAppName, "", http.StatusConflict},
		{"a caller's", oidc.RoleUser, uiDesktopAppName, "user:bob", http.StatusForbidden},
		{"another app's", oidc.RoleUser, "code", "", http.StatusForbidden},
	} {
		t.Run("not "+c.name, func(t *testing.T) {
			h, _ := bgWatchHarness(t)
			rec := h.enterDesktop(bgWatchTicket(h, h.owner, c.role, c.app, c.caller))
			if rec.Code != c.want {
				t.Fatalf("enter = %d %s, want %d", rec.Code, rec.Body.String(), c.want)
			}
			for _, cookie := range rec.Result().Cookies() {
				if cookie.Name == uiCookieName {
					t.Fatalf("a refused enter set the session cookie %+v", cookie)
				}
			}
		})
	}
	for _, c := range bgWatchArms {
		t.Run("refused at enter: "+c.name, func(t *testing.T) {
			h, st := bgWatchHarness(t)
			tok := bgWatchTicket(h, h.owner, oidc.RoleUser, uiDesktopAppName, "")
			c.arm(h, st)
			if rec := h.enterDesktop(tok); rec.Code < 400 {
				t.Fatalf("enter = %d %s, want a refusal", rec.Code, rec.Body.String())
			}
		})
	}
	// A sponsor passes uiTicketMayEnter's arm; the watch asks mayEnterRun as well.
	t.Run("not a sponsor", func(t *testing.T) {
		h, _ := bgWatchHarness(t)
		h.run.CreatedBy = types.ServicePrincipalPrefix + "nightly"
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, uiEnterPath, nil)
		rf := backgroundOnlyRefusal(h.run, "web application gateway")
		ta := ticketActor{principal: "sponsor", role: oidc.RoleUser, app: uiDesktopAppName}
		if !h.srv.uiBackgroundDoorRefused(rec, req, rf, h.run, uiDesktopAppName, ta) || rec.Code != http.StatusForbidden {
			t.Fatalf("door = %d %s, want 403", rec.Code, rec.Body.String())
		}
		sess := bgWatchSession(h)
		sess.Principal = "sponsor"
		if de := h.srv.uiBackgroundWatchDenies(context.Background(), sess, h.run); de == nil || de.reason != uiDeniedReasonNotAuthorized {
			t.Fatalf("re-check = %+v, want %s", de, uiDeniedReasonNotAuthorized)
		}
	})
}

// bgWatchCookieRequest is a request carrying c, for decoding it.
func bgWatchCookieRequest(c *http.Cookie) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(c)
	return req
}

// An open watch ends at its re-check, every 30 seconds, on any answer that changed, with its own code and words.
func TestBgWatchRecheckEndsItWithItsOwnCode(t *testing.T) {
	if uiDesktopRecheck != 30*time.Second {
		t.Fatalf("uiDesktopRecheck = %s, want the 30 s the docs and the switch's hint rely on", uiDesktopRecheck)
	}
	if len(backgroundWatchEndedMsg) > 123 {
		t.Fatalf("the words are %d bytes; a close frame carries 123", len(backgroundWatchEndedMsg))
	}
	t.Run("while nothing changed it goes on", func(t *testing.T) {
		h, _ := bgWatchHarness(t)
		if _, code, why := h.srv.desktopRunCheck(context.Background(), bgWatchSession(h)); code != 0 {
			t.Fatalf("checked = %d %s, want open", code, why)
		}
	})
	for _, c := range bgWatchArms {
		t.Run(c.name, func(t *testing.T) {
			h, st := bgWatchHarness(t)
			c.arm(h, st)
			_, code, why := h.srv.desktopRunCheck(context.Background(), bgWatchSession(h))
			if code != desktopCloseWatchEnded || why != backgroundWatchEndedMsg {
				t.Fatalf("checked = %d %q, want %d %q", code, why, desktopCloseWatchEnded, backgroundWatchEndedMsg)
			}
		})
	}
	t.Run("the person lost the run", func(t *testing.T) {
		h, _ := bgWatchHarness(t)
		h.run.CreatedBy = "someone-else"
		h.store.putRun(h.run)
		if _, code, _ := h.srv.desktopRunCheck(context.Background(), bgWatchSession(h)); code != desktopCloseWatchEnded {
			t.Fatalf("checked = %d, want %d", code, desktopCloseWatchEnded)
		}
	})
	t.Run("the run isn't background, whatever the cookie says", func(t *testing.T) {
		h, _ := bgWatchHarness(t)
		h.run.Experience = types.ExperienceInteractive
		h.store.putRun(h.run)
		if _, code, _ := h.srv.desktopRunCheck(context.Background(), bgWatchSession(h)); code != desktopCloseWatchEnded {
			t.Fatalf("checked = %d, want %d", code, desktopCloseWatchEnded)
		}
	})
	t.Run("a background session not pinned view only refuses, as it always did", func(t *testing.T) {
		h, _ := bgWatchHarness(t)
		sess := bgWatchSession(h)
		sess.ViewOnly = false
		if _, code, _ := h.srv.desktopRunCheck(context.Background(), sess); code != desktopCloseNotAuthorized {
			t.Fatalf("checked = %d, want %d", code, desktopCloseNotAuthorized)
		}
	})
	t.Run("the profile can't be read", func(t *testing.T) {
		h, _ := bgWatchHarness(t)
		walledRun(h)(types.GovernanceLimits{})
		h.srv.cfg.Store = bgWatchUnitUnreadable{h.srv.cfg.Store.(*desktopSiteStore)}
		if rf := h.srv.backgroundWatchRefusal(context.Background(), h.run); rf == nil || rf.err == nil {
			t.Fatalf("refusal = %+v, want a read failure that refuses", rf)
		}
		if _, code, _ := h.srv.desktopRunCheck(context.Background(), bgWatchSession(h)); code != desktopCloseWatchEnded {
			t.Fatalf("checked = %d, want %d", code, desktopCloseWatchEnded)
		}
	})
	t.Run("the limit's own arm of the unit re-check", func(t *testing.T) {
		h, _ := bgWatchHarness(t)
		walledRun(h)(types.GovernanceLimits{DenyBackgroundDesktopWatch: true})
		de := h.srv.uiSessionUnitDenies(context.Background(), bgWatchSession(h), h.run)
		if de == nil || de.msg != backgroundWatchProfileMsg {
			t.Fatalf("unit re-check = %+v, want the limit's refusal", de)
		}
		other := bgWatchSession(h)
		other.ViewOnly = false
		if de := h.srv.uiSessionUnitDenies(context.Background(), other, h.run); de != nil {
			t.Fatalf("the limit reached a session that isn't a watch: %+v", de)
		}
	})
}

// A helper that stops saying forced is refused at the stream, not only at the ticket: at the socket's own request,
// and past a re-assert that had just passed, before the display is dialled.
func TestBgWatchHelperNotForcedIsRefusedAtTheStream(t *testing.T) {
	notForced := func(h *uiHarness) {
		h.run.VMMachine = &types.VMMachine{DesktopViewOnlyAsked: true}
		h.store.putRun(h.run)
	}
	t.Run("at the socket's request", func(t *testing.T) {
		r := bgWatchRig(t)
		notForced(r.h)
		rec := uiGet(r.h, desktopPath(r.h, uiDesktopRFB), r.cookie)
		if rec.Code != http.StatusForbidden || respondedReason(t, rec) != reasonRunBackgroundWatchUnconfirmed {
			t.Fatalf("socket = %d %s, want 403 %s", rec.Code, rec.Body.String(), reasonRunBackgroundWatchUnconfirmed)
		}
	})
	t.Run("at the stream's first check", func(t *testing.T) {
		r := bgWatchRig(t)
		if rec := uiGet(r.h, desktopPath(r.h, "/"), r.cookie); rec.Code != http.StatusOK {
			t.Fatalf("viewer = %d %s", rec.Code, rec.Body.String())
		}
		notForced(r.h)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		v := dialDesktop(t, ctx, r.h, r.url, r.cookie)
		_, _, err := v.ws.Read(ctx)
		var ce websocket.CloseError
		if !errors.As(err, &ce) || ce.Code != desktopCloseWatchEnded || ce.Reason != backgroundWatchEndedMsg {
			t.Fatalf("stream = %v, want close %d", err, desktopCloseWatchEnded)
		}
		if n := len(r.dr.dialled); n != 0 {
			t.Fatalf("the display was dialled %d times", n)
		}
	})
}

// A real RFB client through the door: the picture reaches it, and only read requests reach the display, however
// the client splits its bytes. Every drop is audited, and the session's rows say what it was.
func TestBgWatchClientThroughTheDoorSendsOnlyReadRequests(t *testing.T) {
	for _, split := range []struct {
		name   string
		chunks func([][]byte) [][]byte
	}{
		{"one WebSocket message", rfbClientWhole},
		{"one per RFB message", rfbClientPerMessage},
		{"one per byte", rfbClientPerByte},
	} {
		t.Run(split.name, func(t *testing.T) {
			r := bgWatchRig(t)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			_, want, _ := rfbClientScript()
			if got := r.play(ctx, t, split.chunks); !bytes.Equal(got, want) {
				t.Fatalf("the display read %v, want only %v", got, want)
			}
			rfbClientCheckViewOnlyRows(ctx, t, r, 1)
			for _, action := range []string{"ui.authorize", "ui.open", "ui.close"} {
				rows := r.h.audit.auditRows(action, "success")
				if len(rows) == 0 || !strings.Contains(string(rows[len(rows)-1].Data), `"background":true`) {
					t.Errorf("%s rows = %+v, want background", action, rows)
				}
			}
		})
	}
}

// The rules are fixed when the stream opens: caps turning to control under an open watch give it no input.
func TestBgWatchCapsTurningToControlGiveNoInput(t *testing.T) {
	r := bgWatchRig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	v, d := r.open(ctx, t)
	bgWatchPutRunner(r.h, true, bgWatchMac(runnerwire.MacVMDesktopControl))
	sent, want, _ := rfbClientScript()
	for _, chunk := range rfbClientWhole(sent) {
		v.write(chunk)
	}
	d.waitFor(ctx, t, rfbClientLastUpdate)
	_ = v.ws.CloseNow()
	if got := d.all(ctx, t); !bytes.Equal(got, want) {
		t.Fatalf("the display read %v, want only %v", got, want)
	}
}

// What the guest copies never reaches a watcher, even where the run's rules would allow copying out.
func TestBgWatchClipboardNeverReachesTheViewer(t *testing.T) {
	const secret = "SENTINEL-guest-clipboard-41c2"
	r := bgWatchRig(t)
	r.h.store.putEffectivePolicy(r.h.run.ID, types.RunPolicySpec{MinConfinementClass: types.CC2,
		Desktop: &types.DesktopControls{CopyOut: types.TransferAllowed, PasteIn: types.TransferAllowed}})
	r.dr.after = func(b net.Conn) {
		_, _ = b.Write(append(rfbCut(3, secret), rfbClientFrame...))
		_, _ = io.Copy(io.Discard, b)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	v := dialDesktop(t, ctx, r.h, r.url, r.cookie)
	select {
	case <-r.dr.dialled:
	case <-ctx.Done():
		t.Fatal("the display was never dialled")
	}
	v.handshake()
	if got := v.read(len(rfbClientFrame)); !bytes.Equal(got, rfbClientFrame) {
		t.Fatalf("the viewer got %q, want the frame alone, the guest's cut text dropped", got)
	}
	if rows := r.h.audit.auditRows("ui.clipboard.transfer", "success"); len(rows) != 0 {
		t.Fatalf("a watch moved clipboard: %+v", rows)
	}
}

// A watch is not a person entering: no touch of the idle clock, where an interactive session on the same Mac
// keeps it moving, and a paused run stays paused.
func TestBgWatchKeepsNothingAliveAndWakesNothing(t *testing.T) {
	hold := func(t *testing.T, r *rfbClientRig) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		v, _ := r.open(ctx, t)
		time.Sleep(100 * time.Millisecond)
		_ = v.ws.CloseNow()
	}
	t.Run("an interactive session touches the run", func(t *testing.T) {
		r := rfbClientNewRig(t, runnerwire.MacVMDesktopObserve)
		r.h.srv.keepaliveEvery = 5 * time.Millisecond
		hold(t, r)
		if r.h.store.touchCount() == 0 {
			t.Fatal("no touch: this test would prove nothing of a watch")
		}
	})
	t.Run("a watch doesn't", func(t *testing.T) {
		r := bgWatchRig(t)
		r.h.srv.keepaliveEvery = 5 * time.Millisecond
		hold(t, r)
		if n := r.h.store.touchCount(); n != 0 {
			t.Fatalf("a watch touched the run %d times", n)
		}
	})
	t.Run("a paused run refuses and stays paused", func(t *testing.T) {
		r := bgWatchRig(t)
		now := r.h.clock.now()
		r.h.run.PausedAt = &now
		r.h.store.putRun(r.h.run)
		rec := uiGet(r.h, desktopPath(r.h, uiDesktopRFB), r.cookie)
		if rec.Code != http.StatusForbidden || respondedReason(t, rec) != reasonRunBackgroundWatchPaused {
			t.Fatalf("socket = %d %s, want 403 %s", rec.Code, rec.Body.String(), reasonRunBackgroundWatchPaused)
		}
		if n := len(r.dr.dialled); n != 0 {
			t.Fatalf("the display was dialled %d times", n)
		}
		if rows := r.h.audit.auditRows("run.resume", "success"); len(rows) != 0 {
			t.Fatalf("the watch resumed the run: %+v", rows)
		}
		got, err := r.h.store.GetRun(context.Background(), r.h.run.ID)
		if err != nil || got.PausedAt == nil {
			t.Fatalf("run = %+v %v, want it still paused", got, err)
		}
	})
}

// A real open stream, ended by its own ticker: the profile's limit and, separately, the site switch change in the
// store under an open watch, and the next re-check closes the socket with the watch's own code and words.
func TestBgWatchChangeInTheStoreEndsAnOpenStream(t *testing.T) {
	off := false
	for _, c := range []struct {
		name   string
		change func(set func(types.GovernanceLimits), site *bgWatchLiveSite)
	}{
		{"the profile comes to deny watching", func(set func(types.GovernanceLimits), _ *bgWatchLiveSite) {
			set(types.GovernanceLimits{DenyBackgroundDesktopWatch: true})
		}},
		{"the profile comes to deny interactive sessions", func(set func(types.GovernanceLimits), _ *bgWatchLiveSite) {
			set(types.GovernanceLimits{DenyInteractive: true})
		}},
		{"the profile comes to deny UI apps", func(set func(types.GovernanceLimits), _ *bgWatchLiveSite) {
			set(types.GovernanceLimits{DenyUIApps: true})
		}},
		{"the site switch turns off", func(_ func(types.GovernanceLimits), site *bgWatchLiveSite) {
			site.set(types.SiteConfig{MacOSVM: &types.SiteMacOSVM{AllowBackgroundWatch: &off}})
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := bgWatchRig(t)
			set := walledRun(r.h)
			site := &bgWatchLiveSite{desktopSiteStore: r.h.srv.cfg.Store.(*desktopSiteStore)}
			r.h.srv.cfg.Store = site
			r.h.srv.uiDesktop.recheckEvery = 10 * time.Millisecond
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			v, d := r.open(ctx, t)
			if got := v.read(len(rfbClientFrame)); !bytes.Equal(got, rfbClientFrame) {
				t.Fatalf("the client got %v, want the display's frame", got)
			}
			// Several re-checks pass while nothing has changed.
			time.Sleep(60 * time.Millisecond)
			if rows := r.h.audit.auditRows("ui.close", "success"); len(rows) != 0 {
				t.Fatalf("the watch closed before anything changed: %+v", rows)
			}
			if rows := r.h.audit.auditRows("ui.authorize", "denied"); len(rows) != 0 {
				t.Fatalf("a re-check refused before anything changed: %+v", rows)
			}
			c.change(set, site)
			_, _, err := v.ws.Read(ctx)
			var ce websocket.CloseError
			if !errors.As(err, &ce) || ce.Code != desktopCloseWatchEnded || ce.Reason != backgroundWatchEndedMsg {
				t.Fatalf("the open stream after the change = %v, want close %d %q", err, desktopCloseWatchEnded, backgroundWatchEndedMsg)
			}
			d.all(ctx, t)
			rows := r.h.audit.auditRows("ui.authorize", "denied")
			if len(rows) == 0 || !strings.Contains(string(rows[0].Data), `"background":true`) {
				t.Fatalf("ui.authorize denied rows = %+v, want the watch's refusal, marked", rows)
			}
		})
	}
}

// bgWatchHungStore blocks one armed read until its context ends, as a store that stopped answering does.
type bgWatchHungStore struct {
	*desktopSiteStore
	armed atomic.Value // the read's name, a string
}

func (s *bgWatchHungStore) hang(ctx context.Context, read string) error {
	if name, _ := s.armed.Load().(string); name != read {
		return nil
	}
	<-ctx.Done()
	return ctx.Err()
}

func (s *bgWatchHungStore) GetRun(ctx context.Context, id uuid.UUID) (types.AgentRun, error) {
	if err := s.hang(ctx, "run"); err != nil {
		return types.AgentRun{}, err
	}
	return s.desktopSiteStore.GetRun(ctx, id)
}

func (s *bgWatchHungStore) GetSiteConfig(ctx context.Context) (types.SiteConfig, error) {
	if err := s.hang(ctx, "site"); err != nil {
		return types.SiteConfig{}, err
	}
	return s.desktopSiteStore.GetSiteConfig(ctx)
}

func (s *bgWatchHungStore) UIDesktopClosed(ctx context.Context, key string, now time.Time) (bool, error) {
	if err := s.hang(ctx, "closed"); err != nil {
		return false, err
	}
	return s.desktopSiteStore.UIDesktopClosed(ctx, key, now)
}

// A re-check's read that hangs ends an already open stream within the wait, with the display still alive: a watch
// always by its own code, any other session as a failed read of that kind always has. The test's own deadline is
// what a stream with no bound would run into.
func TestBgWatchHungReadEndsAnOpenStream(t *testing.T) {
	if uiDesktopRecheckWait != 10*time.Second {
		t.Fatalf("uiDesktopRecheckWait = %s, want 10 s: with the 30 s tick, an open view ends within a minute", uiDesktopRecheckWait)
	}
	for _, c := range []struct {
		name, read string
		rig        func(*testing.T) *rfbClientRig
		wantCode   websocket.StatusCode
		wantWhy    string
	}{
		{"a watch, the run", "run", bgWatchRig, desktopCloseWatchEnded, backgroundWatchEndedMsg},
		{"a watch, the site config", "site", bgWatchRig, desktopCloseWatchEnded, backgroundWatchEndedMsg},
		{"a watch, the Closed set", "closed", bgWatchRig, desktopCloseWatchEnded, backgroundWatchEndedMsg},
		{"an interactive session, the run", "run", bgWatchInteractiveRig, desktopCloseUnreachable, "run not found"},
		{"an interactive session, the Closed set", "closed", bgWatchInteractiveRig, desktopCloseSessionEnded, "this desktop session ended"},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := c.rig(t)
			hung := &bgWatchHungStore{desktopSiteStore: r.h.srv.cfg.Store.(*desktopSiteStore)}
			r.h.srv.cfg.Store = hung
			r.h.srv.uiDesktop.recheckWait = 50 * time.Millisecond
			// Only the armed read's own ticker runs, so nothing else can end the stream first.
			r.h.srv.uiDesktop.recheckEvery, r.h.srv.uiDesktopClosedEvery = time.Hour, time.Hour
			if c.read == "closed" {
				r.h.srv.uiDesktopClosedEvery = 10 * time.Millisecond
			} else {
				r.h.srv.uiDesktop.recheckEvery = 10 * time.Millisecond
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			v, d := r.open(ctx, t)
			if got := v.read(len(rfbClientFrame)); !bytes.Equal(got, rfbClientFrame) {
				t.Fatalf("the client got %v, want the display's frame", got)
			}
			select {
			case <-d.ended:
				t.Fatal("the display ended before any read hung")
			default:
			}
			hung.armed.Store(c.read)
			defer hung.armed.Store("")
			_, _, err := v.ws.Read(ctx)
			var ce websocket.CloseError
			// The display's own end closes with desktopCloseUnreachable and other words, so the words say it lived.
			if !errors.As(err, &ce) || ce.Code != c.wantCode || ce.Reason != c.wantWhy {
				t.Fatalf("the open stream with a hung %s read = %v, want close %d %q", c.read, err, c.wantCode, c.wantWhy)
			}
		})
	}
}

// bgWatchInteractiveRig is the same Mac and stream with an interactive session, the rig's own.
func bgWatchInteractiveRig(t *testing.T) *rfbClientRig {
	t.Helper()
	return rfbClientNewRig(t, runnerwire.MacVMDesktopObserve)
}
