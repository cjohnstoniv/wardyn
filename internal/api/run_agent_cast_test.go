// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

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

	mu       sync.Mutex
	required bool
	mark     types.RunNotRecorded
	declines int // SetRunNotRecorded calls with runner_declined
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
			if !ok || run.NotRecorded != "" || cs.declineCount() != 0 || f.st.FailureHint() != "" {
				t.Fatalf("started=%v not_recorded=%q declines=%d hint=%q, want started recorded", ok, run.NotRecorded, cs.declineCount(), f.st.FailureHint())
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
			// The pessimistic mark went down before the drain was released, and the save cleared it.
			if cs.marked() != "" || cs.declineCount() != 1 {
				t.Fatalf("not_recorded = %q after %d marks, want marked once and cleared", cs.marked(), cs.declineCount())
			}
		})
	}
}

// blockingCasts is a recording store whose saves wait until release closes, then fail: a process that
// never finishes its save.
type blockingCasts struct {
	recording.Store
	release chan struct{}
}

func (b blockingCasts) SaveCastNamed(context.Context, string, string, io.Reader) error {
	<-b.release
	return errors.New("the process went before its save")
}

// A process that commits the run's output row while the agent cast's save is still pending, then dies:
// no recovery runs for a run with a final row, so the run is left marked runner_declined (written before
// the drain was released), never marked recorded with no cast.
func TestAgentCast_ADeathMidSaveLeavesTheRunMarked(t *testing.T) {
	f, cs, casts := agentCastFixture(t, types.MachineMacOSVM)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	f.srv.cfg.RecordingStore = blockingCasts{Store: casts, release: release}
	_, spec, ok := startDispatch(f, types.SiteConfig{}, false)
	tw, _ := spec.ExecOutput.(*tailWriter)
	if !ok || tw == nil || tw.cast == nil {
		t.Fatalf("started=%v, want started with a cast", ok)
	}
	tw.BeginDrain()
	_, _ = tw.Write([]byte("agent output\n"))
	tw.EndDrain(nil)
	waitFor(t, "the pessimistic mark", func() bool { return cs.marked() == types.NotRecordedRunnerDeclined })
	f.st.mu.Lock()
	f.st.state = types.RunRunning
	f.st.mu.Unlock()
	f.srv.FinishRunOutput(t.Context(), f.run.ID) // the drain is released while the save waits
	f.mem.mu.Lock()
	final := f.mem.rows[f.run.ID].CapturedAt != nil
	f.mem.mu.Unlock()
	if !final {
		t.Fatal("the output row was not committed while the save was pending")
	}

	dropTail(f) // the process goes; its cast and save with it
	run, _ := f.srv.cfg.Store.GetRun(t.Context(), f.run.ID)
	f.srv.recoverRunOutput(t.Context(), f.srv.runOutputStore(), run, "stale_pending")
	if cs.marked() != types.NotRecordedRunnerDeclined || len(castRows(t, f, agentCastSession)) != 0 || len(castRows(t, f, agentCastRecoveredSession)) != 0 {
		t.Fatalf("mark %q, agent rows %v, recovered rows %v: want the run left marked with no cast",
			cs.marked(), castRows(t, f, agentCastSession), castRows(t, f, agentCastRecoveredSession))
	}
	for _, key := range []string{agentCastSession, agentCastRecoveredSession} {
		if _, err := casts.OpenCast(context.Background(), recording.CastKey(f.run.ID.String(), key)); err == nil {
			t.Fatalf("a %s cast was stored", key)
		}
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
		row := waitCastRow(t, f, agentCastSession)
		if row["outcome"] != "failure" || !strings.Contains(row["error"].(string), "recording store full") {
			t.Fatalf("row %v, want the save's failure", row)
		}
		waitFor(t, "the run to be marked", func() bool { return cs.marked() == types.NotRecordedRunnerDeclined })
	})
	t.Run("the agent printed nothing", func(t *testing.T) {
		f, cs, _ := agentCastFixture(t, types.MachineMacOSVM)
		_, spec, _ := startDispatch(f, types.SiteConfig{}, false)
		tw := spec.ExecOutput.(*tailWriter)
		tw.BeginDrain()
		tw.EndDrain(nil)
		if save := tw.cast.seal(); save != nil { // seals once, or waits for the drain's own seal
			t.Fatal("a cast with no output has a save")
		}
		if rows := castRows(t, f, agentCastSession); len(rows) != 0 || cs.marked() != "" || cs.declineCount() != 0 {
			t.Fatalf("rows %v mark %q, want no row and the run recorded", rows, cs.marked())
		}
	})
}
