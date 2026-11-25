// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// auditRow is what a door's audit row says, without the fields that differ per call (id, time).
type auditRow struct{ action, outcome, reason string }

// rowsSince is the audit rows h recorded after the first n, as auditRows.
func rowsSince(h *uiHarness, n int) []auditRow {
	var out []auditRow
	for _, ev := range h.audit.snapshot()[n:] {
		var data map[string]any
		_ = json.Unmarshal(ev.Data, &data)
		reason, _ := data["reason"].(string)
		out = append(out, auditRow{ev.Action, ev.Outcome, reason})
	}
	return out
}

// A desktop that its governance profile refuses shows the viewer's page with the door's own sentence, the status and
// the audit row of the JSON it replaces; any other refusal, any other app and a gateway with no viewer keep the JSON.
func TestUIEnter_ADesktopRefusedByItsProfileShowsTheViewerPage(t *testing.T) {
	h, _, _, set := signinDoorGateway(t, "")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, uiDesktopViewer), []byte(desktopTestViewer), 0o600); err != nil {
		t.Fatal(err)
	}
	h.srv.cfg.UIDir = dir
	set(types.GovernanceLimits{DenyInteractive: true})
	enter := func(app string) *httptest.ResponseRecorder {
		return h.enter(url.Values{"run": {h.run.ID.String()}, "app": {app}, "ticket": {h.ticket(h.run.ID, h.owner, oidc.RoleUser)}})
	}

	for _, app := range []string{desktopSigninApp, desktopUIApp} {
		t.Run(app, func(t *testing.T) {
			before := len(h.audit.snapshot())
			rec := enter(app)
			body := rec.Body.String()
			if rec.Code != http.StatusForbidden || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html") ||
				!strings.Contains(body, `&#34;state&#34;:&#34;refused&#34;`) || !strings.Contains(body, `&#34;cause&#34;:&#34;denied&#34;`) ||
				!strings.Contains(body, "attaching is not allowed under the governance profile") || len(setUICookies(rec)) != 0 {
				t.Fatalf("enter = %d %s %q, %d cookies; want the denied page and no cookie", rec.Code, rec.Header().Get("Content-Type"), body, len(setUICookies(rec)))
			}
			page := rowsSince(h, before)

			// Control: with no viewer to serve, the same entry answers the JSON of v0.10.2, with the same audit rows.
			h.srv.cfg.UIDir = ""
			before = len(h.audit.snapshot())
			rec = enter(app)
			h.srv.cfg.UIDir = dir
			if rec.Code != http.StatusForbidden || respondedReason(t, rec) != string(authz.ReasonGovernanceUnit) {
				t.Fatalf("enter without a viewer = %d %s, want the JSON 403 %s", rec.Code, rec.Body.String(), authz.ReasonGovernanceUnit)
			}
			if plain := rowsSince(h, before); len(page) == 0 || len(plain) != len(page) || plain[0] != page[0] {
				t.Fatalf("audit rows differ: page %+v, JSON %+v", page, plain)
			}
		})
	}

	t.Run("another app keeps its JSON", func(t *testing.T) {
		set(types.GovernanceLimits{DenyUIApps: true})
		rec := enter("code")
		if rec.Code != http.StatusForbidden || !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
			t.Fatalf("enter = %d %s %q, want the JSON 403", rec.Code, rec.Header().Get("Content-Type"), rec.Body.String())
		}
	})

	t.Run("a refusal that is not the profile's keeps its JSON", func(t *testing.T) {
		for _, reason := range []string{reasonUIGatewayTicketRunMismatch, reasonDataOutBlocked} {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, uiEnterPath, nil)
			h.srv.uiDesktopDoor(rec, req, h.run.ID, true, func(w http.ResponseWriter) bool {
				writeErrorReason(w, http.StatusForbidden, reason, "a refusal that is not a profile's")
				return true
			})
			if rec.Code != http.StatusForbidden || !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") || respondedReason(t, rec) != reason {
				t.Fatalf("%s = %d %s %q, want the JSON 403", reason, rec.Code, rec.Header().Get("Content-Type"), rec.Body.String())
			}
		}
	})
}

// A Linux VM's desktop entry shows the denied card too; a run that is not a VM keeps its JSON, which the door
// before the wrapped one writes.
func TestDesktopEntry_AProfileRefusalShowsTheDeniedCard(t *testing.T) {
	h, _ := newDesktopHarness(t)
	p := types.GovernanceUnit{ID: uuid.New(), Name: "p", Limits: types.GovernanceLimits{DenyInteractive: true}}
	h.store.profiles = []types.GovernanceUnit{p}
	h.run.UnitID = &p.ID
	h.store.putRun(h.run)
	rec := h.enterDesktop(h.ticket(h.run.ID, h.owner, oidc.RoleUser))
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), `&#34;cause&#34;:&#34;denied&#34;`) || len(setUICookies(rec)) != 0 {
		t.Fatalf("enter = %d %q, want the denied card", rec.Code, rec.Body.String())
	}

	h.run.MachineKind = ""
	h.store.putRun(h.run)
	rec = h.enterDesktop(h.ticket(h.run.ID, h.owner, oidc.RoleUser))
	if rec.Code != http.StatusForbidden || respondedReason(t, rec) != reasonUIGatewayAppNotDeclared {
		t.Fatalf("a container run's desktop = %d %q, want the JSON %s", rec.Code, rec.Body.String(), reasonUIGatewayAppNotDeclared)
	}
}
