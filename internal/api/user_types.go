// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const (
	maxUserTypeNameLen        = 128
	maxUserTypeDescriptionLen = 1000
	maxUserTypePriority       = 1000
	maxUserTypeIDLen          = 63
)

// userTypeIDRe is the slug shape migration 0071_user_types CHECKs.
var userTypeIDRe = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// reservedUserTypeIDs are the words a role-map value already means: a type
// with one of these ids would be a type a map value could never name, or a
// tier a type could be mistaken for. "member" is the pre-0.8 name of the user
// tier, still accepted as a role-map value until 0.9.
var reservedUserTypeIDs = map[string]bool{
	oidc.RoleAdmin:         true,
	oidc.RoleSecurityAdmin: true,
	oidc.RoleUser:          true,
	oidc.LegacyRoleMember:  true,
	accessDeniedRole:       true,
}

// mountUserTypeRoutes registers /user-types. Called with securityOps: defining
// a type (and, later, the rows written against it) is the security tier's
// duty, exactly as governance profiles are. Deciding who IS a type stays on
// the operatorOnly /access routes.
func (s *Server) mountUserTypeRoutes(securityOps chi.Router) {
	securityOps.Get("/user-types", s.handleListUserTypes)
	securityOps.Post("/user-types", s.handleCreateUserType)
	securityOps.Put("/user-types/{id}", s.handleUpdateUserType)
	securityOps.Delete("/user-types/{id}", s.handleDeleteUserType)
}

type userTypesResponse struct {
	UserTypes []types.UserType `json:"user_types"`
}

func (s *Server) handleListUserTypes(w http.ResponseWriter, r *http.Request) {
	list, err := s.cfg.Store.ListUserTypes(r.Context())
	if err != nil {
		writeServerError(w, r, "list user types", err)
		return
	}
	if list == nil {
		list = []types.UserType{}
	}
	writeJSON(w, http.StatusOK, userTypesResponse{UserTypes: list})
}

// userTypeRequest is the POST and PUT body. The id is optional on POST (it is
// derived from the name) and, when present on PUT, must equal the path's: an
// id is what role-map values and grant subjects point at, so it never changes.
type userTypeRequest struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Priority    int    `json:"priority"`
}

// userTypeFromRequest normalises and validates a write body, returning the
// row to write or the 400 sentence.
func userTypeFromRequest(req userTypeRequest) (types.UserType, string) {
	t := types.UserType{
		ID:          strings.TrimSpace(req.ID),
		Name:        strings.TrimSpace(req.Name),
		Description: strings.TrimSpace(req.Description),
		Priority:    req.Priority,
	}
	if t.Name == "" {
		return t, "Give the type a name."
	}
	if len(t.Name) > maxUserTypeNameLen || !controlCharFree(t.Name) {
		return t, fmt.Sprintf("The name must be at most %d characters, with no control characters.", maxUserTypeNameLen)
	}
	if len(t.Description) > maxUserTypeDescriptionLen || !controlCharFree(t.Description) {
		return t, fmt.Sprintf("The description must be at most %d characters, with no control characters.", maxUserTypeDescriptionLen)
	}
	if t.Priority < 0 || t.Priority > maxUserTypePriority {
		return t, fmt.Sprintf("Priority must be a whole number from 0 to %d.", maxUserTypePriority)
	}
	if t.ID == "" {
		t.ID = userTypeSlug(t.Name)
		if t.ID == "" {
			return t, "Give the type an id: its name has no letters or digits to make one from."
		}
	}
	if reservedUserTypeIDs[t.ID] {
		return t, fmt.Sprintf("The id %q is a role, so a user type can't use it. Pick another.", t.ID)
	}
	if len(t.ID) > maxUserTypeIDLen || !userTypeIDRe.MatchString(t.ID) {
		return t, fmt.Sprintf("The id must be lowercase letters and digits joined by single hyphens, at most %d characters.", maxUserTypeIDLen)
	}
	return t, ""
}

// userTypeSlug derives an id from a name: ASCII letters and digits kept
// (lowercased), every other run of characters one hyphen.
func userTypeSlug(name string) string {
	var b strings.Builder
	for _, r := range name {
		if 'A' <= r && r <= 'Z' {
			r += 'a' - 'A'
		}
		if ('a' <= r && r <= 'z') || ('0' <= r && r <= '9') {
			b.WriteRune(r)
		} else if b.Len() > 0 && !strings.HasSuffix(b.String(), "-") {
			b.WriteByte('-')
		}
	}
	slug := b.String()
	if len(slug) > maxUserTypeIDLen {
		slug = slug[:maxUserTypeIDLen]
	}
	return strings.TrimRight(slug, "-")
}

func (s *Server) handleCreateUserType(w http.ResponseWriter, r *http.Request) {
	var req userTypeRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	t, msg := userTypeFromRequest(req)
	if msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	t.CreatedBy = principalFromRequest(r)
	saved, err := s.cfg.Store.CreateUserType(r.Context(), t)
	if errors.Is(err, store.ErrConflict) {
		writeError(w, http.StatusConflict,
			fmt.Sprintf("A user type with the id %q or the name %q already exists.", t.ID, t.Name))
		return
	}
	if err != nil {
		writeServerError(w, r, "create user type", err)
		return
	}
	s.auditUserTypeWrite(r, saved)
	writeJSON(w, http.StatusCreated, saved)
}

// handleUpdateUserType edits a type's name, description and priority. The
// built-in type is editable too, but never takes part in a sign-in tie, so it
// has no priority to set.
func (s *Server) handleUpdateUserType(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req userTypeRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	if req.ID != "" && strings.TrimSpace(req.ID) != id {
		writeError(w, http.StatusBadRequest, "A user type's id can't be changed.")
		return
	}
	req.ID = id
	t, msg := userTypeFromRequest(req)
	if msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	current, err := s.cfg.Store.GetUserType(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "User type not found.")
		return
	}
	if err != nil {
		writeServerError(w, r, "get user type", err)
		return
	}
	if current.BuiltIn && t.Priority != 0 {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("%s never wins a tie against another type, so it has no priority.", current.Name))
		return
	}
	saved, err := s.cfg.Store.UpdateUserType(r.Context(), t)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "User type not found.")
		return
	}
	if errors.Is(err, store.ErrConflict) {
		writeError(w, http.StatusConflict, fmt.Sprintf("Another user type is already named %q.", t.Name))
		return
	}
	if err != nil {
		writeServerError(w, r, "update user type", err)
		return
	}
	s.auditUserTypeWrite(r, saved)
	writeJSON(w, http.StatusOK, saved)
}

func (s *Server) auditUserTypeWrite(r *http.Request, t types.UserType) {
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		"user_type.write", t.ID, "success", mustJSON(map[string]any{
			"id":       t.ID,
			"name":     t.Name,
			"priority": t.Priority,
			"built_in": t.BuiltIn,
		})))
}

// handleDeleteUserType removes a custom type nothing names. Deleting a type
// someone is still mapped to would fail their next sign-in, and deleting one a
// grant or assignment still names would drop a wall or an allowance without
// an audit line saying so; both are refused with what still names it.
func (s *Server) handleDeleteUserType(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	t, err := s.cfg.Store.GetUserType(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "User type not found.")
		return
	}
	if err != nil {
		writeServerError(w, r, "get user type", err)
		return
	}
	if t.BuiltIn {
		writeError(w, http.StatusConflict, fmt.Sprintf("%s can't be removed at all.", t.Name))
		return
	}
	var use userTypeInUse
	if s.cfg.OIDC != nil {
		for _, v := range s.cfg.OIDC.ChartRoleMap() {
			use.chart = use.chart || v == id
		}
		use.defaultRole = s.cfg.OIDC.DefaultRole() == id
	}
	if use.rows, err = s.cfg.Store.UserTypeReferences(r.Context(), id); err != nil {
		writeServerError(w, r, "count user type references", err)
		return
	}
	if msg := use.refusal(); msg != "" {
		writeError(w, http.StatusConflict, msg)
		return
	}
	err = s.cfg.Store.DeleteUserType(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "User type not found.")
		return
	}
	if errors.Is(err, store.ErrConflict) {
		writeError(w, http.StatusConflict, "It can't be removed yet: something started naming it just now. Reload and try again.")
		return
	}
	if err != nil {
		writeServerError(w, r, "delete user type", err)
		return
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		"user_type.delete", id, "success", mustJSON(map[string]any{"id": id})))
	w.WriteHeader(http.StatusNoContent)
}

// userTypeInUse is everything that still names a user type a DELETE would
// remove: a WARDYN_OIDC_ROLE_MAP value, WARDYN_OIDC_DEFAULT_ROLE, and the
// capability-grant, governance-assignment and drive-grant rows written
// against it.
type userTypeInUse struct {
	chart, defaultRole bool
	rows               int
}

// refusal is the 409 sentence naming what holds the type, or "" when nothing
// does.
func (u userTypeInUse) refusal() string {
	var mappers, clauses, fixes []string
	if u.chart {
		mappers = append(mappers, "your chart")
	}
	if u.defaultRole {
		mappers = append(mappers, "your default role")
	}
	if len(mappers) > 0 {
		verb := "maps"
		if len(mappers) > 1 {
			verb = "map"
		}
		clauses = append(clauses, strings.Join(mappers, " and ")+" still "+verb+" to it")
		fixes = append(fixes, "remap it in your chart")
	}
	if u.rows > 0 {
		noun := "rows name"
		if u.rows == 1 {
			noun = "row names"
		}
		clauses = append(clauses, fmt.Sprintf("%d permission, profile or drive %s it", u.rows, noun))
		fixes = append(fixes, "remove those rows")
	}
	if len(clauses) == 0 {
		return ""
	}
	fix := strings.Join(fixes, ", then ")
	return "It can't be removed yet: " + strings.Join(clauses, ", and ") + ". " +
		strings.ToUpper(fix[:1]) + fix[1:] + "."
}
