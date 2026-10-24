// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package remote_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire/runnertest"
)

// "Pause the agent to sign in" (#2972) reaches the runner as pause_agent and resume_agent only when
// its caps offer VM sign-ins: an older runner is asked nothing, and the org answers that it cannot.
func TestPauseAgentNeedsTheRunnersSignInsCaps(t *testing.T) {
	for _, signIns := range []bool{false, true} {
		rig := runnertest.NewRig(t, rid)
		advertiseVM(t, rig, &runnerwire.VMCaps{MachineType: "pc-q35-10.0", CPUModel: "Haswell-v4", QEMUVersion: "10.0.13", Desktop: true, SignIns: signIns})
		sb, _ := create(t, rig, nil)
		calls := len(rig.Fake.Calls)
		perr, rerr := rig.Sub.PauseAgent(ctxT(t), sb.Ref), rig.Sub.ResumeAgent(ctxT(t), sb.Ref)
		got := rig.Fake.Calls[calls:]
		if !signIns {
			if !errors.Is(perr, runner.ErrAgentPauseUnsupported) || !errors.Is(rerr, runner.ErrAgentPauseUnsupported) || len(got) != 0 {
				t.Fatalf("without the caps: pause %v, resume %v, calls %v; want refused before the runner", perr, rerr, got)
			}
			continue
		}
		verbs := []string{}
		for _, c := range got {
			verbs = append(verbs, strings.Fields(c)[0])
		}
		if perr != nil || rerr != nil || !slices.Equal(verbs, []string{"pause_agent", "resume_agent"}) {
			t.Fatalf("with the caps: pause %v, resume %v, calls %v", perr, rerr, got)
		}
	}
}
