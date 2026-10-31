// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Exec and sftp channels start in the sandbox user's $HOME, as sshd does,
// not in the image's working directory (#3549). The exec tests run the
// gateway's real argv in a real local /bin/sh, with the sandbox's working
// directory and HOME faked by two temp dirs.

// sshLocalSandbox runs each exec's argv on this host from workdir, with only
// PATH, HOME (when home is non-empty), extraEnv and the exec's own env set.
func sshLocalSandbox(t *testing.T, workdir, home string, extraEnv ...string) *sshFakeRunner {
	return &sshFakeRunner{execFn: func(spec runner.ExecSpec) (*runner.ExecSession, error) {
		cmd := exec.Command(spec.Argv[0], spec.Argv[1:]...)
		cmd.Dir = workdir
		cmd.Env = append([]string{"PATH=" + os.Getenv("PATH")}, spec.Env...)
		cmd.Env = append(cmd.Env, extraEnv...)
		if home != "" {
			cmd.Env = append(cmd.Env, "HOME="+home)
		}
		var stderr strings.Builder
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		code := 0
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		} else if err != nil {
			t.Errorf("local exec %v: %v", spec.Argv, err)
		}
		return fakeExecSession(string(out), stderr.String(), code), nil
	}}
}

// sshHomeOutput runs command over a gateway backed by fr and returns stdout.
func sshHomeOutput(t *testing.T, fr *sshFakeRunner, command string) string {
	t.Helper()
	st, run, principal := sshOwnedRunningRun(t)
	priv, pub := mustSSHKeypair(t)
	st.putKey(types.SSHPublicKey{Fingerprint: ssh.FingerprintSHA256(pub), Principal: principal, PublicKey: string(ssh.MarshalAuthorizedKey(pub))})
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
	out, err := sess.Output(command)
	if err != nil {
		t.Fatalf("exec %q: %v (stdout %q)", command, err, out)
	}
	return string(out)
}

// realDir resolves symlinks so a temp dir compares equal to what pwd prints.
func realDir(t *testing.T, dir string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// TestSSHGateway_ExecStartsInHome: `pwd` over an exec channel prints $HOME,
// and the command reaches the sandbox's shell unchanged (quotes, $0).
func TestSSHGateway_ExecStartsInHome(t *testing.T) {
	work, home := realDir(t, t.TempDir()), realDir(t, t.TempDir())
	out := sshHomeOutput(t, sshLocalSandbox(t, work, home), `pwd; printf '<%s>' "a b" "$0"`)
	if want := home + "\n<a b></bin/sh>"; out != want {
		t.Fatalf("exec output = %q, want %q (pwd is $HOME, command unchanged)", out, want)
	}
}

// TestSSHGateway_ExecWithoutHomeStaysInWorkdir pins the fallback: with HOME
// unset, or naming a directory that does not exist, the exec runs from the
// image's working directory, as it did before #3549, and still runs.
func TestSSHGateway_ExecWithoutHomeStaysInWorkdir(t *testing.T) {
	work := realDir(t, t.TempDir())
	for name, home := range map[string]string{
		"HOME unset":   "",
		"HOME missing": filepath.Join(work, "no-such-home"),
	} {
		t.Run(name, func(t *testing.T) {
			if out := sshHomeOutput(t, sshLocalSandbox(t, work, home), "pwd"); out != work+"\n" {
				t.Fatalf("pwd = %q, want the working directory %q", out, work)
			}
		})
	}
}

// TestSSHGateway_ExecCdPrintsNothingBeforeTheCommand (#3554): a relative HOME that CDPATH resolves makes a bare
// `cd` print the directory it entered; the exec's stdout must be the command's output alone, as under sshd.
func TestSSHGateway_ExecCdPrintsNothingBeforeTheCommand(t *testing.T) {
	work, base := realDir(t, t.TempDir()), realDir(t, t.TempDir())
	if err := os.Mkdir(filepath.Join(base, "home"), 0o755); err != nil {
		t.Fatal(err)
	}
	out := sshHomeOutput(t, sshLocalSandbox(t, work, "home", "CDPATH="+base), "pwd")
	if want := filepath.Join(base, "home") + "\n"; out != want {
		t.Fatalf("exec output = %q, want %q (the command's output alone)", out, want)
	}
}

// TestSSHGateway_MutagenAgentRunsByRelativePath is the gateway side of a bare
// `mutagen sync create`: Mutagen installs its agent under $HOME/.mutagen and
// then runs it as `.mutagen/agents/<version>/mutagen-agent`, relative to the
// exec's start directory. Before #3549 that failed with "not found" unless
// the sandbox had a `.mutagen` link in its working directory; there is none
// here.
func TestSSHGateway_MutagenAgentRunsByRelativePath(t *testing.T) {
	work, home := realDir(t, t.TempDir()), realDir(t, t.TempDir())
	agentDir := filepath.Join(home, ".mutagen", "agents", "0.18.1")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "mutagen-agent"), []byte("#!/bin/sh\necho \"agent $*\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(work, ".mutagen")); !os.IsNotExist(err) {
		t.Fatalf("the working directory must have no .mutagen link: %v", err)
	}
	if out := sshHomeOutput(t, sshLocalSandbox(t, work, home), ".mutagen/agents/0.18.1/mutagen-agent synchronizer"); out != "agent synchronizer\n" {
		t.Fatalf("agent output = %q, want %q", out, "agent synchronizer\n")
	}
}

// TestSSHGateway_SFTPStartsInHome: a plain sftp subsystem runs sftp-server
// with `-d %d`, which starts it in the user's passwd home, so relative paths
// resolve from $HOME (sftp-server stays where it is when it cannot enter it).
// wardyn-sync's explicit directory is pinned in sshgateway_sync_test.go.
func TestSSHGateway_SFTPStartsInHome(t *testing.T) {
	st, run, principal := sshOwnedRunningRun(t)
	priv, pub := mustSSHKeypair(t)
	st.putKey(types.SSHPublicKey{Fingerprint: ssh.FingerprintSHA256(pub), Principal: principal, PublicKey: string(ssh.MarshalAuthorizedKey(pub))})
	fr := &sshFakeRunner{execFn: func(runner.ExecSpec) (*runner.ExecSession, error) { return fakeEchoExecSession(), nil }}
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
	in, _ := sess.StdinPipe()
	out, _ := sess.StdoutPipe()
	if err := sess.RequestSubsystem("sftp"); err != nil {
		t.Fatalf("request sftp subsystem: %v", err)
	}
	syncEcho(t, in, out, "x")
	if argv, _ := fr.lastCall(); !slices.Equal(argv, []string{sftpServerPath, "-e", "-d", "%d"}) {
		t.Fatalf("sftp argv = %v, want [%s -e -d %%d]", argv, sftpServerPath)
	}
}
