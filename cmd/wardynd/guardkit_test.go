// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

// The guard tests in this package used to carry three different function-body
// extractors (two textual, one go/parser-based) and four near-identical "read
// a repo file" helpers. A textual extractor that runs to the next top-level
// `func` folds that function's own doc comment into the "body" a guard
// searches — a false-positive risk a parser does not have. This file is the
// one correct version of each: readRepo for reading a file, citedSymbolBodies
// for resolving a symbol's own source span. The package's other guard files
// keep their old helper names as thin wrappers over these two, so neither this
// move nor a rewrite of the wrapped logic touches every call site.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// readRepo reads one file, given a path relative to the repo root (or one
// that is already absolute, for a caller that joined the root itself),
// failing the test if it is missing — a guard that silently skips its own
// subject passes vacuously forever.
func readRepo(t *testing.T, rel string) string {
	t.Helper()
	path := rel
	if !filepath.IsAbs(rel) {
		path = filepath.Join(repoRoot(t), filepath.FromSlash(rel))
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

// mdHeading matches one markdown heading line, capturing its level and text.
var mdHeading = regexp.MustCompile(`^(#{1,6})\s+(.*)$`)

// headingSlug renders a markdown heading the way GitHub anchors it: lower-cased,
// everything but letters, digits, spaces and hyphens dropped, spaces to hyphens.
// So `## ` + "`wardynd` (control plane)" anchors as wardynd-control-plane, and a
// citation to it is a link a reader can actually follow.
func headingSlug(text string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(text)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			b.WriteRune(r)
		case r == ' ' || r == '_':
			b.WriteRune('-')
		}
	}
	out := b.String()
	for strings.Contains(out, "--") {
		out = strings.ReplaceAll(out, "--", "-")
	}
	return strings.Trim(out, "-")
}

// citedSymbolBodies returns, for one cited file, the source text a citation into
// it is allowed to look at, keyed by symbol; literals, the emitting-position
// string constants found in that same symbol's body (see
// stringLiteralsExcludingReaders); and fileLiterals, the same but unscoped —
// every emitting-position string literal anywhere in the file, for a bare-path
// citation (which names no symbol to scope to).
//
// For Go that is the symbol's OWN BODY and nothing else: the braces of a func or
// method, or the spec of a package-level const/var/type. Deliberately not the
// doc comment and not the signature — a comment that mentions an action is not
// an emit of it, and the file-wide search this replaces is exactly how a
// citation could name the wrong function and still resolve.
//
// For markdown the unit is the heading's SECTION: the heading line down to the
// next heading of the same or a higher level. A doc has no symbols, and the
// section is the smallest thing a reader can be sent to that still contains the
// claim; markdown has no Go tokens, so literals and fileLiterals are both nil.
func citedSymbolBodies(rel string, src []byte) (bodies map[string]string, literals map[string][]string, fileLiterals []string, err error) {
	bodies = map[string]string{}
	if strings.HasSuffix(rel, ".md") {
		lines := strings.Split(string(src), "\n")
		for i, line := range lines {
			m := mdHeading.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			level := len(m[1])
			end := len(lines)
			for j := i + 1; j < len(lines); j++ {
				if h := mdHeading.FindStringSubmatch(lines[j]); h != nil && len(h[1]) <= level {
					end = j
					break
				}
			}
			slug := headingSlug(m[2])
			if slug != "" {
				if _, dup := bodies[slug]; !dup {
					bodies[slug] = strings.Join(lines[i:end], "\n")
				}
			}
		}
		return bodies, nil, nil, nil
	}

	fset := token.NewFileSet()
	f, ferr := parser.ParseFile(fset, rel, src, 0)
	if ferr != nil {
		return nil, nil, nil, ferr
	}
	span := func(from, to token.Pos) string {
		a, b := fset.Position(from).Offset, fset.Position(to).Offset
		if a < 0 || b > len(src) || a >= b {
			return ""
		}
		return string(src[a:b])
	}
	literals = map[string][]string{}
	for _, d := range f.Decls {
		switch v := d.(type) {
		case *ast.FuncDecl:
			name := v.Name.Name
			if v.Recv != nil && len(v.Recv.List) > 0 {
				name = receiverTypeName(v.Recv.List[0].Type) + "." + name
			}
			if v.Body == nil {
				bodies[name] = "" // declared without a body: resolvable, never evidence
				continue
			}
			bodies[name] = span(v.Body.Lbrace, v.Body.Rbrace)
			literals[name] = stringLiteralsExcludingReaders(v.Body)
		case *ast.GenDecl:
			for _, sp := range v.Specs {
				switch s := sp.(type) {
				case *ast.ValueSpec:
					for _, n := range s.Names {
						bodies[n.Name] = span(s.Pos(), s.End())
						literals[n.Name] = stringLiteralsExcludingReaders(s)
					}
				case *ast.TypeSpec:
					bodies[s.Name.Name] = span(s.Pos(), s.End())
					literals[s.Name.Name] = stringLiteralsExcludingReaders(s)
				}
			}
		}
	}
	return bodies, literals, stringLiteralsExcludingReaders(f), nil
}

// stringLiteralsExcludingReaders walks node and returns the unquoted value of
// every string BasicLit found — except one used merely to READ or DISPATCH ON
// an action rather than emit it: the operand of a `==`/`!=` comparison
// (`ev.Action != "egress.deny"`), or a switch's case expression. A doc comment
// or a line comment is never visited at all, because a comment is not part of
// the expression tree — which is what makes this immune to a wildcard prefix
// quoted only in prose (`// e.g. "egress."`), the false positive a raw
// substring scan of the source text could not tell apart from a real emit.
//
// This is deliberately NOT "is this the argument to an audit-emitting call":
// that would need to know every such call by name, and go stale exactly like
// the citations this guard exists to keep honest. "Not a comparison and not a
// dispatch" is the cheap, call-agnostic half of "probably an emit" — sufficient
// to fail on both false positives this guard was found vacuous against
// (AuditFilter's doc-comment example and handleObservedEgress's read-only
// filter), without hand-listing every real emit site.
func stringLiteralsExcludingReaders(node ast.Node) []string {
	var out []string
	var ancestors []ast.Node
	excluded := func(lit *ast.BasicLit) bool {
		if len(ancestors) == 0 {
			return false
		}
		switch p := ancestors[len(ancestors)-1].(type) {
		case *ast.BinaryExpr:
			return p.Op == token.EQL || p.Op == token.NEQ
		case *ast.CaseClause:
			for _, e := range p.List {
				if e == ast.Expr(lit) {
					return true
				}
			}
		}
		return false
	}
	ast.Inspect(node, func(n ast.Node) bool {
		if n == nil {
			ancestors = ancestors[:len(ancestors)-1]
			return true
		}
		if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING && !excluded(lit) {
			if v, uerr := strconv.Unquote(lit.Value); uerr == nil {
				out = append(out, v)
			}
		}
		ancestors = append(ancestors, n)
		return true
	})
	return out
}

// receiverTypeName is the bare type name a method hangs off — the `Server` in
// `func (s *Server) foo()`, so the citation reads `Server.foo` whether the
// receiver is a pointer, a value or generic.
func receiverTypeName(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.StarExpr:
		return receiverTypeName(v.X)
	case *ast.Ident:
		return v.Name
	case *ast.IndexExpr:
		return receiverTypeName(v.X)
	case *ast.IndexListExpr:
		return receiverTypeName(v.X)
	}
	return ""
}

// funcBody returns the source of one function or method's BODY (the braces,
// not the signature), resolved with citedSymbolBodies against the whole file
// `src`. A method is named the way its declaration reads — "(p *Proxy)
// egressTarget" — since that shape doubled as a plain func name here before
// citedSymbolBodies existed; it is translated to citedSymbolBodies' own
// "Type.Method" key.
func funcBody(t *testing.T, src, name string) string {
	t.Helper()
	sym := name
	if strings.HasPrefix(name, "(") {
		closeParen := strings.Index(name, ")")
		if closeParen < 0 {
			t.Fatalf("funcBody: malformed method name %q", name)
		}
		recv := strings.Fields(name[1:closeParen])
		if len(recv) == 0 {
			t.Fatalf("funcBody: malformed method name %q", name)
		}
		typ := strings.TrimPrefix(recv[len(recv)-1], "*")
		sym = typ + "." + strings.TrimSpace(name[closeParen+1:])
	}
	bodies, _, _, err := citedSymbolBodies("src.go", []byte(src))
	if err != nil {
		t.Fatalf("funcBody: parse: %v", err)
	}
	body, ok := bodies[sym]
	if !ok {
		t.Fatalf("func %s not found — the guard's anchor moved, so it is asserting nothing", name)
	}
	return body
}

// methodBody returns a method's body by its bare name, whatever type it hangs
// off. A name two types share is refused rather than resolved: map order would
// pick either body, and an absence check could then pass on the wrong one.
func methodBody(t *testing.T, src, name string) string {
	t.Helper()
	bodies, _, _, err := citedSymbolBodies("src.go", []byte(src))
	if err != nil {
		t.Fatalf("methodBody: parse: %v", err)
	}
	var matches []string
	for sym := range bodies {
		if strings.HasSuffix(sym, "."+name) {
			matches = append(matches, sym)
		}
	}
	switch len(matches) {
	case 0:
		t.Fatalf("method %s not found — the guard's anchor moved, so it is asserting nothing", name)
	case 1:
		return bodies[matches[0]]
	}
	t.Fatalf("method %s is ambiguous (%v) — cite it as Type.Method", name, matches)
	return ""
}
