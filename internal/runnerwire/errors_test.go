// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerwire

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// The code list is closed and pinned: adding one is a design change.
func TestErrorCodeToSentinelMap(t *testing.T) {
	want := map[Code]error{
		"runner_offline":     runner.ErrRunnerOffline,
		"pending_on_runner":  runner.ErrPendingOnRunner,
		"sandbox_gone":       runner.ErrSandboxGone,
		"exec_unsupported":   runner.ErrExecStreamUnsupported,
		"end_unsupported":    runner.ErrEndUnsupported,
		"revive_unsupported": runner.ErrReviveUnsupported,
		"never_started":      runner.ErrExecNeverStarted,
	}
	if len(codeSentinels) != len(want) {
		t.Fatalf("%d sentinel-mapped codes, design lists %d", len(codeSentinels), len(want))
	}
	for code, sentinel := range want {
		wire := ErrorFor(fmt.Errorf("wrapped: %w", sentinel))
		if wire.Code != code {
			t.Errorf("%v maps to code %q, want %q", sentinel, wire.Code, code)
		}
		// Through the wire and back: errors.Is works on the org side.
		b, err := json.Marshal(Reply{Error: wire})
		if err != nil {
			t.Fatal(err)
		}
		var rep Reply
		if err := json.Unmarshal(b, &rep); err != nil {
			t.Fatal(err)
		}
		if got := rep.Error.Err(); !errors.Is(got, sentinel) {
			t.Errorf("code %q came back as %v, errors.Is(%v) is false", code, got, sentinel)
		}
		for other, os := range want {
			if other != code && errors.Is(rep.Error.Err(), os) {
				t.Errorf("code %q also matches %v", code, os)
			}
		}
	}
}

func TestRefusedAndInternalHaveNoSentinel(t *testing.T) {
	for _, e := range []*Error{Refuse("owner mismatch"), ErrorFor(errors.New("boom"))} {
		for _, cs := range codeSentinels {
			if errors.Is(e.Err(), cs.err) {
				t.Errorf("%q matches %v", e.Code, cs.err)
			}
		}
	}
	if c := ErrorFor(errors.New("boom")).Code; c != CodeInternal {
		t.Errorf("an unclassified failure is %q, want internal", c)
	}
	if got := ErrorFor(Refuse("roots")); got.Code != CodeRefused || got.Message != "roots" {
		t.Errorf("a refusal lost its code or reason: %+v", got)
	}
	if ErrorFor(nil) != nil || (*Error)(nil).Err() != nil {
		t.Error("nil must stay nil")
	}
}

func TestUnknownCodeIsNotTrusted(t *testing.T) {
	e := &Error{Code: "made_up", Message: "x"}
	if e.Code.Valid() {
		t.Fatal("an unknown code is valid")
	}
	var we *Error
	if !errors.As(e.Err(), &we) || we.Code != CodeInternal {
		t.Fatalf("unknown code = %v, want internal", e.Err())
	}
}
