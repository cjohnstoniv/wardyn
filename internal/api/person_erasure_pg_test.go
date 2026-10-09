// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// Person erasure against a real Postgres: every scope reaches the copy it is
// named for on every replica, the sealed audit fields read erased with the
// chain still verifying, and a retry finishes what a failure left. Two Servers
// over one database are two wardynds, each with its own key cache. Guarded by
// WARDYN_TEST_PG (throwawayPGPool); skipped cleanly when unset.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/recording"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/subjectkey"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// sealKeysOf is cmd/wardynd's adapter: a generation is named by its handle, and
// a destroyed one is ErrKeyErased.
type sealKeysOf struct{ m *subjectkey.Manager }

func (k sealKeysOf) Current(ctx context.Context, owner, purpose string) (uuid.UUID, []byte, error) {
	return k.m.CurrentHandle(ctx, owner, purpose)
}

func (k sealKeysOf) Key(ctx context.Context, purpose string, handle uuid.UUID) ([]byte, error) {
	key, err := k.m.KeyByHandle(ctx, purpose, handle)
	if errors.Is(err, subjectkey.ErrDataLoss) {
		return nil, fmt.Errorf("%w: %w", audit.ErrKeyErased, err)
	}
	return key, err
}

// failBlankStore is the PG store whose task blanking can be made to fail.
type failBlankStore struct {
	store.PG
	fail *bool
}

func (s failBlankStore) BlankRunTasks(ctx context.Context, by string) (int, error) {
	if *s.fail {
		return 0, errors.New("injected: the store refused the task blanking")
	}
	return s.PG.BlankRunTasks(ctx, by)
}

// erasureReplica is a maskLab replica wired for erasure: this replica's own
// subject keys read and seal audit fields, recordings live in Postgres, and
// run_tasks can be made to fail.
func erasureReplica(l *maskLab, fail *bool) replica {
	rp := l.replica()
	keys := rp.sec.SubjectKeys()
	rp.srv.cfg.SubjectKeys = keys
	rp.srv.cfg.AuditUnsealer = &audit.Sealer{Keys: sealKeysOf{keys}}
	rp.srv.cfg.RecordingStore = recording.NewPGStore(l.pool)
	rp.srv.cfg.Store = failBlankStore{PG: store.NewPG(l.pool), fail: fail}
	rp.srv.router = rp.srv.routes()
	return rp
}

func (l *maskLab) personRun(owner, task string) types.AgentRun {
	l.t.Helper()
	now := time.Now().UTC()
	id := uuid.New()
	run, err := store.NewPG(l.pool).CreateRun(l.t.Context(), types.AgentRun{
		ID: id, CreatedAt: now, UpdatedAt: now, CreatedBy: owner, Agent: "claude-code", Task: task,
		ConfinementClass: types.CC2, State: types.RunStopped, SPIFFEID: "spiffe://wardyn.test/agent-run/" + id.String(), RunnerTarget: "docker",
	})
	if err != nil {
		l.t.Fatalf("create run: %v", err)
	}
	return run
}

func (l *maskLab) taskOf(id uuid.UUID) string {
	l.t.Helper()
	var task string
	if err := l.pool.QueryRow(l.t.Context(), `SELECT task FROM agent_runs WHERE id=$1`, id).Scan(&task); err != nil {
		l.t.Fatal(err)
	}
	return task
}

// auditReason is every approval.decide reason a reader sees through rp, by actor.
func (l *maskLab) auditReason(rp replica, action string) map[string]string {
	l.t.Helper()
	w := do(l.t, rp.srv, http.MethodGet, "/api/v1/audit?action="+action, adminToken, "")
	if w.Code != http.StatusOK {
		l.t.Fatalf("audit read = %d %s", w.Code, w.Body)
	}
	var evs []types.AuditEvent
	if err := json.Unmarshal(w.Body.Bytes(), &evs); err != nil {
		l.t.Fatal(err)
	}
	out := map[string]string{}
	for _, ev := range evs {
		var d map[string]any
		_ = json.Unmarshal(ev.Data, &d)
		out[ev.Actor] = fmt.Sprint(d["reason"])
	}
	return out
}

func TestPG_PersonErasure_ReachesEveryCopyOnEveryReplica(t *testing.T) {
	l := newMaskLab(t)
	fail := false
	a, b := erasureReplica(l, &fail), erasureReplica(l, &fail)
	ctx := t.Context()
	pg := store.NewPG(l.pool)
	const alice, bob = "alice-sub", "bob-sub"

	aliceRuns := []types.AgentRun{l.personRun(alice, "alice secret plan one"), l.personRun(alice, "alice secret plan two")}
	bobRun := l.personRun(bob, "bob plan")
	for _, r := range append(aliceRuns, bobRun) {
		if err := pg.SaveFinalRunOutput(ctx, store.RunOutput{RunID: r.ID, Output: []byte("output of " + r.CreatedBy), Source: "stdout"}); err != nil {
			t.Fatal(err)
		}
		if err := a.srv.cfg.RecordingStore.SaveCast(ctx, r.ID.String(), strings.NewReader("cast of "+r.CreatedBy)); err != nil {
			t.Fatal(err)
		}
		if err := a.srv.cfg.RecordingStore.SaveCastNamed(ctx, r.ID.String(), "sess", strings.NewReader("session of "+r.CreatedBy)); err != nil {
			t.Fatal(err)
		}
	}
	for _, r := range aliceRuns {
		a.dispatch(t, r, "alice-secret-value-"+r.ID.String())
		if !b.srv.cfg.MaskManifests.Covered(ctx, r.ID) {
			t.Fatalf("a second replica does not admit alice's dispatched run %s before the erasure", r.ID)
		}
	}
	if err := a.sec.For(alice).Put(ctx, "anthropic-api-key", []byte("alice-credential-value-0000")); err != nil {
		t.Fatal(err)
	}
	if err := a.sec.For(bob).Put(ctx, "anthropic-api-key", []byte("bob-credential-value-00000")); err != nil {
		t.Fatal(err)
	}

	// Sealed audit fields for both, written through the same sealer the chain uses.
	sealer := &audit.Sealer{Keys: sealKeysOf{a.sec.SubjectKeys()}}
	// Alice's directory knows an email for her; a row written under that email before
	// the directory learned it is under the email's own key, which her erasure must reach.
	const aliceEmail = "alice@corp.example"
	if _, err := pg.UpsertLoginIdentity(ctx, store.LoginIdentity{Principal: alice, Issuer: "https://dex.example", Email: aliceEmail}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	for who, reason := range map[string]string{alice: "alice typed this", bob: "bob typed this", aliceEmail: "alice typed this by email"} {
		data, _ := json.Marshal(map[string]any{"approval_id": "a-" + who, "decision": "APPROVED", "reason": reason})
		ev, pending, err := sealer.Seal(ctx, types.AuditEvent{ID: uuid.New(), Time: time.Now().UTC(), ActorType: types.ActorHuman,
			Actor: who, Action: "approval.decide", Target: "a-" + who, Outcome: "success", Data: data})
		if err != nil || pending {
			t.Fatalf("seal: %v (pending %v)", err, pending)
		}
		if strings.Contains(string(ev.Data), reason) {
			t.Fatalf("the sealed row still carries %q", reason)
		}
		if err := store.InsertAuditEvent(ctx, l.pool, &ev); err != nil {
			t.Fatal(err)
		}
	}
	// Replica B reads them now, so its key cache is warm when the erasure lands on A.
	if got := l.auditReason(b, "approval.decide"); got[alice] != "alice typed this" || got[bob] != "bob typed this" || got[aliceEmail] != "alice typed this by email" {
		t.Fatalf("a warm replica reads %v, want every reason opened", got)
	}
	for name, rp := range map[string]replica{"A": a, "B": b} {
		if code, _, _ := l.readOutput(rp, aliceRuns[0].ID); code != http.StatusOK {
			t.Fatalf("replica %s: alice's output before the erasure = %d, want 200", name, code)
		}
	}

	sec := ssoSession(t, "sec-sub", "sec@corp.example", oidc.RoleSecurityAdmin)
	erase := func(rp replica, who string, scopes ...string) *httptest.ResponseRecorder {
		return doSSO(t, rp.srv, http.MethodPost, "/api/v1/people/"+who+"/erasure", sec, erasureBody(scopes...))
	}

	// A failure in run_tasks stops the run, after the scopes that came before it.
	fail = true
	w := erase(a, alice, "mask_copies", "run_outputs", "run_tasks", "audit_personal_fields", "credentials")
	var inc erasureIncompleteBody
	_ = json.Unmarshal(w.Body.Bytes(), &inc)
	if w.Code != http.StatusInternalServerError || inc.Reason != reasonErasureIncomplete ||
		strings.Join(inc.Done, ",") != "mask_copies,run_outputs" || strings.Join(inc.Remaining, ",") != "run_tasks,audit_personal_fields,credentials" {
		t.Fatalf("partial erasure = %d %s, want 500 with done mask_copies,run_outputs and the rest remaining", w.Code, w.Body)
	}
	if l.taskOf(aliceRuns[0].ID) == "" {
		t.Fatal("run_tasks ran although it was the scope that failed")
	}
	// Retry: every scope finishes, the finished ones run again harmlessly.
	fail = false
	if w := erase(a, alice, "mask_copies", "run_outputs", "run_tasks", "audit_personal_fields", "credentials"); w.Code != http.StatusOK {
		t.Fatalf("retry = %d %s, want 200", w.Code, w.Body)
	}

	for _, r := range aliceRuns {
		if got := l.taskOf(r.ID); got != "" {
			t.Errorf("alice's run task = %q after run_tasks, want empty", got)
		}
		for name, rp := range map[string]replica{"A": a, "B": b} {
			if code, _, body := l.readOutput(rp, r.ID); code != http.StatusNotFound || !strings.Contains(body, reasonRunOutputErased) {
				t.Errorf("replica %s: alice's output after the erasure = %d %s, want 404 %s", name, code, body, reasonRunOutputErased)
			}
		}
	}
	if got := l.taskOf(bobRun.ID); got != "bob plan" {
		t.Errorf("bob's task = %q, want it untouched", got)
	}
	if code, got, _ := l.readOutput(b, bobRun.ID); code != http.StatusOK || got.Output != "output of "+bob {
		t.Errorf("bob's output = %d %q, want it untouched", code, got.Output)
	}
	for _, r := range aliceRuns {
		for name, rp := range map[string]replica{"A": a, "B": b} {
			if rp.srv.cfg.MaskManifests.Covered(ctx, r.ID) {
				t.Errorf("replica %s still admits alice's run %s: its manifest is covered after the fence", name, r.ID)
			}
		}
	}
	if n := l.count(`SELECT count(*) FROM run_mask_values WHERE owner=$1`, alice); n != 0 {
		t.Errorf("%d masking values left for alice, want 0", n)
	}
	if n := l.count(`SELECT count(*) FROM run_mask_manifest WHERE owner=$1 AND fenced_at IS NULL`, alice); n != 0 {
		t.Errorf("%d of alice's manifests are not fenced", n)
	}
	if got := namesOf(t, a.sec, alice); len(got) != 0 {
		t.Errorf("alice still holds credentials %v", got)
	}
	if got := namesOf(t, a.sec, bob); len(got) != 1 {
		t.Errorf("bob holds %v, want his credential untouched", got)
	}
	// Recordings were not asked for: still there.
	for _, r := range aliceRuns {
		for _, key := range []string{r.ID.String(), r.ID.String() + "~sess"} {
			rc, err := a.srv.cfg.RecordingStore.OpenCast(ctx, key)
			if err != nil {
				t.Fatalf("recording %s after an erasure that did not name recordings: %v", key, err)
			}
			_ = rc.Close()
		}
	}

	// Sealed fields: erased on A and on B, whose cache was warm; bob's unchanged; chain intact.
	for name, rp := range map[string]replica{"A": a, "B": b} {
		got := l.auditReason(rp, "approval.decide")
		if got[alice] != audit.ErasedValue || got[aliceEmail] != audit.ErasedValue || got[bob] != "bob typed this" {
			t.Errorf("replica %s reads %v, want alice's (under both names) erased and bob's open", name, got)
		}
	}
	if st, err := pg.VerifyAuditChain(ctx); err != nil || !st.OK {
		t.Fatalf("chain after the erasure = %+v, %v, want ok", st, err)
	}
	row := lastAuditEvent(t, l.rec.snapshot(), "person.erasure")
	if row.Target != alice || row.Outcome != "success" {
		t.Errorf("last person.erasure row = %s %s, want success for %s", row.Outcome, row.Target, alice)
	}

	// Recordings only when asked, and only that person's.
	w = erase(b, alice, "recordings")
	if w.Code != http.StatusOK {
		t.Fatalf("recordings erasure = %d %s", w.Code, w.Body)
	}
	var erased struct {
		Detail map[string]struct {
			RunsFenced int `json:"runs_fenced"`
		} `json:"detail"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &erased); err != nil || erased.Detail["recordings"].RunsFenced != len(aliceRuns) {
		t.Fatalf("recording fence count = %s, %v", w.Body, err)
	}
	newRun := l.personRun(alice, "after the recordings erase")
	if err := b.srv.cfg.RecordingStore.SaveCast(ctx, newRun.ID.String(), strings.NewReader("new run")); err != nil {
		t.Fatalf("new run of erased person could not record: %v", err)
	}
	for _, r := range aliceRuns {
		for _, key := range []string{r.ID.String(), r.ID.String() + "~sess"} {
			if _, err := a.srv.cfg.RecordingStore.OpenCast(ctx, key); !errors.Is(err, recording.ErrErased) {
				t.Errorf("recording %s after the recordings scope: %v, want erased", key, err)
			}
		}
	}
	if rc, err := a.srv.cfg.RecordingStore.OpenCast(ctx, bobRun.ID.String()); err != nil {
		t.Errorf("bob's recording was deleted: %v", err)
	} else {
		_ = rc.Close()
	}
}
