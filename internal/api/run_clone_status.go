// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/sweephealth"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Each repository clone's outcome (#3159): the launcher writes one state file when its clones end, wardynd reads it
// once over the exec channel on the run watcher tick and stores the outcome per repository. The file is the sandbox's
// word, decoded strictly and read as a hint, as the service supervisor's is (run_service_health.go).
const (
	cloneStatePath     = "/home/agent/.wardyn/clones.json"
	cloneStateMaxBytes = 64 << 10 // 64 entries of a 512-byte dest fit with room
	cloneStateMaxRepos = 64
	cloneStateMaxDest  = 512
	cloneReadAttempts  = 10
	cloneSweepBudget   = 20 * time.Second
	// cloneProbeTimeout bounds one run's read and cloneProbeWorkers the reads in flight, as serviceProbeTimeout and
	// serviceProbeWorkers do: a FIFO or a slow file in the sandbox costs one worker five seconds, never the pass.
	cloneProbeTimeout = 5 * time.Second
	cloneProbeWorkers = 8
	// cloneStateGrace is how long after the run's admission a missing file is not counted, as serviceSupervisorGrace
	// is: it covers the launcher's start before dispatch_repo_clones writes its in-progress marker, and an older image
	// that never writes one.
	cloneStateGrace = 10 * time.Minute
	// cloneMarkerCeiling is how long after admission an in-progress marker is believed: twice the grace. A launcher
	// that died mid-clone, or a sandbox that writes the marker on purpose, then counts as missing.
	cloneMarkerCeiling = 2 * cloneStateGrace
)

// The states GET /runs/{id}/clones answers. The first three are stored (store.CloneState*); pending is a run still
// to be heard from, and none is a run that never reached the clone step.
const (
	clonesStateReported    = store.CloneStateReported
	clonesStateNotReported = store.CloneStateNotReported
	clonesStateNone        = store.CloneStateNone
	clonesStatePending     = "pending"
)

// cloneStateArgv is the one command run in the sandbox: fixed, no env, no TTY, as the agent user.
var cloneStateArgv = []string{"cat", cloneStatePath}

// runRepoClone is one repository's outcome, as stored and as the route answers it. Outcome is ok, present, skipped
// or failed from the launcher, or not_reported when Wardyn has none; Cause names why a clone was skipped or failed.
type runRepoClone struct {
	Repo    string `json:"repo,omitempty"`
	Dest    string `json:"dest"`
	Outcome string `json:"outcome"`
	Cause   string `json:"cause,omitempty"`
}

// cloneStateFile is the launcher's clones.json, version 1. Done is false for the in-progress marker.
type cloneStateFile struct {
	V     int               `json:"v"`
	Done  *bool             `json:"done"`
	Repos []cloneStateEntry `json:"repos"`
}

type cloneStateEntry struct {
	Dest    string `json:"dest"`
	Outcome string `json:"outcome"`
	Cause   string `json:"cause"`
}

// decodeCloneState reads the file strictly: one JSON object of exactly the known keys, version 1, a done boolean, at
// most cloneStateMaxRepos entries each with a known outcome and cause and a printable dest, none repeated, and none
// at all in the marker. Anything else is an error, and the caller counts the file unreadable.
func decodeCloneState(raw []byte) (cloneStateFile, error) {
	var f cloneStateFile
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return cloneStateFile{}, err
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return cloneStateFile{}, errors.New("clone state: trailing data")
	}
	switch {
	case f.V != 1:
		return cloneStateFile{}, errors.New("clone state: unknown version")
	case f.Done == nil:
		return cloneStateFile{}, errors.New("clone state: no done flag")
	case len(f.Repos) > cloneStateMaxRepos:
		return cloneStateFile{}, errors.New("clone state: too many repositories")
	case !*f.Done && len(f.Repos) > 0:
		return cloneStateFile{}, errors.New("clone state: entries in the in-progress marker")
	}
	seen := map[string]bool{}
	for _, e := range f.Repos {
		if !slices.Contains([]string{"ok", "present", "skipped", "failed"}, e.Outcome) {
			return cloneStateFile{}, errors.New("clone state: unknown outcome")
		}
		if !slices.Contains([]string{"", "not_a_checkout", "cannot_create", "clone_failed", "ref_failed"}, e.Cause) {
			return cloneStateFile{}, errors.New("clone state: unknown cause")
		}
		if len(e.Dest) > cloneStateMaxDest || strings.ContainsFunc(e.Dest, func(r rune) bool { return !unicode.IsPrint(r) }) {
			return cloneStateFile{}, errors.New("clone state: bad dest")
		}
		if seen[e.Dest] {
			return cloneStateFile{}, errors.New("clone state: a dest repeats")
		}
		seen[e.Dest] = true
	}
	return f, nil
}

// cloneProbe is what one read of the state file amounted to.
type cloneProbe int

const (
	cloneProbeRead        cloneProbe = iota // the file decoded, done
	cloneProbeCloning                       // the file decoded, the in-progress marker
	cloneProbeMissing                       // the sandbox answered and has no such file
	cloneProbeUnreachable                   // no answer, or an answer that is not the file
)

// readCloneState runs the read once on the readServiceState pattern (exec, drain, close), bounded by ctx and
// cloneStateMaxBytes. The caller puts cloneProbeTimeout on ctx, so a slow file is unreachable. The file is returned
// with cloneProbeRead and cloneProbeCloning.
func (s *Server) readCloneState(ctx context.Context, ref string) (cloneStateFile, cloneProbe) {
	sess, err := s.cfg.Runner.ExecStream(ctx, ref, runner.ExecSpec{Argv: cloneStateArgv})
	if sess != nil && sess.Close != nil {
		defer func() { _ = sess.Close() }() // tears down only this exec, never the sandbox
	}
	if err != nil || sess == nil || sess.Stdout == nil {
		return cloneStateFile{}, cloneProbeUnreachable
	}
	if sess.Stderr != nil {
		go func() { _, _ = io.Copy(io.Discard, sess.Stderr) }()
	}
	type answer struct {
		raw  []byte
		code int
		err  error
	}
	done := make(chan answer, 1)
	go func() {
		raw, rerr := io.ReadAll(io.LimitReader(sess.Stdout, cloneStateMaxBytes+1))
		if rerr != nil || len(raw) > cloneStateMaxBytes || sess.Wait == nil {
			done <- answer{raw: raw, err: rerr}
			return
		}
		code, werr := sess.Wait()
		done <- answer{raw, code, werr}
	}()
	select {
	case a := <-done:
		switch {
		case a.err != nil || len(a.raw) > cloneStateMaxBytes:
			return cloneStateFile{}, cloneProbeUnreachable
		case a.code != 0: // cat found no file (or could not read it)
			return cloneStateFile{}, cloneProbeMissing
		}
		f, derr := decodeCloneState(a.raw)
		switch {
		case derr != nil:
			return cloneStateFile{}, cloneProbeUnreachable
		case !*f.Done:
			return f, cloneProbeCloning
		}
		return f, cloneProbeRead
	case <-ctx.Done():
		return cloneStateFile{}, cloneProbeUnreachable
	}
}

// cloneExpect is one repository the run asked to have cloned.
type cloneExpect struct{ dest, slug string }

// expectedClones is the repositories the run's launcher is asked to clone, from the effective policy and the run's
// own repository, by the same buildRepoRecords call dispatch makes. ok is false while the policy is not recorded
// (or cannot be read): the caller waits for the next tick.
func (s *Server) expectedClones(ctx context.Context, run types.AgentRun) (exp []cloneExpect, ok bool) {
	envelope, _, found, err := s.runEffectivePolicy(ctx, run.ID)
	if err != nil || !found {
		return nil, false
	}
	recs, _ := buildRepoRecords(run.Repo, envelope.WorkspaceRepos)
	// recs is the framed WARDYN_REPOS value: records on newline, fields on tab (url, dest, slug, ref).
	for rec := range strings.SplitSeq(recs, "\n") {
		f := strings.Split(rec, "\t")
		if len(f) < 2 {
			continue
		}
		e := cloneExpect{dest: f[1]}
		if len(f) > 2 {
			e.slug = f[2]
		}
		exp = append(exp, e)
	}
	return exp, true
}

// notReportedClones is every expected repository, with no outcome from the sandbox.
func notReportedClones(exp []cloneExpect) []runRepoClone {
	out := make([]runRepoClone, 0, len(exp))
	for _, e := range exp {
		out = append(out, runRepoClone{Repo: e.slug, Dest: e.dest, Outcome: clonesStateNotReported})
	}
	return out
}

// reportedClones is the entries the sandbox wrote for the repositories the run asked for, in the run's order; an
// asked-for repository with no entry is not_reported. An entry for any other dest is dropped: the file can only
// speak about this run's own repositories.
func reportedClones(exp []cloneExpect, f cloneStateFile) []runRepoClone {
	out := make([]runRepoClone, 0, len(exp))
	for _, e := range exp {
		i := slices.IndexFunc(f.Repos, func(en cloneStateEntry) bool { return en.Dest == e.dest })
		if i < 0 {
			out = append(out, runRepoClone{Repo: e.slug, Dest: e.dest, Outcome: clonesStateNotReported})
			continue
		}
		out = append(out, runRepoClone{Repo: e.slug, Dest: e.dest, Outcome: f.Repos[i].Outcome, Cause: f.Repos[i].Cause})
	}
	return out
}

// cloneSweepRun is what this replica remembers of one run between passes.
type cloneSweepRun struct {
	checked    time.Time // when this replica last picked the run, on every outcome
	attempts   int       // counted missing or unreachable reads, after the grace
	markerSeen bool      // the cloning row was written
}

// cloneSweepState is a server's memory between passes, one entry per run still listed.
type cloneSweepState struct {
	mu   sync.Mutex
	runs map[uuid.UUID]*cloneSweepRun
}

// cloneSweepStates holds each server's state, so the sweep adds nothing to the Server struct.
var cloneSweepStates sync.Map // *Server -> *cloneSweepState

func (s *Server) cloneSweepMemory() *cloneSweepState {
	v, _ := cloneSweepStates.LoadOrStore(s, &cloneSweepState{runs: map[uuid.UUID]*cloneSweepRun{}})
	return v.(*cloneSweepState)
}

// pick stamps the run as read now and returns what is remembered of it.
func (m *cloneSweepState) pick(id uuid.UUID, now time.Time) cloneSweepRun {
	m.mu.Lock()
	defer m.mu.Unlock()
	e := m.runs[id]
	if e == nil {
		e = &cloneSweepRun{}
		m.runs[id] = e
	}
	e.checked = now
	return *e
}

func (m *cloneSweepState) get(id uuid.UUID) cloneSweepRun {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e := m.runs[id]; e != nil {
		return *e
	}
	return cloneSweepRun{}
}

func (m *cloneSweepState) update(id uuid.UUID, f func(*cloneSweepRun)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e := m.runs[id]; e != nil {
		f(e)
	}
}

func (m *cloneSweepState) drop(id uuid.UUID) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.runs, id)
}

// lastChecked is when the run was last picked, zero for a run never read.
func (m *cloneSweepState) lastChecked(id uuid.UUID) time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e := m.runs[id]; e != nil {
		return e.checked
	}
	return time.Time{}
}

// keepOnly drops the entries of runs no longer listed.
func (m *cloneSweepState) keepOnly(listed map[uuid.UUID]bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id := range m.runs {
		if !listed[id] {
			delete(m.runs, id)
		}
	}
}

// sweepCloneStatus is the run watcher's clone status pass: each running run with a sandbox and no final outcome is
// read once for the launcher's state file. A run with a runner is read by the replica that holds that runner's
// session; a run with none is read by the sweep leader alone, under its term. Both kinds share one budget and one
// oldest-checked-first order.
func (s *Server) sweepCloneStatus(ctx context.Context) error { return s.sweepCloneStatusN(ctx, 0) }

// sweepCloneStatusN is sweepCloneStatus reading at most maxReads runs (0: no bound beyond the budget), the test's seam
// for a pass cut short.
func (s *Server) sweepCloneStatusN(ctx context.Context, maxReads int) error {
	st, ok := s.cfg.Store.(store.RunCloneStatusStore)
	if !ok || s.cfg.Runner == nil {
		return nil
	}
	runs, err := st.ListCloneStatusRuns(ctx)
	if err != nil {
		return err
	}
	mem := s.cloneSweepMemory()
	listed := make(map[uuid.UUID]bool, len(runs))
	for _, run := range runs {
		listed[run.ID] = true
	}
	mem.keepOnly(listed)
	ctx, cancel := context.WithTimeout(ctx, cloneSweepBudget)
	defer cancel()
	var reads, led []cloneRead
	for _, run := range runs {
		switch {
		case run.RunnerID == nil:
			led = append(led, cloneRead{ctx: ctx, run: run})
		case s.runnerOnline(*run.RunnerID):
			reads = append(reads, cloneRead{ctx: ctx, run: run})
		}
	}
	if len(led) > 0 {
		if lctx, end, ok := s.beginLeaderSweep(ctx); ok {
			defer end()
			for _, rd := range led {
				rd.ctx = lctx
				reads = append(reads, rd)
			}
		} else {
			sweephealth.NotOwner(ctx)
		}
	}
	s.readCloneRuns(st, mem, reads, maxReads)
	return nil
}

// cloneRead is one run a pass reads, with the context it is read under.
type cloneRead struct {
	ctx context.Context
	run types.AgentRun
}

// readCloneRuns reads runs in parallel, the least recently checked first (never-checked first, ties by creation) so
// a pass cut short by its budget never starves the same runs. Every pick stamps its run as checked, whatever the
// read finds. A read whose context is done is skipped, never started.
func (s *Server) readCloneRuns(st store.RunCloneStatusStore, mem *cloneSweepState, reads []cloneRead, maxReads int) {
	slices.SortStableFunc(reads, func(a, b cloneRead) int {
		if c := mem.lastChecked(a.run.ID).Compare(mem.lastChecked(b.run.ID)); c != 0 {
			return c
		}
		return a.run.CreatedAt.Compare(b.run.CreatedAt)
	})
	if maxReads > 0 && len(reads) > maxReads {
		reads = reads[:maxReads]
	}
	var wg sync.WaitGroup
	slots := make(chan struct{}, cloneProbeWorkers)
	defer wg.Wait()
	for _, rd := range reads {
		select {
		case slots <- struct{}{}:
		case <-rd.ctx.Done():
			continue
		}
		if rd.ctx.Err() != nil {
			<-slots
			continue
		}
		mem.pick(rd.run.ID, s.cfg.Now())
		wg.Go(func() {
			defer func() { <-slots }()
			defer func() {
				if r := recover(); r != nil {
					slog.ErrorContext(rd.ctx, "wardynd: PANIC in clone status read (contained)",
						slog.String("run_id", rd.run.ID.String()), slog.Any("panic", r))
				}
			}()
			s.readCloneRun(rd.ctx, st, mem, rd.run)
		})
	}
}

// putCloneStatus stores one state, logging a failure: the status is a hint and the next tick reads again.
func putCloneStatus(ctx context.Context, st store.RunCloneStatusStore, run types.AgentRun, state string, repos []runRepoClone, now time.Time) bool {
	raw, err := json.Marshal(repos)
	if err != nil {
		return false
	}
	if _, err := st.PutRunCloneStatus(ctx, store.RunCloneStatus{RunID: run.ID, State: state, Repos: raw, ObservedAt: now}); err != nil {
		slog.WarnContext(ctx, "wardynd: storing a run's clone status failed", slog.String("run_id", run.ID.String()), slog.Any("err", err))
		return false
	}
	return true
}

// readCloneRun reads one run's state file and stores what it found. It never calls TouchRun: a probe must not keep a
// run alive past an idle wall.
func (s *Server) readCloneRun(ctx context.Context, st store.RunCloneStatusStore, mem *cloneSweepState, run types.AgentRun) {
	exp, ok := s.expectedClones(ctx, run)
	if !ok {
		return // the policy is not recorded yet; the next tick asks again
	}
	now := s.cfg.Now().UTC()
	if len(exp) == 0 {
		if putCloneStatus(ctx, st, run, clonesStateNone, []runRepoClone{}, now) {
			mem.drop(run.ID)
		}
		return
	}
	pctx, cancel := context.WithTimeout(ctx, cloneProbeTimeout)
	f, probe := s.readCloneState(pctx, run.SandboxRef)
	cancel()
	since := run.CreatedAt
	if run.AdmittedAt != nil {
		since = *run.AdmittedAt
	}
	age := now.Sub(since)
	switch probe {
	case cloneProbeRead:
		if putCloneStatus(ctx, st, run, clonesStateReported, reportedClones(exp, f), now) {
			mem.drop(run.ID)
		}
		return
	case cloneProbeCloning:
		if !mem.get(run.ID).markerSeen {
			if putCloneStatus(ctx, st, run, store.CloneStateCloning, []runRepoClone{}, now) {
				mem.update(run.ID, func(e *cloneSweepRun) { e.markerSeen = true })
			}
		}
		if age < cloneMarkerCeiling {
			return // still cloning, as far as the launcher says
		}
	}
	// Missing, unreachable, or a marker held past its ceiling: counted only after the clone must have completed,
	// which the server sees only as the marker's absence once the grace has passed.
	if age < cloneStateGrace {
		return
	}
	var attempts int
	mem.update(run.ID, func(e *cloneSweepRun) { e.attempts++; attempts = e.attempts })
	if attempts < cloneReadAttempts {
		return
	}
	// A cloning row says the launcher started its clones and sent no outcome; with none, it never reached them.
	final, repos := clonesStateNone, []runRepoClone{}
	if row, err := st.GetRunCloneStatus(ctx, run.ID); err == nil && row.State == store.CloneStateCloning {
		final, repos = clonesStateNotReported, notReportedClones(exp)
	} else if err != nil && !errors.Is(err, store.ErrNotFound) {
		return
	}
	if putCloneStatus(ctx, st, run, final, repos, now) {
		mem.drop(run.ID)
	}
}

// clearRunCloneStatus forgets the stored outcome of a run whose agent started again: its launcher clones again, so
// the sweep reads this start's. A failure only warns: the view is a hint, and the revive has already succeeded.
func (s *Server) clearRunCloneStatus(ctx context.Context, runID uuid.UUID) {
	cs, ok := s.cfg.Store.(store.RunCloneStatusStore)
	if !ok {
		return
	}
	if err := cs.DeleteRunCloneStatus(context.WithoutCancel(ctx), runID); err != nil {
		slog.WarnContext(ctx, "wardynd: clearing a revived run's clone status failed", slog.String("run_id", runID.String()), slog.Any("err", err))
	}
}

// runClonesResponse is GET /api/v1/runs/{id}/clones: State is reported, not_reported, pending or none. None, and
// pending, carry no repositories.
type runClonesResponse struct {
	RunID uuid.UUID      `json:"run_id"`
	State string         `json:"state"`
	Repos []runRepoClone `json:"repos"` // never null
}

// handleRunClones is GET /runs/{id}/clones: each repository's clone outcome as the run's launcher reported it, a
// hint and never a verdict. It reads the stored row and never the sandbox.
func (s *Server) handleRunClones(w http.ResponseWriter, r *http.Request) {
	id, ok := parseIDParam(w, r, "id", "run")
	if !ok {
		return
	}
	run, ok := s.getRunAuthorized(w, r, id)
	if !ok {
		return
	}
	ctx := r.Context()
	resp := runClonesResponse{RunID: run.ID, State: clonesStateNone, Repos: []runRepoClone{}}
	cs, ok := s.cfg.Store.(store.RunCloneStatusStore)
	if !ok {
		writeJSON(w, http.StatusOK, resp)
		return
	}
	row, err := cs.GetRunCloneStatus(ctx, run.ID)
	found := err == nil
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		writeServerError(w, r, "read run clones", err)
		return
	}
	switch {
	case found && row.State != store.CloneStateCloning:
		resp.State = row.State
		if row.State != clonesStateNone && json.Unmarshal(row.Repos, &resp.Repos) != nil {
			resp.Repos = []runRepoClone{}
		}
	case !run.State.IsTerminal():
		resp.State = clonesStatePending
	case found:
		// It reached the clone step and ended before the sweep wrote an outcome.
		if exp, ok := s.expectedClones(ctx, run); ok && len(exp) > 0 {
			resp.State, resp.Repos = clonesStateNotReported, notReportedClones(exp)
		}
	}
	writeJSON(w, http.StatusOK, resp)
}
