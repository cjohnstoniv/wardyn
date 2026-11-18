// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"cmp"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/livebus"
	"github.com/cjohnstoniv/wardyn/internal/recording"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const (
	supportOwnerSub  = "owner-sub"
	supportHelperSub = "helper-sub"
)

// supportPersona is who a test request is: what the auth middleware would have published on ctx.
type supportPersona struct {
	human, role           string
	token, cli, delegated bool
	device                bool
}

var supportPersonas = map[string]supportPersona{
	"owner":          {human: supportOwnerSub, role: oidc.RoleUser},
	"owner-token":    {human: supportOwnerSub, role: oidc.RoleUser, token: true},
	"owner-cli":      {human: supportOwnerSub, role: oidc.RoleUser, token: true, cli: true},
	"helper":         {human: supportHelperSub, role: oidc.RoleAdmin},
	"helper-cli":     {human: supportHelperSub, role: oidc.RoleAdmin, token: true, cli: true},
	"helper-token":   {human: supportHelperSub, role: oidc.RoleAdmin, token: true},
	"helper-portal":  {human: supportHelperSub, role: oidc.RoleAdmin, delegated: true},
	"other-admin":    {human: "admin-2", role: oidc.RoleAdmin},
	"security":       {human: "sec-sub", role: oidc.RoleSecurityAdmin},
	"member":         {human: "member-sub", role: oidc.RoleUser},
	"admin-token":    {},
	"device":         {device: true},
	"helper-creator": {human: supportHelperSub, role: oidc.RoleAdmin},
	"former-helper":  {human: supportHelperSub, role: oidc.RoleUser},
}

// supportRig is one replica over the fake grant record, serving only the five support routes behind a
// middleware that publishes the persona named by the X-Persona header.
type supportRig struct {
	t      *testing.T
	srv    *Server
	be     *supportBackend
	aud    *leaseCovAudit
	rev    *attachRevocations
	run    types.AgentRun
	router http.Handler
}

func newSupportRig(t *testing.T, configure func(*Config)) *supportRig {
	t.Helper()
	t.Setenv(envSupportSessions, "1")
	rig := &supportRig{t: t, be: newSupportBackend(), aud: newLeaseCovAudit(), rev: newAttachRevocations()}
	cfg := baseTestConfig(newHarness(t), rig.be)
	cfg.Audit = rig.aud
	cfg.SessionRevocations = rig.rev
	if configure != nil {
		configure(&cfg)
	}
	rig.srv = New(cfg)
	t.Cleanup(rig.srv.WaitBackground)
	rig.run = types.AgentRun{ID: uuid.New(), CreatedBy: supportOwnerSub, State: types.RunRunning}
	rig.be.putRun(rig.run)
	r := chi.NewRouter()
	r.Group(func(r chi.Router) {
		r.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				next.ServeHTTP(w, req.WithContext(supportPersonas[req.Header.Get("X-Persona")].ctx(req.Context())))
			})
		})
		rig.srv.mountSupportRoutes(r)
	})
	rig.router = r
	return rig
}

func (p supportPersona) ctx(ctx context.Context) context.Context {
	if p.human != "" {
		ctx = withOIDCRole(withOIDCEmail(withOIDCHuman(ctx, p.human), p.human+"@example.test"), p.role)
	}
	if p.token {
		ctx = withAPITokenID(ctx, uuid.New())
	}
	if p.cli {
		ctx = withCLISessionID(ctx, uuid.New())
	}
	if p.delegated {
		ctx = audit.WithDelegation(ctx, types.DelegationVia{Delegate: uuid.New(), Grant: uuid.New()})
	}
	if p.device {
		ctx = context.WithValue(ctx, deviceCtxKey{}, types.Device{})
	}
	return ctx
}

func (rig *supportRig) do(who, method, path, body string) *httptest.ResponseRecorder {
	rig.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("X-Persona", who)
	w := httptest.NewRecorder()
	rig.router.ServeHTTP(w, req)
	return w
}

func (rig *supportRig) base() string { return "/runs/" + rig.run.ID.String() + "/support" }

// request is the helper's POST /support, asking for the terminal and files doors.
func (rig *supportRig) request(who string) *httptest.ResponseRecorder {
	return rig.do(who, http.MethodPost, rig.base(), `{"doors":["terminal","files"],"reason":" stuck on login "}`)
}

// seed leaves an accepted, live grant of the run's helper for 30 minutes, written straight to the record.
func (rig *supportRig) seed() store.SupportGrant {
	rig.t.Helper()
	ctx := context.Background()
	g, err := rig.be.RequestSupportGrant(ctx, store.SupportGrant{RunID: rig.run.ID, Helper: supportHelperSub,
		HelperEmail: supportHelperSub + "@example.test", Reason: "r", DoorsAsked: []string{"terminal", "files", "forward"},
		RequestRevSeq: rig.rev.current()})
	if err != nil {
		rig.t.Fatal(err)
	}
	g, err = rig.be.AcceptSupportGrant(ctx, rig.run.ID, g.ID, supportOwnerSub, supportOwnerSub+"@example.test",
		[]string{"terminal", "files"}, 30, rig.rev.current())
	if err != nil {
		rig.t.Fatal(err)
	}
	return g
}

func (rig *supportRig) auditRows(action string) []types.AuditEvent {
	var out []types.AuditEvent
	for _, ev := range rig.aud.snapshot() {
		if ev.Action == action {
			out = append(out, ev)
		}
	}
	return out
}

func supportBody(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatalf("body %q: %v", w.Body.String(), err)
	}
	return m
}

func wantSupportStatus(t *testing.T, what string, w *httptest.ResponseRecorder, status int, reason string) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("%s: status = %d, want %d; body=%s", what, w.Code, status, w.Body.String())
	}
	if reason != "" && supportBody(t, w)["reason"] != reason {
		t.Fatalf("%s: reason = %v, want %s; body=%s", what, supportBody(t, w)["reason"], reason, w.Body.String())
	}
}

// TestSupportCallerValueList is section 4's supportCaller list: who may ask to help.
func TestSupportCallerValueList(t *testing.T) {
	s := &Server{}
	for _, tc := range []struct {
		who  string
		want bool
	}{
		{"helper", true},         // super admin, console session
		{"helper-cli", true},     // super admin, CLI sign-in: a token with a CLI session
		{"helper-token", false},  // a personal token alone
		{"admin-token", false},   // no person
		{"device", false},        // neverOperator
		{"helper-portal", false}, // a portal's delegated token
		{"security", false},      // security admin
		{"member", false},
	} {
		if got := s.supportCaller(supportPersonas[tc.who].ctx(context.Background())); got != tc.want {
			t.Errorf("supportCaller(%s) = %v, want %v", tc.who, got, tc.want)
		}
	}
}

func TestSupportRequest(t *testing.T) {
	t.Run("records a request and audits it without the reason", func(t *testing.T) {
		rig := newSupportRig(t, nil)
		w := rig.request("helper")
		wantSupportStatus(t, "request", w, http.StatusCreated, "")
		body := supportBody(t, w)
		if body["state"] != "requested" || body["reason"] != "stuck on login" || body["helper"] != supportHelperSub {
			t.Fatalf("view = %v", body)
		}
		rows := rig.be.rows()
		if len(rows) != 1 || rows[0].Reason != "stuck on login" || rows[0].RequestRevSeq != rig.rev.current() || rows[0].HelperEmail == "" {
			t.Fatalf("row = %+v", rows)
		}
		ev := rig.auditRows("support.request")
		if len(ev) != 1 || ev[0].Actor != supportHelperSub || strings.Contains(string(ev[0].Data), "stuck") ||
			!strings.Contains(string(ev[0].Data), rows[0].ID.String()) || !strings.Contains(string(ev[0].Data), "doors_asked") {
			t.Fatalf("audit = %+v", ev)
		}
	})
	t.Run("the switch off refuses", func(t *testing.T) {
		rig := newSupportRig(t, nil)
		t.Setenv(envSupportSessions, "0")
		wantSupportStatus(t, "request", rig.request("helper"), http.StatusConflict, reasonSupportDisabled)
	})
	t.Run("local mode is ineligible, before the caller is looked at", func(t *testing.T) {
		rig := newSupportRig(t, func(c *Config) { c.LocalMode = true })
		wantSupportStatus(t, "request", rig.request("member"), http.StatusConflict, reasonSupportRunIneligible)
	})
	t.Run("callers that may not ask", func(t *testing.T) {
		for _, who := range []string{"member", "security", "admin-token", "helper-token", "helper-portal", "device"} {
			rig := newSupportRig(t, nil)
			wantSupportStatus(t, who, rig.request(who), http.StatusForbidden, reasonSupportNotAllowed)
			if len(rig.be.rows()) != 0 {
				t.Errorf("%s: a refused request left a row", who)
			}
		}
		rig := newSupportRig(t, nil)
		rig.run.CreatedBy = supportHelperSub
		rig.be.putRun(rig.run)
		wantSupportStatus(t, "the run's own creator", rig.request("helper-creator"), http.StatusForbidden, reasonSupportNotAllowed)
	})
	t.Run("a CLI sign-in may ask", func(t *testing.T) {
		wantSupportStatus(t, "request", newSupportRig(t, nil).request("helper-cli"), http.StatusCreated, "")
	})
	t.Run("a malformed body is 400 support_request_invalid", func(t *testing.T) {
		long := strings.Repeat("x", supportMaxReason+1)
		for name, body := range map[string]string{
			"no doors":       `{"doors":[],"reason":"r"}`,
			"unknown door":   `{"doors":["root"],"reason":"r"}`,
			"duplicate door": `{"doors":["files","files"],"reason":"r"}`,
			"no reason":      `{"doors":["files"],"reason":"  "}`,
			"long reason":    `{"doors":["files"],"reason":"` + long + `"}`,
		} {
			rig := newSupportRig(t, nil)
			wantSupportStatus(t, name, rig.do("helper", http.MethodPost, rig.base(), body), http.StatusBadRequest, reasonSupportRequestBad)
		}
		rig := newSupportRig(t, nil)
		wantSupportStatus(t, "unknown field", rig.do("helper", http.MethodPost, rig.base(), `{"doors":["files"],"reason":"r","minutes":15}`),
			http.StatusBadRequest, reasonInvalidRequestBody)
	})
	t.Run("the reason is counted in characters", func(t *testing.T) {
		// 500 two-byte characters are 1000 bytes: the table's CHECK counts characters, so the route does too.
		ok := newSupportRig(t, nil)
		wantSupportStatus(t, "500 characters", ok.do("helper", http.MethodPost, ok.base(), `{"doors":["files"],"reason":"`+strings.Repeat("é", supportMaxReason)+`"}`), http.StatusCreated, "")
		over := newSupportRig(t, nil)
		wantSupportStatus(t, "501 characters", over.do("helper", http.MethodPost, over.base(), `{"doors":["files"],"reason":"`+strings.Repeat("é", supportMaxReason+1)+`"}`), http.StatusBadRequest, reasonSupportRequestBad)
	})
	t.Run("a run that cannot take a helper", func(t *testing.T) {
		lost := time.Now()
		for name, mutate := range map[string]func(*types.AgentRun){
			"not running":    func(r *types.AgentRun) { r.State = types.RunStopped },
			"lost":           func(r *types.AgentRun) { r.LostAt = &lost },
			"operator owned": func(r *types.AgentRun) { r.OperatorOwned = true },
			"service":        func(r *types.AgentRun) { r.CreatedBy = types.ServicePrincipalPrefix + "bot" },
			"background":     func(r *types.AgentRun) { r.Experience = types.ExperienceBackground },
			"sign-in run":    func(r *types.AgentRun) { r.Task = harnessLoginTask },
		} {
			rig := newSupportRig(t, nil)
			mutate(&rig.run)
			rig.be.putRun(rig.run)
			wantSupportStatus(t, name, rig.request("helper"), http.StatusConflict, reasonSupportRunIneligible)
		}
	})
	t.Run("a second open request is support_grant_open", func(t *testing.T) {
		rig := newSupportRig(t, nil)
		wantSupportStatus(t, "first", rig.request("helper"), http.StatusCreated, "")
		wantSupportStatus(t, "second", rig.request("other-admin"), http.StatusConflict, reasonSupportGrantOpen)
	})
	t.Run("path mode refuses web_apps and desktop", func(t *testing.T) {
		for _, door := range []string{"web_apps", "desktop"} {
			rig := newSupportRig(t, nil)
			w := rig.do("helper", http.MethodPost, rig.base(), `{"doors":["`+door+`"],"reason":"r"}`)
			wantSupportStatus(t, door, w, http.StatusForbidden, reasonInboundNeedsHostMode)
		}
		rig := newSupportRig(t, func(c *Config) { c.UIOriginTemplate = "https://{run}.apps.example.test" })
		w := rig.do("helper", http.MethodPost, rig.base(), `{"doors":["web_apps","desktop"],"reason":"r"}`)
		wantSupportStatus(t, "host mode", w, http.StatusCreated, "")
	})
	t.Run("a store with no grant record is 503", func(t *testing.T) {
		rig := newSupportRig(t, func(c *Config) { c.Store = newAuthzStore() })
		wantSupportStatus(t, "request", rig.request("helper"), http.StatusServiceUnavailable, reasonSupportUnavailable)
	})
}

func TestSupportAccept(t *testing.T) {
	asked := func(rig *supportRig) string {
		w := rig.request("helper")
		wantSupportStatus(t, "request", w, http.StatusCreated, "")
		return rig.base() + "/" + supportBody(t, w)["grant"].(string)
	}
	t.Run("the owner accepts a subset for 30 minutes", func(t *testing.T) {
		rig := newSupportRig(t, nil)
		path := asked(rig)
		w := rig.do("owner", http.MethodPost, path+"/accept", `{"doors":["terminal"],"minutes":30}`)
		wantSupportStatus(t, "accept", w, http.StatusOK, "")
		row := rig.be.rows()[0]
		if row.AcceptedAt == nil || row.ExpiresAt == nil || row.ExpiresAt.Sub(*row.AcceptedAt) != 30*time.Minute ||
			row.Minutes != 30 || len(row.Doors) != 1 || row.Doors[0] != "terminal" || row.OwnerEmail == "" || row.AcceptRevSeq != rig.rev.current() {
			t.Fatalf("row = %+v", row)
		}
		if body := supportBody(t, w); body["state"] != "active" {
			t.Fatalf("view = %v", body)
		}
		ev := rig.auditRows("support.accept")
		if len(ev) != 1 || ev[0].Actor != supportOwnerSub {
			t.Fatalf("audit = %+v", ev)
		}
		for _, field := range []string{`"support_grant"`, `"helper"`, `"doors"`, `"minutes"`, `"expires_at"`, `"recorded_doors"`} {
			if !strings.Contains(string(ev[0].Data), field) {
				t.Errorf("support.accept lacks %s: %s", field, ev[0].Data)
			}
		}
		if got := string(rig.srv.stampSupportWindow(&rig.run.ID, nil)); !strings.Contains(got, row.ID.String()) {
			t.Errorf("the audit window after accept = %s, want grant %s", got, row.ID)
		}
	})
	t.Run("only the run's creator, from a console or CLI sign-in", func(t *testing.T) {
		rig := newSupportRig(t, nil)
		path := asked(rig)
		body := `{"doors":["terminal"],"minutes":15}`
		wantSupportStatus(t, "the helper", rig.do("helper", http.MethodPost, path+"/accept", body), http.StatusForbidden, reasonSupportNotAllowed)
		wantSupportStatus(t, "the owner's personal token", rig.do("owner-token", http.MethodPost, path+"/accept", body), http.StatusForbidden, reasonSupportNotAllowed)
		wantSupportStatus(t, "another member", rig.do("member", http.MethodPost, path+"/accept", body), http.StatusNotFound, reasonRunNotFound)
		if rig.be.rows()[0].AcceptedAt != nil {
			t.Fatal("a refused accept moved the row")
		}
		wantSupportStatus(t, "the owner's CLI sign-in", rig.do("owner-cli", http.MethodPost, path+"/accept", body), http.StatusOK, "")
	})
	t.Run("a body the route refuses", func(t *testing.T) {
		rig := newSupportRig(t, nil)
		path := asked(rig)
		for name, body := range map[string]string{
			"minutes not offered": `{"doors":["terminal"],"minutes":20}`,
			"no minutes":          `{"doors":["terminal"]}`,
			"no doors":            `{"doors":[],"minutes":15}`,
			"door not asked":      `{"doors":["forward"],"minutes":15}`,
		} {
			wantSupportStatus(t, name, rig.do("owner", http.MethodPost, path+"/accept", body), http.StatusBadRequest, reasonSupportRequestBad)
		}
	})
	t.Run("no such request, already answered, or lapsed", func(t *testing.T) {
		rig := newSupportRig(t, nil)
		path := asked(rig)
		body := `{"doors":["terminal"],"minutes":15}`
		wantSupportStatus(t, "another grant id", rig.do("owner", http.MethodPost, rig.base()+"/"+uuid.NewString()+"/accept", body), http.StatusNotFound, reasonSupportGrantNotFound)
		wantSupportStatus(t, "first", rig.do("owner", http.MethodPost, path+"/accept", body), http.StatusOK, "")
		wantSupportStatus(t, "again", rig.do("owner", http.MethodPost, path+"/accept", body), http.StatusNotFound, reasonSupportGrantNotFound)

		lapsed := newSupportRig(t, nil)
		lpath := asked(lapsed)
		lapsed.be.tick(16 * time.Minute)
		wantSupportStatus(t, "after 15 minutes", lapsed.do("owner", http.MethodPost, lpath+"/accept", body), http.StatusNotFound, reasonSupportGrantNotFound)
	})
	t.Run("the switch off refuses, and so does a run no longer running", func(t *testing.T) {
		rig := newSupportRig(t, nil)
		path := asked(rig)
		body := `{"doors":["terminal"],"minutes":15}`
		t.Setenv(envSupportSessions, "0")
		wantSupportStatus(t, "switch off", rig.do("owner", http.MethodPost, path+"/accept", body), http.StatusConflict, reasonSupportDisabled)
		t.Setenv(envSupportSessions, "1")
		rig.run.State = types.RunStopped
		rig.be.putRun(rig.run)
		wantSupportStatus(t, "stopped", rig.do("owner", http.MethodPost, path+"/accept", body), http.StatusConflict, reasonSupportRunIneligible)
	})
}

func TestSupportDecline(t *testing.T) {
	rig := newSupportRig(t, nil)
	w := rig.request("helper")
	path := rig.base() + "/" + supportBody(t, w)["grant"].(string)
	wantSupportStatus(t, "the helper", rig.do("helper", http.MethodPost, path+"/decline", ""), http.StatusForbidden, reasonSupportNotAllowed)
	wantSupportStatus(t, "another member", rig.do("member", http.MethodPost, path+"/decline", ""), http.StatusNotFound, reasonRunNotFound)
	w = rig.do("owner", http.MethodPost, path+"/decline", "")
	wantSupportStatus(t, "decline", w, http.StatusOK, "")
	row := rig.be.rows()[0]
	if row.EndReason != "declined" || !row.EndAudited || row.EndedAt == nil || supportBody(t, w)["state"] != "ended" {
		t.Fatalf("row = %+v, view = %v", row, supportBody(t, w))
	}
	if ev := rig.auditRows("support.decline"); len(ev) != 1 || ev[0].Actor != supportOwnerSub {
		t.Fatalf("audit = %+v", ev)
	}
	wantSupportStatus(t, "again", rig.do("owner", http.MethodPost, path+"/decline", ""), http.StatusNotFound, reasonSupportGrantNotFound)
	if got, err := rig.be.SweepSupportGrants(context.Background(), false); err != nil || len(got) != 0 {
		t.Fatalf("the sweep claimed %v (%v) after a decline that audited itself", got, err)
	}
}

// TestSupportEndFromAPersonalToken pins that the owner may end from a personal token, which cannot accept or decline:
// ending only restricts. A portal's delegated request still may not.
func TestSupportEndFromAPersonalToken(t *testing.T) {
	rig := newSupportRig(t, nil)
	g := rig.seed()
	wantSupportStatus(t, "the owner's personal token", rig.do("owner-token", http.MethodPost, rig.base()+"/"+g.ID.String()+"/end", ""), http.StatusOK, "")
	if row, _ := rig.be.row(g.ID); row.EndReason != "revoked" || !row.EndAudited {
		t.Errorf("row after the owner's token ended it = %+v, want revoked and audited", row)
	}
	if n := len(rig.auditRows("support.end")); n != 1 {
		t.Errorf("support.end rows = %d, want 1", n)
	}
}

func TestSupportEnd(t *testing.T) {
	for _, tc := range []struct{ who, reason string }{
		{"owner", "revoked"}, {"helper", "ended_by_helper"}, {"security", "ended_by_security"}, {"other-admin", "ended_by_security"},
	} {
		t.Run(tc.reason+" by "+tc.who, func(t *testing.T) {
			rig := newSupportRig(t, nil)
			g := rig.seed()
			rig.srv.setSupportWindow(rig.run.ID, g.ID)
			w := rig.do(tc.who, http.MethodPost, rig.base()+"/"+g.ID.String()+"/end", "")
			wantSupportStatus(t, "end", w, http.StatusOK, "")
			row, _ := rig.be.row(g.ID)
			if row.EndedAt == nil || row.EndReason != tc.reason || !row.EndAudited {
				t.Fatalf("row = %+v", row)
			}
			ev := rig.auditRows("support.end")
			if len(ev) != 1 || !strings.Contains(string(ev[0].Data), `"reason":"`+tc.reason+`"`) || !strings.Contains(string(ev[0].Data), `"ended_at"`) ||
				!strings.Contains(string(ev[0].Data), g.ID.String()) {
				t.Fatalf("audit = %+v", ev)
			}
			if got := string(rig.srv.stampSupportWindow(&rig.run.ID, nil)); got != "" {
				t.Errorf("the audit window after end = %q, want none", got)
			}
			if err := rig.srv.sweepSupportGrants(context.Background()); err != nil {
				t.Fatal(err)
			}
			if n := len(rig.auditRows("support.end")); n != 1 {
				t.Errorf("the sweep wrote a second support.end (%d rows) for an end the route audited", n)
			}
		})
	}
	t.Run("a stranger, and a grant that is not open", func(t *testing.T) {
		rig := newSupportRig(t, nil)
		g := rig.seed()
		wantSupportStatus(t, "another member", rig.do("member", http.MethodPost, rig.base()+"/"+g.ID.String()+"/end", ""), http.StatusNotFound, reasonRunNotFound)
		if row, _ := rig.be.row(g.ID); row.EndedAt != nil {
			t.Fatal("a refused end moved the row")
		}
		wantSupportStatus(t, "another grant id", rig.do("owner", http.MethodPost, rig.base()+"/"+uuid.NewString()+"/end", ""), http.StatusNotFound, reasonSupportGrantNotFound)
		wantSupportStatus(t, "first", rig.do("owner", http.MethodPost, rig.base()+"/"+g.ID.String()+"/end", ""), http.StatusOK, "")
		wantSupportStatus(t, "again", rig.do("owner", http.MethodPost, rig.base()+"/"+g.ID.String()+"/end", ""), http.StatusNotFound, reasonSupportGrantNotFound)
	})
}

func TestSupportStatus(t *testing.T) {
	rig := newSupportRig(t, nil)
	w := rig.request("helper")
	g := supportBody(t, w)["grant"].(string)
	read := func(who string) map[string]any {
		t.Helper()
		w := rig.do(who, http.MethodGet, rig.base(), "")
		wantSupportStatus(t, "status for "+who, w, http.StatusOK, "")
		return supportBody(t, w)
	}
	for who, wantReason := range map[string]bool{"owner": true, "helper": true, "security": false} {
		open, _ := read(who)["open"].(map[string]any)
		if open == nil || open["grant"] != g || open["state"] != "requested" {
			t.Fatalf("%s: open = %v", who, open)
		}
		if _, has := open["reason"]; has != wantReason {
			t.Errorf("%s: reason shown = %v, want %v", who, has, wantReason)
		}
	}
	wantSupportStatus(t, "another member", rig.do("member", http.MethodGet, rig.base(), ""), http.StatusNotFound, reasonRunNotFound)
	if open, _ := read("owner-token")["open"].(map[string]any); open == nil || open["grant"] != g {
		t.Errorf("the owner's personal token reads open = %v", open)
	}
	wantSupportStatus(t, "another admin who is not the helper is the security tier", rig.do("other-admin", http.MethodGet, rig.base(), ""), http.StatusOK, "")
	if offered, _ := read("owner")["doors_offered"].([]any); len(offered) != 4 {
		t.Errorf("doors_offered in path mode = %v, want the first four", offered)
	}
	wantSupportStatus(t, "end", rig.do("owner", http.MethodPost, rig.base()+"/"+g+"/end", ""), http.StatusOK, "")
	got := read("owner")
	if got["open"] != nil || got["ended"] == nil || got["ended"].(map[string]any)["end_reason"] != "revoked" {
		t.Fatalf("after the end: %v", got)
	}
	// An ended grant shows its state and end reason to its helper, never the reason text.
	for _, who := range []string{"helper", "former-helper"} {
		ended, _ := read(who)["ended"].(map[string]any)
		if ended == nil || ended["state"] != "ended" || ended["end_reason"] != "revoked" {
			t.Errorf("%s reads ended = %v, want the ended state and its end reason", who, ended)
		}
		if _, has := ended["reason"]; has {
			t.Errorf("%s was shown the reason text of an ended grant", who)
		}
	}
	if ended, _ := read("owner")["ended"].(map[string]any); ended["reason"] == nil {
		t.Error("the owner lost the reason text of an ended grant")
	}
	// The former helper (no longer a super admin) reads that grant alone: no open key, nobody else's grant.
	if fh := read("former-helper"); fh["open"] != nil || len(fh) != 2 {
		t.Errorf("former helper's view = %v, want only ended and doors_offered", fh)
	}
	wantSupportStatus(t, "a member who was never the helper", rig.do("member", http.MethodGet, rig.base(), ""), http.StatusNotFound, reasonRunNotFound)

	// Before any grant exists a super admin about to ask reads doors_offered, with no grant fields.
	fresh := newSupportRig(t, nil)
	first := supportBody(t, fresh.do("helper", http.MethodGet, fresh.base(), ""))
	if offered, _ := first["doors_offered"].([]any); len(offered) != 4 || first["open"] != nil || first["ended"] != nil {
		t.Errorf("a super admin before any grant reads %v, want doors_offered and no grant", first)
	}
	// recorded_doors is a list (never null) naming what support.accept would record on this run.
	if got, ok := first["recorded_doors"].([]any); !ok || len(got) != 0 {
		t.Errorf("recorded_doors on a run with no recording store = %v, want []", first["recorded_doors"])
	}
	if want := fresh.srv.supportRecordedDoors(fresh.run); len(want) != 0 {
		t.Fatalf("setup: recording store absent but supportRecordedDoors = %v", want)
	}
	recorded := newSupportRig(t, func(c *Config) {
		rs, err := recording.NewFSStore(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		c.RecordingStore = rs
	})
	for level, want := range map[types.RecordingLevel]string{"": "terminal", types.RecordingScreenActions: "terminal,desktop", types.RecordingOff: ""} {
		recorded.run.RecordingLevel = level
		recorded.be.putRun(recorded.run)
		var got []string
		for _, d := range supportBody(t, recorded.do("helper", http.MethodGet, recorded.base(), ""))["recorded_doors"].([]any) {
			got = append(got, d.(string))
		}
		if strings.Join(got, ",") != want {
			t.Errorf("recorded_doors at recording level %q = %v, want %q", level, got, want)
		}
	}
	offeredNow := fresh.srv.supportDoorsOffered(fresh.run)
	offeredNow[0] = "mutated"
	if fresh.srv.supportDoorsOffered(fresh.run)[0] == "mutated" {
		t.Error("supportDoorsOffered returned the shared door list, not a copy")
	}

	host := newSupportRig(t, func(c *Config) { c.UIOriginTemplate = "https://{run}.apps.example.test" })
	hw := host.do("security", http.MethodGet, host.base(), "")
	if offered, _ := supportBody(t, hw)["doors_offered"].([]any); len(offered) != 6 {
		t.Errorf("doors_offered in host mode = %v, want all six", offered)
	}
}

// TestSupportMayEnterVerdicts is the verdict list: each case is asked with a Nil grant and with the grant's id,
// and where they differ the Nil ask is supportNone and the set ask is supportEnded.
func TestSupportMayEnterVerdicts(t *testing.T) {
	lost := time.Now()
	type tc struct {
		name string
		// config and arrange prepare the rig; arrange sees the accepted grant.
		config  func(*Config)
		arrange func(*testing.T, *supportRig, store.SupportGrant)
		env     string // WARDYN_SUPPORT_SESSIONS, default on
		who     string // principal asking, default the helper
		door    supportDoor
		want    supportVerdict // for a set grant
		wantNil supportVerdict // for a Nil grant
		// endReason is the reason the row is left ended with ("" for still open).
		endReason string
	}
	runMut := func(f func(*types.AgentRun)) func(*testing.T, *supportRig, store.SupportGrant) {
		return func(_ *testing.T, rig *supportRig, _ store.SupportGrant) { f(&rig.run); rig.be.putRun(rig.run) }
	}
	cases := []tc{
		{name: "ok", door: supportDoorTerminal, want: supportOK, wantNil: supportOK},
		{name: "switch off", env: "0", door: supportDoorTerminal, want: supportOff, wantNil: supportNone},
		{name: "no grant record in the store", config: func(c *Config) { c.Store = newAuthzStore() }, door: supportDoorTerminal, want: supportUnavailable, wantNil: supportNone},
		{name: "requested only", door: supportDoorTerminal, want: supportEnded, wantNil: supportNone,
			arrange: func(_ *testing.T, rig *supportRig, g store.SupportGrant) {
				// Put the answered row back to requested: the 15-minute window is still open, so it is not ended.
				rig.be.edit(g.ID, func(row *store.SupportGrant) {
					row.AcceptedAt, row.ExpiresAt, row.Doors, row.Minutes = nil, nil, nil, 0
				})
			}},
		// The grant row names the run's own creator as its helper, so only supportMayEnter's creator check
		// (not "no row for this principal") keeps the owner out of the helper arm.
		{name: "the creator as the grant's helper", who: supportOwnerSub, door: supportDoorTerminal, want: supportEnded, wantNil: supportNone,
			arrange: func(_ *testing.T, rig *supportRig, g store.SupportGrant) {
				rig.be.edit(g.ID, func(row *store.SupportGrant) { row.Helper = supportOwnerSub })
			}},
		{name: "a read error", door: supportDoorTerminal, want: supportUnavailable, wantNil: supportUnavailable,
			arrange: func(_ *testing.T, rig *supportRig, _ store.SupportGrant) {
				rig.be.failOn("SupportGrantLive", context.DeadlineExceeded)
			}},
		{name: "the run's creator", who: supportOwnerSub, door: supportDoorTerminal, want: supportEnded, wantNil: supportNone},
		{name: "run not running", arrange: runMut(func(r *types.AgentRun) { r.State = types.RunStopped }), door: supportDoorTerminal, want: supportEnded, wantNil: supportNone},
		{name: "run lost", arrange: runMut(func(r *types.AgentRun) { r.LostAt = &lost }), door: supportDoorTerminal, want: supportEnded, wantNil: supportNone},
		{name: "operator-owned run", arrange: runMut(func(r *types.AgentRun) { r.OperatorOwned = true }), door: supportDoorTerminal, want: supportEnded, wantNil: supportNone},
		{name: "service run", arrange: runMut(func(r *types.AgentRun) { r.CreatedBy = types.ServicePrincipalPrefix + "bot" }), door: supportDoorTerminal, want: supportEnded, wantNil: supportNone},
		{name: "background run", arrange: runMut(func(r *types.AgentRun) { r.Experience = types.ExperienceBackground }), door: supportDoorTerminal, want: supportEnded, wantNil: supportNone},
		{name: "reserved task (a harness sign-in run)", arrange: runMut(func(r *types.AgentRun) { r.Task = harnessLoginTask }), door: supportDoorTerminal, want: supportEnded, wantNil: supportNone},
		{name: "local mode", config: func(c *Config) { c.LocalMode = true }, door: supportDoorTerminal, want: supportEnded, wantNil: supportNone},
		{name: "no row for this principal", who: "someone-else", door: supportDoorTerminal, want: supportEnded, wantNil: supportNone},
		{name: "expired", door: supportDoorTerminal, want: supportEnded, wantNil: supportNone, endReason: "expired",
			arrange: func(_ *testing.T, rig *supportRig, _ store.SupportGrant) { rig.be.tick(31 * time.Minute) }},
		{name: "door not ticked", door: supportDoorForward, want: supportDoorRefused, wantNil: supportDoorRefused},
		{name: "ssh pseudo-door with one of its doors", door: supportDoorSSH, want: supportOK, wantNil: supportOK},
		{name: "ui pseudo-door with none of its doors", door: supportDoorUI, want: supportDoorRefused, wantNil: supportDoorRefused},
		{name: "helper signed out", door: supportDoorTerminal, want: supportEnded, wantNil: supportNone, endReason: "helper_signed_out",
			arrange: func(t *testing.T, rig *supportRig, _ store.SupportGrant) {
				if err := rig.rev.CutSessions(context.Background(), supportHelperSub); err != nil {
					t.Fatal(err)
				}
			}},
		{name: "owner revoked", door: supportDoorTerminal, want: supportEnded, wantNil: supportNone, endReason: "owner_revoked",
			arrange: func(t *testing.T, rig *supportRig, _ store.SupportGrant) {
				if err := rig.rev.RevokeSub(context.Background(), supportOwnerSub); err != nil {
					t.Fatal(err)
				}
			}},
		{name: "owner signed out of one browser", door: supportDoorTerminal, want: supportOK, wantNil: supportOK,
			arrange: func(t *testing.T, rig *supportRig, _ store.SupportGrant) {
				if err := rig.rev.CutSessions(context.Background(), supportOwnerSub); err != nil {
					t.Fatal(err)
				}
			}},
		{name: "revocation store unreadable", door: supportDoorTerminal, want: supportUnavailable, wantNil: supportUnavailable,
			arrange: func(_ *testing.T, rig *supportRig, _ store.SupportGrant) { rig.rev.setFail(1) }},
	}
	for _, c := range cases {
		for _, nilGrant := range []bool{false, true} {
			name := c.name + "/set"
			want := c.want
			if nilGrant {
				name, want = c.name+"/nil", c.wantNil
			}
			t.Run(name, func(t *testing.T) {
				rig := newSupportRig(t, c.config)
				g := rig.seed()
				if c.arrange != nil {
					c.arrange(t, rig, g)
				}
				if c.env != "" {
					t.Setenv(envSupportSessions, c.env)
				}
				who, arg := cmp.Or(c.who, supportHelperSub), g.ID
				if nilGrant {
					arg = uuid.Nil
				}
				got, v := rig.srv.supportMayEnter(context.Background(), rig.run, who, arg, c.door)
				if v != want {
					t.Fatalf("verdict = %d, want %d", v, want)
				}
				row, _ := rig.be.row(g.ID)
				if (row.EndedAt != nil) != (c.endReason != "") || row.EndReason != c.endReason {
					t.Fatalf("row ended=%v reason=%q, want reason %q", row.EndedAt != nil, row.EndReason, c.endReason)
				}
				if c.endReason != "" && row.EndAudited {
					t.Errorf("an end by supportMayEnter was marked audited; the sweep writes its row")
				}
				if v == supportOK && got.ID != g.ID {
					t.Errorf("the grant returned is %v, want %v", got.ID, g.ID)
				}
			})
		}
	}
}

// TestSupportMayEnterStamp pins who moves last_seen_at: every ask but the SSH pseudo-door with a Nil grant.
func TestSupportMayEnterStamp(t *testing.T) {
	for _, tc := range []struct {
		name  string
		door  supportDoor
		nilID bool
		stamp bool
	}{
		{"ssh auth (pseudo-door, Nil grant) does not stamp", supportDoorSSH, true, false},
		{"ssh re-check with the grant stamps", supportDoorSSH, false, true},
		{"another door with a Nil grant stamps", supportDoorTerminal, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newSupportRig(t, nil)
			g := rig.seed()
			arg := g.ID
			if tc.nilID {
				arg = uuid.Nil
			}
			if _, v := rig.srv.supportMayEnter(context.Background(), rig.run, supportHelperSub, arg, tc.door); v != supportOK {
				t.Fatalf("verdict = %d, want supportOK", v)
			}
			calls := rig.be.liveCalls()
			if len(calls) != 1 || calls[0].Stamp != tc.stamp {
				t.Fatalf("SupportGrantLive calls = %+v, want stamp=%v", calls, tc.stamp)
			}
			if row, _ := rig.be.row(g.ID); (row.LastSeenAt != nil) != tc.stamp {
				t.Errorf("last_seen_at set = %v, want %v", row.LastSeenAt != nil, tc.stamp)
			}
		})
	}
}

func TestRefuseSupport(t *testing.T) {
	rig := newSupportRig(t, nil)
	for _, tc := range []struct {
		v      supportVerdict
		status int
		reason string
		audit  bool
	}{
		{supportDoorRefused, http.StatusForbidden, string(authz.ReasonSupportDoorNotGranted), true},
		{supportEnded, http.StatusForbidden, string(authz.ReasonSupportGrantEnded), true},
		{supportEndedUnnamed, http.StatusForbidden, string(authz.ReasonSupportGrantEnded), true},
		{supportOff, http.StatusConflict, reasonSupportDisabled, false},
		{supportUnavailable, http.StatusServiceUnavailable, reasonSupportUnavailable, false},
		{supportNone, 0, "", false},
		{supportOK, 0, "", false},
	} {
		before := len(rig.aud.snapshot())
		w := httptest.NewRecorder()
		answered := rig.srv.refuseSupport(w, httptest.NewRequest(http.MethodGet, "/", nil), rig.run, tc.v)
		if answered != (tc.status != 0) || (answered && (w.Code != tc.status || supportBody(t, w)["reason"] != tc.reason)) {
			t.Errorf("verdict %d: answered=%v status=%d body=%s, want status %d reason %s", tc.v, answered, w.Code, w.Body.String(), tc.status, tc.reason)
		}
		if got := len(rig.aud.snapshot()) - before; (got == 1) != tc.audit || got > 1 {
			t.Errorf("verdict %d wrote %d audit rows, want audited=%v", tc.v, got, tc.audit)
		}
	}
	for _, r := range []authz.Reason{authz.ReasonSupportDoorNotGranted, authz.ReasonSupportGrantEnded, authz.ReasonSupportHelperDecision} {
		if ref, ok := authz.Lookup(r); !ok || ref.Effect != authz.EffectDeny || !ref.Audit {
			t.Errorf("%s registry row = %+v (%v), want an audited deny", r, ref, ok)
		}
	}
}

func TestSupportSweep(t *testing.T) {
	t.Run("writes one support.end per end no route audited, once", func(t *testing.T) {
		rig := newSupportRig(t, nil)
		g := rig.seed()
		rig.be.tick(31 * time.Minute)
		if _, v := rig.srv.supportMayEnter(context.Background(), rig.run, supportHelperSub, g.ID, supportDoorTerminal); v != supportEnded {
			t.Fatalf("verdict = %d, want supportEnded", v)
		}
		for range 2 {
			if err := rig.srv.sweepSupportGrants(context.Background()); err != nil {
				t.Fatal(err)
			}
		}
		ev := rig.auditRows("support.end")
		if len(ev) != 1 || ev[0].ActorType != types.ActorSystem || ev[0].Actor != "system" ||
			!strings.Contains(string(ev[0].Data), `"reason":"expired"`) || !strings.Contains(string(ev[0].Data), g.ID.String()) {
			t.Fatalf("audit = %+v", ev)
		}
	})
	t.Run("switch off ends every open grant as support_off", func(t *testing.T) {
		rig := newSupportRig(t, nil)
		g := rig.seed()
		rig.srv.setSupportWindow(rig.run.ID, g.ID)
		t.Setenv(envSupportSessions, "0")
		if err := rig.srv.sweepSupportGrants(context.Background()); err != nil {
			t.Fatal(err)
		}
		if row, _ := rig.be.row(g.ID); row.EndReason != "support_off" {
			t.Fatalf("row = %+v", row)
		}
		if ev := rig.auditRows("support.end"); len(ev) != 1 || !strings.Contains(string(ev[0].Data), `"reason":"support_off"`) {
			t.Fatalf("audit = %+v", ev)
		}
		if got := string(rig.srv.stampSupportWindow(&rig.run.ID, nil)); got != "" {
			t.Errorf("window after the sweep = %q, want none", got)
		}
	})
	t.Run("the run ending ends the grant", func(t *testing.T) {
		rig := newSupportRig(t, nil)
		g := rig.seed()
		rig.run.State = types.RunStopped
		rig.be.putRun(rig.run)
		if err := rig.srv.sweepSupportGrants(context.Background()); err != nil {
			t.Fatal(err)
		}
		if row, _ := rig.be.row(g.ID); row.EndReason != "run_ended" {
			t.Fatalf("row = %+v", row)
		}
	})
	t.Run("replaces the window from the store, and a failing sweep is reported", func(t *testing.T) {
		rig := newSupportRig(t, nil)
		g := rig.seed()
		if err := rig.srv.sweepSupportGrants(context.Background()); err != nil {
			t.Fatal(err)
		}
		if got := string(rig.srv.stampSupportWindow(&rig.run.ID, nil)); !strings.Contains(got, g.ID.String()) {
			t.Errorf("window = %q, want grant %s", got, g.ID)
		}
		rig.be.failOn("SweepSupportGrants", context.DeadlineExceeded)
		if err := rig.srv.sweepSupportGrants(context.Background()); err == nil {
			t.Error("a failing sweep returned nil")
		}
	})
	t.Run("a store with no grant record is nothing to sweep", func(t *testing.T) {
		rig := newSupportRig(t, func(c *Config) { c.Store = newAuthzStore() })
		if err := rig.srv.sweepSupportGrants(context.Background()); err != nil {
			t.Fatal(err)
		}
	})
}

func TestSupportAuditWindow(t *testing.T) {
	rig := newSupportRig(t, nil)
	run, grant := rig.run.ID, uuid.New()
	stamp := func(data string) string { return string(rig.srv.stampSupportWindow(&run, json.RawMessage(data))) }
	if got := stamp(`{"a":1}`); got != `{"a":1}` {
		t.Errorf("with no window the data = %s, want it unchanged", got)
	}
	rig.srv.setSupportWindow(run, grant)
	for data, want := range map[string]string{
		`{"a":1}`:               `"support_grant":"` + grant.String() + `"`,
		``:                      `"support_grant":"` + grant.String() + `"`,
		`null`:                  `"support_grant":"` + grant.String() + `"`,
		`[1]`:                   `"data":[1]`,
		`{"support_grant":"x"}`: `"support_grant":"x"`,
	} {
		if got := stamp(data); !strings.Contains(got, want) || (data == `{"support_grant":"x"}` && strings.Contains(got, grant.String())) {
			t.Errorf("stamp(%q) = %s, want it to contain %s", data, got, want)
		}
	}
	if got := string(rig.srv.stampSupportWindow(nil, json.RawMessage(`{"a":1}`))); got != `{"a":1}` {
		t.Errorf("a row with no run = %s, want it unchanged", got)
	}
	other := uuid.New()
	if got := string(rig.srv.stampSupportWindow(&other, json.RawMessage(`{"a":1}`))); got != `{"a":1}` {
		t.Errorf("another run's row = %s, want it unchanged", got)
	}
	ev := rig.srv.stampAuditEvent(context.Background(), types.AuditEvent{RunID: &run, Data: json.RawMessage(`{"a":1}`)})
	if !strings.Contains(string(ev.Data), grant.String()) {
		t.Errorf("stampAuditEvent data = %s, want the grant", ev.Data)
	}
	rig.srv.clearSupportWindow(run)
	if got := stamp(`{"a":1}`); got != `{"a":1}` {
		t.Errorf("after clear the data = %s, want it unchanged", got)
	}
}

func TestSupportGrantNotice(t *testing.T) {
	rig := newSupportRig(t, nil)
	g := rig.seed()
	notice := func(origin string) {
		rig.srv.onSupportGrantNotice(livebus.Message{Kind: livebus.KindSupportGrant, Run: rig.run.ID, Origin: origin})
		rig.srv.WaitBackground()
	}
	window := func() string { return string(rig.srv.stampSupportWindow(&rig.run.ID, nil)) }
	notice(rig.srv.replicaName())
	if window() != "" {
		t.Fatal("a notice this replica sent changed its own window")
	}
	notice("replica-b")
	if !strings.Contains(window(), g.ID.String()) {
		t.Fatalf("after another replica's accept the window = %q, want grant %s", window(), g.ID)
	}
	if _, err := rig.be.EndSupportGrant(context.Background(), rig.run.ID, g.ID, "revoked", true); err != nil {
		t.Fatal(err)
	}
	notice("replica-b")
	if window() != "" {
		t.Fatalf("after another replica's end the window = %q, want none", window())
	}
}

func TestSupportTicketCarriesTheGrant(t *testing.T) {
	now, grant := time.Now(), uuid.New()
	a := ticketActor{support: grant, supportExpires: now.Add(5 * time.Minute)}
	if got := a.sessionExpiry(now, time.Hour); !got.Equal(now.Add(5 * time.Minute)) {
		t.Errorf("a support ticket's session ends %v, want at the grant's expiry", got.Sub(now))
	}
	if got := (ticketActor{}).sessionExpiry(now, time.Hour); !got.Equal(now.Add(time.Hour)) {
		t.Errorf("an ordinary ticket's session ends %v, want the configured lifetime", got.Sub(now))
	}
	far := ticketActor{support: grant, supportExpires: now.Add(3 * time.Hour)}
	if got := far.sessionExpiry(now, time.Hour); !got.Equal(now.Add(time.Hour)) {
		t.Errorf("a grant longer than the lifetime moved the session end to %v", got.Sub(now))
	}
	if got := (ticketActor{support: grant}).sessionExpiry(now, time.Hour); !got.Equal(now) {
		t.Errorf("a support ticket with no expiry opens a session that ends %v, want now", got.Sub(now))
	}
	st := newAuthzStore()
	exp := now.Add(10 * time.Minute)
	if err := st.MintAttachTicket(context.Background(), "tok", store.AttachTicket{RunID: uuid.New(), SupportGrant: grant, SupportExpires: exp}, now, now); err != nil {
		t.Fatal(err)
	}
	t.Run("consume copies it", func(t *testing.T) {
		tk := st.tickets["tok"]
		ta, ok, err := consumeAttachTicket(context.Background(), st, "tok", tk.RunID, now)
		if err != nil || !ok || ta.support != grant || !ta.supportExpires.Equal(exp) {
			t.Fatalf("consumeAttachTicket = %+v, %v, %v", ta, ok, err)
		}
	})
}

func TestSupportSwitchInSetupFeatures(t *testing.T) {
	t.Setenv(envSupportSessions, "1")
	if !setupFeatures(false, false).SupportSessions {
		t.Error("the switch on: SupportSessions is false")
	}
	t.Setenv(envSupportSessions, "0")
	if setupFeatures(false, false).SupportSessions {
		t.Error("the switch off: SupportSessions is true")
	}
}
