// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package macvm_test

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"github.com/cjohnstoniv/wardyn/internal/vm/guestagent"
)

// A certificate whose key is missing or unreadable is refused, never replaced: a new identity would change
// the designated requirement and drop every grant without a word. The certificate stays, the push falls back
// to ad hoc, and the WARN says how to start over.
func TestAgentSigningKeyMissingKeepsTheCertificate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "agent-signing")
	if _, err := macvm.AgentSigningIdentity(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	cert, _ := os.ReadFile(filepath.Join(dir, "cert.pem"))
	for name, breakKey := range map[string]func() error{
		"missing":    func() error { return os.Remove(filepath.Join(dir, "key.pem")) },
		"unreadable": func() error { return os.WriteFile(filepath.Join(dir, "key.pem"), []byte("not a key"), 0o600) },
	} {
		if err := breakKey(); err != nil {
			t.Fatal(err)
		}
		before, _ := os.ReadFile(filepath.Join(dir, "key.pem"))
		if _, err := macvm.AgentSigningIdentity(context.Background(), dir); err == nil || !strings.Contains(err.Error(), dir) {
			t.Fatalf("%s key: identity %v, want a refusal naming %s", name, err, dir)
		}
		if after, _ := os.ReadFile(filepath.Join(dir, "cert.pem")); !bytes.Equal(after, cert) {
			t.Fatalf("%s key: the certificate was replaced", name)
		}
		if after, _ := os.ReadFile(filepath.Join(dir, "key.pem")); !bytes.Equal(after, before) {
			t.Fatalf("%s key: the key was rewritten", name)
		}

		f, g := newTools(t), newGuest()
		var logs bytes.Buffer
		f.opts.Log = slog.New(slog.NewTextHandler(&logs, nil))
		bin := filepath.Join(t.TempDir(), "agent")
		if err := os.WriteFile(bin, []byte("new agent"), 0o755); err != nil {
			t.Fatal(err)
		}
		f.opts.Agent = &macvm.AgentPin{Version: "0.10.1", Path: bin, Dest: guestagent.MacAgentBinaryPath}
		f.opts.Signer = &macvm.HostAgentSigner{Dir: dir, Exec: (&fakeSignExec{}).exec}
		var boots, stops atomic.Int64
		res := runStage(t, f.stage(g, &boots, &stops))
		if res.AgentSignature != macvm.AgentSignedAdHoc || string(g.files[guestagent.MacAgentBinaryPath]) != "new agent" {
			t.Fatalf("%s key: signature %q, agent %q", name, res.AgentSignature, g.files[guestagent.MacAgentBinaryPath])
		}
		if !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), "remove "+dir) || !strings.Contains(logs.String(), "Full Disk Access") {
			t.Fatalf("%s key: the WARN does not say how to start over:\n%s", name, logs.String())
		}
	}
}
