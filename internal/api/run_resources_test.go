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

// newResourcesHarnessWithDisk is newResourcesHarness plus a driver
// EphemeralDiskEnforcement word — RL-13's enforcedDiskWord reads it via
// Runner.Capabilities.
func newResourcesHarnessWithDisk(t *testing.T, execFn func(runner.ExecSpec) (*runner.ExecSession, error), enforcement types.StorageEnforcement) (*Server, *authzStore, *harness) {
	t.Helper()
	ast := newAuthzStore()
	h := newHarness(t)
	cfg := baseTestConfig(h, ast)
	cfg.OIDC = &oidc.Authenticator{}
	cfg.Runner = &sshFakeRunner{execFn: execFn, diskEnforcement: enforcement}
	return New(cfg), ast, h
}

func seedResourcesRun(ast *authzStore, createdBy string) uuid.UUID {
	id := uuid.New()
	ast.mu.Lock()
	ast.runs[id] = types.AgentRun{ID: id, CreatedBy: createdBy, State: types.RunRunning, SandboxRef: "sbx-1"}
	ast.mu.Unlock()
	return id
}

// seedResourcesRunWithDisk is seedResourcesRun plus a resolved disk_mib
// (RL-13's enforcedDiskWord reads AgentRun.DiskMiB, which the plain helper
// above leaves at its zero "no cap resolved" value).
func seedResourcesRunWithDisk(ast *authzStore, createdBy string, diskMiB int) uuid.UUID {
	id := uuid.New()
	ast.mu.Lock()
	ast.runs[id] = types.AgentRun{ID: id, CreatedBy: createdBy, State: types.RunRunning, SandboxRef: "sbx-1", DiskMiB: diskMiB}
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

// A run with no sandbox is a 409, and writes NO audit row.
//
// It used to hand an empty ref straight to ExecStream, which errored into the
// failure branch: a 500 plus a run.resources.fail failure row EVERY 4 SECONDS per
// open tab, for a PENDING/STARTING run that simply is not up yet — against a
// handler whose own comment says failures are "the rare, interesting case".
// It also disagreed with the Files widget beside it in the same rail, which
// returns a crisp 409 for the identical fact.
func TestRunResources_NoSandboxIs409AndNeverAudits(t *testing.T) {
	called := false
	srv, ast, h := newResourcesHarness(t, func(runner.ExecSpec) (*runner.ExecSession, error) {
		called = true
		return kvExecSession(""), nil
	})
	id := uuid.New()
	ast.mu.Lock()
	// STARTING, and crucially no SandboxRef — the pre-dispatch window.
	ast.runs[id] = types.AgentRun{ID: id, CreatedBy: "alice", State: types.RunStarting}
	ast.mu.Unlock()

	w := do(t, srv, http.MethodGet, "/api/v1/runs/"+id.String()+"/resources", adminToken, "")
	if w.Code != http.StatusConflict {
		t.Fatalf("code = %d, want 409; body=%s", w.Code, w.Body.String())
	}
	if called {
		t.Error("ExecStream was reached for a run with no sandbox ref")
	}
	if n := len(h.audit.events); n != 0 {
		t.Errorf("wrote %d audit events for a not-yet-dispatched run; the console "+
			"polls this every 4s, so any row here floods the trail", n)
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
		if ev.Action == "run.resources.fail" {
			t.Errorf("unexpected run.resources.fail audit event on a SUCCESSFUL read: %+v", ev)
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
	ev := lastAuditEvent(t, h.audit.events, "run.resources.fail")
	if ev.Outcome != "failure" {
		t.Errorf("run.resources.fail audit outcome = %q, want failure", ev.Outcome)
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

	mallory := ssoSession(t, "mallory-sub", "mallory@corp.example", oidc.RoleUser)
	w := doSSO(t, srv, http.MethodGet, "/api/v1/runs/"+id.String()+"/resources", mallory, "")
	if w.Code != http.StatusNotFound {
		t.Fatalf("non-owning member: code = %d, want 404; body=%s", w.Code, w.Body.String())
	}

	owner := ssoSession(t, "owner-sub", "owner@corp.example", oidc.RoleUser)
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

// getResourcesOK GETs run id's resources as the admin and decodes the 200 body.
func getResourcesOK(t *testing.T, srv *Server, id uuid.UUID) (runResourcesResponse, string) {
	t.Helper()
	w := do(t, srv, http.MethodGet, "/api/v1/runs/"+id.String()+"/resources", adminToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var got runResourcesResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v; body=%s", err, w.Body.String())
	}
	return got, w.Body.String()
}

// TestRunResources_DiskReading_PerEnforcement pins RL-13's measurement choice:
// the enforcement word picks what the script is asked to measure ($1, plus the
// k8s scratch mount points for `eviction`), and the handler reads only that
// arm's line. The canned output carries every arm's line at once, so reading
// the wrong one is visible as the wrong number. The cap appears only beside a
// cap-counting reading; the root walk (image included) never gets one.
func TestRunResources_DiskReading_PerEnforcement(t *testing.T) {
	kv := "disk_wbytes=99\n" +
		"disk_fs_size_kb=4194304\ndisk_fs_used_kb=100\n" + // df under a 4096 MiB project quota
		"disk_scratch_used_kb=200\n" +
		"disk_root_used_kb=300\n"
	cases := []struct {
		name        string
		diskMiB     int
		enforcement types.StorageEnforcement
		wantArgs    []string
		wantUsedKB  int64
		wantCap     bool
	}{
		{"filesystem: df's Used, against the cap", 4096, types.StorageEnforcementFilesystem,
			[]string{"filesystem"}, 100, true},
		{"eviction: the scratch mounts' walk, against the cap", 4096, types.StorageEnforcementEviction,
			[]string{"eviction", runner.ScratchTmpPath, runner.ScratchWorkPath, runner.ScratchCachePath}, 200, true},
		{"none enforced: the root walk, no cap", 4096, types.StorageEnforcementNone, []string{""}, 300, false},
		{"no driver word: the root walk, no cap", 4096, "", []string{""}, 300, false},
		{"enforced but no cap resolved (every legacy run): the root walk, no cap", 0, types.StorageEnforcementFilesystem,
			[]string{""}, 300, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotArgs []string
			execFn := func(spec runner.ExecSpec) (*runner.ExecSession, error) {
				// /bin/sh -c <script> wardyn-resources <args...>
				gotArgs = spec.Argv[4:]
				return kvExecSession(kv), nil
			}
			srv, ast, _ := newResourcesHarnessWithDisk(t, execFn, tc.enforcement)
			id := seedResourcesRunWithDisk(ast, "alice", tc.diskMiB)

			got, _ := getResourcesOK(t, srv, id)
			if strings.Join(gotArgs, "|") != strings.Join(tc.wantArgs, "|") {
				t.Errorf("script args = %q, want %q", gotArgs, tc.wantArgs)
			}
			if got.DiskUsedBytes == nil || *got.DiskUsedBytes != tc.wantUsedKB<<10 {
				t.Errorf("DiskUsedBytes = %v, want %d", got.DiskUsedBytes, tc.wantUsedKB<<10)
			}
			if tc.wantCap {
				want := int64(tc.diskMiB) << 20
				if got.DiskCapBytes == nil || *got.DiskCapBytes != want {
					t.Errorf("DiskCapBytes = %v, want %d", got.DiskCapBytes, want)
				}
			} else if got.DiskCapBytes != nil {
				t.Errorf("DiskCapBytes = %d, want absent", *got.DiskCapBytes)
			}
			if got.DiskWrittenBytes == nil || *got.DiskWrittenBytes != 99 {
				t.Errorf("DiskWrittenBytes = %v, want 99 (the disk lines must not disturb it)", got.DiskWrittenBytes)
			}
		})
	}
}

// TestRunResources_DiskFilesystem_FreshContainerDoesNotWarn is the review's
// baseline case. The numbers are a real fresh container under
// --storage-opt size=1024m on overlay2 over xfs mounted pquota: df -kP / said
// Size 1048576 and Used 8, while du -skx / in the agent image counts ~717 MiB of
// image files, 70% of the cap before the agent writes a byte. The reading must
// be the quota's 8 KB, well under the console's 80% line.
func TestRunResources_DiskFilesystem_FreshContainerDoesNotWarn(t *testing.T) {
	kv := "disk_fs_size_kb=1048576\ndisk_fs_used_kb=8\ndisk_root_used_kb=734576\n"
	srv, ast, _ := newResourcesHarnessWithDisk(t,
		func(runner.ExecSpec) (*runner.ExecSession, error) { return kvExecSession(kv), nil },
		types.StorageEnforcementFilesystem)
	id := seedResourcesRunWithDisk(ast, "alice", 1024)

	got, body := getResourcesOK(t, srv, id)
	if got.DiskUsedBytes == nil || got.DiskCapBytes == nil {
		t.Fatalf("want both disk fields; body=%s", body)
	}
	if *got.DiskUsedBytes != 8<<10 || *got.DiskCapBytes != 1<<30 {
		t.Errorf("disk = %d / %d, want %d / %d", *got.DiskUsedBytes, *got.DiskCapBytes, 8<<10, 1<<30)
	}
	if pct := float64(*got.DiskUsedBytes) / float64(*got.DiskCapBytes) * 100; pct >= 80 {
		t.Errorf("a fresh container reads %.1f%% of its cap; the console would warn", pct)
	}
}

// TestRunResources_DiskFilesystem_SizeNotTheCapIsDropped: a df Size that is not
// the cap means statfs answered for something other than the run's quota. The
// numbers are overlay2 over ext4, where statfs reports the whole host
// filesystem; drawn against a 1024 MiB cap that Used would pin the bar at 100%.
// Both fields must be absent so the console falls back to disk written.
func TestRunResources_DiskFilesystem_SizeNotTheCapIsDropped(t *testing.T) {
	kv := "disk_wbytes=99\ndisk_fs_size_kb=1055762868\ndisk_fs_used_kb=533637780\n"
	srv, ast, _ := newResourcesHarnessWithDisk(t,
		func(runner.ExecSpec) (*runner.ExecSession, error) { return kvExecSession(kv), nil },
		types.StorageEnforcementFilesystem)
	id := seedResourcesRunWithDisk(ast, "alice", 1024)

	_, body := getResourcesOK(t, srv, id)
	if strings.Contains(body, `"disk_used_bytes"`) || strings.Contains(body, `"disk_cap_bytes"`) {
		t.Errorf("a df Size that is not the cap was drawn against it; body=%s", body)
	}
}

// TestRunResources_DiskUsedBytes_Absent pins the honesty case for every arm: no
// disk line (a timed-out or partial walk, an image without du, df unreadable)
// reads as ABSENT, never a fabricated 0, and a cap never appears without a
// reading beside it.
func TestRunResources_DiskUsedBytes_Absent(t *testing.T) {
	for _, word := range []types.StorageEnforcement{types.StorageEnforcementFilesystem, types.StorageEnforcementEviction, ""} {
		name := string(word)
		if name == "" {
			name = "no word"
		}
		t.Run(name, func(t *testing.T) {
			srv, ast, _ := newResourcesHarnessWithDisk(t,
				func(runner.ExecSpec) (*runner.ExecSession, error) { return kvExecSession("nproc=1\n"), nil }, word)
			id := seedResourcesRunWithDisk(ast, "alice", 4096)

			_, body := getResourcesOK(t, srv, id)
			if strings.Contains(body, `"disk_used_bytes"`) || strings.Contains(body, `"disk_cap_bytes"`) {
				t.Errorf("disk fields present with nothing reported; body=%s", body)
			}
		})
	}
}
