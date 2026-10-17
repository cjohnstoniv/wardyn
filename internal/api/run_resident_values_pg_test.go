// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/kek"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// envGrant stores one env_secret grant of run, delivered as given, and its
// operator-namespace secret.
func (f *residentFix) envGrant(t *testing.T, run types.AgentRun, name, secret, val string, delivery types.GrantDelivery) types.CredentialGrant {
	t.Helper()
	if err := f.cfg.Secrets.(*memSecrets).Put(t.Context(), secret, []byte(val)); err != nil {
		t.Fatal(err)
	}
	g, err := f.st.CreateGrant(t.Context(), types.CredentialGrant{ID: uuid.New(), RunID: run.ID, Delivery: delivery,
		Spec: types.GrantSpec{Kind: types.GrantEnvSecret, Scope: json.RawMessage(`{"name":"` + name + `","secret_name":"` + secret + `"}`)}})
	if err != nil {
		t.Fatal(err)
	}
	return g
}

// policyOf is the run policy naming grants, in reverse grant-id order so a
// resolver that kept policy order would deliver out of order.
func policyOf(grants ...types.CredentialGrant) types.RunPolicySpec {
	slices.SortFunc(grants, func(a, b types.CredentialGrant) int { return strings.Compare(b.ID.String(), a.ID.String()) })
	var p types.RunPolicySpec
	for _, g := range grants {
		p.EligibleGrants = append(p.EligibleGrants, g.Spec)
	}
	return p
}

// The RR-c values path: dispatch's env_secret resolver puts each
// runner_resident grant's placeholder in the sandbox env and its value, read
// from the operator's namespace, in the resident list in grant-id order; the
// delivery hands exactly those values to the runner. A grant delivered own
// keeps its inline value and never joins the list.
func TestDispatch_ResidentEnvSecretValuesReachTheRunner(t *testing.T) {
	f := newResidentFix(t, nil, residentEnvPolicy)
	ctx := t.Context()
	run := f.startingRun(t, nil)
	a := f.envGrant(t, run, "A_TOKEN", "a-secret", "a-value", types.GrantDeliveryResident)
	b := f.envGrant(t, run, "B_TOKEN", "b-secret", "b-value", types.GrantDeliveryResident)
	own := f.envGrant(t, run, "OWN_TOKEN", "own-secret", "own-value", types.GrantDeliveryOwn)
	rows, err := f.st.ListGrantsByRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	policy := policyOf(a, b, own)
	env := map[string]string{}
	names, values := f.srv.resolveEnvSecretValues(ctx, run, policy, rows, env)
	slices.Sort(names)
	if !slices.Equal(names, []string{"A_TOKEN", "B_TOKEN", "OWN_TOKEN"}) {
		t.Fatalf("resolved names %v, want all three", names)
	}
	if env["A_TOKEN"] != placement.ResidentPlaceholder(a.ID) || env["B_TOKEN"] != placement.ResidentPlaceholder(b.ID) || env["OWN_TOKEN"] != "own-value" {
		t.Fatalf("sandbox env %v, want the two placeholders and the own value inline", env)
	}
	want := map[uuid.UUID]string{a.ID: "a-value", b.ID: "b-value"}
	if len(values) != 2 || !slices.IsSortedFunc(values, residentByGrantID) {
		t.Fatalf("resident list %+v, want the two resident grants in grant-id order", values)
	}
	for _, v := range values {
		if want[v.grant.ID] != string(v.value) || v.class != placement.ClassEnvSecret {
			t.Fatalf("resident value %+v, want its grant's operator secret", v)
		}
	}
	purposes := 0
	for _, ev := range syncAuditRows(f.audit, "secret.read") {
		if strings.Contains(string(ev.Data), string(secretstore.PurposeRunnerResident)) {
			purposes++
		}
	}
	if purposes != 2 {
		t.Fatalf("%d runner-resident secret.read rows, want one per resident grant", purposes)
	}

	if !f.srv.deliverResident(ctx, run, policy, values) {
		t.Fatal("delivery refused")
	}
	delivered, _ := f.side.calls()
	if len(delivered) != 2 || !slices.IsSortedFunc(delivered, func(x, y runnerwire.DeliverResidentArgs) int {
		return strings.Compare(x.GrantID.String(), y.GrantID.String())
	}) {
		t.Fatalf("delivered %+v, want the two resident values in grant-id order", delivered)
	}
	for _, d := range delivered {
		if want[d.GrantID] != d.Value || !d.Sandbox || d.Kind != string(types.GrantEnvSecret) || d.RunID != run.ID {
			t.Fatalf("delivered %+v, want the grant's value for this run's sandbox", d)
		}
	}
	if exists, _, _, _, _ := f.obligation(t, run.ID); !exists {
		t.Fatal("no obligation row for a delivered run")
	}
}

// A classify refusal at dispatch comes before the lease: a whole dispatch of a
// run with a resident env_secret, under a policy whose posture requirement the
// runner does not report, reads the value for delivery, then the classify
// refuses the run, nothing reaches the runner and no obligation opens.
func TestDispatch_ResidentEnvSecretRefusedAtClassifyBeforeTheLease(t *testing.T) {
	f := newResidentFix(t, nil, `{"classes":{"env_secret":{"mode":"runner_resident","require_posture":{"disk_encrypted":true}}}}`)
	f.srv.cfg.RunConfigKey = make([]byte, kek.DEKSize)
	ctx := t.Context()
	run := f.startingRun(t, func(r *types.AgentRun) { r.State = types.RunPending })
	g := f.envGrant(t, run, "A_TOKEN", "a-secret", "a-value", types.GrantDeliveryResident)
	f.srv.dispatchRun(ctx, run, ceilingForDispatch(governanceCeiling{}, adoEntraUngraded(), bedrockCredUngraded()),
		dispatchParams{RunToken: "run-token", Image: "wardyn/claude-code:latest", Policy: policyOf(g)})
	f.srv.WaitBackground()
	if got := f.dispatchRefusal(t, run.ID); got["reason"] != string(placement.ReasonRunnerPostureUnmet) {
		t.Fatalf("refusal = %v, want runner_posture_unmet", got)
	}
	read := false
	for _, ev := range syncAuditRows(f.audit, "secret.read") {
		read = read || (ev.RunID != nil && *ev.RunID == run.ID && strings.Contains(string(ev.Data), string(secretstore.PurposeRunnerResident)))
	}
	if !read {
		t.Fatal("dispatch did not read the resident value through the runner-resident read")
	}
	if delivered, _ := f.side.calls(); len(delivered) != 0 {
		t.Fatalf("delivered %+v despite the classify refusal", delivered)
	}
	if exists, _, _, _, _ := f.obligation(t, run.ID); exists {
		t.Fatal("an obligation was opened for a run the classify refused")
	}
	if st := f.runState(t, run.ID); st != types.RunFailed {
		t.Fatalf("run is %s, want FAILED", st)
	}
}

// awaitErase is the next run the runner side was asked to erase.
func (f *residentFix) awaitErase(t *testing.T, want uuid.UUID, why string) {
	t.Helper()
	select {
	case id := <-f.side.erasedCh:
		if id != want {
			t.Fatalf("%s erased %s, want %s", why, id, want)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("%s did not erase the ended run", why)
	}
	f.srv.WaitBackground()
	if _, _, erased, _, _ := f.obligation(t, want); erased == nil {
		t.Fatalf("%s did not close the obligation", why)
	}
}

// §5.4: the terminal cascade kicks the ended run's erase on the replica that
// holds its runner's session, without waiting for the watcher's sweep.
func TestRevokeRunCascade_KicksResidentErase(t *testing.T) {
	f := newResidentFix(t, nil, residentEnvPolicy)
	run := f.deliveredRun(t)
	f.side.mu.Lock()
	f.side.erasedCh = make(chan uuid.UUID, 4)
	f.side.mu.Unlock()
	_ = f.srv.revokeRunCascade(t.Context(), run.ID)
	f.awaitErase(t, run.ID, "the revoke cascade")
}

// §5.4: a runner's new session, once READY, kicks the erase of its ended
// runs. The hub reports the session online only after READY returns, so the
// kick does not wait on that.
func TestRunnerReady_KicksResidentErase(t *testing.T) {
	f := newResidentFix(t, nil, residentEnvPolicy)
	run := f.deliveredRun(t)
	f.side.mu.Lock()
	f.side.erasedCh = make(chan uuid.UUID, 4)
	f.side.mu.Unlock()
	f.online.Store(false)
	adm, err := f.srv.admitRunner(context.Background(), f.rn)
	if err != nil {
		t.Fatal(err)
	}
	adm.Ready()
	f.awaitErase(t, run.ID, "the runner-ready path")
}

// The audited read and the delivery each check the grant's recorded delivery
// themselves: a grant not recorded runner_resident (here own, then via_org) is
// never read for a runner nor handed to one, whatever its caller selected.
func TestResident_OnlyAResidentGrantIsReadOrDelivered(t *testing.T) {
	f := newResidentFix(t, nil, residentEnvPolicy)
	ctx := t.Context()
	for _, delivery := range []types.GrantDelivery{types.GrantDeliveryOwn, types.GrantDeliveryViaOrg} {
		run := f.startingRun(t, nil)
		g := f.envGrant(t, run, "A_TOKEN", "a-secret", "a-value", delivery)
		if v, err := f.srv.readResidentValue(ctx, run, g, "a-secret"); err == nil {
			t.Fatalf("%s grant read for a runner: %+v", delivery, v)
		}
		if f.srv.deliverResident(ctx, run, types.RunPolicySpec{}, []residentValue{{grant: g, class: placement.ClassEnvSecret, value: []byte("a-value")}}) {
			t.Fatalf("a %s grant was handed to the runner", delivery)
		}
		if got := f.dispatchRefusal(t, run.ID); got["reason"] != string(placement.ReasonPlacementCredential) || got["grant_id"] != g.ID.String() {
			t.Fatalf("refusal = %v, want placement_credential naming the grant", got)
		}
	}
	if delivered, _ := f.side.calls(); len(delivered) != 0 {
		t.Fatalf("delivered %+v, want nothing", delivered)
	}
}

// A server with no runner audit hook still erases; only the abandonment,
// whose audit rows go through that hook, is skipped.
func TestResidentEraseSweep_ErasesWithoutTheAuditHook(t *testing.T) {
	f := newResidentFix(t, nil, residentEnvPolicy)
	run := f.deliveredRun(t)
	f.srv.cfg.RunnerAuditPrepare = nil
	if err := f.srv.sweepResidentErase(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, erased := f.side.calls(); !slices.Equal(erased, []uuid.UUID{run.ID}) {
		t.Fatalf("erased %v, want the ended run", erased)
	}
}
