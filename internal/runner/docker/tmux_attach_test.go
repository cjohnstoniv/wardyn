// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestTmuxAttach_AppliesSettingsToOldConfig runs the real attachShell against
// a tmux server that read no wardyn tmux.conf, which is what a sandbox started
// from a 0.8.5 image looks like (mouse off, right-click bound). After one
// attach the full settings must be live. The attach runs on a pty from
// script(1); HOME and TMUX_TMPDIR point at scratch dirs so the host's own
// tmux is untouched. Note that focus-events applies only to clients attached
// after it is set, so it is asserted as a server option, not as client
// behaviour. WARDYN_REQUIRE_TMUX=1 turns a missing tmux or script into a
// failure instead of a skip.
func TestTmuxAttach_AppliesSettingsToOldConfig(t *testing.T) {
	for _, bin := range []string{"tmux", "script"} {
		if _, err := exec.LookPath(bin); err != nil {
			if os.Getenv("WARDYN_REQUIRE_TMUX") == "1" {
				t.Fatalf("%s not found and WARDYN_REQUIRE_TMUX=1", bin)
			}
			t.Skipf("%s not installed", bin)
		}
	}
	if _, err := os.Stat("/etc/tmux.conf"); err == nil {
		t.Skip("host /etc/tmux.conf would be read by the test server")
	}
	scratch := t.TempDir()
	env := append(os.Environ(),
		"HOME="+scratch, "TMUX_TMPDIR="+scratch, "TERM=xterm-256color",
		"ATTACH="+attachShell[2], "XDG_CONFIG_HOME="+scratch)
	// script allocates the pty tmux needs; the open stdin pipe keeps the
	// client attached while the test inspects the server.
	cmd := exec.Command("script", "-qec", `exec /bin/sh -c "$ATTACH"`, "/dev/null")
	cmd.Env = env
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(scratch, fmt.Sprintf("tmux-%d", os.Getuid()), "default")
	tm := func(args ...string) (string, error) {
		out, err := exec.Command("tmux", append([]string{"-S", sock}, args...)...).CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	t.Cleanup(func() {
		_, _ = tm("kill-server")
		_ = stdin.Close()
		_ = cmd.Wait()
	})

	// Poll until the chained settings landed (the attach is asynchronous).
	var mouse string
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if mouse, _ = tm("show", "-gv", "mouse"); mouse == "on" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if mouse != "on" {
		t.Fatalf("mouse = %q after attach, want on", mouse)
	}
	for opt, want := range map[string]string{"set-clipboard": "external", "focus-events": "on", "status": "off"} {
		if got, _ := tm("show", "-gv", opt); got != want {
			t.Errorf("%s = %q, want %q", opt, got, want)
		}
	}
	// list-keys for an unbound key exits 1, so read the full table.
	keys, err := tm("list-keys", "-T", "root")
	if err != nil {
		t.Fatalf("list-keys: %v\n%s", err, keys)
	}
	for _, k := range []string{"MouseDown3Pane", "M-MouseDown3Pane"} {
		if strings.Contains(keys, k) {
			t.Errorf("%s is still bound after attach:\n%s", k, keys)
		}
	}
	for _, k := range []string{"WheelUpPane", "WheelDownPane"} {
		if !strings.Contains(keys, k) || !strings.Contains(keys, "alternate_on") {
			t.Errorf("%s binding missing or does not branch on alternate_on", k)
		}
	}
}
