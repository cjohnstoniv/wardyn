// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"go/ast"
	"go/token"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// sentenceConstFragments are named string consts in sentenceConstFiles that
// this guard deliberately does NOT require to start with a capital letter:
// each is composed into the MIDDLE of a larger sentence (after a colon, an em
// dash, or as a %s substitution) and is never itself shown as a sentence's
// first word — confirmed by reading every call site. May only shrink:
// promoting a fragment to a stand-alone sentence is exactly the #585 bug this
// guard exists to catch, so an entry is deleted once its const is fixed, not
// added to.
var sentenceConstFragments = map[string]string{
	"credSourceSSODesc":              "a noun phrase inside credSourceDesc's switch (runs_bedrock_probe.go), never a sentence on its own",
	"llmMechanismRemedyPerUser":      "the %s remedy clause inside llmMechanismDeadSentence / llmMechanismPinContradictedSentence",
	"llmMechanismRemedySharedFmt":    "the admin's %s remedy clause, same two sentences",
	"llmMechanismStateNotConfigured": "the %s state clause inside llmMechanismDeadSentence",
	"llmMechanismStateNotTheLane":    "the other %s state clause, same sentence",
}

// sentenceConstFiles are this package's non-test files whose top-level string
// consts are the named refusal/advisory sentence vocabulary #585 fixed: every
// one is either a complete, stand-alone sentence a person (not only a
// sidecar) can end up reading verbatim — which must start with a capital
// letter, the wire never puts an article in front of it — or a fragment
// listed above.
var sentenceConstFiles = []string{
	"awssso_pin.go", "awssso_refresh.go", "injection_ado.go",
	"injection_ado_signin.go", "injection_awssso.go", "runs_dispatch_llm_mechanism.go",
}

// TestRefusalSentenceConstsStartCapitalized is #585's guard: a lower-case
// regression on any sentence const #585 fixed, or a new one added the same
// way and left lower-case, turns this red. It also fails when
// sentenceConstFragments names a const that no longer exists or is no longer
// sentence-shaped, so the exclude list cannot go stale silently.
func TestRefusalSentenceConstsStartCapitalized(t *testing.T) {
	seenFragment := map[string]bool{}
	for _, f := range parseAPISources(t) {
		if !slices.Contains(sentenceConstFiles, f.name) {
			continue
		}
		ast.Inspect(f.file, func(n ast.Node) bool {
			gd, ok := n.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				return true
			}
			for _, spec := range gd.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok || len(vs.Names) != 1 || len(vs.Values) != 1 {
					continue
				}
				lit := leadingStringLiteral(vs.Values[0])
				if !looksLikeSentence(lit) {
					continue
				}
				name := vs.Names[0].Name
				if _, isFragment := sentenceConstFragments[name]; isFragment {
					seenFragment[name] = true
					continue
				}
				if r := []rune(lit)[0]; r >= 'a' && r <= 'z' {
					t.Errorf("%s: %s = %q — a user-visible sentence must start with a capital letter", f.name, name, lit)
				}
			}
			return true
		})
	}
	for name := range sentenceConstFragments {
		if !seenFragment[name] {
			t.Errorf("sentenceConstFragments lists %s, which no longer occurs (sentence-shaped) in %v — delete the entry", name, sentenceConstFiles)
		}
	}
}

// looksLikeSentence excludes short machine tokens (reason codes, mechanism
// names, env var names) that are lower-case on purpose: a sentence has a
// space and is long enough to be prose, never a bare identifier like
// "signin_busy" or "entra_signin".
func looksLikeSentence(s string) bool {
	return len(s) > 15 && strings.Contains(s, " ")
}

// leadingStringLiteral returns the first quoted string in a `"a" + "b" + ...`
// concatenation chain (a plain BasicLit is its own one-element chain), or ""
// when the expression is not a string literal or a concatenation of ones —
// which is how a non-string const (an int, a struct) and a composed
// expression with no leading literal (a bare fmt.Sprintf call) are skipped.
func leadingStringLiteral(e ast.Expr) string {
	for {
		switch v := e.(type) {
		case *ast.BasicLit:
			if v.Kind != token.STRING {
				return ""
			}
			s, err := strconv.Unquote(v.Value)
			if err != nil {
				return ""
			}
			return s
		case *ast.BinaryExpr:
			if v.Op != token.ADD {
				return ""
			}
			e = v.X
		default:
			return ""
		}
	}
}
