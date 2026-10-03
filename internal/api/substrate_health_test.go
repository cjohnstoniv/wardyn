// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/sweephealth"
)

// substrateRunner is a fakeRunner whose substrate probe answers what a test says.
type substrateRunner struct {
	*fakeRunner
	name  string
	state atomic.Value // runner.SubstrateState
	calls atomic.Int64
	// block, when non-nil, makes the probe wait on it and ignore its context:
	// a wedged client against a black-holed API server.
	block chan struct{}
}

func newSubstrateRunner(state runner.SubstrateState) *substrateRunner {
	r := &substrateRunner{fakeRunner: &fakeRunner{}, name: "fake"}
	r.state.Store(state)
	return r
}

func (r *substrateRunner) Name() string { return r.name }

func (r *substrateRunner) ProbeSubstrate(context.Context) runner.SubstrateState {
	r.calls.Add(1)
	if r.block != nil {
		<-r.block
	}
	return r.state.Load().(runner.SubstrateState)
}

func substrateRow(t *testing.T, st SetupStatus) SetupCheck {
	t.Helper()
	for _, c := range st.Checks {
		if c.ID == "substrate_health" {
			return c
		}
	}
	t.Fatalf("no substrate_health row in %+v", st.Checks)
	return SetupCheck{}
}

func assertNoBlockingCheck(t *testing.T, st SetupStatus) {
	t.Helper()
	for _, c := range st.Checks {
		if c.Blocking {
			t.Errorf("check %q is Blocking during a substrate outage: the console would send every admin into the setup funnel", c.ID)
		}
	}
}

// /readyz stays store-only while the substrate fails: it answers 200 while the
// gauge reads 0, so a substrate fault never pulls a replica out of the Service.
func TestSubstrateHealth_ReadyzStays200WhileTheProbeFails(t *testing.T) {
	rr := newSubstrateRunner(runner.SubstrateUnreachable)
	srv := New(Config{AdminToken: adminToken, Store: &pingStore{}, Runner: rr})

	if w := do(t, srv, http.MethodGet, "/readyz", "", ""); w.Code != http.StatusOK {
		t.Fatalf("/readyz = %d while the substrate probe fails, want 200", w.Code)
	}
	body := do(t, srv, http.MethodGet, "/metrics", adminToken, "").Body.String()
	if !strings.Contains(body, "\nwardyn_runner_up 0\n") {
		t.Fatalf("wardyn_runner_up is not 0 while the probe fails:\n%s", body)
	}
	if strings.Contains(body, "\nwardyn_runner_up 1\n") {
		t.Fatal("wardyn_runner_up reads both 0 and 1")
	}
	if rr.calls.Load() == 0 {
		t.Fatal("the probe was never called")
	}
}

// A probe that blocks forever leaves /setup/status and /metrics answering
// within the deadline, with the row failed and the gauge at 0.
func TestSubstrateHealth_BlockedProbeAnswersWithinTheDeadline(t *testing.T) {
	prev := storePingTimeout
	storePingTimeout = 100 * time.Millisecond
	t.Cleanup(func() { storePingTimeout = prev })

	rr := newSubstrateRunner(runner.SubstrateOK)
	rr.block = make(chan struct{})
	defer close(rr.block)
	srv := New(Config{AdminToken: adminToken, Runner: rr})

	bound := 20 * storePingTimeout
	for _, path := range []string{"/api/v1/setup/status", "/metrics"} {
		start := time.Now()
		w := do(t, srv, http.MethodGet, path, adminToken, "")
		if took := time.Since(start); took > bound {
			t.Fatalf("GET %s took %s against a blocked probe, want within the %s deadline", path, took, storePingTimeout)
		}
		if w.Code != http.StatusOK {
			t.Fatalf("GET %s = %d", path, w.Code)
		}
		if path == "/metrics" && !strings.Contains(w.Body.String(), "\nwardyn_runner_up 0\n") {
			t.Fatalf("a blocked probe must read wardyn_runner_up 0:\n%s", w.Body.String())
		}
	}
	_, st := decodeSetup(t, srv, adminToken)
	if row := substrateRow(t, st); row.Status != "fail" || row.Cause != "runner_unreachable" || row.Blocking {
		t.Fatalf("row = %+v, want fail/runner_unreachable and not Blocking", row)
	}
	// The wedged call is judged once, not once per scrape.
	if calls := rr.calls.Load(); calls != 1 {
		t.Fatalf("the blocked probe was started %d times, want 1", calls)
	}
}

// A runner whose Capabilities() errors yields a failed substrate_health row and
// no Blocking check anywhere: the runner row is non-blocking and points to it.
func TestSubstrateHealth_CapabilitiesErrorIsUnreachableAndNeverBlocking(t *testing.T) {
	secret := "dial tcp 10.1.2.3:2376: apiserver-internal-text"
	srv := New(Config{AdminToken: adminToken, Runner: &fakeRunner{capsErr: errors.New(secret)}})

	_, st := decodeSetup(t, srv, adminToken)
	row := substrateRow(t, st)
	if row.Status != "fail" || row.Cause != "runner_unreachable" || row.Blocking {
		t.Fatalf("row = %+v, want fail/runner_unreachable and not Blocking", row)
	}
	assertNoBlockingCheck(t, st)
	var runnerRow SetupCheck
	for _, c := range st.Checks {
		if c.ID == "runner" {
			runnerRow = c
		}
	}
	if runnerRow.Status != "fail" || !strings.Contains(runnerRow.Fix, "substrate_health") {
		t.Fatalf("runner row = %+v, want a non-blocking fail that points to substrate_health", runnerRow)
	}
	for _, c := range st.Checks {
		if strings.Contains(c.Detail+c.Fix, "apiserver-internal-text") || strings.Contains(c.Detail+c.Fix, "10.1.2.3") {
			t.Fatalf("check %q echoes raw substrate error text: %+v", c.ID, c)
		}
	}
}

// With no runner configured the runner row stays the one blocking failure.
func TestSubstrateHealth_NoRunnerStaysBlockingAndHasNoRow(t *testing.T) {
	srv := New(Config{AdminToken: adminToken})
	_, st := decodeSetup(t, srv, adminToken)
	for _, c := range st.Checks {
		if c.ID == "substrate_health" {
			t.Fatalf("substrate_health row with no runner and no sweeps: %+v", c)
		}
		if c.ID == "runner" && !c.Blocking {
			t.Fatalf("runner row = %+v, want Blocking when no runner is configured", c)
		}
	}
}

// A refused credential and a refused verb both read runner_auth, and the row
// names the classified state, not the substrate's words.
func TestSubstrateHealth_AuthStatesAreRunnerAuth(t *testing.T) {
	for _, tc := range []struct {
		state  runner.SubstrateState
		driver string
	}{
		{runner.SubstrateUnauthorized, "docker"},
		{runner.SubstrateForbidden, "k8s"},
	} {
		t.Run(string(tc.state), func(t *testing.T) {
			rr := newSubstrateRunner(tc.state)
			rr.name = tc.driver
			srv := New(Config{AdminToken: adminToken, Runner: rr})
			_, st := decodeSetup(t, srv, adminToken)
			row := substrateRow(t, st)
			if row.Status != "fail" || row.Cause != "runner_auth" || row.Blocking {
				t.Fatalf("row = %+v, want fail/runner_auth and not Blocking", row)
			}
			if !strings.Contains(row.Detail, string(tc.state)) {
				t.Errorf("detail %q does not name the classified state %q", row.Detail, tc.state)
			}
			assertNoBlockingCheck(t, st)
		})
	}
}

// Restoring the binding flips the row and the gauge back once the cached answer
// ages out, and not before.
func TestSubstrateHealth_RecoversAfterTheTTL(t *testing.T) {
	var mu sync.Mutex
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	rr := newSubstrateRunner(runner.SubstrateForbidden)
	srv := New(Config{AdminToken: adminToken, Runner: rr, Now: clock})

	if _, st := decodeSetup(t, srv, adminToken); substrateRow(t, st).Cause != "runner_auth" {
		t.Fatal("row did not fail while the binding is gone")
	}
	rr.state.Store(runner.SubstrateOK)
	if _, st := decodeSetup(t, srv, adminToken); substrateRow(t, st).Status != "fail" {
		t.Fatal("the cached answer was not served inside the TTL")
	}
	if got := rr.calls.Load(); got != 1 {
		t.Fatalf("%d probes inside the TTL, want 1 (cached per replica)", got)
	}
	mu.Lock()
	now = now.Add(substrateProbeTTL + time.Second)
	mu.Unlock()

	_, st := decodeSetup(t, srv, adminToken)
	if row := substrateRow(t, st); row.Status != "ok" || row.Cause != "" {
		t.Fatalf("row = %+v after the binding is restored, want ok", row)
	}
	if body := do(t, srv, http.MethodGet, "/metrics", adminToken, "").Body.String(); !strings.Contains(body, "\nwardyn_runner_up 1\n") {
		t.Fatalf("wardyn_runner_up did not return to 1:\n%s", body)
	}
}

// Concurrent readers share one probe.
func TestSubstrateHealth_SingleFlight(t *testing.T) {
	rr := newSubstrateRunner(runner.SubstrateOK)
	rr.block = make(chan struct{})
	srv := New(Config{AdminToken: adminToken, Runner: rr})

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			srv.runnerSubstrateState(context.Background())
		}()
	}
	time.Sleep(50 * time.Millisecond)
	close(rr.block)
	wg.Wait()
	if got := rr.calls.Load(); got != 1 {
		t.Fatalf("%d probes for 8 concurrent readers, want 1", got)
	}
}

// A member's poll neither triggers nor waits on a substrate call, and the row
// is not in a member's body.
func TestSubstrateHealth_MemberIsRedactedAndNeverProbes(t *testing.T) {
	h := newHarness(t)
	rr := newSubstrateRunner(runner.SubstrateUnreachable)
	cfg := baseTestConfig(h, nil)
	cfg.Runner = rr
	cfg.OIDC = &oidc.Authenticator{}
	srv := New(cfg)

	code, st := decodeSetupSSO(t, srv, ssoSession(t, "sub-member", "member@corp.example", oidc.RoleUser))
	if code != http.StatusOK {
		t.Fatalf("member: code = %d", code)
	}
	if len(st.Checks) != 0 || !st.ChecksRedacted {
		t.Fatalf("member checks = %+v (redacted=%v), want none", st.Checks, st.ChecksRedacted)
	}
	if got := rr.calls.Load(); got != 0 {
		t.Fatalf("a member's /setup/status probed the substrate %d times, want 0", got)
	}
	// The operator sees it.
	_, admin := decodeSetupSSO(t, srv, ssoSession(t, "sub-admin", "admin@corp.example", oidc.RoleAdmin))
	if row := substrateRow(t, admin); row.Status != "fail" {
		t.Fatalf("admin row = %+v, want fail", row)
	}
}

// substrateHealthCheck: the sweep-stale warn, its precedence under a runner
// failure, and that the detail names sweeps, never anything a substrate said.
func TestSubstrateHealthCheck_Grades(t *testing.T) {
	stale := []sweephealth.Status{
		{Sweep: sweephealth.Sweep{Name: sweephealth.CredentialExpiry, Interval: 24 * time.Hour}, Stale: true},
		{Sweep: sweephealth.Sweep{Name: sweephealth.RunSecret, Interval: 15 * time.Minute}},
	}
	fresh := []sweephealth.Status{{Sweep: sweephealth.Sweep{Name: sweephealth.RunSecret, Interval: 15 * time.Minute}}}

	for _, tc := range []struct {
		name       string
		state      runner.SubstrateState
		haveRunner bool
		sweeps     []sweephealth.Status
		wantOK     bool
		status     string
		cause      string
		mentions   string
	}{
		{"nothing to grade", "", false, nil, false, "", "", ""},
		{"runner and sweeps fine", runner.SubstrateOK, true, fresh, true, "ok", "", ""},
		{"stale sweep warns", runner.SubstrateOK, true, stale, true, "warn", "sweep_stale", "credential_expiry"},
		{"runner unreachable fails", runner.SubstrateUnreachable, true, fresh, true, "fail", "runner_unreachable", ""},
		{"runner failure outranks a stale sweep and names it", runner.SubstrateForbidden, true, stale, true, "fail", "runner_auth", "credential_expiry"},
		{"sweeps only", "", false, stale, true, "warn", "sweep_stale", "credential_expiry"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			chk, ok := substrateHealthCheck("k8s", tc.state, tc.haveRunner, tc.sweeps)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if chk.ID != "substrate_health" || chk.Status != tc.status || chk.Cause != tc.cause || chk.Blocking {
				t.Fatalf("check = %+v, want %s/%q and never Blocking", chk, tc.status, tc.cause)
			}
			if tc.mentions != "" && !strings.Contains(chk.Detail, tc.mentions) {
				t.Fatalf("detail %q does not name %q", chk.Detail, tc.mentions)
			}
		})
	}
}

// The sweep gauges and the row read the shared record: a follower that never
// ticks reports the leader's ticks, and a stopped leader reads stale.
func TestSweepHealth_GaugesAndRowReadTheSharedRecord(t *testing.T) {
	var mu sync.Mutex
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	advance := func(d time.Duration) { mu.Lock(); now = now.Add(d); mu.Unlock() }

	store := sweephealth.NewMemStore()
	leader := sweephealth.New(store, "leader", clock)
	follower := sweephealth.New(store, "follower", clock)
	sweeps := []sweephealth.Sweep{
		{Name: sweephealth.ApprovalExpiry, Interval: 10 * time.Minute},
		{Name: sweephealth.CredentialExpiry, Interval: 24 * time.Hour},
	}
	leader.Register(sweeps...)
	follower.Register(sweeps...)
	srv := New(Config{AdminToken: adminToken, Runner: newSubstrateRunner(runner.SubstrateOK), SweepHealth: follower, Now: clock})

	advance(10 * time.Minute)
	ctx := context.Background()
	_ = leader.Tick(ctx, sweephealth.ApprovalExpiry, func(context.Context) error { return nil })
	_ = leader.Tick(ctx, sweephealth.CredentialExpiry, func(context.Context) error { return errors.New("store blip") })

	body := do(t, srv, http.MethodGet, "/metrics", adminToken, "").Body.String()
	at := now.Unix()
	for _, want := range []string{
		`wardyn_sweep_last_tick_seconds{sweep="approval_expiry",result="attempt"} ` + strconv.FormatInt(at, 10) + "\n",
		`wardyn_sweep_last_tick_seconds{sweep="approval_expiry",result="success"} ` + strconv.FormatInt(at, 10) + "\n",
		`wardyn_sweep_last_tick_seconds{sweep="credential_expiry",result="attempt"} ` + strconv.FormatInt(at, 10) + "\n",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("a follower's scrape lacks the leader's tick %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, `sweep="credential_expiry",result="success"`) {
		t.Error("an erroring tick shows a success sample")
	}
	if _, st := decodeSetup(t, srv, adminToken); substrateRow(t, st).Status != "ok" {
		t.Fatalf("row = %+v, want ok while the leader ticks", substrateRow(t, st))
	}

	// The leader stops. After three of approval_expiry's intervals the follower
	// warns, whatever its own state: it never ran the sweep.
	advance(3 * 10 * time.Minute)
	_, st := decodeSetup(t, srv, adminToken)
	row := substrateRow(t, st)
	if row.Status != "warn" || row.Cause != "sweep_stale" || row.Blocking || !strings.Contains(row.Detail, "approval_expiry") {
		t.Fatalf("row = %+v, want a non-blocking warn/sweep_stale naming approval_expiry", row)
	}
	if strings.Contains(row.Detail, "credential_expiry") {
		t.Errorf("credential_expiry is nowhere near three intervals and is named: %q", row.Detail)
	}
}

// A store that cannot be read is "cannot tell": no sweep series, no stale row.
func TestSweepHealth_UnreadableRecordReportsNothing(t *testing.T) {
	store := sweephealth.NewMemStore()
	tr := sweephealth.New(store, "r", nil)
	tr.Register(sweephealth.Sweep{Name: sweephealth.RunSecret, Interval: time.Nanosecond})
	store.SetErr(errors.New("down"))
	time.Sleep(5 * time.Millisecond)
	srv := New(Config{AdminToken: adminToken, Runner: newSubstrateRunner(runner.SubstrateOK), SweepHealth: tr})

	if body := do(t, srv, http.MethodGet, "/metrics", adminToken, "").Body.String(); strings.Contains(body, "wardyn_sweep_last_tick_seconds") {
		t.Fatalf("sweep series with an unreadable record:\n%s", body)
	}
	_, st := decodeSetup(t, srv, adminToken)
	if row := substrateRow(t, st); row.Status != "ok" {
		t.Fatalf("row = %+v, want ok: an unreadable record is not a stale sweep", row)
	}
}

// The api-owned sweeps register only when their start condition holds.
func TestHealthSweeps_StartConditions(t *testing.T) {
	if got := New(Config{AdminToken: adminToken}).HealthSweeps(); len(got) != 0 {
		t.Fatalf("no runner and no sweeping builder registered %+v", got)
	}
	withRunner := New(Config{AdminToken: adminToken, Runner: &fakeRunner{}}).HealthSweeps()
	if len(withRunner) != 1 || withRunner[0].Name != sweephealth.RunWatcher || withRunner[0].Interval != watcherSweepInterval {
		t.Fatalf("with a runner: %+v, want run_watcher at %s", withRunner, watcherSweepInterval)
	}
	both := New(Config{AdminToken: adminToken, Runner: &fakeRunner{}, ImageBuilder: &sweepableImageBuilder{}}).HealthSweeps()
	if len(both) != 2 || both[1].Name != sweephealth.OrphanedBuild || both[1].Interval != buildSweepInterval {
		t.Fatalf("with a sweeping builder: %+v, want orphaned_build at %s", both, buildSweepInterval)
	}
}
