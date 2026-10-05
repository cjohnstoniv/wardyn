// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// Governance four-eyes (WARDYN_GOVERNANCE_SECOND_HUMAN) against a real Postgres: the proposal, the
// decision transaction and what it must never do. A mocked store can pass while the real transaction
// races, so every test here that is about atomicity, staleness or concurrency runs the real one.
//
// Guarded by WARDYN_TEST_PG (throwawayPGPool): skipped cleanly when unset, must PASS when set.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const (
	govSubAlice = "sub-alice"
	govSubBob   = "sub-bob"
	govSubCarol = "sub-carol"
)

// govEnv is one deployment with the switch on: a Postgres, a server over it with SSO configured, and
// the humans the tests are about. alice and bob and carol are security admins with their own principal
// and email; twin is a different principal with alice's mailbox in another case; member is no admin.
type govEnv struct {
	t     *testing.T
	pool  *pgxpool.Pool
	pg    store.PG
	h     *harness
	srv   *Server
	cfg   Config
	alice *http.Cookie
	twin  *http.Cookie
	bob   *http.Cookie
	carol *http.Cookie
	super *http.Cookie
	user  *http.Cookie
}

var govTestPolicy = types.RunPolicySpec{
	AllowedDomains:      []string{"api.anthropic.com", "pypi.org", "github.com", "npmjs.org"},
	MinConfinementClass: types.CC2,
}

func newGovEnv(t *testing.T, mutate ...func(*Config)) *govEnv {
	t.Helper()
	t.Setenv(envGovernanceSecondHuman, "true")
	return newGovEnvOn(t, throwawayPGPool(t), mutate...)
}

func newGovEnvOn(t *testing.T, pool *pgxpool.Pool, mutate ...func(*Config)) *govEnv {
	t.Helper()
	h := newHarness(t)
	pg := store.NewPG(pool)
	cfg := baseTestConfig(h, pg)
	cfg.OIDC = &oidc.Authenticator{}
	cfg.DefaultPolicy = govTestPolicy
	for _, m := range mutate {
		m(&cfg)
	}
	e := &govEnv{t: t, pool: pool, pg: pg, h: h, cfg: cfg, srv: New(cfg)}
	e.alice = ssoSession(t, govSubAlice, "alice@corp.example", oidc.RoleSecurityAdmin)
	e.twin = ssoSession(t, "sub-alice-twin", "Alice@Corp.Example", oidc.RoleSecurityAdmin)
	e.bob = ssoSession(t, govSubBob, "bob@corp.example", oidc.RoleSecurityAdmin)
	e.carol = ssoSession(t, govSubCarol, "carol@corp.example", oidc.RoleSecurityAdmin)
	e.super = ssoSession(t, "sub-super", "super@corp.example", oidc.RoleAdmin)
	e.user = ssoSession(t, "sub-user", "user@corp.example", oidc.RoleUser)
	return e
}

// otherServer is a second wardynd over the same database with its configuration changed.
func (e *govEnv) otherServer(mutate func(*Config)) *Server {
	cfg := e.cfg
	mutate(&cfg)
	return New(cfg)
}

func (e *govEnv) call(c *http.Cookie, method, path, body string) *httptest.ResponseRecorder {
	e.t.Helper()
	return doSSO(e.t, e.srv, method, path, c, body)
}

func (e *govEnv) admin(method, path, body string) *httptest.ResponseRecorder {
	e.t.Helper()
	return do(e.t, e.srv, method, path, adminToken, body)
}

// profileBody is a standalone profile write body.
func profileBody(name string, domains ...string) string {
	b, _ := json.Marshal(map[string]any{
		"name": name, "ceiling": map[string]any{"allowed_domains": domains, "min_confinement_class": "CC2"},
	})
	return string(b)
}

func (e *govEnv) seedProfile(name string, domains ...string) types.GovernanceProfile {
	e.t.Helper()
	p, err := e.pg.UpsertGovernanceProfile(context.Background(), types.GovernanceProfile{
		Name: name, CreatedBy: "seed",
		Ceiling: types.RunPolicySpec{AllowedDomains: domains, MinConfinementClass: types.CC2},
	})
	if err != nil {
		e.t.Fatalf("seed profile %q: %v", name, err)
	}
	return p
}

func (e *govEnv) seedAssignment(profile types.GovernanceProfile, subject string) types.GovernanceAssignment {
	e.t.Helper()
	a, err := e.pg.UpsertGovernanceAssignment(context.Background(), types.GovernanceAssignment{
		SubjectType: types.CapabilitySubjectUser, Subject: subject, ProfileID: profile.ID, Priority: 1, CreatedBy: "seed",
	})
	if err != nil {
		e.t.Fatalf("seed assignment: %v", err)
	}
	return a
}

func (e *govEnv) profile(id uuid.UUID) types.GovernanceProfile {
	e.t.Helper()
	p, err := e.pg.GetGovernanceProfile(context.Background(), id)
	if err != nil {
		e.t.Fatalf("read profile %s: %v", id, err)
	}
	return p
}

// pending decodes a 202's pending_change, failing the test on any other status.
func (e *govEnv) pending(w *httptest.ResponseRecorder) types.GovernanceChange {
	e.t.Helper()
	if w.Code != http.StatusAccepted {
		e.t.Fatalf("status = %d %s, want 202", w.Code, w.Body)
	}
	var body struct {
		Pending types.GovernanceChange `json:"pending_change"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body.Pending.ID == uuid.Nil {
		e.t.Fatalf("202 body %s does not carry a pending_change: %v", w.Body, err)
	}
	if loc := w.Header().Get("Location"); loc != "/api/v1/governance/changes/"+body.Pending.ID.String() {
		e.t.Fatalf("Location = %q, want the change's own URL", loc)
	}
	if body.Pending.State != types.GovernanceChangePending {
		e.t.Fatalf("pending_change.state = %q", body.Pending.State)
	}
	return body.Pending
}

func (e *govEnv) changeState(id uuid.UUID) string {
	e.t.Helper()
	var s string
	if err := e.pool.QueryRow(context.Background(), `SELECT state FROM governance_changes WHERE id = $1`, id).Scan(&s); err != nil {
		e.t.Fatalf("read change %s: %v", id, err)
	}
	return s
}

func (e *govEnv) pendingCount() int {
	e.t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM governance_changes WHERE state = 'pending'`).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func (e *govEnv) audits(action string) []types.AuditEvent {
	var out []types.AuditEvent
	for _, ev := range e.h.audit.snapshot() {
		if ev.Action == action {
			out = append(out, ev)
		}
	}
	return out
}

func approvePath(id uuid.UUID) string {
	return "/api/v1/governance/changes/" + id.String() + "/approve"
}
func rejectPath(id uuid.UUID) string { return "/api/v1/governance/changes/" + id.String() + "/reject" }

func wireReason(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var b errorBody
	_ = json.Unmarshal(w.Body.Bytes(), &b)
	return b.Reason
}

// TestPG_GovernanceChanges_SwitchOffIsUnchanged: with the switch unset, every covered route answers
// exactly as 0.8.5 did (201, 200, 204; no 202, no Location, no pending row, no governance.change audit
// row), including for a human who would otherwise have been held.
func TestPG_GovernanceChanges_SwitchOffIsUnchanged(t *testing.T) {
	pool := throwawayPGPool(t) // the switch is not set
	e := newGovEnvOn(t, pool)
	p := e.seedProfile("existing", "pypi.org")
	doomed := e.seedProfile("doomed", "pypi.org")
	subj := e.seedProfile("for-subject", "pypi.org")
	a := e.seedAssignment(subj, "carol@corp.example")

	type step struct {
		name, method, path, body string
		want                     int
	}
	steps := []step{
		{"create profile", http.MethodPost, "/api/v1/governance/profiles", profileBody("fresh", "pypi.org"), http.StatusCreated},
		{"widen profile", http.MethodPut, "/api/v1/governance/profiles/" + p.ID.String(), profileBody("existing", "pypi.org", "github.com"), http.StatusOK},
		{"delete profile", http.MethodDelete, "/api/v1/governance/profiles/" + doomed.ID.String(), "", http.StatusNoContent},
		{"upsert assignment", http.MethodPost, "/api/v1/governance/assignments",
			fmt.Sprintf(`{"subject_type":"user","subject":"dave@corp.example","profile_id":%q,"priority":2}`, subj.ID), http.StatusCreated},
		{"repoint assignment", http.MethodPost, "/api/v1/governance/assignments",
			fmt.Sprintf(`{"subject_type":"user","subject":"dave@corp.example","profile_id":%q,"priority":3}`, p.ID), http.StatusOK},
		{"delete assignment", http.MethodDelete, "/api/v1/governance/assignments/" + a.ID.String(), "", http.StatusNoContent},
	}
	for _, s := range steps {
		w := e.call(e.alice, s.method, s.path, s.body)
		if w.Code != s.want || w.Header().Get("Location") != "" {
			t.Errorf("%s: %d %s (Location %q), want %d with no Location", s.name, w.Code, w.Body, w.Header().Get("Location"), s.want)
		}
		if strings.Contains(w.Body.String(), "pending_change") {
			t.Errorf("%s: a switch-off body mentions pending_change: %s", s.name, w.Body)
		}
	}
	if n := e.pendingCount(); n != 0 {
		t.Errorf("%d pending changes with the switch off", n)
	}
	var all int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM governance_changes`).Scan(&all); err != nil || all != 0 {
		t.Errorf("governance_changes holds %d rows (%v) with the switch off", all, err)
	}
	for _, ev := range e.h.audit.snapshot() {
		if strings.HasPrefix(ev.Action, "governance.change.") {
			t.Errorf("a switch-off write recorded %s", ev.Action)
		}
	}
}

// TestPG_GovernanceChanges_RepeatCreateNamesTheHeldOne: a create mints its id, so the same new profile
// applied again while its create waits must still meet that create: a 409 governance_change_pending
// carrying it, never a second held create of the same name. A lapsed create of the name does not hold it.
func TestPG_GovernanceChanges_RepeatCreateNamesTheHeldOne(t *testing.T) {
	e := newGovEnv(t)
	body := profileBody("repeat-new", "github.com")
	first := e.pending(e.call(e.alice, http.MethodPost, "/api/v1/governance/profiles", body))
	if first.Op != "create" {
		t.Fatalf("first create held as %q", first.Op)
	}
	var matches *bool
	heldBy := func(w *httptest.ResponseRecorder) uuid.UUID {
		t.Helper()
		if w.Code != http.StatusConflict || wireReason(t, w) != reasonGovernanceChangePending {
			t.Fatalf("a repeat create = %d %s, want 409 %s", w.Code, w.Body, reasonGovernanceChangePending)
		}
		var held struct {
			Pending types.GovernanceChange `json:"pending_change"`
			Matches *bool                  `json:"pending_change_matches"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &held); err != nil || held.Pending.State != types.GovernanceChangePending {
			t.Fatalf("the 409 carries no held change: %s (%v)", w.Body, err)
		}
		matches = held.Matches
		return held.Pending.ID
	}
	if id := heldBy(e.call(e.alice, http.MethodPost, "/api/v1/governance/profiles", body)); id != first.ID {
		t.Errorf("a repeat POST names %s, want the held create %s", id, first.ID)
	}
	if matches == nil || !*matches {
		t.Errorf("a repeat of the same create: pending_change_matches = %v, want true", matches)
	}
	// A create at a caller-chosen id is the same name, so the same held create, with other content.
	if id := heldBy(e.call(e.carol, http.MethodPut, "/api/v1/governance/profiles/"+uuid.NewString(), profileBody("repeat-new", "pypi.org"))); id != first.ID {
		t.Errorf("a create by PUT of the same name names %s, want %s", id, first.ID)
	}
	if matches == nil || *matches {
		t.Errorf("a create of the same name with other content: pending_change_matches = %v, want false", matches)
	}
	if n := e.pendingCount(); n != 1 {
		t.Fatalf("%d pending changes after repeat creates, want 1", n)
	}
	// Another name is another create.
	other := e.pending(e.call(e.alice, http.MethodPost, "/api/v1/governance/profiles", profileBody("repeat-other", "github.com")))

	// A lapsed create of the name holds nothing: the next create expires it and is held itself.
	if _, err := e.pool.Exec(context.Background(), `UPDATE governance_changes SET expires_at = now() - interval '1 second' WHERE id = $1`, first.ID); err != nil {
		t.Fatal(err)
	}
	again := e.pending(e.call(e.alice, http.MethodPost, "/api/v1/governance/profiles", body))
	if again.ID == first.ID || e.changeState(first.ID) != types.GovernanceChangeExpired {
		t.Fatalf("after the first lapsed: new change %s, first %s; want a new change and the first expired", again.ID, e.changeState(first.ID))
	}
	rows := e.audits("governance.change.expire")
	if len(rows) != 1 || rows[0].Target != first.ID.String() {
		t.Fatalf("governance.change.expire rows = %+v, want one naming %s", rows, first.ID)
	}
	var data map[string]any
	if err := json.Unmarshal(rows[0].Data, &data); err != nil || data["target_key"] != first.TargetKey || data["target_key"] == again.TargetKey {
		t.Errorf("expire row data = %s, want target_key %s (the lapsed create's own, not the new proposal's %s)", rows[0].Data, first.TargetKey, again.TargetKey)
	}
	e.approve(again.ID)
	e.approve(other.ID)
	if n := e.pendingCount(); n != 0 {
		t.Errorf("%d pending changes after both creates applied, want 0", n)
	}
}

// TestPG_GovernanceChanges_RefusalSaysWhetherTheHeldChangeIsTheProposal: a write at a held target is a
// 409 carrying the held change and whether it is this proposal. The same update again matches; one with
// other content, or a delete, does not, so the caller never mistakes the held change for its own write.
func TestPG_GovernanceChanges_RefusalSaysWhetherTheHeldChangeIsTheProposal(t *testing.T) {
	e := newGovEnv(t)
	prof := e.seedProfile("held-target", "pypi.org")
	path := "/api/v1/governance/profiles/" + prof.ID.String()
	widened := func(extra string) string { return profileBody("held-target", "pypi.org", extra) }
	held := e.pending(e.call(e.alice, http.MethodPut, path, widened("github.com")))
	for _, c := range []struct {
		name, method, body string
		want               bool
	}{
		{"the same update", http.MethodPut, widened("github.com"), true},
		{"an update with other content", http.MethodPut, widened("npmjs.org"), false},
		{"a delete", http.MethodDelete, "", false},
	} {
		w := e.call(e.carol, c.method, path, c.body)
		var got struct {
			Pending types.GovernanceChange `json:"pending_change"`
			Matches *bool                  `json:"pending_change_matches"`
		}
		if w.Code != http.StatusConflict || json.Unmarshal(w.Body.Bytes(), &got) != nil || got.Pending.ID != held.ID {
			t.Fatalf("%s = %d %s, want 409 naming %s", c.name, w.Code, w.Body, held.ID)
		}
		if got.Matches == nil || *got.Matches != c.want {
			t.Errorf("%s: pending_change_matches = %v, want %v", c.name, got.Matches, c.want)
		}
	}
	if n := e.pendingCount(); n != 1 {
		t.Errorf("%d pending changes, want only the first", n)
	}
}

// TestPG_GovernanceChanges_QueuedWritesAnswer202: with the switch on, each profile and assignment
// operation by a human answers 202 with the pending change, the target is unchanged on a direct store
// read, GET /governance is unchanged, and one pending row exists per target.
func TestPG_GovernanceChanges_QueuedWritesAnswer202(t *testing.T) {
	e := newGovEnv(t)
	upd := e.seedProfile("to-update", "pypi.org")
	del := e.seedProfile("to-delete", "pypi.org")
	subj := e.seedProfile("for-subject", "pypi.org")
	repoint := e.seedProfile("repoint-to", "pypi.org", "github.com")
	a := e.seedAssignment(subj, "carol@corp.example")
	a2 := e.seedAssignment(subj, "erin@corp.example")
	before := e.admin(http.MethodGet, "/api/v1/governance", "").Body.String()

	cases := []struct {
		name, method, path, body string
	}{
		{"create", http.MethodPost, "/api/v1/governance/profiles", profileBody("fresh", "pypi.org")},
		{"update", http.MethodPut, "/api/v1/governance/profiles/" + upd.ID.String(), profileBody("to-update", "pypi.org", "github.com")},
		{"delete", http.MethodDelete, "/api/v1/governance/profiles/" + del.ID.String(), ""},
		{"assignment upsert", http.MethodPost, "/api/v1/governance/assignments",
			fmt.Sprintf(`{"subject_type":"user","subject":"dave@corp.example","profile_id":%q,"priority":2}`, subj.ID)},
		{"assignment repoint", http.MethodPost, "/api/v1/governance/assignments",
			fmt.Sprintf(`{"subject_type":"user","subject":"carol@corp.example","profile_id":%q,"priority":2}`, repoint.ID)},
		{"assignment delete", http.MethodDelete, "/api/v1/governance/assignments/" + a2.ID.String(), ""},
	}
	for _, c := range cases {
		ch := e.pending(e.call(e.alice, c.method, c.path, c.body))
		if ch.ProposedBy != govSubAlice || ch.TargetKind == "" || ch.Op == "" || ch.TargetKey == "" || !ch.ExpiresAt.After(ch.ProposedAt) {
			t.Errorf("%s: pending change %+v is incomplete", c.name, ch)
		}
		var diff struct {
			Changed []string `json:"changed"`
		}
		if err := json.Unmarshal(ch.Diff, &diff); err != nil || len(diff.Changed) == 0 {
			t.Errorf("%s: diff %s names no changed field (%v)", c.name, ch.Diff, err)
		}
	}
	if n := e.pendingCount(); n != len(cases) {
		t.Fatalf("%d pending changes, want %d", n, len(cases))
	}
	if after := e.admin(http.MethodGet, "/api/v1/governance", "").Body.String(); after != before {
		t.Fatalf("a held write changed GET /governance:\nbefore %s\nafter  %s", before, after)
	}
	if got := e.profile(upd.ID); len(got.Ceiling.AllowedDomains) != 1 {
		t.Errorf("a held update changed the stored profile: %+v", got.Ceiling.AllowedDomains)
	}
	if _, err := e.pg.GetGovernanceProfile(context.Background(), del.ID); err != nil {
		t.Errorf("a held delete removed the profile: %v", err)
	}
	if got, _ := e.pg.ListGovernanceAssignments(context.Background()); len(got) != 2 {
		t.Errorf("%d assignments after held writes, want the 2 seeded", len(got))
	}
	_ = a

	// A second change to a target that already has a live one is a 409 naming it.
	w := e.call(e.alice, http.MethodPut, "/api/v1/governance/profiles/"+upd.ID.String(), profileBody("to-update", "pypi.org", "npmjs.org"))
	if w.Code != http.StatusConflict || wireReason(t, w) != reasonGovernanceChangePending {
		t.Errorf("a second change to a held target = %d %s, want 409 %s", w.Code, w.Body, reasonGovernanceChangePending)
	}
	var held struct {
		Pending types.GovernanceChange `json:"pending_change"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &held); err != nil || held.Pending.State != types.GovernanceChangePending || held.Pending.TargetKey != upd.ID.String() {
		t.Errorf("the 409 carries no held change for %s: %s (%v)", upd.ID, w.Body, err)
	}
	// A write the store would refuse is refused as it always was, not held.
	if w := e.call(e.alice, http.MethodPost, "/api/v1/governance/profiles", `{"name":""}`); w.Code != http.StatusBadRequest {
		t.Errorf("an invalid body = %d %s, want the direct 400", w.Code, w.Body)
	}
	if w := e.call(e.alice, http.MethodDelete, "/api/v1/governance/profiles/"+subj.ID.String(), ""); w.Code != http.StatusConflict ||
		wireReason(t, w) != reasonGovernanceProfileInUse {
		t.Errorf("deleting an assigned profile = %d %s, want the direct 409 %s", w.Code, w.Body, reasonGovernanceProfileInUse)
	}
	if w := e.call(e.alice, http.MethodDelete, "/api/v1/governance/assignments/"+uuid.NewString(), ""); w.Code != http.StatusNotFound {
		t.Errorf("deleting an unknown assignment = %d, want the direct 404", w.Code)
	}
	if w := e.call(e.alice, http.MethodPost, "/api/v1/governance/assignments",
		fmt.Sprintf(`{"subject_type":"user","subject":"x@corp.example","profile_id":%q}`, uuid.New())); w.Code != http.StatusNotFound {
		t.Errorf("an assignment to an unknown profile = %d %s, want the direct 404", w.Code, w.Body)
	}
	if n := e.pendingCount(); n != len(cases) {
		t.Errorf("refused writes left %d pending changes, want %d", n, len(cases))
	}
}

// TestPG_GovernanceChanges_SelfApprovalRefused: the proposer is not a second human, whether they come
// back as the same principal or as another principal with the same mailbox (an SSO session and a token,
// or two identity-provider subjects). Either way the change stays pending, and the proposer may reject
// their own change.
func TestPG_GovernanceChanges_SelfApprovalRefused(t *testing.T) {
	e := newGovEnv(t)
	p := e.seedProfile("p", "pypi.org")
	ch := e.pending(e.call(e.alice, http.MethodPut, "/api/v1/governance/profiles/"+p.ID.String(), profileBody("p", "pypi.org", "github.com")))

	for name, who := range map[string]*http.Cookie{"same principal": e.alice, "same mailbox, other principal": e.twin} {
		w := e.call(who, http.MethodPost, approvePath(ch.ID), "")
		if w.Code != http.StatusForbidden || wireReason(t, w) != "second_human_required" {
			t.Errorf("%s: %d %s, want 403 second_human_required", name, w.Code, w.Body)
		}
		if e.changeState(ch.ID) != types.GovernanceChangePending {
			t.Errorf("%s: the change left pending", name)
		}
	}
	if got := e.profile(p.ID); len(got.Ceiling.AllowedDomains) != 1 {
		t.Errorf("a refused self-approval applied the change: %v", got.Ceiling.AllowedDomains)
	}
	denied := 0
	for _, ev := range e.audits("authz.denied") {
		if d := auditData(t, ev); d["reason"] == "second_human_required" && ev.Target == "governance.change" {
			denied++
		}
	}
	if denied != 2 {
		t.Errorf("%d authz.denied rows for second_human_required at governance.change, want 2", denied)
	}

	// The proposer may withdraw it.
	if w := e.call(e.alice, http.MethodPost, rejectPath(ch.ID), ""); w.Code != http.StatusOK {
		t.Fatalf("the proposer's own reject = %d %s, want 200", w.Code, w.Body)
	}
	if st := e.changeState(ch.ID); st != types.GovernanceChangeRejected {
		t.Errorf("state after the proposer's reject = %q", st)
	}
	if len(e.audits("governance.change.reject")) != 1 {
		t.Errorf("the proposer's rejection left %d reject rows, want 1", len(e.audits("governance.change.reject")))
	}
}

// TestPG_GovernanceChanges_DistinctAdminApplies: a distinct security admin's approval applies the
// target, and the target's own audit row names the approver, the change and the proposer. A member
// and an admin of the wrong tier cannot approve.
func TestPG_GovernanceChanges_DistinctAdminApplies(t *testing.T) {
	e := newGovEnv(t)
	p := e.seedProfile("p", "pypi.org")
	subj := e.seedProfile("for-subject", "pypi.org")
	ch := e.pending(e.call(e.alice, http.MethodPut, "/api/v1/governance/profiles/"+p.ID.String(), profileBody("p", "pypi.org", "github.com")))
	asg := e.pending(e.call(e.alice, http.MethodPost, "/api/v1/governance/assignments",
		fmt.Sprintf(`{"subject_type":"user","subject":"dave@corp.example","profile_id":%q,"priority":4}`, subj.ID)))

	if w := e.call(e.user, http.MethodPost, approvePath(ch.ID), ""); w.Code != http.StatusForbidden {
		t.Errorf("a member approving = %d, want 403", w.Code)
	}
	list := e.call(e.bob, http.MethodGet, "/api/v1/governance/changes", "")
	var pendingList []types.GovernanceChange
	if err := json.Unmarshal(list.Body.Bytes(), &pendingList); err != nil || len(pendingList) != 2 {
		t.Fatalf("the pending queue = %d %s (%v), want 2 changes", list.Code, list.Body, err)
	}
	if got := e.call(e.bob, http.MethodGet, "/api/v1/governance/changes/"+ch.ID.String(), ""); got.Code != http.StatusOK ||
		strings.Contains(got.Body.String(), "bob@corp") || strings.Contains(got.Body.String(), "alice@corp") {
		t.Errorf("GET one change = %d %s, want 200 with no email in it", got.Code, got.Body)
	}

	for _, id := range []uuid.UUID{ch.ID, asg.ID} {
		w := e.call(e.bob, http.MethodPost, approvePath(id), "")
		if w.Code != http.StatusOK {
			t.Fatalf("a distinct admin approving %s = %d %s, want 200", id, w.Code, w.Body)
		}
		var got types.GovernanceChange
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || got.State != types.GovernanceChangeApplied || got.DecidedBy != govSubBob {
			t.Errorf("approved change = %+v (%v), want applied, decided by %s", got, err, govSubBob)
		}
	}
	if got := e.profile(p.ID); len(got.Ceiling.AllowedDomains) != 2 {
		t.Errorf("the approved update did not apply: %v", got.Ceiling.AllowedDomains)
	}
	as, _ := e.pg.ListGovernanceAssignments(context.Background())
	if len(as) != 1 || as[0].Subject != "dave@corp.example" || as[0].Priority != 4 || as[0].CreatedBy != govSubAlice {
		t.Errorf("the approved assignment = %+v, want dave's, created_by the proposer", as)
	}

	for action, target := range map[string]string{"governance.profile.write": p.ID.String(), "governance.assignment.write": as[0].ID.String()} {
		rows := e.audits(action)
		if len(rows) != 1 || rows[0].Actor != govSubBob || rows[0].Target != target {
			t.Fatalf("%s rows = %+v, want one by %s naming %s", action, rows, govSubBob, target)
		}
		d := auditData(t, rows[0])
		if d["proposed_by"] != govSubAlice || d["change_id"] == nil || d["change_id"] == "" {
			t.Errorf("%s data = %v, want change_id and proposed_by %s", action, d, govSubAlice)
		}
	}
	for _, action := range []string{"governance.change.propose", "governance.change.approve"} {
		if n := len(e.audits(action)); n != 2 {
			t.Errorf("%d %s rows, want 2", n, action)
		}
	}
	if w := e.call(e.carol, http.MethodPost, approvePath(ch.ID), ""); w.Code != http.StatusConflict || wireReason(t, w) != reasonGovernanceChangeNotPending {
		t.Errorf("approving an applied change = %d %s, want 409 %s", w.Code, w.Body, reasonGovernanceChangeNotPending)
	}
}

// TestPG_GovernanceChanges_BypassBetweenProposalAndApprovalIsStale: a write to the target after the
// proposal changes what the reviewer saw, so the approval is refused as stale and never overwrites.
func TestPG_GovernanceChanges_BypassBetweenProposalAndApprovalIsStale(t *testing.T) {
	e := newGovEnv(t)
	p := e.seedProfile("p", "pypi.org")
	ch := e.pending(e.call(e.alice, http.MethodPut, "/api/v1/governance/profiles/"+p.ID.String(), profileBody("p", "pypi.org", "github.com", "npmjs.org")))

	// The admin token writes the same target directly; it is the break-glass and leaves its own row.
	if w := e.admin(http.MethodPut, "/api/v1/governance/profiles/"+p.ID.String(), profileBody("p", "github.com")); w.Code != http.StatusOK {
		t.Fatalf("the admin-token write = %d %s, want 200 (applied directly)", w.Code, w.Body)
	}
	bypass := e.audits("governance.change.bypass")
	if len(bypass) != 1 || bypass[0].Actor != adminTokenPrincipal || bypass[0].Outcome != "success" || bypass[0].Target != p.ID.String() {
		t.Fatalf("bypass rows = %+v, want one success row by %s on the profile", bypass, adminTokenPrincipal)
	}

	w := e.call(e.bob, http.MethodPost, approvePath(ch.ID), "")
	if w.Code != http.StatusConflict || wireReason(t, w) != reasonGovernanceChangeStale {
		t.Fatalf("approving after a bypass write = %d %s, want 409 %s", w.Code, w.Body, reasonGovernanceChangeStale)
	}
	if got := e.profile(p.ID); len(got.Ceiling.AllowedDomains) != 1 || got.Ceiling.AllowedDomains[0] != "github.com" {
		t.Errorf("the stale approval overwrote the bypass write: %v", got.Ceiling.AllowedDomains)
	}
	if st := e.changeState(ch.ID); st != types.GovernanceChangeStale {
		t.Errorf("state = %q, want stale", st)
	}
	var failed bool
	for _, ev := range e.audits("governance.change.approve") {
		if d := auditData(t, ev); ev.Outcome == "failure" && d["error"] == "stale" {
			failed = true
		}
	}
	if !failed {
		t.Error("no governance.change.approve failure row with error class stale")
	}
	// A stale change cannot be approved again, and the target can be proposed afresh.
	if w := e.call(e.carol, http.MethodPost, approvePath(ch.ID), ""); w.Code != http.StatusConflict || wireReason(t, w) != reasonGovernanceChangeNotPending {
		t.Errorf("approving a stale change = %d %s, want 409 %s", w.Code, w.Body, reasonGovernanceChangeNotPending)
	}
	e.pending(e.call(e.alice, http.MethodPut, "/api/v1/governance/profiles/"+p.ID.String(), profileBody("p", "github.com", "pypi.org")))
}

// TestPG_GovernanceChanges_AdminTokenApprovalIsBreakGlass: the admin token approves a change and
// applies it, and the break-glass row names the change.
func TestPG_GovernanceChanges_AdminTokenApprovalIsBreakGlass(t *testing.T) {
	e := newGovEnv(t)
	p := e.seedProfile("p", "pypi.org")
	ch := e.pending(e.call(e.alice, http.MethodPut, "/api/v1/governance/profiles/"+p.ID.String(), profileBody("p", "pypi.org", "github.com")))
	if w := e.admin(http.MethodPost, approvePath(ch.ID), ""); w.Code != http.StatusOK {
		t.Fatalf("the admin token approving = %d %s, want 200", w.Code, w.Body)
	}
	if got := e.profile(p.ID); len(got.Ceiling.AllowedDomains) != 2 {
		t.Errorf("the admin-token approval did not apply: %v", got.Ceiling.AllowedDomains)
	}
	rows := e.audits("governance.change.bypass")
	if len(rows) != 1 || rows[0].Target != ch.ID.String() || rows[0].Outcome != "success" {
		t.Fatalf("bypass rows = %+v, want one success row on the change", rows)
	}
}

// TestPG_GovernanceChanges_DeploymentPolicyChangeIsStale: DefaultPolicy is env-borne and can change
// between proposal and approval, and what the reviewer approved is no longer what would be applied.
func TestPG_GovernanceChanges_DeploymentPolicyChangeIsStale(t *testing.T) {
	e := newGovEnv(t)
	p := e.seedProfile("p", "pypi.org")
	ch := e.pending(e.call(e.alice, http.MethodPut, "/api/v1/governance/profiles/"+p.ID.String(), profileBody("p", "pypi.org", "github.com")))

	restarted := e.otherServer(func(c *Config) {
		c.DefaultPolicy.AllowedDomains = append([]string{"extra.example.com"}, c.DefaultPolicy.AllowedDomains...)
	})
	w := doSSO(t, restarted, http.MethodPost, approvePath(ch.ID), e.bob, "")
	if w.Code != http.StatusConflict || wireReason(t, w) != reasonGovernanceChangeStale {
		t.Fatalf("approving under a changed DefaultPolicy = %d %s, want 409 %s", w.Code, w.Body, reasonGovernanceChangeStale)
	}
	if got := e.profile(p.ID); len(got.Ceiling.AllowedDomains) != 1 {
		t.Errorf("the stale approval applied the change: %v", got.Ceiling.AllowedDomains)
	}
}

// TestPG_GovernanceChanges_LapsedChangeAndReproposal: a change past expires_at cannot be approved (409,
// and it is moved to expired), and the target can then be proposed again.
func TestPG_GovernanceChanges_LapsedChangeAndReproposal(t *testing.T) {
	e := newGovEnv(t)
	p := e.seedProfile("p", "pypi.org")
	ch := e.pending(e.call(e.alice, http.MethodPut, "/api/v1/governance/profiles/"+p.ID.String(), profileBody("p", "pypi.org", "github.com")))
	if _, err := e.pool.Exec(context.Background(), `UPDATE governance_changes SET expires_at = now() - interval '1 minute' WHERE id = $1`, ch.ID); err != nil {
		t.Fatal(err)
	}

	// A read already says expired, before anything has swept it.
	got := e.call(e.bob, http.MethodGet, "/api/v1/governance/changes/"+ch.ID.String(), "")
	var read types.GovernanceChange
	if err := json.Unmarshal(got.Body.Bytes(), &read); err != nil || read.State != types.GovernanceChangeExpired {
		t.Fatalf("a lapsed change reads %q (%v), want expired", read.State, err)
	}
	if list := e.call(e.bob, http.MethodGet, "/api/v1/governance/changes", ""); strings.Contains(list.Body.String(), ch.ID.String()) {
		t.Errorf("a lapsed change is in the pending queue: %s", list.Body)
	}

	w := e.call(e.bob, http.MethodPost, approvePath(ch.ID), "")
	if w.Code != http.StatusConflict || wireReason(t, w) != reasonGovernanceChangeNotPending {
		t.Fatalf("approving a lapsed change = %d %s, want 409 %s", w.Code, w.Body, reasonGovernanceChangeNotPending)
	}
	if st := e.changeState(ch.ID); st != types.GovernanceChangeExpired {
		t.Errorf("state = %q, want expired", st)
	}
	if n := len(e.audits("governance.change.expire")); n != 1 {
		t.Errorf("%d governance.change.expire rows, want 1", n)
	}
	if got := e.profile(p.ID); len(got.Ceiling.AllowedDomains) != 1 {
		t.Errorf("a lapsed approval applied the change: %v", got.Ceiling.AllowedDomains)
	}
	e.pending(e.call(e.alice, http.MethodPut, "/api/v1/governance/profiles/"+p.ID.String(), profileBody("p", "pypi.org", "github.com")))

	// A lapsed row that nobody has touched is also expired by the next proposal at its target.
	q := e.seedProfile("q", "pypi.org")
	old := e.pending(e.call(e.alice, http.MethodPut, "/api/v1/governance/profiles/"+q.ID.String(), profileBody("q", "pypi.org", "github.com")))
	if _, err := e.pool.Exec(context.Background(), `UPDATE governance_changes SET expires_at = now() - interval '1 minute' WHERE id = $1`, old.ID); err != nil {
		t.Fatal(err)
	}
	e.pending(e.call(e.alice, http.MethodPut, "/api/v1/governance/profiles/"+q.ID.String(), profileBody("q", "pypi.org", "npmjs.org")))
	if st := e.changeState(old.ID); st != types.GovernanceChangeExpired {
		t.Errorf("the re-proposal left the lapsed change %q, want expired", st)
	}
}

// TestPG_GovernanceChanges_ConcurrentApprovalsApplyOnce: two distinct admins approving one change at
// once apply it exactly once; the other answers 409.
func TestPG_GovernanceChanges_ConcurrentApprovalsApplyOnce(t *testing.T) {
	e := newGovEnv(t)
	for round := 0; round < 5; round++ {
		p := e.seedProfile(fmt.Sprintf("p%d", round), "pypi.org")
		ch := e.pending(e.call(e.alice, http.MethodPut, "/api/v1/governance/profiles/"+p.ID.String(), profileBody(fmt.Sprintf("p%d", round), "pypi.org", "github.com")))
		codes := make([]int, 2)
		var wg sync.WaitGroup
		for i, who := range []*http.Cookie{e.bob, e.carol} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				codes[i] = e.call(who, http.MethodPost, approvePath(ch.ID), "").Code
			}()
		}
		wg.Wait()
		ok, conflict := 0, 0
		for _, c := range codes {
			switch c {
			case http.StatusOK:
				ok++
			case http.StatusConflict:
				conflict++
			}
		}
		if ok != 1 || conflict != 1 {
			t.Fatalf("round %d: concurrent approvals answered %v, want exactly one 200 and one 409", round, codes)
		}
		writes := 0
		for _, ev := range e.audits("governance.profile.write") {
			if ev.Target == p.ID.String() {
				writes++
			}
		}
		if writes != 1 {
			t.Fatalf("round %d: %d governance.profile.write rows for the profile, want 1", round, writes)
		}
	}
}

// failAfterApplyStore runs the real decision transaction but fails it after the target's apply has
// run inside it, the way a crash or a failed state move would.
type failAfterApplyStore struct {
	store.PG
	fail *bool
}

func (s failAfterApplyStore) DecideGovernanceChange(ctx context.Context, id uuid.UUID, d store.GovernanceDecision, fn store.GovernanceDecideFunc) (types.GovernanceChange, error) {
	return s.PG.DecideGovernanceChange(ctx, id, d, func(q store.Querier, ch types.GovernanceChange) error {
		if err := fn(q, ch); err != nil {
			return err
		}
		if *s.fail {
			return errors.New("injected: the transaction failed after apply")
		}
		return nil
	})
}

// TestPG_GovernanceChanges_RollbackAfterApplyLeavesTargetAndChangeUntouched: a failure after the
// target's apply, inside the decision transaction, unwrites the target and leaves the change pending
// and approvable.
func TestPG_GovernanceChanges_RollbackAfterApplyLeavesTargetAndChangeUntouched(t *testing.T) {
	fail := true
	e := newGovEnv(t, func(c *Config) {
		c.Store = failAfterApplyStore{PG: store.NewPG(c.Store.(store.PG).Pool), fail: &fail}
	})
	p := e.seedProfile("p", "pypi.org")
	subj := e.seedProfile("for-subject", "pypi.org")
	ch := e.pending(e.call(e.alice, http.MethodPut, "/api/v1/governance/profiles/"+p.ID.String(), profileBody("p", "pypi.org", "github.com")))
	asg := e.pending(e.call(e.alice, http.MethodPost, "/api/v1/governance/assignments",
		fmt.Sprintf(`{"subject_type":"user","subject":"dave@corp.example","profile_id":%q}`, subj.ID)))

	for _, id := range []uuid.UUID{ch.ID, asg.ID} {
		if w := e.call(e.bob, http.MethodPost, approvePath(id), ""); w.Code != http.StatusInternalServerError {
			t.Fatalf("a failed decision transaction = %d %s, want 500", w.Code, w.Body)
		}
		if st := e.changeState(id); st != types.GovernanceChangePending {
			t.Errorf("change %s = %q after a rolled-back approval, want pending", id, st)
		}
	}
	if got := e.profile(p.ID); len(got.Ceiling.AllowedDomains) != 1 {
		t.Errorf("the target row kept an apply that was rolled back: %v", got.Ceiling.AllowedDomains)
	}
	if as, _ := e.pg.ListGovernanceAssignments(context.Background()); len(as) != 0 {
		t.Errorf("the assignment kept an apply that was rolled back: %+v", as)
	}
	for _, action := range []string{"governance.profile.write", "governance.assignment.write"} {
		if n := len(e.audits(action)); n != 0 {
			t.Errorf("a rolled-back approval recorded %s", action)
		}
	}
	// And it is still approvable once the fault clears.
	fail = false
	for _, id := range []uuid.UUID{ch.ID, asg.ID} {
		if w := e.call(e.bob, http.MethodPost, approvePath(id), ""); w.Code != http.StatusOK {
			t.Fatalf("approving after the fault cleared = %d %s, want 200", w.Code, w.Body)
		}
	}
}

// beforeDecideStore runs a hook between the approver's authentication and the decision transaction,
// the window a token can be revoked in.
type beforeDecideStore struct {
	store.PG
	hook func(ctx context.Context)
}

func (s beforeDecideStore) DecideGovernanceChange(ctx context.Context, id uuid.UUID, d store.GovernanceDecision, fn store.GovernanceDecideFunc) (types.GovernanceChange, error) {
	s.hook(ctx)
	return s.PG.DecideGovernanceChange(ctx, id, d, fn)
}

func (e *govEnv) mintToken(sub, email, role string) (types.APIToken, string) {
	e.t.Helper()
	raw := apiTokenPrefix + strings.ReplaceAll(uuid.NewString(), "-", "")
	notTruncated := false
	tok, err := e.pg.CreateAPIToken(context.Background(), types.APIToken{
		ID: uuid.New(), Principal: sub, Email: email, Role: role, UserType: types.UserTypeStandard,
		Name: "approver", GroupsTruncated: &notTruncated,
	}, raw)
	if err != nil {
		e.t.Fatalf("mint token: %v", err)
	}
	return tok, raw
}

// TestPG_GovernanceChanges_ApproverRevokedInsideTheTransactionIsRefused: an approver whose token was
// revoked after it authenticated (RevokeAPIToken), or invalidated by the session-revocation cutoff, or
// demoted below the tier, is refused inside the transaction and the change stays pending.
func TestPG_GovernanceChanges_ApproverRevokedInsideTheTransactionIsRefused(t *testing.T) {
	revocations := &memRevocations{cut: map[string]time.Time{}}
	var hook func(ctx context.Context)
	e := newGovEnv(t, func(c *Config) {
		c.SessionRevocations = revocations
		c.Store = beforeDecideStore{PG: store.NewPG(c.Store.(store.PG).Pool), hook: func(ctx context.Context) { hook(ctx) }}
	})
	p := e.seedProfile("p", "pypi.org")
	propose := func(domains ...string) types.GovernanceChange {
		return e.pending(e.call(e.alice, http.MethodPut, "/api/v1/governance/profiles/"+p.ID.String(), profileBody("p", domains...)))
	}
	assertRefused := func(name string, ch types.GovernanceChange, w *httptest.ResponseRecorder, wantStatus int) {
		t.Helper()
		if w.Code != wantStatus {
			t.Fatalf("%s: %d %s, want %d", name, w.Code, w.Body, wantStatus)
		}
		if st := e.changeState(ch.ID); st != types.GovernanceChangePending {
			t.Errorf("%s: the change is %q, want pending", name, st)
		}
		if got := e.profile(p.ID); len(got.Ceiling.AllowedDomains) != 1 {
			t.Errorf("%s: the target was written: %v", name, got.Ceiling.AllowedDomains)
		}
	}

	// 1. Revoked with RevokeAPIToken after authentication.
	tok, raw := e.mintToken(govSubBob, "bob@corp.example", oidc.RoleSecurityAdmin)
	ch := propose("pypi.org", "github.com")
	hook = func(ctx context.Context) {
		if _, err := e.pg.RevokeAPIToken(ctx, tok.ID, "", time.Now().UTC()); err != nil {
			t.Errorf("revoke: %v", err)
		}
	}
	assertRefused("revoked token", ch, do(t, e.srv, http.MethodPost, approvePath(ch.ID), raw, ""), http.StatusUnauthorized)

	// 2. Invalidated by the session-revocation cutoff after authentication.
	tok2, raw2 := e.mintToken("sub-dave", "dave@corp.example", oidc.RoleSecurityAdmin)
	_ = tok2
	time.Sleep(20 * time.Millisecond) // the cutoff must fall after the token's created_at
	hook = func(ctx context.Context) {
		if err := revocations.RevokeSub(ctx, "sub-dave"); err != nil {
			t.Errorf("revoke sub: %v", err)
		}
	}
	assertRefused("session cutoff", ch, do(t, e.srv, http.MethodPost, approvePath(ch.ID), raw2, ""), http.StatusUnauthorized)

	// 3. The token's role no longer passes the approver predicate (a demotion stamped after
	// authentication): refused with the tier's own 403.
	tok3, raw3 := e.mintToken("sub-erin", "erin@corp.example", oidc.RoleSecurityAdmin)
	hook = func(ctx context.Context) {
		if _, err := e.pool.Exec(ctx, `UPDATE api_tokens SET role = $2 WHERE id = $1`, tok3.ID, oidc.RoleUser); err != nil {
			t.Errorf("demote: %v", err)
		}
	}
	assertRefused("demoted token", ch, do(t, e.srv, http.MethodPost, approvePath(ch.ID), raw3, ""), http.StatusForbidden)

	// Control: an intact token approves, so the refusals above were the revocation and nothing else.
	_, raw4 := e.mintToken("sub-frank", "frank@corp.example", oidc.RoleSecurityAdmin)
	hook = func(context.Context) {}
	if w := do(t, e.srv, http.MethodPost, approvePath(ch.ID), raw4, ""); w.Code != http.StatusOK {
		t.Fatalf("an intact approver token = %d %s, want 200", w.Code, w.Body)
	}
}

// TestPG_GovernanceChanges_AssignmentGoesStaleWhenItsProfileChanges: an assignment change's base covers
// the profile it references, so a rename after proposal makes the approval stale.
func TestPG_GovernanceChanges_AssignmentGoesStaleWhenItsProfileChanges(t *testing.T) {
	e := newGovEnv(t)
	prof := e.seedProfile("contractors", "pypi.org")
	ch := e.pending(e.call(e.alice, http.MethodPost, "/api/v1/governance/assignments",
		fmt.Sprintf(`{"subject_type":"group","subject":"contractors","profile_id":%q,"priority":1}`, prof.ID)))
	var diff struct {
		After struct {
			Profile struct {
				ID   uuid.UUID `json:"id"`
				Name string    `json:"name"`
			} `json:"profile"`
		} `json:"after"`
	}
	if err := json.Unmarshal(ch.Diff, &diff); err != nil || diff.After.Profile.ID != prof.ID || diff.After.Profile.Name != "contractors" {
		t.Fatalf("diff.after does not embed the referenced profile: %s (%v)", ch.Diff, err)
	}

	// A rename is metadata, so it applies directly, and it is exactly what the reviewer did not see.
	if w := e.call(e.carol, http.MethodPut, "/api/v1/governance/profiles/"+prof.ID.String(), profileBody("renamed", "pypi.org")); w.Code != http.StatusOK {
		t.Fatalf("the rename = %d %s, want 200 (metadata is exempt)", w.Code, w.Body)
	}
	w := e.call(e.bob, http.MethodPost, approvePath(ch.ID), "")
	if w.Code != http.StatusConflict || wireReason(t, w) != reasonGovernanceChangeStale {
		t.Fatalf("approving after the profile was renamed = %d %s, want 409 %s", w.Code, w.Body, reasonGovernanceChangeStale)
	}
	if as, _ := e.pg.ListGovernanceAssignments(context.Background()); len(as) != 0 {
		t.Errorf("the stale approval wrote the assignment: %+v", as)
	}
}

// TestPG_GovernanceChanges_RejectReasonIsBounded: a reject reason over 512 characters or with a control
// character is a 400 that leaves the change pending; a good one is stored on the row and in the audit
// row only.
func TestPG_GovernanceChanges_RejectReasonIsBounded(t *testing.T) {
	e := newGovEnv(t)
	p := e.seedProfile("p", "pypi.org")
	ch := e.pending(e.call(e.alice, http.MethodPut, "/api/v1/governance/profiles/"+p.ID.String(), profileBody("p", "pypi.org", "github.com")))

	for name, reason := range map[string]string{
		"too long":          strings.Repeat("x", 513),
		"control character": "line one\nline two",
		"nul":               "a\x00b",
	} {
		body, _ := json.Marshal(map[string]string{"reason": reason})
		if w := e.call(e.bob, http.MethodPost, rejectPath(ch.ID), string(body)); w.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s, want 400", name, w.Code, w.Body)
		}
		if st := e.changeState(ch.ID); st != types.GovernanceChangePending {
			t.Errorf("%s: the change left pending (%q)", name, st)
		}
	}
	atLimit, _ := json.Marshal(map[string]string{"reason": "  " + strings.Repeat("é", 512) + "  "})
	if w := e.call(e.bob, http.MethodPost, rejectPath(ch.ID), string(atLimit)); w.Code != http.StatusOK {
		t.Fatalf("a 512-character reason = %d %s, want 200", w.Code, w.Body)
	}
	var stored string
	if err := e.pool.QueryRow(context.Background(), `SELECT reason FROM governance_changes WHERE id = $1`, ch.ID).Scan(&stored); err != nil || stored != strings.Repeat("é", 512) {
		t.Errorf("stored reason = %q (%v), want the trimmed 512 characters", stored, err)
	}
	rows := e.audits("governance.change.reject")
	if len(rows) != 1 || auditData(t, rows[0])["reason"] != strings.Repeat("é", 512) {
		t.Errorf("reject audit rows = %+v, want one carrying the reason", rows)
	}
	if got := e.profile(p.ID); len(got.Ceiling.AllowedDomains) != 1 {
		t.Errorf("a rejection applied the change: %v", got.Ceiling.AllowedDomains)
	}
}

// TestPG_GovernanceChanges_ExemptionsApplyDirectly: a Leq-proven narrowing update and a pure rename
// apply directly (200, no change row); a widening update, a contact-only change, a rename that also
// widens a composed profile, and the delete of an unassigned profile are held.
func TestPG_GovernanceChanges_ExemptionsApplyDirectly(t *testing.T) {
	e := newGovEnv(t)
	narrow := e.seedProfile("narrowing", "pypi.org", "github.com")
	rename := e.seedProfile("rename-me", "pypi.org")
	contact := e.seedProfile("contact-only", "pypi.org")
	del := e.seedProfile("unassigned", "pypi.org")
	base := e.seedProfile("base", "pypi.org", "github.com", "npmjs.org")
	composedBody := func(name string, domains ...string) string {
		b, _ := json.Marshal(map[string]any{
			"name": name, "base_profile_id": base.ID.String(), "overlay": map[string]any{"allowed_domains": domains},
		})
		return string(b)
	}
	// Created through the admin token: the break-glass applies directly.
	composedRes := e.admin(http.MethodPost, "/api/v1/governance/profiles", composedBody("composed", "pypi.org"))
	var created governanceProfileResponse
	if err := json.Unmarshal(composedRes.Body.Bytes(), &created); err != nil || created.Profile.ID == uuid.Nil {
		t.Fatalf("seed composed profile: %d %s (%v)", composedRes.Code, composedRes.Body, err)
	}
	composedPath := "/api/v1/governance/profiles/" + created.Profile.ID.String()

	// Narrowing: fewer domains.
	if w := e.call(e.alice, http.MethodPut, "/api/v1/governance/profiles/"+narrow.ID.String(), profileBody("narrowing", "pypi.org")); w.Code != http.StatusOK {
		t.Errorf("a Leq-proven narrowing = %d %s, want 200 applied directly", w.Code, w.Body)
	}
	if got := e.profile(narrow.ID); len(got.Ceiling.AllowedDomains) != 1 {
		t.Errorf("the narrowing did not apply: %v", got.Ceiling.AllowedDomains)
	}
	// Pure rename.
	if w := e.call(e.alice, http.MethodPut, "/api/v1/governance/profiles/"+rename.ID.String(), profileBody("renamed", "pypi.org")); w.Code != http.StatusOK {
		t.Errorf("a pure rename = %d %s, want 200", w.Code, w.Body)
	}
	if got := e.profile(rename.ID); got.Name != "renamed" {
		t.Errorf("the rename did not apply: %q", got.Name)
	}
	if n := e.pendingCount(); n != 0 {
		t.Fatalf("exempt writes left %d pending changes", n)
	}
	if rows := e.audits("governance.profile.write"); len(rows) < 3 {
		t.Errorf("exempt writes are audited as ever: %d rows", len(rows))
	}

	// Held: widening, contact-only, rename + widen, delete.
	e.pending(e.call(e.alice, http.MethodPut, "/api/v1/governance/profiles/"+narrow.ID.String(), profileBody("narrowing", "pypi.org", "github.com")))
	withContact := `{"name":"contact-only","ceiling":{"allowed_domains":["pypi.org"],"min_confinement_class":"CC2"},"contact":{"owner":"Sec","email":"sec@example.com"}}`
	e.pending(e.call(e.alice, http.MethodPut, "/api/v1/governance/profiles/"+contact.ID.String(), withContact))
	if got := e.profile(contact.ID); got.Contact != nil {
		t.Errorf("a contact-only PUT was applied: %+v", got.Contact)
	}
	e.pending(e.call(e.alice, http.MethodPut, composedPath, composedBody("composed-renamed", "pypi.org", "github.com")))
	e.pending(e.call(e.alice, http.MethodDelete, "/api/v1/governance/profiles/"+del.ID.String(), ""))
	if _, err := e.pg.GetGovernanceProfile(context.Background(), del.ID); err != nil {
		t.Errorf("a held delete removed the profile: %v", err)
	}

	// A narrowing overlay on the composed profile is exempt too (it is Leq the current effective).
	if w := e.call(e.carol, http.MethodPut, "/api/v1/governance/profiles/"+base.ID.String(), profileBody("base", "pypi.org", "github.com")); w.Code != http.StatusOK {
		// base was narrowed from three domains to two: Leq, so applied directly.
		t.Errorf("narrowing the base = %d %s, want 200", w.Code, w.Body)
	}
}

// TestPG_GovernanceChanges_DiffIsRedactedAndPayloadHoldsNoSecret: a stored ceiling that carries a raw
// llm_inspection secret value (a direct database edit) never reaches the diff, the 202, a read of the
// change or the payload.
func TestPG_GovernanceChanges_DiffIsRedactedAndPayloadHoldsNoSecret(t *testing.T) {
	e := newGovEnv(t)
	const raw = "raw-secret-value-must-never-appear"
	p, err := e.pg.UpsertGovernanceProfile(context.Background(), types.GovernanceProfile{
		Name: "inspected", CreatedBy: "seed",
		Ceiling: types.RunPolicySpec{
			AllowedDomains: []string{"pypi.org"}, MinConfinementClass: types.CC2,
			LLMInspection: &types.LLMInspectionSpec{Mode: "alert", DetectSecrets: true, WorkspaceSecretValues: []string{raw}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	w := e.call(e.alice, http.MethodPut, "/api/v1/governance/profiles/"+p.ID.String(), profileBody("inspected", "pypi.org", "github.com"))
	ch := e.pending(w)
	if strings.Contains(w.Body.String(), raw) {
		t.Errorf("the 202 carries the raw secret value: %s", w.Body)
	}
	if !strings.Contains(string(ch.Diff), "redacted") {
		t.Errorf("diff.before does not show the redaction: %s", ch.Diff)
	}
	if got := e.call(e.bob, http.MethodGet, "/api/v1/governance/changes/"+ch.ID.String(), ""); strings.Contains(got.Body.String(), raw) {
		t.Errorf("GET one change carries the raw secret value: %s", got.Body)
	}
	var payload, diff string
	if err := e.pool.QueryRow(context.Background(), `SELECT payload::text, diff::text FROM governance_changes WHERE id = $1`, ch.ID).Scan(&payload, &diff); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(payload, raw) || strings.Contains(diff, raw) {
		t.Errorf("the stored change carries the raw secret value:\npayload %s\ndiff %s", payload, diff)
	}
}

// TestPG_GovernanceChanges_LocalModeRefuses: local mode authenticates nobody, so no request there can
// prove a second human: every covered write and every decision is a 503 with the named reason. (The
// admin token has no lane of its own there: local mode answers before any credential is read.)
func TestPG_GovernanceChanges_LocalModeRefuses(t *testing.T) {
	e := newGovEnv(t, func(c *Config) {
		c.LocalMode = true
		c.LocalOperator = "local:alice"
		c.OIDC = nil
	})
	p := e.seedProfile("p", "pypi.org")
	local := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Host = "127.0.0.1:8080"
		req.RemoteAddr = "127.0.0.1:54321"
		w := httptest.NewRecorder()
		panicFails(t, e.srv.Handler()).ServeHTTP(w, req)
		return w
	}
	for name, w := range map[string]*httptest.ResponseRecorder{
		"create profile": local(http.MethodPost, "/api/v1/governance/profiles", profileBody("n", "pypi.org")),
		"update profile": local(http.MethodPut, "/api/v1/governance/profiles/"+p.ID.String(), profileBody("p", "pypi.org")),
		"delete profile": local(http.MethodDelete, "/api/v1/governance/profiles/"+p.ID.String(), ""),
		"assignment":     local(http.MethodPost, "/api/v1/governance/assignments", fmt.Sprintf(`{"subject_type":"all","profile_id":%q}`, p.ID)),
		"approve":        local(http.MethodPost, approvePath(uuid.New()), ""),
		"reject":         local(http.MethodPost, rejectPath(uuid.New()), ""),
	} {
		if w.Code != http.StatusServiceUnavailable || wireReason(t, w) != reasonGovernanceSecondHumanLocalMode {
			t.Errorf("%s in local mode: %d %s, want 503 %s", name, w.Code, w.Body, reasonGovernanceSecondHumanLocalMode)
		}
	}
	if got := e.profile(p.ID); len(got.Ceiling.AllowedDomains) != 1 {
		t.Errorf("a refused local-mode write changed the profile: %v", got.Ceiling.AllowedDomains)
	}
	if n := e.pendingCount(); n != 0 {
		t.Errorf("local mode held %d changes", n)
	}
}

// TestPG_GovernanceChanges_ChangeListHonoursState: the queue defaults to pending, ?state= narrows, and
// an unknown state is a 400.
func TestPG_GovernanceChanges_ChangeListHonoursState(t *testing.T) {
	e := newGovEnv(t)
	p := e.seedProfile("p", "pypi.org")
	q := e.seedProfile("q", "pypi.org")
	a := e.pending(e.call(e.alice, http.MethodPut, "/api/v1/governance/profiles/"+p.ID.String(), profileBody("p", "pypi.org", "github.com")))
	b := e.pending(e.call(e.alice, http.MethodPut, "/api/v1/governance/profiles/"+q.ID.String(), profileBody("q", "pypi.org", "github.com")))
	if w := e.call(e.bob, http.MethodPost, rejectPath(a.ID), ""); w.Code != http.StatusOK {
		t.Fatalf("reject = %d %s", w.Code, w.Body)
	}
	ids := func(state string) []uuid.UUID {
		path := "/api/v1/governance/changes"
		if state != "" {
			path += "?state=" + state
		}
		w := e.call(e.bob, http.MethodGet, path, "")
		var out []types.GovernanceChange
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || w.Code != http.StatusOK {
			t.Fatalf("list %q = %d %s (%v)", state, w.Code, w.Body, err)
		}
		var got []uuid.UUID
		for _, c := range out {
			got = append(got, c.ID)
		}
		return got
	}
	if got := ids(""); len(got) != 1 || got[0] != b.ID {
		t.Errorf("the default list = %v, want only the pending %s", got, b.ID)
	}
	if got := ids("rejected"); len(got) != 1 || got[0] != a.ID {
		t.Errorf("state=rejected = %v, want %s", got, a.ID)
	}
	if w := e.call(e.bob, http.MethodGet, "/api/v1/governance/changes?state=bogus", ""); w.Code != http.StatusBadRequest ||
		wireReason(t, w) != reasonGovernanceChangeStateInvalid {
		t.Errorf("state=bogus = %d %s, want 400 %s", w.Code, w.Body, reasonGovernanceChangeStateInvalid)
	}
	if w := e.call(e.bob, http.MethodGet, "/api/v1/governance/changes/"+uuid.NewString(), ""); w.Code != http.StatusNotFound ||
		wireReason(t, w) != reasonGovernanceChangeNotFound {
		t.Errorf("an unknown change = %d %s, want 404 %s", w.Code, w.Body, reasonGovernanceChangeNotFound)
	}
}
