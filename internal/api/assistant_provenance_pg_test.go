// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The assistant draft mark on the four saves it covers and on a unit save applied directly, on a real Postgres:
// the value table, the stored column, and that a marked save is judged exactly as an unmarked one. Guarded by
// WARDYN_TEST_PG: skipped cleanly when unset, must PASS when set.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// draftSaved is what one save answered and wrote: the status, the data of the audit row it wrote, the session id
// a held change stored, and a rendering of the verdict, risk, holds and approvals the save was judged by.
type draftSaved struct {
	status int
	audit  map[string]any
	stored *uuid.UUID
	judged string
}

// draftRoute is one of the four saves. role is the role of the person who saves, kind the session target kind a
// draft of it has, and prepare makes the target a person saves to and answers its key, empty for a new draft.
type draftRoute struct {
	name    string
	role    string
	kind    string
	prepare func(t *testing.T, f *draftFixture, person string, c *http.Cookie) string
	save    func(t *testing.T, f *draftFixture, c *http.Cookie, header, key string) draftSaved
}

type draftFixture struct {
	presetFixture
	n int
}

func (f *draftFixture) fresh(prefix string) string {
	f.n++
	return fmt.Sprintf("%s-%d", prefix, f.n)
}

// doSSOHeader is doSSO with one request header.
func doSSOHeader(t *testing.T, f *draftFixture, method, path string, c *http.Cookie, header, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.AddCookie(c)
	if header != "" {
		r.Header.Set(assistantSessionHeader, header)
	}
	w := httptest.NewRecorder()
	panicFails(t, f.srv.Handler()).ServeHTTP(w, r)
	return w
}

// auditData is the data of the one row of action on target.
func (f *draftFixture) auditData(t *testing.T, action, target string) map[string]any {
	t.Helper()
	var out map[string]any
	for _, ev := range f.audit.snapshot() {
		if ev.Action == action && ev.Target == target {
			if out != nil {
				t.Fatalf("two %s rows on %s", action, target)
			}
			out = map[string]any{}
			if err := json.Unmarshal(ev.Data, &out); err != nil {
				t.Fatalf("decode the %s row: %v", action, err)
			}
		}
	}
	if out == nil {
		t.Fatalf("no %s row on %s", action, target)
	}
	return out
}

func draftRoutes() []draftRoute {
	newName := func(f *draftFixture) string { return f.fresh("prov") }
	templateSave := func(t *testing.T, f *draftFixture, c *http.Cookie, header, method, path string, body string, wantStatus int, action string) draftSaved {
		w := doSSOHeader(t, f, method, path, c, header, body)
		if w.Code != wantStatus {
			t.Fatalf("%s %s = %d %s, want %d", method, path, w.Code, w.Body, wantStatus)
		}
		tpl := decodeTemplate(t, w.Body.String())
		return draftSaved{status: w.Code, audit: f.auditData(t, action, tpl.ID.String()), judged: fmt.Sprintf("%d %s %s %d", w.Code, tpl.Scope, tpl.Kind, tpl.Revision)}
	}
	return []draftRoute{
		{name: "run", role: oidc.RoleUser, kind: types.AssistantTargetRun,
			prepare: func(*testing.T, *draftFixture, string, *http.Cookie) string { return "" },
			save: func(t *testing.T, f *draftFixture, c *http.Cookie, header, _ string) draftSaved {
				w := doSSOHeader(t, f, http.MethodPost, "/api/v1/runs", c, header, `{"agent":"claude-code","task":"t"}`)
				run := mustCreated(t, w)
				f.dispatched(t, run.ID)
				warnings, _ := json.Marshal(run.Warnings)
				return draftSaved{status: w.Code, audit: f.auditData(t, "run.create", run.ID.String()), judged: fmt.Sprintf("%d %s", w.Code, warnings)}
			}},
		{name: "template-create", role: oidc.RoleUser, kind: types.AssistantTargetTemplate,
			prepare: func(*testing.T, *draftFixture, string, *http.Cookie) string { return "" },
			save: func(t *testing.T, f *draftFixture, c *http.Cookie, header, _ string) draftSaved {
				return templateSave(t, f, c, header, http.MethodPost, "/api/v1/templates", templateSaveBody("person", "", newName(f)), http.StatusCreated, "template.create")
			}},
		{name: "template-update", role: oidc.RoleUser, kind: types.AssistantTargetTemplate,
			prepare: func(t *testing.T, f *draftFixture, _ string, c *http.Cookie) string {
				w := doSSOHeader(t, f, http.MethodPost, "/api/v1/templates", c, "", templateSaveBody("person", "", newName(f)))
				if w.Code != http.StatusCreated {
					t.Fatalf("seed the template = %d %s", w.Code, w.Body)
				}
				return decodeTemplate(t, w.Body.String()).ID.String()
			},
			save: func(t *testing.T, f *draftFixture, c *http.Cookie, header, key string) draftSaved {
				b, _ := json.Marshal(map[string]any{"name": newName(f), "expected_revision": 1, "document": json.RawMessage(templateDocBody(templateIntentEgress))})
				return templateSave(t, f, c, header, http.MethodPut, "/api/v1/templates/"+key, string(b), http.StatusOK, "template.update")
			}},
		{name: "unit-propose", role: oidc.RoleSecurityAdmin, kind: types.AssistantTargetUnit,
			prepare: func(t *testing.T, f *draftFixture, _ string, _ *http.Cookie) string {
				return draftSeedUnit(t, f).ID.String()
			},
			save: func(t *testing.T, f *draftFixture, c *http.Cookie, header, key string) draftSaved {
				id := uuid.MustParse(key)
				u, err := f.pg.GetGovernanceUnit(context.Background(), id)
				if err != nil {
					t.Fatalf("read the unit: %v", err)
				}
				w := doSSOHeader(t, f, http.MethodPut, "/api/v1/governance/units/"+key, c, header, profileBody(u.Name, "pypi.org", "api.anthropic.com"))
				if w.Code != http.StatusAccepted {
					t.Fatalf("held unit save = %d %s, want 202", w.Code, w.Body)
				}
				var body struct {
					Pending types.GovernanceChange `json:"pending_change"`
				}
				if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body.Pending.ID == uuid.Nil {
					t.Fatalf("202 body %s: %v", w.Body, err)
				}
				stored, err := f.pg.GetGovernanceChange(context.Background(), body.Pending.ID)
				if err != nil {
					t.Fatalf("read the change: %v", err)
				}
				var diff struct {
					Review json.RawMessage `json:"review"`
				}
				_ = json.Unmarshal(stored.Diff, &diff)
				return draftSaved{status: w.Code, stored: stored.DraftedWithSession, audit: f.auditData(t, "governance.change.propose", stored.ID.String()),
					judged: fmt.Sprintf("%d %s %s %d %v", w.Code, stored.State, diff.Review, stored.RequiredApprovals, stored.RequireSecurity)}
			}},
	}
}

// draftSeedUnit is a unit whose ceiling allows pypi.org only, so a save that adds a host is held.
func draftSeedUnit(t *testing.T, f *draftFixture) types.GovernanceUnit {
	t.Helper()
	u, err := f.pg.UpsertGovernanceUnit(context.Background(), types.GovernanceUnit{
		Name: f.fresh("prov-unit"), CreatedBy: "seed",
		Ceiling: types.RunPolicySpec{AllowedDomains: []string{"pypi.org"}, MinConfinementClass: types.CC2},
	})
	if err != nil {
		t.Fatalf("seed the unit: %v", err)
	}
	return u
}

// draftSession is a live session of person, with the events given, optionally ended at endedAt.
type draftSession struct {
	purpose  types.AssistantPurpose
	kind     string
	key      string
	proposal bool
	edit     bool
	endedAt  time.Time
}

func (f *draftFixture) seedSession(t *testing.T, person string, s draftSession) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	if s.purpose == "" {
		s.purpose = types.AssistantCompose
	}
	sess, err := f.pg.CreateAssistantSession(ctx, types.AssistantSession{ID: uuid.New(), Principal: person, Email: person + "@corp.example",
		Role: oidc.RoleUser, Groups: []string{}, Purpose: s.purpose, TargetKind: s.kind, TargetKey: s.key,
		Binding: types.AssistantBinding{ProviderID: "a", ProviderUID: "uid-a", Model: "m1"}, State: types.AssistantSessionFetching})
	if err != nil {
		t.Fatalf("seed the session: %v", err)
	}
	if s.proposal {
		if _, err := f.pg.AppendAssistantEvent(ctx, sess.ID, types.AssistantEventProposal, []byte(`{"parts":[]}`)); err != nil {
			t.Fatal(err)
		}
	}
	if s.edit {
		if _, err := f.pg.AppendAssistantEvent(ctx, sess.ID, types.AssistantEventPersonEdit, []byte(`{"changed":[{"field":"task","value":"x"}]}`)); err != nil {
			t.Fatal(err)
		}
	}
	if !s.endedAt.IsZero() {
		if _, err := f.pg.Pool.Exec(ctx, `UPDATE assistant_sessions SET state = 'ended', ended_at = $2, end_reason = 'person' WHERE id = $1`, sess.ID, s.endedAt); err != nil {
			t.Fatal(err)
		}
	}
	return sess.ID
}

// draftWant checks a save's row and column against whether and how it should be marked: by is empty for none.
func draftWant(t *testing.T, got draftSaved, session uuid.UUID, by string) {
	t.Helper()
	if by == "" {
		for _, k := range []string{"drafted_with_ai", "assistant_session", "detected_by"} {
			if _, ok := got.audit[k]; ok {
				t.Errorf("the audit row carries %s: %v", k, got.audit)
			}
		}
		if got.stored != nil {
			t.Errorf("the change stored session %s, want none", got.stored)
		}
		return
	}
	if got.audit["drafted_with_ai"] != true || got.audit["assistant_session"] != session.String() || got.audit["detected_by"] != by {
		t.Errorf("the audit row = %v, want drafted_with_ai, session %s, detected_by %s", got.audit, session, by)
	}
}

// draftRows is one row of the value table; the header and the sessions it needs are made by setup, which answers
// the session the save must name and how it was found.
type draftRow struct {
	name  string
	setup func(t *testing.T, f *draftFixture, r draftRoute, person, key string) (header string, want uuid.UUID, by string)
}

func draftOtherKind(r draftRoute) (string, string) {
	if r.kind == types.AssistantTargetRun {
		return types.AssistantTargetTemplate, ""
	}
	return types.AssistantTargetRun, ""
}

func draftRows() []draftRow {
	ended := func(d time.Duration) func(t *testing.T, f *draftFixture, r draftRoute, person, key string) (string, uuid.UUID, string) {
		return func(t *testing.T, f *draftFixture, r draftRoute, person, key string) (string, uuid.UUID, string) {
			id := f.seedSession(t, person, draftSession{kind: r.kind, key: draftSessionKey(r, key), proposal: true, edit: true, endedAt: time.Now().Add(-d)})
			return "", id, draftedBySession
		}
	}
	notMatched := func(d time.Duration, proposal, edit bool) func(t *testing.T, f *draftFixture, r draftRoute, person, key string) (string, uuid.UUID, string) {
		return func(t *testing.T, f *draftFixture, r draftRoute, person, key string) (string, uuid.UUID, string) {
			f.seedSession(t, person, draftSession{kind: r.kind, key: draftSessionKey(r, key), proposal: proposal, edit: edit, endedAt: time.Now().Add(-d)})
			return "", uuid.Nil, ""
		}
	}
	return []draftRow{
		{"own-compose-header", func(t *testing.T, f *draftFixture, r draftRoute, person, key string) (string, uuid.UUID, string) {
			id := f.seedSession(t, person, draftSession{kind: r.kind, key: draftSessionKey(r, key)})
			return id.String(), id, draftedByHeader
		}},
		{"other-person", func(t *testing.T, f *draftFixture, r draftRoute, person, key string) (string, uuid.UUID, string) {
			id := f.seedSession(t, f.fresh("sub-else"), draftSession{kind: r.kind, key: draftSessionKey(r, key)})
			return id.String(), uuid.Nil, ""
		}},
		{"own-review", func(t *testing.T, f *draftFixture, r draftRoute, person, key string) (string, uuid.UUID, string) {
			id := f.seedSession(t, person, draftSession{purpose: types.AssistantReview, kind: types.AssistantTargetChange, key: uuid.NewString()})
			return id.String(), uuid.Nil, ""
		}},
		{"own-other-kind", func(t *testing.T, f *draftFixture, r draftRoute, person, key string) (string, uuid.UUID, string) {
			kind, k := draftOtherKind(r)
			id := f.seedSession(t, person, draftSession{kind: kind, key: k})
			return id.String(), uuid.Nil, ""
		}},
		{"match-ended-1h", ended(time.Hour)},
		{"match-no-accepted-part", notMatched(time.Hour, true, false)},
		{"match-ended-3h", notMatched(3*time.Hour, true, true)},
		{"no-match", func(*testing.T, *draftFixture, draftRoute, string, string) (string, uuid.UUID, string) {
			return "", uuid.Nil, ""
		}},
		{"match-ended-2h00", notMatched(2*time.Hour, true, true)},
		{"match-ended-1h59", ended(time.Hour + 59*time.Minute)},
		{"header-beats-match", func(t *testing.T, f *draftFixture, r draftRoute, person, key string) (string, uuid.UUID, string) {
			f.seedSession(t, person, draftSession{kind: r.kind, key: draftSessionKey(r, key), proposal: true, edit: true, endedAt: time.Now().Add(-time.Hour)})
			id := f.seedSession(t, person, draftSession{kind: r.kind, key: draftSessionKey(r, key)})
			return id.String(), id, draftedByHeader
		}},
	}
}

// draftSessionKey is the target key a session has for the route's target: a new draft has none.
func draftSessionKey(r draftRoute, key string) string {
	if r.name == "template-update" || r.name == "unit-propose" {
		return key
	}
	return ""
}

// Every row of the value table on each of the four save routes, 11 on each.
func TestPG_AssistantProvenanceValue(t *testing.T) {
	t.Setenv(envGovernanceSecondHuman, "true")
	f := &draftFixture{presetFixture: newPresetFixture(t)}
	for _, r := range draftRoutes() {
		t.Run(r.name, func(t *testing.T) {
			for _, row := range draftRows() {
				t.Run(row.name, func(t *testing.T) {
					person := f.fresh("sub-prov")
					c := ssoSession(t, person, person+"@corp.example", r.role)
					key := r.prepare(t, f, person, c)
					header, want, by := row.setup(t, f, r, person, key)
					got := r.save(t, f, c, header, key)
					draftWant(t, got, want, by)
					if r.name == "unit-propose" {
						if by == "" && got.stored != nil || by != "" && (got.stored == nil || *got.stored != want) {
							t.Errorf("stored session = %v, want %v for %q", got.stored, want, by)
						}
					}
				})
			}
		})
	}
}

// A unit save applied directly makes no change record, so the mark rides its audit row alone, whatever the
// switch says: off, or on for a unit admin's exempt save and new unit.
func TestPG_AssistantProvenanceDirectWrite(t *testing.T) {
	save := func(t *testing.T, e *unitAdminEnv, c *http.Cookie, header string, id uuid.UUID, body string) map[string]any {
		t.Helper()
		r := httptest.NewRequest(http.MethodPut, "/api/v1/governance/units/"+id.String(), strings.NewReader(body))
		r.AddCookie(c)
		if header != "" {
			r.Header.Set(assistantSessionHeader, header)
		}
		w := httptest.NewRecorder()
		panicFails(t, e.srv.Handler()).ServeHTTP(w, r)
		if w.Code != http.StatusOK && w.Code != http.StatusCreated {
			t.Fatalf("direct unit save = %d %s, want it applied", w.Code, w.Body)
		}
		for _, ev := range e.audits("governance.unit.write") {
			if ev.Target == id.String() {
				row := map[string]any{}
				if err := json.Unmarshal(ev.Data, &row); err != nil {
					t.Fatalf("decode the row: %v", err)
				}
				return row
			}
		}
		t.Fatalf("no governance.unit.write row on %s", id)
		return nil
	}
	session := func(t *testing.T, e *unitAdminEnv, person, key string) uuid.UUID {
		t.Helper()
		id := uuid.New()
		_, err := e.pg.CreateAssistantSession(context.Background(), types.AssistantSession{ID: id, Principal: person, Email: person + "@corp.example",
			Role: oidc.RoleUser, Groups: []string{}, Purpose: types.AssistantCompose, TargetKind: types.AssistantTargetUnit, TargetKey: key,
			Binding: types.AssistantBinding{ProviderID: "a", ProviderUID: "uid-a", Model: "m1"}, State: types.AssistantSessionFetching})
		if err != nil {
			t.Fatalf("seed the session: %v", err)
		}
		return id
	}
	marked := func(t *testing.T, row map[string]any, session uuid.UUID, by string) {
		t.Helper()
		if by == "" {
			if _, ok := row["drafted_with_ai"]; ok {
				t.Errorf("the row is marked: %v", row)
			}
			return
		}
		if row["drafted_with_ai"] != true || row["assistant_session"] != session.String() || row["detected_by"] != by {
			t.Errorf("the row = %v, want session %s detected by %s", row, session, by)
		}
	}
	t.Run("switch-off-header-own-unit", func(t *testing.T) {
		e := newUnitAdminEnv(t, false)
		sess := session(t, e, govSubAlice, e.a.ID.String())
		marked(t, save(t, e, e.alice, sess.String(), e.a.ID, e.restate(e.a, nil)), sess, draftedByHeader)
	})
	t.Run("switch-off-no-header", func(t *testing.T) {
		e := newUnitAdminEnv(t, false)
		session(t, e, govSubAlice, e.a.ID.String())
		marked(t, save(t, e, e.alice, "", e.a.ID, e.restate(e.a, nil)), uuid.Nil, "")
	})
	t.Run("switch-off-other-unit", func(t *testing.T) {
		e := newUnitAdminEnv(t, false)
		sess := session(t, e, govSubAlice, e.a2.ID.String())
		marked(t, save(t, e, e.alice, sess.String(), e.a.ID, e.restate(e.a, nil)), uuid.Nil, "")
	})
	t.Run("switch-on-unit-admin-narrowing-header", func(t *testing.T) {
		e := newUnitAdminEnv(t, true)
		sess := session(t, e, uaSubAdminA, e.a1.ID.String())
		body := uaUnitBody("ua-a1", e.a.ID, uaDomains("pypi.org"), nil)
		marked(t, save(t, e, e.adminA, sess.String(), e.a1.ID, body), sess, draftedByHeader)
	})
	t.Run("switch-on-unit-admin-new-unit-header", func(t *testing.T) {
		e := newUnitAdminEnv(t, true)
		id := uuid.New()
		sess := session(t, e, uaSubAdminA, id.String())
		body := uaUnitBody(e.fresh("ua-new"), e.a.ID, map[string]any{}, nil)
		marked(t, save(t, e, e.adminA, sess.String(), id, body), sess, draftedByHeader)
	})
}

// A marked save is judged as the same save unmarked: the verdict, risk, holds and approvals needed are equal,
// on each of the four routes.
func TestPG_AssistantProvenanceEquality(t *testing.T) {
	t.Setenv(envGovernanceSecondHuman, "true")
	f := &draftFixture{presetFixture: newPresetFixture(t)}
	for _, r := range draftRoutes() {
		t.Run(r.name, func(t *testing.T) {
			saveAs := func(t *testing.T, mark bool) draftSaved {
				person := f.fresh("sub-eq")
				c := ssoSession(t, person, person+"@corp.example", r.role)
				key := r.prepare(t, f, person, c)
				header := ""
				if mark {
					header = f.seedSession(t, person, draftSession{kind: r.kind, key: draftSessionKey(r, key)}).String()
				}
				return r.save(t, f, c, header, key)
			}
			with, without := saveAs(t, true), saveAs(t, false)
			if with.audit["drafted_with_ai"] != true || without.audit["drafted_with_ai"] != nil {
				t.Fatalf("the marks are %v and %v, want one marked save and one unmarked", with.audit, without.audit)
			}
			if with.judged != without.judged {
				t.Errorf("a marked save was judged %q, the unmarked one %q", with.judged, without.judged)
			}
		})
	}
}
