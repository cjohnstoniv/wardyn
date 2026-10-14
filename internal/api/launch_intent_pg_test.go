// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// Launch intents through POST /runs and the two read routes, on a real
// Postgres. Guarded by WARDYN_TEST_PG: skipped cleanly when unset, must PASS
// when set.

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/erasure"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// liLaunch creates a run as c and waits for its dispatch, so no write of it
// lands after the throwaway database is dropped.
func (f presetFixture) liLaunch(t *testing.T, c *http.Cookie, body string) uuid.UUID {
	t.Helper()
	run := mustCreated(t, doSSO(t, f.srv, http.MethodPost, "/api/v1/runs", c, body))
	f.dispatched(t, run.ID)
	return run.ID
}

// DR-0097: the intent shares the run row's transaction, through handleCreateRun.
// An intent insert that fails leaves no run.
func TestLaunchIntentSameTransaction(t *testing.T) {
	f := newPresetFixture(t)
	ctx := context.Background()
	if _, err := f.pg.Pool.Exec(ctx, `
		CREATE FUNCTION li_refuse() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'intent refused'; END $$;
		CREATE TRIGGER li_refuse BEFORE INSERT ON run_launch_intents FOR EACH ROW EXECUTE FUNCTION li_refuse();`); err != nil {
		t.Fatal(err)
	}
	w := doSSO(t, f.srv, http.MethodPost, "/api/v1/runs", f.member, `{"agent":"claude-code","task":"t"}`)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("create with a failing intent insert = %d %s, want 500", w.Code, w.Body.String())
	}
	var runs int
	if err := f.pg.Pool.QueryRow(ctx, `SELECT count(*) FROM agent_runs`).Scan(&runs); err != nil || runs != 0 {
		t.Errorf("agent_runs holds %d row(s) (%v) after the intent insert failed, want none", runs, err)
	}
}

// A preset launch keeps the request the preset stands for, without its stamp.
func TestLaunchIntentContentPresetExpands(t *testing.T) {
	f := newPresetFixture(t)
	if w := f.putPreset(t, "widgets", client.PresetRequest{Request: client.CreateRunRequest{Agent: "claude-code", Repo: "acme/widgets"}}); w.Code/100 != 2 {
		t.Fatalf("put preset = %d %s", w.Code, w.Body.String())
	}
	id := f.liLaunch(t, f.member, `{"preset":"widgets","title":"Nightly","task":"make test"}`)
	rec, err := f.pg.GetLaunchIntent(context.Background(), id, "sub-preset-member")
	if err != nil {
		t.Fatal(err)
	}
	intent := liIntent(t, rec.Document)
	for k, want := range map[string]string{"agent": `"claude-code"`, "repo": `"acme/widgets"`, "task": `"make test"`} {
		if string(intent[k]) != want {
			t.Errorf("intent.%s = %s, want %s", k, intent[k], want)
		}
	}
	for _, k := range []string{"preset", "preset_version", "title"} {
		if _, ok := intent[k]; ok {
			t.Errorf("intent holds %s: %s", k, rec.Document)
		}
	}
}

func TestLaunchIntentListNewestOwnOnly(t *testing.T) {
	f := newPresetFixture(t)
	other := ssoSession(t, "sub-preset-other", "other@corp.example", oidc.RoleUser)
	older := f.liLaunch(t, f.member, `{"title":"older","agent":"claude-code","task":"one"}`)
	newer := f.liLaunch(t, f.member, `{"title":"newer","agent":"claude-code","task":"two"}`)
	theirs := f.liLaunch(t, other, `{"agent":"claude-code","task":"three"}`)
	list := func(c *http.Cookie, query string) (int, launchIntentList) {
		w := doSSO(t, f.srv, http.MethodGet, "/api/v1/me/launch-intents"+query, c, "")
		var out launchIntentList
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	ids := func(l launchIntentList) []uuid.UUID {
		var out []uuid.UUID
		for _, s := range l.Intents {
			out = append(out, s.RunID)
		}
		return out
	}
	if code, l := list(f.member, ""); code != http.StatusOK || !slices.Equal(ids(l), []uuid.UUID{newer, older}) || l.Intents[0].Title != "newer" {
		t.Errorf("member's list = %d %+v, want newer then older", code, l)
	}
	if _, l := list(f.member, "?limit=1"); !slices.Equal(ids(l), []uuid.UUID{newer}) {
		t.Errorf("limit 1 = %v", ids(l))
	}
	if _, l := list(f.member, "?limit=500"); len(l.Intents) != 2 {
		t.Errorf("limit 500 = %v", ids(l))
	}
	if code, _ := list(f.member, "?limit=x"); code != http.StatusBadRequest {
		t.Errorf("limit=x = %d, want 400", code)
	}
	if _, l := list(other, ""); !slices.Equal(ids(l), []uuid.UUID{theirs}) {
		t.Errorf("the other member's list = %v, want only theirs", ids(l))
	}
	w := doSSO(t, f.srv, http.MethodGet, "/api/v1/runs/"+older.String()+"/launch-intent", f.member, "")
	var got launchIntentView
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || w.Code != http.StatusOK || got.RunID != older ||
		got.Document.Kind != client.RunDocumentKind || string(got.Document.Intent.Raw("task")) != `"one"` {
		t.Errorf("GET own launch intent = %d %s", w.Code, w.Body.String())
	}
}

// run_tasks, components and run_setups each delete the person's launch
// intents; the SCIM purge asks for the last two.
func TestLaunchIntentErasure(t *testing.T) {
	f := newPresetFixture(t)
	ctx := context.Background()
	for _, scope := range []erasure.Scope{erasure.RunTasks, erasure.Components, erasure.RunSetups} {
		t.Run(string(scope), func(t *testing.T) {
			id := f.liLaunch(t, f.member, `{"agent":"claude-code","task":"secret plans"}`)
			run, err := f.pg.GetRun(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.pg.GetLaunchIntent(ctx, id, run.CreatedBy); err != nil {
				t.Fatalf("the run kept no intent: %v", err)
			}
			rep, err := f.srv.erasureOrchestrator(nil, nil).Orchestrate(ctx, run.CreatedBy, []erasure.Scope{scope})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.pg.GetLaunchIntent(ctx, id, run.CreatedBy); err != store.ErrNotFound {
				t.Errorf("after %s the intent is still there: %v", scope, err)
			}
			if got := string(mustJSON(rep.Details[scope])); !json.Valid([]byte(got)) || !containsJSONKey(got, "launch_intents", 1) {
				t.Errorf("%s detail = %s, want launch_intents 1", scope, got)
			}
		})
	}
	for _, s := range []erasure.Scope{erasure.Components, erasure.RunSetups} {
		if !slices.Contains(scimPurgeScopes, s) {
			t.Errorf("the SCIM purge no longer asks for %s", s)
		}
	}
}

func containsJSONKey(obj, key string, want float64) bool {
	var m map[string]any
	return json.Unmarshal([]byte(obj), &m) == nil && m[key] == want
}

// The SCIM purge erases the person's launch intents with their run setups, and
// its person.deprovision counts still read the templates it erased.
func TestLaunchIntentSCIMPurge(t *testing.T) {
	f := newPresetFixture(t)
	ctx := context.Background()
	if w := doSSO(t, f.srv, http.MethodPost, "/api/v1/templates", f.member, templateSaveBody("person", "", "mine")); w.Code != http.StatusCreated {
		t.Fatalf("save a template = %d %s", w.Code, w.Body.String())
	}
	id := f.liLaunch(t, f.member, `{"agent":"claude-code","task":"t"}`)
	run, err := f.pg.GetRun(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	detail, err := f.srv.eraseForPurge(ctx, run.CreatedBy)
	if err != nil {
		t.Fatal(err)
	}
	if detail["templates_erased"] != 1 {
		t.Errorf("purge detail = %v, want templates_erased 1", detail)
	}
	if _, err := f.pg.GetLaunchIntent(ctx, id, run.CreatedBy); err != store.ErrNotFound {
		t.Errorf("the purge left the launch intent: %v", err)
	}
}

// A launch that waits in a capped pool's queue keeps its intent in the queued
// create's own transaction, without the wait it chose.
func TestLaunchIntentQueuedLaunchKeepsOne(t *testing.T) {
	e := newQueueEnv(t)
	pool := e.cappedPool("Busy", 1)
	e.occupy(pool)
	q := e.queuedLaunch(nil, pool, `,"max_wait_sec":600`)
	run := e.run(q.ID)
	rec, err := e.pg.GetLaunchIntent(context.Background(), q.ID, run.CreatedBy)
	if err != nil {
		t.Fatalf("the queued run kept no intent: %v", err)
	}
	if intent := liIntent(t, rec.Document); intent["max_wait_sec"] != nil || intent["workload"] == nil {
		t.Errorf("queued intent = %s", rec.Document)
	}
	if data := queueAuditData(t, e.waitAudit(q.ID, "run.create")[0]); data["launch_intent"] != "kept" {
		t.Errorf("queued run.create launch_intent = %v, want kept", data["launch_intent"])
	}
}
