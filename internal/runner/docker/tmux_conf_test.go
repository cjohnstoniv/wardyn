// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The bundled images whose Dockerfile installs tmux. Each must COPY
// deploy/images/common/tmux.conf to /etc/tmux.conf: a test that reads only the
// conf file passes while an image never copies it (how codex-cli was missed).
var tmuxImages = []string{"base", "claude-code", "codex-cli", "aws-sso"}

const tmuxConfSrc = "../../../deploy/images/common/tmux.conf"

// TestTmuxConf asserts the options of a RUNNING tmux server started from the
// file each image actually installs. WARDYN_REQUIRE_TMUX=1 turns a missing
// tmux into a failure instead of a skip.
func TestTmuxConf(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		if os.Getenv("WARDYN_REQUIRE_TMUX") == "1" {
			t.Fatal("tmux not found and WARDYN_REQUIRE_TMUX=1")
		}
		t.Skip("tmux not installed")
	}
	copyRe := regexp.MustCompile(`(?m)^COPY\s+deploy/images/common/tmux\.conf\s+/etc/tmux\.conf\s*$`)
	installRe := regexp.MustCompile(`(?m)^\s+tmux\s*\\?\s*$`)
	for _, img := range tmuxImages {
		t.Run(img, func(t *testing.T) {
			df, err := os.ReadFile(filepath.Join("../../../deploy/images", img, "Dockerfile"))
			if err != nil {
				t.Fatal(err)
			}
			if !installRe.Match(df) {
				t.Fatalf("%s Dockerfile no longer installs tmux; drop it from tmuxImages", img)
			}
			if !copyRe.Match(df) {
				t.Fatalf("%s Dockerfile installs tmux but does not COPY tmux.conf to /etc/tmux.conf", img)
			}
			sock := filepath.Join(t.TempDir(), "s")
			tm := func(args ...string) string {
				t.Helper()
				out, err := exec.Command("tmux", append([]string{"-S", sock, "-f", tmuxConfSrc}, args...)...).CombinedOutput()
				if err != nil {
					t.Fatalf("tmux %v: %v\n%s", args, err, out)
				}
				return strings.TrimSpace(string(out))
			}
			tm("new-session", "-d", "-s", "w")
			t.Cleanup(func() { _ = exec.Command("tmux", "-S", sock, "kill-server").Run() })
			for opt, want := range map[string]string{
				"mouse": "on", "set-clipboard": "external", "focus-events": "on",
				"status": "off", "history-limit": "50000",
			} {
				if got := tm("show", "-gv", opt); got != want {
					t.Errorf("%s = %q, want %q", opt, got, want)
				}
			}
			keys := tm("list-keys", "-T", "root")
			for _, k := range []string{"WheelUpPane", "WheelDownPane"} {
				if !regexp.MustCompile(k + `.*alternate_on`).MatchString(keys) {
					t.Errorf("%s binding does not branch on alternate_on:\n%s", k, keys)
				}
			}
			for _, k := range []string{"MouseDown3Pane", "M-MouseDown3Pane"} {
				if strings.Contains(keys, k) {
					t.Errorf("%s is still bound", k)
				}
			}
		})
	}
}
