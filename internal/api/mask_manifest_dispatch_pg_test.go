// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// dispatchRun and the run masking manifest, against a real Postgres (see
// mask_manifest_pg_test.go): the manifest is committed, complete, before the
// sandbox exists, and a run whose manifest cannot be committed never launches.

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/maskmanifest"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	secretspg "github.com/cjohnstoniv/wardyn/internal/secretstore/pg"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// coverProbeRunner records, at the moment CreateSandbox is called, whether the
// run's masking manifest was already complete.
type coverProbeRunner struct {
	fakeRunner
	m  *maskmanifest.Manifests
	mu sync.Mutex
	// coveredAtCreate is Covered(run) as CreateSandbox began; sawCreate says it ran.
	coveredAtCreate, sawCreate bool
}

func (r *coverProbeRunner) CreateSandbox(ctx context.Context, spec runner.SandboxSpec) (runner.Sandbox, error) {
	covered := r.m.Covered(ctx, spec.RunID)
	r.mu.Lock()
	r.coveredAtCreate, r.sawCreate = covered, true
	r.mu.Unlock()
	return r.fakeRunner.CreateSandbox(ctx, spec)
}

// maskedDispatch is a dispatch fixture whose run is also a row in Postgres.
func maskedDispatch(t *testing.T, createdBy string) (*maskLab, *Server, *coverProbeRunner, *recRecorder, types.AgentRun) {
	t.Helper()
	l := newMaskLab(t)
	probe := &coverProbeRunner{}
	srv, _, audit, run := dispatchTeardownFixture(t, probe, types.RunPending)
	run.Task, run.CreatedBy = "", createdBy
	now := time.Now().UTC()
	pgRun := run
	pgRun.CreatedAt, pgRun.UpdatedAt, pgRun.ConfinementClass = now, now, types.CC2
	pgRun.SPIFFEID, pgRun.RunnerTarget, pgRun.Agent = "spiffe://wardyn.test/agent-run/"+run.ID.String(), "docker", "claude-code"
	if pgRun.CreatedBy == "" {
		pgRun.CreatedBy = "placeholder@example.com" // the row's NOT NULL; the dispatched run below has none
	}
	if _, err := store.NewPG(l.pool).CreateRun(context.Background(), pgRun); err != nil {
		t.Fatalf("persist the run: %v", err)
	}
	sec, err := secretspg.New(l.pool, l.id)
	if err != nil {
		t.Fatal(err)
	}
	reg := secretmask.NewRegistry()
	srv.cfg.MaskRegistry, srv.cfg.Secrets = reg, sec
	srv.cfg.MaskManifests = maskmanifest.New(l.pool, sec.SubjectKeys(), reg)
	probe.m = srv.cfg.MaskManifests
	return l, srv, probe, audit, run
}

func dispatchOf(srv *Server, run types.AgentRun, policy types.RunPolicySpec) {
	srv.dispatchRun(context.Background(), run, ceilingForDispatch(governanceCeiling{}, adoEntraUngraded(), bedrockCredUngraded()), dispatchParams{
		RunToken: "run-token", Image: "wardyn/claude-code:latest", Policy: policy,
	})
}

// A dispatched run's env_secret value is on the manifest, which is complete
// before the sandbox exists; a second wardynd then masks it.
func TestMaskManifest_DispatchCommitsTheManifestBeforeTheSandbox(t *testing.T) {
	l, srv, probe, audit, run := maskedDispatch(t, maskOwner)
	ctx := context.Background()
	if err := srv.cfg.Secrets.For(maskOwner).Put(ctx, "deploy-token", []byte("dispatch-time-secret-value")); err != nil {
		t.Fatal(err)
	}
	scope, _ := json.Marshal(map[string]string{"name": "DEPLOY_TOKEN", "secret_name": "deploy-token"})
	dispatchOf(srv, run, types.RunPolicySpec{EligibleGrants: []types.GrantSpec{{Kind: types.GrantEnvSecret, Scope: scope}}})

	if !probe.sawCreate {
		t.Fatalf("the sandbox was never created; events: %s", auditDump(audit.events, run.ID))
	}
	if !probe.coveredAtCreate {
		t.Fatal("the run's masking manifest was not complete when the sandbox was created")
	}
	if probe.lastSpec.SecretEnv["DEPLOY_TOKEN"] != "dispatch-time-secret-value" {
		t.Fatalf("the env_secret was not delivered: %v", probe.lastSpec.SecretEnv)
	}

	other := l.replica()
	if w := l.upload(other, types.AgentRun{ID: run.ID, CreatedBy: maskOwner}, "leak: dispatch-time-secret-value\n"); w.Code != 204 {
		t.Fatalf("upload to another replica: %d %s", w.Code, w.Body.String())
	}
	if strings.Contains(string(other.rec.saved), "dispatch-time-secret-value") {
		t.Errorf("another replica persisted the dispatch-time value unmasked: %q", other.rec.saved)
	}
}

// A value that cannot be put on the manifest is not delivered; a manifest that
// cannot begin refuses the launch before any value is resolved or sandbox made.
func TestMaskManifest_UnrecordableRunNeverLaunches(t *testing.T) {
	_, srv, probe, audit, run := maskedDispatch(t, "")
	dispatchOf(srv, run, types.RunPolicySpec{})
	if probe.sawCreate {
		t.Error("a sandbox was created for a run whose masking manifest could not begin")
	}
	if ev := findAudit(audit.events, run.ID, "run.dispatch", "failure"); ev == nil || !strings.Contains(string(ev.Data), "masking manifest") {
		t.Errorf("no run.dispatch failure naming the manifest; events: %s", auditDump(audit.events, run.ID))
	}

	// A fenced owner: Begin succeeds on the existing row, the first Append fails.
	_, srv2, probe2, _, run2 := maskedDispatch(t, maskOwner)
	ctx := context.Background()
	if err := srv2.cfg.Secrets.For(maskOwner).Put(ctx, "deploy-token", []byte("never-delivered-secret")); err != nil {
		t.Fatal(err)
	}
	if err := srv2.cfg.MaskManifests.Start(ctx, run2.ID, maskOwner); err != nil {
		t.Fatal(err)
	}
	if _, err := srv2.cfg.MaskManifests.FenceSubject(ctx, maskOwner); err != nil {
		t.Fatal(err)
	}
	scope, _ := json.Marshal(map[string]string{"name": "DEPLOY_TOKEN", "secret_name": "deploy-token"})
	dispatchOf(srv2, run2, types.RunPolicySpec{EligibleGrants: []types.GrantSpec{{Kind: types.GrantEnvSecret, Scope: scope}}})
	if probe2.sawCreate {
		t.Error("a run whose manifest is fenced was launched")
	}
}
