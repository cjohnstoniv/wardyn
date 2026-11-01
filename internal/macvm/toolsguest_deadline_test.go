// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package macvm

import (
	"context"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/vm/guestagent"
)

// A guest command that never answers, such as one held by a macOS prompt, fails when the exec deadline
// passes, with a message that says why, and does not wait for the VM's life or the caller's context.
func TestToolsGuestRunEndsAtTheExecDeadline(t *testing.T) {
	old := toolsExecDeadline
	toolsExecDeadline = 200 * time.Millisecond
	defer func() { toolsExecDeadline = old }()
	dial := func(context.Context) (net.Conn, error) {
		c, peer := net.Pipe()
		go func() { _, _ = io.Copy(io.Discard, peer) }() // reads the request, never answers
		return c, nil
	}
	g := &toolsGuest{tools: guestagent.NewClient(dial), life: context.Background()}
	defer g.tools.Close()

	done := make(chan error, 1)
	go func() {
		_, _, err := g.Run(context.Background(), []string{"/usr/bin/security", "delete-certificate"})
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "security did not finish within") || !strings.Contains(err.Error(), "waiting for a person") {
			t.Fatalf("Run = %v; want the deadline message", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return: a guest command has no deadline")
	}
}
