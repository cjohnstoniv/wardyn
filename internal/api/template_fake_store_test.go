// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// fakeTemplateStore is the in-memory store.TemplateStore the template route
// tests use: the same name, limit, revision and grant rules as the Postgres
// store (store_run_templates_pg_test.go pins those against the real one).
type fakeTemplateStore struct {
	mu        sync.Mutex
	rows      map[uuid.UUID]types.RunTemplate
	revisions map[uuid.UUID][]types.RunTemplate
	grants    []types.TemplateGroupAdmin
	labels    map[string]string
	// tokens is the identity directory the grant route resolves a person in.
	tokens []types.APIToken
}

func newFakeTemplateStore() *fakeTemplateStore {
	return &fakeTemplateStore{rows: map[uuid.UUID]types.RunTemplate{}, revisions: map[uuid.UUID][]types.RunTemplate{}, labels: map[string]string{}}
}

var _ store.TemplateStore = (*fakeTemplateStore)(nil)

func (f *fakeTemplateStore) ListTemplatesVisible(_ context.Context, person string, groups []string) ([]types.RunTemplate, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []types.RunTemplate
	for _, t := range f.rows {
		if t.Owner.Scope == types.TemplateScopeOrg || (t.Owner.Scope == types.TemplateScopePerson && t.Owner.Person == person) ||
			(t.Owner.Scope == types.TemplateScopeGroup && slices.Contains(groups, t.Owner.Group)) {
			out = append(out, t)
		}
	}
	slices.SortFunc(out, func(a, b types.RunTemplate) int {
		return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
	})
	return out, nil
}

func (f *fakeTemplateStore) GetTemplate(_ context.Context, id uuid.UUID) (types.RunTemplate, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.rows[id]
	if !ok {
		return t, store.ErrNotFound
	}
	return t, nil
}

func (f *fakeTemplateStore) GetTemplateRevision(_ context.Context, id uuid.UUID, revision int) (types.RunTemplate, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.revisions[id] {
		if r.Revision == revision {
			cur := f.rows[id]
			r.CreatedAt, r.CreatedBy = cur.CreatedAt, cur.CreatedBy
			return r, nil
		}
	}
	return types.RunTemplate{}, store.ErrNotFound
}

func (f *fakeTemplateStore) grantHeld(g *store.TemplateGrantCheck) bool {
	return g == nil || slices.ContainsFunc(f.grants, func(x types.TemplateGroupAdmin) bool {
		return x.Group == g.Group && slices.Contains(g.Persons, x.Person)
	})
}

func (f *fakeTemplateStore) nameTaken(t types.RunTemplate) bool {
	for _, o := range f.rows {
		if o.ID != t.ID && o.Owner == t.Owner && strings.EqualFold(o.Name, t.Name) {
			return true
		}
	}
	return false
}

func (f *fakeTemplateStore) CreateTemplate(_ context.Context, t types.RunTemplate, grant *store.TemplateGrantCheck) (types.RunTemplate, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.grantHeld(grant) {
		return types.RunTemplate{}, store.ErrTemplateGrantGone
	}
	n := 0
	for _, o := range f.rows {
		if o.Owner == t.Owner {
			n++
		}
	}
	if limit := store.TemplateLimit(t.Owner); n >= limit {
		return types.RunTemplate{}, &store.TemplateLimitError{Limit: limit}
	}
	if f.nameTaken(t) {
		return types.RunTemplate{}, store.ErrTemplateNameTaken
	}
	now := time.Now().UTC()
	t.Revision, t.CreatedAt, t.UpdatedAt, t.UpdatedBy = 1, now, now, t.CreatedBy
	f.rows[t.ID] = t
	f.revisions[t.ID] = append(f.revisions[t.ID], t)
	return t, nil
}

func (f *fakeTemplateStore) UpdateTemplate(_ context.Context, t types.RunTemplate, expected int, grant *store.TemplateGrantCheck) (types.RunTemplate, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.grantHeld(grant) {
		return types.RunTemplate{}, store.ErrTemplateGrantGone
	}
	cur, ok := f.rows[t.ID]
	switch {
	case !ok:
		return types.RunTemplate{}, store.ErrNotFound
	case cur.Revision != expected:
		return types.RunTemplate{}, &store.TemplateRevisionConflictError{Current: cur.Revision}
	}
	t.Owner, t.Kind = cur.Owner, cur.Kind
	if f.nameTaken(t) {
		return types.RunTemplate{}, store.ErrTemplateNameTaken
	}
	cur.Name, cur.Description, cur.Document = t.Name, t.Description, t.Document
	cur.Revision++
	cur.UpdatedAt, cur.UpdatedBy = time.Now().UTC(), t.UpdatedBy
	f.rows[t.ID] = cur
	f.revisions[t.ID] = append(f.revisions[t.ID], cur)
	return cur, nil
}

func (f *fakeTemplateStore) DeleteTemplate(_ context.Context, id uuid.UUID, grant *store.TemplateGrantCheck) (types.RunTemplate, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.grantHeld(grant) {
		return types.RunTemplate{}, store.ErrTemplateGrantGone
	}
	t, ok := f.rows[id]
	if !ok {
		return t, store.ErrNotFound
	}
	delete(f.rows, id)
	delete(f.revisions, id)
	return t, nil
}

func (f *fakeTemplateStore) ListTemplateGroupAdmins(context.Context) ([]types.TemplateGroupAdmin, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := slices.Clone(f.grants)
	for i := range out {
		out[i].Label = f.labels[out[i].Group]
	}
	return out, nil
}

func (f *fakeTemplateStore) PutTemplateGroupAdmin(_ context.Context, g types.TemplateGroupAdmin) (types.TemplateGroupAdmin, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.grants = slices.DeleteFunc(f.grants, func(x types.TemplateGroupAdmin) bool { return x.Group == g.Group && x.Person == g.Person })
	if g.Label != "" {
		f.labels[g.Group] = g.Label
	}
	g.Label = f.labels[g.Group]
	stored := g
	stored.Label = ""
	f.grants = append(f.grants, stored)
	return g, nil
}

func (f *fakeTemplateStore) DeleteTemplateGroupAdmin(_ context.Context, group, person string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := len(f.grants)
	f.grants = slices.DeleteFunc(f.grants, func(x types.TemplateGroupAdmin) bool { return x.Group == group && x.Person == person })
	return len(f.grants) < n, nil
}

func (f *fakeTemplateStore) TemplateGroupAdminGroups(_ context.Context, persons []string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, g := range f.grants {
		if slices.Contains(persons, g.Person) && !slices.Contains(out, g.Group) {
			out = append(out, g.Group)
		}
	}
	return out, nil
}

func (f *fakeTemplateStore) TemplateGroupLabels(_ context.Context, groups []string) (map[string]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]string{}
	for _, g := range groups {
		if l, ok := f.labels[g]; ok {
			out[g] = l
		}
	}
	return out, nil
}

func (f *fakeTemplateStore) EraseRunSetupsOf(_ context.Context, person string) (store.RunSetupsErased, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out store.RunSetupsErased
	for id, t := range f.rows {
		if t.Owner.Scope == types.TemplateScopePerson && t.Owner.Person == person {
			delete(f.rows, id)
			delete(f.revisions, id)
			out.Templates++
			continue
		}
		if t.CreatedBy == person || t.UpdatedBy == person {
			t.CreatedBy, t.UpdatedBy = blankIf(t.CreatedBy, person), blankIf(t.UpdatedBy, person)
			f.rows[id] = t
			out.AuthorshipCleared++
		}
		for i, r := range f.revisions[id] {
			if r.UpdatedBy == person {
				f.revisions[id][i].UpdatedBy = ""
				out.AuthorshipCleared++
			}
		}
	}
	n := len(f.grants)
	f.grants = slices.DeleteFunc(f.grants, func(g types.TemplateGroupAdmin) bool { return g.Person == person })
	out.Grants = n - len(f.grants)
	return out, nil
}

func blankIf(v, person string) string {
	if v == person {
		return ""
	}
	return v
}

// templateTestStore is the routes' store: the package's quiet base fake for
// everything else, and the template seam above.
type templateTestStore struct {
	rbacStore
	*fakeTemplateStore
}

// ListAPITokens is the identity directory the grant route resolves a person in.
func (s templateTestStore) ListAPITokens(context.Context) ([]types.APIToken, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.tokens), nil
}

// signIn records a person in the identity directory, as their first token does.
func (e *templateEnv) signIn(principal, email string) {
	e.ts.mu.Lock()
	defer e.ts.mu.Unlock()
	e.ts.tokens = append(e.ts.tokens, types.APIToken{ID: uuid.New(), Principal: principal, Email: email})
}

// templateEnv is a server with SSO on and the fake template store.
type templateEnv struct {
	t     *testing.T
	srv   *Server
	ts    *fakeTemplateStore
	audit *recRecorder
}

func newTemplateEnv(t *testing.T) *templateEnv {
	t.Helper()
	h := newHarness(t)
	ts := newFakeTemplateStore()
	cfg := baseTestConfig(h, templateTestStore{fakeTemplateStore: ts})
	cfg.OIDC = &oidc.Authenticator{}
	cfg.Secrets = getErrStore{getErr: secretstore.ErrNotFound}
	return &templateEnv{t: t, srv: New(cfg), ts: ts, audit: h.audit}
}

// templateCookie is an SSO session: role, the User view (member mode), and a
// group snapshot (nil: none recorded; truncated: incomplete).
func templateCookie(t *testing.T, sub, role string, memberMode bool, groups []string, truncated bool) *http.Cookie {
	t.Helper()
	payload, err := json.Marshal(oidc.Session{V: oidc.SessionCodecVersion, Sub: sub, Email: sub + "@corp.example", Role: role,
		UserType: types.UserTypeStandard, MemberMode: memberMode, UserViewType: userViewTypeIf(memberMode),
		Groups: groups, GroupsTruncated: truncated, Expiry: time.Now().UTC().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	return signedSessionCookie(payload)
}

func userViewTypeIf(on bool) string {
	if on {
		return types.UserTypeStandard
	}
	return ""
}

func (e *templateEnv) call(c *http.Cookie, method, path, body string) (int, string) {
	e.t.Helper()
	w := doSSO(e.t, e.srv, method, path, c, body)
	return w.Code, w.Body.String()
}

// seed stores a template directly, as its author saved it.
func (e *templateEnv) seed(owner types.TemplateOwner, kind types.TemplateKind, name, intent string) types.RunTemplate {
	e.t.Helper()
	doc := `{"api_version":"wardyn/v1","kind":"RunTemplate","coverage":"partial","intent":` + intent + `}`
	t, err := e.ts.CreateTemplate(context.Background(), types.RunTemplate{
		ID: uuid.New(), Owner: owner, Kind: kind, Name: name, Document: json.RawMessage(doc), CreatedBy: "author-of-" + name,
	}, nil)
	if err != nil {
		e.t.Fatalf("seed %s: %v", name, err)
	}
	return t
}

func (e *templateEnv) grant(group, person string) {
	e.t.Helper()
	if _, err := e.ts.PutTemplateGroupAdmin(context.Background(), types.TemplateGroupAdmin{Group: group, Person: person, GrantedBy: "admin"}); err != nil {
		e.t.Fatal(err)
	}
}
