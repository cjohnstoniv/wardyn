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
// assigned from one in the same function — or to be one of the function's own
// parameters (directly, or through a context.With* call on it). A parameter
// makes every caller pass a marked context in that position; the walk climbs
// the static calls until each path is marked. A context from anywhere else
// (context.Background(), a field, a closure's parameter) fails the guard, as
// does a path that reaches a reference that is not a call, or a function
// nothing references (an HTTP handler, a method only an interface calls): a
// caller's mark counts only if it is the context the Get receives. Every
// function that calls SiteAudited must itself record a "secret.read". Audited
// also refuses an unmarked Get at run time; this guard finds it before then.
//
// Two reads never go through Get: the pg store's bulk readers, ConvertV0 (the
// boot conversion) and Migrate (`wardynd -migrate-secrets`), open values
// directly and their callers record each read. Their calls are held to the same
// rule as a Get: the context must be marked. And inside the pg store, every
// function that can reach a value-opening primitive (a local envelope's
// kek.Open, a legacy row's age.Decrypt, or the external store's Get, vaultkv's
// included) must be reached only from Get or those two bulk readers, so a new
// way to open a value fails here until it is classified. Reconcile is not a
// read: it calls the external store's Check and Walk, which read metadata only.
//
// A Get (or bulk reader) taken as a method value or passed as a function value
// fails: the call its context reaches cannot be followed. And External.Get is
// the pg store's alone: any use of it elsewhere fails.

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
	"maps"
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
	guardPGPkg       = guardStorePkg + "/pg"
	guardPGStore     = "(*" + guardPGPkg + ".Store)"
)

// guardBulkReaders are the pg store's methods that open values without a Get.
// A call of one is checked like a Get: its context must be marked.
var guardBulkReaders = map[string]bool{
	guardPGStore + ".ConvertV0": true,
	guardPGStore + ".Migrate":   true,
}

// guardValueOpeners are the primitives that turn a stored row into its value.
var guardValueOpeners = []string{
	guardStorePkg + "/kek.Open",
	"filippo.io/age.Decrypt",
	"(" + guardStorePkg + ".External).Get",
}

// guardPGReadEntries are the only pg-store entry points allowed to reach a
// value opener: Get, which the Audited decorator records, and the bulk readers.
var guardPGReadEntries = map[string]bool{
	guardPGStore + ".Get":       true,
	guardPGStore + ".ConvertV0": true,
	guardPGStore + ".Migrate":   true,
}

type listedPkg struct {
	ImportPath, Dir, Export string
	GoFiles                 []string
}

// Where a context argument comes from: a mark, the enclosing function's
// parameter i (i >= 0), or anything else.
const (
	ctxOther  = -2
	ctxMarked = -1
	ctxNone   = -3 // a variable already being classified (a self-assignment)
)

// secretRef is one static reference to a function: from which function, and,
// when it is a call, where each argument's context comes from (nil otherwise).
type secretRef struct {
	from string
	pos  string
	args []int
}

// getSite is one Get call and where its context comes from.
type getSite struct {
	pos  string
	ctx  int
	note string // why the site cannot be followed, when that is the finding
}

type secretReadScan struct {
	fset      *token.FileSet
	storeType *types.Interface
	extType   *types.Interface       // secretstore.External
	getSites  map[string][]getSite   // enclosing function -> its Get calls
	refs      map[string][]secretRef // function -> references to it
	siteMarks map[string]string      // function calling SiteAudited -> its position
	emitters  map[string]bool        // function whose body holds the literal "secret.read"
	purposes  int
	pgCallers map[string][]string // inside the pg store: callee -> its callers
	extGets   []string            // External.Get used outside the pg store
}

// newSecretReadScan returns an empty scan against store, the secretstore
// package (the real one, or a fixture's).
func newSecretReadScan(fset *token.FileSet, store *types.Package) *secretReadScan {
	return &secretReadScan{
		fset:      fset,
		storeType: store.Scope().Lookup("Store").Type().Underlying().(*types.Interface),
		extType:   store.Scope().Lookup("External").Type().Underlying().(*types.Interface),
		getSites:  map[string][]getSite{},
		refs:      map[string][]secretRef{},
		siteMarks: map[string]string{},
		emitters:  map[string]bool{},
		pgCallers: map[string][]string{},
	}
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

// ctxScope classifies the context arguments inside one function declaration.
type ctxScope struct {
	info    *types.Info
	params  map[types.Object]int
	assigns map[types.Object][]ast.Expr // nil entry: assigned from something untracked
}

func newCtxScope(info *types.Info, fd *ast.FuncDecl) *ctxScope {
	c := &ctxScope{info: info, params: map[types.Object]int{}, assigns: map[types.Object][]ast.Expr{}}
	i := 0
	for _, f := range fd.Type.Params.List {
		if len(f.Names) == 0 {
			i++
		}
		for _, n := range f.Names {
			if obj := info.Defs[n]; obj != nil {
				c.params[obj] = i
			}
			i++
		}
	}
	assign := func(lhs []ast.Expr, rhs []ast.Expr) {
		for j, l := range lhs {
			id, ok := l.(*ast.Ident)
			if !ok {
				continue
			}
			obj := info.ObjectOf(id)
			if obj == nil {
				continue
			}
			var e ast.Expr
			if len(rhs) == len(lhs) || j == 0 && len(rhs) == 1 {
				e = rhs[min(j, len(rhs)-1)]
			}
			c.assigns[obj] = append(c.assigns[obj], e)
		}
	}
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.AssignStmt:
			assign(v.Lhs, v.Rhs)
		case *ast.ValueSpec:
			if len(v.Values) > 0 {
				lhs := make([]ast.Expr, len(v.Names))
				for j, name := range v.Names {
					lhs[j] = name
				}
				assign(lhs, v.Values)
			}
		case *ast.RangeStmt:
			assign([]ast.Expr{v.Key, v.Value}, nil)
		}
		return true
	})
	return c
}

// class says where e, a context expression, comes from. A variable assigned
// exactly once, from a mark, counts as marked; a variable assigned more than
// once (a parameter's incoming value counts as one) fails closed as ctxOther
// even when one of those assignments is a mark, because a Get that reaches
// it can still run on whichever path skipped the mark — the single-assignment idiom
// (rctx := secretstore.WithPurpose(...)) is the only shape this guard trusts.
func (c *ctxScope) class(e ast.Expr, seen map[types.Object]bool) int {
	switch a := ast.Unparen(e).(type) {
	case *ast.CallExpr:
		switch k := calleeKey(c.info, a); {
		case k == guardWithPurpose || k == guardSiteAudited:
			return ctxMarked
		case strings.HasPrefix(k, "context.") && len(a.Args) > 0:
			return c.class(a.Args[0], seen)
		}
	case *ast.Ident:
		obj := c.info.ObjectOf(a)
		if obj == nil {
			return ctxOther
		}
		if seen[obj] {
			return ctxNone
		}
		seen[obj] = true
		defer delete(seen, obj)
		out, isParam := c.params[obj]
		n := len(c.assigns[obj])
		if isParam {
			n++ // the incoming value is an assignment too
		}
		if n > 1 {
			return ctxOther
		}
		if !isParam {
			out = ctxNone
		}
		for _, rhs := range c.assigns[obj] {
			k := ctxOther
			if rhs != nil {
				k = c.class(rhs, seen)
			}
			switch {
			case k == ctxMarked:
				return ctxMarked
			case k == ctxOther:
				out = ctxOther
			case k >= 0 && out == ctxNone:
				out = k
			}
		}
		if out == ctxNone {
			return ctxOther
		}
		return out
	}
	return ctxOther
}

func (c *ctxScope) args(call *ast.CallExpr) []int {
	out := make([]int, len(call.Args))
	for i, a := range call.Args {
		out[i] = c.class(a, map[types.Object]bool{})
	}
	return out
}

func (s *secretReadScan) scanFunc(info *types.Info, fd *ast.FuncDecl) {
	from := funcKey(info.Defs[fd.Name])
	sc := newCtxScope(info, fd)
	callOf := map[*ast.Ident]*ast.CallExpr{}
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.BasicLit:
			if v.Value == `"secret.read"` {
				s.emitters[from] = true
			}
		case *ast.CallExpr:
			s.scanCall(info, sc, from, v, callOf)
		case *ast.Ident:
			s.scanReadValue(info, from, v, callOf[v] != nil)
			to := funcKey(info.Uses[v])
			if to == "" || to == from || strings.HasPrefix(to, guardStorePkg+".") {
				return true
			}
			r := secretRef{from: from, pos: s.fset.Position(v.Pos()).String()}
			if call := callOf[v]; call != nil {
				r.args = sc.args(call)
			}
			s.refs[to] = append(s.refs[to], r)
		}
		return true
	})
}

// scanReadValue flags two reads the call walk cannot see: a store's Get (or a
// bulk reader) referenced other than as a call's function, i.e. taken as a
// method value or passed on as a function value, whose context cannot be
// followed; and any use of External.Get, which only the pg store may call.
func (s *secretReadScan) scanReadValue(info *types.Info, from string, id *ast.Ident, called bool) {
	fn, ok := info.Uses[id].(*types.Func)
	if !ok {
		return
	}
	recv := fn.Signature().Recv()
	pos := s.fset.Position(id.Pos()).String()
	switch {
	case fn.Name() == "Get" && recv != nil && s.implements(recv.Type(), s.extType):
		s.extGets = append(s.extGets, pos+" in "+from)
	case called:
	case fn.Name() == "Get" && recv != nil && s.isStore(recv.Type()), guardBulkReaders[funcKey(fn)]:
		s.getSites[from] = append(s.getSites[from], getSite{pos: pos, ctx: ctxOther, note: "a method value cannot be followed"})
	}
}

func (s *secretReadScan) implements(t types.Type, iface *types.Interface) bool {
	if types.Implements(t, iface) || types.Implements(types.NewPointer(t), iface) {
		return true
	}
	u, ok := t.Underlying().(*types.Interface)
	return ok && types.Implements(iface, u)
}

// scanCall records a Get site or a mark call, and notes which identifier names
// the callee so the identifier's reference knows it is a call.
func (s *secretReadScan) scanCall(info *types.Info, sc *ctxScope, from string, call *ast.CallExpr, callOf map[*ast.Ident]*ast.CallExpr) {
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
		sel := info.Selections[f]
		isGet := sel != nil && f.Sel.Name == "Get" && sel.Kind() == types.MethodVal && s.isStore(sel.Recv())
		if isGet || guardBulkReaders[calleeKey(info, call)] {
			g := getSite{pos: s.fset.Position(call.Pos()).String(), ctx: ctxOther}
			if len(call.Args) > 0 {
				g.ctx = sc.class(call.Args[0], map[types.Object]bool{})
			}
			s.getSites[from] = append(s.getSites[from], g)
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
	fset := token.NewFileSet()
	imp := importer.ForCompiler(fset, "gc", func(path string) (io.ReadCloser, error) {
		if exports[path] == "" {
			return nil, fmt.Errorf("no export data for %s", path)
		}
		return os.Open(exports[path])
	})
	ss, err := imp.Import(guardStorePkg)
	if err != nil {
		t.Fatalf("import %s: %v", guardStorePkg, err)
	}
	s := newSecretReadScan(fset, ss)

	for _, p := range pkgs {
		pgStore := p.ImportPath == guardPGPkg
		if !pgStore && (!strings.HasPrefix(p.ImportPath, guardModPath+"/") || p.ImportPath == guardStorePkg ||
			strings.HasPrefix(p.ImportPath, guardStorePkg+"/")) || len(p.GoFiles) == 0 {
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
				fd, ok := d.(*ast.FuncDecl)
				switch {
				case !ok || fd.Body == nil:
				case pgStore:
					s.scanPGCalls(info, fd)
				default:
					s.scanFunc(info, fd)
				}
			}
		}
	}
	return s
}

// scanPGCalls records, inside the pg store, which functions fd references.
func (s *secretReadScan) scanPGCalls(info *types.Info, fd *ast.FuncDecl) {
	from := funcKey(info.Defs[fd.Name])
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok {
			if to := funcKey(info.Uses[id]); to != "" && to != from {
				s.pgCallers[to] = append(s.pgCallers[to], from)
			}
		}
		return true
	})
}

// pgValueReaders returns every pg-store function that can reach a value
// opener, and the entry points among them (exported, or referenced by nothing
// in the package) that are not Get or a bulk reader.
func (s *secretReadScan) pgValueReaders() (reached map[string]bool, unclassified []string) {
	reached = map[string]bool{}
	queue := slices.Clone(guardValueOpeners)
	for len(queue) > 0 {
		fn := queue[0]
		queue = queue[1:]
		for _, c := range s.pgCallers[fn] {
			if !reached[c] {
				reached[c] = true
				queue = append(queue, c)
			}
		}
	}
	for fn := range reached {
		name := fn[strings.LastIndex(fn, ".")+1:]
		if (token.IsExported(name) || len(s.pgCallers[fn]) == 0) && !guardPGReadEntries[fn] {
			unclassified = append(unclassified, fn)
		}
	}
	slices.Sort(unclassified)
	return reached, unclassified
}

// unmarkedPaths climbs from each Get whose context is a parameter through the
// callers that pass it along, and returns, per Get, the first path that ends
// anywhere but a mark — one line per Get, so fixing one path never hides
// another Get's.
func (s *secretReadScan) unmarkedPaths() []string {
	var bad []string
	for fn, sites := range s.getSites {
		for _, g := range sites {
			why := "read at " + g.pos + " in " + fn
			switch g.ctx {
			case ctxMarked:
			case ctxOther:
				if g.note != "" {
					bad = append(bad, why+" — "+g.note)
					continue
				}
				bad = append(bad, why+" — its context is neither marked nor a parameter of "+fn)
			default:
				if line := s.firstUnmarked(fn, g.ctx, why); line != "" {
					bad = append(bad, line)
				}
			}
		}
	}
	slices.Sort(bad)
	return bad
}

// ctxNeed is a function whose parameter arg must arrive marked.
type ctxNeed struct {
	fn  string
	arg int
}

func (s *secretReadScan) firstUnmarked(start string, arg int, why string) string {
	first := ctxNeed{start, arg}
	chain := map[ctxNeed]string{first: why}
	queue := []ctxNeed{first}
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		refs := s.refs[n.fn]
		if len(refs) == 0 {
			return chain[n] + " — nothing that references it marks the context"
		}
		for _, r := range refs {
			at := chain[n] + " <- " + r.from + " (" + r.pos + ")"
			switch {
			case r.from == "":
				return chain[n] + " <- package-level reference at " + r.pos
			case r.args == nil:
				return at + " — not a call, so the context cannot be followed"
			case n.arg >= len(r.args) || r.args[n.arg] == ctxOther:
				return at + " — passes a context that is neither marked nor its own parameter"
			case r.args[n.arg] >= 0:
				next := ctxNeed{r.from, r.args[n.arg]}
				if _, ok := chain[next]; !ok {
					chain[next] = at
					queue = append(queue, next)
				}
			}
		}
	}
	return ""
}

// classifyFixture type-checks src as package p, resolving guardWithPurpose
// and guardSiteAudited to a synthetic secretstore package so class() sees
// them exactly as it would the real marks, and returns a ctxScope for fn
// plus the type-checked function declaration.
func classifyFixture(t *testing.T, src, fn string) (*ctxScope, *ast.FuncDecl) {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "fixture.go", src, 0)
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	store := types.NewPackage(guardStorePkg, "secretstore")
	params := types.NewTuple(
		types.NewVar(token.NoPos, store, "ctx", types.Typ[types.Int]),
		types.NewVar(token.NoPos, store, "purpose", types.Typ[types.String]),
	)
	results := types.NewTuple(types.NewVar(token.NoPos, store, "", types.Typ[types.Int]))
	sig := types.NewSignatureType(nil, nil, nil, params, results, false)
	store.Scope().Insert(types.NewFunc(token.NoPos, store, "WithPurpose", sig))
	store.Scope().Insert(types.NewFunc(token.NoPos, store, "SiteAudited", sig))
	store.MarkComplete()
	imp := importerFunc(func(path string) (*types.Package, error) {
		if path == guardStorePkg {
			return store, nil
		}
		return nil, fmt.Errorf("unexpected import %q", path)
	})
	info := &types.Info{Uses: map[*ast.Ident]types.Object{}, Defs: map[*ast.Ident]types.Object{}}
	conf := types.Config{Importer: imp}
	if _, err := conf.Check("p", fset, []*ast.File{file}, info); err != nil {
		t.Fatalf("type-check fixture: %v", err)
	}
	var fd *ast.FuncDecl
	for _, d := range file.Decls {
		if f, ok := d.(*ast.FuncDecl); ok && f.Name.Name == fn {
			fd = f
		}
	}
	if fd == nil {
		t.Fatalf("fixture has no func %s", fn)
	}
	return newCtxScope(info, fd), fd
}

type importerFunc func(path string) (*types.Package, error)

func (f importerFunc) Import(path string) (*types.Package, error) { return f(path) }

// callArg0 returns the first argument of the named call inside fd's body.
func callArg0(fd *ast.FuncDecl, callee string) ast.Expr {
	var arg ast.Expr
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			if id, ok := call.Fun.(*ast.Ident); ok && id.Name == callee && len(call.Args) > 0 {
				arg = call.Args[0]
			}
		}
		return true
	})
	return arg
}

// TestCtxScopeClassRejectsAContextMarkedOnOnlyOneBranch pins rule 23's
// guard-of-the-guard: a context variable marked on one branch (WithPurpose)
// and left untouched on another must classify as ctxOther, not ctxMarked,
// because the Get it reaches can still run with no purpose on the untouched
// path. Before the fix, any marked assignment made class() return ctxMarked
// immediately, so this mutation — the shape SD-5's review found in
// resolveLLMInspectionSecrets — passed the guard silently.
func TestCtxScopeClassRejectsAContextMarkedOnOnlyOneBranch(t *testing.T) {
	const src = `package p

import "github.com/cjohnstoniv/wardyn/internal/secretstore"

func f(ctx int, name string) {
	rctx := ctx
	if len(name) == 0 {
		rctx = secretstore.WithPurpose(ctx, "x")
	}
	get(rctx)
}

func get(ctx int) {}
`
	sc, fd := classifyFixture(t, src, "f")
	arg := callArg0(fd, "get")
	if arg == nil {
		t.Fatal("fixture has no get(...) call")
	}
	if got := sc.class(arg, map[types.Object]bool{}); got != ctxOther {
		t.Errorf("class(rctx) = %d, want ctxOther (%d): a context marked on only one branch must not classify as marked", got, ctxOther)
	}
}

// TestCtxScopeClassRejectsAParameterMarkedOnOnlyOneBranch is the parameter
// form of the one-branch mutation: a context parameter reassigned from a mark
// on one branch still carries its unmarked incoming value on the other, so it
// must classify as ctxOther. The parameter's incoming value is an assignment
// too; before the fix it was not counted, so the lone marked reassignment
// made class() return ctxMarked.
func TestCtxScopeClassRejectsAParameterMarkedOnOnlyOneBranch(t *testing.T) {
	const src = `package p

import "github.com/cjohnstoniv/wardyn/internal/secretstore"

func f(ctx int, name string) {
	if len(name) == 0 {
		ctx = secretstore.WithPurpose(ctx, "x")
	}
	get(ctx)
}

func get(ctx int) {}
`
	sc, fd := classifyFixture(t, src, "f")
	arg := callArg0(fd, "get")
	if arg == nil {
		t.Fatal("fixture has no get(...) call")
	}
	if got := sc.class(arg, map[types.Object]bool{}); got != ctxOther {
		t.Errorf("class(ctx) = %d, want ctxOther (%d): a parameter marked on only one branch must not classify as marked", got, ctxOther)
	}
}

// TestCtxScopeClassAcceptsTheSingleAssignmentIdiom is the fix's control: the
// shape every real WithPurpose call site in this repo uses today, a single
// unconditional assignment, must still classify as ctxMarked.
func TestCtxScopeClassAcceptsTheSingleAssignmentIdiom(t *testing.T) {
	const src = `package p

import "github.com/cjohnstoniv/wardyn/internal/secretstore"

func f(ctx int, name string) {
	rctx := secretstore.WithPurpose(ctx, "x")
	get(rctx)
}

func get(ctx int) {}
`
	sc, fd := classifyFixture(t, src, "f")
	arg := callArg0(fd, "get")
	if arg == nil {
		t.Fatal("fixture has no get(...) call")
	}
	if got := sc.class(arg, map[types.Object]bool{}); got != ctxMarked {
		t.Errorf("class(rctx) = %d, want ctxMarked (%d): the single-assignment idiom must still classify as marked", got, ctxMarked)
	}
}

// guardFixtureStore stands in for internal/secretstore in the scan fixtures.
const guardFixtureStore = `package secretstore

import "context"

type Store interface {
	Get(ctx context.Context, name string) ([]byte, error)
}

type External interface {
	Get(ctx context.Context, owner, name, ref string) ([]byte, error)
}

func WithPurpose(ctx context.Context, p string) context.Context { return ctx }
`

// scanFixture runs the guard's scan over src, a package that imports the
// fixture secretstore, exactly as it scans a real package.
func scanFixture(t *testing.T, src string) *secretReadScan {
	t.Helper()
	fset := token.NewFileSet()
	std := importer.Default()
	parse := func(name, text string) *ast.File {
		f, err := parser.ParseFile(fset, name, text, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		return f
	}
	store, err := (&types.Config{Importer: std}).Check(guardStorePkg, fset, []*ast.File{parse("store.go", guardFixtureStore)}, nil)
	if err != nil {
		t.Fatalf("type-check the fixture store: %v", err)
	}
	imp := importerFunc(func(path string) (*types.Package, error) {
		if path == guardStorePkg {
			return store, nil
		}
		return std.Import(path)
	})
	file := parse("fixture.go", src)
	info := &types.Info{Uses: map[*ast.Ident]types.Object{}, Defs: map[*ast.Ident]types.Object{}, Selections: map[*ast.SelectorExpr]*types.Selection{}}
	if _, err := (&types.Config{Importer: imp}).Check(guardModPath+"/cmd/fixture", fset, []*ast.File{file}, info); err != nil {
		t.Fatalf("type-check fixture: %v", err)
	}
	s := newSecretReadScan(fset, store)
	for _, d := range file.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Body != nil {
			s.scanFunc(info, fd)
		}
	}
	return s
}

// TestScanFlagsReadsItCannotFollow pins the scan against reads that reach a
// store without a call it can classify: a Get taken as a method value (P1) or
// passed on as a function value (P4) is flagged, since neither context can be
// followed, and External.Get used outside the pg store (P5) is flagged, since
// only the pg store may open a value behind a pointer row. The control (P0), a
// marked direct Get, is not.
func TestScanFlagsReadsItCannotFollow(t *testing.T) {
	const src = `package fixture

import (
	"context"

	"github.com/cjohnstoniv/wardyn/internal/secretstore"
)

func p0(ctx context.Context, s secretstore.Store) {
	_, _ = s.Get(secretstore.WithPurpose(ctx, "x"), "p0")
}

func p1(ctx context.Context, s secretstore.Store) {
	g := s.Get
	_, _ = g(secretstore.WithPurpose(ctx, "x"), "p1")
}

func p4(s secretstore.Store) {
	call(s.Get)
}

func call(func(context.Context, string) ([]byte, error)) {}

func p5(ext secretstore.External) {
	_, _ = ext.Get(context.Background(), "", "p5", "ref")
}
`
	s := scanFixture(t, src)
	bad := strings.Join(s.unmarkedPaths(), "\n")
	for _, fn := range []string{"fixture.p1", "fixture.p4"} {
		if !strings.Contains(bad, "in "+guardModPath+"/cmd/"+fn+" — a method value") {
			t.Errorf("the scan did not flag the Get taken as a value in %s; findings:\n%s", fn, bad)
		}
	}
	if strings.Contains(bad, "fixture.p0") {
		t.Errorf("the scan flagged the marked direct Get in p0: %s", bad)
	}
	if len(s.extGets) != 1 || !strings.Contains(s.extGets[0], "fixture.go:25") {
		t.Errorf("External.Get outside the pg store = %v; want p5's call at fixture.go:25", s.extGets)
	}
}

func TestEverySecretReadIsAuditedOnce(t *testing.T) {
	s := scanSecretReads(t, repoRoot(t))

	// Guard the guard: the scan must see the reads this rule was written for.
	for _, fn := range []string{
		guardModPath + "/cmd/wardynd.loadOrCreateSecret",
		guardModPath + "/internal/api.readHarnessBlob",
		"(*" + guardModPath + "/internal/api.Server).handleInternalInjection",
		"(*" + guardModPath + "/internal/broker.Broker).mintGitPAT",
		guardModPath + "/cmd/wardynd.convertSecretStore", // ConvertV0
		guardModPath + "/cmd/wardynd.migrateMode",        // Migrate
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
	reached, unclassified := s.pgValueReaders()
	for fn := range guardPGReadEntries {
		if !reached[fn] {
			t.Errorf("the scan does not see %s reach a value opener; the pg-store half of the guard no longer sees the reads it pins", fn)
		}
	}
	for _, fn := range unclassified {
		t.Errorf("%s can open a stored value but is neither Get nor a bulk reader the guard checks (%v): route the read through Get, or add it to guardBulkReaders and guardPGReadEntries with a marked context at every call", fn, slices.Sorted(maps.Keys(guardBulkReaders)))
	}
	for _, pos := range s.extGets {
		t.Errorf("secretstore.External.Get used at %s: External.Get is the pg store's alone; route the read through Store.Get", pos)
	}
	for fn, pos := range s.siteMarks {
		if !s.emitters[fn] {
			t.Errorf("%s marks a read SiteAudited at %s but never records \"secret.read\": the decorator stays silent, so the read goes unrecorded", fn, pos)
		}
	}
}
