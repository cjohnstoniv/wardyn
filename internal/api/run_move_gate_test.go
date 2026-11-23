// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// withMoveGateOpen opens the move gate for one test and closes it again when the test ends. A caller never runs in
// parallel.
func withMoveGateOpen(t *testing.T) {
	t.Helper()
	saved := moveGateOpen
	moveGateOpen = func() bool { return true }
	t.Cleanup(func() { moveGateOpen = saved })
}

// moveGateStore is the pause fixture's store with run_move stored on, as a database edit or a downgrade leaves it,
// counting every move a handler admits.
type moveGateStore struct {
	*pauseStore
	site   types.SiteConfig
	admits int
}

func (s *moveGateStore) GetSiteConfig(context.Context) (types.SiteConfig, error) { return s.site, nil }

func (s *moveGateStore) AdmitRunMove(_ context.Context, mv types.RunMove, _ store.RunMoveLimits) (types.RunMove, error) {
	s.admits++
	return mv, nil
}

const runMoveStoredOn = `{"features":{"run_move":{"enabled":true,"to_personal_runners":true,"max_carried_gib":9}}}`

// TestMoveUnreachableInEveryConfiguration is the proof that no configuration reaches a move on this tree, on the real
// hook registration: site config refuses to turn it on, and with it stored on, every move answers move_unavailable
// and nothing is stopped, created or written.
func TestMoveUnreachableInEveryConfiguration(t *testing.T) {
	if moveGateOpen() {
		t.Fatal("the move gate is open on this tree")
	}
	for _, body := range []string{
		`{"features":{"run_move":{"enabled":true}}}`,
		`{"features":{"run_move":{"max_carried_gib":9}}}`,
		`{"features":{"run_move":{"to_personal_runners":true}}}`,
	} {
		fake := &fakeSiteConfigStore{}
		wantSiteFeatureRefusal(t, putSiteFeatures(t, fake, body), fake, reasonSiteFeatureUnavailable)
	}
	stored := siteFeaturesDoc(t, runMoveStoredOn)
	fake := &fakeSiteConfigStore{cfg: stored}
	srv, _ := newSiteConfigHarness(t, fake)
	get := do(t, srv, http.MethodGet, "/api/v1/site-config", adminToken, "")
	if w := putSiteFeatures(t, fake, get.Body.String()); w.Code != http.StatusOK {
		t.Fatalf("PUT of the stored run_move block = %d %s, want 200 unchanged", w.Code, w.Body.String())
	}
	if !reflect.DeepEqual(fake.putSeen.FeatureSwitches, stored.FeatureSwitches) {
		t.Errorf("PUT of the stored block stored %+v, want it unchanged", fake.putSeen.FeatureSwitches)
	}
	if moveAvailable(stored) {
		t.Error("a stored run_move true reads on")
	}
	for gib, ok := range map[int]bool{-1: false, 0: true, 1: true, 200: true, 201: false} {
		next := &types.SiteFeatureSwitches{RunMove: &types.RunMoveFeature{MaxCarriedGiB: gib}}
		if err := validateRunMoveFeature(context.Background(), nil, next, nil); (err == nil) != ok {
			t.Errorf("max_carried_gib %d: %v, want accepted %v", gib, err, ok)
		}
	}
	// The settings still read as stored, and as their defaults when absent: they apply once a move can be made.
	if !moveToPersonalRunners(stored) || moveMaxCarriedBytes(stored) != 9<<30 ||
		moveToPersonalRunners(types.SiteConfig{}) || moveMaxCarriedBytes(types.SiteConfig{}) != 20<<30 {
		t.Errorf("run_move settings read %v %d stored, %v %d absent; want true 9 GiB and false 20 GiB",
			moveToPersonalRunners(stored), moveMaxCarriedBytes(stored), moveToPersonalRunners(types.SiteConfig{}), moveMaxCarriedBytes(types.SiteConfig{}))
	}
	if v, ok := setupFeatureFlags(t, stored)["run_move"]; ok {
		t.Errorf("GET /setup/status features.run_move = %s, want it absent", v)
	}

	stopped := types.PauseReason("stopped")
	for _, state := range []struct {
		name   string
		reason types.PauseReason
	}{{"paused-stopped", stopped}, {"paused-frozen", types.PauseIdle}, {"running", ""}} {
		t.Run(state.name, func(t *testing.T) {
			f, ms := newMoveGateFixture(t)
			if state.reason != "" {
				paused := time.Now().UTC()
				f.st.run.PausedAt, f.st.run.PausedReason = &paused, state.reason
			}
			requests := 0
			for _, to := range []string{"runner:" + f.run.ID.String(), "pool:" + f.run.ID.String(), "laptop", "org", "site:eu"} {
				body := `{"to":"` + to + `"}`
				for actor, w := range moveAsEveryActor(t, f, body) {
					requests++
					if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), `"reason":"`+reasonMoveUnavailable+`"`) {
						t.Errorf("%s, to %s: %d %s, want 409 %s", actor, to, w.Code, w.Body.String(), reasonMoveUnavailable)
					}
				}
			}
			if freezes, thaws := f.rn.counts(); f.rn.createCalls != 0 || freezes != 0 || thaws != 0 || ms.admits != 0 {
				t.Errorf("creates %d freezes %d thaws %d admits %d, want nothing done", f.rn.createCalls, freezes, thaws, ms.admits)
			}
			if rows := f.rows("run.move", "denied"); len(rows) != requests {
				t.Errorf("run.move denied rows = %d, want one per refused move (%d)", len(rows), requests)
			}
		})
	}

	f, _ := newMoveGateFixture(t)
	if err := f.srv.startMove(context.Background(), f.run); !errors.Is(err, errMoveUnavailable) {
		t.Errorf("startMove = %v, want errMoveUnavailable", err)
	}
	withMoveGateOpen(t)
	if err := f.srv.startMove(context.Background(), f.run); !errors.Is(err, errMoveUnavailable) {
		t.Errorf("startMove with the gate open = %v, want errMoveUnavailable: this build has no executor", err)
	}
}

// newMoveGateFixture is the pause fixture's running run, with run_move stored on.
func newMoveGateFixture(t *testing.T) (*pauseFixture, *moveGateStore) {
	t.Helper()
	var ms *moveGateStore
	f := newPauseFixture(t, time.Hour, func(c *Config) {
		ms = &moveGateStore{pauseStore: c.Store.(*pauseStore), site: siteFeaturesDoc(t, runMoveStoredOn)}
		c.Store = ms
	})
	return f, ms
}

// moveAsEveryActor sends one move as the run's owner, a super admin, the admin token and a service identity that
// owns the run. A service identity's token never reaches the route (service_scope), so its request is handed to the
// handler directly, as the router would after authentication.
func moveAsEveryActor(t *testing.T, f *pauseFixture, body string) map[string]*httptest.ResponseRecorder {
	t.Helper()
	path := "/api/v1/runs/" + f.run.ID.String() + "/move"
	out := map[string]*httptest.ResponseRecorder{
		"owner":       doSSO(t, f.srv, http.MethodPost, path, ssoSession(t, pauseOwner, "owner@corp.example", oidc.RoleUser), body),
		"super admin": doSSO(t, f.srv, http.MethodPost, path, ssoSession(t, "sub-super", "super@corp.example", oidc.RoleAdmin), body),
		"admin token": do(t, f.srv, http.MethodPost, path, adminToken, body),
	}
	saved := f.st.run.CreatedBy
	f.st.run.CreatedBy = types.ServicePrincipalPrefix + "move-bot"
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", f.run.ID.String())
	r = r.WithContext(context.WithValue(withServiceIdentity(r.Context(), "move-bot", pauseOwner), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()
	f.srv.handleMoveRun(w, r)
	out["service identity"] = w
	f.st.run.CreatedBy = saved
	return out
}

// TestMoveGateCallers: startMove is called only by handleMoveRun and the move sweep, and never read as a value.
func TestMoveGateCallers(t *testing.T) {
	allowed := map[string]bool{"handleMoveRun": true, "sweepRunMoves": true}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	callers := map[string]int{}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			called := map[*ast.SelectorExpr]bool{}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				if c, ok := n.(*ast.CallExpr); ok {
					if sel, ok := c.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "startMove" {
						called[sel] = true
						callers[fd.Name.Name]++
						if !allowed[fd.Name.Name] {
							t.Errorf("%s: %s calls startMove; only handleMoveRun and the move sweep may", name, fd.Name.Name)
						}
					}
				}
				if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == "startMove" && !called[sel] {
					t.Errorf("%s: %s reads startMove without calling it", name, fd.Name.Name)
				}
				return true
			})
		}
	}
	if callers["handleMoveRun"] == 0 {
		t.Fatal("handleMoveRun does not call startMove: the guard reads nothing")
	}
}

// TestMoveGate_NotDelegable: a delegated token reaches neither move route.
func TestMoveGate_NotDelegable(t *testing.T) {
	for route, ok := range delegationAllowed {
		if ok && (strings.HasSuffix(route, "/move") || strings.HasSuffix(route, "/placements")) {
			t.Errorf("delegationAllowed lists %s", route)
		}
	}
	for _, route := range []string{"POST /api/v1/runs/{id}/move", "GET /api/v1/runs/{id}/placements"} {
		if delegationAllowed[route] {
			t.Errorf("delegationAllowed lists %s", route)
		}
	}
}

// The seam guard. moveSeamPort is a port variable's name; the type movePort and its method are not seams.
var moveSeamPort = regexp.MustCompile(`^movePort[A-Z]\w*$`)

const (
	moveGateFile  = "internal/api/run_move_gate.go"
	movePortsFile = "internal/api/run_move_ports.go"
)

func isMoveSeam(name string) bool { return name == "moveGateOpen" || moveSeamPort.MatchString(name) }

// moveSeamFindings is the seam guard's check of one file at rel, a slash path from the module root: src is its text,
// nil for an assembly file.
func moveSeamFindings(rel string, src []byte) []string {
	if strings.HasSuffix(rel, ".s") {
		if strings.HasPrefix(rel, "internal/api/") {
			return []string{rel + ": an assembly file in internal/api, which this guard cannot read"}
		}
		return nil
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, rel, src, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return []string{rel + ": " + err.Error()}
	}
	var out []string
	at := func(p token.Pos, msg string) { out = append(out, fset.Position(p).String()+": "+msg) }
	for _, cg := range f.Comments {
		for _, c := range cg.List {
			if fields := strings.Fields(c.Text); len(fields) > 0 && fields[0] == "//go:linkname" {
				for _, sym := range fields[1:] {
					if isMoveSeam(sym[strings.LastIndex(sym, ".")+1:]) {
						at(c.Pos(), "a //go:linkname names "+sym)
					}
				}
			}
			if (rel == moveGateFile || rel == movePortsFile) && (strings.HasPrefix(c.Text, "//go:build") || strings.HasPrefix(c.Text, "// +build")) {
				at(c.Pos(), "a build constraint on the move gate's own file")
			}
		}
	}
	inAPI := strings.HasPrefix(rel, "internal/api/") && !strings.Contains(strings.TrimPrefix(rel, "internal/api/"), "/")
	var stack []ast.Node
	ast.Inspect(f, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		parent := ast.Node(nil)
		if len(stack) > 0 {
			parent = stack[len(stack)-1]
		}
		stack = append(stack, n)
		if gd, ok := n.(*ast.GenDecl); ok && inAPI {
			for _, sp := range gd.Specs {
				if vs, ok := sp.(*ast.ValueSpec); ok {
					for _, name := range vs.Names {
						if name.Name == "moveReachable" && (gd.Tok != token.CONST || vs.Type != nil) {
							at(name.Pos(), "moveReachable is declared as something other than an untyped const")
						}
					}
				}
			}
		}
		id, ok := n.(*ast.Ident)
		if !ok || !isMoveSeam(id.Name) {
			return true
		}
		if msg := moveSeamUse(rel, id, parent, stack); msg != "" {
			at(id.Pos(), msg)
		}
		return true
	})
	return out
}

// moveSeamUse is what is wrong with one use of a seam, or "" for an allowed one: its declaration in its own file,
// moveGateOpen(), or movePortX.call(...).
func moveSeamUse(rel string, id *ast.Ident, parent ast.Node, stack []ast.Node) string {
	grand := ast.Node(nil)
	if len(stack) > 2 {
		grand = stack[len(stack)-3]
	}
	switch p := parent.(type) {
	case *ast.ValueSpec:
		for _, name := range p.Names {
			if name == id {
				if (id.Name == "moveGateOpen" && rel == moveGateFile) || (id.Name != "moveGateOpen" && rel == movePortsFile) {
					return ""
				}
				return "declares " + id.Name + " outside its own file"
			}
		}
	case *ast.SelectorExpr:
		if p.Sel == id {
			return "" // a field or method of another value that shares the name
		}
		if c, ok := grand.(*ast.CallExpr); ok && c.Fun == p && p.Sel.Name == "call" && id.Name != "moveGateOpen" {
			return ""
		}
	case *ast.CallExpr:
		if p.Fun == id {
			if id.Name == "moveGateOpen" {
				return ""
			}
			return "calls " + id.Name + " directly; call a port only through its call method"
		}
	case *ast.AssignStmt:
		for _, l := range p.Lhs {
			if l == id {
				return "assigns " + id.Name + "; only a _test.go file may"
			}
		}
	case *ast.IncDecStmt:
		return "assigns " + id.Name + "; only a _test.go file may"
	case *ast.UnaryExpr:
		if p.Op == token.AND {
			return "takes the address of " + id.Name
		}
	}
	return "reads " + id.Name + " without calling it"
}

// moduleRoot is the directory holding go.mod, two levels up from internal/api.
func moduleRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("no go.mod at %s: %v", root, err)
	}
	return root
}

// TestMoveGateSeamsSetOnlyInTests reads every non-test Go file in the module, behind any build constraint, and every
// assembly file in internal/api, and fails on any use of the move gate or a port but a call.
func TestMoveGateSeamsSetOnlyInTests(t *testing.T) {
	root := moduleRoot(t)
	files, gateFiles := 0, 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" || d.Name() == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		var src []byte
		switch {
		case strings.HasSuffix(rel, ".s"):
		case strings.HasSuffix(rel, ".go") && !strings.HasSuffix(rel, "_test.go"):
			if src, err = os.ReadFile(path); err != nil {
				return err
			}
			files++
			if rel == moveGateFile || rel == movePortsFile {
				gateFiles++
			}
		default:
			return nil
		}
		for _, msg := range moveSeamFindings(rel, src) {
			t.Error(msg)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if files < 1000 || gateFiles != 2 {
		t.Fatalf("read %d non-test Go files (%d of the gate's two), want the whole module", files, gateFiles)
	}
}

// TestMoveGateSeamsSetOnlyInTests_Planted: each form the guard refuses reports one finding, wherever it is and
// whatever constraint its file carries; a call, the wrapper call and a use of the type report none.
func TestMoveGateSeamsSetOnlyInTests_Planted(t *testing.T) {
	const api = "internal/api/planted.go"
	for _, tc := range []struct {
		rel, src string
		want     int
	}{
		{api, `func f() { moveGateOpen = func() bool { return true } }`, 1},
		{api, `func f() { movePortPauseStop = nil }`, 1},
		{api, `func f() { _ = &moveGateOpen }`, 1},
		{api, `func f() { g := moveGateOpen; _ = g }`, 1},
		{api, `func f() { g := movePortPauseStop.call; _ = g }`, 1},
		{api, `func f() { use(movePortRevokeFor) }`, 1},
		{api, `var x = struct{ f func() bool }{moveGateOpen}`, 1},
		{api, `func f() { movePortPauseStop(ctx, a) }`, 1},
		{api, "import _ \"unsafe\"\n//go:linkname x github.com/cjohnstoniv/wardyn/internal/api.moveGateOpen\nvar x func() bool", 1},
		{moveGateFile, "//go:build never\n\npackage api\n", 1},
		{movePortsFile, "//go:build never\n\npackage api\n", 1},
		{api, `const moveReachable bool = false`, 1},
		{api, `var moveReachable = false`, 1},
		{"internal/api/planted.s", "", 1},
		{api, "//go:build ignore\n\npackage api\nfunc f() { moveGateOpen = nil }", 1},
		{"test/planted/planted.go", "package planted\nfunc f() { movePortPauseStop = nil }", 1},
		{api, `func f() { if moveGateOpen() {} }`, 0},
		{api, `func f() { movePortPauseStop.call(ctx, a) }`, 0},
		{api, `var p movePort[moveStopArgs, movePauseReceipt]`, 0},
		{api, `const moveReachable = false`, 0},
	} {
		src := tc.src
		if !strings.Contains(src, "package ") && !strings.HasSuffix(tc.rel, ".s") {
			src = "package api\n" + src + "\n"
		}
		if got := moveSeamFindings(tc.rel, []byte(src)); len(got) != tc.want {
			t.Errorf("%s %q: %d findings %v, want %d", tc.rel, tc.src, len(got), got, tc.want)
		}
	}
}
