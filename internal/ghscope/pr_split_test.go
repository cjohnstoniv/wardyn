// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package ghscope

import (
	"maps"
	"slices"
	"testing"
)

// The pull-request split on GitHub: pr_write and pr_review in place of pr, which stays a list alias.
// Review routes: https://docs.github.com/en/rest/pulls/reviews

func TestPullsReviewIsPRReview(t *testing.T) {
	classifyAll(t, []restCase{
		{"POST " + in + "/pulls/5/reviews", CapPRReview},
		{"POST " + in + "/pulls/5/reviews/9/events", CapPRReview},
		{"PUT " + in + "/pulls/5/reviews/9/dismissals", CapPRReview},
		{"PUT " + in + "/pulls/5/reviews/9", CapPRReview},
		{"DELETE " + in + "/pulls/5/reviews/9", CapPRReview},

		{"POST " + in + "/pulls", CapPRWrite},
		{"PATCH " + in + "/pulls/5", CapPRWrite},
		{"POST " + in + "/pulls/5/comments", CapPRWrite},
		{"POST " + in + "/pulls/5/requested_reviewers", CapPRWrite},
		{"DELETE " + in + "/pulls/5/requested_reviewers", CapPRWrite},
		{"PATCH " + in + "/pulls/comments/7", CapPRWrite},

		{"PUT " + in + "/pulls/5/merge", CapRepoContentWriteREST},
		{"GET " + in + "/pulls/5/reviews", CapCodeRead},
	}, "acme/app", "acme")
	for _, line := range []string{"POST " + in + "/pulls", "POST " + in + "/pulls/5/reviews", "PATCH " + in + "/pulls/5"} {
		if v, _ := Classify(rest(line)); v.Capability == CapPR {
			t.Errorf("%s: the classifier returned pr", line)
		}
	}
}

func TestGitHubExpandPRAndHas(t *testing.T) {
	if ExpandPR(nil) != nil {
		t.Error("ExpandPR(nil) is not nil")
	}
	if got := ExpandPR([]Capability{}); got == nil || len(got) != 0 {
		t.Errorf("ExpandPR([]) = %#v, want an empty non-nil list", got)
	}
	if got := ExpandPR([]Capability{CapPRReview, CapPR, CapCodeRead}); !slices.Equal(got, []Capability{CapPRReview, CapPRWrite, CapCodeRead}) {
		t.Errorf("ExpandPR = %v", got)
	}
	arg := make([]Capability, 1, 4)
	arg[0] = CapCodeRead
	out := ExpandPR(arg)
	out[0] = CapRepoAdmin
	_ = append(out, CapOrgAdmin)
	if slices.Contains(arg[:cap(arg)], CapRepoAdmin) || slices.Contains(arg[:cap(arg)], CapOrgAdmin) {
		t.Errorf("ExpandPR shares its argument: %v", arg[:cap(arg)])
	}
	if !Has([]Capability{CapPR}, CapPRReview) || !Has([]Capability{CapPRWrite, CapPRReview}, CapPR) ||
		Has([]Capability{CapPRWrite}, CapPR) || Has([]Capability{CapPRWrite}, CapPRReview) {
		t.Error("Has does not read pr as its two parts")
	}
}

func TestGitHubPermitsPRAlias(t *testing.T) {
	repos := []string{"acme/app"}
	review, _ := Classify(rest("POST " + in + "/pulls/5/reviews"))
	write, _ := Classify(rest("POST " + in + "/pulls"))
	if !Permits([]Capability{CapPR}, repos, review) || !Permits([]Capability{CapPR}, repos, write) {
		t.Error("granted [pr] refuses a part")
	}
	if !Permits([]Capability{CapPRWrite}, repos, write) || Permits([]Capability{CapPRWrite}, repos, review) {
		t.Error("granted [pr_write] does not stop at pr_write")
	}
}

func TestPermissionsForPRSplit(t *testing.T) {
	want := map[string]string{"pull_requests": "write"}
	for _, caps := range [][]Capability{{CapPRWrite}, {CapPRReview}, {CapPRWrite, CapPRReview}, {CapPR}} {
		got, err := PermissionsFor(caps)
		if err != nil || !maps.Equal(got, want) {
			t.Errorf("PermissionsFor(%v) = %v, %v; want %v", caps, got, err, want)
		}
	}
	for _, c := range []Capability{CapPRWrite, CapPRReview} {
		if !c.Grantable() || Label(c) == "" {
			t.Errorf("%s is not grantable with a label", c)
		}
	}
}
