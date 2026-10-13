// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A row the sealer refuses is logged by its action and a fixed word, never by the row's fields or the error.
func TestSealingRecorderLogsARowItRefuses(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	human := types.AuditEvent{ActorType: types.ActorHuman, Actor: "alice-the-secret-name", Action: "run.kill"}
	full := sealingRecorder{inner: &captureRecorder{}, src: newAuditSealSource(audit.SealFull)}
	if err := full.Record(t.Context(), human); err == nil {
		t.Fatal("a human row before the keys were armed was not refused")
	}
	out := buf.String()
	if !strings.Contains(out, "action=run.kill") || !strings.Contains(out, "why=keys_not_ready") {
		t.Errorf("log = %q, want the action and why=keys_not_ready", out)
	}
	if strings.Contains(out, "alice-the-secret-name") {
		t.Errorf("log carries the row's fields: %q", out)
	}
}
