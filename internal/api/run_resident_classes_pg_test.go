// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The per-class pins of the runner_resident lift (design 0.9 §7.4, RR §7): each
// of env_secret, file_secret, ssh_key and git_pat_helper is authored where its
// value is read, classified, delivered, refused, erased and abandoned through
// the same org path, one subtest per class. Guarded by WARDYN_TEST_PG.

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/substrate"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/kek"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const residentAllClassesPolicy = `{"classes":{"env_secret":{"mode":"runner_resident"},"file_secret":{"mode":"runner_resident"},` +
	`"ssh_key":{"mode":"runner_resident"},"git_pat_helper":{"mode":"runner_resident"}}}`

// residentClassCase is one lifted class: its grant kind, whether its value may
// enter the sandbox, its scope over a secret name, and the sandbox map dispatch
// writes for it (ssh_key and git_pat only).
type residentClassCase struct {
	class   string
	kind    types.GrantKind
	sandbox bool
	scope   func(secret string) string
	host    string
}

var residentClassCases = []residentClassCase{
	{placement.ClassEnvSecret, types.GrantEnvSecret, true, func(s string) string { return `{"name":"RES_TOKEN","secret_name":"` + s + `"}` }, ""},
	{placement.ClassFileSecret, types.GrantFileSecret, true, func(s string) string { return `{"file":"res.key","secret_name":"` + s + `"}` }, ""},
	{placement.ClassSSHKey, types.GrantSSHKey, false, func(s string) string { return `{"host":"github.com","key_secret_ref":"` + s + `"}` }, "github.com"},
	{placement.ClassGitPATHelper, types.GrantGitPAT, false, func(s string) string { return `{"host":"git.example.com","secret_name":"` + s + `"}` }, "git.example.com"},
}

// classGrant stores a runner_resident grant of c for run, reading secret.
func (f *residentFix) classGrant(t *testing.T, run types.AgentRun, c residentClassCase, secret string) types.CredentialGrant {
	t.Helper()
	g, err := f.st.CreateGrant(t.Context(), types.CredentialGrant{ID: uuid.New(), RunID: run.ID, Delivery: types.GrantDeliveryResident,
		Spec: types.GrantSpec{Kind: c.kind, Scope: json.RawMessage(c.scope(secret))}})
	if err != nil {
		t.Fatal(err)
	}
	return g
}

// reportManagedFiles has the runner report that it delivers managed files, as
// a file_secret dispatch requires.
func (f *residentFix) reportManagedFiles(t *testing.T) {
	t.Helper()
	data, err := json.Marshal(runnerwire.Caps{Support: substrate.ClassSupport{Classes: []types.ConfinementClass{types.CC2}, ManagedFiles: true}, Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	f.srv.runnerEndpoint(f.rn.ID).sub.HandleEvent(runnerwire.Event{Kind: runnerwire.EventCaps, Data: data})
}

// authorClass runs c's dispatch author over run's stored grants: the spec it
// writes (the policy naming every grant), the values it returns, and false
// when it failed the run.
func (f *residentFix) authorClass(t *testing.T, run types.AgentRun, c residentClassCase, patBroker bool) (runner.SandboxSpec, []residentValue, bool) {
	t.Helper()
	ctx := t.Context()
	rows, err := f.st.ListGrantsByRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	spec := runner.SandboxSpec{RunID: run.ID, ProxyConfig: runner.ProxyConfig{Policy: policyOf(rows...)}}
	env := map[string]string{}
	var values []residentValue
	ok := true
	switch c.kind {
	case types.GrantEnvSecret:
		_, values = f.srv.resolveEnvSecretValues(ctx, run, spec.ProxyConfig.Policy, rows, env)
		spec.SecretEnv = env
	case types.GrantFileSecret:
		spec.ManagedFiles, values, ok = f.srv.resolveFileSecretValues(ctx, run, spec.ProxyConfig.Policy, rows)
	default:
		m := map[string]string{}
		for _, r := range rows {
			m[c.host] = r.ID.String()
		}
		b, _ := json.Marshal(m)
		key := "WARDYN_SSH_GRANTS"
		if c.kind == types.GrantGitPAT {
			key = "WARDYN_GIT_PAT_GRANTS"
		}
		spec.Env = map[string]string{key: string(b)}
		ok = f.srv.authorResidentMints(ctx, run, rows, patBroker, &spec, &values)
	}
	return spec, values, ok
}

// placeholderOf is where c's authored spec carries grant: the placeholder at
// the class's one place, or "" when it is absent.
func placeholderOf(spec runner.SandboxSpec, c residentClassCase, grant uuid.UUID) string {
	switch c.kind {
	case types.GrantEnvSecret:
		return spec.SecretEnv["RES_TOKEN"]
	case types.GrantFileSecret:
		for _, mf := range spec.ManagedFiles {
			if mf.Path == runner.ComponentSecretDir+"/res.key" {
				return string(mf.Content)
			}
		}
	default:
		for _, m := range spec.ProxyConfig.ResidentMints {
			if m.GrantID == grant && m.Kind == c.kind && !m.ExpiresAt.IsZero() {
				return m.Token
			}
		}
	}
	return ""
}

// classify is the dispatch classification of an authored spec, as
// classifyLocalDispatch runs it: nil when the run may go to its runner.
func (f *residentFix) classify(t *testing.T, run types.AgentRun, spec runner.SandboxSpec) *placement.Refusal {
	t.Helper()
	sc, err := f.st.GetSiteConfig(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	p, err := f.srv.localResolvedPlan(t.Context(), run, false, sc, spec, nil, llmTransport{})
	if err != nil {
		t.Fatal(err)
	}
	_, r := placement.LocalEligibility(p)
	return r
}

// Grant ownership, masking before use, the lift, run/runner binding and the
// expiry, per class: the value is read from the operator's namespace only and
// is on the run's masking manifest before dispatch completes; the spec carries
// only its placeholder; the dispatch classification admits it; and it reaches
// the run's runner once, for this run, with the not_after the obligation keeps.
func TestResidentClassPins_AuthorClassifyDeliver(t *testing.T) {
	for _, c := range residentClassCases {
		t.Run(c.class, func(t *testing.T) {
			f := newResidentFix(t, nil, residentAllClassesPolicy)
			f.srv.cfg.DisableGitPATBroker = true
			f.reportManagedFiles(t)
			ctx := t.Context()
			sec := f.cfg.Secrets.(*memSecrets)
			val := "resident " + c.class + " value\n"
			if err := sec.Put(ctx, "op-secret", []byte(val)); err != nil {
				t.Fatal(err)
			}
			run := f.startingRun(t, func(r *types.AgentRun) { lifetime := f.clock().Add(2 * time.Hour); r.LifetimeCapAt = &lifetime })
			g := f.classGrant(t, run, c, "op-secret")

			spec, values, ok := f.authorClass(t, run, c, false)
			if !ok || len(values) != 1 || values[0].grant.ID != g.ID || string(values[0].value) != val || values[0].class != c.class {
				t.Fatalf("authored ok=%v values=%d, want the one operator value of class %s", ok, len(values), c.class)
			}
			if got := placeholderOf(spec, c, g.ID); got != placement.ResidentPlaceholder(g.ID) {
				t.Fatalf("the spec carries %q where the placeholder belongs", got)
			}
			if b, _ := json.Marshal(spec); bytes.Contains(b, []byte(strings.TrimSpace(val))) {
				t.Fatalf("the spec carries the value: %s", b)
			}
			for _, mf := range spec.ManagedFiles {
				if bytes.Contains(mf.Content, []byte(strings.TrimSpace(val))) {
					t.Fatalf("managed file %s carries the value", mf.Path)
				}
			}
			masker := f.cfg.MaskRegistry.Masker(run.ID)
			renderings := []string{val}
			if c.kind != types.GrantEnvSecret {
				renderings = append(renderings, strings.TrimRight(val, "\n"))
			}
			for _, r := range renderings {
				if bytes.Contains(masker.Mask([]byte("leak "+r+" end")), []byte(strings.TrimSpace(r))) {
					t.Fatalf("rendering %q is not on the masking manifest before create", r)
				}
			}
			read := false
			for _, ev := range syncAuditRows(f.audit, "secret.read") {
				read = read || (ev.Outcome == "success" && strings.Contains(string(ev.Data), string(secretstore.PurposeRunnerResident)) && strings.Contains(string(ev.Data), g.ID.String()))
			}
			if !read {
				t.Fatal("no runner-resident secret.read row for the grant")
			}

			if r := f.classify(t, run, spec); r != nil {
				t.Fatalf("the dispatch classification refused a lifted %s: %+v", c.class, r)
			}

			if !f.srv.deliverResident(ctx, run, spec.ProxyConfig.Policy, values) {
				t.Fatal("delivery refused")
			}
			delivered, _ := f.side.calls()
			wantNotAfter := residentNotAfter(run, f.clock())
			if len(delivered) != 1 || delivered[0].RunID != run.ID || delivered[0].GrantID != g.ID || delivered[0].Kind != string(c.kind) ||
				delivered[0].Sandbox != c.sandbox || delivered[0].Value != val || !delivered[0].NotAfter.Equal(wantNotAfter) {
				t.Fatalf("delivered %+v, want one value of %s for this run, sandbox=%v, not_after %v", delivered, c.class, c.sandbox, wantNotAfter)
			}
			exists, _, _, _, notAfter := f.obligation(t, run.ID)
			var boundRunner uuid.UUID
			if err := f.pool.QueryRow(ctx, `SELECT runner_id FROM runner_resident_runs WHERE run_id = $1`, run.ID).Scan(&boundRunner); err != nil || !exists ||
				boundRunner != f.rn.ID || !notAfter.Equal(wantNotAfter) {
				t.Fatalf("obligation exists=%v runner=%v not_after=%v (%v), want bound to %v until %v", exists, boundRunner, notAfter, err, f.rn.ID, wantNotAfter)
			}
			for _, m := range spec.ProxyConfig.ResidentMints {
				if m.ExpiresAt.IsZero() || m.ExpiresAt.After(notAfter) {
					t.Fatalf("resident mint expires %v, want set and no later than not_after %v", m.ExpiresAt, notAfter)
				}
			}

			// No owner fallback: a secret only in the owner's namespace is not read.
			other := f.startingRun(t, nil)
			if err := sec.For(other.CreatedBy).(*memSecrets).Put(ctx, "owner-only", []byte("owner value")); err != nil {
				t.Fatal(err)
			}
			g2 := f.classGrant(t, other, c, "owner-only")
			if spec, values, _ := f.authorClass(t, other, c, false); len(values) != 0 || placeholderOf(spec, c, g2.ID) != "" {
				t.Fatalf("an owner-namespace secret was authored as %s: %d values", c.class, len(values))
			}
		})
	}
}

// Current policy and posture at delivery, failed delivery, revoke and offline,
// and retryable erase reporting, per class.
func TestResidentClassPins_RefuseEraseRevoke(t *testing.T) {
	for _, c := range residentClassCases {
		t.Run(c.class, func(t *testing.T) {
			f := newResidentFix(t, nil, residentAllClassesPolicy)
			f.srv.cfg.DisableGitPATBroker = true
			f.reportManagedFiles(t)
			ctx := t.Context()
			if err := f.cfg.Secrets.(*memSecrets).Put(ctx, "op-secret", []byte("held value")); err != nil {
				t.Fatal(err)
			}
			authored := func() (types.AgentRun, types.CredentialGrant, runner.SandboxSpec, []residentValue) {
				t.Helper()
				run := f.startingRun(t, nil)
				g := f.classGrant(t, run, c, "op-secret")
				spec, values, ok := f.authorClass(t, run, c, false)
				if !ok || len(values) != 1 {
					t.Fatalf("authored ok=%v values=%d", ok, len(values))
				}
				return run, g, spec, values
			}
			refusedWith := func(run types.AgentRun, g types.CredentialGrant, reason placement.Reason) {
				t.Helper()
				if got := f.dispatchRefusal(t, run.ID); got["reason"] != string(reason) || got["grant_id"] != g.ID.String() {
					t.Fatalf("refusal %v, want %s for grant %s", got, reason, g.ID)
				}
				if st := f.runState(t, run.ID); st != types.RunFailed {
					t.Fatalf("run is %s, want FAILED", st)
				}
			}
			setPolicy := func(raw string) {
				t.Helper()
				if _, err := f.st.PutCredentialDeliveryPolicy(ctx, json.RawMessage(raw), "seed", time.Now()); err != nil {
					t.Fatal(err)
				}
			}

			// The class no longer delivered runner_resident by the time of delivery.
			run, g, spec, values := authored()
			setPolicy(`{"classes":{"` + c.class + `":{"mode":"refuse"}}}`)
			if f.srv.deliverResident(ctx, run, spec.ProxyConfig.Policy, values) {
				t.Fatal("delivered under a policy that refuses the class")
			}
			refusedWith(run, g, placement.ReasonPlacementCredential)
			// A posture requirement the runner does not report.
			setPolicy(`{"classes":{"` + c.class + `":{"mode":"runner_resident","require_posture":{"disk_encrypted":true}}}}`)
			run, g, spec, values = authored()
			if f.srv.deliverResident(ctx, run, spec.ProxyConfig.Policy, values) {
				t.Fatal("delivered to a runner whose posture misses the requirement")
			}
			refusedWith(run, g, placement.ReasonRunnerPostureUnmet)
			setPolicy(residentAllClassesPolicy)

			// A delivery the runner refuses fails the run before any create.
			f.side.mu.Lock()
			f.side.deliver = func(runnerwire.DeliverResidentArgs) error { return runnerwire.Refuse("keystore write failed") }
			f.side.mu.Unlock()
			run, g, spec, values = authored()
			if f.srv.deliverResident(ctx, run, spec.ProxyConfig.Policy, values) {
				t.Fatal("a refused delivery reported success")
			}
			refusedWith(run, g, placement.ReasonPlacementCredential)
			f.side.mu.Lock()
			f.side.deliver = nil
			f.side.mu.Unlock()
			f.srv.WaitBackground()

			// Offline waits; a failed erase is retried after its backoff; success closes it.
			run, _, spec, values = authored()
			if !f.srv.deliverResident(ctx, run, spec.ProxyConfig.Policy, values) {
				t.Fatal("delivery refused")
			}
			if ok, err := f.st.UpdateRunStateIf(ctx, run.ID, types.RunStarting, types.RunCompleted); err != nil || !ok {
				t.Fatalf("end run: %v %v", ok, err)
			}
			f.online.Store(false)
			if err := f.srv.sweepResidentErase(ctx); err != nil {
				t.Fatal(err)
			}
			if _, attempts, erased, _, _ := f.obligation(t, run.ID); attempts != 0 || erased != nil {
				t.Fatalf("offline: attempts %d erased %v, want untouched", attempts, erased)
			}
			f.online.Store(true)
			f.side.mu.Lock()
			f.side.eraseErr = runnerwire.Refuse("resident erase incomplete")
			f.side.mu.Unlock()
			if err := f.srv.sweepResidentErase(ctx); err == nil {
				t.Fatal("a failed erase was not reported")
			}
			if _, attempts, erased, _, _ := f.obligation(t, run.ID); attempts != 1 || erased != nil {
				t.Fatalf("after a failed erase: attempts %d erased %v, want one attempt, still open", attempts, erased)
			}
			f.side.mu.Lock()
			f.side.eraseErr = nil
			f.side.mu.Unlock()
			f.advance(31 * time.Second)
			if err := f.srv.sweepResidentErase(ctx); err != nil {
				t.Fatal(err)
			}
			if _, attempts, erased, _, _ := f.obligation(t, run.ID); attempts != 2 || erased == nil {
				t.Fatalf("after the backoff: attempts %d erased %v, want closed on the retry", attempts, erased)
			}

			// A revoked runner's obligation is closed unconfirmed.
			run, _, spec, values = authored()
			if !f.srv.deliverResident(ctx, run, spec.ProxyConfig.Policy, values) {
				t.Fatal("delivery refused")
			}
			if ok, err := f.st.UpdateRunStateIf(ctx, run.ID, types.RunStarting, types.RunCompleted); err != nil || !ok {
				t.Fatalf("end run: %v %v", ok, err)
			}
			if _, err := f.st.RevokeRunner(ctx, f.rn.ID, time.Now(), "admin", ""); err != nil {
				t.Fatal(err)
			}
			if err := f.srv.sweepResidentErase(ctx); err != nil {
				t.Fatal(err)
			}
			if _, _, _, abandoned, _ := f.obligation(t, run.ID); abandoned == nil || *abandoned != store.ResidentAbandonRunnerRevoked {
				t.Fatalf("abandoned %v, want runner_revoked", abandoned)
			}
		})
	}
}

// Every class, through a whole dispatch: the run passes the classification,
// its value reaches the runner before the create, and no run.dispatch refusal
// is written. Before the lift the classification refused each of them.
func TestDispatch_ResidentClassesLiftedThroughDispatch(t *testing.T) {
	for _, c := range residentClassCases {
		t.Run(c.class, func(t *testing.T) {
			f := newResidentFix(t, nil, residentAllClassesPolicy)
			f.srv.cfg.RunConfigKey = make([]byte, kek.DEKSize)
			f.srv.cfg.DisableGitPATBroker = true
			f.reportManagedFiles(t)
			ctx := t.Context()
			if err := f.cfg.Secrets.(*memSecrets).Put(ctx, "op-secret", []byte("dispatched value")); err != nil {
				t.Fatal(err)
			}
			run := f.startingRun(t, func(r *types.AgentRun) { r.State = types.RunPending })
			g := f.classGrant(t, run, c, "op-secret")
			p := dispatchParams{RunToken: "run-token", Image: "wardyn/claude-code:latest", Policy: policyOf(g)}
			switch c.kind {
			case types.GrantSSHKey:
				p.SSHGrants = map[string]string{c.host: g.ID.String()}
			case types.GrantGitPAT:
				p.GitPATGrants = map[string]string{c.host: g.ID.String()}
			}
			f.srv.dispatchRun(ctx, run, ceilingForDispatch(governanceCeiling{}, adoEntraUngraded(), bedrockCredUngraded()), p)
			f.srv.WaitBackground()
			for _, ev := range syncAuditRows(f.audit, "run.dispatch") {
				if ev.RunID != nil && *ev.RunID == run.ID && ev.Outcome == "failure" {
					t.Fatalf("dispatch refused a lifted %s: %s", c.class, ev.Data)
				}
			}
			delivered, _ := f.side.calls()
			if len(delivered) != 1 || delivered[0].GrantID != g.ID || delivered[0].Value != "dispatched value" {
				t.Fatalf("delivered %+v, want the grant's value before the create", delivered)
			}
		})
	}
}

// The git_pat lane with the PAT broker, at each door: create admits a resident
// git_pat only with the broker off; a dispatch with it on refuses the run and
// delivers nothing; a revive with it on is refused.
func TestResidentGitPAT_FlagMatrix(t *testing.T) {
	c := residentClassCases[3]
	f := newResidentFix(t, nil, residentAllClassesPolicy)
	ctx := t.Context()
	if err := f.cfg.Secrets.(*memSecrets).Put(ctx, "op-pat", []byte("pat value")); err != nil {
		t.Fatal(err)
	}
	policy := localJudgement{policy: storedPolicy(t, f.srv), trust: types.RunnerTrustPersonal}
	grant := types.GrantSpec{Kind: types.GrantGitPAT, Scope: json.RawMessage(c.scope("op-pat"))}
	create := func(broker bool) ([]localGrantDelivery, *runRefusal) {
		f.srv.cfg.DisableGitPATBroker = !broker
		_, d, ref := f.srv.ownLocalGrantSpecs(localMemberRequest("bob"), types.RunPolicySpec{EligibleGrants: []types.GrantSpec{grant}}, policy)
		return d, ref
	}
	if d, ref := create(false); ref != nil || len(d) != 1 || d[0].mode != placement.ModeRunnerResident || d[0].class != placement.ClassGitPATHelper {
		t.Fatalf("create, broker off: %+v %+v, want admitted runner_resident", d, ref)
	}
	if _, ref := create(true); ref == nil || ref.body.Reason != string(placement.ReasonPlacementCredential) || !strings.Contains(ref.body.Error, "WARDYN_GIT_PAT_BROKER") {
		t.Fatalf("create, broker on: %+v, want placement_credential naming the broker", ref)
	}
	approval := grant
	approval.RequiresApproval = true
	f.srv.cfg.DisableGitPATBroker = true
	if _, _, ref := f.srv.ownLocalGrantSpecs(localMemberRequest("bob"), types.RunPolicySpec{EligibleGrants: []types.GrantSpec{approval}}, policy); ref == nil {
		t.Fatal("a grant that requires approval was admitted runner_resident")
	}
	ado := types.GrantSpec{Kind: types.GrantGitPAT, Scope: json.RawMessage(`{"host":"dev.azure.com","secret_name":"op-pat"}`)}
	if _, _, ref := f.srv.ownLocalGrantSpecs(localMemberRequest("bob"), types.RunPolicySpec{EligibleGrants: []types.GrantSpec{ado}}, policy); ref == nil || !strings.Contains(ref.body.Error, residentADOGitPATRefused) {
		t.Fatalf("an Azure DevOps git token was admitted runner_resident: %+v", ref)
	}

	// Off at create, on at dispatch.
	run := f.startingRun(t, nil)
	g := f.classGrant(t, run, c, "op-pat")
	if _, values, ok := f.authorClass(t, run, c, true); ok || len(values) != 0 {
		t.Fatalf("dispatch with the broker on: ok=%v values=%d, want the run refused with nothing read", ok, len(values))
	}
	if got := f.dispatchRefusal(t, run.ID); got["reason"] != string(placement.ReasonPlacementCredential) || got["grant_id"] != g.ID.String() {
		t.Fatalf("refusal %v", got)
	}
	if delivered, _ := f.side.calls(); len(delivered) != 0 {
		t.Fatalf("delivered %+v", delivered)
	}

	// Off through dispatch, on at revive.
	run = f.startingRun(t, nil)
	f.classGrant(t, run, c, "op-pat")
	spec, _, ok := f.authorClass(t, run, c, false)
	if !ok || len(spec.ProxyConfig.ResidentMints) != 1 {
		t.Fatalf("dispatch with the broker off authored %+v", spec.ProxyConfig.ResidentMints)
	}
	cfg := &proxy.Config{RunID: run.ID, Policy: spec.ProxyConfig.Policy, ResidentMints: spec.ProxyConfig.ResidentMints}
	sc, _ := f.st.GetSiteConfig(ctx)
	f.srv.cfg.DisableGitPATBroker = false
	var ref *placement.Refusal
	if err := f.srv.refreshLocalDeploymentConfig(ctx, run, sc, cfg); !errors.As(err, &ref) || ref.Reason != placement.ReasonPlacementCredential || ref.Detail != residentPATBrokered {
		t.Fatalf("revive with the broker on: %v, want the brokered refusal", err)
	}
	f.srv.cfg.DisableGitPATBroker = true
	if err := f.srv.refreshLocalDeploymentConfig(ctx, run, sc, cfg); errors.As(err, &ref) && ref.Detail == residentPATBrokered {
		t.Fatalf("revive with the broker off was refused as brokered: %v", err)
	}

	// One run through every door with the broker off: create admits, persist
	// records the grant runner_resident, dispatch authors and delivers its mint,
	// and the revive of that stored config is admitted.
	run = f.startingRun(t, nil)
	rec := httptest.NewRecorder()
	gw, ok := f.srv.persistRunGrants(ctx, rec, localMemberRequest("bob"), run.ID, time.Now(), types.RunPolicySpec{EligibleGrants: []types.GrantSpec{grant}}, types.PlacementLocal, policy)
	if !ok || len(gw.delivery) != 1 || gw.delivery[0].Mode != placement.ModeRunnerResident {
		t.Fatalf("persist: ok=%v %+v (%s), want one runner_resident grant", ok, gw.delivery, rec.Body.String())
	}
	rows, err := f.st.ListGrantsByRun(ctx, run.ID)
	if err != nil || len(rows) != 1 || rows[0].Delivery != types.GrantDeliveryResident || rows[0].ID != gw.delivery[0].GrantID {
		t.Fatalf("stored grants %+v (%v), want the one runner_resident grant persist recorded", rows, err)
	}
	spec, values, ok := f.authorClass(t, run, c, false)
	if !ok || placeholderOf(spec, c, rows[0].ID) != placement.ResidentPlaceholder(rows[0].ID) || len(values) != 1 {
		t.Fatalf("dispatch authored %+v with %d values", spec.ProxyConfig.ResidentMints, len(values))
	}
	if r := f.classify(t, run, spec); r != nil {
		t.Fatalf("dispatch classify of the persisted run: %v", r)
	}
	if !f.srv.deliverResident(ctx, run, spec.ProxyConfig.Policy, values) {
		t.Fatal("delivery of the persisted run's git token refused")
	}
	cfg = &proxy.Config{RunID: run.ID, Policy: spec.ProxyConfig.Policy, ResidentMints: spec.ProxyConfig.ResidentMints}
	if err := f.srv.refreshLocalDeploymentConfig(ctx, run, sc, cfg); err != nil {
		t.Fatalf("revive of the persisted run: %v", err)
	}
}

// A resident ssh_key or git_pat is authored only when dispatch's post-drop
// sandbox map names it: a grant withheld for a forge the GitHub App brokers
// gets no resident answer and no value is read.
func TestResidentMints_OnlyPostDropGrants(t *testing.T) {
	for _, c := range residentClassCases[2:] {
		t.Run(c.class, func(t *testing.T) {
			f := newResidentFix(t, nil, residentAllClassesPolicy)
			ctx := t.Context()
			if err := f.cfg.Secrets.(*memSecrets).Put(ctx, "op-secret", []byte("withheld value")); err != nil {
				t.Fatal(err)
			}
			run := f.startingRun(t, nil)
			f.classGrant(t, run, c, "op-secret")
			rows, err := f.st.ListGrantsByRun(ctx, run.ID)
			if err != nil {
				t.Fatal(err)
			}
			spec := runner.SandboxSpec{RunID: run.ID}
			var values []residentValue
			if !f.srv.authorResidentMints(ctx, run, rows, false, &spec, &values) {
				t.Fatal("the run was refused")
			}
			if len(spec.ProxyConfig.ResidentMints) != 0 || len(values) != 0 {
				t.Fatalf("a withheld grant was authored: %+v, %d values", spec.ProxyConfig.ResidentMints, len(values))
			}
		})
	}
}

// A resident grant of another run is never read for this run's runner, and a
// git token for an Azure DevOps host is refused at delivery even when it is
// recorded runner_resident (the site config of that moment decides).
func TestResident_ForeignRunAndAzureDevOpsGitTokenRefused(t *testing.T) {
	f := newResidentFix(t, nil, residentAllClassesPolicy)
	f.srv.cfg.DisableGitPATBroker = true
	ctx := t.Context()
	if err := f.cfg.Secrets.(*memSecrets).Put(ctx, "op-secret", []byte("foreign value")); err != nil {
		t.Fatal(err)
	}
	runA, runB := f.startingRun(t, nil), f.startingRun(t, nil)
	gB := f.classGrant(t, runB, residentClassCases[0], "op-secret")
	if v, err := f.srv.readResidentValue(ctx, runA, gB, "op-secret"); err == nil {
		t.Fatalf("a grant of run B was read for run A: %+v", v.grant.ID)
	}
	run := f.startingRun(t, nil)
	ado := f.classGrant(t, run, residentClassCase{kind: types.GrantGitPAT, scope: func(s string) string { return `{"host":"dev.azure.com","secret_name":"` + s + `"}` }}, "op-secret")
	if f.srv.deliverResident(ctx, run, policyOf(ado), []residentValue{{grant: ado, class: placement.ClassGitPATHelper, value: []byte("ado value")}}) {
		t.Fatal("an Azure DevOps git token was delivered runner_resident")
	}
	if got := f.dispatchRefusal(t, run.ID); got["reason"] != string(placement.ReasonPlacementCredential) || got["grant_id"] != ado.ID.String() {
		t.Fatalf("refusal = %v, want placement_credential naming the grant", got)
	}
	if delivered, _ := f.side.calls(); len(delivered) != 0 {
		t.Fatalf("delivered %+v, want nothing", delivered)
	}
}
