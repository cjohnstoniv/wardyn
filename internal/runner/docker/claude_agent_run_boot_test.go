// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package docker

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The claude-code image's BOOT: what the agent dials before it has been asked to
// do anything, and what it shows the human when an interactive run comes up.
//
// These are PURE-SHELL tests over the REAL scripts, the idiom
// attach_bashrc_test.go uses: the behaviour under test is shell, so only a shell
// can prove it. Nothing here needs a container or a docker daemon, which is why
// this file carries no build tag — the boot path must be covered by the same
// gate that covers the attach path.
const (
	ccAgentRunPath    = "../../../deploy/images/claude-code/agent-run"
	ccDockerfilePath  = "../../../deploy/images/claude-code/Dockerfile"
	ccAgentRunLibPath = "../../../deploy/images/common/agent-run-lib.sh"
)

func ccAbs(t *testing.T, rel string) string {
	t.Helper()
	p, err := filepath.Abs(rel)
	if err != nil {
		t.Fatalf("resolve %s: %v", rel, err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("%s not found: %v", p, err)
	}
	return p
}

func ccRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // fixed in-repo or test-owned path
	if err != nil {
		return ""
	}
	return string(b)
}

func ccCount(s, want string) int {
	n := 0
	for _, l := range strings.Split(s, "\n") {
		if strings.Contains(l, want) {
			n++
		}
	}
	return n
}

// ── the agent's own boot traffic ─────────────────────────────────────────────

// TestClaudeImage_DisablesAutoUpdater — finding 5: on a stock install the FIRST
// Claude Code run anyone launches stops on a first-use approval for
// downloads.claude.ai, before they have asked the agent to do anything. The CLI
// checks that CDN for a newer release ON STARTUP, and that check does not care
// how it was installed — this image installs from npm at a pinned version, so
// the BUILD is clean and the RUN still dials it.
//
// Two disjoint paths, so two assertions: the image ENV covers an attach shell
// where a human types `claude` and agent-run never ran; the library export
// covers a BYOI/corp image that COPYs agent-run but not our Dockerfile. `:-` in
// the library so an operator who deliberately wants the updater can still say so
// on the run — asserted, because a hard `=1` would be a control the operator
// cannot turn off.
func TestClaudeImage_DisablesAutoUpdater(t *testing.T) {
	dockerfile := ccRead(t, ccAbs(t, ccDockerfilePath))
	for _, want := range []string{"DISABLE_AUTOUPDATER=1", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1"} {
		if !strings.Contains(dockerfile, want) {
			t.Errorf("claude-code Dockerfile never sets %s — a first run parks a first-use approval on the agent's own CDN", want)
		}
	}

	lib := ccAbs(t, ccAgentRunLibPath)
	// Sourcing the library is the whole contract: the vars must be set by the
	// time any prep or --selftest runs, not by a function a BYOI agent-run may
	// never call.
	run := func(t *testing.T, pre ...string) map[string]string {
		t.Helper()
		cmd := exec.Command("bash", "-c",
			`set -euo pipefail; source "$1"; `+
				`printf 'DISABLE_AUTOUPDATER=%s\n' "${DISABLE_AUTOUPDATER:-<unset>}"; `+
				`printf 'CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=%s\n' "${CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC:-<unset>}"`,
			"agent-run-lib-test", lib,
		)
		cmd.Env = append(os.Environ(), pre...)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("sourcing agent-run-lib.sh failed: %v", err)
		}
		got := map[string]string{}
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			k, v, _ := strings.Cut(line, "=")
			got[k] = v
		}
		return got
	}

	t.Run("defaults to off", func(t *testing.T) {
		got := run(t, "DISABLE_AUTOUPDATER=", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=")
		for k, want := range map[string]string{
			"DISABLE_AUTOUPDATER":                      "1",
			"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1",
		} {
			if got[k] != want {
				t.Errorf("after sourcing agent-run-lib.sh %s = %q, want %q", k, got[k], want)
			}
		}
	})

	t.Run("operator opt-out survives", func(t *testing.T) {
		got := run(t, "DISABLE_AUTOUPDATER=0", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=0")
		for k := range got {
			if got[k] != "0" {
				t.Errorf("%s = %q, want the operator's own 0 preserved (the export must use `:-`)", k, got[k])
			}
		}
	})
}

// ── the onboarding seed ──────────────────────────────────────────────────────

// ccSeed sources the real library and calls seed_claude_onboarding with the
// given env, returning the scratch $HOME it ran against.
func ccSeed(t *testing.T, env ...string) string {
	t.Helper()
	home := t.TempDir()
	cmd := exec.Command("bash", "-c",
		`set -euo pipefail; source "$1"; seed_claude_onboarding`,
		"agent-run-lib-test", ccAbs(t, ccAgentRunLibPath),
	)
	cmd.Env = append(os.Environ(), append([]string{"HOME=" + home}, env...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("seed_claude_onboarding failed: %v\noutput: %s", err, out)
	}
	return home
}

// TestSeedClaudeOnboarding_WritesUnderBedrock — the bedrock_sso lane is the one
// this release cares about, and it is the lane with NO CLAUDE_CONFIG_DIR:
// dispatch sets that var only for the subscription and managed paths, so the
// seed's second file is the ${CLAUDE_CONFIG_DIR:-$HOME/.claude} FALLBACK. Both
// files matter: claude v1 reads $HOME/.claude.json, v2 keeps its state inside
// the config dir.
//
// Without the seed an interactive bedrock run met the CLI's own first-use
// screens (theme picker, then "Security notes") before it could reach the model
// — which is why ui/e2e/live/sso-member.spec.ts could only ever launch an
// autonomous run.
func TestSeedClaudeOnboarding_WritesUnderBedrock(t *testing.T) {
	home := ccSeed(t, "CLAUDE_CONFIG_DIR=", "CLAUDE_CODE_USE_BEDROCK=1")
	for _, rel := range []string{".claude.json", ".claude/.claude.json"} {
		got := ccRead(t, filepath.Join(home, rel))
		if !strings.Contains(got, `"hasCompletedOnboarding":true`) {
			t.Errorf("~/%s = %q, want the onboarding marker (CLAUDE_CONFIG_DIR is UNSET on the bedrock lane, so this is the fallback path)", rel, got)
		}
	}
}

// TestSeedClaudeOnboarding_HonoursClaudeConfigDir — the subscription/managed
// lanes DO set CLAUDE_CONFIG_DIR (dispatch points it at a writable dir because
// ~/.claude may be a read-only mount), and the seed must land there rather than
// in the image default nobody reads.
func TestSeedClaudeOnboarding_HonoursClaudeConfigDir(t *testing.T) {
	cfg := t.TempDir()
	home := ccSeed(t, "CLAUDE_CONFIG_DIR="+cfg)
	if got := ccRead(t, filepath.Join(cfg, ".claude.json")); !strings.Contains(got, "hasCompletedOnboarding") {
		t.Errorf("$CLAUDE_CONFIG_DIR/.claude.json = %q, want the onboarding marker", got)
	}
	if _, err := os.Stat(filepath.Join(home, ".claude", ".claude.json")); err == nil {
		t.Error("the seed also wrote the image-default config dir, which claude does not read when CLAUDE_CONFIG_DIR is set")
	}
}

// TestSeedClaudeOnboarding_NeverClobbers — a resident ~/.claude.json (the
// subscription wizard mounts the operator's own) or one claude itself wrote is
// state we do not own. Fill-missing only.
func TestSeedClaudeOnboarding_NeverClobbers(t *testing.T) {
	home := t.TempDir()
	const resident = `{"hasCompletedOnboarding":true,"theme":"light","userID":"resident"}`
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte(resident), 0o600); err != nil {
		t.Fatalf("write resident config: %v", err)
	}
	cmd := exec.Command("bash", "-c",
		`set -euo pipefail; source "$1"; seed_claude_onboarding`,
		"agent-run-lib-test", ccAbs(t, ccAgentRunLibPath),
	)
	cmd.Env = append(os.Environ(), "HOME="+home, "CLAUDE_CONFIG_DIR=")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("seed_claude_onboarding failed: %v\noutput: %s", err, out)
	}
	if got := ccRead(t, filepath.Join(home, ".claude.json")); got != resident {
		t.Errorf("~/.claude.json = %q, want the resident file byte-for-byte %q", got, resident)
	}
}

// TestSeedClaudeOnboarding_NeverSeedsASecurityKey — GREEN TODAY, and it is the
// pin that has to stay green. `hasCompletedOnboarding` is PRODUCT onboarding: a
// theme picker and a "Security notes / Press Enter" page. The two keys below are
// not, and neither may ever be seeded:
//
//   - projects.<workdir>.hasTrustDialogAccepted answers "do you trust this
//     folder's code and its project settings?" — the one screen a human is meant
//     to see, and the gate on a cloned repo's own settings taking effect unseen.
//   - bypassPermissionsModeAccepted answers the CLI's "Bypass Permissions mode"
//     confirmation, whose default selection is `No, exit`.
//
// Seeding either would not even buy an unattended seeded run (measured on claude
// 2.1.231, local/v075/evidence/w0-spike/RESULT.md: pre-accepting trust still
// leaves the bypass confirmation in the way). It would only pre-answer a
// security question on the operator's behalf. Asserted in EVERY mode, including
// the auto-tools + agent-start combination that is the tempting one.
func TestSeedClaudeOnboarding_NeverSeedsASecurityKey(t *testing.T) {
	modes := []struct {
		name string
		env  []string
	}{
		{"plain interactive", []string{"CLAUDE_CONFIG_DIR="}},
		{"agent start, no seed", []string{"CLAUDE_CONFIG_DIR=", "WARDYN_INTERACTIVE_START=agent"}},
		{"agent start + seed + auto-tools", []string{
			"CLAUDE_CONFIG_DIR=",
			"WARDYN_INTERACTIVE_START=agent",
			"WARDYN_INTERACTIVE_SEED=say hello in five words",
			"WARDYN_SEED_AUTO_TOOLS=1",
		}},
		{"shell start + seed", []string{
			"CLAUDE_CONFIG_DIR=",
			"WARDYN_INTERACTIVE_START=shell",
			"WARDYN_INTERACTIVE_SEED=make test",
			"WARDYN_SEED_AUTO_TOOLS=1",
		}},
	}
	for _, m := range modes {
		t.Run(m.name, func(t *testing.T) {
			home := ccSeed(t, m.env...)
			for _, rel := range []string{".claude.json", ".claude/.claude.json"} {
				got := ccRead(t, filepath.Join(home, rel))
				for _, forbidden := range []string{"hasTrustDialogAccepted", "bypassPermissionsModeAccepted"} {
					if strings.Contains(got, forbidden) {
						t.Errorf("~/%s seeds %s — Wardyn pre-answers product onboarding, never a security prompt\nfile: %s", rel, forbidden, got)
					}
				}
			}
		})
	}
}

// TestManagedMode_StillSeedsOnboarding — NEGATIVE CONTROL for the hoist. The
// managed-subscription path used to write the onboarding marker itself, inside
// materialize_managed_claude_config. Hoisting it out must not cost the managed
// lane the marker (the sentinel credentials alone leave an interactive managed
// session on the onboarding screens), and must not clobber the config
// prepare_claude_config_dir may already have copied in from the read-only mount.
func TestManagedMode_StillSeedsOnboarding(t *testing.T) {
	home := t.TempDir()
	cfg := filepath.Join(home, "cfg")
	// The order the image runs them in.
	cmd := exec.Command("bash", "-c",
		`set -euo pipefail; source "$1"; prepare_claude_config_dir; seed_claude_onboarding; materialize_managed_claude_config`,
		"agent-run-lib-test", ccAbs(t, ccAgentRunLibPath),
	)
	cmd.Env = append(os.Environ(),
		"HOME="+home,
		"CLAUDE_CONFIG_DIR="+cfg,
		// base64 of {"sentinel":true}
		"WARDYN_CLAUDE_MANAGED_B64=eyJzZW50aW5lbCI6dHJ1ZX0=",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("managed prep failed: %v\noutput: %s", err, out)
	}
	if got := ccRead(t, filepath.Join(cfg, ".claude.json")); !strings.Contains(got, "hasCompletedOnboarding") {
		t.Errorf("$CLAUDE_CONFIG_DIR/.claude.json = %q — the managed lane lost its onboarding marker to the hoist", got)
	}
	if got := ccRead(t, filepath.Join(home, ".claude.json")); !strings.Contains(got, "hasCompletedOnboarding") {
		t.Errorf("~/.claude.json = %q — the managed lane lost its v1 onboarding marker to the hoist", got)
	}
	if got := ccRead(t, filepath.Join(cfg, ".credentials.json")); !strings.Contains(got, "sentinel") {
		t.Errorf("$CLAUDE_CONFIG_DIR/.credentials.json = %q — the hoist broke the managed sentinel itself", got)
	}
}

// ── the boot session, and the race it has to win ─────────────────────────────

// ccRunnableAgentRun copies the REAL claude-code agent-run and redirects its one
// absolute `source` at the real library. Nothing is stubbed — /usr/local/bin
// does not exist on a test host, and the body under test (the boot-session
// bootstrap, and where it sits relative to the shared prep) is byte-for-byte the
// shipped script.
func ccRunnableAgentRun(t *testing.T) string {
	t.Helper()
	body := ccRead(t, ccAbs(t, ccAgentRunPath))
	const from = "source /usr/local/bin/agent-run-lib.sh"
	if !strings.Contains(body, from) {
		t.Fatalf("claude-code agent-run no longer sources %s", from)
	}
	body = strings.Replace(body, from, "source "+ccAbs(t, ccAgentRunLibPath), 1)
	dst := filepath.Join(t.TempDir(), "agent-run")
	if err := os.WriteFile(dst, []byte(body), 0o700); err != nil { //nolint:gosec // test fixture
		t.Fatalf("write runnable agent-run: %v", err)
	}
	return dst
}

// ccIdleEnv lays out a scratch $HOME plus a PATH dir holding a fake `tmux` that
// appends its argv AND the three facts the ordering tests turn on, sampled at
// the instant agent-run calls it: whether the agent-started marker is already
// there, whether the first prep side effect has happened, and whether prep-done
// has.
func ccIdleEnv(t *testing.T) (home, binDir, tmuxLog string) {
	t.Helper()
	root := t.TempDir()
	home, binDir, tmuxLog = filepath.Join(root, "home"), filepath.Join(root, "bin"), filepath.Join(root, "tmux.log")
	for _, d := range []string{home, binDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	fake := `#!/bin/sh
{
  printf 'argv:'
  for a in "$@"; do printf ' %s' "$a"; done
  printf '\n'
  if [ -e "$HOME/.wardyn/agent-started" ]; then printf 'marker: present\n'; else printf 'marker: absent\n'; fi
  if [ -e "$CLAUDE_CONFIG_DIR" ]; then printf 'claude-config-dir: present\n'; else printf 'claude-config-dir: absent\n'; fi
  if [ -e "$HOME/.wardyn/prep-done" ]; then printf 'prep-done: present\n'; else printf 'prep-done: absent\n'; fi
} >> "$TMUX_LOG"
case "$1" in
  new-session) rc=${FAKE_TMUX_NEW_SESSION_RC:-0} ;;
  *)           rc=0 ;;
esac
[ "$rc" -eq 0 ] || echo "duplicate session: wardyn" >&2
exit "$rc"
`
	if err := os.WriteFile(filepath.Join(binDir, "tmux"), []byte(fake), 0o700); err != nil { //nolint:gosec // test fixture
		t.Fatalf("write fake tmux: %v", err)
	}
	return home, binDir, tmuxLog
}

// ccRunIdle runs the real `agent-run --idle` under `timeout`, the same proof
// oracle_agent_run_idle_test.go uses: a script that holds the container open is
// KILLED by timeout (124); one that exits on its own reports its own code.
func ccRunIdle(t *testing.T, home, binDir, tmuxLog string, extraEnv ...string) (code int, out string) {
	t.Helper()
	cmd := exec.Command("timeout", "3s", "bash", ccRunnableAgentRun(t), "--idle")
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

const ccSeedEnv = "WARDYN_INTERACTIVE_SEED=say hello in five words"

// TestClaudeAgentRun_BootSessionPrecedesPrep — the boot session MUST win the
// `wardyn` session name. Both drivers attach with `tmux new-session -A -s wardyn
// bash` the instant the container runs (attachShell, session.go), and the shared
// prep is a measured 18 s when the run clones a repo. Created after prep the
// session loses outright: `new-session` fails as `duplicate session: wardyn`,
// the seed never starts, and the only record is a warning on the container's
// stderr where nobody looks — the operator's task text silently dropped.
func TestClaudeAgentRun_BootSessionPrecedesPrep(t *testing.T) {
	home, binDir, tmuxLog := ccIdleEnv(t)
	code, out := ccRunIdle(t, home, binDir, tmuxLog, ccSeedEnv)
	if code != 124 {
		t.Fatalf("agent-run --idle exited %d (output: %s), want 124 — it must hold the container open for attach", code, out)
	}
	log := ccRead(t, tmuxLog)
	if n := ccCount(log, "argv: new-session -d -s wardyn agent-run --boot-seed"); n != 1 {
		t.Fatalf("tmux got %d `new-session -d -s wardyn agent-run --boot-seed` calls, want exactly 1\ntmux log:\n%s\nagent-run output:\n%s", n, log, out)
	}
	if !strings.Contains(log, "claude-config-dir: absent") {
		t.Errorf("the boot session was created AFTER the shared prep had already run — an attach that arrives during prep wins the session name and the seed is lost\ntmux log:\n%s", log)
	}
	if _, err := os.Stat(filepath.Join(home, "cfg")); err != nil {
		t.Fatalf("the shared prep never created CLAUDE_CONFIG_DIR, so the ordering assertion above proved nothing: %v", err)
	}
}

// TestClaudeAgentRun_MarkerPrecedesPrep — the OTHER half of the same race, and
// the one the plan's first sketch missed. attach-bashrc.sh's first-shell block
// starts a bare `claude` unless ~/.wardyn/agent-started already exists, so an
// attach shell that sourced ~/.bashrc during prep launched its own UNSEEDED
// agent beside the boot pane: the run's task text dropped AND its posture
// silently flipped from auto-tools to supervised (the bare `claude` carries no
// --dangerously-skip-permissions). The marker has to be written before any
// attach shell can read it, which means before prep, not after.
func TestClaudeAgentRun_MarkerPrecedesPrep(t *testing.T) {
	home, binDir, tmuxLog := ccIdleEnv(t)
	if _, out := ccRunIdle(t, home, binDir, tmuxLog, ccSeedEnv); out == "" {
		t.Log("agent-run produced no output")
	}
	log := ccRead(t, tmuxLog)
	if !strings.Contains(log, "argv: new-session") {
		t.Fatalf("agent-run --idle never created the boot session\ntmux log:\n%s", log)
	}
	if !strings.Contains(log, "marker: present") {
		t.Errorf("the agent-started marker was not written before the boot session — an attach shell that lands during prep bare-launches a second, unseeded claude\ntmux log:\n%s", log)
	}
	if !strings.Contains(log, "claude-config-dir: absent") {
		t.Errorf("the marker was written, but only after the shared prep had begun — that is exactly the window the early attach lands in\ntmux log:\n%s", log)
	}
}

// TestClaudeAgentRun_EarlyAttachWonTheName — the belt for the race we cannot
// win. An attach that beats agent-run created the session running its own bare
// `bash`; respawning that pane on the seed is what keeps the promise. Exactly
// one respawn: a loop would restart the agent under a human already using it.
func TestClaudeAgentRun_EarlyAttachWonTheName(t *testing.T) {
	home, binDir, tmuxLog := ccIdleEnv(t)
	code, out := ccRunIdle(t, home, binDir, tmuxLog, ccSeedEnv, "FAKE_TMUX_NEW_SESSION_RC=1")
	if code != 124 {
		t.Fatalf("agent-run --idle exited %d (output: %s), want 124 — a duplicate session must not kill the run", code, out)
	}
	log := ccRead(t, tmuxLog)
	if n := ccCount(log, "argv: respawn-pane -k -t wardyn agent-run --boot-seed"); n != 1 {
		t.Errorf("tmux got %d `respawn-pane -k -t wardyn agent-run --boot-seed` calls, want exactly 1 — an attach won the session name and nothing replaced its bare shell\ntmux log:\n%s\nagent-run output:\n%s", n, log, out)
	}
}

// TestClaudeAgentRun_NoSeedChangesNothing — NEGATIVE CONTROL. The console's
// DEFAULT interactive run carries no seed (the Initial-prompt textarea is
// optional), and that shape must be byte-identical in behaviour to before this
// change: no boot session, no agent-started marker, and therefore
// attach-bashrc.sh still starts the agent on the human's first attach — which is
// the whole of the owner's literal path.
func TestClaudeAgentRun_NoSeedChangesNothing(t *testing.T) {
	home, binDir, tmuxLog := ccIdleEnv(t)
	code, out := ccRunIdle(t, home, binDir, tmuxLog, "WARDYN_INTERACTIVE_START=agent")
	if code != 124 {
		t.Fatalf("agent-run --idle exited %d (output: %s), want 124", code, out)
	}
	if log := ccRead(t, tmuxLog); log != "" {
		t.Errorf("an unseeded interactive run called tmux; it must start no boot session at all\ntmux log:\n%s", log)
	}
	if _, err := os.Stat(filepath.Join(home, ".wardyn", "agent-started")); err == nil {
		t.Error("an unseeded interactive run wrote the agent-started marker — attach-bashrc.sh would then start nothing and the human gets a bare shell")
	}
	if _, err := os.Stat(filepath.Join(home, ".wardyn", "prep-done")); err != nil {
		t.Errorf("the unseeded run never finished prep: %v", err)
	}
}

// TestClaudeAgentRun_BootSeedWaitsForPrep — the cost of creating the session
// first: the pane opens while ~/work may still be empty and ~/.wardyn/workdir
// unwritten. A seed that started there would run the agent against a workspace
// with no repo in it, which is the failure the boot session existed to prevent
// in the first place.
func TestClaudeAgentRun_BootSeedWaitsForPrep(t *testing.T) {
	root := t.TempDir()
	home, binDir := filepath.Join(root, "home"), filepath.Join(root, "bin")
	for _, d := range []string{filepath.Join(home, ".wardyn"), binDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	// A `claude` that records the workspace it was started in.
	fake := "#!/bin/sh\npwd >> \"$HOME/claude.log\"\nprintf '%s\\n' \"$*\" >> \"$HOME/claude.log\"\n" +
		"printf 'secret=%s\\n' \"${WARDYN_GIT_HELPER_SECRET:-<unset>}\" >> \"$HOME/claude.log\"\n"
	if err := os.WriteFile(filepath.Join(binDir, "claude"), []byte(fake), 0o700); err != nil { //nolint:gosec // test fixture
		t.Fatalf("write fake claude: %v", err)
	}
	work := filepath.Join(home, "work", "repo")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatalf("mkdir work: %v", err)
	}

	// The git-helper caller-auth secret prep writes. The tmux server was started
	// BEFORE prep now, so the pane cannot have inherited the exported value — it
	// has to recover it from this file or the agent's brokered git goes dark.
	const helperSecret = "d0dd0d0dbeefcafe"
	if err := os.WriteFile(filepath.Join(home, ".wardyn", "git-helper.secret"), []byte(helperSecret), 0o400); err != nil {
		t.Fatalf("write git-helper secret: %v", err)
	}

	cmd := exec.Command("timeout", "10s", "bash", ccRunnableAgentRun(t), "--boot-seed")
	cmd.Env = append(os.Environ(),
		"HOME="+home,
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"WARDYN_INTERACTIVE_START=agent",
		ccSeedEnv,
		"CLAUDE_CODE_USE_BEDROCK=1",
		"WARDYN_RECORDING=",
		"WARDYN_GIT_HELPER_SECRET=",
	)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start agent-run --boot-seed: %v", err)
	}
	// Still waiting: nothing may be started against the unprepared workspace.
	// The wait's own granularity is 1s (`sleep 1`), so give it two turns.
	time.Sleep(2500 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(home, "claude.log")); err == nil {
		t.Fatalf("the boot pane started the seed before prep finished — it would run the agent against an empty workspace\nclaude log:\n%s", ccRead(t, filepath.Join(home, "claude.log")))
	}
	// Release prep, pointing the pane at the "cloned" repo.
	if err := os.WriteFile(filepath.Join(home, ".wardyn", "workdir"), []byte(work+"\n"), 0o600); err != nil {
		t.Fatalf("write workdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(home, ".wardyn", "prep-done"), nil, 0o600); err != nil {
		t.Fatalf("write prep-done: %v", err)
	}
	_ = cmd.Wait()
	got := ccRead(t, filepath.Join(home, "claude.log"))
	if !strings.Contains(got, work) {
		t.Errorf("the seed did not run in the prepared workspace %q\nclaude log:\n%s", work, got)
	}
	if !strings.Contains(got, "say hello in five words") {
		t.Errorf("the seed never reached claude\nclaude log:\n%s", got)
	}
	// The pane was created before prep, so it can only have this by re-reading
	// the 0400 file. Without it the credential helper refuses to mint and the
	// seeded agent's brokered git silently stops working.
	if !strings.Contains(got, "secret="+helperSecret) {
		t.Errorf("the boot pane did not recover WARDYN_GIT_HELPER_SECRET from prep's 0400 file — a seeded agent's brokered git would be refused a token\nclaude log:\n%s", got)
	}
}
