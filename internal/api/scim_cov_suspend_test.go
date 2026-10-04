// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

var scimCovObjectForm = entraPrincipalPrefix + scimCovTenant + ":" + scimCovOID

// scimCovLeaver is a person known by every form: a sub, the object-id principal and two email forms,
// with a token under each of the sub and the email, a key, and runs under the sub and the email.
type scimCovLeaver struct {
	st                       *scimCovStore
	id                       uuid.UUID
	tokSub, tokEmail, tokBy  uuid.UUID
	runSub, runEmail, runEnd uuid.UUID
	runBy                    uuid.UUID
}

func scimCovSeedLeaver(st *scimCovStore) scimCovLeaver {
	row := scimCovBound()
	row.ScimUserName = "Pat@Corp.example"
	ident := st.addIdentity(row, "pat.alias@corp.example", "pat@corp.example")
	l := scimCovLeaver{st: st, id: ident.ID}
	l.tokSub = st.addToken(types.APIToken{Principal: "sub-pat", Name: "sub"})
	l.tokEmail = st.addToken(types.APIToken{Principal: "pat@corp.example", Email: "pat@corp.example", Name: "email"})
	l.tokBy = st.addToken(types.APIToken{Principal: "sub-bystander", Email: "other@corp.example", Name: "other"})
	st.keys = append(st.keys,
		types.SSHPublicKey{Fingerprint: "fp-pat", Principal: "sub-pat"},
		types.SSHPublicKey{Fingerprint: "fp-other", Principal: "sub-bystander"})
	l.runSub = st.addRun("sub-pat", types.RunRunning, "sbx-sub")
	l.runEmail = st.addRun("pat@corp.example", types.RunStarting, "sbx-email")
	l.runEnd = st.addRun("sub-pat", types.RunCompleted, "sbx-ended")
	l.runBy = st.addRun("sub-bystander", types.RunRunning, "sbx-other")
	return l
}

// scimCovTargets are the forms the leaver above is swept under: bound forms first, then the emails the
// aliases, the login email and the SCIM user name resolve to.
var scimCovTargets = []string{"sub-pat", scimCovObjectForm, "pat.alias@corp.example", "pat@corp.example"}

func scimCovContainsAll(t *testing.T, got, want []string, what string) {
	t.Helper()
	for _, w := range want {
		if !slices.Contains(got, w) {
			t.Errorf("%s = %v, missing %q", what, got, w)
		}
	}
}

func TestSCIMCovSuspensionReachesEveryFormAndWritesTheLedger(t *testing.T) {
	st := newSCIMCovStore()
	l := scimCovSeedLeaver(st)
	e := newSCIMCovEnv(t, st)

	w := e.scim(http.MethodPatch, "/scim/v2/Users/"+l.id.String(), scimCovPatch(scimCovReplace("active", "false")))
	if w.Code != http.StatusOK || scimCovDecode[scimCovUser](t, w).Active {
		t.Fatalf("PATCH active=false = %d %s", w.Code, w.Body.String())
	}

	if len(st.plans) != 1 {
		t.Fatalf("SuspendIdentity ran %d times, want once", len(st.plans))
	}
	plan := st.plans[0]
	if plan.IdentityID != l.id || !slices.Equal(plan.Principals, scimCovTargets[:2]) || !slices.Equal(plan.CutoffSubs, scimCovTargets) || plan.PurgeAfter != 0 {
		t.Errorf("plan = %+v, want the bound forms deactivated and the cutoff cut under every form", plan)
	}
	if st.callCount("PrincipalsByEmail") != 0 {
		t.Error("a bound identity resolved its emails to subs")
	}
	revoked, _ := e.rev.snapshot()
	scimCovContainsAll(t, revoked, scimCovTargets, "sessions revoked")

	for name, id := range map[string]uuid.UUID{"the sub's token": l.tokSub, "the email-form token": l.tokEmail} {
		if !st.tokenRevoked(id) {
			t.Errorf("%s was left live", name)
		}
	}
	if st.tokenRevoked(l.tokBy) {
		t.Error("a bystander's token was revoked")
	}
	if i := slices.IndexFunc(st.keys, func(k types.SSHPublicKey) bool { return k.Principal == "sub-pat" }); i >= 0 {
		t.Error("the leaver's SSH key was left")
	}
	if !slices.ContainsFunc(st.keys, func(k types.SSHPublicKey) bool { return k.Fingerprint == "fp-other" }) || slices.Contains(st.deletedKeys, "") {
		t.Errorf("a bystander's key was touched: keys %v deleted for %v", st.keys, st.deletedKeys)
	}

	for name, id := range map[string]uuid.UUID{"sub": l.runSub, "email": l.runEmail} {
		if got := st.runState(id); got != types.RunKilled {
			t.Errorf("run under the %s = %s, want KILLED", name, got)
		}
	}
	if st.runState(l.runEnd) != types.RunCompleted || st.runState(l.runBy) != types.RunRunning {
		t.Errorf("an ended run or a bystander's run was touched: %s %s", st.runState(l.runEnd), st.runState(l.runBy))
	}
	if got := e.runner.killCount(); got != 2 {
		t.Errorf("KillSandbox ran %d times, want once per live run", got)
	}
	if got := e.auditRows("run.kill"); len(got) != 2 || e.auditData(got[0])["reason"] != "scim_suspend" || got[0].Outcome != "success" {
		t.Errorf("run.kill rows = %+v, want two successes naming scim_suspend", got)
	}

	for _, j := range st.ledger(l.id, store.JobKindSuspend) {
		if !j.Done {
			t.Errorf("ledger row %s/%s is still pending: %q", j.Step, j.Target, j.LastError)
		}
	}
	deact, deprov := e.auditRows("scim.user.deactivate"), e.auditRows("person.deprovision")
	if len(deact) != 1 || e.auditData(deact[0])["slot"] != scimSlotPrimary || deact[0].Target != l.id.String() {
		t.Errorf("scim.user.deactivate rows = %+v, want one naming the slot", deact)
	}
	if len(deprov) != 1 {
		t.Fatalf("person.deprovision rows = %+v, want one", deprov)
	}
	d := e.auditData(deprov[0])
	if d["kind"] != store.JobKindSuspend || d["sessions_cut"] != float64(len(scimCovTargets)) || d["tokens_revoked"] != float64(2) ||
		d["keys_deleted"] != float64(1) || d["runs_killed"] != float64(2) {
		t.Errorf("person.deprovision data = %v, want 4 cuts, 2 tokens, 1 key, 2 runs", d)
	}
}

func TestSCIMCovRepeatedSuspensionWritesNothingNew(t *testing.T) {
	st := newSCIMCovStore()
	l := scimCovSeedLeaver(st)
	e := newSCIMCovEnv(t, st)
	path, body := "/scim/v2/Users/"+l.id.String(), scimCovPatch(scimCovReplace("active", "false"))
	e.scim(http.MethodPatch, path, body)
	rows, kills, revokes := len(e.h.audit.snapshot()), e.runner.killCount(), len(st.revokedToks)
	rev, _ := e.rev.snapshot()

	w := e.scim(http.MethodPatch, path, body)
	if w.Code != http.StatusOK {
		t.Fatalf("the repeated request = %d %s", w.Code, w.Body.String())
	}
	if len(st.plans) != 1 {
		t.Errorf("SuspendIdentity ran %d times, want the cutoff left alone once it is done", len(st.plans))
	}
	if got := len(st.revokedToks); got != revokes || e.runner.killCount() != kills {
		t.Errorf("the repeat revoked %d more tokens and killed %d more sandboxes", got-revokes, e.runner.killCount()-kills)
	}
	if now, _ := e.rev.snapshot(); len(now) != len(rev) {
		t.Errorf("the repeat revoked sessions again: %v", now[len(rev):])
	}
	for _, ev := range e.h.audit.snapshot()[rows:] {
		if ev.Action == "scim.user.deactivate" || ev.Action == "person.deprovision" || ev.Action == "run.kill" {
			t.Errorf("the repeat wrote %s again", ev.Action)
		}
	}
}

func TestSCIMCovSuspensionOfAnUnboundIdentityResolvesEmailsToSubs(t *testing.T) {
	st := newSCIMCovStore()
	ident := st.addIdentity(store.PrincipalIdentity{
		Issuer: scimCovIssuer, TenantID: scimCovTenant, ObjectID: scimCovOID, ScimUserName: "Pat@Corp.example", EmailLower: "pat@corp.example",
	})
	st.emailSubs["pat@corp.example"] = []string{"sub-resolved"}
	tok := st.addToken(types.APIToken{Principal: "sub-resolved", Name: "t"})
	bystander := st.addToken(types.APIToken{Principal: "sub-bystander", Name: "b"})
	run := st.addRun("sub-resolved", types.RunRunning, "sbx")
	e := newSCIMCovEnv(t, st)

	if err := e.srv.suspendIdentity(context.Background(), st, ident.ID, scimSlotNext); err != nil {
		t.Fatal(err)
	}
	if len(st.plans) != 1 || !slices.Equal(st.plans[0].Principals, []string{scimCovObjectForm}) ||
		!slices.Equal(st.plans[0].CutoffSubs, []string{scimCovObjectForm, "pat@corp.example", "sub-resolved"}) {
		t.Errorf("plan = %+v, want the object-id principal deactivated and the cutoff under the resolved sub too", st.plans)
	}
	if !st.tokenRevoked(tok) || st.tokenRevoked(bystander) || st.runState(run) != types.RunKilled {
		t.Errorf("the resolved sub's token revoked %t, bystander revoked %t, run %s", st.tokenRevoked(tok), st.tokenRevoked(bystander), st.runState(run))
	}
	if rows := e.auditRows("scim.user.deactivate"); len(rows) != 1 || e.auditData(rows[0])["slot"] != scimSlotNext {
		t.Errorf("scim.user.deactivate rows = %+v, want one naming the slot the caller passed", rows)
	}
}

func TestSCIMCovLeaverFormsTenantFallback(t *testing.T) {
	st := newSCIMCovStore()
	for _, c := range []struct {
		name    string
		tenant  string
		scimCfg bool
		want    string
	}{
		{"the row's own tenant wins", "row-tenant", true, entraPrincipalPrefix + "row-tenant:" + scimCovOID},
		{"the configured tenant fills an empty one", "", true, scimCovObjectForm},
		{"no configuration leaves it empty", "", false, entraPrincipalPrefix + ":" + scimCovOID},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newSCIMCovEnv(t, st, func(cfg *Config) {
				if !c.scimCfg {
					cfg.SCIM = nil
				}
			})
			forms, err := e.srv.leaverForms(context.Background(), st, store.PrincipalIdentity{ID: uuid.New(), TenantID: c.tenant, ObjectID: scimCovOID})
			if err != nil || !slices.Equal(forms.bound, []string{c.want}) {
				t.Errorf("bound = %v (%v), want [%s]", forms.bound, err, c.want)
			}
		})
	}
}

func TestSCIMCovSuspensionSchedulesThePurgeOnlyWhenConfigured(t *testing.T) {
	for _, c := range []struct {
		name string
		cfg  func(*Config)
		want time.Duration
	}{
		{"a configured delay", func(cfg *Config) { cfg.SCIM.PurgeAfter = 48 * time.Hour }, 48 * time.Hour},
		{"no delay", nil, 0},
		{"no SCIM configuration", func(cfg *Config) { cfg.SCIM = nil }, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			st := newSCIMCovStore()
			ident := st.addIdentity(scimCovBound())
			var shape []func(*Config)
			if c.cfg != nil {
				shape = append(shape, c.cfg)
			}
			e := newSCIMCovEnv(t, st, shape...)
			if err := e.srv.suspendIdentity(context.Background(), st, ident.ID, scimSlotPrimary); err != nil {
				t.Fatal(err)
			}
			if len(st.plans) != 1 || st.plans[0].PurgeAfter != c.want {
				t.Errorf("plans = %+v, want PurgeAfter %s", st.plans, c.want)
			}
			if got := st.identity(ident.ID).PurgeAfter; (got != nil) != (c.want > 0) || (got != nil && !got.Equal(scimCovNow.Add(c.want))) {
				t.Errorf("purge_after = %v, want the deactivation plus %s", got, c.want)
			}
		})
	}
}

func TestSCIMCovAFailedTeardownIsRepairedOnRetry(t *testing.T) {
	st := newSCIMCovStore()
	l := scimCovSeedLeaver(st)
	e := newSCIMCovEnv(t, st)
	e.runner.setFailKills(1)
	ctx := context.Background()

	err := e.srv.suspendIdentity(ctx, st, l.id, scimSlotPrimary)
	if !errors.Is(err, errDeprovisionIncomplete) {
		t.Fatalf("first attempt = %v, want errDeprovisionIncomplete", err)
	}
	if st.runState(l.runSub) != types.RunKilled || st.runState(l.runEmail) != types.RunKilled {
		t.Errorf("runs are %s and %s, want both KILLED", st.runState(l.runSub), st.runState(l.runEmail))
	}
	var failed []store.DeprovisionJob
	for _, j := range st.ledger(l.id, store.JobKindSuspend) {
		if !j.Done && j.Step == jobStepKillRun {
			failed = append(failed, j)
		}
	}
	if len(failed) != 1 || !strings.Contains(failed[0].LastError, "is killed but its teardown failed") || failed[0].Attempts != 1 {
		t.Fatalf("pending ledger rows = %+v, want the run whose teardown failed, with its error and one attempt", failed)
	}
	for _, action := range []string{"scim.user.deactivate", "person.deprovision"} {
		if rows := e.auditRows(action); len(rows) != 0 {
			t.Errorf("%s was written before the suspension finished: %+v", action, rows)
		}
	}

	if err := e.srv.suspendIdentity(ctx, st, l.id, scimSlotPrimary); err != nil {
		t.Fatalf("retry = %v", err)
	}
	if len(st.plans) != 1 {
		t.Errorf("the retry cut the sessions again: %d plans", len(st.plans))
	}
	if got := e.runner.killCount(); got != 3 {
		t.Errorf("KillSandbox ran %d times, want 2 live runs plus one repeat of the failed teardown", got)
	}
	if left := st.pendingSteps(l.id, store.JobKindSuspend); len(left) != 0 {
		t.Errorf("rows still pending after the retry: %v", left)
	}
	if got := e.auditRows("person.deprovision"); len(got) != 1 || e.auditData(got[0])["runs_killed"] != float64(2) {
		t.Errorf("person.deprovision rows = %+v, want one counting both runs", got)
	}
}

func TestSCIMCovAnIncompleteSuspensionAnswers5xxToTheProvider(t *testing.T) {
	st := newSCIMCovStore()
	l := scimCovSeedLeaver(st)
	e := newSCIMCovEnv(t, st)
	e.runner.setFailKills(10)

	w := e.scim(http.MethodPatch, "/scim/v2/Users/"+l.id.String(), scimCovPatch(scimCovReplace("active", "false")))
	scimCovWantRetry(t, w, "unreachable", "sbx-")
	if !strings.Contains(w.Body.String(), "deprovisioning is incomplete; retry the request") {
		t.Errorf("the 500 does not tell the provider to retry: %s", w.Body.String())
	}
	if st.identity(l.id).DeactivatedAt == nil {
		t.Error("the identity must stay deactivated while the rest is retried")
	}
}

func TestSCIMCovAFailedSweepDoesNotStopTheKill(t *testing.T) {
	st := newSCIMCovStore()
	l := scimCovSeedLeaver(st)
	e := newSCIMCovEnv(t, st)
	e.rev.set(errors.New("revocation store unavailable"), nil)
	ctx := context.Background()

	err := e.srv.suspendIdentity(ctx, st, l.id, scimSlotPrimary)
	if !errors.Is(err, errDeprovisionIncomplete) || !strings.Contains(err.Error(), "revocation store unavailable") {
		t.Fatalf("first attempt = %v, want the sweep's error inside errDeprovisionIncomplete", err)
	}
	if st.runState(l.runSub) != types.RunKilled || st.runState(l.runEmail) != types.RunKilled {
		t.Error("a failed sweep stopped the run kill")
	}
	var sweeps int
	for _, j := range st.ledger(l.id, store.JobKindSuspend) {
		if j.Step == jobStepSweep {
			sweeps++
			if j.Done || j.LastError != "revocation store unavailable" {
				t.Errorf("sweep row %s = done %t error %q, want pending with the error", j.Target, j.Done, j.LastError)
			}
		}
	}
	if sweeps != len(scimCovTargets) {
		t.Errorf("sweep rows = %d, want one per form", sweeps)
	}
	if st.tokenRevoked(l.tokSub) {
		t.Error("tokens were swept although the session cutoff could not be written")
	}
	if rows := e.auditRows("person.deprovision"); len(rows) != 0 {
		t.Errorf("person.deprovision written while a sweep is pending: %+v", rows)
	}

	e.rev.set(nil, nil)
	if err := e.srv.suspendIdentity(ctx, st, l.id, scimSlotPrimary); err != nil {
		t.Fatalf("retry = %v", err)
	}
	if !st.tokenRevoked(l.tokSub) || len(st.pendingSteps(l.id, store.JobKindSuspend)) != 0 {
		t.Error("the retry did not finish the sweep")
	}
	if got := e.auditRows("run.kill"); len(got) != 2 {
		t.Errorf("run.kill rows = %d, want the runs killed once, not again on the retry", len(got))
	}
}

func TestSCIMCovAuditRowsAreWrittenOnceEachAfterAFailedWrite(t *testing.T) {
	st := newSCIMCovStore()
	l := scimCovSeedLeaver(st)
	e := newSCIMCovEnv(t, st)
	e.audit.fail("scim.user.deactivate", true)
	ctx := context.Background()

	err := e.srv.suspendIdentity(ctx, st, l.id, scimSlotPrimary)
	if !errors.Is(err, errDeprovisionIncomplete) || !strings.Contains(err.Error(), "audit sink unavailable") {
		t.Fatalf("first attempt = %v", err)
	}
	if len(e.auditRows("scim.user.deactivate")) != 0 || len(e.auditRows("person.deprovision")) != 1 {
		t.Errorf("rows deactivate %d deprovision %d, want the failed one absent and the other written",
			len(e.auditRows("scim.user.deactivate")), len(e.auditRows("person.deprovision")))
	}
	if left := st.pendingSteps(l.id, store.JobKindSuspend); !slices.Equal(left, []string{jobStepAuditDeact + "/"}) {
		t.Errorf("pending = %v, want only the audit row that failed", left)
	}

	e.audit.fail("scim.user.deactivate", false)
	if err := e.srv.suspendIdentity(ctx, st, l.id, scimSlotPrimary); err != nil {
		t.Fatalf("retry = %v", err)
	}
	if len(e.auditRows("scim.user.deactivate")) != 1 || len(e.auditRows("person.deprovision")) != 1 {
		t.Errorf("after the retry deactivate %d deprovision %d, want one each", len(e.auditRows("scim.user.deactivate")), len(e.auditRows("person.deprovision")))
	}
}

func TestSCIMCovSuspensionWithoutAnAuditSinkStillFinishes(t *testing.T) {
	st := newSCIMCovStore()
	ident := st.addIdentity(scimCovBound())
	e := newSCIMCovEnv(t, st, func(cfg *Config) { cfg.Audit = nil })
	if err := e.srv.suspendIdentity(context.Background(), st, ident.ID, scimSlotPrimary); err != nil {
		t.Fatal(err)
	}
	if left := st.pendingSteps(ident.ID, store.JobKindSuspend); len(left) != 0 {
		t.Errorf("pending = %v, want the audit steps finished with no sink to write to", left)
	}
}

func TestSCIMCovSuspensionStopsAtAFailedStoreStep(t *testing.T) {
	for _, c := range []struct {
		name, method string
		wantPlans    int
		wantRevoked  bool
	}{
		{"reading the identity", "GetIdentity", 0, false},
		{"reading the aliases", "IdentityAliasValues", 0, false},
		{"reading the ledger", "ListDeprovisionJobs", 0, false},
		{"the cutoff transaction", "SuspendIdentity", 1, false},
		{"opening the ledger rows", "EnsureDeprovisionJobs", 1, false},
		{"listing the person's runs", "ListNonTerminalRunsBy", 1, true},
		{"recording a finished step", "FinishDeprovisionJob", 1, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			st := newSCIMCovStore()
			l := scimCovSeedLeaver(st)
			e := newSCIMCovEnv(t, st)
			st.failNext(c.method, errSCIMCovBoom, -1)

			err := e.srv.suspendIdentity(context.Background(), st, l.id, scimSlotPrimary)
			if !errors.Is(err, errSCIMCovBoom) {
				t.Fatalf("err = %v, want the store's error", err)
			}
			if len(st.plans) != c.wantPlans {
				t.Errorf("SuspendIdentity ran %d times, want %d", len(st.plans), c.wantPlans)
			}
			if revoked, _ := e.rev.snapshot(); (len(revoked) > 0) != c.wantRevoked {
				t.Errorf("sessions revoked = %v, want revoked %t", revoked, c.wantRevoked)
			}
			for _, action := range []string{"scim.user.deactivate", "person.deprovision"} {
				if rows := e.auditRows(action); len(rows) != 0 {
					t.Errorf("%s was written after a failed step: %+v", action, rows)
				}
			}
		})
	}
}

func TestSCIMCovARunTheLedgerCountedDoneIsKilledAgainWhenLive(t *testing.T) {
	st := newSCIMCovStore()
	row := scimCovBound()
	gone := scimCovNow
	row.DeactivatedAt = &gone
	ident := st.addIdentity(row)
	run := st.addRun("sub-pat", types.RunRunning, "sbx")
	key := store.JobKey{Step: jobStepKillRun, Target: run.String()}
	st.jobs = append(st.jobs,
		&store.DeprovisionJob{IdentityID: ident.ID, Kind: store.JobKindSuspend, Step: store.JobStepCutoff, Done: true, Attempts: 1},
		&store.DeprovisionJob{IdentityID: ident.ID, Kind: store.JobKindSuspend, Step: key.Step, Target: key.Target, Done: true, Attempts: 1})
	e := newSCIMCovEnv(t, st)

	if err := e.srv.suspendIdentity(context.Background(), st, ident.ID, scimSlotPrimary); err != nil {
		t.Fatal(err)
	}
	if st.runState(run) != types.RunKilled || e.runner.killCount() != 1 || len(st.plans) != 0 {
		t.Errorf("run %s, kills %d, plans %d, want the live run killed without cutting the sessions again", st.runState(run), e.runner.killCount(), len(st.plans))
	}
	if st.callCount("ReopenDeprovisionJob") != 1 {
		t.Errorf("ReopenDeprovisionJob ran %d times, want once", st.callCount("ReopenDeprovisionJob"))
	}

	// A reopen that fails keeps the suspension incomplete, and the run is left as it was.
	st2 := newSCIMCovStore()
	ident2 := st2.addIdentity(row)
	run2 := st2.addRun("sub-pat", types.RunRunning, "sbx")
	st2.jobs = append(st2.jobs,
		&store.DeprovisionJob{IdentityID: ident2.ID, Kind: store.JobKindSuspend, Step: store.JobStepCutoff, Done: true},
		&store.DeprovisionJob{IdentityID: ident2.ID, Kind: store.JobKindSuspend, Step: jobStepKillRun, Target: run2.String(), Done: true})
	st2.failNext("ReopenDeprovisionJob", errSCIMCovBoom, -1)
	e2 := newSCIMCovEnv(t, st2)
	if err := e2.srv.suspendIdentity(context.Background(), st2, ident2.ID, scimSlotPrimary); !errors.Is(err, errSCIMCovBoom) {
		t.Errorf("err = %v, want the reopen's error", err)
	}
	if st2.runState(run2) != types.RunRunning {
		t.Errorf("run = %s, want it untouched while the ledger could not be reopened", st2.runState(run2))
	}
}

func TestSCIMCovTeardownOfARunThatEndedItselfIsDone(t *testing.T) {
	st := newSCIMCovStore()
	row := scimCovBound()
	gone := scimCovNow
	row.DeactivatedAt = &gone
	ident := st.addIdentity(row)
	run := st.addRun("sub-pat", types.RunCompleted, "sbx")
	st.jobs = append(st.jobs,
		&store.DeprovisionJob{IdentityID: ident.ID, Kind: store.JobKindSuspend, Step: store.JobStepCutoff, Done: true},
		&store.DeprovisionJob{IdentityID: ident.ID, Kind: store.JobKindSuspend, Step: jobStepKillRun, Target: run.String()})
	e := newSCIMCovEnv(t, st)

	if err := e.srv.suspendIdentity(context.Background(), st, ident.ID, scimSlotPrimary); err != nil {
		t.Fatal(err)
	}
	if e.runner.killCount() != 0 || st.runState(run) != types.RunCompleted {
		t.Errorf("a run that ended on its own was killed: state %s kills %d", st.runState(run), e.runner.killCount())
	}
	if j := st.ledger(ident.ID, store.JobKindSuspend); !slices.ContainsFunc(j, func(j store.DeprovisionJob) bool { return j.Step == jobStepKillRun && j.Done }) {
		t.Errorf("ledger = %+v, want the run's row done", j)
	}
}

func TestSCIMCovTeardownRefusals(t *testing.T) {
	for _, c := range []struct {
		name      string
		arm       func(*scimCovStore)
		wantErr   string
		wantState types.RunState
		wantCAS   int
	}{
		{"a run that cannot be read", func(s *scimCovStore) { s.failNext("GetRun", errSCIMCovBoom, -1) }, "read run", types.RunRunning, 0},
		{"a kill the store refuses", func(s *scimCovStore) { s.failNext("UpdateRunStateIf", errSCIMCovBoom, -1) }, "kill run", types.RunRunning, 1},
		{"a run that keeps changing state", func(s *scimCovStore) { s.casLost = 10 }, "kept changing state under the kill", types.RunRunning, teardownAttempts},
	} {
		t.Run(c.name, func(t *testing.T) {
			st := newSCIMCovStore()
			run := st.addRun("sub-pat", types.RunRunning, "sbx")
			e := newSCIMCovEnv(t, st)
			c.arm(st)
			err := e.srv.teardownRun(context.Background(), st, run)
			if err == nil || !strings.Contains(err.Error(), c.wantErr) || !strings.Contains(err.Error(), run.String()) {
				t.Fatalf("err = %v, want %q naming the run", err, c.wantErr)
			}
			if c.wantErr != "kept changing state under the kill" && !errors.Is(err, errSCIMCovBoom) {
				t.Errorf("err = %v, want the store's error wrapped", err)
			}
			if st.runState(run) != c.wantState || e.runner.killCount() != 0 {
				t.Errorf("run %s, kills %d, want the run untouched", st.runState(run), e.runner.killCount())
			}
			if got := st.callCount("UpdateRunStateIf"); got != c.wantCAS {
				t.Errorf("UpdateRunStateIf ran %d times, want %d", got, c.wantCAS)
			}
		})
	}

	// One lost compare-and-swap is read as "someone moved the run": it is re-read and killed on the next try.
	st := newSCIMCovStore()
	run := st.addRun("sub-pat", types.RunRunning, "sbx")
	e := newSCIMCovEnv(t, st)
	st.casLost = 1
	if err := e.srv.teardownRun(context.Background(), st, run); err != nil {
		t.Fatal(err)
	}
	if st.runState(run) != types.RunKilled || st.callCount("GetRun") < 2 || e.runner.killCount() != 1 {
		t.Errorf("state %s, GetRun %d, kills %d, want one re-read and one kill", st.runState(run), st.callCount("GetRun"), e.runner.killCount())
	}
}

func TestSCIMCovLedgerHelpers(t *testing.T) {
	if got := nonEmptyForms(" a ", "", "b", "a", "  "); !slices.Equal(got, []string{"a", "b"}) {
		t.Errorf("nonEmptyForms = %v, want the trimmed distinct non-empty forms", got)
	}
	jobs := []store.DeprovisionJob{{Step: "sweep", Target: "a"}, {Step: "sweep", Target: "b", Done: true}}
	if j, ok := jobByKey(jobs, "sweep", "b"); !ok || !j.Done {
		t.Errorf("jobByKey(sweep, b) = %+v %t", j, ok)
	}
	if _, ok := jobByKey(jobs, "kill_run", "b"); ok {
		t.Error("jobByKey matched another step")
	}
	if err := (&Server{}).recordAuditStrict(context.Background(), types.AuditEvent{}); err != nil {
		t.Errorf("no sink must be no error, got %v", err)
	}
}

// The ledger is read before every step, and each read that fails stops that step alone: the others still run,
// and the audit rows wait for all of them.
func TestSCIMCovALedgerReadFailureStopsOnlyItsStep(t *testing.T) {
	for _, c := range []struct {
		name       string
		skip       int
		wantSweeps bool
		wantKilled bool
	}{
		{"the sweep's read", 1, false, true},
		{"the kill's first read", 2, true, false},
		{"the kill's read after opening its rows", 3, true, false},
		{"the audit step's read", 4, true, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			st := newSCIMCovStore()
			l := scimCovSeedLeaver(st)
			e := newSCIMCovEnv(t, st)
			st.failAfter("ListDeprovisionJobs:suspend", errSCIMCovBoom, c.skip)

			err := e.srv.suspendIdentity(context.Background(), st, l.id, scimSlotPrimary)
			if !errors.Is(err, errDeprovisionIncomplete) || !errors.Is(err, errSCIMCovBoom) {
				t.Fatalf("err = %v, want errDeprovisionIncomplete wrapping the read's error", err)
			}
			if got := st.tokenRevoked(l.tokSub); got != c.wantSweeps {
				t.Errorf("tokens swept = %t, want %t", got, c.wantSweeps)
			}
			if got := st.runState(l.runSub) == types.RunKilled; got != c.wantKilled {
				t.Errorf("run killed = %t, want %t", got, c.wantKilled)
			}
			for _, action := range []string{"scim.user.deactivate", "person.deprovision"} {
				if len(e.auditRows(action)) != 0 {
					t.Errorf("%s written although a step was unfinished or its ledger could not be read", action)
				}
			}

			if err := e.srv.suspendIdentity(context.Background(), st, l.id, scimSlotPrimary); err != nil {
				t.Fatalf("retry = %v", err)
			}
			if len(st.pendingSteps(l.id, store.JobKindSuspend)) != 0 || len(e.auditRows("person.deprovision")) != 1 {
				t.Errorf("after the retry: pending %v, deprovision rows %d", st.pendingSteps(l.id, store.JobKindSuspend), len(e.auditRows("person.deprovision")))
			}
		})
	}
}
