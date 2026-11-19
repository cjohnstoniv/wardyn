// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package guestagent

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The sign-in hold's macOS pieces, run as root on the hosted macOS job (go test -c, then sudo): the console
// owner, auto-login written, read back from the loginwindow file and removed, and the carry's copy run as
// another account. No guest boots there, so they prove neither the order against loginwindow nor what
// Setup Assistant shows. Each changes this Mac's login settings and restores them, so it skips unless it
// runs as root with WARDYN_LOGINWINDOW_PIN=1.
func loginHoldPinGate(t *testing.T) {
	t.Helper()
	if os.Getenv("WARDYN_LOGINWINDOW_PIN") != "1" || os.Getuid() != 0 {
		t.Skip("changes this Mac's login settings: only as root on the hosted macOS job (WARDYN_LOGINWINDOW_PIN=1)")
	}
}

// refuseDefaults fails every defaults command and runs the rest, so the hold falls back to plutil.
type refuseDefaults struct{}

func (refuseDefaults) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	if name == "/usr/bin/defaults" {
		return nil, errors.New("defaults refused by the test")
	}
	return ExecRunner{}.Run(ctx, name, args...)
}

// The console owner the hold reads is the one stat(1) shows: root at the login window reads as nobody.
func TestDarwinLoginHoldConsoleOwner(t *testing.T) {
	loginHoldPinGate(t)
	out, err := exec.Command("/usr/bin/stat", "-f", "%u", "/dev/console").Output()
	if err != nil {
		t.Fatal(err)
	}
	want, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		t.Fatalf("stat printed %q", out)
	}
	uid, err := ConsoleOwner{}.Console(t.Context())
	switch {
	case want == 0 && !errors.Is(err, ErrNoConsoleUser):
		t.Fatalf("console = %d, %v; stat says root, want ErrNoConsoleUser", uid, err)
	case want != 0 && (err != nil || int(uid) != want):
		t.Fatalf("console = %d, %v; stat says %d", uid, err, want)
	}
}

// The carry's id read: ioreg on real macOS prints an IOPlatformUUID of the shape the carry takes.
func TestDarwinLoginHoldIoregUUID(t *testing.T) {
	loginHoldPinGate(t)
	if id := (HostPrefsCarry{Run: ExecRunner{}}).thisBootID(t.Context()); !hostPrefsID.MatchString(id) {
		t.Fatalf("this Mac's IOPlatformUUID reads as %q", id)
	}
}

// Auto-login written by the hold reaches the loginwindow file, where loginwindow reads it, through
// defaults and through the plutil fallback, and its removal leaves the file without it.
func TestDarwinLoginHoldAutoLoginWriteAndReadBack(t *testing.T) {
	loginHoldPinGate(t)
	ctx := t.Context()
	h := LoginHold{Run: ExecRunner{}, Every: 100 * time.Millisecond}
	orig := h.autoLogin(ctx)
	t.Logf("autoLoginUser before the test: %q", orig)
	t.Cleanup(func() {
		c := context.Background()
		if orig == "" {
			_, _ = ExecRunner{}.Run(c, "/usr/bin/defaults", "delete", loginWindowDomain, "autoLoginUser")
		} else {
			_, _ = ExecRunner{}.Run(c, "/usr/bin/defaults", "write", loginWindowDomain, "autoLoginUser", orig)
		}
	})
	// The plutil fallback over another value, once the file holds that value.
	if _, err := (ExecRunner{}).Run(ctx, "/usr/bin/defaults", "write", loginWindowDomain, "autoLoginUser", "wardyn-pin"); err != nil {
		t.Fatal(err)
	}
	if !h.readsBack(ctx, "wardyn-pin") {
		t.Fatalf("the file never held the value defaults wrote: %q", h.autoLogin(ctx))
	}
	if !(LoginHold{Run: refuseDefaults{}, Every: 100 * time.Millisecond}).setAutoLogin(ctx) || h.autoLogin(ctx) != MacAgentUser {
		t.Fatalf("the plutil fallback: the file reads %q", h.autoLogin(ctx))
	}
	if !h.clearAutoLogin(ctx) || h.autoLogin(ctx) != "" {
		t.Fatalf("after the removal the file reads %q", h.autoLogin(ctx))
	}
	if !h.setAutoLogin(ctx) || h.autoLogin(ctx) != MacAgentUser {
		t.Fatalf("after the write the file reads %q", h.autoLogin(ctx))
	}
	if out, err := (ExecRunner{}).Run(ctx, "/usr/bin/defaults", "read", loginWindowDomain, "autoLoginUser"); err != nil || strings.TrimSpace(string(out)) != MacAgentUser {
		t.Fatalf("defaults reads %q, %v; the file reads %q", out, err, h.autoLogin(ctx))
	}
	if !h.clearAutoLogin(ctx) {
		t.Fatalf("the second removal: the file reads %q", h.autoLogin(ctx))
	}
}

// The carry's child runs as another account, in a session of its own, copies the base identity's file as
// that account and never replaces one already there.
func TestDarwinLoginHoldCarryCopiesAsAnotherAccount(t *testing.T) {
	loginHoldPinGate(t)
	uid, err := strconv.Atoi(os.Getenv("SUDO_UID"))
	if err != nil || uid == 0 {
		t.Fatalf("SUDO_UID %q names no other account", os.Getenv("SUDO_UID"))
	}
	gid, err := strconv.Atoi(os.Getenv("SUDO_GID"))
	if err != nil {
		t.Fatalf("SUDO_GID %q", os.Getenv("SUDO_GID"))
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	for _, d := range []string{filepath.Dir(dir), dir} {
		if err := os.Chmod(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	byHost := filepath.Join(dir, "ByHost")
	src := filepath.Join(byHost, "com.apple.SetupAssistant."+baseID+".plist")
	dest := filepath.Join(byHost, "com.apple.SetupAssistant."+thisID+".plist")
	if err := os.Mkdir(byHost, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, []byte("base"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{byHost, src} {
		if err := os.Chown(p, uid, gid); err != nil {
			t.Fatal(err)
		}
	}
	c := HostPrefsCarry{Cred: &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)}, ByHost: byHost,
		argv: []string{self, "-test.run=^TestDarwinLoginHoldCarryChild$", "--"}}
	if got, reason := c.runChild(t.Context(), thisID); got != "host_prefs=carried files=1 kept=0 skipped=0 failed=0" || reason != "" {
		t.Fatalf("first carry = %q, %q", got, reason)
	}
	fi, err := os.Lstat(dest)
	if err != nil {
		t.Fatal(err)
	}
	if st := fi.Sys().(*syscall.Stat_t); int(st.Uid) != uid {
		t.Errorf("the copy belongs to uid %d, want %d", st.Uid, uid)
	}
	if b, _ := os.ReadFile(dest); !bytes.Equal(b, []byte("base")) {
		t.Errorf("the copy holds %q", b)
	}
	if got, reason := c.runChild(t.Context(), thisID); got != "host_prefs=carried files=0 kept=1 skipped=0 failed=0" || reason != "" {
		t.Fatalf("second carry = %q, %q; want the copy kept", got, reason)
	}
}

// TestDarwinLoginHoldCarryChild is the carry's child for the test above, run with the ByHost folder and the
// id after "--". Alone it skips. syscall.Exit, not os.Exit, which a test may not call with 0.
func TestDarwinLoginHoldCarryChild(t *testing.T) {
	if len(flag.Args()) != 2 {
		t.Skip("only as the carry's child")
	}
	syscall.Exit(HostPrefsCarryMain(flag.Args(), os.Stdout))
}
