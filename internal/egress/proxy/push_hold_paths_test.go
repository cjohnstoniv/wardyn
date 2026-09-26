// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestPushPathListCarriesEveryPath is the F16 verifier probe
// (TestVerifierF16PathCarriers), flipped: with 150 review-matched paths the
// stored scope still names ten and the log sample a hundred, and the path list
// the raise carries names all 150, verified against the scope's digest.
func TestPushPathListCarriesEveryPath(t *testing.T) {
	var paths []string
	for i := range 150 {
		paths = append(paths, fmt.Sprintf(".github/workflows/w%03d.yml", i))
	}
	scope, _ := pushScope(paths, nil, pushTarget{repo: "github.com/o/r", actsAs: "git_pat:x"})
	raw, _ := json.Marshal(scope)
	var back map[string]any
	_ = json.Unmarshal(raw, &back)
	list := types.NewPushPathList(paths)
	t.Logf("stored scope paths=%d paths_total=%v log sample=%d path list=%d truncated=%v",
		len(back["paths"].([]any)), back["paths_total"], len(sampleOf(paths)), len(list.Paths), list.Truncated)
	if len(back["paths"].([]any)) != 10 || len(sampleOf(paths)) != 100 {
		t.Fatalf("the card list or the log sample changed size")
	}
	if len(list.Paths) != 150 || list.Truncated {
		t.Fatalf("path list = %d paths truncated=%v, want all 150", len(list.Paths), list.Truncated)
	}
	if err := list.VerifyAgainst(scope); err != nil {
		t.Fatalf("the list does not verify against its own scope: %v", err)
	}
}

// TestPushPathListBounds: 10,001 paths keep the first 10,000, and paths past
// 1 MiB of text keep what fits; both say truncated, and both still verify.
func TestPushPathListBounds(t *testing.T) {
	var many, big []string
	for i := range types.PushPathListMaxPaths + 1 {
		many = append(many, fmt.Sprintf("infra/%05d.tf", i))
	}
	for i := range 300 { // 4,000 bytes each: 262 fit in 1 MiB
		big = append(big, fmt.Sprintf("d/%03d/%s", i, strings.Repeat("x", 3994)))
	}
	for name, c := range map[string]struct {
		paths []string
		want  int
	}{
		"10,001 paths": {many, types.PushPathListMaxPaths},
		"over 1 MiB":   {big, 262},
	} {
		scope, _ := pushScope(c.paths, nil, pushTarget{repo: "github.com/o/r", actsAs: "git_pat:x"})
		list := types.NewPushPathList(c.paths)
		if len(list.Paths) != c.want || !list.Truncated {
			t.Errorf("%s: path list = %d paths truncated=%v, want %d truncated", name, len(list.Paths), list.Truncated, c.want)
		}
		if err := list.VerifyAgainst(scope); err != nil {
			t.Errorf("%s: a truncated list does not verify: %v", name, err)
		}
	}
}

// TestPushHoldRaisesTheFullPathList: a real held push touching 150 review
// paths raises its approval with all 150 beside the scope, while the refusal
// body still names ten.
func TestPushHoldRaisesTheFullPathList(t *testing.T) {
	p, _, _, cp, _ := newAppLaneHold(t, reviewSpec(5, []string{".github/workflows/**"}), types.ApprovalDenied)
	files := map[string]string{"README.md": "hello\n"}
	for i := range 150 {
		files[fmt.Sprintf(".github/workflows/w%03d.yml", i)] = "on: push\n"
	}
	rec := postPush(t, p, string(recordedPush(t, BranchNSPrefix(p.runID)+"work", files)))
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "... and 140 more matched path(s)") {
		t.Fatalf("status = %d body %q, want the denial naming ten paths and 140 more", rec.Code, rec.Body.String())
	}
	cp.mu.Lock()
	defer cp.mu.Unlock()
	if len(cp.lists) != 1 || cp.lists[0] == nil {
		t.Fatalf("raises carried path lists %v, want one", cp.lists)
	}
	if got := cp.lists[0]; len(got.Paths) != 150 || got.Truncated || got.VerifyAgainst(cp.raises[0]) != nil {
		t.Errorf("path list = %d paths truncated=%v verify=%v, want all 150 verified",
			len(got.Paths), got.Truncated, got.VerifyAgainst(cp.raises[0]))
	}
}

// TestQuotePathMatchesGit: the quoted form is git's with core.quotePath on.
func TestQuotePathMatchesGit(t *testing.T) {
	for in, want := range map[string]string{
		".github/workflows/ci.yml": ".github/workflows/ci.yml",
		"a b/c-d_e.yml":            "a b/c-d_e.yml",
		"b\xe9.yml":                `"b\351.yml"`,
		"café.yml":                 `"caf\303\251.yml"`,
		"tab\there":                `"tab\there"`,
		"nl\nx":                    `"nl\nx"`,
		`q"uote`:                   `"q\"uote"`,
		`back\slash`:               `"back\\slash"`,
		"bell\a del\x7f esc\x1b/":  `"bell\a del\177 esc\033/"`,
	} {
		if got := quotePath(in); got != want {
			t.Errorf("quotePath(%q) = %s, want %s", in, got, want)
		}
	}
}

// TestPushHoldQuotesANonUTF8Path: a review path that is not UTF-8 is held
// under its git-quoted name, not refused, and its list verifies.
func TestPushHoldQuotesANonUTF8Path(t *testing.T) {
	p, _, up, cp, _ := newAppLaneHold(t, reviewSpec(5, []string{".github/workflows/**"}), types.ApprovalApproved)
	body := recordedPush(t, BranchNSPrefix(p.runID)+"work", map[string]string{".github/workflows/b\xe9.yml": "on: push\n"})
	if rec := postPush(t, p, string(body)); rec.Code != http.StatusOK || len(up.gitBody) == 0 {
		t.Fatalf("status = %d body %q, want the approved push forwarded", rec.Code, rec.Body.String())
	}
	cp.mu.Lock()
	defer cp.mu.Unlock()
	if len(cp.raises) != 1 || cp.lists[0] == nil {
		t.Fatalf("raises = %d, want one with a path list", len(cp.raises))
	}
	want := []string{`".github/workflows/b\351.yml"`}
	if got := cp.raises[0].Paths; len(got) != 1 || got[0] != want[0] {
		t.Errorf("card paths = %q, want %q", got, want)
	}
	if err := cp.lists[0].VerifyAgainst(cp.raises[0]); err != nil {
		t.Errorf("the quoted list does not verify: %v", err)
	}
}

// TestPushHoldRefusedRaiseSaysDoNotRetry: a raise the control plane refuses
// (a 400, say) is not a transient failure, so the refusal does not tell git's
// user to push again.
func TestPushHoldRefusedRaiseSaysDoNotRetry(t *testing.T) {
	p, _, up, cp, _ := newAppLaneHold(t, reviewSpec(5, []string{".github/workflows/**"}), types.ApprovalPending)
	cp.mu.Lock()
	cp.refuse = http.StatusBadRequest
	cp.mu.Unlock()
	rec := postPush(t, p, string(recordedPush(t, BranchNSPrefix(p.runID)+"work", workflowPush)))
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "pushing again will be refused the same way") ||
		strings.Contains(rec.Body.String(), "retry the push") || len(up.gitBody) != 0 {
		t.Fatalf("status = %d body %q, want a 403 with the non-retry remedy", rec.Code, rec.Body.String())
	}
}
