// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package guestagent

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// sessionEnvPaths is a Desktop's session files in a test directory.
func sessionEnvPaths(t *testing.T) SessionEnvPaths {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "wardyn")
	return SessionEnvPaths{Dir: dir, Script: filepath.Join(dir, "session-env.zsh"),
		Status: filepath.Join(dir, "session-env.status"), Zshenv: filepath.Join(root, "zshenv")}
}

// guiFake stands in for launchctl: the variables set in the user's GUI session, and the commands run.
// asuserWorks false makes `launchctl asuser ... setenv` do nothing, as the design fears it may on a
// guest, so only the as-user form sets.
type guiFake struct {
	mu          sync.Mutex
	vars        map[string]string
	calls       [][]string
	asuserWorks bool
	// block, when set, holds the first setenv until it is closed; entered is closed when that setenv arrives.
	block   chan struct{}
	entered chan struct{}
	once    sync.Once
}

func newGUIFake(asuserWorks bool) *guiFake {
	return &guiFake{vars: map[string]string{}, asuserWorks: asuserWorks}
}

func (g *guiFake) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	argv := append([]string{name}, args...)
	g.mu.Lock()
	g.calls = append(g.calls, argv)
	g.mu.Unlock()
	// argv is launchctl [asuser UID [sudo -u #UID]] launchctl|(none) op ...
	rest := argv[1:]
	via := "direct"
	if len(rest) > 0 && rest[0] == "asuser" {
		via, rest = "asuser", rest[2:]
		if rest[0] == "/usr/bin/sudo" {
			via, rest = "user", rest[3:]
		}
		rest = rest[1:] // /bin/launchctl
	}
	switch rest[0] {
	case "setenv":
		if g.block != nil {
			g.once.Do(func() { close(g.entered) })
			<-g.block
		}
		if via != "asuser" || g.asuserWorks {
			g.mu.Lock()
			g.vars[rest[1]] = rest[2]
			g.mu.Unlock()
		}
	case "getenv":
		g.mu.Lock()
		defer g.mu.Unlock()
		return []byte(g.vars[rest[1]] + "\n"), nil
	}
	return nil, nil
}

func (g *guiFake) argv() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	var b strings.Builder
	for _, c := range g.calls {
		b.WriteString(strings.Join(c, "\x00") + "\n")
	}
	return b.String()
}

func (g *guiFake) setenvs() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	n := 0
	for _, c := range g.calls {
		if slices.Contains(c, "setenv") {
			n++
		}
	}
	return n
}

// seqConsole answers the console's owner from a list, the last answer for good.
type seqConsole struct {
	mu      sync.Mutex
	answers []fakeConsole
	n       int
}

func (c *seqConsole) Console(context.Context) (uint32, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	a := c.answers[min(c.n, len(c.answers)-1)]
	c.n++
	return a.uid, a.err
}

func TestSessionEnvCarriesTheRunsNonSecretEnv(t *testing.T) {
	env := map[string]string{
		"HTTPS_PROXY":               "http://proxy:3128",
		"ANTHROPIC_BASE_URL":        "http://proxy:3128/anthropic",
		"CLAUDE_CONFIG_DIR":         "/Users/agent/.claude-run",
		"SSL_CERT_FILE":             "/etc/ssl/certs/ca-certificates.crt", // a Linux path
		"WARDYN_CLAUDE_MANAGED_B64": "e30=",
		"WARDYN_MANAGED_FILES":      "[]",
		"PATH":                      "/usr/bin",
		"HOME":                      "/home/agent",
	}
	ca := CAEnv(MacCABundlePath, MacCAFullPath)
	got := SessionEnv(env, nil, ca)
	for _, want := range []string{"HTTPS_PROXY=http://proxy:3128", "ANTHROPIC_BASE_URL=http://proxy:3128/anthropic",
		"CLAUDE_CONFIG_DIR=/Users/agent/.claude-run", "SSL_CERT_FILE=" + MacCAFullPath, "NODE_EXTRA_CA_CERTS=" + MacCABundlePath} {
		if !slices.Contains(got, want) {
			t.Errorf("%q is not in the session env %v", want, got)
		}
	}
	for _, k := range []string{"WARDYN_CLAUDE_MANAGED_B64", "WARDYN_MANAGED_FILES", "PATH", "HOME"} {
		if slices.ContainsFunc(got, func(kv string) bool { return strings.HasPrefix(kv, k+"=") }) {
			t.Errorf("%s is in the session env %v", k, got)
		}
	}
	if slices.Contains(got, "SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt") {
		t.Errorf("a Linux CA path reached the session: %v", got)
	}
	if !slices.IsSorted(got) {
		t.Errorf("the session env is not sorted: %v", got)
	}
}

func TestSessionEnvNeverCarriesSecretEnv(t *testing.T) {
	const canary = "ghp_canarycanarycanary"
	const collide = "collidecanary"
	env := map[string]string{"HTTPS_PROXY": "http://proxy:3128", "API_KEY": "public-placeholder", "WARDYN_RUN_ID": "run1"}
	secret := map[string]string{"GITHUB_TOKEN": canary, "API_KEY": collide}
	got := SessionEnv(env, secret, nil)
	joined := strings.Join(got, "\n")
	if strings.Contains(joined, canary) || strings.Contains(joined, collide) || strings.Contains(joined, "GITHUB_TOKEN") || strings.Contains(joined, "API_KEY") {
		t.Errorf("a secret key or value is in the session env: %v", got)
	}
	// The whole path: the file the shells source, the status line and every command line.
	p := sessionEnvPaths(t)
	g := newGUIFake(true)
	d := &Desktop{Run: g, Console: fakeConsole{uid: 502}}
	<-d.StartSessionEnv(context.Background(), p, 502, env, secret, nil)
	var files strings.Builder
	for _, f := range []string{p.Script, p.Status, p.Zshenv} {
		b, err := os.ReadFile(f)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		files.Write(b)
	}
	for _, where := range []string{files.String(), g.argv()} {
		if strings.Contains(where, canary) || strings.Contains(where, collide) || strings.Contains(where, "GITHUB_TOKEN") {
			t.Errorf("a secret reached the window's shells or GUI session: %q", where)
		}
	}
	if st, err := ReadSessionEnvStatus(p); err != nil || st.State != SessionEnvSet || st.Vars != 2 {
		t.Errorf("status = %+v, %v; want set with 2 variables (the secret's and the colliding key are out)", st, err)
	}
}

func TestSessionEnvSkipsUnsafeKeysAndValues(t *testing.T) {
	env := map[string]string{
		"GOOD":      "fine value",
		"bad key":   "x",
		"1BAD":      "x",
		"BAD-KEY":   "x",
		"":          "x",
		"NEWLINE":   "a\nb",
		"NUL":       "a\x00b",
		"TERM":      "xterm",
		"SHELL":     "/bin/bash",
		"TMPDIR":    "/tmp",
		"LOGNAME":   "agent",
		"USER":      "agent",
		"_UNDER_OK": "1",
	}
	got := SessionEnv(env, nil, nil)
	want := []string{"GOOD=fine value", "_UNDER_OK=1"}
	if !slices.Equal(got, want) {
		t.Errorf("SessionEnv = %q, want %q", got, want)
	}
}

// Variables a shell or the loader reads at start would let the run choose what another program loads; none
// reaches the shell file or the desktop session.
func TestSessionEnvRefusesShellAndLoaderStartupVariables(t *testing.T) {
	for _, k := range []string{"ZDOTDIR", "ENV", "BASH_ENV", "FPATH", "PS4", "PROMPT_COMMAND",
		"DYLD_INSERT_LIBRARIES", "DYLD_LIBRARY_PATH", "LD_PRELOAD", "LD_LIBRARY_PATH"} {
		if got := SessionEnv(map[string]string{k: "/tmp/x", "PLAIN": "ok"}, nil, nil); !slices.Equal(got, []string{"PLAIN=ok"}) {
			t.Errorf("%s: SessionEnv = %q, want only PLAIN=ok", k, got)
		}
	}
}

// The zsh hook is read by every zsh on the clone; it sources the run's settings only for the agent account,
// so root's and the admin's shells never load them.
func TestZshHookSourcesTheSettingsOnlyForTheAgentAccount(t *testing.T) {
	bash, err := exec.LookPath("bash") // bash, like zsh, has EUID
	if err != nil {
		t.Skip("no bash to read the hook with")
	}
	me := uint32(os.Geteuid())
	for _, c := range []struct {
		name string
		uid  uint32
		want string
	}{
		{"the agent account", me, "ok"},
		{"another account", me + 1, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := sessionEnvPaths(t)
			d := &Desktop{Run: newGUIFake(true), Console: fakeConsole{err: ErrNoConsoleUser}, SessionEnvEvery: time.Millisecond, SessionEnvWait: time.Millisecond}
			<-d.StartSessionEnv(context.Background(), p, c.uid, map[string]string{"PLAIN": "ok"}, nil, nil)
			got, err := exec.Command(bash, "-c", `. "$1"; printf %s "$PLAIN"`, "bash", p.Zshenv).Output()
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != c.want {
				t.Errorf("PLAIN through the hook as uid %d (agent account %d) = %q, want %q", me, c.uid, got, c.want)
			}
		})
	}
}

// /var, /tmp and /etc are links on macOS (to /private/...), and the root-only write walk refuses any link
// on the way (walkDir), so the paths it writes never name one.
func TestSessionEnvPathsNameNoLinkedMacDirectory(t *testing.T) {
	p := DefaultSessionEnvPaths()
	for _, path := range []string{p.Dir, p.Script, p.Status, p.Zshenv, MacVNCSettingsPath, MacItemStageDir, MacToolsBinDir} {
		for _, link := range []string{"/var", "/tmp", "/etc"} {
			if path == link || strings.HasPrefix(path, link+"/") {
				t.Errorf("%s is under %s, a link on macOS", path, link)
			}
		}
	}
	// The walk with a linked parent, as /var is: the write through the link is refused, through its target it goes in.
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "private", "var", "run"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("private", "var"), filepath.Join(root, "var")); err != nil {
		t.Fatal(err)
	}
	at := func(dir string) SessionEnvPaths {
		return SessionEnvPaths{Dir: dir, Script: dir + "/session-env.zsh", Status: dir + "/session-env.status", Zshenv: filepath.Join(root, "zshenv")}
	}
	if err := SessionEnvFile(at(filepath.Join(root, "var", "run", "wardyn")), []string{"A=b"}); !errors.Is(err, ErrUnsafeParent) {
		t.Errorf("a write through a linked parent = %v, want ErrUnsafeParent", err)
	}
	if err := SessionEnvFile(at(filepath.Join(root, "private", "var", "run", "wardyn")), []string{"A=b"}); err != nil {
		t.Errorf("a write through the link's target: %v", err)
	}
}

// stockLayout is a test root with a stock guest's modes on both places the settings could go: /Library and
// the /Library/Wardyn stage 1 makes, 0755, and /private/var/run, which a stock guest has as root:daemon
// 0775 (here the test's own group, which the walk does not trust either: skipIfTrustedGroup).
func stockLayout(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	mkdirMode(t, mkdirMode(t, filepath.Join(root, "Library"), 0o755)+"/Wardyn", 0o755)
	mkdirMode(t, mkdirMode(t, mkdirMode(t, filepath.Join(root, "private"), 0o755)+"/var", 0o755)+"/run", 0o775)
	return root
}

// skipIfTrustedGroup skips where dir's group is one the walk trusts, as a BSD directory under a wheel one is.
func skipIfTrustedGroup(t *testing.T, dir string) {
	t.Helper()
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if gid := fi.Sys().(*syscall.Stat_t).Gid; slices.Contains(trustedGroupGIDs, gid) {
		t.Skipf("the test's group %d is one the walk trusts", gid)
	}
}

// startUnder runs a run's settings and one desktop view status write with the guest's files in dir under
// root, and returns the paths.
func startUnder(t *testing.T, root, dir string) (SessionEnvPaths, string) {
	t.Helper()
	p := SessionEnvPaths{Dir: root + dir, Script: root + dir + "/session-env.zsh", Status: root + dir + "/session-env.status",
		Zshenv: filepath.Join(root, "zshenv")}
	d := &Desktop{Run: newGUIFake(true), Console: fakeConsole{uid: 502}, ViewStatus: root + dir + "/desktop-view.status"}
	<-d.StartSessionEnv(context.Background(), p, 502, map[string]string{"WARDYN_RUN_ID": "r1"}, nil, nil)
	d.writeViewStatus("view=console")
	return p, d.ViewStatus
}

// The settings file and the two status files live beside the pairing key, in a directory root's alone
// (0755) under the root:wheel 0755 one stage 1 makes: on a stock layout every write goes in, and the zsh
// hook names the new file.
func TestSessionEnvFilesLiveUnderARootOnlyParent(t *testing.T) {
	if MacSessionEnvDir != "/Library/Wardyn/run" || filepath.Dir(MacSessionEnvDir) != filepath.Dir(MacPairKeyPath) {
		t.Fatalf("MacSessionEnvDir = %s, want /Library/Wardyn/run, beside the pairing key", MacSessionEnvDir)
	}
	for _, p := range []string{MacSessionEnvPath, MacSessionEnvStatusPath, MacDesktopViewStatusPath, DefaultSessionEnvPaths().Script} {
		if filepath.Dir(p) != MacSessionEnvDir {
			t.Errorf("%s is not in %s", p, MacSessionEnvDir)
		}
	}
	root := stockLayout(t)
	p, view := startUnder(t, root, MacSessionEnvDir)
	fi, err := os.Lstat(p.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if st := fi.Sys().(*syscall.Stat_t); !fi.IsDir() || st.Uid != trustedOwnerUID || fi.Mode().Perm() != 0o755 {
		t.Errorf("%s is uid %d mode %v, want root's directory at 0755", p.Dir, st.Uid, fi.Mode())
	}
	for _, f := range []string{p.Script, p.Status, view} {
		if fi, err := os.Lstat(f); err != nil || fi.Mode().Perm() != 0o644 {
			t.Errorf("%s = %v, %v; want written at 0644", f, fi, err)
		}
	}
	if b, err := os.ReadFile(p.Status); err != nil || string(b) != "session_env=set vars=1 via=asuser\n" {
		t.Errorf("status = %q, %v", b, err)
	}
	if b, err := os.ReadFile(p.Zshenv); err != nil || !strings.Contains(string(b), "] && . "+p.Script+" "+sessionEnvHookMarker) {
		t.Errorf("zshenv = %q, %v; want the hook sourcing %s", b, err, p.Script)
	}
}

// The walk is the same at the new place: a /Library/Wardyn that a group it does not trust can write, as
// daemon's /private/var/run is, refuses every write, and the file is never sourced.
func TestSessionEnvRefusesADaemonStyleParent(t *testing.T) {
	root := stockLayout(t)
	skipIfTrustedGroup(t, root+"/Library/Wardyn")
	if err := os.Chmod(root+"/Library/Wardyn", 0o775); err != nil {
		t.Fatal(err)
	}
	p, view := startUnder(t, root, MacSessionEnvDir)
	if err := SessionEnvFile(p, []string{"A=b"}); !errors.Is(err, ErrUnsafeParent) || !strings.Contains(err.Error(), `/Library/Wardyn" is writable by gid`) {
		t.Errorf("the settings file under a group-writable parent = %v", err)
	}
	for _, f := range []string{p.Dir, p.Zshenv, view} {
		if _, err := os.Lstat(f); err == nil {
			t.Errorf("%s exists after the walk refused its parent", f)
		}
	}
}

// The old place, /private/var/run/wardyn, on a stock layout: the walk refuses /private/var/run, so the
// settings file, the zsh hook (a restored Terminal's zsh got no run settings) and both status files (the
// control plane could not say where the settings stood) were never written.
func TestTheOldSessionEnvDirIsRefusedOnAStockLayout(t *testing.T) {
	const oldDir = "/private/var/run/wardyn"
	root := stockLayout(t)
	skipIfTrustedGroup(t, root+"/private/var/run")
	p, view := startUnder(t, root, oldDir)
	if err := SessionEnvFile(p, []string{"A=b"}); !errors.Is(err, ErrUnsafeParent) || !strings.Contains(err.Error(), `/private/var/run" is writable by gid`) {
		t.Errorf("the settings file at the old place = %v", err)
	}
	for _, f := range []string{p.Dir, p.Zshenv, view} {
		if _, err := os.Lstat(f); err == nil {
			t.Errorf("%s exists at the old place on a stock layout", f)
		}
	}
}

// With no agent account known (uid 0 is root's), the hook is not installed: root's zsh would source the run's
// settings.
func TestZshHookIsRefusedWithoutAnAgentAccount(t *testing.T) {
	p := sessionEnvPaths(t)
	if err := EnsureZshHook(p, 0); err == nil {
		t.Error("a hook for uid 0 was installed")
	}
	if b, err := os.ReadFile(p.Zshenv); err == nil && strings.Contains(string(b), sessionEnvHookMarker) {
		t.Errorf("zshenv carries the hook for uid 0: %q", b)
	}
}

func TestSessionEnvFileRoundTripsThroughAShell(t *testing.T) {
	p := sessionEnvPaths(t)
	env := []string{`Q=it's a "test"`, `CMD=$(id)`, "TICK=`id`", "SPACES=a  b   c", `BS=back\slash`, "PLAIN=ok"}
	if err := SessionEnvFile(p, env); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(p.Script); err != nil || fi.Mode().Perm() != 0o644 {
		t.Fatalf("script = %v, %v; want mode 0644", fi, err)
	}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		got, err := exec.Command("/bin/sh", "-c", `. "$1"; eval "printf %s \"\$$2\""`, "sh", p.Script, k).Output()
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != v {
			t.Errorf("%s came back %q, want %q", k, got, v)
		}
	}
}

// The hook sources a file every zsh on the clone reads, root's and the admin's included: the directory
// and the file must be root's and nobody else's to change.
func TestSessionEnvFileRefusesANonRootDir(t *testing.T) {
	p := sessionEnvPaths(t)
	setVar(t, &trustedOwnerUID, uint32(os.Getuid())+1) // the test user is no longer root's stand-in
	if err := os.MkdirAll(p.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := SessionEnvFile(p, []string{"A=b"}); err == nil {
		t.Fatal("wrote the shell file into a directory root does not own")
	}
	if _, err := os.Lstat(p.Script); err == nil {
		t.Error("the file exists after the refusal")
	}
	trustedOwnerUID = uint32(os.Getuid())
	if err := os.Chmod(p.Dir, 0o775); err != nil {
		t.Fatal(err)
	}
	if err := SessionEnvFile(p, []string{"A=b"}); err == nil {
		t.Error("wrote the shell file into a group-writable directory")
	}
	if err := os.Chmod(p.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := SessionEnvFile(p, []string{"A=b"}); err != nil {
		t.Errorf("a root directory at 0755 was refused: %v", err)
	}
}

func TestEnsureZshHookCreatesAppendsOnceAndRefusesASymlink(t *testing.T) {
	p := sessionEnvPaths(t)
	if err := EnsureZshHook(p, 502); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p.Zshenv)
	if err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(p.Zshenv); fi.Mode().Perm() != 0o644 {
		t.Errorf("created at %v, want 0644", fi.Mode().Perm())
	}
	if !strings.Contains(string(b), p.Script) || strings.Count(string(b), "# Wardyn run settings") != 1 {
		t.Errorf("hook = %q", b)
	}
	if err := EnsureZshHook(p, 502); err != nil {
		t.Fatal(err)
	}
	if again, _ := os.ReadFile(p.Zshenv); string(again) != string(b) {
		t.Errorf("a second call changed the file: %q", again)
	}
	// A person's own lines stay; the hook is appended once, after a newline.
	if err := os.WriteFile(p.Zshenv, []byte("export FOO=1"), 0o644); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := EnsureZshHook(p, 502); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := os.ReadFile(p.Zshenv)
	if !strings.HasPrefix(string(got), "export FOO=1\n") || strings.Count(string(got), "# Wardyn run settings") != 1 {
		t.Errorf("zshenv = %q", got)
	}
	// A link is refused, and what it points at is untouched.
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := sessionEnvPaths(t)
	if err := os.Symlink(target, link.Zshenv); err != nil {
		t.Fatal(err)
	}
	if err := EnsureZshHook(link, 502); err == nil {
		t.Error("followed a symlink")
	}
	if b, _ := os.ReadFile(target); string(b) != "keep" {
		t.Errorf("the link's target was written: %q", b)
	}
	// So is anything that is not a regular file.
	dir := sessionEnvPaths(t)
	if err := os.Mkdir(dir.Zshenv, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := EnsureZshHook(dir, 502); err == nil {
		t.Error("accepted a directory as the zsh file")
	}
}

func TestApplySessionEnvWaitsForTheAgentAtTheConsole(t *testing.T) {
	env := []string{"WARDYN_RUN_ID=r1", "HTTPS_PROXY=http://p"}
	run := func(c ConsoleUser, ctx context.Context) (*guiFake, SessionEnvStatus) {
		g := newGUIFake(true)
		d := &Desktop{Run: g, Console: c}
		return g, d.applySessionEnv(ctx, sessionEnvPaths(t), 502, env, time.Millisecond, 150*time.Millisecond)
	}
	t.Run("no console twice, then the agent", func(t *testing.T) {
		c := &seqConsole{answers: []fakeConsole{{err: ErrNoConsoleUser}, {err: ErrNoConsoleUser}, {uid: 502}}}
		g, st := run(c, context.Background())
		if st.State != SessionEnvSet || st.Vars != 2 || st.Via != "asuser" {
			t.Errorf("status = %+v", st)
		}
		if n := g.setenvs(); n != 2 {
			t.Errorf("setenv ran %d times, want once per variable (2)", n)
		}
		if c.n < 3 {
			t.Errorf("the console was asked %d times, want 3", c.n)
		}
	})
	t.Run("a cancelled context sets nothing", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		g, st := run(fakeConsole{uid: 502}, ctx)
		if g.setenvs() != 0 || st.State == SessionEnvSet {
			t.Errorf("setenv ran %d times, status %+v", g.setenvs(), st)
		}
	})
	t.Run("another user at the console sets nothing", func(t *testing.T) {
		g, st := run(fakeConsole{uid: 501}, context.Background())
		if g.setenvs() != 0 {
			t.Errorf("setenv ran %d times for another user's session", g.setenvs())
		}
		if st.State != SessionEnvFailed || st.Reason != SessionEnvNotSignedIn {
			t.Errorf("status = %+v, want failed not_signed_in", st)
		}
	})
	t.Run("nobody ever signs in", func(t *testing.T) {
		_, st := run(fakeConsole{err: ErrNoConsoleUser}, context.Background())
		if st.State != SessionEnvFailed || st.Reason != SessionEnvNotSignedIn {
			t.Errorf("status = %+v, want failed not_signed_in", st)
		}
	})
}

// The window must not look ready while the setter is still going: the status the host reads stays
// pending until the setter returns.
func TestApplySessionEnvStaysPendingWhileTheSetterIsBlocked(t *testing.T) {
	p := sessionEnvPaths(t)
	g := newGUIFake(true)
	g.block, g.entered = make(chan struct{}), make(chan struct{})
	d := &Desktop{Run: g, Console: fakeConsole{uid: 502}}
	done := d.StartSessionEnv(context.Background(), p, 502, map[string]string{"WARDYN_RUN_ID": "r1"}, nil, nil)
	st, err := ReadSessionEnvStatus(p)
	if err != nil || st.State != SessionEnvPending {
		t.Fatalf("status right after the start = %+v, %v; want pending", st, err)
	}
	select {
	case <-g.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the setter never ran")
	}
	if st, err := ReadSessionEnvStatus(p); err != nil || st.State != SessionEnvPending {
		t.Fatalf("status with the setter blocked = %+v, %v; want pending", st, err)
	}
	close(g.block)
	<-done
	if st, err := ReadSessionEnvStatus(p); err != nil || st.State != SessionEnvSet || st.Via != "asuser" {
		t.Errorf("status after the setter = %+v, %v; want set via asuser", st, err)
	}
}

// A setter that never returns does not hold the status at pending past the wait: the step ends, and the
// status says failed with a reason, so the host can say so and offer the retry.
func TestApplySessionEnvFailsWhenTheSetterOutlastsTheWait(t *testing.T) {
	p := sessionEnvPaths(t)
	g := newGUIFake(true)
	g.block, g.entered = make(chan struct{}), make(chan struct{})
	d := &Desktop{Run: g, Console: fakeConsole{uid: 502}, SessionEnvEvery: time.Millisecond, SessionEnvWait: 200 * time.Millisecond}
	done := d.StartSessionEnv(context.Background(), p, 502, map[string]string{"WARDYN_RUN_ID": "r1"}, nil, nil)
	t.Cleanup(func() { close(g.block); <-done })
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the settings step is still going with the setter blocked past the wait")
	}
	if st, err := ReadSessionEnvStatus(p); err != nil || st.State != SessionEnvFailed || st.Reason != SessionEnvSetenvFailed {
		t.Errorf("status with the setter blocked past the wait = %+v, %v; want failed setenv_failed", st, err)
	}
}

// The console can change hands between the wait and the set (a fast user switch). The settings go only to
// the agent account the wait saw, never to whoever is at the console when the setter looks again.
func TestApplySessionEnvNeverSetsInAnotherUsersSessionAfterASwitch(t *testing.T) {
	g := newGUIFake(true)
	c := &seqConsole{answers: []fakeConsole{{uid: 502}, {uid: 501}}}
	d := &Desktop{Run: g, Console: c}
	st := d.applySessionEnv(context.Background(), sessionEnvPaths(t), 502, []string{"WARDYN_RUN_ID=r1", "HTTPS_PROXY=http://p"}, time.Millisecond, 150*time.Millisecond)
	for _, call := range strings.Split(strings.TrimSpace(g.argv()), "\n") {
		if slices.Contains(strings.Split(call, "\x00"), "501") || slices.Contains(strings.Split(call, "\x00"), "#501") {
			t.Errorf("a launchctl call reached uid 501's session: %q", strings.ReplaceAll(call, "\x00", " "))
		}
	}
	if st.State == SessionEnvSet {
		t.Errorf("status = %+v after the console switched to another user, want not set", st)
	}
}

func TestSetSessionEnvRetriesAsTheUserWhenGetenvDisagrees(t *testing.T) {
	g := newGUIFake(false) // the asuser form does not take
	d := &Desktop{Run: g, Console: fakeConsole{uid: 502}}
	via, err := d.SetSessionEnv(context.Background(), 502, []string{"HTTPS_PROXY=http://p", "WARDYN_RUN_ID=r1"})
	if err != nil || via != "user" {
		t.Fatalf("via %q, err %v; want the as-user form to take", via, err)
	}
	retry := []string{"/bin/launchctl", "asuser", "502", "/usr/bin/sudo", "-u", "#502", "/bin/launchctl", "setenv", "WARDYN_RUN_ID", "r1"}
	if !slices.ContainsFunc(g.calls, func(c []string) bool { return slices.Equal(c, retry) }) {
		t.Errorf("no as-user setenv of WARDYN_RUN_ID in %v", g.calls)
	}
	last := g.calls[len(g.calls)-1]
	if last[len(last)-2] != "getenv" || last[len(last)-1] != "WARDYN_RUN_ID" {
		t.Errorf("the redo was not checked: last call %v", last)
	}
	// When neither form takes, the error names the step and holds no value.
	never := &guiFake{vars: map[string]string{}}
	nd := &Desktop{Run: neverTakes{never}, Console: fakeConsole{uid: 502}}
	if _, err := nd.SetSessionEnv(context.Background(), 502, []string{"WARDYN_RUN_ID=secretish-value"}); err == nil || strings.Contains(err.Error(), "secretish-value") {
		t.Errorf("err = %v, want a failure that holds no value", err)
	}
}

// neverTakes is a launchctl whose setenv never sticks.
type neverTakes struct{ g *guiFake }

func (n neverTakes) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	if slices.Contains(args, "setenv") {
		return nil, nil
	}
	return n.g.Run(ctx, name, args...)
}

func TestSessionEnvStatusNamesTheOutcomeAndNoValue(t *testing.T) {
	for _, c := range []struct {
		st   SessionEnvStatus
		line string
	}{
		{SessionEnvStatus{State: SessionEnvPending}, "session_env=pending"},
		{SessionEnvStatus{State: SessionEnvSet, Vars: 12, Via: "asuser"}, "session_env=set vars=12 via=asuser"},
		{SessionEnvStatus{State: SessionEnvSet, Vars: 3, Via: "user"}, "session_env=set vars=3 via=user"},
		{SessionEnvStatus{State: SessionEnvFailed, Reason: SessionEnvNotSignedIn}, "session_env=failed reason=not_signed_in"},
		{SessionEnvStatus{State: SessionEnvFailed, Reason: SessionEnvSetenvFailed}, "session_env=failed reason=setenv_failed"},
	} {
		if got := c.st.Line(); got != c.line {
			t.Errorf("Line = %q, want %q", got, c.line)
		}
		p := sessionEnvPaths(t)
		if err := os.MkdirAll(p.Dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p.Status, []byte(c.line+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if got, err := ReadSessionEnvStatus(p); err != nil || got != c.st {
			t.Errorf("read %q = %+v, %v; want %+v", c.line, got, err, c.st)
		}
	}
	p := sessionEnvPaths(t)
	_ = os.MkdirAll(p.Dir, 0o755)
	for _, junk := range []string{"", "session_env=maybe", "session_env=set vars=x via=asuser", "other=set", "session_env=failed reason=a b"} {
		_ = os.WriteFile(p.Status, []byte(junk), 0o644)
		if got, err := ReadSessionEnvStatus(p); err == nil {
			t.Errorf("read %q = %+v, want an error", junk, got)
		}
	}
	if _, err := ReadSessionEnvStatus(sessionEnvPaths(t)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a missing status file: err = %v, want not-exist", err)
	}
}

// The retry a person runs from the window's Terminal reapplies the root-owned file's settings as
// themselves, with no asuser, and records the outcome.
func TestSessionEnvRetryReappliesTheFileAsTheUser(t *testing.T) {
	p := sessionEnvPaths(t)
	env := []string{`Q=it's a "test"`, "WARDYN_RUN_ID=r1", "TICK=`id` $(id)"}
	if err := SessionEnvFile(p, env); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.Status, []byte("session_env=failed reason=not_signed_in\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	g := newGUIFake(false)
	st, err := SessionEnvRetry(context.Background(), p, g)
	if err != nil || st.State != SessionEnvSet || st.Vars != 3 || st.Via != "retry" {
		t.Fatalf("retry = %+v, %v", st, err)
	}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		if g.vars[k] != v {
			t.Errorf("%s = %q in the session, want %q", k, g.vars[k], v)
		}
	}
	for _, c := range g.calls {
		if slices.Contains(c, "asuser") {
			t.Errorf("the retry used asuser: %v", c)
		}
	}
	if got, err := ReadSessionEnvStatus(p); err != nil || got.State != SessionEnvSet {
		t.Errorf("status after the retry = %+v, %v", got, err)
	}
}

func TestAgentRunSessionEnvRetryPrintsTheOutcome(t *testing.T) {
	p := sessionEnvPaths(t)
	if err := SessionEnvFile(p, []string{"WARDYN_RUN_ID=r1"}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.Status, []byte("session_env=failed reason=not_signed_in\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	setVar(t, &sessionEnvRetryPaths, p)
	setVar[CmdRunner](t, &sessionEnvRetryRun, newGUIFake(false))
	var out, errOut strings.Builder
	a := &AgentRun{Env: map[string]string{}, Stdout: &out, Stderr: &errOut}
	if code := a.Main([]string{"--session-env-retry"}); code != 0 || strings.TrimSpace(out.String()) != "session_env=set vars=1 via=retry" {
		t.Errorf("exit %d, stdout %q, stderr %q", code, out.String(), errOut.String())
	}
}
