// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestManagedHomeTemplateDocMatchesTheValidator pins docs/OPERATIONS.md's
// managed-backend home-template rule to the function that enforces it.
//
// The defect this was written for: the rule WIDENED (0.7 refuses every non-hash
// template on a managed backend, where it previously refused only email_local)
// and OPERATIONS.md went on telling operators to "use `hash` (the default) or
// `sub`". An admin following that sentence gets a 400 from a page that told them
// to send it, and the sentence read as an ordinary recommendation rather than a
// stale one — nothing failed.
//
// It ASKS the validator which templates a managed backend accepts rather than
// restating an answer, so a future widening or narrowing changes the expected
// documentation with it.
func TestManagedHomeTemplateDocMatchesTheValidator(t *testing.T) {
	accepted, refused := managedTemplateVerdicts(t)
	if len(accepted) == 0 || len(refused) == 0 {
		t.Fatalf("managed backends accept %v and refuse %v — one side is empty, so this guard would grade nothing; re-derive it", accepted, refused)
	}

	root := repoRoot(t)
	b, err := os.ReadFile(filepath.Join(root, "docs", "OPERATIONS.md"))
	if err != nil {
		t.Fatalf("read docs/OPERATIONS.md: %v", err)
	}
	// Scoped to the PARAGRAPH that states the rule — located by the refusal's
	// own wording rather than by a line number (which every edit above it
	// moves), and whitespace-normalized because the doc hard-wraps: the quoted
	// refusal spans a line break today and would span a different one after any
	// reflow, so a guard matching the current wrap would be a guard about
	// formatting.
	//
	// One paragraph, not a character window: the SHARE paragraphs a few lines
	// away legitimately recommend `sub` and `email_local`, because a share
	// backend keeps every template. A window wide enough to be safe against
	// re-wrapping is wide enough to read those as managed advice.
	const anchor = "is not allowed on a managed backend"
	var window string
	for _, para := range strings.Split(string(b), "\n\n") {
		flat := strings.Join(strings.Fields(para), " ")
		if strings.Contains(flat, anchor) {
			window += flat + " "
		}
	}
	if window == "" {
		t.Fatalf("docs/OPERATIONS.md no longer quotes %q — the managed-backend home-template rule is the one an admin hits as a 400, and it has to be stated where drives are documented", anchor)
	}

	for _, tmpl := range accepted {
		if !strings.Contains(window, "`"+string(tmpl)+"`") {
			t.Errorf("docs/OPERATIONS.md's managed-backend paragraph never names %q, the only home template a managed backend ACCEPTS — the remedy the 400 gives is the one thing this passage has to carry", tmpl)
		}
	}
	// The failure mode is a REFUSED template recommended as a remedy: "use
	// `hash` (the default) or `sub`". Match the recommendation shape, not the
	// bare mention — the passage legitimately names refused templates while
	// explaining WHY they are refused.
	for _, tmpl := range refused {
		for _, phrase := range []string{
			"use `hash` (the default) or `" + string(tmpl) + "`",
			"use `" + string(tmpl) + "`",
			"or `" + string(tmpl) + "` instead",
		} {
			if strings.Contains(window, phrase) {
				t.Errorf("docs/OPERATIONS.md recommends %q for a managed backend (%q), but types.ValidateUserDrive REFUSES it with a 400 — an admin following that sentence gets an error from the page that told them to send it", tmpl, phrase)
			}
		}
	}
}

// managedTemplateVerdicts asks types.ValidateUserDrive, over every managed
// backend and every template in the closed set, which templates survive.
// A template accepted by EVERY managed backend is accepted; one refused by every
// managed backend is refused. (A split verdict would be a third case this guard
// deliberately does not invent a documentation rule for — it fails loudly via
// the empty-side check above only if that split emptied one list.)
func managedTemplateVerdicts(t *testing.T) (accepted, refused []types.HomeTemplate) {
	t.Helper()
	for _, tmpl := range types.HomeTemplates {
		ok, bad := 0, 0
		managed := 0
		for _, backend := range types.DriveBackends {
			if backend.Kind() != types.DriveKindManaged {
				continue
			}
			managed++
			d := types.UserDrive{
				Name:         "guard",
				Backend:      backend,
				HomeTemplate: tmpl,
				// k8s_pvc refuses size 0 on its own (the claim's storage
				// request); irrelevant to the template rule, so give it one.
				SizeMiB: 1024,
			}
			err := types.ValidateUserDrive(&d, backend.RunnerTarget())
			switch {
			case err == nil:
				ok++
			case strings.Contains(err.Error(), "is not allowed on a managed backend"):
				bad++
			default:
				t.Fatalf("backend %s + template %s refused for an unrelated reason (%v) — this guard's probe drive no longer clears the other validations", backend, tmpl, err)
			}
		}
		if managed == 0 {
			t.Fatal("types.DriveBackends contains no managed backend — the enumeration, not the docs, is what changed")
		}
		switch {
		case ok == managed:
			accepted = append(accepted, tmpl)
		case bad == managed:
			refused = append(refused, tmpl)
		}
	}
	return accepted, refused
}
