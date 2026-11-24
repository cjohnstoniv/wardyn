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

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The profile arms split into a denial that writes no row and the caller that writes it: the denial answers what
// the re-check answers, and the re-check's rows are the ones it always wrote.
func TestUISessionUnitDenial_NoRowAndSameAnswer(t *testing.T) {
	sessOn := func(h *uiHarness, app string, mutate func(*uiSession)) uiSession {
		now := time.Now()
		sess := uiSession{Run: h.run.ID, App: app, Port: uiTestPort, Principal: h.owner, Role: oidc.RoleUser,
			Expires: now.Add(time.Hour).Unix(), IssuedAt: now.Unix(), AuthorizedAt: now.Unix()}
		if mutate != nil {
			mutate(&sess)
		}
		return sess
	}
	for _, tc := range []struct {
		name   string
		app    string
		mutate func(*uiSession)
		limits *types.GovernanceLimits // nil: the run has no profile
		unread bool
		want   int // the arm's status, 0 for none
	}{
		{name: "no profile", app: "code"},
		{name: "deny_ui_apps", app: "code", limits: &types.GovernanceLimits{DenyUIApps: true}, want: http.StatusForbidden},
		{name: "deny_interactive on novnc", app: "novnc", limits: &types.GovernanceLimits{DenyInteractive: true}, want: http.StatusForbidden},
		{name: "deny_interactive on code", app: "code", limits: &types.GovernanceLimits{DenyInteractive: true}},
		{name: "a background watch", app: uiDesktopAppName, mutate: func(s *uiSession) { s.Desktop, s.ViewOnly = true, true },
			limits: &types.GovernanceLimits{DenyBackgroundDesktopWatch: true}, want: http.StatusForbidden},
		{name: "an unreadable profile", app: "code", limits: &types.GovernanceLimits{}, unread: true, want: http.StatusServiceUnavailable},
		{name: "an admin on an exempt run", app: "code", mutate: func(s *uiSession) { s.Role = oidc.RoleAdmin },
			limits: &types.GovernanceLimits{DenyUIApps: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newUIHarness(t, okBackend())
			if tc.limits != nil {
				h.setLimits(*tc.limits)
			}
			if tc.unread {
				h.failProfileRead(errors.New("db down"))
			}
			sess := sessOn(h, tc.app, tc.mutate)
			de := h.srv.uiSessionUnitDenial(context.Background(), sess, h.run)
			if rows := len(h.audit.snapshot()); rows != 0 {
				t.Fatalf("uiSessionUnitDenial wrote %d rows, want 0", rows)
			}
			if (de == nil) != (tc.want == 0) || de != nil && de.status != tc.want {
				t.Fatalf("uiSessionUnitDenial = %+v, want status %d", de, tc.want)
			}
			with := h.srv.uiSessionUnitDenies(context.Background(), sess, h.run)
			rows := h.audit.auditRows("ui.authorize", "denied")
			switch {
			case de == nil && (with != nil || len(rows) != 0):
				t.Fatalf("uiSessionUnitDenies = %+v with %d rows, want nil and none", with, len(rows))
			case de != nil && (with == nil || *with != *de || len(rows) != 1):
				t.Fatalf("uiSessionUnitDenies = %+v with %d rows, want %+v and exactly one row", with, len(rows), de)
			}
		})
	}
}

// The inbound arm (the same split): the denial writes nothing; uiInboundUnitDenies writes the unit_denies row for a
// 403 and the run_unreadable row for a failed read; readmitInbound writes the run_unreadable row and answers it.
func TestUISessionUnitDenial_InboundNoRowAndSameRows(t *testing.T) {
	for _, tc := range []struct {
		name   string
		limits *types.GovernanceLimits
		unread bool
		status int
	}{
		{name: "no profile"},
		{name: "deny_ui_apps", limits: &types.GovernanceLimits{DenyUIApps: true}, status: http.StatusForbidden},
		{name: "deny_interactive", limits: &types.GovernanceLimits{DenyInteractive: true}, status: http.StatusForbidden},
		{name: "an unreadable profile", limits: &types.GovernanceLimits{}, unread: true, status: http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := inboundHarness(t)
			if tc.limits != nil {
				h.setLimits(*tc.limits)
			}
			if tc.unread {
				h.failProfileRead(errors.New("db down"))
			}
			now := time.Now()
			sess := uiSession{Run: h.run.ID, App: "code", Port: uiTestPort, Principal: h.owner, Inbound: true,
				Expires: now.Add(time.Hour).Unix(), IssuedAt: now.Unix(), AuthorizedAt: now.Unix()}
			de := h.srv.uiInboundUnitDenial(context.Background(), sess, h.run)
			if n := anyDeniedRows(h); n != 0 || len(h.audit.snapshot()) != 0 {
				t.Fatalf("uiInboundUnitDenial wrote rows, want none: %v", h.audit.actions())
			}
			if (de == nil) != (tc.status == 0) || de != nil && de.status != tc.status {
				t.Fatalf("uiInboundUnitDenial = %+v, want status %d", de, tc.status)
			}
			with := h.srv.uiInboundUnitDenies(context.Background(), sess, h.run)
			if (with == nil) != (de == nil) || with != nil && *with != *de {
				t.Fatalf("uiInboundUnitDenies = %+v, want %+v", with, de)
			}
			wantUnreadRows, wantDenyRows := 0, 0
			switch tc.status {
			case http.StatusServiceUnavailable:
				wantUnreadRows = 1
			case http.StatusForbidden:
				wantDenyRows = 1
			}
			if got := denyRowCount(h, "ui.authorize", reasonRunUnreadable); got != wantUnreadRows ||
				len(h.audit.auditRows("ui.authorize", "denied")) != wantUnreadRows {
				t.Fatalf("ui.authorize denied rows = %d (run_unreadable %d), want %d run_unreadable", len(h.audit.auditRows("ui.authorize", "denied")), got, wantUnreadRows)
			}
			if got := denyRowCount(h, "inbound.deny", inboundDenyUnitDenies); got != wantDenyRows ||
				len(h.audit.auditRows("inbound.deny", "denied")) != wantDenyRows {
				t.Fatalf("inbound.deny rows = %d (unit_denies %d), want %d unit_denies", len(h.audit.auditRows("inbound.deny", "denied")), got, wantDenyRows)
			}
		})
	}

	t.Run("readmitInbound on an unreadable profile", func(t *testing.T) {
		h, _ := inboundHarness(t)
		h.setLimits(types.GovernanceLimits{})
		h.failProfileRead(errors.New("db down"))
		tok, err := h.srv.cfg.Store.GetAPITokenByRaw(context.Background(), "wdn_alice")
		if err != nil {
			t.Fatalf("read the owner's token: %v", err)
		}
		req := httptest.NewRequest(http.MethodGet, uiRelayPrefix(h.run.ID, "code")+"/ws", nil)
		ok, unavailable, _ := h.srv.readmitInbound(context.Background(), inboundReadmission{r: req, runID: h.run.ID, app: "code",
			raw: "wdn_alice", principal: h.owner, tokenID: tok.ID, checked: time.Now()})
		if ok || unavailable != reasonRunUnreadable {
			t.Fatalf("readmitInbound = %v %q, want false %q", ok, unavailable, reasonRunUnreadable)
		}
		if got := denyRowCount(h, "ui.authorize", reasonRunUnreadable); got != 1 || len(h.audit.auditRows("ui.authorize", "denied")) != 1 {
			t.Fatalf("ui.authorize run_unreadable rows = %d of %d denied rows, want exactly one", got, len(h.audit.auditRows("ui.authorize", "denied")))
		}
	})
}
