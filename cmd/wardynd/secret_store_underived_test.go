// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/secretstore"
)

// The boot warning for pointer rows the store no longer derives names the
// settings that fix where values live, per store (#2807).
func TestUnderivedPointersWarningNamesTheSettings(t *testing.T) {
	for store, want := range map[string][]string{
		"vaultkv": {"WARDYN_VAULT_KV_MOUNT", "WARDYN_VAULT_KV_PREFIX"},
		"azurekv": {"WARDYN_AZURE_KV_PREFIX", "WARDYN_AZURE_KV_URL"},
	} {
		got := underivedPointersWarning(store)
		for _, w := range want {
			if !strings.Contains(got, w) {
				t.Errorf("warning for %s = %q; want it to name %s", store, got, w)
			}
		}
	}
}

type failingPointerStore struct {
	secretstore.Store
	err error
}

func (f failingPointerStore) UnderivedPointers(context.Context) (int, string, error) {
	return 0, "", f.err
}
func (failingPointerStore) ExternalName() string { return "vaultkv" }

// A scan that cannot run is logged, not swallowed, and never stops the boot.
func TestWarnUnderivedPointersLogsAFailedScan(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	warnUnderivedPointers(context.Background(), failingPointerStore{err: errors.New("scan boom")})
	if got := buf.String(); !strings.Contains(got, "scan boom") {
		t.Errorf("log = %q, want it to carry the scan error", got)
	}
}
