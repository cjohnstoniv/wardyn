// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package remote

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func eventually(t *testing.T, cond func() bool, within time.Duration, msg string) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal(msg)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func check(t *testing.T, ok bool, msg string) {
	t.Helper()
	if !ok {
		t.Fatal(msg)
	}
}

func sameIDs(t *testing.T, want, got []uuid.UUID, msg string) {
	t.Helper()
	w, g := slices.Clone(want), slices.Clone(got)
	cmp := func(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) }
	slices.SortFunc(w, cmp)
	slices.SortFunc(g, cmp)
	if !slices.Equal(w, g) {
		t.Fatalf("%s: want %v, got %v", msg, w, g)
	}
}

// liveFixture is one session the stored-row recheck covers, built as hub_recheck_test.go builds one.
type liveFixture struct {
	row   types.Runner
	link  *Link
	peer  *runnerwire.Peer
	local *runnerwire.LoopConn
	s     *hubSession
}

func personRunner(owner string) types.Runner {
	return types.Runner{ID: uuid.New(), Owner: owner, State: types.RunnerClaimed}
}

// setRecheckEvery sets storedRowRecheck for the Hub that NewHub builds next, and restores it after the test.
func setRecheckEvery(t *testing.T, d time.Duration) {
	t.Helper()
	old := storedRowRecheck
	storedRowRecheck = d
	t.Cleanup(func() { storedRowRecheck = old })
}

// addLiveSession puts a ready session for row into hub.sessions and registers it with addLive. wrap, when set,
// wraps the org end of the link.
func addLiveSession(t *testing.T, hub *Hub, row types.Runner, wrap func(runnerwire.Conn) runnerwire.Conn) *liveFixture {
	t.Helper()
	org, local := runnerwire.Loopback()
	var orgEnd runnerwire.Conn = org
	if wrap != nil {
		orgEnd = wrap(org)
	}
	t.Cleanup(func() { _ = local.Close() })
	link := &Link{}
	peer := link.AttachSession(orgEnd, runnerwire.PeerConfig{})
	if err := link.Ready(peer); err != nil {
		t.Fatal(err)
	}
	s := &hubSession{id: "session-" + row.ID.String(), peer: peer, link: link}
	id := s.id
	s.readyID.Store(&id)
	pr := perRunOf(row)
	s.perRun.Store(&pr)
	hub.mu.Lock()
	hub.sessions[row.ID] = s
	hub.mu.Unlock()
	hub.addLive(row.ID, s, peer)
	return &liveFixture{row: row, link: link, peer: peer, local: local, s: s}
}

// waitRecheckIdle waits until no stored-row action is running.
func waitRecheckIdle(t *testing.T, hub *Hub) {
	t.Helper()
	eventually(t, func() bool {
		hub.mu.Lock()
		defer hub.mu.Unlock()
		return len(hub.rechecking) == 0
	}, 2*time.Second, "a stored-row action is still running")
}

func requireOffline(t *testing.T, fx *liveFixture) {
	t.Helper()
	eventually(t, func() bool { return !fx.link.Online() && !fx.peer.Online() }, 2*time.Second, "the session is still online")
}

func requireOnline(t *testing.T, fx *liveFixture) {
	t.Helper()
	check(t, fx.link.Online() && fx.peer.Online(), "the session ended")
}

// rowsOf serves a fixed set of rows by id, counting each call that read them.
type rowsOf struct {
	mu      sync.Mutex
	rows    map[uuid.UUID]types.Runner
	many    [][]uuid.UUID
	singles []uuid.UUID
}

func (r *rowsOf) set(row types.Runner) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rows[row.ID] = row
}

func (r *rowsOf) lookupMany(_ context.Context, ids []uuid.UUID) ([]types.Runner, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.many = append(r.many, slices.Clone(ids))
	var out []types.Runner
	for _, id := range ids {
		if row, ok := r.rows[id]; ok {
			out = append(out, row)
		}
	}
	return out, nil
}

func (r *rowsOf) lookup(_ context.Context, id uuid.UUID) (types.Runner, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.singles = append(r.singles, id)
	row, ok := r.rows[id]
	if !ok {
		return types.Runner{}, errors.New("not found")
	}
	return row, nil
}

func newRows(rows ...types.Runner) *rowsOf {
	r := &rowsOf{rows: map[uuid.UUID]types.Runner{}}
	for _, row := range rows {
		r.rows[row.ID] = row
	}
	return r
}

func TestHubRecheckAllReadsEachThingOncePerTick(t *testing.T) {
	setRecheckEvery(t, time.Hour)
	a, b := personRunner("alice"), personRunner("alice")
	c := personRunner("org")
	c.OwnerKind = types.RunnerOwnerOrg
	rows := newRows(a, b, c)
	var disabled, inactive atomic.Int32
	hub := NewHub(t.Context(), HubConfig{
		Lookup:     rows.lookup,
		LookupMany: rows.lookupMany,
		Disabled:   func(context.Context) bool { disabled.Add(1); return false },
		Inactive:   func(context.Context, types.Runner) (bool, error) { inactive.Add(1); return false, nil },
	})
	t.Cleanup(hub.Close)
	fa, fb, fc := addLiveSession(t, hub, a, nil), addLiveSession(t, hub, b, nil), addLiveSession(t, hub, c, nil)

	check(t, hub.recheckAll(t.Context()), "recheckAll reported a closed Hub")
	waitRecheckIdle(t, hub)

	rows.mu.Lock()
	defer rows.mu.Unlock()
	check(t, len(rows.many) == 1, "LookupMany must be called once")
	sameIDs(t, []uuid.UUID{a.ID, b.ID, c.ID}, rows.many[0], "LookupMany ids")
	check(t, len(rows.singles) == 0, "Lookup must not be called")
	check(t, disabled.Load() == 1, "Disabled must be read once")
	check(t, inactive.Load() == 2, "Inactive must be asked once for alice and once for the organisation runner")
	for _, fx := range []*liveFixture{fa, fb, fc} {
		requireOnline(t, fx)
	}
}

func TestHubRecheckAllEndsEachSessionAsItsRowSays(t *testing.T) {
	setRecheckEvery(t, time.Hour)
	a := personRunner("alice")
	a.State = types.RunnerRevoked
	b := personRunner("bob")
	c := personRunner("carol")
	c.Limits = &types.RunnerLimits{RunsAtOnce: 1, PerRun: types.RunnerAmounts{CPUMillis: 4000, MemoryMiB: 8192}}
	d := personRunner("dave")
	rows := newRows(a, b, c, d)
	var mu sync.Mutex
	var asked []uuid.UUID
	hub := NewHub(t.Context(), HubConfig{
		LookupMany: rows.lookupMany,
		Inactive: func(_ context.Context, rn types.Runner) (bool, error) {
			mu.Lock()
			asked = append(asked, rn.ID)
			mu.Unlock()
			return rn.Owner == "bob", nil
		},
	})
	t.Cleanup(hub.Close)
	fa, fb, fd := addLiveSession(t, hub, a, nil), addLiveSession(t, hub, b, nil), addLiveSession(t, hub, d, nil)
	fc := addLiveSession(t, hub, c, nil)
	stale := perRunLimits{cpuMillis: 2000, memoryMiB: 4096}
	fc.s.perRun.Store(&stale)

	check(t, hub.recheckAll(t.Context()), "recheckAll reported a closed Hub")
	waitRecheckIdle(t, hub)

	requireOffline(t, fa)
	requireOffline(t, fb)
	requireOffline(t, fc)
	requireOnline(t, fd)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	for {
		f, err := fa.local.ReadFrame(ctx)
		if err != nil {
			t.Fatalf("no REVOKED frame reached the revoked runner: %v", err)
		}
		if f.Type == runnerwire.TypeRevoked {
			break
		}
	}
	mu.Lock()
	defer mu.Unlock()
	check(t, !slices.Contains(asked, a.ID), "Inactive was asked about a revoked row")
}

func TestHubRecheckAllSkipsATickItCannotRead(t *testing.T) {
	setRecheckEvery(t, time.Hour)
	a, b := personRunner("alice"), personRunner("bob")
	a.State, b.State = types.RunnerRevoked, types.RunnerRevoked
	var mode atomic.Int32
	var disabled atomic.Int32
	hub := NewHub(t.Context(), HubConfig{
		LookupMany: func(_ context.Context, ids []uuid.UUID) ([]types.Runner, error) {
			switch mode.Load() {
			case 0:
				return nil, errors.New("store down")
			case 1:
				return []types.Runner{b}, nil
			}
			return []types.Runner{a}, nil
		},
		Disabled: func(context.Context) bool { disabled.Add(1); return false },
	})
	t.Cleanup(hub.Close)
	fa, fb := addLiveSession(t, hub, a, nil), addLiveSession(t, hub, b, nil)

	check(t, hub.recheckAll(t.Context()), "recheckAll reported a closed Hub")
	waitRecheckIdle(t, hub)
	requireOnline(t, fa)
	requireOnline(t, fb)
	check(t, disabled.Load() == 0, "Disabled was read although no row was")

	mode.Store(1)
	check(t, hub.recheckAll(t.Context()), "recheckAll reported a closed Hub")
	requireOffline(t, fb)
	waitRecheckIdle(t, hub)
	requireOnline(t, fa)

	mode.Store(2)
	check(t, hub.recheckAll(t.Context()), "recheckAll reported a closed Hub")
	requireOffline(t, fa)
}

func TestHubRecheckAllFallsBackToLookup(t *testing.T) {
	setRecheckEvery(t, time.Hour)
	a, b := personRunner("alice"), personRunner("bob")
	a.State = types.RunnerRevoked
	rows := newRows(a, b)
	hub := NewHub(t.Context(), HubConfig{Lookup: rows.lookup})
	t.Cleanup(hub.Close)
	fa, fb := addLiveSession(t, hub, a, nil), addLiveSession(t, hub, b, nil)

	check(t, hub.recheckAll(t.Context()), "recheckAll reported a closed Hub")
	requireOffline(t, fa)
	waitRecheckIdle(t, hub)
	requireOnline(t, fb)

	rows.mu.Lock()
	defer rows.mu.Unlock()
	sameIDs(t, []uuid.UUID{a.ID, b.ID}, rows.singles, "Lookup is called once per live runner")
}

func TestHubRecheckAllEndsStalledSessionsTogether(t *testing.T) {
	setRecheckEvery(t, time.Hour)
	a, b := personRunner("alice"), personRunner("bob")
	a.State, b.State = types.RunnerRevoked, types.RunnerRevoked
	rows := newRows(a, b)
	hub := NewHub(t.Context(), HubConfig{LookupMany: rows.lookupMany})
	t.Cleanup(hub.Close)
	var stalls []*stallConn
	wrap := func(c runnerwire.Conn) runnerwire.Conn {
		sc := &stallConn{Conn: c, blocked: make(chan struct{}), closed: make(chan struct{})}
		stalls = append(stalls, sc)
		return sc
	}
	fa, fb := addLiveSession(t, hub, a, wrap), addLiveSession(t, hub, b, wrap)
	for _, sc := range stalls {
		sc.stalled.Store(true)
	}

	start := time.Now()
	check(t, hub.recheckAll(t.Context()), "recheckAll reported a closed Hub")
	check(t, time.Since(start) < 500*time.Millisecond, "recheckAll waited for the stalled links")
	// Each Revoke waits out its own 1 s bound, so calls run one after another would take 2 s or more.
	eventually(t, func() bool { return !fa.link.Online() && !fb.link.Online() }, 1800*time.Millisecond, "the stalled links did not end together")
	requireOffline(t, fa)
	requireOffline(t, fb)
}

func TestHubRecheckAllDoesNotWaitOnAHeldSlot(t *testing.T) {
	const every = 100 * time.Millisecond
	setRecheckEvery(t, every)
	a, b := personRunner("alice"), personRunner("bob")
	a.Limits = &types.RunnerLimits{RunsAtOnce: 1, PerRun: types.RunnerAmounts{CPUMillis: 4000, MemoryMiB: 8192}}
	rows := newRows(a, b)
	hub := NewHub(t.Context(), HubConfig{LookupMany: rows.lookupMany})
	t.Cleanup(hub.Close)
	fa, fb := addLiveSession(t, hub, a, nil), addLiveSession(t, hub, b, nil)
	stale := perRunLimits{cpuMillis: 2000, memoryMiB: 4096}
	fa.s.perRun.Store(&stale)

	// An admission in flight holds the runner's slot: A's action waits on it.
	fa.s.mu.Lock()
	var release sync.Once
	unlock := func() { release.Do(fa.s.mu.Unlock) }
	t.Cleanup(unlock)
	eventually(t, func() bool {
		hub.mu.Lock()
		defer hub.mu.Unlock()
		return hub.rechecking[fa.peer]
	}, 2*time.Second, "the first tick did not start A's action")

	b.State = types.RunnerRevoked
	rows.set(b)
	within := 2*every + 300*time.Millisecond // two periods, and a scheduling allowance for -race
	eventually(t, func() bool { return !fb.link.Online() }, within, "B's revocation waited on A's held slot")
	check(t, fa.link.Online(), "A ended while its slot was held")

	unlock()
	requireOffline(t, fa)
}

// Two live peers of one runner (a reconnect before the old session's entry is gone) are rechecked on their own: the
// old one's action still running does not skip the new one.
func TestHubRecheckAllClaimsEachPeerOfARunner(t *testing.T) {
	setRecheckEvery(t, time.Hour)
	a := personRunner("alice")
	a.State = types.RunnerRevoked
	rows := newRows(a)
	hub := NewHub(t.Context(), HubConfig{LookupMany: rows.lookupMany})
	t.Cleanup(hub.Close)
	fx := addLiveSession(t, hub, a, nil)
	orgNew, localNew := runnerwire.Loopback()
	t.Cleanup(func() { _ = localNew.Close() })
	newPeer := fx.link.AttachSession(orgNew, runnerwire.PeerConfig{})
	if err := fx.link.Ready(newPeer); err != nil {
		t.Fatal(err)
	}
	fx.s.mu.Lock()
	fx.s.peer = newPeer
	fx.s.mu.Unlock()
	hub.addLive(a.ID, fx.s, newPeer)
	check(t, hub.claimRecheck(fx.peer), "the old session's action could not be marked running")

	check(t, hub.recheckAll(t.Context()), "recheckAll reported a closed Hub")
	eventually(t, func() bool { return !newPeer.Online() }, 2*time.Second, "the new session was skipped")
	hub.releaseRecheck(fx.peer)
	waitRecheckIdle(t, hub)
}
