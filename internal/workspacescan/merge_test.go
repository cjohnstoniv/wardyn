// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package workspacescan

import (
	"crypto/sha256"
	"encoding/hex"
	"reflect"
	"testing"
)

// TestMergeProfiles_EmptyAndSingle pins the empty short-circuit and the
// single-profile case: no profiles merges to the zero value; ONE profile
// whose identity IS primaryIdentity is returned completely unchanged
// (including its own Source, which the N>1 path always overwrites to
// SourceDeterministic) — nothing ambiguous to resolve when the lone scanned
// source is also primary. When the lone profile is NOT primary (Sources[0]
// is a different, unscanned/ephemeral attachment), HasDevcontainer/
// HasDockerfile must not ride along unchanged — the exact reconcile-wave1
// regression, which lived in this len==1 shortcut.
func TestMergeProfiles_EmptyAndSingle(t *testing.T) {
	if got := MergeProfiles(nil, nil, ""); !reflect.DeepEqual(got, WorkspaceProfile{}) {
		t.Errorf("MergeProfiles(nil) = %+v, want the zero profile", got)
	}
	solo := WorkspaceProfile{
		Languages: []string{"Go"}, Confidence: ConfidenceMedium, Source: SourceAIAssisted,
		LeakFindings: []LeakFinding{{Path: "x/.env", Kind: "aws-access-key"}},
	}
	got := MergeProfiles([]WorkspaceProfile{solo}, []string{"only-source"}, "only-source")
	if !reflect.DeepEqual(got, solo) {
		t.Errorf("MergeProfiles(one profile, primary) = %+v, want it returned byte-identical (incl. Source=%q, unprefixed paths)", got, solo.Source)
	}

	// The lone SCANNED source is NOT primary: e.g. Sources[0] is a second,
	// unscanned attachment, so primaryIdentity names a source this profiles
	// slice has no entry for.
	withDC := WorkspaceProfile{Languages: []string{"Go"}, HasDevcontainer: true, HasDockerfile: true, Confidence: ConfidenceHigh}
	notPrimary := MergeProfiles([]WorkspaceProfile{withDC}, []string{"secondary-source"}, "primary-but-unscanned")
	if notPrimary.HasDevcontainer || notPrimary.HasDockerfile {
		t.Errorf("MergeProfiles(one profile, NOT primary): HasDevcontainer=%v HasDockerfile=%v, want false/false — a non-primary source's devcontainer must not be attributed to an unscanned primary",
			notPrimary.HasDevcontainer, notPrimary.HasDockerfile)
	}
	if want := []string{"Go"}; !reflect.DeepEqual(notPrimary.Languages, want) {
		t.Errorf("Languages = %v, want %v (everything else about the lone profile still applies)", notPrimary.Languages, want)
	}
}

// TestMergeProfiles_PrimaryByIdentityNotSliceOrder pins the reconcile-wave1
// fix in the N>1 merge path: primary is matched by identity against
// primaryIdentity, never profiles[0]/identities[0]. Feeds the same shape of
// two profiles as TestMergeProfiles_FieldByField's a/b but with the NON-
// primary one first in both slices — if primary were still slice-order
// based this would (wrongly) read the first entry as primary.
func TestMergeProfiles_PrimaryByIdentityNotSliceOrder(t *testing.T) {
	a := WorkspaceProfile{HasDevcontainer: true, HasDockerfile: false, Confidence: ConfidenceHigh, Source: SourceDeterministic}
	b := WorkspaceProfile{HasDevcontainer: false, HasDockerfile: true, Confidence: ConfidenceHigh, Source: SourceDeterministic}

	// b (NOT primary) is profiles[0]/identities[0] here — deliberately the
	// reverse of attachment/primary order.
	got := MergeProfiles([]WorkspaceProfile{b, a}, []string{"repoB", "repoA"}, "repoA")
	if !got.HasDevcontainer {
		t.Error("HasDevcontainer = false, want true (repoA's own value) — slice position 0 (repoB) must not win over primaryIdentity")
	}
	if got.HasDockerfile {
		t.Error("HasDockerfile = true, want false (repoA's own value) — repoB's must not bleed in just because it is profiles[0]")
	}
}

// TestMergeProfiles_FieldByField is the table test every WorkspaceProfile
// field must be pinned against: MergeProfiles is the derivation behind every
// multi-source workspace's profile, and had no test at all before this,
// which is why the dropped ContextHash and the OR'd HasDevcontainer/
// HasDockerfile misattribution both went unnoticed.
func TestMergeProfiles_FieldByField(t *testing.T) {
	a := WorkspaceProfile{
		Languages: []string{"go", "python"}, PackageManagers: []string{"pip"},
		EgressDomains: []string{"a.example"}, Tools: []string{"make"},
		GitRemotes:      GitRemotes{GitHub: []string{"acme/a"}, OtherHosts: []string{"git.corp.example"}},
		HasDevcontainer: true, HasDockerfile: false,
		RequiredSecrets:    []SecretNeed{{Name: "SHARED_KEY", Kind: "a-kind"}, {Name: "STRIPE_KEY", Kind: "generic"}},
		ServicesNeeded:     []string{"postgres"},
		SuggestedEgress:    []string{"s1.example"},
		SecretFilesPresent: []string{".env"},
		BuildMemoryMiB:     512,
		LeakFindings:       []LeakFinding{{Path: "config/.env", Kind: "aws-access-key", Line: 3}},
		SetupCommands: []SetupCommand{
			{Stage: "build", Command: "go build ./...", Source: "convention:go"},
			{Stage: "install", Command: "pip install -r requirements.txt", Source: "convention:python"},
		},
		ContextHash: "hashA",
		Confidence:  ConfidenceHigh,
		NeedsReview: false,
		Source:      SourceDeterministic,
	}
	b := WorkspaceProfile{
		Languages: []string{"go", "ts"}, PackageManagers: []string{"npm"},
		EgressDomains: []string{"b.example"}, Tools: []string{"docker"},
		GitRemotes:      GitRemotes{GitHub: []string{"acme/b"}},
		HasDevcontainer: false, HasDockerfile: true, // must NOT bleed into the merge — A is primary
		RequiredSecrets:    []SecretNeed{{Name: "SHARED_KEY", Kind: "b-kind", Optional: true}}, // dup name: A's (first) wins
		ServicesNeeded:     []string{"redis"},
		SuggestedEgress:    []string{"s2.example"},
		SecretFilesPresent: []string{".env"}, // same relative path as A's — must stay distinguishable
		BuildMemoryMiB:     1024,
		LeakFindings:       []LeakFinding{{Path: "config/.env", Kind: "aws-access-key", Line: 3}}, // same relative path as A's
		SetupCommands: []SetupCommand{
			{Stage: "build", Command: "go build ./...", Source: "convention:go"}, // duplicate of A's — must dedupe to ONE
		},
		ContextHash: "hashB",
		Confidence:  ConfidenceLow,
		NeedsReview: true,
		Source:      SourceAIAssisted, // merged Source must always read SourceDeterministic
	}

	got := MergeProfiles([]WorkspaceProfile{a, b}, []string{"repoA", "repoB"}, "repoA")

	if want := []string{"go", "python", "ts"}; !reflect.DeepEqual(got.Languages, want) {
		t.Errorf("Languages = %v, want %v (union, sorted)", got.Languages, want)
	}
	if want := []string{"npm", "pip"}; !reflect.DeepEqual(got.PackageManagers, want) {
		t.Errorf("PackageManagers = %v, want %v", got.PackageManagers, want)
	}
	if want := []string{"a.example", "b.example"}; !reflect.DeepEqual(got.EgressDomains, want) {
		t.Errorf("EgressDomains = %v, want %v", got.EgressDomains, want)
	}
	if want := []string{"docker", "make"}; !reflect.DeepEqual(got.Tools, want) {
		t.Errorf("Tools = %v, want %v", got.Tools, want)
	}
	wantRemotes := GitRemotes{GitHub: []string{"acme/a", "acme/b"}, OtherHosts: []string{"git.corp.example"}}
	if !reflect.DeepEqual(got.GitRemotes, wantRemotes) {
		t.Errorf("GitRemotes = %+v, want %+v", got.GitRemotes, wantRemotes)
	}
	if !got.HasDevcontainer {
		t.Error("HasDevcontainer = false, want true (from the PRIMARY/first profile, A)")
	}
	if got.HasDockerfile {
		t.Error("HasDockerfile = true, want false (from the PRIMARY/first profile, A) — B's must not bleed in")
	}
	wantSecrets := []SecretNeed{{Name: "SHARED_KEY", Kind: "a-kind"}, {Name: "STRIPE_KEY", Kind: "generic"}}
	if !reflect.DeepEqual(got.RequiredSecrets, wantSecrets) {
		t.Errorf("RequiredSecrets = %+v, want %+v (first-wins on dup name, sorted by name)", got.RequiredSecrets, wantSecrets)
	}
	if want := []string{"postgres", "redis"}; !reflect.DeepEqual(got.ServicesNeeded, want) {
		t.Errorf("ServicesNeeded = %v, want %v", got.ServicesNeeded, want)
	}
	if want := []string{"s1.example", "s2.example"}; !reflect.DeepEqual(got.SuggestedEgress, want) {
		t.Errorf("SuggestedEgress = %v, want %v", got.SuggestedEgress, want)
	}
	if want := []string{"repoA/.env", "repoB/.env"}; !reflect.DeepEqual(got.SecretFilesPresent, want) {
		t.Errorf("SecretFilesPresent = %v, want %v (identity-prefixed so the same relative path from two sources stays distinguishable)", got.SecretFilesPresent, want)
	}
	if got.BuildMemoryMiB != 1024 {
		t.Errorf("BuildMemoryMiB = %d, want 1024 (largest)", got.BuildMemoryMiB)
	}
	// Path stays SCAN-ROOT-RELATIVE (it is path-classified downstream: prefixing
	// it with a locator that itself contains a testdata/fixtures segment would
	// reclassify every leak in that source as a fixture); attribution rides
	// Source, which also keeps the two identical relative paths distinct.
	wantLeaks := []LeakFinding{
		{Path: "config/.env", Kind: "aws-access-key", Line: 3, Source: "repoA"},
		{Path: "config/.env", Kind: "aws-access-key", Line: 3, Source: "repoB"},
	}
	if !reflect.DeepEqual(got.LeakFindings, wantLeaks) {
		t.Errorf("LeakFindings = %+v, want %+v (attributed via Source, both kept — different sources, same relative path)", got.LeakFindings, wantLeaks)
	}
	wantCmds := []SetupCommand{
		{Stage: "build", Command: "go build ./...", Source: "convention:go"},
		{Stage: "install", Command: "pip install -r requirements.txt", Source: "convention:python"},
	}
	if !reflect.DeepEqual(got.SetupCommands, wantCmds) {
		t.Errorf("SetupCommands = %+v, want %+v (B's duplicate build step deduped away)", got.SetupCommands, wantCmds)
	}
	sum := sha256.Sum256([]byte("hashA" + "hashB")) // sorted: "hashA" < "hashB"
	if want := hex.EncodeToString(sum[:]); got.ContextHash != want {
		t.Errorf("ContextHash = %q, want %q (sha256 over the sorted per-source hashes)", got.ContextHash, want)
	}
	if got.Confidence != ConfidenceLow {
		t.Errorf("Confidence = %q, want %q (lowest of the two)", got.Confidence, ConfidenceLow)
	}
	if !got.NeedsReview {
		t.Error("NeedsReview = false, want true (OR across sources)")
	}
	if got.Source != SourceDeterministic {
		t.Errorf("Source = %q, want %q (the merge itself is deterministic, regardless of input Source values)", got.Source, SourceDeterministic)
	}
}

// TestMergeProfiles_LeakFindingsRecapped guards the re-cap half of dedupe +
// re-cap: N sources each under the per-source maxLeakFindings bound can still
// jointly exceed it once concatenated, and the merge must not let that
// through uncapped.
func TestMergeProfiles_LeakFindingsRecapped(t *testing.T) {
	mk := func(identity string, n int) (WorkspaceProfile, string) {
		var leaks []LeakFinding
		for i := 0; i < n; i++ {
			leaks = append(leaks, LeakFinding{Path: "f", Kind: "aws-access-key", Line: i + 1})
		}
		return WorkspaceProfile{LeakFindings: leaks, Confidence: ConfidenceHigh, Source: SourceDeterministic}, identity
	}
	pa, ia := mk("repoA", maxLeakFindings)
	pb, ib := mk("repoB", maxLeakFindings)

	got := MergeProfiles([]WorkspaceProfile{pa, pb}, []string{ia, ib}, ia)
	if len(got.LeakFindings) != maxLeakFindings {
		t.Errorf("LeakFindings = %d entries, want capped at %d (each source alone was already at the per-source bound)",
			len(got.LeakFindings), maxLeakFindings)
	}
}
