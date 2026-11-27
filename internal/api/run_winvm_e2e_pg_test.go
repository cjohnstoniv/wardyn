// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runner/substrate"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire/runnertest"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// winvmSite stores the site config with runners on and the Windows VM switch at on.
func (e *queueEnv) winvmSite(on bool) {
	e.t.Helper()
	sc := types.SiteConfig{Runners: &types.RunnerSettings{Enabled: true},
		FeatureSwitches: &types.SiteFeatureSwitches{WindowsVM: &types.WindowsVMFeature{Enabled: &on}}}
	if _, err := e.pg.PutSiteConfig(context.Background(), sc); err != nil {
		e.t.Fatal(err)
	}
}

// winvmStream connects runner id's stream, publishing caps that offer Windows VM sandboxes.
func (e *queueEnv) winvmStream(id uuid.UUID) {
	e.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	e.t.Cleanup(cancel)
	fake := runnertest.NewFake()
	ep := e.srv.runnerEndpoint(id)
	org, run := runnerwire.Loopback()
	ep.link.Attach(ctx, org)
	served := runnertest.Serve(ctx, run, id.String(), fake)
	caps := runnerwire.Caps{Support: substrate.ClassSupport{Classes: []types.ConfinementClass{types.CC1, types.CC2, types.CC3}},
		Capacity: placement.Capacity{CPUMillisMax: 4000, MemoryMiBMax: 8192}, AllowedRoots: []string{"/srv/dev/work"}, Version: "test",
		ProxySettings: runnerwire.ProxySettings, ProxyFence: true, FolderPassOnce: true,
		LinuxVM:   &runnerwire.VMCaps{MachineType: "pc-q35-10.0", CPUModel: "Haswell-v4", QEMUVersion: "10.0.13", Desktop: true},
		WindowsVM: &runnerwire.VMCaps{MachineType: "pc-q35-10.0", CPUModel: "Haswell-v4", QEMUVersion: "10.0.13", Desktop: true}}
	data, err := json.Marshal(caps)
	if err != nil {
		e.t.Fatal(err)
	}
	if err := served.Peer.Event(ctx, runnerwire.Event{Kind: runnerwire.EventCaps, Data: data}); err != nil {
		e.t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		if _, ok := ep.sub.Caps(); ok {
			return
		}
		if time.Now().After(deadline) {
			e.t.Fatal("the runner's caps never arrived")
		}
	}
}

// newWinvmEnv is a queue env with runners and Windows VMs on, the person holding the one seat of a licence pool
// whose image is "Server", and an organisation pool (capped at max runs when max > 0) of one organisation runner
// whose stream offers Windows VMs.
func newWinvmEnv(t *testing.T, max int) (*queueEnv, types.RunnerPool, uuid.UUID, types.WindowsImage) {
	t.Helper()
	e := newQueueEnv(t)
	e.winvmSite(true)
	ctx := context.Background()
	lp, err := e.pg.CreateWindowsLicencePool(ctx, types.WindowsLicencePool{Name: "Engineering", Seats: 1, CreatedBy: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	img, err := e.pg.RegisterWindowsImage(ctx, types.WindowsImage{Name: "Server", Ref: "registry.example/windows/server@sha256:" + strings.Repeat("cd", 32),
		LicencePoolID: lp.ID, CreatedBy: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.pg.AddWindowsLicenceHolder(ctx, lp.ID, e.personP, "admin"); err != nil {
		t.Fatal(err)
	}
	body := `{"name":"Windows"}`
	if max > 0 {
		body = fmt.Sprintf(`{"name":"Windows","limits":{"max_concurrent_runs":%d}}`, max)
	}
	w := e.admin(http.MethodPost, "/api/v1/runner-pools", body)
	if w.Code != http.StatusCreated {
		t.Fatalf("create pool = %d %s", w.Code, w.Body)
	}
	pool := decodeJSON[types.RunnerPool](t, w)
	org := e.orgRunner()
	if w := e.admin(http.MethodPut, orgRunnerPath(pool.ID, org), ""); w.Code != http.StatusCreated {
		t.Fatalf("add the org runner = %d %s", w.Code, w.Body)
	}
	e.winvmStream(org)
	return e, e.reread(pool), org, img
}

func winvmCreateBody(pool types.RunnerPool) string {
	return `{"title":"Windows desk","machine_kind":"windows_vm","windows_image":"Server","interactive":true,` +
		`"placement":"local","runner_pool_id":"` + pool.ID.String() + `"}`
}

// A Windows VM create driven through POST /api/v1/runs end to end: the fold admits the person's launch on the
// organisation runner that offers Windows VMs, the insert takes the seat and stores the registered ref, the
// run.create row names the image, its digest and the pool, and dispatch hands the runner that ref.
func TestPG_WindowsCreateThroughPOSTRuns(t *testing.T) {
	e, pool, org, img := newWinvmEnv(t, 0)
	run := e.localCreate(winvmCreateBody(pool))
	if run.MachineKind != types.MachineWindowsVM || run.Image != img.Ref || run.RunnerID == nil || *run.RunnerID != org {
		t.Fatalf("row = kind %q image %q runner %v, want windows_vm %s on %s", run.MachineKind, run.Image, run.RunnerID, img.Ref, org)
	}
	if n := windowsLeaseCount(t, e.pool, run.ID); n != 1 {
		t.Fatalf("seats taken = %d, want 1", n)
	}
	var created map[string]any
	for _, ev := range e.h.audit.snapshot() {
		if ev.RunID != nil && *ev.RunID == run.ID && ev.Action == "run.create" && ev.Outcome == "success" {
			if err := json.Unmarshal(ev.Data, &created); err != nil {
				t.Fatal(err)
			}
		}
	}
	if created["windows_image"] != "Server" || created["windows_image_digest"] != "sha256:"+strings.Repeat("cd", 32) ||
		created["windows_licence_pool"] != "Engineering" {
		t.Errorf("run.create row = %v, want the image, its digest and the pool", created)
	}
	spec := e.dispatchedSpec(run.ID)
	if spec.Image != img.Ref || spec.MachineKind != types.MachineWindowsVM || spec.RunnerID != org.String() {
		t.Errorf("dispatched image %q kind %q runner %q, want %s windows_vm on %s", spec.Image, spec.MachineKind, spec.RunnerID, img.Ref, org)
	}
}

// A queued Windows VM run whose switch went off while it waited is ended by the queue's own start: the
// slot-freeing kick promotes it, its start's fold refuses windows_vms_off, the run ends failed with a
// run.queue.start failure row, and its seat is free again.
func TestPG_QueuedWindowsRunEndedByItsStart(t *testing.T) {
	e, pool, _, _ := newWinvmEnv(t, 1)
	occupant := e.occupy(pool)
	w := e.call(e.person, http.MethodPost, "/api/v1/runs", winvmCreateBody(pool))
	queued := decodeJSON[queuedLaunchAnswer](t, w)
	if w.Code != http.StatusCreated || queued.State != types.RunQueued {
		t.Fatalf("launch into a full pool = %d %s; want 201 QUEUED", w.Code, w.Body)
	}
	if n := windowsLeaseCount(t, e.pool, queued.ID); n != 1 {
		t.Fatalf("the queued run holds %d seats, want 1", n)
	}
	e.winvmSite(false)
	e.free(occupant)
	e.eventually("the queued Windows run to end", func() bool { return e.run(queued.ID).State == types.RunFailed })
	if n := windowsLeaseCount(t, e.pool, queued.ID); n != 0 {
		t.Errorf("the ended run still holds %d seats", n)
	}
	if got := e.auditCount(func(ev types.AuditEvent) bool {
		return ev.RunID != nil && *ev.RunID == queued.ID && ev.Action == "run.queue.start" && ev.Outcome == "failure"
	}); got != 1 {
		t.Errorf("run.queue.start failure rows = %d, want 1; audit: %s", got, auditDump(e.h.audit.snapshot(), queued.ID))
	}
}

// A queued Windows VM run, its switch still on, is started by the queue's own start: the start's fold passes
// the person, token and seat checks (the seat its insert took is its own), and dispatch hands the runner the
// registered ref.
func TestPG_QueuedWindowsRunStartsThroughItsStart(t *testing.T) {
	e, pool, org, img := newWinvmEnv(t, 1)
	occupant := e.occupy(pool)
	w := e.call(e.person, http.MethodPost, "/api/v1/runs", winvmCreateBody(pool))
	queued := decodeJSON[queuedLaunchAnswer](t, w)
	if w.Code != http.StatusCreated || queued.State != types.RunQueued {
		t.Fatalf("launch into a full pool = %d %s; want 201 QUEUED", w.Code, w.Body)
	}
	e.free(occupant)
	started := e.waitStarted(queued.ID)
	if started.State == types.RunFailed || started.Image != img.Ref || started.RunnerID == nil || *started.RunnerID != org {
		t.Fatalf("started = state %s image %q runner %v, want %s on %s; audit: %s", started.State, started.Image, started.RunnerID,
			img.Ref, org, auditDump(e.h.audit.snapshot(), queued.ID))
	}
	if n := windowsLeaseCount(t, e.pool, queued.ID); n != 1 {
		t.Errorf("the started run holds %d seats, want 1", n)
	}
	spec := e.dispatchedSpec(queued.ID)
	if spec.Image != img.Ref || spec.MachineKind != types.MachineWindowsVM || spec.RunnerID != org.String() {
		t.Errorf("dispatched image %q kind %q runner %q, want %s windows_vm on %s", spec.Image, spec.MachineKind, spec.RunnerID, img.Ref, org)
	}
}
