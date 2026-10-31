// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerwire

import (
	"errors"
	"fmt"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// Code is the closed set of REPLY error codes. Each code that has a sentinel
// maps to it, so errors.Is works on the org side of the wire.
type Code string

const (
	CodeRunnerOffline      Code = "runner_offline"
	CodePendingOnRunner    Code = "pending_on_runner"
	CodeSandboxGone        Code = "sandbox_gone"
	CodeExecUnsupported    Code = "exec_unsupported"
	CodeEndUnsupported     Code = "end_unsupported"
	CodeReviveUnsupported  Code = "revive_unsupported"
	CodeNeverStarted       Code = "never_started"
	CodeImageArch          Code = "image_arch"
	CodeRelayIPFenced      Code = "relay_ip_fenced"
	CodeObserveUnsupported Code = "observe_unsupported"
	CodeRefused            Code = "refused"
	CodeInternal           Code = "internal"
)

var codeSentinels = []struct {
	code Code
	err  error
}{
	{CodeRunnerOffline, runner.ErrRunnerOffline},
	{CodePendingOnRunner, runner.ErrPendingOnRunner},
	{CodeSandboxGone, runner.ErrSandboxGone},
	{CodeExecUnsupported, runner.ErrExecStreamUnsupported},
	{CodeEndUnsupported, runner.ErrEndUnsupported},
	{CodeReviveUnsupported, runner.ErrReviveUnsupported},
	{CodeNeverStarted, runner.ErrExecNeverStarted},
	{CodeImageArch, runner.ErrImageArch},
	{CodeRelayIPFenced, runner.ErrRelayIPFenced},
	{CodeObserveUnsupported, runner.ErrObserveUnsupported},
}

// Valid reports whether c is in the closed list.
func (c Code) Valid() bool {
	if c == CodeRefused || c == CodeInternal {
		return true
	}
	for _, cs := range codeSentinels {
		if cs.code == c {
			return true
		}
	}
	return false
}

// Error is a REPLY's typed error. For a code with a sentinel, errors.Is(err,
// sentinel) is true; refused and internal have none.
type Error struct {
	Code    Code   `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string {
	if e.Message == "" {
		return "runner: " + string(e.Code)
	}
	return fmt.Sprintf("runner: %s: %s", e.Code, e.Message)
}

// Is makes errors.Is(err, runner.ErrX) true for the code's sentinel.
func (e *Error) Is(target error) bool {
	for _, cs := range codeSentinels {
		if cs.code == e.Code {
			return target == cs.err
		}
	}
	return false
}

// ErrorFor is the runner side: it turns err into the wire error. A sentinel
// anywhere in the chain picks its code; a *Error keeps its own; anything else
// is internal. nil stays nil.
func ErrorFor(err error) *Error {
	if err == nil {
		return nil
	}
	var we *Error
	if errors.As(err, &we) && we.Code.Valid() {
		return &Error{Code: we.Code, Message: we.Message}
	}
	for _, cs := range codeSentinels {
		if errors.Is(err, cs.err) {
			return &Error{Code: cs.code, Message: err.Error()}
		}
	}
	return &Error{Code: CodeInternal, Message: err.Error()}
}

// Refuse is the runner-side refusal (owner, roots, capacity), carried with its reason.
func Refuse(format string, a ...any) *Error {
	return &Error{Code: CodeRefused, Message: fmt.Sprintf(format, a...)}
}

// Err is the org side: the error a received REPLY stands for. A code outside
// the closed list is not trusted as one: it becomes internal.
func (e *Error) Err() error {
	if e == nil {
		return nil
	}
	if !e.Code.Valid() {
		return &Error{Code: CodeInternal, Message: fmt.Sprintf("unknown error code %q: %s", e.Code, e.Message)}
	}
	return e
}

// EventRefusal is the organisation's verdict that an EVENT can never be accepted: the same bytes would get the same
// answer, and the refusal is already recorded durably. The org peer acknowledges the frame and keeps the session;
// any other error from a checked handler still ends the link, so a transient failure is replayed (DR-0436, #3648).
type EventRefusal struct {
	Code string // one of the Refusal* codes
	Err  error  // the ingest's own sentinel, for logs
}

func (e *EventRefusal) Error() string {
	return fmt.Sprintf("runnerwire: event refused (%s): %v", e.Code, e.Err)
}
func (e *EventRefusal) Unwrap() error { return e.Err }

// Refusal codes: the closed set an audit row's `code` carries.
const (
	RefusalShape           = "shape"            // undecodable, unknown field, trailing data, size, RunID presence
	RefusalUnsupportedKind = "unsupported_kind" // a kind this organisation has no ingestion for
	RefusalStoreRefused    = "store_refused"    // ownership, generation, hash, run state, grant, or a changed repeat
	RefusalUnknownRun      = "unknown_run"      // the run (or its execution) no longer exists
)
