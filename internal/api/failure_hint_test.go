// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// createFailRunner fails CreateSandbox — dispatch's pre-agent failure arm. This
// is D9's exact scenario: a run that dies BEFORE the agent starts (here, an image
// that resolves control-plane-side but not on the daemon).
type createFailRunner struct {
	*fakeRunner
	err error
}

func (r *createFailRunner) CreateSandbox(context.Context, runner.SandboxSpec) (runner.Sandbox, error) {
	return runner.Sandbox{}, r.err
}

// TestDispatch_PreAgentFailure_StampsFailureHint pins D9: a dispatch failure
// BEFORE the agent starts must leave a non-empty FailureHint on the persisted
// run, not just a reason-less FAILED badge. Without the failAndRevoke hint stamp
// the run goes FAILED with an empty hint and the reason lives only in an audit row.
func TestDispatch_PreAgentFailure_StampsFailureHint(t *testing.T) {
	rn := &createFailRunner{fakeRunner: &fakeRunner{}, err: errors.New("no such image: agent-claude-code:0.6.0")}
	srv, st, audit, run := dispatchTeardownFixture(t, rn, types.RunPending)

	srv.dispatchRun(context.Background(), run, ceilingForDispatch(governanceCeiling{}, adoEntraUngraded(), bedrockCredUngraded()), dispatchParams{
		RunToken: "run-token", Image: "wardyn/claude-code:latest",
		Policy: types.RunPolicySpec{MinConfinementClass: types.CC1},
	})

	if got := st.State(); got != types.RunFailed {
		t.Fatalf("a pre-agent CreateSandbox failure must land the run FAILED, got %q", got)
	}
	hint := st.FailureHint()
	if hint == "" {
		t.Fatal("a pre-agent dispatch failure must stamp a non-empty FailureHint on the persisted run (D9)")
	}
	if !strings.Contains(hint, "no such image") {
		t.Errorf("the failure hint must carry the real reason, got %q", hint)
	}
	// The console grades the ending by this stamped reason, never by the message.
	ev := findAudit(audit.events, run.ID, "run.create", "failure")
	if ev == nil || !strings.Contains(string(ev.Data), `"reason":"sandbox_create"`) {
		t.Errorf("run.create/failure must carry reason sandbox_create, got %v", ev)
	}
}

// A runner's image_arch refusal reaches the person as its own message, the audit reason says
// image_arch, and the hint says where an administrator fixes it; the wire prefix is not shown.
func TestDispatch_ImageArchRefusal_ReasonAndHint(t *testing.T) {
	msg := "this runner's engine is arm64 and cannot run m/agent:1, a single-architecture amd64 image (exec format error). Name an image for arm64 or publish a multi-arch one"
	wire := runnerwire.ErrorFor(fmt.Errorf("%w", runner.ErrImageArch))
	wire.Message = msg
	rn := &createFailRunner{fakeRunner: &fakeRunner{}, err: wire.Err()}
	srv, st, audit, run := dispatchTeardownFixture(t, rn, types.RunPending)

	srv.dispatchRun(context.Background(), run, ceilingForDispatch(governanceCeiling{}, adoEntraUngraded(), bedrockCredUngraded()), dispatchParams{
		RunToken: "run-token", Image: "m/agent:1",
		Policy: types.RunPolicySpec{MinConfinementClass: types.CC1},
	})

	hint := st.FailureHint()
	if !strings.Contains(hint, msg) || !strings.Contains(hint, `"<key>@<arch>"`) || strings.Contains(hint, "runner: ") {
		t.Errorf("hint %q: want the runner's message, the <key>@<arch> sentence, no wire prefix", hint)
	}
	ev := findAudit(audit.events, run.ID, "run.create", "failure")
	if ev == nil || !strings.Contains(string(ev.Data), `"reason":"image_arch"`) {
		t.Errorf("run.create/failure must carry reason image_arch, got %v", ev)
	}
}
