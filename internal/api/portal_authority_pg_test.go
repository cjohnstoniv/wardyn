// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// portalState is everything the SCIM entry points could change about one portal person.
func (l *portalLife) portalState(t *testing.T, principal string) string {
	t.Helper()
	i := l.ident(t, principal)
	var b strings.Builder
	fmt.Fprintf(&b, "deactivated=%v purge_after=%v purged=%v epoch=%d email=%q\n", i.DeactivatedAt, i.PurgeAfter, i.PurgedAt, i.AuthorityEpoch, i.EmailLower)
	b.WriteString(ledgerText(t, l.pool, i.ID))
	fmt.Fprintf(&b, "grants=%d audit=%d kills=%d\n", l.count(t, `SELECT count(*) FROM portal_grants WHERE principal = $1`, principal),
		len(l.h.audit.snapshot()), l.run.killCount())
	return b.String()
}

// scimLeavesPortalRowAlone suspends a portal person with a deadline that has passed (step one only, so the
// ledger holds pending steps and an owed refusal row), then runs every SCIM lifecycle entry point and the SCIM
// sweep over it: the row, its ledger, its grants and its run are exactly as found, and no trigger error is
// raised, because none of them reaches a write.
func (l *portalLife) scimLeavesPortalRowAlone(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	principal, _ := l.signIn(t, "u1")
	run := l.seedRun(t, principal, types.RunRunning)
	id := l.ident(t, principal).ID
	if _, _, _, err := l.h.srv.portalSuspendStepOne(ctx, l.st, id, portalOrigin(types.ActorHuman, "admin"), time.Second, nil); err != nil {
		t.Fatalf("step one: %v", err)
	}
	if err := oweRefusalAudit(ctx, l.st, id, store.JobKindSuspend, store.JobKey{Step: store.JobStepRefusalAudit + "failure/test"}); err != nil {
		t.Fatal(err)
	}
	l.waitDue(t, id)
	if _, err := l.pool.Exec(ctx, `UPDATE deprovision_jobs SET updated_at = now() - interval '1 hour'`); err != nil {
		t.Fatal(err)
	}
	before := l.portalState(t, principal)
	if !strings.Contains(before, "pending") {
		t.Fatalf("control: the person has no pending ledger rows:\n%s", before)
	}
	srv := l.h.srv
	// Each old entry point answers with what it answers for an identity of another issuer, which is no error and
	// no work: suspendIdentity, finishPurge, drainOwedRefusals and the sweep return nil; the due-only purge reports
	// that it purged nothing; the unconditional purge reports true, as it does for every row it is asked to finish,
	// and its finish step leaves the foreign row alone.
	if err := srv.suspendIdentity(ctx, l.st, id, "primary"); err != nil {
		t.Errorf("suspendIdentity = %v, want nil", err)
	}
	if purged, err := srv.purgeIdentity(ctx, l.st, id, "primary", true); purged || err != nil {
		t.Errorf("purgeIdentity(requireDue) = %v, %v; want false, nil", purged, err)
	}
	if purged, err := srv.purgeIdentity(ctx, l.st, id, "primary", false); !purged || err != nil {
		t.Errorf("purgeIdentity(unconditional) = %v, %v; want true, nil", purged, err)
	}
	if err := srv.finishPurge(ctx, l.st, id, "primary"); err != nil {
		t.Errorf("finishPurge = %v, want nil", err)
	}
	if err := srv.drainOwedRefusals(ctx, l.st, store.PendingRefusalAudit{IdentityID: id, Kind: store.JobKindSuspend}); err != nil {
		t.Errorf("drainOwedRefusals = %v, want nil", err)
	}
	if err := srv.SweepSCIMPurge(ctx); err != nil {
		t.Errorf("SCIM sweep = %v, want nil", err)
	}
	if after := l.portalState(t, principal); after != before {
		t.Errorf("a SCIM path changed the portal person:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	if st := l.runState(t, run); st != types.RunRunning {
		t.Errorf("the portal person's run = %s, want RUNNING", st)
	}
}

// The 0.10.1 SCIM paths, unchanged, skip a portal row: with SCIM on, a deactivated portal person with pending
// suspend, purge and refusal jobs and a passed deadline is left as found by every entry point.
func TestPortalLifecycle_OldSCIMPathsSkipPortalRows_PG(t *testing.T) {
	l := newPortalLife(t)
	l.scimOn(lifeWorkforceIssuer, time.Hour)
	l.scimLeavesPortalRowAlone(t)
}

// The SCIM sweep's and the status read's scope: with SCIM configured, a portal row never appears on the SCIM
// page, even when the configured issuer is the portal's synthetic one, and never fills its cap.
func TestSCIMQueries_RefuseDelegatePersonIssuer_PG(t *testing.T) {
	l := newPortalLife(t)
	principal, _ := l.signIn(t, "u1")
	if w := l.post(t, l.admin, principal, "suspend", ""); w.Code != http.StatusOK {
		t.Fatalf("suspend = %d", w.Code)
	}
	k := oidc.PortalIssuerKey(portalTestIssuer)
	for _, issuer := range []string{types.PortalPersonPrefix + k, portalTestIssuer, lifeWorkforceIssuer} {
		l.scimOn(issuer, time.Hour)
		l.h.srv.router = l.h.srv.routes()
		w := l.get(t, l.secAdmin, "/api/v1/scim/status")
		if w.Code != http.StatusOK {
			t.Fatalf("GET /scim/status (issuer %s) = %d %s", issuer, w.Code, w.Body.String())
		}
		var st scimStatus
		if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil {
			t.Fatal(err)
		}
		if len(st.Deactivated) != 0 || len(st.Pending) != 0 {
			t.Errorf("SCIM status with issuer %s lists %+v %+v, want no portal person", issuer, st.Deactivated, st.Pending)
		}
	}
	// The portal view lists the person whatever the SCIM setting is.
	l.h.srv.cfg.SCIM = nil
	l.h.srv.router = l.h.srv.routes()
	var pv portalLifecycleStatus
	w := l.get(t, l.secAdmin, "/api/v1/admin/portal-lifecycle/status")
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &pv) != nil || len(pv.Deactivated) != 1 || pv.Deactivated[0].Person != principal {
		t.Errorf("portal lifecycle status with SCIM off = %d %s, want the one suspended person", w.Code, w.Body.String())
	}
}

// More portal rows than the cap and a few workforce rows, each deactivated with a failed step: the SCIM page lists
// only the workforce rows, all of them; the portal view lists only portal rows, up to its cap.
func TestLifecycleStatus_PortalAndWorkforceRowsSeparated_PG(t *testing.T) {
	l := newPortalLife(t)
	ctx := context.Background()
	l.scimOn(lifeWorkforceIssuer, time.Hour)
	l.h.srv.router = l.h.srv.routes()
	// More portal rows than the page cap, each deactivated, more recently than the workforce rows.
	if _, err := l.pool.Exec(ctx, `
		INSERT INTO principal_identities (principal, issuer, tenant_id, object_id, deactivated_at)
		SELECT 'wf-' || g, $1, 'tenant-a', 'oid-' || g, now() - interval '3 days' FROM generate_series(1, 3) g`, lifeWorkforceIssuer); err != nil {
		t.Fatal(err)
	}
	principals := make([]string, 0, scimStatusListCap+3)
	for i := range scimStatusListCap + 3 {
		l.ver.subj = l.person(fmt.Sprintf("p%03d", i), "")
		principals = append(principals, l.ver.subj.Principal)
		if _, _, err := l.st.IssuePortalIdentity(ctx, l.ver.subj.Principal, l.ver.subj.K, "", portalTestIssuer, l.d.ID, l.d.AdmissionRev); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := l.pool.Exec(ctx, `
		UPDATE principal_identities SET deactivated_at = now() - interval '1 hour' WHERE starts_with(issuer, 'delegate:person:')`); err != nil {
		t.Fatal(err)
	}
	// A failed step on every row of both kinds.
	if _, err := l.pool.Exec(ctx, `
		INSERT INTO deprovision_jobs (identity_id, kind, step, target, state, attempts, last_error)
		SELECT id, 'suspend', 'kill_run', '', 'pending', 1, 'the runner is unreachable' FROM principal_identities
		 WHERE starts_with(issuer, 'delegate:person:') OR principal LIKE 'wf-%'`); err != nil {
		t.Fatal(err)
	}
	var scimPage scimStatus
	if w := l.get(t, l.secAdmin, "/api/v1/scim/status"); w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &scimPage) != nil {
		t.Fatalf("GET /scim/status = %d %s", w.Code, w.Body.String())
	}
	if len(scimPage.Deactivated) != 3 || len(scimPage.Pending) != 3 {
		t.Errorf("SCIM page: %d deactivated, %d pending; want the 3 workforce rows only", len(scimPage.Deactivated), len(scimPage.Pending))
	}
	for _, d := range scimPage.Deactivated {
		if strings.HasPrefix(d.Person, "delegate:person:") {
			t.Errorf("a portal person on the SCIM page: %s", d.Person)
		}
	}
	// With SCIM off the portal view still answers, and lists portal rows only.
	l.h.srv.cfg.SCIM = nil
	l.h.srv.router = l.h.srv.routes()
	var pv portalLifecycleStatus
	if w := l.get(t, l.secAdmin, "/api/v1/admin/portal-lifecycle/status"); w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &pv) != nil {
		t.Fatalf("GET portal status = %d %s", w.Code, w.Body.String())
	}
	if len(pv.Deactivated) != scimStatusListCap {
		t.Errorf("portal view lists %d deactivated, want its cap %d", len(pv.Deactivated), scimStatusListCap)
	}
	for _, d := range pv.Deactivated {
		if !slices.Contains(principals, d.Person) {
			t.Errorf("a non-portal person on the portal view: %s", d.Person)
		}
	}
	for _, p := range pv.Pending {
		if !strings.HasPrefix(p.Person, "delegate:person:") {
			t.Errorf("a workforce failure on the portal view: %s", p.Person)
		}
	}
}

// The portal sweep acts on portal rows only. Workforce rows, more than a batch of each kind (due, pending, owed),
// sort ahead of a portal row of each kind; the portal rows are all finished and the workforce rows are untouched.
func TestPortalSweep_NeverSelectsWorkforceRows_PG(t *testing.T) {
	l := newPortalLife(t)
	ctx := context.Background()
	n := scimSweepBatch + 10
	// Workforce people, older than every portal row: due, pending and owed.
	for _, q := range []string{
		`INSERT INTO principal_identities (principal, issuer, tenant_id, object_id, created_at, deactivated_at, purge_after)
		 SELECT 'wfdue-' || g, $1, 'tenant-a', 'due-' || g, now() - interval '30 days', now() - interval '3 days', now() - interval '2 days' - g * interval '1 second'
		   FROM generate_series(1, $2::int) g`,
		`INSERT INTO principal_identities (principal, issuer, tenant_id, object_id, created_at, deactivated_at)
		 SELECT 'wfpend-' || g, $1, 'tenant-a', 'pend-' || g, now() - interval '30 days', now() - interval '3 days' FROM generate_series(1, $2::int) g`,
		`INSERT INTO principal_identities (principal, issuer, tenant_id, object_id, created_at, deactivated_at)
		 SELECT 'wfowed-' || g, $1, 'tenant-a', 'owed-' || g, now() - interval '30 days', now() - interval '3 days' FROM generate_series(1, $2::int) g`,
	} {
		if _, err := l.pool.Exec(ctx, q, lifeWorkforceIssuer, n); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range []string{
		`INSERT INTO deprovision_jobs (identity_id, kind, step, target, state, attempts, detail)
		 SELECT id, 'suspend', 'cutoff', '', 'done', 1, jsonb_build_object('deactivated_at_us', (extract(epoch FROM deactivated_at) * 1000000)::bigint)
		   FROM principal_identities WHERE principal LIKE 'wfdue-%'`,
		`INSERT INTO deprovision_jobs (identity_id, kind, step, target) SELECT id, 'suspend', 'audit_deactivate', '' FROM principal_identities WHERE principal LIKE 'wfpend-%'`,
		`INSERT INTO deprovision_jobs (identity_id, kind, step, target, state, attempts, last_error)
		 SELECT id, 'suspend', 'refusal_audit/failure/test', '', 'pending', 1, 'owed' FROM principal_identities WHERE principal LIKE 'wfowed-%'`,
		`UPDATE deprovision_jobs SET updated_at = now() - interval '1 hour'`,
	} {
		if _, err := l.pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	// One portal person of each kind.
	due, _ := l.signIn(t, "u-due")
	if w := l.post(t, l.admin, due, "suspend", `{"purge_after_seconds": 1}`); w.Code != http.StatusOK {
		t.Fatalf("suspend the due person = %d %s", w.Code, w.Body.String())
	}
	pending, _ := l.signIn(t, "u-pending")
	pendingRun := l.seedRun(t, pending, types.RunRunning)
	pendingID := l.ident(t, pending).ID
	if _, _, _, err := l.h.srv.portalSuspendStepOne(ctx, l.st, pendingID, portalOrigin(types.ActorHuman, "admin"), 0, nil); err != nil {
		t.Fatal(err)
	}
	owed, _ := l.signIn(t, "u-owed")
	if w := l.post(t, l.admin, owed, "suspend", ""); w.Code != http.StatusOK {
		t.Fatalf("suspend the owed person = %d", w.Code)
	}
	owedID := l.ident(t, owed).ID
	if err := oweRefusalAudit(ctx, l.st, owedID, store.JobKindSuspend, store.JobKey{Step: store.JobStepRefusalAudit + "failure/test"}); err != nil {
		t.Fatal(err)
	}
	l.waitDue(t, l.ident(t, due).ID)

	workforce := func() string {
		var out string
		_ = l.pool.QueryRow(ctx, `SELECT coalesce(string_agg(i.principal || ':' || coalesce(i.purged_at::text, '-') || ':' || j.state || ':' || j.attempts || ':' || j.updated_at::text, ',' ORDER BY i.principal, j.step), '')
			FROM principal_identities i JOIN deprovision_jobs j ON j.identity_id = i.id WHERE i.issuer = $1`, lifeWorkforceIssuer).Scan(&out)
		return out
	}
	before := workforce()
	if err := l.h.srv.SweepPortalLifecycle(ctx); err != nil {
		t.Fatalf("portal sweep: %v", err)
	}
	if got := l.ident(t, due); got.PurgedAt == nil {
		t.Error("the due portal person was not purged: workforce rows took the batch")
	}
	if st := l.runState(t, pendingRun); st != types.RunKilled || !l.allDone(t, pendingID) {
		t.Errorf("the pending portal suspension was not resumed (run %s):\n%s", st, ledgerText(t, l.pool, pendingID))
	}
	var drained int
	for _, ev := range l.rows("person.deprovision") {
		if ev.Outcome == "failure" && lifeData(ev)["reason"] == "test" && ev.Target == owedID.String() {
			drained++
		}
	}
	if drained != 1 {
		t.Errorf("the owed portal refusal row was written %d times, want 1", drained)
	}
	if after := workforce(); after != before {
		t.Error("the portal sweep changed a workforce row or its ledger")
	}
}

// auditRowText is one audit event as a line with nothing left out: action, actor type, actor, outcome, target, run
// and the whole data object. An id the test knows is its role name (roles maps id to name), an id it does not know
// stays an id so the line cannot match, a time is "<time>", and an empty or absent list is "[]".
func auditRowText(ev types.AuditEvent, roles map[string]string) string {
	var walk func(v any) any
	walk = func(v any) any {
		switch x := v.(type) {
		case string:
			if r, ok := roles[x]; ok {
				return "<" + r + ">"
			}
			if _, err := time.Parse(time.RFC3339Nano, x); err == nil {
				return "<time>"
			}
			return x
		case nil:
			return []any{}
		case []any:
			out := make([]any, len(x))
			for i, e := range x {
				out[i] = walk(e)
			}
			return out
		case map[string]any:
			out := make(map[string]any, len(x))
			for k, e := range x {
				out[k] = walk(e)
			}
			return out
		}
		return v
	}
	data := map[string]any{}
	_ = json.Unmarshal(ev.Data, &data)
	raw, _ := json.Marshal(walk(data))
	role := func(id string) string {
		if r, ok := roles[id]; ok {
			return "<" + r + ">"
		}
		return id
	}
	run := "-"
	if ev.RunID != nil {
		run = role(ev.RunID.String())
	}
	return fmt.Sprintf("%s|%s|%s|%s|target=%s|run=%s|%s", ev.Action, ev.ActorType, ev.Actor, ev.Outcome, role(ev.Target), run, raw)
}

// The origin argument on the shared helpers changes nothing for SCIM: one SCIM suspension and one purge of a known
// person write exactly these audit events and no others. Every event of the request is compared whole, with its
// target and data; the ids are the fixture's own (named by role), the counts are bound to what the fixture holds
// (one run, one workspace, no credentials, tokens or keys) and to the session cuts the database holds, and only
// times are normalised. The fixture is the pre-origin behaviour written out; a run that disagrees prints both lists.
func TestPortalLifecycle_SharedHelpersSCIMOutputUnchanged_PG(t *testing.T) {
	e := newSCIMEnv(t)
	const sub, email, oid = "sub-baseline", "baseline.me@corp.example", "aaaaaaaa-0000-4000-8000-0000000000c1"
	ctx := context.Background()
	e.seedEntra(sub, email, oid)
	userID := e.postUserID(e.a, oid, email, email)
	ws, err := e.st.CreateWorkspace(ctx, types.Workspace{
		ID: uuid.New(), Name: "ws-baseline", OwnedBy: sub, Status: types.WorkspaceScanned,
		Sources:   []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeEphemeral, Target: "/home/agent/work"}},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	run := e.seedRun(sub, types.RunRunning)
	if w := e.del(e.b, userID); w.Code != http.StatusNoContent {
		t.Fatalf("DELETE = %d %s", w.Code, w.Body.String())
	}
	if e.runState(run) != types.RunKilled {
		t.Fatalf("control: the run is %s", e.runState(run))
	}
	// The forms the suspension cuts: the bound principal, the email, and the directory's tenant/object form. Each
	// carries exactly one cutoff, and the receipt counts all of them.
	forms := []string{sub, email, entraPrincipalPrefix + e.tenant + ":" + oid}
	for _, form := range forms {
		var n int
		if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM oidc_session_revocations WHERE sub = $1`, form).Scan(&n); err != nil || n != 1 {
			t.Fatalf("control: session cutoffs for %q = %d, %v; want 1", form, n, err)
		}
	}
	cuts := len(forms)
	roles := map[string]string{userID: "person", run.String(): "run", ws.ID.String(): "workspace"}
	var got []string
	for _, ev := range e.b.h.audit.snapshot() {
		got = append(got, auditRowText(ev, roles))
	}
	slices.Sort(got)
	zero := `"assignments_deleted":0,"components_erased":0,"credentials_erased":0,"drives":[],"email_rows_kept":0,"granted_drives":[],"grants_deleted":0,` +
		`"invitations_cleared":0,"invitations_deleted":0,"kind":"purge","live_left":0,"masks_fenced":0,"pools_emptied":0,"pools_narrowed":0,` +
		`"runner_pools_deleted":0,"service_identities_cleared":0,"templates_erased":0,"workspaces_reassigned":1`
	want := []string{
		"person.deprovision|system|wardyn/scim|success|target=<person>|run=-|{" + zero + "}",
		fmt.Sprintf(`person.deprovision|system|wardyn/scim|success|target=<person>|run=-|{"keys_deleted":0,"kind":"suspend","runs_killed":1,"sessions_cut":%d,"tokens_revoked":0}`, cuts),
		`run.kill|system|wardyn/scim|success|target=<run>|run=<run>|{"reason":"scim_suspend"}`,
		`scim.user.deactivate|system|wardyn/scim|success|target=<person>|run=-|{"slot":"primary"}`,
		`workspace.reassign|system|wardyn/scim|success|target=<workspace>|run=-|{"from_owner":"` + sub + `","reason":"scim_purge"}`,
	}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("SCIM audit events of one suspension and purge:\n got:\n  %s\nwant:\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
	// The origin the SCIM paths use, field by field: the actor, the kill reason, the deactivation action, the
	// purge reason, and no portal behaviour.
	o := e.b.srv.scimOrigin()
	if o.actorType != types.ActorSystem || o.actor != scimActor || o.killReason != "scim_suspend" || o.deactivateAction != "scim.user.deactivate" ||
		o.purgeReason != "scim_purge" || o.portal {
		t.Errorf("scimOrigin() = %+v", o)
	}
}
