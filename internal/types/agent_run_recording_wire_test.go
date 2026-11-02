// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"encoding/json"
	"strings"
	"testing"
)

// A run whose recording fields were never projected (a list that did not ask for
// include=recording_meta) must leave the keys off the wire: has_recording:false would read
// as "no recording" for a run that has one. A projected run says false explicitly.
func TestAgentRunHasRecordingWire(t *testing.T) {
	b, err := json.Marshal(AgentRun{})
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"has_recording", "recording_bytes", "recording_duration_sec"} {
		if strings.Contains(string(b), `"`+k+`"`) {
			t.Errorf("unprojected run carries %q: %s", k, b)
		}
	}
	none := false
	b, err = json.Marshal(AgentRun{HasRecording: &none})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"has_recording":false`) {
		t.Errorf("projected run without a recording must say has_recording:false: %s", b)
	}
}
