// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package composer_test

import (
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func presidioInspection() *types.LLMInspectionSpec {
	return &types.LLMInspectionSpec{
		Mode: "block", DetectorSidecarURL: "https://detector.corp.example/analyze",
		DetectorSidecarFormat: types.DetectorSidecarFormatPresidio, BlockMinSeverity: "medium",
	}
}

// The detector format rides with the ceiling's inspection through Clamp: a proposal can neither
// keep its own format nor drop the ceiling's.
func TestClampKeepsTheCeilingsDetectorSidecarFormat(t *testing.T) {
	ceiling := types.RunPolicySpec{MinConfinementClass: types.CC2, AllowedDomains: []string{"api.anthropic.com"}, LLMInspection: presidioInspection()}
	for name, proposal := range map[string]*types.LLMInspectionSpec{
		"none":           nil,
		"its own format": {Mode: "block", DetectSecrets: true, DetectorSidecarURL: "https://detector.corp.example/analyze"},
	} {
		got, _ := composer.Clamp(types.RunPolicySpec{AllowedDomains: []string{"api.anthropic.com"}, LLMInspection: proposal}, ceiling, types.GovernanceLimits{})
		if got.LLMInspection == nil || got.LLMInspection.DetectorSidecarFormat != types.DetectorSidecarFormatPresidio {
			t.Errorf("proposal %s: llm_inspection = %+v, want the ceiling's presidio format", name, got.LLMInspection)
		}
	}
}

// Two inspections that differ only in format are not comparable, so a composition that swaps the
// ceiling's format is never proven within it.
func TestLeqRefusesADifferentDetectorSidecarFormat(t *testing.T) {
	withFormat := func(format string) composer.Authority {
		li := presidioInspection()
		li.DetectorSidecarFormat = format
		return composer.Authority{Ceiling: types.RunPolicySpec{MinConfinementClass: types.CC2, LLMInspection: li}}
	}
	if !leqOf(withFormat("presidio"), withFormat("presidio")) {
		t.Fatal("Leq does not hold for an identical inspection")
	}
	if leqOf(withFormat(""), withFormat("presidio")) || leqOf(withFormat("presidio"), withFormat("")) {
		t.Error("Leq holds across a changed detector_sidecar_format")
	}
}

// An overlay that adds an inspection naming a Presidio sidecar to a base with none is held, as any
// added sidecar is.
func TestMeetRefusesAnAddedPresidioSidecar(t *testing.T) {
	base := composer.Authority{Ceiling: types.RunPolicySpec{MinConfinementClass: types.CC2}}
	got, _, err := composer.ApplyOverlay(base, composer.Overlay{Ceiling: types.CeilingOverlay{LLMInspection: presidioInspection()}})
	if err != nil {
		t.Fatalf("ApplyOverlay: %v", err)
	}
	if got.Ceiling.LLMInspection == nil || got.Ceiling.LLMInspection.DetectorSidecarFormat != types.DetectorSidecarFormatPresidio {
		t.Fatalf("the overlay's inspection = %+v, want the presidio format kept", got.Ceiling.LLMInspection)
	}
	if leqOf(got, base) {
		t.Error("Leq holds for an added inspection that names a presidio sidecar")
	}
}
