// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/keydomain"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// directoryFake is the people half of store.Store over memory: the person store,
// the directory page read, and the token/workspace reads the owner resolver
// uses, so the people and key-domain handlers run with no Postgres.
type directoryFake struct {
	*roleMapStore
	mu     sync.Mutex
	people map[string]types.Person
	tokens []types.APIToken

	createErr, listErr, dirErr error
	page                       store.PeopleDirectoryPage
	gotFilter                  store.PeopleDirectoryFilter
}

func newDirectoryFake() *directoryFake {
	return &directoryFake{roleMapStore: &roleMapStore{}, people: map[string]types.Person{}}
}

func (s *directoryFake) ListAPITokens(context.Context) ([]types.APIToken, error) {
	return s.tokens, nil
}

func (s *directoryFake) ListWorkspaces(context.Context) ([]types.Workspace, error) { return nil, nil }

func (s *directoryFake) CreatePerson(_ context.Context, p types.Person) (types.Person, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.createErr != nil {
		return types.Person{}, false, s.createErr
	}
	if have, ok := s.people[p.Principal]; ok {
		return have, false, nil
	}
	p.CreatedAt = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	s.people[p.Principal] = p
	return p, true, nil
}

func (s *directoryFake) GetPerson(_ context.Context, principal string) (types.Person, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p, ok := s.people[principal]; ok {
		return p, nil
	}
	return types.Person{}, store.ErrNotFound
}

func (s *directoryFake) ListPeople(context.Context) ([]types.Person, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listErr != nil {
		return nil, s.listErr
	}
	out := make([]types.Person, 0, len(s.people))
	for _, p := range s.people {
		out = append(out, p)
	}
	return out, nil
}

func (s *directoryFake) MarkPersonSignedIn(context.Context, string, time.Time) error { return nil }

func (s *directoryFake) ListPeopleDirectory(_ context.Context, f store.PeopleDirectoryFilter) (store.PeopleDirectoryPage, error) {
	s.gotFilter = f
	return s.page, s.dirErr
}

func peopleServer(t *testing.T, st store.Store, auth *oidc.Authenticator) (*Server, *harness) {
	t.Helper()
	h := newHarness(t)
	cfg := baseTestConfig(h, st)
	cfg.OIDC = auth
	return New(cfg), h
}

func TestPeopleCreate_RefusalsAnswerBeforeAnythingIsWritten(t *testing.T) {
	st := newDirectoryFake()
	st.people["taken-sub"] = types.Person{Principal: "taken-sub", Email: "taken@corp.example"}
	st.people["Mixed-Case"] = types.Person{Principal: "Mixed-Case"}
	srv, h := peopleServer(t, st, nil)

	for _, c := range []struct {
		name, body string
		want       int
		reason     string
	}{
		{"an unknown field", `{"principal":"p","bogus":1}`, http.StatusBadRequest, ""},
		{"object-id keying on a non-Entra deployment", `{"tenant_id":"x","object_id":"y"}`, http.StatusUnprocessableEntity, reasonPersonPrincipalInvalid},
		{"a subject with a space", `{"principal":"has space"}`, http.StatusUnprocessableEntity, reasonPersonPrincipalInvalid},
		{"no subject", `{"email":"a@corp.example"}`, http.StatusUnprocessableEntity, reasonPersonPrincipalInvalid},
		{"a reserved subject", `{"principal":"local:root"}`, http.StatusUnprocessableEntity, reasonPersonPrincipalReserved},
		{"a bad email", `{"principal":"fresh","email":"not-an-email"}`, http.StatusUnprocessableEntity, reasonPersonEmailInvalid},
		{"a known subject under a different email", `{"principal":"taken-sub","email":"other@corp.example"}`, http.StatusConflict, reasonPersonCollision},
		{"a subject differing only by case", `{"principal":"mixed-case"}`, http.StatusConflict, reasonPersonCollision},
		{"an email another subject holds", `{"principal":"fresh","email":"TAKEN@corp.example"}`, http.StatusConflict, reasonPersonCollision},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := do(t, srv, http.MethodPost, "/api/v1/people", adminToken, c.body)
			if w.Code != c.want || (c.reason != "" && errorReason(w) != c.reason) {
				t.Errorf("POST /people = %d %s, want %d %s", w.Code, w.Body.String(), c.want, c.reason)
			}
		})
	}
	if _, ok := st.people["fresh"]; ok || len(st.people) != 2 {
		t.Errorf("a refused create stored people: %v", st.people)
	}
	for _, ev := range h.audit.snapshot() {
		if ev.Action == "person.create" && ev.Outcome == "success" {
			t.Errorf("a refused create audited a success: %+v", ev)
		}
	}
}

func TestPeopleCreate_CreatesOnceAndConfirmsTheSecondTime(t *testing.T) {
	st := newDirectoryFake()
	srv, h := peopleServer(t, st, nil)
	body := `{"principal":"new-sub","email":"  New@Corp.Example "}`

	w := do(t, srv, http.MethodPost, "/api/v1/people", adminToken, body)
	if w.Code != http.StatusCreated {
		t.Fatalf("first POST = %d %s, want 201", w.Code, w.Body.String())
	}
	var p types.Person
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	if p.Principal != "new-sub" || p.Email != "new@corp.example" || p.CreatedBy == "" {
		t.Errorf("person = %+v, want the folded email and the creating admin recorded", p)
	}

	w = do(t, srv, http.MethodPost, "/api/v1/people", adminToken, body)
	if w.Code != http.StatusOK {
		t.Errorf("second POST = %d %s, want 200 (confirm, not create)", w.Code, w.Body.String())
	}
	var rows int
	for _, ev := range h.audit.snapshot() {
		if ev.Action == "person.create" && ev.Outcome == "success" && ev.Target == "new-sub" {
			rows++
		}
	}
	if rows != 1 {
		t.Errorf("person.create success rows = %d, want exactly 1 (the confirm writes none)", rows)
	}
}

func TestPeopleCreate_StoreFailures(t *testing.T) {
	for _, c := range []struct {
		name  string
		setup func(*directoryFake)
		want  int
	}{
		{"the listing read fails", func(s *directoryFake) { s.listErr = errors.New("boom") }, http.StatusInternalServerError},
		{"the email is taken at the store", func(s *directoryFake) { s.createErr = store.ErrConflict }, http.StatusConflict},
		{"the insert fails", func(s *directoryFake) { s.createErr = errors.New("boom") }, http.StatusInternalServerError},
	} {
		t.Run(c.name, func(t *testing.T) {
			st := newDirectoryFake()
			c.setup(st)
			srv, _ := peopleServer(t, st, nil)
			w := do(t, srv, http.MethodPost, "/api/v1/people", adminToken, `{"principal":"p1","email":"p1@corp.example"}`)
			if w.Code != c.want {
				t.Errorf("POST = %d %s, want %d", w.Code, w.Body.String(), c.want)
			}
			if c.want == http.StatusConflict && errorReason(w) != reasonPersonEmailTaken {
				t.Errorf("reason = %q, want %q", errorReason(w), reasonPersonEmailTaken)
			}
		})
	}
}

func TestPeopleCreate_NeedsAPersonStore(t *testing.T) {
	srv, _ := peopleServer(t, &roleMapStore{}, nil)
	for _, path := range []string{"/api/v1/people"} {
		w := do(t, srv, http.MethodPost, path, adminToken, `{"principal":"p"}`)
		if w.Code != http.StatusNotImplemented || errorReason(w) != reasonPeopleStoreUnavailable {
			t.Errorf("POST %s = %d %s, want 501 %s", path, w.Code, w.Body.String(), reasonPeopleStoreUnavailable)
		}
	}
	if w := do(t, srv, http.MethodGet, "/api/v1/people", adminToken, ""); w.Code != http.StatusNotImplemented || errorReason(w) != reasonPeopleStoreUnavailable {
		t.Errorf("GET /people = %d %s, want 501 %s", w.Code, w.Body.String(), reasonPeopleStoreUnavailable)
	}
}

func TestPeopleList_ParamsReachTheStoreAndBadOnesAreRefused(t *testing.T) {
	st := newDirectoryFake()
	srv, _ := peopleServer(t, st, nil)
	cursor := base64.RawURLEncoding.EncodeToString([]byte("sub-050"))

	if w := do(t, srv, http.MethodGet, "/api/v1/people?q=%20ann%20&state=active&limit=7&cursor="+cursor, adminToken, ""); w.Code != http.StatusOK {
		t.Fatalf("GET = %d %s, want 200", w.Code, w.Body.String())
	}
	want := store.PeopleDirectoryFilter{Query: "ann", State: "active", After: "sub-050", Limit: 7}
	if st.gotFilter != want {
		t.Errorf("filter = %+v, want %+v", st.gotFilter, want)
	}

	for _, c := range []struct{ query, reason string }{
		{"state=gone", reasonPeopleListParamInvalid},
		{"limit=abc", reasonInvalidLimitParam},
		{"limit=-1", reasonInvalidLimitParam},
		{"cursor=!!not-base64!!", reasonPeopleListParamInvalid},
	} {
		w := do(t, srv, http.MethodGet, "/api/v1/people?"+c.query, adminToken, "")
		if w.Code != http.StatusBadRequest || errorReason(w) != c.reason {
			t.Errorf("GET ?%s = %d %s, want 400 %s", c.query, w.Code, w.Body.String(), c.reason)
		}
	}

	st.dirErr = errors.New("boom")
	if w := do(t, srv, http.MethodGet, "/api/v1/people", adminToken, ""); w.Code != http.StatusInternalServerError {
		t.Errorf("a failing directory read = %d, want 500", w.Code)
	}
}

func TestPeopleList_RowsCarryIssuerKindRoleAndANextCursor(t *testing.T) {
	st := newDirectoryFake()
	seen := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	st.page = store.PeopleDirectoryPage{
		Next: "sub-zed",
		People: []store.PersonListing{
			{Principal: "ann-sub", Email: "ann@corp.example", LastSignInAt: &seen, ActiveSessions: 1, APITokens: 2, SSHKeys: 3, Credentials: 4, ActiveRuns: 5},
			{Principal: "oid-person", Email: "oid@corp.example", Entra: true, PreCreated: true},
			{Principal: "local:root", Email: ""},
			{Principal: "stranger-sub", Email: "stranger@elsewhere.example"},
		},
	}
	// A role map is set and there is no default, so an email no mapping names is denied.
	auth := newAccessAuth(t, map[string]string{"ann@corp.example": oidc.RoleSecurityAdmin, "oid@corp.example": oidc.RoleUser}, "", nil, nil)
	srv, _ := peopleServer(t, st, auth)

	w := do(t, srv, http.MethodGet, "/api/v1/people", adminToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET = %d %s", w.Code, w.Body.String())
	}
	var got types.PersonList
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.People) != 4 {
		t.Fatalf("people = %d, want 4", len(got.People))
	}
	for i, want := range []struct{ kind, role string }{
		{issuerKindOIDC, oidc.RoleSecurityAdmin},
		{issuerKindEntra, oidc.RoleUser},
		{issuerKindLocal, accessDeniedRole},
		{issuerKindOIDC, accessDeniedRole},
	} {
		if got.People[i].IssuerKind != want.kind || got.People[i].Role != want.role {
			t.Errorf("%s: kind %q role %q, want %q %q", got.People[i].Principal, got.People[i].IssuerKind, got.People[i].Role, want.kind, want.role)
		}
	}
	first := got.People[0]
	if first.ActiveSessions != 1 || first.APITokens != 2 || first.SSHKeys != 3 || first.Credentials != 4 || first.ActiveRuns != 5 || first.LastSignedInAt == nil {
		t.Errorf("counts were not carried through: %+v", first)
	}
	if !got.People[1].PreCreated {
		t.Error("pre_created was dropped")
	}
	if raw, err := base64.RawURLEncoding.DecodeString(got.NextCursor); err != nil || string(raw) != "sub-zed" {
		t.Errorf("next_cursor = %q, want the encoding of sub-zed", got.NextCursor)
	}
}

func TestPeopleList_RoleReadFailuresAreServerErrors(t *testing.T) {
	for name, mutate := range map[string]func(*roleMapStore){
		"role mappings": func(s *roleMapStore) { s.listErr = errors.New("boom") },
		"user types":    func(s *roleMapStore) { s.userTypesErr = errors.New("boom") },
	} {
		t.Run(name, func(t *testing.T) {
			st := newDirectoryFake()
			mutate(st.roleMapStore)
			srv, _ := peopleServer(t, st, newAccessAuth(t, map[string]string{"a@corp.example": oidc.RoleUser}, oidc.RoleUser, nil, nil))
			if w := do(t, srv, http.MethodGet, "/api/v1/people", adminToken, ""); w.Code != http.StatusInternalServerError {
				t.Errorf("GET = %d %s, want 500", w.Code, w.Body.String())
			}
		})
	}
}

// keyDomainServer is a Server whose key-domain service sits on a Postgres that
// refuses every connection, declaring the one domain "vault-a". Whatever reaches
// the service is therefore an outage; what the handlers decide before it must
// answer without it.
func keyDomainServer(t *testing.T, st store.Store) *Server {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), "postgres://wardyn@127.0.0.1:1/wardyn?connect_timeout=1&sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	h := newHarness(t)
	cfg := baseTestConfig(h, st)
	cfg.KeyDomains = keydomain.NewService(pool, []string{"vault-a"})
	return New(cfg)
}

func TestKeyDomains_NeedPostgres(t *testing.T) {
	srv, _ := peopleServer(t, newDirectoryFake(), nil)
	for _, c := range []struct{ method, path, body string }{
		{http.MethodGet, "/api/v1/key-domains", ""},
		{http.MethodPut, "/api/v1/key-domains/assignments/all/all", `{"domain":"vault-a"}`},
		{http.MethodDelete, "/api/v1/key-domains/assignments/all/all", ""},
	} {
		w := do(t, srv, c.method, c.path, adminToken, c.body)
		if w.Code != http.StatusNotImplemented || errorReason(w) != reasonKeyDomainsStoreUnavailable {
			t.Errorf("%s %s = %d %s, want 501 %s", c.method, c.path, w.Code, w.Body.String(), reasonKeyDomainsStoreUnavailable)
		}
	}
}

func TestKeyDomains_RequestsAreValidatedBeforeTheServiceIsAsked(t *testing.T) {
	st := newDirectoryFake()
	st.tokens = []types.APIToken{{Principal: "Bob", Email: "bob@corp.example"}, {Principal: "bob", Email: "robert@corp.example"}, {Principal: "carol-sub", Email: "carol@corp.example"}}
	srv := keyDomainServer(t, st)
	long := strings.Repeat("g", maxKeyDomainSubjectLen+1)

	put := func(subjectType, subject, body string) (int, string) {
		w := do(t, srv, http.MethodPut, "/api/v1/key-domains/assignments/"+subjectType+"/"+subject, adminToken, body)
		return w.Code, errorReason(w)
	}
	for _, c := range []struct {
		name, subjectType, subject, body, reason string
		want                                     int
	}{
		{"an unknown subject type", "planet", "mars", `{"domain":"vault-a"}`, reasonKeyDomainRequestInvalid, http.StatusBadRequest},
		{"no domain", "all", "all", `{"domain":"  "}`, reasonKeyDomainRequestInvalid, http.StatusBadRequest},
		{"an unknown body field", "all", "all", `{"domain":"vault-a","x":1}`, "", http.StatusBadRequest},
		{"all with another subject", "all", "everyone", `{"domain":"vault-a"}`, reasonKeyDomainRequestInvalid, http.StatusBadRequest},
		{"a group that is not printable ASCII", "group", "caf%C3%A9", `{"domain":"vault-a"}`, reasonKeyDomainRequestInvalid, http.StatusBadRequest},
		{"an over-long group", "group", long, `{"domain":"vault-a"}`, reasonKeyDomainRequestInvalid, http.StatusBadRequest},
		{"a user named by blanks", "user", "%20%20", `{"domain":"vault-a"}`, reasonKeyDomainRequestInvalid, http.StatusBadRequest},
		{"a user email nobody here has", "user", "ghost%40corp.example", `{"domain":"vault-a"}`, reasonOwnerUnresolved, http.StatusUnprocessableEntity},
		{"a user two subjects fold onto", "user", "BOB", `{"domain":"vault-a"}`, reasonOwnerAmbiguous, http.StatusUnprocessableEntity},
		{"a domain the file does not declare", "all", "all", `{"domain":"nowhere"}`, string(authz.ReasonKeyDomainUnknown), http.StatusUnprocessableEntity},
	} {
		t.Run(c.name, func(t *testing.T) {
			code, reason := put(c.subjectType, c.subject, c.body)
			if code != c.want || (c.reason != "" && reason != c.reason) {
				t.Errorf("PUT = %d %q, want %d %q", code, reason, c.want, c.reason)
			}
		})
	}

	// A malformed DELETE is refused the same way, and never reads a body.
	w := do(t, srv, http.MethodDelete, "/api/v1/key-domains/assignments/planet/mars", adminToken, "")
	if w.Code != http.StatusBadRequest || errorReason(w) != reasonKeyDomainRequestInvalid {
		t.Errorf("DELETE of an unknown subject type = %d %s, want 400 %s", w.Code, w.Body.String(), reasonKeyDomainRequestInvalid)
	}
}

func TestKeyDomains_AValidRequestThatReachesAnUnansweringPostgresIsAServerError(t *testing.T) {
	st := newDirectoryFake()
	st.tokens = []types.APIToken{{Principal: "carol-sub", Email: "carol@corp.example"}}
	srv := keyDomainServer(t, st)

	for _, c := range []struct{ name, method, path, body string }{
		{"the list", http.MethodGet, "/api/v1/key-domains", ""},
		{"a group set checks membership first", http.MethodPut, "/api/v1/key-domains/assignments/group/Eng", `{"domain":"vault-a"}`},
		{"a user set", http.MethodPut, "/api/v1/key-domains/assignments/user/carol%40corp.example", `{"domain":"vault-a"}`},
		{"a user delete", http.MethodDelete, "/api/v1/key-domains/assignments/user/carol-sub", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := do(t, srv, c.method, c.path, adminToken, c.body)
			if w.Code != http.StatusInternalServerError {
				t.Errorf("%s %s = %d %s, want 500", c.method, c.path, w.Code, w.Body.String())
			}
		})
	}
}

func TestKeyDomainTargetAndDeclaredList(t *testing.T) {
	if got := keyDomainTarget(keyDomainChange{SubjectType: keydomain.SubjectAll}); got != "all" {
		t.Errorf("target of an all assignment = %q, want all", got)
	}
	if got := keyDomainTarget(keyDomainChange{SubjectType: keydomain.SubjectGroup, Subject: "Eng"}); got != "group:Eng" {
		t.Errorf("target = %q, want group:Eng", got)
	}
	svc := keydomain.NewService(nil, []string{"zeta", "alpha"})
	if got := declaredKeyDomains(svc); got != keydomain.Default+", alpha, zeta" {
		t.Errorf("declared = %q, want the default first then the declared names sorted", got)
	}
}
