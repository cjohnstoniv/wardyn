// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package audit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestLogWriteFailure asserts the two properties the helper exists to hold:
// a dropped audit write is reported at ERROR with its routing fields as
// structured attrs, and ev.Data — the one secret-bearing field on an
// AuditEvent — never reaches the log.
func TestLogWriteFailure(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	ev := types.AuditEvent{
		Action:  "credential.mint",
		Target:  "grant-123",
		Outcome: "failure",
		// A realistic payload: this is what must NOT be logged.
		Data: json.RawMessage(`{"token":"ghs_SUPERSECRETVALUE"}`),
	}
	LogWriteFailure(context.Background(), ev, errors.New("boom"))

	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("log line is not valid JSON (not structured): %v: %q", err, buf.String())
	}

	if got := rec["level"]; got != "ERROR" {
		t.Errorf("level = %v, want ERROR (a dropped audit write must be alertable)", got)
	}
	for k, want := range map[string]string{
		"action":  "credential.mint",
		"target":  "grant-123",
		"outcome": "failure",
		"err":     "boom",
	} {
		if got := rec[k]; got != want {
			t.Errorf("attr %q = %v, want %q", k, got, want)
		}
	}

	// The invariant: no part of ev.Data may appear anywhere in the output.
	if strings.Contains(buf.String(), "ghs_SUPERSECRETVALUE") {
		t.Errorf("ev.Data leaked into the log: %q", buf.String())
	}
	if _, ok := rec["data"]; ok {
		t.Errorf("ev.Data was logged as an attr: %q", buf.String())
	}
}
