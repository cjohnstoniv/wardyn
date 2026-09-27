// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// userTypeStore is an in-memory user_types table: the seeded built-in row plus
// whatever a test writes. refs is what UserTypeReferences answers, tokens what
// UserTypeTokenStamps answers (tokensErr fails that read), and deleteConflict
// makes the store's own delete refuse (a row written between the handler's
// check and the DELETE).
type userTypeStore struct {
	store.Store
	rows           map[string]types.UserType
	refs, tokens   int
	tokensErr      error
	deleteConflict bool
}

func newUserTypeStore() *userTypeStore {
	return &userTypeStore{rows: map[string]types.UserType{
		types.UserTypeStandard: {ID: types.UserTypeStandard, Name: "Standard user", BuiltIn: true},
	}}
}

func (s *userTypeStore) ListUserTypes(context.Context) ([]types.UserType, error) {
	var out []types.UserType
	for _, t := range s.rows {
		out = append(out, t)
	}
	return out, nil
}

func (s *userTypeStore) GetUserType(_ context.Context, id string) (types.UserType, error) {
	t, ok := s.rows[id]
	if !ok {
		return types.UserType{}, store.ErrNotFound
	}
	return t, nil
}

func (s *userTypeStore) nameTaken(id, name string) bool {
	for _, t := range s.rows {
		if t.ID != id && t.Name == name {
			return true
		}
	}
	return false
}

func (s *userTypeStore) CreateUserType(_ context.Context, t types.UserType) (types.UserType, error) {
	if _, ok := s.rows[t.ID]; ok || s.nameTaken(t.ID, t.Name) {
		return types.UserType{}, store.ErrConflict
	}
	s.rows[t.ID] = t
	return t, nil
}

func (s *userTypeStore) UpdateUserType(_ context.Context, t types.UserType) (types.UserType, error) {
	cur, ok := s.rows[t.ID]
	if !ok {
		return types.UserType{}, store.ErrNotFound
	}
	if s.nameTaken(t.ID, t.Name) {
		return types.UserType{}, store.ErrConflict
	}
	cur.Name, cur.Description, cur.Priority = t.Name, t.Description, t.Priority
	s.rows[t.ID] = cur
	return cur, nil
}

func (s *userTypeStore) UserTypeReferences(context.Context, string) (int, error) { return s.refs, nil }

func (s *userTypeStore) UserTypeTokenStamps(context.Context, string) (int, error) {
	return s.tokens, s.tokensErr
}

func (s *userTypeStore) DeleteUserType(_ context.Context, id string) error {
	t, ok := s.rows[id]
	if !ok {
		return store.ErrNotFound
	}
	if t.BuiltIn || s.refs > 0 || s.tokens > 0 || s.deleteConflict {
		return store.ErrConflict
	}
	delete(s.rows, id)
	return nil
}

func userTypeServer(t *testing.T, st *userTypeStore, auth *oidc.Authenticator) (*Server, *harness) {
	t.Helper()
	h := newHarness(t)
	cfg := baseTestConfig(h, st)
	cfg.OIDC = auth
	return New(cfg), h
}

func userTypeError(t *testing.T, body []byte) string {
	t.Helper()
	var e errorBody
	if err := json.Unmarshal(body, &e); err != nil {
		t.Fatalf("decode error body %q: %v", body, err)
	}
	return e.Error
}

func auditActions(h *harness, action string) []types.AuditEvent {
	var out []types.AuditEvent
	for _, ev := range h.audit.events {
		if ev.Action == action {
			out = append(out, ev)
		}
	}
	return out
}

func TestUserTypeFromRequest(t *testing.T) {
	for _, tc := range []struct {
		name   string
		req    userTypeRequest
		wantID string
		want   string // a substring of the refusal; "" means accepted
	}{
		{"the id comes from the name", userTypeRequest{Name: "Portfolio manager"}, "portfolio-manager", ""},
		{"punctuation runs fold to one hyphen", userTypeRequest{Name: "  R&D -- Contractors (EU) "}, "r-d-contractors-eu", ""},
		{"an explicit id is kept", userTypeRequest{ID: "pm", Name: "Portfolio manager"}, "pm", ""},
		{"no name", userTypeRequest{Name: "   "}, "", "Give the type a name."},
		{"a name with no ASCII letters", userTypeRequest{Name: "日本"}, "", "its name has no letters or digits"},
		{"a control character in the name", userTypeRequest{Name: "a\nb"}, "", "no control characters"},
		{"a negative priority", userTypeRequest{Name: "x", Priority: -1}, "", "Priority must be"},
		{"a priority over the bound", userTypeRequest{Name: "x", Priority: 1001}, "", "Priority must be"},
		{"an upper-case id", userTypeRequest{ID: "PM", Name: "x"}, "", "The id must be"},
		{"a double hyphen", userTypeRequest{ID: "a--b", Name: "x"}, "", "The id must be"},
		{"a too-long id", userTypeRequest{ID: strings.Repeat("a", 64), Name: "x"}, "", "The id must be"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, msg := userTypeFromRequest(tc.req)
			if tc.want == "" {
				if msg != "" || got.ID != tc.wantID {
					t.Fatalf("got id %q, refusal %q; want id %q accepted", got.ID, msg, tc.wantID)
				}
				return
			}
			if !strings.Contains(msg, tc.want) {
				t.Fatalf("refusal = %q, want it to contain %q", msg, tc.want)
			}
		})
	}
}

// TestUserTypeReservedIDs pins that no type can take a word a role-map value
// already means: such a type could never be named by a map value, or would be
// read as a tier.
func TestUserTypeReservedIDs(t *testing.T) {
	for _, id := range []string{"admin", "security_admin", "user", "member", "denied"} {
		_, msg := userTypeFromRequest(userTypeRequest{ID: id, Name: "Sneaky"})
		if msg == "" {
			t.Errorf("id %q accepted; it is a role word", id)
		}
		// Named by the name, too: "Admin" slugs to "admin".
		if _, msg := userTypeFromRequest(userTypeRequest{Name: id}); id != "security_admin" && msg == "" {
			t.Errorf("name %q slugged to a reserved id and was accepted", id)
		}
	}
}

func TestUserTypeRefusalsStartWithACapital(t *testing.T) {
	for _, req := range []userTypeRequest{
		{}, {Name: "日本"}, {Name: "x", Priority: -1}, {ID: "admin", Name: "x"}, {ID: "A", Name: "x"},
		{Name: strings.Repeat("n", 200)}, {Name: "x", Description: "\x00"},
	} {
		if _, msg := userTypeFromRequest(req); msg == "" || msg[0] < 'A' || msg[0] > 'Z' {
			t.Errorf("refusal %q does not start with a capital letter", msg)
		}
	}
	for _, u := range []userTypeInUse{{chart: true}, {defaultRole: true}, {rows: 1}, {chart: true, rows: 2}, {tokens: 1}, {tokens: 3}} {
		if msg := u.refusal(); msg[0] < 'A' || msg[0] > 'Z' {
			t.Errorf("delete refusal %q does not start with a capital letter", msg)
		}
	}
}

func TestUserTypesCRUDAudits(t *testing.T) {
	st := newUserTypeStore()
	srv, h := userTypeServer(t, st, nil)

	w := do(t, srv, http.MethodPost, "/api/v1/user-types", adminToken, `{"name":"Portfolio manager","priority":10}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("POST = %d %s, want 201", w.Code, w.Body.String())
	}
	var created types.UserType
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil || created.ID != "portfolio-manager" || created.BuiltIn {
		t.Fatalf("created = %+v (%v), want id portfolio-manager, not built in", created, err)
	}

	w = do(t, srv, http.MethodPost, "/api/v1/user-types", adminToken, `{"id":"pm2","name":"Portfolio manager"}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("POST taken name = %d, want 409", w.Code)
	}

	w = do(t, srv, http.MethodPut, "/api/v1/user-types/portfolio-manager", adminToken,
		`{"name":"Portfolio manager","description":"Desk staff","priority":20}`)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT = %d %s, want 200", w.Code, w.Body.String())
	}
	w = do(t, srv, http.MethodPut, "/api/v1/user-types/portfolio-manager", adminToken, `{"id":"pm","name":"x"}`)
	if w.Code != http.StatusBadRequest || userTypeError(t, w.Body.Bytes()) != "A user type's id can't be changed." {
		t.Fatalf("PUT changing the id = %d %s, want the immutable-id 400", w.Code, w.Body.String())
	}
	w = do(t, srv, http.MethodPut, "/api/v1/user-types/nobody", adminToken, `{"name":"x"}`)
	if w.Code != http.StatusNotFound {
		t.Fatalf("PUT unknown = %d, want 404", w.Code)
	}

	// The built-in type is editable but has no priority.
	w = do(t, srv, http.MethodPut, "/api/v1/user-types/standard", adminToken, `{"name":"Everyone else","priority":5}`)
	if w.Code != http.StatusBadRequest || !strings.Contains(userTypeError(t, w.Body.Bytes()), "has no priority") {
		t.Fatalf("PUT standard priority = %d %s, want 400", w.Code, w.Body.String())
	}
	w = do(t, srv, http.MethodPut, "/api/v1/user-types/standard", adminToken, `{"name":"Everyone else"}`)
	if w.Code != http.StatusOK || st.rows["standard"].Name != "Everyone else" || !st.rows["standard"].BuiltIn {
		t.Fatalf("PUT standard rename = %d %s, row %+v", w.Code, w.Body.String(), st.rows["standard"])
	}

	w = do(t, srv, http.MethodGet, "/api/v1/user-types", adminToken, "")
	var list userTypesResponse
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &list) != nil || len(list.UserTypes) != 2 {
		t.Fatalf("GET = %d %s, want both types", w.Code, w.Body.String())
	}

	w = do(t, srv, http.MethodDelete, "/api/v1/user-types/portfolio-manager", adminToken, "")
	if w.Code != http.StatusNoContent {
		t.Fatalf("DELETE = %d %s, want 204", w.Code, w.Body.String())
	}

	writes := auditActions(h, "user_type.write")
	if len(writes) != 3 {
		t.Fatalf("user_type.write rows = %d, want 3 (create, update, standard rename)", len(writes))
	}
	var data map[string]any
	if err := json.Unmarshal(writes[0].Data, &data); err != nil {
		t.Fatal(err)
	}
	if writes[0].Target != "portfolio-manager" || data["id"] != "portfolio-manager" ||
		data["name"] != "Portfolio manager" || data["priority"] != float64(10) || data["built_in"] != false {
		t.Errorf("user_type.write = target %q data %v", writes[0].Target, data)
	}
	if dels := auditActions(h, "user_type.delete"); len(dels) != 1 || dels[0].Target != "portfolio-manager" {
		t.Errorf("user_type.delete rows = %+v, want one for portfolio-manager", dels)
	}
}

// TestDeleteUserTypeRefusedWhileNamed is the delete guard: a type is never
// removed while a sign-in could still resolve to it or a row still names it,
// and the built-in type never at all. Every refusal writes no delete audit row
// and leaves the row in place.
func TestDeleteUserTypeRefusedWhileNamed(t *testing.T) {
	contractor := types.UserType{ID: "contractor", Name: "Contractor"}
	for _, tc := range []struct {
		name           string
		roleMap        map[string]string
		defaultRole    string
		refs, tokens   int
		deleteConflict bool
		id             string
		want           string
	}{
		{
			name: "the built-in type", id: "standard",
			want: "Standard user can't be removed at all.",
		},
		{
			name: "a chart role-map value", id: "contractor", roleMap: map[string]string{"ext-group": "contractor"},
			want: "It can't be removed yet: your chart still maps to it. Remap it in your chart.",
		},
		{
			name: "the default role", id: "contractor", roleMap: map[string]string{"g": "admin"}, defaultRole: "contractor",
			want: "It can't be removed yet: your default role still maps to it. Remap it in your chart.",
		},
		{
			name: "a subject row", id: "contractor", refs: 1,
			want: "It can't be removed yet: 1 permission, profile or drive row names it. Remove those rows.",
		},
		{
			name: "all of them", id: "contractor", roleMap: map[string]string{"ext-group": "contractor"},
			defaultRole: "contractor", refs: 3,
			want: "It can't be removed yet: your chart and your default role still map to it, and " +
				"3 permission, profile or drive rows name it. Remap it in your chart, then remove those rows.",
		},
		{
			name: "one live API token", id: "contractor", tokens: 1,
			want: "It can't be removed yet: 1 API token carries it. Revoke the token or wait for its holder to sign in again.",
		},
		{
			name: "the chart and live API tokens", id: "contractor", roleMap: map[string]string{"ext-group": "contractor"}, tokens: 2,
			want: "It can't be removed yet: your chart still maps to it, and 2 API tokens carry it. " +
				"Remap it in your chart, then revoke the tokens or wait for their holders to sign in again.",
		},
		{
			name: "a row written after the check", id: "contractor", deleteConflict: true,
			want: "It can't be removed yet: something started naming it just now. Reload and try again.",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newUserTypeStore()
			st.rows[contractor.ID] = contractor
			st.refs, st.tokens, st.deleteConflict = tc.refs, tc.tokens, tc.deleteConflict
			var auth *oidc.Authenticator
			if tc.roleMap != nil {
				auth = newAccessAuth(t, tc.roleMap, tc.defaultRole, nil, nil)
			}
			srv, h := userTypeServer(t, st, auth)
			w := do(t, srv, http.MethodDelete, "/api/v1/user-types/"+tc.id, adminToken, "")
			if w.Code != http.StatusConflict {
				t.Fatalf("DELETE = %d %s, want 409", w.Code, w.Body.String())
			}
			if got := userTypeError(t, w.Body.Bytes()); got != tc.want {
				t.Errorf("refusal = %q\nwant      %q", got, tc.want)
			}
			if _, ok := st.rows[tc.id]; !ok {
				t.Error("the row was removed despite the refusal")
			}
			if n := len(auditActions(h, "user_type.delete")); n != 0 {
				t.Errorf("user_type.delete rows = %d on a refusal, want 0", n)
			}
		})
	}

	// An unreadable token count refuses the delete: a type a token may still
	// carry is never removed on a guess.
	st := newUserTypeStore()
	st.rows[contractor.ID] = contractor
	st.tokensErr = errors.New("pg: connection refused")
	srv, h := userTypeServer(t, st, nil)
	if w := do(t, srv, http.MethodDelete, "/api/v1/user-types/contractor", adminToken, ""); w.Code != http.StatusInternalServerError {
		t.Fatalf("DELETE with the token count unreadable = %d %s, want 500", w.Code, w.Body.String())
	}
	if _, ok := st.rows[contractor.ID]; !ok || len(auditActions(h, "user_type.delete")) != 0 {
		t.Fatal("the type was removed although its token stamps could not be counted")
	}

	// THE CONTROL: the same deployment with nothing naming the type removes it.
	st = newUserTypeStore()
	st.rows[contractor.ID] = contractor
	srv, _ = userTypeServer(t, st, newAccessAuth(t, map[string]string{"ext-group": "admin"}, "", nil, nil))
	if w := do(t, srv, http.MethodDelete, "/api/v1/user-types/contractor", adminToken, ""); w.Code != http.StatusNoContent {
		t.Fatalf("DELETE unnamed = %d %s, want 204", w.Code, w.Body.String())
	}
	if w := do(t, srv, http.MethodDelete, "/api/v1/user-types/contractor", adminToken, ""); w.Code != http.StatusNotFound {
		t.Fatalf("DELETE again = %d, want 404", w.Code)
	}
}
