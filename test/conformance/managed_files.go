// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package conformance

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// The managed-file case's fixtures. Two files in ONE directory, because the
// contract has two halves and no single mode proves both on every image:
//
//   - managedReadablePath at 0644 is the SHAPE THE PRODUCT SHIPS: an operator
//     ceiling the agent is meant to READ and unable to write, because it is
//     root-owned and the agent is uid 1000 (image contract §3).
//   - managedLockedPath at 0444 is the shape that proves the refusal ON ANY
//     IMAGE, including one whose sandbox identity is root. A sandbox has
//     CapDrop ALL, so it holds no CAP_DAC_OVERRIDE: a 0444 file refuses even
//     in-container root's write, where a root-owned 0644 file would not.
//
// Without the second file this case would be vacuous against a root-running
// conformance image — it would read the file back, print a green line and
// prove nothing about immutability, which is the only property the field
// exists for.
const (
	managedFileDir      = "/etc/wardyn/conformance"
	managedReadablePath = managedFileDir + "/managed.json"
	managedLockedPath   = managedFileDir + "/locked.txt"
	// No trailing newline on purpose: `cat` plus a byte count is what proves
	// the substrate delivered the content EXACTLY rather than approximately.
	managedReadableBody = `{"wardyn":"managed-file-ok","n":1}`
	managedLockedBody   = "wardyn-managed-locked"
)

// managedFileProbeScript reports everything the case needs in one exec, as
// key=value lines. One exec rather than eight: on a substrate that wraps every
// exec with the recorder (kubernetes), eight execs are eight recordings and
// eight chances for a transport hiccup to look like a contract failure.
//
// Each write attempt runs in a subshell with stderr discarded so the shell's
// own diagnostic never lands in the parsed stream; the WORD is the verdict.
var managedFileProbeScript = fmt.Sprintf(`echo uid=$(id -u)
printf 'body='; cat %[1]s; echo
echo bytes=$(wc -c < %[1]s)
echo own_readable=$(stat -c '%%u:%%g:%%a' %[1]s)
echo own_locked=$(stat -c '%%u:%%g:%%a' %[2]s)
echo own_dir=$(stat -c '%%u:%%g' %[3]s)
if (echo x > %[1]s) 2>/dev/null; then echo w_readable=WROTE; else echo w_readable=REFUSED; fi
if (echo x > %[2]s) 2>/dev/null; then echo w_locked=WROTE; else echo w_locked=REFUSED; fi
if (: > %[3]s/wardyn-intruder) 2>/dev/null; then echo w_dir=WROTE; else echo w_dir=REFUSED; fi
`, managedReadablePath, managedLockedPath, managedFileDir)

// testManagedFiles is conformance case 8: a driver that advertises
// Capabilities.ManagedFiles must place an operator-authored file in the
// sandbox that the AGENT CANNOT MODIFY.
//
// THE REFUSAL IS THE CASE. Reading the file back only proves a file arrived,
// and a driver that materialised it as the sandbox user would pass that half
// while delivering nothing of value — the bug this case exists to catch is
// invisible to a read. So the verdict rests on three assertions a mis-delivery
// fails: the file is owned by ROOT at the mode that was asked for, its
// DIRECTORY is owned by root (an agent that can write the parent can unlink
// the file and put its own there, which makes the mode irrelevant), and a
// write is REFUSED.
//
// Self-skipping when the driver does not advertise the capability, and when it
// declares no confinement classes (an honest stub has no sandbox to deliver
// into). Never skipping merely because the write succeeded.
func testManagedFiles(t *testing.T, r runner.Runner, opts Options) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), opts.timeout())
	defer cancel()

	caps, err := r.Capabilities(ctx)
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	if !caps.ManagedFiles {
		t.Skipf("driver %q does not advertise ManagedFiles; nothing to hold it to", r.Name())
	}
	if len(caps.ConfinementClasses) == 0 {
		t.Skipf("driver %q declares no confinement classes; managed files are not testable without a sandbox substrate", r.Name())
	}

	sb := createStrongestSandboxWith(t, ctx, r, caps, opts, "ManagedFiles", func(spec *runner.SandboxSpec) {
		spec.ManagedFiles = []runner.ManagedFile{
			{Path: managedReadablePath, Mode: 0o644, Content: []byte(managedReadableBody)},
			{Path: managedLockedPath, Mode: 0o444, Content: []byte(managedLockedBody)},
		}
	})

	sess, err := r.ExecStream(ctx, sb.Ref, runner.ExecSpec{Argv: []string{"sh", "-c", managedFileProbeScript}})
	if errors.Is(err, runner.ErrExecStreamUnsupported) {
		t.Skipf("ExecStream not implemented (driver %q): %v", r.Name(), err)
	}
	if err != nil {
		t.Fatalf("ExecStream: %v", err)
	}
	stderrDone := make(chan struct{})
	go func() {
		defer close(stderrDone)
		_, _ = io.Copy(io.Discard, sess.Stderr)
	}()
	if sess.Stdin != nil {
		// Half-close: the probe reads no stdin, but a substrate that waits for
		// EOF before flushing must not hang the case.
		_ = sess.Stdin.Close()
	}
	out, _ := io.ReadAll(sess.Stdout)
	<-stderrDone
	if _, err := sess.Wait(); err != nil {
		t.Fatalf("Wait on the managed-file probe: %v", err)
	}

	got := parseKeyValueLines(string(out))
	for _, k := range []string{"uid", "body", "bytes", "own_readable", "own_locked", "own_dir", "w_readable", "w_locked", "w_dir"} {
		if _, ok := got[k]; !ok {
			t.Fatalf("managed-file probe produced no %q line; the image must provide sh, cat, wc, stat and id.\nprobe output:\n%s", k, out)
		}
	}

	// ── 1. the content arrived EXACTLY ─────────────────────────────────────
	if got["body"] != managedReadableBody {
		t.Errorf("cat %s = %q, want %q", managedReadablePath, got["body"], managedReadableBody)
	}
	if want := strconv.Itoa(len(managedReadableBody)); got["bytes"] != want {
		t.Errorf("wc -c < %s = %s, want %s — the substrate must deliver the content byte for byte, not add a trailing newline", managedReadablePath, got["bytes"], want)
	}

	// ── 2. root owns the file AND the directory holding it ─────────────────
	if got["own_readable"] != "0:0:644" {
		t.Errorf("stat %s = %s, want 0:0:644 — a managed file the agent's own uid owns is one it can rewrite", managedReadablePath, got["own_readable"])
	}
	if got["own_locked"] != "0:0:444" {
		t.Errorf("stat %s = %s, want 0:0:444", managedLockedPath, got["own_locked"])
	}
	if got["own_dir"] != "0:0" {
		t.Errorf("stat %s = %s, want 0:0 — an agent that owns the directory can unlink the file and put its own there, whatever the file's mode says", managedFileDir, got["own_dir"])
	}

	// ── 3. the write is REFUSED ────────────────────────────────────────────
	// The 0444 file, unconditionally: a sandbox has CapDrop ALL, so this
	// refusal holds even when the image's sandbox identity is root.
	if got["w_locked"] != "REFUSED" {
		t.Errorf("`echo x > %s` = %s, want REFUSED — the agent rewrote a managed file, so it is not a ceiling", managedLockedPath, got["w_locked"])
	}
	// The 0644 file and the directory, for the identity the product actually
	// runs: every Wardyn agent image is uid 1000.
	if got["uid"] == "0" {
		t.Logf("this image's sandbox identity is root (uid 0), so a root-owned 0644 file is writable by definition and the %s / %s checks are not assertions here; the unconditional refusal above rides %s, which CapDrop ALL refuses to root as well", managedReadablePath, managedFileDir, managedLockedPath)
		t.Logf("for the record, as uid 0: w_readable=%s w_dir=%s", got["w_readable"], got["w_dir"])
	} else {
		if got["w_readable"] != "REFUSED" {
			t.Errorf("`echo x > %s` as uid %s = %s, want REFUSED", managedReadablePath, got["uid"], got["w_readable"])
		}
		if got["w_dir"] != "REFUSED" {
			t.Errorf("creating a file in %s as uid %s = %s, want REFUSED — the agent can replace the managed file by unlinking it", managedFileDir, got["uid"], got["w_dir"])
		}
	}
}

// parseKeyValueLines collects the `key=value` lines out of a probe's stdout,
// ignoring anything else on the stream (a recorder banner, a shell notice).
// The LAST occurrence of a key wins, so a replayed line cannot mask a later
// real one.
func parseKeyValueLines(s string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimRight(line, "\r")
		k, v, ok := strings.Cut(line, "=")
		if !ok || k == "" || strings.ContainsAny(k, " \t") {
			continue
		}
		out[k] = v
	}
	return out
}
