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

// The managed-file case's fixtures. Two files in ONE directory:
//
//   - managedReadablePath is THE CONSUMER'S OWN PATH — where Claude Code reads
//     its managed settings — at the 0644 the product ships: readable by the
//     agent, and unwritable because it is root-owned and the agent is uid 1000
//     (image contract §3). The case runs on the real location, not a benign
//     stand-in, so anything a substrate does to that directory shows up here.
//   - managedLockedPath at 0444 is the shape that proves a refusal ON ANY
//     IMAGE, including one whose sandbox identity is root: a sandbox has
//     CapDrop ALL, so it holds no CAP_DAC_OVERRIDE and a 0444 file refuses
//     even in-container root's write. The owner can still chmod it without
//     any capability, which is why the chmod probe below needs uid 1000.
const (
	managedReadablePath = "/etc/claude-code/managed-settings.json"
	managedLockedPath   = runner.ManagedFileDir + "/locked.txt"
	// No trailing newline on purpose: `cat` plus a byte count is what proves
	// the substrate delivered the content EXACTLY rather than approximately.
	managedReadableBody = `{"wardyn":"managed-file-ok","n":1}`
	managedLockedBody   = "wardyn-managed-locked"
)

func managedFilesFixture() []runner.ManagedFile {
	return []runner.ManagedFile{
		{Path: managedReadablePath, Mode: 0o644, Content: []byte(managedReadableBody)},
		{Path: managedLockedPath, Mode: 0o444, Content: []byte(managedLockedBody)},
	}
}

// managedFileProbeScript reports everything the case needs in one exec, as
// key=value lines. One exec rather than ten: on a substrate that wraps every
// exec with the recorder (kubernetes), ten execs are ten recordings and ten
// chances for a transport hiccup to look like a contract failure.
//
// The file checks stat with -L: a Kubernetes Secret volume projects each item
// as a symlink into its timestamped data directory, and the symlink's own
// 0777 says nothing about the file the agent reads.
//
// Each attempt runs in a subshell with stderr discarded so the shell's own
// diagnostic never lands in the parsed stream; the WORD is the verdict. The
// two replacement routes come last, because either one that succeeds changes
// what the probes before it would see:
//
//   - w_chmod: the owner of a file may chmod it without a capability, so a
//     read-only mode alone is no ceiling against an agent that owns the file.
//   - w_rename: a rename within one parent needs write on the parent only, so
//     an agent that can write the directory's parent can move the directory
//     aside and put its own in its place.
var managedFileProbeScript = fmt.Sprintf(`echo uid=$(id -u)
printf 'body='; cat %[1]s; echo
echo bytes=$(wc -c < %[1]s)
echo own_readable=$(stat -L -c '%%u:%%g:%%a' %[1]s)
echo own_locked=$(stat -L -c '%%u:%%g:%%a' %[2]s)
echo own_dir=$(stat -c '%%u:%%g' %[3]s)
if (echo x > %[1]s) 2>/dev/null; then echo w_readable=WROTE; else echo w_readable=REFUSED; fi
if (echo x > %[2]s) 2>/dev/null; then echo w_locked=WROTE; else echo w_locked=REFUSED; fi
if (: > %[3]s/wardyn-intruder) 2>/dev/null; then echo w_dir=WROTE; else echo w_dir=REFUSED; fi
if (chmod 0644 %[2]s && echo EVIL > %[2]s) 2>/dev/null; then echo w_chmod=WROTE; else echo w_chmod=REFUSED; fi
if (mv %[3]s %[3]s.aside && mkdir %[3]s && echo EVIL > %[1]s) 2>/dev/null; then echo w_rename=WROTE; else echo w_rename=REFUSED; fi
`, managedReadablePath, managedLockedPath, runner.ManagedFileDir)

// testManagedFiles is conformance case 8: a driver that advertises
// Capabilities.ManagedFiles must place an operator-authored file in the
// sandbox that the AGENT CANNOT MODIFY.
//
// THE REFUSAL IS THE CASE. Reading the file back only proves a file arrived,
// and a driver that materialised it as the sandbox user would pass that half
// while delivering nothing of value — the bug this case exists to catch is
// invisible to a read. So the verdict rests on assertions a mis-delivery
// fails: the file is owned by ROOT at the mode that was asked for, its
// DIRECTORY is owned by root, and every route to a different file at that path
// — a write, a new entry in the directory, chmod-then-write, and renaming the
// directory aside — is REFUSED to the agent's identity.
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
		if opts.AgentUserImage != "" {
			spec.Image = opts.AgentUserImage
		}
		spec.ManagedFiles = managedFilesFixture()
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
	for _, k := range []string{"uid", "body", "bytes", "own_readable", "own_locked", "own_dir", "w_readable", "w_locked", "w_dir", "w_chmod", "w_rename"} {
		if _, ok := got[k]; !ok {
			t.Fatalf("managed-file probe produced no %q line; the image must provide sh, cat, wc, stat, id, chmod, mv and mkdir.\nprobe output:\n%s", k, out)
		}
	}
	if opts.AgentUserImage != "" && got["uid"] != "1000" {
		t.Fatalf("the probe ran as uid %s on AgentUserImage %q, want 1000 — as any other identity its refusals are not the agent's", got["uid"], opts.AgentUserImage)
	}
	t.Logf("probe as uid %s: w_readable=%s w_locked=%s w_dir=%s w_chmod=%s w_rename=%s", got["uid"], got["w_readable"], got["w_locked"], got["w_dir"], got["w_chmod"], got["w_rename"])

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
		t.Errorf("stat %s = %s, want 0:0 — an agent that owns the directory can unlink the file and put its own there, whatever the file's mode says", runner.ManagedFileDir, got["own_dir"])
	}

	// ── 3. every route to a different file is REFUSED ──────────────────────
	// The 0444 write, unconditionally: a sandbox has CapDrop ALL, so this
	// refusal holds even when the image's sandbox identity is root.
	if got["w_locked"] != "REFUSED" {
		t.Errorf("`echo x > %s` = %s, want REFUSED — the agent rewrote a managed file, so it is not a ceiling", managedLockedPath, got["w_locked"])
	}
	// The rest, for the identity the product actually runs: every Wardyn agent
	// image is uid 1000. Root owns the file and /etc, so as root each of these
	// succeeds by definition.
	if got["uid"] == "0" {
		t.Logf("this image's sandbox identity is root (uid 0), so the write, directory, chmod and rename checks are recorded above, not asserted; set Options.AgentUserImage to assert them")
		return
	}
	for _, c := range []struct{ key, what string }{
		{"w_readable", "`echo x > " + managedReadablePath + "`"},
		{"w_dir", "creating a file in " + runner.ManagedFileDir + " (the agent could unlink the managed file and put its own there)"},
		{"w_chmod", "`chmod 0644 " + managedLockedPath + " && echo EVIL > " + managedLockedPath + "` (a read-only mode is no ceiling against its owner)"},
		{"w_rename", "`mv " + runner.ManagedFileDir + " " + runner.ManagedFileDir + ".aside && mkdir " + runner.ManagedFileDir + "` then writing " + managedReadablePath + " (the agent could replace the whole directory)"},
	} {
		if got[c.key] != "REFUSED" {
			t.Errorf("%s as uid %s = %s, want REFUSED", c.what, got["uid"], got[c.key])
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
