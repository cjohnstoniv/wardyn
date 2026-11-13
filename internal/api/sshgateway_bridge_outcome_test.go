// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"testing"

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
		end          func(t *testing.T, client *ssh.Client)
	}{
		{"sftp, the channel closed", "ssh.sftp.transfer", func(t *testing.T, client *ssh.Client) {
			sess, err := client.NewSession()
			if err != nil {
				t.Fatal(err)
			}
			if err := sess.RequestSubsystem("sftp"); err != nil {
				t.Fatal(err)
			}
			_ = sess.Close()
		}},
		{"forward, the connection closed", "ssh.forward", func(t *testing.T, client *ssh.Client) {
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
			fr := &sshFakeRunner{execFn: func(runner.ExecSpec) (*runner.ExecSession, error) {
				return &runner.ExecSession{Stdout: stdoutR, Close: stdoutW.Close,
					Wait: func() (int, error) { return 0, fmt.Errorf("exec wait: %w", context.Canceled) }}, nil
			}}
			h := newSSHTestHarness(t, st, fr)
			client, err := sshDial(t, h, run.ID.String(), priv)
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer client.Close()
			c.end(t, client)
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
