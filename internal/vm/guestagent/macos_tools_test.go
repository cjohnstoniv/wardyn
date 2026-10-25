// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package guestagent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// fakeUpdateList is a `softwareupdate -l` output: two CLT labels (one in the pre-"Label:" form) and an
// OS update.
const fakeUpdateList = `Software Update Tool

Finding available software
Software Update found the following new or updated software:
* Label: Command Line Tools for Xcode-16.0
	Title: Command Line Tools for Xcode, Version: 16.0, Size: 751216KiB, Recommended: YES,
* Command Line Tools (macOS High Sierra version 10.13) for Xcode-10.1
* Label: macOS Sequoia 15.1-24B83
	Title: macOS Sequoia 15.1, Version: 15.1, Size: 3021586KiB, Recommended: YES, Action: restart,
`

// toolsRig is a Tools whose bin dir, agent files and settings file are in a test directory, and whose
// exec records instead of running (`softwareupdate -l` answers fakeUpdateList).
type toolsRig struct {
	t     *Tools
	bin   string
	mu    sync.Mutex
	ran   [][]string
	cmds  *fakeRunner
	agent string
}

func newToolsRig(t *testing.T) *toolsRig {
	t.Helper()
	dir := t.TempDir()
	rig := &toolsRig{bin: filepath.Join(dir, "bin"), cmds: &fakeRunner{out: func(a []string) ([]byte, error) { return []byte("10 20"), nil }}}
	rig.agent = filepath.Join(dir, "wardyn-guest-agent")
	d := &Desktop{Run: rig.cmds, Console: fakeConsole{uid: 502}, VNCSettings: filepath.Join(dir, "com.apple.VNCSettings.txt")}
	rig.t = &Tools{Desktop: d, BinDir: rig.bin, AgentBinary: rig.agent, AgentPlist: filepath.Join(dir, "agent.plist"),
		run: func(_ context.Context, argv []string) ([]byte, int, error) {
			rig.mu.Lock()
			rig.ran = append(rig.ran, argv)
			rig.mu.Unlock()
			if slices.Equal(argv, []string{"/usr/sbin/softwareupdate", "-l"}) {
				return []byte(fakeUpdateList), 0, nil
			}
			return []byte("git version 2.50.1 (Apple Git-155)\n"), 0, nil
		}}
	return rig
}

func (r *toolsRig) do(method, path, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r.t.ToolsHandler().ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(body)))
	return w
}

func (r *toolsRig) exec(argv ...string) *httptest.ResponseRecorder {
	b := `{"argv":[` + strings.Join(quoteAll(argv), ",") + `]}`
	return r.do("POST", "/v1/tools/exec", b)
}

func quoteAll(ss []string) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = fmt.Sprintf("%q", s)
	}
	return out
}

// The CLT's git runs with exactly `--version`. Each rejection control is refused before anything runs:
// another argv of that binary, the binary by another path (a ".." component, a link to it in the bin
// dir, a relative path), a shell, launchctl in any form, softwareupdate but `-l` or `-i` with a CLT
// label, and touch or rm on anything but the trigger file.
func TestToolsExecAllowsExactlyTheCLTGitVersion(t *testing.T) {
	rig := newToolsRig(t)
	if err := os.MkdirAll(rig.bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rig.bin, "claude"), []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(MacCLTGit, filepath.Join(rig.bin, "git")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/bin/sh", filepath.Join(rig.bin, "sh")); err != nil {
		t.Fatal(err)
	}
	refused := [][]string{
		{},
		{MacCLTGit},
		{MacCLTGit, "--help"},
		{MacCLTGit, "init"},
		{MacCLTGit, "--version", "--help"},
		{MacCLTGit, "-c", "core.pager=sh", "--version"},
		{MacCLTGit, "config", "--global", "core.sshCommand", "sh"},
		{"/Library/Developer/CommandLineTools/usr/bin/../bin/git", "--version"},
		{"/Library/Developer/CommandLineTools/usr//bin/git", "--version"},
		{filepath.Join(rig.bin, "git"), "--version"},
		{"git", "--version"},
		{"Library/Developer/CommandLineTools/usr/bin/git", "--version"},
		{"/bin/sh", "-c", MacCLTGit + " --version"},
		{"/bin/zsh", "-c", "id"},
		{filepath.Join(rig.bin, "sh"), "-c", "id"},
		{"/usr/bin/env", MacCLTGit, "--version"},
		{"/usr/bin/touch", "/etc/sudoers"},
		{"/usr/bin/touch", MacCLTTrigger, "/etc/sudoers"},
		{"/bin/rm", "-rf", "/"},
		{"/bin/rm", MacCLTTrigger},
		{"/bin/launchctl", "asuser", "0", "/bin/sh"},
		{"/bin/launchctl", "submit", "-l", "x", "--", "/bin/sh"},
		{"/bin/launchctl", "kickstart", "-k", "system/com.wardyn.guest-agent"},
		{"/usr/sbin/softwareupdate"},
		{"/usr/sbin/softwareupdate", "-l", "-a"},
		{"/usr/sbin/softwareupdate", "-i", "Command Line Tools for Xcode-16.0", "--restart"},
		{"/usr/sbin/softwareupdate", "-i", "macOS Sequoia 15.1-24B83"},
		{"/usr/sbin/softwareupdate", "-i", "-a"},
		{"/usr/sbin/softwareupdate", "-i", "Command Line Tools\n--all"},
		{"/usr/sbin/softwareupdate", "-i", "--all Command Line Tools"},
		{"/usr/sbin/softwareupdate", "--install-rosetta"},
		{"/usr/sbin/softwareupdate", "--schedule", "on"},
		{filepath.Join(rig.bin, "missing"), "--version"},
		{filepath.Join(rig.bin, "sub", "claude"), "--version"},
	}
	for _, argv := range refused {
		// ToolsExecAllowed alone too: handleExec's listed-label check would mask a loosened cltLabel.
		if err := ToolsExecAllowed(argv, rig.bin); !errors.Is(err, ErrToolsRefused) {
			t.Errorf("ToolsExecAllowed(%q) = %v, want ErrToolsRefused", argv, err)
		}
		if w := rig.exec(argv...); w.Code != http.StatusForbidden {
			t.Errorf("%q = %d %s, want 403", argv, w.Code, w.Body)
		}
	}
	if len(rig.ran) != 0 {
		t.Fatalf("a refused argv ran: %q", rig.ran)
	}
	allowed := [][]string{
		CLTGitArgv,
		{"/usr/sbin/softwareupdate", "-l"},
		{"/usr/sbin/softwareupdate", "-i", "Command Line Tools for Xcode-16.0"},
		{"/usr/bin/touch", MacCLTTrigger},
		{"/bin/rm", "-f", MacCLTTrigger},
		{filepath.Join(rig.bin, "claude"), "--version"},
	}
	for _, argv := range allowed {
		if w := rig.exec(argv...); w.Code != 200 {
			t.Errorf("%q = %d %s, want 200", argv, w.Code, w.Body)
		}
	}
	if !slices.EqualFunc(rig.ran, allowed, slices.Equal) {
		t.Errorf("ran %q, want %q", rig.ran, allowed)
	}
	w := rig.exec(CLTGitArgv...)
	if !strings.Contains(w.Body.String(), `"output":"git version 2.50.1`) || !strings.Contains(w.Body.String(), `"exit_code":0`) {
		t.Errorf("the CLT check's answer = %s", w.Body)
	}
}

// softwareupdate -i installs only a label this process's own `softwareupdate -l` listed: an -i before
// any -l, and an -i of a well-formed label that -l did not list, are refused before anything runs.
func TestToolsExecInstallsOnlyAListedCLTLabel(t *testing.T) {
	rig := newToolsRig(t)
	install := func(label string) []string { return []string{"/usr/sbin/softwareupdate", "-i", label} }
	if w := rig.exec(install("Command Line Tools for Xcode-16.0")...); w.Code != http.StatusForbidden {
		t.Errorf("-i before any -l = %d %s, want 403", w.Code, w.Body)
	}
	if len(rig.ran) != 0 {
		t.Fatalf("a refused -i ran: %q", rig.ran)
	}
	list := []string{"/usr/sbin/softwareupdate", "-l"}
	if w := rig.exec(list...); w.Code != 200 {
		t.Fatalf("-l = %d %s", w.Code, w.Body)
	}
	for _, label := range []string{"Command Line Tools for Xcode-16.4", "Command Line Tools for Xcode-16", "Command Line Tools"} {
		if w := rig.exec(install(label)...); w.Code != http.StatusForbidden {
			t.Errorf("-i of the unlisted %q = %d %s, want 403", label, w.Code, w.Body)
		}
	}
	allowed := [][]string{list, install("Command Line Tools for Xcode-16.0"),
		install("Command Line Tools (macOS High Sierra version 10.13) for Xcode-10.1")}
	for _, argv := range allowed[1:] {
		if w := rig.exec(argv...); w.Code != 200 {
			t.Errorf("%q after -l = %d %s, want 200", argv, w.Code, w.Body)
		}
	}
	if !slices.EqualFunc(rig.ran, allowed, slices.Equal) {
		t.Errorf("ran %q, want %q", rig.ran, allowed)
	}
}

func pushPath(dest string, mode string, size int, body []byte) string {
	sum := sha256.Sum256(body)
	return fmt.Sprintf("/v1/tools/push?dest=%s&mode=%s&size=%d&sha256=%s", dest, mode, size, hex.EncodeToString(sum[:]))
}

// A push lands as a file of the given mode only when its body is exactly size bytes with the given
// SHA-256, and only in the bin dir or on the agent's own files; anything else leaves dest as it was.
func TestToolsPush(t *testing.T) {
	rig := newToolsRig(t)
	body := []byte("#!/bin/true\n")
	dest := filepath.Join(rig.bin, "claude")
	if w := rig.do("POST", pushPath(dest, "755", len(body), body), string(body)); w.Code != 200 {
		t.Fatalf("push = %d %s", w.Code, w.Body)
	}
	if got, err := os.ReadFile(dest); err != nil || !bytes.Equal(got, body) {
		t.Fatalf("pushed file = %q, %v", got, err)
	}
	if fi, err := os.Stat(dest); err != nil || fi.Mode().Perm() != 0o755 {
		t.Errorf("pushed file: %v, want mode 0755", err)
	}
	bad := []struct {
		name, path, body string
		code             int
	}{
		{"sha mismatch", pushPath(dest, "755", 5, []byte("other")), "hello", http.StatusUnprocessableEntity},
		{"short body", pushPath(dest, "755", 6, []byte("hello!")), "hello", http.StatusBadRequest},
		{"setuid", pushPath(dest, "4755", 5, []byte("hello")), "hello", http.StatusBadRequest},
		{"world-writable", pushPath(dest, "777", 5, []byte("hello")), "hello", http.StatusBadRequest},
		{"group-writable", pushPath(dest, "775", 5, []byte("hello")), "hello", http.StatusBadRequest},
		{"a dot component under the bin dir", pushPath(rig.bin+"/x/../claude", "755", 5, []byte("hello")), "hello", http.StatusForbidden},
		{"a doubled slash", pushPath(rig.bin+"//claude", "755", 5, []byte("hello")), "hello", http.StatusForbidden},
		{"outside the bin dir", pushPath("/etc/sudoers", "644", 5, []byte("hello")), "hello", http.StatusForbidden},
		{"dot component", pushPath(rig.bin+"/../x", "755", 5, []byte("hello")), "hello", http.StatusForbidden},
		{"dot file", pushPath(rig.bin+"/.claude", "755", 5, []byte("hello")), "hello", http.StatusForbidden},
		{"relative", pushPath("bin/claude", "755", 5, []byte("hello")), "hello", http.StatusForbidden},
		{"no sha", "/v1/tools/push?dest=" + dest + "&mode=755&size=5", "hello", http.StatusBadRequest},
	}
	for _, c := range bad {
		if w := rig.do("POST", c.path, c.body); w.Code != c.code {
			t.Errorf("%s: push = %d %s, want %d", c.name, w.Code, w.Body, c.code)
		}
	}
	if got, _ := os.ReadFile(dest); !bytes.Equal(got, body) {
		t.Errorf("a refused push changed dest: %q", got)
	}
	if left, _ := filepath.Glob(filepath.Join(rig.bin, ".*")); len(left) != 0 {
		t.Errorf("temp files left: %v", left)
	}
	if w := rig.do("POST", pushPath(rig.agent, "755", 5, []byte("agent")), "agent"); w.Code != 200 {
		t.Errorf("push of the agent's own binary = %d %s", w.Code, w.Body)
	}
}

// replaceFile never follows a link at its destination: it replaces it.
func TestReplaceFileReplacesALinkNotItsTarget(t *testing.T) {
	dir := t.TempDir()
	target, dst := filepath.Join(dir, "target"), filepath.Join(dir, "dst")
	if err := os.WriteFile(target, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, dst); err != nil {
		t.Fatal(err)
	}
	old := syscall.Umask(0o077)
	defer syscall.Umask(old)
	if err := replaceFile(dst, 0o644, func(f *os.File) error { _, err := f.WriteString("new"); return err }); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(target); string(got) != "keep" {
		t.Errorf("the link's target changed: %q", got)
	}
	if fi, err := os.Lstat(dst); err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm() != 0o644 {
		t.Errorf("dst: %v; want a 0644 file despite the umask", err)
	}
	if err := replaceFile(dst, 0o600, func(*os.File) error { return errors.New("no") }); err == nil {
		t.Error("a failed fill was not an error")
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 2 {
		t.Errorf("a failed fill left %d entries", len(ents))
	}
}

// Tools mode's desktop_password writes the same settings file as Init's, runs no command that carries
// the password, and answers only whether it was set.
func TestToolsDesktopPasswordStep(t *testing.T) {
	rig := newToolsRig(t)
	w := rig.do("POST", "/v1/tools/desktop_password", `{"password":"Abcd2345"}`)
	if w.Code != 200 || strings.TrimSpace(w.Body.String()) != `{"desktop_password_set":true}` {
		t.Fatalf("desktop_password = %d %s", w.Code, w.Body)
	}
	if got, err := os.ReadFile(rig.t.Desktop.VNCSettings); err != nil || !bytes.Equal(got, VNCSettingsLine("Abcd2345")) {
		t.Errorf("settings file = %q, %v", got, err)
	}
	if fi, err := os.Stat(rig.t.Desktop.VNCSettings); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("settings file: %v, want mode 0600", err)
	}
	if _, held := rig.t.Desktop.Password(); held {
		t.Error("the step's password is held: only Init's run password is")
	}
	// The control check's second password replaces the first.
	if w := rig.do("POST", "/v1/tools/desktop_password", `{"password":"Zyxw9876"}`); !strings.Contains(w.Body.String(), "true") {
		t.Fatalf("second desktop_password = %s", w.Body)
	}
	if got, _ := os.ReadFile(rig.t.Desktop.VNCSettings); !bytes.Equal(got, VNCSettingsLine("Zyxw9876")) {
		t.Errorf("the second password did not replace the first: %q", got)
	}
	noArgvHolds(t, rig.cmds, "Abcd2345")
	noArgvHolds(t, rig.cmds, "Zyxw9876")
	w = rig.do("POST", "/v1/tools/desktop_password", `{"password":"Abcd23456"}`)
	if strings.TrimSpace(w.Body.String()) != `{"desktop_password_set":false}` {
		t.Errorf("a 9-byte password = %s", w.Body)
	}
	if s := fmt.Sprintf("%v %+v %#v %s", DesktopPassword{"Abcd2345"}, DesktopPassword{"Abcd2345"}, DesktopPassword{"Abcd2345"}, DesktopPassword{"Abcd2345"}); strings.Contains(s, "Abcd2345") {
		t.Errorf("DesktopPassword prints its password: %s", s)
	}
}

func TestClassifyDial(t *testing.T) {
	wrap := func(e error) error { return fmt.Errorf("vsock connect to the host on port 1026: %w", e) }
	cases := map[error]string{
		wrap(syscall.ECONNREFUSED):                              DialRefused,
		wrap(syscall.ECONNRESET):                                DialRefused,
		fmt.Errorf("vsock socket: %w", syscall.EAFNOSUPPORT):    DialUnsupported,
		fmt.Errorf("vsock socket: %w", syscall.EPROTONOSUPPORT): DialUnsupported,
		wrap(context.DeadlineExceeded):                          DialTimeout,
		wrap(syscall.ETIMEDOUT):                                 DialTimeout,
		wrap(syscall.EHOSTUNREACH):                              DialTransportFailure,
		errors.New("something else"):                            DialTransportFailure,
	}
	for err, want := range cases {
		if got := ClassifyDial(err); got != want {
			t.Errorf("ClassifyDial(%v) = %q, want %q", err, got, want)
		}
	}
}

func waitDial(t *testing.T, rec *ToolsDialRecord, class string) ToolsDial {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if d := rec.Get(); d.Class == class {
			return d
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("the tools dial never read %q: %+v", class, rec.Get())
	return ToolsDial{}
}

// The tools dial's record reads refused for a host with no listener (a run boot), timeout for one that
// never answers, transport failure for a session dropped before its hello, and connected once the host's
// hello opens a session, which then serves the tools steps through the host's Client.
func TestServeToolsClassifiesItsDial(t *testing.T) {
	for _, c := range []struct {
		class string
		dial  DialFunc
	}{
		{DialRefused, func(context.Context) (net.Conn, error) {
			return nil, fmt.Errorf("vsock connect to the host on port 1026: %w", syscall.ECONNREFUSED)
		}},
		{DialTimeout, func(ctx context.Context) (net.Conn, error) { <-ctx.Done(); return nil, ctx.Err() }},
		{DialTransportFailure, func(context.Context) (net.Conn, error) {
			g, h := net.Pipe()
			_ = h.Close()
			return g, nil
		}},
	} {
		ctx, cancel := context.WithCancel(context.Background())
		rec := &ToolsDialRecord{}
		go func() { _ = serveTools(ctx, c.dial, http.NewServeMux(), rec, 5*time.Millisecond, 20*time.Millisecond) }()
		if d := waitDial(t, rec, c.class); d.Error == "" || d.Dials == 0 {
			t.Errorf("%s: record = %+v", c.class, d)
		}
		cancel()
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rig := newToolsRig(t)
	rec := &ToolsDialRecord{}
	b := newBroker()
	// A short dial timeout, outlived below: a served session must outlive the hello's read deadline.
	const timeout = 100 * time.Millisecond
	go func() { _ = serveTools(ctx, b.dial, rig.t.ToolsHandler(), rec, 5*time.Millisecond, timeout) }()
	host := b.next(t)
	if d := rec.Get(); d.Class != "" {
		t.Errorf("a dial waiting for its hello already reads %q", d.Class)
	}
	cl := NewClient(func(context.Context) (net.Conn, error) { return host, nil })
	defer cl.Close()
	out, exit, err := cl.ToolsRun(ctx, CLTGitArgv)
	if err != nil || exit != 0 || !strings.HasPrefix(out, "git version") {
		t.Fatalf("CLT check over the tools session = %q %d %v", out, exit, err)
	}
	if d := rec.Get(); d.Class != DialConnected || d.Error != "" {
		t.Errorf("record = %+v, want connected", d)
	}
	time.Sleep(3 * timeout)
	if _, _, err := cl.ToolsRun(ctx, []string{"/bin/sh", "-c", "id"}); err == nil {
		t.Error("a shell over the tools session was not refused")
	}
	body := []byte("cli")
	sum := sha256.Sum256(body)
	if err := cl.ToolsPush(ctx, filepath.Join(rig.bin, "codex"), 0o755, bytes.NewReader(body), int64(len(body)), hex.EncodeToString(sum[:])); err != nil {
		t.Errorf("ToolsPush = %v", err)
	}
	if set, err := cl.ToolsDesktopPassword(ctx, "Abcd2345"); err != nil || !set {
		t.Errorf("ToolsDesktopPassword = %v, %v", set, err)
	}
	if p, err := cl.ToolsPointer(ctx); err != nil || p != (Pointer{X: 10, Y: 20}) {
		t.Errorf("ToolsPointer = %+v, %v", p, err)
	}
}

// A host that accepts the tools dial but never sends its hello leaves the record timeout once the
// dial's timeout passes, never the refused of the dial before: the class clears on connect.
func TestServeToolsHelloLessListenerIsATimeout(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var host net.Conn // the host end, which never writes; set before handed closes
	handed := make(chan struct{})
	dials := 0
	dial := func(context.Context) (net.Conn, error) {
		dials++
		switch dials {
		case 1:
			return nil, fmt.Errorf("vsock connect to the host on port 1026: %w", syscall.ECONNREFUSED)
		case 2:
			g, h := net.Pipe()
			host = h
			close(handed)
			return g, nil
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}
	rec := &ToolsDialRecord{}
	go func() { _ = serveTools(ctx, dial, http.NewServeMux(), rec, 5*time.Millisecond, 300*time.Millisecond) }()
	<-handed
	defer host.Close()
	waitDial(t, rec, "")
	if d := waitDial(t, rec, DialTimeout); d.Dials != 2 || !strings.Contains(d.Error, "hello") {
		t.Errorf("record = %+v, want a hello timeout on the second dial", d)
	}
}
