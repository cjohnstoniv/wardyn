// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A dispatch's force view only comes from the run's stored mode alone: not from Interactive, so a
// background run forced as interactive at the door stays forced and an interactive or older run never is.
func TestDispatchDesktopViewOnlyFollowsTheStoredMode(t *testing.T) {
	for _, c := range []struct {
		experience  types.RunExperience
		interactive bool
		want        bool
	}{
		{types.ExperienceBackground, false, true},
		{types.ExperienceBackground, true, true},
		{types.ExperienceInteractive, false, false},
		{types.ExperienceInteractive, true, false},
		{"", false, false},
		{"", true, false},
	} {
		fr := &fakeRunner{}
		srv, _, _, run := dispatchTeardownFixture(t, fr, types.RunPending)
		run.Task = ""
		run.Experience = c.experience
		srv.dispatchRun(context.Background(), run, ceilingForDispatch(governanceCeiling{}, adoEntraUngraded(), bedrockCredUngraded()), dispatchParams{
			RunToken: "run-token", Image: "wardyn/claude-code:latest", Interactive: c.interactive,
		})
		if got := fr.lastSpec.DesktopViewOnly; got != c.want {
			t.Errorf("experience %q Interactive=%v: dispatched DesktopViewOnly=%v, want %v", c.experience, c.interactive, got, c.want)
		}
	}
}

// What a boot's helper said is recorded with the machine, each boot replacing the last: a later boot
// that doesn't report (a revive without the setting, an older helper) or says false reads as not forced,
// an interactive or Linux run is never forced, and a runner can't set it through Machine.
func TestRecordVMMachineKeepsThisBootsForcedViewOnly(t *testing.T) {
	st := vmMachineStore{ownedRunners: ownedRunners{rows: map[uuid.UUID]types.Runner{}}, got: map[uuid.UUID]types.VMMachine{}}
	s := &Server{cfg: Config{Store: st}}
	mac := types.VMMachine{MachineType: types.MacVMMachineType, Accelerator: types.MacVMAccelerator, GuestOS: "macOS 27.0.1 (26A434)", HostOS: "macOS 27.0 (26A400)"}
	claims := mac
	claims.DesktopForcedViewOnly = true
	background := types.AgentRun{ID: uuid.New(), MachineKind: types.MachineMacOSVM, Experience: types.ExperienceBackground}
	interactive := background
	interactive.ID, interactive.Experience = uuid.New(), types.ExperienceInteractive

	for _, c := range []struct {
		name string
		run  types.AgentRun
		sb   runner.Sandbox
		want bool
	}{
		{"a forced boot", background, runner.Sandbox{Machine: &mac, DesktopForcedViewOnly: true}, true},
		{"a later boot without the setting", background, runner.Sandbox{Machine: &mac}, false},
		{"forced again", background, runner.Sandbox{Machine: &mac, DesktopForcedViewOnly: true}, true},
		{"a runner claiming it through Machine only", background, runner.Sandbox{Machine: &claims}, false},
		{"an interactive run a helper says is forced", interactive, runner.Sandbox{Machine: &mac, DesktopForcedViewOnly: true}, false},
	} {
		if err := s.recordVMMachine(context.Background(), c.run, c.sb); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		got := st.got[c.run.ID]
		if got.DesktopForcedViewOnly != c.want {
			t.Errorf("%s: stored %v, want %v", c.name, got.DesktopForcedViewOnly, c.want)
		}
		c.run.VMMachine = &got
		if c.run.DesktopForcedViewOnly() != c.want {
			t.Errorf("%s: the run reads %v, want %v", c.name, c.run.DesktopForcedViewOnly(), c.want)
		}
		// The ask is the dispatch's own, whatever the runner answered: a background macOS run's boot always has it.
		if wantAsked := c.run.MachineKind.IsMacOSVM() && c.run.BackgroundOnly(); got.DesktopViewOnlyAsked != wantAsked {
			t.Errorf("%s: asked %v, want %v", c.name, got.DesktopViewOnlyAsked, wantAsked)
		}
	}
	if !claims.DesktopForcedViewOnly || mac.DesktopForcedViewOnly {
		t.Fatal("recording changed the Sandbox's own machine")
	}
}
