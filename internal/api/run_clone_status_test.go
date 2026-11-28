// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

var _ store.RunCloneStatusStore = (*cloneStatusStore)(nil)

// cloneStatusStore is the clone-status surface over maps, with Postgres's replace-once rule. The embedded Store is
// nil on purpose: a pass that called any other store method would panic and fail its test. A run is "recorded"
// when it has a policy: the audit read answers its run.policy.resolve row.
type cloneStatusStore struct {
	store.Store
	mu       sync.Mutex
	runs     map[uuid.UUID]types.AgentRun
	policies map[uuid.UUID][]types.WorkspaceRepo
	rows     map[uuid.UUID]store.RunCloneStatus
	finals   int // writes that stored a final state
}

func (s *cloneStatusStore) QueryAuditEvents(_ context.Context, runID uuid.UUID, _ int) ([]types.AuditEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	repos, ok := s.policies[runID]
	if !ok {
		return nil, nil
	}
	data := mustJSON(effectivePolicyDatum{RunPolicySpec: types.RunPolicySpec{WorkspaceRepos: repos}})
	return []types.AuditEvent{{ID: uuid.New(), Time: time.Now().UTC(), RunID: &runID, Action: "run.policy.resolve", Outcome: "success", Data: data}}, nil
}

func (s *cloneStatusStore) PutRunCloneStatus(_ context.Context, st store.RunCloneStatus) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cur, ok := s.rows[st.RunID]; ok && !(cur.State == store.CloneStateCloning && st.State != store.CloneStateCloning) {
		return false, nil
	}
	s.rows[st.RunID] = st
	if st.State != store.CloneStateCloning {
		s.finals++
	}
	return true, nil
}

func (s *cloneStatusStore) GetRunCloneStatus(_ context.Context, id uuid.UUID) (store.RunCloneStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.rows[id]; ok {
		return r, nil
	}
	return store.RunCloneStatus{}, store.ErrNotFound
}

func (s *cloneStatusStore) DeleteRunCloneStatus(_ context.Context, id uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.rows, id)
	return nil
}

func (s *cloneStatusStore) ListCloneStatusRuns(context.Context) ([]types.AgentRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []types.AgentRun
	for id, r := range s.runs {
		if row, ok := s.rows[id]; ok && row.State != store.CloneStateCloning {
			continue
		}
		out = append(out, r)
	}
	slices.SortFunc(out, func(a, b types.AgentRun) int { return a.CreatedAt.Compare(b.CreatedAt) })
	return out, nil
}

func (s *cloneStatusStore) row(id uuid.UUID) (store.RunCloneStatus, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.rows[id]
	return r, ok
}

func (s *cloneStatusStore) finalCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.finals
}

type cloneFixture struct {
	srv *Server
	st  *cloneStatusStore
	rn  *serviceStateRunner
	now time.Time
}

const cloneFixtureDest = "/home/agent/work/"

var cloneAskedRepos = []types.WorkspaceRepo{{Repo: "acme/alpha"}, {Repo: "acme/beta"}}

func newCloneFixture(t *testing.T, answer func(ref string) (*runner.ExecSession, error)) *cloneFixture {
	t.Helper()
	f := &cloneFixture{now: time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)}
	f.st = &cloneStatusStore{runs: map[uuid.UUID]types.AgentRun{}, policies: map[uuid.UUID][]types.WorkspaceRepo{}, rows: map[uuid.UUID]store.RunCloneStatus{}}
	f.rn = &serviceStateRunner{fakeRunner: &fakeRunner{}, answer: answer}
	h := newHarness(t)
	cfg := baseTestConfig(h, f.st)
	cfg.Runner = f.rn
	cfg.Now = func() time.Time { return f.now }
	f.srv = New(cfg)
	return f
}

// addRun adds a RUNNING run admitted age ago in sandbox ref, asking for repos (nil: none) with its policy recorded.
func (f *cloneFixture) addRun(ref string, age time.Duration, repos []types.WorkspaceRepo) types.AgentRun {
	run := types.AgentRun{ID: uuid.New(), State: types.RunRunning, SandboxRef: ref, CreatedAt: f.now.Add(-age)}
	f.st.runs[run.ID] = run
	f.st.policies[run.ID] = repos
	return run
}

func (f *cloneFixture) sweep(t *testing.T) {
	t.Helper()
	if err := f.srv.sweepCloneStatus(context.Background()); err != nil {
		t.Fatalf("sweepCloneStatus: %v", err)
	}
}

func (f *cloneFixture) sweeps(t *testing.T, n int) {
	t.Helper()
	for range n {
		f.now = f.now.Add(time.Second)
		f.sweep(t)
	}
}

func cloneFile(done bool, entries ...string) string {
	if entries == nil {
		entries = []string{}
	}
	return fmt.Sprintf(`{"v":1,"done":%v,"repos":[%s]}`+"\n", done, strings.Join(entries, ","))
}

func cloneEntry(dest, outcome, cause string) string {
	return fmt.Sprintf(`{"dest":%q,"outcome":%q,"cause":%q}`, dest, outcome, cause)
}

const cloneMarker = `{"v":1,"done":false,"repos":[]}` + "\n"

// switchable is an answer whose file the test changes between passes.
type switchable struct {
	mu   sync.Mutex
	text string
	code int
}

func (s *switchable) set(text string, code int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.text, s.code = text, code
}

func (s *switchable) answer(string) (*runner.ExecSession, error) {
	s.mu.Lock()
	text, code := s.text, s.code
	s.mu.Unlock()
	return textPane(text, code)(runner.ExecSpec{})
}

func repoOutcomes(t *testing.T, row store.RunCloneStatus) []string {
	t.Helper()
	var repos []runRepoClone
	if err := json.Unmarshal(row.Repos, &repos); err != nil {
		t.Fatalf("repos %s: %v", row.Repos, err)
	}
	var out []string
	for _, r := range repos {
		out = append(out, r.Dest+"="+r.Outcome)
	}
	return out
}

func TestDecodeCloneState_Strict(t *testing.T) {
	ok := cloneEntry("/home/agent/work/a", "ok", "")
	for name, in := range map[string]string{
		"unknown key":             `{"v":1,"done":true,"repos":[],"extra":1}`,
		"unknown entry key":       `{"v":1,"done":true,"repos":[{"dest":"/a","outcome":"ok","cause":"","x":1}]}`,
		"version 2":               `{"v":2,"done":true,"repos":[]}`,
		"no done":                 `{"v":1,"repos":[]}`,
		"marker with entry":       cloneFile(false, ok),
		"bad outcome":             cloneFile(true, cloneEntry("/a", "great", "")),
		"not_reported":            cloneFile(true, cloneEntry("/a", "not_reported", "")),
		"bad cause":               cloneFile(true, cloneEntry("/a", "failed", "because")),
		"failed no cause":         cloneFile(true, cloneEntry("/a", "failed", "")),
		"skipped no cause":        cloneFile(true, cloneEntry("/a", "skipped", "")),
		"skipped, a failed cause": cloneFile(true, cloneEntry("/a", "skipped", "clone_failed")),
		"failed, a skipped cause": cloneFile(true, cloneEntry("/a", "failed", "cannot_create")),
		"ok with a cause":         cloneFile(true, cloneEntry("/a", "ok", "ref_failed")),
		"two objects":             cloneFile(true) + cloneFile(true),
		"trailing junk":           cloneFile(true) + "x",
		"garbage":                 `not json`,
		"control in dest":         cloneFile(true, cloneEntry("/a\tb", "ok", "")),
		"dest too long":           cloneFile(true, cloneEntry("/"+strings.Repeat("d", 512), "ok", "")),
		"same dest twice":         cloneFile(true, cloneEntry("/a", "ok", ""), cloneEntry("/a", "failed", "clone_failed")),
		"65 repositories":         cloneFile(true, manyCloneEntries(65, 10)...),
		"done is text":            `{"v":1,"done":"yes","repos":[]}`,
	} {
		if got, err := decodeCloneState([]byte(in)); err == nil {
			t.Errorf("%s: decoded %+v, want an error", name, got)
		}
	}
	for name, in := range map[string]string{
		"marker":       cloneMarker,
		"every kind":   cloneFile(true, cloneEntry("/a", "ok", ""), cloneEntry("/b", "present", ""), cloneEntry("/c", "skipped", "not_a_checkout"), cloneEntry("/d", "skipped", "cannot_create"), cloneEntry("/e", "failed", "clone_failed"), cloneEntry("/f", "failed", "ref_failed")),
		"no repos":     cloneFile(true),
		"a 512 dest":   cloneFile(true, cloneEntry("/"+strings.Repeat("d", 511), "ok", "")),
		"64 repos max": cloneFile(true, manyCloneEntries(64, 512)...),
	} {
		if _, err := decodeCloneState([]byte(in)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	if raw := cloneFile(true, manyCloneEntries(64, 512)...); len(raw) > cloneStateMaxBytes {
		t.Errorf("64 entries of 512-byte dests are %d bytes, over cloneStateMaxBytes %d", len(raw), cloneStateMaxBytes)
	}
}

// manyCloneEntries is n entries with distinct dests of size bytes each.
func manyCloneEntries(n, size int) []string {
	out := make([]string, n)
	for i := range out {
		d := fmt.Sprintf("/%0*d", size-1, i)
		out[i] = cloneEntry(d, "ok", "")
	}
	return out
}

// A read that outlives its bound is unreachable, and its exec is torn down.
func TestReadCloneState_SlowReaderTimesOut(t *testing.T) {
	pr, pw := io.Pipe()
	closed := make(chan struct{})
	f := newCloneFixture(t, func(string) (*runner.ExecSession, error) {
		return &runner.ExecSession{
			Stdout: pr, Stderr: strings.NewReader(""),
			Wait:  func() (int, error) { return 0, nil },
			Close: func() error { _ = pw.Close(); close(closed); return nil },
		}, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, probe := f.srv.readCloneState(ctx, "ref")
	if probe != cloneProbeUnreachable || time.Since(start) > cloneProbeTimeout {
		t.Fatalf("probe = %v after %v, want unreachable within the bound", probe, time.Since(start))
	}
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Error("the exec was not closed")
	}
}

func TestSweepCloneStatus_DropsUnaskedDest(t *testing.T) {
	file := cloneFile(true, cloneEntry(cloneFixtureDest+"alpha", "ok", ""), cloneEntry(cloneFixtureDest+"evil", "failed", "clone_failed"))
	f := newCloneFixture(t, answerWith(file, 0))
	run := f.addRun("ref", time.Minute, cloneAskedRepos)
	f.sweep(t)
	row, ok := f.st.row(run.ID)
	if !ok || row.State != store.CloneStateReported {
		t.Fatalf("row %+v (%v), want reported", row, ok)
	}
	if got, want := repoOutcomes(t, row), []string{cloneFixtureDest + "alpha=ok", cloneFixtureDest + "beta=not_reported"}; !slices.Equal(got, want) {
		t.Errorf("repos %v, want %v: the unasked dest is dropped and the silent one is not reported", got, want)
	}
	var repos []runRepoClone
	_ = json.Unmarshal(row.Repos, &repos)
	if repos[0].Repo != "acme/alpha" {
		t.Errorf("repo %q, want the slug the run asked for", repos[0].Repo)
	}
}

func TestSweepCloneStatus_NoReposNoExec(t *testing.T) {
	f := newCloneFixture(t, func(string) (*runner.ExecSession, error) {
		t.Error("the sandbox was read for a run that clones nothing")
		return nil, io.EOF
	})
	run := f.addRun("ref", time.Minute, nil)
	f.sweep(t)
	if row, ok := f.st.row(run.ID); !ok || row.State != store.CloneStateNone || string(row.Repos) != "[]" {
		t.Errorf("row %+v (%v), want none with no repos", row, ok)
	}
}

func TestSweepCloneStatus_PolicyNotRecordedWaits(t *testing.T) {
	f := newCloneFixture(t, answerWith(cloneFile(true), 0))
	run := types.AgentRun{ID: uuid.New(), State: types.RunRunning, SandboxRef: "ref", CreatedAt: f.now.Add(-time.Minute)}
	f.st.runs[run.ID] = run // no policy row yet
	f.sweep(t)
	if _, ok := f.st.row(run.ID); ok {
		t.Error("a run with no recorded policy got a row")
	}
}

func TestSweepCloneStatus_MissingTenTimesWithMarkerIsNotReported(t *testing.T) {
	file := &switchable{text: cloneMarker}
	f := newCloneFixture(t, file.answer)
	run := f.addRun("ref", 15*time.Minute, cloneAskedRepos)
	f.sweep(t) // the marker, inside the ceiling: a cloning row
	if row, ok := f.st.row(run.ID); !ok || row.State != store.CloneStateCloning {
		t.Fatalf("row %+v (%v), want cloning", row, ok)
	}
	file.set("", 1)
	f.sweeps(t, cloneReadAttempts-1)
	if row, _ := f.st.row(run.ID); row.State != store.CloneStateCloning {
		t.Fatalf("state %q before the last attempt, want cloning", row.State)
	}
	f.sweeps(t, 1)
	row, _ := f.st.row(run.ID)
	if row.State != store.CloneStateNotReported {
		t.Fatalf("state %q, want not_reported", row.State)
	}
	if got, want := repoOutcomes(t, row), []string{cloneFixtureDest + "alpha=not_reported", cloneFixtureDest + "beta=not_reported"}; !slices.Equal(got, want) {
		t.Errorf("repos %v, want %v", got, want)
	}
}

// No marker was ever seen: an older image, or a launcher that never reached the clones. It reads none.
func TestSweepCloneStatus_MissingTenTimesNoMarkerIsNone(t *testing.T) {
	f := newCloneFixture(t, answerWith("", 1))
	run := f.addRun("ref", 15*time.Minute, cloneAskedRepos)
	f.sweeps(t, cloneReadAttempts)
	if row, ok := f.st.row(run.ID); !ok || row.State != store.CloneStateNone || string(row.Repos) != "[]" {
		t.Errorf("row %+v (%v), want none", row, ok)
	}
}

func TestSweepCloneStatus_FirstMarkerWritesCloning(t *testing.T) {
	f := newCloneFixture(t, answerWith(cloneMarker, 0))
	run := f.addRun("ref", time.Minute, cloneAskedRepos)
	f.sweep(t)
	if row, ok := f.st.row(run.ID); !ok || row.State != store.CloneStateCloning {
		t.Fatalf("row %+v (%v), want cloning", row, ok)
	}
	if runs, _ := f.st.ListCloneStatusRuns(context.Background()); len(runs) != 1 {
		t.Errorf("%d runs listed, want the cloning run to stay listed", len(runs))
	}
}

// A revive that starts the agent again clears the row and this replica's memory of the run, so the new start's
// marker writes a fresh cloning row instead of being taken for the old one.
func TestClearRunCloneStatus_SweepReadsTheNextMarkerAfresh(t *testing.T) {
	f := newCloneFixture(t, answerWith(cloneMarker, 0))
	run := f.addRun("ref", time.Minute, cloneAskedRepos)
	f.sweep(t)
	if row, ok := f.st.row(run.ID); !ok || row.State != store.CloneStateCloning {
		t.Fatalf("row %+v (%v), want cloning", row, ok)
	}
	f.srv.clearRunCloneStatus(context.Background(), run.ID)
	if _, ok := f.st.row(run.ID); ok {
		t.Fatal("the row survived the clear")
	}
	f.sweeps(t, 1)
	if row, ok := f.st.row(run.ID); !ok || row.State != store.CloneStateCloning {
		t.Errorf("after the clear: row %+v (%v), want a fresh cloning row", row, ok)
	}
}

func TestSweepCloneStatus_CloningNotCountedBeforeCeiling(t *testing.T) {
	f := newCloneFixture(t, answerWith(cloneMarker, 0))
	run := f.addRun("ref", 15*time.Minute, cloneAskedRepos)
	f.sweeps(t, 20)
	if row, _ := f.st.row(run.ID); row.State != store.CloneStateCloning || f.st.finalCount() != 0 {
		t.Errorf("state %q, %d final writes; want only the cloning mark", row.State, f.st.finalCount())
	}
}

func TestSweepCloneStatus_CloningPastCeilingCountsAsMissing(t *testing.T) {
	f := newCloneFixture(t, answerWith(cloneMarker, 0))
	run := f.addRun("ref", 25*time.Minute, cloneAskedRepos)
	f.sweeps(t, cloneReadAttempts-1)
	if row, _ := f.st.row(run.ID); row.State != store.CloneStateCloning {
		t.Fatalf("state %q before the last attempt, want cloning", row.State)
	}
	f.sweeps(t, 1)
	if row, _ := f.st.row(run.ID); row.State != store.CloneStateNotReported {
		t.Errorf("state %q, want not_reported", row.State)
	}
}

func TestSweepCloneStatus_MissingInsideGraceNotCounted(t *testing.T) {
	f := newCloneFixture(t, answerWith("", 1))
	run := f.addRun("ref", 5*time.Minute, cloneAskedRepos)
	f.sweeps(t, 20)
	if _, ok := f.st.row(run.ID); ok {
		t.Error("a missing file inside the grace stored a row")
	}
}

// With a pass that reads one run, three runs are read in turn, never-checked first; every outcome stamps its run,
// so one that reads as cloning, or sits inside the grace, still goes to the back.
func TestSweepCloneStatus_FairPickOldestChecked(t *testing.T) {
	files := map[string]string{"cloning": cloneMarker, "grace": "", "plain": ""}
	f := newCloneFixture(t, func(ref string) (*runner.ExecSession, error) {
		code := 0
		if files[ref] == "" {
			code = 1
		}
		return textPane(files[ref], code)(runner.ExecSpec{})
	})
	f.addRun("cloning", 15*time.Minute, cloneAskedRepos)
	f.addRun("plain", 13*time.Minute, cloneAskedRepos)
	f.addRun("grace", 5*time.Minute, cloneAskedRepos) // inside the grace: nothing counted
	var order []string
	for range 4 {
		f.now = f.now.Add(time.Second)
		before := len(f.rn.readRefs())
		if err := f.srv.sweepCloneStatusN(context.Background(), 1); err != nil {
			t.Fatal(err)
		}
		refs := f.rn.readRefs()
		if len(refs) != before+1 {
			t.Fatalf("a pass read %d runs, want 1", len(refs)-before)
		}
		f.rn.mu.Lock()
		order = append(order, f.rn.refs[len(f.rn.refs)-1])
		f.rn.mu.Unlock()
	}
	if want := []string{"cloning", "plain", "grace", "cloning"}; !slices.Equal(order, want) {
		t.Errorf("reads in order %v, want %v", order, want)
	}
}

func TestSweepCloneStatus_WorkersBounded(t *testing.T) {
	var inflight, peak atomic.Int32
	f := newCloneFixture(t, func(string) (*runner.ExecSession, error) {
		n := inflight.Add(1)
		defer inflight.Add(-1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(40 * time.Millisecond)
		return textPane("", 1)(runner.ExecSpec{})
	})
	for i := range 12 {
		f.addRun(fmt.Sprintf("ref-%d", i), time.Minute, cloneAskedRepos)
	}
	f.sweep(t)
	if got := len(f.rn.readRefs()); got != 12 {
		t.Errorf("%d reads, want all 12 runs read", got)
	}
	if p := peak.Load(); p > cloneProbeWorkers || p < 2 {
		t.Errorf("peak %d reads in flight, want between 2 and %d", p, cloneProbeWorkers)
	}
}
