// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package main

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/api"
)

// The adapter's mapping, against a real daemon (WARDYN_TEST_DOCKER=1) and the public registry:
// a manifest the registry does not have is api.ErrRemoteDigestNotFound, and a known one resolves.
func TestLive_RemoteDigestUnknownManifestIsNotFound(t *testing.T) {
	if os.Getenv("WARDYN_TEST_DOCKER") != "1" {
		t.Skip("set WARDYN_TEST_DOCKER=1 to run the real-Docker adapter case")
	}
	builder, err := newEnvBuilder("", "unused.invalid/repo")
	if err != nil {
		t.Fatal(err)
	}
	ider, ok := builder.(api.ImageIdentifier)
	if !ok {
		t.Fatal("the envbuild adapter is not an api.ImageIdentifier")
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()

	if d, err := ider.RemoteDigest(ctx, "ghcr.io/devcontainers/features/go:1"); err != nil || !strings.HasPrefix(d, "sha256:") {
		t.Fatalf("a known feature: digest %q, err %v", d, err)
	}
	_, err = ider.RemoteDigest(ctx, "ghcr.io/devcontainers/features/go@sha256:"+strings.Repeat("0", 64))
	if !errors.Is(err, api.ErrRemoteDigestNotFound) {
		t.Fatalf("an unknown manifest: err = %v, want api.ErrRemoteDigestNotFound", err)
	}
}
