// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// sftp-server is the gateway's own transport, so its nonzero exit is a failed
// transfer: a missing binary a lazy substrate reports only after launch (127,
// nothing sent) and a late exit after some bytes both record
// ssh.sftp.transfer failure with the exit, never success.
func TestSSHGateway_SFTPNonzeroExitIsAFailedTransfer(t *testing.T) {
	for _, c := range []struct {
		name, out string
		code      int
	}{
		{"missing after launch", "", 127},
		{"late exit", "partial", 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			st, run, principal := sshOwnedRunningRun(t)
			priv, pub := mustSSHKeypair(t)
			st.putKey(types.SSHPublicKey{Fingerprint: ssh.FingerprintSHA256(pub), Principal: principal, PublicKey: string(ssh.MarshalAuthorizedKey(pub))})
			fr := &sshFakeRunner{execFn: func(runner.ExecSpec) (*runner.ExecSession, error) {
				return &runner.ExecSession{Stdout: strings.NewReader(c.out), Wait: func() (int, error) { return c.code, nil }}, nil
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
			out, _ := sess.StdoutPipe()
			if err := sess.RequestSubsystem("sftp"); err != nil {
				t.Fatal(err)
			}
			_, _ = io.ReadAll(out)
			ev := waitForAudit(t, h.audit, run.ID, "ssh.sftp.transfer", "failure")
			if ev == nil {
				t.Fatalf("no failed ssh.sftp.transfer; events=%s", auditDump(h.audit.snapshot(), run.ID))
			}
			if d := sshAuditData(t, ev); d["error"] != "sftp-server exited "+strconv.Itoa(c.code) || d["bytes_out"] != float64(len(c.out)) {
				t.Errorf("row data = %v, want the exit in error and the bytes sent", d)
			}
			if ok := findAudit(h.audit.snapshot(), run.ID, "ssh.sftp.transfer", "success"); ok != nil {
				t.Errorf("a failed transfer also recorded success: %s", ok.Data)
			}
		})
	}
}

// A forward whose relay exits nonzero (its loopback target refused, so nothing
// came back) records ssh.forward failure with the exit, not a zero-byte success.
func TestSSHGateway_ForwardToAClosedPortIsAFailedForward(t *testing.T) {
	st, run, principal := sshOwnedRunningRun(t)
	priv, pub := mustSSHKeypair(t)
	st.putKey(types.SSHPublicKey{Fingerprint: ssh.FingerprintSHA256(pub), Principal: principal, PublicKey: string(ssh.MarshalAuthorizedKey(pub))})
	fr := &sshFakeRunner{execFn: func(runner.ExecSpec) (*runner.ExecSession, error) {
		return &runner.ExecSession{Stdout: strings.NewReader(""), Wait: func() (int, error) { return 1, nil }}, nil
	}}
	h := newSSHTestHarness(t, st, fr)
	client, err := sshDial(t, h, run.ID.String(), priv)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()
	conn, err := client.Dial("tcp", "127.0.0.1:9999")
	if err != nil {
		t.Fatalf("forward to sandbox loopback: %v", err)
	}
	_, _ = io.ReadAll(conn) // the relay ends at once; the gateway closes the channel
	_ = conn.Close()

	ev := waitForAudit(t, h.audit, run.ID, "ssh.forward", "failure")
	if ev == nil {
		t.Fatalf("no failed ssh.forward; events=%s", auditDump(h.audit.snapshot(), run.ID))
	}
	if d := sshAuditData(t, ev); d["error"] != "the forward relay exited 1" || d["port"] != float64(9999) || d["bytes"] != float64(0) {
		t.Errorf("row data = %v, want the exit in error, the port and zero bytes", d)
	}
	if ok := findAudit(h.audit.snapshot(), run.ID, "ssh.forward", "success"); ok != nil {
		t.Errorf("a failed forward also recorded success: %s", ok.Data)
	}
}

// A client that closes its channel or its whole connection before the process
// ends leaves no exit to read: the bridge closes the session under the process,
// Wait errs on the ended context and the bridge folds that to exit 1. Most SFTP
// libraries end a session this way, and so does an ssh -L stopped with a
// connection still open. That is the client's end, not a failed transfer: the
// row stays a success and names no exit.
func TestSSHGateway_ClientClosedFirstIsNotAFailedTransfer(t *testing.T) {
	for _, c := range []struct {
		name, action string
		end          func(t *testing.T, client *ssh.Client, started <-chan struct{})
	}{
		{"sftp, the channel closed", "ssh.sftp.transfer", func(t *testing.T, client *ssh.Client, started <-chan struct{}) {
			sess, err := client.NewSession()
			if err != nil {
				t.Fatal(err)
			}
			if err := sess.RequestSubsystem("sftp"); err != nil {
				t.Fatal(err)
			}
			// The subsystem's reply comes before its process starts: close only once it runs, so the close is
			// the one under the running process this test is about, never one that cuts the start (its own pin
			// is TestSSHGateway_AnSFTPStartItsClientsCloseCutIsNotAFailedTransfer).
			select {
			case <-started:
			case <-time.After(10 * time.Second):
				t.Fatal("the sftp-server never started")
			}
			_ = sess.Close()
		}},
		{"forward, the connection closed", "ssh.forward", func(t *testing.T, client *ssh.Client, _ <-chan struct{}) {
			if _, err := client.Dial("tcp", "127.0.0.1:9999"); err != nil {
				t.Fatalf("forward to sandbox loopback: %v", err)
			}
			_ = client.Close()
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			st, run, principal := sshOwnedRunningRun(t)
			priv, pub := mustSSHKeypair(t)
			st.putKey(types.SSHPublicKey{Fingerprint: ssh.FingerprintSHA256(pub), Principal: principal, PublicKey: string(ssh.MarshalAuthorizedKey(pub))})
			// The process runs until the bridge closes its session, as a real one does.
			stdoutR, stdoutW := io.Pipe()
			started := make(chan struct{})
			markStarted := sync.OnceFunc(func() { close(started) })
			fr := &sshFakeRunner{execFn: func(runner.ExecSpec) (*runner.ExecSession, error) {
				markStarted()
				return &runner.ExecSession{Stdout: stdoutR, Close: stdoutW.Close,
					Wait: func() (int, error) { return 0, fmt.Errorf("exec wait: %w", context.Canceled) }}, nil
			}}
			h := newSSHTestHarness(t, st, fr)
			client, err := sshDial(t, h, run.ID.String(), priv)
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer client.Close()
			c.end(t, client, started)
			ev := waitForAudit(t, h.audit, run.ID, c.action, "success")
			if ev == nil {
				t.Fatalf("no successful %s; events=%s", c.action, auditDump(h.audit.snapshot(), run.ID))
			}
			if d := sshAuditData(t, ev); d["error"] != nil {
				t.Errorf("row data = %v, want no error: no exit was read", d)
			}
			if bad := findAudit(h.audit.snapshot(), run.ID, c.action, "failure"); bad != nil {
				t.Errorf("a session its client closed was recorded as a failure: %s", bad.Data)
			}
		})
	}
}

// A process that ends without an exit while its client is still there (the
// stream broke, the sandbox went away) is a failed transfer that names no exit.
func TestSSHGateway_SFTPEndedWithoutAnExitIsAFailedTransfer(t *testing.T) {
	st, run, principal := sshOwnedRunningRun(t)
	priv, pub := mustSSHKeypair(t)
	st.putKey(types.SSHPublicKey{Fingerprint: ssh.FingerprintSHA256(pub), Principal: principal, PublicKey: string(ssh.MarshalAuthorizedKey(pub))})
	fr := &sshFakeRunner{execFn: func(runner.ExecSpec) (*runner.ExecSession, error) {
		return &runner.ExecSession{Stdout: strings.NewReader("partial"), Wait: func() (int, error) { return 0, fmt.Errorf("wait: %w", runner.ErrSandboxGone) }}, nil
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
	out, _ := sess.StdoutPipe()
	if err := sess.RequestSubsystem("sftp"); err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(out)
	ev := waitForAudit(t, h.audit, run.ID, "ssh.sftp.transfer", "failure")
	if ev == nil {
		t.Fatalf("no failed ssh.sftp.transfer; events=%s", auditDump(h.audit.snapshot(), run.ID))
	}
	if d := sshAuditData(t, ev); d["error"] != "sftp-server ended without an exit status" {
		t.Errorf("row data = %v, want an error that names no exit", d)
	}
}

// A forward's client that closes its channel while the sandbox is still sending ends the forward then, with the
// connection still open. The fake is shaped like a k8s or VM exec: its output never ends, and once the bridge stops
// reading (the channel refused a write) Wait returns only after the session is closed.
func TestSSHGateway_ForwardChannelClosedWhileSendingEndsTheForward(t *testing.T) {
	st, run, principal := sshOwnedRunningRun(t)
	priv, pub := mustSSHKeypair(t)
	st.putKey(types.SSHPublicKey{Fingerprint: ssh.FingerprintSHA256(pub), Principal: principal, PublicKey: string(ssh.MarshalAuthorizedKey(pub))})
	stdoutR, stdoutW := io.Pipe()
	done := make(chan struct{})
	closeSess := sync.OnceFunc(func() { _ = stdoutR.Close(); close(done) })
	go func() {
		chunk := []byte(strings.Repeat("x", 4096))
		for {
			if _, err := stdoutW.Write(chunk); err != nil {
				return
			}
		}
	}()
	fr := &sshFakeRunner{execFn: func(runner.ExecSpec) (*runner.ExecSession, error) {
		return &runner.ExecSession{Stdout: stdoutR, Close: func() error { closeSess(); return nil },
			Wait: func() (int, error) { <-done; return 0, fmt.Errorf("exec wait: %w", context.Canceled) }}, nil
	}}
	h := newSSHTestHarness(t, st, fr)
	client, err := sshDial(t, h, run.ID.String(), priv)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()
	conn, err := client.Dial("tcp", "127.0.0.1:9999")
	if err != nil {
		t.Fatalf("forward to sandbox loopback: %v", err)
	}
	got, err := io.ReadFull(conn, make([]byte, 1024))
	if err != nil {
		t.Fatalf("read the forward: %v", err)
	}
	_ = conn.Close() // the channel only: the connection stays up until the test returns

	ev := waitForAudit(t, h.audit, run.ID, "ssh.forward", "success")
	if ev == nil {
		t.Fatalf("no ssh.forward while the connection is still open; events=%s", auditDump(h.audit.snapshot(), run.ID))
	}
	if d := sshAuditData(t, ev); d["error"] != nil || d["port"] != float64(9999) || !(d["bytes"].(float64) >= float64(got)) {
		t.Errorf("row data = %v, want no error, the port and at least the %d bytes read", d, got)
	}
	if bad := findAudit(h.audit.snapshot(), run.ID, "ssh.forward", "failure"); bad != nil {
		t.Errorf("a forward its client closed was recorded as a failure: %s", bad.Data)
	}
}

// A forward's client that only half-closes (its EOF) still gets the whole reply: the sandbox answers only after its
// stdin ends, and the forward is not cut before that answer arrives.
func TestSSHGateway_ForwardHalfCloseStillGetsTheReply(t *testing.T) {
	st, run, principal := sshOwnedRunningRun(t)
	priv, pub := mustSSHKeypair(t)
	st.putKey(types.SSHPublicKey{Fingerprint: ssh.FingerprintSHA256(pub), Principal: principal, PublicKey: string(ssh.MarshalAuthorizedKey(pub))})
	const reply = "pong"
	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()
	go func() {
		_, _ = io.Copy(io.Discard, stdinR)
		time.Sleep(50 * time.Millisecond) // the answer comes a while after the EOF
		_, _ = stdoutW.Write([]byte(reply))
		_ = stdoutW.Close()
	}()
	fr := &sshFakeRunner{execFn: func(runner.ExecSpec) (*runner.ExecSession, error) {
		return &runner.ExecSession{Stdin: stdinW, Stdout: stdoutR,
			Close: func() error { _ = stdinR.Close(); _ = stdoutR.Close(); return nil },
			Wait:  func() (int, error) { return 0, nil }}, nil
	}}
	h := newSSHTestHarness(t, st, fr)
	client, err := sshDial(t, h, run.ID.String(), priv)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()
	conn, err := client.Dial("tcp", "127.0.0.1:9999")
	if err != nil {
		t.Fatalf("forward to sandbox loopback: %v", err)
	}
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	if err := conn.(interface{ CloseWrite() error }).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	if got, _ := io.ReadAll(conn); string(got) != reply {
		t.Fatalf("reply after the half-close = %q, want %q", got, reply)
	}
	ev := waitForAudit(t, h.audit, run.ID, "ssh.forward", "success")
	if ev == nil {
		t.Fatalf("no ssh.forward; events=%s", auditDump(h.audit.snapshot(), run.ID))
	}
	if d := sshAuditData(t, ev); d["bytes"] != float64(len(reply)) {
		t.Errorf("row data = %v, want the %d reply bytes", d, len(reply))
	}
}
