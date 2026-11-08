// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The unit-scoped audit read: a unit admin reads the rows of runs frozen to a unit in their subtrees,
// through the allow-listed unit view, and nothing else; every other reader is served as before.

const uaRunOwner = "sub-ua-run-owner"

// uaNeverKeys are data keys a run.create of another person's run carries that the unit view never serves.
var uaNeverKeys = []string{"policy_source", "personal_walls", "restriction_ids", "contact", "spec", "error", "detail", "owner"}

// unitAuditEnv is unitAdminEnv plus runs frozen to its units, three rows each: runA and runA1 below
// adminA's grant, runB beside it, runNone with no unit, and runOwnA, adminA's own run in ua-a. Two rows
// carry no run at all.
type unitAuditEnv struct {
	*unitAdminEnv
	runA, runA1, runB, runNone, runOwnA uuid.UUID
}

func newUnitAuditEnv(t *testing.T) *unitAuditEnv {
	t.Helper()
	e := &unitAuditEnv{unitAdminEnv: newUnitAdminEnv(t, false)}
	e.runA = e.seedRun(&e.a.ID, uaRunOwner)
	e.runA1 = e.seedRun(&e.a1.ID, uaRunOwner)
	e.runB = e.seedRun(&e.b.ID, uaRunOwner)
	e.runNone = e.seedRun(nil, uaRunOwner)
	e.runOwnA = e.seedRun(&e.a.ID, uaSubAdminA)
	e.seedRow(nil, "governance.unit.write", "ua-a", map[string]any{"unit": "ua-a"})
	e.seedRow(nil, "auth.sign_in", uaSubAdminA, map[string]any{"reason": "ok"})
	return e
}

// seedRun stores a run frozen to unit (nil: none) and its three rows.
func (e *unitAuditEnv) seedRun(unit *uuid.UUID, owner string) uuid.UUID {
	e.t.Helper()
	id, now := uuid.New(), time.Now().UTC()
	run := types.AgentRun{ID: id, CreatedAt: now, UpdatedAt: now, CreatedBy: owner, Agent: "claude-code",
		Task: "unit audit", ConfinementClass: types.CC2, State: types.RunRunning,
		SPIFFEID: "spiffe://wardyn.local/agent-run/" + id.String(), RunnerTarget: "docker"}
	if unit != nil {
		u := *unit
		run.UnitID = &u
	}
	if _, err := e.pg.CreateRun(context.Background(), run); err != nil {
		e.t.Fatalf("create run: %v", err)
	}
	e.seedRow(&id, "run.create", id.String(), map[string]any{
		"agent": "claude-code", "repo": "octocat/hello", "confinement_class": "CC2",
		"policy_source": map[string]any{"personal_walls": true,
			"provenance": []any{map[string]any{"layer": "restriction", "name": "no-prod-access"}}},
		"personal_walls": true, "restriction_ids": []string{uuid.NewString()},
		"contact": map[string]any{"email": "owner@corp.example"},
		"spec":    map[string]any{"allowed_domains": []string{"internal.corp.example"}},
		"error":   "open /home/alice/.ssh/id_ed25519: permission denied", "detail": "alice's laptop",
		"owner": "alice@corp.example",
	})
	e.seedRow(&id, "egress.deny", "evil.example", map[string]any{
		"host": "evil.example", "port": 443, "reason": "not_allowed", "cause": map[string]any{"path": "/home/alice/x"},
	})
	e.seedRow(&id, "secret.read", "prod-db-password", map[string]any{"secret_name": "prod-db-password"})
	return id
}

func (e *unitAuditEnv) seedRow(run *uuid.UUID, action, target string, data map[string]any) {
	e.t.Helper()
	raw, err := json.Marshal(data)
	if err != nil {
		e.t.Fatalf("marshal row data: %v", err)
	}
	ev := types.AuditEvent{ID: uuid.New(), Time: time.Now().UTC(), RunID: run, ActorType: types.ActorHuman,
		Actor: "alice@corp.example", Action: action, Target: target, Outcome: "success", SourceIP: "203.0.113.7", Data: raw}
	if err := store.InsertAuditEvent(context.Background(), e.pool, &ev); err != nil {
		e.t.Fatalf("seed %s: %v", action, err)
	}
}

// read is GET path+query as c, which must answer 200, decoded from JSON or NDJSON by the path.
func (e *unitAuditEnv) read(c *http.Cookie, path, query string) []types.AuditEvent {
	e.t.Helper()
	w := e.call(c, http.MethodGet, path+query, "")
	if w.Code != http.StatusOK {
		e.t.Fatalf("GET %s%s: %d %s", path, query, w.Code, w.Body.String())
	}
	return uaRows(e.t, path, w)
}

func uaRows(t *testing.T, path string, w *httptest.ResponseRecorder) []types.AuditEvent {
	t.Helper()
	if path == "/api/v1/audit/export" {
		return ndjsonLines(t, w.Body.Bytes())
	}
	var out []types.AuditEvent
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %s: %v (body=%s)", path, err, w.Body.String())
	}
	return out
}

// uaPaths are both audit reads; a unit admin is served rows on the first only, so a test that reads
// rows of a unit admin uses uaViewPaths and one that compares an answer with a member's uses uaPaths.
var uaPaths = []string{"/api/v1/audit", "/api/v1/audit/export"}
var uaViewPaths = []string{"/api/v1/audit"}

// uaRunCounts is the number of rows per run, and of rows with no run.
func uaRunCounts(evs []types.AuditEvent) (map[uuid.UUID]int, int) {
	runs, none := map[uuid.UUID]int{}, 0
	for _, ev := range evs {
		if ev.RunID == nil {
			none++
			continue
		}
		runs[*ev.RunID]++
	}
	return runs, none
}

func uaRow(evs []types.AuditEvent, run uuid.UUID, action string) (types.AuditEvent, bool) {
	for _, ev := range evs {
		if ev.RunID != nil && *ev.RunID == run && ev.Action == action {
			return ev, true
		}
	}
	return types.AuditEvent{}, false
}

func uaDataKeys(t *testing.T, ev types.AuditEvent) map[string]json.RawMessage {
	t.Helper()
	m := map[string]json.RawMessage{}
	if len(ev.Data) > 0 {
		if err := json.Unmarshal(ev.Data, &m); err != nil {
			t.Fatalf("%s data is not an object: %s", ev.Action, ev.Data)
		}
	}
	return m
}

// uaSame fails unless got and want answer with the same status, media type and body bytes.
func uaSame(t *testing.T, what string, got, want *httptest.ResponseRecorder) {
	t.Helper()
	if got.Code != want.Code || got.Header().Get("Content-Type") != want.Header().Get("Content-Type") ||
		!bytes.Equal(got.Body.Bytes(), want.Body.Bytes()) {
		t.Errorf("%s: %d %q %q, want what a member gets: %d %q %q", what,
			got.Code, got.Header().Get("Content-Type"), got.Body.String(),
			want.Code, want.Header().Get("Content-Type"), want.Body.String())
	}
}

func TestAuditUnitAdminSeesOnlySubtreeRuns(t *testing.T) {
	e := newUnitAuditEnv(t)
	for _, path := range uaViewPaths {
		runs, none := uaRunCounts(e.read(e.adminA, path, ""))
		for name, run := range map[string]uuid.UUID{"ua-a": e.runA, "ua-a1": e.runA1, "their own in ua-a": e.runOwnA} {
			if runs[run] != 3 {
				t.Errorf("%s: adminA got %d rows of the run in %s, want 3", path, runs[run], name)
			}
		}
		if runs[e.runB] != 0 || runs[e.runNone] != 0 || none != 0 {
			t.Errorf("%s: adminA got %d rows of ua-b's run, %d of the run with no unit and %d with no run, want none",
				path, runs[e.runB], runs[e.runNone], none)
		}
		runs, none = uaRunCounts(e.read(e.adminA1, path, ""))
		if runs[e.runA1] != 3 || runs[e.runA] != 0 || runs[e.runOwnA] != 0 || runs[e.runB] != 0 || none != 0 {
			t.Errorf("%s: adminA1 got %v and %d rows with no run, want ua-a1's run alone", path, runs, none)
		}
		runs, _ = uaRunCounts(e.read(e.groupA, path, ""))
		if runs[e.runA] != 3 || runs[e.runB] != 0 {
			t.Errorf("%s: the group-granted admin of ua-a got %v, want ua-a's runs and not ua-b's", path, runs)
		}
	}
}

func TestAuditUnitAdminForeignRunIsEmpty200(t *testing.T) {
	e := newUnitAuditEnv(t)
	for _, path := range uaPaths {
		if path == "/api/v1/audit" {
			if got := e.read(e.adminA, path, "?run_id="+e.runA.String()); len(got) != 3 {
				t.Errorf("%s: adminA got %d rows of a subtree run's trail, want 3", path, len(got))
			}
		}
		for name, q := range map[string]string{
			"ua-b's run": "?run_id=" + e.runB.String(), "a run with no unit": "?run_id=" + e.runNone.String(),
			"an unknown run": "?run_id=" + uuid.NewString(), "a malformed id": "?run_id=not-a-uuid",
		} {
			uaSame(t, path+", "+name, e.call(e.adminA, http.MethodGet, path+q, ""), e.call(e.member, http.MethodGet, path+q, ""))
		}
	}
}

func TestAuditUnitViewDropsPrivateKeys(t *testing.T) {
	e := newUnitAuditEnv(t)
	for _, path := range uaViewPaths {
		for name, evs := range map[string][]types.AuditEvent{
			"trail": e.read(e.adminA, path, "?run_id="+e.runA.String()),
			"feed":  e.read(e.adminA, path, ""),
		} {
			create, ok := uaRow(evs, e.runA, "run.create")
			if !ok {
				t.Errorf("%s %s: no run.create of ua-a's run", path, name)
				continue
			}
			keys := uaDataKeys(t, create)
			for _, k := range uaNeverKeys {
				if _, has := keys[k]; has {
					t.Errorf("%s %s: run.create serves %q to a unit admin", path, name, k)
				}
			}
			for _, k := range []string{"agent", "repo", "confinement_class"} {
				if _, has := keys[k]; !has {
					t.Errorf("%s %s: run.create lost %q, which the unit view keeps", path, name, k)
				}
			}
			if create.Target != e.runA.String() || create.SourceIP != "" {
				t.Errorf("%s %s: run.create target %q source_ip %q, want the run id and no address", path, name, create.Target, create.SourceIP)
			}
			deny, _ := uaRow(evs, e.runA, "egress.deny")
			keys = uaDataKeys(t, deny)
			if deny.Target != "evil.example" || string(keys["host"]) != `"evil.example"` || string(keys["port"]) != "443" {
				t.Errorf("%s %s: egress.deny = %q %s, want its target, host and port", path, name, deny.Target, deny.Data)
			}
			if _, has := keys["cause"]; has {
				t.Errorf("%s %s: egress.deny serves an object under an allowed key: %s", path, name, deny.Data)
			}
			read, ok := uaRow(evs, e.runA, "secret.read")
			if !ok || read.Target != "" || len(uaDataKeys(t, read)) != 0 {
				t.Errorf("%s %s: secret.read = %v %q %s, want the row with no target and no data", path, name, ok, read.Target, read.Data)
			}
		}
	}
}

func TestAuditUnitAdminStaleGroupSnapshot(t *testing.T) {
	e := newUnitAuditEnv(t)
	if runs, _ := uaRunCounts(e.read(e.groupA, "/api/v1/audit", "")); runs[e.runA] != 3 {
		t.Fatalf("a fresh snapshot of the granted group got %v, want ua-a's run", runs)
	}
	for name, c := range map[string]*http.Cookie{
		"never taken": uaSession(t, uaSubGroupA, nil, false),
		"truncated":   uaSession(t, uaSubGroupA, []string{uaGroupA}, true),
	} {
		for _, path := range uaPaths {
			for _, q := range []string{"", "?run_id=" + e.runA.String()} {
				uaSame(t, name+" "+path+q, e.call(c, http.MethodGet, path+q, ""), e.call(e.member, http.MethodGet, path+q, ""))
			}
		}
	}
}

func TestAuditUnitAdminRevokedMidPaging(t *testing.T) {
	e := newUnitAuditEnv(t)
	const sub = "sub-ua-revoked"
	g := e.grant(e.a, types.CapabilitySubjectUser, sub)
	c := uaSession(t, sub, []string{}, false)
	w := e.call(c, http.MethodGet, "/api/v1/audit?limit=2", "")
	if got := uaRows(t, "/api/v1/audit", w); w.Code != http.StatusOK || len(got) != 2 || w.Header().Get("X-Wardyn-Truncated") != "true" {
		t.Fatalf("first page: %d, %d rows, truncated %q; want 2 rows of more", w.Code, len(got), w.Header().Get("X-Wardyn-Truncated"))
	}
	if _, err := store.RevokeUnitAdminQ(context.Background(), e.pool, e.a.ID, g.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	for _, path := range uaPaths {
		q := "?limit=2&offset=2"
		uaSame(t, "after the revoke "+path, e.call(c, http.MethodGet, path+q, ""), e.call(e.member, http.MethodGet, path+q, ""))
	}
}

func TestAuditUnitAdminReparentOut(t *testing.T) {
	e := newUnitAuditEnv(t)
	moved := e.child(e.a)
	run := e.seedRun(&moved.ID, uaRunOwner)
	if runs, _ := uaRunCounts(e.read(e.adminA, "/api/v1/audit", "")); runs[run] != 3 {
		t.Fatalf("before the move adminA got %d rows of the run below ua-a, want 3", runs[run])
	}
	if w := e.admin(http.MethodPut, "/api/v1/governance/units/"+moved.ID.String(), e.restate(moved, map[string]any{"parent_id": e.b.ID})); w.Code != http.StatusOK {
		t.Fatalf("move the unit below ua-b: %d %s", w.Code, w.Body.String())
	}
	for _, path := range uaViewPaths {
		if runs, _ := uaRunCounts(e.read(e.adminA, path, "")); runs[run] != 0 {
			t.Errorf("%s: after the move adminA still got %d rows of the moved unit's run", path, runs[run])
		}
		if got := e.read(e.adminA, path, "?run_id="+run.String()); len(got) != 0 {
			t.Errorf("%s: after the move adminA still got the moved run's trail (%d rows)", path, len(got))
		}
		if runs, _ := uaRunCounts(e.read(e.adminB, path, "")); runs[run] != 3 {
			t.Errorf("%s: after the move adminB got %d rows of the run now below ua-b, want 3", path, runs[run])
		}
	}
}

func TestAuditNoGrantCallers(t *testing.T) {
	e := newUnitAuditEnv(t)
	person := withOIDCGroups(operatorCtx(uaSubAdminA, uaSubAdminA+"@corp.example", oidc.RoleUser), []string{})
	member := withOIDCGroups(operatorCtx("sub-ua-member", "sub-ua-member@corp.example", oidc.RoleUser), []string{})
	handlers := map[string]http.HandlerFunc{"/api/v1/audit": e.srv.handleQueryAudit, "/api/v1/audit/export": e.srv.handleExportAudit}
	for path, h := range handlers {
		want := 3 // the view; the export is a member's empty answer, with or without a grant
		if path == "/api/v1/audit/export" {
			want = 0
		}
		if runs, _ := uaRunCounts(uaRows(t, path, memberCall(person, h, http.MethodGet, path, "", nil))); runs[e.runA] != want {
			t.Errorf("%s: the granted person got %d rows of ua-a's run, want %d", path, runs[e.runA], want)
		}
		for name, ctx := range map[string]context.Context{
			"device":           context.WithValue(person, deviceCtxKey{}, types.Device{ID: uuid.New()}),
			"delegated":        audit.WithDelegation(person, types.DelegationVia{Delegate: uuid.New(), Grant: uuid.New()}),
			"service identity": withServiceIdentity(person, "ci-bot", uaSubAdminA),
			"scim":             withSCIMCaller(person, "primary"),
		} {
			uaSame(t, path+" "+name, memberCall(ctx, h, http.MethodGet, path, "", nil), memberCall(member, h, http.MethodGet, path, "", nil))
		}
		for name, ctx := range map[string]context.Context{
			"admin token": context.Background(),
			"local mode":  withLocalPrincipal(context.Background(), "local:ops"),
		} {
			evs := uaRows(t, path, memberCall(ctx, h, http.MethodGet, path, "", nil))
			create, ok := uaRow(evs, e.runB, "run.create")
			if _, walls := uaDataKeys(t, create)["personal_walls"]; !ok || !walls {
				t.Errorf("%s %s: the whole log's run.create of ua-b's run = %v %s, want it whole", path, name, ok, create.Data)
			}
		}
	}
}

func TestAuditSecurityAndMemberGolden(t *testing.T) {
	e := newUnitAuditEnv(t)
	ctx := context.Background()
	canon := func(evs []types.AuditEvent) string {
		b, err := json.Marshal(evs)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return string(b)
	}
	security := func(what string) {
		t.Helper()
		recent, err := e.pg.QueryRecentAuditEventsPage(ctx, store.Page{Limit: auditGlobalDefaultLimit})
		if err != nil {
			t.Fatalf("recent: %v", err)
		}
		if got, want := canon(e.read(e.alice, "/api/v1/audit", "")), canon(withoutHostDetail(recent)); got != want {
			t.Errorf("%s: the security tier's feed changed:\n got %s\nwant %s", what, got, want)
		}
	}
	security("security admin")
	e.grant(e.a, types.CapabilitySubjectUser, govSubAlice)
	security("security admin who also holds a unit grant")

	trail, err := e.pg.QueryAuditEventsPage(ctx, e.runOwnA, store.Page{Limit: auditPerRunDefaultLimit})
	if err != nil {
		t.Fatalf("trail: %v", err)
	}
	if got, want := canon(e.read(e.adminA, "/api/v1/audit", "?run_id="+e.runOwnA.String())), canon(trail); got != want {
		t.Errorf("a unit admin's own run's trail changed:\n got %s\nwant %s", got, want)
	}

	feed := e.read(e.adminA, "/api/v1/audit", "")
	own, ok := uaRow(feed, e.runOwnA, "run.create")
	if _, walls := uaDataKeys(t, own)["personal_walls"]; !ok || !walls {
		t.Errorf("the unit feed's run.create of adminA's own run = %v %s, want the member view of it", ok, own.Data)
	}
	other, ok := uaRow(feed, e.runA, "run.create")
	if _, walls := uaDataKeys(t, other)["personal_walls"]; !ok || walls {
		t.Errorf("the unit feed's run.create of another person's run = %v %s, want the unit view of it", ok, other.Data)
	}
}

// A unit admin has the view on GET /audit and no export: the export answers the empty NDJSON 200 a member
// gets, for the feed, for a run frozen to their unit that someone else created, and for a run outside it.
// The creator of a run still exports it, as any owner does.
func TestAuditExportUnitAdminIsEmpty(t *testing.T) {
	e := newUnitAuditEnv(t)
	if runs, _ := uaRunCounts(e.read(e.adminA, "/api/v1/audit", "")); runs[e.runA] != 3 {
		t.Fatalf("adminA's view has %d rows of ua-a's run, want the 3 the export must not serve", runs[e.runA])
	}
	for name, q := range map[string]string{
		"the feed":                          "",
		"a filtered feed":                   "?action=egress.deny",
		"a subtree run they did not create": "?run_id=" + e.runA.String(),
		"a child unit's run":                "?run_id=" + e.runA1.String(),
		"ua-b's run":                        "?run_id=" + e.runB.String(),
	} {
		w := e.call(e.adminA, http.MethodGet, "/api/v1/audit/export"+q, "")
		if w.Code != http.StatusOK || w.Body.Len() != 0 || w.Header().Get("Content-Type") != "application/x-ndjson" {
			t.Errorf("adminA's export of %s: %d %q %q, want an empty NDJSON 200", name, w.Code, w.Header().Get("Content-Type"), w.Body.String())
		}
		uaSame(t, "adminA's export of "+name, w, e.call(e.member, http.MethodGet, "/api/v1/audit/export"+q, ""))
	}
	if own := e.read(e.adminA, "/api/v1/audit/export", "?run_id="+e.runOwnA.String()); len(own) != 3 {
		t.Errorf("adminA's export of the run they created = %d rows, want its 3", len(own))
	}
}
