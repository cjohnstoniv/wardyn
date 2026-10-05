// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A session channel answers each request by its type: malformed payloads are
// refused rather than guessed at, requests the gateway never grants are refused
// by name, and once the one shell/exec/subsystem has started, every later
// request that would start or reshape it is refused. No reply may ever be
// "true" for something that did not happen.
func TestSSHGateway_SessionRequestsAreAnsweredByWhatTheyAre(t *testing.T) {
	st, run, principal := sshOwnedRunningRun(t)
	priv, pub := mustSSHKeypair(t)
	st.putKey(types.SSHPublicKey{Fingerprint: ssh.FingerprintSHA256(pub), Principal: principal, PublicKey: string(ssh.MarshalAuthorizedKey(pub))})
	execSess, release := fakeBlockingExecSession()
	t.Cleanup(func() { _ = release.Close() })
	fr := &sshFakeRunner{execFn: func(runner.ExecSpec) (*runner.ExecSession, error) { return execSess, nil }}
	h := newSSHTestHarness(t, st, fr)
	client, err := sshDial(t, h, run.ID.String(), priv)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()
	sess, err := client.NewSession()
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	defer sess.Close()

	ask := func(name string, wantReply bool, payload []byte) bool {
		t.Helper()
		ok, err := sess.SendRequest(name, wantReply, payload)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return ok
	}
	pty := ssh.Marshal(sshPTYReqMsg{Term: "xterm", Columns: 80, Rows: 24})
	garbage := []byte{0x01}

	// Before anything has started.
	if ask("pty-req", true, garbage) {
		t.Error("a pty-req with a malformed payload was accepted")
	}
	if !ask("pty-req", true, pty) {
		t.Error("a well-formed pty-req was refused")
	}
	if ask("exec", true, garbage) {
		t.Error("an exec with a malformed payload was accepted")
	}
	if ask("subsystem", true, garbage) {
		t.Error("a subsystem with a malformed payload was accepted")
	}
	if ask("auth-agent-req@openssh.com", true, nil) {
		t.Error("agent forwarding was granted: it is refused by omission")
	}
	if ask("x11-req", true, nil) {
		t.Error("X11 forwarding was granted")
	}
	if !ask("env", true, ssh.Marshal(sshEnvMsg{Name: "TERM", Value: "xterm"})) {
		t.Error("env is always acknowledged, whether or not it is kept")
	}
	// A burst of resizes with nothing reading them coalesces rather than blocking the loop
	// (a blocked loop would never answer the third), a client that asks for a reply gets
	// one, and a malformed one sent without a reply does not stall the exec behind it.
	for range 3 {
		if !ask("window-change", true, ssh.Marshal(sshWindowChangeMsg{Columns: 100, Rows: 30})) {
			t.Error("window-change with a reply requested was refused")
		}
	}
	ask("window-change", false, garbage)

	// The first exec starts the session; every later request that would start or
	// reshape it is refused.
	if !ask("exec", true, ssh.Marshal(sshExecReqMsg{Command: "sleep 1"})) {
		t.Fatal("the first exec was refused")
	}
	for name, payload := range map[string][]byte{
		"pty-req":   pty,
		"shell":     nil,
		"exec":      ssh.Marshal(sshExecReqMsg{Command: "true"}),
		"subsystem": ssh.Marshal(sshSubsystemMsg{Subsystem: "sftp"}),
	} {
		if ask(name, true, payload) {
			t.Errorf("a %s after the session started was accepted", name)
		}
	}
}
