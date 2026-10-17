// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The BYOI selftest of a task run on a runner, against the real runner side (runnerLeaseFixture): the
// selftest is a call of its own under the task's issued generation, which it neither spends nor binds.
// Guarded by WARDYN_TEST_PG: skipped cleanly when unset, must PASS when set.

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// selftestRecord is what the runner's substrate was asked to run as a selftest.
var selftestRecord struct {
	mu    sync.Mutex
	argvs map[string][][]string // local ref -> argvs
}

// ExecTo makes the fixture's substrate one that can send a selftest's output to the runner's own log, as
// the docker driver does. It writes there, never to the run's output.
func (s *stallingSub) ExecTo(ctx context.Context, ref string, argv []string, out io.Writer) (string, error) {
	selftestRecord.mu.Lock()
	if selftestRecord.argvs == nil {
		selftestRecord.argvs = map[string][][]string{}
	}
	selftestRecord.argvs[ref] = append(selftestRecord.argvs[ref], argv)
	selftestRecord.mu.Unlock()
	id, err := s.Fake.Exec(ctx, ref, argv)
	_, _ = io.WriteString(out, "selftest output\n")
	if c, ok := out.(io.Closer); ok {
		_ = c.Close()
	}
	return "selftest-" + id, err
}

func selftestsOf(ref string) [][]string {
	selftestRecord.mu.Lock()
	defer selftestRecord.mu.Unlock()
	return slices.Clone(selftestRecord.argvs[ref])
}

// placedBYOI leases and creates a RUNNING BYOI task run on the runner, as dispatch leaves it before its
// agent starts.
func (f *runnerLeaseFixture) placedBYOI(t *testing.T) (types.AgentRun, runnerExecution) {
	t.Helper()
	run := f.localRun(t, 0)
	e, err := f.srv.leaseRunOnRunner(f.ctx, run)
	if err != nil || e.generation == [16]byte{} {
		t.Fatalf("lease = %+v, %v", e, err)
	}
	sb, err := f.srv.createSandboxAtTarget(f.ctx, run, runner.SandboxSpec{RunID: run.ID, ConfinementClass: types.CC1, Image: byoiImage})
	if err != nil {
		t.Fatal(err)
	}
	run.SandboxRef = sb.Ref
	if err := f.st.SetSandboxRef(f.ctx, run.ID, sb.Ref); err != nil {
		t.Fatal(err)
	}
	f.running(t, &run, nil)
	return run, e
}

const byoiImage = "wardyn-byoi/custom:latest"

// The bug: a BYOI task run placed on an exec-capable runner was aborted by its own selftest, which had no
// generation to carry. It now passes its selftest and starts its agent under the task's generation.
func TestPG_RunnerBYOI_SelftestDoesNotAbortTheRun(t *testing.T) {
	f := newRunnerLeaseFixture(t)
	run, e := f.placedBYOI(t)

	f.srv.startAgentOrIdle(f.ctx, run, run.SandboxRef, byoiImage, false, e, nil)

	if got := selftestsOf("fake-" + run.ID.String()); len(got) != 1 {
		t.Fatalf("selftests on the runner = %v, want one", got)
	}
	if got, _ := f.st.GetRun(f.ctx, run.ID); got.State == types.RunFailed {
		t.Fatalf("run FAILED: a valid BYOI run was aborted by its selftest")
	}
	if n := f.execs.Load(); n != 1 {
		t.Fatalf("Exec frames = %d, want the task's one", n)
	}
	if cur := f.execution(t, run); cur.Generation != e.generation || cur.ExecID != "exec-fake-"+run.ID.String() {
		t.Fatalf("execution = %+v, want the task's generation bound to the task's exec", cur)
	}
	if !f.srv.cfg.Audit.(*syncAudit).has(run.ID, "run.selftest", "success") {
		t.Fatal("no run.selftest success audit")
	}
}

// After the selftest, the task's generation is still unattempted on both sides, nothing of the selftest
// is recorded as the task's execution, and the task's Exec then succeeds under that generation.
func TestPG_RunnerBYOI_SelftestLeavesTheTaskGenerationUnspent(t *testing.T) {
	f := newRunnerLeaseFixture(t)
	run, e := f.placedBYOI(t)
	local := "fake-" + run.ID.String()
	st := f.srv.cfg.Runner.(runner.Selftester)

	code, err := st.Selftest(e.selftest().bind(f.ctx), run.SandboxRef)
	if err != nil || code != 0 {
		t.Fatalf("selftest = %d, %v", code, err)
	}
	if cur := f.execution(t, run); cur.Generation != e.generation || cur.ExecID != "" {
		t.Fatalf("org execution after selftest = %+v, want the generation unbound", cur)
	}
	if res, err := f.manager.ExecutionResult(f.ctx, local, e.generation); err == nil {
		t.Fatalf("runner execution after selftest = %+v, want no attempt recorded", res)
	}
	if n := f.execs.Load(); n != 0 {
		t.Fatalf("Exec frames = %d, want none: the selftest is not an Exec", n)
	}

	argv := []string{"/usr/local/bin/agent-run", "do the thing"}
	execID, err := f.srv.cfg.Runner.Exec(e.bind(f.ctx), run.SandboxRef, argv)
	if err == nil {
		err = f.srv.bindRunnerExecution(f.ctx, e, execID)
	}
	if err != nil {
		t.Fatalf("task Exec after selftest: %v", err)
	}
	res, err := f.manager.ExecutionResult(f.ctx, local, e.generation)
	if err != nil || !res.Attempted || res.ExecID != execID || execID != "exec-"+local {
		t.Fatalf("runner execution = %+v, %v; want the task's exec %q, not the selftest's", res, err, execID)
	}
	if cur := f.execution(t, run); cur.ExecID != execID {
		t.Fatalf("org execution = %+v, want the task's exec", cur)
	}
	// Spent by the task, the generation admits no further selftest.
	if _, err := st.Selftest(e.selftest().bind(f.ctx), run.SandboxRef); err == nil {
		t.Fatal("a selftest after the task's Exec was admitted")
	}
	if got := selftestsOf(local); len(got) != 1 || !slices.Equal(got[0], []string{"/usr/local/bin/agent-run", "--selftest"}) {
		t.Fatalf("selftests on the runner = %v, want the one selftest argv", got)
	}
}

// The selftest's identity authorizes the selftest only: an Exec under it is refused before it is sent,
// and the task's own identity does not authorize a selftest.
func TestPG_RunnerBYOI_SelftestIdentityIsNotAnExec(t *testing.T) {
	f := newRunnerLeaseFixture(t)
	run, e := f.placedBYOI(t)

	_, err := f.srv.cfg.Runner.Exec(e.selftest().bind(f.ctx), run.SandboxRef, []string{"/bin/sh", "-c", "anything"})
	if !errors.Is(err, errRunnerExecutionUnbound) {
		t.Fatalf("Exec under the selftest identity = %v, want refused", err)
	}
	if n := f.execs.Load(); n != 0 {
		t.Fatalf("Exec frames = %d, want none", n)
	}
	if _, err := f.srv.cfg.Runner.(runner.Selftester).Selftest(e.bind(f.ctx), run.SandboxRef); !errors.Is(err, errRunnerExecutionUnbound) {
		t.Fatalf("selftest under the task's identity = %v, want refused", err)
	}
	if got := selftestsOf("fake-" + run.ID.String()); len(got) != 0 {
		t.Fatalf("selftests on the runner = %v, want none", got)
	}
	if cur := f.execution(t, run); cur.ExecID != "" {
		t.Fatalf("execution = %+v, want unspent", cur)
	}
}

// A runner from before the selftest call fails the run closed with the reason; it never skips the check
// and never starts the task.
func TestPG_RunnerBYOI_OldRunnerFailsClosed(t *testing.T) {
	f := newRunnerLeaseFixture(t)
	f.oldRunner.Store(true)
	f.disconnect()
	f.disconnect = f.connect(t)
	if c, _ := f.srv.runnerEndpoint(f.rn.ID).sub.Caps(); c.BYOISelftest {
		t.Fatal("the old runner advertises the selftest")
	}
	run, e := f.placedBYOI(t)

	f.srv.startAgentOrIdle(f.ctx, run, run.SandboxRef, byoiImage, false, e, nil)

	if got, _ := f.st.GetRun(f.ctx, run.ID); got.State != types.RunFailed {
		t.Fatalf("run state = %q, want FAILED", got.State)
	}
	if n := f.execs.Load(); n != 0 {
		t.Fatalf("Exec frames = %d, want none: the task must not start", n)
	}
	var reason string
	for _, ev := range f.srv.cfg.Audit.(*syncAudit).eventsFor(run.ID, "run.selftest") {
		if ev.Outcome == "failure" {
			reason = string(ev.Data)
		}
	}
	if !strings.Contains(reason, "does not support the BYOI selftest") {
		t.Fatalf("selftest failure audit = %q, want the runner's missing support", reason)
	}
}
