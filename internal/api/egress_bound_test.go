// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"slices"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A requirement row that is not an operator's own (reachable only with the operator-set gate off) is
// repository content: its private-range literal host widens the run only where the ceiling already
// covers it. An operator_set row, and a name, are never clamped.
func TestEgressBound_ClampsANonOperatorSetPrivateLiteral(t *testing.T) {
	ws := types.Workspace{}
	scan := types.WorkspaceRequirement{Level: "required", Provenance: "scan_seeded"}
	op := types.WorkspaceRequirement{Level: "required", Provenance: "operator_set"}
	var none *egressBound
	if !none.admits(scan, "10.0.0.5", ws) {
		t.Error("no bound: a row is admitted as before")
	}
	b := &egressBound{}
	if b.admits(scan, "10.0.0.5", ws) {
		t.Error("a scan-seeded private literal outside the ceiling was admitted")
	}
	if !b.admits(op, "10.0.0.5", ws) || !b.admits(scan, "registry.example.com", ws) {
		t.Error("an operator_set row or a name was clamped")
	}
	if !(&egressBound{trusted: []string{"10.0.0.5"}}).admits(scan, "10.0.0.5", ws) {
		t.Error("a private literal the ceiling names exactly was refused")
	}
}

// The confined replay holds a scan-seeded private literal to the run's ceiling when the gate is off.
func TestConfinedEgressDomains_ClampsAScanSeededPrivateLiteral(t *testing.T) {
	srv := &Server{cfg: Config{RequireOperatorSetEgress: false}}
	ws := types.Workspace{Requirements: map[string]types.WorkspaceRequirement{
		"egress:10.0.0.5": {Level: "required", Provenance: "scan_seeded"},
		"egress:ok.test":  {Level: "required", Provenance: "scan_seeded"},
	}}
	got := srv.confinedEgressDomains(ws, nil)
	if slices.Contains(got, "10.0.0.5") || !slices.Contains(got, "ok.test") {
		t.Fatalf("domains = %v, want ok.test and not the private literal", got)
	}
	if got := srv.confinedEgressDomains(ws, []string{"10.0.0.5"}); !slices.Contains(got, "10.0.0.5") {
		t.Fatalf("domains = %v, want the literal the ceiling names", got)
	}
}
