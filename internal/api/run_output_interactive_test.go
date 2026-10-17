// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/recording"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const (
	interactiveRecordingSentence = "an interactive run keeps no output here: its terminal is the recording's to keep"
	interactiveNotStoppedOff     = "Nothing was kept from this interactive session: it did not end through a Wardyn stop, and recording is off on this deployment"
	interactiveNothingKeptOff    = "Nothing is kept from this interactive session, and recording is off on this deployment"
)

// TestRunOutput_InteractiveSentenceRecordingOff: with no recording store, an
// interactive run with no snapshot never points at a recording. Only a run
// that ended other than STOPPED is said not to have ended through a Wardyn stop.
func TestRunOutput_InteractiveSentenceRecordingOff(t *testing.T) {
	srv, _, seed := runOutputServer(t)
	for state, want := range map[types.RunState]string{
		types.RunKilled:    interactiveNotStoppedOff,
		types.RunFailed:    interactiveNotStoppedOff,
		types.RunCompleted: interactiveNotStoppedOff,
		types.RunStopped:   interactiveNothingKeptOff,
		types.RunRunning:   interactiveNothingKeptOff,
	} {
		id := seed(types.AgentRun{Interactive: true, State: state})
		code, _, refusal := getRunOutput(t, srv, id, "", outputOwnerCookie(t))
		if code != http.StatusConflict || refusal.Reason != reasonRunOutputInteractive || refusal.Error != want {
			t.Errorf("%s: %d %q %q, want 409 %s %q", state, code, refusal.Reason, refusal.Error, reasonRunOutputInteractive, want)
		}
	}
}

// TestRunOutput_InteractiveSentenceRecordingOn: with a recording store, the
// sentence keeps pointing at the recording.
func TestRunOutput_InteractiveSentenceRecordingOn(t *testing.T) {
	rec, err := recording.NewFSStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFSStore: %v", err)
	}
	srv, _, seed := runOutputServer(t, func(c *Config) { c.RecordingStore = rec })
	id := seed(types.AgentRun{Interactive: true, State: types.RunKilled})
	code, _, refusal := getRunOutput(t, srv, id, "", outputOwnerCookie(t))
	if code != http.StatusConflict || refusal.Reason != reasonRunOutputInteractive || refusal.Error != interactiveRecordingSentence {
		t.Fatalf("recording on: %d %q %q, want 409 %s %q", code, refusal.Reason, refusal.Error, reasonRunOutputInteractive, interactiveRecordingSentence)
	}
}

// TestRunOutput_InteractiveSentenceHidesSnapshot: a reader who may not see a
// pane snapshot gets the recording sentence for a run with no row too, so the
// sentence never tells them whether a snapshot exists.
func TestRunOutput_InteractiveSentenceHidesSnapshot(t *testing.T) {
	srv, _, seed := runOutputServer(t)
	id := seed(types.AgentRun{Interactive: true, State: types.RunKilled})
	code, _, refusal := getRunOutput(t, srv, id, "", ssoSession(t, secAdminSub, secAdminMail, oidc.RoleSecurityAdmin))
	if code != http.StatusConflict || refusal.Reason != reasonRunOutputInteractive || refusal.Error != interactiveRecordingSentence {
		t.Fatalf("security admin: %d %q %q, want 409 %s %q", code, refusal.Reason, refusal.Error, reasonRunOutputInteractive, interactiveRecordingSentence)
	}
}
