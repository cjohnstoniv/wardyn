// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// orderLog is the one list the identity, the broker, the runner's exec and its
// teardown all report into, so the order of the four is the test's evidence.
type orderLog struct {
	mu sync.Mutex
	l  []string
}

func (o *orderLog) add(s string) { o.mu.Lock(); o.l = append(o.l, s); o.mu.Unlock() }
func (o *orderLog) got() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return slices.Clone(o.l)
}

type orderIdentity struct {
	identity.Provider
	o *orderLog
}

func (i orderIdentity) RevokeRun(ctx context.Context, id uuid.UUID) error {
	i.o.add("revoke identity")
	return i.Provider.RevokeRun(ctx, id)
}

type orderBroker struct {
	raceBroker
	o *orderLog
}

func (b *orderBroker) RevokeRun(ctx context.Context, id uuid.UUID) error {
	b.o.add("revoke broker")
	return b.raceBroker.RevokeRun(ctx, id)
}

// paneRunner is a runner whose ExecStream is the test's tmux: every exec is
// recorded with its spec, and the teardown goes into the order log.
type paneRunner struct {
	*outputRunner
	o    *orderLog
	pane func(spec runner.ExecSpec) (*runner.ExecSession, error)

	mu    sync.Mutex
	specs []runner.ExecSpec
}

func (r *paneRunner) ExecStream(_ context.Context, _ string, spec runner.ExecSpec) (*runner.ExecSession, error) {
	r.mu.Lock()
	r.specs = append(r.specs, spec)
	r.mu.Unlock()
	r.o.add("snapshot")
	return r.pane(spec)
}

func (r *paneRunner) StopSandbox(context.Context, string) error { r.o.add("stop"); return nil }

func (r *paneRunner) execs() []runner.ExecSpec {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.specs)
}

// textPane is a tmux that prints text and exits with code.
func textPane(text string, code int) func(runner.ExecSpec) (*runner.ExecSession, error) {
	return func(runner.ExecSpec) (*runner.ExecSession, error) {
		return &runner.ExecSession{
			Stdout: strings.NewReader(text),
			Stderr: strings.NewReader(""),
			Wait:   func() (int, error) { return code, nil },
			Close:  func() error { return nil },
		}, nil
	}
}

// paneFixture is an output fixture whose run is interactive and whose runner is
// a paneRunner; the order log sees the revocations, the exec and the teardown.
type paneFixture struct {
	*outputFixture
	pr *paneRunner
	o  *orderLog
}

func newPaneFixture(t *testing.T, pane func(runner.ExecSpec) (*runner.ExecSession, error), shape ...func(*Config)) *paneFixture {
	t.Helper()
	o := &orderLog{}
	pr := &paneRunner{o: o, pane: pane}
	f := newOutputFixture(t, append([]func(*Config){func(c *Config) {
		pr.outputRunner = c.Runner.(*outputRunner)
		c.Runner = pr
		c.Identity = orderIdentity{Provider: c.Identity, o: o}
		c.Broker = &orderBroker{o: o}
	}}, shape...)...)
	f.st.mu.Lock()
	f.st.run.Interactive = true
	f.st.mu.Unlock()
	f.run.Interactive = true
	return &paneFixture{outputFixture: f, pr: pr, o: o}
}

func (f *paneFixture) snapshotAudit() []types.AuditEvent {
	return f.audit.eventsFor(f.run.ID, "run.output.snapshot")
}

// An idle-stopped interactive run keeps a pane_snapshot row in which a
// registered secret shown in the pane is absent from the stored bytes; the
// audit row names the outcome and the byte count and never the pane.
func TestPaneSnapshot_MasksBeforeInsertAndAuditsWithoutContent(t *testing.T) {
	reg := secretmask.NewRegistry()
	const secret = "s3cr3t-token-value"
	f := newPaneFixture(t, textPane("$ echo "+secret+"\nhello "+secret+" there\n$ ", 0),
		func(c *Config) { c.MaskRegistry = reg })
	reg.Add(f.run.ID, []byte(secret))

	f.srv.SnapshotRunPane(t.Context(), f.run.ID)
	f.srv.FinishRunOutput(t.Context(), f.run.ID)

	row, ok := f.mem.row(f.run.ID)
	if !ok || row.CapturedAt == nil || row.Source != "pane_snapshot" {
		t.Fatalf("row %+v (found %v), want a final pane_snapshot row", row, ok)
	}
	if strings.Contains(string(row.Output), "s3cr3t") {
		t.Fatalf("the stored pane holds the secret: %q", row.Output)
	}
	if want := "$ echo <secret-hidden>\nhello <secret-hidden> there\n$ "; string(row.Output) != want {
		t.Fatalf("stored %q, want %q", row.Output, want)
	}
	if row.Incomplete || row.CaptureGap || row.MaskScope != "" {
		t.Errorf("a clean capture came out %+v", row)
	}
	specs := f.pr.execs()
	if len(specs) != 1 || specs[0].TTY || !slices.Equal(specs[0].Argv, paneSnapshotArgv) {
		t.Fatalf("execs %+v, want exactly the capture-pane argv with no TTY", specs)
	}
	if got := strings.Join(paneSnapshotArgv, " "); got != "tmux capture-pane -p -J -S - -t wardyn" {
		t.Errorf("argv %q", got)
	}
	evs := f.snapshotAudit()
	if len(evs) != 1 || evs[0].Outcome != "success" || evs[0].ActorType != types.ActorSystem || evs[0].Actor != "wardynd" {
		t.Fatalf("audit rows %+v, want one successful system row by wardynd", evs)
	}
	if d := string(evs[0].Data); !strings.Contains(d, `"bytes":`) || strings.Contains(d, "hello") || strings.Contains(d, "echo") {
		t.Errorf("audit data %s must carry a byte count and no pane content", d)
	}
	if f.mem.saveCount(f.run.ID) != 1 {
		t.Errorf("%d final writes, want exactly 1", f.mem.saveCount(f.run.ID))
	}
}

// The history is cut to the tail size from the front, and the row says so.
func TestPaneSnapshot_KeepsTheLastTailBytesOfALongPane(t *testing.T) {
	const tail = 64
	pane := strings.Repeat("a", 3*tail) + strings.Repeat("z", tail)
	f := newPaneFixture(t, textPane(pane, 0), func(c *Config) { c.RunOutputTailBytes = tail })
	f.srv.SnapshotRunPane(t.Context(), f.run.ID)
	row, ok := f.mem.row(f.run.ID)
	if !ok || string(row.Output) != strings.Repeat("z", tail) || !row.Truncated {
		t.Fatalf("row %+v (found %v), want the last %d bytes, truncated", row, ok, tail)
	}
}

// A stop that is not a graceful one snapshots nothing and issues no exec, on
// every other path that ends an interactive run.
func TestPaneSnapshot_OtherEndsNeverExec(t *testing.T) {
	for _, tc := range []struct {
		name    string
		trigger func(t *testing.T, f *paneFixture)
	}{
		{"kill", func(t *testing.T, f *paneFixture) {
			w := do(t, f.srv, http.MethodPost, "/api/v1/runs/"+f.run.ID.String()+"/kill", adminToken, "")
			if w.Code != http.StatusAccepted {
				t.Fatalf("kill = %d %s", w.Code, w.Body)
			}
			f.srv.WaitBackground()
		}},
		{"reconcile", func(t *testing.T, f *paneFixture) {
			f.srv.reconcileFinalize(t.Context(), f.run.ID, types.RunFailed, "sbx-out", "reconciled exit")
		}},
		{"failure", func(t *testing.T, f *paneFixture) {
			f.srv.failAndRevoke(t.Context(), f.run.ID, types.RunRunning, "the task could not start")
		}},
		{"probe reclaim", func(t *testing.T, f *paneFixture) { f.srv.reclaimProbeRun(t.Context(), f.run.ID) }},
		{"watcher win", func(t *testing.T, f *paneFixture) {
			f.srv.finalizeRunTail(t.Context(), f.run.ID, "sbx-out", "run.complete", "success", map[string]any{})
		}},
		{"lease end into FAILED", func(t *testing.T, f *paneFixture) {
			f.srv.finalizeRunTailOrdered(t.Context(), f.run.ID, "sbx-out", "run.lost", "success", map[string]any{}, false, false)
		}},
		{"the whole contract without a graceful stop", func(t *testing.T, f *paneFixture) {
			f.srv.FinishRunOutput(t.Context(), f.run.ID)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPaneFixture(t, textPane("the screen\n", 0))
			tc.trigger(t, f)
			if n := len(f.pr.execs()); n != 0 {
				t.Fatalf("%d execs, want none", n)
			}
			if _, ok := f.mem.row(f.run.ID); ok {
				if r, _ := f.mem.row(f.run.ID); r.Source == "pane_snapshot" {
					t.Fatalf("a pane_snapshot row exists: %+v", r)
				}
			}
			if n := len(f.snapshotAudit()); n != 0 {
				t.Errorf("%d run.output.snapshot rows, want none", n)
			}
		})
	}
}

// The harness sign-in run's pane holds the credential it exists to print: no
// exec is issued, however the run is stopped.
func TestPaneSnapshot_SignInRunNeverExecs(t *testing.T) {
	f := newPaneFixture(t, textPane("sk-ant-oat01-the-token\n", 0))
	f.st.mu.Lock()
	f.st.run.Task = harnessLoginTask
	f.st.mu.Unlock()

	f.srv.SnapshotRunPane(t.Context(), f.run.ID)
	f.srv.finalizeRunTailOrdered(t.Context(), f.run.ID, "sbx-out", "run.ended", "success", map[string]any{}, false, true)
	if n := len(f.pr.execs()); n != 0 {
		t.Fatalf("%d execs for the sign-in run, want none", n)
	}
	if r, ok := f.mem.row(f.run.ID); ok && r.Source == "pane_snapshot" {
		t.Fatalf("a pane_snapshot row exists for the sign-in run: %+v", r)
	}
	if n := len(f.snapshotAudit()); n != 0 {
		t.Errorf("%d run.output.snapshot rows, want none", n)
	}
}

// The identity and broker revocations are done before the snapshot's exec
// starts, and the exec is done before StopSandbox.
func TestPaneSnapshot_RunsAfterTheRevocationsAndBeforeTeardown(t *testing.T) {
	f := newPaneFixture(t, textPane("the screen\n", 0))
	f.srv.finalizeRunTailOrdered(t.Context(), f.run.ID, "sbx-out", "run.ended", "success", map[string]any{}, false, true)
	want := []string{"revoke identity", "revoke broker", "snapshot", "stop"}
	if got := f.o.got(); !slices.Equal(got, want) {
		t.Fatalf("order %v, want %v", got, want)
	}
	if r, _ := f.mem.row(f.run.ID); r.Source != "pane_snapshot" {
		t.Fatalf("row %+v, want the snapshot", r)
	}
}

// Every way the capture can fail leaves no row, one failure audit row, and a
// teardown that goes ahead.
func TestPaneSnapshot_FailuresLeaveNoRow(t *testing.T) {
	for _, tc := range []struct {
		name, reason string
		pane         func(runner.ExecSpec) (*runner.ExecSession, error)
	}{
		{"no tmux", "exec_failed", func(runner.ExecSpec) (*runner.ExecSession, error) { return nil, errors.New("exec: tmux: not found") }},
		{"sandbox gone", "sandbox_gone", func(runner.ExecSpec) (*runner.ExecSession, error) { return nil, runner.ErrSandboxGone }},
		{"no session", "exit_nonzero", textPane("no server running on /tmp/tmux-1000/default\n", 1)},
		{"no exec session", "exec_failed", func(runner.ExecSpec) (*runner.ExecSession, error) { return nil, nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPaneFixture(t, tc.pane)
			f.srv.finalizeRunTailOrdered(t.Context(), f.run.ID, "sbx-out", "run.ended", "success", map[string]any{}, false, true)
			if r, ok := f.mem.row(f.run.ID); ok && r.Source == "pane_snapshot" {
				t.Fatalf("a row exists: %+v", r)
			}
			evs := f.snapshotAudit()
			if len(evs) != 1 || evs[0].Outcome != "failure" || !strings.Contains(string(evs[0].Data), `"reason":"`+tc.reason+`"`) {
				t.Fatalf("audit rows %+v, want one failure with reason %s", evs, tc.reason)
			}
			if got := f.o.got(); got[len(got)-1] != "stop" {
				t.Errorf("order %v: the teardown did not go ahead", got)
			}
		})
	}
}

// A pane that streams without end is cut at the bound: no row, a memory use
// capped by the tail size, and teardown completes.
func TestPaneSnapshot_EndlessPaneIsCutAtTheBound(t *testing.T) {
	var cutOff sync.WaitGroup
	f := newPaneFixture(t, func(runner.ExecSpec) (*runner.ExecSession, error) {
		pr, pw := io.Pipe()
		er, ew := io.Pipe()
		cutOff.Add(2)
		go func() {
			defer cutOff.Done()
			chunk := []byte(strings.Repeat("never ends\n", 100))
			for {
				if _, err := pw.Write(chunk); err != nil {
					return
				}
			}
		}()
		go func() {
			defer cutOff.Done()
			for {
				if _, err := ew.Write([]byte("tmux: still talking\n")); err != nil {
					return
				}
			}
		}()
		return &runner.ExecSession{
			Stdout: pr, Stderr: er,
			Wait:  func() (int, error) { select {} },
			Close: func() error { _ = pr.Close(); _ = er.Close(); return nil },
		}, nil
	}, func(c *Config) { c.RunOutputTailBytes = 256 })
	f.srv.paneSnapshotTimeoutOverride = 150 * time.Millisecond

	start := time.Now()
	f.srv.finalizeRunTailOrdered(t.Context(), f.run.ID, "sbx-out", "run.ended", "success", map[string]any{}, false, true)
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("the teardown took %s, want it held only by the %s bound", d, f.srv.paneSnapshotBound())
	}
	if r, ok := f.mem.row(f.run.ID); ok && r.Source == "pane_snapshot" {
		t.Fatalf("a row exists for a pane that never ended: %+v", r)
	}
	evs := f.snapshotAudit()
	if len(evs) != 1 || evs[0].Outcome != "failure" || !strings.Contains(string(evs[0].Data), `"reason":"timeout"`) {
		t.Fatalf("audit rows %+v, want one timeout failure", evs)
	}
	if got := f.o.got(); got[len(got)-1] != "stop" {
		t.Errorf("order %v: the teardown did not go ahead", got)
	}
	cutOff.Wait() // closing the exec ended both streams
}

// Stderr is drained while stdout is read: a tmux that fills an unbuffered
// stderr pipe before it can exit does not hold the capture to its bound.
func TestPaneSnapshot_DrainsStderr(t *testing.T) {
	f := newPaneFixture(t, func(runner.ExecSpec) (*runner.ExecSession, error) {
		er, ew := io.Pipe()
		done := make(chan struct{})
		go func() {
			defer close(done)
			defer ew.Close()
			_, _ = ew.Write([]byte(strings.Repeat("warning\n", 1<<14))) // blocks until read
		}()
		return &runner.ExecSession{
			Stdout: strings.NewReader("the screen\n"), Stderr: er,
			Wait:  func() (int, error) { <-done; return 0, nil },
			Close: func() error { return nil },
		}, nil
	})
	f.srv.SnapshotRunPane(t.Context(), f.run.ID)
	if r, _ := f.mem.row(f.run.ID); string(r.Output) != "the screen\n" || r.Source != "pane_snapshot" {
		t.Fatalf("row %+v, want the snapshot (stderr was not drained)", r)
	}
}

// Persistence off, or the master switch off, takes no snapshot.
func TestPaneSnapshot_OffSwitches(t *testing.T) {
	for name, shape := range map[string]func(*Config){
		"persistence off": func(c *Config) { c.RunOutputPersistOff = true },
		"capture off":     func(c *Config) { c.ExecOutputTailOff = true },
	} {
		t.Run(name, func(t *testing.T) {
			f := newPaneFixture(t, textPane("the screen\n", 0), shape)
			f.srv.SnapshotRunPane(t.Context(), f.run.ID)
			if n := len(f.pr.execs()); n != 0 {
				t.Fatalf("%d execs, want none", n)
			}
			if _, ok := f.mem.row(f.run.ID); ok {
				t.Fatal("a row was written")
			}
		})
	}
}

// A run ended by the max-age stop gets exactly one final row on either shape:
// the pane snapshot for an interactive run, stdout for a headless one. The
// stopper calls the same two functions the idle stop does.
func TestPaneSnapshot_MaxAgeStopYieldsOneFinalRow(t *testing.T) {
	t.Run("interactive", func(t *testing.T) {
		f := newPaneFixture(t, textPane("the screen\n", 0))
		f.srv.SnapshotRunPane(t.Context(), f.run.ID)
		f.srv.FinishRunOutput(t.Context(), f.run.ID)
		f.srv.FinishRunOutput(t.Context(), f.run.ID)
		if r, _ := f.mem.row(f.run.ID); r.Source != "pane_snapshot" || f.mem.saveCount(f.run.ID) != 1 {
			t.Fatalf("row %+v after %d writes, want one pane_snapshot", r, f.mem.saveCount(f.run.ID))
		}
		f.srv.SnapshotRunPane(t.Context(), f.run.ID)
		if n := len(f.pr.execs()); n != 1 {
			t.Errorf("%d execs, want the snapshot taken once", n)
		}
	})
	t.Run("headless", func(t *testing.T) {
		f := newPaneFixture(t, textPane("the screen\n", 0))
		f.st.mu.Lock()
		f.st.run.Interactive = false
		f.st.mu.Unlock()
		run := f.run
		run.Interactive = false
		w, ok := f.srv.openExecOutput(run, false).(*tailWriter)
		if !ok {
			t.Fatal("no tail writer")
		}
		writeExecOutput(t, w, "line one\n")
		f.srv.SnapshotRunPane(t.Context(), f.run.ID)
		f.srv.FinishRunOutput(t.Context(), f.run.ID)
		if r, _ := f.mem.row(f.run.ID); r.Source != "stdout" || string(r.Output) != "line one\n" || f.mem.saveCount(f.run.ID) != 1 {
			t.Fatalf("row %+v after %d writes, want one stdout row", r, f.mem.saveCount(f.run.ID))
		}
		if n := len(f.pr.execs()); n != 0 {
			t.Errorf("%d execs for a headless run, want none", n)
		}
	})
}
