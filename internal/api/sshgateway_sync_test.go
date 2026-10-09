// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func TestSSHSyncDir(t *testing.T) {
	for in, want := range map[string]string{
		"":                           "/home/agent/work",
		"/home/agent/work/proj":      "/home/agent/work/proj",
		"/home/agent//work/./proj/":  "/home/agent/work/proj",
		"/home/agent/a b/ünï":        "/home/agent/a b/ünï",
		"/home/agent/work/..hidden":  "/home/agent/work/..hidden",
		"/home/agent/work/proj/....": "/home/agent/work/proj/....",
	} {
		got, err := sshSyncDir(in)
		if err != nil || got != want {
			t.Errorf("sshSyncDir(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{
		"work", "./work", "/", "/etc", "/home/agent", "/home/agent/", "/home/agentx/work", "/home/other/work",
		"/home/agent/../../etc", "/home/agent/work/../../..", "/home/agent/work/..", "/home/agent/../agent/work",
		"/home/agent/work/%d", "/home/agent/w\x00ork", "/home/agent/w\nork", "/home/agent/w\x7fork",
	} {
		if got, err := sshSyncDir(in); err == nil {
			t.Errorf("sshSyncDir(%q) = %q, want refused", in, got)
		}
	}
}

func TestSSHSyncDirection(t *testing.T) {
	for in, want := range map[string]string{"push": "push", "pull": "pull", "": "unknown", "PUSH": "unknown", "push;rm": "unknown"} {
		if got := sshSyncDirection(in); got != want {
			t.Errorf("sshSyncDirection(%q) = %q, want %q", in, got, want)
		}
	}
}

type sshSyncFixture struct {
	h      *sshTestHarness
	fr     *sshFakeRunner
	client *ssh.Client
	run    types.AgentRun
}

func newSSHSyncFixture(t *testing.T, configure ...func(*Config)) *sshSyncFixture {
	t.Helper()
	st, run, principal := sshOwnedRunningRun(t)
	priv, pub := mustSSHKeypair(t)
	st.putKey(types.SSHPublicKey{Fingerprint: ssh.FingerprintSHA256(pub), Principal: principal, PublicKey: string(ssh.MarshalAuthorizedKey(pub))})
	fr := &sshFakeRunner{
		execFn:   func(runner.ExecSpec) (*runner.ExecSession, error) { return fakeEchoExecSession(), nil },
		attachFn: func() (runner.Session, error) { return newFakeShellSession(), nil },
	}
	h := newSSHTestHarness(t, st, fr, configure...)
	client, err := sshDial(t, h, run.ID.String(), priv)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return &sshSyncFixture{h: h, fr: fr, client: client, run: run}
}

// refused is sshSessionRefused, then waits for the probe's own slot to be given
// back so it cannot be mistaken for a held one.
func (f *sshSyncFixture) refused(t *testing.T) bool {
	t.Helper()
	held := func() int {
		f.h.srv.sshSessionsMu.Lock()
		defer f.h.srv.sshSessionsMu.Unlock()
		return f.h.srv.sshSessions[f.run.ID] + f.h.srv.sshSyncSessions[f.run.ID]
	}
	before := held()
	got := sshSessionRefused(t, f.client)
	for i := 0; i < 200 && held() > before; i++ {
		time.Sleep(5 * time.Millisecond)
	}
	return got
}

// holdShell opens a shell that stays open for the test.
func (f *sshSyncFixture) holdShell(t *testing.T) {
	t.Helper()
	sess, err := f.client.NewSession()
	if err != nil {
		t.Fatalf("shell channel: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	if _, err := sess.StdinPipe(); err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	if err := sess.Shell(); err != nil {
		t.Fatalf("shell: %v", err)
	}
}

// openSync opens a session, offers env, and requests wardyn-sync. The session
// stays open (its stdin pipe is returned) until the caller closes it.
func (f *sshSyncFixture) openSync(t *testing.T, env map[string]string) (*ssh.Session, io.WriteCloser, io.Reader, error) {
	t.Helper()
	sess, err := f.client.NewSession()
	if err != nil {
		return nil, nil, nil, err
	}
	t.Cleanup(func() { _ = sess.Close() })
	for k, v := range env {
		if err := sess.Setenv(k, v); err != nil {
			t.Fatalf("setenv %s: %v", k, err)
		}
	}
	in, err := sess.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	out, err := sess.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	return sess, in, out, sess.RequestSubsystem(sshSyncSubsystem)
}

func syncEcho(t *testing.T, in io.WriteCloser, out io.Reader, payload string) {
	t.Helper()
	if _, err := io.WriteString(in, payload); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, len(payload))
	if _, err := io.ReadFull(out, buf); err != nil || string(buf) != payload {
		t.Fatalf("echo = %q, %v; want %q", buf, err, payload)
	}
}

func sshAuditData(t *testing.T, ev *types.AuditEvent) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(ev.Data, &m); err != nil {
		t.Fatalf("audit data %s: %v", ev.Data, err)
	}
	return m
}

func TestSSHGateway_SyncSubsystemRunsSFTPInTheValidatedDir(t *testing.T) {
	f := newSSHSyncFixture(t)
	_, in, out, err := f.openSync(t, map[string]string{
		"WARDYN_SYNC_DIR": "/home/agent/work//proj/", "WARDYN_SYNC_DIRECTION": "push", "TERM": "xterm",
	})
	if err != nil {
		t.Fatalf("wardyn-sync refused: %v", err)
	}
	syncEcho(t, in, out, "twelve-bytes")
	argv, env := f.fr.lastCall()
	if want := []string{sftpServerPath, "-e", "-d", "/home/agent/work/proj"}; !slices.Equal(argv, want) {
		t.Errorf("argv = %v, want %v", argv, want)
	}
	if len(env) != 0 {
		t.Errorf("sync exec env = %v, want none: the sync names must not travel into the sandbox", env)
	}
	_ = in.Close()

	ev := waitForAudit(t, f.h.audit, f.run.ID, "ssh.sync.transfer", "success")
	if ev == nil {
		t.Fatalf("no ssh.sync.transfer; events=%s", auditDump(f.h.audit.snapshot(), f.run.ID))
	}
	d := sshAuditData(t, ev)
	if d["dir"] != "/home/agent/work/proj" || d["direction"] != "push" || d["bytes_in"] != float64(12) || d["bytes_out"] != float64(12) {
		t.Errorf("ssh.sync.transfer data = %s", ev.Data)
	}
}

func TestSSHGateway_SyncDefaultsAndRefusals(t *testing.T) {
	t.Run("no env is the default workspace and direction unknown", func(t *testing.T) {
		f := newSSHSyncFixture(t)
		_, in, out, err := f.openSync(t, nil)
		if err != nil {
			t.Fatalf("wardyn-sync refused: %v", err)
		}
		syncEcho(t, in, out, "x")
		argv, _ := f.fr.lastCall()
		if want := []string{sftpServerPath, "-e", "-d", "/home/agent/work"}; !slices.Equal(argv, want) {
			t.Errorf("argv = %v, want %v", argv, want)
		}
		_ = in.Close()
		ev := waitForAudit(t, f.h.audit, f.run.ID, "ssh.sync.transfer", "success")
		if ev == nil || sshAuditData(t, ev)["direction"] != "unknown" {
			t.Errorf("want a success row with direction unknown; events=%s", auditDump(f.h.audit.snapshot(), f.run.ID))
		}
	})
	for _, dir := range []string{"/etc", "/home/agent/../../etc", "relative", "/home/agent/work/%d"} {
		t.Run("refuses "+dir, func(t *testing.T) {
			f := newSSHSyncFixture(t)
			sess, in, _, err := f.openSync(t, map[string]string{"WARDYN_SYNC_DIR": dir, "WARDYN_SYNC_DIRECTION": "pull"})
			if err != nil {
				t.Fatalf("the request is accepted and the channel then ends with the reason: %v", err)
			}
			_ = in.Close()
			if err := sess.Wait(); err == nil {
				t.Error("session ended 0, want a nonzero exit")
			}
			if argv, _ := f.fr.lastCall(); argv != nil {
				t.Errorf("sftp-server was launched with %v for a refused directory", argv)
			}
			ev := waitForAudit(t, f.h.audit, f.run.ID, "ssh.sync.transfer", "failure")
			if ev == nil || !strings.Contains(string(ev.Data), "WARDYN_SYNC_DIR") || sshAuditData(t, ev)["direction"] != "pull" {
				t.Errorf("want a failure row naming WARDYN_SYNC_DIR; events=%s", auditDump(f.h.audit.snapshot(), f.run.ID))
			}
		})
	}
}

// The sync names are honoured only on a channel that then requests wardyn-sync.
func TestSSHGateway_SyncEnvIgnoredOnOtherChannels(t *testing.T) {
	f := newSSHSyncFixture(t)
	offer := func(sess *ssh.Session) {
		_ = sess.Setenv("WARDYN_SYNC_DIR", "/home/agent/work/proj")
		_ = sess.Setenv("WARDYN_SYNC_DIRECTION", "push")
		_ = sess.Setenv("LANG", "C")
	}

	t.Run("exec", func(t *testing.T) {
		sess, err := f.client.NewSession()
		if err != nil {
			t.Fatal(err)
		}
		defer sess.Close()
		offer(sess)
		if err := sess.Run("true"); err != nil {
			t.Fatalf("run: %v", err)
		}
		_, env := f.fr.lastCall()
		if containsPrefix(env, "WARDYN_SYNC_") || !containsEnv(env, "LANG=C") {
			t.Errorf("exec env = %v, want LANG only", env)
		}
	})

	t.Run("plain sftp", func(t *testing.T) {
		sess, err := f.client.NewSession()
		if err != nil {
			t.Fatal(err)
		}
		defer sess.Close()
		offer(sess)
		in, _ := sess.StdinPipe()
		out, _ := sess.StdoutPipe()
		if err := sess.RequestSubsystem("sftp"); err != nil {
			t.Fatal(err)
		}
		syncEcho(t, in, out, "x")
		argv, env := f.fr.lastCall()
		if want := []string{sftpServerPath, "-e"}; !slices.Equal(argv, want) || containsPrefix(env, "WARDYN_SYNC_") {
			t.Errorf("sftp argv = %v env = %v; the sync directory must not reach a plain sftp", argv, env)
		}
		_ = in.Close()
		ev := waitForAudit(t, f.h.audit, f.run.ID, "ssh.sftp.transfer", "success")
		if ev == nil {
			t.Fatalf("no ssh.sftp.transfer; events=%s", auditDump(f.h.audit.snapshot(), f.run.ID))
		}
		if strings.Contains(string(ev.Data), `"dir"`) {
			t.Errorf("sftp row %s must not carry sync fields", ev.Data)
		}
	})

	t.Run("shell", func(t *testing.T) {
		sess, err := f.client.NewSession()
		if err != nil {
			t.Fatal(err)
		}
		defer sess.Close()
		offer(sess)
		_, _ = sess.StdinPipe()
		if err := sess.Shell(); err != nil {
			t.Fatalf("shell: %v", err)
		}
	})
}

func TestSSHGateway_SyncHasItsOwnBudget(t *testing.T) {
	t.Run("shells at the cap do not starve a sync", func(t *testing.T) {
		f := newSSHSyncFixture(t)
		for i := 0; i < defaultSSHSessionsPerRun; i++ {
			f.holdShell(t)
		}
		if !f.refused(t) {
			t.Fatal("a shell past the shared cap was accepted")
		}
		for i := 0; i < maxSSHSyncSessionsPerRun; i++ {
			_, in, out, err := f.openSync(t, nil)
			if err != nil {
				t.Fatalf("sync %d refused while only shells hold the shared cap: %v", i, err)
			}
			syncEcho(t, in, out, "x")
		}
		if _, _, _, err := f.openSync(t, nil); err == nil {
			t.Error("a third sync was accepted past its cap")
		}
	})

	t.Run("syncs at the cap do not starve a shell", func(t *testing.T) {
		f := newSSHSyncFixture(t)
		for i := 0; i < maxSSHSyncSessionsPerRun; i++ {
			_, in, out, err := f.openSync(t, nil)
			if err != nil {
				t.Fatalf("sync %d: %v", i, err)
			}
			syncEcho(t, in, out, "x")
		}
		for i := 0; i < defaultSSHSessionsPerRun; i++ {
			f.holdShell(t)
		}
		if !f.refused(t) {
			t.Error("a shell past the shared cap was accepted: syncs must not widen it")
		}
	})

	t.Run("the third sync is refused and audited as a sync cap hit", func(t *testing.T) {
		f := newSSHSyncFixture(t)
		for i := 0; i < maxSSHSyncSessionsPerRun; i++ {
			_, in, out, err := f.openSync(t, nil)
			if err != nil {
				t.Fatalf("sync %d: %v", i, err)
			}
			syncEcho(t, in, out, "x")
		}
		if _, _, _, err := f.openSync(t, nil); err == nil {
			t.Fatal("a third sync was accepted")
		}
		ev := waitForAudit(t, f.h.audit, f.run.ID, "ssh.channel.reject", "failure")
		d := sshAuditData(t, ev)
		if d["reason"] != sshCapReasonSync || d["max"] != float64(maxSSHSyncSessionsPerRun) {
			t.Errorf("reject row = %s, want the sync cap", ev.Data)
		}
	})

	t.Run("a sync gives its shared slot back", func(t *testing.T) {
		f := newSSHSyncFixture(t, func(c *Config) { c.SSHMaxSessionsPerRun = 1 })
		_, in, out, err := f.openSync(t, nil)
		if err != nil {
			t.Fatal(err)
		}
		syncEcho(t, in, out, "x")
		f.holdShell(t)
	})

	t.Run("a sync-only channel may not become a shell", func(t *testing.T) {
		f := newSSHSyncFixture(t, func(c *Config) { c.SSHMaxSessionsPerRun = 1 })
		f.holdShell(t)
		sess, err := f.client.NewSession()
		if err != nil {
			t.Fatalf("admitted on a sync slot: %v", err)
		}
		defer sess.Close()
		if err := sess.Run("true"); err == nil {
			t.Error("exec ran on a channel admitted only for wardyn-sync")
		}
		if err := sess.RequestSubsystem("sftp"); err == nil {
			t.Error("plain sftp ran on a channel admitted only for wardyn-sync")
		}
	})
}

func TestSSHGateway_ConfiguredChannelCap(t *testing.T) {
	f := newSSHSyncFixture(t, func(c *Config) { c.SSHMaxSessionsPerRun = 2 })
	f.holdShell(t)
	f.holdShell(t)
	if !f.refused(t) {
		t.Fatal("a third shell was accepted with the cap at 2")
	}
	// Fill the two sync slots too, so the next channel is refused at open.
	for i := 0; i < maxSSHSyncSessionsPerRun; i++ {
		_, in, out, err := f.openSync(t, nil)
		if err != nil {
			t.Fatal(err)
		}
		syncEcho(t, in, out, "x")
	}
	_, err := f.client.NewSession()
	if err == nil || !strings.Contains(err.Error(), "(max 2)") {
		t.Fatalf("open past both caps = %v, want a refusal naming (max 2)", err)
	}
	var max float64
	for _, e := range f.h.audit.snapshot() {
		if e.Action == "ssh.channel.reject" {
			max = sshAuditData(t, &e)["max"].(float64)
		}
	}
	if max != 2 {
		t.Errorf("ssh.channel.reject max = %v, want the configured 2", max)
	}
}

func TestSSHGateway_SFTPTransferAuditCountsBothWays(t *testing.T) {
	st, run, principal := sshOwnedRunningRun(t)
	priv, pub := mustSSHKeypair(t)
	st.putKey(types.SSHPublicKey{Fingerprint: ssh.FingerprintSHA256(pub), Principal: principal, PublicKey: string(ssh.MarshalAuthorizedKey(pub))})
	// Takes 5 bytes in, sends 9 back: bytes (the pre-0.9 field) must stay the
	// sandbox-to-client count, not the sum or the upload.
	fr := &sshFakeRunner{execFn: func(runner.ExecSpec) (*runner.ExecSession, error) {
		stdinR, stdinW := io.Pipe()
		stdoutR, stdoutW := io.Pipe()
		go func() {
			_, _ = io.Copy(io.Discard, stdinR)
			_, _ = io.WriteString(stdoutW, "123456789")
			_ = stdoutW.Close()
		}()
		return &runner.ExecSession{Stdin: stdinW, Stdout: stdoutR, Wait: func() (int, error) { return 0, nil }}, nil
	}}
	h := newSSHTestHarness(t, st, fr)
	client, err := sshDial(t, h, run.ID.String(), priv)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()
	sess, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	in, _ := sess.StdinPipe()
	out, _ := sess.StdoutPipe()
	if err := sess.RequestSubsystem("sftp"); err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(in, "hello")
	_ = in.Close()
	if got, _ := io.ReadAll(out); string(got) != "123456789" {
		t.Fatalf("out = %q", got)
	}
	ev := waitForAudit(t, h.audit, run.ID, "ssh.sftp.transfer", "success")
	if ev == nil {
		t.Fatalf("no ssh.sftp.transfer; events=%s", auditDump(h.audit.snapshot(), run.ID))
	}
	d := sshAuditData(t, ev)
	if d["bytes"] != float64(9) || d["bytes_in"] != float64(5) || d["bytes_out"] != float64(9) {
		t.Errorf("ssh.sftp.transfer data = %s, want bytes 9 (unchanged), bytes_in 5, bytes_out 9", ev.Data)
	}
}
