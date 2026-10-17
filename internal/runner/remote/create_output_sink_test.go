// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package remote

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/substrate"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// createFailTransport answers every call with err.
type createFailTransport struct{ err error }

func (*createFailTransport) Online() bool       { return true }
func (*createFailTransport) SetHandler(Handler) {}
func (*createFailTransport) TakeStream(uint32) (*runnerwire.Stream, error) {
	return nil, runnerwire.ErrNoStream
}
func (t *createFailTransport) Call(context.Context, string, any, any, ...runnerwire.CallOption) error {
	return t.err
}

// A create the runner itself refused leaves no output sink behind; a create whose reply was lost keeps
// it, because the runner may have made the sandbox and a retry resumes the tail.
func TestCreateSandboxDropsTheOutputSinkOnlyOnARefusal(t *testing.T) {
	for name, c := range map[string]struct {
		err  error
		kept bool
	}{
		"refused":       {&runnerwire.Error{Code: runnerwire.CodeRefused, Message: "no"}, false},
		"wrong arch":    {&runnerwire.Error{Code: runnerwire.CodeImageArch}, false},
		"link dropped":  {runner.ErrRunnerOffline, true},
		"internal fail": {&runnerwire.Error{Code: runnerwire.CodeInternal}, true},
	} {
		t.Run(name, func(t *testing.T) {
			s := New("r1", &createFailTransport{err: c.err}, Options{})
			s.caps = &runnerwire.Caps{Support: substrate.ClassSupport{Classes: []types.ConfinementClass{types.CC1}}}
			spec := runner.SandboxSpec{RunID: uuid.New(), Image: "img", ConfinementClass: types.CC1, ExecOutput: io.Discard}
			if _, err := s.CreateSandbox(context.Background(), spec); err == nil {
				t.Fatal("the create succeeded")
			}
			s.mu.Lock()
			_, held := s.outputs[spec.RunID]
			s.mu.Unlock()
			if held != c.kept {
				t.Errorf("output sink held = %v, want %v", held, c.kept)
			}
		})
	}
}

// recoverTransport answers recover_output with an unrecoverable result and lets the test look at the
// substrate at the moment the runner is asked.
type recoverTransport struct{ atCall func() }

func (*recoverTransport) Online() bool       { return true }
func (*recoverTransport) SetHandler(Handler) {}
func (*recoverTransport) TakeStream(uint32) (*runnerwire.Stream, error) {
	return nil, runnerwire.ErrNoStream
}
func (t *recoverTransport) Call(_ context.Context, _ string, _, result any, _ ...runnerwire.CallOption) error {
	t.atCall()
	if r, ok := result.(*runnerwire.RecoverResult); ok {
		r.Unrecoverable = true
	}
	return nil
}

// An adopted run's output sink exists before the runner is asked to recover, so the OPEN the runner
// sends at connect finds it; when the runner answers unrecoverable the sink is not left behind.
func TestRecoverOutputRegistersTheSinkBeforeAskingTheRunner(t *testing.T) {
	run := uuid.New()
	held := func(s *Substrate) bool { s.mu.Lock(); defer s.mu.Unlock(); return s.outputs[run] != nil }
	var s *Substrate
	var atCall bool
	s = New("r1", &recoverTransport{atCall: func() { atCall = held(s) }}, Options{})
	ref := "runner:r1/local"
	err := s.RecoverOutput(runner.WithOutputRun(context.Background(), run), ref, io.Discard)
	if !errors.Is(err, runner.ErrOutputUnrecoverable) {
		t.Fatalf("RecoverOutput = %v, want unrecoverable", err)
	}
	if !atCall {
		t.Error("the sink was not registered when the runner was asked")
	}
	if held(s) {
		t.Error("an unrecoverable answer left the sink behind")
	}
}
