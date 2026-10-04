// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const (
	gi = int64(1) << 30
	mi = int64(1) << 20
)

// fitRunner is a fakeRunner whose substrate has a quota to check: CheckFit answers fit.
type fitRunner struct {
	*fakeRunner
	fit    runner.Fit
	err    error
	asked  runner.Resources
	checks int
}

func (f *fitRunner) CheckFit(_ context.Context, res runner.Resources) (runner.Fit, error) {
	f.checks++
	f.asked = res
	return f.fit, f.err
}

func TestJudgeFit_Sentences(t *testing.T) {
	cpuMem := func(needCPU, leftCPU, hardCPU, needMem, leftMem, hardMem int64) []runner.QuotaAxis {
		return []runner.QuotaAxis{
			{Key: "requests.cpu", Need: needCPU, Left: leftCPU, Hard: hardCPU},
			{Key: "requests.memory", Need: needMem, Left: leftMem, Hard: hardMem},
		}
	}
	for _, tc := range []struct {
		name         string
		fit          runner.Fit
		wantRefusal  string
		wantWarnings []string
	}{
		{
			name: "empty quota list says nothing",
			fit:  runner.Fit{Quotas: runner.ReadOK, Nodes: runner.ReadSkipped},
		},
		{
			name: "a quota with room says nothing",
			fit: runner.Fit{Quotas: runner.ReadOK, Quota: []runner.QuotaFit{
				{Name: "runs-quota", Axes: cpuMem(1500, 8000, 16000, 2*gi, 16*gi, 32*gi)},
			}},
		},
		{
			name: "a breach is refused and counts only the limits breached",
			fit: runner.Fit{Quotas: runner.ReadOK, Quota: []runner.QuotaFit{
				{Name: "runs-quota", Axes: cpuMem(2000, 1000, 4000, 4*gi, 2*gi, 8*gi)},
			}},
			wantRefusal: "this run needs 2 CPU, 4Gi memory, more than quota runs-quota has left (1 CPU, 2Gi memory). Stop a run, or ask your admin to raise the quota.",
		},
		{
			name: "the second of two applicable quotas decides",
			fit: runner.Fit{Quotas: runner.ReadOK, Quota: []runner.QuotaFit{
				{Name: "a-roomy", Axes: cpuMem(1500, 8000, 16000, 2*gi, 16*gi, 32*gi)},
				{Name: "b-tight", Axes: []runner.QuotaAxis{{Key: "pods", Need: 2, Left: 1, Hard: 10}}},
			}},
			wantRefusal: "this run needs 2 pods, more than quota b-tight has left (1 pod). Stop a run, or ask your admin to raise the quota.",
		},
		{
			name: "a limit axis says so",
			fit: runner.Fit{Quotas: runner.ReadOK, Quota: []runner.QuotaFit{
				{Name: "lim", Axes: []runner.QuotaAxis{{Key: "limits.cpu", Need: 2500, Left: 500, Hard: 4000}}},
			}},
			wantRefusal: "this run needs 2.5 CPU limit, more than quota lim has left (0.5 CPU limit). Stop a run, or ask your admin to raise the quota.",
		},
		{
			name: "near full warns at 90 percent and names the room left",
			fit: runner.Fit{Quotas: runner.ReadOK, Quota: []runner.QuotaFit{
				{Name: "runs-quota", Axes: cpuMem(1500, 2000, 8000, 3*gi, 4*gi, 16*gi)},
			}},
			wantWarnings: []string{"this run would fill quota runs-quota to 93% (0.5 CPU, 1Gi memory left after it) — later runs may be refused."},
		},
		{
			name: "89 percent does not warn",
			fit: runner.Fit{Quotas: runner.ReadOK, Quota: []runner.QuotaFit{
				{Name: "q", Axes: []runner.QuotaAxis{{Key: "pods", Need: 2, Left: 13, Hard: 100}}},
			}},
		},
		{
			name: "forbidden quota list is its own sentence",
			fit:  runner.Fit{Quotas: runner.ReadForbidden, Nodes: runner.ReadSkipped},
			wantWarnings: []string{
				"couldn't read this namespace's quotas (permission denied) — they are still enforced when the run starts."},
		},
		{
			name: "unavailable quota list is another",
			fit:  runner.Fit{Quotas: runner.ReadUnavailable, Nodes: runner.ReadSkipped},
			wantWarnings: []string{
				"couldn't read this namespace's quotas (unavailable) — they are still enforced when the run starts."},
		},
		{
			name: "forbidden node list says the check was skipped",
			fit:  runner.Fit{Quotas: runner.ReadOK, Nodes: runner.ReadForbidden},
			wantWarnings: []string{
				"couldn't read node sizes (permission denied) — the node-size check was skipped."},
		},
		{
			name: "no node large enough",
			fit: runner.Fit{Quotas: runner.ReadOK, Nodes: runner.ReadOK,
				NodeShortfall: &runner.NodeShortfall{CPUMillis: 2000, MemoryBytes: 4 * gi}},
			wantWarnings: []string{
				"no node this run may be placed on is large enough for 2 CPU, 4Gi memory. It may wait unscheduled until one is."},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := judgeFit(tc.fit)
			if v.refusal != tc.wantRefusal {
				t.Errorf("refusal = %q, want %q", v.refusal, tc.wantRefusal)
			}
			if strings.Join(v.warnings, "\n") != strings.Join(tc.wantWarnings, "\n") {
				t.Errorf("warnings = %q, want %q", v.warnings, tc.wantWarnings)
			}
		})
	}
}

func TestRunFit_RunnerWithoutAFitToCheckSaysNothing(t *testing.T) {
	spec := types.RunPolicySpec{}
	if v := (&Server{}).runFit(context.Background(), spec); v.refusal != "" || v.warnings != nil {
		t.Fatalf("nil runner: %+v", v)
	}
	if v := (&Server{cfg: Config{Runner: &fakeRunner{}}}).runFit(context.Background(), spec); v.refusal != "" || v.warnings != nil {
		t.Fatalf("runner without a FitChecker: %+v", v)
	}
	unsupported := &fitRunner{fakeRunner: &fakeRunner{}, err: runner.ErrFitUnsupported}
	if v := (&Server{cfg: Config{Runner: unsupported}}).runFit(context.Background(), spec); v.refusal != "" || v.warnings != nil {
		t.Fatalf("ErrFitUnsupported: %+v", v)
	}
	broken := &fitRunner{fakeRunner: &fakeRunner{}, err: context.DeadlineExceeded}
	v := (&Server{cfg: Config{Runner: broken}}).runFit(context.Background(), spec)
	if v.refusal != "" || len(v.warnings) != 1 || !strings.Contains(v.warnings[0], "(unavailable)") {
		t.Fatalf("a failed check must be an advisory, never a refusal: %+v", v)
	}
}

// fitStore ends a run's detached launch at its first state claim: these tests are about the
// answer POST /runs gives, and a claim that does not apply makes dispatch stop and write one audit row.
type fitStore struct {
	*runCapAPIStore
	site types.SiteConfig
}

func (s *fitStore) GetSiteConfig(context.Context) (types.SiteConfig, error) { return s.site, nil }

func (*fitStore) UpdateRunStateIf(context.Context, uuid.UUID, types.RunState, types.RunState) (bool, error) {
	return false, nil
}

// fitServer is the POST /runs fixture of TestCreateRun_DeploymentCapRefusesBeforeTheMint with a
// runner that answers a fit.
func fitServer(t *testing.T, fit runner.Fit, cpuMillis, memMiB int) (*Server, *runCapAPIStore, *fitRunner, *harness) {
	t.Helper()
	h := newHarness(t)
	st := &runCapAPIStore{runWarnStore: &runWarnStore{capStore: &capStore{}}}
	fr := &fitRunner{fakeRunner: &fakeRunner{}, fit: fit}
	cfg := baseTestConfig(h, &fitStore{runCapAPIStore: st})
	cfg.Runner = fr
	cfg.DefaultPolicy = types.RunPolicySpec{
		MinConfinementClass: types.CC2, AllowedDomains: []string{"api.anthropic.com"},
		Resources: &types.ResourceLimits{CPUMillis: cpuMillis, MemoryMiB: memMiB},
	}
	return New(cfg), st, fr, h
}

// TestCreateRun_QuotaBreachRefusesBeforeDispatch: a run the quota cannot hold is a 422 with the
// quota's sentence and writes no run row, no audit row and creates no sandbox; preflight answers
// the same 422; with room, both pass and preflight carries the advisories.
func TestCreateRun_QuotaBreachRefusesBeforeDispatch(t *testing.T) {
	breach := runner.Fit{Quotas: runner.ReadOK, Nodes: runner.ReadSkipped, Quota: []runner.QuotaFit{
		{Name: "runs-quota", Axes: []runner.QuotaAxis{{Key: "requests.cpu", Need: 2500, Left: 1000, Hard: 4000}}},
	}}
	srv, st, fr, h := fitServer(t, breach, 2000, 4096)
	// An org default disk and a policy that names none: dispatch will fill it, so both doors
	// must ask the quota for it (the ephemeral-storage axes).
	srv.cfg.Store.(*fitStore).site = ephemeralSite(10240, 0)
	const body = `{"agent":"claude-code","task":"t"}`
	const want = "this run needs 2.5 CPU, more than quota runs-quota has left (1 CPU). Stop a run, or ask your admin to raise the quota."

	w := do(t, srv, http.MethodPost, "/api/v1/runs", adminToken, body)
	if w.Code != http.StatusUnprocessableEntity || errorReason(w) != reasonNamespaceQuotaExceeded {
		t.Fatalf("create: %d %q, want 422 %q (body %s)", w.Code, errorReason(w), reasonNamespaceQuotaExceeded, w.Body)
	}
	var got errorBody
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || got.Error != want {
		t.Fatalf("create message = %q (%v), want %q", got.Error, err, want)
	}
	if st.created.ID != uuid.Nil {
		t.Fatal("a refused run wrote a run row")
	}
	if rows := h.audit.snapshot(); len(rows) != 0 {
		t.Fatalf("a refused run wrote audit rows: %+v", rows)
	}
	if fr.createCalls != 0 {
		t.Fatalf("a refused run created %d sandbox(es)", fr.createCalls)
	}
	// The check is asked the run's own size: the policy's CPU and memory, not a default.
	if fr.asked.CPUMillis != 2000 || fr.asked.MemoryMiB != 4096 {
		t.Fatalf("CheckFit asked %+v, want the run's 2000m/4096Mi", fr.asked)
	}
	if fr.asked.DiskMiB != 10240 {
		t.Fatalf("create asked the quota for %d MiB of disk, want the org default 10240", fr.asked.DiskMiB)
	}

	fr.asked = runner.Resources{}
	w = do(t, srv, http.MethodPost, "/api/v1/runs/preflight", adminToken, body)
	if w.Code != http.StatusUnprocessableEntity || errorReason(w) != reasonNamespaceQuotaExceeded {
		t.Fatalf("preflight: %d %q, want the same 422 (body %s)", w.Code, errorReason(w), w.Body)
	}
	if fr.asked.DiskMiB != 10240 {
		t.Fatalf("preflight asked the quota for %d MiB of disk, want the org default 10240", fr.asked.DiskMiB)
	}

	// 2500m needed, 2800m left of 30000m: admitted, with the quota 99% used once the run is in.
	fr.fit = runner.Fit{Quotas: runner.ReadOK, Nodes: runner.ReadSkipped, Quota: []runner.QuotaFit{
		{Name: "runs-quota", Axes: []runner.QuotaAxis{{Key: "requests.cpu", Need: 2500, Left: 2800, Hard: 30000}}},
	}}
	w = do(t, srv, http.MethodPost, "/api/v1/runs/preflight", adminToken, body)
	if w.Code != http.StatusOK {
		t.Fatalf("preflight with room: %d %s", w.Code, w.Body)
	}
	var pf preflightResponse
	if err := json.Unmarshal(w.Body.Bytes(), &pf); err != nil {
		t.Fatal(err)
	}
	if len(pf.Warnings) != 1 || !strings.HasPrefix(pf.Warnings[0], "this run would fill quota runs-quota to 99%") {
		t.Fatalf("preflight warnings = %q, want the near-full advisory", pf.Warnings)
	}

	w = do(t, srv, http.MethodPost, "/api/v1/runs", adminToken, body)
	if w.Code != http.StatusCreated || st.created.ID == uuid.Nil {
		t.Fatalf("create with room: %d %s", w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), "this run would fill quota runs-quota") {
		t.Fatalf("create's 201 omits the advisory: %s", w.Body)
	}
}
