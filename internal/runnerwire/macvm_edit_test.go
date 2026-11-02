// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerwire

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

// A base's last edit and its recheck ride in the caps as edited_at (RFC 3339) and edit_recheck, and a base
// never edited carries neither.
func TestMacVMBaseEditFieldsEncode(t *testing.T) {
	at := time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)
	c := MacVMCaps{Max: 1, Base: MacVMBase{State: MacVMBaseReady, EditedAt: &at, EditRecheck: MacVMEditRecheckPassed}}
	b, err := Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if want := `"base":{"state":"ready","edited_at":"2026-11-03T09:00:00Z","edit_recheck":"passed"}`; !strings.Contains(string(b), want) {
		t.Fatalf("caps %s, want %s", b, want)
	}
	var back MacVMCaps
	if err := Unmarshal(b, &back); err != nil || !reflect.DeepEqual(back, c) {
		t.Fatalf("round trip %+v %v", back, err)
	}
	b, _ = Marshal(MacVMCaps{Base: MacVMBase{State: MacVMBaseReady}})
	if strings.Contains(string(b), "edit") {
		t.Fatalf("an unedited base's caps carry an edit: %s", b)
	}
}
