// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const restPushTarget = "/acme/proj/_apis/git/repositories/app/pushes?api-version=7.1"

// restPush is a Git Pushes - Create body adding one file to the run's branch.
func restPush(runBranch, path string) string {
	return `{"refUpdates":[{"name":"` + runBranch + `","oldObjectId":"` + zeroOID + `"}],` +
		`"commits":[{"comment":"x","changes":[{"changeType":"add","item":{"path":"` + path + `"},` +
		`"newContent":{"content":"on: push","contentType":"rawtext"}}]}]}`
}

// newADORESTRules is the REST gate harness with push rules and a fake control
// plane for held pushes. The grant holds code_write, so anything refused on a
// run-branch push is the content rules'.
func newADORESTRules(t *testing.T, spec types.RunPolicySpec, state types.ApprovalState, caps ...adoscope.Capability) (*adoHarness, *pushCP) {
	t.Helper()
	h := newADOHarness(t, caps...)
	h.p.policy = CompilePolicy(spec)
	cp := newPushCP(t, state)
	cp.wire(t, h.p)
	return h, cp
}

func (h *adoHarness) branch() string { return BranchNSPrefix(h.p.runID) + "work" }

// TestADORESTPushRules is the REST door of push_rules: a Git Pushes - Create
// body carries its files inline, and before this change never reached the
// rules at all (the review probe: deny_paths [".github/**"] and a REST push
// adding a workflow answered 201).
func TestADORESTPushRules(t *testing.T) {
	caps := []adoscope.Capability{adoscope.CapRead, adoscope.CapCodeWrite}
	review := reviewSpec(1, []string{".github/workflows/**"})

	t.Run("deny refuses", func(t *testing.T) {
		h, cp := newADORESTRules(t, contentRulesSpec(".github/**"), types.ApprovalApproved, caps...)
		rec := h.do(t, http.MethodPost, restPushTarget, restPush(h.branch(), "/.github/workflows/exfil.yml"), nil)
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), ".github/workflows/exfil.yml") {
			t.Fatalf("status = %d body %s, want 403 naming the path", rec.Code, rec.Body.String())
		}
		if n := len(h.fake.Requests()); n != 0 {
			t.Errorf("the upstream saw %d request(s)", n)
		}
		if raises, _ := cp.counts(); raises != 0 || !strings.Contains(h.log(), `"`+ruleSourceGitRules+`"`) {
			t.Errorf("raises = %d; want none and a %s row", raises, ruleSourceGitRules)
		}
	})
	t.Run("review holds and forwards on approve", func(t *testing.T) {
		h, cp := newADORESTRules(t, review, types.ApprovalApproved, caps...)
		body := restPush(h.branch(), "/.github/workflows/ci.yml")
		rec := h.do(t, http.MethodPost, restPushTarget, body, nil)
		if rec.Code/100 != 2 {
			t.Fatalf("status = %d body %s, want it forwarded", rec.Code, rec.Body.String())
		}
		sum := sha256.Sum256([]byte(body))
		_, raises := cp.raised()
		if len(raises) != 1 || raises[0].Repo != "dev.azure.com/acme/proj/_git/app" || raises[0].Branch != h.branch() ||
			len(raises[0].Commits) != 1 || raises[0].Commits[0] != hex.EncodeToString(sum[:]) ||
			!strings.HasPrefix(raises[0].ActsAs, "api_key:") || raises[0].Paths[0] != ".github/workflows/ci.yml" {
			t.Errorf("raises = %+v, want one naming the repo, the branch, the body's digest and the person's grant", raises)
		}
	})
	t.Run("review refuses on deny", func(t *testing.T) {
		h, _ := newADORESTRules(t, review, types.ApprovalDenied, caps...)
		rec := h.do(t, http.MethodPost, restPushTarget, restPush(h.branch(), "/.github/workflows/ci.yml"), nil)
		if rec.Code != http.StatusForbidden || len(h.fake.Requests()) != 0 || !strings.Contains(h.log(), ruleSourceGitPushHeld) {
			t.Fatalf("status = %d, upstream %d; want a 403, nothing forwarded and a %s row", rec.Code, len(h.fake.Requests()), ruleSourceGitPushHeld)
		}
	})
	t.Run("unattended refuses without a row", func(t *testing.T) {
		h, cp := newADORESTRules(t, review, types.ApprovalApproved, caps...)
		h.p.pushHolds.unattended = true
		rec := h.do(t, http.MethodPost, restPushTarget, restPush(h.branch(), "/.github/workflows/ci.yml"), nil)
		if raises, _ := cp.counts(); rec.Code != http.StatusForbidden || raises != 0 || len(h.fake.Requests()) != 0 {
			t.Fatalf("status = %d, raises %d, upstream %d; want a 403 and nothing asked or forwarded", rec.Code, raises, len(h.fake.Requests()))
		}
		if !strings.Contains(h.log(), ruleSourceGitPushHeldUnattended) {
			t.Errorf("no %s row", ruleSourceGitPushHeldUnattended)
		}
	})
	t.Run("a path no rule names passes to the capability check", func(t *testing.T) {
		h, cp := newADORESTRules(t, review, types.ApprovalDenied, caps...)
		if rec := h.do(t, http.MethodPost, restPushTarget, restPush(h.branch(), "/src/main.go"), nil); rec.Code/100 != 2 {
			t.Fatalf("status = %d body %s, want it forwarded", rec.Code, rec.Body.String())
		}
		if raises, _ := cp.counts(); raises != 0 {
			t.Errorf("an unmatched push raised %d approvals", raises)
		}
	})
	t.Run("content rules before the capability check", func(t *testing.T) {
		h, _ := newADORESTRules(t, contentRulesSpec(".github/**"), types.ApprovalApproved, adoscope.CapRead)
		rec := h.do(t, http.MethodPost, restPushTarget, restPush(h.branch(), "/.github/workflows/exfil.yml"), nil)
		log := h.log()
		if rec.Code != http.StatusForbidden || !strings.Contains(log, ruleSourceGitRules) || strings.Contains(log, ruleSourceADODenied) {
			t.Fatalf("status = %d log %s; want the %s refusal alone, before any capability refusal", rec.Code, log, ruleSourceGitRules)
		}
	})
	t.Run("nil push_rules unchanged", func(t *testing.T) {
		h, cp := newADORESTRules(t, types.RunPolicySpec{}, types.ApprovalDenied, caps...)
		if rec := h.do(t, http.MethodPost, restPushTarget, restPush(h.branch(), "/.github/workflows/exfil.yml"), nil); rec.Code/100 != 2 {
			t.Fatalf("status = %d body %s, want it forwarded as before", rec.Code, rec.Body.String())
		}
		if raises, polls := cp.counts(); raises != 0 || polls != 0 {
			t.Errorf("nil push_rules asked the control plane (raises %d, polls %d)", raises, polls)
		}
	})
}

// TestADORESTPushRulesRefuseWhatTheyCannotRead: a REST push the gate cannot
// read whole, and every route that puts content on a branch without naming
// its paths, is refused while the run has push rules — and only then.
func TestADORESTPushRulesRefuseWhatTheyCannotRead(t *testing.T) {
	caps := adoscope.GrantableCapabilities()
	branch := func(h *adoHarness) string { return h.branch() }
	cases := []struct {
		name, method, target string
		body                 func(h *adoHarness) string
		// gateFirst: the capability classifier cannot read this body either,
		// and refuses it outright before the push rules are consulted.
		gateFirst bool
	}{
		{"a push repeating a key", http.MethodPost, restPushTarget, func(h *adoHarness) string {
			return strings.Replace(restPush(branch(h), "/src/a.go"), `"commits":`, `"commits":[],"Commits":`, 1)
		}, true},
		{"a push change with no item path", http.MethodPost, restPushTarget, func(h *adoHarness) string {
			return strings.Replace(restPush(branch(h), "/src/a.go"), `"item":{"path":"/src/a.go"}`, `"item":{}`, 1)
		}, false},
		{"a push path with a .. segment", http.MethodPost, restPushTarget, func(h *adoHarness) string {
			return restPush(branch(h), "/src/../.github/workflows/x.yml")
		}, false},
		{"an import request", http.MethodPost, "/acme/proj/_apis/git/repositories/app/importrequests?api-version=7.1",
			func(*adoHarness) string { return `{"parameters":{"gitSource":{"url":"https://example.com/x.git"}}}` }, false},
		{"a ref pointed at a commit", http.MethodPost, "/acme/proj/_apis/git/repositories/app/refs?api-version=7.1",
			func(h *adoHarness) string {
				return `[{"name":"` + branch(h) + `","oldObjectId":"` + zeroOID + `","newObjectId":"` + strings.Repeat("a", 40) + `"}]`
			}, false},
		{"a pull request completion", http.MethodPatch, "/acme/proj/_apis/git/repositories/app/pullrequests/7?api-version=7.1",
			func(*adoHarness) string {
				return `{"status":"completed","lastMergeSourceCommit":{"commitId":"` + strings.Repeat("a", 40) + `"}}`
			}, false},
		{"a wiki page write", http.MethodPut, "/acme/proj/_apis/wiki/wikis/proj.wiki/pages?path=/x&api-version=7.1",
			func(*adoHarness) string { return `{"content":"x"}` }, false},
		{"a push with no commits and a set newObjectId", http.MethodPost, restPushTarget, func(h *adoHarness) string {
			return `{"refUpdates":[{"name":"` + branch(h) + `","newObjectId":"` + strings.Repeat("a", 40) + `"}],"commits":[]}`
		}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h, _ := newADORESTRules(t, contentRulesSpec(".github/**"), types.ApprovalApproved, caps...)
			rec := h.do(t, c.method, c.target, c.body(h), nil)
			if rec.Code != http.StatusForbidden || len(h.fake.Requests()) != 0 {
				t.Fatalf("status = %d body %s, upstream %d; want a 403 and nothing forwarded", rec.Code, rec.Body.String(), len(h.fake.Requests()))
			}
			want := ruleSourceGitPackBlind
			if c.gateFirst {
				want = ruleSourceADODenied
			}
			if log := h.log(); !strings.Contains(log, `"`+want+`"`) {
				t.Errorf("decision log %s does not name %s", log, want)
			}
		})
	}
	// With no push rules the same routes are the capability gate's alone, as
	// before: none of them is refused as uninspectable.
	for _, c := range cases {
		t.Run(c.name+" without push rules", func(t *testing.T) {
			h, _ := newADORESTRules(t, types.RunPolicySpec{}, types.ApprovalApproved, caps...)
			h.do(t, c.method, c.target, c.body(h), nil)
			if log := h.log(); strings.Contains(log, ruleSourceGitPackBlind) {
				t.Errorf("with no push rules the request was refused as uninspectable: %s", log)
			}
		})
	}
	// An unreadable rule entry is the same failure the git door reports at
	// compile time: an unenforceable rule refuses every push, not just the
	// ones it would have matched.
	t.Run("an unreadable rule entry", func(t *testing.T) {
		h, _ := newADORESTRules(t, contentRulesSpec(".github/**", "src//secret"), types.ApprovalApproved, caps...)
		rec := h.do(t, http.MethodPost, restPushTarget, restPush(h.branch(), "/src/main.go"), nil)
		if rec.Code != http.StatusForbidden || len(h.fake.Requests()) != 0 {
			t.Fatalf("status = %d body %s, upstream %d; want a 403 and nothing forwarded", rec.Code, rec.Body.String(), len(h.fake.Requests()))
		}
		if log := h.log(); !strings.Contains(log, `"`+ruleSourceGitPackBlind+`"`) {
			t.Errorf("decision log %s does not name %s", log, ruleSourceGitPackBlind)
		}
	})
}

// A pull request that is only created, or a ref that is only deleted, writes
// no content and is left to the capability gate.
func TestADORESTPushRulesLeaveNonContentWritesAlone(t *testing.T) {
	h, _ := newADORESTRules(t, contentRulesSpec(".github/**"), types.ApprovalApproved, adoscope.GrantableCapabilities()...)
	h.do(t, http.MethodPost, "/acme/proj/_apis/git/repositories/app/pullrequests?api-version=7.1",
		`{"sourceRefName":"`+h.branch()+`","targetRefName":"refs/heads/main","title":"x"}`, nil)
	h.do(t, http.MethodPost, "/acme/proj/_apis/git/repositories/app/refs?api-version=7.1",
		`[{"name":"`+h.branch()+`","oldObjectId":"`+strings.Repeat("a", 40)+`","newObjectId":"`+zeroOID+`"}]`, nil)
	if log := h.log(); strings.Contains(log, ruleSourceGitPackBlind) || strings.Contains(log, ruleSourceGitRules) {
		t.Errorf("a non-content write met the push rules: %s", log)
	}
}

// TestADORESTPushRulesSeeTheEffectiveMethod: a push body sent as POST with an
// X-HTTP-Method-Override the service honours is still a write to pushes, and
// with push rules set it is refused — before this, PUT and PATCH classified as
// no content write and the push was forwarded (201, allow). A pull request
// created already set to auto-complete is refused the same way: its merge is
// content the request does not show.
func TestADORESTPushRulesSeeTheEffectiveMethod(t *testing.T) {
	caps := adoscope.GrantableCapabilities()
	for _, override := range []string{http.MethodPut, http.MethodPatch} {
		t.Run("push overridden to "+override, func(t *testing.T) {
			h, _ := newADORESTRules(t, contentRulesSpec(".github/**"), types.ApprovalApproved, caps...)
			rec := h.do(t, http.MethodPost, restPushTarget, restPush(h.branch(), "/.github/workflows/exfil.yml"),
				map[string]string{"X-HTTP-Method-Override": override})
			if rec.Code != http.StatusForbidden || len(h.fake.Requests()) != 0 {
				t.Fatalf("status = %d body %s, upstream %d; want a 403 and nothing forwarded", rec.Code, rec.Body.String(), len(h.fake.Requests()))
			}
			if log := h.log(); !strings.Contains(log, `"`+ruleSourceGitPackBlind+`"`) {
				t.Errorf("decision log %s does not name %s", log, ruleSourceGitPackBlind)
			}
		})
	}
	t.Run("pull request created with auto-complete", func(t *testing.T) {
		h, _ := newADORESTRules(t, contentRulesSpec(".github/**"), types.ApprovalApproved, caps...)
		rec := h.do(t, http.MethodPost, "/acme/proj/_apis/git/repositories/app/pullrequests?api-version=7.1",
			`{"sourceRefName":"`+h.branch()+`","targetRefName":"refs/heads/main","title":"x","autoCompleteSetBy":{"id":"x"}}`, nil)
		if rec.Code != http.StatusForbidden || len(h.fake.Requests()) != 0 || !strings.Contains(h.log(), ruleSourceGitPackBlind) {
			t.Fatalf("status = %d body %s; want a 403 %s and nothing forwarded", rec.Code, rec.Body.String(), ruleSourceGitPackBlind)
		}
	})
}
