// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package macvm_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
)

// cancelExec is a fake host whose commands honour ctx as exec.CommandContext does: a cancelled ctx runs
// nothing. It cancels the update after the command named cancelAfter, and records whether each command it
// ran had a live ctx.
type cancelExec struct {
	mu          sync.Mutex
	ran         []string
	cancelAfter string
	cancel      context.CancelFunc
	failCreate  bool // create-keychain reports the cancelled ctx after making the keychain
	failDelete  bool
}

func (c *cancelExec) exec(ctx context.Context, argv []string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.ran = append(c.ran, argv[1])
	c.mu.Unlock()
	switch {
	case argv[1] == "create-keychain" && c.failCreate:
		c.cancel()
		return nil, context.Canceled
	case argv[1] == "delete-keychain" && c.failDelete:
		return []byte("refused"), errors.New("exit status 1")
	case argv[1] == c.cancelAfter:
		c.cancel()
	}
	return nil, nil
}

// A signing the update cancels after create-keychain still deletes the keychain, under its own live
// context, and a delete that fails is logged as a WARN even though the signing already failed.
func TestCancelledSigningDeletesTheKeychain(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "agent-signing")
	if _, err := macvm.AgentSigningIdentity(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), "agent")
	if err := os.WriteFile(bin, []byte("new agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, c := range []*cancelExec{{cancelAfter: "import"}, {cancelAfter: "create-keychain"}, {failCreate: true}, {cancelAfter: "import", failDelete: true}} {
		ctx, cancel := context.WithCancel(context.Background())
		c.cancel = cancel
		var logs bytes.Buffer
		s := &macvm.HostAgentSigner{Dir: dir, Exec: c.exec, Log: slog.New(slog.NewTextHandler(&logs, nil))}
		if _, err := s.SignAgent(ctx, bin); err == nil {
			t.Fatalf("%+v: a cancelled signing succeeded", c)
		}
		cancel()
		if !slices.Contains(c.ran, "delete-keychain") {
			t.Fatalf("%+v: delete-keychain did not run: %q", c, c.ran)
		}
		if c.failDelete && (!strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), "keychain")) {
			t.Fatalf("a failed delete was not logged:\n%s", logs.String())
		}
	}
}
