// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// The component routes: a person's own saved components (/me/components) and the
// organisation's (/components). A component is a descriptor over reach and
// credentials the proxy already enforces, so these routes only store, validate
// and project definitions; attaching one to a run is the gate's decision, not
// theirs.
package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

type componentRequest = client.ComponentRequest

const (
	// componentsMaxPerPerson and componentsMaxOrg bound saved rows, the shape of
	// secretsMaxPerOwner: a runaway-add guard answered with a 422, no audit row.
	componentsMaxPerPerson = 32
	componentsMaxOrg       = 256
)

// mountComponentRoutes registers the component family. The org routes are
// operatorOnly: an org component is admin-authored content that reaches hosts and
// carries the operator's credential, the same tier as the other content writes.
// Who may ATTACH one is a different question, answered per row by the permission
// kind `component` (components_authz.go), not by this tier.
func (s *Server) mountComponentRoutes(r, operatorOnly chi.Router) {
	r.Get("/me/components", s.handleMyComponents)
	r.Post("/me/components", s.handleSaveMyComponent)
	r.Put("/me/components/{id}", s.handleUpdateMyComponent)
	r.Delete("/me/components/{id}", s.handleDeleteMyComponent)
	operatorOnly.Get("/components", s.handleListComponents)
	operatorOnly.Put("/components/{id}", s.handlePutComponent)
	operatorOnly.Delete("/components/{id}", s.handleDeleteComponent)
}

func (s *Server) componentStoreOr501(w http.ResponseWriter) (store.ComponentStore, bool) {
	cs, ok := s.cfg.Store.(store.ComponentStore)
	if !ok {
		writeErrorReason(w, http.StatusNotImplemented, reasonComponentStoreUnavailable, "components require the Postgres store backend")
	}
	return cs, ok
}

// componentOwner is the owner a person's saved component carries: the subject
// their runs are minted with, which is what the gate looks a row up by. Never "":
// owner "" is the organisation's rows, and a person's request must not be able to
// land on one.
func (s *Server) componentOwner(w http.ResponseWriter, r *http.Request) (string, bool) {
	owner := runIdentitySubject(r.Context(), principalFromRequest(r))
	if owner == "" {
		writeServerError(w, r, "resolve component owner", errors.New("the request carries no principal"))
		return "", false
	}
	return owner, true
}

// ownsComponent loads a person's own row, writing the 404 a missing one gets.
// The store scopes every read to the owner, so another person's row and an
// absent id are the same ErrNotFound and the same bytes; the owner check here is
// the predicate the matrix's owner class asks for, not a second filter.
func (s *Server) ownsComponent(w http.ResponseWriter, r *http.Request, cs store.ComponentStore, id uuid.UUID, owner string) (types.Component, bool) {
	row, err := cs.GetComponent(r.Context(), id, owner)
	if err == nil && (owner == "" || row.Owner != owner) {
		err = store.ErrNotFound
	}
	if notFoundIf(w, err, "component", reasonComponentNotFound) {
		return types.Component{}, false
	}
	if err != nil {
		writeServerError(w, r, "read component", err)
		return types.Component{}, false
	}
	return row, true
}

// saveRefusal writes the 422 for a definition the save routes refuse.
func saveRefusal(w http.ResponseWriter, reason, msg string) {
	writeErrorReason(w, http.StatusUnprocessableEntity, reason, msg)
}

// componentSaveRefusal is the checks every save shares, run on the row as its
// owner will hold it: the types layer's shape and ownership rules (an org row may
// share an operator secret and name a literal address; a person's may not), then
// the two the types layer leaves to the api (a reserved secret name, and a
// shared secret the operator has not stored). "" reason means saveable.
func (s *Server) componentSaveRefusal(ctx context.Context, c types.Component) (reason, msg string) {
	if err := c.Validate(proxy.ValidDomainEntry); err != nil {
		return reasonComponentDefinitionInvalid, "invalid component: " + err.Error()
	}
	var operatorHas map[string]bool
	for i, sec := range c.Definition.Secrets {
		reserved := nameSinkReservedSecret(sec.SecretName)
		if sec.Delivery.Mode == types.ComponentDeliveryHeader {
			reserved = sinkReservedSecret(sec.SecretName)
		}
		if reserved {
			return reasonComponentDefinitionInvalid, fmt.Sprintf("invalid component: definition.secrets[%d].secret_name: %q is managed by Wardyn", i, sec.SecretName)
		}
		if !sec.Shared {
			continue
		}
		if operatorHas == nil {
			operatorHas = s.presentSecretNames(ctx)
		}
		if !operatorHas[sec.SecretName] {
			return reasonComponentSecretMissing, fmt.Sprintf("definition.secrets[%d]: the organisation has no secret named %q; add it under Secrets first", i, sec.SecretName)
		}
	}
	return "", ""
}

// componentNameTaken reports whether another row of rows already has name. The
// database's UNIQUE (owner, name) is byte-exact, so "Stripe" and "stripe" would
// both save; two components a person cannot tell apart in a list are one name.
func componentNameTaken(rows []types.Component, name string, self uuid.UUID) bool {
	for _, c := range rows {
		if c.ID != self && strings.EqualFold(c.Name, name) {
			return true
		}
	}
	return false
}

// componentWriteDatum is the component.write / component.delete audit datum. A
// person's row records id, kind, version and counts and nothing it was told: a
// component is that person's personal data, and the log is append-only. An org
// row is admin-authored and keeps its name.
func componentWriteDatum(c types.Component, op string) map[string]any {
	d := map[string]any{
		"op": op, "id": c.ID, "kind": string(types.ComponentCustom), "version": c.Version,
		"hosts": len(c.Definition.Hosts), "secrets": len(c.Definition.Secrets), "config_keys": len(c.Definition.Config),
	}
	if c.Owner == "" {
		d["source"], d["name"] = "org", c.Name
	} else {
		d["source"] = "self"
	}
	return d
}

func (s *Server) auditComponent(r *http.Request, action string, c types.Component, op string) {
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		action, c.ID.String(), "success", mustJSON(componentWriteDatum(c, op))))
}

func componentSaved(c types.Component) client.ComponentSaved {
	return client.ComponentSaved{Component: c, Requirements: []client.ComponentRequirement{}}
}

// handleSaveMyComponent is POST /me/components: a new component of the caller's
// own. The same feature value that gates defining one inline on a run gates
// saving one.
func (s *Server) handleSaveMyComponent(w http.ResponseWriter, r *http.Request) {
	cs, ok := s.componentStoreOr501(w)
	if !ok {
		return
	}
	if s.componentAttachRefusal(r, "").write(s, w, r) {
		return
	}
	owner, ok := s.componentOwner(w, r)
	if !ok {
		return
	}
	var req componentRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	c := types.Component{ID: uuid.New(), Owner: owner, Name: req.Name, Definition: req.Definition, CreatedBy: owner}
	if reason, msg := s.componentSaveRefusal(r.Context(), c); reason != "" {
		saveRefusal(w, reason, msg)
		return
	}
	rows, err := cs.ListComponents(r.Context(), owner)
	if err != nil {
		writeServerError(w, r, "list components", err)
		return
	}
	if len(rows) >= componentsMaxPerPerson {
		saveRefusal(w, reasonComponentCapReached, fmt.Sprintf("too many saved components (max %d); remove one first", componentsMaxPerPerson))
		return
	}
	if componentNameTaken(rows, c.Name, c.ID) {
		writeErrorReason(w, http.StatusConflict, reasonComponentNameConflict, "you already have a component with that name")
		return
	}
	created, err := cs.CreateComponent(r.Context(), c)
	if errors.Is(err, store.ErrConflict) {
		writeErrorReason(w, http.StatusConflict, reasonComponentNameConflict, "you already have a component with that name")
		return
	}
	if err != nil {
		writeServerError(w, r, "create component", err)
		return
	}
	s.auditComponent(r, "component.write", created, "create")
	writeJSON(w, http.StatusCreated, componentSaved(created))
}

// handleUpdateMyComponent is PUT /me/components/{id}: replaces the name and
// definition of one of the caller's own rows and moves its version. The id is
// looked up before the body is read, so a row that is not theirs answers 404
// whatever they send.
func (s *Server) handleUpdateMyComponent(w http.ResponseWriter, r *http.Request) {
	cs, ok := s.componentStoreOr501(w)
	if !ok {
		return
	}
	if s.componentAttachRefusal(r, "").write(s, w, r) {
		return
	}
	id, ok := parseIDParam(w, r, "id", "component")
	if !ok {
		return
	}
	owner, ok := s.componentOwner(w, r)
	if !ok {
		return
	}
	row, ok := s.ownsComponent(w, r, cs, id, owner)
	if !ok {
		return
	}
	var req componentRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	c := types.Component{ID: row.ID, Owner: owner, Name: req.Name, Definition: req.Definition}
	if reason, msg := s.componentSaveRefusal(r.Context(), c); reason != "" {
		saveRefusal(w, reason, msg)
		return
	}
	s.updateComponent(w, r, cs, c)
}

// updateComponent is the write the two PUT routes share once a row is known to
// exist and its new shape passed.
func (s *Server) updateComponent(w http.ResponseWriter, r *http.Request, cs store.ComponentStore, c types.Component) {
	rows, err := cs.ListComponents(r.Context(), c.Owner)
	if err != nil {
		writeServerError(w, r, "list components", err)
		return
	}
	if componentNameTaken(rows, c.Name, c.ID) {
		writeErrorReason(w, http.StatusConflict, reasonComponentNameConflict, "a component with that name already exists")
		return
	}
	updated, err := cs.UpdateComponent(r.Context(), c)
	switch {
	case errors.Is(err, store.ErrConflict):
		writeErrorReason(w, http.StatusConflict, reasonComponentNameConflict, "a component with that name already exists")
		return
	case notFoundIf(w, err, "component", reasonComponentNotFound):
		return
	case err != nil:
		writeServerError(w, r, "update component", err)
		return
	}
	s.auditComponent(r, "component.write", updated, "update")
	writeJSON(w, http.StatusOK, componentSaved(updated))
}

// handleDeleteMyComponent is DELETE /me/components/{id}. Deleting is never
// refused for want of the feature value: removing a component only narrows what
// a person can attach, so someone who has lost the right to define them can
// still clear the ones they have. A run launched with it keeps its snapshot.
func (s *Server) handleDeleteMyComponent(w http.ResponseWriter, r *http.Request) {
	cs, ok := s.componentStoreOr501(w)
	if !ok {
		return
	}
	id, ok := parseIDParam(w, r, "id", "component")
	if !ok {
		return
	}
	owner, ok := s.componentOwner(w, r)
	if !ok {
		return
	}
	if _, ok := s.ownsComponent(w, r, cs, id, owner); !ok {
		return
	}
	deleted, err := cs.DeleteComponent(r.Context(), id, owner)
	if notFoundIf(w, err, "component", reasonComponentNotFound) {
		return
	}
	if err != nil {
		writeServerError(w, r, "delete component", err)
		return
	}
	s.auditComponent(r, "component.delete", deleted, "delete")
	w.WriteHeader(http.StatusNoContent)
}

// handleListComponents is GET /components: the organisation's rows, whole.
func (s *Server) handleListComponents(w http.ResponseWriter, r *http.Request) {
	cs, ok := s.componentStoreOr501(w)
	if !ok {
		return
	}
	rows, err := cs.ListComponents(r.Context(), "")
	if err != nil {
		writeServerError(w, r, "list components", err)
		return
	}
	if rows == nil {
		rows = []types.Component{}
	}
	writeJSON(w, http.StatusOK, rows)
}

// handlePutComponent is PUT /components/{id}: replaces the org component with
// this id, or creates it. A new one is created RESTRICTED in the same
// transaction as its row, so it is attachable by nobody until an allow row names
// who may; there is no moment a member could attach a component nobody has
// granted.
func (s *Server) handlePutComponent(w http.ResponseWriter, r *http.Request) {
	cs, ok := s.componentStoreOr501(w)
	if !ok {
		return
	}
	id, ok := parseIDParam(w, r, "id", "component")
	if !ok {
		return
	}
	if id == uuid.Nil {
		writeErrorReason(w, http.StatusBadRequest, reasonInvalidIDParam, "invalid component id")
		return
	}
	var req componentRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	c := types.Component{ID: id, Name: req.Name, Definition: req.Definition, CreatedBy: principalFromRequest(r)}
	if reason, msg := s.componentSaveRefusal(r.Context(), c); reason != "" {
		saveRefusal(w, reason, msg)
		return
	}
	_, err := cs.GetComponent(r.Context(), id, "")
	switch {
	case err == nil:
		s.updateComponent(w, r, cs, c)
	case errors.Is(err, store.ErrNotFound):
		s.createOrgComponent(w, r, cs, c)
	default:
		writeServerError(w, r, "read component", err)
	}
}

func (s *Server) createOrgComponent(w http.ResponseWriter, r *http.Request, cs store.ComponentStore, c types.Component) {
	creator, ok := cs.(store.RestrictedComponentCreator)
	if !ok {
		writeErrorReason(w, http.StatusNotImplemented, reasonComponentStoreUnavailable, "components require the Postgres store backend")
		return
	}
	rows, err := cs.ListComponents(r.Context(), "")
	if err != nil {
		writeServerError(w, r, "list components", err)
		return
	}
	if len(rows) >= componentsMaxOrg {
		saveRefusal(w, reasonComponentCapReached, fmt.Sprintf("too many organisation components (max %d); remove one first", componentsMaxOrg))
		return
	}
	if componentNameTaken(rows, c.Name, c.ID) {
		writeErrorReason(w, http.StatusConflict, reasonComponentNameConflict, "a component with that name already exists")
		return
	}
	created, err := creator.CreateRestrictedComponent(r.Context(), c, capComponent, principalFromRequest(r))
	if errors.Is(err, store.ErrConflict) {
		writeErrorReason(w, http.StatusConflict, reasonComponentNameConflict, "a component with that id or name already exists")
		return
	}
	if err != nil {
		writeServerError(w, r, "create component", err)
		return
	}
	// A tightening, so it is written directly rather than held: the same row
	// PUT /permissions/availability would write, audited the same way.
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		"capability.availability.write", capComponent, "success", mustJSON(availabilityAuditData(capComponent, created.ID.String(), true))))
	s.auditComponent(r, "component.write", created, "create")
	writeJSON(w, http.StatusCreated, componentSaved(created))
}

// handleDeleteComponent is DELETE /components/{id}. The row goes; its
// restriction stays, so the id is never open to anyone — a deleted component's
// door must not read as unrestricted. The allow and deny rows naming it are
// swept: they grant a thing that no longer exists.
func (s *Server) handleDeleteComponent(w http.ResponseWriter, r *http.Request) {
	cs, ok := s.componentStoreOr501(w)
	if !ok {
		return
	}
	id, ok := parseIDParam(w, r, "id", "component")
	if !ok {
		return
	}
	deleted, err := cs.DeleteComponent(r.Context(), id, "")
	if notFoundIf(w, err, "component", reasonComponentNotFound) {
		return
	}
	if err != nil {
		writeServerError(w, r, "delete component", err)
		return
	}
	swept, err := s.sweepComponentGrants(r.Context(), id)
	if err != nil {
		writeServerError(w, r, "sweep component grants", err)
		return
	}
	datum := componentWriteDatum(deleted, "delete")
	datum["grants_removed"], datum["restriction_kept"] = swept, true
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		"component.delete", id.String(), "success", mustJSON(datum)))
	w.WriteHeader(http.StatusNoContent)
}

// sweepComponentGrants removes the capability grants whose value is this
// component's id, and returns how many it removed.
func (s *Server) sweepComponentGrants(ctx context.Context, id uuid.UUID) (int, error) {
	grants, err := s.cfg.Store.ListCapabilityGrants(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, g := range grants {
		if g.Capability != capComponent || strings.TrimSpace(g.Value) != id.String() {
			continue
		}
		if err := s.cfg.Store.DeleteCapabilityGrant(ctx, g.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
			return n, err
		}
		n++
	}
	return n, nil
}
