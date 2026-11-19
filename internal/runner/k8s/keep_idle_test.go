// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package k8s

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

func TestKeepIdleScript(t *testing.T) {
	if runner.ScratchWorkPath != "/home/agent/work" {
		t.Fatalf("runner.ScratchWorkPath = %q; the script below is written for /home/agent/work", runner.ScratchWorkPath)
	}
	got := keepIdleScript("11111111-2222-3333-4444-555555555555")
	want := []string{"sh", "-c", `m=/home/agent/work/.wardyn/booted-11111111-2222-3333-4444-555555555555; h="${HOME:-/home/agent}"; ` +
		`if [ -f "$m" ]; then mkdir -p "$h/.wardyn" && true > "$h/.wardyn/booted" || { echo "wardyn: cannot restore $h/.wardyn/booted for this kept run" >&2; exit 1; }; ` +
		`elif [ -e "$m" ] || [ -L "$m" ]; then echo "wardyn: $m is not a file" >&2; exit 1; ` +
		`else mkdir -p /home/agent/work/.wardyn && true > "$m" || { echo "wardyn: cannot write $m, so a revive would run the first task again" >&2; exit 1; }; fi; ` +
		`exec agent-run --idle`}
	if !slices.Equal(got, want) {
		t.Fatalf("keepIdleScript =\n%q\nwant\n%q", got, want)
	}
	if strings.Contains(got[2], "--revive") {
		t.Fatal("the kept run's command passes --revive, which not every image has; the image picks its revive branch itself")
	}
}

func TestKeepHarnessRefusal(t *testing.T) {
	for _, agent := range []string{"claude-code", "codex-cli"} {
		if got := keepHarnessRefusal(agent); got != "" {
			t.Errorf("keepHarnessRefusal(%q) = %q, want kept", agent, got)
		}
	}
	for _, agent := range []string{"aws-sso", "oracle", "base", "none", "", "claude-code+codex-cli", "Claude-Code"} {
		got := keepHarnessRefusal(agent)
		want := `k8s: this run is not kept: only claude-code and codex-cli runs are revived on Kubernetes, and its agent is "` + agent + `"`
		if got != want {
			t.Errorf("keepHarnessRefusal(%q) = %q, want %q", agent, got, want)
		}
	}
}

// TestReviveHarnessesMatchTheImages ties reviveHarnesses to the images: an image is listed exactly when its agent-run has
// a revive mode, and every listed image reaches it through booted_before, which keepIdleScript relies on.
func TestReviveHarnessesMatchTheImages(t *testing.T) {
	paths, err := filepath.Glob("../../../deploy/images/*/agent-run")
	if err != nil || len(paths) == 0 {
		t.Fatalf("no agent-run under deploy/images (err %v)", err)
	}
	seen := map[string]bool{}
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		name := filepath.Base(filepath.Dir(p))
		seen[name] = true
		listed := slices.Contains(reviveHarnesses, name)
		if has := strings.Contains(string(b), "--revive"); has != listed {
			t.Errorf("%s: agent-run has a --revive mode: %v; listed in reviveHarnesses: %v", p, has, listed)
		}
		if listed && !strings.Contains(string(b), "booted_before") {
			t.Errorf("%s: listed in reviveHarnesses but never calls booted_before, so a kept run would boot as new", p)
		}
	}
	for _, h := range reviveHarnesses {
		if !seen[h] {
			t.Errorf("reviveHarnesses names %q, which has no deploy/images/%s/agent-run", h, h)
		}
	}
}

// TestKeepIdleScriptStartsTheAgentOnlyWithItsMarker runs the script under sh, with the work folder and HOME moved into
// temp dirs and an agent-run stub on PATH that records its arguments. The agent starts only when the marker this boot
// needs is written or restored; a failed write, a failed restore, or anything but a file at the marker's path stops it.
func TestKeepIdleScriptStartsTheAgentOnlyWithItsMarker(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Fatalf("no sh on PATH: %v", err)
	}
	const id = "11111111-2222-3333-4444-555555555555"
	marker := func(work string) string { return filepath.Join(work, ".wardyn", "booted-"+id) }
	cases := []struct {
		name    string
		setup   func(t *testing.T, work, home string)
		started bool
		want    func(work, home string) string // a file that must exist afterwards, when the agent started
	}{
		{"first boot writes the work marker", func(*testing.T, string, string) {}, true, func(work, _ string) string { return marker(work) }},
		{"a later boot restores the home marker", func(t *testing.T, work, _ string) { writeTestFile(t, marker(work)) }, true,
			func(_, home string) string { return filepath.Join(home, ".wardyn", "booted") }},
		{"the work folder's .wardyn is a file", func(t *testing.T, work, _ string) { writeTestFile(t, filepath.Join(work, ".wardyn")) }, false, nil},
		{"the marker's path is a folder", func(t *testing.T, work, _ string) { mkdirTest(t, marker(work)) }, false, nil},
		{"the home marker cannot be restored", func(t *testing.T, work, home string) {
			writeTestFile(t, marker(work))
			writeTestFile(t, filepath.Join(home, ".wardyn"))
		}, false, nil},
		{"the work marker cannot be written", func(t *testing.T, work, _ string) {
			if os.Geteuid() == 0 {
				t.Skip("root writes into a read-only folder, which this case relies on")
			}
			mkdirTest(t, filepath.Join(work, ".wardyn"))
			if err := os.Chmod(filepath.Join(work, ".wardyn"), 0o500); err != nil {
				t.Fatal(err)
			}
		}, false, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			work, home, bin := filepath.Join(dir, "work"), filepath.Join(dir, "home"), filepath.Join(dir, "bin")
			for _, d := range []string{work, home, bin} {
				mkdirTest(t, d)
			}
			ran := filepath.Join(dir, "agent-run-args")
			if err := os.WriteFile(filepath.Join(bin, "agent-run"), []byte("#!/bin/sh\necho \"$@\" > '"+ran+"'\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			tc.setup(t, work, home)
			script := keepIdleScript(id)
			cmd := exec.Command(script[0], script[1], strings.ReplaceAll(script[2], runner.ScratchWorkPath, work))
			cmd.Env = append(os.Environ(), "HOME="+home, "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			out, err := cmd.CombinedOutput()
			args, readErr := os.ReadFile(ran)
			if !tc.started {
				if err == nil || readErr == nil {
					t.Fatalf("the agent started (agent-run args %q, exit error %v) without the marker this boot needs; output: %s", args, err, out)
				}
				return
			}
			if err != nil || string(args) != "--idle\n" {
				t.Fatalf("the agent did not start as agent-run --idle: exit error %v, agent-run args %q, output: %s", err, args, out)
			}
			if _, err := os.Stat(tc.want(work, home)); err != nil {
				t.Fatalf("the marker this boot needs is missing: %v", err)
			}
		})
	}
}

func writeTestFile(t *testing.T, path string) {
	t.Helper()
	mkdirTest(t, filepath.Dir(path))
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
}

func mkdirTest(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
}
