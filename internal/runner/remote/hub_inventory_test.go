// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package remote

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/federation"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

var errFencedInventory = errors.New("inventory session fenced by a newer BeginInventory")

// inventoryStore models the H3 store contract: BeginInventory replaces the
// runner's single current inventory and pages for any other session refuse.
type inventoryStore struct {
	mu        sync.Mutex
	current   string
	log       []string
	begins    []string
	pages     map[string][]runnerwire.StatePage
	link      *Link
	beginErr  error
	refuseAt  int
	pageErr   error
	pageCalls int
	// stateFrames counts raw STATE frames the org read, valid or not.
	stateFrames int
}

func newInventoryStore(link *Link) *inventoryStore {
	return &inventoryStore{link: link, pages: map[string][]runnerwire.StatePage{}}
}

func (f *inventoryStore) record(entry string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log = append(f.log, entry)
}

func (f *inventoryStore) snapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.log)
}

func (f *inventoryStore) beginsSnapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.begins)
}

func (f *inventoryStore) sessionPages(session string) []runnerwire.StatePage {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.pages[session])
}

func (f *inventoryStore) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pageCalls
}

func (f *inventoryStore) framesRead() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stateFrames
}

func (f *inventoryStore) begin(_ context.Context, session string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log = append(f.log, "begin:"+session)
	if f.beginErr != nil {
		return f.beginErr
	}
	f.begins = append(f.begins, session)
	f.current = session
	return nil
}

func (f *inventoryStore) pending(context.Context) ([]runnerwire.PendingAction, error) {
	f.record("pending")
	return nil, nil
}

func (f *inventoryStore) page(_ context.Context, session string, page runnerwire.StatePage) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pageCalls++
	if session != f.current {
		return errFencedInventory
	}
	if f.link != nil && f.link.Online() {
		return errors.New("STATE page arrived after READY admitted dispatch")
	}
	if f.pageErr != nil && f.pageCalls == f.refuseAt {
		return f.pageErr
	}
	f.log = append(f.log, fmt.Sprintf("page:%s:%d:%t", session, len(f.pages[session]), page.Final))
	f.pages[session] = append(f.pages[session], page)
	return nil
}

func (f *inventoryStore) admission(sub *Substrate, link *Link) Admission {
	return Admission{Substrate: sub, Link: link, Pending: f.pending, BeginInventory: f.begin, StatePage: f.page,
		Event: func(context.Context, runnerwire.Event) error { return nil }}
}

// recordingConn logs the org's own AUTH_OK and PENDING writes into the store's
// order so callback order is checked against the wire, not timing.
type recordingConn struct {
	runnerwire.Conn
	store *inventoryStore
}

func (c recordingConn) ReadFrame(ctx context.Context) (runnerwire.Frame, error) {
	f, err := c.Conn.ReadFrame(ctx)
	if err == nil && f.Type == runnerwire.TypeState {
		c.store.mu.Lock()
		c.store.stateFrames++
		c.store.mu.Unlock()
	}
	return f, err
}

func (c recordingConn) WriteFrame(ctx context.Context, f runnerwire.Frame) error {
	if f.Type == runnerwire.TypeAuthOK || f.Type == runnerwire.TypePending {
		c.store.record("write:" + f.Type.String())
	}
	return c.Conn.WriteFrame(ctx, f)
}

type inventoryFixture struct {
	ctx     context.Context
	row     types.Runner
	key     ed25519.PrivateKey
	orgURL  string
	link    *Link
	store   *inventoryStore
	hub     *Hub
	hubDone chan error
	dial    func() runnerwire.Conn
}

func newInventoryFixture(t *testing.T, realWire bool) *inventoryFixture {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	t.Cleanup(cancel)
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	f := &inventoryFixture{ctx: ctx, key: key, orgURL: "https://org.example", link: &Link{}, hubDone: make(chan error, 8)}
	f.row = types.Runner{ID: uuid.New(), Owner: "owner", PublicKey: pub, State: types.RunnerClaimed}
	f.store = newInventoryStore(f.link)
	sub := New(f.row.ID.String(), f.link, Options{})
	config := HubConfig{Lookup: func(context.Context, uuid.UUID) (types.Runner, error) { return f.row, nil },
		Admit: func(context.Context, types.Runner) (Admission, error) { return f.store.admission(sub, f.link), nil }}
	if realWire {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			conn, err := websocket.Accept(w, r, nil)
			if err == nil {
				err = f.hub.Serve(ctx, recordingConn{runnerwire.NewWebSocket(conn), f.store}, f.row.ID)
			}
			f.hubDone <- err
		}))
		t.Cleanup(server.Close)
		f.orgURL = server.URL
	}
	config.OrgURL = f.orgURL
	f.row.OrgURLSHA256 = federation.OrgURLSHA256(f.orgURL)
	f.hub = NewHub(ctx, config)
	t.Cleanup(f.hub.Close)
	f.dial = func() runnerwire.Conn {
		if realWire {
			conn, err := runnerwire.DialWebSocket(ctx, f.orgURL, f.row.ID.String(), nil)
			if err != nil {
				t.Fatal(err)
			}
			return conn
		}
		org, run := runnerwire.Loopback()
		go func() { f.hubDone <- f.hub.Serve(ctx, recordingConn{org, f.store}, f.row.ID) }()
		return run
	}
	return f
}

// open authenticates a raw runner connection and consumes PENDING.
func (f *inventoryFixture) open(t *testing.T, resume *runnerwire.Resume) (runnerwire.Conn, runnerwire.AuthOK, uint64) {
	t.Helper()
	conn := f.dial()
	t.Cleanup(func() { _ = conn.Close() })
	ok, err := runnerwire.AuthenticateOrg(f.ctx, conn, f.row.ID.String(), f.orgURL, f.key, resume)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := conn.ReadFrame(f.ctx)
	if err != nil || pending.Type != runnerwire.TypePending {
		t.Fatal("missing PENDING", pending.Type, err)
	}
	return conn, ok, pending.Seq
}

func orderedRuns(n int) []runnerwire.RunState {
	runs := make([]runnerwire.RunState, n)
	for i := range runs {
		runs[i].RunID = uuid.New()
	}
	slices.SortFunc(runs, func(a, b runnerwire.RunState) int { return bytes.Compare(a.RunID[:], b.RunID[:]) })
	return runs
}

// inventoryPages splits runs into maximal ordered pages of one inventory.
func inventoryPages(runs []runnerwire.RunState) []runnerwire.StatePage {
	id, after := uuid.New(), uuid.Nil
	var pages []runnerwire.StatePage
	for {
		n := min(len(runs), runnerwire.StatePageRows)
		pages = append(pages, runnerwire.StatePage{InventoryID: id, After: after, Runs: runs[:n], Final: n == len(runs)})
		if n == len(runs) {
			return pages
		}
		after, runs = runs[n-1].RunID, runs[n:]
	}
}

func writeFrames(t *testing.T, ctx context.Context, conn runnerwire.Conn, seq uint64, frames ...runnerwire.Frame) {
	t.Helper()
	for _, f := range frames {
		seq++
		f.Seq = seq
		if err := conn.WriteFrame(ctx, f); err != nil {
			t.Fatal(err)
		}
	}
}

func stateFrame(t *testing.T, page runnerwire.StatePage) runnerwire.Frame {
	t.Helper()
	payload, err := runnerwire.Marshal(page)
	if err != nil {
		t.Fatal(err)
	}
	return runnerwire.Frame{Type: runnerwire.TypeState, Payload: payload}
}

func eachTransport(t *testing.T, run func(*testing.T, bool)) {
	for _, realWire := range []bool{false, true} {
		name := "loopback"
		if realWire {
			name = "websocket"
		}
		t.Run(name, func(t *testing.T) { run(t, realWire) })
	}
}

func TestHubInventoryBeginsBeforePendingAndEveryPagePrecedesReady(t *testing.T) {
	eachTransport(t, func(t *testing.T, realWire bool) {
		f := newInventoryFixture(t, realWire)
		runs := orderedRuns(2*runnerwire.StatePageRows + 1)
		online, resumed := make(chan *runnerwire.Peer, 1), make(chan bool, 1)
		hooks := sessionHooks(online, resumed)
		var emitted int
		hooks.StatePages = func(_ context.Context, send func(runnerwire.StatePage) error) error {
			for _, page := range inventoryPages(runs) {
				if err := send(page); err != nil {
					return err
				}
				emitted++
			}
			return nil
		}
		client, err := runnerwire.NewClient(runnerwire.ClientConfig{OrgURL: f.orgURL, RunnerID: f.row.ID.String(), PrivateKey: f.key, Hooks: hooks,
			Dial: func(context.Context) (runnerwire.Conn, error) { return f.dial(), nil }})
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- client.Connect(f.ctx) }()
		peer := awaitValue(t, f.ctx, online)
		waitOnline(t, f.ctx, f.link)
		if emitted != 3 {
			t.Fatalf("runner emitted %d pages, want 3 bounded pages", emitted)
		}
		session := f.store.beginsSnapshot()[0]
		want := []string{"begin:" + session, "pending", "write:AUTH_OK", "write:PENDING",
			"page:" + session + ":0:false", "page:" + session + ":1:false", "page:" + session + ":2:true"}
		if got := f.store.snapshot(); !slices.Equal(got, want) {
			t.Fatalf("admission order\n got %q\nwant %q", got, want)
		}
		peer.Close()
		awaitValue(t, f.ctx, done)
	})
}

func TestHubInventoryPartialPagesNeverReadyAndRestartFresh(t *testing.T) {
	eachTransport(t, func(t *testing.T, realWire bool) {
		f := newInventoryFixture(t, realWire)
		pages := inventoryPages(orderedRuns(runnerwire.StatePageRows + 1))

		// Disconnect mid-stream: one durable page, then the link drops.
		first, ok1, seq := f.open(t, nil)
		writeFrames(t, f.ctx, first, 0, stateFrame(t, pages[0]))
		_ = first.Close()
		awaitValue(t, f.ctx, f.hubDone)
		if f.link.Online() {
			t.Fatal("partial inventory admitted dispatch")
		}

		// A resume request for the incomplete session is a fresh inventory.
		second, ok2, _ := f.open(t, &runnerwire.Resume{SessionID: ok1.SessionID, LastSeq: seq})
		if ok2.Resumed || ok2.SessionID == ok1.SessionID {
			t.Fatal("incomplete inventory session was resumed", ok2)
		}
		repeat := inventoryPages(orderedRuns(runnerwire.StatePageRows + 1))
		writeFrames(t, f.ctx, second, 0, stateFrame(t, repeat[0]), runnerwire.Frame{Type: runnerwire.TypeReady})
		if err := awaitValue(t, f.ctx, f.hubDone); !errors.Is(err, runnerwire.ErrBadFrame) {
			t.Fatal("READY before Final was not refused", err)
		}
		if f.link.Online() {
			t.Fatal("READY before Final admitted dispatch")
		}

		third, ok3, _ := f.open(t, &runnerwire.Resume{SessionID: ok2.SessionID, LastSeq: seq})
		if ok3.Resumed {
			t.Fatal("refused READY session was resumed")
		}
		final := inventoryPages(orderedRuns(runnerwire.StatePageRows + 1))
		writeFrames(t, f.ctx, third, 0, stateFrame(t, final[0]), stateFrame(t, final[1]), runnerwire.Frame{Type: runnerwire.TypeReady})
		waitOnline(t, f.ctx, f.link)
		if want := []string{ok1.SessionID, ok2.SessionID, ok3.SessionID}; !slices.Equal(f.store.beginsSnapshot(), want) {
			t.Fatalf("every fresh session must begin its own inventory: %q want %q", f.store.beginsSnapshot(), want)
		}
		if got := f.store.sessionPages(ok3.SessionID); len(got) != 2 || !got[1].Final || got[0].InventoryID != final[0].InventoryID {
			t.Fatalf("final session pages %+v", got)
		}
		_ = third.Close()
		awaitValue(t, f.ctx, f.hubDone)
	})
}

func TestHubInventoryFreshSessionFencesOldSessionPage(t *testing.T) {
	store := newInventoryStore(nil)
	h := &Hub{cfg: HubConfig{}}
	s := &hubSession{}
	a := store.admission(nil, nil)
	if _, err := h.fresh(t.Context(), s, a, uuid.New()); err != nil {
		t.Fatal(err)
	}
	var old, current runnerwire.PeerConfig
	configureAdmission(t.Context(), &old, false, nil, a, s, s.id, func() *runnerwire.Peer { return nil })
	oldID := s.id
	if _, err := h.fresh(t.Context(), s, a, uuid.New()); err != nil {
		t.Fatal(err)
	}
	configureAdmission(t.Context(), &current, false, nil, a, s, s.id, func() *runnerwire.Peer { return nil })
	page := inventoryPages(orderedRuns(1))[0]
	if err := old.OnControlChecked(stateFrame(t, page)); !errors.Is(err, errFencedInventory) {
		t.Fatal("superseded session's first page reached the newer inventory", err)
	}
	if len(store.pages[oldID]) != 0 || len(store.pages[s.id]) != 0 {
		t.Fatal("fenced page was recorded", store.pages)
	}
	if err := old.OnControlChecked(runnerwire.Frame{Type: runnerwire.TypeReady}); !errors.Is(err, runnerwire.ErrBadFrame) {
		t.Fatal("fenced session reached READY", err)
	}
	if err := current.OnControlChecked(stateFrame(t, page)); err != nil || len(store.pages[s.id]) != 1 {
		t.Fatal("current session page refused", err)
	}
}

func TestHubInventoryPageBoundsRefuseBeforeCallback(t *testing.T) {
	runs := orderedRuns(runnerwire.StatePageRows + 1)
	id := uuid.New()
	reversed := []runnerwire.RunState{runs[1], runs[0]}
	aggregate, _ := runnerwire.Marshal(runs[:1])
	for name, payload := range map[string][]byte{
		"over row bound":  stateFrame(t, runnerwire.StatePage{InventoryID: id, Runs: runs, Final: true}).Payload,
		"unordered rows":  stateFrame(t, runnerwire.StatePage{InventoryID: id, Runs: reversed, Final: true}).Payload,
		"wrong after":     stateFrame(t, runnerwire.StatePage{InventoryID: id, After: runs[0].RunID, Runs: runs[1:2], Final: true}).Payload,
		"nil inventory":   stateFrame(t, runnerwire.StatePage{Runs: runs[:1], Final: true}).Payload,
		"empty non-final": stateFrame(t, runnerwire.StatePage{InventoryID: id}).Payload,
		"aggregate STATE": aggregate,
		"null":            []byte("null"),
	} {
		t.Run(name, func(t *testing.T) {
			store := newInventoryStore(nil)
			store.current = "session"
			var cfg runnerwire.PeerConfig
			configureAdmission(t.Context(), &cfg, false, nil, store.admission(nil, nil), &hubSession{}, "session", func() *runnerwire.Peer { return nil })
			if err := cfg.OnControlChecked(runnerwire.Frame{Type: runnerwire.TypeState, Payload: payload}); !errors.Is(err, runnerwire.ErrBadFrame) {
				t.Fatal("invalid page accepted", err)
			}
			if store.pageCalls != 0 {
				t.Fatal("invalid page reached durable intake")
			}
			if err := cfg.OnControlChecked(runnerwire.Frame{Type: runnerwire.TypeReady}); !errors.Is(err, runnerwire.ErrBadFrame) {
				t.Fatal("READY after refused page", err)
			}
		})
	}
}

func TestHubInventoryOverBoundPageClosesSessionOnWire(t *testing.T) {
	eachTransport(t, func(t *testing.T, realWire bool) {
		f := newInventoryFixture(t, realWire)
		conn, _, _ := f.open(t, nil)
		page := runnerwire.StatePage{InventoryID: uuid.New(), Runs: orderedRuns(runnerwire.StatePageRows + 1), Final: true}
		writeFrames(t, f.ctx, conn, 0, stateFrame(t, page), runnerwire.Frame{Type: runnerwire.TypeReady})
		if err := awaitValue(t, f.ctx, f.hubDone); !errors.Is(err, runnerwire.ErrBadFrame) {
			t.Fatal("over-bound page did not close the session", err)
		}
		if f.store.calls() != 0 || f.link.Online() {
			t.Fatal("over-bound page reached intake or READY")
		}
	})
}

func TestHubInventoryBeginRefusalPrecedesAuthOK(t *testing.T) {
	eachTransport(t, func(t *testing.T, realWire bool) {
		f := newInventoryFixture(t, realWire)
		f.store.beginErr = errors.New("inventory capture failed")
		conn := f.dial()
		defer conn.Close()
		_, err := runnerwire.AuthenticateOrg(f.ctx, conn, f.row.ID.String(), f.orgURL, f.key, nil)
		if hubErr := awaitValue(t, f.ctx, f.hubDone); !errors.Is(hubErr, f.store.beginErr) {
			t.Fatal("Begin refusal did not end the connection", hubErr)
		}
		if err == nil {
			t.Fatal("runner received AUTH_OK after inventory refusal")
		}
		if got := f.store.snapshot(); len(got) != 1 || f.link.Online() {
			t.Fatalf("refused inventory continued admission: %q", got)
		}
		f.store.beginErr = nil
		next, ok, _ := f.open(t, nil)
		writeFrames(t, f.ctx, next, 0, stateFrame(t, runnerwire.StatePage{InventoryID: uuid.New(), Final: true}), runnerwire.Frame{Type: runnerwire.TypeReady})
		waitOnline(t, f.ctx, f.link)
		if begins := f.store.beginsSnapshot(); len(begins) != 1 || begins[0] != ok.SessionID {
			t.Fatal("recovery did not begin a fresh inventory", begins)
		}
		_ = next.Close()
		awaitValue(t, f.ctx, f.hubDone)
	})
}

func TestHubInventoryPageRefusalForcesFreshReconnect(t *testing.T) {
	eachTransport(t, func(t *testing.T, realWire bool) {
		f := newInventoryFixture(t, realWire)
		f.store.pageErr, f.store.refuseAt = errors.New("reconciliation required"), 2
		pages := inventoryPages(orderedRuns(runnerwire.StatePageRows + 1))
		conn, ok, seq := f.open(t, nil)
		writeFrames(t, f.ctx, conn, 0, stateFrame(t, pages[0]), stateFrame(t, pages[1]), runnerwire.Frame{Type: runnerwire.TypeReady})
		if err := awaitValue(t, f.ctx, f.hubDone); !errors.Is(err, f.store.pageErr) {
			t.Fatal("Final page refusal did not close admission", err)
		}
		if f.link.Online() || f.link.Call(f.ctx, "probe", nil, nil) == nil {
			t.Fatal("refused inventory advertised readiness")
		}
		_, again, _ := f.open(t, &runnerwire.Resume{SessionID: ok.SessionID, LastSeq: seq})
		if begins := f.store.beginsSnapshot(); again.Resumed || len(begins) != 2 || begins[1] != again.SessionID {
			t.Fatal("refused inventory resumed instead of beginning fresh", again, begins)
		}
	})
}

func TestClientRefusesReadyWithoutFinalPage(t *testing.T) {
	f := newInventoryFixture(t, false)
	online, resumed := make(chan *runnerwire.Peer, 1), make(chan bool, 1)
	hooks := sessionHooks(online, resumed)
	hooks.StatePages = func(_ context.Context, send func(runnerwire.StatePage) error) error {
		return send(inventoryPages(orderedRuns(runnerwire.StatePageRows + 1))[0])
	}
	client, err := runnerwire.NewClient(runnerwire.ClientConfig{OrgURL: f.orgURL, RunnerID: f.row.ID.String(), PrivateKey: f.key, Hooks: hooks,
		Dial: func(context.Context) (runnerwire.Conn, error) { return f.dial(), nil }})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Connect(f.ctx); !errors.Is(err, runnerwire.ErrBadFrame) {
		t.Fatal("client sent READY after an incomplete inventory", err)
	}
	if err := awaitValue(t, f.ctx, f.hubDone); errors.Is(err, runnerwire.ErrBadFrame) {
		t.Fatal("org received a premature READY", err)
	}
	if f.link.Online() {
		t.Fatal("incomplete inventory admitted dispatch")
	}
}

// A READY earlier session must not make the next fresh, incomplete one resumable.
func TestHubInventoryEarlierReadyCannotResumeIncompleteFreshSession(t *testing.T) {
	eachTransport(t, func(t *testing.T, realWire bool) {
		f := newInventoryFixture(t, realWire)
		a, _, _ := f.open(t, nil)
		writeFrames(t, f.ctx, a, 0, stateFrame(t, inventoryPages(orderedRuns(1))[0]), runnerwire.Frame{Type: runnerwire.TypeReady})
		waitOnline(t, f.ctx, f.link)
		_ = a.Close()
		awaitValue(t, f.ctx, f.hubDone)

		b, okB, seqB := f.open(t, nil)
		writeFrames(t, f.ctx, b, 0, stateFrame(t, inventoryPages(orderedRuns(runnerwire.StatePageRows + 1))[0]))
		_ = b.Close()
		awaitValue(t, f.ctx, f.hubDone)

		c := f.dial()
		t.Cleanup(func() { _ = c.Close() })
		okC, err := runnerwire.AuthenticateOrg(f.ctx, c, f.row.ID.String(), f.orgURL, f.key, &runnerwire.Resume{SessionID: okB.SessionID, LastSeq: seqB})
		if err != nil {
			t.Fatal(err)
		}
		if okC.Resumed || f.link.Online() {
			t.Fatalf("incomplete fresh session resumed on an earlier READY: resumed=%v online=%v", okC.Resumed, f.link.Online())
		}
	})
}

func TestClientValidatesPagesBeforeSending(t *testing.T) {
	runs := orderedRuns(2)
	id := uuid.New()
	final := runnerwire.StatePage{InventoryID: id, Final: true}
	unordered := runnerwire.StatePage{InventoryID: id, Runs: []runnerwire.RunState{runs[1], runs[0]}, Final: true}
	for name, tc := range map[string]struct {
		pages   []runnerwire.StatePage
		discard bool
		sent    int
	}{
		"unordered rows":     {pages: []runnerwire.StatePage{unordered}},
		"wrong after":        {pages: []runnerwire.StatePage{{InventoryID: id, After: runs[0].RunID, Runs: runs[1:], Final: true}}},
		"page after final":   {pages: []runnerwire.StatePage{final, {InventoryID: id, Runs: runs[:1], Final: true}}, discard: true, sent: 1},
		"discarded refusal":  {pages: []runnerwire.StatePage{unordered}, discard: true},
		"oversized row page": {pages: []runnerwire.StatePage{{InventoryID: id, Runs: orderedRuns(runnerwire.StatePageRows + 1), Final: true}}},
	} {
		t.Run(name, func(t *testing.T) {
			eachTransport(t, func(t *testing.T, realWire bool) {
				f := newInventoryFixture(t, realWire)
				online, resumed := make(chan *runnerwire.Peer, 1), make(chan bool, 1)
				hooks := sessionHooks(online, resumed)
				hooks.StatePages = func(_ context.Context, send func(runnerwire.StatePage) error) error {
					for _, page := range tc.pages {
						if err := send(page); err != nil && !tc.discard {
							return err
						}
					}
					return nil
				}
				client, err := runnerwire.NewClient(runnerwire.ClientConfig{OrgURL: f.orgURL, RunnerID: f.row.ID.String(), PrivateKey: f.key, Hooks: hooks,
					Dial: func(context.Context) (runnerwire.Conn, error) { return f.dial(), nil }})
				if err != nil {
					t.Fatal(err)
				}
				if err := client.Connect(f.ctx); !errors.Is(err, runnerwire.ErrBadFrame) {
					t.Fatal("client accepted an invalid inventory", err)
				}
				awaitValue(t, f.ctx, f.hubDone)
				if got := f.store.framesRead(); got != tc.sent || f.link.Online() {
					t.Fatalf("org read %d STATE frames (want %d), online=%v", got, tc.sent, f.link.Online())
				}
			})
		})
	}
}
