// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

// #1050 item 3: pins the break-glass boot WARN itself (Q3/finding 5 of the
// FINAL-PR-1293 review) — connectAndMigrate must log
// warnAllowUnknownMigrations's message whenever allowUnknownMigrations is
// true, and must NOT log it when false, regardless of whether the connect
// that follows succeeds. No live Postgres needed: the WARN is the first
// thing connectAndMigrate does, before db.Connect, so an address nothing
// listens on lets this run as a fast unit test — the connect failure that
// follows is expected and ignored.
//
// Mirrors the capture pattern in bedrock_plaintext_warn_test.go
// (TestBedrockPlainHTTPIsAudibleAtBoot).

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestConnectAndMigrate_WarnsWheneverAllowUnknownMigrationsIsSet(t *testing.T) {
	const warnText = "WARDYN_ALLOW_UNKNOWN_MIGRATIONS is set"

	capture := func(t *testing.T, allow bool) string {
		t.Helper()
		var buf bytes.Buffer
		prev := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
		t.Cleanup(func() { slog.SetDefault(prev) })

		// No listener on 127.0.0.1:1: db.Connect fails fast, well inside the
		// 5s budget, so this test stays a fast unit test. The WARN this pins
		// is logged BEFORE that attempt (boot_deps.go), so its presence or
		// absence is unaffected by the connect outcome.
		_, _ = connectAndMigrate(t.Context(), "postgres://nobody@127.0.0.1:1/nope?connect_timeout=1", "",
			5*time.Second, 5*time.Second, allow)
		return buf.String()
	}

	t.Run("allowUnknownMigrations=true logs the WARN", func(t *testing.T) {
		logged := capture(t, true)
		if !strings.Contains(logged, warnText) {
			t.Errorf("boot with the break-glass set logged nothing about it:\n%s", logged)
		}
	})

	t.Run("allowUnknownMigrations=false logs nothing", func(t *testing.T) {
		logged := capture(t, false)
		if strings.Contains(logged, warnText) {
			t.Errorf("boot WITHOUT the break-glass logged the warning anyway:\n%s", logged)
		}
	})
}
