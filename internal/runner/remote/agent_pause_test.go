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
		_, perr := rig.Sub.PauseAgent(ctxT(t), sb.Ref, 1)
		_, rerr := rig.Sub.ResumeAgent(ctxT(t), sb.Ref, 1)
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

// The pause's generation reaches the runner, and the guest agent's fenced_pause comes back. A runner
// or guest image that predates the generation answers nothing, which reads as unfenced and is not
// refused: the pause and resume go through as before.
func TestPauseAgentCarriesTheGenerationAndTheGuestsAnswer(t *testing.T) {
	for _, fenced := range []bool{false, true} {
		rig := runnertest.NewRig(t, rid)
		advertiseVM(t, rig, &runnerwire.VMCaps{MachineType: "pc-q35-10.0", CPUModel: "Haswell-v4", QEMUVersion: "10.0.13", Desktop: true, SignIns: true})
		rig.Fake.FencedPause = fenced
		sb, _ := create(t, rig, nil)
		pf, perr := rig.Sub.PauseAgent(ctxT(t), sb.Ref, 7)
		rf, rerr := rig.Sub.ResumeAgent(ctxT(t), sb.Ref, 7)
		if perr != nil || rerr != nil || pf != fenced || rf != fenced {
			t.Fatalf("guest fenced %v: pause %v %v, resume %v %v", fenced, pf, perr, rf, rerr)
		}
		if !slices.Equal(rig.Fake.PauseGens, []int64{7, 7}) {
			t.Fatalf("generations the runner got = %v, want 7 and 7", rig.Fake.PauseGens)
		}
	}
}
