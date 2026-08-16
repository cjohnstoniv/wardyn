// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"math"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// newResourcesHarness builds a Server wired to an in-memory authzStore and an
// sshFakeRunner (reused from sshgateway_test.go — Attach/ExecStream-capable,
// exactly what this handler needs) whose ExecStream is execFn. cfg.OIDC is set
// so SSO session cookies (ssoSession/doSSO, from rbac_test.go) authenticate a
// specific member/admin principal — the admin bearer token alone cannot model
// a non-owning MEMBER, since an admin-token caller is always an operator
// (see http.go's isOperator doc).
func newResourcesHarness(t *testing.T, execFn func(runner.ExecSpec) (*runner.ExecSession, error)) (*Server, *authzStore, *harness) {
	t.Helper()
	ast := newAuthzStore()
	h := newHarness(t)
	cfg := baseTestConfig(h, ast)
	cfg.OIDC = &oidc.Authenticator{}
	cfg.Runner = &sshFakeRunner{execFn: execFn}
	return New(cfg), ast, h
}

func seedResourcesRun(ast *authzStore, createdBy string) uuid.UUID {
	id := uuid.New()
	ast.mu.Lock()
	ast.runs[id] = types.AgentRun{ID: id, CreatedBy: createdBy, State: types.RunRunning, SandboxRef: "sbx-1"}
	ast.mu.Unlock()
	return id
}

// kvExecSession is a minimal ExecSession backing a canned `key=value` stdout —
// ExecSession is documented as a plain, fake-friendly struct precisely for
// this: a strings.Reader Stdout, no Stdin, and a nil Stderr are all valid
// zero-effort fields (run_resources.go's execRunResourcesScript guards every
// one of them).
func kvExecSession(stdout string) *runner.ExecSession {
	return &runner.ExecSession{
		Stdout: strings.NewReader(stdout),
		Wait:   func() (int, error) { return 0, nil },
	}
}

// TestRunResources_FullKeySet pins the "a full key set parses into the right
// numbers" case: every metric present, computed against hand-checked values.
func TestRunResources_FullKeySet(t *testing.T) {
	kv := "nproc=4\n" +
		"cpu_usage_usec_1=1000000\nuptime_1=10.00\n" +
		"cpu_usage_usec_2=1100000\nuptime_2=10.20\n" +
		"memory_current=104857600\nmemory_max=209715200\nmem_total_kb=32000000\n" +
		"disk_wbytes=5242880\n" +
		"proc_count=42\n"
	srv, ast, h := newResourcesHarness(t, func(runner.ExecSpec) (*runner.ExecSession, error) { return kvExecSession(kv), nil })
	id := seedResourcesRun(ast, "alice")

	w := do(t, srv, http.MethodGet, "/api/v1/runs/"+id.String()+"/resources", adminToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var got runResourcesResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v; body=%s", err, w.Body.String())
	}

	// 100000usec CPU delta over a 0.20s (200000usec) wall delta on 4 CPUs:
	// (100000/200000)/4*100 = 12.5%. Tolerance, not exact equality: uptime_1/
	// uptime_2 are parsed as float64 seconds, and 10.20-10.00 is not exactly
	// representable in binary floating point.
	if got.CPUPercent == nil || math.Abs(*got.CPUPercent-12.5) > 0.001 {
		t.Errorf("CPUPercent = %v, want ~12.5", got.CPUPercent)
	}
	if got.MemoryUsedBytes == nil || *got.MemoryUsedBytes != 104857600 {
		t.Errorf("MemoryUsedBytes = %v, want 104857600", got.MemoryUsedBytes)
	}
	if got.MemoryLimitBytes == nil || *got.MemoryLimitBytes != 209715200 {
		t.Errorf("MemoryLimitBytes = %v, want 209715200 (numeric memory.max, no MemTotal fallback needed)", got.MemoryLimitBytes)
	}
	if got.DiskWrittenBytes == nil || *got.DiskWrittenBytes != 5242880 {
		t.Errorf("DiskWrittenBytes = %v, want 5242880", got.DiskWrittenBytes)
	}
	if got.ProcessCount == nil || *got.ProcessCount != 42 {
		t.Errorf("ProcessCount = %v, want 42", got.ProcessCount)
	}

	// Success is not audited — the console polls this endpoint; see the
	// audit-on-failure-only test below for the contrast case.
	for _, ev := range h.audit.events {
		if ev.Action == "run.resources" {
			t.Errorf("unexpected run.resources audit event on a SUCCESSFUL read: %+v", ev)
		}
	}
}

// TestRunResources_PartialKeySet_AbsentNotZero is THE honesty test: a partial
// key set (modeling gVisor/CC3 withholding the cgroup files it doesn't
// synthesize) must leave those fields entirely ABSENT from the JSON, never
// present as a lying 0. Asserted on the raw marshalled body — a bug that
// dropped the pointer/omitempty and fell back to a bare int would still pass
// a decode-then-check-nil test (a bare 0 decodes into "zero value" same as a
// omitted field would for some sloppy comparisons), so the wire bytes are
// the only place this particular lie is visible.
func TestRunResources_PartialKeySet_AbsentNotZero(t *testing.T) {
	// Only nproc and proc_count reported: no cpu.stat, no memory.current/.max,
	// no io.stat — exactly what a synthetic gVisor sysfs/cgroupfs looks like,
	// while /proc/cpuinfo and /proc/[pid] (real procfs entries) still work.
	kv := "nproc=2\nproc_count=7\n"
	srv, ast, _ := newResourcesHarness(t, func(runner.ExecSpec) (*runner.ExecSession, error) { return kvExecSession(kv), nil })
	id := seedResourcesRun(ast, "alice")

	w := do(t, srv, http.MethodGet, "/api/v1/runs/"+id.String()+"/resources", adminToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	for _, absentKey := range []string{`"cpu_percent"`, `"memory_used_bytes"`, `"memory_limit_bytes"`, `"disk_written_bytes"`} {
		if strings.Contains(body, absentKey) {
			t.Errorf("body contains %s, want it entirely absent (not even a 0) when the sandbox never reported it; body=%s", absentKey, body)
		}
	}
	if !strings.Contains(body, `"process_count":7`) {
		t.Errorf("body missing process_count:7 (the one metric that WAS reported); body=%s", body)
	}
}

// TestRunResources_MemoryMaxFallsBackToMemTotal covers both memory.max
// branches: a literal "max" (unlimited cgroup) falls back to the sandbox's
// own MemTotal, and — the honesty corollary — a literal "max" WITHOUT a
// readable MemTotal has no fallback to fabricate from, so the limit stays
// absent rather than guessing.
func TestRunResources_MemoryMaxFallsBackToMemTotal(t *testing.T) {
	t.Run("MemTotal available", func(t *testing.T) {
		kv := "nproc=1\nmemory_current=1048576\nmemory_max=max\nmem_total_kb=16000000\n"
		srv, ast, _ := newResourcesHarness(t, func(runner.ExecSpec) (*runner.ExecSession, error) { return kvExecSession(kv), nil })
		id := seedResourcesRun(ast, "alice")

		w := do(t, srv, http.MethodGet, "/api/v1/runs/"+id.String()+"/resources", adminToken, "")
		var got runResourcesResponse
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v; body=%s", err, w.Body.String())
		}
		const wantBytes = int64(16000000) * 1024
		if got.MemoryLimitBytes == nil || *got.MemoryLimitBytes != wantBytes {
			t.Errorf("MemoryLimitBytes = %v, want %d (MemTotal fallback)", got.MemoryLimitBytes, wantBytes)
		}
		if got.MemoryUsedBytes == nil || *got.MemoryUsedBytes != 1048576 {
			t.Errorf("MemoryUsedBytes = %v, want 1048576", got.MemoryUsedBytes)
		}
	})

	t.Run("MemTotal unavailable too", func(t *testing.T) {
		kv := "nproc=1\nmemory_current=1048576\nmemory_max=max\n" // no mem_total_kb
		srv, ast, _ := newResourcesHarness(t, func(runner.ExecSpec) (*runner.ExecSession, error) { return kvExecSession(kv), nil })
		id := seedResourcesRun(ast, "alice")

		w := do(t, srv, http.MethodGet, "/api/v1/runs/"+id.String()+"/resources", adminToken, "")
		if strings.Contains(w.Body.String(), `"memory_limit_bytes"`) {
			t.Errorf("memory_limit_bytes present with no fallback data to compute it from; body=%s", w.Body.String())
		}
	})
}

// TestRunResources_ExecStreamUnsupported_Returns501 pins the confinement-tier
// escape hatch: a substrate that cannot ExecStream at all (errors.Is against
// runner.ErrExecStreamUnsupported) is a clean 501 naming the reason, not a
// 500 or a hang.
func TestRunResources_ExecStreamUnsupported_Returns501(t *testing.T) {
	srv, ast, h := newResourcesHarness(t, func(runner.ExecSpec) (*runner.ExecSession, error) {
		return nil, runner.ErrExecStreamUnsupported
	})
	id := seedResourcesRun(ast, "alice")

	w := do(t, srv, http.MethodGet, "/api/v1/runs/"+id.String()+"/resources", adminToken, "")
	if w.Code != http.StatusNotImplemented {
		t.Fatalf("code = %d, want 501; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), runner.ErrExecStreamUnsupported.Error()) {
		t.Errorf("501 body does not carry the unsupported reason; body=%s", w.Body.String())
	}

	// FAILURE is audited (unlike the success path above).
	ev := lastAuditEvent(t, h.audit.events, "run.resources")
	if ev.Outcome != "failure" {
		t.Errorf("run.resources audit outcome = %q, want failure", ev.Outcome)
	}
}

// TestRunResources_ForeignRun404 pins owner-or-admin: a member who did not
// create the run gets the same 404 a nonexistent run would, and ExecStream is
// never even attempted against a sandbox that request has no business
// reading.
func TestRunResources_ForeignRun404(t *testing.T) {
	execCalls := 0
	srv, ast, _ := newResourcesHarness(t, func(runner.ExecSpec) (*runner.ExecSession, error) {
		execCalls++
		return kvExecSession("proc_count=1\n"), nil
	})
	id := seedResourcesRun(ast, "owner-sub")

	mallory := ssoSession(t, "mallory-sub", "mallory@corp.example", oidc.RoleMember)
	w := doSSO(t, srv, http.MethodGet, "/api/v1/runs/"+id.String()+"/resources", mallory, "")
	if w.Code != http.StatusNotFound {
		t.Fatalf("non-owning member: code = %d, want 404; body=%s", w.Code, w.Body.String())
	}

	owner := ssoSession(t, "owner-sub", "owner@corp.example", oidc.RoleMember)
	if w := doSSO(t, srv, http.MethodGet, "/api/v1/runs/"+id.String()+"/resources", owner, ""); w.Code != http.StatusOK {
		t.Fatalf("owning member: code = %d, want 200 (contrast case — the run itself is reachable); body=%s", w.Code, w.Body.String())
	}

	if execCalls != 1 {
		t.Errorf("ExecStream calls = %d, want exactly 1 (only the owner's request, never the denied one)", execCalls)
	}
}

// TestRunResources_StderrDrainedBeforeStdout is the hang-if-wrong test: it
// reuses fakeExecSession (sshgateway_test.go) with UNBUFFERED io.Pipes and
// stderr written BEFORE any stdout byte. If execRunResourcesScript read
// Stdout without draining Stderr concurrently, the goroutine feeding stderr
// would block forever on its write (nobody reading stderrR), stdout would
// never arrive, and this test would hang until the suite's own timeout kills
// it — exactly the HARD CONTRACT called out on ExecSession.Stderr.
func TestRunResources_StderrDrainedBeforeStdout(t *testing.T) {
	srv, ast, _ := newResourcesHarness(t, func(runner.ExecSpec) (*runner.ExecSession, error) {
		return fakeExecSession("nproc=2\ndisk_wbytes=99\n", "diagnostic noise on stderr\n", 0), nil
	})
	id := seedResourcesRun(ast, "alice")

	w := do(t, srv, http.MethodGet, "/api/v1/runs/"+id.String()+"/resources", adminToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var got runResourcesResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v; body=%s", err, w.Body.String())
	}
	if got.DiskWrittenBytes == nil || *got.DiskWrittenBytes != 99 {
		t.Errorf("DiskWrittenBytes = %v, want 99 (stdout must still parse correctly past the stderr drain)", got.DiskWrittenBytes)
	}
}
