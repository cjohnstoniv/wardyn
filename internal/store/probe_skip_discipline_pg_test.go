// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

// THE SKIP DISCIPLINE FOR THIS PACKAGE'S PROBES, derived rather than declared.
//
// A test that skips produces `--- SKIP` -> `ok` -> exit 0, scripts/test-report.sh
// graded on the exit code alone, and nothing inspected the JSON stream for
// skips — so a probe that quietly stopped running looked exactly like a probe
// that passed, and the invariant it proves (audit_events is append-only) was
// unfalsifiable from CI's own output. internal/db's F11 probes were given this
// treatment; this package's were not, and its tamper probes skip on precisely
// the precondition CI's lane always satisfies.
//
// DERIVED FROM THE CONNECTION, not only from a marker: a role that can bypass
// the append-only triggers, over a URL-form DSN, can satisfy every precondition
// these probes guard on, and any failure after that is a real one. Requiring an
// env marker alone would leave the hole open on every lane nobody remembered to
// set it on. The marker stays as an explicit override, and CI's test-pg job sets
// it.
//
// It is a THIRD copy of internal/db's rule only because Go test helpers cannot
// cross a package boundary without a new non-test package; the words and the
// derivation are deliberately identical so the two lanes cannot drift into
// different ideas of what a hidden failure is.

import (
	"context"
	"net/url"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// storeProbeSkipMarker lets an operator ASSERT that this lane is fully
// provisioned, turning every precondition guard in the F11 probes into a failure
// instead of a skip.
const storeProbeSkipMarker = "WARDYN_TEST_PG_SUPERUSER"

// storeProbeMustNotSkip reports whether a skip from here on would be HIDING a
// failure rather than reporting an unmet precondition.
func storeProbeMustNotSkip(t *testing.T, pool *pgxpool.Pool) bool {
	t.Helper()
	if os.Getenv(storeProbeSkipMarker) == "1" {
		return true
	}
	u, err := url.Parse(os.Getenv("WARDYN_TEST_PG"))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return false
	}
	var super bool
	if err := pool.QueryRow(context.Background(),
		`SELECT rolsuper FROM pg_roles WHERE rolname = current_user`).Scan(&super); err != nil {
		return false
	}
	return super
}

// storeSkipOrFatal reports an unmet precondition the way this lane deserves: a
// skip where the environment genuinely cannot provide it, a FAILURE where it can
// and did not.
func storeSkipOrFatal(t *testing.T, pool *pgxpool.Pool, format string, args ...any) {
	t.Helper()
	if storeProbeMustNotSkip(t, pool) {
		t.Fatalf("this lane can satisfy this precondition, so a skip here would hide a failure ("+
			storeProbeSkipMarker+"=1, or a trigger-bypass-capable role over a URL-form DSN): "+format, args...)
	}
	t.Skipf(format, args...)
}

// TestPG_ProbeF11_StoreLaneCannotSilentlySelfSkip pins the derivation itself, the
// way internal/db's sibling does. On the lane CI actually runs — superuser, URL-
// form DSN — every F11 probe in this package MUST be in fail-not-skip mode; if
// that stops being true, the tamper and splice probes can go back to reporting
// `ok` while proving nothing.
//
// Its own name matches the skip floor scripts/test-report.sh applies to the pg
// suite, so a lane that cannot even run THIS is caught by the tooling.
func TestPG_ProbeF11_StoreLaneCannotSilentlySelfSkip(t *testing.T) {
	pool := runsPGPool(t)
	if !storeProbeMustNotSkip(t, pool) {
		u, _ := url.Parse(os.Getenv("WARDYN_TEST_PG"))
		t.Fatalf("this lane is in SKIP-ALLOWED mode (url_dsn=%v, %s=%q): the F11 append-only probes in this package "+
			"can self-skip to a green `ok` here and nothing in the go test exit code would show it. Point "+
			"WARDYN_TEST_PG at a trigger-bypass-capable role over a URL-form DSN, or set %s=1.",
			u != nil && u.Scheme != "" && u.Host != "", storeProbeSkipMarker, os.Getenv(storeProbeSkipMarker), storeProbeSkipMarker)
	}
}
