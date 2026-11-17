// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
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

type channelSwitchRig struct {
	h      *sshTestHarness
	client *ssh.Client
	run    types.AgentRun
	execs  *atomic.Int32
}

// newChannelSwitchRig dials the gateway with WARDYN_SSH_CHANNELS set to channels (nil = unset).
func newChannelSwitchRig(t *testing.T, channels map[string]bool) *channelSwitchRig {
	t.Helper()
	st, run, principal := sshOwnedRunningRun(t)
	priv, pub := mustSSHKeypair(t)
	st.putKey(types.SSHPublicKey{Fingerprint: ssh.FingerprintSHA256(pub), Principal: principal, PublicKey: string(ssh.MarshalAuthorizedKey(pub))})
	execs := &atomic.Int32{}
	fr := &sshFakeRunner{
		execFn:   func(runner.ExecSpec) (*runner.ExecSession, error) { execs.Add(1); return fakeEchoExecSession(), nil },
		attachFn: func() (runner.Session, error) { return newFakeShellSession(), nil },
	}
	h := newSSHTestHarness(t, st, fr, func(c *Config) { c.SSHChannels = channels })
	client, err := sshDial(t, h, run.ID.String(), priv)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return &channelSwitchRig{h: h, client: client, run: run, execs: execs}
}

// stderrOf reads what the gateway wrote to a refused session's stderr, or "" after two seconds.
func stderrOf(sess *ssh.Session) string {
	r, err := sess.StderrPipe()
	if err != nil {
		return ""
	}
	got := make(chan string, 1)
	go func() { b, _ := io.ReadAll(r); got <- string(b) }()
	select {
	case s := <-got:
		return s
	case <-time.After(2 * time.Second):
		return ""
	}
}

// TestSSHChannelSwitch drives a real ssh client at each of the four kinds, listed and not listed. A kind that is
// not listed is refused with the one sentence and one ssh.channel.reject row (reason channel_off); a listed
// one still works and writes no such row.
func TestSSHChannelSwitch(t *testing.T) {
	all := []string{"shell", "exec", "sftp", "forward"}
	for _, tc := range []struct {
		kind        string
		channelType string
		sentence    string
		// try opens the kind and returns what the client was told when it was refused ("" when it was served).
		try func(t *testing.T, r *channelSwitchRig) string
	}{
		{"shell", "shell", "this deployment's SSH gateway does not serve a terminal (WARDYN_SSH_CHANNELS)", func(t *testing.T, r *channelSwitchRig) string {
			sess, err := r.client.NewSession()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = sess.Close() })
			if _, err := sess.StdinPipe(); err != nil {
				t.Fatal(err)
			}
			if err := sess.Shell(); err != nil {
				return stderrOf(sess) + " " + err.Error()
			}
			return ""
		}},
		{"exec", "exec", "this deployment's SSH gateway does not serve commands (exec) (WARDYN_SSH_CHANNELS)", func(t *testing.T, r *channelSwitchRig) string {
			sess, err := r.client.NewSession()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = sess.Close() })
			if err := sess.Run("cat /etc/hostname"); err != nil {
				var exit *ssh.ExitError
				var missing *ssh.ExitMissingError
				if !errors.As(err, &exit) && !errors.As(err, &missing) {
					return stderrOf(sess) + " " + err.Error()
				}
			}
			return ""
		}},
		{"sftp", "sftp", "this deployment's SSH gateway does not serve file transfer (sftp) (WARDYN_SSH_CHANNELS)", func(t *testing.T, r *channelSwitchRig) string {
			sess, err := r.client.NewSession()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = sess.Close() })
			if err := sess.RequestSubsystem("sftp"); err != nil {
				return stderrOf(sess) + " " + err.Error()
			}
			return ""
		}},
		{"forward", "direct-tcpip", "this deployment's SSH gateway does not serve port forwarding (WARDYN_SSH_CHANNELS)", func(t *testing.T, r *channelSwitchRig) string {
			conn, err := r.client.Dial("tcp", "127.0.0.1:9999")
			if err != nil {
				var oce *ssh.OpenChannelError
				if errors.As(err, &oce) {
					return oce.Message
				}
				return err.Error()
			}
			_ = conn.Close()
			return ""
		}},
	} {
		for _, on := range []bool{false, true} {
			name := tc.kind + "/off"
			listed := slices.DeleteFunc(slices.Clone(all), func(k string) bool { return k == tc.kind })
			if on {
				name, listed = tc.kind+"/on", []string{tc.kind}
			}
			t.Run(name, func(t *testing.T) {
				channels := map[string]bool{}
				for _, k := range listed {
					channels[k] = true
				}
				r := newChannelSwitchRig(t, channels)
				refused := tc.try(t, r)
				rejected := findAudit(r.h.audit.snapshot(), r.run.ID, "ssh.channel.reject", "failure")
				if on {
					if refused != "" || rejected != nil {
						t.Fatalf("a listed %s was refused (%q) or audited as rejected; events=%s", tc.kind, refused, auditDump(r.h.audit.snapshot(), r.run.ID))
					}
					if tc.kind == "exec" && r.execs.Load() != 1 {
						t.Errorf("a listed exec reached the sandbox %d times, want 1", r.execs.Load())
					}
					return
				}
				if !strings.Contains(refused, tc.sentence) {
					t.Errorf("the client was told %q, want it to hold %q", refused, tc.sentence)
				}
				rejected = waitForAudit(t, r.h.audit, r.run.ID, "ssh.channel.reject", "failure")
				if rejected == nil {
					t.Fatalf("no ssh.channel.reject row; events=%s", auditDump(r.h.audit.snapshot(), r.run.ID))
				}
				if d := sshAuditData(t, rejected); d["channel_type"] != tc.channelType || d["reason"] != "channel_off" {
					t.Errorf("row data = %v, want channel_type %q and reason channel_off", d, tc.channelType)
				}
				if n := r.execs.Load(); n != 0 {
					t.Errorf("%d execs reached the sandbox through a kind that is off", n)
				}
			})
		}
	}
}

// TestSSHChannelSwitch_UnsetServesAllAndSyncIsAlwaysOn: no list serves every kind, and a list never turns off
// wardyn-sync, which is not a name an operator can list.
func TestSSHChannelSwitch_UnsetServesAllAndSyncIsAlwaysOn(t *testing.T) {
	r := newChannelSwitchRig(t, nil)
	for _, k := range []sshChannelKind{sshKindShell, sshKindExec, sshKindSFTP, sshKindForward, sshKindSync} {
		if !r.h.srv.sshChannelOn(k) {
			t.Errorf("unset: %s is off, want every kind served", k)
		}
	}
	only := newChannelSwitchRig(t, map[string]bool{"exec": true})
	if !only.h.srv.sshChannelOn(sshKindSync) || only.h.srv.sshChannelOn(sshKindShell) {
		t.Errorf("with only exec listed: sync on = %v, shell on = %v, want true and false", only.h.srv.sshChannelOn(sshKindSync), only.h.srv.sshChannelOn(sshKindShell))
	}
}

// TestSSHChannelSwitch_HealthzListsTheServedKinds: /healthz's ssh.channels is the served kinds, sorted, without
// sync, and is present whether or not the operator set a list.
func TestSSHChannelSwitch_HealthzListsTheServedKinds(t *testing.T) {
	for _, tc := range []struct {
		set  map[string]bool
		want []string
	}{
		{nil, []string{"exec", "forward", "sftp", "shell"}},
		{map[string]bool{"sftp": true, "exec": true}, []string{"exec", "sftp"}},
	} {
		got, _ := newChannelSwitchRig(t, tc.set).h.srv.sshGatewayHealthz()["channels"].([]string)
		if !slices.Equal(got, tc.want) {
			t.Errorf("channels for %v = %v, want %v", tc.set, got, tc.want)
		}
	}
}
