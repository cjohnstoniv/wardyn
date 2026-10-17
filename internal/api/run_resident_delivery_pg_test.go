// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// residentRunnerSide is the runner end of a session for the resident tests:
// it records each deliver_resident and erase_resident call and answers with
// the configured refusal or failure.
type residentRunnerSide struct {
	mu        sync.Mutex
	delivered []runnerwire.DeliverResidentArgs
	erased    []uuid.UUID
	deliver   func(runnerwire.DeliverResidentArgs) error
	eraseErr  error
	erasedCh  chan uuid.UUID
}

func (r *residentRunnerSide) handle(_ context.Context, _ uint32, method string, args json.RawMessage) (any, error) {
	switch method {
	case runnerwire.MethodDeliverResident:
		var a runnerwire.DeliverResidentArgs
		if err := json.Unmarshal(args, &a); err != nil {
			return nil, err
		}
		r.mu.Lock()
		refuse := r.deliver
		r.mu.Unlock()
		if refuse != nil {
			if err := refuse(a); err != nil {
				return nil, err
			}
		}
		r.mu.Lock()
		r.delivered = append(r.delivered, a)
		r.mu.Unlock()
		return nil, nil
	case runnerwire.MethodEraseResident:
		var a runnerwire.EraseResidentArgs
		if err := json.Unmarshal(args, &a); err != nil {
			return nil, err
		}
		r.mu.Lock()
		r.erased = append(r.erased, a.RunID)
		err := r.eraseErr
		r.mu.Unlock()
		if r.erasedCh != nil {
			r.erasedCh <- a.RunID
		}
		return nil, err
	}
	return nil, runnerwire.Refuse("unexpected %s", method)
}

func (r *residentRunnerSide) calls() ([]runnerwire.DeliverResidentArgs, []uuid.UUID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.delivered), slices.Clone(r.erased)
}

type residentFix struct {
	pool   *pgxpool.Pool
	st     store.PG
	cfg    Config
	srv    *Server
	rn     types.Runner
	side   *residentRunnerSide
	online atomic.Bool
	now    atomic.Int64
	audit  *syncAudit
}

const residentEnvPolicy = `{"classes":{"env_secret":{"mode":"runner_resident"}}}`

// newResidentFix is a claimed runner with posture (nil: none reported), the
// delivery policy given, runners turned on, and the runner side attached.
func newResidentFix(t *testing.T, posture *types.RunnerPosture, policy string) *residentFix {
	t.Helper()
	pool := throwawayPGPool(t)
	st := store.NewPG(pool)
	ctx := t.Context()
	f := &residentFix{pool: pool, st: st, side: &residentRunnerSide{}, audit: &syncAudit{}}
	f.now.Store(time.Now().UTC().Truncate(time.Second).UnixNano())
	f.online.Store(true)
	id := uuid.New()
	rn, err := st.CreateRunner(ctx, types.Runner{ID: id, Owner: "resident-owner", Name: "resident-test", PublicKey: make([]byte, 32), KeyFingerprint: id.String(), Version: "0.9.3", Limits: roomyRunnerLimits()})
	if err != nil {
		t.Fatal(err)
	}
	if f.rn, err = st.ClaimRunner(ctx, rn.ID, rn.Owner, rn.KeyFingerprint, time.Now()); err != nil {
		t.Fatal(err)
	}
	if posture != nil {
		if _, err := st.RecordRunnerPosture(ctx, f.rn.ID, *posture, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.PutCredentialDeliveryPolicy(ctx, json.RawMessage(policy), "seed", time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := st.PutSiteConfig(ctx, types.SiteConfig{Runners: &types.RunnerSettings{Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	f.cfg = baseTestConfig(newHarness(t), st)
	f.cfg.Runner = &fakeRunner{}
	f.cfg.Audit = f.audit
	f.cfg.RunnerAuditPrepare = func(_ context.Context, ev types.AuditEvent) (types.AuditEvent, error) { return ev, nil }
	f.cfg.RunnerOnline = func(id uuid.UUID) bool { return id == f.rn.ID && f.online.Load() }
	f.cfg.Now = func() time.Time { return time.Unix(0, f.now.Load()).UTC() }
	f.cfg.Secrets = &memSecrets{m: map[string][]byte{}}
	f.cfg.MaskRegistry = secretmask.NewRegistry()
	f.srv = New(f.cfg)
	f.attach(t)
	return f
}

func (f *residentFix) attach(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ep := f.srv.runnerEndpoint(f.rn.ID)
	org, run := runnerwire.Loopback()
	ep.link.Attach(ctx, org)
	peer := runnerwire.NewPeer(run, runnerwire.PeerConfig{OnCall: f.side.handle})
	go func() { _ = peer.Run(ctx) }()
}

func (f *residentFix) advance(d time.Duration) { f.now.Add(int64(d)) }

func (f *residentFix) clock() time.Time { return f.cfg.Now() }

// startingRun is a STARTING local run bound to the fixture's runner.
func (f *residentFix) startingRun(t *testing.T, mutate func(*types.AgentRun)) types.AgentRun {
	t.Helper()
	run := newFinalizeRun()
	run.State, run.CreatedBy, run.Placement, run.RunnerID = types.RunStarting, f.rn.Owner, types.PlacementLocal, &f.rn.ID
	if mutate != nil {
		mutate(&run)
	}
	run, err := f.st.CreateRun(t.Context(), run)
	if err != nil {
		t.Fatal(err)
	}
	return run
}

func (f *residentFix) value(t *testing.T, run types.AgentRun, val string) residentValue {
	t.Helper()
	g, err := f.st.CreateGrant(t.Context(), types.CredentialGrant{ID: uuid.New(), RunID: run.ID, Delivery: types.GrantDeliveryResident,
		Spec: types.GrantSpec{Kind: types.GrantEnvSecret, Scope: json.RawMessage(`{"name":"TOKEN","secret_name":"ci-token"}`)}})
	if err != nil {
		t.Fatal(err)
	}
	return residentValue{grant: g, class: placement.ClassEnvSecret, value: []byte(val)}
}

func (f *residentFix) runState(t *testing.T, id uuid.UUID) types.RunState {
	t.Helper()
	run, err := f.st.GetRun(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	return run.State
}

func (f *residentFix) obligation(t *testing.T, runID uuid.UUID) (exists bool, attempts int, erased, abandoned *string, notAfter time.Time) {
	t.Helper()
	var erasedAt *time.Time
	err := f.pool.QueryRow(t.Context(), `SELECT erase_attempts, erased_at, abandoned_reason, not_after FROM runner_resident_runs WHERE run_id = $1`, runID).
		Scan(&attempts, &erasedAt, &abandoned, &notAfter)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, 0, nil, nil, time.Time{}
	}
	if err != nil {
		t.Fatal(err)
	}
	if erasedAt != nil {
		s := erasedAt.String()
		erased = &s
	}
	return true, attempts, erased, abandoned, notAfter
}

// dispatchRefusal is the run's one run.dispatch failure row's data.
func (f *residentFix) dispatchRefusal(t *testing.T, runID uuid.UUID) map[string]string {
	t.Helper()
	var rows []types.AuditEvent
	for _, ev := range syncAuditRows(f.audit, "run.dispatch") {
		if ev.RunID != nil && *ev.RunID == runID && ev.Outcome == "failure" {
			rows = append(rows, ev)
		}
	}
	if len(rows) != 1 {
		t.Fatalf("%d run.dispatch failure rows for the run, want one: %+v", len(rows), rows)
	}
	var data map[string]string
	if err := json.Unmarshal(rows[0].Data, &data); err != nil {
		t.Fatal(err)
	}
	return data
}

// not_after is the run's lifetime cap or end, whichever comes first, and never
// past 24 hours after delivery; a run ending within a minute is refused before
// anything is written or sent.
func TestDispatch_ResidentNotAfterBound(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) *time.Time { v := now.Add(d); return &v }
	for name, c := range map[string]struct {
		capAt, endsAt *time.Time
		want          time.Time
	}{
		"72h cap":             {capAt: at(72 * time.Hour), want: now.Add(24 * time.Hour)},
		"2h cap":              {capAt: at(2 * time.Hour), want: now.Add(2 * time.Hour)},
		"earlier end":         {capAt: at(5 * time.Hour), endsAt: at(3 * time.Hour), want: now.Add(3 * time.Hour)},
		"earlier cap":         {capAt: at(3 * time.Hour), endsAt: at(5 * time.Hour), want: now.Add(3 * time.Hour)},
		"no cap and no end":   {want: now.Add(24 * time.Hour)},
		"end past the bound":  {endsAt: at(30 * time.Hour), want: now.Add(24 * time.Hour)},
		"cap inside a minute": {capAt: at(30 * time.Second), want: now.Add(30 * time.Second)},
	} {
		if got := residentNotAfter(types.AgentRun{LifetimeCapAt: c.capAt, EndsAt: c.endsAt}, now); !got.Equal(c.want) {
			t.Errorf("%s: not_after = %v, want %v", name, got, c.want)
		}
	}

	f := newResidentFix(t, nil, residentEnvPolicy)
	soon := f.clock().Add(50 * time.Second)
	run := f.startingRun(t, func(r *types.AgentRun) { r.EndsAt = &soon })
	if f.srv.deliverResident(t.Context(), run, types.RunPolicySpec{}, []residentValue{f.value(t, run, "v1")}) {
		t.Fatal("a run ending within a minute was delivered to")
	}
	if got := f.dispatchRefusal(t, run.ID); got["reason"] != string(placement.ReasonPlacementCredential) || got["runner_id"] != f.rn.ID.String() {
		t.Fatalf("refusal = %v, want placement_credential naming the runner", got)
	}
	if exists, _, _, _, _ := f.obligation(t, run.ID); exists {
		t.Fatal("a refused delivery wrote an obligation")
	}
	if delivered, _ := f.side.calls(); len(delivered) != 0 {
		t.Fatalf("a refused delivery reached the runner: %+v", delivered)
	}

	capAt := f.clock().Add(2 * time.Hour)
	ok := f.startingRun(t, func(r *types.AgentRun) { r.LifetimeCapAt = &capAt })
	if !f.srv.deliverResident(t.Context(), ok, types.RunPolicySpec{}, []residentValue{f.value(t, ok, "v2")}) {
		t.Fatal("a delivery inside the bound was refused")
	}
	delivered, _ := f.side.calls()
	if len(delivered) != 1 || !delivered[0].NotAfter.Equal(capAt) {
		t.Fatalf("delivered %+v, want one value bounded by the 2h cap %v", delivered, capAt)
	}
	if _, _, _, _, notAfter := f.obligation(t, ok.ID); !notAfter.Equal(capAt) {
		t.Fatalf("stored not_after %v, want %v", notAfter, capAt)
	}
}

// Every value goes to the runner in grant-id order, env values for the
// sandbox, through the runner's substrate; a runner refusal fails the run with
// a run.dispatch row naming the reason, the grant and the runner, and kicks the
// erase; an offline runner is runner_offline.
func TestDispatch_ResidentDeliveryFailureRefusesRun(t *testing.T) {
	f := newResidentFix(t, nil, residentEnvPolicy)
	ctx := t.Context()
	run := f.startingRun(t, nil)
	values := []residentValue{f.value(t, run, "first"), f.value(t, run, "second"), f.value(t, run, "third")}
	if !f.srv.deliverResident(ctx, run, types.RunPolicySpec{}, values) {
		t.Fatal("delivery refused")
	}
	delivered, _ := f.side.calls()
	if len(delivered) != 3 || !slices.IsSortedFunc(delivered, func(a, b runnerwire.DeliverResidentArgs) int {
		return strings.Compare(a.GrantID.String(), b.GrantID.String())
	}) {
		t.Fatalf("delivered %+v, want all three in grant-id order", delivered)
	}
	for _, d := range delivered {
		if !d.Sandbox || d.Kind != string(types.GrantEnvSecret) || d.RunID != run.ID {
			t.Fatalf("delivered %+v, want an env value for the sandbox of this run", d)
		}
	}
	for _, v := range values {
		if !bytes.Equal(v.value, make([]byte, len(v.value))) {
			t.Fatalf("value %q was not overwritten after delivery", v.value)
		}
	}
	if f.runState(t, run.ID) != types.RunStarting {
		t.Fatal("a delivered run left STARTING")
	}

	// The runner's keystore refuses the second value.
	f.side.mu.Lock()
	f.side.delivered, f.side.erasedCh = nil, make(chan uuid.UUID, 4)
	f.side.mu.Unlock()
	refused := f.startingRun(t, nil)
	values = []residentValue{f.value(t, refused, "a"), f.value(t, refused, "b")}
	slices.SortFunc(values, func(a, b residentValue) int { return strings.Compare(a.grant.ID.String(), b.grant.ID.String()) })
	second := values[1].grant.ID
	f.side.mu.Lock()
	f.side.deliver = func(a runnerwire.DeliverResidentArgs) error {
		if a.GrantID == second {
			return runnerwire.Refuse("%s: the keychain is locked", placement.ReasonRunnerKeystoreUnavailable)
		}
		return nil
	}
	f.side.mu.Unlock()
	if f.srv.deliverResident(ctx, refused, types.RunPolicySpec{}, values) {
		t.Fatal("a delivery the runner refused succeeded")
	}
	if got := f.dispatchRefusal(t, refused.ID); got["reason"] != string(placement.ReasonRunnerKeystoreUnavailable) ||
		got["grant_id"] != second.String() || got["runner_id"] != f.rn.ID.String() {
		t.Fatalf("refusal = %v, want runner_keystore_unavailable naming the second grant and the runner", got)
	}
	if st := f.runState(t, refused.ID); st != types.RunFailed {
		t.Fatalf("refused run is %s, want FAILED", st)
	}
	select {
	case id := <-f.side.erasedCh:
		if id != refused.ID {
			t.Fatalf("the kick erased %s, want the refused run", id)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a refused delivery did not kick the erase")
	}
	f.srv.WaitBackground()
	if _, _, erased, _, _ := f.obligation(t, refused.ID); erased == nil {
		t.Fatal("the kicked erase did not close the obligation")
	}

	// Offline: a server that holds no session for the runner.
	offline := f.startingRun(t, nil)
	sessionless := New(f.cfg)
	defer sessionless.WaitBackground()
	if sessionless.deliverResident(ctx, offline, types.RunPolicySpec{}, []residentValue{f.value(t, offline, "x")}) {
		t.Fatal("a delivery to an offline runner succeeded")
	}
	if got := f.dispatchRefusal(t, offline.ID); got["reason"] != string(placement.ReasonRunnerOffline) {
		t.Fatalf("refusal = %v, want runner_offline", got)
	}
}

// The runner's posture and the delivery policy are judged again at delivery,
// not taken from create: a posture that no longer meets the class's
// requirement, a class no longer runner_resident, or a run with a blast-radius
// floor refuses the run before any value is sent.
func TestDispatch_ResidentPostureChangedBeforeDelivery(t *testing.T) {
	healthy := types.RunnerPosture{OS: "linux", OSVersion: "6.6", DiskEncrypted: true, Evidence: types.PostureEvidenceOSProbe}
	f := newResidentFix(t, &healthy, `{"classes":{"env_secret":{"mode":"runner_resident","require_posture":{"disk_encrypted":true}}}}`)
	ctx := t.Context()
	run := f.startingRun(t, nil)
	if !f.srv.deliverResident(ctx, run, types.RunPolicySpec{}, []residentValue{f.value(t, run, "ok")}) {
		t.Fatal("a delivery to a runner meeting the requirement was refused")
	}

	degraded := healthy
	degraded.DiskEncrypted = false
	if _, err := f.st.RecordRunnerPosture(ctx, f.rn.ID, degraded, time.Now()); err != nil {
		t.Fatal(err)
	}
	f.side.mu.Lock()
	f.side.delivered = nil
	f.side.mu.Unlock()
	unmet := f.startingRun(t, nil)
	v := f.value(t, unmet, "no")
	if f.srv.deliverResident(ctx, unmet, types.RunPolicySpec{}, []residentValue{v}) {
		t.Fatal("a runner whose posture changed was delivered to")
	}
	if got := f.dispatchRefusal(t, unmet.ID); got["reason"] != string(placement.ReasonRunnerPostureUnmet) || got["grant_id"] != v.grant.ID.String() {
		t.Fatalf("refusal = %v, want runner_posture_unmet naming the grant", got)
	}

	if _, err := f.st.RecordRunnerPosture(ctx, f.rn.ID, healthy, time.Now()); err != nil {
		t.Fatal(err)
	}
	floored := f.startingRun(t, nil)
	floor := types.RunPolicySpec{EligibleGrants: []types.GrantSpec{{Kind: types.GrantAPIKey, Scope: json.RawMessage(`{"host":"api.unlisted.example"}`)}}}
	if f.srv.deliverResident(ctx, floored, floor, []residentValue{f.value(t, floored, "no")}) {
		t.Fatal("a run with a blast-radius floor was delivered to")
	}
	if got := f.dispatchRefusal(t, floored.ID); got["reason"] != string(placement.ReasonRunnerPostureUnmet) {
		t.Fatalf("refusal = %v, want runner_posture_unmet for the floor", got)
	}

	if _, err := f.st.PutCredentialDeliveryPolicy(ctx, json.RawMessage(`{"classes":{"env_secret":{"mode":"refuse"}}}`), "seed", time.Now()); err != nil {
		t.Fatal(err)
	}
	turnedOff := f.startingRun(t, nil)
	if f.srv.deliverResident(ctx, turnedOff, types.RunPolicySpec{}, []residentValue{f.value(t, turnedOff, "no")}) {
		t.Fatal("a class the policy no longer delivers runner_resident was delivered")
	}
	if got := f.dispatchRefusal(t, turnedOff.ID); got["reason"] != string(placement.ReasonPlacementCredential) {
		t.Fatalf("refusal = %v, want placement_credential for the policy change", got)
	}
	if delivered, _ := f.side.calls(); len(delivered) != 0 {
		t.Fatalf("a refused delivery reached the runner: %+v", delivered)
	}
}

// On a macOS runner a value whose keychain command would pass 4096 bytes is
// refused before sending; the same value goes to a Linux runner, and the
// longest value that fits goes to the macOS one.
func TestDispatch_ResidentDarwinOversizeRefused(t *testing.T) {
	runner, run, grant := uuid.New(), uuid.New(), uuid.New()
	// 36 fixed + 51 service + 73 account + 18 prefix leave 3918 bytes of base64: 2937 value bytes.
	if macKeychainCommandTooLong(runner, run, grant, make([]byte, 2937)) || !macKeychainCommandTooLong(runner, run, grant, make([]byte, 2938)) {
		t.Fatal("the keychain bound is not at 2937 value bytes")
	}

	mac := types.RunnerPosture{OS: "darwin", OSVersion: "15.2", Evidence: types.PostureEvidenceOSProbe}
	f := newResidentFix(t, &mac, residentEnvPolicy)
	ctx := t.Context()
	big := f.startingRun(t, nil)
	v := f.value(t, big, strings.Repeat("k", 2938))
	if f.srv.deliverResident(ctx, big, types.RunPolicySpec{}, []residentValue{v}) {
		t.Fatal("an oversize value was delivered to a macOS runner")
	}
	if got := f.dispatchRefusal(t, big.ID); got["reason"] != string(placement.ReasonPlacementCredential) || got["grant_id"] != v.grant.ID.String() {
		t.Fatalf("refusal = %v, want placement_credential naming the grant", got)
	}
	if delivered, _ := f.side.calls(); len(delivered) != 0 {
		t.Fatalf("an oversize value reached the runner: %+v", delivered)
	}
	fits := f.startingRun(t, nil)
	if !f.srv.deliverResident(ctx, fits, types.RunPolicySpec{}, []residentValue{f.value(t, fits, strings.Repeat("k", 2937))}) {
		t.Fatal("the longest value that fits was refused")
	}
	linux := mac
	linux.OS, linux.OSVersion = "linux", "6.6"
	if _, err := f.st.RecordRunnerPosture(ctx, f.rn.ID, linux, time.Now()); err != nil {
		t.Fatal(err)
	}
	onLinux := f.startingRun(t, nil)
	if !f.srv.deliverResident(ctx, onLinux, types.RunPolicySpec{}, []residentValue{f.value(t, onLinux, strings.Repeat("k", 2938))}) {
		t.Fatal("a Linux runner was refused a value only the macOS keychain bounds")
	}
}

// deliveredRun is an ended run whose one resident value its runner holds.
func (f *residentFix) deliveredRun(t *testing.T) types.AgentRun {
	t.Helper()
	run := f.startingRun(t, nil)
	if !f.srv.deliverResident(t.Context(), run, types.RunPolicySpec{}, []residentValue{f.value(t, run, "held")}) {
		t.Fatal("delivery refused")
	}
	if ok, err := f.st.UpdateRunStateIf(t.Context(), run.ID, types.RunStarting, types.RunCompleted); err != nil || !ok {
		t.Fatalf("end run: %v %v", ok, err)
	}
	return run
}

// A revoked runner's obligation is closed unconfirmed, with the org's
// runner.resident.erase row, and the runner is not asked.
func TestResidentEraseSweep_RevokedRunnerAbandonedUnconfirmed(t *testing.T) {
	f := newResidentFix(t, nil, residentEnvPolicy)
	ctx := t.Context()
	run := f.deliveredRun(t)
	if _, err := f.st.RevokeRunner(ctx, f.rn.ID, time.Now(), "admin", ""); err != nil {
		t.Fatal(err)
	}
	if err := f.srv.sweepResidentErase(ctx); err != nil {
		t.Fatal(err)
	}
	if _, _, _, abandoned, _ := f.obligation(t, run.ID); abandoned == nil || *abandoned != store.ResidentAbandonRunnerRevoked {
		t.Fatalf("abandoned %v, want runner_revoked", abandoned)
	}
	var n int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE action = 'runner.resident.erase' AND run_id = $1
		AND outcome = 'failure' AND data->>'outcome' = 'unconfirmed' AND data->>'reason' = 'runner_revoked'`, run.ID).Scan(&n); err != nil || n != 1 {
		t.Fatalf("%d org runner.resident.erase rows (%v), want one", n, err)
	}
	if _, erased := f.side.calls(); len(erased) != 0 {
		t.Fatalf("a revoked runner was asked to erase: %v", erased)
	}
}

// An ended run's obligation waits while its runner is offline here, and is
// erased and closed once this replica holds the runner's session.
func TestResidentEraseSweep_OfflineWaitsThenErases(t *testing.T) {
	f := newResidentFix(t, nil, residentEnvPolicy)
	ctx := t.Context()
	run := f.deliveredRun(t)
	f.online.Store(false)
	if err := f.srv.sweepResidentErase(ctx); err != nil {
		t.Fatal(err)
	}
	if _, attempts, erased, _, _ := f.obligation(t, run.ID); attempts != 0 || erased != nil {
		t.Fatalf("an offline runner's obligation was claimed: attempts %d erased %v", attempts, erased)
	}
	f.online.Store(true)
	if err := f.srv.sweepResidentErase(ctx); err != nil {
		t.Fatal(err)
	}
	if _, erased := f.side.calls(); !slices.Equal(erased, []uuid.UUID{run.ID}) {
		t.Fatalf("erased %v, want the ended run", erased)
	}
	if _, _, erased, _, _ := f.obligation(t, run.ID); erased == nil {
		t.Fatal("the confirmed erase did not close the obligation")
	}
}

// A failed erase keeps its reason and is retried only after its backoff.
func TestResidentEraseSweep_RetriesWithBackoff(t *testing.T) {
	f := newResidentFix(t, nil, residentEnvPolicy)
	ctx := t.Context()
	run := f.deliveredRun(t)
	f.side.mu.Lock()
	f.side.eraseErr = runnerwire.Refuse("resident erase incomplete")
	f.side.mu.Unlock()
	if err := f.srv.sweepResidentErase(ctx); err == nil {
		t.Fatal("a failed erase was not reported by the pass")
	}
	var reason *string
	if err := f.pool.QueryRow(ctx, `SELECT erase_last_reason FROM runner_resident_runs WHERE run_id = $1`, run.ID).Scan(&reason); err != nil ||
		reason == nil || !strings.Contains(*reason, "resident erase incomplete") {
		t.Fatalf("last reason %v (%v), want the runner's", reason, err)
	}
	f.advance(10 * time.Second)
	if err := f.srv.sweepResidentErase(ctx); err != nil {
		t.Fatal(err)
	}
	if _, erased := f.side.calls(); len(erased) != 1 {
		t.Fatalf("erase asked %d times inside the backoff, want once", len(erased))
	}
	f.side.mu.Lock()
	f.side.eraseErr = nil
	f.side.mu.Unlock()
	f.advance(25 * time.Second)
	if err := f.srv.sweepResidentErase(ctx); err != nil {
		t.Fatal(err)
	}
	if _, attempts, erased, _, _ := f.obligation(t, run.ID); attempts != 2 || erased == nil {
		t.Fatalf("after the backoff: attempts %d erased %v, want the second attempt to close it", attempts, erased)
	}
}

// The dispatch read of a runner_resident value is SiteAudited and recorded as
// secret.read, purpose runner-resident, with the grant, the runner and its
// posture; it reads the operator's namespace only, and the value is masked
// before it is returned.
func TestResident_ReadIsAuditedAndMasked(t *testing.T) {
	posture := types.RunnerPosture{OS: "linux", OSVersion: "6.6", DiskEncrypted: true, Evidence: types.PostureEvidenceOSProbe}
	f := newResidentFix(t, &posture, residentEnvPolicy)
	ctx := t.Context()
	sec := f.cfg.Secrets.(*memSecrets)
	if err := sec.Put(ctx, "ci-token", []byte("resident-secret-value")); err != nil {
		t.Fatal(err)
	}
	if err := sec.For(f.rn.Owner).Put(ctx, "owner-only", []byte("owner-value")); err != nil {
		t.Fatal(err)
	}
	run := f.startingRun(t, nil)
	grant := f.value(t, run, "").grant
	got, err := f.srv.readResidentValue(ctx, run, grant, "ci-token")
	if err != nil || string(got.value) != "resident-secret-value" || got.class != placement.ClassEnvSecret || got.grant.ID != grant.ID {
		t.Fatalf("read = %+v (%v)", got, err)
	}
	if masked := f.cfg.MaskRegistry.Masker(run.ID).Mask([]byte("leak resident-secret-value")); bytes.Contains(masked, []byte("resident-secret-value")) {
		t.Fatalf("the value is not on the run's masking manifest: %q", masked)
	}
	if _, err := f.srv.readResidentValue(ctx, run, grant, "owner-only"); !errors.Is(err, secretstore.ErrNotFound) {
		t.Fatalf("an owner-namespace secret = %v, want not found (no owner fallback)", err)
	}
	rows := syncAuditRows(f.audit, "secret.read")
	if len(rows) != 2 || rows[0].Target != "ci-token" || rows[0].Outcome != "success" || rows[1].Outcome != "failure" {
		t.Fatalf("secret.read rows = %+v, want a success then a failure", rows)
	}
	var data struct {
		Purpose  string              `json:"purpose"`
		Grant    uuid.UUID           `json:"grant_id"`
		Runner   uuid.UUID           `json:"runner_id"`
		Delivery string              `json:"delivery"`
		Owner    *string             `json:"owner"`
		Source   string              `json:"posture_source"`
		Posture  types.RunnerPosture `json:"posture"`
		Store    string              `json:"store"`
		Ref      string              `json:"ref"`
	}
	if err := json.Unmarshal(rows[0].Data, &data); err != nil {
		t.Fatal(err)
	}
	if data.Purpose != string(secretstore.PurposeRunnerResident) || data.Grant != grant.ID || data.Runner != f.rn.ID ||
		data.Delivery != placement.ClassEnvSecret || data.Owner == nil || *data.Owner != "" ||
		data.Source != types.RunnerPostureSourceRunnerAsserted || data.Posture != posture || data.Store != "mem" || data.Ref == "" {
		t.Fatalf("secret.read data = %s", rows[0].Data)
	}
}

// A delivery refusal ends dispatch before the create: the sandbox is never
// asked for; with no resident values the create follows the lease as before.
func TestDispatch_ResidentRefusalNeverReachesCreate(t *testing.T) {
	f := newResidentFix(t, nil, residentEnvPolicy)
	ctx := t.Context()
	fake := f.cfg.Runner.(*fakeRunner)
	unbound := func(r *types.AgentRun) { r.RunnerID, r.Placement = nil, "" }
	refusedRun := f.startingRun(t, unbound)
	spec := runner.SandboxSpec{RunID: refusedRun.ID}
	_, _, refused, err := f.srv.leaseDeliverCreate(ctx, ctx, refusedRun, spec, []residentValue{f.value(t, refusedRun, "v")})
	if !refused || err != nil {
		t.Fatalf("refused=%v err=%v, want the delivery's refusal", refused, err)
	}
	fake.mu.Lock()
	calls := fake.createCalls
	fake.mu.Unlock()
	if calls != 0 {
		t.Fatalf("%d creates after a refused delivery, want none", calls)
	}
	if st := f.runState(t, refusedRun.ID); st != types.RunFailed {
		t.Fatalf("refused run is %s, want FAILED", st)
	}
	plain := f.startingRun(t, unbound)
	_, sb, refused, err := f.srv.leaseDeliverCreate(ctx, ctx, plain, runner.SandboxSpec{RunID: plain.ID}, nil)
	fake.mu.Lock()
	calls = fake.createCalls
	fake.mu.Unlock()
	if refused || err != nil || calls != 1 || sb.Ref != "fake-"+plain.ID.String() {
		t.Fatalf("refused=%v err=%v creates=%d sandbox=%+v, want one create for a run with no resident values", refused, err, calls, sb)
	}
}
