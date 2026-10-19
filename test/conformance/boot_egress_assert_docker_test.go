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
func assertQuiet(logs, control string) (failed bool) {
	rec := &failureRecorder{}
	defer func() {
		if r := recover(); r != nil && r != any(rec) {
			panic(r)
		}
		failed = rec.failed
	}()
	bootEgressAssertQuiet(rec, "wardyn/agent-claude-code:local", "test", logs, control)
	return
}

const (
	bootEgressControlLine = `{"decision":"allow","request":{"host":"bedrock-runtime.us-east-1.amazonaws.com"}}`
	bootEgressOtherLine   = `{"decision":"allow","request":{"host":"api.anthropic.com"}}`
	bootEgressPendingLine = `{"decision":"hold","request":{"host":"statsig.anthropic.com"}}`
)

// A boot whose control dial left no decision in the log is a failure, not a quiet boot, even
// when the agent's own traffic filled the log; the control's decision passes, a first-use
// approval beside it still fails, and an image held to no control may dial nothing.
func TestBootEgressAssertQuietRefusesAnEmptyDecisionStream(t *testing.T) {
	const control = bootEgressModelHost
	if !assertQuiet("", control) {
		t.Error("an empty proxy log passed with a control dial")
	}
	if !assertQuiet("proxy started\nnot a decision\n", control) {
		t.Error("a log with no parsed decision passed with a control dial")
	}
	if !assertQuiet(bootEgressOtherLine, control) {
		t.Error("a log holding only the agent's own host passed with a control dial")
	}
	if assertQuiet(bootEgressControlLine, control) {
		t.Error("a log with the control's decision failed")
	}
	if !assertQuiet(bootEgressControlLine+"\n"+bootEgressPendingLine, control) {
		t.Error("a pending first-use approval beside the control passed")
	}
	if assertQuiet("", "") {
		t.Error("an empty log failed for an image not held to a control")
	}
}
