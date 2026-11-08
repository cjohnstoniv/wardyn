// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package adoscope

import (
	"net/http"
	"slices"
	"testing"
)

// The pull-request split: pr_create, pr_vote and pr_complete in place of pr, which stays a list alias.
//
// Field names and enum values come from the Azure DevOps REST reference (api-version 7.1):
//   - https://learn.microsoft.com/en-us/rest/api/azure/devops/git/pull-request-threads/update?view=azure-devops-rest-7.1
//     (CommentThreadStatus: unknown, active, fixed, wontFix, closed, byDesign, pending)
//   - https://learn.microsoft.com/en-us/rest/api/azure/devops/git/pull-request-reviewers/create-pull-request-reviewer?view=azure-devops-rest-7.1
//     (vote: 10, 5, 0, -5, -10)
//   - https://learn.microsoft.com/en-us/rest/api/azure/devops/git/pull-request-statuses/create?view=azure-devops-rest-7.1
//     (iterationId may ride in the body of a status on the pull request)
//   - https://learn.microsoft.com/en-us/rest/api/azure/devops/git/pull-request-iteration-statuses/create?view=azure-devops-rest-7.1
//   - https://learn.microsoft.com/en-us/rest/api/azure/devops/git/pull-requests/update?view=azure-devops-rest-7.1
//   - https://learn.microsoft.com/en-us/rest/api/azure/devops/git/pull-requests/create?view=azure-devops-rest-7.1
//     (PullRequestStatus: notSet, active, abandoned, completed, all; a create body takes reviewers,
//     autoCompleteSetBy, completionOptions and status)
//
// A thread status of pending or unknown is a vote: only absent or active leaves a thread open.

// prSplitCases is one case per classification row, on the pull request at pr.
func prSplitCases(pr, create string) []caseT {
	return []caseT{
		// Row 1: reviewers, any method, body not read.
		{name: "reviewer add", req: adoReq(http.MethodPut, pr+"/reviewers/a1b2", `{"vote":0}`), want: CapPRVote},
		{name: "reviewer vote", req: adoReq(http.MethodPut, pr+"/reviewers/a1b2", `{"vote":10}`), want: CapPRVote},
		{name: "reviewers bulk (array)", req: adoReq(http.MethodPost, pr+"/reviewers", `[{"id":"a1b2"}]`), want: CapPRVote},
		{name: "reviewer remove", req: adoReq(http.MethodDelete, pr+"/reviewers/a1b2", ""), want: CapPRVote},
		{name: "reviewer body unreadable", req: adoReq(http.MethodPatch, pr+"/reviewers/a1b2", `not json`), want: CapPRVote},
		// Row 2 and 3: statuses, on the pull request and on an iteration.
		{name: "status on the pull request", req: adoReq(http.MethodPost, pr+"/statuses", `{"iterationId":3,"state":"succeeded"}`), want: CapPRVote},
		{name: "status delete", req: adoReq(http.MethodPatch, pr+"/statuses", `[{"op":"remove","path":"/1"}]`), want: CapPRVote},
		{name: "status on an iteration", req: adoReq(http.MethodPost, pr+"/iterations/3/statuses", `{"state":"succeeded"}`), want: CapPRVote},
		{name: "an iteration's changes stay a read", req: adoReq(http.MethodGet, pr+"/iterations/3/changes", ""), want: CapCodeRead},
		// Row 4: a thread update reads its status.
		{name: "thread resolved fixed", req: adoReq(http.MethodPatch, pr+"/threads/9", `{"status":"fixed"}`), want: CapPRVote},
		{name: "thread resolved by number", req: adoReq(http.MethodPatch, pr+"/threads/9", `{"status":2}`), want: CapPRVote},
		{name: "thread closed by PUT", req: adoReq(http.MethodPut, pr+"/threads/9", `{"status":"closed"}`), want: CapPRVote},
		{name: "thread pending", req: adoReq(http.MethodPatch, pr+"/threads/9", `{"status":"pending"}`), want: CapPRVote},
		{name: "thread unknown", req: adoReq(http.MethodPatch, pr+"/threads/9", `{"status":"unknown"}`), want: CapPRVote},
		{name: "thread status null", req: adoReq(http.MethodPatch, pr+"/threads/9", `{"status":null}`), want: CapPRVote},
		{name: "thread active", req: adoReq(http.MethodPatch, pr+"/threads/9", `{"status":"active"}`), want: CapPRCreate},
		{name: "thread active in another case", req: adoReq(http.MethodPatch, pr+"/threads/9", `{"status":"ACTIVE"}`), want: CapPRCreate},
		{name: "thread active by number", req: adoReq(http.MethodPatch, pr+"/threads/9", `{"status":1}`), want: CapPRCreate},
		{name: "thread edit without a status", req: adoReq(http.MethodPatch, pr+"/threads/9", `{"properties":{}}`), want: CapPRCreate},
		{name: "thread update with no body", req: adoReq(http.MethodPatch, pr+"/threads/9", ""), want: CapPRCreate},
		{name: "thread body unreadable", req: adoReq(http.MethodPatch, pr+"/threads/9", `not json`), want: CapPRVote},
		{name: "thread body repeats status", req: adoReq(http.MethodPatch, pr+"/threads/9", `{"status":"active","STATUS":"fixed"}`), want: CapPRVote},
		{name: "thread body an array", req: adoReq(http.MethodPatch, pr+"/threads/9", `[{"status":"active"}]`), want: CapPRVote},
		// Row 5: a new thread or comment, and a delete.
		{name: "thread create", req: adoReq(http.MethodPost, pr+"/threads", `{"status":"fixed","comments":[]}`), want: CapPRCreate},
		{name: "comment create", req: adoReq(http.MethodPost, pr+"/threads/9/comments", `{"content":"x"}`), want: CapPRCreate},
		{name: "comment delete", req: adoReq(http.MethodDelete, pr+"/threads/9/comments/1", ""), want: CapPRCreate},
		// Row 6: the listed subs, body not read.
		{name: "label add", req: adoReq(http.MethodPost, pr+"/labels", `{"name":"x"}`), want: CapPRCreate},
		{name: "label body unreadable", req: adoReq(http.MethodPost, pr+"/labels", `not json`), want: CapPRCreate},
		{name: "label delete", req: adoReq(http.MethodDelete, pr+"/labels/x", ""), want: CapPRCreate},
		{name: "work item link", req: adoReq(http.MethodPost, pr+"/workitems/12", ""), want: CapPRCreate},
		{name: "properties JSON-Patch", req: adoReq(http.MethodPatch, pr+"/properties", `[{"op":"add","path":"/k","value":"v"}]`), want: CapPRCreate},
		{name: "attachment upload", req: adoReq(http.MethodPost, pr+"/attachments/x.bin", "\x00\x01binary"), want: CapPRCreate},
		{name: "conflict resolution", req: adoReq(http.MethodPatch, pr+"/conflicts/4", `{}`), want: CapPRCreate},
		{name: "share", req: adoReq(http.MethodPost, pr+"/share", `{}`), want: CapPRCreate},
		{name: "iteration other than statuses", req: adoReq(http.MethodPost, pr+"/iterations/3/other", `{}`), want: CapPRCreate},
		// Row 7: an unknown sub fails closed.
		{name: "an unknown sub is a vote", req: adoReq(http.MethodPost, pr+"/somethingnew", `{}`), want: CapPRVote},
		// Row 8: the pull request itself, with nothing to read.
		{name: "abandon by delete", req: adoReq(http.MethodDelete, pr, ""), want: CapPRCreate},
		{name: "update with no body", req: adoReq(http.MethodPatch, pr, ""), want: CapPRCreate},
		// Row 9: the pull request's body.
		{name: "title update", req: adoReq(http.MethodPatch, pr, `{"title":"x"}`), want: CapPRCreate},
		{name: "abandon", req: adoReq(http.MethodPatch, pr, `{"status":"abandoned"}`), want: CapPRCreate},
		{name: "complete", req: adoReq(http.MethodPatch, pr, `{"status":"completed"}`), want: CapPRComplete},
		{name: "complete in another case", req: adoReq(http.MethodPatch, pr, `{"status":"Completed"}`), want: CapPRComplete},
		{name: "complete by number", req: adoReq(http.MethodPatch, pr, `{"status":3}`), want: CapPRComplete},
		{name: "auto-complete set", req: adoReq(http.MethodPatch, pr, `{"autoCompleteSetBy":{"id":"a1b2"}}`), want: CapPRComplete},
		{name: "auto-complete cleared is not a completion", req: adoReq(http.MethodPatch, pr, `{"autoCompleteSetBy":null}`), want: CapPRCreate},
		{name: "completion options", req: adoReq(http.MethodPatch, pr, `{"completionOptions":{"deleteSourceBranch":true}}`), want: CapPRComplete},
		{name: "bypassPolicy false still completes", req: adoReq(http.MethodPatch, pr, `{"completionOptions":{"bypassPolicy":false}}`), want: CapPRComplete},
		{name: "bypassPolicy", req: adoReq(http.MethodPatch, pr, `{"status":"completed","completionOptions":{"bypassPolicy":true}}`), want: CapPolicyBypass},
		{name: "create", req: adoReq(http.MethodPost, create, `{"title":"x","reviewers":[{"id":"a1b2"}]}`), want: CapPRCreate},
		{name: "create with a zero vote", req: adoReq(http.MethodPost, create, `{"reviewers":[{"id":"a1b2","vote":0}]}`), want: CapPRCreate},
		{name: "create with auto-complete", req: adoReq(http.MethodPost, create, `{"autoCompleteSetBy":{"id":"a1b2"}}`), want: CapPRComplete},
		{name: "create with a vote", req: adoReq(http.MethodPost, create, `{"reviewers":[{"id":"a1b2","vote":10}]}`), want: CapPRVote},
		{name: "create with a rejection", req: adoReq(http.MethodPost, create, `{"reviewers":[{"id":"a1b2","vote":-10}]}`), want: CapPRVote},
		{name: "create with the bypass", req: adoReq(http.MethodPost, create, `{"completionOptions":{"bypassPolicy":true}}`), want: CapPolicyBypass},
		{name: "a body that completes and votes is refused", req: adoReq(http.MethodPost, create, `{"status":"completed","reviewers":[{"vote":10}]}`), wantErr: true},
		{name: "a bypass that also votes is refused", req: adoReq(http.MethodPost, create, `{"completionOptions":{"bypassPolicy":true},"reviewers":[{"vote":10}]}`), wantErr: true},
		// Row 10: an unreadable body on the pull request itself.
		{name: "undecodable body on the pull request", req: adoReq(http.MethodPatch, pr, `not json`), wantErr: true},
		{name: "an array on the pull request", req: adoReq(http.MethodPatch, pr, `[{"status":"completed"}]`), wantErr: true},
	}
}

func TestClassifyPRSplitOrgRoute(t *testing.T) {
	runCases(t, prSplitCases("/acme/proj/_apis/git/pullrequests/7", "/acme/proj/_apis/git/repositories/r1/pullrequests"))
}

func TestClassifyPRSplitRepoRoute(t *testing.T) {
	runCases(t, prSplitCases("/acme/proj/_apis/git/repositories/r1/pullrequests/7", "/acme/proj/_apis/git/repositories/r1/pullrequests"))
}

// A withheld body is asked for where a row reads it, and not where a row does not.
func TestClassifyPRSplitWithheldBody(t *testing.T) {
	pr := "/acme/proj/_apis/git/repositories/r1/pullrequests/7"
	withheld := func(method, path string) Request {
		r := adoReq(method, path, "")
		r.BodyWithheld = true
		return r
	}
	for _, path := range []string{pr, pr + "/threads/9"} {
		if _, err := Classify(withheld(http.MethodPatch, path)); err != ErrNeedsBody {
			t.Errorf("PATCH %s withheld: err %v, want ErrNeedsBody", path, err)
		}
	}
	for path, want := range map[string]Capability{pr + "/reviewers/a1": CapPRVote, pr + "/labels": CapPRCreate, pr + "/statuses": CapPRVote} {
		if v, err := Classify(withheld(http.MethodPost, path)); err != nil || v.Capability != want {
			t.Errorf("POST %s withheld = %q, %v; want %q", path, v.Capability, err, want)
		}
	}
}

func TestClassifyNeverReturnsPR(t *testing.T) {
	for _, pr := range []string{"/acme/proj/_apis/git/pullrequests/7", "/acme/proj/_apis/git/repositories/r1/pullrequests/7"} {
		for _, tc := range prSplitCases(pr, "/acme/proj/_apis/git/repositories/r1/pullrequests") {
			if v, _ := Classify(tc.req); v.Capability == CapPR {
				t.Errorf("%s %s: the classifier returned pr", tc.name, pr)
			}
		}
	}
}

func TestExpandPRAndHas(t *testing.T) {
	if got := ExpandPR(nil); got != nil {
		t.Errorf("ExpandPR(nil) = %v, want nil", got)
	}
	if got := ExpandPR([]Capability{}); got == nil || len(got) != 0 {
		t.Errorf("ExpandPR([]) = %#v, want an empty non-nil list", got)
	}
	parts := []Capability{CapPRCreate, CapPRVote, CapPRComplete}
	if got := ExpandPR([]Capability{CapCodeRead, CapPR}); !slices.Equal(got, append([]Capability{CapCodeRead}, parts...)) {
		t.Errorf("ExpandPR([code_read pr]) = %v", got)
	}
	if got := ExpandPR([]Capability{CapPRVote, CapPR}); !slices.Equal(got, []Capability{CapPRVote, CapPRCreate, CapPRComplete}) {
		t.Errorf("ExpandPR([pr_vote pr]) = %v, want each part once", got)
	}
	// The result never shares the argument's backing array, with or without pr.
	for _, in := range [][]Capability{{CapCodeRead, CapPR}, {CapCodeRead, CapCodeWrite}} {
		arg := make([]Capability, len(in), len(in)+4)
		copy(arg, in)
		out := ExpandPR(arg)
		out[0] = CapRepoAdmin
		_ = append(out, CapWorkAdmin)
		if !slices.Equal(arg, in) || slices.Contains(arg[:cap(arg)], CapRepoAdmin) || slices.Contains(arg[:cap(arg)], CapWorkAdmin) {
			t.Errorf("ExpandPR(%v) shares its argument: %v", in, arg[:cap(arg)])
		}
	}
	for _, c := range parts {
		if !Has([]Capability{CapPR}, c) {
			t.Errorf("Has([pr], %s) = false", c)
		}
	}
	if !Has(parts, CapPR) {
		t.Error("Has(the three parts, pr) = false")
	}
	if Has([]Capability{CapPRCreate, CapPRVote}, CapPR) || Has([]Capability{CapPRCreate}, CapPRVote) || Has(nil, CapPR) {
		t.Error("Has read a part, or pr, that the list does not hold")
	}
	if !HasName([]string{"code_read", "pr"}, CapPRVote) || HasName([]string{"pr_create"}, CapPRVote) {
		t.Error("HasName does not read pr as its parts")
	}
}

func TestPermitsPRAlias(t *testing.T) {
	for _, c := range []Capability{CapPRCreate, CapPRVote, CapPRComplete} {
		if !Permits([]Capability{CapPR}, Verdict{Capability: c}) {
			t.Errorf("granted [pr] refuses %s", c)
		}
	}
	if !Permits([]Capability{CapPRCreate}, Verdict{Capability: CapPRCreate}) {
		t.Error("granted [pr_create] refuses pr_create")
	}
	for _, c := range []Capability{CapPRVote, CapPRComplete} {
		if Permits([]Capability{CapPRCreate}, Verdict{Capability: c}) {
			t.Errorf("granted [pr_create] permits %s", c)
		}
	}
}

func TestScopesForPRSplit(t *testing.T) {
	for _, c := range []Capability{CapPRCreate, CapPRVote, CapPRComplete} {
		if !c.Grantable() || Label(c) == string(c) || ShortLabel(c) == string(c) {
			t.Errorf("%s is not grantable with a label and a short label", c)
		}
		got, err := ScopesFor([]Capability{c})
		if err != nil || !slices.Equal(got, []string{ResourceID + "/vso.code_write"}) {
			t.Errorf("ScopesFor(%s) = %v, %v; want vso.code_write", c, got, err)
		}
	}
}

// The server's enum parser also reads a status spelled as a string of digits.
func TestClassifyPRSplitEnumAsDigits(t *testing.T) {
	pr := "/acme/proj/_apis/git/repositories/r1/pullrequests/7"
	runCases(t, []caseT{
		{name: "complete as a digit string", req: adoReq(http.MethodPatch, pr, `{"status":"3"}`), want: CapPRComplete},
		{name: "thread active as a digit string", req: adoReq(http.MethodPatch, pr+"/threads/9", `{"status":"1"}`), want: CapPRCreate},
		{name: "thread fixed as a digit string", req: adoReq(http.MethodPatch, pr+"/threads/9", `{"status":"2"}`), want: CapPRVote},
	})
}
