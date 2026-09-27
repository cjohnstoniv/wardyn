// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package testlive is the opt-in live-local harness: tests that reach the
// owner's real identity provider, Azure DevOps organisation and AWS account.
// The live suites build only under `-tags live`, skip unless their own
// WARDYN_LIVE_* gate is set, and never run in CI. docs/LIVE-TESTS.md is the
// owner's setup and run guide.
//
// Everything the live suites print goes through Redact first, and they take
// secrets only as paths to files kept outside the repository.
package testlive

import (
	"fmt"
	"regexp"
	"testing"
)

// redactions run in order: the specific shapes first, so a JWT is reported as
// a JWT rather than as an anonymous long run of characters.
var redactions = []struct {
	re   *regexp.Regexp
	mask string
}{
	{regexp.MustCompile(`eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]*`), "[jwt]"},
	{regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/=-]+`), "Bearer [token]"},
	{regexp.MustCompile(`\b(?:AKIA|ASIA)[A-Z0-9]{16}\b`), "[aws-key-id]"},
	{regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`), "[email]"},
	{regexp.MustCompile(`\b[0-9A-Fa-f]{8}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{12}\b`), "[guid]"},
	{regexp.MustCompile(`\b\d{12}\b`), "[12-digit]"},
	// An AWS secret key, a session token, an Azure DevOps token or a refresh
	// token: none has a fixed prefix, but every one is a long unbroken run.
	{regexp.MustCompile(`[A-Za-z0-9/+_-]{40,}={0,2}`), "[long-secret]"},
}

// Redact masks every credential- or identity-shaped substring of s.
func Redact(s string) string {
	for _, r := range redactions {
		s = r.re.ReplaceAllString(s, r.mask)
	}
	return s
}

// Logf, Errorf, Fatalf and Skipf are the only ways a live suite writes output.
func Logf(t testing.TB, format string, args ...any) {
	t.Helper()
	t.Log(Redact(fmt.Sprintf(format, args...)))
}

func Errorf(t testing.TB, format string, args ...any) {
	t.Helper()
	t.Error(Redact(fmt.Sprintf(format, args...)))
}

func Fatalf(t testing.TB, format string, args ...any) {
	t.Helper()
	t.Fatal(Redact(fmt.Sprintf(format, args...)))
}

func Skipf(t testing.TB, format string, args ...any) {
	t.Helper()
	t.Skip(Redact(fmt.Sprintf(format, args...)))
}
