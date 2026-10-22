// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pkg/sftp"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	folderrunner "github.com/cjohnstoniv/wardyn/internal/runner/folders"
	"github.com/cjohnstoniv/wardyn/internal/runner/remote"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// folderRunnerEndpoint serves a folder-only runner (internal/runner/folders) with roots over the
// in-memory loopback and returns the org's endpoint for it, as a connected runner has one.
func folderRunnerEndpoint(t *testing.T, id uuid.UUID, shares bool, roots ...string) runnerEndpoint {
	t.Helper()
	fs, err := folderrunner.New(context.Background(), roots, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	org, run := runnerwire.Loopback()
	link := &remote.Link{}
	sub := remote.New(id.String(), link, remote.Options{})
	orgPeer := link.Attach(ctx, org)
	var peer *runnerwire.Peer
	peer = runnerwire.NewPeer(run, runnerwire.PeerConfig{OnCall: func(ctx context.Context, cid uint32, method string, args json.RawMessage) (any, error) {
		return fs.Call(ctx, peer, cid, method, args)
	}})
	go func() { _ = peer.Run(ctx) }()
	t.Cleanup(func() { orgPeer.Close(); peer.Close() })
	data, _ := json.Marshal(runnerwire.Caps{Folders: shares, MachineName: "alice-laptop"})
	if err := peer.Event(ctx, runnerwire.Event{Kind: runnerwire.EventCaps, Data: data}); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		if _, ok := sub.Caps(); ok {
			return runnerEndpoint{sub: sub, link: link}
		}
		if time.Now().After(deadline) {
			t.Fatal("the runner's caps never arrived")
		}
	}
}

// laptopHome is a home folder with a project, a credential folder and a link out of it.
func laptopHome(t *testing.T) (home, app string) {
	t.Helper()
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	app = filepath.Join(home, "code", "my-app")
	for p, body := range map[string]string{
		filepath.Join(app, "main.go"):             "package main\n",
		filepath.Join(app, "README.md"):           "hello",
		filepath.Join(home, ".ssh", "id_ed25519"): "secret",
	} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("/etc", filepath.Join(home, "code", "etc")); err != nil {
		t.Fatal(err)
	}
	return home, app
}

func TestListMyRunnerFolders_StatesTheNewRunTileShows(t *testing.T) {
	srv, f, _ := newRunnerRegistrationServer(t)
	home, app := laptopHome(t)
	mine := f.addRunner(srv, "alice", "laptop", types.RunnerClaimed, time.Hour)
	old := f.addRunner(srv, "alice", "old-laptop", types.RunnerClaimed, time.Hour)
	away := f.addRunner(srv, "alice", "desk", types.RunnerClaimed, time.Hour)
	bobs := f.addRunner(srv, "bob", "bobs", types.RunnerClaimed, time.Hour)
	srv.runnerStreams.subs = map[uuid.UUID]runnerEndpoint{
		mine.ID: folderRunnerEndpoint(t, mine.ID, true, home),
		old.ID:  folderRunnerEndpoint(t, old.ID, false, home),
		bobs.ID: folderRunnerEndpoint(t, bobs.ID, true, home),
	}
	srv.cfg.RunnerOnline = func(id uuid.UUID) bool { return id != away.ID }
	alice := memberModeSSOSession(t, "alice", "alice@example.com", oidc.RoleUser, false)
	get := func(id uuid.UUID, q string) (int, errorBody, computerFolderView) {
		w := doSSO(t, srv, http.MethodGet, "/api/v1/me/runners/"+id.String()+"/folders"+q, alice, "")
		var e errorBody
		var v computerFolderView
		_ = json.Unmarshal(w.Body.Bytes(), &e)
		_ = json.Unmarshal(w.Body.Bytes(), &v)
		return w.Code, e, v
	}

	if code, e, _ := get(mine.ID, ""); code != http.StatusForbidden || e.Reason != reasonComputerFoldersOff {
		t.Fatalf("gate off: %d %+v, want 403 computer_folders_off", code, e)
	}
	srv.cfg.ComputerFolders = true
	for name, c := range map[string]struct {
		id     uuid.UUID
		code   int
		reason string
	}{
		"someone else's runner": {bobs.ID, http.StatusNotFound, reasonRunnerNotFound},
		"an unknown id":         {uuid.New(), http.StatusNotFound, reasonRunnerNotFound},
		"offline":               {away.ID, http.StatusConflict, reasonComputerRunnerOffline},
		"an older runner":       {old.ID, http.StatusConflict, reasonComputerRunnerNoFolders},
	} {
		if code, e, _ := get(c.id, ""); code != c.code || e.Reason != c.reason {
			t.Errorf("%s: %d %+v, want %d %s", name, code, e, c.code, c.reason)
		}
	}
	code, _, v := get(mine.ID, "?path="+filepath.Join(home, "code"))
	if code != http.StatusOK || len(v.Folders) != 1 || v.Folders[0].Name != "my-app" || v.Folders[0].Files != 2 || v.Runner != "laptop" {
		t.Fatalf("listing: %d %+v, want my-app only (no link out) with 2 files", code, v)
	}
	if code, _, v := get(mine.ID, "?survey=1&path="+app); code != http.StatusOK || v.Survey == nil || v.Survey.Files != 2 {
		t.Fatalf("survey: %d %+v", code, v)
	}
	for _, p := range []string{filepath.Join(home, ".ssh"), filepath.Join(home, "code", "etc"), "/etc", filepath.Dir(home)} {
		if code, e, _ := get(mine.ID, "?path="+p); code != http.StatusUnprocessableEntity || e.Reason != reasonComputerFolderRefused {
			t.Errorf("path %s: %d %+v, want 422 computer_folder_refused", p, code, e)
		}
	}
}

// computerFolderStore is localFolderStore plus a runner row and the folder's pass records.
type computerFolderStore struct {
	*localFolderStore
	runner types.Runner
	smu    sync.Mutex
	status types.RunLocalFolderStatus
}

func (s *computerFolderStore) GetRunner(_ context.Context, id uuid.UUID) (types.Runner, error) {
	if id != s.runner.ID {
		return types.Runner{}, store.ErrNotFound
	}
	return s.runner, nil
}

// The status reads and writes fail on a done ctx, as Postgres does.
func (s *computerFolderStore) RunLocalFolderStatus(ctx context.Context, _ uuid.UUID) (types.RunLocalFolderStatus, error) {
	if err := ctx.Err(); err != nil {
		return types.RunLocalFolderStatus{}, err
	}
	s.smu.Lock()
	defer s.smu.Unlock()
	return s.status, nil
}

func (s *computerFolderStore) SetRunLocalFolderStatus(ctx context.Context, _ uuid.UUID, st types.RunLocalFolderStatus) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	s.smu.Lock()
	defer s.smu.Unlock()
	s.status = st
	return true, nil
}

// folderSandboxRunner is execCountRunner whose ExecStream runs an in-memory sftp-server, the sandbox's.
type folderSandboxRunner struct {
	*execCountRunner
	files   sftp.Handlers
	stopped func()
	// hang makes the sandbox's sftp-server never answer, so a pass runs out of time.
	hang bool
	// block is a file whose read never answers until its session closes, so a pass that reaches
	// it runs out of time after the files before it.
	block string
	// proxyErr fails StopProxy. proxyStopped and agentKilled are a kept sandbox's quiesce: its
	// proxy removed and its agent's processes killed.
	proxyErr                  error
	proxyStopped, agentKilled atomic.Bool
	// killErr fails the agent kill's exec; agentLives keeps the agent running through it; ended is
	// a sandbox already stopped, as a lease end's EndSandbox leaves it.
	killErr           error
	agentLives, ended bool
	// proxyEnds is Docker's StopProxy failing closed: it stops the agent's container and still
	// returns proxyErr.
	proxyEnds bool
	// onPass runs as a pass's sftp-server starts in the sandbox's folder.
	onPass func()
}

// StopProxy removes the sandbox's proxy, its agent's only way out.
func (r *folderSandboxRunner) StopProxy(context.Context, string) error {
	if r.proxyErr != nil {
		r.ended = r.ended || r.proxyEnds
		return r.proxyErr
	}
	r.proxyStopped.Store(true)
	return nil
}

// AgentStatus is the agent's exec: running until keptAgentKill reaches the sandbox.
func (r *folderSandboxRunner) AgentStatus(context.Context, string, string) (runner.Status, error) {
	if r.ended || r.agentKilled.Load() && !r.agentLives {
		return runner.Status{State: types.RunFailed}, nil
	}
	return runner.Status{State: types.RunRunning}, nil
}

// Status is the sandbox's container: running unless ended.
func (r *folderSandboxRunner) Status(context.Context, string) (runner.Status, error) {
	if r.ended {
		return runner.Status{State: types.RunStopped}, nil
	}
	return runner.Status{State: types.RunRunning}, nil
}

// errNoEgress is an agent's connection refused: with its proxy gone it has no route out.
var errNoEgress = errors.New("no route: the sandbox has no proxy")

// agentEgress is the agent dialling out through its proxy.
func (r *folderSandboxRunner) agentEgress() error {
	if r.proxyStopped.Load() {
		return errNoEgress
	}
	return nil
}

// blockingGet is a sandbox's file reads where the read of block waits for release. A pass's
// sftp-server is its own process, so a pass that ends with a file open leaves that file whole for
// the next: the read hides the in-memory file's TransferError, which would poison it for good.
type blockingGet struct {
	sftp.FileReader
	block   string
	release <-chan struct{}
}

func (b blockingGet) Fileread(r *sftp.Request) (io.ReaderAt, error) {
	if path.Base(r.Filepath) == b.block {
		<-b.release
	}
	ra, err := b.FileReader.Fileread(r)
	return struct{ io.ReaderAt }{ra}, err
}

func (r *folderSandboxRunner) ExecStream(_ context.Context, _ string, spec runner.ExecSpec) (*runner.ExecSession, error) {
	if r.ended {
		return nil, errors.New("container is not running")
	}
	if slices.Equal(spec.Argv, keptAgentKill) {
		if r.killErr != nil {
			return nil, r.killErr
		}
		r.agentKilled.Store(true)
		return &runner.ExecSession{
			Stdin: nopWriteCloser{io.Discard}, Stdout: strings.NewReader(""), Stderr: strings.NewReader(""),
			Wait: func() (int, error) { return 0, nil }, Close: func() error { return nil },
		}, nil
	}
	if slices.Contains(spec.Argv, "-d") && r.onPass != nil {
		r.onPass()
	}
	if r.hang {
		stdout, quiet := io.Pipe()
		return &runner.ExecSession{
			Stdin: nopWriteCloser{io.Discard}, Stdout: stdout, Stderr: strings.NewReader(""),
			Wait:  func() (int, error) { return 0, nil },
			Close: func() error { return quiet.Close() },
		}, nil
	}
	dir := "/"
	for i, a := range spec.Argv {
		if a == "-d" && i+1 < len(spec.Argv) {
			dir = spec.Argv[i+1]
		}
	}
	toServer, stdin := io.Pipe()
	stdout, fromServer := io.Pipe()
	files, release := r.files, make(chan struct{})
	files.FileGet = blockingGet{FileReader: r.files.FileGet, block: r.block, release: release}
	srv := sftp.NewRequestServer(struct {
		io.Reader
		io.WriteCloser
	}{toServer, fromServer}, files, sftp.WithStartDirectory(dir))
	done := make(chan struct{})
	go func() { _ = srv.Serve(); _ = fromServer.Close(); close(done) }()
	var closed sync.Once
	return &runner.ExecSession{
		Stdin: stdin, Stdout: stdout, Stderr: strings.NewReader(""),
		Wait: func() (int, error) { <-done; return 0, nil },
		Close: func() error {
			closed.Do(func() { close(release) })
			_ = stdin.Close()
			_ = srv.Close()
			return nil
		},
	}, nil
}

// StopSandbox fails on a done ctx, as the Docker client does.
func (r *folderSandboxRunner) StopSandbox(ctx context.Context, ref string) error {
	if r.stopped != nil {
		r.stopped()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return r.execCountRunner.StopSandbox(ctx, ref)
}

func (r *folderSandboxRunner) client(t *testing.T) *sftp.Client {
	t.Helper()
	sess, _ := r.ExecStream(context.Background(), "", runner.ExecSpec{})
	c, err := sftp.NewClientPipe(sess.Stdout, sess.Stdin)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sess.Close(); _ = c.Close() })
	return c
}

func computerFolderFixture(t *testing.T, direction string) (*Server, *computerFolderStore, *recRecorder, types.AgentRun, *folderSandboxRunner, string) {
	t.Helper()
	s, ls, audit, run, rn := localFolderFixture(t)
	sb := &folderSandboxRunner{execCountRunner: rn, files: sftp.InMemHandler()}
	s.cfg.Runner = sb
	if err := sb.client(t).MkdirAll("/home/agent/work"); err != nil {
		t.Fatal(err)
	}
	home, app := laptopHome(t)
	row := types.Runner{ID: uuid.New(), Owner: run.CreatedBy, Name: "laptop", State: types.RunnerClaimed}
	id := row.ID
	ls.folder = &types.RunLocalFolder{Name: "my-app", RemoteDir: "/home/agent/work", RunnerID: &id, Path: app, Direction: direction}
	ls.run.AgentExecID = "agent-exec"
	cs := &computerFolderStore{localFolderStore: ls, runner: row}
	s.cfg.Store = cs
	s.cfg.ComputerFolders = true
	s.cfg.LocalFolderSeedWait = 20 * time.Second
	s.cfg.RunnerOnline = func(uuid.UUID) bool { return true }
	s.runnerStreams.subs = map[uuid.UUID]runnerEndpoint{id: folderRunnerEndpoint(t, id, true, home)}
	return s, cs, audit, run, sb, app
}

func TestHoldForLocalFolder_SeedsAComputerFolderThroughItsRunner(t *testing.T) {
	s, cs, audit, run, sb, _ := computerFolderFixture(t, types.ComputerFolderBoth)
	if !s.holdForLocalFolder(context.Background(), run, "sbx") {
		t.Fatalf("the hold refused the start: %q", cs.FailureHint())
	}
	f, err := sb.client(t).Open("/home/agent/work/main.go")
	if err != nil {
		t.Fatalf("the folder never reached the sandbox: %v", err)
	}
	b, _ := io.ReadAll(f)
	_ = f.Close()
	if string(b) != "package main\n" {
		t.Errorf("sandbox main.go = %q", b)
	}
	if st := cs.status; st.Seed == nil || st.Seed.Files != 2 || st.Seed.Error != "" {
		t.Errorf("seed record = %+v, want 2 files and no error", st.Seed)
	}
	if findAudit(audit.snapshot(), run.ID, "run.seed", "success") == nil {
		t.Error("the seed is not audited")
	}
	if ev := findAudit(audit.snapshot(), run.ID, "run.folder.transfer", "success"); ev == nil ||
		!strings.Contains(string(ev.Data), `"direction":"seed"`) || !strings.Contains(string(ev.Data), `"files":2`) {
		t.Errorf("the seed's transfer is not audited with its direction and count: %v", ev)
	}
	v := s.computerFolderRunView(context.Background(), run.ID)
	if v == nil || v.State != computerFolderSynced || v.Runner != "laptop" || v.Direction != types.ComputerFolderBoth {
		t.Fatalf("run view = %+v", v)
	}
	if b, _ := json.Marshal(v); strings.Contains(string(b), cs.folder.Path) {
		t.Errorf("the run view shows the path on the person's computer: %s", b)
	}
}

func TestHoldForLocalFolder_AComputerFolderWhoseRunnerIsGoneFailsTheRun(t *testing.T) {
	for name, breakIt := range map[string]func(*Server, *computerFolderStore){
		"offline":                 func(s *Server, _ *computerFolderStore) { s.cfg.RunnerOnline = func(uuid.UUID) bool { return false } },
		"no longer the creator's": func(_ *Server, cs *computerFolderStore) { cs.runner.Owner = "mallory" },
		"gate turned off":         func(s *Server, _ *computerFolderStore) { s.cfg.ComputerFolders = false },
	} {
		t.Run(name, func(t *testing.T) {
			s, cs, _, run, rn, _ := computerFolderFixture(t, types.ComputerFolderIn)
			breakIt(s, cs)
			if s.holdForLocalFolder(context.Background(), run, "sbx") {
				t.Fatal("the agent was let start without its folder")
			}
			if cs.State() != types.RunFailed || !strings.Contains(cs.FailureHint(), `"my-app"`) {
				t.Errorf("state %s, hint %q", cs.State(), cs.FailureHint())
			}
			if rn.execs.Load() != 0 {
				t.Error("the agent was started")
			}
		})
	}
}

func TestBringComputerFolderBack_RunsBeforeTheSandboxStopsAndOnlyForBothWays(t *testing.T) {
	s, cs, audit, run, sb, app := computerFolderFixture(t, types.ComputerFolderBoth)
	if !s.holdForLocalFolder(context.Background(), run, "sbx") {
		t.Fatal(cs.FailureHint())
	}
	c := sb.client(t)
	f, err := c.OpenFile("/home/agent/work/README.md", os.O_WRONLY|os.O_TRUNC)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.Write([]byte("edited by the agent"))
	_ = f.Close()
	_ = c.Chtimes("/home/agent/work/README.md", time.Now(), time.Now().Add(time.Hour))
	var backBeforeStop bool
	sb.stopped = func() {
		b, _ := os.ReadFile(filepath.Join(app, "README.md"))
		backBeforeStop = string(b) == "edited by the agent"
	}
	if err := s.finalizeRunEffects(context.Background(), run.ID, "sbx", "run.stop", nil, false, true); err != nil {
		t.Logf("finalize: %v", err)
	}
	if !backBeforeStop {
		t.Fatal("the sandbox was stopped before its changes came back")
	}
	if cs.status.Back == nil || cs.status.Back.Files != 1 {
		t.Errorf("back record = %+v, want 1 file", cs.status.Back)
	}
	if ev := findAuditData(audit.snapshot(), run.ID, "run.folder.transfer", `"direction":"back"`); ev == nil || ev.Outcome != "success" {
		t.Error("the pass back is not audited")
	}

	// Copy in only: nothing comes back.
	s, cs, _, run, sb, app = computerFolderFixture(t, types.ComputerFolderIn)
	if !s.holdForLocalFolder(context.Background(), run, "sbx") {
		t.Fatal(cs.FailureHint())
	}
	c = sb.client(t)
	f, _ = c.OpenFile("/home/agent/work/README.md", os.O_WRONLY|os.O_TRUNC)
	_, _ = f.Write([]byte("edited by the agent"))
	_ = f.Close()
	_ = c.Chtimes("/home/agent/work/README.md", time.Now(), time.Now().Add(time.Hour))
	s.bringComputerFolderBack(context.Background(), run.ID, "sbx")
	if b, _ := os.ReadFile(filepath.Join(app, "README.md")); string(b) != "hello" || cs.status.Back != nil {
		t.Errorf("a copy-in-only folder came back: %q, %+v", b, cs.status.Back)
	}
}

func TestLocalFolderRefusal_AComputerFolder(t *testing.T) {
	s, cs, _, run, _, app := computerFolderFixture(t, types.ComputerFolderBoth)
	ctx := withOIDCHuman(context.Background(), run.CreatedBy)
	id := cs.runner.ID
	ok := func() *createRunRequest {
		return &createRunRequest{Task: "x", LocalFolder: &types.RunLocalFolder{RunnerID: &id, Path: app}}
	}
	req := ok()
	if r := s.localFolderRefusal(ctx, req); r != nil {
		t.Fatalf("a good computer folder was refused: %+v", r.body)
	}
	if f := req.LocalFolder; f.Name != "my-app" || f.Direction != types.ComputerFolderIn || f.RemoteDir != "/home/agent/work" {
		t.Errorf("normalised to %+v, want the name from the path, copy in only, the default folder", f)
	}
	for name, c := range map[string]struct {
		edit   func(*createRunRequest)
		ctx    context.Context
		reason string
	}{
		"a relative path":     {func(r *createRunRequest) { r.LocalFolder.Path = "code/my-app" }, ctx, reasonLocalFolderInvalid},
		"an unclean path":     {func(r *createRunRequest) { r.LocalFolder.Path = app + "/../my-app" }, ctx, reasonLocalFolderInvalid},
		"a bad direction":     {func(r *createRunRequest) { r.LocalFolder.Direction = "live" }, ctx, reasonLocalFolderInvalid},
		"an interactive run":  {func(r *createRunRequest) { r.Interactive = true }, ctx, reasonLocalFolderUnsupported},
		"someone else":        {func(*createRunRequest) {}, withOIDCHuman(context.Background(), "mallory"), reasonRunnerNotFound},
		"no signed-in person": {func(*createRunRequest) {}, context.Background(), reasonRunnerNotFound},
	} {
		r := ok()
		c.edit(r)
		if got := s.localFolderRefusal(c.ctx, r); got == nil || got.body.Reason != c.reason {
			t.Errorf("%s: refusal %+v, want %s", name, got, c.reason)
		}
	}
	s.cfg.ComputerFolders = false
	if got := s.localFolderRefusal(ctx, ok()); got == nil || got.body.Reason != reasonComputerFoldersOff {
		t.Errorf("gate off: %+v", got)
	}
}

// A pass that runs out of time still leaves its record and its audit: both are written on the
// caller's ctx, not the pass's own deadline.
func TestSeedComputerFolder_APassThatTimesOutIsStillRecorded(t *testing.T) {
	s, cs, audit, run, sb, _ := computerFolderFixture(t, types.ComputerFolderBoth)
	s.cfg.LocalFolderSeedWait = 300 * time.Millisecond
	sb.hang = true
	if s.holdForLocalFolder(context.Background(), run, "sbx") {
		t.Fatal("the agent was let start with its folder's first pass timed out")
	}
	cs.smu.Lock()
	seed := cs.status.Seed
	cs.smu.Unlock()
	if seed == nil || seed.Error == "" {
		t.Fatalf("seed record = %+v, want the timed-out pass with its error", seed)
	}
	ev := findAudit(audit.snapshot(), run.ID, "run.folder.transfer", "failure")
	if ev == nil || !strings.Contains(string(ev.Data), `"direction":"seed"`) {
		t.Fatalf("the timed-out pass is not audited: %v", ev)
	}
}

// hangingFolderBack is a "both" run whose folder seeded and whose pass back never finishes.
func hangingFolderBack(t *testing.T) (*Server, *computerFolderStore, *recRecorder, types.AgentRun, *folderSandboxRunner) {
	t.Helper()
	s, cs, audit, run, sb, _ := computerFolderFixture(t, types.ComputerFolderBoth)
	if !s.holdForLocalFolder(context.Background(), run, "sbx") {
		t.Fatal(cs.FailureHint())
	}
	sb.hang = true
	return s, cs, audit, run, sb
}

// folderStatus is the folder's pass records as the store holds them.
func (s *computerFolderStore) folderStatus() types.RunLocalFolderStatus {
	s.smu.Lock()
	defer s.smu.Unlock()
	return s.status
}

// assertFolderBackTornDown checks the sandbox was stopped and the pass back recorded, with want
// in its error, and audited.
func assertFolderBackTornDown(t *testing.T, cs *computerFolderStore, audit *recRecorder, run types.AgentRun, sb *folderSandboxRunner, want string) {
	t.Helper()
	if n := sb.stopCount(); n != 1 {
		t.Errorf("StopSandbox succeeded %d times, want 1", n)
	}
	if ev := findAudit(audit.snapshot(), run.ID, "run.complete", "failure"); ev != nil {
		t.Errorf("the teardown failed: %s", ev.Data)
	}
	st := cs.folderStatus()
	if st.Back == nil || !strings.Contains(st.Back.Error, want) || st.Resume != nil {
		t.Errorf("back record = %+v, resume = %+v; want the pass's error naming %q and no resume", st.Back, st.Resume, want)
	}
	if ev := findAuditData(audit.snapshot(), run.ID, "run.folder.transfer", want); ev == nil || ev.Outcome != "failure" || !strings.Contains(string(ev.Data), `"direction":"back"`) {
		t.Errorf("the pass back is not audited with %q: %v", want, ev)
	}
}

// assertFolderBackKept checks a finalize kept the sandbox for a resume, with want as its resume.
func assertFolderBackKept(t *testing.T, err error, cs *computerFolderStore, sb *folderSandboxRunner, want types.FolderResume) {
	t.Helper()
	if !errors.Is(err, errComputerFolderResumes) {
		t.Errorf("finalize = %v, want the kept sandbox's errComputerFolderResumes", err)
	}
	if n := sb.stopCount(); n != 0 {
		t.Errorf("StopSandbox succeeded %d times, want the sandbox kept", n)
	}
	st := cs.folderStatus()
	if st.Resume == nil || *st.Resume != want || st.Back == nil || !strings.Contains(st.Back.Error, "a later sweep resumes the sync") {
		t.Errorf("resume = %+v, back = %+v; want %+v and the cut pass's error", st.Resume, st.Back, want)
	}
	// Kept past the run's end, the sandbox runs only the pass back: no agent, no egress.
	if agent, _ := sb.AgentStatus(context.Background(), "sbx", "agent-exec"); agent.State == types.RunRunning {
		t.Error("the kept sandbox's agent is still running")
	}
	if err := sb.agentEgress(); !errors.Is(err, errNoEgress) {
		t.Errorf("the kept sandbox's agent egress = %v, want it refused", err)
	}
}

// boundedFinalize is a finalize whose caller allows its pass back stop seconds.
func boundedFinalize(t *testing.T, s *Server, run types.AgentRun, stop time.Duration) (time.Duration, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), runTeardownBudget+computerFolderAccountTime+stop)
	defer cancel()
	start := time.Now()
	err := s.finalizeRunEffects(ctx, run.ID, "sbx", "run.complete", nil, false, false)
	return time.Since(start), err
}

// A pass back that would outlast the caller's deadline is cut before it, inside the caller's
// bound. The first cut that saves nothing keeps the sandbox for a resume; the second in a row
// stops it, and the record and the audit say so (#3466).
func TestFinalizeRunEffects_TwoCutPassesThatSaveNothingStopTheSandbox(t *testing.T) {
	s, cs, audit, run, sb := hangingFolderBack(t)
	took, err := boundedFinalize(t, s, run, 300*time.Millisecond)
	if took >= runTeardownBudget {
		t.Errorf("finalize took %s: the pass back used the teardown's time", took)
	}
	assertFolderBackKept(t, err, cs, sb, types.FolderResume{Stalled: 1})
	if _, err := boundedFinalize(t, s, run, 300*time.Millisecond); err != nil {
		t.Errorf("finalize: %v", err)
	}
	assertFolderBackTornDown(t, cs, audit, run, sb, "2 passes in a row brought nothing back, so the sandbox was stopped")
}

// A pass back cut short after it saved a file keeps the sandbox, and the next pass starts after
// that file. Once it saves nothing twice in a row, the sandbox is stopped with the count of the
// changed files it never brought back (#3466).
func TestFinalizeRunEffects_ACutPassThatSavedFilesKeepsTheSandbox(t *testing.T) {
	s, cs, audit, run, sb, app := computerFolderFixture(t, types.ComputerFolderBoth)
	if !s.holdForLocalFolder(context.Background(), run, "sbx") {
		t.Fatal(cs.FailureHint())
	}
	agentWrites(t, sb, "a.txt", "z.txt")
	sb.block = "z.txt"
	_, err := boundedFinalize(t, s, run, time.Second)
	assertFolderBackKept(t, err, cs, sb, types.FolderResume{Unsynced: 1})
	if b, _ := os.ReadFile(filepath.Join(app, "a.txt")); string(b) != "a.txt by the agent" {
		t.Errorf("a.txt on the computer = %q, want the agent's", b)
	}
	if back := cs.folderStatus().Back; back == nil || back.Files != 1 {
		t.Errorf("back record = %+v, want the one file the cut pass saved", back)
	}
	_, err = boundedFinalize(t, s, run, time.Second)
	assertFolderBackKept(t, err, cs, sb, types.FolderResume{Stalled: 1, Unsynced: 1})
	if _, err := boundedFinalize(t, s, run, time.Second); err != nil {
		t.Errorf("finalize: %v", err)
	}
	assertFolderBackTornDown(t, cs, audit, run, sb, "changed files not brought back: 1")
}

// A pass back that completes after a cut one stops the sandbox, having brought back only what
// the cut pass had not.
func TestFinalizeRunEffects_APassThatCompletesAfterACutOneStopsTheSandbox(t *testing.T) {
	s, cs, _, run, sb, app := computerFolderFixture(t, types.ComputerFolderBoth)
	if !s.holdForLocalFolder(context.Background(), run, "sbx") {
		t.Fatal(cs.FailureHint())
	}
	agentWrites(t, sb, "a.txt", "z.txt")
	sb.block = "z.txt"
	_, err := boundedFinalize(t, s, run, time.Second)
	assertFolderBackKept(t, err, cs, sb, types.FolderResume{Unsynced: 1})
	sb.block = ""
	if err := s.finalizeRunEffects(context.Background(), run.ID, "sbx", "run.complete", nil, false, false); err != nil {
		t.Errorf("finalize: %v", err)
	}
	if n := sb.stopCount(); n != 1 {
		t.Errorf("StopSandbox succeeded %d times, want 1", n)
	}
	st := cs.folderStatus()
	if st.Resume != nil || st.Back == nil || st.Back.Error != "" || st.Back.Files != 1 {
		t.Errorf("resume = %+v, back = %+v; want the completed pass with z.txt alone and no resume", st.Resume, st.Back)
	}
	if b, _ := os.ReadFile(filepath.Join(app, "z.txt")); string(b) != "z.txt by the agent" {
		t.Errorf("z.txt on the computer = %q, want the agent's", b)
	}
}

// A sandbox that cannot be kept with its agent stopped and no egress is stopped after a cut pass
// back that saved files, and the record says why and what it leaves behind: one on a runner, and
// one whose substrate cannot stop its proxy alone (Kubernetes in server mode) (#3466).
func TestFinalizeRunEffects_ASandboxThatCannotBeQuiescedIsStoppedWithTheRecord(t *testing.T) {
	cases := map[string]struct {
		setup func(*localFolderStore, *folderSandboxRunner)
		want  string
	}{
		"on a runner": {func(ls *localFolderStore, _ *folderSandboxRunner) {
			id := uuid.New()
			ls.run.RunnerID = &id
		}, "a sandbox on a runner is not kept"},
		"its proxy cannot stop alone": {func(_ *localFolderStore, sb *folderSandboxRunner) {
			sb.proxyErr = runner.ErrEndUnsupported
		}, "its substrate cannot stop its proxy alone"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			s, cs, audit, run, sb, _ := computerFolderFixture(t, types.ComputerFolderBoth)
			if !s.holdForLocalFolder(context.Background(), run, "sbx") {
				t.Fatal(cs.FailureHint())
			}
			tc.setup(cs.localFolderStore, sb)
			agentWrites(t, sb, "a.txt", "z.txt")
			sb.block = "z.txt"
			if _, err := boundedFinalize(t, s, run, time.Second); err != nil {
				t.Errorf("finalize: %v", err)
			}
			assertFolderBackTornDown(t, cs, audit, run, sb, tc.want)
			assertFolderBackTornDown(t, cs, audit, run, sb, "it cannot be kept with its agent stopped and no egress")
			assertFolderBackTornDown(t, cs, audit, run, sb, "changed files not brought back: 1")
		})
	}
}

// At a lease end the first pass back starts with the agent already stopped and no egress, and
// still brings the agent's work home (#3466).
func TestFinalizeRunEffects_TheFirstPassBackRunsWithTheAgentStoppedAndNoEgress(t *testing.T) {
	s, cs, _, run, sb, app := computerFolderFixture(t, types.ComputerFolderBoth)
	if !s.holdForLocalFolder(context.Background(), run, "sbx") {
		t.Fatal(cs.FailureHint())
	}
	agentWrites(t, sb, "a.txt")
	var running atomic.Bool
	var egress atomic.Value
	sb.onPass = func() {
		agent, _ := sb.AgentStatus(context.Background(), "sbx", "agent-exec")
		running.Store(agent.State == types.RunRunning)
		egress.Store(fmt.Sprint(sb.agentEgress()))
	}
	if err := s.finalizeRunEffects(context.Background(), run.ID, "sbx", "run.stop", nil, false, true); err != nil {
		t.Errorf("finalize: %v", err)
	}
	if running.Load() || egress.Load() != errNoEgress.Error() {
		t.Errorf("as the pass back started: agent running %v, agent egress %v; want stopped and refused", running.Load(), egress.Load())
	}
	if b, _ := os.ReadFile(filepath.Join(app, "a.txt")); string(b) != "a.txt by the agent" || sb.stopCount() != 1 {
		t.Errorf("a.txt on the computer = %q, stops = %d; want the agent's file and the sandbox stopped", b, sb.stopCount())
	}
}

// A sandbox whose substrate can stop its agent and egress, but where that fails, runs no pass
// back: it is stopped, and the record says why (#3466). An exec-less sandbox, whose agent is its
// main process, is refused before any kill, and one already stopped is named as that.
func TestFinalizeRunEffects_ASandboxWhoseAgentCannotBeStoppedRunsNoPass(t *testing.T) {
	cases := map[string]struct {
		setup func(*localFolderStore, *folderSandboxRunner)
		want  string
	}{
		"its proxy is wedged": {func(_ *localFolderStore, sb *folderSandboxRunner) {
			sb.proxyErr = errors.New("the proxy is wedged")
		}, "no pass ran; its agent and egress could not be stopped first: its proxy did not stop: the proxy is wedged"},
		"its proxy stop fails closed": {func(_ *localFolderStore, sb *folderSandboxRunner) {
			sb.proxyErr, sb.proxyEnds = errors.New("the proxy is wedged"), true
		}, "no pass ran; its agent and egress could not be stopped first: its proxy did not stop: the proxy is wedged"},
		"its agent is the main process": {func(ls *localFolderStore, _ *folderSandboxRunner) {
			ls.run.AgentExecID = runner.MainProcessExecID
		}, "no pass ran; its agent and egress could not be stopped first: its agent is the sandbox's main process, which cannot be stopped without stopping the sandbox"},
		"its kill cannot reach it": {func(_ *localFolderStore, sb *folderSandboxRunner) {
			sb.killErr = errors.New("exec refused")
		}, "no pass ran; its agent and egress could not be stopped first: its agent could not be stopped: exec refused"},
		"its agent outlives the kill": {func(_ *localFolderStore, sb *folderSandboxRunner) {
			sb.agentLives = true
		}, "no pass ran; its agent and egress could not be stopped first: its agent still reads as running,"},
		// A lease end's EndSandbox stopped it: its proxy stop succeeds, and the kill's exec fails.
		"it had already stopped": {func(_ *localFolderStore, sb *folderSandboxRunner) {
			sb.ended = true
		}, "no pass ran; the sandbox had already stopped, with its changes not brought back yet, uncounted"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			s, cs, audit, run, sb, _ := computerFolderFixture(t, types.ComputerFolderBoth)
			if !s.holdForLocalFolder(context.Background(), run, "sbx") {
				t.Fatal(cs.FailureHint())
			}
			tc.setup(cs.localFolderStore, sb)
			var passed atomic.Bool
			sb.onPass = func() { passed.Store(true) }
			if err := s.finalizeRunEffects(context.Background(), run.ID, "sbx", "run.complete", nil, false, false); err != nil {
				t.Errorf("finalize: %v", err)
			}
			if passed.Load() {
				t.Error("a pass back ran with the agent and its egress not stopped")
			}
			if name == "its agent is the main process" && sb.agentKilled.Load() {
				t.Error("an exec-less sandbox's agent kill ran; want it refused before any kill")
			}
			assertFolderBackTornDown(t, cs, audit, run, sb, tc.want)
			assertFolderBackTornDown(t, cs, audit, run, sb, "its changes not brought back yet, uncounted")
			if st := cs.folderStatus(); st.Back != nil && sb.proxyErr != nil && strings.Contains(st.Back.Error, "already stopped") {
				t.Errorf("back record = %q; want the proxy's failure, not an already stopped sandbox", st.Back.Error)
			}
		})
	}
}

// agentWrites has the agent write each name, new, in the sandbox's folder.
func agentWrites(t *testing.T, sb *folderSandboxRunner, names ...string) {
	t.Helper()
	c := sb.client(t)
	for _, name := range names {
		f, err := c.Create("/home/agent/work/" + name)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = f.Write([]byte(name + " by the agent"))
		_ = f.Close()
	}
}

// A caller's ctx cancelled before finalize leaves no time to start the pass, which is recorded,
// and the sandbox is stopped; one cancelled during a second cut pass in a row that saves nothing
// stops it too. The pass's outcome is recorded and audited, and the caller hears its ctx ended.
func TestFinalizeRunEffects_TeardownRunsAfterTheCallersCtxIsCancelled(t *testing.T) {
	cases := map[string]struct {
		after time.Duration
		want  string
	}{
		"before finalize":      {0, "no time was left for the sync"},
		"during the pass back": {200 * time.Millisecond, "2 passes in a row brought nothing back"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			s, cs, audit, run, sb := hangingFolderBack(t)
			cs.status.Resume = &types.FolderResume{Stalled: 1}
			ctx, cancel := context.WithCancel(context.Background())
			if tc.after == 0 {
				cancel()
			} else {
				time.AfterFunc(tc.after, cancel)
			}
			if err := s.finalizeRunEffects(ctx, run.ID, "sbx", "run.complete", nil, false, false); !errors.Is(err, context.Canceled) {
				t.Errorf("finalize = %v, want the caller's cancel", err)
			}
			assertFolderBackTornDown(t, cs, audit, run, sb, tc.want)
		})
	}
}

// findAuditData is the first run event of action whose data holds want.
func findAuditData(events []types.AuditEvent, runID uuid.UUID, action, want string) *types.AuditEvent {
	for i := range events {
		ev := &events[i]
		if ev.RunID != nil && *ev.RunID == runID && ev.Action == action && strings.Contains(string(ev.Data), want) {
			return ev
		}
	}
	return nil
}
