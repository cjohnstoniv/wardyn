// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// The route census for the spec read tier, read from the real router: every GET route whose response
// path handles a policy spec, a governance profile, a ceiling overlay, a preset or a held change must
// reach one of the tier-taking redactions (tierTakers), or be listed in specRouteExemptions with why.
// Like TestAuthzMatrixHandlersReachAuthz it is a necessary condition, not a per-path proof: a route that
// redacts on one branch and writes the raw spec on another still passes. It catches a new read that
// never redacts at all.
//
// "Handles" is read from the source without type checking. A route's response path is its handler and
// every package function or (*Server) method it reaches that takes an http.ResponseWriter. A function on
// that path handles spec data when it names one of specCensusRoots, calls a package function, a
// (*Server) method or a store method whose declared results carry one, or reads a Server or Config
// field that does. "Carries" follows struct fields, pointers, slices and maps through the declarations
// of this package, internal/types, pkg/client, internal/store and internal/composer. A function off the response path (one
// that decides with a ceiling and answers a bool or a list of names) is not read, so a route that only
// decides with a policy is not reported. The redaction is looked for on the route's data path: the
// response path plus every function it calls whose declared results carry a root, transitively, so a
// redaction reached only through, say, a refusal helper does not count.

// specCensusRoots are the payload types the read tier withholds host paths from.
var specCensusRoots = []string{
	"types.RunPolicySpec", "types.CeilingOverlay", "types.GovernanceProfile", "types.GovernanceChange", "types.LaunchPreset",
}

// specRouteExemptions are the GET routes whose response path handles spec data without reaching a
// tierTaker, or whose handler is not a (*Server) method this census can read, each with why. via, when
// set, is the function that withholds the host paths instead; the route must still reach it.
var specRouteExemptions = map[string]struct{ why, via string }{
	"GET /":              {"the console's static files (the UI handler); no API payload", ""},
	"GET /*":             {"the console's static files (the UI handler); no API payload", ""},
	"GET /auth/login":    {"the sign-in redirect, oidc.Authenticator's own handler; no API payload", ""},
	"GET /auth/callback": {"the sign-in callback, oidc.Authenticator's own handler; no API payload", ""},
	"GET /api/v1/runs/{id}/recording/{runID}": {"recording.Handler's closure serves a run's recorded session bytes; no policy payload", ""},
	"GET /api/v1/governance/changes": {"a held change's diff is stored rendered and re-withheld for the reader's tier by changeForRead",
		"changeForRead"},
	"GET /api/v1/governance/changes/{id}": {"a held change's diff is stored rendered and re-withheld for the reader's tier by changeForRead",
		"changeForRead"},
	"GET /api/v1/runner-pools":     {"the caller's effective ceiling decides which pools are listed (listRunnerPools); the list carries no spec", ""},
	"GET /api/v1/me/runner-pools":  {"the caller's effective ceiling decides which pools are listed (listRunnerPools); the list carries no spec", ""},
	"GET /api/v1/runs/{id}/attach": {"the run's profile decides whether attaching is allowed (refuseInteractiveAttach); the refusal names the profile only", ""},
	"GET /api/v1/setup/status":     {"the deployment default's minimum confinement class decides one check row; no spec is served", ""},
}

// specCensus is the parsed source the route census reads.
type specCensus struct {
	pkgs     map[string]*censusPkg // "api", "types", "client", "store"
	carriers map[string]bool       // "pkg.Type" that carries a root
	funcs    map[string]*ast.FuncDecl
	methods  map[string]*ast.FuncDecl // (*Server) methods by name
	roots    map[string]bool
	takers   map[string]bool
	refs     map[string][]string // "f:name" / "m:name" -> the functions it names
	handles  map[string]bool     // the functions that handle spec data
	writers  map[string]bool     // the functions that take an http.ResponseWriter
	produces map[string]bool     // the functions whose declared results carry a root
	resultOf map[string]bool     // store methods (any receiver) whose results carry a root
}

type censusPkg struct {
	types map[string]ast.Expr
	funcs map[string]*ast.FuncDecl
}

func parseCensusPkg(t *testing.T, dir string) (*censusPkg, []*ast.File) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	p := &censusPkg{types: map[string]ast.Expr{}, funcs: map[string]*ast.FuncDecl{}}
	var files []*ast.File
	fset := token.NewFileSet()
	for _, e := range entries {
		if n := e.Name(); strings.HasSuffix(n, ".go") && !strings.HasSuffix(n, "_test.go") {
			f, err := parser.ParseFile(fset, filepath.Join(dir, n), nil, parser.SkipObjectResolution)
			if err != nil {
				t.Fatal(err)
			}
			files = append(files, f)
		}
	}
	for _, f := range files {
		for _, d := range f.Decls {
			switch d := d.(type) {
			case *ast.GenDecl:
				for _, sp := range d.Specs {
					if ts, ok := sp.(*ast.TypeSpec); ok {
						p.types[ts.Name.Name] = ts.Type
					}
				}
			case *ast.FuncDecl:
				if d.Recv == nil {
					p.funcs[d.Name.Name] = d
				}
			}
		}
	}
	return p, files
}

// newSpecCensus parses the package (plus extra, for a planted twin) and its three payload packages.
func newSpecCensus(t *testing.T, extra ...*ast.File) *specCensus {
	t.Helper()
	c := &specCensus{pkgs: map[string]*censusPkg{}, carriers: map[string]bool{}, funcs: map[string]*ast.FuncDecl{},
		methods: map[string]*ast.FuncDecl{}, roots: map[string]bool{}, takers: map[string]bool{}, refs: map[string][]string{},
		handles: map[string]bool{}, writers: map[string]bool{}, produces: map[string]bool{}, resultOf: map[string]bool{}}
	for _, r := range specCensusRoots {
		c.roots[r] = true
	}
	for name := range tierTakers {
		c.takers[name] = true
	}
	var apiFiles, storeFiles []*ast.File
	for name, dir := range map[string]string{"api": ".", "types": "../types", "client": "../../pkg/client", "store": "../store", "composer": "../composer"} {
		p, files := parseCensusPkg(t, dir)
		c.pkgs[name] = p
		switch name {
		case "api":
			apiFiles = files
		case "store":
			storeFiles = files
		}
	}
	// Which named types carry a root, to a fixpoint: a type that holds itself is decided by its other fields.
	for changed := true; changed; {
		changed = false
		for in, p := range c.pkgs {
			for name, typ := range p.types {
				if key := in + "." + name; !c.carriers[key] && c.carries(in, typ) {
					c.carriers[key], changed = true, true
				}
			}
		}
	}
	c.storeResults(storeFiles)
	apiFiles = append(apiFiles, extra...)
	for _, f := range apiFiles {
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			switch {
			case !ok || fd.Body == nil:
			case fd.Recv == nil:
				c.funcs[fd.Name.Name] = fd
			case isServerReceiver(fd):
				c.methods[fd.Name.Name] = fd
			}
		}
	}
	for name, fd := range c.funcs {
		c.scan("f:"+name, fd)
	}
	for name, fd := range c.methods {
		c.scan("m:"+name, fd)
	}
	return c
}

// storeResults records every store function, interface method and method whose results carry a root.
func (c *specCensus) storeResults(files []*ast.File) {
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.FuncDecl:
				if c.listCarries("store", x.Type.Results) {
					c.resultOf[x.Name.Name] = true
				}
			case *ast.Field:
				if ft, ok := x.Type.(*ast.FuncType); ok && len(x.Names) == 1 && c.listCarries("store", ft.Results) {
					c.resultOf[x.Names[0].Name] = true
				}
			}
			return true
		})
	}
}

// carries reports whether a value of type e, declared in package in, holds a root.
func (c *specCensus) carries(in string, e ast.Expr) bool {
	switch x := e.(type) {
	case *ast.Ident:
		return c.named(in, x.Name)
	case *ast.SelectorExpr:
		if id, ok := x.X.(*ast.Ident); ok && c.pkgs[id.Name] != nil {
			return c.named(id.Name, x.Sel.Name)
		}
	case *ast.StarExpr:
		return c.carries(in, x.X)
	case *ast.ArrayType:
		return c.carries(in, x.Elt)
	case *ast.MapType:
		return c.carries(in, x.Key) || c.carries(in, x.Value)
	case *ast.IndexExpr:
		return c.carries(in, x.X) || c.carries(in, x.Index)
	case *ast.StructType:
		// Only what encoding/json writes: an unexported or `json:"-"` field is never served.
		return slices.ContainsFunc(x.Fields.List, func(f *ast.Field) bool { return jsonVisible(f) && c.carries(in, f.Type) })
	}
	return false
}

func jsonVisible(f *ast.Field) bool {
	if f.Tag != nil && strings.Contains(f.Tag.Value, `json:"-"`) {
		return false
	}
	if len(f.Names) == 0 {
		t := f.Type
		if st, ok := t.(*ast.StarExpr); ok {
			t = st.X
		}
		if sel, ok := t.(*ast.SelectorExpr); ok {
			t = sel.Sel
		}
		id, ok := t.(*ast.Ident)
		return ok && id.IsExported()
	}
	return slices.ContainsFunc(f.Names, (*ast.Ident).IsExported)
}

func (c *specCensus) listCarries(in string, fl *ast.FieldList) bool {
	return fl != nil && slices.ContainsFunc(fl.List, func(f *ast.Field) bool { return c.carries(in, f.Type) })
}

func (c *specCensus) named(in, name string) bool {
	return c.roots[in+"."+name] || c.carriers[in+"."+name]
}

// field reports whether field name of api struct typ carries a root.
func (c *specCensus) field(typ, name string) bool {
	st, ok := c.pkgs["api"].types[typ].(*ast.StructType)
	if !ok {
		return false
	}
	for _, f := range st.Fields.List {
		if slices.ContainsFunc(f.Names, func(n *ast.Ident) bool { return n.Name == name }) {
			return c.carries("api", f.Type)
		}
	}
	return false
}

// scan records what fd names and whether it handles spec data.
func (c *specCensus) scan(key string, fd *ast.FuncDecl) {
	for _, p := range fd.Type.Params.List {
		if sel, ok := p.Type.(*ast.SelectorExpr); ok && sel.Sel.Name == "ResponseWriter" {
			c.writers[key] = true
		}
	}
	c.produces[key] = c.listCarries("api", fd.Type.Results)
	handles := false
	var visit func(n ast.Node) bool
	visit = func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.SelectorExpr:
			if id, ok := x.X.(*ast.Ident); ok {
				switch {
				case id.Name == "s":
					if m, ok := c.methods[x.Sel.Name]; ok {
						c.refs[key] = append(c.refs[key], "m:"+x.Sel.Name)
						handles = handles || c.listCarries("api", m.Type.Results)
					} else {
						handles = handles || c.field("Server", x.Sel.Name)
					}
					return false
				case c.pkgs[id.Name] != nil && id.Name != "api":
					f, ok := c.pkgs[id.Name].funcs[x.Sel.Name]
					handles = handles || c.roots[id.Name+"."+x.Sel.Name] || ok && c.listCarries(id.Name, f.Type.Results)
					return false
				}
			}
			if in, ok := x.X.(*ast.SelectorExpr); ok {
				if id, ok := in.X.(*ast.Ident); ok && id.Name == "s" && in.Sel.Name == "cfg" {
					handles = handles || c.field("Config", x.Sel.Name)
					return false
				}
			}
			handles = handles || c.resultOf[x.Sel.Name]
			ast.Inspect(x.X, visit)
			return false
		case *ast.Ident:
			if f, ok := c.funcs[x.Name]; ok {
				c.refs[key] = append(c.refs[key], "f:"+x.Name)
				handles = handles || c.listCarries("api", f.Type.Results)
			}
		}
		return true
	}
	ast.Inspect(fd.Body, visit)
	c.handles[key] = handles
}

// reach is every function handler reaches through the functions follow admits.
func (c *specCensus) reach(handler string, follow func(string) bool) map[string]bool {
	seen := map[string]bool{"m:" + handler: true}
	queue := []string{"m:" + handler}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, ref := range c.refs[cur] {
			if !seen[ref] && follow(ref) {
				seen[ref] = true
				queue = append(queue, ref)
			}
		}
	}
	return seen
}

// responsePath is handler and every writer it reaches through writers.
func (c *specCensus) responsePath(handler string) map[string]bool {
	return c.reach(handler, func(fn string) bool { return c.writers[fn] })
}

// dataPath is the response path plus every function that produces spec data for it: what the served
// value can come from.
func (c *specCensus) dataPath(handler string) map[string]bool {
	return c.reach(handler, func(fn string) bool { return c.writers[fn] || c.produces[fn] })
}

// serves reports whether handler's response path handles spec data.
func (c *specCensus) serves(handler string) bool {
	for fn := range c.responsePath(handler) {
		if c.handles[fn] {
			return true
		}
	}
	return false
}

// redacts reports whether handler's data path reaches a tierTaker.
func (c *specCensus) redacts(handler string) bool {
	for fn := range c.dataPath(handler) {
		if c.takers[strings.TrimPrefix(fn, "f:")] {
			return true
		}
	}
	return false
}

// unredacted is every GET route in routes ("METHOD /path" -> handler) that is not exempt and serves spec
// data without reaching a tierTaker, or whose handler is not a (*Server) method this census can read.
func (c *specCensus) unredacted(routes map[string]string) []string {
	var bad []string
	for key, handler := range routes {
		_, exempt := specRouteExemptions[key]
		_, readable := c.methods[handler]
		if !exempt && strings.HasPrefix(key, "GET ") && (!readable || c.serves(handler) && !c.redacts(handler)) {
			bad = append(bad, key)
		}
	}
	slices.Sort(bad)
	return bad
}

// censusRoutes is every route of the API router and the UI-enabled one, "METHOD /path" -> its handler's
// name ("" when chi.Walk hands back something other than a method value).
func censusRoutes(t *testing.T) map[string]string {
	t.Helper()
	srv, _, _, _ := newAuthzMatrixServer(t)
	out := map[string]string{}
	for _, router := range []chi.Router{srv.router, newAuthzMatrixServerWithUI(t).router} {
		if err := chi.Walk(router, func(method, route string, h http.Handler, _ ...func(http.Handler) http.Handler) error {
			out[method+" "+route] = handlerFuncName(h)
			return nil
		}); err != nil {
			t.Fatalf("chi.Walk: %v", err)
		}
	}
	return out
}

func TestPolMount_CensusRouteReach(t *testing.T) {
	routes := censusRoutes(t)
	c := newSpecCensus(t)
	if bad := c.unredacted(routes); len(bad) > 0 {
		t.Errorf("CENSUS: these GET routes serve a policy spec, profile, overlay, preset or held change and reach none of the tier-taking redactions %v; redact for the reader's tier, or list the route in specRouteExemptions with why: %v",
			slices.Sorted(func(yield func(string) bool) {
				for k := range tierTakers {
					if !yield(k) {
						return
					}
				}
			}), bad)
	}
	// Not vacuous: the reads this lane redacts are found as serving, and as redacting.
	for _, key := range []string{"GET /api/v1/policies", "GET /api/v1/policies/{id}", "GET /api/v1/policies/default", "GET /api/v1/presets",
		"GET /api/v1/presets/{name}", "GET /api/v1/governance", "GET /api/v1/runs/{id}/policy"} {
		if h, ok := routes[key]; !ok || !c.serves(h) || !c.redacts(h) {
			t.Errorf("CENSUS: %s (handler %q) is not read as a redacted spec read; the census has lost sight of it", key, h)
		}
	}
	// Every exemption is live: its route exists, still serves spec data, still reaches no tierTaker,
	// and reaches the function it names.
	for key, ex := range specRouteExemptions {
		h, ok := routes[key]
		_, readable := c.methods[h]
		switch {
		case !ok:
			t.Errorf("specRouteExemptions lists %s, which the router no longer has", key)
		case !readable:
		case !c.serves(h):
			t.Errorf("specRouteExemptions lists %s, which no longer serves spec data; drop the entry", key)
		case c.redacts(h):
			t.Errorf("specRouteExemptions lists %s, which now reaches a tierTaker; drop the entry", key)
		case ex.via != "" && !c.dataPath(h)["m:"+ex.via] && !c.dataPath(h)["f:"+ex.via]:
			t.Errorf("specRouteExemptions lists %s as withheld by %s, which it no longer reaches", key, ex.via)
		}
	}
}

// TestPolMount_CensusRouteReach_PlantedViolation is the census's twin: planted GET routes beside the real
// ones. The four that answer a spec unredacted are reported by name (one is handleGetPolicy without its
// redaction, so its refusal helpers are proved not to count as one; one writes a composer.Authority's
// ceiling); the redacted one and the one that
// answers no spec are not, and neither is any real route.
func TestPolMount_CensusRouteReach_PlantedViolation(t *testing.T) {
	const planted = `package api
func (s *Server) handlePlantedDefault(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.cfg.DefaultPolicy)
}
func (s *Server) handlePlantedStored(w http.ResponseWriter, r *http.Request) {
	s.servePlantedPolicy(w, r)
}
func (s *Server) servePlantedPolicy(w http.ResponseWriter, r *http.Request) {
	pol, _ := s.cfg.Store.GetPolicy(r.Context(), uuid.Nil)
	writeJSON(w, http.StatusOK, pol)
}
func (s *Server) handlePlantedGetPolicy(w http.ResponseWriter, r *http.Request) {
	id, ok := parseIDParam(w, r, "id", "policy")
	if !ok {
		return
	}
	p, err := s.cfg.Store.GetPolicy(r.Context(), id)
	if notFoundIf(w, err, "policy", reasonPolicyNotFound) {
		return
	}
	if !s.isSecurityOperator(r.Context()) && s.denyUserCapability(w, r, capPolicy, p.ID.String(), "policies.read", "x") {
		return
	}
	writeJSON(w, http.StatusOK, p)
}
func (s *Server) handlePlantedAuthority(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.deploymentAuthority().Ceiling)
}
func (s *Server) handlePlantedRedacted(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, redactSpecForRead(s.cfg.DefaultPolicy, s.specReadTierFor(r.Context())))
}
func (s *Server) handlePlantedOther(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]int{"n": 1})
}
`
	f, err := parser.ParseFile(token.NewFileSet(), "planted.go", planted, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	routes := censusRoutes(t)
	for path, h := range map[string]string{"default": "handlePlantedDefault", "stored": "handlePlantedStored", "get-policy": "handlePlantedGetPolicy", "authority": "handlePlantedAuthority",
		"redacted": "handlePlantedRedacted", "other": "handlePlantedOther"} {
		routes["GET /api/v1/planted-"+path] = h
	}
	got := newSpecCensus(t, f).unredacted(routes)
	if want := []string{"GET /api/v1/planted-authority", "GET /api/v1/planted-default", "GET /api/v1/planted-get-policy", "GET /api/v1/planted-stored"}; !slices.Equal(got, want) {
		t.Errorf("the census reported %v, want exactly %v", got, want)
	}
}
