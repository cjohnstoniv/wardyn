// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// wallsLiveStore serves the live streams' reads from memory: decision rows as the accessor filters them (action,
// recorded_at, seq, limit), and runs by id. It keeps every query, so a test reads the cursor and floor each beat
// sent. Methods the streams never call are the nil embedded Store's.
type wallsLiveStore struct {
	store.Store
	mu      sync.Mutex
	rows    []store.DecisionRow
	runs    map[uuid.UUID]types.AgentRun
	queries []store.DecisionQuery
	readErr error
	runErr  error
	anyRow  bool // serve every row whatever its action
}

var (
	_ store.WallsAuditReader = (*wallsLiveStore)(nil)
	_ store.RunSweepReader   = (*wallsLiveStore)(nil)
)

func (s *wallsLiveStore) AuditWallsRows(_ context.Context, q store.DecisionQuery) ([]store.DecisionRow, store.DecisionCursor, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.queries = append(s.queries, q)
	if s.readErr != nil {
		return nil, q.After, s.readErr
	}
	var out []store.DecisionRow
	next := q.After
	for _, r := range s.rows {
		if r.Seq <= q.After.Seq || r.RecordedAt.Before(q.Since) || (!s.anyRow && !slices.Contains(q.Actions, r.Action)) {
			continue
		}
		if len(out) == q.Limit {
			break
		}
		out, next = append(out, r), store.DecisionCursor{Seq: r.Seq}
	}
	return out, next, nil
}

func (s *wallsLiveStore) GetRunsByID(_ context.Context, ids []uuid.UUID) ([]types.AgentRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.runErr != nil {
		return nil, s.runErr
	}
	var out []types.AgentRun
	for _, id := range ids {
		if r, ok := s.runs[id]; ok {
			out = append(out, r)
		}
	}
	return out, nil
}

func (s *wallsLiveStore) GetRun(_ context.Context, id uuid.UUID) (types.AgentRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.runs[id]; ok {
		return r, nil
	}
	return types.AgentRun{}, store.ErrNotFound
}

func (*wallsLiveStore) ListUndispatchedRuns(context.Context) ([]types.AgentRun, error) {
	return nil, nil
}

func (*wallsLiveStore) ListTerminalRunsWithSandbox(context.Context) ([]types.AgentRun, error) {
	return nil, nil
}

func (*wallsLiveStore) ListLiveRuns(context.Context) ([]types.AgentRun, error) { return nil, nil }

// addRun stores a running run of owner's frozen to unit (nil: none).
func (s *wallsLiveStore) addRun(owner string, unit *uuid.UUID) uuid.UUID {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := uuid.New()
	if s.runs == nil {
		s.runs = map[uuid.UUID]types.AgentRun{}
	}
	s.runs[id] = types.AgentRun{ID: id, CreatedBy: owner, UnitID: unit, State: types.RunRunning}
	return id
}

// add appends rows, numbering their seq in order from the last.
func (s *wallsLiveStore) add(rows ...store.DecisionRow) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range rows {
		r.Seq = int64(len(s.rows) + 1)
		r.RecordedAt = wallsLiveEpoch.Add(time.Duration(r.Seq) * time.Millisecond)
		s.rows = append(s.rows, r)
	}
}

func (s *wallsLiveStore) queryList() []store.DecisionQuery {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.queries)
}

func (s *wallsLiveStore) setReadErr(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.readErr = err
}

// wallsLiveEpoch is the instant every test row is recorded just after.
var wallsLiveEpoch = wallsTopoNewClock().now

// wallsLiveRow is a decision row for run decoded as the store decodes it, with the data keys opts set. Its
// RecordedAt and Seq are set by the store's add; until then it is decided at the epoch.
func wallsLiveRow(t *testing.T, run uuid.UUID, action, source, host string, opts ...func(map[string]any)) store.DecisionRow {
	t.Helper()
	data := map[string]any{"rule_source": source, "port": 443, "observed_at": wallsLiveEpoch}
	if host != "" {
		data["host"] = host
	}
	for _, o := range opts {
		o(data)
	}
	raw, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	ev := types.AuditEvent{ID: uuid.New(), Time: wallsLiveEpoch, Action: action, Data: raw}
	if run != uuid.Nil {
		ev.RunID = &run
	}
	row, err := store.DecodeDecisionRow(ev, wallsLiveEpoch, 0)
	if err != nil {
		t.Fatal(err)
	}
	return row
}

// wallsLiveWeight sets a row's multiplicity.
func wallsLiveWeight(n int) func(map[string]any) {
	return func(d map[string]any) { d["repeat"] = n }
}

// wallsLiveLate decides a row long before it is recorded.
func wallsLiveLate(d map[string]any) { d["observed_at"] = wallsLiveEpoch.Add(-time.Hour) }

// wallsLiveData sets one data key.
func wallsLiveData(key string, value any) func(map[string]any) {
	return func(d map[string]any) { d[key] = value }
}

// wallsLiveServer is a server on st whose clock is a fixed instant and whose sampler is beaten by hand.
func wallsLiveServer(st store.Store) (*Server, *wallsTopoClock) {
	clk := wallsTopoNewClock()
	s := wallsTopoServer(st, clk)
	s.wallsLive.sampler.manual = true
	return s, clk
}

// wallsLiveBeat makes one beat and returns it.
func wallsLiveBeat(t *testing.T, s *Server) wallsBeat {
	t.Helper()
	sm := &s.wallsLive.sampler
	before := sm.headN()
	sm.beatOnce(context.Background(), s)
	beats, head, _, gap := sm.since(before)
	if gap || len(beats) != 1 || beats[0].N != head {
		t.Fatalf("a beat after %d left %d beats, head %d, gap %v; want exactly the new one", before, len(beats), head, gap)
	}
	return beats[0]
}

// TestWallsSamplerFoldsByRun: a run's rows fold by weight, a late row counts only in Late, the unit counts are one
// per row and leave out the control plane's host and the start rows, hosts and samples leave them out too, and the
// samples are the first five refused rows, then the first five others, in seq order.
func TestWallsSamplerFoldsByRun(t *testing.T) {
	unit := uuid.New()
	st := &wallsLiveStore{}
	run := st.addRun("sub-walls-owner", &unit)
	allow := func(host string, opts ...func(map[string]any)) store.DecisionRow {
		return wallsLiveRow(t, run, "egress.allow", "policy:allowlist", host, opts...)
	}
	rows := []store.DecisionRow{
		allow("pypi.org", wallsLiveWeight(3)),
		wallsLiveRow(t, run, "egress.deny", "policy:denied", "evil.example", wallsLiveData("flow", "flow-1")),
		allow("pypi.org", wallsLiveWeight(2), wallsLiveLate),
		wallsLiveRow(t, run, "egress.allow", "brokered:mint", "cp.example", wallsLiveWeight(4)),
		wallsLiveRow(t, run, "egress.allow", "builtin:upstream-proxy", "proxy.example"),
		wallsLiveRow(t, run, "egress.hold", "approval:pending", "pypi.org", wallsLiveData("approval_id", "ap-1")),
		wallsLiveRow(t, uuid.Nil, "egress.allow", "policy:allowlist", "stray.example"),
	}
	st.add(rows...)
	s, _ := wallsLiveServer(st)
	fold := wallsLiveBeat(t, s).Folds[run]
	if fold == nil {
		t.Fatal("no fold for the run")
	}
	if want := (WallsCounters{Allowed: 3, Refused: 1, Guest: 1, Held: 1, Control: 4, Config: 1, Late: 2}); fold.Counters != want {
		t.Errorf("counters %+v, want %+v", fold.Counters, want)
	}
	if want := (WallsUnitCounters{Allowed: 2, Refused: 1, Guest: 1, Held: 1}); fold.Unit != want {
		t.Errorf("unit counters %+v, want %+v", fold.Unit, want)
	}
	wantHosts := []wallsHostFold{{Host: "pypi.org", Port: 443, Weight: 6, Rows: 3}, {Host: "evil.example", Port: 443, Weight: 1, Rows: 1}}
	if !slices.Equal(fold.Hosts, wantHosts) {
		t.Errorf("hosts %+v, want %+v", fold.Hosts, wantHosts)
	}
	var got []uuid.UUID
	for _, sm := range fold.Samples {
		got = append(got, sm.ID)
	}
	if want := []uuid.UUID{rows[0].ID, rows[1].ID, rows[2].ID, rows[5].ID}; !slices.Equal(got, want) {
		t.Errorf("samples %v, want the rows at seq 1, 2, 3 and 6", got)
	}
	if fold.Meta.UnitID == nil || *fold.Meta.UnitID != unit || fold.Meta.CreatedBy != "sub-walls-owner" {
		t.Errorf("meta %+v, want the run's unit and creator", fold.Meta)
	}

	// The sample rule: five non-late refused rows, then five others (a sixth refused row is an other), no more.
	st2 := &wallsLiveStore{}
	run2 := st2.addRun("sub-walls-owner", nil)
	var many []store.DecisionRow
	for range 7 {
		many = append(many, wallsLiveRow(t, run2, "egress.deny", "policy:denied", "a.example"))
	}
	for range 7 {
		many = append(many, wallsLiveRow(t, run2, "egress.allow", "policy:allowlist", "b.example"))
	}
	st2.add(many...)
	s2, _ := wallsLiveServer(st2)
	samples := wallsLiveBeat(t, s2).Folds[run2].Samples
	if len(samples) != 10 {
		t.Fatalf("%d samples, want 10", len(samples))
	}
	for i, sm := range samples {
		if sm.ID != many[i].ID {
			t.Errorf("sample %d is not the row at seq %d", i, i+1)
		}
	}
}

// TestWallsSamplerHeldRun: a held run's polling of the approvals endpoint is the run calling Wardyn, so a hundred
// such rows are Control and nothing else: no allowed count, no host, no sample.
func TestWallsSamplerHeldRun(t *testing.T) {
	st := &wallsLiveStore{}
	run := st.addRun("sub-walls-owner", nil)
	for range 100 {
		st.add(wallsLiveRow(t, run, "egress.allow", "brokered:approvals", "wardyn.example"))
	}
	s, _ := wallsLiveServer(st)
	fold := wallsLiveBeat(t, s).Folds[run]
	if fold == nil {
		t.Fatal("no fold for the run")
	}
	if want := (WallsCounters{Control: 100}); fold.Counters != want {
		t.Errorf("counters %+v, want %+v", fold.Counters, want)
	}
	if fold.Unit != (WallsUnitCounters{}) || len(fold.Hosts) != 0 || len(fold.Samples) != 0 {
		t.Errorf("unit %+v, %d hosts, %d samples; want none of them", fold.Unit, len(fold.Hosts), len(fold.Samples))
	}
}

// TestWallsSamplerCursorAndFloor: the first read starts at the start time and the zero cursor; after a beat with
// rows, the next starts at the last row's recorded time and cursor; a failed read leaves both, and its beat is
// stale with nothing in it.
func TestWallsSamplerCursorAndFloor(t *testing.T) {
	st := &wallsLiveStore{}
	run := st.addRun("sub-walls-owner", nil)
	st.add(wallsLiveRow(t, run, "egress.allow", "policy:allowlist", "a.example"), wallsLiveRow(t, run, "egress.allow", "policy:allowlist", "a.example"))
	s, clk := wallsLiveServer(st)
	wallsLiveBeat(t, s)
	st.setReadErr(errors.New("read refused"))
	stale := wallsLiveBeat(t, s)
	if !stale.Stale || len(stale.Folds) != 0 || len(stale.Notices) != 0 {
		t.Errorf("a failed read made %+v, want a stale beat with nothing in it", stale)
	}
	st.setReadErr(nil)
	st.add(wallsLiveRow(t, run, "egress.allow", "policy:allowlist", "a.example"))
	last := wallsLiveBeat(t, s)
	if last.Stale || last.Folds[run] == nil || last.Folds[run].Counters.Allowed != 1 {
		t.Errorf("the beat after the failure %+v, want the one new row", last)
	}
	qs := st.queryList()
	if len(qs) != 3 {
		t.Fatalf("%d reads, want 3", len(qs))
	}
	twoAt := wallsLiveEpoch.Add(2 * time.Millisecond)
	for i, want := range []struct {
		after int64
		since time.Time
	}{{0, clk.now}, {2, twoAt}, {2, twoAt}} {
		if qs[i].After.Seq != want.after || !qs[i].Since.Equal(want.since) || qs[i].Limit != wallsBeatLimit {
			t.Errorf("read %d: after %d since %v limit %d; want after %d since %v limit %d",
				i+1, qs[i].After.Seq, qs[i].Since, qs[i].Limit, want.after, want.since, wallsBeatLimit)
		}
		if !slices.Equal(qs[i].Actions, wallsSampleActions) {
			t.Errorf("read %d asked for actions %v", i+1, qs[i].Actions)
		}
	}
}

// TestWallsSamplerBeatNumbersAdvanceWhenEmpty: three beats with no rows number N, N+1, N+2, and after an idle stop
// the ring is empty and the next beat's number is above every number before.
func TestWallsSamplerBeatNumbersAdvanceWhenEmpty(t *testing.T) {
	s, clk := wallsLiveServer(&wallsLiveStore{})
	sm := &s.wallsLive.sampler
	first := wallsLiveBeat(t, s).N
	if second, third := wallsLiveBeat(t, s).N, wallsLiveBeat(t, s).N; second != first+1 || third != first+2 {
		t.Errorf("beat numbers %d, %d, %d; want consecutive", first, second, third)
	}
	sm.stop()
	if beats, head, _, gap := sm.since(first); len(beats) != 0 || head != first+2 || !gap {
		t.Errorf("after a stop: %d beats, head %d, gap %v; want an empty ring, the old head and a gap", len(beats), head, gap)
	}
	clk.now = clk.now.Add(100 * time.Second)
	sm.beatOnce(context.Background(), s)
	if n := sm.headN(); n <= first+2 {
		t.Errorf("the first beat after a stop is %d, want above %d", n, first+2)
	}
}

// TestWallsSamplerRingBounds: the 121st beat evicts the first, the volume bound evicts oldest first, and since
// reports a gap for a number the ring no longer holds.
func TestWallsSamplerRingBounds(t *testing.T) {
	s, _ := wallsLiveServer(&wallsLiveStore{})
	sm := &s.wallsLive.sampler
	for range wallsRingBeats + 1 {
		wallsLiveBeat(t, s)
	}
	beats, head, _, gap := sm.since(1)
	if gap || len(beats) != wallsRingBeats || beats[0].N != 2 || head != wallsRingBeats+1 {
		t.Errorf("after %d beats: %d held from N %d, head %d, gap %v; want %d held from 2 with no gap",
			wallsRingBeats+1, len(beats), beats[0].N, head, gap, wallsRingBeats)
	}
	if _, _, _, gap := sm.since(0); !gap {
		t.Error("a resume before the oldest held beat is not a gap")
	}
	if _, _, _, gap := sm.since(head + 1); !gap {
		t.Error("a resume past the head is not a gap")
	}
	if _, _, _, gap := sm.since(head); gap {
		t.Error("a resume at the head is a gap")
	}

	st := &wallsLiveStore{}
	run := st.addRun("sub-walls-owner", nil)
	s2, _ := wallsLiveServer(st)
	s2.wallsLive.sampler.maxVolume = 10
	for range 5 {
		st.add(wallsLiveRow(t, run, "egress.allow", "policy:allowlist", "a.example"))
		wallsLiveBeat(t, s2)
	}
	// Each beat holds a fold, a host and a sample: three entries, so three beats fit under ten.
	beats, _, _, gap = s2.wallsLive.sampler.since(2)
	if gap || len(beats) != 3 || beats[0].N != 3 {
		t.Errorf("under a volume bound of 10: %d beats from N %d, gap %v; want 3 from N 3", len(beats), beats[0].N, gap)
	}
	if _, _, _, gap := s2.wallsLive.sampler.since(1); !gap {
		t.Error("a resume at an evicted beat is not a gap")
	}
}

// TestWallsSamplerStopsWhenIdle: with the idle time at 20 ms, the loop stops after the last release and reads
// nothing more.
func TestWallsSamplerStopsWhenIdle(t *testing.T) {
	st := &wallsLiveStore{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &Server{cfg: Config{Store: st, Now: time.Now, BaseCtx: ctx}}
	sm := &s.wallsLive.sampler
	sm.tick, sm.idle = 5*time.Millisecond, 20*time.Millisecond
	release := sm.subscribe(s)
	waitFor(t, "two reads", func() bool { return len(st.queryList()) >= 2 })
	release()
	waitFor(t, "the loop to stop", func() bool {
		sm.mu.Lock()
		defer sm.mu.Unlock()
		return !sm.running
	})
	n := len(st.queryList())
	time.Sleep(60 * time.Millisecond)
	if after := len(st.queryList()); after != n {
		t.Errorf("%d reads after the loop stopped, want none", after-n)
	}
	s.bg.Wait()
}

// TestWallsSamplerNeverWaitsOnASubscriber: a stream that never reads does not hold up a beat.
func TestWallsSamplerNeverWaitsOnASubscriber(t *testing.T) {
	s, _ := wallsLiveServer(&wallsLiveStore{})
	sm := &s.wallsLive.sampler
	release := sm.subscribe(s)
	defer release()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 50 {
			sm.beatOnce(context.Background(), s)
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("50 beats did not finish with a subscriber that never reads")
	}
	if head := sm.headN(); head != 50 {
		t.Errorf("head %d after 50 beats, want 50", head)
	}
}

// TestWallsSamplerNotices: each mapped action makes a notice, a repeat of one in a beat is one notice, an action the
// map lacks is dropped, and no notice row is in any fold.
func TestWallsSamplerNotices(t *testing.T) {
	unit := uuid.New()
	st := &wallsLiveStore{anyRow: true}
	run := st.addRun("sub-walls-owner", &unit)
	only := st.addRun("sub-walls-owner", nil)
	notice := func(action, approval string) store.DecisionRow {
		var opts []func(map[string]any)
		if approval != "" {
			opts = append(opts, wallsLiveData("approval_id", approval))
		}
		return wallsLiveRow(t, run, action, "", "", opts...)
	}
	st.add(
		notice("approval.decide", "ap-1"), notice("approval.decide", "ap-1"), notice("approval.decide", "ap-2"),
		notice("approval.expire", "ap-3"), notice("approval.cancel", "ap-4"), notice("run.policy.generation", ""),
		notice("credential.mint", ""), notice("credential.revoke", ""), notice("approval.request", "ap-5"),
		wallsLiveRow(t, only, "approval.decide", "", "", wallsLiveData("approval_id", "ap-6")),
		wallsLiveRow(t, run, "egress.allow", "policy:allowlist", "a.example"),
	)
	s, _ := wallsLiveServer(st)
	beat := wallsLiveBeat(t, s)
	var got []string
	for _, n := range beat.Notices {
		got = append(got, n.Action+"/"+n.ApprovalID)
		if n.RunID == run && (n.Unit == nil || *n.Unit != unit) {
			t.Errorf("notice %s names unit %v, want the run's", n.Action, n.Unit)
		}
	}
	want := []string{"decide/ap-1", "decide/ap-2", "expire/ap-3", "cancel/ap-4", "policy.generation/", "credential.mint/", "revoke/", "decide/ap-6"}
	if !slices.Equal(got, want) {
		t.Errorf("notices %v, want %v", got, want)
	}
	if beat.Folds[only] != nil {
		t.Error("a run with only notice rows has a fold")
	}
	if fold := beat.Folds[run]; fold == nil || fold.Counters != (WallsCounters{Allowed: 1}) || len(fold.Samples) != 1 {
		t.Errorf("fold %+v, want the one egress row and no notice row in it", fold)
	}
}

// TestWallsSamplerUnitFoldLeavesOutStartRows: the upstream proxy's start row is a config count and nothing else: not
// in the unit counts, not a host, not a sample.
func TestWallsSamplerUnitFoldLeavesOutStartRows(t *testing.T) {
	st := &wallsLiveStore{}
	run := st.addRun("sub-walls-owner", nil)
	st.add(wallsLiveRow(t, run, "egress.allow", "builtin:upstream-proxy", "proxy.example"))
	s, _ := wallsLiveServer(st)
	fold := wallsLiveBeat(t, s).Folds[run]
	if fold == nil || fold.Counters != (WallsCounters{Config: 1}) {
		t.Fatalf("fold %+v, want one config count", fold)
	}
	if fold.Unit != (WallsUnitCounters{}) || len(fold.Hosts) != 0 || len(fold.Samples) != 0 {
		t.Errorf("unit %+v, %d hosts, %d samples; want none of them", fold.Unit, len(fold.Hosts), len(fold.Samples))
	}
}
