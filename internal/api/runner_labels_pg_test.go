// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// Runner labels against a real Postgres: what a runner proposes at registration, the security operator's
// write, and the operator labels on run.create, run.queue.start and the run read. Guarded by WARDYN_TEST_PG.

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func (e *poolEnv) putLabels(c *http.Cookie, id uuid.UUID, body string) (int, string) {
	e.t.Helper()
	w := e.as(c, http.MethodPut, "/api/v1/runners/"+id.String()+"/labels", body)
	return w.Code, w.Body.String()
}

func (e *poolEnv) labelSets(id uuid.UUID) map[types.RunnerLabelSource]map[string]string {
	e.t.Helper()
	got, err := e.pg.RunnerLabels(context.Background(), []uuid.UUID{id})
	if err != nil {
		e.t.Fatal(err)
	}
	out := map[types.RunnerLabelSource]map[string]string{}
	for src, set := range got[id] {
		out[src] = set.Labels
	}
	return out
}

func (e *poolEnv) registerRaw(tok string, labels map[string]string) *httptest.ResponseRecorder {
	e.t.Helper()
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	body, _ := json.Marshal(types.RunnerRegisterRequest{Token: tok, PublicKey: pub, Name: "cluster-a", Limits: roomyRunnerLimits(), Labels: labels})
	return do(e.t, e.srv, http.MethodPost, "/api/v1/runners/register", "", string(body))
}

func TestRunnerRegister_LabelsStoredAsserted_PG(t *testing.T) {
	e := newOrgRunnerEnv(t, false)
	bad := e.mintOrg(e.alice)
	if w := e.registerRaw(bad.Token, map[string]string{"cluster": "https://x"}); w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), "runner_registration_invalid") {
		t.Fatalf("a registration with a bad label = %d %s, want 422 runner_registration_invalid", w.Code, w.Body)
	}
	// The refused registration did not spend its token.
	w := e.registerRaw(bad.Token, map[string]string{"cluster": "eu-1", "region": "west"})
	if w.Code != http.StatusCreated {
		t.Fatalf("the same token with good labels = %d %s, want 201", w.Code, w.Body)
	}
	id := decodeJSON[types.RunnerRegistration](t, w).RunnerID
	if got := e.labelSets(id); len(got) != 1 || !maps.Equal(got[types.RunnerLabelsAsserted], map[string]string{"cluster": "eu-1", "region": "west"}) {
		t.Fatalf("stored label sets = %v, want one runner_asserted row", got)
	}
	if rows := e.auditData(placement.ActionRunnerEnrol, id.String()); len(rows) != 1 || rows[0]["labels"] != nil || rows[0]["runner_labels"] != nil {
		t.Fatalf("runner.enrol rows = %v, want none carrying labels", rows)
	}
	if rows := e.auditData(placement.ActionRunnerLabelWrite, id.String()); len(rows) != 0 {
		t.Fatalf("a registration wrote runner.label.write rows: %v", rows)
	}
}

func TestRunnerLabels_Route_PG(t *testing.T) {
	e := newOrgRunnerEnv(t, false)
	id := e.claimedRunner("sub-person")
	body := `{"labels":{"cluster":"eu-1","region":"west"}}`
	if code, got := e.putLabels(e.user, id, body); code != http.StatusForbidden {
		t.Fatalf("a member's label write = %d %s, want 403", code, got)
	}
	if code, got := e.putLabels(e.person, id, body); code != http.StatusForbidden {
		t.Fatalf("the runner's owner's label write = %d %s, want 403", code, got)
	}
	for _, bad := range []string{`{"labels":{"a/b/c":"x"}}`, `{"labels":{"cluster":"a:b"}}`, `{"labels":{"cluster":"a b"}}`, `{"labels":{"cluster":"x"},"extra":1}`} {
		if code, got := e.putLabels(e.alice, id, bad); code != http.StatusBadRequest {
			t.Fatalf("PUT labels %s = %d %s, want 400", bad, code, got)
		}
	}
	if code, got := e.putLabels(e.alice, id, `{"labels":{"cluster":"x"}}`); code != http.StatusOK || !strings.Contains(got, `"labels_set_by"`) {
		t.Fatalf("a security admin's label write = %d %s", code, got)
	}
	code, got := e.putLabels(e.alice, id, body)
	var view types.RunnerView
	if code != http.StatusOK || json.Unmarshal([]byte(got), &view) != nil || !maps.Equal(view.Labels, map[string]string{"cluster": "eu-1", "region": "west"}) || view.LabelsSetBy != govSubAlice {
		t.Fatalf("a security admin's label write = %d %s", code, got)
	}
	rows := e.auditData(placement.ActionRunnerLabelWrite, id.String())
	if len(rows) != 2 || fmt.Sprint(rows[1]["before"]) != "map[cluster:x]" || fmt.Sprint(rows[1]["after"]) != "map[cluster:eu-1 region:west]" {
		t.Fatalf("runner.label.write rows = %v", rows)
	}
	// The same set again is a 200 and no row.
	if code, got := e.putLabels(e.alice, id, body); code != http.StatusOK || len(e.auditData(placement.ActionRunnerLabelWrite, id.String())) != 2 {
		t.Fatalf("the same labels again = %d %s, rows %v", code, got, e.auditData(placement.ActionRunnerLabelWrite, id.String()))
	}
	// An empty set clears them, and says so in the row.
	if code, got := e.putLabels(e.alice, id, `{"labels":{}}`); code != http.StatusOK || strings.Contains(got, `"labels":`) {
		t.Fatalf("clearing the labels = %d %s", code, got)
	}
	if rows := e.auditData(placement.ActionRunnerLabelWrite, id.String()); len(rows) != 3 || fmt.Sprint(rows[2]["after"]) != "map[]" {
		t.Fatalf("runner.label.write rows = %v", rows)
	}
	if code, got := e.putLabels(e.alice, uuid.New(), body); code != http.StatusNotFound {
		t.Fatalf("labels on no runner = %d %s, want 404", code, got)
	}
	org := e.registerWith(e.mintOrg(e.alice).Token)
	if code, got := e.putLabels(e.alice, org, body); code != http.StatusConflict || !strings.Contains(got, "runner_labels_invalid") {
		t.Fatalf("labels on an unclaimed runner = %d %s, want 409 runner_labels_invalid", code, got)
	}
}

// labelledLaptop is a pool of one laptop of the person's, reporting Wall and Fence, with operator labels.
func labelledLaptop(e *queueEnv, operator string) (types.RunnerPool, uuid.UUID) {
	e.t.Helper()
	laptops := e.createPool("Laptops", types.RunnerPoolUser)
	a := e.laptop(e.personP, 4000, 8192, types.CC1, types.CC2)
	e.addToPool(e.person, laptops.ID, a)
	e.stream(a, types.CC1, types.CC2)
	if operator != "" {
		if code, got := e.putLabels(e.alice, a, `{"labels":{"cluster":"`+operator+`"}}`); code != http.StatusOK {
			e.t.Fatalf("label the laptop = %d %s", code, got)
		}
	}
	return e.reread(laptops), a
}

// createdOn launches a run into pool and returns it once its run.create row is written.
func (e *queueEnv) createdOn(pool types.RunnerPool) (types.AgentRun, map[string]any) {
	e.t.Helper()
	w := e.call(e.person, http.MethodPost, "/api/v1/runs", runBody(fmt.Sprintf(`"runner_pool_id":%q`, pool.ID)))
	if w.Code != http.StatusCreated {
		e.t.Fatalf("launch = %d %s", w.Code, w.Body)
	}
	run := decodeJSON[types.AgentRun](e.t, w)
	return run, queueAuditData(e.t, e.waitAudit(run.ID, "run.create")[0])
}

func TestRunCreate_RecordsOperatorLabelsOnly_PG(t *testing.T) {
	e := newQueueEnv(t)
	pool, a := labelledLaptop(e, "")
	// Only a runner's own proposal: the row carries nothing.
	if _, err := e.pg.SetRunnerLabels(context.Background(), a, types.RunnerLabelsAsserted, map[string]string{"cluster": "a"}, "runner:"+a.String(), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	run, data := e.createdOn(pool)
	if _, has := data["runner_labels"]; has || run.RunnerID == nil || *run.RunnerID != a {
		t.Fatalf("run %v on %v, run.create = %v; want the laptop and no runner_labels", run.ID, run.RunnerID, data)
	}
	// With an operator set that differs, the row carries the operator's.
	if code, got := e.putLabels(e.alice, a, `{"labels":{"cluster":"b"}}`); code != http.StatusOK {
		t.Fatalf("label the laptop = %d %s", code, got)
	}
	_, data = e.createdOn(pool)
	if got, _ := data["runner_labels"].(map[string]any); len(got) != 1 || got["cluster"] != "b" {
		t.Fatalf("run.create runner_labels = %v, want {cluster: b}", data["runner_labels"])
	}
}

func TestRunQueueStart_RecordsOperatorLabels_PG(t *testing.T) {
	e := newQueueEnv(t)
	pool, a := labelledLaptop(e, "b")
	e.maxRuns(a, e.personP, 1)
	occupyRunner(e, e.reread(pool), a)
	waiting := e.waitsFor(runBody(fmt.Sprintf(`"runner_pool_id":%q`, pool.ID)), "the only laptop is full")
	e.endOccupant(a.String())
	if got := e.settles(waiting.ID); got.State != types.RunRunning || got.RunnerID == nil || *got.RunnerID != a {
		t.Fatalf("the run is %s on %v (%s), want it RUNNING on the laptop", got.State, got.RunnerID, got.StatusDetail)
	}
	data := queueAuditData(t, e.waitAudit(waiting.ID, "run.queue.start")[0])
	if got, _ := data["runner_labels"].(map[string]any); len(got) != 1 || got["cluster"] != "b" {
		t.Fatalf("run.queue.start = %v, want runner_labels {cluster: b}", data)
	}
}

func TestRunRead_ProjectsCurrentOperatorLabels_PG(t *testing.T) {
	e := newQueueEnv(t)
	pool, a := labelledLaptop(e, "b")
	run, _ := e.createdOn(pool)
	read := func() map[string]any {
		got, _ := e.runDetail(run.ID)["runner_labels"].(map[string]any)
		return got
	}
	if got := read(); len(got) != 1 || got["cluster"] != "b" {
		t.Fatalf("the run read shows %v, want {cluster: b}", got)
	}
	if code, got := e.putLabels(e.alice, a, `{"labels":{"cluster":"c"}}`); code != http.StatusOK {
		t.Fatalf("relabel = %d %s", code, got)
	}
	if got := read(); len(got) != 1 || got["cluster"] != "c" {
		t.Fatalf("after the relabel the run read shows %v, want {cluster: c}", got)
	}
	created := queueAuditData(t, e.audits(run.ID, "run.create")[0])
	if got, _ := created["runner_labels"].(map[string]any); len(got) != 1 || got["cluster"] != "b" {
		t.Fatalf("the run.create row now says %v, want the labels of its create ({cluster: b})", created["runner_labels"])
	}
	// A run on a runner with no operator labels shows none.
	if code, _ := e.putLabels(e.alice, a, `{"labels":{}}`); code != http.StatusOK {
		t.Fatal("clear the labels")
	}
	if got := read(); got != nil {
		t.Fatalf("a runner with no labels shows %v on its run, want none", got)
	}
}

func TestRunnerView_LabelsSetByOnAdminRoutesOnly_PG(t *testing.T) {
	e := newOrgRunnerEnv(t, false)
	id := e.claimedRunner(e.personP)
	if _, err := e.pg.SetRunnerLabels(context.Background(), id, types.RunnerLabelsAsserted, map[string]string{"region": "west"}, "runner:"+id.String(), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if code, got := e.putLabels(e.alice, id, `{"labels":{"cluster":"eu-1"}}`); code != http.StatusOK {
		t.Fatalf("label = %d %s", code, got)
	}
	mine := e.call(e.person, http.MethodGet, "/api/v1/me/runners/"+id.String(), "")
	if mine.Code != http.StatusOK || strings.Contains(mine.Body.String(), "labels_set_by") || strings.Contains(mine.Body.String(), "labels_set_at") {
		t.Fatalf("the owner's read = %d %s, want labels and no labels_set_by", mine.Code, mine.Body)
	}
	if v := decodeJSON[types.RunnerView](t, mine); !maps.Equal(v.Labels, map[string]string{"cluster": "eu-1"}) || !maps.Equal(v.AssertedLabels, map[string]string{"region": "west"}) {
		t.Fatalf("the owner's read = %+v, want both label sets", v)
	}
	admin := e.call(e.alice, http.MethodGet, "/api/v1/runners/"+id.String(), "")
	if v := decodeJSON[types.RunnerView](t, admin); admin.Code != http.StatusOK || v.LabelsSetBy != govSubAlice || v.LabelsSetAt == nil {
		t.Fatalf("an administrator's read = %d %s, want labels_set_by", admin.Code, admin.Body)
	}
}
