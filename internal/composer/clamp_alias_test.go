// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package composer

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// aliasProposal is a proposal that sets EVERY reference-semantics field of
// RunPolicySpec, paired with aliasCeiling so the clamp tightens nothing: the
// pass-through paths are exactly the ones where a ceiling "has no opinion",
// and where a shallow copy would hand the caller's own memory back.
func aliasProposal(t *testing.T) types.RunPolicySpec {
	t.Helper()
	ro := true
	return types.RunPolicySpec{
		MinConfinementClass: types.CC2,
		// Spare capacity on purpose: the historical incident this whole
		// ownership rule exists for was an append into a SHARED backing
		// array's spare capacity (see types.RunPolicySpec.Clone's comment).
		AllowedDomains:  append(make([]string, 0, 4), "api.example.com", "github.com"),
		DeniedDomains:   []string{"blocked.example.com"},
		AllowedMethods:  []string{"GET", "POST"},
		UIApps:          []types.UIApp{{Name: "code", Port: 8080, Path: "/"}},
		ToolRules:       []types.ToolRule{{Tool: "Bash", Effect: types.ToolDeny}},
		WorkspaceRepos:  []types.WorkspaceRepo{{Repo: "acme/widgets", Target: "/work/widgets"}},
		WorkspaceMounts: []types.WorkspaceMount{{Source: "/srv/repo", Target: "/work/repo", ReadOnly: &ro}},
		Resources:       &types.ResourceLimits{CPUMillis: 500, MemoryMiB: 512, PidsLimit: 64},
		EligibleGrants: []types.GrantSpec{{
			Kind: types.GrantAPIKey, TTLSeconds: 300, RequiresApproval: true,
			Scope: mustJSON(t, map[string]any{"host": "api.example.com", "secret_name": "acme-key"}),
		}},
		LLMInspection: &types.LLMInspectionSpec{Mode: "alert", DetectSecrets: true},
	}
}

// aliasCeiling permits everything aliasProposal asks for, so nothing is
// narrowed and every field reaches the caller by the untouched path.
func aliasCeiling(t *testing.T) types.RunPolicySpec {
	t.Helper()
	return types.RunPolicySpec{
		MinConfinementClass: types.CC1,
		AllowedDomains:      []string{"api.example.com", "github.com"},
		Resources:           &types.ResourceLimits{CPUMillis: 2000, MemoryMiB: 4096, PidsLimit: 512},
		EligibleGrants: []types.GrantSpec{{
			Kind: types.GrantAPIKey, TTLSeconds: 600, RequiresApproval: true,
			Scope: mustJSON(t, map[string]any{"host": "api.example.com", "secret_name": "acme-key"}),
		}},
	}
}

// widenInPlace mutates every reference field of s THROUGH its backing array or
// pointee — never by replacing a slice header or a pointer, which no aliasing
// bug could be caught by. Each write is a WIDENING: a new egress host, a
// dropped deny entry, an extra method, a different relay port, a denied tool
// turned allow, another repo, a re-homed credential, a 1 TiB memory cap.
func widenInPlace(t *testing.T, s *types.RunPolicySpec) {
	t.Helper()
	s.AllowedDomains[0] = "evil.example.com"
	s.DeniedDomains[0] = "allowed.example.com"
	s.AllowedMethods[0] = "DELETE"
	s.UIApps[0].Port = 31337
	s.ToolRules[0].Effect = types.ToolAllow
	s.WorkspaceRepos[0].Repo = "attacker/payload"
	s.Resources.MemoryMiB = 1 << 20
	smashInPlace(t, s.EligibleGrants[0].Scope, "api.example.com", "evl.example.com")
}

// smashInPlace overwrites old with an equal-length new INSIDE b's backing
// array, keeping the JSON valid so a spec carrying it still marshals.
func smashInPlace(t *testing.T, b []byte, old, replacement string) {
	t.Helper()
	if len(old) != len(replacement) {
		t.Fatalf("smashInPlace needs equal lengths, got %q/%q", old, replacement)
	}
	i := bytes.Index(b, []byte(old))
	if i < 0 {
		t.Fatalf("smashInPlace: %q not found in %s", old, b)
	}
	copy(b[i:], replacement)
}

func specJSON(t *testing.T, s types.RunPolicySpec) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestClamp_ClampedSpecDoesNotAliasTheProposal is the ownership half of the
// clamp's contract, and it is deliberately NOT an equality check: two aliased
// slices are trivially equal right after the call. Independence only shows up
// under MUTATION, and the direction that matters is the one that widens a
// ceiling already enforced.
func TestClamp_ClampedSpecDoesNotAliasTheProposal(t *testing.T) {
	proposed, ceiling := aliasProposal(t), aliasCeiling(t)
	got, _ := Clamp(proposed, ceiling, 0)

	// Nothing may be narrowed here, or the pass-through paths this test exists
	// to cover were never taken.
	if len(got.AllowedDomains) != 2 || len(got.EligibleGrants) != 1 || got.Resources == nil {
		t.Fatalf("fixture drift: the ceiling narrowed the proposal, got %s", specJSON(t, got))
	}
	// Mounts are dropped outright (host mounts are never composer-proposed), so
	// the caller's own WorkspaceMount — and its ReadOnly pointee — cannot reach
	// the clamped spec at all.
	if got.WorkspaceMounts != nil {
		t.Errorf("workspace_mounts must be dropped, got %v", got.WorkspaceMounts)
	}

	// Direction 1: mutate the ORIGINAL, the clamped spec must not move.
	before := specJSON(t, got)
	widenInPlace(t, &proposed)
	if after := specJSON(t, got); after != before {
		t.Errorf("mutating the proposal changed the clamped spec (aliased):\n before %s\n after  %s", before, after)
	}

	// Direction 2: mutate the RESULT, the caller's proposal must not move.
	fresh, _ := Clamp(aliasProposal(t), ceiling, 0)
	proposed2 := aliasProposal(t)
	got2, _ := Clamp(proposed2, ceiling, 0)
	beforeProposal := specJSON(t, proposed2)
	widenInPlace(t, &got2)
	if after := specJSON(t, proposed2); after != beforeProposal {
		t.Errorf("mutating the clamped spec changed the proposal (aliased):\n before %s\n after  %s", beforeProposal, after)
	}
	// ...and the clamp itself is unaffected by either mutation: a third call on
	// a fresh proposal answers exactly what the first one did.
	if again := specJSON(t, fresh); again != before {
		t.Errorf("Clamp is not stable across the mutations:\n first %s\n again %s", before, again)
	}

	// The documented incident: an append into SHARED spare capacity. Both sides
	// carry len 2 of a cap-4 array, so an aliased pair writes index 2 twice.
	proposed3 := aliasProposal(t)
	got3, _ := Clamp(proposed3, ceiling, 0)
	proposed3.AllowedDomains = append(proposed3.AllowedDomains, "from-the-proposal.example.com")
	got3.AllowedDomains = append(got3.AllowedDomains, "from-the-clamp.example.com")
	if proposed3.AllowedDomains[2] != "from-the-proposal.example.com" {
		t.Errorf("the clamped spec's append landed in the proposal's spare capacity, got %v", proposed3.AllowedDomains)
	}
	if got3.AllowedDomains[2] != "from-the-clamp.example.com" {
		t.Errorf("the proposal's append landed in the clamped spec's spare capacity, got %v", got3.AllowedDomains)
	}
}

// TestClamp_InheritedLLMInspectionDoesNotAliasTheCeiling covers the one
// reference field whose clamped value comes from the CEILING rather than the
// proposal (the unconditional inherit), so its independence is a statement
// about the ceiling — which is shared, process-global config, where an
// in-place write would leak into every later run.
func TestClamp_InheritedLLMInspectionDoesNotAliasTheCeiling(t *testing.T) {
	ceiling := aliasCeiling(t)
	ceiling.LLMInspection = &types.LLMInspectionSpec{
		Mode: "block", DetectSecrets: true,
		WorkspaceSecretNames: []string{"prod-db-password"},
		ClassifiedMarkers:    []string{"WARDYN-CONFIDENTIAL"},
	}
	got, _ := Clamp(aliasProposal(t), ceiling, 0)
	if got.LLMInspection == nil {
		t.Fatal("expected llm_inspection inherited from the ceiling")
	}

	got.LLMInspection.Mode = "off"
	got.LLMInspection.WorkspaceSecretNames[0] = "attacker-chosen"
	got.LLMInspection.ClassifiedMarkers[0] = "ignored"
	if ceiling.LLMInspection.Mode != "block" ||
		ceiling.LLMInspection.WorkspaceSecretNames[0] != "prod-db-password" ||
		ceiling.LLMInspection.ClassifiedMarkers[0] != "WARDYN-CONFIDENTIAL" {
		t.Errorf("mutating the inherited llm_inspection changed the ceiling (aliased): %+v", *ceiling.LLMInspection)
	}

	ceiling.LLMInspection.WorkspaceSecretNames[0] = "rotated"
	if got.LLMInspection.WorkspaceSecretNames[0] != "attacker-chosen" {
		t.Errorf("mutating the ceiling changed an already-clamped spec (aliased): %v", got.LLMInspection.WorkspaceSecretNames)
	}
}

// TestCloneProposal_SharesNothingWithItsInput pins the ownership helper
// directly, because two of the fields it copies can never reach a clamped spec
// and so no test written against Clamp can show their copies are real:
// workspace_mounts is dropped outright, and llm_inspection is replaced
// wholesale by the ceiling's or dropped. Their copies are the contract holding
// for every reference field rather than only the ones today's clamp happens to
// let through — the moment either stops being dropped, an untested copy is the
// difference between a closed hole and a silent one.
func TestCloneProposal_SharesNothingWithItsInput(t *testing.T) {
	orig := aliasProposal(t)
	orig.LLMInspection = &types.LLMInspectionSpec{
		Mode: "block", DetectSecrets: true,
		WorkspaceSecretNames:  []string{"prod-db-password"},
		WorkspaceSecretValues: []string{"resolved-at-dispatch-only"},
		ClassifiedMarkers:     []string{"WARDYN-CONFIDENTIAL"},
	}
	orig.PushRules = &types.PushRulesSpec{DenyPaths: []string{".github/workflows/**"}, MaxInspectPackMiB: 8,
		RequireReviewPaths: []string{"infra/**"}}
	beforeOrig := specJSON(t, orig)

	clone := cloneProposal(orig)
	beforeClone := specJSON(t, clone)
	if beforeClone != beforeOrig {
		t.Fatalf("the copy is not equal to its input:\n input %s\n copy  %s", beforeOrig, beforeClone)
	}

	// Direction 1: widen every reference field THROUGH the copy — backing
	// arrays and pointees only, never by replacing a header or a pointer.
	widenInPlace(t, &clone)
	*clone.WorkspaceMounts[0].ReadOnly = false
	clone.WorkspaceMounts[0].Source = "/"
	clone.LLMInspection.Mode = "off"
	clone.LLMInspection.WorkspaceSecretNames[0] = "attacker-chosen"
	clone.LLMInspection.WorkspaceSecretValues[0] = ""
	clone.LLMInspection.ClassifiedMarkers[0] = "ignored"
	clone.PushRules.DenyPaths[0] = "nothing/**"
	clone.PushRules.MaxInspectPackMiB = 64
	clone.PushRules.RequireReviewPaths[0] = "nothing/**"
	if after := specJSON(t, orig); after != beforeOrig {
		t.Errorf("mutating the copy changed its input (aliased):\n before %s\n after  %s", beforeOrig, after)
	}

	// Direction 2: the same widening applied to a fresh input must leave an
	// already-taken copy where it was.
	orig2 := aliasProposal(t)
	clone2 := cloneProposal(orig2)
	beforeClone2 := specJSON(t, clone2)
	widenInPlace(t, &orig2)
	*orig2.WorkspaceMounts[0].ReadOnly = false
	if after := specJSON(t, clone2); after != beforeClone2 {
		t.Errorf("mutating the input changed the copy (aliased):\n before %s\n after  %s", beforeClone2, after)
	}
}
