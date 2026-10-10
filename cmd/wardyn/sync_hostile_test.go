// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// A hostile or compromised sandbox, daemon or /healthz: what each of these
// must not be able to make the laptop do.

func healthzFor(addr, proxy string) string {
	b, _ := json.Marshal(map[string]any{"ssh": map[string]any{
		"enabled": true, "advertise_addr": addr, "host_key_fingerprint": "SHA256:x", "proxy_command": proxy,
	}})
	return string(b)
}

func TestResolveSSHGateway_RefusesAnAddressThatIsNotAHostAndPort(t *testing.T) {
	for addr, want := range map[string]string{
		"gw$(touch x):22":                "gw$(touch x)",
		"gw`id`:22":                      "gw`id`",
		"gw;rm:22":                       "gw;rm",
		"gw\nProxyCommand evil:22":       `gw\nProxyCommand evil`,
		"gw host:22":                     "gw host",
		"-oProxyCommand=x:22":            "-oProxyCommand=x",
		"gw.example.com:99999":           "99999",
		"gw.example.com:0":               `"0"`,
		"gw.example.com:+22":             "+22",
		"gw.example.com:2x2":             "2x2",
		strings.Repeat("a", 254) + ":22": "not a DNS name",
	} {
		srv := fakeHealthzServer(t, healthzFor(addr, ""))
		_, err := resolveSSHGateway(context.Background(), &sdk.Client{BaseURL: srv.URL})
		srv.Close()
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("advertise_addr %q: err = %v, want a refusal naming %q", addr, err, want)
		}
	}
	for _, addr := range []string{"gw.example.com:2222", "gw.example.com", "10.0.0.5:22", "[::1]:2222", "2001:db8::1", "localhost:22", "a-b.example:22"} {
		srv := fakeHealthzServer(t, healthzFor(addr, ""))
		_, err := resolveSSHGateway(context.Background(), &sdk.Client{BaseURL: srv.URL})
		srv.Close()
		if err != nil {
			t.Errorf("advertise_addr %q refused: %v", addr, err)
		}
	}
}

func TestSyncCommand_HostPoCNeverRunsAndRunSSHConfigRefusesANewlineHost(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "pwned")
	srv := fakeHealthzServer(t, healthzFor("gw$(touch "+marker+"):22", "true %h %p"))
	defer srv.Close()
	home := t.TempDir()
	t.Setenv("HOME", home)
	if _, _, err := ensureLocalSSHKey(filepath.Join(home, ".wardyn", "id_ed25519")); err != nil {
		t.Fatal(err)
	}
	cmd := syncCmd(func() *sdk.Client { return &sdk.Client{BaseURL: srv.URL} })
	cmd.SetArgs([]string{testRunID, t.TempDir(), "--advertised-proxy"})
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "not a DNS name") {
		t.Fatalf("err = %v, want the host refused", err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the advertised host ran a command on the laptop")
	}

	out, err := runSSHWith(t, healthzFor("gw\nProxyCommand evil:22", ""), "--config")
	if err == nil || strings.Contains(out, "ProxyCommand") {
		t.Errorf("run ssh --config with a newline host: out=%q err=%v, want a refusal and no output", out, err)
	}
}

func TestSyncCommand_ConsentErrorShowsTheExpandedCommand(t *testing.T) {
	srv := fakeHealthzServer(t, healthzFor("gw.example.com:2222", "nc %h %p %r"))
	defer srv.Close()
	cmd := syncCmd(func() *sdk.Client { return &sdk.Client{BaseURL: srv.URL} })
	cmd.SetArgs([]string{testRunID, t.TempDir()})
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "nc gw.example.com 2222 "+testRunID) || !strings.Contains(err.Error(), "--advertised-proxy") {
		t.Errorf("err = %v, want the command as it would run", err)
	}
}

func TestSyncCommand_RefusesPlainHTTPToANonLoopbackServer(t *testing.T) {
	for url, refused := range map[string]bool{
		"http://wardyn.example.com:8080": true,
		"http://10.1.2.3":                true,
		"http://localhost:8080":          false,
		"http://127.0.0.1:8080":          false,
		"http://[::1]:8080":              false,
		"https://wardyn.example.com":     false,
	} {
		err := syncRefusePlainHTTP(url)
		if refused != (err != nil) || (err != nil && !strings.Contains(err.Error(), "/healthz")) {
			t.Errorf("%s: err = %v, want refused=%v with the reason", url, err, refused)
		}
	}
	cmd := syncCmd(func() *sdk.Client { return &sdk.Client{BaseURL: "http://wardyn.example.invalid"} })
	cmd.SetArgs([]string{testRunID, t.TempDir()})
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "plain http") {
		t.Errorf("err = %v, want the plain-http refusal before any request", err)
	}
}

func TestSyncGateway_PullSizeLimits(t *testing.T) {
	t.Run("one file over 256 MiB is refused unread and the rest still pull", func(t *testing.T) {
		g := newSyncRig(t)
		box := g.mkSandboxDir(syncRemote)
		big := filepath.Join(box, "big.bin")
		putSyncFile(t, big, "", 0o644)
		if err := os.Truncate(big, maxPullFile+1); err != nil {
			t.Fatal(err)
		}
		putSyncFile(t, filepath.Join(box, "small.txt"), "ok", 0o644)
		sy, _, root := g.syncer(syncRemote, true)
		rep := mustPass(t, sy)
		if _, err := os.Stat(filepath.Join(root, "big.bin")); err == nil {
			t.Error("an over-size file was pulled")
		}
		if !slices.Equal(rep.Pulled, []string{"small.txt"}) || len(rep.Refused) != 1 || !strings.HasPrefix(rep.Refused[0].Reason, "too large") || rep.BytesIn != 2 {
			t.Errorf("report = %+v", rep)
		}
	})
	t.Run("a pass has a byte budget", func(t *testing.T) {
		g := newSyncRig(t)
		box := g.mkSandboxDir(syncRemote)
		putSyncFile(t, filepath.Join(box, "a.txt"), "123456", 0o644)
		putSyncFile(t, filepath.Join(box, "b.txt"), "123456", 0o644)
		sy, _, _ := g.syncer(syncRemote, true)
		sy.o.maxPullBytes = 10
		rep := mustPass(t, sy)
		if !slices.Equal(rep.Pulled, []string{"a.txt"}) || len(rep.Refused) != 1 || rep.Refused[0].Path != "b.txt" || rep.BytesIn != 6 {
			t.Errorf("report = %+v", rep)
		}
	})
	t.Run("the bytes read count, not the size the sandbox lists", func(t *testing.T) {
		g := newSyncRig(t)
		box := g.mkSandboxDir(syncRemote)
		putSyncFile(t, filepath.Join(box, "liar.bin"), strings.Repeat("x", 100), 0o644)
		g.rn.liar = map[string]int64{"liar.bin": 1}
		sy, _, root := g.syncer(syncRemote, true)
		sy.o.maxPullBytes = 50
		rep := mustPass(t, sy)
		if _, err := os.Stat(filepath.Join(root, "liar.bin")); err == nil {
			t.Error("a file larger than it listed was pulled past the budget")
		}
		if len(rep.Pulled) != 0 || len(rep.Refused) != 1 || rep.BytesIn != 51 {
			t.Errorf("report = %+v", rep)
		}
		if left, _ := os.ReadDir(root); len(left) != 0 {
			t.Errorf("a temp file was left behind: %v", left)
		}
	})
}

func TestSyncGateway_OnePathProblemDoesNotAbortThePass(t *testing.T) {
	t.Run("a sandbox directory where the laptop has a file", func(t *testing.T) {
		g := newSyncRig(t)
		box := g.mkSandboxDir(syncRemote)
		sy, _, root := g.syncer(syncRemote, false)
		putSyncFile(t, filepath.Join(root, "clash"), "file here", 0o644)
		putSyncFile(t, filepath.Join(root, "fine.txt"), "fine", 0o644)
		putSyncFile(t, filepath.Join(box, "clash/inner"), "dir there", 0o644)
		rep := mustPass(t, sy)
		if !slices.Equal(rep.Pushed, []string{"fine.txt"}) || len(rep.Refused) != 1 || rep.Refused[0].Path != "clash" {
			t.Errorf("report = %+v", rep)
		}
		if got := mustPass(t, sy); len(got.Pushed) != 0 {
			t.Errorf("the next pass = %+v", got)
		}
	})
	t.Run("a sandbox file the agent user cannot read", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root reads mode 000")
		}
		g := newSyncRig(t)
		box := g.mkSandboxDir(syncRemote)
		putSyncFile(t, filepath.Join(box, "locked"), "secret", 0o000)
		putSyncFile(t, filepath.Join(box, "ok.txt"), "ok", 0o644)
		sy, _, _ := g.syncer(syncRemote, true)
		rep := mustPass(t, sy)
		if !slices.Equal(rep.Pulled, []string{"ok.txt"}) || len(rep.Refused) != 1 || rep.Refused[0].Path != "locked" {
			t.Errorf("report = %+v", rep)
		}
	})
	t.Run("a sandbox directory it cannot list", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root lists mode 000")
		}
		g := newSyncRig(t)
		box := g.mkSandboxDir(syncRemote)
		putSyncFile(t, filepath.Join(box, "closed/x"), "x", 0o644)
		_ = os.Chmod(filepath.Join(box, "closed"), 0o000)
		t.Cleanup(func() { _ = os.Chmod(filepath.Join(box, "closed"), 0o755) })
		putSyncFile(t, filepath.Join(box, "ok.txt"), "ok", 0o644)
		sy, _, _ := g.syncer(syncRemote, true)
		rep := mustPass(t, sy)
		if !slices.Equal(rep.Pulled, []string{"ok.txt"}) || len(rep.Refused) != 1 || rep.Refused[0].Path != "closed" {
			t.Errorf("report = %+v", rep)
		}
	})
	t.Run("a local entry in the way of a sandbox file", func(t *testing.T) {
		g := newSyncRig(t)
		box := g.mkSandboxDir(syncRemote)
		sy, _, root := g.syncer(syncRemote, true)
		_ = os.MkdirAll(filepath.Join(root, "d"), 0o755)
		putSyncFile(t, filepath.Join(box, "d"), "a file", 0o644)
		putSyncFile(t, filepath.Join(box, "ok.txt"), "ok", 0o644)
		rep := mustPass(t, sy)
		if !slices.Equal(rep.Pulled, []string{"ok.txt"}) || len(rep.Refused) != 1 || rep.Refused[0].Path != "d" {
			t.Errorf("report = %+v", rep)
		}
	})
}

func TestSyncAbortIsOnlyForAConnectionOrAnInterrupt(t *testing.T) {
	ctx := context.Background()
	if syncAbort(ctx, os.ErrPermission) || syncAbort(ctx, &os.PathError{Op: "open", Err: os.ErrExist}) {
		t.Error("a per-path error aborts the pass")
	}
	if !syncAbort(ctx, errors.New("connection lost")) {
		t.Error("an unknown error is not treated as the connection going away")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if !syncAbort(cancelled, os.ErrPermission) {
		t.Error("an interrupt does not abort")
	}
}

func TestSyncFoldAndInvisibleNames(t *testing.T) {
	if syncFold("cafe\u0301.txt") != syncFold("caf\u00e9.TXT") {
		t.Error("NFD and NFC spellings with different case have different keys")
	}
	if syncFold("ǅ") != syncFold("ǆ") || syncFold("Straße") != syncFold("STRASSE") {
		t.Error("full case folding is not applied")
	}
	for _, name := range []string{".g\u200cit", ".git\u200d", "a\u00adb", "x\ufe0f", "\u2060a", "a\u034fb"} {
		if !syncInvisible(name) || syncEntryName(name) == nil {
			t.Errorf("%q is not refused as invisible", name)
		}
	}
	for _, name := range []string{"plain", "caf\u00e9", "日本語", ".git"} {
		if syncInvisible(name) {
			t.Errorf("%q is refused as invisible", name)
		}
	}
	if !syncDenied(".GIT/config") || !syncDenied("a/.vscode/TASKS.json") {
		t.Error("folded denylist match failed")
	}
	got := syncCollisions([]string{"dir/cafe\u0301.txt", "dir/caf\u00e9.txt", "other"})
	if len(got) != 2 {
		t.Errorf("NFD/NFC spellings are not a collision: %v", got)
	}
}

func TestSyncGateway_AnotherSpellingOrInvisibleNameNeverReachesTheLaptop(t *testing.T) {
	g := newSyncRig(t)
	box := g.mkSandboxDir(syncRemote)
	sy, _, root := g.syncer(syncRemote, true)
	putSyncFile(t, filepath.Join(root, "caf\u00e9.txt"), "my edit", 0o644)
	putSyncFile(t, filepath.Join(box, "cafe\u0301.txt"), "from the sandbox", 0o644)
	putSyncFile(t, filepath.Join(box, ".g\u200cit/config"), "[core]\nfsmonitor = x", 0o644)
	putSyncFile(t, filepath.Join(box, "ok.txt"), "ok", 0o644)
	rep := mustPass(t, sy)
	if got := getSyncFile(t, filepath.Join(root, "caf\u00e9.txt")); got != "my edit" {
		t.Errorf("the laptop's file was overwritten: %q", got)
	}
	if _, err := os.Stat(filepath.Join(root, ".g\u200cit")); err == nil {
		t.Error("an invisible-character .git reached the laptop")
	}
	if !slices.Equal(rep.Pulled, []string{"ok.txt"}) {
		t.Errorf("pulled = %v", rep.Pulled)
	}
}

func TestSyncGateway_PullRefusesALocalFileTheWalkNeverListed(t *testing.T) {
	g := newSyncRig(t)
	box := g.mkSandboxDir(syncRemote)
	sy, _, root := g.syncer(syncRemote, true)
	putSyncFile(t, filepath.Join(root, "x.txt"), "local", 0o644)
	putSyncFile(t, filepath.Join(box, "x.txt"), "remote", 0o644)
	fi, err := sy.c.Stat(syncRemote + "/x.txt")
	if err != nil {
		t.Fatal(err)
	}
	rep := newSyncReport()
	pulled := int64(0)
	if err := sy.pull(context.Background(), "x.txt", fi, nil, rep, &pulled); err != nil {
		t.Fatal(err)
	}
	if got := getSyncFile(t, filepath.Join(root, "x.txt")); got != "local" || len(rep.Pulled) != 0 || len(rep.Refused) != 1 {
		t.Errorf("file = %q, report = %+v", got, rep)
	}
}

func TestSyncGateway_PullDoesNotOverwriteAFileSavedAfterTheWalk(t *testing.T) {
	g := newSyncRig(t)
	box := g.mkSandboxDir(syncRemote)
	sy, _, root := g.syncer(syncRemote, true)
	putSyncFile(t, filepath.Join(root, "x.txt"), "saved after the walk", 0o644)
	putSyncFile(t, filepath.Join(box, "x.txt"), "remote", 0o644)
	fi, _ := sy.c.Stat(syncRemote + "/x.txt")
	stale := &syncFile{side: syncSide{Size: 3, MTime: 1}, mode: 0o644}
	rep := newSyncReport()
	pulled := int64(0)
	if err := sy.pull(context.Background(), "x.txt", fi, stale, rep, &pulled); err != nil {
		t.Fatal(err)
	}
	if got := getSyncFile(t, filepath.Join(root, "x.txt")); got != "saved after the walk" || !slices.Equal(rep.Conflicts, []string{"x.txt"}) || len(rep.Pulled) != 0 {
		t.Errorf("file = %q, report = %+v", got, rep)
	}
	if left, _ := os.ReadDir(root); len(left) != 1 {
		t.Errorf("a temp file was left behind: %v", left)
	}
}

func TestSyncGateway_LocalWritesStayInsideTheRoot(t *testing.T) {
	g := newSyncRig(t)
	g.mkSandboxDir(syncRemote)
	sy, _, root := g.syncer(syncRemote, true)
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "swapped")); err != nil {
		t.Fatal(err)
	}
	if _, f, err := sy.localTemp("swapped/x"); err == nil {
		_ = f.Close()
		t.Error("a temp file was created through a symlink that leaves the root")
	}
	if ents, _ := os.ReadDir(outside); len(ents) != 0 {
		t.Errorf("something was written outside the root: %v", ents)
	}
	if err := sy.root.MkdirAll("swapped/deeper", 0o755); err == nil {
		t.Error("a directory was created through a symlink that leaves the root")
	}
}

// A sandbox that stalls mid-pass: Ctrl-C (the context ending) must end the
// command, not wait on the sandbox.
func TestSyncCommand_InterruptEndsAPassStuckOnTheSandbox(t *testing.T) {
	g := newSyncRig(t)
	box := g.mkSandboxDir(syncRemote)
	putSyncFile(t, filepath.Join(box, "slow.bin"), "listed as a regular file, read never returns", 0o644)
	g.rn.hangRead = "slow.bin"
	cmd := g.command(t, t.TempDir(), "--pull")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- cmd.ExecuteContext(ctx) }()
	time.Sleep(500 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "interrupted") {
			t.Errorf("err = %v, want interrupted", err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("the command did not end after the interrupt")
	}
}

// command builds `wardyn sync` against this rig, with the CLI's key in $HOME
// and a /healthz that advertises the rig's gateway.
func (g *syncRig) command(t *testing.T, local string, extra ...string) *cobra.Command {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	pub, _, err := ensureLocalSSHKey(filepath.Join(home, ".wardyn", "id_ed25519"))
	if err != nil {
		t.Fatal(err)
	}
	g.st.register(g.st.run.CreatedBy, pub)
	web := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"ssh": map[string]any{
			"enabled": true, "advertise_addr": net.JoinHostPort(g.gw.host, g.gw.port), "host_key_fingerprint": g.gw.fingerprint,
		}})
	}))
	t.Cleanup(web.Close)
	cmd := syncCmd(func() *sdk.Client { return &sdk.Client{BaseURL: web.URL, HTTPClient: web.Client()} })
	cmd.SetOut(&strings.Builder{})
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	cmd.SetArgs(append([]string{g.runID, local}, extra...))
	return cmd
}

func TestSyncGateway_ExitStatusIsWaitedForBeforeTheChannelCloses(t *testing.T) {
	g := newSyncRig(t)
	g.rn.exitDelay = 100 * time.Millisecond
	g.mkSandboxDir(syncRemote)
	sy, sess, root := g.syncer(syncRemote, false)
	putSyncFile(t, filepath.Join(root, "a.txt"), "a", 0o644)
	mustPass(t, sy)
	if err := sess.close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if outcome, data := g.audit.row(t); outcome != "success" {
		t.Errorf("audit row = %s %v: the channel was closed before the sandbox's exit-status", outcome, data)
	}
}

func TestSyncGateway_ANonZeroExitIsAFailure(t *testing.T) {
	g := newSyncRig(t)
	g.rn.exitCode = 3
	g.mkSandboxDir(syncRemote)
	_, sess, _ := g.syncer(syncRemote, false)
	err := sess.close()
	if err == nil || !strings.Contains(err.Error(), "exited 3") {
		t.Errorf("close = %v, want the exit status", err)
	}
	if outcome, _ := g.audit.row(t); outcome != "failure" {
		t.Errorf("audit outcome = %s", outcome)
	}
}

func TestSyncExitError(t *testing.T) {
	if syncExitError(0, "") != nil {
		t.Error("0 is a clean end")
	}
	if err := syncExitError(-1, ""); err == nil || !strings.Contains(err.Error(), "without an exit status") {
		t.Errorf("no exit status = %v, want a failure", err)
	}
	if err := syncExitError(3, " boom \n"); err == nil || !strings.Contains(err.Error(), "exited 3: boom") {
		t.Errorf("exit 3 = %v", err)
	}
}

func TestSyncGateway_PushNeverWritesThroughASandboxSymlink(t *testing.T) {
	g := newSyncRig(t)
	box := g.mkSandboxDir(syncRemote)
	target := filepath.Join(t.TempDir(), "target")
	putSyncFile(t, target, "untouched", 0o644)
	if err := os.Symlink(target, filepath.Join(box, "x")); err != nil {
		t.Fatal(err)
	}
	sy, _, root := g.syncer(syncRemote, false)
	putSyncFile(t, filepath.Join(root, "x"), "from the laptop", 0o644)
	putSyncFile(t, filepath.Join(root, "y"), "from the laptop", 0o644)
	rep := mustPass(t, sy)
	if got := getSyncFile(t, target); got != "untouched" {
		t.Errorf("a push went through the sandbox's symlink: %q", got)
	}
	if !slices.Equal(rep.Pushed, []string{"y"}) || len(rep.Refused) != 1 || rep.Refused[0].Path != "x" {
		t.Errorf("report = %+v: x must be refused only", rep)
	}
}

func TestSyncGateway_PulledFileModes(t *testing.T) {
	g := newSyncRig(t)
	box := g.mkSandboxDir(syncRemote)
	sy, _, root := g.syncer(syncRemote, true)
	for name, mode := range map[string]os.FileMode{"private": 0o600, "tool": 0o755, "group": 0o664} {
		putSyncFile(t, filepath.Join(root, name), "old", mode)
	}
	mustPass(t, sy)
	later := time.Now().Add(time.Hour)
	for _, name := range []string{"private", "tool", "group", "fresh"} {
		putSyncFile(t, filepath.Join(box, name), "new from the sandbox", 0o777)
		_ = os.Chtimes(filepath.Join(box, name), later, later)
	}
	rep := mustPass(t, sy)
	if len(rep.Pulled) != 4 {
		t.Fatalf("report = %+v", rep)
	}
	for name, want := range map[string]os.FileMode{"private": 0o600, "tool": 0o644, "group": 0o664, "fresh": 0o644} {
		fi, err := os.Stat(filepath.Join(root, name))
		if err != nil || fi.Mode() != want {
			t.Errorf("%s mode = %v, %v; want %v", name, fi.Mode(), err, want)
		}
	}
}
