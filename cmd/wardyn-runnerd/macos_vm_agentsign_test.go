// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
)

// macos-vm status says how the base's guest agent is signed. Only an agent signed ad hoc loses Full Disk
// Access at each tools update; one signed on this Mac whose certificate the base does not trust names
// edit-base; Full Disk Access as the base last read it is shown.
func TestStatusSaysTheAgentIsAdHoc(t *testing.T) {
	status := func(rec string) string {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, macvm.ToolsRecordFile), []byte(rec), 0o600); err != nil {
			t.Fatal(err)
		}
		st := macOSVMStatus{State: runnerwire.MacVMBaseReady, Harnesses: []string{"none"}, withheld: selftestWithheld(dir)}
		var out strings.Builder
		st.writeText(&out)
		return out.String()
	}
	const lost = "Full Disk Access is lost at each tools update"
	if got := status(`{"agent_signature":"ad_hoc"}`); !strings.Contains(got, "guest agent: signed ad hoc; "+lost) {
		t.Fatalf("ad hoc status:\n%s", got)
	}
	got := status(`{"agent_signature":"this_mac","agent_signing_cert":"trusted","guest":{"full_disk_access":"allowed"}}`)
	if strings.Contains(got, lost) || !strings.Contains(got, "guest agent: signed on this Mac\n") ||
		!strings.Contains(got, "Full Disk Access: allowed (as the base last read it)\n") {
		t.Fatalf("signed status:\n%s", got)
	}
	got = status(`{"agent_signature":"this_mac","agent_signing_cert":"not_trusted"}`)
	if strings.Contains(got, lost) || !strings.Contains(got, "guest agent: signed on this Mac; the base does not trust its certificate yet: run macos-vm edit-base and trust it\n") {
		t.Fatalf("untrusted certificate status:\n%s", got)
	}
	// A tools update that could not carry the reading across says so, never allowed.
	if got := status(`{"agent_signature":"this_mac","agent_signing_cert":"trusted"}`); !strings.Contains(got, "Full Disk Access: unknown; recheck it in macos-vm edit-base\n") {
		t.Fatalf("unread Full Disk Access status:\n%s", got)
	}
	if got := status(`{}`); strings.Contains(got, "guest agent:") || strings.Contains(got, "Full Disk Access:") {
		t.Fatalf("a record from before the signature says something:\n%s", got)
	}
}
