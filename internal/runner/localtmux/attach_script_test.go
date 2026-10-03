// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build e2etmux && !docker && linux

package localtmux

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// The harness attach must stay the command the Docker substrate runs.
func TestAttachScriptMatchesDockerSession(t *testing.T) {
	src, err := os.ReadFile("../docker/session.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), attachScript) {
		t.Fatalf("docker/session.go no longer runs %q", attachScript)
	}
}

// A real tmux, a real PTY: output flows, and the requested size reaches the pane.
func TestAttachRunsRealTmuxAtRequestedSize(t *testing.T) {
	conf, err := filepath.Abs("../../../deploy/images/common/tmux.conf")
	if err != nil {
		t.Fatal(err)
	}
	s, err := New("wardyn-test-"+t.Name(), conf)
	if err != nil {
		t.Fatal(err) // a missing tmux fails here; it never skips
	}
	t.Cleanup(func() { _ = s.KillSandbox(context.Background(), "") })

	sess, err := s.Attach(context.Background(), "ref", runner.AttachOptions{Cols: 100, Rows: 30})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	if _, err := sess.Write([]byte("echo size-$(stty size | tr ' ' x)\n")); err != nil {
		t.Fatal(err)
	}
	got := make(chan string, 1)
	go func() {
		var acc strings.Builder
		buf := make([]byte, 4096)
		for {
			n, err := sess.Read(buf)
			acc.Write(buf[:n])
			if strings.Contains(acc.String(), "size-30x100") || err != nil {
				got <- acc.String()
				return
			}
		}
	}()
	select {
	case out := <-got:
		if !strings.Contains(out, "size-30x100") {
			t.Fatalf("pane did not report 30x100: %q", out)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("no output from tmux")
	}
}
