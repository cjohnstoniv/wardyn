// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// heldEndFixture is a run ended one minute ago on a target whose executors answer keeps, whose run lock the
// test holds (released at cleanup), and the time it was lost.
func heldEndFixture(t *testing.T, keeps ...bool) (*leaseFixture, time.Time) {
	t.Helper()
	f := newLeaseFixture(t, -time.Minute)
	f.srv.cfg.Runner = leaseRouter{leaseRunner: f.rn, keeps: keeps}
	if applied, err := f.st.MarkRunEnded(context.Background(), f.run.ID, f.now); err != nil || !applied {
		t.Fatalf("MarkRunEnded = %v, %v; want the claim to land", applied, err)
	}
	_, hold, ok := f.srv.tryLockRunOp(context.Background(), f.run.ID)
	if !ok {
		t.Fatal("could not take the run's lock")
	}
	t.Cleanup(hold)
	return f, f.now
}

// sweepAt is a lease sweep at lostAt plus after.
func (f *leaseFixture) sweepAt(t *testing.T, lostAt time.Time, after time.Duration) {
	t.Helper()
	f.now = lostAt.Add(after)
	f.sweep(t)
}

// A run whose target cannot keep its sandbox, ended while another step holds its lock, is forced to end
// keptEndHoldOffLimit after it was lost, not after its keep grace (7 d here) plus the limit.
func TestLease_UnkeptEndUnderHeldLockIsForcedAfterHoldOff(t *testing.T) {
	f, lostAt := heldEndFixture(t, false)
	f.sweepAt(t, lostAt, time.Minute)
	f.sweepAt(t, lostAt, keptEndHoldOffLimit-time.Second)
	if f.st.State() != types.RunRunning || f.rn.stopCount() != 0 {
		t.Fatalf("state %s, StopSandbox %d inside the hold-off; want RUNNING, 0", f.st.State(), f.rn.stopCount())
	}
	f.sweepAt(t, lostAt, keptEndHoldOffLimit)
	if f.st.State() != types.RunStopped || f.rn.stopCount() != 1 {
		t.Fatalf("state %s, StopSandbox %d at the hold-off; want STOPPED, 1", f.st.State(), f.rn.stopCount())
	}
	forced := 0
	for _, ev := range f.audit.eventsFor(f.run.ID, "run.ended") {
		if leaseAuditData(t, ev)["forced"] == true {
			forced++
		}
	}
	if forced != 1 {
		t.Errorf("%d forced run.ended rows, want 1", forced)
	}
}

// A target that keeps its sandbox still waits for its keep grace, then the limit.
func TestLease_KeptEndUnderHeldLockStillWaits(t *testing.T) {
	f, lostAt := heldEndFixture(t, true)
	grace := f.srv.cfg.EndedRunGrace
	f.sweepAt(t, lostAt, keptEndHoldOffLimit+time.Minute)
	f.sweepAt(t, lostAt, grace+keptEndHoldOffLimit-time.Second)
	if f.st.State() != types.RunRunning || f.rn.stopCount() != 0 {
		t.Fatalf("state %s, StopSandbox %d before keptUntil + the limit; want RUNNING, 0", f.st.State(), f.rn.stopCount())
	}
	f.sweepAt(t, lostAt, grace+keptEndHoldOffLimit)
	if f.st.State() != types.RunStopped || f.rn.stopCount() != 1 {
		t.Fatalf("state %s, StopSandbox %d at keptUntil + the limit; want STOPPED, 1", f.st.State(), f.rn.stopCount())
	}
}

// A target whose answer is not known (executors that disagree) is not forced early.
func TestLease_UnknownTargetUnderHeldLockWaits(t *testing.T) {
	f, lostAt := heldEndFixture(t, false, true)
	f.sweepAt(t, lostAt, keptEndHoldOffLimit+time.Minute)
	f.sweepAt(t, lostAt, f.srv.cfg.EndedRunGrace-time.Minute)
	if f.st.State() != types.RunRunning || f.rn.stopCount() != 0 {
		t.Fatalf("state %s, StopSandbox %d for an unknown target; want RUNNING, 0", f.st.State(), f.rn.stopCount())
	}
}

// resumedRows counts the rows under action that a forced stop's retry wrote for a run it picked up.
func resumedRows(audit *recRecorder, action string) int {
	n := 0
	for _, ev := range audit.snapshot() {
		var d map[string]any
		if ev.Action == action && json.Unmarshal(ev.Data, &d) == nil && d["resumed"] == true {
			n++
		}
	}
	return n
}

// A process that died after the forced claim left the run terminal with its approval pending and no end row.
// The first retry, on a new server, cancels the approval and writes the row; the second writes neither again.
func TestForcedStop_CrashAfterClaimGetsApprovalsAndRow(t *testing.T) {
	f := newForced(t)
	fa := newFakeApprovals()
	seedPendingApproval(t, fa, f.run.ID)
	f.s.cfg.Approvals = fa
	f.claimOnly(t)
	f.sb.stopErr = errors.New("the runner did not answer the stop") // the lock is held, so the mark stays
	restarted := New(f.s.cfg)
	f.tick(t, restarted)
	if calls := fa.cancelledCalls(); len(calls) != 1 || calls[0].Count != 1 || calls[0].Reason != "run_ended" {
		t.Fatalf("approval cancels after the first retry = %+v; want one, of one row, run_ended", calls)
	}
	if n := resumedRows(f.audit, "run.ended"); n != 1 {
		t.Fatalf("%d resumed rows after the first retry, want 1", n)
	}
	if f.leaser.markCount() != 1 {
		t.Fatalf("%d marks after a failed stop, want 1", f.leaser.markCount())
	}
	f.tick(t, restarted)
	if calls := fa.cancelledCalls(); len(calls) != 1 {
		t.Errorf("approval cancels after the second retry = %+v; want still one", calls)
	}
	if n := resumedRows(f.audit, "run.ended"); n != 1 {
		t.Errorf("%d resumed rows after the second retry, want 1", n)
	}
}

// The resumed row is written once per process per run, and a restart writes its own.
func TestForcedStop_ResumedRowOncePerProcess(t *testing.T) {
	f := newForced(t)
	f.claimOnly(t)
	f.sb.stopErr = errors.New("the runner did not answer the stop")
	f.tick(t, f.s)
	f.tick(t, f.s)
	f.tick(t, f.s)
	if n := resumedRows(f.audit, "run.ended"); n != 1 {
		t.Fatalf("%d resumed rows after three ticks, want 1", n)
	}
	f.tick(t, New(f.s.cfg))
	if n := resumedRows(f.audit, "run.ended"); n != 2 {
		t.Errorf("%d resumed rows after a restart's first tick, want 2", n)
	}
}

// fencingRunner is the forced fixture's runner that also fences, counting its cut calls. fenceErr is what
// FenceProxy answers.
type fencingRunner struct {
	*passWindowSandbox
	fences     atomic.Int32
	proxyStops atomic.Int32
	fenceErr   error
}

func (r *fencingRunner) FenceProxy(context.Context, string) error {
	r.fences.Add(1)
	return r.fenceErr
}

func (r *fencingRunner) StopProxy(ctx context.Context, ref string) error {
	r.proxyStops.Add(1)
	return r.passWindowSandbox.StopProxy(ctx, ref)
}

// calls is every call that would cut, stop or queue anything on the runner.
func (r *fencingRunner) calls() int {
	return int(r.fences.Load()) + int(r.proxyStops.Load()) + r.sb().stopCount()
}

func (r *fencingRunner) sb() *agentSandbox { return r.passWindowSandbox.agentSandbox }

// owedRecord is a computer folder record with a pass back owed (a resume), its generation settled.
var owedRecord = types.RunLocalFolderStatus{Gen: 3, Settled: 3, Resume: &types.FolderResume{Unsynced: 1}}

// unsettledOwedRecord is owedRecord left unsettled: a forced end died between its claim and the settle.
var unsettledOwedRecord = types.RunLocalFolderStatus{Gen: 3, Settled: 2, Resume: &types.FolderResume{Unsynced: 1}}

// onFencedRunner puts the forced fixture's run on its runner, connected here with the fence capability, and
// answers the switch that reports the runner online or not.
func (f *forced) onFencedRunner(t *testing.T) (*fencingRunner, *atomic.Bool) {
	t.Helper()
	id := f.cs.runner.ID
	f.onRunner(id)
	fr := &fencingRunner{passWindowSandbox: f.s.cfg.Runner.(*passWindowSandbox)}
	f.s.cfg.Runner = fr
	online := &atomic.Bool{}
	online.Store(true)
	f.s.cfg.RunnerOnline = func(uuid.UUID) bool { return online.Load() }
	seamConnect(t, f.s, id, runnerwire.Caps{ProxyFence: true})
	return fr, online
}

// terminalRun is the run as the store holds it.
func (f *forced) terminalRun(t *testing.T) types.AgentRun {
	t.Helper()
	run, err := f.cs.GetRun(context.Background(), f.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	return run
}

// A forced end whose cut was observed keeps the sandbox for the owed pass: nothing stops the sandbox, the
// record carries a resume at its new generation, and the mark waits for the lock; with the lock free the retry
// keeps it and clears the mark. A hosted container and a connected runner that fences.
func TestForcedEnd_ObservedCutKeepsTheOwedSandbox(t *testing.T) {
	for name, setup := range map[string]func(*testing.T, *forced) *fencingRunner{
		"a hosted container": func(*testing.T, *forced) *fencingRunner { return nil },
		"a fenced runner": func(t *testing.T, f *forced) *fencingRunner {
			fr, _ := f.onFencedRunner(t)
			return fr
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newForced(t)
			fr := setup(t, f)
			f.setRecord(owedRecord)
			f.end(f.s)
			if f.sb.stopCount() != 0 {
				t.Fatalf("%d sandbox stops, want the sandbox kept for its pass back", f.sb.stopCount())
			}
			if fr != nil && fr.fences.Load() == 0 {
				t.Error("the runner's agent was not fenced")
			}
			rec := f.cs.folderStatus()
			if rec.Resume == nil || rec.Gen != 4 || rec.Settled != 4 || rec.Back == nil || !strings.Contains(rec.Back.Error, "Kept for its copy back") {
				t.Fatalf("record %+v; want generation 4 settled with a resume, kept for its copy back", rec)
			}
			if run := f.terminalRun(t); run.SandboxRef == "" || run.State != types.RunStopped || f.leaser.markCount() != 1 {
				t.Errorf("ref %q, state %s, %d marks; want the ref kept, STOPPED, marked until the lock is free", run.SandboxRef, run.State, f.leaser.markCount())
			}
			f.release()
			f.tick(t, f.s)
			if f.sb.stopCount() != 0 || f.leaser.markCount() != 0 {
				t.Errorf("with the lock free: %d stops, %d marks; want the sandbox kept and the mark cleared", f.sb.stopCount(), f.leaser.markCount())
			}
		})
	}
}

// A cut that was not observed, or a sandbox that cannot be kept, settles the record as not brought back and
// stops the sandbox: a virtual machine, Kubernetes in server mode, a runner without the fence, and a runner
// whose last caps are known but whose session is not online.
func TestForcedEnd_UnobservedCutStillSettlesAndStops(t *testing.T) {
	for name, set := range map[string]func(*testing.T, *forced){
		"a virtual machine": func(_ *testing.T, f *forced) {
			f.cs.dispatchTestStore.mu.Lock()
			f.cs.run.MachineKind = types.MachineLinuxVM
			f.cs.dispatchTestStore.mu.Unlock()
		},
		"kubernetes in server mode": func(_ *testing.T, f *forced) { f.sb.proxyErr = runner.ErrEndUnsupported },
		"a runner without the fence": func(t *testing.T, f *forced) {
			id := f.cs.runner.ID
			f.onRunner(id)
			seamConnect(t, f.s, id, runnerwire.Caps{})
		},
		"a runner with stale caps and offline": func(t *testing.T, f *forced) {
			fr, online := f.onFencedRunner(t)
			_ = fr
			online.Store(false)
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newForced(t)
			set(t, f)
			f.setRecord(owedRecord)
			f.end(f.s)
			rec := f.cs.folderStatus()
			if rec.Resume != nil || rec.Gen != 4 || rec.Settled != 4 || rec.Back == nil || !strings.Contains(rec.Back.Error, "Not brought back") {
				t.Errorf("record %+v; want generation 4 settled, no resume, not brought back", rec)
			}
			if f.sb.stopCount() != 1 {
				t.Errorf("%d sandbox stops, want 1: the cut was not observed", f.sb.stopCount())
			}
		})
	}
}

// A crash after the claim with a pass back owed: the retry keeps the sandbox the forced end would have kept,
// settling an unsettled record with a resume and not as not brought back.
func TestForcedRetry_KeepsWhatTheForcedEndKept(t *testing.T) {
	f := newForced(t)
	f.claimOnly(t)
	f.setRecord(unsettledOwedRecord)
	f.tick(t, f.s) // the lock is held
	rec := f.cs.folderStatus()
	if rec.Resume == nil || rec.Gen != 4 || rec.Settled != 4 || rec.Back == nil || !strings.Contains(rec.Back.Error, "Kept for its copy back") {
		t.Fatalf("record %+v; want generation 4 settled with a resume, kept for its copy back", rec)
	}
	if f.sb.stopCount() != 0 || f.leaser.markCount() != 1 {
		t.Fatalf("%d stops, %d marks with the lock held; want the sandbox kept and the mark waiting", f.sb.stopCount(), f.leaser.markCount())
	}
	f.release()
	f.tick(t, f.s)
	if f.sb.stopCount() != 0 || f.leaser.markCount() != 0 {
		t.Errorf("with the lock free: %d stops, %d marks; want the sandbox kept and the mark cleared", f.sb.stopCount(), f.leaser.markCount())
	}
}

// A connected runner that fences keeps a sandbox whose pass back is owed.
func TestStopTerminalSandbox_FencedRunnerRunIsKept(t *testing.T) {
	f := newForced(t)
	fr, _ := f.onFencedRunner(t)
	f.claimOnly(t)
	f.setRecord(owedRecord)
	left, err := f.s.stopTerminalSandbox(context.Background(), f.terminalRun(t), sandboxSweepAction)
	if !left || err != nil || f.sb.stopCount() != 0 || fr.fences.Load() != 1 {
		t.Errorf("left %v, err %v, %d sandbox stops, %d fences; want kept, no error, none, one", left, err, f.sb.stopCount(), fr.fences.Load())
	}
}

// A connected runner without the fence is kept once its proxy stop answers nil, stopped with one failure row
// when the proxy stop fails, and left in place, with nothing sent or written, when it is not connected.
func TestStopTerminalSandbox_RunnerProxyStoppedResumeIsKept(t *testing.T) {
	setup := func(t *testing.T) (*forced, *fencingRunner, *atomic.Bool) {
		f := newForced(t)
		fr, online := f.onFencedRunner(t)
		seamConnect(t, f.s, f.cs.runner.ID, runnerwire.Caps{}) // no fence: the proxy is stopped alone
		fr.fenceErr = errors.New("a fence must not be asked")
		f.claimOnly(t)
		f.setRecord(owedRecord)
		return f, fr, online
	}
	t.Run("the proxy stop answers nil", func(t *testing.T) {
		f, fr, _ := setup(t)
		left, err := f.s.stopTerminalSandbox(context.Background(), f.terminalRun(t), sandboxSweepAction)
		if !left || err != nil || f.sb.stopCount() != 0 || fr.proxyStops.Load() != 1 {
			t.Errorf("left %v, err %v, %d sandbox stops, %d proxy stops; want kept, no error, none, one", left, err, f.sb.stopCount(), fr.proxyStops.Load())
		}
	})
	t.Run("the proxy stop fails", func(t *testing.T) {
		f, _, _ := setup(t)
		f.sb.proxyErr = errors.New("the runner did not answer the proxy stop")
		left, err := f.s.stopTerminalSandbox(context.Background(), f.terminalRun(t), sandboxSweepAction)
		if left || err != nil || f.sb.stopCount() != 1 {
			t.Errorf("left %v, err %v, %d sandbox stops; want stopped, no error, one", left, err, f.sb.stopCount())
		}
		if rows := sweepRows(f.audit, "failure", "teardown_error"); len(rows) != 1 {
			t.Errorf("%d failure rows, want 1", len(rows))
		}
	})
	t.Run("the runner is not connected", func(t *testing.T) {
		f, fr, online := setup(t)
		online.Store(false)
		left, err := f.s.stopTerminalSandbox(context.Background(), f.terminalRun(t), sandboxSweepAction)
		if !left || !errors.Is(err, errRunnerNotHere) || fr.calls() != 0 || len(f.audit.snapshot()) != 0 {
			t.Errorf("left %v, err %v, %d calls, %d rows; want left, errRunnerNotHere, 0, 0", left, err, fr.calls(), len(f.audit.snapshot()))
		}
	})
}

// Kubernetes in server mode cannot stop a proxy alone, so its owed sandbox is stopped, and no cut other than
// the one proxy stop is made for it.
func TestStopTerminalSandbox_K8sServerHeldIsStopped(t *testing.T) {
	f := newForced(t)
	f.sb.proxyErr = runner.ErrEndUnsupported
	f.claimOnly(t)
	f.setRecord(owedRecord)
	left, err := f.s.stopTerminalSandbox(context.Background(), f.terminalRun(t), sandboxSweepAction)
	if left || err != nil || f.sb.stopCount() != 1 {
		t.Errorf("left %v, err %v, %d sandbox stops; want stopped, no error, one", left, err, f.sb.stopCount())
	}
}

// A runner run whose runner is not connected here, with a pass back owed or a record that cannot be read, is
// left alone: nothing is stopped, queued or counted as swept, and no row is written.
func TestStopTerminalSandbox_RunnerNotHereLeavesTheOwedSandbox(t *testing.T) {
	t.Run("an owed pass, through the terminal sandbox sweep", func(t *testing.T) {
		f := newForced(t)
		fr, online := f.onFencedRunner(t)
		f.claimOnly(t)
		f.setRecord(owedRecord)
		online.Store(false)
		f.release()
		if f.s.sweepTerminalSandboxRun(context.Background(), f.terminalRun(t)) {
			t.Error("the sweep counted a left sandbox as swept")
		}
		if rows := sweepRows(f.audit, "failure", ""); fr.calls() != 0 || len(rows) != 0 {
			t.Errorf("%d calls, %d failure rows; want none", fr.calls(), len(rows))
		}
	})
	t.Run("a record that cannot be read", func(t *testing.T) {
		f := newForced(t)
		fr, online := f.onFencedRunner(t)
		f.claimOnly(t)
		f.s.cfg.Store = unreadableRecordStore{f.s.cfg.Store.(markedStore)}
		online.Store(false)
		left, err := f.s.stopTerminalSandbox(context.Background(), f.terminalRun(t), sandboxSweepAction)
		if !left || !errors.Is(err, errRunnerNotHere) || fr.calls() != 0 {
			t.Errorf("left %v, err %v, %d calls; want left, errRunnerNotHere, 0", left, err, fr.calls())
		}
	})
}

// A hosted sandbox whose proxy stop fails is stopped, not kept, with the cut's one failure row.
func TestStopTerminalSandbox_HostedProxyStopErrorStopsTheSandboxWithRow(t *testing.T) {
	f := newForced(t)
	f.sb.proxyErr = errors.New("the daemon did not answer the proxy stop")
	f.claimOnly(t)
	f.setRecord(owedRecord)
	left, err := f.s.stopTerminalSandbox(context.Background(), f.terminalRun(t), sandboxSweepAction)
	if left || err != nil || f.sb.stopCount() != 1 {
		t.Errorf("left %v, err %v, %d sandbox stops; want not left, no error and one sandbox stop", left, err, f.sb.stopCount())
	}
	if rows := sweepRows(f.audit, "failure", "teardown_error"); len(rows) != 1 {
		t.Errorf("%d failure rows, want 1", len(rows))
	}
}

// The runner that kept an owed sandbox drops: the retry sends it nothing and the mark waits, lock held or free;
// when it returns the next tick fences, keeps and clears the mark; a runner whose row is revoked clears the
// mark with the runner_gone row and no stop.
func TestForcedRetry_RunnerNotHereKeepsTheOwedSandbox(t *testing.T) {
	kept := func(t *testing.T) (*forced, *fencingRunner, *atomic.Bool) {
		f := newForced(t)
		fr, online := f.onFencedRunner(t)
		f.setRecord(owedRecord)
		f.end(f.s)
		if f.sb.stopCount() != 0 || f.leaser.markCount() != 1 {
			t.Fatalf("%d stops, %d marks after the forced end; want the sandbox kept and marked", f.sb.stopCount(), f.leaser.markCount())
		}
		return f, fr, online
	}
	t.Run("the runner returns", func(t *testing.T) {
		f, fr, online := kept(t)
		online.Store(false)
		before := fr.calls()
		f.tick(t, f.s) // the lock is held
		f.release()
		f.tick(t, f.s) // the lock is free
		if fr.calls() != before || f.leaser.markCount() != 1 || len(sweepRows(f.audit, "failure", "")) != 0 {
			t.Fatalf("%d calls after %d, %d marks; want no call and the mark kept", fr.calls(), before, f.leaser.markCount())
		}
		online.Store(true)
		f.tick(t, f.s)
		if f.sb.stopCount() != 0 || fr.fences.Load() < 2 || f.leaser.markCount() != 0 {
			t.Errorf("%d stops, %d fences, %d marks; want the sandbox fenced and kept and the mark cleared", f.sb.stopCount(), fr.fences.Load(), f.leaser.markCount())
		}
	})
	t.Run("the runner is revoked", func(t *testing.T) {
		f, fr, online := kept(t)
		online.Store(false)
		f.cs.runner.State = types.RunnerRevoked
		before := fr.calls()
		f.tick(t, f.s)
		rows := sweepRows(f.audit, "failure", "runner_gone")
		if fr.calls() != before || len(rows) != 1 || f.leaser.markCount() != 0 {
			t.Errorf("%d calls after %d, %d runner-gone rows, %d marks; want no call, one row, the mark cleared", fr.calls(), before, len(rows), f.leaser.markCount())
		}
	})
}

// A forced end whose process died after the claim and before the settle, on a runner that is not here, settles
// nothing and sends nothing; the runner's return settles with a resume and keeps the sandbox; a revoked
// runner settles as not brought back and clears the mark with the runner_gone row.
func TestForcedRetry_CrashBeforeSettleRunnerNotHereKeepsTheCopyBack(t *testing.T) {
	t.Run("not connected, then back", func(t *testing.T) {
		f := newForced(t)
		fr, online := f.onFencedRunner(t)
		f.claimOnly(t)
		f.setRecord(unsettledOwedRecord)
		online.Store(false)
		f.tick(t, f.s)
		if rec := f.cs.folderStatus(); rec.Gen != 3 || rec.Settled != 2 {
			t.Fatalf("record %+v; want generation 3 left unsettled", rec)
		}
		if fr.calls() != 0 || f.leaser.markCount() != 1 {
			t.Fatalf("%d calls, %d marks; want none and the mark kept", fr.calls(), f.leaser.markCount())
		}
		online.Store(true)
		f.release()
		f.tick(t, f.s)
		rec := f.cs.folderStatus()
		if rec.Resume == nil || rec.Gen != 4 || rec.Settled != 4 || f.sb.stopCount() != 0 || f.leaser.markCount() != 0 {
			t.Errorf("record %+v, %d stops, %d marks; want generation 4 settled with a resume, no stop, the mark cleared", rec, f.sb.stopCount(), f.leaser.markCount())
		}
	})
	t.Run("connected at the gate, offline at the settle", func(t *testing.T) {
		f := newForced(t)
		fr, _ := f.onFencedRunner(t)
		f.claimOnly(t)
		f.setRecord(unsettledOwedRecord)
		fr.fenceErr = fmt.Errorf("%w: the link dropped", runner.ErrRunnerOffline)
		f.tick(t, f.s)
		if rec := f.cs.folderStatus(); rec.Gen != 3 || rec.Settled != 2 {
			t.Fatalf("record %+v; want generation 3 left unsettled", rec)
		}
		if fr.proxyStops.Load() != 0 || f.sb.stopCount() != 0 || f.leaser.markCount() != 1 {
			t.Errorf("%d proxy stops, %d sandbox stops, %d marks; want none and the mark kept", fr.proxyStops.Load(), f.sb.stopCount(), f.leaser.markCount())
		}
	})
	t.Run("revoked", func(t *testing.T) {
		f := newForced(t)
		_, online := f.onFencedRunner(t)
		f.claimOnly(t)
		f.setRecord(unsettledOwedRecord)
		online.Store(false)
		f.cs.runner.State = types.RunnerRevoked
		f.sb.stopErr = fmt.Errorf("%w: no registered substrate", runner.ErrRunnerOffline)
		f.tick(t, f.s)
		rec := f.cs.folderStatus()
		if rec.Resume != nil || rec.Back == nil || !strings.Contains(rec.Back.Error, "Not brought back") {
			t.Errorf("record %+v; want settled as not brought back", rec)
		}
		if rows := sweepRows(f.audit, "failure", "runner_gone"); len(rows) != 1 || f.leaser.markCount() != 0 {
			t.Errorf("%d runner-gone rows, %d marks; want one row and the mark cleared", len(rows), f.leaser.markCount())
		}
	})
}

// clearOnly is a ForcedStopper whose mark clears whenever asked.
type clearOnly struct{ store.ForcedStopper }

func (clearOnly) ClearForcedStop(context.Context, uuid.UUID, time.Time) (bool, error) {
	return true, nil
}

// A mark cleared after a revoke that left only a pending run PAT revoke drops the revoke row's
// once-per-process entry: the revoke itself keeps it, as the PAT sweep owns the retry.
func TestForcedMark_ClearDropsTheRevokeEntry(t *testing.T) {
	fx := newADOPATFixture(t)
	fx.ok(t, "dev.azure.com", nil)
	fx.pats.revokeErr = &adoPATError{Status: http.StatusServiceUnavailable}
	key := teardownRowKey{fx.srv, fx.run.ID, "run.revoke"}
	ctx := context.Background()
	if err := fx.srv.revokeRunCascade(forcedCtx(ctx), fx.run.ID); err != nil {
		t.Fatalf("the cascade with only a run PAT revoke pending = %v, want it done", err)
	}
	if _, held := teardownRowsWritten.Load(key); !held {
		t.Fatal("the pending-only revoke left no once-per-process entry; the case is not exercised")
	}
	if !fx.srv.clearForcedMarkIf(ctx, clearOnly{}, fx.run.ID, time.Now(), true, nil) {
		t.Fatal("the mark was not cleared")
	}
	if _, held := teardownRowsWritten.Load(key); held {
		t.Error("the once-per-process revoke entry outlived the cleared mark")
	}
}

// stopBarrierRunner stops a sandbox only once n callers are stopping it, so concurrent retries reach the
// clear of the run's ref together.
type stopBarrierRunner struct {
	runner.Runner
	n       int32
	arrived atomic.Int32
	stops   atomic.Int32
}

func (r *stopBarrierRunner) StopSandbox(ctx context.Context, _ string) error {
	r.stops.Add(1)
	r.arrived.Add(1)
	deadline := time.Now().Add(10 * time.Second)
	for r.arrived.Load() < r.n && time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Millisecond):
		}
	}
	return nil
}

// Two replicas retrying one forced stop both stop the sandbox, and only the clear that changed the run's ref
// writes the success row.
func TestForcedRetry_TwoReplicasOneSuccessRow(t *testing.T) {
	pool := throwawayPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	id := uuid.New()
	now := time.Now().UTC()
	if _, err := pg.CreateRun(ctx, types.AgentRun{
		ID: id, CreatedAt: now, UpdatedAt: now, CreatedBy: "t@example.com", Agent: "claude-code",
		ConfinementClass: types.CC1, State: types.RunRunning, RunnerTarget: "docker",
		SandboxRef: "sbx-forced", SPIFFEID: "spiffe://wardyn.local/agent-run/" + id.String(),
	}); err != nil {
		t.Fatalf("create run: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE agent_runs SET lost_at = $2, lost_reason = 'ended' WHERE id = $1`, id, now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	run, err := pg.GetRun(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, applied, err := pg.StopKeptRunForcedIf(ctx, id, types.RunStopped, run.LostAt, run.LostReason, run.EndsAt); err != nil || !applied {
		t.Fatalf("forced claim = %v, %v; want applied", applied, err)
	}
	marks, err := pg.ListForcedStops(ctx)
	if err != nil || len(marks) != 1 {
		t.Fatalf("marks = %v, %v; want one", marks, err)
	}
	rec := &recRecorder{}
	rn := &stopBarrierRunner{n: 2}
	var wg sync.WaitGroup
	for range 2 {
		srv := New(Config{Store: pg, Runner: rn, Audit: rec})
		wg.Add(1)
		go func() {
			defer wg.Done()
			srv.retryForcedStop(ctx, pg, marks[0])
		}()
	}
	wg.Wait()
	if rn.stops.Load() != 2 {
		t.Fatalf("%d sandbox stops, want both replicas to stop it", rn.stops.Load())
	}
	if rows := sweepRows(rec, "success", "sandbox_stopped"); len(rows) != 1 {
		t.Errorf("%d success rows, want 1", len(rows))
	}
}

// withoutOwedPass takes the fixture's computer folder out of the both-ways shape, so a forced end owes no pass back
// and reaches the sandbox stop.
func (f *forced) withoutOwedPass() {
	f.cs.localFolderStore.mu.Lock()
	defer f.cs.localFolderStore.mu.Unlock()
	f.cs.folder.Direction = types.ComputerFolderIn
}
