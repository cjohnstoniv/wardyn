// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The two emit shapes, PLUS the context override. auditAuthFailed is the
// public-lane wrapper around auditAuthFailedAs, so walking those two calls
// covers every call site — but not every reason that reaches the row:
// auditAuthFailedAs REPLACES its argument with oidc.SessionRejectedFromContext
// when the middleware set one, and those four values are declared in
// internal/auth/oidc and appear at no call site at all. withSessionRejectedCall
// walks them too, so a fifth rejection reason added upstream cannot land in
// auth.fail with this fence green.
//
// `(?s)` and `[^)]` (rather than `[^),]`) so a call gofmt has wrapped across
// lines is still read — a multi-line emit was invisible to the first version of
// this guard, and the count floor below is too coarse to notice one.
var (
	authFailedCall          = regexp.MustCompile(`(?s)auditAuthFailed\(r,\s*([^),]+)\)`)
	authFailedAsCall        = regexp.MustCompile(`(?s)auditAuthFailedAs\(r,\s*([^,)]+),\s*([^),]+)\)`)
	withSessionRejectedCall = regexp.MustCompile(`withSessionRejected\([^,]+,\s*"([^"]+)"\)`)
	// A CONST line of the shape `name = "value"` — how both the reasons and the
	// *Actor names are declared. Scanned only inside a const declaration, so a
	// local `reason = "..."` assignment in some handler cannot shadow one.
	goStringConst = regexp.MustCompile(`^\s*(?:const\s+)?(\w+)\s*=\s*"([^"]*)"\s*$`)
	// A CONST line of the shape `name = pkg.Ident` — the one-spelling alias G4
	// prefers over a second string literal (authFailedUserTypeUnknown =
	// oidc.DenialUserTypeUnknown). Resolved in a second pass once the aliased
	// package's own consts are known.
	goQualifiedConstAlias = regexp.MustCompile(`^\s*(?:const\s+)?(\w+)\s*=\s*(\w+\.\w+)\s*$`)
	// authFailedForward is auditAuthFailed's own one-line body forwarding its
	// PARAMETER. Removed before the walk: it is the wrapper, not an emit, and
	// leaving it in would make "the reason is a constant" unenforceable for
	// every real call site.
	authFailedForward = "s.auditAuthFailedAs(r, adminAuthActor, reason)"
)

// constLines walks src's const blocks and single-line `const x = ...`
// declarations only, never a function body, applying re to each candidate
// line and recording its two capture groups into into.
func constLines(src string, re *regexp.Regexp, into map[string]string) {
	inBlock := false
	for _, line := range strings.Split(src, "\n") {
		switch {
		case strings.HasPrefix(line, "const ("):
			inBlock = true
			continue
		case inBlock && strings.HasPrefix(line, ")"):
			inBlock = false
			continue
		case !inBlock && !strings.HasPrefix(line, "const "):
			continue
		}
		if m := re.FindStringSubmatch(line); m != nil {
			into[m[1]] = m[2]
		}
	}
}

// constStrings returns the string constants one Go source declares.
func constStrings(src string, into map[string]string) { constLines(src, goStringConst, into) }

// constAliases returns the qualified-identifier constants one Go source
// declares (name -> "pkg.Ident"), e.g. authFailedUserTypeUnknown ->
// "oidc.DenialUserTypeUnknown".
func constAliases(src string, into map[string]string) { constLines(src, goQualifiedConstAlias, into) }

// TestAuthFailedReasonEnumIsDocumented is the fence under AUDIT-ACTIONS.md's
// claim that `auth.fail`'s `reason` is a CLOSED ENUM — the row an operator
// builds a SIEM rule from.
//
// It exists because the claim drifted the moment it was tested: the CSRF lane
// added `cross_origin_refused`, said in csrf.go that it "joins the enum the
// auth.fail row documents", and the row was then left without it through a
// hand-resolved rebase conflict. Nothing went red, because nothing tied the
// emits to the doc.
//
// The ACTOR half is here for the same reason and in the same row: the cell
// promises the actor "names WHICH boundary refused", so a new boundary that is
// not listed makes the promise false.
func TestAuthFailedReasonEnumIsDocumented(t *testing.T) {
	root := repoRoot(t)
	apiDir := filepath.Join(root, "internal", "api")
	entries, err := os.ReadDir(apiDir)
	if err != nil {
		t.Fatalf("read internal/api: %v", err)
	}

	consts := map[string]string{}
	aliases := map[string]string{}
	var sources []string
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		b, rerr := os.ReadFile(filepath.Join(apiDir, name))
		if rerr != nil {
			t.Fatalf("read internal/api/%s: %v", name, rerr)
		}
		src := strings.Replace(string(b), authFailedForward, "", 1)
		sources = append(sources, src)
		constStrings(src, consts)
		constAliases(src, aliases)
	}

	// oidcConsts, qualified "oidc.Name": authFailedUserTypeUnknown is declared
	// AS oidc.DenialUserTypeUnknown (one spelling, not a second literal G4
	// would flag), so a call site naming it reads a constant this package
	// never declares itself.
	oidcConsts := map[string]string{}
	rawOidcConsts := map[string]string{}
	oidcSrcDir := filepath.Join(root, "internal", "auth", "oidc")
	oidcSrcEntries, err := os.ReadDir(oidcSrcDir)
	if err != nil {
		t.Fatalf("read internal/auth/oidc: %v", err)
	}
	for _, e := range oidcSrcEntries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		b, rerr := os.ReadFile(filepath.Join(oidcSrcDir, name))
		if rerr != nil {
			t.Fatalf("read internal/auth/oidc/%s: %v", name, rerr)
		}
		constStrings(string(b), rawOidcConsts)
	}
	for k, v := range rawOidcConsts {
		oidcConsts["oidc."+k] = v
	}
	// Fold a resolved alias (authFailedUserTypeUnknown -> oidc.DenialUserTypeUnknown
	// -> "user_type_unknown") into consts under its own LOCAL name, so a call
	// site naming the alias resolves exactly as one naming the literal would.
	for local, qualified := range aliases {
		if v, ok := oidcConsts[qualified]; ok {
			consts[local] = v
		}
	}

	resolve := func(arg string) (string, bool) {
		arg = strings.TrimSpace(arg)
		if strings.HasPrefix(arg, `"`) && strings.HasSuffix(arg, `"`) {
			return strings.Trim(arg, `"`), true
		}
		if v, ok := consts[arg]; ok {
			return v, true
		}
		v, ok := oidcConsts[arg]
		return v, ok
	}

	reasons, actors := map[string]bool{}, map[string]bool{}
	for _, src := range sources {
		for _, m := range authFailedCall.FindAllStringSubmatch(src, -1) {
			v, ok := resolve(m[1])
			if !ok {
				t.Errorf("auditAuthFailed reason %q is neither a literal nor a resolvable internal/api constant", m[1])
				continue
			}
			reasons[v] = true
			actors[consts["adminAuthActor"]] = true // the wrapper's fixed boundary
		}
		for _, m := range authFailedAsCall.FindAllStringSubmatch(src, -1) {
			actor, aok := resolve(m[1])
			reason, rok := resolve(m[2])
			if !aok || !rok {
				t.Errorf("auditAuthFailedAs(%q, %q) does not resolve to constants", m[1], m[2])
				continue
			}
			actors[actor] = true
			reasons[reason] = true
		}
	}
	// A regex that stopped matching would make this vacuously green.
	if len(reasons) < 10 || len(actors) < 4 {
		t.Fatalf("found %d reasons and %d actors; the emits number ~15 and 5 — the walk stopped enumerating", len(reasons), len(actors))
	}

	// The reasons that never appear at a call site: oidc.Middleware stamps them
	// on the context and auditAuthFailedAs substitutes them for whatever the
	// caller passed (http.go's session-rejection override).
	oidcDir := filepath.Join(root, "internal", "auth", "oidc")
	oidcFiles, err := os.ReadDir(oidcDir)
	if err != nil {
		t.Fatalf("read internal/auth/oidc: %v", err)
	}
	rejections := 0
	for _, e := range oidcFiles {
		if !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		b, rerr := os.ReadFile(filepath.Join(oidcDir, e.Name()))
		if rerr != nil {
			t.Fatalf("read internal/auth/oidc/%s: %v", e.Name(), rerr)
		}
		for _, m := range withSessionRejectedCall.FindAllStringSubmatch(string(b), -1) {
			reasons[m[1]] = true
			rejections++
		}
	}
	if rejections < 4 {
		t.Fatalf("found %d withSessionRejected reasons; oidc.Middleware stamps 4 — the walk stopped enumerating", rejections)
	}

	row := auditDocRowFor(t, root, "auth.fail")
	for reason := range reasons {
		if !strings.Contains(row, "`"+reason+"`") {
			t.Errorf("auth.fail reason %q is emitted but is NOT in docs/AUDIT-ACTIONS.md's closed enum", reason)
		}
	}
	for actor := range actors {
		if !strings.Contains(row, "`"+actor+"`") {
			t.Errorf("auth.fail actor %q refuses requests but is NOT named in docs/AUDIT-ACTIONS.md's row", actor)
		}
	}
}

// auditDocRowFor returns the AUDIT-ACTIONS.md table row for one action.
func auditDocRowFor(t *testing.T, root, action string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, "docs", "AUDIT-ACTIONS.md"))
	if err != nil {
		t.Fatalf("read docs/AUDIT-ACTIONS.md: %v", err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "| `"+action+"` |") {
			return line
		}
	}
	t.Fatalf("docs/AUDIT-ACTIONS.md has no row for %q", action)
	return ""
}
