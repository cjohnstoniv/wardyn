// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package conformance_test

import (
	"cmp"
	"context"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/agentpolicy"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/docker"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/test/conformance"
)

// harnessSandboxProbe runs, as the agent, what agent-run does under WARDYN_HARNESS_SANDBOX ($1):
// the sandbox probe, then writes from inside Codex's sandbox to the workspace, outside it, and to
// the repository's hooks; then the bypass flag an older agent-run would pass, and a session asking
// for full access. Each line is key=value.
const harnessSandboxProbe = `mode="$1"; cfg=()
[ "$mode" = workspace-write ] && cfg=(-c 'sandbox_workspace_write.network_access=true')
mkdir -p "$HOME/work" && cd "$HOME/work" && git init -q . || exit 9
codex sandbox -c "sandbox_mode=\"$mode\"" "${cfg[@]}" -- true >/dev/null 2>&1; echo probe=$?
codex sandbox -c "sandbox_mode=\"$mode\"" "${cfg[@]}" -- sh -c 'echo x > "$HOME/work/in.txt"; echo x > "$HOME/out.txt"; echo x > "$HOME/work/.git/hooks/pre-commit"' >/dev/null 2>&1
for f in work/in.txt out.txt work/.git/hooks/pre-commit; do [ -e "$HOME/$f" ] && echo "$f=WROTE" || echo "$f=none"; done
rm -f "$HOME/work/in.txt"
codex sandbox -c 'sandbox_mode="danger-full-access"' -- sh -c 'echo x > "$HOME/work/in.txt"' >/dev/null 2>&1
[ -e "$HOME/work/in.txt" ] && echo full_access=WROTE || echo full_access=none
CODEX_API_KEY=x timeout 30 codex exec --skip-git-repo-check --dangerously-bypass-approvals-and-sandbox hi >/tmp/bypass.out 2>&1; echo bypass=$?
grep -q 'requirements do not allow' /tmp/bypass.out && echo bypass_refused=yes || echo bypass_refused=no
CODEX_API_KEY=x timeout 30 codex exec --skip-git-repo-check -s "$mode" -c 'approval_policy="never"' "${cfg[@]}" hi >/tmp/exec.out 2>&1
grep -q 'requirements' /tmp/exec.out && echo exec_refused=yes || echo exec_refused=no
`

// TestHarnessSandbox_CodexHoldsToItsMode is the image gate for harness_inner_walls.sandbox_mode
// (#2822): the real codex-cli image, created by the Docker driver with Wardyn's hardening at the
// class under test, with the requirements dispatch delivers. Under read-only nothing inside Codex's
// sandbox writes anywhere; under workspace-write it writes the workspace and nothing else, the
// repository's hooks included; a full-access session writes nothing; the bypass flag is refused;
// agent-run's own launch flags are not. A mode the class does not start (agentpolicy.CodexSandboxStarts)
// is skipped, since the create door refuses it.
//
// Needs WARDYN_TEST_DOCKER=1, WARDYN_TEST_CODEX_IMAGE (e.g. wardyn/agent-codex-cli:local) and a daemon
// with the class's runtime registered; WARDYN_TEST_HARNESS_SANDBOX_CLASS picks the class (CC3).
func TestHarnessSandbox_CodexHoldsToItsMode(t *testing.T) {
	if os.Getenv("WARDYN_TEST_DOCKER") != "1" {
		t.Skip("WARDYN_TEST_DOCKER=1 not set; skipping the harness sandbox image gate")
	}
	image := os.Getenv("WARDYN_TEST_CODEX_IMAGE")
	if image == "" {
		t.Skip("WARDYN_TEST_CODEX_IMAGE not set; skipping the harness sandbox image gate")
	}
	class := types.ConfinementClass(cmp.Or(os.Getenv("WARDYN_TEST_HARNESS_SANDBOX_CLASS"), string(types.CC3)))
	sub, err := docker.New(docker.Config{ProxyImage: "busybox:latest", ProxyCmd: []string{"sleep", "infinity"}})
	if err != nil {
		t.Fatalf("docker.New: %v", err)
	}
	ensureConformanceNetwork(t, "wardyn-internal")

	for _, tc := range []struct{ mode, inWork string }{
		{types.InnerSandboxReadOnly, "none"},
		{types.InnerSandboxWorkspaceWrite, "WROTE"},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			if !agentpolicy.CodexSandboxStarts(tc.mode, class, false) {
				t.Skipf("%s does not start at %s; the create door refuses it", tc.mode, class)
			}
			path, content, _ := agentpolicy.CodexRequirements(tc.mode)
			got := harnessSandboxRun(t, sub, image, class, tc.mode, runner.ManagedFile{Path: path, Content: content})
			for key, want := range map[string]string{
				"probe": "0", "work/in.txt": tc.inWork, "out.txt": "none", "work/.git/hooks/pre-commit": "none",
				"full_access": "none", "bypass_refused": "yes", "exec_refused": "no",
			} {
				if got[key] != want {
					t.Errorf("%s at %s: %s = %q, want %q", tc.mode, class, key, got[key], want)
				}
			}
			if got["bypass"] == "0" {
				t.Errorf("%s at %s: codex exec with the bypass flag exited 0 under the requirements", tc.mode, class)
			}
		})
	}
}

// harnessSandboxRun creates one sandbox carrying file and returns the probe's key=value lines.
func harnessSandboxRun(t *testing.T, sub *docker.Driver, image string, class types.ConfinementClass, mode string, file runner.ManagedFile) map[string]string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	sb, err := sub.CreateSandbox(ctx, runner.SandboxSpec{
		RunID: uuid.New(), Image: image, ConfinementClass: class,
		Labels:       map[string]string{"wardyn.conformance": "true"},
		ProxyConfig:  conformance.LoadableProxyConfig(),
		ManagedFiles: []runner.ManagedFile{file},
	})
	if err != nil {
		t.Fatalf("CreateSandbox(%s, %s): %v", image, class, err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := sub.KillSandbox(ctx, sb.Ref); err != nil {
			t.Logf("KillSandbox: %v", err)
		}
	})
	sess, err := sub.ExecStream(ctx, sb.Ref, runner.ExecSpec{Argv: []string{"bash", "-c", harnessSandboxProbe, "probe", mode}})
	if err != nil {
		t.Fatalf("ExecStream: %v", err)
	}
	if sess.Stdin != nil {
		_ = sess.Stdin.Close()
	}
	go func() { _, _ = io.Copy(io.Discard, sess.Stderr) }()
	out, _ := io.ReadAll(sess.Stdout)
	if _, err := sess.Wait(); err != nil {
		t.Fatalf("Wait on the probe: %v", err)
	}
	t.Logf("%s at %s:\n%s", mode, class, out)
	got := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			got[k] = v
		}
	}
	return got
}
