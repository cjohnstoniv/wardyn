// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package docker

// The held boot seed (held_seed_adopt, boot_seed_wait_for_release), driven as pure shell over the real
// library. No build tag, like claude_agent_run_boot_test.go: the boot path is covered by the plain go test run.

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	heldSeedHeld = "Waiting for the network policy to be confirmed; the agent starts here then."
	heldSeedGone = "wardyn: the network policy was not confirmed before the sandbox stopped, so the agent was not started."
)

// heldSeedCmd is bash sourcing the real library and running script, with HOME=home and env on top of the
// test's own env less the variables the library reads.
func heldSeedCmd(t *testing.T, ctx context.Context, home, script string, env ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.CommandContext(ctx, "bash", "-c", `set -euo pipefail; source "$1"; `+script,
		"agent-run-lib-test", ccAbs(t, ccAgentRunLibPath))
	var base []string
	for _, kv := range os.Environ() {
		switch strings.SplitN(kv, "=", 2)[0] {
		case "WARDYN_HELD_SEED", "WARDYN_INTERACTIVE_SEED", "WARDYN_IDLE_PID", "HOME":
			continue
		}
		base = append(base, kv)
	}
	cmd.Env = append(base, append([]string{"HOME=" + home}, env...)...)
	cmd.Stdin = strings.NewReader("")
	return cmd
}

// heldSeedRun runs script to its end and returns its combined output.
func heldSeedRun(t *testing.T, home, script string, env ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, err := heldSeedCmd(t, ctx, home, script, env...).CombinedOutput()
	if err != nil {
		t.Fatalf("script failed: %v\noutput: %s", err, out)
	}
	return string(out)
}

// startedLine reports whether out has the script's own "started" line; the not-confirmed sentence also holds
// the word.
func startedLine(out string) bool {
	return slices.Contains(strings.Split(out, "\n"), "started")
}

func writeSeedRelease(t *testing.T, home string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(home, ".wardyn"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".wardyn", "seed-release"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
}

// held_seed_adopt makes the held seed the image's seed, never over a seed already set.
func TestHeldSeed_AdoptsTheHeldSeed(t *testing.T) {
	script := `held_seed_adopt; printf %s "${WARDYN_INTERACTIVE_SEED:-}"`
	if got := heldSeedRun(t, t.TempDir(), script, "WARDYN_HELD_SEED=echo hi"); got != "echo hi" {
		t.Errorf("held only: seed = %q, want %q", got, "echo hi")
	}
	if got := heldSeedRun(t, t.TempDir(), script, "WARDYN_HELD_SEED=echo hi", "WARDYN_INTERACTIVE_SEED=echo own"); got != "echo own" {
		t.Errorf("both set: seed = %q, want %q", got, "echo own")
	}
	if got := heldSeedRun(t, t.TempDir(), script); got != "" {
		t.Errorf("neither set: seed = %q, want empty", got)
	}
}

// A marker already written lets the pane go on at once, without the waiting sentence.
func TestHeldSeed_ReleasedStartsAtOnce(t *testing.T) {
	home := t.TempDir()
	writeSeedRelease(t, home)
	out := heldSeedRun(t, home, `boot_seed_wait_for_release; echo started`, "WARDYN_HELD_SEED=echo hi")
	if !startedLine(out) || strings.Contains(out, heldSeedHeld) {
		t.Errorf("output = %q, want started and no waiting sentence", out)
	}
}

// With no marker the pane waits while the idle process lives, and goes on once the marker is written.
func TestHeldSeed_HoldsUntilTheMarker(t *testing.T) {
	home := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := heldSeedCmd(t, ctx, home, `boot_seed_wait_for_release; echo started`,
		"WARDYN_HELD_SEED=echo hi", "WARDYN_IDLE_PID="+strconv.Itoa(os.Getpid()))
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() {
		select {
		case <-done:
		default:
			_ = cmd.Process.Kill()
			<-done
		}
	})
	select {
	case err := <-done:
		done <- err
		t.Fatalf("the pane did not wait for the marker: %v\noutput: %s", err, out.String())
	case <-time.After(1500 * time.Millisecond):
	}
	writeSeedRelease(t, home)
	select {
	case err := <-done:
		done <- err
		if err != nil {
			t.Fatalf("exit after the marker: %v\noutput: %s", err, out.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the pane did not go on within 5 s of the marker")
	}
	if got := out.String(); !startedLine(got) || !strings.Contains(got, heldSeedHeld) {
		t.Errorf("output = %q, want the waiting sentence, then started", got)
	}
}

// An idle process that ended before the marker drops the pane to a shell: the seed never starts.
func TestHeldSeed_IdleGoneNeverStarts(t *testing.T) {
	gone := exec.Command("true")
	if err := gone.Run(); err != nil {
		t.Fatal(err)
	}
	out := heldSeedRun(t, t.TempDir(), `boot_seed_wait_for_release; echo started`,
		"WARDYN_HELD_SEED=echo hi", "WARDYN_IDLE_PID="+strconv.Itoa(gone.Process.Pid))
	if !strings.Contains(out, heldSeedGone) || startedLine(out) {
		t.Errorf("output = %q, want the not-confirmed sentence and no started", out)
	}
}

// No held seed: nothing waits.
func TestHeldSeed_NoHeldSeedDoesNotWait(t *testing.T) {
	out := heldSeedRun(t, t.TempDir(), `boot_seed_wait_for_release; echo started`)
	if strings.TrimSpace(out) != "started" {
		t.Errorf("output = %q, want started alone", out)
	}
}

// Every agent-run adopts the held seed on the line after sourcing the library, the exact line the server's
// release checks for.
func TestHeldSeed_EveryAgentRunAdopts(t *testing.T) {
	for _, rel := range []string{
		"../../../deploy/images/claude-code/agent-run",
		"../../../deploy/images/codex-cli/agent-run",
		"../../../deploy/images/common/agent-run-stub",
	} {
		b, err := os.ReadFile(ccAbs(t, rel))
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(string(b), "\n")
		found := false
		for i, l := range lines {
			if l != "source /usr/local/bin/agent-run-lib.sh" {
				continue
			}
			found = true
			if i+1 >= len(lines) || lines[i+1] != "held_seed_adopt" {
				t.Errorf("%s: the line after the source line is not held_seed_adopt", rel)
			}
		}
		if !found {
			t.Errorf("%s: no source /usr/local/bin/agent-run-lib.sh line", rel)
		}
	}
	// The harnesses image's agent-run sources no library: it carries the exact line, after a no-op definition so
	// it runs under set -e, and the start harness's own agent-run adopts the seed.
	b, err := os.ReadFile(ccAbs(t, "../../../deploy/images/common/agent-run-set"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(b), "\n")
	def := slices.Index(lines, "held_seed_adopt() { :; }")
	call := slices.Index(lines, "held_seed_adopt")
	if def < 0 || call < 0 || call < def {
		t.Errorf("agent-run-set: want a no-op held_seed_adopt definition, then the exact held_seed_adopt line (def %d, call %d)", def, call)
	}
}
