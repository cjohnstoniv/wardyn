// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/doctor"
	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
)

// guest-agent fails when the last tools update pushed the agent signed ad hoc, quoting why each signer failed,
// and passes when this Mac's cert signed it, by either signer. Whether the base trusts the cert is shown as
// information and never decides the stage. Warning lines never fail it. A record from before the signature
// skips.
func TestGuestAgentStage(t *testing.T) {
	errs := []string{"wardyn: machosign: not a thin 64-bit Mach-O executable", "codesign: macvm: signing the guest agent: codesign --force: exit status 1: ABC: no identity found"}
	for _, tc := range []struct {
		name     string
		rec      macvm.ToolsRecord
		warnings []string
		status   doctor.Status
		want     []string
		not      []string
	}{
		{"ad hoc", macvm.ToolsRecord{AgentSignature: "ad_hoc", AgentSignRecord: macvm.AgentSignRecord{AgentSigner: "ad_hoc", AgentSignErrors: errs}}, nil,
			doctor.StatusFail, []string{"signed ad hoc", "Full Disk Access", errs[0], errs[1], "tools-update.log"}, nil},
		{"ad hoc, no errors recorded", macvm.ToolsRecord{AgentSignature: "ad_hoc"}, nil, doctor.StatusFail, []string{"signed ad hoc"}, []string{"Why each signer failed"}},
		{"wardyn", macvm.ToolsRecord{AgentSignature: "this_mac", AgentSignRecord: macvm.AgentSignRecord{AgentSigner: "wardyn"}}, nil,
			doctor.StatusOK, []string{"signed with this Mac's cert (by Wardyn's signer)"}, []string{"warning", "trust"}},
		{"codesign after wardyn failed", macvm.ToolsRecord{AgentSignature: "this_mac", AgentSignRecord: macvm.AgentSignRecord{AgentSigner: "codesign", AgentSignErrors: errs[:1]}}, nil,
			doctor.StatusOK, []string{"signed with this Mac's cert (by codesign)"}, []string{"trust"}},
		{"signed before the signer was recorded", macvm.ToolsRecord{AgentSignature: "this_mac"}, nil, doctor.StatusOK, []string{"signed with this Mac's cert"}, []string{"(by"}},
		{"signed, with a warning", macvm.ToolsRecord{AgentSignature: "this_mac", AgentSignRecord: macvm.AgentSignRecord{AgentSigner: "wardyn"}}, []string{"a base setting was skipped"},
			doctor.StatusOK, []string{"signed with this Mac's cert", "; warning: a base setting was skipped"}, nil},
		{"ad hoc, with a warning", macvm.ToolsRecord{AgentSignature: "ad_hoc"}, []string{"a base setting was skipped"},
			doctor.StatusFail, []string{"signed ad hoc", "; warning: a base setting was skipped"}, nil},
		{"no record", macvm.ToolsRecord{}, nil, doctor.StatusSkip, []string{"no tools update has recorded"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := guestAgentStage(tc.rec, tc.warnings)
			msg := st.Message()
			if st.Name != "guest-agent" || st.Status != tc.status {
				t.Fatalf("stage %q status %q, want guest-agent %q: %s", st.Name, st.Status, tc.status, msg)
			}
			for _, w := range tc.want {
				if !strings.Contains(msg, w) {
					t.Fatalf("message lacks %q:\n%s", w, msg)
				}
			}
			for _, n := range tc.not {
				if strings.Contains(msg, n) {
					t.Fatalf("message has %q:\n%s", n, msg)
				}
			}
		})
	}
}

// doctor runs the guest-agent stage on the base's tools.json, before folder-shares.
func TestMacOSVMDoctorRunsTheGuestAgentStage(t *testing.T) {
	st := macOSVMStagesFor(t, nil, macvm.ToolsRecord{AgentSignature: "ad_hoc", AgentSignRecord: macvm.AgentSignRecord{AgentSigner: "ad_hoc", AgentSignErrors: []string{"wardyn: x"}}}, nil)
	if len(st) != 2 || st[0].Name != "guest-agent" || st[0].Status != doctor.StatusFail || !strings.Contains(st[0].Detail, "wardyn: x") || st[1].Name != "folder-shares" {
		t.Fatalf("stages %+v", st)
	}
	if (doctor.Report{Stages: st}).OK() {
		t.Fatal("an ad hoc agent did not fail doctor")
	}
}

// macos-vm status names the signer the last tools update used and why each signer before it failed.
func TestStatusNamesTheSignerAndItsErrors(t *testing.T) {
	dir := t.TempDir()
	rec := `{"agent_signature":"this_mac","agent_signing_cert":"trusted","agent_signer":"codesign","agent_sign_errors":["wardyn: machosign: no LC_CODE_SIGNATURE"]}`
	if err := os.WriteFile(filepath.Join(dir, macvm.ToolsRecordFile), []byte(rec), 0o600); err != nil {
		t.Fatal(err)
	}
	st := macOSVMStatus{State: runnerwire.MacVMBaseReady, Harnesses: []string{"none"}, withheld: selftestWithheld(dir)}
	var out strings.Builder
	st.writeText(&out)
	for _, want := range []string{"guest agent: signed on this Mac\n", "guest agent signer: codesign\n", "guest agent signing errors: wardyn: machosign: no LC_CODE_SIGNATURE\n"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("status lacks %q:\n%s", want, out.String())
		}
	}
}
