// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package adofake is a local, hermetic fake of the Azure DevOps HTTP surface
// Wardyn's ADO broker/gate work (#375) talks to: git smart-HTTP (served by
// the real `git http-backend`, not reimplemented), and the REST endpoints for
// branch policies, pull requests, refs, pushes, work items, connection data,
// projects and the personal-access-token lifecycle.
//
// The fake exists to make a NARROWED token provable without parsing it: every
// endpoint declares the real Azure DevOps OAuth scope it requires, a test
// registers which scopes a given bearer/PAT carries, and a request presenting
// a token that lacks the scope gets back exactly the shape the real service
// returns — a 401 with typeKey UnauthorizedRequestException. A caller that can
// only see the fake's responses (not its internals) still learns whether its
// token was accepted for the right reason.
package adofake

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"
)

// The real Azure DevOps OAuth scope literals each endpoint below requires —
// see https://learn.microsoft.com/azure/devops/integrate/get-started/authentication/oauth
// for the canonical list. A fake that invented its own names would not prove
// anything about a token minted against the real scope model.
const (
	ScopeCodeRead    = "vso.code"
	ScopeCodeWrite   = "vso.code_write"
	ScopeWorkRead    = "vso.work"
	ScopeWorkWrite   = "vso.work_write"
	ScopeProjectRead = "vso.project"
	ScopeTokens      = "vso.tokens"
)

// Endpoint names one handler's counter/override slot and its entry in a
// recorded request — stable strings a test can assert against, independent of
// the URL shape underneath.
type Endpoint string

const (
	EndpointGitAdvertise      Endpoint = "git.advertise"
	EndpointGitUploadPack     Endpoint = "git.upload-pack"
	EndpointGitReceivePack    Endpoint = "git.receive-pack"
	EndpointPolicyGet         Endpoint = "policy.get"
	EndpointPolicyPost        Endpoint = "policy.post"
	EndpointPolicyPut         Endpoint = "policy.put"
	EndpointPullRequestsPost  Endpoint = "pullrequests.post"
	EndpointPullRequestsPatch Endpoint = "pullrequests.patch"
	EndpointRefsPost          Endpoint = "refs.post"
	EndpointPushesGet         Endpoint = "pushes.get"
	EndpointPushesPost        Endpoint = "pushes.post"
	EndpointWiqlPost          Endpoint = "wiql.post"
	EndpointWorkItemsPatch    Endpoint = "workitems.patch"
	EndpointConnectionData    Endpoint = "connectiondata.get"
	EndpointProjectsGet       Endpoint = "projects.get"
	EndpointRepositoriesGet   Endpoint = "repositories.get"
	EndpointPatsList          Endpoint = "tokens.pats.list"
	EndpointPatsCreate        Endpoint = "tokens.pats.create"
	EndpointPatsUpdate        Endpoint = "tokens.pats.update"
	EndpointPatsRevoke        Endpoint = "tokens.pats.revoke"
)

// RecordedRequest is one call the fake answered, kept so a test can assert
// what actually reached the forge — method, path, every header seen, and the
// scope decision the fake made.
type RecordedRequest struct {
	Method        string
	Path          string
	Query         string
	Headers       http.Header
	Endpoint      Endpoint
	RequiredScope string // "" when the endpoint needs no scope
	Token         string // the bearer/PAT presented, "" if none
	Authorized    bool   // true when no scope was required, or the token carried it
	// APIVersion is the request's ?api-version= value, "" if absent. Real
	// Azure DevOps requires this on every REST call; this fake does NOT
	// refuse its absence (that would break every caller that hasn't wired it
	// up yet), but records it so a lane that cares can assert its own client
	// actually sent one.
	APIVersion string
}

type override struct {
	status int
	header http.Header // nil: Content-Type application/json
	body   []byte
}

// write answers with the override: its own headers when it carries any (a
// fault's shape, content type included, or none), else JSON.
func (ov override) write(w http.ResponseWriter) {
	if ov.header == nil {
		w.Header().Set("Content-Type", "application/json")
	}
	for k, vs := range ov.header {
		w.Header()[k] = vs
	}
	w.WriteHeader(ov.status)
	_, _ = w.Write(ov.body)
}

// tokenGrant is what RegisterToken (or a minted PAT) attaches to a token: the
// scopes it carries, and — for a PAT — the validTo it expires at (zero means
// no expiry, e.g. a plain RegisterToken caller).
type tokenGrant struct {
	scopes  map[string]bool
	validTo time.Time
}

func setOf(scopes []string) map[string]bool {
	set := make(map[string]bool, len(scopes))
	for _, sc := range scopes {
		set[sc] = true
	}
	return set
}

// Server is the fake Azure DevOps org. Zero value is not usable; use New.
type Server struct {
	httpSrv *httptest.Server

	mu sync.Mutex

	tokens map[string]*tokenGrant // token -> grant

	requests  []RecordedRequest
	overrides map[Endpoint]override
	counts    map[Endpoint]int

	repos map[string]string // "org/project/repo" -> bare repo path

	policies     []policyConfig
	nextPolicyID int

	pullRequests map[int]map[string]any
	nextPRID     int

	pushes     []map[string]any
	nextPushID int

	projects map[string][]map[string]any // org -> registered project fixtures

	pats           map[string]*pat // authorizationId -> pat
	patCreateError PatTokenError

	workItemRev map[int]int // work item id -> current revision
}

// New starts a fake Azure DevOps server.
func New() *Server {
	s, h := NewHandler()
	s.httpSrv = httptest.NewServer(h)
	return s
}

// NewHandler returns an UNSTARTED fake plus its handler, for a caller that owns
// its own listener (test/adofake/cmd serves it behind a TLS front). Mirrors
// entrafake.NewHandler; an unstarted Server's URL is "".
func NewHandler() (*Server, http.Handler) {
	s := &Server{
		tokens:       map[string]*tokenGrant{},
		overrides:    map[Endpoint]override{},
		counts:       map[Endpoint]int{},
		repos:        map[string]string{},
		pullRequests: map[int]map[string]any{},
		nextPRID:     1,
		nextPushID:   1,
		projects:     map[string][]map[string]any{},
		pats:         map[string]*pat{},
		workItemRev:  map[int]int{},
	}
	return s, s.handler()
}

// URL is the fake's base URL (an org's own address in the real service — the
// caller supplies the {org}/{project} path segments).
func (s *Server) URL() string {
	if s.httpSrv == nil {
		return ""
	}
	return s.httpSrv.URL
}

// Close shuts down the underlying httptest server.
func (s *Server) Close() {
	if s.httpSrv != nil {
		s.httpSrv.Close()
	}
}

// RegisterToken makes token (a bearer access token OR a personal access
// token, presented either as "Authorization: Bearer <token>" or as HTTP Basic
// with the token as the password) carry exactly scopes, with no expiry —
// replacing any previous grant for the same token. A PAT minted through
// handlePatsCreate registers itself the same way, but with the validTo it
// declared.
func (s *Server) RegisterToken(token string, scopes ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokens[token] = &tokenGrant{scopes: setOf(scopes)}
}

// SetOverride forces every subsequent call to endpoint to answer status/body
// verbatim, bypassing scope enforcement — for exercising a retry or a sweep
// against a simulated outage. ClearOverride restores normal behaviour.
func (s *Server) SetOverride(endpoint Endpoint, status int, body []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.overrides[endpoint] = override{status: status, body: body}
}

// ClearOverride removes a forced response set by SetOverride.
func (s *Server) ClearOverride(endpoint Endpoint) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.overrides, endpoint)
}

// Count returns how many requests endpoint has answered so far (overridden
// responses count too).
func (s *Server) Count(endpoint Endpoint) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.counts[endpoint]
}

// Requests returns every recorded request in arrival order.
func (s *Server) Requests() []RecordedRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]RecordedRequest(nil), s.requests...)
}

// AddProject registers a project fixture that GET /{org}/_apis/projects
// returns; id defaults to name if empty.
func (s *Server) AddProject(org, id, name string) {
	if id == "" {
		id = name
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.projects[org] = append(s.projects[org], map[string]any{
		"id": id, "name": name, "state": "wellFormed", "visibility": "private",
	})
}

// tokenFromRequest extracts the presented token from either a Bearer
// Authorization header or HTTP Basic auth (the personal-access-token
// convention: an empty or arbitrary username, the PAT as the password).
func tokenFromRequest(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	if after, ok := strings.CutPrefix(auth, "Bearer "); ok {
		return after
	}
	if _, pass, ok := r.BasicAuth(); ok {
		return pass
	}
	return ""
}

// checkScope reports whether the request's token carries scope AND, for a
// token with a validTo (a minted PAT), has not expired. An empty scope means
// the endpoint needs none, and is always granted regardless of the token.
func (s *Server) checkScope(r *http.Request, scope string) (token string, granted bool) {
	token = tokenFromRequest(r)
	if scope == "" {
		return token, true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	g, ok := s.tokens[token]
	if !ok {
		return token, false
	}
	if !g.validTo.IsZero() && time.Now().After(g.validTo) {
		return token, false
	}
	return token, g.scopes[scope]
}

// record appends a RecordedRequest and bumps endpoint's counter. Headers are
// cloned so a later mutation of the live request can't change history.
func (s *Server) record(endpoint Endpoint, scope string, r *http.Request, token string, authorized bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.counts[endpoint]++
	s.requests = append(s.requests, RecordedRequest{
		Method:        r.Method,
		Path:          r.URL.Path,
		Query:         r.URL.RawQuery,
		Headers:       r.Header.Clone(),
		Endpoint:      endpoint,
		RequiredScope: scope,
		Token:         token,
		Authorized:    authorized,
		APIVersion:    r.URL.Query().Get("api-version"),
	})
}

// writeADOUnauthorized writes the real Azure DevOps shape for a bearer that
// lacks the required scope: TF400813, typeKey UnauthorizedRequestException,
// eventId 3000 — matching the live service's TfsServiceException wire shape
// field-for-field, so a caller can tell "wrong scope" from any other 401.
func writeADOUnauthorized(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"$id":            "1",
		"innerException": nil,
		"message":        "TF400813: The user is not authorized to access this resource.",
		"typeName":       "Microsoft.TeamFoundation.Framework.Server.UnauthorizedRequestException, Microsoft.TeamFoundation.Framework.Server",
		"typeKey":        "UnauthorizedRequestException",
		"errorCode":      0,
		"eventId":        3000,
	})
}

// requireScope wraps next with request recording, an optional forced
// SetOverride response, and — when scope is non-empty — enforcement that the
// presented token carries it. The scope decision is always computed and
// recorded, even when an override answers instead: a lane asserting on
// Requests() must see what WOULD have happened, not a decision that never
// ran.
func (s *Server) requireScope(endpoint Endpoint, scope string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, granted := s.checkScope(r, scope)
		s.record(endpoint, scope, r, token, granted)

		s.mu.Lock()
		ov, overridden := s.overrides[endpoint]
		s.mu.Unlock()
		if overridden {
			ov.write(w)
			return
		}
		if scope != "" && !granted {
			writeADOUnauthorized(w)
			return
		}
		next(w, r)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// handler builds the full route table. Every route is mounted under BOTH its
// org-scoped and project-scoped URL where the real API documents both (git
// APIs address a repository by id, so the project segment is redundant but
// accepted), so a caller using either documented form gets a real answer
// instead of an indistinguishable-from-a-refusal 404.
func (s *Server) handler() http.Handler {
	mux := http.NewServeMux()

	// Git smart-HTTP — see git.go.
	mux.HandleFunc("/{org}/{project}/_git/{repo}/{path...}", s.handleGit)

	// Branch policies — project-scoped, as in the real API.
	mux.HandleFunc("GET /{org}/{project}/_apis/policy/configurations",
		s.requireScope(EndpointPolicyGet, ScopeCodeRead, s.handlePolicyGet))
	mux.HandleFunc("POST /{org}/{project}/_apis/policy/configurations",
		s.requireScope(EndpointPolicyPost, ScopeCodeWrite, s.handlePolicyPost))
	mux.HandleFunc("PUT /{org}/{project}/_apis/policy/configurations/{id}",
		s.requireScope(EndpointPolicyPut, ScopeCodeWrite, s.handlePolicyPut))

	// Pull requests, refs, pushes — addressed by repository id, which the
	// real API accepts both org-scoped and project-scoped.
	prPost := s.requireScope(EndpointPullRequestsPost, ScopeCodeWrite, s.handlePullRequestsPost)
	mux.HandleFunc("POST /{org}/_apis/git/repositories/{repoId}/pullrequests", prPost)
	mux.HandleFunc("POST /{org}/{project}/_apis/git/repositories/{repoId}/pullrequests", prPost)

	prPatch := s.requireScope(EndpointPullRequestsPatch, ScopeCodeWrite, s.handlePullRequestsPatch)
	mux.HandleFunc("PATCH /{org}/_apis/git/repositories/{repoId}/pullrequests/{prId}", prPatch)
	mux.HandleFunc("PATCH /{org}/{project}/_apis/git/repositories/{repoId}/pullrequests/{prId}", prPatch)

	refsPost := s.requireScope(EndpointRefsPost, ScopeCodeWrite, s.handleRefsPost)
	mux.HandleFunc("POST /{org}/_apis/git/repositories/{repoId}/refs", refsPost)
	mux.HandleFunc("POST /{org}/{project}/_apis/git/repositories/{repoId}/refs", refsPost)

	pushesGet := s.requireScope(EndpointPushesGet, ScopeCodeRead, s.handlePushesGet)
	mux.HandleFunc("GET /{org}/_apis/git/repositories/{repoId}/pushes", pushesGet)
	mux.HandleFunc("GET /{org}/{project}/_apis/git/repositories/{repoId}/pushes", pushesGet)

	pushesPost := s.requireScope(EndpointPushesPost, ScopeCodeWrite, s.handlePushesPost)
	mux.HandleFunc("POST /{org}/_apis/git/repositories/{repoId}/pushes", pushesPost)
	mux.HandleFunc("POST /{org}/{project}/_apis/git/repositories/{repoId}/pushes", pushesPost)

	// Work item tracking — project-scoped. wiql additionally accepts a
	// {team}-scoped form (see stripWiqlTeamSegment: it is canonicalized onto
	// this same route in normalizeRouting rather than mounted as its own
	// pattern, because an unconstrained {team} wildcard there is not
	// provably disjoint from the git route's literal "_git" segment and
	// net/http's ServeMux refuses to register it).
	mux.HandleFunc("POST /{org}/{project}/_apis/wit/wiql",
		s.requireScope(EndpointWiqlPost, ScopeWorkRead, s.handleWiqlPost))
	mux.HandleFunc("PATCH /{org}/{project}/_apis/wit/workitems/{id}",
		s.requireScope(EndpointWorkItemsPatch, ScopeWorkWrite, s.handleWorkItemsPatch))

	// Org-level metadata.
	//
	// PENDING LIVE VERIFICATION (owner is checking tonight against a real
	// organisation): connectionData's required scope is undocumented by
	// Microsoft and is currently a guess (ScopeProjectRead); projects'
	// Learn page lists BOTH vso.profile and vso.project, and only the latter
	// is modelled here. Do not change either row until the observed values
	// come back.
	mux.HandleFunc("GET /{org}/_apis/connectionData",
		s.requireScope(EndpointConnectionData, ScopeProjectRead, s.handleConnectionData))
	mux.HandleFunc("GET /{org}/_apis/projects",
		s.requireScope(EndpointProjectsGet, ScopeProjectRead, s.handleProjectsGet))
	mux.HandleFunc("GET /{org}/{project}/_apis/git/repositories",
		s.requireScope(EndpointRepositoriesGet, ScopeCodeRead, s.handleRepositoriesGet))

	// Token lifecycle — see pats.go.
	//
	// PENDING LIVE VERIFICATION: Microsoft documents these four routes as
	// requiring an ENTRA token carrying vso.pats, on vssps.dev.azure.com, and
	// refusing a PAT presented over Basic auth entirely. This fake currently
	// accepts a PAT via Basic on the same dev.azure.com host as everything
	// else. Do not change this until the owner's live-tenant check comes
	// back.
	mux.HandleFunc("GET /{org}/_apis/tokens/pats",
		s.requireScope(EndpointPatsList, ScopeTokens, s.handlePatsList))
	mux.HandleFunc("POST /{org}/_apis/tokens/pats",
		s.requireScope(EndpointPatsCreate, ScopeTokens, s.handlePatsCreate))
	mux.HandleFunc("PUT /{org}/_apis/tokens/pats",
		s.requireScope(EndpointPatsUpdate, ScopeTokens, s.handlePatsUpdate))
	mux.HandleFunc("DELETE /{org}/_apis/tokens/pats",
		s.requireScope(EndpointPatsRevoke, ScopeTokens, s.handlePatsRevoke))

	return normalizeRouting(mux)
}

// normalizeRouting wraps mux so routing is lenient exactly where the real
// service is: a single trailing slash is tolerated, "_apis" plus
// "connectionData" match regardless of case, and a {team}-scoped wiql URL is
// canonicalized onto the plain project-scoped route. Segments that are
// caller-chosen values (org/project/repo/ids) are left untouched.
func normalizeRouting(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if len(path) > 1 && strings.HasSuffix(path, "/") {
			path = strings.TrimSuffix(path, "/")
		}
		segments := strings.Split(path, "/")
		for i, seg := range segments {
			switch strings.ToLower(seg) {
			case "_apis":
				segments[i] = "_apis"
			case "connectiondata":
				segments[i] = "connectionData"
			}
		}
		r.URL.Path = stripWiqlTeamSegment(strings.Join(segments, "/"))
		r.URL.RawPath = ""
		next.ServeHTTP(w, r)
	})
}

// stripWiqlTeamSegment canonicalizes the real API's {team}-scoped wiql URL
// (.../{project}/{team}/_apis/wit/wiql) onto the plain project-scoped one
// (.../{project}/_apis/wit/wiql), which is the only shape mounted in the
// route table.
//
// This is a path rewrite rather than a second mux.HandleFunc registration
// because an unconstrained {team} wildcard at that position is not provably
// disjoint from the git route's literal "_git" segment ("/org/project/_git/
// _apis/wit/wiql" would match a {team}-scoped pattern with team="_git" AND
// the git route with repo="_apis") — net/http's ServeMux refuses to register
// two patterns it cannot prove are non-overlapping, regardless of how
// unlikely that literal value is in practice.
func stripWiqlTeamSegment(path string) string {
	const suffix = "/_apis/wit/wiql"
	if !strings.HasSuffix(path, suffix) {
		return path
	}
	segs := strings.Split(strings.TrimSuffix(path, suffix), "/") // ["", org, project] or ["", org, project, team]
	if len(segs) != 4 {
		return path // already org/project-shaped, or not this endpoint at all
	}
	return "/" + segs[1] + "/" + segs[2] + suffix
}
