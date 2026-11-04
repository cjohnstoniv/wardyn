// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/internal/vm/guestagent"
)

const (
	linuxAgentBin = "/usr/local/bin/wardyn-guest-agent"
	macAgentBin   = "/usr/local/libexec/wardyn-guest-agent"
)

// The relay, the listening check and the sftp-server are chosen by the run's machine kind; a container
// keeps the argv it always had.
func TestSSHRelayArgvByMachineKind(t *testing.T) {
	socat := []string{"socat", "-", "TCP:127.0.0.1:8080"}
	for _, c := range []struct {
		name string
		kind types.MachineKind
		want []string
		sftp string
	}{
		{"unset (every run before VMs)", "", socat, "/usr/lib/openssh/sftp-server"},
		{"container", types.MachineContainer, socat, "/usr/lib/openssh/sftp-server"},
		{"linux vm", types.MachineLinuxVM, []string{linuxAgentBin, "forward", "8080"}, "/usr/lib/openssh/sftp-server"},
		{"macos vm", types.MachineMacOSVM, []string{macAgentBin, "forward", "8080"}, "/usr/libexec/sftp-server"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := sshRelayArgv(c.kind, 8080); !slices.Equal(got, c.want) {
				t.Errorf("sshRelayArgv = %v, want %v", got, c.want)
			}
			if got := sftpServerPathFor(c.kind); got != c.sftp {
				t.Errorf("sftpServerPathFor = %q, want %q", got, c.sftp)
			}
		})
	}
	if guestagent.LinuxAgentPath != linuxAgentBin || guestagent.MacAgentBinaryPath != macAgentBin {
		t.Errorf("the guest agent paths moved: %q %q", guestagent.LinuxAgentPath, guestagent.MacAgentBinaryPath)
	}
}

// The UI app's probe-launch script asks socat in a container (as before) and the agent's forward -probe
// in a VM, every time it asks.
func TestUILauncherScriptProbeByMachineKind(t *testing.T) {
	const socatProbe = `socat -u /dev/null "TCP:127.0.0.1:$p" >/dev/null 2>&1`
	for _, kind := range []types.MachineKind{"", types.MachineContainer} {
		s := uiLauncherScriptFor(kind, "/usr/local/bin/wardyn-ui-code", 8080, 0)
		if n := strings.Count(s, socatProbe); n != 3 || strings.Contains(s, "forward") {
			t.Errorf("kind %q: %d socat probes, forward present %v; want 3 and none", kind, n, strings.Contains(s, "forward"))
		}
	}
	for kind, bin := range map[types.MachineKind]string{types.MachineLinuxVM: linuxAgentBin, types.MachineMacOSVM: macAgentBin} {
		s := uiLauncherScriptFor(kind, "/usr/local/bin/wardyn-ui-code", 8080, 0)
		probe := "'" + bin + `' forward -probe "$p" >/dev/null 2>&1`
		if n := strings.Count(s, probe); n != 3 || strings.Contains(s, "socat") {
			t.Errorf("kind %q: %d agent probes, socat present %v; want 3 and none", kind, n, strings.Contains(s, "socat"))
		}
	}
}

// argvLog records every argv the fake sandbox is asked to exec.
type argvLog struct {
	mu   sync.Mutex
	argv [][]string
}

func (l *argvLog) add(a []string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.argv = append(l.argv, slices.Clone(a))
}

func (l *argvLog) reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.argv = nil
}

func (l *argvLog) all() [][]string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.argv)
}

// -L and sftp against a VM run exec the guest agent's forward mode and the kind's sftp-server; a
// destination that is not the sandbox's own loopback, and a port outside 1-65535, are refused for a VM run
// with no exec at all, exactly as for a container.
func TestSSHGateway_VMRunDoors(t *testing.T) {
	for _, c := range []struct {
		kind    types.MachineKind
		agent   string
		sftpBin string
	}{
		{types.MachineLinuxVM, linuxAgentBin, "/usr/lib/openssh/sftp-server"},
		{types.MachineMacOSVM, macAgentBin, "/usr/libexec/sftp-server"},
	} {
		t.Run(string(c.kind), func(t *testing.T) {
			st, run, principal := sshOwnedRunningRun(t)
			run.MachineKind = c.kind
			st.putRun(run)
			priv, pub := mustSSHKeypair(t)
			st.putKey(types.SSHPublicKey{Fingerprint: ssh.FingerprintSHA256(pub), Principal: principal, PublicKey: string(ssh.MarshalAuthorizedKey(pub))})
			var log argvLog
			fr := &sshFakeRunner{execFn: func(spec runner.ExecSpec) (*runner.ExecSession, error) {
				log.add(spec.Argv)
				return fakeEchoExecSession(), nil
			}}
			h := newSSHTestHarness(t, st, fr)
			client, err := sshDial(t, h, run.ID.String(), priv)
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer client.Close()

			for _, dst := range []string{"10.0.0.5:8080", "0.0.0.0:8080", "example.com:8080", "127.0.0.1:0"} {
				if _, err := client.Dial("tcp", dst); err == nil {
					t.Errorf("forward to %s succeeded for a %s run, want refused", dst, c.kind)
				}
			}
			if got := log.all(); len(got) != 0 {
				t.Fatalf("a refused forward ran %v, want no exec", got)
			}

			// Screen Sharing's ports are the macOS window's door: refused there, forwardable elsewhere.
			for _, port := range []string{"5900", "5905", "5909"} {
				conn, err := client.Dial("tcp", "127.0.0.1:"+port)
				if err == nil {
					_ = conn.Close() // an open forward holds one of the run's channel slots
				}
				if c.kind.IsMacOSVM() && err == nil {
					t.Errorf("forward to %s succeeded for a macOS VM run, want refused", port)
				}
				if !c.kind.IsMacOSVM() && err != nil {
					t.Errorf("forward to %s for a %s run: %v, want it allowed", port, c.kind, err)
				}
			}
			if c.kind.IsMacOSVM() {
				if got := log.all(); len(got) != 0 {
					t.Fatalf("a refused Screen Sharing forward ran %v, want no exec", got)
				}
			}
			log.reset()

			conn, err := client.Dial("tcp", "127.0.0.1:9999")
			if err != nil {
				t.Fatalf("forward to the sandbox loopback: %v", err)
			}
			if _, err := conn.Write([]byte("x")); err != nil {
				t.Fatalf("write: %v", err)
			}
			buf := make([]byte, 1)
			if _, err := io.ReadFull(conn, buf); err != nil {
				t.Fatalf("read: %v", err)
			}
			_ = conn.Close()
			if got := log.all(); len(got) != 1 || !slices.Equal(got[0], []string{c.agent, "forward", "9999"}) {
				t.Errorf("-L exec = %v, want [[%s forward 9999]]", got, c.agent)
			}

			sess, err := client.NewSession()
			if err != nil {
				t.Fatalf("new session: %v", err)
			}
			defer sess.Close()
			in, err := sess.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			out, err := sess.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := sess.RequestSubsystem("sftp"); err != nil {
				t.Fatalf("sftp subsystem: %v", err)
			}
			if _, err := in.Write([]byte("hello")); err != nil {
				t.Fatal(err)
			}
			if _, err := io.ReadFull(out, make([]byte, 5)); err != nil {
				t.Fatal(err)
			}
			got := log.all()
			if last := got[len(got)-1]; len(last) < 2 || last[0] != c.sftpBin || last[1] != "-e" {
				t.Errorf("sftp exec = %v, want it to start [%s -e]", last, c.sftpBin)
			}
		})
	}
}

// A container run's -L stays on socat, with the argv it always had.
func TestSSHGateway_ContainerRunForwardStaysSocat(t *testing.T) {
	st, run, principal := sshOwnedRunningRun(t)
	priv, pub := mustSSHKeypair(t)
	st.putKey(types.SSHPublicKey{Fingerprint: ssh.FingerprintSHA256(pub), Principal: principal, PublicKey: string(ssh.MarshalAuthorizedKey(pub))})
	var log argvLog
	fr := &sshFakeRunner{execFn: func(spec runner.ExecSpec) (*runner.ExecSession, error) {
		log.add(spec.Argv)
		return fakeEchoExecSession(), nil
	}}
	h := newSSHTestHarness(t, st, fr)
	client, err := sshDial(t, h, run.ID.String(), priv)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()
	conn, err := client.Dial("tcp", "127.0.0.1:9999")
	if err != nil {
		t.Fatalf("forward: %v", err)
	}
	_ = conn.Close()
	if got := log.all(); len(got) != 1 || !slices.Equal(got[0], []string{"socat", "-", "TCP:127.0.0.1:9999"}) {
		t.Errorf("container -L exec = %v, want [[socat - TCP:127.0.0.1:9999]]", got)
	}
}

// A UI app on a macOS VM's Screen Sharing ports is refused by the relay with a sentence, before any exec and
// with no ui.open row; the same port on a Linux VM run is not refused by this check.
func TestUIGateway_MacOSVMScreenSharingPortRefused(t *testing.T) {
	for _, port := range []int{5900, 5905, 5909} {
		h := newUIHarness(t, okBackend())
		h.run.MachineKind = types.MachineMacOSVM
		h.store.putRun(h.run)
		now := time.Now()
		sess := uiSession{Run: h.run.ID, App: "code", Port: port, Principal: h.owner,
			Expires: now.Add(time.Hour).Unix(), IssuedAt: 1, AuthorizedAt: now.Unix()}
		ctx := context.WithValue(context.Background(), uiSessionCtxKey{}, sess)
		_, err := h.srv.uiDial(ctx, "tcp", h.run.ID.String()+":"+strconv.Itoa(port))
		var de *uiDialError
		if !errors.As(err, &de) || de.status != http.StatusForbidden || !strings.Contains(de.msg, "open the run's window instead") {
			t.Fatalf("port %d: uiDial = %v, want 403 naming the run's window", port, err)
		}
		if rows := h.audit.auditRows("ui.open", "success"); len(rows) != 0 {
			t.Errorf("port %d: a refused relay wrote ui.open rows %+v", port, rows)
		}
	}
}
