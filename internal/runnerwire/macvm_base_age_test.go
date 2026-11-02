// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerwire

import (
	"encoding/json"
	"strings"
	"testing"
)

// The base's age rides the caps as created_at and build_version, and an older runner's base, which has
// neither, decodes to empty and encodes without them.
func TestMacVMBaseAgeFieldsAreAdditive(t *testing.T) {
	raw, err := json.Marshal(MacVMBase{State: MacVMBaseReady, CreatedAt: "2026-10-28T07:30:00Z", BuildVersion: "25G83"})
	if err != nil || !strings.Contains(string(raw), `"created_at":"2026-10-28T07:30:00Z"`) || !strings.Contains(string(raw), `"build_version":"25G83"`) {
		t.Fatalf("encoded %s, %v", raw, err)
	}
	var old MacVMBase
	if err := json.Unmarshal([]byte(`{"state":"ready","guest_os":"macOS 26.1"}`), &old); err != nil || old.CreatedAt != "" || old.BuildVersion != "" {
		t.Fatalf("an older runner's base = %+v, %v", old, err)
	}
	if raw, _ = json.Marshal(old); strings.Contains(string(raw), "created_at") || strings.Contains(string(raw), "build_version") {
		t.Fatalf("a base with no age encoded %s", raw)
	}
}
