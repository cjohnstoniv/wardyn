// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/workspacescan"
)

func TestExecute_StopsAtFirstFailure(t *testing.T) {
	dir := t.TempDir()
	cmds := []workspacescan.SetupCommand{
		{Stage: "install", Command: "echo installing"},
		{Stage: "build", Command: "echo building && exit 3"},
		{Stage: "test", Command: "echo SHOULD_NOT_RUN"},
	}
	res := execute(dir, cmds, nil)
	if res.OK {
		t.Error("OK should be false after a failing step")
	}
	if len(res.Steps) != 2 {
		t.Fatalf("expected 2 steps (stop at first failure), got %d", len(res.Steps))
	}
	if res.Steps[0].ExitCode != 0 || res.Steps[1].ExitCode != 3 {
		t.Errorf("exit codes = %d,%d want 0,3", res.Steps[0].ExitCode, res.Steps[1].ExitCode)
	}
	if !strings.Contains(res.Steps[0].LogHead, "installing") {
		t.Errorf("expected captured output, got %q", res.Steps[0].LogHead)
	}
}

func TestExecute_AllPass(t *testing.T) {
	res := execute(t.TempDir(), []workspacescan.SetupCommand{{Stage: "test", Command: "true"}}, nil)
	if !res.OK || !res.Ran {
		t.Errorf("all-pass: OK=%v Ran=%v", res.OK, res.Ran)
	}
}

func TestRollingCollector_HeadAndTail(t *testing.T) {
	c := newRollingCollector(4, 4)
	c.Write([]byte("ABCDEFGHIJ")) // 10 bytes
	if c.head() != "ABCD" {
		t.Errorf("head = %q, want ABCD", c.head())
	}
	if c.tail() != "GHIJ" {
		t.Errorf("tail = %q, want GHIJ", c.tail())
	}
	// Short output: head holds all, tail empty (no duplication).
	c2 := newRollingCollector(10, 4)
	c2.Write([]byte("hi"))
	if c2.head() != "hi" || c2.tail() != "" {
		t.Errorf("short: head=%q tail=%q", c2.head(), c2.tail())
	}
}
