// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

// Every stored-secret read is audited exactly once (credential-storage design,
// fail-closed rule 23): either the secretstore.Audited decorator records it,
// with the purpose its caller put in the context (secretstore.WithPurpose), or
// the call site records its own richer secret.read and marks the context
// secretstore.SiteAudited so the decorator stays silent. A Get with neither
// mark would be recorded with no purpose; a SiteAudited mark on a site that
// records nothing would be a read with no record at all.
//
// This guard makes that mechanical, with the type checker rather than by name:
// it finds every call of Get on a secret store (any value whose type implements
// secretstore.Store, or an interface secretstore.Store satisfies) in every
// non-test package outside internal/secretstore, and requires its context
// argument to be marked — a WithPurpose or SiteAudited call, or a variable
// assigned from one in the same function. An unmarked Get makes its enclosing
// function need a marked context from every caller; the walk climbs the static
// references until each path is marked. A function nobody references (an HTTP
// handler, a method only an interface calls) is where an unmarked path ends,
// and fails the guard. Every function that calls SiteAudited must itself record
// a "secret.read".

import (
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const (
	guardModPath     = "github.com/cjohnstoniv/wardyn"
	guardStorePkg    = guardModPath + "/internal/secretstore"
	guardWithPurpose = guardStorePkg + ".WithPurpose"
	guardSiteAudited = guardStorePkg + ".SiteAudited"
)

type listedPkg struct {
	ImportPath, Dir, Export string
	GoFiles                 []string
}

// secretRef is one static reference to a function: from which function, and
// whether it is a call whose context argument is marked.
type secretRef struct {
	from   string
	pos    string
	marked bool
}

type secretReadScan struct {
	fset      *token.FileSet
	storeType *types.Interface
	getSites  map[string][]secretRef // enclosing function -> its Get calls
	refs      map[string][]secretRef // function -> references to it
	siteMarks map[string]string      // function calling SiteAudited -> its position
	emitters  map[string]bool        // function whose body holds the literal "secret.read"
	purposes  int
}

func goListExport(t *testing.T, root string) []listedPkg {
	t.Helper()
	cmd := exec.Command("go", "list", "-deps", "-export", "-json=ImportPath,Dir,Export,GoFiles", "./...")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			t.Fatalf("go list: %v\n%s", err, ee.Stderr)
		}
		t.Fatalf("go list: %v", err)
	}
	var pkgs []listedPkg
	dec := json.NewDecoder(strings.NewReader(string(out)))
	for dec.More() {
		var p listedPkg
		if err := dec.Decode(&p); err != nil {
			t.Fatalf("decode go list: %v", err)
		}
		pkgs = append(pkgs, p)
	}
	return pkgs
}

func funcKey(obj types.Object) string {
	if f, ok := obj.(*types.Func); ok {
		return f.Origin().FullName()
	}
	return ""
}

func calleeKey(info *types.Info, call *ast.CallExpr) string {
	fun := ast.Unparen(call.Fun)
	switch f := fun.(type) {
	case *ast.IndexExpr:
		fun = f.X
	case *ast.IndexListExpr:
		fun = f.X
	}
	switch f := fun.(type) {
	case *ast.Ident:
		return funcKey(info.Uses[f])
	case *ast.SelectorExpr:
		return funcKey(info.Uses[f.Sel])
	}
	return ""
}

func (s *secretReadScan) isStore(t types.Type) bool {
	if types.Implements(t, s.storeType) || types.Implements(types.NewPointer(t), s.storeType) {
		return true
	}
	iface, ok := t.Underlying().(*types.Interface)
	return ok && types.Implements(s.storeType, iface)
}

// markedVars are the variables a function assigns from a mark call.
func markedVars(info *types.Info, body *ast.BlockStmt) map[types.Object]bool {
	vars := map[types.Object]bool{}
	ast.Inspect(body, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Rhs) != 1 || len(as.Lhs) == 0 {
			return true
		}
		call, ok := as.Rhs[0].(*ast.CallExpr)
		if !ok {
			return true
		}
		if k := calleeKey(info, call); k != guardWithPurpose && k != guardSiteAudited {
			return true
		}
		if id, ok := as.Lhs[0].(*ast.Ident); ok {
			if obj := info.ObjectOf(id); obj != nil {
				vars[obj] = true
			}
		}
		return true
	})
	return vars
}

func ctxMarked(info *types.Info, vars map[types.Object]bool, call *ast.CallExpr) bool {
	if len(call.Args) == 0 {
		return false
	}
	switch a := ast.Unparen(call.Args[0]).(type) {
	case *ast.CallExpr:
		k := calleeKey(info, a)
		return k == guardWithPurpose || k == guardSiteAudited
	case *ast.Ident:
		return vars[info.ObjectOf(a)]
	}
	return false
}

func (s *secretReadScan) scanFunc(info *types.Info, fd *ast.FuncDecl) {
	from := funcKey(info.Defs[fd.Name])
	vars := markedVars(info, fd.Body)
	callOf := map[*ast.Ident]*ast.CallExpr{}
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.BasicLit:
			if v.Value == `"secret.read"` {
				s.emitters[from] = true
			}
		case *ast.CallExpr:
			s.scanCall(info, vars, from, v, callOf)
		case *ast.Ident:
			to := funcKey(info.Uses[v])
			if to == "" || to == from || strings.HasPrefix(to, guardStorePkg+".") {
				return true
			}
			r := secretRef{from: from, pos: s.fset.Position(v.Pos()).String()}
			if call := callOf[v]; call != nil {
				r.marked = ctxMarked(info, vars, call)
			}
			s.refs[to] = append(s.refs[to], r)
		}
		return true
	})
}

// scanCall records a Get site or a mark call, and notes which identifier names
// the callee so the identifier's reference knows it is a call.
func (s *secretReadScan) scanCall(info *types.Info, vars map[types.Object]bool, from string, call *ast.CallExpr, callOf map[*ast.Ident]*ast.CallExpr) {
	fun := ast.Unparen(call.Fun)
	switch f := fun.(type) {
	case *ast.IndexExpr:
		fun = f.X
	case *ast.IndexListExpr:
		fun = f.X
	}
	switch f := fun.(type) {
	case *ast.Ident:
		callOf[f] = call
	case *ast.SelectorExpr:
		callOf[f.Sel] = call
		if sel := info.Selections[f]; sel != nil && f.Sel.Name == "Get" && sel.Kind() == types.MethodVal && s.isStore(sel.Recv()) {
			s.getSites[from] = append(s.getSites[from], secretRef{
				from: from, pos: s.fset.Position(call.Pos()).String(), marked: ctxMarked(info, vars, call),
			})
		}
	}
	switch calleeKey(info, call) {
	case guardSiteAudited:
		s.siteMarks[from] = s.fset.Position(call.Pos()).String()
	case guardWithPurpose:
		s.purposes++
	}
}

func scanSecretReads(t *testing.T, root string) *secretReadScan {
	t.Helper()
	pkgs := goListExport(t, root)
	exports := map[string]string{}
	for _, p := range pkgs {
		exports[p.ImportPath] = p.Export
	}
	s := &secretReadScan{
		fset:      token.NewFileSet(),
		getSites:  map[string][]secretRef{},
		refs:      map[string][]secretRef{},
		siteMarks: map[string]string{},
		emitters:  map[string]bool{},
	}
	imp := importer.ForCompiler(s.fset, "gc", func(path string) (io.ReadCloser, error) {
		if exports[path] == "" {
			return nil, fmt.Errorf("no export data for %s", path)
		}
		return os.Open(exports[path])
	})
	ss, err := imp.Import(guardStorePkg)
	if err != nil {
		t.Fatalf("import %s: %v", guardStorePkg, err)
	}
	s.storeType = ss.Scope().Lookup("Store").Type().Underlying().(*types.Interface)

	for _, p := range pkgs {
		if !strings.HasPrefix(p.ImportPath, guardModPath+"/") || p.ImportPath == guardStorePkg ||
			strings.HasPrefix(p.ImportPath, guardStorePkg+"/") || len(p.GoFiles) == 0 {
			continue
		}
		var files []*ast.File
		for _, name := range p.GoFiles {
			f, perr := parser.ParseFile(s.fset, filepath.Join(p.Dir, name), nil, parser.SkipObjectResolution)
			if perr != nil {
				t.Fatalf("parse %s: %v", name, perr)
			}
			files = append(files, f)
		}
		info := &types.Info{
			Uses:       map[*ast.Ident]types.Object{},
			Defs:       map[*ast.Ident]types.Object{},
			Selections: map[*ast.SelectorExpr]*types.Selection{},
		}
		conf := types.Config{Importer: imp.(types.ImporterFrom)}
		if _, cerr := conf.Check(p.ImportPath, s.fset, files, info); cerr != nil {
			t.Fatalf("type-check %s: %v", p.ImportPath, cerr)
		}
		for _, f := range files {
			for _, d := range f.Decls {
				if fd, ok := d.(*ast.FuncDecl); ok && fd.Body != nil {
					s.scanFunc(info, fd)
				}
			}
		}
	}
	return s
}

// unmarkedPaths climbs from each unmarked Get through the functions whose
// callers do not mark the context, and returns, per Get, the first path that
// reaches a function nothing marks — one line per Get, so fixing one path never
// hides another Get's.
func (s *secretReadScan) unmarkedPaths() []string {
	var bad []string
	for fn, sites := range s.getSites {
		for _, g := range sites {
			if !g.marked {
				if line := s.firstUnmarked(fn, "Get at "+g.pos+" in "+fn); line != "" {
					bad = append(bad, line)
				}
			}
		}
	}
	slices.Sort(bad)
	return bad
}

func (s *secretReadScan) firstUnmarked(start, why string) string {
	chain := map[string]string{start: why}
	queue := []string{start}
	for len(queue) > 0 {
		fn := queue[0]
		queue = queue[1:]
		refs := s.refs[fn]
		if len(refs) == 0 {
			return chain[fn] + " — nothing that references it marks the context"
		}
		for _, r := range refs {
			switch {
			case r.marked:
			case r.from == "":
				return chain[fn] + " <- package-level reference at " + r.pos
			case chain[r.from] == "":
				chain[r.from] = chain[fn] + " <- " + r.from + " (" + r.pos + ")"
				queue = append(queue, r.from)
			}
		}
	}
	return ""
}

func TestEverySecretReadIsAuditedOnce(t *testing.T) {
	s := scanSecretReads(t, repoRoot(t))

	// Guard the guard: the scan must see the reads this rule was written for.
	for _, fn := range []string{
		guardModPath + "/cmd/wardynd.loadOrCreateSecret",
		guardModPath + "/internal/api.readHarnessBlob",
		"(*" + guardModPath + "/internal/api.Server).handleInternalInjection",
		"(*" + guardModPath + "/internal/broker.Broker).mintGitPAT",
	} {
		if len(s.getSites[fn]) == 0 {
			t.Errorf("the scan found no secret-store Get in %s; the guard no longer sees the reads it pins", fn)
		}
	}
	if len(s.siteMarks) == 0 || s.purposes == 0 {
		t.Errorf("the scan found %d SiteAudited and %d WithPurpose calls; it no longer sees the marks", len(s.siteMarks), s.purposes)
	}

	for _, line := range s.unmarkedPaths() {
		t.Errorf("secret read with no audit mark: %s\n\tmark the context with secretstore.WithPurpose, or record secret.read at the site and mark it secretstore.SiteAudited", line)
	}
	for fn, pos := range s.siteMarks {
		if !s.emitters[fn] {
			t.Errorf("%s marks a read SiteAudited at %s but never records \"secret.read\": the decorator stays silent, so the read goes unrecorded", fn, pos)
		}
	}
}
