// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package workspacescan

import (
	"fmt"
	"testing"
)

// TestMergeProfiles_LeakTruncationIsSeverityOrderedAndFlagged pins B11b-F9.
// The merge concatenated every source's leak findings and then took the first
// maxLeakFindings of them — silently, in whatever order the sources happened to
// be attached. So a private key committed in the ninth source was simply absent
// from the merged profile, with Confidence and NeedsReview both saying the
// profile was complete. Against a package whose whole promise is never to drop
// a suspected secret.
//
// Two things fix it and neither is a bigger cap: the survivors are the HIGHEST
// severity ones rather than the earliest-attached, and a truncation sets
// NeedsReview so the profile stops claiming to be the whole picture.
func TestMergeProfiles_LeakTruncationIsSeverityOrderedAndFlagged(t *testing.T) {
	// Source A fills the cap on its own with the lowest-severity kind.
	var low []LeakFinding
	for i := 0; i < maxLeakFindings; i++ {
		low = append(low, LeakFinding{Path: fmt.Sprintf("a/%d.env", i), Kind: "jwt", Line: i + 1})
	}
	// Source B, attached second, is where the real credentials are.
	high := []LeakFinding{
		{Path: "b/id_rsa", Kind: "private-key-block", Line: 1},
		{Path: "b/.aws/credentials", Kind: "aws-access-key", Line: 2},
	}

	pa := WorkspaceProfile{LeakFindings: low, Confidence: ConfidenceHigh, Source: SourceDeterministic}
	pb := WorkspaceProfile{LeakFindings: high, Confidence: ConfidenceHigh, Source: SourceDeterministic}
	got := MergeProfiles([]WorkspaceProfile{pa, pb}, []string{"repoA", "repoB"}, "repoA")

	if len(got.LeakFindings) != maxLeakFindings {
		t.Fatalf("LeakFindings = %d, want capped at %d", len(got.LeakFindings), maxLeakFindings)
	}
	for _, want := range high {
		found := false
		for _, f := range got.LeakFindings {
			if f.Path == want.Path && f.Kind == want.Kind {
				found = true
			}
		}
		if !found {
			t.Errorf("%s (%s) was truncated away by a lower-severity finding from an earlier source",
				want.Path, want.Kind)
		}
	}
	if !got.NeedsReview {
		t.Error("NeedsReview = false after leak findings were dropped: a truncated profile must not " +
			"read as the whole picture")
	}
}

// TestMergeProfiles_UntruncatedLeaksDoNotForceReview is the negative control
// for the flag: the review stamp belongs to the truncation, not to having
// leaks at all.
func TestMergeProfiles_UntruncatedLeaksDoNotForceReview(t *testing.T) {
	pa := WorkspaceProfile{
		LeakFindings: []LeakFinding{{Path: "a/.env", Kind: "aws-access-key", Line: 1}},
		Confidence:   ConfidenceHigh, Source: SourceDeterministic,
	}
	pb := WorkspaceProfile{
		LeakFindings: []LeakFinding{{Path: "b/.env", Kind: "github-token", Line: 1}},
		Confidence:   ConfidenceHigh, Source: SourceDeterministic,
	}
	got := MergeProfiles([]WorkspaceProfile{pa, pb}, []string{"repoA", "repoB"}, "repoA")
	if got.NeedsReview {
		t.Error("NeedsReview = true with nothing truncated")
	}
	if len(got.LeakFindings) != 2 {
		t.Errorf("LeakFindings = %d, want both kept", len(got.LeakFindings))
	}
}

// TestDeriveProfile_LeakTruncationFlagsReview is the same silent drop at the
// single-source door: validateLeakFindings capped at maxLeakFindings with
// nothing recording that it had.
func TestDeriveProfile_LeakTruncationFlagsReview(t *testing.T) {
	var facts ScanFacts
	for i := 0; i < maxLeakFindings; i++ {
		facts.LeakFindings = append(facts.LeakFindings,
			LeakFinding{Path: fmt.Sprintf("a/%d.env", i), Kind: "jwt", Line: i + 1})
	}
	facts.LeakFindings = append(facts.LeakFindings,
		LeakFinding{Path: "z/id_rsa", Kind: "private-key-block", Line: 1})

	got := DeriveProfile(facts)
	if len(got.LeakFindings) != maxLeakFindings {
		t.Fatalf("LeakFindings = %d, want capped at %d", len(got.LeakFindings), maxLeakFindings)
	}
	survived := false
	for _, f := range got.LeakFindings {
		if f.Kind == "private-key-block" {
			survived = true
		}
	}
	if !survived {
		t.Error("the private key was truncated away by lower-severity findings")
	}
	if !got.NeedsReview {
		t.Error("NeedsReview = false after leak findings were dropped")
	}
}
