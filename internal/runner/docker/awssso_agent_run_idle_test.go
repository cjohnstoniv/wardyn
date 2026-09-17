// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package docker

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// The AWS sign-in sandbox runs the chained login command ITSELF now: an
// operator who opened that run from the Runs list used to get a bare prompt,
// type the obvious half (`aws sso login`), see "Successfully logged into Start
// URL", and capture nothing — the token stays in ~/.aws/sso/cache and dies with
// the container, because wardyn-aws-sso is the half that uploads it.
//
// These are PURE-SHELL tests, the idiom attach_bashrc_test.go uses: they run the
// REAL scripts, because the behaviour under test is shell and only a shell can
// prove it. Nothing here needs a container or a docker daemon.
const (
	awsSSOAgentRunPath    = "../../../deploy/images/aws-sso/agent-run"
	awsSSOSigninPane      = "../../../deploy/images/aws-sso/signin-pane.sh"
	awsSSOLoginHintPath   = "../../../deploy/images/aws-sso/login-hint.sh"
	awsSSOAgentRunLibPath = "../../../deploy/images/common/agent-run-lib.sh"
)

// The three operator-facing lines the sign-in pane prints, read from their ONE
// definition so every assertion below goes through the constant rather than a
// second copy of the prose (login-hint.sh is what the image installs at
// /usr/local/lib/wardyn-attach-hint.sh).
var shellConst = regexp.MustCompile(`(?m)^(WARDYN_AWS_SSO_[A-Z_]+)='([^']*)'`)

func loginHintConsts(t *testing.T) map[string]string {
	t.Helper()
	b, err := os.ReadFile(awsSSOLoginHintPath) //nolint:gosec // fixed in-repo path
	if err != nil {
		t.Fatalf("read login-hint.sh: %v", err)
	}
	out := map[string]string{}
	for _, m := range shellConst.FindAllStringSubmatch(string(b), -1) {
		out[m[1]] = m[2]
	}
	for _, k := range []string{
		"WARDYN_AWS_SSO_LOGIN_COMMAND",
		"WARDYN_AWS_SSO_SELFRUN_BANNER",
		"WARDYN_AWS_SSO_SELFRUN_DONE",
		"WARDYN_AWS_SSO_SELFRUN_FAILED",
	} {
		if out[k] == "" {
			t.Fatalf("login-hint.sh defines no %s — the sign-in pane has no %s to print", k, k)
		}
	}
	return out
}

// writeExec drops a small executable script into dir.
func writeExec(t *testing.T, dir, name, body string) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o700); err != nil { //nolint:gosec // test fixture
		t.Fatalf("write fake %s: %v", name, err)
	}
}

// runnableAgentRun copies the REAL aws-sso agent-run and redirects its two
// absolute `source` paths at the REAL files they are installed from. Nothing is
// stubbed — /usr/local/bin and /usr/local/lib simply do not exist on a test host,
// and the body under test (the tmux bootstrap, and where it sits relative to the
// shared prep) is byte-for-byte the shipped script.
func runnableAgentRun(t *testing.T) string {
	t.Helper()
	src, err := os.ReadFile(awsSSOAgentRunPath) //nolint:gosec // fixed in-repo path
	if err != nil {
		t.Fatalf("read aws-sso agent-run: %v", err)
	}
	lib, err := filepath.Abs(awsSSOAgentRunLibPath)
	if err != nil {
		t.Fatalf("resolve agent-run-lib.sh: %v", err)
	}
	hint, err := filepath.Abs(awsSSOLoginHintPath)
	if err != nil {
		t.Fatalf("resolve login-hint.sh: %v", err)
	}
	body := string(src)
	for from, to := range map[string]string{
		"/usr/local/bin/agent-run-lib.sh":      lib,
		"/usr/local/lib/wardyn-attach-hint.sh": hint,
	} {
		if !strings.Contains(body, "source "+from) {
			t.Fatalf("aws-sso agent-run no longer sources %s", from)
		}
		body = strings.Replace(body, "source "+from, "source "+to, 1)
	}
	dst := filepath.Join(t.TempDir(), "agent-run")
	if err := os.WriteFile(dst, []byte(body), 0o700); err != nil { //nolint:gosec // test fixture
		t.Fatalf("write runnable agent-run: %v", err)
	}
	return dst
}

// fakeTmuxBin lays out a $HOME and a PATH dir holding a fake `tmux` that appends
// its argv — plus the two facts the ordering tests turn on: whether the shared
// prep has written ~/.aws/config yet, and whether it has written prep-done.
func fakeTmuxBin(t *testing.T) (home, binDir, tmuxLog string) {
	t.Helper()
	root := t.TempDir()
	home = filepath.Join(root, "home")
	binDir = filepath.Join(root, "bin")
	tmuxLog = filepath.Join(root, "tmux.log")
	for _, d := range []string{home, binDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	writeExec(t, binDir, "tmux", `#!/bin/sh
{
  printf 'argv:'
  for a in "$@"; do printf ' %s' "$a"; done
  printf '\n'
  printf 'login-command: %s\n' "${WARDYN_AWS_SSO_LOGIN_COMMAND:-<unset>}"
  if grep -qs "$MITM_NONCE" /tmp/wardyn/mitm-ca.pem; then printf 'mitm-ca: this-run\n'; else printf 'mitm-ca: not-yet\n'; fi
  if [ -e "$HOME/.aws/config" ]; then printf 'aws-config: present\n'; else printf 'aws-config: absent\n'; fi
  if [ -e "$HOME/.wardyn/prep-done" ]; then printf 'prep-done: present\n'; else printf 'prep-done: absent\n'; fi
} >> "$TMUX_LOG"
case "$1" in
  new-session) rc=${FAKE_TMUX_NEW_SESSION_RC:-0} ;;
  *)           rc=0 ;;
esac
[ "$rc" -eq 0 ] || echo "duplicate session: wardyn" >&2
exit "$rc"
`)
	return home, binDir, tmuxLog
}

// runIdle runs the real `agent-run --idle` under `timeout`, the same proof
// oracle_agent_run_idle_test.go uses: a script that holds the container open is
// KILLED by timeout (124); one that exits on its own reports its own code.
func runIdle(t *testing.T, home, binDir, tmuxLog string, extraEnv ...string) (code int, out string) {
	t.Helper()
	return runIdleOnPath(t, home, binDir+string(os.PathListSeparator)+os.Getenv("PATH"), tmuxLog, extraEnv...)
}

// runIdleOnPath is runIdle with the child's PATH given VERBATIM — the no-tmux
// control needs a PATH that does not reach the host's own tmux.
func runIdleOnPath(t *testing.T, home, pathValue, tmuxLog string, extraEnv ...string) (code int, out string) {
	t.Helper()
	cmd := exec.Command("timeout", "3s", "bash", runnableAgentRun(t), "--idle")
	cmd.Env = append(os.Environ(), append([]string{
		"HOME=" + home,
		"PATH=" + pathValue,
		"TMUX_LOG=" + tmuxLog,
		// install_mitm_ca is the FIRST prep call and writes /tmp/wardyn/mitm-ca.pem
		// — a path it hard-codes and every lane on the box shares, so the fake tmux
		// keys on THIS run's nonce being in it rather than on the file existing.
		"MITM_NONCE=" + mitmNonce(t),
		"WARDYN_MITM_CA_PEM=-----BEGIN CERTIFICATE-----\n" + mitmNonce(t) + "\n-----END CERTIFICATE-----",
		// The one prep step that writes a file this test can see, so "before the
		// FIRST prep side effect" is an observable claim and not merely "before
		// prep-done" (which would go green with the race still open).
		"WARDYN_AWS_SSO_CONFIG_B64=.aws/config\tW3Nzby1zZXNzaW9uIHdhcmR5bl0K",
		"WARDYN_REPOS=",
		"WARDYN_REPO_URL=",
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

// mitmNonce is stable per test, so the value the fake tmux greps for is the value
// this run's install_mitm_ca wrote.
func mitmNonce(t *testing.T) string {
	t.Helper()
	return "wardyn-test-nonce-" + strings.ReplaceAll(t.Name(), "/", "-")
}

func readLog(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // test-owned temp path
	if err != nil {
		return ""
	}
	return string(b)
}

func countLines(s, want string) int {
	n := 0
	for _, l := range strings.Split(s, "\n") {
		if strings.Contains(l, want) {
			n++
		}
	}
	return n
}

// TestAWSSSOAgentRun_IdleStartsTheSignInSession — finding 4's core: the IMAGE
// starts the sign-in, so every attach path joins a session that is already
// running it instead of landing on a bare prompt.
func TestAWSSSOAgentRun_IdleStartsTheSignInSession(t *testing.T) {
	home, binDir, tmuxLog := fakeTmuxBin(t)
	code, out := runIdle(t, home, binDir, tmuxLog)
	if code != 124 {
		t.Fatalf("agent-run --idle exited %d (output: %s), want 124 — it must hold the container open for attach", code, out)
	}
	log := readLog(t, tmuxLog)
	if n := countLines(log, "argv: new-session -d -s wardyn signin-pane.sh"); n != 1 {
		t.Errorf("tmux got %d `new-session -d -s wardyn signin-pane.sh` calls, want exactly 1\ntmux log:\n%s\nagent-run output:\n%s", n, log, out)
	}
	// The pane reads the pair out of the environment the tmux server inherits,
	// so the ONE definition in login-hint.sh has to reach it exported — and it
	// has to still be the pair, not the login alone.
	if !strings.Contains(log, "aws sso login") || !strings.Contains(log, "wardyn-aws-sso") {
		t.Errorf("the tmux server did not inherit the chained login command (a login alone captures nothing)\ntmux log:\n%s", log)
	}
}

// TestAWSSSOAgentRun_SessionIsCreatedBeforePrep — the session must win the name.
// Both drivers attach with `tmux new-session -A -s wardyn bash` the instant the
// container runs, and the shared prep below is a measured ~18s: a session
// created after it loses to an early attach, `new-session` fails as a duplicate,
// and the human is back at the bare shell this whole change removes.
func TestAWSSSOAgentRun_SessionIsCreatedBeforePrep(t *testing.T) {
	home, binDir, tmuxLog := fakeTmuxBin(t)
	if _, out := runIdle(t, home, binDir, tmuxLog); out == "" {
		t.Log("agent-run produced no output")
	}
	log := readLog(t, tmuxLog)
	if !strings.Contains(log, "argv: new-session") {
		t.Fatalf("agent-run --idle never created the sign-in session\ntmux log:\n%s", log)
	}
	// Before the FIRST prep call (install_mitm_ca), not merely before prep-done —
	// and not merely before materialize_aws_sso_config either, which is the third:
	// a bootstrap misplaced between the two would still pass that weaker test.
	if !strings.Contains(log, "mitm-ca: not-yet") {
		t.Errorf("the sign-in session was created AFTER install_mitm_ca, the first prep call — an attach that arrives during prep wins the session name\ntmux log:\n%s", log)
	}
	if !strings.Contains(log, "aws-config: absent") {
		t.Errorf("the sign-in session was created AFTER the shared prep had already written ~/.aws/config\ntmux log:\n%s", log)
	}
	// Both assertions are only worth anything if the prep they name actually ran.
	if b, err := os.ReadFile("/tmp/wardyn/mitm-ca.pem"); err != nil || !strings.Contains(string(b), mitmNonce(t)) {
		t.Fatalf("install_mitm_ca never wrote this run's CA (%v), so the ordering assertion above proved nothing", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".aws", "config")); err != nil {
		t.Fatalf("the shared prep never wrote ~/.aws/config, so this ordering assertion proved nothing: %v", err)
	}
}

// TestAWSSSOAgentRun_EarlyAttachWonTheName — the belt for the race we cannot
// win. An attach that beats agent-run created the session running its own bare
// `bash`; respawning that pane on the sign-in is what keeps the promise.
func TestAWSSSOAgentRun_EarlyAttachWonTheName(t *testing.T) {
	home, binDir, tmuxLog := fakeTmuxBin(t)
	code, out := runIdle(t, home, binDir, tmuxLog, "FAKE_TMUX_NEW_SESSION_RC=1")
	if code != 124 {
		t.Fatalf("agent-run --idle exited %d (output: %s), want 124 — a duplicate session must not kill the run", code, out)
	}
	log := readLog(t, tmuxLog)
	if n := countLines(log, "argv: respawn-pane -k -t wardyn signin-pane.sh"); n != 1 {
		t.Errorf("tmux got %d `respawn-pane -k -t wardyn signin-pane.sh` calls, want exactly 1 — an attach won the session name and nothing replaced its bare shell\ntmux log:\n%s\nagent-run output:\n%s", n, log, out)
	}
}

// pathWithoutTmux mirrors every PATH entry into one scratch dir, minus tmux —
// the only honest way to ask "what does this image do without tmux?" on a host
// that has it installed.
func pathWithoutTmux(t *testing.T) string {
	t.Helper()
	dst := filepath.Join(t.TempDir(), "notmux")
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatalf("mkdir notmux: %v", err)
	}
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir == "" {
			continue
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.Name() == "tmux" {
				continue
			}
			_ = os.Symlink(filepath.Join(dir, e.Name()), filepath.Join(dst, e.Name()))
		}
	}
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Fatalf("sleep is not on PATH at all: %v", err)
	}
	return dst
}

// TestAWSSSOAgentRun_NoTmuxStillHoldsOpen — NEGATIVE CONTROL. An interactive run
// must still come up attachable when the session cannot be created; the sign-in
// is the thing that is lost, not the sandbox, and the WARNING says so.
func TestAWSSSOAgentRun_NoTmuxStillHoldsOpen(t *testing.T) {
	home, _, tmuxLog := fakeTmuxBin(t)
	code, out := runIdleOnPath(t, home, pathWithoutTmux(t), tmuxLog)
	if code != 124 {
		t.Fatalf("agent-run --idle exited %d without tmux (output: %s), want 124 — losing the sign-in must not lose the sandbox", code, out)
	}
	if !strings.Contains(out, "WARNING no tmux in this image") {
		t.Errorf("no WARNING that the sign-in cannot start on its own\noutput:\n%s", out)
	}
}

// TestLoginHint_SilentAfterSelfRun — login-hint.sh's line fires for EVERY
// interactive shell in the image, INCLUDING the plain one the sign-in pane execs
// when it is done. Unguarded, a Runs-list user reads "the sign-in did not start
// on its own; run: …" right after a SUCCESSFUL capture, runs the pair again, and
// meets an already_captured refusal plus the console's fail marker.
//
// MUST be `bash -i -c`: the hint is guarded on `case $- in *i*`, so a plain
// source prints nothing in BOTH arms and the test would be vacuous.
func TestLoginHint_SilentAfterSelfRun(t *testing.T) {
	hint, err := filepath.Abs(awsSSOLoginHintPath)
	if err != nil {
		t.Fatalf("resolve login-hint.sh: %v", err)
	}
	// A scratch HOME: `bash -i` sources ~/.bashrc, and the developer's own would
	// otherwise be part of what this test measures.
	home := t.TempDir()
	run := func(env ...string) string {
		cmd := exec.Command("bash", "-i", "-c", ". "+hint)
		cmd.Env = append(os.Environ(), append([]string{"HOME=" + home}, env...)...)
		out, _ := cmd.CombinedOutput() // bash -i without a tty warns about job control
		return string(out)
	}
	const fallback = "did not start on its own"
	// The NEGATIVE CONTROL half: on an image whose sign-in really did not start
	// (no tmux, an old image), the line is the only thing that tells the human.
	if got := run("WARDYN_AWS_SSO_SELFRAN="); !strings.Contains(got, fallback) {
		t.Errorf("with WARDYN_AWS_SSO_SELFRAN unset the hint must still tell the human to run the pair\noutput:\n%s", got)
	}
	if got := run("WARDYN_AWS_SSO_SELFRAN=1"); strings.Contains(got, fallback) {
		t.Errorf("the hint claims the sign-in did not start in a shell the sign-in pane itself execs\noutput:\n%s", got)
	}
}

// signinPaneEnv lays out a $HOME + fake PATH for the sign-in pane: an `aws` that
// logs each invocation, a `wardyn-aws-sso` whose exit code the test picks, and a
// `tmux` that LOGS its argv — the session-environment mark is the thing that
// keeps a second tmux window from being told the sign-in never started, so it has
// to be observable, not merely swallowed.
type paneEnv struct {
	home, binDir, awsLog, tmuxLog string
	consts                        map[string]string
}

func signinPaneEnv(t *testing.T) paneEnv {
	t.Helper()
	root := t.TempDir()
	pe := paneEnv{
		home:    filepath.Join(root, "home"),
		binDir:  filepath.Join(root, "bin"),
		awsLog:  filepath.Join(root, "aws.log"),
		tmuxLog: filepath.Join(root, "tmux.log"),
		consts:  loginHintConsts(t),
	}
	for _, d := range []string{pe.home, pe.binDir, filepath.Join(pe.home, ".wardyn")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	writeExec(t, pe.binDir, "aws", "#!/bin/sh\necho \"aws $*\" >> \"$AWS_LOG\"\nexit 0\n")
	writeExec(t, pe.binDir, "wardyn-aws-sso", "#!/bin/sh\nexit ${FAKE_UPLOAD_RC:-0}\n")
	writeExec(t, pe.binDir, "tmux", `#!/bin/sh
{ printf 'argv:'; for a in "$@"; do printf ' %s' "$a"; done; printf '\n'; } >> "$TMUX_LOG"
exit 0
`)
	return pe
}

// runnableSigninPane copies the REAL signin-pane.sh and points its one absolute
// source path at the REAL login-hint.sh — the same remap runnableAgentRun does,
// and for the same reason: /usr/local/lib does not exist on a test host. Nothing
// is stubbed, and the strings under test come from the file the image installs
// rather than from anything the test put in the environment.
func runnableSigninPane(t *testing.T) string {
	t.Helper()
	src, err := os.ReadFile(awsSSOSigninPane) //nolint:gosec // fixed in-repo path
	if err != nil {
		t.Fatalf("read signin-pane.sh: %v", err)
	}
	hint, err := filepath.Abs(awsSSOLoginHintPath)
	if err != nil {
		t.Fatalf("resolve login-hint.sh: %v", err)
	}
	const installed = "/usr/local/lib/wardyn-attach-hint.sh"
	body := string(src)
	if !strings.Contains(body, installed) {
		t.Fatalf("signin-pane.sh no longer sources %s, so its strings are not image-baked", installed)
	}
	body = strings.ReplaceAll(body, installed, hint)
	dst := filepath.Join(t.TempDir(), "signin-pane.sh")
	if err := os.WriteFile(dst, []byte(body), 0o700); err != nil { //nolint:gosec // test fixture
		t.Fatalf("write runnable signin-pane.sh: %v", err)
	}
	return dst
}

func signinPaneCmd(t *testing.T, pe paneEnv, extraEnv ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("timeout", "20s", "bash", runnableSigninPane(t))
	cmd.Env = append(os.Environ(), append([]string{
		"HOME=" + pe.home,
		"PATH=" + pe.binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"AWS_LOG=" + pe.awsLog,
		"TMUX_LOG=" + pe.tmuxLog,
	}, extraEnv...)...)
	return cmd
}

// TestSigninPane_RunsThePairOnce — ONCE, with no re-arm loop: a retry mints a
// SECOND live device code while the human may still be entering the first, and
// `A && B` would re-run the login after a deterministic wardyn-aws-sso refusal
// (a pin contradiction) that no second login can fix.
func TestSigninPane_RunsThePairOnce(t *testing.T) {
	pe := signinPaneEnv(t)
	if err := os.WriteFile(filepath.Join(pe.home, ".wardyn", "prep-done"), nil, 0o600); err != nil {
		t.Fatalf("write prep-done: %v", err)
	}
	cmd := signinPaneCmd(t, pe, "FAKE_UPLOAD_RC=1")
	cmd.Stdin = strings.NewReader("")
	out, _ := cmd.CombinedOutput()
	if n := countLines(readLog(t, pe.awsLog), "aws sso login"); n != 1 {
		t.Errorf("`aws sso login` ran %d times, want exactly 1 — every extra run mints another live device code\naws log:\n%s\npane output:\n%s", n, readLog(t, pe.awsLog), out)
	}
	wantFailed := strings.Replace(pe.consts["WARDYN_AWS_SSO_SELFRUN_FAILED"], "%s", pe.consts["WARDYN_AWS_SSO_LOGIN_COMMAND"], 1)
	if !strings.Contains(string(out), wantFailed) {
		t.Errorf("a refused upload did not print the FAILED line naming the command to re-run\nwant: %s\noutput:\n%s", wantFailed, out)
	}
	if strings.Contains(string(out), pe.consts["WARDYN_AWS_SSO_SELFRUN_DONE"]) {
		t.Errorf("the pane claimed the sign-in finished after the uploader refused it\noutput:\n%s", out)
	}
	// The session-environment mark: a tmux WINDOW opened later inherits the
	// SESSION's environment, not this process's, so without this line the attach
	// hint tells that window the sign-in never started.
	if n := countLines(readLog(t, pe.tmuxLog), "argv: set-environment -t wardyn WARDYN_AWS_SSO_SELFRAN 1"); n != 1 {
		t.Errorf("tmux got %d `set-environment -t wardyn WARDYN_AWS_SSO_SELFRAN 1` calls, want exactly 1\ntmux log:\n%s", n, readLog(t, pe.tmuxLog))
	}
}

// TestSigninPane_HostileInheritedCommandIsIgnored — the pane `bash -c`s the
// chained command, so WHERE that string comes from is the whole question. It is
// re-read from the image's own login-hint.sh on every start, so an inherited
// value (the respawn path's tmux server inherits the ATTACH exec's environment,
// not agent-run's) can never be what runs.
func TestSigninPane_HostileInheritedCommandIsIgnored(t *testing.T) {
	pe := signinPaneEnv(t)
	if err := os.WriteFile(filepath.Join(pe.home, ".wardyn", "prep-done"), nil, 0o600); err != nil {
		t.Fatalf("write prep-done: %v", err)
	}
	cmd := signinPaneCmd(t, pe,
		"WARDYN_AWS_SSO_LOGIN_COMMAND=echo PWNED-BY-INHERITED-COMMAND",
		"WARDYN_AWS_SSO_SELFRUN_BANNER=PWNED-BANNER")
	cmd.Stdin = strings.NewReader("")
	out, _ := cmd.CombinedOutput()
	if strings.Contains(string(out), "PWNED") {
		t.Errorf("the pane ran a command (or printed a banner) it inherited from its environment instead of the image's own\noutput:\n%s", out)
	}
	if n := countLines(readLog(t, pe.awsLog), "aws sso login"); n != 1 {
		t.Errorf("the image's own pair did not run (`aws sso login` ran %d times, want 1)\naws log:\n%s\noutput:\n%s", n, readLog(t, pe.awsLog), out)
	}
	if !strings.Contains(string(out), pe.consts["WARDYN_AWS_SSO_SELFRUN_BANNER"]) {
		t.Errorf("the image's own banner was not printed\noutput:\n%s", out)
	}
}

// TestSigninPane_BannerPrecedesThePrepWait — the banner is the FIRST act, before
// the prep wait. The console gives an old image a grace window and then types
// the pair itself, and the ONLY thing that stops it is seeing this marker; prep
// is a measured ~18s, so a banner printed after the wait arrives too late and
// the pane types a SECOND device code into a pane that is still waiting.
func TestSigninPane_BannerPrecedesThePrepWait(t *testing.T) {
	pe := signinPaneEnv(t)
	outPath := filepath.Join(t.TempDir(), "pane.out")
	f, err := os.Create(outPath) //nolint:gosec // test-owned temp path
	if err != nil {
		t.Fatalf("create pane out: %v", err)
	}
	defer f.Close()
	cmd := signinPaneCmd(t, pe)
	cmd.Stdin = strings.NewReader("")
	cmd.Stdout, cmd.Stderr = f, f
	if err := cmd.Start(); err != nil {
		t.Fatalf("start signin-pane.sh: %v", err)
	}
	prepDone := filepath.Join(pe.home, ".wardyn", "prep-done")
	banner := pe.consts["WARDYN_AWS_SSO_SELFRUN_BANNER"]
	var sawBanner bool
	for i := 0; i < 40 && !sawBanner; i++ { // up to ~4s, well inside the console's grace window
		time.Sleep(100 * time.Millisecond)
		sawBanner = strings.Contains(readLog(t, outPath), banner)
	}
	if _, err := os.Stat(prepDone); err == nil {
		t.Fatal("prep-done appeared on its own — this test proved nothing about the ordering")
	}
	if !sawBanner {
		t.Errorf("the pane printed no banner while it waited for prep-done; the console would type the pair over a waiting pane\noutput:\n%s", readLog(t, outPath))
	}
	// …AND the session is already marked self-run, while the login is still in
	// flight. tmux's prefix is intact, so a human can open a second WINDOW right
	// now; it inherits the SESSION environment, and the attach hint it sources
	// must not tell them the sign-in never started.
	if n := countLines(readLog(t, pe.tmuxLog), "argv: set-environment -t wardyn WARDYN_AWS_SSO_SELFRAN 1"); n != 1 {
		t.Errorf("the session was not marked self-run before the login finished (%d set-environment calls) — a window opened mid-login would read the false \"did not start on its own\" line\ntmux log:\n%s", n, readLog(t, pe.tmuxLog))
	}
	// And that mark, fed to a fresh interactive shell exactly as tmux would hand
	// it to a new window, silences the line.
	hint, err := filepath.Abs(awsSSOLoginHintPath)
	if err != nil {
		t.Fatalf("resolve login-hint.sh: %v", err)
	}
	newWindow := exec.Command("bash", "-i", "-c", ". "+hint)
	newWindow.Env = append(os.Environ(), "HOME="+t.TempDir(), "WARDYN_AWS_SSO_SELFRAN=1")
	windowOut, _ := newWindow.CombinedOutput()
	if strings.Contains(string(windowOut), "did not start on its own") {
		t.Errorf("a window opened while the login is in flight is told the sign-in never started\noutput:\n%s", windowOut)
	}
	if err := os.WriteFile(prepDone, nil, 0o600); err != nil {
		t.Fatalf("write prep-done: %v", err)
	}
	_ = cmd.Wait()
}

// TestSigninPane_KeysTypedDuringTheWaitAreDiscarded — tmux BUFFERS keys sent to
// a pane whose script is still running, and the trailing `exec bash` would run
// them. A console on an older version that typed the pair anyway must not get a
// second sign-in out of it.
func TestSigninPane_KeysTypedDuringTheWaitAreDiscarded(t *testing.T) {
	pe := signinPaneEnv(t)
	cmd := signinPaneCmd(t, pe)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	outPath := filepath.Join(t.TempDir(), "pane.out")
	f, err := os.Create(outPath) //nolint:gosec // test-owned temp path
	if err != nil {
		t.Fatalf("create pane out: %v", err)
	}
	defer f.Close()
	cmd.Stdout, cmd.Stderr = f, f
	if err := cmd.Start(); err != nil {
		t.Fatalf("start signin-pane.sh: %v", err)
	}
	// Typed while the pane is still waiting for prep-done.
	if _, err := stdin.Write([]byte("echo PWNED-BY-BUFFERED-KEYS\n")); err != nil {
		t.Fatalf("write to the waiting pane: %v", err)
	}
	if err := stdin.Close(); err != nil {
		t.Fatalf("close pane stdin: %v", err)
	}
	time.Sleep(1200 * time.Millisecond)
	if err := os.WriteFile(filepath.Join(pe.home, ".wardyn", "prep-done"), nil, 0o600); err != nil {
		t.Fatalf("write prep-done: %v", err)
	}
	_ = cmd.Wait()
	got := readLog(t, outPath)
	if strings.Contains(got, "PWNED-BY-BUFFERED-KEYS") {
		t.Errorf("bytes typed at the waiting pane were executed by the shell it hands over to\noutput:\n%s", got)
	}
	if !strings.Contains(got, pe.consts["WARDYN_AWS_SSO_SELFRUN_DONE"]) {
		t.Errorf("the pane did not report a finished sign-in, so the drain assertion above proved nothing\noutput:\n%s", got)
	}
}
