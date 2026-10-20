// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestSSHGateway_RemoteSSHChannelsOnOneConnection is the gateway side of an editor's remote session
// (#2988), as VS Code Remote-SSH with "remote.SSH.localServerDownload": "always" opens it: on one
// connection, an exec channel that runs a setup script, an sftp transfer of the server the editor
// downloaded itself, a second exec that starts it, and a -L forward to the port it listens on in the
// sandbox's loopback. The sandbox is faked: an in-memory sftp-server, a shell that answers with its
// argv, and an echo behind socat. It checks the channels, not a real editor.
func TestSSHGateway_RemoteSSHChannelsOnOneConnection(t *testing.T) {
	st, run, principal := sshOwnedRunningRun(t)
	priv, pub := mustSSHKeypair(t)
	st.putKey(types.SSHPublicKey{Fingerprint: ssh.FingerprintSHA256(pub), Principal: principal, PublicKey: string(ssh.MarshalAuthorizedKey(pub))})
	files := sftp.InMemHandler()
	fr := &sshFakeRunner{execFn: func(spec runner.ExecSpec) (*runner.ExecSession, error) {
		switch {
		case len(spec.Argv) > 0 && spec.Argv[0] == sftpServerPath:
			return inMemSFTPSession(files), nil
		case len(spec.Argv) > 0 && spec.Argv[0] == "socat":
			return fakeEchoExecSession(), nil
		}
		return fakeOutputExecSession(strings.Join(spec.Argv, " ")), nil
	}}
	h := newSSHTestHarness(t, st, fr)
	client, err := sshDial(t, h, run.ID.String(), priv)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	exec := func(cmd string) string {
		t.Helper()
		sess, err := client.NewSession()
		if err != nil {
			t.Fatalf("new session: %v", err)
		}
		defer sess.Close()
		out, err := sess.Output(cmd)
		if err != nil {
			t.Fatalf("exec %q: %v", cmd, err)
		}
		return string(out)
	}
	if out := exec("uname -rsv; echo setup"); !strings.Contains(out, "uname -rsv; echo setup") {
		t.Fatalf("the setup script did not run as sent: %q", out)
	}

	sc, err := sftp.NewClient(client)
	if err != nil {
		t.Fatalf("sftp: %v", err)
	}
	server := bytes.Repeat([]byte("vscode-server"), 4096)
	if err := sc.MkdirAll("/home/agent/.vscode-server/bin"); err != nil {
		t.Fatalf("sftp mkdir: %v", err)
	}
	f, err := sc.Create("/home/agent/.vscode-server/bin/server.tar.gz")
	if err != nil {
		t.Fatalf("sftp create: %v", err)
	}
	if _, err := f.Write(server); err != nil {
		t.Fatalf("sftp write: %v", err)
	}
	_ = f.Close()
	got, err := sc.Open("/home/agent/.vscode-server/bin/server.tar.gz")
	if err != nil {
		t.Fatalf("sftp open: %v", err)
	}
	back, _ := io.ReadAll(got)
	_ = got.Close()
	_ = sc.Close()
	if !bytes.Equal(back, server) {
		t.Fatalf("the uploaded server came back %d bytes, want %d", len(back), len(server))
	}

	if out := exec("sh ~/.vscode-server/bin/start.sh --port 0"); !strings.Contains(out, "start.sh") {
		t.Fatalf("the start script did not run as sent: %q", out)
	}

	conn, err := client.Dial("tcp", "127.0.0.1:40123")
	if err != nil {
		t.Fatalf("-L to the server's port: %v", err)
	}
	if _, err := conn.Write([]byte("hello-server")); err != nil {
		t.Fatalf("forward write: %v", err)
	}
	buf := make([]byte, len("hello-server"))
	if _, err := io.ReadFull(conn, buf); err != nil || string(buf) != "hello-server" {
		t.Fatalf("forward read = %q, %v", buf, err)
	}
	_ = conn.Close()
	if ev := waitForAudit(t, h.audit, run.ID, "ssh.forward", "success"); ev == nil {
		t.Errorf("the forward is not audited; events=%s", auditDump(h.audit.snapshot(), run.ID))
	}
}

// inMemSFTPSession is a sandbox sftp-server over files.
func inMemSFTPSession(files sftp.Handlers) *runner.ExecSession {
	toServer, stdin := io.Pipe()
	stdout, fromServer := io.Pipe()
	stderrR, stderrW := io.Pipe()
	srv := sftp.NewRequestServer(struct {
		io.Reader
		io.WriteCloser
	}{toServer, fromServer}, files)
	done := make(chan struct{})
	go func() {
		_ = srv.Serve()
		_ = fromServer.Close()
		_ = stderrW.Close()
		close(done)
	}()
	return &runner.ExecSession{
		Stdin: stdin, Stdout: stdout, Stderr: stderrR,
		Wait:  func() (int, error) { <-done; return 0, nil },
		Close: func() error { _ = stdin.Close(); return srv.Close() },
	}
}

// fakeOutputExecSession prints out and exits 0.
func fakeOutputExecSession(out string) *runner.ExecSession {
	stdin := nopWriteCloser{io.Discard}
	return &runner.ExecSession{
		Stdin: stdin, Stdout: strings.NewReader(out), Stderr: strings.NewReader(""),
		Wait: func() (int, error) { return 0, nil },
	}
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }
