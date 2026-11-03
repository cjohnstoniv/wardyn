// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/maskmanifest"
	"github.com/cjohnstoniv/wardyn/internal/recording"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const agentCastSecret = "macos-agent-secret-token-5678"

// castStore is the output fixture's store plus what the agent output decision reads and writes: the
// organisation's recording requirement, the run's mark (GetRun reads it back) and the failure hint.
type castStore struct {
	*memRunOutputs
	store.RunnerPoolStore
	st *dispatchTestStore

	mu         sync.Mutex
	required   bool
	mark       types.RunNotRecorded
	declines   int   // runner_declined marks written
	owed       bool  // agent_cast_owed_at is set
	oweErr     error // SetRunAgentCastOwed fails with it
	clearFails int   // the next N ClearRunAgentCastOwed calls fail
}

func (c *castStore) SetRunAgentCastOwed(context.Context, uuid.UUID) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.oweErr != nil {
		return c.oweErr
	}
	c.owed = true
	return nil
}

func (c *castStore) DeclineRunAgentCast(context.Context, uuid.UUID) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.owed || c.mark == types.NotRecordedRunnerDeclined {
		return false, nil
	}
	c.mark = types.NotRecordedRunnerDeclined
	c.declines++
	return true, nil
}

func (c *castStore) ClearRunAgentCastOwed(_ context.Context, _ uuid.UUID, recorded bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.clearFails > 0 {
		c.clearFails--
		return errors.New("store unavailable")
	}
	c.owed = false
	if recorded && c.mark == types.NotRecordedRunnerDeclined {
		c.mark = ""
	}
	return nil
}

// ListRunsAgentCastOwed lists the fixture's run while it owes its cast and is terminal; age is not kept.
func (c *castStore) ListRunsAgentCastOwed(context.Context, time.Duration, int) ([]uuid.UUID, error) {
	state := c.st.State()
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.owed || slices.Contains(types.NonTerminalRunStates, state) {
		return nil, nil
	}
	return []uuid.UUID{c.st.run.ID}, nil
}

func (c *castStore) owing() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.owed
}

func (c *castStore) GetRunnerPoolOrgDefaults(context.Context) (types.RunnerPoolDefaults, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.required {
		return types.RunnerPoolDefaults{Recording: types.RecordingRequired}, nil
	}
	return types.RunnerPoolDefaults{}, nil
}

func (c *castStore) GetRun(ctx context.Context, id uuid.UUID) (types.AgentRun, error) {
	run, err := c.memRunOutputs.GetRun(ctx, id)
	run.NotRecorded = c.marked()
	return run, err
}

func (c *castStore) SetRunNotRecorded(_ context.Context, _ uuid.UUID, v types.RunNotRecorded) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.mark = v
	if v == types.NotRecordedRunnerDeclined {
		c.declines++
	}
	return nil
}

func (c *castStore) ClearRunDeclined(context.Context, uuid.UUID) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.mark != types.NotRecordedRunnerDeclined {
		return false, nil
	}
	c.mark = ""
	return true, nil
}

func (c *castStore) SetRunFailureHint(ctx context.Context, id uuid.UUID, hint string) error {
	return c.st.SetRunFailureHint(ctx, id, hint)
}

func (c *castStore) marked() types.RunNotRecorded {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.mark
}

func (c *castStore) declineCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.declines
}

// failingCasts is a recording store whose saves fail.
type failingCasts struct{ recording.Store }

func (failingCasts) SaveCastNamed(context.Context, string, string, io.Reader) error {
	return errors.New("recording store full")
}

// agentCastFixture is an output fixture for a run of kind, with a recording store, a registry holding
// agentCastSecret for the run, and castStore over its store. shape edits the config first.
func agentCastFixture(t *testing.T, kind types.MachineKind, shape ...func(*Config)) (*outputFixture, *castStore, recording.Store) {
	t.Helper()
	casts := fsRecordingStore(t)
	reg := secretmask.NewRegistry()
	f := newOutputFixture(t, append([]func(*Config){func(c *Config) { c.RecordingStore, c.MaskRegistry = casts, reg }}, shape...)...)
	reg.Add(f.run.ID, []byte(agentCastSecret))
	f.run.MachineKind, f.st.run.MachineKind = kind, kind
	cs := &castStore{memRunOutputs: f.mem, st: f.st}
	f.srv.cfg.Store = cs
	return f, cs, casts
}

// castRows is the run's session.recording.write rows for session, decoded.
func castRows(t *testing.T, f *outputFixture, session string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, ev := range f.audit.eventsFor(f.run.ID, "session.recording.write") {
		var d map[string]any
		if err := json.Unmarshal(ev.Data, &d); err != nil {
			t.Fatal(err)
		}
		if d["session"] == session {
			d["outcome"], d["target"] = ev.Outcome, ev.Target
			out = append(out, d)
		}
	}
	return out
}

// waitCastRow waits for the run's one session.recording.write row for session, and returns it.
func waitCastRow(t *testing.T, f *outputFixture, session string) map[string]any {
	t.Helper()
	waitFor(t, "the "+session+" recording row", func() bool { return len(castRows(t, f, session)) > 0 })
	rows := castRows(t, f, session)
	if len(rows) != 1 {
		t.Fatalf("%d %s rows, want 1: %v", len(rows), session, rows)
	}
	return rows[0]
}

// readCast is the stored cast for the run's session, its output decoded, asserting it holds no secret.
func readCast(t *testing.T, casts recording.Store, runID uuid.UUID, session string) string {
	t.Helper()
	rc, err := casts.OpenCast(context.Background(), recording.CastKey(runID.String(), session))
	if err != nil {
		t.Fatalf("open the %s cast: %v", session, err)
	}
	defer rc.Close()
	body, _ := io.ReadAll(rc)
	assertCastHasNoSecret(t, string(body), agentCastSecret, "the "+session+" cast")
	return decodeCastOutput(t, string(body))
}

// startDispatch runs dispatch's recording step for the fixture's run, STARTING, under site.
func startDispatch(f *outputFixture, site types.SiteConfig, interactive bool) (types.AgentRun, runner.SandboxSpec, bool) {
	f.st.mu.Lock()
	f.st.state = types.RunStarting
	f.st.mu.Unlock()
	run := f.run
	var spec runner.SandboxSpec
	ok := f.srv.startRunRecording(context.Background(), &run, &spec, types.RunPolicySpec{}, dispatchCeiling{}, site, nil, interactive)
	return run, spec, ok
}

// A macOS VM task run at Terminal gets its agent output cast on its tail: it starts recorded, and the
// agent's output is stored masked under <run>~agent with a session.recording.write row. Under a
// requirement it starts too (its cast opened).
func TestAgentCast_MacOSVMTaskRunIsRecorded(t *testing.T) {
	for _, required := range []bool{false, true} {
		t.Run(map[bool]string{false: "preferred", true: "required"}[required], func(t *testing.T) {
			f, cs, casts := agentCastFixture(t, types.MachineMacOSVM)
			cs.required = required
			run, spec, ok := startDispatch(f, types.SiteConfig{}, false)
			if !ok || run.NotRecorded != "" || !cs.owing() || f.st.FailureHint() != "" {
				t.Fatalf("started=%v not_recorded=%q owed=%v hint=%q, want started recorded, its cast owed", ok, run.NotRecorded, cs.owing(), f.st.FailureHint())
			}
			tw, _ := spec.ExecOutput.(*tailWriter)
			if tw == nil || tw.cast == nil || tw.cast.session != agentCastSession {
				t.Fatalf("exec output %T, want the tail with its agent cast", spec.ExecOutput)
			}
			tw.BeginDrain()
			_, _ = tw.Write([]byte("token is " + agentCastSecret + "\r\n"))
			tw.EndDrain(nil)
			row := waitCastRow(t, f, agentCastSession)
			if row["outcome"] != "success" || row["target"] != recording.CastKey(run.ID.String(), agentCastSession) || row["recovered"] != nil {
				t.Fatalf("row %v, want one success row for the agent cast", row)
			}
			if out := readCast(t, casts, run.ID, agentCastSession); !strings.Contains(out, "<secret-hidden>") {
				t.Fatalf("the cast is not masked: %q", out)
			}
			// The save settled the obligation; the run was never marked.
			waitFor(t, "the obligation to clear", func() bool { return !cs.owing() })
			if cs.marked() != "" || cs.declineCount() != 0 {
				t.Fatalf("not_recorded = %q after %d marks, want the run recorded", cs.marked(), cs.declineCount())
			}
		})
	}
}

// blockingCasts is a recording store whose dispatch cast saves wait until release closes, then fail: a
// process that never finishes its save. Other saves go through.
type blockingCasts struct {
	recording.Store
	release chan struct{}
}

func (b blockingCasts) SaveCastNamed(ctx context.Context, runID, suffix string, r io.Reader) error {
	if suffix != agentCastSession {
		return b.Store.SaveCastNamed(ctx, runID, suffix, r)
	}
	<-b.release
	return errors.New("the process went before its save")
}

// A process commits the run's output row while the dispatch cast's save is still pending, then dies (its
// tail and cast are dropped, the save never lands). No recovery runs for a run with a final row; the
// sweep settles the obligation: with a runner that keeps the copy, a recovered cast that leaves the run
// recorded; without one, a durable runner_declined and the failure row.
func TestAgentCast_ADeathMidSaveIsSettledByTheSweep(t *testing.T) {
	for _, recoverable := range []bool{true, false} {
		t.Run(map[bool]string{true: "the runner keeps the copy", false: "nothing to re-read"}[recoverable], func(t *testing.T) {
			var shape []func(*Config)
			rr := newRecoveringRunner()
			rr.log = "agent output, re-read\n"
			if recoverable {
				shape = append(shape, func(c *Config) { c.Runner = rr })
			}
			f, cs, casts := agentCastFixture(t, types.MachineMacOSVM, shape...)
			release := make(chan struct{})
			t.Cleanup(func() { close(release) })
			f.srv.cfg.RecordingStore = blockingCasts{Store: casts, release: release}
			_, spec, ok := startDispatch(f, types.SiteConfig{}, false)
			tw, _ := spec.ExecOutput.(*tailWriter)
			if !ok || tw == nil || tw.cast == nil || !cs.owing() {
				t.Fatalf("started=%v owed=%v, want started with a cast owed", ok, cs.owing())
			}
			tw.BeginDrain()
			_, _ = tw.Write([]byte("agent output\n"))
			tw.EndDrain(nil)
			f.st.mu.Lock()
			f.st.state = types.RunCompleted
			f.st.mu.Unlock()
			f.srv.FinishRunOutput(t.Context(), f.run.ID) // the drain is released while the save waits
			f.mem.mu.Lock()
			final := f.mem.rows[f.run.ID].CapturedAt != nil
			f.mem.mu.Unlock()
			if !final || cs.marked() != "" || !cs.owing() {
				t.Fatalf("final row %v, mark %q, owed %v: want the row committed with the cast still owed", final, cs.marked(), cs.owing())
			}

			dropTail(f) // the process goes; its cast and save with it
			if err := f.srv.SweepRunOutputs(t.Context()); err != nil {
				t.Fatal(err)
			}
			if cs.owing() {
				t.Fatal("the sweep left the obligation")
			}
			if recoverable {
				row := waitCastRow(t, f, agentCastRecoveredSession)
				if row["outcome"] != "success" || row["recovered"] != true || cs.marked() != "" {
					t.Fatalf("row %v mark %q, want a recovered cast and the run recorded", row, cs.marked())
				}
				if out := readCast(t, casts, f.run.ID, agentCastRecoveredSession); !strings.Contains(out, "re-read") {
					t.Fatalf("recovered cast %q", out)
				}
				return
			}
			row := waitCastRow(t, f, agentCastSession)
			if cs.marked() != types.NotRecordedRunnerDeclined || row["outcome"] != "failure" || row["reason"] != "no_tail" {
				t.Fatalf("mark %q row %v, want runner_declined with a no_tail failure row", cs.marked(), row)
			}
		})
	}
}

// The obligation cannot be stored at dispatch: the run cannot promise its recording, so it follows the
// required rule as a cast that would not open does.
func TestAgentCast_AnObligationThatCannotBeStoredIsNoCast(t *testing.T) {
	for _, required := range []bool{true, false} {
		f, cs, _ := agentCastFixture(t, types.MachineMacOSVM)
		cs.required, cs.oweErr = required, errors.New("connection reset")
		run, spec, ok := startDispatch(f, types.SiteConfig{}, false)
		if tw, _ := spec.ExecOutput.(*tailWriter); tw == nil || tw.cast != nil {
			t.Fatalf("required=%v: the tail kept a cast with no obligation behind it", required)
		}
		assertNoCastDecision(t, f, cs, run, ok, required)
	}
}

// Required rule: a macOS VM task run whose agent output cast cannot open (the tail is off, masking cannot
// be proven, its output was erased, or the recorder does not open) is marked runner_declined; under a
// requirement it fails at start with the sentence and a run.create failure row; under a locked level
// alone, or with nothing asking, it starts with the mark stored.
func TestAgentCast_MacOSVMWithNoCastFollowsTheRequiredRule(t *testing.T) {
	exits := map[string]struct {
		shape func(*Config)
		edit  func(*outputFixture)
	}{
		"the tail is off": {shape: func(c *Config) { c.ExecOutputTailOff = true }},
		"masking cannot be proven": {shape: func(c *Config) {
			pool, _ := miscCovClosedPool(t)
			c.MaskManifests = maskmanifest.New(pool, nil, c.MaskRegistry)
		}},
		"the output was erased": {edit: func(f *outputFixture) { f.mem.erased[f.run.ID] = true }},
	}
	sites := map[string]struct {
		required bool
		site     types.SiteConfig
	}{
		"required":     {required: true},
		"locked level": {site: types.SiteConfig{Recording: &types.SiteRecording{Level: types.RecordingTerminal, Locked: true}}},
		"nothing asks": {},
	}
	for exit, x := range exits {
		for name, sc := range sites {
			t.Run(exit+", "+name, func(t *testing.T) {
				var shape []func(*Config)
				if x.shape != nil {
					shape = append(shape, x.shape)
				}
				f, cs, _ := agentCastFixture(t, types.MachineMacOSVM, shape...)
				cs.required = sc.required
				if x.edit != nil {
					x.edit(f)
				}
				run, spec, ok := startDispatch(f, sc.site, false)
				if spec.ExecOutput != nil {
					t.Fatalf("exec output %T, want none", spec.ExecOutput)
				}
				assertNoCastDecision(t, f, cs, run, ok, sc.required)
			})
		}
	}
	for name, sc := range sites {
		t.Run("the recorder does not open, "+name, func(t *testing.T) {
			f, cs, _ := agentCastFixture(t, types.MachineMacOSVM)
			cs.required = sc.required
			f.st.mu.Lock()
			f.st.state = types.RunStarting
			f.st.mu.Unlock()
			spec := runner.SandboxSpec{ExecOutput: f.open(t)}
			f.srv.cfg.RecordingStore = nil // what the recorder needs; the mark was settled before
			run := f.run
			ok := f.srv.recordMacOSAgent(context.Background(), &run, &spec, false)
			assertNoCastDecision(t, f, cs, run, ok, sc.required)
		})
	}
}

func assertNoCastDecision(t *testing.T, f *outputFixture, cs *castStore, run types.AgentRun, started, required bool) {
	t.Helper()
	if run.NotRecorded != types.NotRecordedRunnerDeclined {
		t.Fatalf("not_recorded = %q, want runner_declined", run.NotRecorded)
	}
	fails := 0
	for _, ev := range f.audit.eventsFor(f.run.ID, "run.create") {
		if ev.Outcome == "failure" {
			fails++
		}
	}
	if required {
		if started || f.st.FailureHint() != macOSAgentOutputRequiredMsg || fails != 1 || f.st.State() != types.RunFailed {
			t.Fatalf("started=%v hint=%q run.create failures=%d state=%s, want failed at start with the sentence and one row",
				started, f.st.FailureHint(), fails, f.st.State())
		}
		return
	}
	if !started || cs.marked() != types.NotRecordedRunnerDeclined || fails != 0 || f.st.FailureHint() != "" {
		t.Fatalf("started=%v stored mark=%q run.create failures=%d hint=%q, want started with runner_declined stored",
			started, cs.marked(), fails, f.st.FailureHint())
	}
}

// What gets no tee: a run at off (marked policy_off, no cast), a Linux VM and a container (their own
// runner's recording decides them; this test runner keeps none, so they are marked runner_declined as
// before), and an interactive macOS VM run (no agent exec; it is recorded, with no tail at all).
func TestAgentCast_OnlyAMacOSVMTaskRunAboveOffIsTeed(t *testing.T) {
	off := types.SiteConfig{Recording: &types.SiteRecording{Level: types.RecordingOff, Locked: true}}
	for _, tc := range []struct {
		name        string
		kind        types.MachineKind
		site        types.SiteConfig
		interactive bool
		mark        types.RunNotRecorded
		tail        bool
	}{
		{"a macOS VM run at off", types.MachineMacOSVM, off, false, types.NotRecordedPolicyOff, true},
		{"a Linux VM run", types.MachineLinuxVM, types.SiteConfig{}, false, types.NotRecordedRunnerDeclined, true},
		{"a container run", types.MachineContainer, types.SiteConfig{}, false, types.NotRecordedRunnerDeclined, true},
		{"an interactive macOS VM run", types.MachineMacOSVM, types.SiteConfig{}, true, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, _, _ := agentCastFixture(t, tc.kind)
			run, spec, ok := startDispatch(f, tc.site, tc.interactive)
			if !ok || run.NotRecorded != tc.mark {
				t.Fatalf("started=%v not_recorded=%q, want started marked %q", ok, run.NotRecorded, tc.mark)
			}
			tw, _ := spec.ExecOutput.(*tailWriter)
			if (tw != nil) != tc.tail || (tw != nil && tw.cast != nil) {
				t.Fatalf("exec output %T (cast %v), want a tail=%v with no cast", spec.ExecOutput, tw != nil && tw.cast != nil, tc.tail)
			}
		})
	}
}

// The dispatch cast's own save failing takes the same decision as a re-read that gives nothing: the run
// is marked runner_declined on the row's failure. An agent that printed nothing saves no cast and stays
// recorded: there was nothing to record.
func TestAgentCast_SaveFailureMarksTheRunAndSilenceKeepsItRecorded(t *testing.T) {
	t.Run("the save fails", func(t *testing.T) {
		f, cs, _ := agentCastFixture(t, types.MachineMacOSVM)
		f.srv.cfg.RecordingStore = failingCasts{Store: f.srv.cfg.RecordingStore}
		run, spec, ok := startDispatch(f, types.SiteConfig{}, false)
		tw, _ := spec.ExecOutput.(*tailWriter)
		if !ok || tw == nil || tw.cast == nil || run.NotRecorded != "" {
			t.Fatalf("started=%v not_recorded=%q, want started recorded with a cast", ok, run.NotRecorded)
		}
		tw.BeginDrain()
		_, _ = tw.Write([]byte("some output\n"))
		tw.EndDrain(nil)
		waitFor(t, "the run to be marked", func() bool { return cs.marked() == types.NotRecordedRunnerDeclined && !cs.owing() })
		var saveErr, declined bool
		for _, row := range castRows(t, f, agentCastSession) {
			e, _ := row["error"].(string)
			saveErr = saveErr || (row["outcome"] == "failure" && strings.Contains(e, "recording store full"))
			declined = declined || (row["outcome"] == "failure" && row["reason"] == "save_failed")
		}
		if !saveErr || !declined {
			t.Fatalf("rows %v, want the save's failure and the save_failed row", castRows(t, f, agentCastSession))
		}
	})
	t.Run("the agent printed nothing", func(t *testing.T) {
		f, cs, _ := agentCastFixture(t, types.MachineMacOSVM)
		_, spec, _ := startDispatch(f, types.SiteConfig{}, false)
		tw := spec.ExecOutput.(*tailWriter)
		tw.BeginDrain()
		tw.EndDrain(nil)
		waitFor(t, "the obligation to clear", func() bool { return !cs.owing() })
		if rows := castRows(t, f, agentCastSession); len(rows) != 0 || cs.marked() != "" || cs.declineCount() != 0 {
			t.Fatalf("rows %v mark %q, want no row and the run recorded", rows, cs.marked())
		}
	})
	t.Run("the copy ends in an error with nothing copied", func(t *testing.T) {
		rr := newRecoveringRunner()
		rr.log = "agent output, re-read\n"
		f, cs, casts := agentCastFixture(t, types.MachineMacOSVM, func(c *Config) { c.Runner = rr })
		_, spec, _ := startDispatch(f, types.SiteConfig{}, false)
		tw := spec.ExecOutput.(*tailWriter)
		tw.BeginDrain()
		tw.EndDrain(errors.New("output ack lost"))
		ended := time.Now()
		f.st.mu.Lock()
		f.st.state, f.st.run.EndedAt = types.RunCompleted, &ended
		f.st.mu.Unlock()
		f.srv.FinishRunOutput(t.Context(), f.run.ID)
		if rows := castRows(t, f, agentCastSession); len(rows) != 0 || !cs.owing() || cs.marked() != "" {
			t.Fatalf("rows %v owed %v mark %q, want no row and the obligation kept for the sweep", rows, cs.owing(), cs.marked())
		}
		dropTail(f)
		if err := f.srv.SweepRunOutputs(t.Context()); err != nil {
			t.Fatal(err)
		}
		if row := waitCastRow(t, f, agentCastRecoveredSession); row["outcome"] != "success" || cs.owing() || cs.marked() != "" {
			t.Fatalf("row %v owed %v mark %q, want the sweep's cast and the run recorded", row, cs.owing(), cs.marked())
		}
		if out := readCast(t, casts, f.run.ID, agentCastRecoveredSession); !strings.Contains(out, "re-read") {
			t.Fatalf("recovered cast %q", out)
		}
	})
	t.Run("the copy ends in an error part way", func(t *testing.T) {
		f, cs, casts := agentCastFixture(t, types.MachineMacOSVM)
		_, spec, _ := startDispatch(f, types.SiteConfig{}, false)
		tw := spec.ExecOutput.(*tailWriter)
		tw.BeginDrain()
		_, _ = tw.Write([]byte("the first half\n"))
		tw.EndDrain(errors.New("output ack lost"))
		row := waitCastRow(t, f, agentCastSession)
		if row["outcome"] != "success" || row["incomplete"] != true || row["read_error"] != "output ack lost" {
			t.Fatalf("row %v, want a saved cast marked incomplete with the copy's error", row)
		}
		if out := readCast(t, casts, f.run.ID, agentCastSession); !strings.Contains(out, "the first half") || !strings.HasSuffix(out, agentCastCutMarker) {
			t.Fatalf("cast %q, want the copied half then the cut marker", out)
		}
		waitFor(t, "the obligation to settle", func() bool { return !cs.owing() && cs.marked() == "" })
	})
}

// A macOS VM run whose sandbox create fails after its cast was owed never ran: dispatch clears the
// obligation as not owed (unoweAgentCast, on both create-failure returns), so the sweep finds nothing,
// and the run gets no runner_declined mark and no failure row.
func TestAgentCast_ACreateFailureOwesNothing(t *testing.T) {
	f, cs, _ := agentCastFixture(t, types.MachineMacOSVM)
	cs.required = true
	run, _, ok := startDispatch(f, types.SiteConfig{}, false)
	if !ok || !cs.owing() {
		t.Fatalf("started=%v owed=%v, want started with a cast owed", ok, cs.owing())
	}
	f.srv.unoweAgentCast(t.Context(), run) // dispatchRun, when the create fails
	f.st.mu.Lock()
	f.st.state = types.RunFailed
	f.st.mu.Unlock()
	if err := f.srv.SweepRunOutputs(t.Context()); err != nil {
		t.Fatal(err)
	}
	if rows := castRows(t, f, agentCastSession); len(rows) != 0 || cs.owing() || cs.marked() != "" || cs.declineCount() != 0 {
		t.Fatalf("rows %v owed %v mark %q, want nothing owed, no mark and no row", rows, cs.owing(), cs.marked())
	}
}

// A decline whose clear fails is retried by the next sweep, which finds the run already marked and only
// clears: one failure row, not two.
func TestAgentCast_ADeclineRetriedAfterAFailedClearWritesOneRow(t *testing.T) {
	f, cs, _ := agentCastFixture(t, types.MachineMacOSVM) // its runner keeps no copy: the sweep declines, no_tail
	cs.owed, cs.clearFails = true, 1
	ended := time.Now()
	f.st.mu.Lock()
	f.st.state, f.st.run.EndedAt = types.RunCompleted, &ended
	f.st.mu.Unlock()
	if err := f.srv.SweepRunOutputs(t.Context()); err == nil {
		t.Fatal("the first sweep hid the failed clear")
	}
	if !cs.owing() || cs.marked() != types.NotRecordedRunnerDeclined {
		t.Fatalf("owed %v mark %q, want the run marked and the obligation kept", cs.owing(), cs.marked())
	}
	if err := f.srv.SweepRunOutputs(t.Context()); err != nil {
		t.Fatal(err)
	}
	rows := castRows(t, f, agentCastSession)
	if len(rows) != 1 || rows[0]["reason"] != "no_tail" || cs.owing() || cs.marked() != types.NotRecordedRunnerDeclined {
		t.Fatalf("rows %v owed %v mark %q, want one no_tail row, the run marked and nothing owed", rows, cs.owing(), cs.marked())
	}
}
