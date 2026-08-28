// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// ─── harness ────────────────────────────────────────────────────────────────

// runFilesStore is a one-run store.Store: every other method panics through the
// embedded nil Store, the sshMemStore/notFoundStore convention in this package.
type runFilesStore struct {
	store.Store
	run types.AgentRun
}

func (s runFilesStore) GetRun(_ context.Context, id uuid.UUID) (types.AgentRun, error) {
	if id != s.run.ID {
		return types.AgentRun{}, store.ErrNotFound
	}
	return s.run, nil
}

// newRunFilesHarness wires a Server around ONE running run and the shared
// sshFakeRunner (whose execFn is exactly the ExecStream behaviour this test
// wants — see its doc in sshgateway_test.go).
func newRunFilesHarness(execFn func(spec runner.ExecSpec) (*runner.ExecSession, error)) (*Server, *sshFakeRunner, *recRecorder, types.AgentRun) {
	run := types.AgentRun{
		ID:         uuid.New(),
		CreatedBy:  "sub-owner@corp.example",
		State:      types.RunRunning,
		SandboxRef: "sandbox-abc",
	}
	fr := &sshFakeRunner{execFn: execFn}
	audit := &recRecorder{}
	srv := New(Config{Store: runFilesStore{run: run}, Runner: fr, Audit: audit})
	return srv, fr, audit, run
}

// doRunFiles calls the handler directly with the chi URL param the route would
// have bound. ctxMod (nil for the default admin-ish caller: no OIDC session on
// ctx means isOperator says yes) layers a session onto the request context the
// way humanOrAdminAuth would.
func doRunFiles(srv *Server, id uuid.UUID, ctxMod func(context.Context) context.Context) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, "/api/v1/runs/"+id.String()+"/files", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", id.String())
	ctx := context.WithValue(r.Context(), chi.RouteCtxKey, rctx)
	if ctxMod != nil {
		ctx = ctxMod(ctx)
	}
	w := httptest.NewRecorder()
	srv.handleRunFiles(w, r.WithContext(ctx))
	return w
}

func decodeRunFiles(t *testing.T, w *httptest.ResponseRecorder) runFilesResponse {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
	}
	var got runFilesResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode body %q: %v", w.Body.String(), err)
	}
	return got
}

func fileByPath(t *testing.T, resp runFilesResponse, path string) runFileStat {
	t.Helper()
	for _, f := range resp.Files {
		if f.Path == path {
			return f
		}
	}
	t.Fatalf("no file %q in %+v", path, resp.Files)
	return runFileStat{}
}

// ─── tests ──────────────────────────────────────────────────────────────────

// TestRunFiles_JoinsNumstatAndStatus is the ordinary case: the +/− counts come
// from numstat, the status letter from porcelain, and an UNTRACKED file (which
// numstat never lists at all) still shows up — joined on path across the
// separator. It also pins the exec shape: one /bin/sh -c, workspace dir via
// Env, never interpolated into the script.
func TestRunFiles_JoinsNumstatAndStatus(t *testing.T) {
	stdout := strings.Join([]string{
		// Section 0: the directory the script settled on. The response echoes
		// it so a vcs=none answer can name where it looked.
		"path=/home/agent/work",
		runFilesSeparator,
		"12\t3\tinternal/api/run_files.go",
		"0\t7\told.go",
		runFilesSeparator,
		" M internal/api/run_files.go",
		" D old.go",
		"?? scratch.txt",
		"",
	}, "\n")
	srv, fr, audit, run := newRunFilesHarness(func(runner.ExecSpec) (*runner.ExecSession, error) {
		return fakeExecSession(stdout, "", 0), nil
	})

	resp := decodeRunFiles(t, doRunFiles(srv, run.ID, nil))
	if resp.VCS != runFilesVCSGit || resp.Truncated {
		t.Fatalf("vcs/truncated = %q/%v, want %q/false", resp.VCS, resp.Truncated, runFilesVCSGit)
	}
	if len(resp.Files) != 3 {
		t.Fatalf("files = %+v, want 3", resp.Files)
	}
	changed := fileByPath(t, resp, "internal/api/run_files.go")
	if changed.Status != "M" || changed.Added == nil || *changed.Added != 12 || changed.Deleted == nil || *changed.Deleted != 3 {
		t.Errorf("modified file = %+v, want status M +12/-3", changed)
	}
	deleted := fileByPath(t, resp, "old.go")
	if deleted.Status != "D" || deleted.Added == nil || *deleted.Added != 0 || deleted.Deleted == nil || *deleted.Deleted != 7 {
		t.Errorf("deleted file = %+v, want status D +0/-7", deleted)
	}
	// Untracked: porcelain knows it, numstat does not — status yes, counts ABSENT
	// (a count we never read is not zero).
	untracked := fileByPath(t, resp, "scratch.txt")
	if untracked.Status != "??" || untracked.Added != nil || untracked.Deleted != nil {
		t.Errorf("untracked file = %+v, want status ?? with no counts", untracked)
	}

	argv, env := fr.lastCall()
	if len(argv) != 3 || argv[0] != "/bin/sh" || argv[1] != "-c" || argv[2] != runFilesScript {
		t.Errorf("argv = %q, want /bin/sh -c <runFilesScript>", argv)
	}
	// W is the mount target; R is the repo clone's leaf under it (empty here: the
	// seeded run has no repo). Both are read by runFilesScript, in that order.
	if len(env) != 2 || env[0] != "W="+composerWorkspaceTarget || env[1] != "R=" {
		t.Errorf("env = %q, want [W=<workspace dir> R=]", env)
	}
	// Audit on FAILURE only: a polled read must not write a row per tick.
	if len(audit.events) != 0 {
		t.Errorf("successful read wrote %d audit events, want 0", len(audit.events))
	}
}

// TestRunFiles_BinaryIsNotZeroZero: git prints "-\t-" for a file it declined to
// count. Reporting that as +0/−0 would claim a rewritten binary asset changed by
// nothing — it must come back binary:true with the count fields ABSENT.
func TestRunFiles_BinaryIsNotZeroZero(t *testing.T) {
	stdout := "path=/home/agent/work\n" + runFilesSeparator + "\n-\t-\tdocs/logo.png\n" + runFilesSeparator + "\nA  docs/logo.png\n"
	srv, _, _, run := newRunFilesHarness(func(runner.ExecSpec) (*runner.ExecSession, error) {
		return fakeExecSession(stdout, "", 0), nil
	})

	w := doRunFiles(srv, run.ID, nil)
	resp := decodeRunFiles(t, w)
	if len(resp.Files) != 1 {
		t.Fatalf("files = %+v, want 1", resp.Files)
	}
	f := resp.Files[0]
	if !f.Binary || f.Added != nil || f.Deleted != nil || f.Status != "A" {
		t.Fatalf("binary file = %+v, want binary:true, no counts, status A", f)
	}
	// The wire, not just the struct: absent means absent, no zero-valued keys.
	if body := w.Body.String(); strings.Contains(body, `"added"`) || strings.Contains(body, `"deleted"`) {
		t.Errorf("binary row serialized counts: %s", body)
	}
}

// TestRunFiles_NotAGitWorkTree: git exiting nonzero is a FACT about the
// workspace (no repo here), not a failure — 200, vcs=none, an empty (never
// null) list, and no audit row.
func TestRunFiles_NotAGitWorkTree(t *testing.T) {
	// The script still prints the directory it looked in before exiting 3 —
	// that is the whole point of reporting Path (see below).
	srv, _, audit, run := newRunFilesHarness(func(runner.ExecSpec) (*runner.ExecSession, error) {
		return fakeExecSession("path=/srv/custom-target\n", "fatal: not a git repository\n", 3), nil
	})

	w := doRunFiles(srv, run.ID, nil)
	resp := decodeRunFiles(t, w)
	if resp.VCS != runFilesVCSNone || len(resp.Files) != 0 || resp.Truncated {
		t.Fatalf("resp = %+v, want vcs=none with an empty file list", resp)
	}
	if !strings.Contains(w.Body.String(), `"files":[]`) {
		t.Errorf("files serialized as null, not []: %s", w.Body.String())
	}
	// NAMING THE PATH is what makes a wrong answer legible. The mount target is
	// configurable per workspace source (workspace_run.go), so "not a git
	// repository" on its own cannot be told apart from "we looked in the wrong
	// directory" — which, silently, is the worst failure this endpoint has.
	if resp.Path != "/srv/custom-target" {
		t.Errorf("path = %q, want the inspected directory echoed back", resp.Path)
	}
	if len(audit.events) != 0 {
		t.Errorf("vcs=none wrote %d audit events, want 0 (it is not a failure)", len(audit.events))
	}
}

// TestRunFiles_ExecStreamUnsupported: a runner with no exec primitive cannot be
// asked this question at all — 501 naming the reason, plus the one audit row
// (this IS a failure).
func TestRunFiles_ExecStreamUnsupported(t *testing.T) {
	srv, _, audit, run := newRunFilesHarness(nil) // nil execFn => ErrExecStreamUnsupported

	w := doRunFiles(srv, run.ID, nil)
	if w.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501 (body %s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "does not support") {
		t.Errorf("501 body does not carry the reason: %s", w.Body.String())
	}
	if len(audit.events) != 1 || audit.events[0].Action != "run.files" || audit.events[0].Outcome != "failure" {
		t.Fatalf("audit = %+v, want one run.files/failure row", audit.events)
	}
}

// TestRunFiles_ForeignRun404: a member who did not create this run gets the
// byte-identical 404 a missing run would — no existence oracle — and the
// sandbox is never touched.
func TestRunFiles_ForeignRun404(t *testing.T) {
	execCalled := false
	srv, _, _, run := newRunFilesHarness(func(runner.ExecSpec) (*runner.ExecSession, error) {
		execCalled = true
		return fakeExecSession("", "", 0), nil
	})

	member := func(ctx context.Context) context.Context {
		return withOIDCRole(withOIDCHuman(ctx, "sub-someone-else@corp.example"), oidc.RoleMember)
	}
	w := doRunFiles(srv, run.ID, member)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body %s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "run not found") {
		t.Errorf("body = %s, want the same wording a missing run gets", w.Body.String())
	}
	if execCalled {
		t.Error("a foreign run reached ExecStream: the gate runs BEFORE the sandbox read")
	}
}

// TestRunFiles_CapSetsTruncated: past runFilesMaxFiles the response says it
// stopped counting. A short list presented as the whole truth is the failure
// mode this pins.
func TestRunFiles_CapSetsTruncated(t *testing.T) {
	var b strings.Builder
	fmt.Fprintf(&b, "path=/home/agent/work\n%s\n", runFilesSeparator)
	const rows = runFilesMaxFiles + 100
	for i := 0; i < rows; i++ {
		fmt.Fprintf(&b, "1\t1\tfile%04d.go\n", i)
	}
	srv, _, _, run := newRunFilesHarness(func(runner.ExecSpec) (*runner.ExecSession, error) {
		return fakeExecSession(b.String(), "", 0), nil
	})

	resp := decodeRunFiles(t, doRunFiles(srv, run.ID, nil))
	if len(resp.Files) != runFilesMaxFiles {
		t.Fatalf("files = %d, want the %d cap", len(resp.Files), runFilesMaxFiles)
	}
	if !resp.Truncated {
		t.Error("truncated = false with rows dropped: the cap must be visible in the response")
	}
}

// TestRunFiles_StderrDrainedConcurrently is the HANG test. fakeExecSession
// backs the session with UNBUFFERED io.Pipes (the real docker demux's shape)
// and writes stderr BEFORE any stdout byte — exactly what git does when HEAD is
// missing or the repo is odd. A handler that reads Stdout before starting a
// Stderr drain blocks the single demux goroutine and never returns; this test
// fails on a timer instead of wedging the suite.
func TestRunFiles_StderrDrainedConcurrently(t *testing.T) {
	stdout := "path=/home/agent/work\n" + runFilesSeparator + "\n4\t0\tnotes.md\n" + runFilesSeparator + "\n M notes.md\n"
	stderr := strings.Repeat("warning: this stderr is written first and nobody buffered it\n", 200)
	srv, _, _, run := newRunFilesHarness(func(runner.ExecSpec) (*runner.ExecSession, error) {
		return fakeExecSession(stdout, stderr, 0), nil
	})

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- doRunFiles(srv, run.ID, nil) }()
	select {
	case w := <-done:
		resp := decodeRunFiles(t, w)
		f := fileByPath(t, resp, "notes.md")
		if f.Status != "M" || f.Added == nil || *f.Added != 4 {
			t.Fatalf("file = %+v, want status M +4", f)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("handler hung: stderr was not drained concurrently with stdout (runner.ExecSession's streaming contract)")
	}
}

// TestRunFilesScript_FindsClonedRepoUnderWorkDir runs the REAL inspection script
// against a workspace laid out the way agent-run lays out a --repo run: the mount
// target W is a plain directory and the clone is its child. Before the
// candidate ordering in runFilesScript existed this reported vcs=none at W —
// the console's files widget then claimed "no git repository" for a sandbox
// holding a full clone one level down.
func TestRunFilesScript_FindsClonedRepoUnderWorkDir(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	w := t.TempDir()
	repo := filepath.Join(w, "hello-world")
	// A SECOND child repo that sorts before hello-world: with it present, only
	// R= can explain the script picking hello-world — the child glob alone
	// would pick a-first. Delete the ${R:+…} candidate and this test fails.
	decoy := filepath.Join(w, "a-first")
	for _, args := range [][]string{
		{"init", "-q", repo},
		{"-C", repo, "-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "init"},
		{"init", "-q", decoy},
		{"-C", decoy, "-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "init"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run := func(env ...string) (string, int) {
		cmd := exec.Command("/bin/sh", "-c", runFilesScript)
		cmd.Dir = t.TempDir() // the exec's own cwd is NOT a work tree either
		cmd.Env = append(os.Environ(), env...)
		out, err := cmd.Output()
		code := 0
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		} else if err != nil {
			t.Fatalf("run script: %v", err)
		}
		return string(out), code
	}
	// R names the clone directly (the --repo run shape).
	out, code := run("W="+w, "R=hello-world")
	if code != 0 || !strings.HasPrefix(out, "path="+repo+"\n") {
		t.Fatalf("with R: code=%d out=%q, want exit 0 and path=%s", code, out, repo)
	}
	// No R (a workspace-sourced run): the child search finds the FIRST child
	// work tree in glob order — the decoy — which is exactly why R exists.
	out, code = run("W="+w, "R=")
	if code != 0 || !strings.HasPrefix(out, "path="+decoy+"\n") {
		t.Fatalf("without R: code=%d out=%q, want exit 0 and path=%s", code, out, decoy)
	}
	// A wrong R must not break the fallback ordering.
	out, code = run("W="+w, "R=nope")
	if code != 0 || !strings.HasPrefix(out, "path="+decoy+"\n") {
		t.Fatalf("wrong R: code=%d out=%q", code, out)
	}
	// Nothing anywhere: exit 3 and the honest path.
	empty := t.TempDir()
	out, code = run("W="+empty, "R=")
	if code != 3 || out != "path="+empty+"\n" {
		t.Fatalf("empty: code=%d out=%q, want exit 3 and path=%s", code, out, empty)
	}
}

func TestRepoCloneLeaf(t *testing.T) {
	for in, want := range map[string]string{
		"octocat/Hello-World": "Hello-World", "org/repo.git": "repo", "https://github.com/o/r.git": "r",
		"repo": "repo", "": "", "org/repo/": "repo",
	} {
		if got := repoCloneLeaf(in); got != want {
			t.Errorf("repoCloneLeaf(%q) = %q, want %q", in, got, want)
		}
	}
}
