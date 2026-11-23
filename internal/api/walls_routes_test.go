// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// wallsRoutesWireTypes are the Walls view's wire types. TestWallsPayloadsCarryNoMountsKey walks each one and every
// type it reaches; the counters behind an any field are listed because a walk cannot see through any.
var wallsRoutesWireTypes = []reflect.Type{
	reflect.TypeFor[WallsTopology](), reflect.TypeFor[WallsRuns](), reflect.TypeFor[WallsEdges](),
	reflect.TypeFor[WallsEdge](), reflect.TypeFor[WallsHost](), reflect.TypeFor[WallsUnit](),
	reflect.TypeFor[WallsSubject](), reflect.TypeFor[WallsUnitRuns](), reflect.TypeFor[WallsRunItem](),
	reflect.TypeFor[WallsCounters](), reflect.TypeFor[WallsUnitCounters](), reflect.TypeFor[WallsElsewhere](),
	reflect.TypeFor[WallsBoundary](), reflect.TypeFor[WallsRestrictionRef](), reflect.TypeFor[WallsDevices](),
	reflect.TypeFor[WallsPool](), reflect.TypeFor[WallsSite](), reflect.TypeFor[WallsGate](),
	reflect.TypeFor[RunWalls](), reflect.TypeFor[RunWallsLayer](), reflect.TypeFor[RunWallsPath](),
	reflect.TypeFor[RunWallsStruck](), reflect.TypeFor[RunWallsRemovedBy](), reflect.TypeFor[RunWallsGrant](),
	reflect.TypeFor[RunWallsHold](), reflect.TypeFor[RunWallsDoor](), reflect.TypeFor[RunWallsExit](),
	reflect.TypeFor[RunWallsInner](),
}

// wallsRoutesStub is one stub and the path it is probed at.
type wallsRoutesStub struct {
	pattern string
	run     bool // a run stub: its own run gate, not requireUnitScope
}

// wallsRoutesStubs are the four stubs, at the patterns mountWallsRoutes mounts their handlers on.
var wallsRoutesStubs = []wallsRoutesStub{
	{"/api/v1/walls/topology", false},
	{"/api/v1/walls/stream", false},
	{"/api/v1/runs/{id}/walls", true},
	{"/api/v1/runs/{id}/walls/stream", true},
}

// TestWallsPayloadsCarryNoMountsKey: no Walls view wire type, and no type one reaches, has a json key named mounts
// or a field holding a policy spec, a unit row, an overlay, a restriction or the boundary; and every field of a
// wire type names its json key.
func TestWallsPayloadsCarryNoMountsKey(t *testing.T) {
	banned := map[reflect.Type]bool{
		reflect.TypeFor[types.RunPolicySpec](): true, reflect.TypeFor[types.GovernanceUnit](): true,
		reflect.TypeFor[types.CeilingOverlay](): true, reflect.TypeFor[types.Restriction](): true,
		reflect.TypeFor[types.Boundary](): true,
	}
	own := reflect.TypeFor[WallsTopology]().PkgPath()
	module := own[:strings.Index(own, "/internal/")+1]
	seen := map[reflect.Type]bool{}
	var walk func(at string, rt reflect.Type)
	walk = func(at string, rt reflect.Type) {
		for rt.Kind() == reflect.Pointer || rt.Kind() == reflect.Slice || rt.Kind() == reflect.Array || rt.Kind() == reflect.Map {
			if rt.Kind() == reflect.Map {
				walk(at+"[key]", rt.Key())
			}
			rt = rt.Elem()
		}
		if banned[rt] {
			t.Errorf("%s holds %s, which no Walls view payload may carry", at, rt)
		}
		if rt.Kind() != reflect.Struct || seen[rt] || !strings.HasPrefix(rt.PkgPath(), module) {
			return
		}
		seen[rt] = true
		for i := range rt.NumField() {
			f := rt.Field(i)
			key, _, _ := strings.Cut(f.Tag.Get("json"), ",")
			if rt.PkgPath() == own && key == "" {
				t.Errorf("%s.%s names no json key", at, f.Name)
			}
			if key == "mounts" {
				t.Errorf("%s.%s is the json key mounts", at, f.Name)
			}
			walk(at+"."+f.Name, f.Type)
		}
	}
	for _, rt := range wallsRoutesWireTypes {
		walk(rt.Name(), rt)
	}
	if !seen[reflect.TypeFor[types.RunnerView]()] {
		t.Error("the walk never reached types.RunnerView, so it no longer follows the fields of the wire types")
	}
}

// TestWallsStubsKeepTheGate: each stub, mounted as its handler is, answers only past that handler's gate. A
// non-owner's run stub gets the missing run's 404, byte for byte; a member's unit-tier stub gets 403
// unit_scope_denied; whoever passes the gate gets 501 walls_view_unavailable.
func TestWallsStubsKeepTheGate(t *testing.T) {
	srv, ast, _, _ := newAuthzMatrixServer(t)
	stubs := wallsRoutesStubRouter(srv)
	const owner = "sub-walls-owner"
	run := wallsRoutesSeedRun(ast, owner)
	ownerSess := ssoSession(t, owner, "walls-owner@corp.example", oidc.RoleUser)
	memberSess := ssoSession(t, "sub-walls-member", "walls-member@corp.example", oidc.RoleUser)
	secSess := ssoSession(t, secAdminSub, secAdminMail, oidc.RoleSecurityAdmin)
	unitAdminSess := ssoSession(t, matrixUnitAdminSub, "unit-admin@corp.example", oidc.RoleUser)

	for _, stub := range wallsRoutesStubs {
		t.Run(stub.pattern, func(t *testing.T) {
			path := buildPath(stub.pattern, run.String())
			if stub.run {
				wallsRoutesWantUnavailable(t, "the run's owner", wallsRoutesServe(t, stubs, path, ownerSess, ""))
				wallsRoutesWantUnavailable(t, "a security admin", wallsRoutesServe(t, stubs, path, secSess, ""))
				foreign := wallsRoutesServe(t, stubs, path, memberSess, "")
				if foreign.Code != http.StatusNotFound {
					t.Errorf("a non-owning member: %d %s, want the missing run's 404", foreign.Code, foreign.Body.String())
				}
				assertSameAsMissing404(t, srv, http.MethodGet, stub.pattern, memberSess, "", foreign, "a non-owning member")
				return
			}
			wallsRoutesWantUnavailable(t, "a security admin", wallsRoutesServe(t, stubs, path, secSess, ""))
			wallsRoutesWantUnavailable(t, "a unit admin", wallsRoutesServe(t, stubs, path, unitAdminSess, ""))
			if w := wallsRoutesServe(t, stubs, path, memberSess, ""); w.Code != http.StatusForbidden || errorReason(w) != string(authz.ReasonUnitScopeDenied) {
				t.Errorf("a member: %d %s, want 403 %s", w.Code, w.Body.String(), authz.ReasonUnitScopeDenied)
			}
		})
	}
}

// TestWallsRoutesRefuseDelegatedAndServiceTokens: a portal's delegated token and a service identity's token are
// refused on all four Walls view routes at the door, with the delegation and service allow-lists' reasons, the
// owner's own run included.
func TestWallsRoutesRefuseDelegatedAndServiceTokens(t *testing.T) {
	const person = "sub-walls-portal-person"
	srv, ast, _, _ := newAuthzMatrixServer(t)
	delegated, _ := seedDelegation(t, ast.fakeDelegateStore, person)
	svc, svcAST, svcToken := wallsRoutesServiceServer(t)
	run, svcRun := wallsRoutesSeedRun(ast, person), wallsRoutesSeedRun(svcAST, types.ServicePrincipalPrefix+wallsRoutesServiceName)
	for _, stub := range wallsRoutesStubs {
		if w := wallsRoutesServe(t, srv.Handler(), buildPath(stub.pattern, run.String()), nil, delegated); w.Code != http.StatusForbidden ||
			errorReason(w) != string(authz.ReasonDelegationScope) {
			t.Errorf("a delegated token on %s: %d %s, want 403 %s", stub.pattern, w.Code, w.Body.String(), authz.ReasonDelegationScope)
		}
		if w := wallsRoutesServe(t, svc.Handler(), buildPath(stub.pattern, svcRun.String()), nil, svcToken); w.Code != http.StatusForbidden ||
			errorReason(w) != string(authz.ReasonServiceScope) {
			t.Errorf("a service token on %s: %d %s, want 403 %s", stub.pattern, w.Code, w.Body.String(), authz.ReasonServiceScope)
		}
	}
}

// wallsRoutesStubRouter mounts the four stubs as mountWallsRoutes mounts the handlers they stand in for: in the
// authenticated group behind its middleware, the two unit-tier ones also behind requireUnitScope.
func wallsRoutesStubRouter(s *Server) http.Handler {
	r := chi.NewRouter()
	r.Route("/api/v1", func(r chi.Router) {
		r.Group(func(r chi.Router) {
			r.Use(s.humanOrAdminAuth, s.expectedPrincipalGuard, s.auditStrictIntent)
			unit := r.With(s.requireUnitScope)
			unit.Get("/walls/topology", s.handleWallsTopologyStub)
			unit.Get("/walls/stream", s.handleWallsStreamStub)
			r.Get("/runs/{id}/walls", s.handleRunWallsStub)
			r.Get("/runs/{id}/walls/stream", s.handleRunWallsStreamStub)
		})
	})
	return r
}

// wallsRoutesSeedRun is a running run of owner's in the matrix store.
func wallsRoutesSeedRun(ast *authzStore, owner string) uuid.UUID {
	id := uuid.New()
	ast.mu.Lock()
	defer ast.mu.Unlock()
	ast.runs[id] = types.AgentRun{ID: id, CreatedBy: owner, State: types.RunRunning, Agent: "claude-code"}
	return id
}

// wallsRoutesServe sends one GET to h with a session cookie or a bearer. The request ends once h answers
// anything, so a live stream returns at its first write and a refusal or a stub as usual (wallsStreamAuthzRecorder).
func wallsRoutesServe(t *testing.T, h http.Handler, path string, cookie *http.Cookie, bearer string) *httptest.ResponseRecorder {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	r := httptest.NewRequestWithContext(ctx, http.MethodGet, path, nil)
	if cookie != nil {
		r.AddCookie(cookie)
	}
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
		r.Host, r.RemoteAddr = "127.0.0.1", "127.0.0.1:54321" // as doVia sends a bearer
	}
	w := httptest.NewRecorder()
	panicFails(t, h).ServeHTTP(wallsStreamAuthzRecorder{ResponseRecorder: w, answered: cancel}, r)
	return w
}

// wallsRoutesWantUnavailable fails t unless w is a stub's 501 walls_view_unavailable.
func wallsRoutesWantUnavailable(t *testing.T, who string, w *httptest.ResponseRecorder) {
	t.Helper()
	if w.Code != http.StatusNotImplemented || errorReason(w) != reasonWallsViewUnavailable {
		t.Errorf("%s: %d %s, want 501 %s", who, w.Code, w.Body.String(), reasonWallsViewUnavailable)
	}
}

// wallsRoutesServiceName is the one service identity wallsRoutesServiceStore holds.
const wallsRoutesServiceName = "walls-ci"

// wallsRoutesServiceStore is the matrix store with one usable service identity and one live token of it.
type wallsRoutesServiceStore struct {
	*authzStore
	store.ServiceIdentityStore // only GetServiceIdentity is asked
	raw                        string
	row                        types.APIToken
}

func (s wallsRoutesServiceStore) GetAPITokenByRaw(_ context.Context, raw string) (types.APIToken, error) {
	if raw != s.raw {
		return types.APIToken{}, store.ErrNotFound
	}
	return s.row, nil
}

func (s wallsRoutesServiceStore) GetServiceIdentity(context.Context, string) (types.ServiceIdentity, error) {
	return types.ServiceIdentity{Name: wallsRoutesServiceName, Sponsor: "sub-walls-sponsor"}, nil
}

// wallsRoutesServiceServer is a matrix server whose store holds wallsRoutesServiceName and a token of it, which it
// returns raw. The matrix's session revocations answer every sponsor live, so the token authenticates.
func wallsRoutesServiceServer(t *testing.T) (*Server, *authzStore, string) {
	t.Helper()
	raw := newBearer(apiTokenPrefix)
	row := types.APIToken{ID: uuid.New(), Principal: types.ServicePrincipalPrefix + wallsRoutesServiceName, CreatedAt: time.Now().UTC()}
	var ast *authzStore
	srv, _, _, _ := newAuthzMatrixServer(t, func(c *Config) {
		ast = c.Store.(*authzStore)
		c.Store = wallsRoutesServiceStore{authzStore: ast, raw: raw, row: row}
	})
	return srv, ast, raw
}
