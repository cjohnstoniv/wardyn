// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker && k8s

package main

import (
	"context"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
)

// TestStartDecisionSinkRefuses: a sink key file others can read, or a sink port already bound, is
// an error, which k8sRunnerHooks returns as a boot refusal.
func TestStartDecisionSinkRefuses(t *testing.T) {
	ctx := context.Background()
	lo := netip.MustParseAddr("127.0.0.1")
	t.Run("readable key file", func(t *testing.T) {
		dir := t.TempDir()
		if _, _, err := loadOrCreateSinkCert(filepath.Join(dir, "decision-sink.key")); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(filepath.Join(dir, "decision-sink.key"), 0o644); err != nil {
			t.Fatal(err)
		}
		if s, err := startDecisionSink(ctx, dir, k8sEnv{podIP: lo, sink: 1}, "10.96.0.40", spoolSinkOptions{}); err == nil {
			s.Close()
			t.Fatal("a readable sink key file started the sink")
		}
	})
	t.Run("port in use", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()
		port := int32(ln.Addr().(*net.TCPAddr).Port)
		if s, err := startDecisionSink(ctx, t.TempDir(), k8sEnv{podIP: lo, sink: port}, "10.96.0.40", spoolSinkOptions{}); err == nil {
			s.Close()
			t.Fatal("a bound sink port started the sink")
		}
	})
}
