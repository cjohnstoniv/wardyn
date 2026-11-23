// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The SSH door keeps its fixed error for a background run and says a Windows VM run's own sentence.
func TestSSHDoorRefusalErr(t *testing.T) {
	bg := backgroundOnlyRefusal(types.AgentRun{Experience: types.ExperienceBackground}, "SSH access")
	if bg == nil || bg.body.Reason != reasonRunBackgroundOnly {
		t.Fatalf("background run: refusal %+v, want %s", bg, reasonRunBackgroundOnly)
	}
	if err := sshDoorRefusalErr(bg); err != errSSHBackgroundOnly {
		t.Errorf("background run: %v, want %v", err, errSSHBackgroundOnly)
	}
	win := runError(http.StatusConflict, reasonWindowsVMFeatureRefused, windowsDeskOnlyMsg("SSH access"))
	if got, want := sshDoorRefusalErr(win).Error(), "ssh: A Windows VM run is desktop-only: no SSH access."; got != want {
		t.Errorf("Windows VM run: %q, want %q", got, want)
	}
}
