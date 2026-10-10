// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"github.com/cjohnstoniv/wardyn/internal/api"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// The real gateway handler, in process, with a fake sandbox whose sftp-server
// serves a temp directory as if it were the sandbox's filesystem: sandbox path
// /home/agent/x is <sandbox>/home/agent/x.

type syncStore struct {
	store.Store
	run types.AgentRun
	mu  sync.Mutex
	key map[string]types.SSHPublicKey
}

func (s *syncStore) GetRun(_ context.Context, id uuid.UUID) (types.AgentRun, error) {
	if id != s.run.ID {
		return types.AgentRun{}, store.ErrNotFound
	}
	return s.run, nil
}
func (s *syncStore) register(principal string, pub ssh.PublicKey) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fp := ssh.FingerprintSHA256(pub)
	s.key[fp] = types.SSHPublicKey{Fingerprint: fp, Principal: principal, PublicKey: string(ssh.MarshalAuthorizedKey(pub))}
}

func (s *syncStore) TouchRun(context.Context, uuid.UUID) error { return nil }
func (s *syncStore) GetSSHKeyByFingerprint(_ context.Context, fp string) (types.SSHPublicKey, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k, ok := s.key[fp]
	if !ok {
		return types.SSHPublicKey{}, store.ErrNotFound
	}
	return k, nil
}
func (s *syncStore) ListGovernanceProfiles(context.Context) ([]types.GovernanceProfile, error) {
	return nil, nil
}

type syncAudit struct {
	mu sync.Mutex
	ev []types.AuditEvent
}

func (a *syncAudit) Record(ctx context.Context, ev types.AuditEvent) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.ev = append(a.ev, ev)
	return nil
}

// row waits for the ssh.sync.transfer row and returns its data.
func (a *syncAudit) row(t *testing.T) (string, map[string]any) {
	t.Helper()
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		a.mu.Lock()
		for _, e := range a.ev {
			if e.Action == "ssh.sync.transfer" {
				a.mu.Unlock()
				var d map[string]any
				_ = json.Unmarshal(e.Data, &d)
				return e.Outcome, d
			}
		}
		a.mu.Unlock()
	}
	t.Fatal("no ssh.sync.transfer row")
	return "", nil
}

// syncRunner implements only ExecStream; any other call panics through the
// nil embedded interface and fails the test loudly.
type syncRunner struct {
	runner.Runner
	sandbox string
	// exitDelay is how long the sandbox's sftp-server takes to exit after its
	// stdin closes; exitCode is what it exits with.
	exitDelay time.Duration
	exitCode  int
	// hangRead names a file whose read blocks until release is closed (a FIFO
	// or a trickling sandbox); liar lists a file's size as a smaller number.
	hangRead string
	release  chan struct{}
	liar     map[string]int64
}

func (r *syncRunner) ExecStream(ctx context.Context, _ string, spec runner.ExecSpec) (*runner.ExecSession, error) {
	start := "/"
	if i := slices.Index(spec.Argv, "-d"); i >= 0 && i+1 < len(spec.Argv) {
		// sftp-server's own chdir failure does not stop it: it serves from
		// its working directory.
		if fi, err := os.Stat(r.host(spec.Argv[i+1])); err == nil && fi.IsDir() {
			start = spec.Argv[i+1]
		}
	}
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	errR, errW := io.Pipe()
	srv := sftp.NewRequestServer(struct {
		io.Reader
		io.WriteCloser
	}{inR, outW}, r.handlers(), sftp.WithStartDirectory(start))
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = srv.Serve()
		_ = outW.Close()
		_ = errW.Close()
		time.Sleep(r.exitDelay)
	}()
	return &runner.ExecSession{
		Stdin: inW, Stdout: outR, Stderr: errR,
		// A channel closed before sftp-server saw EOF cancels the exec, which
		// reads as a failed sync.
		Wait: func() (int, error) {
			select {
			case <-done:
				return r.exitCode, nil
			case <-ctx.Done():
				return 137, nil
			}
		},
	}, nil
}

func (r *syncRunner) host(p string) string {
	return filepath.Join(r.sandbox, filepath.FromSlash(path.Clean("/"+p)))
}

func (r *syncRunner) handlers() sftp.Handlers {
	h := &rootedFS{r}
	return sftp.Handlers{FileGet: h, FilePut: h, FileCmd: h, FileList: h}
}

// rootedFS serves the real filesystem under the sandbox directory.
type rootedFS struct{ r *syncRunner }

func (h *rootedFS) Fileread(rq *sftp.Request) (io.ReaderAt, error) {
	if h.r.hangRead != "" && path.Base(rq.Filepath) == h.r.hangRead {
		<-h.r.release
	}
	return os.Open(h.r.host(rq.Filepath))
}

func (h *rootedFS) Filewrite(rq *sftp.Request) (io.WriterAt, error) {
	return os.OpenFile(h.r.host(rq.Filepath), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
}

func (h *rootedFS) Filecmd(rq *sftp.Request) error {
	p := h.r.host(rq.Filepath)
	switch rq.Method {
	case "Setstat":
		fl := rq.AttrFlags()
		a := rq.Attributes()
		if fl.Permissions {
			if err := os.Chmod(p, a.FileMode()); err != nil {
				return err
			}
		}
		if fl.Acmodtime {
			return os.Chtimes(p, time.Unix(int64(a.Atime), 0), time.Unix(int64(a.Mtime), 0))
		}
		return nil
	case "Rename":
		return os.Rename(p, h.r.host(rq.Target))
	case "Rmdir", "Remove":
		return os.Remove(p)
	case "Mkdir":
		return os.Mkdir(p, 0o755)
	case "Symlink":
		return os.Symlink(rq.Filepath, h.r.host(rq.Target))
	}
	return errors.New("unsupported")
}

type liarInfo struct {
	os.FileInfo
	size int64
}

func (l liarInfo) Size() int64 { return l.size }

type fileLister []os.FileInfo

func (l fileLister) ListAt(out []os.FileInfo, off int64) (int, error) {
	if off >= int64(len(l)) {
		return 0, io.EOF
	}
	n := copy(out, l[off:])
	if n < len(out) {
		return n, io.EOF
	}
	return n, nil
}

func (h *rootedFS) Filelist(rq *sftp.Request) (sftp.ListerAt, error) {
	p := h.r.host(rq.Filepath)
	switch rq.Method {
	case "List":
		ents, err := os.ReadDir(p)
		if err != nil {
			return nil, err
		}
		out := make(fileLister, 0, len(ents))
		for _, e := range ents {
			fi, err := os.Lstat(filepath.Join(p, e.Name()))
			if err != nil {
				return nil, err
			}
			if size, ok := h.r.liar[e.Name()]; ok {
				fi = liarInfo{fi, size}
			}
			out = append(out, fi)
		}
		return out, nil
	case "Stat":
		fi, err := os.Stat(p)
		return fileLister{fi}, err
	case "Lstat":
		fi, err := os.Lstat(p)
		return fileLister{fi}, err
	}
	return nil, errors.New("unsupported")
}

type syncRig struct {
	t       *testing.T
	gw      sshGateway
	runID   string
	signer  ssh.Signer
	sandbox string
	audit   *syncAudit
	st      *syncStore
	rn      *syncRunner
}

func newSyncRig(t *testing.T) *syncRig {
	t.Helper()
	_, hostPriv, _ := ed25519.GenerateKey(rand.Reader)
	hostSigner, _ := ssh.NewSignerFromKey(hostPriv)
	_, userPriv, _ := ed25519.GenerateKey(rand.Reader)
	signer, _ := ssh.NewSignerFromKey(userPriv)
	run := types.AgentRun{ID: uuid.New(), CreatedBy: "alice@example.com", State: types.RunRunning, SandboxRef: "sbx"}
	st := &syncStore{run: run, key: map[string]types.SSHPublicKey{}}
	st.register(run.CreatedBy, signer.PublicKey())
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := probe.Addr().String()
	_ = probe.Close()
	sandbox := t.TempDir()
	audit := &syncAudit{}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	rn := &syncRunner{sandbox: sandbox, release: make(chan struct{})}
	t.Cleanup(func() { close(rn.release) })
	srv := api.New(api.Config{
		Store: st, Audit: audit, Runner: rn, BaseCtx: ctx,
		SSHListenAddr: addr, SSHAdvertiseAddr: addr, SSHHostKey: hostPriv,
	})
	go func() { _ = srv.ServeSSHGateway(ctx) }()
	for deadline := time.Now().Add(3 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if c, err := net.Dial("tcp", addr); err == nil {
			_ = c.Close()
			break
		} else if time.Now().After(deadline) {
			t.Fatalf("gateway never listened: %v", err)
		}
	}
	host, port := splitHostPort(addr)
	return &syncRig{t: t, gw: sshGateway{host: host, port: port, fingerprint: ssh.FingerprintSHA256(hostSigner.PublicKey())},
		runID: run.ID.String(), signer: signer, sandbox: sandbox, audit: audit, st: st, rn: rn}
}

// mkSandboxDir creates a sandbox directory and returns its host path.
func (g *syncRig) mkSandboxDir(sandboxPath string) string {
	g.t.Helper()
	p := filepath.Join(g.sandbox, filepath.FromSlash(sandboxPath))
	if err := os.MkdirAll(p, 0o755); err != nil {
		g.t.Fatal(err)
	}
	return p
}

func (g *syncRig) dial() *ssh.Client {
	g.t.Helper()
	c, err := dialSyncGateway(context.Background(), g.gw, g.runID, g.signer)
	if err != nil {
		g.t.Fatal(err)
	}
	g.t.Cleanup(func() { _ = c.Close() })
	return c
}

// syncer opens a session and returns a syncer over it plus the session.
func (g *syncRig) syncer(remote string, pull bool) (*syncer, *syncSession, string) {
	g.t.Helper()
	root := g.t.TempDir()
	stateFile := filepath.Join(g.t.TempDir(), "state.json")
	dir := "push"
	if pull {
		dir = "pull"
	}
	sess, err := openSyncSession(g.dial(), remote, dir)
	if err != nil {
		g.t.Fatal(err)
	}
	g.t.Cleanup(func() { _ = sess.close() })
	st, err := loadSyncState(stateFile, root, remote)
	if err != nil {
		g.t.Fatal(err)
	}
	lroot, err := os.OpenRoot(root)
	if err != nil {
		g.t.Fatal(err)
	}
	g.t.Cleanup(func() { _ = lroot.Close() })
	return &syncer{c: sess.sftp, root: lroot, o: syncOptions{remote: remote, pull: pull, maxPullBytes: defaultMaxPullBytes}, st: st, stateFile: stateFile}, sess, root
}

func putSyncFile(t *testing.T, p, body string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
}

func getSyncFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func mustPass(t *testing.T, sy *syncer) *syncReport {
	t.Helper()
	rep, err := sy.pass(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

const syncRemote = "/home/agent/work"

func TestSyncGateway_PushIsTheDefaultAndIdempotent(t *testing.T) {
	g := newSyncRig(t)
	box := g.mkSandboxDir(syncRemote)
	sy, sess, root := g.syncer(syncRemote, false)
	putSyncFile(t, filepath.Join(root, "a.txt"), "alpha", 0o644)
	putSyncFile(t, filepath.Join(root, "sub/deep/b.sh"), "#!/bin/sh\n", 0o755)
	putSyncFile(t, filepath.Join(box, "theirs.txt"), "sandbox only", 0o644)

	rep := mustPass(t, sy)
	if !slices.Equal(rep.Pushed, []string{"a.txt", "sub/deep/b.sh"}) {
		t.Fatalf("pushed = %v", rep.Pushed)
	}
	if got := getSyncFile(t, filepath.Join(box, "sub/deep/b.sh")); got != "#!/bin/sh\n" {
		t.Errorf("remote content = %q", got)
	}
	if fi, _ := os.Stat(filepath.Join(box, "sub/deep/b.sh")); fi.Mode().Perm() != 0o755 {
		t.Errorf("a pushed script lost its mode: %v", fi.Mode())
	}
	if _, err := os.Stat(filepath.Join(root, "theirs.txt")); err == nil {
		t.Error("without --pull a sandbox file reached the laptop")
	}
	if rep := mustPass(t, sy); !rep.empty() {
		t.Errorf("a second pass with nothing changed did something: %+v", rep)
	}
	putSyncFile(t, filepath.Join(box, "a.txt"), "changed in the sandbox", 0o644)
	later := time.Now().Add(time.Hour)
	_ = os.Chtimes(filepath.Join(box, "a.txt"), later, later)
	if rep := mustPass(t, sy); !rep.empty() || getSyncFile(t, filepath.Join(root, "a.txt")) != "alpha" {
		t.Errorf("without --pull a changed sandbox file was pulled or reported: %+v", rep)
	}
	putSyncFile(t, filepath.Join(root, "a.txt"), "alpha two", 0o644)
	if rep := mustPass(t, sy); !slices.Equal(rep.Pushed, []string{"a.txt"}) {
		t.Errorf("changed file: pushed = %v", rep.Pushed)
	}

	if err := sess.close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	outcome, data := g.audit.row(t)
	if outcome != "success" || data["direction"] != "push" || data["dir"] != syncRemote {
		t.Errorf("audit row = %s %v; closing stdin then waiting for exit must read as success", outcome, data)
	}
	if in, _ := data["bytes_in"].(float64); in == 0 {
		t.Errorf("bytes_in = 0 in %v: a push goes into the sandbox", data)
	}
}

func TestSyncGateway_PullNeverSetsAnExecBit(t *testing.T) {
	g := newSyncRig(t)
	box := g.mkSandboxDir(syncRemote)
	putSyncFile(t, filepath.Join(box, "run.sh"), "#!/bin/sh\n", 0o755)
	putSyncFile(t, filepath.Join(box, "nested/x.txt"), "x", 0o777)
	sy, _, root := g.syncer(syncRemote, true)
	mustPass(t, sy)
	for _, rel := range []string{"run.sh", "nested/x.txt"} {
		fi, err := os.Stat(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("%s was not pulled: %v", rel, err)
		}
		if fi.Mode().Perm()&0o111 != 0 {
			t.Errorf("pulled %s has an exec bit: %v", rel, fi.Mode())
		}
	}
}

func TestSyncGateway_PullOverAnExecutableDropsTheBit(t *testing.T) {
	g := newSyncRig(t)
	box := g.mkSandboxDir(syncRemote)
	sy, _, root := g.syncer(syncRemote, true)
	putSyncFile(t, filepath.Join(root, "tool"), "old", 0o755)
	mustPass(t, sy) // pushes tool
	putSyncFile(t, filepath.Join(box, "tool"), "changed on the sandbox", 0o755)
	later := time.Now().Add(time.Hour)
	_ = os.Chtimes(filepath.Join(box, "tool"), later, later)
	rep := mustPass(t, sy)
	if !slices.Equal(rep.Pulled, []string{"tool"}) {
		t.Fatalf("report = %+v", rep)
	}
	fi, _ := os.Stat(filepath.Join(root, "tool"))
	if fi.Mode().Perm()&0o111 != 0 || getSyncFile(t, filepath.Join(root, "tool")) != "changed on the sandbox" {
		t.Errorf("tool = %v %q", fi.Mode(), getSyncFile(t, filepath.Join(root, "tool")))
	}
}

func TestSyncGateway_SymlinksAreRefusedBothWays(t *testing.T) {
	g := newSyncRig(t)
	box := g.mkSandboxDir(syncRemote)
	outside := t.TempDir()
	putSyncFile(t, filepath.Join(outside, "secret"), "do not touch", 0o600)
	sy, _, root := g.syncer(syncRemote, true)

	putSyncFile(t, filepath.Join(root, "real.txt"), "real", 0o644)
	_ = os.Symlink(filepath.Join(outside, "secret"), filepath.Join(root, "link-to-file"))
	_ = os.Symlink(outside, filepath.Join(root, "link-to-dir"))
	// The sandbox plants links, and a directory to be written through.
	_ = os.Symlink("/etc/passwd", filepath.Join(box, "etc-link"))
	_ = os.Symlink(outside, filepath.Join(box, "dirlink"))
	putSyncFile(t, filepath.Join(box, "ok.txt"), "fine", 0o644)
	// A local symlink already standing where the sandbox's file would land.
	_ = os.Symlink(outside, filepath.Join(root, "planted"))
	putSyncFile(t, filepath.Join(box, "planted/payload"), "x", 0o644)

	rep := mustPass(t, sy)
	if !slices.Equal(rep.Pushed, []string{"real.txt"}) {
		t.Errorf("pushed = %v: a symlink was pushed", rep.Pushed)
	}
	for _, name := range []string{"link-to-file", "link-to-dir", "etc-link", "dirlink", "planted"} {
		found := false
		for _, r := range rep.Refused {
			found = found || (r.Path == name && r.Reason == "symlink")
		}
		if !found {
			t.Errorf("%s not reported as a refused symlink: %+v", name, rep.Refused)
		}
	}
	if _, err := os.Stat(filepath.Join(box, "link-to-file")); err == nil {
		t.Error("a local symlink reached the sandbox")
	}
	for _, rel := range []string{"etc-link", "dirlink"} {
		if _, err := os.Lstat(filepath.Join(root, rel)); err == nil {
			t.Errorf("the sandbox's symlink %s reached the laptop", rel)
		}
	}
	if _, err := os.Stat(filepath.Join(outside, "payload")); err == nil {
		t.Error("a pull wrote through a local symlink")
	}
	if getSyncFile(t, filepath.Join(outside, "secret")) != "do not touch" {
		t.Error("the file behind a symlink changed")
	}
}

func TestSyncGateway_DenylistBothWays(t *testing.T) {
	g := newSyncRig(t)
	box := g.mkSandboxDir(syncRemote)
	sy, _, root := g.syncer(syncRemote, true)
	denied := []string{
		".git/hooks/pre-commit", ".git/config", ".envrc", ".direnv/x", ".vscode/tasks.json", ".vscode/launch.json",
		".idea/runConfigurations/a.xml", ".idea/workspace.xml", "pkg/.git/HEAD", "pkg/.envrc", ".GIT/config",
	}
	allowed := []string{".vscode/settings.json", ".idea/misc.xml", "src/main.go"}
	for _, rel := range denied {
		putSyncFile(t, filepath.Join(root, "from-laptop", rel), "L", 0o644)
		putSyncFile(t, filepath.Join(box, "from-sandbox", rel), "S", 0o644)
	}
	for _, rel := range allowed {
		putSyncFile(t, filepath.Join(root, "from-laptop", rel), "L", 0o644)
		putSyncFile(t, filepath.Join(box, "from-sandbox", rel), "S", 0o644)
	}
	mustPass(t, sy)
	for _, rel := range denied {
		if _, err := os.Stat(filepath.Join(box, "from-laptop", rel)); err == nil {
			t.Errorf("%s was pushed", rel)
		}
		if _, err := os.Stat(filepath.Join(root, "from-sandbox", rel)); err == nil {
			t.Errorf("%s was pulled", rel)
		}
	}
	for _, rel := range allowed {
		if _, err := os.Stat(filepath.Join(box, "from-laptop", rel)); err != nil {
			t.Errorf("%s was not pushed: %v", rel, err)
		}
		if _, err := os.Stat(filepath.Join(root, "from-sandbox", rel)); err != nil {
			t.Errorf("%s was not pulled: %v", rel, err)
		}
	}
}

func TestSyncGateway_ConflictsTheLaptopWins(t *testing.T) {
	g := newSyncRig(t)
	box := g.mkSandboxDir(syncRemote)
	sy, _, root := g.syncer(syncRemote, true)
	putSyncFile(t, filepath.Join(root, "both.txt"), "v1", 0o644)
	putSyncFile(t, filepath.Join(root, "mine.txt"), "m1", 0o644)
	putSyncFile(t, filepath.Join(root, "gone.txt"), "g1", 0o644)
	mustPass(t, sy)

	later := time.Now().Add(2 * time.Hour)
	touch := func(p string) { _ = os.Chtimes(p, later, later) }
	putSyncFile(t, filepath.Join(root, "both.txt"), "laptop edit!", 0o644)
	putSyncFile(t, filepath.Join(box, "both.txt"), "sandbox edit", 0o644)
	touch(filepath.Join(box, "both.txt"))
	putSyncFile(t, filepath.Join(box, "mine.txt"), "sandbox only edit", 0o644)
	touch(filepath.Join(box, "mine.txt"))
	// Deleted on the laptop, changed in the sandbox: not brought back.
	_ = os.Remove(filepath.Join(root, "gone.txt"))
	putSyncFile(t, filepath.Join(box, "gone.txt"), "sandbox kept editing", 0o644)
	touch(filepath.Join(box, "gone.txt"))

	rep := mustPass(t, sy)
	slices.Sort(rep.Conflicts)
	if !slices.Equal(rep.Conflicts, []string{"both.txt", "gone.txt"}) {
		t.Errorf("conflicts = %v", rep.Conflicts)
	}
	if got := getSyncFile(t, filepath.Join(root, "both.txt")); got != "laptop edit!" {
		t.Errorf("the laptop's file was overwritten by a pull: %q", got)
	}
	if got := getSyncFile(t, filepath.Join(box, "both.txt")); got != "laptop edit!" {
		t.Errorf("the laptop did not win: sandbox has %q", got)
	}
	if _, err := os.Stat(filepath.Join(root, "gone.txt")); err == nil {
		t.Error("a file deleted locally was pulled back")
	}
	if got := getSyncFile(t, filepath.Join(root, "mine.txt")); got != "sandbox only edit" || !slices.Contains(rep.Pulled, "mine.txt") {
		t.Errorf("a sandbox-only change was not pulled: %q %v", got, rep.Pulled)
	}
}

func TestSyncGateway_CaseCollisionsAreRefused(t *testing.T) {
	g := newSyncRig(t)
	box := g.mkSandboxDir(syncRemote)
	sy, _, root := g.syncer(syncRemote, true)
	putSyncFile(t, filepath.Join(box, "Readme.md"), "one", 0o644)
	putSyncFile(t, filepath.Join(box, "README.md"), "two", 0o644)
	putSyncFile(t, filepath.Join(box, "fine.md"), "ok", 0o644)
	rep := mustPass(t, sy)
	for _, name := range []string{"Readme.md", "README.md"} {
		if _, err := os.Stat(filepath.Join(root, name)); err == nil {
			t.Errorf("%s was pulled despite the case collision", name)
		}
	}
	if len(rep.Refused) != 2 || rep.Refused[0].Reason != "case collision" {
		t.Errorf("refused = %+v", rep.Refused)
	}
	if !slices.Equal(rep.Pulled, []string{"fine.md"}) {
		t.Errorf("pulled = %v", rep.Pulled)
	}
}

func TestSyncGateway_RemoteNamesCannotEscape(t *testing.T) {
	g := newSyncRig(t)
	box := g.mkSandboxDir(syncRemote)
	sy, _, root := g.syncer(syncRemote, true)
	putSyncFile(t, filepath.Join(box, `..\evil`), "x", 0o644)
	putSyncFile(t, filepath.Join(box, "ok"), "x", 0o644)
	rep := mustPass(t, sy)
	if !slices.Equal(rep.Pulled, []string{"ok"}) || len(rep.Refused) != 1 {
		t.Errorf("report = %+v", rep)
	}
	if ents, _ := os.ReadDir(filepath.Dir(root)); len(ents) > 1 {
		for _, e := range ents {
			if strings.Contains(e.Name(), "evil") {
				t.Errorf("an escaped file %s exists", e.Name())
			}
		}
	}
}

func TestSyncGateway_RefusedDirectoryIsExitOneRightAfterOpen(t *testing.T) {
	g := newSyncRig(t)
	g.mkSandboxDir("/home/agent")
	_, err := openSyncSession(g.dial(), "/home/agent", "push")
	if err == nil || !strings.Contains(err.Error(), "WARDYN_SYNC_DIR") {
		t.Fatalf("err = %v, want the gateway's own refusal text", err)
	}
	if outcome, _ := g.audit.row(t); outcome != "failure" {
		t.Errorf("audit outcome = %s", outcome)
	}
}

func TestSyncGateway_WrongStartDirectoryAborts(t *testing.T) {
	g := newSyncRig(t)
	g.mkSandboxDir("/home/agent/work")
	_, err := openSyncSession(g.dial(), "/home/agent/work/missing", "push")
	if err == nil || !strings.Contains(err.Error(), "started in /, not /home/agent/work/missing") {
		t.Fatalf("err = %v, want the start-directory abort", err)
	}
}

func TestSyncGateway_HostKeyMustMatchTheAdvertisedFingerprint(t *testing.T) {
	g := newSyncRig(t)
	g.gw.fingerprint = "SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	if _, err := dialSyncGateway(context.Background(), g.gw, g.runID, g.signer); err == nil || !strings.Contains(err.Error(), "not the advertised") {
		t.Fatalf("err = %v, want a host key refusal", err)
	}
}

// The whole command: /healthz, the CLI key in $HOME, the gateway, the engine,
// state outside the tree.
func TestSyncCommand_EndToEnd(t *testing.T) {
	g := newSyncRig(t)
	box := g.mkSandboxDir(syncRemote)
	home := t.TempDir()
	t.Setenv("HOME", home)
	keyPath := filepath.Join(home, ".wardyn", "id_ed25519")
	pub, _, err := ensureLocalSSHKey(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	// Register the CLI's own key with the gateway's store.
	g.st.register(g.st.run.CreatedBy, pub)

	web := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"ssh": map[string]any{
			"enabled": true, "advertise_addr": net.JoinHostPort(g.gw.host, g.gw.port), "host_key_fingerprint": g.gw.fingerprint,
		}})
	}))
	t.Cleanup(web.Close)
	local := t.TempDir()
	putSyncFile(t, filepath.Join(local, "main.go"), "package main", 0o644)

	cmd := syncCmd(func() *sdk.Client { return &sdk.Client{BaseURL: web.URL, HTTPClient: web.Client()} })
	var out strings.Builder
	cmd.SetOut(&out)
	cmd.SetArgs([]string{g.runID, local, "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	var rep syncReport
	if err := json.Unmarshal([]byte(out.String()), &rep); err != nil || !slices.Equal(rep.Pushed, []string{"main.go"}) {
		t.Fatalf("output %q: %v %+v", out.String(), err, rep)
	}
	if getSyncFile(t, filepath.Join(box, "main.go")) != "package main" {
		t.Error("main.go did not arrive")
	}
	if _, err := os.Stat(filepath.Join(home, ".local", "state", "wardyn", "sync", g.runID+".json")); err != nil {
		t.Errorf("state file: %v", err)
	}
	if outcome, _ := g.audit.row(t); outcome != "success" {
		t.Errorf("audit outcome = %s", outcome)
	}
}

func TestSyncCommand_RefusesBeforeConnecting(t *testing.T) {
	local := t.TempDir()
	for want, args := range map[string][]string{
		"invalid run id":             {"not-a-uuid", local},
		"is not a directory":         {testRunID, filepath.Join(local, "nope")},
		"must not contain a '..'":    {testRunID, local, "--remote-dir", "/home/agent/../etc"},
		"must be under /home/agent/": {testRunID, local, "--remote-dir", "/etc"},
		"must be an absolute path":   {testRunID, local, "--remote-dir", "work"},
	} {
		cmd := syncCmd(func() *sdk.Client { return &sdk.Client{BaseURL: "http://127.0.0.1:1"} })
		cmd.SetArgs(args)
		cmd.SilenceUsage, cmd.SilenceErrors = true, true
		if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%v: err = %v, want %q before any connection", args, err, want)
		}
	}
}

func TestSyncGateway_WatchPicksUpLaterChanges(t *testing.T) {
	old := syncWatchEvery
	syncWatchEvery = 20 * time.Millisecond
	t.Cleanup(func() { syncWatchEvery = old })
	g := newSyncRig(t)
	box := g.mkSandboxDir(syncRemote)
	sy, sess, root := g.syncer(syncRemote, false)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	cmd := syncCmd(nil)
	var out strings.Builder
	cmd.SetOut(&out)
	go func() { done <- runSyncLoop(ctx, cmd, sy, syncFlags{watch: true, asJSON: true}) }()
	putSyncFile(t, filepath.Join(root, "late.txt"), "arrived later", 0o644)
	for deadline := time.Now().Add(3 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if b, err := os.ReadFile(filepath.Join(box, "late.txt")); err == nil && string(b) == "arrived later" {
			break
		} else if time.Now().After(deadline) {
			t.Fatal("--watch never pushed the new file")
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := sess.close(); err != nil {
		t.Fatal(err)
	}
}

func TestDialProxyCommandExpandsTheTokens(t *testing.T) {
	c, err := dialProxyCommand(context.Background(), "printf '%s' '%h:%p:%r:%%'", "gw.example", "2222", "run-1")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	b, _ := io.ReadAll(c)
	if string(b) != "gw.example:2222:run-1:%" {
		t.Errorf("proxy output = %q", b)
	}
}
