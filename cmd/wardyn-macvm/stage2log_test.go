// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"flag"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
)

// logGuest is a stage-2 guest that takes every push and answers every command with nothing.
type logGuest struct{}

func (logGuest) Push(_ context.Context, _ string, _ fs.FileMode, r io.Reader, _ int64) error {
	_, err := io.Copy(io.Discard, r)
	return err
}
func (logGuest) Run(context.Context, []string) (string, int, error)    { return "", 0, nil }
func (logGuest) DesktopPassword(context.Context, []byte) (bool, error) { return true, nil }
func (logGuest) Pointer(context.Context) (int, int, error)             { return 0, 0, nil }
func (logGuest) ToolsDial(context.Context) (string, error)             { return macvm.ToolsDialConnected, nil }
func (logGuest) AgentExec(context.Context, []string, []string) (string, int, error) {
	return "", 0, nil
}

// The job's stage 2 logs its warnings to the command's log (stderr, the job's log file): here the agent that
// could not be signed on this host and is pushed ad hoc.
func TestStage2WarningsReachTheJobLog(t *testing.T) {
	root := t.TempDir()
	agent := filepath.Join(root, "agent")
	if err := os.WriteFile(agent, []byte("agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	j := newJobFlags(fs)
	if err := fs.Parse([]string{"--" + macvm.FlagRoot, root}); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	st, err := j.stage2(nil, deps{stderr: &stderr}, nil, agent)
	if err != nil {
		t.Fatal(err)
	}
	st.Tooling = macvm.Tooling{Version: 1}
	st.Boot = func(context.Context, macvm.MachineSpec) (macvm.ToolsGuest, func(context.Context) error, error) {
		return logGuest{}, func(context.Context) error { return nil }, nil
	}
	if signer, ok := st.Signer.(*macvm.HostAgentSigner); ok {
		signer.Exec = func(context.Context, []string) ([]byte, error) { return []byte("no codesign here"), os.ErrPermission }
	}
	res, _ := st.Run(context.Background(), macvm.Stage2Input{})
	if res.AgentSignature != macvm.AgentSignedAdHoc || !strings.Contains(stderr.String(), "level=WARN") || !strings.Contains(stderr.String(), "not signed on this Mac") {
		t.Fatalf("signature %q; the job log:\n%s", res.AgentSignature, stderr.String())
	}
}
