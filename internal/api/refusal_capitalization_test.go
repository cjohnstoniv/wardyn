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
	"mpSubNotSignedIn":               "the %s state clause inside mpRunRefusal (run_model_provider.go), never shown alone",
	"mpSubNoStore":                   "the %s state clause inside mpRunRefusal, same sentence",
	"mpSubNotPerson":                 "the %s state clause inside mpRunRefusal, same sentence",
	"mpSubNoImage":                   "the %s state clause inside mpRunRefusal, same sentence",
	"mpRunRemedySignIn":              "the %s remedy clause inside mpRunRefusal, same sentence",
	"mpRunRemedyPerson":              "the %s remedy clause inside mpRunRefusal, same sentence",
}

// sentenceConstNotSentences are named string consts in sentenceConstFiles
// that #585 never covered at all — its rule is "api + console" sentences,
// and these are something else entirely, lower-case on purpose. May only
// shrink, same discipline as sentenceConstFragments.
var sentenceConstNotSentences = map[string]string{
	"EntraAuthorityOverrideWarn": `opens with the literal "wardynd: TEST HATCH ACTIVE" on purpose (own doc comment) — a boot log line, never rendered by the console or the API`,
	"EntraAuthorityOverrideRefusal": "a Go error string returned from fmt.Errorf, not an HTTP/console sentence — " +
		"staticcheck ST1005 refuses a capitalised one so it composes when wrapped with %w",
}

// sentenceConstFiles are the ONLY files this guard scans — every non-test
// internal/api file with a top-level string const #585 (or its F2 fix round)
// capitalised. Each surviving const is either a complete, stand-alone
// sentence a person (not only a sidecar) can end up reading verbatim — which
// must start with a capital letter, the wire never puts an article in front
// of it — or a fragment listed above.
//
// This is a NAMED-CONST scanner, not a whole-package one: it cannot and does
// not see a sentence written as an inline literal inside a function body
// (uigateway.go's writeError bodies, runs_autonomy.go's autonomyAgentLabel,
// runs_autonomy_bedrock.go's and runs_dispatch_ado_inject.go's
// failAndRevoke details, dispatchRun's own two failAndRevoke calls in
// runs_dispatch.go) even where #585 fixed one — those were fixed by hand and
// stay that way by hand. Adding a file here
// that has no sentence-shaped top-level const changes nothing; adding one
// that does makes every such const in it subject to this check, fragments
// aside — grow this list deliberately, not by habit.
var sentenceConstFiles = []string{
	"awssso_pin.go", "awssso_refresh.go", "injection_ado.go",
	"injection_ado_signin.go", "injection_awssso.go", "runs_dispatch_llm_mechanism.go",
	"injection_provider_key.go", "provider_subscription.go", "approvals_push.go",
	"ado_entra.go", "injection_ado_capability.go",
}

// TestRefusalSentenceConstsStartCapitalized is #585's guard, scoped to
// exactly sentenceConstFiles' top-level string consts (see its own comment
// for what that does and does not cover): a lower-case regression on any
// sentence const #585 fixed, or a new sentence-shaped const added to one of
// these files and left lower-case, turns this red. It also fails when
// sentenceConstFragments names a const that no longer exists or is no longer
// sentence-shaped, so the exclude list cannot go stale silently.
func TestRefusalSentenceConstsStartCapitalized(t *testing.T) {
	seenFragment := map[string]bool{}
	seenNotSentence := map[string]bool{}
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
				if _, notSentence := sentenceConstNotSentences[name]; notSentence {
					seenNotSentence[name] = true
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
	for name := range sentenceConstNotSentences {
		if !seenNotSentence[name] {
			t.Errorf("sentenceConstNotSentences lists %s, which no longer occurs (sentence-shaped) in %v — delete the entry", name, sentenceConstFiles)
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
