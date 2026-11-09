// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package main

import (
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/doctor"
	"github.com/cjohnstoniv/wardyn/internal/macvm"
)

// guestAgentStage says how the last tools update signed the base's guest agent (rec is the base's tools.json).
// It fails when that update pushed the agent signed ad hoc, which loses Full Disk Access at each update, and
// quotes why each signer failed. It passes when this Mac's cert signed it, by Wardyn's signer or by codesign.
// The base does not trust that cert: Full Disk Access keeps to the designated requirement. warnings are lines
// that do not fail the stage (guestAgentWarnings).
func guestAgentStage(rec macvm.ToolsRecord, warnings []string) doctor.Stage {
	const name = "guest-agent"
	warn := strings.Join(append([]string{""}, warnings...), "; warning: ")
	switch rec.AgentSignature {
	case "":
		return doctor.SkipStage(name, "no tools update has recorded how the guest agent is signed"+warn)
	case macvm.AgentSignedAdHoc:
		detail := "the last tools update pushed the guest agent signed ad hoc, so macOS drops its Full Disk Access at each tools update"
		if len(rec.AgentSignErrors) > 0 {
			detail += ". Why each signer failed: " + strings.Join(rec.AgentSignErrors, "; ")
		}
		return doctor.FailStage(name, detail+warn, "", "the next tools update signs it again; if it fails the same way, send this output and tools-update.log")
	}
	detail := "signed with this Mac's cert"
	switch rec.AgentSigner {
	case macvm.AgentSignerWardyn:
		detail += " (by Wardyn's signer)"
	case macvm.AgentSignerCodesign:
		detail += " (by codesign)"
	}
	return doctor.OKStage(name, detail+warn)
}

// guestAgentWarnings are the guest-agent stage's warning lines, read from the base's tools.json. Each further
// source of a warning adds its lines here.
func guestAgentWarnings(rec macvm.ToolsRecord) []string {
	return BaseSettingsWarnings(rec.BaseSettingsSkipped)
}
