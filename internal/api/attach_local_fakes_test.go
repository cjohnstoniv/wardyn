// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/recording"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// localAttachStore is the attach tests' store plus the runner rows a local attach reads.
type localAttachStore struct {
	*profileListStore
	mu      sync.Mutex
	runners map[uuid.UUID]types.Runner
}

func (s *localAttachStore) GetRunner(_ context.Context, id uuid.UUID) (types.Runner, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rn, ok := s.runners[id]
	if !ok {
		return types.Runner{}, store.ErrNotFound
	}
	return rn, nil
}

// localAttachLeaseStore keeps writer leases too (attach_lease.go).
type localAttachLeaseStore struct {
	*localAttachStore
	*leaseCovLeases
}

// localThawFailStore says every run is paused and fails to clear the pause, so a thaw fails.
type localThawFailStore struct {
	*localAttachStore
	store.RunPauser // only the two methods below are reached
}

func (localThawFailStore) StampRunActive(context.Context, uuid.UUID) (bool, error) { return true, nil }
func (localThawFailStore) ClearRunPaused(context.Context, uuid.UUID) (bool, error) {
	return false, errors.New("thaw: store refused")
}

// fakeLocalSub is a runner's substrate for a local attach: its caps and the sessions it grants.
type fakeLocalSub struct {
	mu       sync.Mutex
	caps     runnerwire.Caps
	reported bool
	grantErr error
	reqs     []runner.LocalAttachRequest
	sessions []*fakeLocalSession
	ackDelay time.Duration
	ackErr   error
}

func (f *fakeLocalSub) Caps() (runnerwire.Caps, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.caps, f.reported
}

func (f *fakeLocalSub) LocalAttach(_ context.Context, _ string, req runner.LocalAttachRequest) (runner.LocalSession, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, req)
	if f.grantErr != nil {
		return nil, f.grantErr
	}
	pr, pw := io.Pipe()
	sess := &fakeLocalSession{pr: pr, pw: pw, closed: make(chan struct{}), ackDelay: f.ackDelay, ackErr: f.ackErr}
	f.sessions = append(f.sessions, sess)
	return sess, nil
}

func (f *fakeLocalSub) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.reqs)
}

func (f *fakeLocalSub) req(t *testing.T, i int) runner.LocalAttachRequest {
	t.Helper()
	waitFor(t, "the local_attach call", func() bool { return f.calls() > i })
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reqs[i]
}

func (f *fakeLocalSub) session(t *testing.T, i int) *fakeLocalSession {
	t.Helper()
	waitFor(t, "the local session", func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return len(f.sessions) > i
	})
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sessions[i]
}

// fakeLocalSession is the org's end of one granted session: pty_tee (emit is the runner writing
// it), the mode pushes and the acks the org sent.
type fakeLocalSession struct {
	pr *io.PipeReader
	pw *io.PipeWriter

	closed chan struct{}
	once   sync.Once

	mu       sync.Mutex
	modes    []bool
	acks     []int64
	ackDelay time.Duration
	ackErr   error
	onAck    func(offset int64) // runs before the ack is recorded
	modeHang atomic.Bool        // SetObserver is never acknowledged
}

func (s *fakeLocalSession) Read(p []byte) (int, error) { return s.pr.Read(p) }

func (s *fakeLocalSession) Close() error {
	s.once.Do(func() {
		close(s.closed)
		_ = s.pw.Close()
		_ = s.pr.Close()
	})
	return nil
}

func (s *fakeLocalSession) SetObserver(ctx context.Context, observer bool, _, _ uint16) error {
	if s.modeHang.Load() {
		<-ctx.Done()
		_ = s.Close() // what the remote session does with an unacknowledged change
		return ctx.Err()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.modes = append(s.modes, observer)
	return nil
}

func (s *fakeLocalSession) AckTee(ctx context.Context, offset int64) error {
	s.mu.Lock()
	delay, err, hook := s.ackDelay, s.ackErr, s.onAck
	s.mu.Unlock()
	if hook != nil {
		hook(offset)
	}
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.acks = append(s.acks, offset)
	return nil
}

// emit is the runner writing b on pty_tee; it returns once the org has read it.
func (s *fakeLocalSession) emit(t *testing.T, b []byte) {
	t.Helper()
	if _, err := s.pw.Write(b); err != nil {
		t.Fatalf("emit on pty_tee: %v", err)
	}
}

func (s *fakeLocalSession) isClosed() bool {
	select {
	case <-s.closed:
		return true
	default:
		return false
	}
}

func (s *fakeLocalSession) modeLog() []bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]bool(nil), s.modes...)
}

func (s *fakeLocalSession) ackLog() []int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]int64(nil), s.acks...)
}

// localRig is a server whose run alice owns, placed on alice's own runner, with local attach on.
type localRig struct {
	srv   *Server
	ts    *httptest.Server
	st    *localAttachStore
	sub   *fakeLocalSub
	run   types.AgentRun
	rn    types.Runner
	audit *sshTestRecorder
	casts *recording.FSStore // nil unless recorded
}

type localRigOpts struct {
	record bool                                // wire a recording store
	mask   *secretmask.Registry                // with record: the run's secrets
	leases *leaseCovLeases                     // keep writer leases
	wrap   func(*localAttachStore) store.Store // replaces the store
	cfg    func(*Config)
	run    func(*types.AgentRun)
	rn     func(*types.Runner)
}

func newLocalRig(t *testing.T, o localRigOpts) *localRig {
	t.Helper()
	ast := newAuthzStore()
	ls := &localAttachStore{profileListStore: &profileListStore{authzStore: ast}, runners: map[uuid.UUID]types.Runner{}}
	rn := types.Runner{ID: uuid.New(), Owner: holderOwner, OwnerKind: types.RunnerOwnerPerson, State: types.RunnerClaimed}
	if o.rn != nil {
		o.rn(&rn)
	}
	run := types.AgentRun{ID: uuid.New(), CreatedBy: holderOwner, State: types.RunRunning, SandboxRef: "sbx-1",
		Placement: types.PlacementLocal, RunnerID: &rn.ID}
	if o.run != nil {
		o.run(&run)
	}
	ast.runs[run.ID] = run
	ls.runners[rn.ID] = rn
	var st store.Store = ls
	switch {
	case o.wrap != nil:
		st = o.wrap(ls)
	case o.leases != nil:
		st = &localAttachLeaseStore{localAttachStore: ls, leaseCovLeases: o.leases}
	}
	rig := &localRig{st: ls, run: run, rn: rn, audit: &sshTestRecorder{}}
	cfg := baseTestConfig(newHarness(t), st)
	cfg.Audit = rig.audit
	cfg.Runner = &holderTestRunner{}
	cfg.OIDC = &oidc.Authenticator{}
	cfg.AttachLocal = true
	cfg.RunnerOnline = func(uuid.UUID) bool { return true }
	if o.record {
		fs, err := recording.NewFSStore(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		rig.casts, cfg.RecordingStore, cfg.MaskRegistry = fs, fs, o.mask
	}
	if o.cfg != nil {
		o.cfg(&cfg)
	}
	rig.srv = New(cfg)
	t.Cleanup(rig.srv.WaitBackground)
	if o.leases != nil {
		rig.srv.leaseKeeperOnce.Do(func() {})
	}
	rig.sub = &fakeLocalSub{caps: runnerwire.Caps{LocalAttach: true}, reported: true}
	rig.srv.localAttachSubs = func(uuid.UUID) localAttachSub { return rig.sub }
	rig.ts = httptest.NewServer(panicFails(t, rig.srv.Handler()))
	t.Cleanup(rig.ts.Close)
	return rig
}

func (rig *localRig) ticket(t *testing.T, principal, role string) string {
	t.Helper()
	tok, err := mintAttachTicket(context.Background(), rig.srv.cfg.Store, rig.run.ID, types.ActorHuman, principal, role, oidc.Issue{At: time.Now()})
	if err != nil {
		t.Fatalf("mint ticket: %v", err)
	}
	return tok
}

const localQuery = "&transport=local&local_nonce=nonce-1"

// dial opens alice's local attach socket.
func (rig *localRig) dial(t *testing.T) *websocket.Conn {
	t.Helper()
	c, resp, err := rig.tryDial(t, rig.ticket(t, holderOwner, oidc.RoleUser), localQuery)
	if err != nil {
		t.Fatalf("dial local attach: %v (%v)", err, resp)
	}
	return c
}

func (rig *localRig) tryDial(t *testing.T, ticket, query string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	url := "ws" + strings.TrimPrefix(rig.ts.URL, "http") + "/api/v1/runs/" + rig.run.ID.String() + "/attach?ticket=" + ticket + query
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, resp, err := websocket.Dial(ctx, url, nil)
	if err == nil {
		t.Cleanup(func() { _ = c.CloseNow() })
	}
	return c, resp, err
}

// rows is every audit row of action on the rig's run.
func (rig *localRig) rows(action string) []types.AuditEvent {
	var out []types.AuditEvent
	for _, ev := range rig.audit.snapshot() {
		if ev.Action == action && ev.RunID != nil && *ev.RunID == rig.run.ID {
			out = append(out, ev)
		}
	}
	return out
}

// detach waits for the one session.detach row and returns its data.
func (rig *localRig) detach(t *testing.T) string {
	t.Helper()
	waitFor(t, "the session.detach row", func() bool { return len(rig.rows("session.detach")) > 0 })
	rows := rig.rows("session.detach")
	if len(rows) != 1 {
		t.Fatalf("session.detach rows = %d, want 1", len(rows))
	}
	return string(rows[0].Data)
}

// drain reads c until it ends and returns the error that ended it.
func drainLocal(c *websocket.Conn) <-chan error {
	done := make(chan error, 1)
	go func() {
		for {
			if _, _, err := c.Read(context.Background()); err != nil {
				done <- err
				return
			}
		}
	}()
	return done
}
