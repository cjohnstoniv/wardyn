// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/binary"
	"errors"
	"io"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

type dataOutSSH struct {
	h      *sshTestHarness
	st     *sshMemStore
	fr     *sshFakeRunner
	client *ssh.Client
	run    types.AgentRun
	execs  *atomic.Int32
}

// newDataOutSSH dials an SSH connection to a run whose recorded policy carries rule. The rule is read when the
// connection opens, so it is set before the dial; mutate sets the store's other answers.
func newDataOutSSH(t *testing.T, rule types.TransferRule, mutate ...func(*sshMemStore)) *dataOutSSH {
	t.Helper()
	st, run, principal := sshOwnedRunningRun(t)
	st.policy = types.RunPolicySpec{DataOut: rule}
	for _, m := range mutate {
		m(st)
	}
	priv, pub := mustSSHKeypair(t)
	st.putKey(types.SSHPublicKey{Fingerprint: ssh.FingerprintSHA256(pub), Principal: principal, PublicKey: string(ssh.MarshalAuthorizedKey(pub))})
	execs := &atomic.Int32{}
	fr := &sshFakeRunner{
		execFn:   func(runner.ExecSpec) (*runner.ExecSession, error) { execs.Add(1); return fakeEchoExecSession(), nil },
		attachFn: func() (runner.Session, error) { return newFakeShellSession(), nil },
	}
	h := newSSHTestHarness(t, st, fr)
	client, err := sshDial(t, h, run.ID.String(), priv)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return &dataOutSSH{h: h, st: st, fr: fr, client: client, run: run, execs: execs}
}

func (d *dataOutSSH) denied(t *testing.T, path string) *types.AuditEvent {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, ev := range d.h.audit.snapshot() {
			if ev.Action == "run.data_out.deny" && ev.Outcome == "denied" && strings.Contains(string(ev.Data), `"path":"`+path+`"`) {
				return &ev
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	return nil
}

func TestDataOutSSHExecRefused(t *testing.T) {
	for _, rule := range []types.TransferRule{types.TransferReview, types.TransferBlocked} {
		d := newDataOutSSH(t, rule)
		sess, err := d.client.NewSession()
		if err != nil {
			t.Fatal(err)
		}
		err = sess.Run("cat /etc/hostname")
		var exit *ssh.ExitError
		if err == nil || errors.As(err, &exit) && exit.ExitStatus() == 0 {
			t.Errorf("%s: exec ran: %v", rule, err)
		}
		if n := d.execs.Load(); n != 0 {
			t.Errorf("%s: %d execs reached the sandbox", rule, n)
		}
		if d.denied(t, "exec") == nil {
			t.Errorf("%s: no run.data_out.deny row for exec; events=%s", rule, auditDump(d.h.audit.snapshot(), d.run.ID))
		}
	}
}

func TestDataOutSSHSFTPRefused(t *testing.T) {
	d := newDataOutSSH(t, types.TransferReview)
	sess, err := d.client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.RequestSubsystem("sftp"); err == nil {
		t.Error("the sftp subsystem was accepted under review")
	}
	if d.denied(t, "sftp") == nil {
		t.Errorf("no run.data_out.deny row for sftp; events=%s", auditDump(d.h.audit.snapshot(), d.run.ID))
	}
	if n := d.execs.Load(); n != 0 {
		t.Errorf("%d execs reached the sandbox", n)
	}
}

// TestDataOutSSHShellOpen: the shell is a display path and stays open under blocked.
func TestDataOutSSHShellOpen(t *testing.T) {
	d := newDataOutSSH(t, types.TransferBlocked)
	sess, err := d.client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	if _, err := sess.StdinPipe(); err != nil {
		t.Fatal(err)
	}
	if err := sess.Shell(); err != nil {
		t.Fatalf("shell under blocked: %v", err)
	}
	if ev := waitForAudit(t, d.h.audit, d.run.ID, "session.attach", "success"); ev == nil {
		t.Errorf("the shell did not attach; events=%s", auditDump(d.h.audit.snapshot(), d.run.ID))
	}
}

func TestDataOutDirectTCPIPRefused(t *testing.T) {
	d := newDataOutSSH(t, types.TransferReview)
	if conn, err := d.client.Dial("tcp", "127.0.0.1:9999"); err == nil {
		_ = conn.Close()
		t.Error("a port forward was opened under review")
	} else if !strings.Contains(err.Error(), "data_out") {
		t.Errorf("the refusal = %v, want it to name the data_out rule", err)
	}
	if d.denied(t, "port_forward") == nil {
		t.Errorf("no run.data_out.deny row for the forward; events=%s", auditDump(d.h.audit.snapshot(), d.run.ID))
	}
}

// TestDataOutSSHUnreadableFailsClosed: a rule that cannot be read, or a run with no recorded policy, refuses
// exec and forwards, and the shell still opens.
func TestDataOutSSHUnreadableFailsClosed(t *testing.T) {
	for name, mutate := range map[string]func(*sshMemStore){
		"read error":  func(s *sshMemStore) { s.policyErr = errors.New("audit store unavailable") },
		"no envelope": func(s *sshMemStore) { s.noPolicy = true },
	} {
		d := newDataOutSSH(t, "", mutate)
		sess, err := d.client.NewSession()
		if err != nil {
			t.Fatal(err)
		}
		if err := sess.Run("true"); err == nil {
			t.Errorf("%s: exec ran", name)
		}
		if conn, err := d.client.Dial("tcp", "127.0.0.1:9999"); err == nil {
			_ = conn.Close()
			t.Errorf("%s: a port forward opened", name)
		}
	}
}

// TestDataOutSSHAbsentChangesNothing: with the rule absent or allowed, exec and forwards answer as on the base.
func TestDataOutSSHAbsentChangesNothing(t *testing.T) {
	for _, rule := range []types.TransferRule{"", types.TransferAllowed} {
		d := newDataOutSSH(t, rule)
		sess, err := d.client.NewSession()
		if err != nil {
			t.Fatal(err)
		}
		if err := sess.Run("true"); err != nil {
			t.Errorf("data_out %q: exec = %v, want it to run", rule, err)
		}
		conn, err := d.client.Dial("tcp", "127.0.0.1:9999")
		if err != nil {
			t.Errorf("data_out %q: forward = %v, want it to open", rule, err)
			continue
		}
		_ = conn.Close()
	}
}

// TestDataOutSyncStartsReadDenied: under review or blocked the sync's sftp-server starts with -P read,readlink;
// under allowed its argv is as before.
func TestDataOutSyncStartsReadDenied(t *testing.T) {
	for rule, want := range map[types.TransferRule][]string{
		types.TransferReview:  {sftpServerPath, "-e", "-d", "/home/agent/work/proj", "-P", "read,readlink"},
		types.TransferBlocked: {sftpServerPath, "-e", "-d", "/home/agent/work/proj", "-P", "read,readlink"},
		"":                    {sftpServerPath, "-e", "-d", "/home/agent/work/proj"},
	} {
		d := newDataOutSSH(t, rule)
		f := &sshSyncFixture{st: d.st, h: d.h, fr: d.fr, client: d.client, run: d.run}
		_, in, out, err := f.openSync(t, map[string]string{"WARDYN_SYNC_DIR": "/home/agent/work/proj", "WARDYN_SYNC_DIRECTION": "push"})
		if err != nil {
			t.Fatalf("%q: wardyn-sync refused: %v", rule, err)
		}
		syncEcho(t, in, out, "twelve-bytes")
		if argv, _ := d.fr.lastCall(); !slices.Equal(argv, want) {
			t.Errorf("data_out %q: argv = %v, want %v", rule, argv, want)
		}
		_ = in.Close()
	}
}

// sftpDataThenName is a sandbox whose sftp-server answers with a NAME reply and then, ignoring -P, a DATA reply.
func sftpDataThenName() *runner.ExecSession {
	pkt := func(typ byte, n int) []byte {
		b := binary.BigEndian.AppendUint32(nil, uint32(1+n))
		return append(append(b, typ), make([]byte, n)...)
	}
	stream := append(append(pkt(104, 8), pkt(103, 32)...), pkt(104, 8)...)
	pr, pw := io.Pipe()
	go func() { _, _ = pw.Write(stream); _ = pw.Close() }()
	return &runner.ExecSession{
		Stdin: nopWriteCloser{io.Discard}, Stdout: pr, Stderr: strings.NewReader(""),
		Wait: func() (int, error) { return 0, nil }, Close: func() error { return nil },
	}
}

// TestDataOutSyncClosesOnData: the first file-content reply ends the sync, none of its bytes reach the client,
// and the denial is audited as sync_down.
func TestDataOutSyncClosesOnData(t *testing.T) {
	d := newDataOutSSH(t, types.TransferReview)
	d.fr.execFn = func(runner.ExecSpec) (*runner.ExecSession, error) { return sftpDataThenName(), nil }
	f := &sshSyncFixture{st: d.st, h: d.h, fr: d.fr, client: d.client, run: d.run}
	_, _, out, err := f.openSync(t, map[string]string{"WARDYN_SYNC_DIR": "/home/agent/work/proj", "WARDYN_SYNC_DIRECTION": "pull"})
	if err != nil {
		t.Fatalf("wardyn-sync refused: %v", err)
	}
	got, _ := io.ReadAll(out)
	if len(got) != 12 {
		t.Errorf("the client read %d bytes, want only the 12 of the NAME reply before the DATA reply", len(got))
	}
	if d.denied(t, "sync_down") == nil {
		t.Errorf("no run.data_out.deny row for sync_down; events=%s", auditDump(d.h.audit.snapshot(), d.run.ID))
	}
}
