// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// tmuxConfCommands returns the non-comment, non-blank lines of the tmux.conf
// every image installs.
func tmuxConfCommands(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile("../../deploy/images/common/tmux.conf")
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
			out = append(out, l)
		}
	}
	return out
}

// The attach chain is a second copy of tmux.conf for sandboxes that never read
// it; a line added to the file without the chain fails here.
func TestTmuxAttachCommandsMatchConf(t *testing.T) {
	conf := tmuxConfCommands(t)
	if strings.Join(conf, "\n") != strings.Join(TmuxAttachCommands, "\n") {
		t.Errorf("TmuxAttachCommands differs from tmux.conf\nconf:\n%s\nchain:\n%s",
			strings.Join(conf, "\n"), strings.Join(TmuxAttachCommands, "\n"))
	}
	if !strings.Contains(TmuxAttachSh, strings.Join(conf, ` \; `)) {
		t.Error("TmuxAttachSh does not chain the tmux.conf commands in order")
	}
}

// TestTmuxAttachShVersionGate runs the fragment against a fake tmux whose -V
// output is hostile or old: only a strict `tmux N.M` with N.M >= 3.2 chains the
// settings, and every other case still execs the plain attach.
func TestTmuxAttachShVersionGate(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "args")
	fake := "#!/bin/sh\nif [ \"$1\" = -V ]; then printf '%s' \"$FAKE_V\"; exit 0; fi\nprintf '%s\\n' \"$@\" > \"" + out + "\"\n"
	if err := os.WriteFile(filepath.Join(dir, "tmux"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		v     string
		chain bool
	}{
		{"tmux 3.2\n", true},
		{"tmux 3.5a\n", true},
		{"tmux 3.4", true},
		{"tmux 4.0\n", true},
		{"tmux 10.1\n", true},
		{"tmux 3.10\n", true},
		{"tmux 3.1\n", false},
		{"tmux 2.9\n", false},
		{"tmux next-3.4\n", false},
		{"tmux master\n", false},
		{"", false},
		{"garbage 3.4\n", false},
		{"banner\ntmux 3.4\n", false},
		{"tmux 99999999999999999999.1\n", false},
		{strings.Repeat("x", 100000), false},
	}
	for _, c := range cases {
		name := c.v[:min(len(c.v), 20)]
		_ = os.Remove(out)
		cmd := exec.Command("/bin/sh", "-c", TmuxAttachSh)
		cmd.Env = []string{"PATH=" + dir + ":/usr/bin:/bin", "FAKE_V=" + c.v}
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%q: %v\n%s", name, err, b)
		}
		b, err := os.ReadFile(out)
		if err != nil {
			t.Fatalf("%q: fake tmux was not exec'd: %v", name, err)
		}
		args := string(b)
		if !strings.HasPrefix(args, "new-session\n-A\n-s\nwardyn\nbash\n") {
			t.Errorf("%q: attach args = %q", name, args)
		}
		if got := strings.Contains(args, "WheelUpPane"); got != c.chain {
			t.Errorf("%q: chained = %v, want %v", name, got, c.chain)
		}
	}
}
