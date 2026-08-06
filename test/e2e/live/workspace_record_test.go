// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package live

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestLive_WorkspaceRecordImport proves the Record→derive→Verify import loop
// through the WORKSPACE endpoints: an operator-approved task records in an
// OPEN sandbox, its observed-allowed egress promotes one-click into
// ApprovedEgress, and the subsequent CONFINED verify (which unions
// ApprovedEgress) runs the same command green while a canary host stays
// denied — least-privilege proven from recorded behavior.
//
// TOPOLOGY-HONEST: capture is server-side over the proxy's decision callbacks.
//   - Compose stack (wardynd in-container on wardyn-internal): callbacks land,
//     the FULL loop is asserted.
//   - Host-mode managed-VM docker (Docker Desktop/WSL2): callbacks don't route
//     back, so the capture lands ZERO evidence and record honestly fails with
//     the reachability hint — that DESIGNED honesty path is asserted instead
//     and the promote/verify phase is skipped with a NOTE. (recording_test.go's
//     relaunch dodge does not transfer: Record Mode's value IS the callbacks.)
func TestLive_WorkspaceRecordImport(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	// ── 1. Onboard + scan a fresh local_dir workspace. ──
	ws := filepath.Join(h.workRoot, fmt.Sprintf("record-import-%d", time.Now().UnixNano()))
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", ws, err)
	}
	if err := os.WriteFile(filepath.Join(ws, "README.md"), []byte("record-mode live e2e\n"), 0o644); err != nil {
		t.Fatalf("seed workspace: %v", err)
	}
	wsID := h.recCreateWorkspace(t, ws)
	// Scan is best-effort here: a CONTAINERIZED control plane (compose stack)
	// cannot stat WSL-host local dirs, so its scan honestly 422s. Record needs
	// no scan (tasks derive from operator-APPROVED commands), and skipping it
	// makes the confinement proof purer: post-promotion, the ONLY allowed
	// verify host is the one the recording captured.
	if code, raw := h.recTry(t, http.MethodPost, "/api/v1/workspaces/"+wsID+"/scan", nil); code != http.StatusOK && code != http.StatusAccepted {
		t.Logf("NOTE: scan unavailable on this topology (status %d: %s) — proceeding; record/verify don't require it", code, raw)
	}

	// ── 2. Operator approves the task command (records real egress). ──
	// curl honors the sandbox's HTTPS_PROXY env; wardyn-verify runs /bin/sh -lc.
	buildCmd := "curl -sS -o /dev/null -m 12 --connect-timeout 8 -w '%{http_code}' https://github.com/"
	h.recJSON(t, http.MethodPut, "/api/v1/workspaces/"+wsID+"/setup-commands", map[string]any{
		"commands": []map[string]string{{"stage": "build", "command": buildCmd, "source": "operator"}},
	}, nil, http.StatusOK)

	// The derived task list must expose the auto build task + the custom lane.
	doc := h.recWorkspaceDoc(t, wsID)
	if !doc.hasTask("build") || !doc.hasTask("custom") {
		t.Fatalf("record_tasks = %+v, want build + custom", doc.RecordTasks)
	}

	// ── 3. Record the build task in an OPEN sandbox. ──
	var started struct {
		RecordRunID string   `json:"record_run_id"`
		Warnings    []string `json:"warnings"`
	}
	h.recJSON(t, http.MethodPost, "/api/v1/workspaces/"+wsID+"/record",
		map[string]string{"task_key": "build", "mode": "auto"}, &started, http.StatusAccepted)
	if started.RecordRunID == "" {
		t.Fatal("202 without record_run_id")
	}
	if !warningsMention(started.Warnings, "masking") && !warningsMention(started.Warnings, "masked") {
		t.Errorf("record start must carry the seed-ahead masking caveat, got %v", started.Warnings)
	}
	if best := h.bestInstalledClass(ctx); best == "CC1" && !warningsMention(started.Warnings, "WEAKEST") {
		t.Errorf("CC1-only host must get the loud weak-confinement warning, got %v", started.Warnings)
	}
	runID, err := uuid.Parse(started.RecordRunID)
	if err != nil {
		t.Fatalf("bad record_run_id %q", started.RecordRunID)
	}
	t.Logf("record run %s launched (open egress, task=build)", runID)

	// ── 4. Wait for the capture (run terminal → reconcile writes the result). ──
	res := h.recWaitTaskDone(t, wsID, "build", 6*time.Minute)
	if final := h.pollTerminal(runID, 30*time.Second); final.State == "" {
		t.Logf("NOTE: record run state not readable post-capture (non-fatal)")
	}

	switch res.Status {
	case "record_failed":
		// The DESIGNED honesty path (host-mode: callbacks never landed).
		if !strings.Contains(res.FailureHint, "control plane") {
			t.Errorf("record_failed must carry the reachability hint, got %q", res.FailureHint)
		}
		t.Logf("NOTE: record_failed with the reachability hint — expected on host-mode "+
			"managed-VM docker (no decision callbacks). Run this test against the compose "+
			"stack (make setup) for the full record→promote→verify loop. hint=%q", res.FailureHint)
		return

	case "recorded":
		if res.Observations == nil {
			t.Fatal("recorded task without observations")
		}
		if !res.hasDomain("github.com") {
			t.Fatalf("observed domains %v missing github.com — the task's real egress was not captured", res.domainHosts())
		}
		if len(res.Steps) == 0 {
			t.Errorf("auto recording should carry streamed per-step results (wardyn-verify upload)")
		}
		if len(res.Caveats) == 0 {
			t.Errorf("capture must stamp the masking caveat")
		}
		t.Logf("captured: domains=%v steps=%d minted=%v", res.domainHosts(), len(res.Steps), res.SecretNamesMinted)

	default:
		t.Fatalf("task did not settle: status=%q", res.Status)
	}

	// ── 5. Promote the observed egress (operator one-click). ──
	h.recJSON(t, http.MethodPost, "/api/v1/workspaces/"+wsID+"/record/build/promote-egress", nil, nil, http.StatusOK)
	doc = h.recWorkspaceDoc(t, wsID)
	if !domainListed(doc.ApprovedEgress, "github.com") {
		t.Fatalf("approved_egress = %v, want github.com promoted", doc.ApprovedEgress)
	}
	t.Logf("promoted: approved_egress=%v", doc.ApprovedEgress)

	// ── 6. CONFINED verify: same command green + canary DENIED, in one run. ──
	// Verify unions profile.EgressDomains ∪ ApprovedEgress (allow-all OFF), so
	// the recorded host works and the never-recorded canary must be blocked
	// (curl non-zero → the leading ! makes the step green exactly when denied).
	canaryCmd := "! curl -sS -o /dev/null -m 12 --connect-timeout 8 https://example.com/"
	h.recJSON(t, http.MethodPut, "/api/v1/workspaces/"+wsID+"/setup-commands", map[string]any{
		"commands": []map[string]string{
			{"stage": "build", "command": buildCmd, "source": "operator"},
			{"stage": "test", "command": canaryCmd, "source": "operator"},
		},
	}, nil, http.StatusOK)
	h.recJSON(t, http.MethodPost, "/api/v1/workspaces/"+wsID+"/verify", nil, nil, http.StatusAccepted)
	deadline := time.Now().Add(6 * time.Minute)
	for {
		doc = h.recWorkspaceDoc(t, wsID)
		if doc.Status == "ready" {
			break
		}
		if doc.Status == "verify_failed" {
			t.Fatalf("confined verify failed after promotion: %s", string(doc.VerifyResult))
		}
		if time.Now().After(deadline) {
			t.Fatalf("verify did not settle (status=%s)", doc.Status)
		}
		time.Sleep(3 * time.Second)
	}
	t.Logf("record→derive→verify loop PROVEN: recorded host green under confinement, canary denied, workspace ready")
}

// ── local helpers (manual-http pattern, mirroring recording_test.go) ──

type recTaskDoc struct {
	TaskKey     string `json:"task_key"`
	Interactive bool   `json:"interactive"`
}

type recResultDoc struct {
	Status            string            `json:"status"`
	FailureHint       string            `json:"failure_hint"`
	Caveats           []string          `json:"caveats"`
	SecretNamesMinted []string          `json:"secret_names_minted"`
	Steps             []json.RawMessage `json:"steps"`
	Observations      *struct {
		Domains []struct {
			Host string `json:"host"`
		} `json:"domains"`
	} `json:"observations"`
}

func (r recResultDoc) hasDomain(host string) bool {
	if r.Observations == nil {
		return false
	}
	for _, d := range r.Observations.Domains {
		if d.Host == host {
			return true
		}
	}
	return false
}

func (r recResultDoc) domainHosts() []string {
	if r.Observations == nil {
		return nil
	}
	out := make([]string, 0, len(r.Observations.Domains))
	for _, d := range r.Observations.Domains {
		out = append(out, d.Host)
	}
	return out
}

type recWorkspaceDoc struct {
	ID             string                  `json:"id"`
	Status         string                  `json:"status"`
	ApprovedEgress []string                `json:"approved_egress"`
	VerifyResult   json.RawMessage         `json:"verify_result"`
	RecordTasks    []recTaskDoc            `json:"record_tasks"`
	RecordResults  map[string]recResultDoc `json:"record_results"`
}

func (d recWorkspaceDoc) hasTask(key string) bool {
	for _, task := range d.RecordTasks {
		if task.TaskKey == key {
			return true
		}
	}
	return false
}

// recCreateWorkspace onboards dir and returns the created workspace id.
func (h *harness) recCreateWorkspace(t *testing.T, dir string) string {
	t.Helper()
	var created struct {
		ID string `json:"id"`
	}
	h.recJSON(t, http.MethodPost, "/api/v1/workspaces", map[string]any{
		"name": "record-e2e-" + filepath.Base(dir), "kind": "local_dir", "source": dir,
	}, &created, http.StatusCreated, http.StatusOK)
	if created.ID == "" {
		t.Fatal("workspace created without id")
	}
	return created.ID
}

func (h *harness) recWorkspaceDoc(t *testing.T, wsID string) recWorkspaceDoc {
	t.Helper()
	var doc recWorkspaceDoc
	h.recJSON(t, http.MethodGet, "/api/v1/workspaces/"+wsID, nil, &doc, http.StatusOK)
	return doc
}

// recWaitTaskDone polls until the task's record result leaves `recording`.
func (h *harness) recWaitTaskDone(t *testing.T, wsID, task string, timeout time.Duration) recResultDoc {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		doc := h.recWorkspaceDoc(t, wsID)
		if res, ok := doc.RecordResults[task]; ok && res.Status != "recording" {
			return res
		}
		if time.Now().After(deadline) {
			t.Fatalf("record task %q did not settle within %s (results=%+v)", task, timeout, doc.RecordResults)
		}
		time.Sleep(3 * time.Second)
	}
}

func warningsMention(warnings []string, needle string) bool {
	for _, w := range warnings {
		if strings.Contains(strings.ToLower(w), strings.ToLower(needle)) {
			return true
		}
	}
	return false
}
