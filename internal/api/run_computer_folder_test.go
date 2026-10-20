// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
}

func (r *folderSandboxRunner) ExecStream(_ context.Context, _ string, spec runner.ExecSpec) (*runner.ExecSession, error) {
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
	srv := sftp.NewRequestServer(struct {
		io.Reader
		io.WriteCloser
	}{toServer, fromServer}, r.files, sftp.WithStartDirectory(dir))
	done := make(chan struct{})
	go func() { _ = srv.Serve(); _ = fromServer.Close(); close(done) }()
	return &runner.ExecSession{
		Stdin: stdin, Stdout: stdout, Stderr: strings.NewReader(""),
		Wait:  func() (int, error) { <-done; return 0, nil },
		Close: func() error { _ = stdin.Close(); _ = srv.Close(); return nil },
	}, nil
}

func (r *folderSandboxRunner) StopSandbox(ctx context.Context, ref string) error {
	if r.stopped != nil {
		r.stopped()
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
