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
	"sync/atomic"
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
	declines   int    // runner_declined marks written
	owed       bool   // agent_cast_owed_at is set
	oweErr     error  // SetRunAgentCastOwed fails with it
	clearFails int    // the next N ClearRunAgentCastOwed calls fail
	onClear    func() // called as ClearRunAgentCastOwed begins, outside the lock
	// others are further runs, like this one, that owe their cast (true while they do), listed after it.
	others map[uuid.UUID]bool
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
	if !c.owed {
		return false, nil
	}
	c.mark = types.NotRecordedRunnerDeclined
	c.declines++
	return true, nil
}

func (c *castStore) ClearRunAgentCastOwed(_ context.Context, id uuid.UUID, recorded bool) error {
	if c.onClear != nil {
		c.onClear()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.others[id]; ok {
		c.others[id] = false
		return nil
	}
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
	var ids []uuid.UUID
	if c.owed && !slices.Contains(types.NonTerminalRunStates, state) {
		ids = append(ids, c.st.run.ID)
	}
	for id, owed := range c.others {
		if owed {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// otherOwing is whether the further run id still owes its cast.
func (c *castStore) otherOwing(id uuid.UUID) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.others[id]
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
	c.mu.Lock()
	_, other := c.others[id]
	c.mu.Unlock()
	if other {
		run.ID = id // a further run, as this one is
		return run, err
	}
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
		assertNoCastDecision(t, f, cs, run, ok, required, "save_failed")
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
				assertNoCastDecision(t, f, cs, run, ok, sc.required, "no_tail")
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
			assertNoCastDecision(t, f, cs, run, ok, sc.required, "no_tail")
		})
	}
}

// assertNoCastDecision also checks the failure row a decline leaves in Audit: one session.recording.write
// failure row for the run's agent cast naming wantReason when the run started (a run that failed at start
// has its run.create failure row instead and no cast row).
func assertNoCastDecision(t *testing.T, f *outputFixture, cs *castStore, run types.AgentRun, started, required bool, wantReason string) {
	t.Helper()
	defer func() {
		rows := f.audit.eventsFor(f.run.ID, "session.recording.write")
		if required {
			if len(rows) != 0 {
				t.Errorf("a run that failed at start wrote %d agent cast rows, want none", len(rows))
			}
			return
		}
		var d map[string]any
		if len(rows) == 1 {
			_ = json.Unmarshal(rows[0].Data, &d)
		}
		if len(rows) != 1 || rows[0].Outcome != "failure" || rows[0].Target != recording.CastKey(f.run.ID.String(), agentCastSession) || d["reason"] != wantReason {
			t.Errorf("decline rows = %+v, want one failure row for the agent cast with reason %q", rows, wantReason)
		}
	}()
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
		if row["outcome"] != "success" || row["incomplete"] != true || row["read_error"] != true {
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

// rejectingAudit is an audit recorder that accepts nothing, durably or into its spool, and counts what it
// refused.
type rejectingAudit struct{ refused atomic.Int32 }

func (a *rejectingAudit) Record(context.Context, types.AuditEvent) error {
	a.refused.Add(1)
	return errors.New("audit store down and spool full")
}

// A decline at dispatch whose failure row the audit recorder rejects (durably and into its spool) is never
// silently lost: the run is stopped with its own sentence and a run.create failure row, and no run is started
// marked and unexplained. With the recorder accepting, the same run starts and the row is there.
func TestAgentCast_ADispatchDeclineWhoseRowIsRejectedStopsTheRun(t *testing.T) {
	f, _, _ := agentCastFixture(t, types.MachineMacOSVM, func(c *Config) { c.ExecOutputTailOff = true })
	rejecting := &rejectingAudit{}
	f.srv.cfg.Audit = rejecting
	run, _, ok := startDispatch(f, types.SiteConfig{}, false)
	if ok || f.st.FailureHint() != macOSAgentDeclineUnrecordedMsg || f.st.State() != types.RunFailed {
		t.Fatalf("started=%v hint=%q state=%s, want the run stopped with %q", ok, f.st.FailureHint(), f.st.State(), macOSAgentDeclineUnrecordedMsg)
	}
	if rejecting.refused.Load() == 0 || run.NotRecorded != types.NotRecordedRunnerDeclined {
		t.Fatalf("refused rows %d, not_recorded %q, want the row attempted for a declined run", rejecting.refused.Load(), run.NotRecorded)
	}

	f2, cs2, _ := agentCastFixture(t, types.MachineMacOSVM, func(c *Config) { c.ExecOutputTailOff = true })
	run2, _, ok2 := startDispatch(f2, types.SiteConfig{}, false)
	if !ok2 || f2.st.State() == types.RunFailed || cs2.marked() != types.NotRecordedRunnerDeclined {
		t.Fatalf("accepting recorder: started=%v state=%s mark=%q, want the run started with the mark stored", ok2, f2.st.State(), cs2.marked())
	}
	if rows := castRows(t, f2, agentCastSession); len(rows) != 1 || rows[0]["outcome"] != "failure" || rows[0]["reason"] != "no_tail" {
		t.Fatalf("rows %v, want one no_tail failure row for %s", rows, run2.ID)
	}
}

// agentCastDeclineFixture is a macOS VM task run that has ended and still owes its agent cast, on a
// runner that keeps no copy of it: the sweep can only decline it, no_tail.
func agentCastDeclineFixture(t *testing.T) (*outputFixture, *castStore) {
	t.Helper()
	f, cs, _ := agentCastFixture(t, types.MachineMacOSVM)
	cs.owed = true
	ended := time.Now()
	f.st.mu.Lock()
	f.st.state, f.st.run.EndedAt = types.RunCompleted, &ended
	f.st.mu.Unlock()
	return f, cs
}

// A decline whose clear fails is retried by the next sweep, which writes the row again before it clears:
// a second row is the price of never losing one.
func TestAgentCast_ADeclineRetriedAfterAFailedClearIsSettled(t *testing.T) {
	f, cs := agentCastDeclineFixture(t)
	cs.clearFails = 1
	if err := f.srv.SweepRunOutputs(t.Context()); err == nil {
		t.Fatal("the first sweep hid the failed clear")
	}
	if rows := castRows(t, f, agentCastSession); len(rows) != 1 || !cs.owing() || cs.marked() != types.NotRecordedRunnerDeclined {
		t.Fatalf("rows %v owed %v mark %q, want one row, the run marked and the obligation kept", rows, cs.owing(), cs.marked())
	}
	if err := f.srv.SweepRunOutputs(t.Context()); err != nil {
		t.Fatal(err)
	}
	rows := castRows(t, f, agentCastSession)
	if len(rows) != 2 || rows[1]["reason"] != "no_tail" || cs.owing() || cs.marked() != types.NotRecordedRunnerDeclined {
		t.Fatalf("rows %v owed %v mark %q, want the row again, the run marked and nothing owed", rows, cs.owing(), cs.marked())
	}
}

// A process that died between the runner_declined mark and its failure row: after the restart the run is
// marked and still owes its cast, with no row. The sweep writes the row from that state, then clears the
// obligation.
func TestAgentCast_AMarkWithNoRowGetsItsRowBeforeTheClear(t *testing.T) {
	f, cs := agentCastDeclineFixture(t)
	cs.mark = types.NotRecordedRunnerDeclined // committed before the crash; the row never was
	if err := f.srv.SweepRunOutputs(t.Context()); err != nil {
		t.Fatal(err)
	}
	rows := castRows(t, f, agentCastSession)
	if len(rows) != 1 || rows[0]["outcome"] != "failure" || rows[0]["reason"] != "no_tail" {
		t.Fatalf("rows %v, want the failure row written by the sweep", rows)
	}
	if cs.owing() || cs.marked() != types.NotRecordedRunnerDeclined {
		t.Fatalf("owed %v mark %q, want the obligation cleared after the row and the run marked", cs.owing(), cs.marked())
	}
}

// A failure row the audit recorder rejects keeps the obligation: the run is marked, nothing is cleared,
// and the sweep says so. Once the recorder accepts again, the next sweep writes the row and clears it.
func TestAgentCast_ARejectedRowKeepsTheObligation(t *testing.T) {
	f, cs := agentCastDeclineFixture(t)
	f.srv.cfg.Audit = &rejectingAudit{}
	if err := f.srv.SweepRunOutputs(t.Context()); !errors.Is(err, errAgentCastRowRejected) {
		t.Fatalf("sweep: %v, want the rejected row", err)
	}
	if !cs.owing() || cs.marked() != types.NotRecordedRunnerDeclined {
		t.Fatalf("owed %v mark %q, want the run marked and the obligation kept", cs.owing(), cs.marked())
	}
	f.srv.cfg.Audit = f.audit
	if err := f.srv.SweepRunOutputs(t.Context()); err != nil {
		t.Fatal(err)
	}
	if rows := castRows(t, f, agentCastSession); len(rows) != 1 || rows[0]["reason"] != "no_tail" || cs.owing() {
		t.Fatalf("rows %v owed %v, want the row written, then the obligation cleared", rows, cs.owing())
	}
}

// castGuard swaps the agent cast's masking guard, as a manifest check that starts failing after the cast
// opened does.
func castGuard(c *agentCast, guard func() bool) {
	c.mw.mu.Lock()
	c.mw.guard = guard
	c.mw.mu.Unlock()
}

// The masker drops every byte of a copy that ends cleanly (its guard failed after the cast opened): that
// is not silence. The run is declined, mask_gap, with its failure row, and no cast is stored.
func TestAgentCast_AllBytesDroppedByTheMaskerIsAMaskGap(t *testing.T) {
	f, cs, casts := agentCastFixture(t, types.MachineMacOSVM)
	_, spec, ok := startDispatch(f, types.SiteConfig{}, false)
	tw, _ := spec.ExecOutput.(*tailWriter)
	if !ok || tw == nil || tw.cast == nil {
		t.Fatalf("started=%v, want started with a cast", ok)
	}
	castGuard(tw.cast, func() bool { return false })
	tw.BeginDrain()
	_, _ = tw.Write([]byte("output the masker cannot vouch for\n"))
	tw.EndDrain(nil)
	row := waitCastRow(t, f, agentCastSession)
	if row["outcome"] != "failure" || row["reason"] != "mask_gap" {
		t.Fatalf("row %v, want a failure row with reason mask_gap", row)
	}
	waitFor(t, "the run to be declined", func() bool { return cs.marked() == types.NotRecordedRunnerDeclined && !cs.owing() })
	if _, err := casts.OpenCast(context.Background(), recording.CastKey(f.run.ID.String(), agentCastSession)); err == nil {
		t.Fatal("a cast was stored for a copy the masker dropped whole")
	}
}

// The masker drops only a middle chunk of a copy that ends cleanly: the cast is saved with what was
// kept, and its row says incomplete and mask_gap. The run stays recorded.
func TestAgentCast_AMiddleChunkDroppedByTheMaskerIsSavedIncomplete(t *testing.T) {
	f, cs, casts := agentCastFixture(t, types.MachineMacOSVM)
	_, spec, ok := startDispatch(f, types.SiteConfig{}, false)
	tw, _ := spec.ExecOutput.(*tailWriter)
	if !ok || tw == nil || tw.cast == nil {
		t.Fatalf("started=%v, want started with a cast", ok)
	}
	var failing atomic.Bool
	castGuard(tw.cast, func() bool { return !failing.Load() })
	tw.BeginDrain()
	for _, chunk := range []struct {
		text string
		fail bool
	}{{"the first chunk\n", false}, {"the middle chunk\n", true}, {"the last chunk\n", false}} {
		failing.Store(chunk.fail)
		_, _ = tw.Write([]byte(chunk.text))
		tw.t.in.flush()
		tw.cast.pipe.flush()
	}
	tw.EndDrain(nil)
	row := waitCastRow(t, f, agentCastSession)
	if row["outcome"] != "success" || row["incomplete"] != true || row["mask_gap"] != true {
		t.Fatalf("row %v, want a saved cast marked incomplete and mask_gap", row)
	}
	out := readCast(t, casts, f.run.ID, agentCastSession)
	if !strings.Contains(out, "the first chunk") || !strings.Contains(out, "the last chunk") || strings.Contains(out, "the middle chunk") {
		t.Fatalf("cast %q, want the first and last chunks without the middle one", out)
	}
	waitFor(t, "the obligation to settle", func() bool { return !cs.owing() && cs.marked() == "" })
}

// A process saved the agent cast and went before its session.recording.write row: the run still owes the
// cast and the run page cannot find it. The sweep writes the success row naming the stored cast, then
// clears the obligation with the run recorded.
func TestAgentCast_AStoredCastWithNoRowGetsItsRowBeforeTheClear(t *testing.T) {
	f, cs := agentCastDeclineFixture(t)
	key := recording.CastKey(f.run.ID.String(), agentCastSession)
	cast := "{\"version\": 2}\n[0.1, \"o\", \"agent output\\n\"]\n"
	if err := f.srv.cfg.RecordingStore.SaveCastNamed(t.Context(), f.run.ID.String(), agentCastSession, strings.NewReader(cast)); err != nil {
		t.Fatal(err)
	}
	rowsAtClear := -1
	cs.onClear = func() { rowsAtClear = len(castRows(t, f, agentCastSession)) }
	if err := f.srv.SweepRunOutputs(t.Context()); err != nil {
		t.Fatal(err)
	}
	rows := castRows(t, f, agentCastSession)
	if len(rows) != 1 || rows[0]["outcome"] != "success" || rows[0]["target"] != key || rows[0]["key"] != key || rows[0]["swept"] != true {
		t.Fatalf("rows %v, want one success row naming %s", rows, key)
	}
	if rowsAtClear != 1 || cs.owing() || cs.marked() != "" {
		t.Fatalf("rows at the clear %d, owed %v, mark %q: want the row written before the obligation cleared, the run recorded",
			rowsAtClear, cs.owing(), cs.marked())
	}
}

// A save that succeeds while the audit recorder refuses its row keeps the obligation: the cast is
// stored but has no row yet. The next sweep, with the recorder back, writes the row and clears it.
func TestAgentCast_ASavedCastWhoseRowIsRejectedKeepsTheObligation(t *testing.T) {
	f, cs, casts := agentCastFixture(t, types.MachineMacOSVM)
	_, spec, ok := startDispatch(f, types.SiteConfig{}, false)
	tw, _ := spec.ExecOutput.(*tailWriter)
	if !ok || tw == nil || tw.cast == nil {
		t.Fatalf("started=%v, want started with a cast", ok)
	}
	rej := &rejectingAudit{}
	f.srv.cfg.Audit = rej
	tw.BeginDrain()
	_, _ = tw.Write([]byte("agent output\n"))
	tw.EndDrain(nil)
	key := recording.CastKey(f.run.ID.String(), agentCastSession)
	waitFor(t, "the save and its refused row", func() bool {
		_, _, err := casts.StatAndTail(context.Background(), key, 0)
		return err == nil && rej.refused.Load() > 0
	})
	if !cs.owing() || cs.marked() != "" {
		t.Fatalf("owed %v mark %q, want the obligation kept with the run recorded", cs.owing(), cs.marked())
	}

	f.srv.cfg.Audit = f.audit
	ended := time.Now()
	f.st.mu.Lock()
	f.st.state, f.st.run.EndedAt = types.RunCompleted, &ended
	f.st.mu.Unlock()
	dropTail(f)
	if err := f.srv.SweepRunOutputs(t.Context()); err != nil {
		t.Fatal(err)
	}
	rows := castRows(t, f, agentCastSession)
	if len(rows) != 1 || rows[0]["outcome"] != "success" || rows[0]["key"] != key || cs.owing() || cs.marked() != "" {
		t.Fatalf("rows %v owed %v mark %q, want the sweep's success row and the obligation cleared", rows, cs.owing(), cs.marked())
	}
}

// A cast saved with a middle chunk the masker dropped, whose row the audit recorder refuses: the sweep
// that writes the row from the stored cast still says incomplete and mask_gap, from the completeness the
// save stamped in the cast's header. The swept row never reads as a clean recording.
func TestAgentCast_ASweptRowKeepsTheSavedCastsWarning(t *testing.T) {
	f, cs, casts := agentCastFixture(t, types.MachineMacOSVM)
	_, spec, ok := startDispatch(f, types.SiteConfig{}, false)
	tw, _ := spec.ExecOutput.(*tailWriter)
	if !ok || tw == nil || tw.cast == nil {
		t.Fatalf("started=%v, want started with a cast", ok)
	}
	var failing atomic.Bool
	castGuard(tw.cast, func() bool { return !failing.Load() })
	rej := &rejectingAudit{}
	f.srv.cfg.Audit = rej
	tw.BeginDrain()
	for _, fail := range []bool{false, true, false} {
		failing.Store(fail)
		_, _ = tw.Write([]byte("a chunk\n"))
		tw.cast.pipe.flush()
	}
	tw.EndDrain(nil)
	key := recording.CastKey(f.run.ID.String(), agentCastSession)
	waitFor(t, "the save and its refused row", func() bool {
		_, _, err := casts.StatAndTail(context.Background(), key, 0)
		return err == nil && rej.refused.Load() > 0
	})

	f.srv.cfg.Audit = f.audit
	ended := time.Now()
	f.st.mu.Lock()
	f.st.state, f.st.run.EndedAt = types.RunCompleted, &ended
	f.st.mu.Unlock()
	dropTail(f)
	if err := f.srv.SweepRunOutputs(t.Context()); err != nil {
		t.Fatal(err)
	}
	rows := castRows(t, f, agentCastSession)
	if len(rows) != 1 || rows[0]["outcome"] != "success" || rows[0]["swept"] != true || rows[0]["incomplete"] != true || rows[0]["mask_gap"] != true {
		t.Fatalf("rows %v, want the swept row still incomplete with mask_gap", rows)
	}
	if cs.owing() {
		t.Fatal("the obligation was not cleared after the swept row")
	}
}

// A stored cast whose header cannot be read gets a swept row that says incomplete, completeness
// unknown: completeness is never taken as known on no evidence.
func TestAgentCast_ASweptRowWithAnUnreadableHeaderIsUnknown(t *testing.T) {
	f, cs := agentCastDeclineFixture(t)
	if err := f.srv.cfg.RecordingStore.SaveCastNamed(t.Context(), f.run.ID.String(), agentCastSession, strings.NewReader("not a header\n")); err != nil {
		t.Fatal(err)
	}
	if err := f.srv.SweepRunOutputs(t.Context()); err != nil {
		t.Fatal(err)
	}
	rows := castRows(t, f, agentCastSession)
	if len(rows) != 1 || rows[0]["outcome"] != "success" || rows[0]["incomplete"] != true || rows[0]["completeness"] != "unknown" || cs.owing() {
		t.Fatalf("rows %v owed %v, want one row saying incomplete, completeness unknown, and nothing owed", rows, cs.owing())
	}
}

// A copy whose masker dropped every byte and that ended in an error goes to the sweep's re-read, not to
// mask_gap: the obligation stays, and the re-read, masked afresh, stores the cast with the run recorded.
func TestAgentCast_AFailedCopyTheMaskerDroppedIsReRead(t *testing.T) {
	rr := newRecoveringRunner()
	rr.log = "agent output, re-read\n"
	f, cs, casts := agentCastFixture(t, types.MachineMacOSVM, func(c *Config) { c.Runner = rr })
	_, spec, ok := startDispatch(f, types.SiteConfig{}, false)
	tw, _ := spec.ExecOutput.(*tailWriter)
	if !ok || tw == nil || tw.cast == nil {
		t.Fatalf("started=%v, want started with a cast", ok)
	}
	castGuard(tw.cast, func() bool { return false })
	tw.BeginDrain()
	_, _ = tw.Write([]byte("output the masker cannot vouch for\n"))
	tw.EndDrain(errors.New("output ack lost"))
	ended := time.Now()
	f.st.mu.Lock()
	f.st.state, f.st.run.EndedAt = types.RunCompleted, &ended
	f.st.mu.Unlock()
	f.srv.FinishRunOutput(t.Context(), f.run.ID)
	if rows := castRows(t, f, agentCastSession); len(rows) != 0 || !cs.owing() || cs.marked() != "" {
		t.Fatalf("rows %v owed %v mark %q, want no mask_gap decline and the obligation kept", rows, cs.owing(), cs.marked())
	}
	dropTail(f)
	if err := f.srv.SweepRunOutputs(t.Context()); err != nil {
		t.Fatal(err)
	}
	if row := waitCastRow(t, f, agentCastRecoveredSession); row["outcome"] != "success" || row["mask_gap"] != nil || cs.owing() || cs.marked() != "" {
		t.Fatalf("row %v owed %v mark %q, want the re-read stored and the run recorded", row, cs.owing(), cs.marked())
	}
	if out := readCast(t, casts, f.run.ID, agentCastRecoveredSession); !strings.Contains(out, "re-read") {
		t.Fatalf("recovered cast %q", out)
	}
}

// A run whose recordings were erased (the recording store's fence) while it still owes its agent cast:
// the sweep clears the obligation with no re-read, no cast and no row, and the fence stays. A run owing
// its cast behind it is still swept.
func TestAgentCast_AnErasedRunsObligationIsClearedAndTheNextIsSwept(t *testing.T) {
	rr := newRecoveringRunner()
	rr.log = "never read\n"
	f, cs, casts := agentCastFixture(t, types.MachineMacOSVM, func(c *Config) { c.Runner = rr })
	cs.owed = true
	ended := time.Now()
	f.st.mu.Lock()
	f.st.state, f.st.run.EndedAt = types.RunCompleted, &ended
	f.st.mu.Unlock()
	next := uuid.New()
	cs.others = map[uuid.UUID]bool{next: true}
	cast := "{\"version\": 2, \"width\": 80, \"height\": 24}\n[0.1, \"o\", \"agent output\\n\"]\n"
	if err := casts.SaveCastNamed(t.Context(), next.String(), agentCastSession, strings.NewReader(cast)); err != nil {
		t.Fatal(err)
	}
	if _, err := casts.(recording.RunDeleter).DeleteRun(t.Context(), f.run.ID.String()); err != nil {
		t.Fatal(err)
	}

	if err := f.srv.SweepRunOutputs(t.Context()); err != nil {
		t.Fatal(err)
	}
	rr.rmu.Lock()
	reads := rr.recovers
	rr.rmu.Unlock()
	rows := len(castRows(t, f, agentCastSession)) + len(castRows(t, f, agentCastRecoveredSession))
	if cs.owing() || cs.marked() != "" || reads != 0 || rows != 0 {
		t.Fatalf("owed %v mark %q re-reads %d rows %d, want the obligation cleared with no re-read, mark or row",
			cs.owing(), cs.marked(), reads, rows)
	}
	if _, _, err := casts.StatAndTail(t.Context(), recording.CastKey(f.run.ID.String(), agentCastRecoveredSession), 0); !errors.Is(err, recording.ErrErased) {
		t.Fatalf("the erased run's recovered cast: %v, want the fence kept", err)
	}
	if cs.otherOwing(next) || !f.audit.has(next, "session.recording.write", "success") {
		t.Fatalf("the next run: owed %v, want swept with its cast's row", cs.otherOwing(next))
	}
}
