// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package docker

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The codex-cli image's BOOT — the same race claude-code and aws-sso fixed, in
// the image that is actually PUBLISHED (release.yml builds and signs
// agent-codex-cli; agent-claude-code has not been published since 0.6.2).
//
// Through 0.7.4 this image created its `wardyn` tmux session AFTER the shared
// prep, wrote the agent-started marker first, and had no respawn fallback. A
// seeded interactive codex run attached during prep therefore got `duplicate
// session`, the seed was dropped, AND the marker suppressed attach-bashrc's
// auto-start: a bare shell, with the operator's task text gone and no record
// anywhere a human looks.
//
// PURE-SHELL tests over the REAL scripts, the idiom the claude-code and
// attach-bashrc files use. Nothing here needs a container or a docker daemon.
const cxAgentRunPath = "../../../deploy/images/codex-cli/agent-run"

// cxRunnableAgentRun copies the REAL codex-cli agent-run and redirects its one
// absolute `source` at the real library. Nothing is stubbed — /usr/local/bin does
// not exist on a test host, and the body under test is byte-for-byte the shipped
// script.
func cxRunnableAgentRun(t *testing.T) string {
	t.Helper()
	body := ccRead(t, ccAbs(t, cxAgentRunPath))
	const from = "source /usr/local/bin/agent-run-lib.sh"
	if !strings.Contains(body, from) {
		t.Fatalf("codex-cli agent-run no longer sources %s", from)
	}
	body = strings.Replace(body, from, "source "+ccAbs(t, ccAgentRunLibPath), 1)
	dst := filepath.Join(t.TempDir(), "agent-run")
	if err := os.WriteFile(dst, []byte(body), 0o700); err != nil { //nolint:gosec // test fixture
		t.Fatalf("write runnable agent-run: %v", err)
	}
	return dst
}

// cxRunIdle runs the real `agent-run --idle` under `timeout`: a script that holds
// the container open is KILLED by timeout (124); one that exits on its own
// reports its own code. The fake tmux is ccIdleEnv's — the same argv+facts
// recorder, because the claim being checked is the same one.
func cxRunIdle(t *testing.T, home, binDir, tmuxLog string, extraEnv ...string) (code int, out string) {
	t.Helper()
	cmd := exec.Command("timeout", "3s", "bash", cxRunnableAgentRun(t), "--idle")
	cmd.Env = append(os.Environ(), append([]string{
		"HOME=" + home,
		"PATH=" + binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"TMUX_LOG=" + tmuxLog,
		// The FIRST prep side effect this test can see: prepare_claude_config_dir
		// creates it, one line into the prep. "before prep-done" alone would go
		// green with the whole race still open.
		"CLAUDE_CONFIG_DIR=" + filepath.Join(home, "cfg"),
		"WARDYN_REPOS=",
		"WARDYN_REPO_URL=",
		"WARDYN_MITM_CA_PEM=",
		"WARDYN_GITHUB_GRANT_ID=",
		"WARDYN_CLAUDE_MANAGED_B64=",
	}, extraEnv...)...)
	b, err := cmd.CombinedOutput()
	if exitErr, ok := err.(*exec.ExitError); ok {
		return exitErr.ExitCode(), string(b)
	}
	if err != nil {
		t.Fatalf("running agent-run --idle: %v\noutput: %s", err, b)
	}
	return 0, string(b)
}

// TestCodexAgentRun_BootSessionPrecedesPrep — the session must win the `wardyn`
// name, and the marker must beat any attach shell, so BOTH happen before the
// shared prep. 0.7.4's codex did the opposite of both.
func TestCodexAgentRun_BootSessionPrecedesPrep(t *testing.T) {
	home, binDir, tmuxLog := ccIdleEnv(t)
	code, out := cxRunIdle(t, home, binDir, tmuxLog, ccSeedEnv)
	if code != 124 {
		t.Fatalf("agent-run --idle exited %d (output: %s), want 124 — it must hold the container open for attach", code, out)
	}
	log := ccRead(t, tmuxLog)
	if n := ccCount(log, "argv: new-session -d -s wardyn agent-run --boot-seed"); n != 1 {
		t.Fatalf("tmux got %d `new-session -d -s wardyn agent-run --boot-seed` calls, want exactly 1\ntmux log:\n%s\nagent-run output:\n%s", n, log, out)
	}
	if !strings.Contains(log, "claude-config-dir: absent") {
		t.Errorf("the boot session was created AFTER the shared prep had already run — an attach that arrives during "+
			"prep wins the session name, `new-session` fails as a duplicate, and the seed is lost\ntmux log:\n%s", log)
	}
	if !strings.Contains(log, "marker: present") {
		t.Errorf("the agent-started marker was not written before the boot session — an attach shell that lands "+
			"during prep bare-launches a second, unseeded agent beside this one\ntmux log:\n%s", log)
	}
	// Both assertions are only worth anything if the prep they name actually ran.
	if _, err := os.Stat(filepath.Join(home, "cfg")); err != nil {
		t.Fatalf("the shared prep never created CLAUDE_CONFIG_DIR, so the ordering assertions above proved nothing: %v", err)
	}
}

// TestCodexAgentRun_EarlyAttachWonTheName — the belt for the race that cannot be
// won: an attach that beat agent-run already created the session running its own
// bare `bash`, so respawn THAT pane on the seed. 0.7.4's codex had no fallback at
// all — it printed a warning to a stderr nobody reads and dropped the seed.
func TestCodexAgentRun_EarlyAttachWonTheName(t *testing.T) {
	home, binDir, tmuxLog := ccIdleEnv(t)
	code, out := cxRunIdle(t, home, binDir, tmuxLog, ccSeedEnv, "FAKE_TMUX_NEW_SESSION_RC=1")
	if code != 124 {
		t.Fatalf("agent-run --idle exited %d (output: %s), want 124 — a duplicate session must not kill the run", code, out)
	}
	log := ccRead(t, tmuxLog)
	if n := ccCount(log, "argv: respawn-pane -k -t wardyn agent-run --boot-seed"); n != 1 {
		t.Errorf("tmux got %d `respawn-pane -k -t wardyn agent-run --boot-seed` calls, want exactly 1 — an attach won "+
			"the session name and nothing replaced its bare shell\ntmux log:\n%s\nagent-run output:\n%s", n, log, out)
	}
	// The respawned pane inherits its environment from the ATTACH exec, not from
	// agent-run, so the idle pid the pane's prep-wait is bounded by has to be
	// handed over through the session environment BEFORE the respawn. Behaviour
	// survives without it only via the pid-1 fallback; this pins the hand-off.
	setEnv := strings.Index(log, "argv: set-environment -t wardyn WARDYN_IDLE_PID ")
	respawn := strings.Index(log, "argv: respawn-pane -k -t wardyn")
	if setEnv < 0 || respawn < 0 || setEnv > respawn {
		t.Errorf("want `set-environment -t wardyn WARDYN_IDLE_PID <pid>` BEFORE `respawn-pane` (set-environment at %d, respawn at %d)\ntmux log:\n%s", setEnv, respawn, log)
	}
}

// TestCodexAgentRun_TotalTmuxFailureLeavesNoMarker — when tmux refuses both the
// create and the respawn there is no pane, so the marker written in anticipation
// of one is a lie that costs the human their agent entirely: attach-bashrc reads
// it, starts nothing, and hands over a bare shell while no seed runs anywhere.
func TestCodexAgentRun_TotalTmuxFailureLeavesNoMarker(t *testing.T) {
	home, binDir, tmuxLog := ccIdleEnv(t)
	code, out := cxRunIdle(t, home, binDir, tmuxLog, ccSeedEnv,
		"FAKE_TMUX_NEW_SESSION_RC=1", "FAKE_TMUX_RESPAWN_RC=1")
	if code != 124 {
		t.Fatalf("agent-run --idle exited %d (output: %s), want 124 — a tmux that refuses everything must not kill the run", code, out)
	}
	if _, err := os.Stat(filepath.Join(home, ".wardyn", "agent-started")); err == nil {
		t.Error("the agent-started marker survived a total tmux failure — attach-bashrc.sh reads it, starts nothing, " +
			"and the human gets a bare shell while no seed is running anywhere")
	}
	if !strings.Contains(out, "could not create the boot tmux session") {
		t.Errorf("no WARNING that the seed will not run\noutput:\n%s", out)
	}
}

// TestCodexAgentRun_NoSeedChangesNothing — NEGATIVE CONTROL, and the one that
// has to stay green through any consolidation. The console's DEFAULT interactive
// run carries no seed, and that shape must be byte-identical in behaviour: no
// boot session, no agent-started marker, so attach-bashrc.sh still starts the
// agent on the human's first attach.
func TestCodexAgentRun_NoSeedChangesNothing(t *testing.T) {
	home, binDir, tmuxLog := ccIdleEnv(t)
	code, out := cxRunIdle(t, home, binDir, tmuxLog, "WARDYN_INTERACTIVE_START=agent")
	if code != 124 {
		t.Fatalf("agent-run --idle exited %d (output: %s), want 124", code, out)
	}
	if log := ccRead(t, tmuxLog); log != "" {
		t.Errorf("an unseeded interactive run called tmux; it must start no boot session at all\ntmux log:\n%s", log)
	}
	if _, err := os.Stat(filepath.Join(home, ".wardyn", "agent-started")); err == nil {
		t.Error("an unseeded interactive run wrote the agent-started marker — attach-bashrc.sh would then start " +
			"nothing and the human gets a bare shell")
	}
	if _, err := os.Stat(filepath.Join(home, ".wardyn", "prep-done")); err != nil {
		t.Errorf("the unseeded run never finished prep: %v", err)
	}
}

// TestCodexAgentRun_BootSeedWaitsForPrep — the cost of creating the session
// first, and the two things 0.7.4's codex boot pane did not do because it never
// paid that cost: the pane now opens while ~/work may still be empty and
// ~/.wardyn/workdir unwritten (so it waits, bounded by prep still RUNNING), and
// its tmux server predates provision_git_helper_secret's export (so it re-reads
// the secret from prep's 0400 file, or the seeded agent's brokered git is
// refused a token with no error the human can see).
func TestCodexAgentRun_BootSeedWaitsForPrep(t *testing.T) {
	root := t.TempDir()
	home, binDir := filepath.Join(root, "home"), filepath.Join(root, "bin")
	for _, d := range []string{filepath.Join(home, ".wardyn"), binDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	// A `codex` that records the workspace it was started in, its argv, and
	// whether the caller-auth secret reached it.
	fake := "#!/bin/sh\npwd >> \"$HOME/codex.log\"\nprintf '%s\\n' \"$*\" >> \"$HOME/codex.log\"\n" +
		"printf 'secret=%s\\n' \"${WARDYN_GIT_HELPER_SECRET:-<unset>}\" >> \"$HOME/codex.log\"\n"
	if err := os.WriteFile(filepath.Join(binDir, "codex"), []byte(fake), 0o700); err != nil { //nolint:gosec // test fixture
		t.Fatalf("write fake codex: %v", err)
	}
	work := filepath.Join(home, "work", "repo")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatalf("mkdir work: %v", err)
	}
	const helperSecret = "d0dd0d0dbeefcafe"
	if err := os.WriteFile(filepath.Join(home, ".wardyn", "git-helper.secret"), []byte(helperSecret), 0o400); err != nil {
		t.Fatalf("write git-helper secret: %v", err)
	}

	cmd := exec.Command("timeout", "10s", "bash", cxRunnableAgentRun(t), "--boot-seed")
	cmd.Env = append(os.Environ(),
		"HOME="+home,
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"WARDYN_INTERACTIVE_START=agent",
		ccSeedEnv,
		"WARDYN_RECORDING=",
		"WARDYN_GIT_HELPER_SECRET=",
		// The pane waits while prep is owed AND the idle process is alive. Point
		// that at this test process: alive, and the same uid, which `kill -0`
		// needs. In the sandbox it is pid 1 — agent-run --idle itself.
		"WARDYN_IDLE_PID="+strconv.Itoa(os.Getpid()),
	)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start agent-run --boot-seed: %v", err)
	}
	// Still waiting: nothing may be started against the unprepared workspace. The
	// wait's own granularity is 1s (`sleep 1`), so give it two turns.
	time.Sleep(2500 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(home, "codex.log")); err == nil {
		t.Fatalf("the boot pane started the seed before prep finished — it would run the agent against an empty "+
			"workspace\ncodex log:\n%s", ccRead(t, filepath.Join(home, "codex.log")))
	}
	// Release prep, pointing the pane at the "cloned" repo.
	if err := os.WriteFile(filepath.Join(home, ".wardyn", "workdir"), []byte(work+"\n"), 0o600); err != nil {
		t.Fatalf("write workdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(home, ".wardyn", "prep-done"), nil, 0o600); err != nil {
		t.Fatalf("write prep-done: %v", err)
	}
	_ = cmd.Wait()
	got := ccRead(t, filepath.Join(home, "codex.log"))
	if !strings.Contains(got, work) {
		t.Errorf("the seed did not run in the prepared workspace %q\ncodex log:\n%s", work, got)
	}
	if !strings.Contains(got, "say hello in five words") {
		t.Errorf("the seed never reached codex\ncodex log:\n%s", got)
	}
	if !strings.Contains(got, "secret="+helperSecret) {
		t.Errorf("the boot pane did not recover WARDYN_GIT_HELPER_SECRET from prep's 0400 file — a seeded agent's "+
			"brokered git would be refused a token\ncodex log:\n%s", got)
	}
}
