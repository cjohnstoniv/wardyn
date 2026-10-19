// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package conformance_test

import (
	"fmt"
	"testing"
)

// failureRecorder stands in for the *testing.T bootEgressAssertQuiet reports to: it records the
// failures instead of failing the real test, and Fatalf ends the calling goroutine as it does.
type failureRecorder struct {
	testing.TB
	failed bool
	msgs   []string
}

func (f *failureRecorder) Helper()             {}
func (f *failureRecorder) Logf(string, ...any) {}
func (f *failureRecorder) Errorf(s string, a ...any) {
	f.failed = true
	f.msgs = append(f.msgs, fmt.Sprintf(s, a...))
}
func (f *failureRecorder) Fatalf(s string, a ...any) { f.Errorf(s, a...); panic(f) }

// assertQuiet runs bootEgressAssertQuiet against logs and reports whether it failed the test.
func assertQuiet(logs string, wantHosts bool) (failed bool) {
	rec := &failureRecorder{}
	defer func() {
		if r := recover(); r != nil && r != any(rec) {
			panic(r)
		}
		failed = rec.failed
	}()
	bootEgressAssertQuiet(rec, "wardyn/agent-claude-code:local", "test", logs, wantHosts)
	return
}

const bootEgressAllowLine = `{"decision":"allow","request":{"host":"api.anthropic.com"}}`

// A claude-code boot that parsed no decisions at all is a failure, not a quiet boot; a log with
// an allowed host passes, and an image that may legitimately dial nothing is not held to it.
func TestBootEgressAssertQuietRefusesAnEmptyDecisionStream(t *testing.T) {
	if !assertQuiet("", true) {
		t.Error("an empty proxy log passed for the claude-code image")
	}
	if !assertQuiet("proxy started\nnot a decision\n", true) {
		t.Error("a log with no parsed decision passed for the claude-code image")
	}
	if assertQuiet(bootEgressAllowLine, true) {
		t.Error("a log with an allowed host failed")
	}
	if assertQuiet("", false) {
		t.Error("an empty log failed for an image not required to dial anything")
	}
}
