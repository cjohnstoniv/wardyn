// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package guestagent

import (
	"bytes"
	"cmp"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// shareProbeFix ends every ShareProbe refusal: the one fix, made in the base.
const shareProbeFix = "approve Full Disk Access for the Wardyn guest agent in edit-base; see macos-vms.md#folders"

// ShareProbe reads a mounted share as the agent before the run starts. The read starts the way a run's exec
// does (Server.command: its own session, the agent's ids), so macOS charges it to the guest agent as it would
// the run's reads. A guest agent without Full Disk Access waits there for a network-volume privacy approval
// that nobody in the agent's locked session can give, and every read the run made would wait the same way.
// That a listing meets the same wait as a read of a file is UNVERIFIED (external) until a real Mac shows it.
type ShareProbe struct {
	// Cred is the agent's ids; nil reads as the guest agent's own user (tests only).
	Cred *syscall.Credential
	// argv is the read, before the folder; nil is ls -a (tests set one that fails or hangs).
	argv []string
	// bound is how long the read may take; zero is 10 s (tests shorten it).
	bound time.Duration
}

// Read lists dir as the agent. A listing that fails, or that has no answer within the bound, is an error
// that names the fix. Past the bound the probe's whole process group is killed first, so nothing of it
// lingers; Wait reaps it in the background.
func (p ShareProbe) Read(ctx context.Context, dir string) error {
	argv := p.argv
	if argv == nil {
		argv = []string{"/bin/ls", "-a"}
	}
	bound := cmp.Or(p.bound, 10*time.Second)
	cmd := exec.Command(argv[0], append(argv[1:len(argv):len(argv)], dir)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Credential: p.Cred}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("listing it as the agent: %w; %s", err, shareProbeFix)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	timer := time.NewTimer(bound)
	defer timer.Stop()
	select {
	case err := <-done:
		if err == nil {
			return nil
		}
		return fmt.Errorf("listing it as the agent failed (%v: %s); %s", err, strings.TrimSpace(stderr.String()), shareProbeFix)
	case <-timer.C:
	case <-ctx.Done():
	}
	// Setsid made the probe its own process group, so this also ends whatever it started.
	_ = unix.Kill(-cmd.Process.Pid, unix.SIGKILL)
	if err := ctx.Err(); err != nil {
		return err
	}
	return fmt.Errorf("listing it as the agent had no answer in %s; %s", bound, shareProbeFix)
}
