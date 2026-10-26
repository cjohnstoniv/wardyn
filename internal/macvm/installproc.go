// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package macvm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"syscall"
)

// InstallCommand is the helper's subcommand that runs one install for
// prepare, in a process of its own (RunInstall). Only prepare starts it.
const InstallCommand = "install-base"

// installRequest is what prepare writes to the install process's stdin.
type installRequest struct {
	Spec MachineSpec
	IPSW string
}

// installProgress is one line the install process writes to its stdout. A
// line without a fraction is not progress.
type installProgress struct {
	Fraction *float64 `json:"fraction"`
}

// stderrTail is how much of the install process's stderr its error keeps.
const stderrTail = 2048

// tailWriter keeps the last stderrTail bytes written to it.
type tailWriter struct{ b []byte }

func (w *tailWriter) Write(p []byte) (int, error) {
	if len(p) >= stderrTail {
		w.b = append(w.b[:0], p[len(p)-stderrTail:]...)
		return len(p), nil
	}
	w.b = append(w.b, p...)
	if over := len(w.b) - stderrTail; over > 0 {
		w.b = append(w.b[:0], w.b[over:]...)
	}
	return len(p), nil
}

// RunInstall is the install process's side: it reads an installRequest from
// in, builds the installer's VM with eng and runs Apple's installer, writing
// each progress fraction to out as one JSON line. The process then exits.
//
// It runs apart from prepare because the installer's VM is never released
// while the process that built it lives: Code-Hex/vz's MacOSInstaller.Install
// keeps a cgo.Handle to its progress callback that it never deletes, the
// callback holds the installer, the installer holds the VirtualMachine, and
// only their finalizers release the framework's objects. The framework then
// keeps the VM's lock on the aux storage, and refuses the first boot on it.
// The process's exit is the release.
func RunInstall(ctx context.Context, eng Engine, in io.Reader, out io.Writer) error {
	var req installRequest
	if err := json.NewDecoder(in).Decode(&req); err != nil {
		return fmt.Errorf("macvm: install request: %w", err)
	}
	enc := json.NewEncoder(out)
	return installVM(ctx, eng, req.Spec, req.IPSW, func(f float64) { _ = enc.Encode(installProgress{Fraction: &f}) })
}

// installVM builds the installer's VM and runs the installer on it.
func installVM(ctx context.Context, eng Engine, spec MachineSpec, ipsw string, progress func(float64)) error {
	guest, host, err := NewNetPair()
	if err != nil {
		return err
	}
	defer guest.Close()
	defer host.Close() // nothing reads it: the installer's VM has no network
	spec.Net = guest
	plan, err := BuildPlan(spec)
	if err != nil {
		return err
	}
	m, err := eng.NewMachine(plan)
	if err != nil {
		return err
	}
	return eng.Install(ctx, m, ipsw, progress)
}

// installInProcess runs the install in the process argv starts (RunInstall)
// and returns only once that process has exited. ctx ending sends it SIGTERM,
// which cancels the installer as ctx would in this process.
func installInProcess(ctx context.Context, argv []string, spec MachineSpec, ipsw string, progress func(float64)) error {
	spec.Net = nil
	req, err := json.Marshal(installRequest{Spec: spec, IPSW: ipsw})
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = DefaultStopTimeout
	cmd.Stdin = bytes.NewReader(req)
	stderr := &tailWriter{}
	cmd.Stderr = stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start the install process: %w", err)
	}
	sc := bufio.NewScanner(stdout)
	for sc.Scan() {
		var ev installProgress
		if json.Unmarshal(sc.Bytes(), &ev) == nil && ev.Fraction != nil {
			progress(*ev.Fraction)
		}
	}
	_, _ = io.Copy(io.Discard, stdout) // a line too long for the scanner
	if err := cmd.Wait(); err != nil {
		msg := strings.TrimSpace(string(stderr.b))
		if msg == "" {
			return fmt.Errorf("the install process: %w", err)
		}
		return fmt.Errorf("%s (the install process: %w)", msg, err)
	}
	return nil
}
