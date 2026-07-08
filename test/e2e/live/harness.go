// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

// Package live is the Wardyn end-to-end task orchestrator: it drives an
// ALREADY-RUNNING host-mode wardynd (docker runner + real composer) through its
// public API, launches REAL confined sandboxes against the task corpus in
// test/e2e/tasks/, and proves — with deterministic graders that inspect final
// workspace STATE, never a transcript — that each agent actually completed its
// task AND that the sandbox allowed/blocked egress correctly.
//
// It is the "did the agent really do the work, and were the walls real?" layer
// that test/e2e/e2e.sh (security invariants only, with a fail-fast print task)
// never covered. See the plan: buzzing-twirling-stonebraker.md.
//
// Target stack: start it with `scripts/run-host.sh` (host-mode, docker runner,
// oracle + claude-code images mapped). The orchestrator runs on the HOST, so it
// shares the docker daemon (to run grader containers + inspect sandboxes) and the
// host filesystem (to seed workspaces and read back what the agent produced).
//
// Guarded by WARDYN_TEST_DOCKER=1 (mirrors the repo convention); a no-op without
// it, so plain `go test ./...` is unaffected.
package live

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	yaml "gopkg.in/yaml.v3"

	"github.com/cjohnstoniv/wardyn/internal/cliutil"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// ── configuration (all overridable via env) ─────────────────────────────────

// harness bundles the SDK client + raw HTTP for the endpoints the SDK lacks
// (compose, attach) and the resolved config.
type harness struct {
	t         *testing.T
	base      string
	token     string
	sdk       *client.Client
	http      *http.Client
	tasksDir  string
	credsDir  string // staged Claude subscription creds (subscription real-model lane)
	workRoot  string // host dir where per-run workspaces are seeded + read back
	realModel bool
}

// newHarness resolves config, skips cleanly when the suite is disabled or the
// stack is unreachable, and returns a ready harness.
func newHarness(t *testing.T) *harness {
	t.Helper()
	if os.Getenv("WARDYN_TEST_DOCKER") != "1" {
		t.Skip("WARDYN_TEST_DOCKER=1 not set; skipping the live docker e2e orchestrator")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not found on PATH; the live orchestrator needs the host docker daemon")
	}
	base := cliutil.EnvOr("WARDYN_E2E_BASE_URL", "http://localhost:8080")
	repoRoot, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	home, _ := os.UserHomeDir()
	h := &harness{
		t:         t,
		base:      base,
		token:     cliutil.EnvOr("WARDYN_ADMIN_TOKEN", "demo-admin-token"),
		http:      &http.Client{Timeout: 60 * time.Second},
		tasksDir:  cliutil.EnvOr("WARDYN_E2E_TASKS_DIR", filepath.Join(repoRoot, "test", "e2e", "tasks")),
		credsDir:  cliutil.EnvOr("WARDYN_E2E_CLAUDE_CREDS", filepath.Join(home, ".wardyn", "claude-creds")),
		workRoot:  cliutil.EnvOr("WARDYN_E2E_WORK_ROOT", filepath.Join(home, ".wardyn", "e2e-work")),
		realModel: os.Getenv("WARDYN_E2E_REAL_MODEL") == "1",
	}
	h.sdk = client.New(base, h.token)

	// Fail fast (not skip) if the stack is unreachable: WARDYN_TEST_DOCKER=1 is an
	// explicit "I brought a stack up" signal, so an unreachable one is a real error.
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if _, err := h.healthz(ctx); err != nil {
		t.Fatalf("cannot reach wardynd at %s (start it with scripts/run-host.sh): %v", base, err)
	}
	if err := os.MkdirAll(h.workRoot, 0o755); err != nil {
		t.Fatalf("create work root %s: %v", h.workRoot, err)
	}
	return h
}

// ── /healthz + confinement classes ──────────────────────────────────────────

type healthz struct {
	Status            string            `json:"status"`
	ConfinementClass  []string          `json:"confinement_classes"`
	ConfinementSubstr map[string]string `json:"confinement_substrates"`
}

func (h *harness) healthz(ctx context.Context) (healthz, error) {
	var hz healthz
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, h.base+"/healthz", nil)
	resp, err := h.http.Do(req)
	if err != nil {
		return hz, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return hz, fmt.Errorf("healthz status %d", resp.StatusCode)
	}
	return hz, json.NewDecoder(resp.Body).Decode(&hz)
}

// installedClasses returns the confinement classes the running stack can actually
// enforce (from /healthz — the control plane only advertises a class whose runtime
// is installed, fail-closed).
func (h *harness) installedClasses(ctx context.Context) map[string]bool {
	h.t.Helper()
	hz, err := h.healthz(ctx)
	if err != nil {
		h.t.Fatalf("healthz: %v", err)
	}
	set := map[string]bool{}
	for _, c := range hz.ConfinementClass {
		set[strings.ToUpper(c)] = true
	}
	return set
}

// ── task corpus ─────────────────────────────────────────────────────────────

// Task is the flat test/e2e/tasks/<name>/task.yaml schema (only the fields the
// orchestrator consumes; the interactive `expects` block is driven in code).
type Task struct {
	Name        string   `yaml:"name"`
	Agent       string   `yaml:"agent"`
	NeedsModel  bool     `yaml:"needs_model"`
	Interactive bool     `yaml:"interactive"`
	Launch      string   `yaml:"launch"`
	Tiers       []string `yaml:"tiers"`
	GraderImage string   `yaml:"grader_image"`
	Prompt      string   `yaml:"prompt"`

	dir string // absolute task directory (filled by loadTasks)
}

func (h *harness) loadTasks() []Task {
	h.t.Helper()
	entries, err := os.ReadDir(h.tasksDir)
	if err != nil {
		h.t.Fatalf("read tasks dir %s: %v", h.tasksDir, err)
	}
	var tasks []Task
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		p := filepath.Join(h.tasksDir, e.Name(), "task.yaml")
		b, err := os.ReadFile(p)
		if err != nil {
			continue // not a task dir
		}
		var tk Task
		if err := yaml.Unmarshal(b, &tk); err != nil {
			h.t.Fatalf("parse %s: %v", p, err)
		}
		tk.dir = filepath.Join(h.tasksDir, e.Name())
		tasks = append(tasks, tk)
	}
	if len(tasks) == 0 {
		h.t.Fatalf("no tasks found under %s", h.tasksDir)
	}
	return tasks
}

func (t Task) hasSolution() bool {
	_, err := os.Stat(filepath.Join(t.dir, "solution.sh"))
	return err == nil
}

// ── per-run workspace seeding ───────────────────────────────────────────────

// The workspace target inside the sandbox. Matches the composer + agent-run
// contract (agent-run cds into the first existing of /home/agent/work, ...).
const workspaceTarget = "/home/agent/work"

// seedWorkspace copies the task's seed workspace/ into a fresh per-run host dir
// under workRoot. For an oracle run it also stages solution.sh at the
// workspace-relative path the oracle agent-run executes (.wardyn-task/solution.sh).
// Returns the absolute host workspace dir (== the run's mount Source and the dir
// the grader inspects).
func (h *harness) seedWorkspace(task Task, runLabel string, oracle bool) string {
	h.t.Helper()
	ws := filepath.Join(h.workRoot, fmt.Sprintf("%s-%s-%d", task.Name, runLabel, time.Now().UnixNano()))
	if err := os.MkdirAll(ws, 0o755); err != nil {
		h.t.Fatalf("mkdir workspace: %v", err)
	}
	// Copy the seed workspace (may be just a .gitkeep).
	src := filepath.Join(task.dir, "workspace")
	if _, err := os.Stat(src); err == nil {
		copyTreeExec(h.t, src, ws)
	}
	if oracle {
		td := filepath.Join(ws, ".wardyn-task")
		if err := os.MkdirAll(td, 0o755); err != nil {
			h.t.Fatalf("mkdir .wardyn-task: %v", err)
		}
		copyFileExec(h.t, filepath.Join(task.dir, "solution.sh"), filepath.Join(td, "solution.sh"))
	}
	// ONBOARD the seeded dir so it passes the run-create mount gate
	// (validateWorkspaceSources rejects any local-dir mount source that is not a
	// pre-onboarded workspace). Every live lane mounts its seeded workspace, so
	// this is the single chokepoint that keeps the whole suite green under the gate
	// — the real operator flow (onboard, then mount), not a bypass.
	h.onboardLocalDir(ws)
	return ws
}

// onboardLocalDir registers a host dir as an onboarded local_dir workspace via
// the admin API so a run may mount it. The mount gate checks membership only (any
// status), so a plain create suffices — no scan needed. Sources are unique per
// run (seedWorkspace timestamps the dir), so this never collides; a 409 (already
// onboarded) is tolerated for re-use. Onboarded rows accumulate across runs
// (harmless; `make reset` clears them).
func (h *harness) onboardLocalDir(dir string) {
	h.t.Helper()
	body, _ := json.Marshal(map[string]any{
		"name":   "e2e-" + filepath.Base(dir),
		"kind":   "local_dir",
		"source": dir,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, h.base+"/api/v1/workspaces", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+h.token)
	resp, err := h.http.Do(req)
	if err != nil {
		h.t.Fatalf("onboard local dir %s: %v", dir, err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK, http.StatusCreated, http.StatusConflict:
		return // created, or already onboarded (idempotent)
	default:
		raw, _ := readCapped(resp.Body)
		h.t.Fatalf("onboard local dir %s: status %d: %s", dir, resp.StatusCode, raw)
	}
}

// copyTreeExec / copyFileExec shell out to cp so directory copies preserve the
// exact bytes + perms the graders expect (matches how the corpus was verified).
func copyTreeExec(t *testing.T, src, dst string) {
	t.Helper()
	// `cp -a src/. dst` copies CONTENTS (incl. dotfiles) into dst.
	out, err := exec.Command("cp", "-a", src+"/.", dst).CombinedOutput()
	if err != nil {
		t.Fatalf("cp -a %s -> %s: %v (%s)", src, dst, err, out)
	}
}

func copyFileExec(t *testing.T, src, dst string) {
	t.Helper()
	out, err := exec.Command("cp", "-a", src, dst).CombinedOutput()
	if err != nil {
		t.Fatalf("cp -a %s -> %s: %v (%s)", src, dst, err, out)
	}
}

// ── policy construction ─────────────────────────────────────────────────────

func boolPtr(b bool) *bool { return &b }

// subscriptionMounts returns the read-only Claude credential mounts for the
// subscription real-model lane, or nil if the operator hasn't staged creds
// (scripts/stage-claude-creds.sh). The manual real-model lane needs these; the
// oracle lane and the composer lane (which injects them server-side) do not.
func (h *harness) subscriptionMounts() []types.WorkspaceMount {
	credDir := filepath.Join(h.credsDir, ".claude")
	credJSON := filepath.Join(h.credsDir, ".claude.json")
	if _, err := os.Stat(credDir); err != nil {
		return nil
	}
	if _, err := os.Stat(credJSON); err != nil {
		return nil
	}
	return []types.WorkspaceMount{
		{Source: credDir, Target: "/home/agent/.claude", ReadOnly: boolPtr(true)},
		{Source: credJSON, Target: "/home/agent/.claude.json", ReadOnly: boolPtr(true)},
	}
}

// egressForTask returns the allowed_domains for a MANUAL run of a task. The
// egress-boundary task deliberately allows ONLY github.com (so evil.example.com
// and the metadata IP are denied — the allow/block proof); model tasks in the
// subscription lane allow anthropic; oracle model-less tasks need no egress.
func egressForTask(task Task, wantModel bool) []string {
	switch task.Name {
	case "egress-boundary", "interactive-repl":
		// Allow ONE host so a denied host proves SELECTIVE blocking (not a
		// deny-everything proxy): github.com allowed, everything else denied.
		return []string{"github.com"}
	default:
		if wantModel {
			return []string{"api.anthropic.com", "*.anthropic.com"}
		}
		return nil
	}
}

// buildManualPolicy assembles the inline RunPolicySpec for a manual launch.
func (h *harness) buildManualPolicy(task Task, class, wsDir string, wantModel, interactive bool) types.RunPolicySpec {
	mounts := []types.WorkspaceMount{
		{Source: wsDir, Target: workspaceTarget, ReadOnly: boolPtr(false)},
	}
	if wantModel {
		mounts = append(mounts, h.subscriptionMounts()...)
	}
	// Default unattended posture: an unknown host HARD-denies (always_deny — never
	// blocks on an approval nobody will answer). The egress-boundary task instead
	// uses deny_with_review so a denied host is HELD with a clean 403 (the
	// documented e2e.sh behavior its grader asserts) + an egress.pending audit
	// event, without blocking the short probe. (first_use_approval became a
	// three-mode enum; legacy true→deny_with_review, false→always_deny.)
	firstUse := types.FirstUseAlwaysDeny
	if task.Name == "egress-boundary" {
		firstUse = types.FirstUseDenyWithReview
	}
	spec := types.RunPolicySpec{
		MinConfinementClass: types.ConfinementClass(class),
		AllowedDomains:      egressForTask(task, wantModel),
		FirstUseApproval:    firstUse,
		WorkspaceMounts:     mounts,
	}
	if interactive {
		spec.AutoStopAfterSec = -1 // never reap an idle interactive sandbox
	}
	return spec
}

// ── launch: manual + composer ───────────────────────────────────────────────

// launchManual creates a run directly via the SDK with an inline policy.
func (h *harness) launchManual(ctx context.Context, agent, task, class string, spec types.RunPolicySpec, interactive bool) types.AgentRun {
	h.t.Helper()
	run, err := h.sdk.CreateRun(ctx, client.CreateRunRequest{
		Agent:            agent,
		Repo:             "local:e2e",
		Task:             task,
		ConfinementClass: class,
		Interactive:      interactive,
		InlinePolicy:     &spec,
	})
	if err != nil {
		h.t.Fatalf("CreateRun(agent=%s class=%s): %v", agent, class, err)
	}
	return run
}

// composeProposal is the subset of POST /runs/compose we consume.
type composeProposal struct {
	Kind     string `json:"kind"`
	Proposed struct {
		Run struct {
			Agent            string `json:"agent"`
			Repo             string `json:"repo"`
			Task             string `json:"task"`
			ConfinementClass string `json:"confinement_class"`
			Interactive      bool   `json:"interactive"`
		} `json:"run"`
		InlinePolicy types.RunPolicySpec `json:"inline_policy"`
	} `json:"proposed"`
	Warnings []string `json:"warnings"`
}

// compose calls POST /api/v1/runs/compose (not in the SDK) and returns the
// proposal. useSubscription drives the per-run subscription opt-in.
func (h *harness) compose(ctx context.Context, prompt, wsPath string, useSubscription bool) (composeProposal, error) {
	body := map[string]any{
		"prompt":           prompt,
		"workspace":        map[string]any{"kind": "local", "path": wsPath, "read_write": true},
		"mode":             "skip",
		"use_subscription": useSubscription,
	}
	b, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, h.base+"/api/v1/runs/compose", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+h.token)
	// Composer calls a real model; give it room.
	cl := &http.Client{Timeout: 90 * time.Second}
	resp, err := cl.Do(req)
	if err != nil {
		return composeProposal{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		raw, _ := readCapped(resp.Body)
		return composeProposal{}, fmt.Errorf("compose status %d: %s", resp.StatusCode, raw)
	}
	var p composeProposal
	if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
		return composeProposal{}, err
	}
	return p, nil
}

var classRank = map[string]int{"": 0, "CC1": 1, "CC2": 2, "CC3": 3}

// launchComposer composes a proposal for the task then launches it — the human
// "review, then approve & launch" flow, minus the human. The one operator
// decision it makes explicit: if the composer's RISK model proposed a stronger
// confinement class than this host can enforce (e.g. it wants Wall for a
// subscription-cred build, on a Fence-only box), floor the run + policy to the
// host's best class — exactly what a real operator does at the review step on a
// CC1-only machine — and log it, because Wall/Vault isolation is then UNVERIFIED.
// Returns a non-empty skipReason (instead of failing) when the composer's own
// ANALYSIS backend (the host claude CLI) flakes — a composer-robustness issue
// orthogonal to the sandbox boundary this suite verifies.
func (h *harness) launchComposer(ctx context.Context, task Task, wsPath, bestClass string) (run types.AgentRun, p composeProposal, skipReason string) {
	h.t.Helper()
	p, err := h.compose(ctx, task.Prompt, wsPath, true /* use subscription on this box */)
	if err != nil {
		if isComposerBackendFlake(err) {
			return run, p, err.Error()
		}
		h.t.Fatalf("compose(%s): %v", task.Name, err)
	}
	if p.Kind != "proposal" {
		h.t.Fatalf("compose(%s): expected a proposal, got kind=%q", task.Name, p.Kind)
	}
	runClass := p.Proposed.Run.ConfinementClass
	spec := p.Proposed.InlinePolicy
	if classRank[runClass] > classRank[bestClass] || classRank[string(spec.MinConfinementClass)] > classRank[bestClass] {
		h.t.Logf("composer proposed confinement %q (policy floor %q); this host's best is %q — "+
			"flooring the run to %q for the test. Isolation above %q is UNVERIFIED here.",
			runClass, spec.MinConfinementClass, bestClass, bestClass, bestClass)
		runClass = bestClass
		spec.MinConfinementClass = types.ConfinementClass(bestClass)
	}
	run, err = h.sdk.CreateRun(ctx, client.CreateRunRequest{
		Agent:            p.Proposed.Run.Agent,
		Repo:             p.Proposed.Run.Repo,
		Task:             p.Proposed.Run.Task,
		ConfinementClass: runClass,
		Interactive:      p.Proposed.Run.Interactive,
		InlinePolicy:     &spec,
	})
	if err != nil {
		h.t.Fatalf("CreateRun(composed %s): %v", task.Name, err)
	}
	return run, p, ""
}

// isComposerBackendFlake reports whether a compose() error is the host claude-CLI
// analysis backend flaking (502 + the CLI's own error envelope) rather than a
// real proposal/policy/validation defect.
func isComposerBackendFlake(err error) bool {
	s := err.Error()
	return strings.Contains(s, "compose status 502") &&
		(strings.Contains(s, "composer backend") || strings.Contains(s, "max_turns") || strings.Contains(s, "claude exited"))
}

// bestInstalledClass returns the strongest confinement class the running stack
// can enforce (from /healthz).
func (h *harness) bestInstalledClass(ctx context.Context) string {
	best := "CC1"
	for c := range h.installedClasses(ctx) {
		if classRank[c] > classRank[best] {
			best = c
		}
	}
	return best
}

// ── poll to terminal ────────────────────────────────────────────────────────

func isTerminal(s types.RunState) bool {
	switch s {
	case types.RunCompleted, types.RunFailed, types.RunKilled, types.RunStopped, types.RunArchived:
		return true
	}
	return false
}

// pollTerminal polls GetRun until the run reaches a terminal state or timeout.
func (h *harness) pollTerminal(id uuid.UUID, timeout time.Duration) types.AgentRun {
	h.t.Helper()
	deadline := time.Now().Add(timeout)
	var last types.AgentRun
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		run, err := h.sdk.GetRun(ctx, id)
		cancel()
		if err == nil {
			last = run
			if isTerminal(run.State) {
				return run
			}
		}
		time.Sleep(2 * time.Second)
	}
	h.t.Fatalf("run %s did not reach a terminal state within %s (last=%s)", id, timeout, last.State)
	return last
}

// ── grade ───────────────────────────────────────────────────────────────────

// gradeExit runs a grader in a FRESH disposable container against a workspace
// dir (read-only) with the task dir mounted read-only and returns its EXIT CODE
// (0 = pass; grade.sh exits 1 on a graded failure) plus output. It bounds the
// run so a hung grader can't stall to the Go test timeout. An exit code >= 125
// is a docker infra error (missing image / mount), NOT a graded verdict — the
// callers distinguish it so an infra hiccup can never impersonate pass OR a
// correct rejection.
func (h *harness) gradeExit(image, taskDir, wsDir string) (int, string) {
	h.t.Helper()
	if image == "" {
		image = "alpine:3.20"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", "run", "--rm",
		"-v", wsDir+":/ws:ro",
		"-v", taskDir+":/task:ro",
		image, "sh", "/task/grade.sh")
	out, err := cmd.CombinedOutput()
	if err == nil {
		return 0, string(out)
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), string(out)
	}
	return -1, string(out) // could not run the grader at all
}

// grade runs the task's grader against the run's final workspace state.
// Exit 0 = the agent's work is correct. A non-zero (incl. docker infra) exit is
// a failure — never a false pass.
func (h *harness) grade(task Task, wsDir string) (bool, string) {
	h.t.Helper()
	code, out := h.gradeExit(task.GraderImage, task.dir, wsDir)
	return code == 0, out
}

// ── audit ───────────────────────────────────────────────────────────────────

// auditEgress counts egress decision events on a run (allow/deny/pending) via the
// API — the control-plane-side proof that the proxy actually enforced the
// boundary (complementing the in-sandbox evidence files the grader checks).
func (h *harness) auditEgress(id uuid.UUID) (allow, deny, pending int) {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	events, err := h.sdk.AuditEvents(ctx, id)
	if err != nil {
		h.t.Fatalf("AuditEvents(%s): %v", id, err)
	}
	for _, e := range events {
		switch e.Action {
		case "egress.allow":
			allow++
		case "egress.deny":
			deny++
		case "egress.pending":
			pending++
		}
	}
	return
}

// ── fail-closed tier assertion ──────────────────────────────────────────────

// expectFailClosed asserts the control plane REFUSES to schedule a run at a class
// whose runtime is not installed — a 422 (no silent downgrade). This is what
// "CC2/CC3 tested" honestly means on a CC1-only host: the scheduler correctly
// refuses, though it does NOT prove Wall/Vault confine an agent.
func (h *harness) expectFailClosed(class string) {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, err := h.sdk.CreateRun(ctx, client.CreateRunRequest{
		Agent:            "oracle",
		Repo:             "local:e2e",
		Task:             "noop",
		ConfinementClass: class,
		InlinePolicy: &types.RunPolicySpec{
			MinConfinementClass: types.ConfinementClass(class),
			FirstUseApproval:    types.FirstUseAlwaysDeny,
		},
	})
	if err == nil {
		h.t.Errorf("%s: expected a fail-closed rejection (runtime not installed), but the run was accepted", class)
		return
	}
	var apiErr *client.APIError
	if !errors.As(err, &apiErr) {
		h.t.Errorf("%s: expected an APIError, got %T: %v", class, err, err)
		return
	}
	if apiErr.Status != http.StatusUnprocessableEntity {
		h.t.Errorf("%s: expected 422 fail-closed, got %d: %s", class, apiErr.Status, apiErr.Body)
		return
	}
	// Assert the 422 is the RUNTIME gate, not some other 422 in the path — the
	// request is otherwise valid (no grants, class == floor), so this message can
	// only come from the capability check (handleCreateRun).
	if !strings.Contains(apiErr.Body, "cannot enforce confinement_class") {
		h.t.Errorf("%s: 422 body is not the runtime fail-closed reason: %s", class, apiErr.Body)
	}
}

// readCapped reads up to 4 KiB from an error-response body for diagnostics.
func readCapped(r io.Reader) (string, error) {
	raw, err := io.ReadAll(io.LimitReader(r, 4096))
	return string(raw), err
}

// errorsIsDeadline reports whether err is a context deadline (a WS read timeout
// between output bursts, expected and non-fatal in driveExpect).
func errorsIsDeadline(err error) bool {
	return errors.Is(err, context.DeadlineExceeded)
}
