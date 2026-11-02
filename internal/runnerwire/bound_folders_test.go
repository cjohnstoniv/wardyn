// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerwire

import (
	"encoding/json"
	"testing"
)

// A runner that binds folders bound to it says so, and an older runner, which never sends the field,
// reads as one that does not: the org then places no such folder on it.
func TestBoundFoldersCapRoundTrip(t *testing.T) {
	raw, err := json.Marshal(Caps{Version: "0.10", BoundFolders: true})
	if err != nil {
		t.Fatal(err)
	}
	var back Caps
	if err := Unmarshal(raw, &back); err != nil || !back.BoundFolders {
		t.Fatalf("round trip = %+v, %v; want bound_folders", back, err)
	}
	var old Caps
	if err := Unmarshal([]byte(`{"version":"0.10.0-rc.8","linux_vm":{}}`), &old); err != nil || old.BoundFolders {
		t.Fatalf("an older runner's caps read as binding folders: %+v, %v", old, err)
	}
	off, _ := json.Marshal(Caps{Version: "x"})
	if containsKey(off, "bound_folders") {
		t.Fatalf("an unset bound_folders is sent: %s", off)
	}
}
