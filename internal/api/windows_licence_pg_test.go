// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// windowsSeat turns Windows VMs on in pool's site config and gives holder a seat in a one-seat pool with one
// registered image, whose ref it returns. It writes the stored switch directly: the key is not registered yet.
func windowsSeat(t *testing.T, pool *pgxpool.Pool, holder string) (types.WindowsLicencePool, types.WindowsImage) {
	t.Helper()
	ctx := t.Context()
	if _, err := pool.Exec(ctx, `DELETE FROM site_config`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO site_config (singleton, config) VALUES (true, '{"features":{"windows_vm":{"enabled":true}}}'::jsonb)`); err != nil {
		t.Fatal(err)
	}
	st := store.NewPG(pool)
	p, err := st.CreateWindowsLicencePool(ctx, types.WindowsLicencePool{Name: "Engineering", Seats: 1, CreatedBy: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	img, err := st.RegisterWindowsImage(ctx, types.WindowsImage{Name: "Server", Ref: "registry.example/windows/server@sha256:" + strings.Repeat("cd", 32), LicencePoolID: p.ID, CreatedBy: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddWindowsLicenceHolder(ctx, p.ID, holder, "admin"); err != nil {
		t.Fatal(err)
	}
	return p, img
}

func windowsLeaseCount(t *testing.T, pool *pgxpool.Pool, run uuid.UUID) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM windows_licence_leases WHERE run_id = $1`, run).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// newWindowsGoneAPIRig is newGoneAPIRig's twin whose run is inserted as windows_vm with its holder and image, so
// the take writes its seat: the fence refuses the macOS case's later kind change. The run has no agent and so no
// execution; its runner reports what a Docker runner reports for an exited Windows VM, the run with no sandbox,
// no exit and no agent state.
func newWindowsGoneAPIRig(t *testing.T) *goneAPIRig {
	t.Helper()
	pool := throwawayPGPool(t)
	st := store.NewPG(pool)
	ctx := t.Context()
	id := uuid.New()
	rn, err := st.CreateRunner(ctx, types.Runner{ID: id, Owner: "exit-owner", Name: "exit-test", PublicKey: make([]byte, 32), KeyFingerprint: id.String(), Version: "0.9.0", Limits: roomyRunnerLimits()})
	if err != nil {
		t.Fatal(err)
	}
	if rn, err = st.ClaimRunner(ctx, rn.ID, rn.Owner, rn.KeyFingerprint, time.Now()); err != nil {
		t.Fatal(err)
	}
	_, img := windowsSeat(t, pool, rn.Owner)
	run := newFinalizeRun()
	run.CreatedBy, run.Placement, run.RunnerID = rn.Owner, types.PlacementLocal, &rn.ID
	run.MachineKind, run.Image, run.Interactive, run.Task = types.MachineWindowsVM, img.Ref, true, ""
	if run, err = st.CreateRun(ctx, run); err != nil {
		t.Fatalf("insert the Windows run: %v", err)
	}
	if err := st.RaiseRunDataOut(ctx, run.ID, types.TransferAllowed); err != nil {
		t.Fatal(err)
	}
	ref := "runner:" + rn.ID.String() + "/sandbox"
	if err := st.SetSandboxRef(ctx, run.ID, ref); err != nil {
		t.Fatal(err)
	}
	cfg := baseTestConfig(newHarness(t), st)
	probe := &offlineContainmentProbe{fakeRunner: &fakeRunner{}}
	cfg.Runner = probe
	cfg.Audit = &syncAudit{}
	cfg.RunnerAuditPrepare = func(_ context.Context, ev types.AuditEvent) (types.AuditEvent, error) { return ev, nil }
	g := &goneAPIRig{t: t, pool: pool, cfg: cfg, st: st, rn: rn, run: run, probe: probe,
		gone: runnerwire.RunState{RunID: run.ID, Ref: ref}, present: runnerwire.RunState{RunID: run.ID, Ref: ref, SandboxPresent: true}}
	g.exec(`UPDATE agent_runs SET ends_at=NULL, lifetime_cap_at=NULL WHERE id=$1`)
	return g
}

// An exited Windows VM is reported gone by its runner, by either of two paths: the STATE page a runner sends once
// per session (the first absent report refuses READY, and the reconnect that refusal causes sends the second), or,
// while the runner stays connected, the watcher's probe, whose agent status for an empty exec id is the QEMU
// container's own. Either ends the run FAILED, lost (gone), and the end frees its seat. A clean guest shutdown, an
// OOM kill and a QEMU crash all end this way, with the gone hint. This test drives the STATE path.
func TestPG_WindowsRunObservedGoneFreesSeat(t *testing.T) {
	g := newWindowsGoneAPIRig(t)
	if windowsLeaseCount(t, g.pool, g.run.ID) != 1 {
		t.Fatal("the Windows run took no seat")
	}
	g.cfg.BaseCtx = t.Context()
	srv := New(g.cfg)
	g.confirm(srv)
	g.awaitGoneEnd()
	if n := windowsLeaseCount(t, g.pool, g.run.ID); n != 0 {
		t.Fatalf("the gone end left %d seats", n)
	}
	if err := finalStatePage(t, srv, g.rn.ID); err != nil {
		t.Fatalf("runner not READY after the gone end: %v", err)
	}
}

// Erasing a person whose seat a lost run still holds fails the runner_pools scope, naming the run, and deletes
// no seat; once the run is killed the retried scope deletes it.
func TestPG_PersonErasure_WindowsSeatHeldByLostRunFailsScope(t *testing.T) {
	pool := throwawayPGPool(t)
	st := store.NewPG(pool)
	ctx := t.Context()
	const person = "ana@example.com"
	p, img := windowsSeat(t, pool, person)
	run := newFinalizeRun()
	run.CreatedBy, run.MachineKind, run.Image, run.Interactive = person, types.MachineWindowsVM, img.Ref, true
	run, err := st.CreateRun(ctx, run)
	if err != nil {
		t.Fatalf("insert the Windows run: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE agent_runs SET lost_at = now(), lost_reason = 'gone' WHERE id = $1`, run.ID); err != nil {
		t.Fatal(err)
	}
	s := &Server{cfg: Config{Store: st}}
	step := s.alsoEraseWindowsSeats(s.eraseRunnerPoolsOf)

	_, err = step(ctx, person)
	var live *store.WindowsSeatLiveError
	if !errors.As(err, &live) || live.RunID != run.ID || !strings.Contains(err.Error(), run.ID.String()) {
		t.Fatalf("erasure beside a lost run's seat = %v, want a failure naming %s", err, run.ID)
	}
	if hs, _ := st.ListWindowsLicenceHolders(ctx, p.ID); len(hs) != 1 {
		t.Fatalf("holders after the failed scope = %v, want the seat kept", hs)
	}
	if _, err := pool.Exec(ctx, `UPDATE agent_runs SET state = 'KILLED' WHERE id = $1`, run.ID); err != nil {
		t.Fatal(err)
	}
	detail, err := step(ctx, person)
	if err != nil {
		t.Fatalf("the retried scope: %v", err)
	}
	if d, ok := detail.(map[string]any); !ok || d["windows_licence_seats"] != int64(1) {
		t.Fatalf("the retried scope's detail = %#v, want one Windows seat erased", detail)
	}
	if hs, _ := st.ListWindowsLicenceHolders(ctx, p.ID); len(hs) != 0 {
		t.Fatalf("holders after the scope = %v, want none", hs)
	}
}
