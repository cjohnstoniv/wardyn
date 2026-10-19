// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package folders

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pkg/sftp"

	"github.com/cjohnstoniv/wardyn/internal/runner/remote"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
)

func put(t *testing.T, p, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// home is a root that looks like a person's home folder.
func home(t *testing.T) string {
	t.Helper()
	h, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	put(t, filepath.Join(h, "code", "app", "main.go"), "package main\n")
	put(t, filepath.Join(h, "code", "app", "README.md"), "hello")
	put(t, filepath.Join(h, "code", ".hidden", "x"), "x")
	put(t, filepath.Join(h, "code", ".ssh", "id_ed25519"), "secret")
	put(t, filepath.Join(h, "code", "notes.txt"), "n")
	outside := t.TempDir()
	put(t, filepath.Join(outside, "elsewhere.txt"), "e")
	if err := os.Symlink(outside, filepath.Join(h, "code", "link")); err != nil {
		t.Fatal(err)
	}
	return h
}

func newServer(t *testing.T, roots ...string) *Server {
	t.Helper()
	s, err := New(context.Background(), roots, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func refused(err error) bool {
	var werr *runnerwire.Error
	return errors.As(err, &werr) && werr.Code == runnerwire.CodeRefused
}

func TestNew_RefusesARelativeRootAndACredentialRoot(t *testing.T) {
	h := home(t)
	for _, root := range []string{"code", filepath.Join(h, "code", ".ssh"), filepath.Join(h, "missing")} {
		if _, err := New(context.Background(), []string{root}, t.TempDir()); err == nil {
			t.Errorf("New accepted the root %q", root)
		}
	}
	if _, err := New(context.Background(), nil, t.TempDir()); err == nil {
		t.Error("New accepted no root")
	}
}

func TestList_ShowsOnlyRealFoldersWithTheirCounts(t *testing.T) {
	h := home(t)
	s := newServer(t, h)
	top, err := s.list(context.Background(), runnerwire.ListFoldersArgs{})
	if err != nil || len(top.Folders) != 1 || top.Folders[0].Path != h {
		t.Fatalf("roots = %+v, %v", top, err)
	}
	got, err := s.list(context.Background(), runnerwire.ListFoldersArgs{Path: filepath.Join(h, "code")})
	if err != nil {
		t.Fatal(err)
	}
	// No file, no hidden or credential folder, and no symlink: only app.
	if len(got.Folders) != 1 || got.Folders[0].Name != "app" {
		t.Fatalf("folders = %+v, want app only", got.Folders)
	}
	if f := got.Folders[0]; f.Files != 2 || f.Bytes != int64(len("package main\n")+len("hello")) || f.More {
		t.Errorf("app = %+v, want 2 files and their bytes", f)
	}
	if got.Survey != nil {
		t.Error("a listing that asked for no survey carried one")
	}
}

func TestFolder_RefusesWhatIsOutsideALinkAwayOrHoldsCredentials(t *testing.T) {
	h := home(t)
	s := newServer(t, filepath.Join(h, "code"))
	for name, p := range map[string]string{
		"outside the root":    h,
		"through a symlink":   filepath.Join(h, "code", "link"),
		"a credential folder": filepath.Join(h, "code", ".ssh"),
		"not clean":           filepath.Join(h, "code") + "/app/../app",
		"relative":            "code/app",
		"missing":             filepath.Join(h, "code", "gone"),
		"a file":              filepath.Join(h, "code", "notes.txt"),
	} {
		if _, err := s.list(context.Background(), runnerwire.ListFoldersArgs{Path: p}); !refused(err) {
			t.Errorf("%s (%s): err = %v, want a refusal", name, p, err)
		}
	}
	if _, err := s.list(context.Background(), runnerwire.ListFoldersArgs{Path: filepath.Join(h, "code", "app")}); err != nil {
		t.Errorf("a folder inside the root was refused: %v", err)
	}
}

func TestList_SurveyCountsWhatAPassWouldSend(t *testing.T) {
	h := home(t)
	app := filepath.Join(h, "code", "app")
	put(t, filepath.Join(app, ".git", "HEAD"), "ref: refs/heads/main\n")
	put(t, filepath.Join(app, ".gitignore"), "build/\n")
	put(t, filepath.Join(app, "build", "out.bin"), "0123456789")
	put(t, filepath.Join(app, ".env.local"), "TOKEN=x")
	s := newServer(t, h)
	got, err := s.list(context.Background(), runnerwire.ListFoldersArgs{Path: app, Survey: true, Deny: []string{".env*"}})
	if err != nil {
		t.Fatal(err)
	}
	sv := got.Survey
	// main.go, README.md and .gitignore: .git always, build by .gitignore, .env.local by the organisation.
	if sv == nil || sv.Files != 3 || !slices.Equal(sv.LeftOut, []string{".git"}) || sv.Ignored != 1 {
		t.Fatalf("survey = %+v", sv)
	}
}

func TestCall_RefusesEveryMethodButTheFolderOnes(t *testing.T) {
	s := newServer(t, home(t))
	for _, m := range []string{runnerwire.MethodCreateSandbox, runnerwire.MethodExec, runnerwire.MethodAttach, runnerwire.MethodDeliverResident} {
		if _, err := s.Call(context.Background(), nil, 1, m, json.RawMessage(`{}`)); !refused(err) {
			t.Errorf("%s: err = %v, want a refusal", m, err)
		}
	}
}

// wire is a folder-only runner served over the in-memory loopback, and the org's substrate for it.
func wire(t *testing.T, s *Server) *remote.Substrate {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	org, run := runnerwire.Loopback()
	link := &remote.Link{}
	sub := remote.New("folders-test", link, remote.Options{})
	orgPeer := link.Attach(ctx, org)
	var peer *runnerwire.Peer
	peer = runnerwire.NewPeer(run, runnerwire.PeerConfig{OnCall: func(ctx context.Context, id uint32, method string, args json.RawMessage) (any, error) {
		return s.Call(ctx, peer, id, method, args)
	}})
	go func() { _ = peer.Run(ctx) }()
	t.Cleanup(func() { orgPeer.Close(); peer.Close() })
	data, _ := json.Marshal(runnerwire.Caps{Folders: true})
	if err := peer.Event(ctx, runnerwire.Event{Kind: runnerwire.EventCaps, Data: data}); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		if c, ok := sub.Caps(); ok && c.Folders {
			return sub
		}
		if time.Now().After(deadline) {
			t.Fatal("caps never arrived")
		}
	}
}

// sandbox is an in-memory sftp filesystem standing in for the sandbox, with /home/agent/work in it.
type sandbox struct{ h sftp.Handlers }

func newSandbox(t *testing.T) sandbox {
	t.Helper()
	sb := sandbox{h: sftp.InMemHandler()}
	if err := sb.client(t, "/").MkdirAll("/home/agent/work"); err != nil {
		t.Fatal(err)
	}
	return sb
}

// serve runs a sandbox sftp-server started in dir over rw.
func (sb sandbox) serve(rw io.ReadWriteCloser, dir string) *sftp.RequestServer {
	srv := sftp.NewRequestServer(rw, sb.h, sftp.WithStartDirectory(dir))
	go func() { _ = srv.Serve() }()
	return srv
}

func (sb sandbox) client(t *testing.T, dir string) *sftp.Client {
	t.Helper()
	toServer, fromClient := io.Pipe()
	toClient, fromServer := io.Pipe()
	sb.serve(struct {
		io.Reader
		io.WriteCloser
	}{toServer, fromServer}, dir)
	c, err := sftp.NewClientPipe(toClient, fromClient)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fromServer.Close(); _ = fromClient.Close(); _ = c.Close() })
	return c
}

func (sb sandbox) read(t *testing.T, p string) string {
	t.Helper()
	f, err := sb.client(t, "/").Open(p)
	if err != nil {
		t.Fatalf("open %s in the sandbox: %v", p, err)
	}
	defer f.Close()
	b, _ := io.ReadAll(f)
	return string(b)
}

// runPass runs one sync_folder pass, joining the runner's stream to the sandbox as the org does.
func runPass(t *testing.T, sub *remote.Substrate, sb sandbox, start string, args runnerwire.SyncFolderArgs) runnerwire.FolderPass {
	t.Helper()
	p, err := sub.SyncFolder(context.Background(), args)
	if err != nil {
		t.Fatalf("sync_folder: %v", err)
	}
	srv := sb.serve(p.Stream, start)
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	res, err := p.Wait(ctx)
	if err != nil {
		t.Fatalf("sync_folder_wait: %v", err)
	}
	return res
}

func TestSyncFolder_OverTheWireSeedsTheSandboxAndBringsChangesBack(t *testing.T) {
	h := home(t)
	app := filepath.Join(h, "code", "app")
	put(t, filepath.Join(app, ".git", "HEAD"), "ref: refs/heads/main\n")
	sub := wire(t, newServer(t, h))
	sb := newSandbox(t)
	run := uuid.New()

	in := runPass(t, sub, sb, "/home/agent/work", runnerwire.SyncFolderArgs{RunID: run, Path: app, RemoteDir: "/home/agent/work"})
	if in.Error != "" || in.Pushed != 2 {
		t.Fatalf("seed = %+v, want 2 files pushed", in)
	}
	if got := sb.read(t, "/home/agent/work/main.go"); got != "package main\n" {
		t.Errorf("sandbox main.go = %q", got)
	}
	if _, err := sb.client(t, "/").Stat("/home/agent/work/.git"); err == nil {
		t.Error(".git reached the sandbox")
	}

	// The agent edits a file; the pass at the end brings it back.
	f, err := sb.client(t, "/").OpenFile("/home/agent/work/README.md", os.O_WRONLY|os.O_TRUNC)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.Write([]byte("edited in the sandbox"))
	_ = f.Close()
	_ = sb.client(t, "/").Chtimes("/home/agent/work/README.md", time.Now(), time.Now().Add(time.Hour))
	back := runPass(t, sub, sb, "/home/agent/work", runnerwire.SyncFolderArgs{RunID: run, Path: app, RemoteDir: "/home/agent/work", Pull: true})
	if back.Error != "" || back.Pulled != 1 {
		t.Fatalf("back = %+v, want 1 file pulled", back)
	}
	if b, _ := os.ReadFile(filepath.Join(app, "README.md")); string(b) != "edited in the sandbox" {
		t.Errorf("laptop README.md = %q", b)
	}
}

func TestSyncFolder_RefusesASandboxWithoutTheFolderAndAPathOutsideTheRoots(t *testing.T) {
	h := home(t)
	sub := wire(t, newServer(t, filepath.Join(h, "code")))
	sb := newSandbox(t)
	app := filepath.Join(h, "code", "app")
	// sftp-server started elsewhere: the sandbox has no such folder.
	res := runPass(t, sub, sb, "/home/agent", runnerwire.SyncFolderArgs{RunID: uuid.New(), Path: app, RemoteDir: "/home/agent/work"})
	if !strings.Contains(res.Error, "has no folder /home/agent/work") || res.Pushed != 0 {
		t.Errorf("pass = %+v, want the missing folder refused", res)
	}
	for name, args := range map[string]runnerwire.SyncFolderArgs{
		"outside the roots": {RunID: uuid.New(), Path: h, RemoteDir: "/home/agent/work"},
		"a credential path": {RunID: uuid.New(), Path: filepath.Join(h, "code", ".ssh"), RemoteDir: "/home/agent/work"},
		"no run":            {Path: app, RemoteDir: "/home/agent/work"},
		"outside the agent": {RunID: uuid.New(), Path: app, RemoteDir: "/etc"},
	} {
		if _, err := sub.SyncFolder(context.Background(), args); !refused(err) {
			t.Errorf("%s: err = %v, want a refusal", name, err)
		}
	}
}
