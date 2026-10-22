// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package main

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// rawPipe is a pipe whose ends are bare descriptors without close-on-exec, as a token pipe inherited
// from `wardyn runner setup` is. No *os.File owns them, so nothing closes them behind the test's back.
func rawPipe(t *testing.T, token string) (read int) {
	t.Helper()
	p := make([]int, 2)
	if err := syscall.Pipe(p); err != nil {
		t.Fatal(err)
	}
	if _, err := syscall.Write(p[1], []byte(token+"\n")); err != nil {
		t.Fatal(err)
	}
	_ = syscall.Close(p[1])
	return p[0]
}

// childSees is whether a child process started now has descriptor n open.
func childSees(t *testing.T, n int) bool {
	t.Helper()
	err := exec.Command("/bin/sh", "-c", fmt.Sprintf("[ -e /dev/fd/%d ]", n)).Run()
	if _, ok := err.(*exec.ExitError); err != nil && !ok {
		t.Fatal(err)
	}
	return err == nil
}

func fdOpen(n int) bool { _, err := unix.FcntlInt(uintptr(n), unix.F_GETFD, 0); return err == nil }

// (a) The first thing setup does with a /dev/fd/N token file is mark N close-on-exec: even when setup
// stops before reading it (here, no --org), no child can inherit it.
func TestSetupMarksTheTokenDescriptorCloseOnExecFirst(t *testing.T) {
	n := rawPipe(t, "wdr_"+strings.Repeat("a", 64))
	t.Cleanup(func() { _ = syscall.Close(n) })
	if !childSees(t, n) {
		t.Fatal("precondition: a child should inherit the raw pipe")
	}
	cmd := newSetupCommand()
	cmd.SetArgs([]string{"--token-file", fmt.Sprintf("/dev/fd/%d", n)})
	cmd.SetOut(new(strings.Builder))
	cmd.SetErr(new(strings.Builder))
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), `"org" not set`) {
		t.Fatalf("err = %v", err)
	}
	if childSees(t, n) {
		t.Fatal("a child started after setup began inherits the token descriptor")
	}
}

// (b) setup reads the token and closes its descriptor before preflight starts any child, so the first
// child it runs (systemctl --user show) sees no token descriptor, and the token is still registered.
func TestSetupReadsAndClosesTheTokenDescriptorBeforeAnyChild(t *testing.T) {
	f := newSetupFixture(t, "linux")
	n := rawPipe(t, "wdr_"+strings.Repeat("a", 64))
	run, checked := f.host.run, false
	f.host.run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if !checked {
			checked = true
			if fdOpen(n) {
				t.Errorf("the token descriptor is still open when setup runs %s %v", name, args)
			}
			if childSees(t, n) {
				t.Errorf("the first child setup runs (%s) inherits the token descriptor", name)
			}
		}
		return run(ctx, name, args...)
	}
	if out, err := f.run(setupOptions{tokenFile: fmt.Sprintf("/dev/fd/%d", n)}); err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	if !checked || f.count("POST /api/v1/runners/register") != 1 {
		t.Fatalf("checked=%v registers=%d", checked, f.count("POST /api/v1/runners/register"))
	}
}
