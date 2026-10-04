// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// Key domains (WARDYN_KEY_DOMAINS_FILE, migration 0121): a domain is a tenant of
// the deployment's key service, declared in deploy configuration and proven at
// boot. The API never declares one. It only says which declared domain a
// subject's NEXT principal-key generation is wrapped under, with the governance
// subject vocabulary: a user, a group or everyone.
//
// A reassignment never moves what is already written. The generation a person's
// credentials were sealed under stays in its own domain, because re-wrapping it
// into the new one would hand the new domain's key holder their whole history;
// `wardynd -rewrap-principal-keys` re-seals the rows into the new generation.

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/keydomain"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// mountKeyDomainRoutes registers /key-domains. Called with securityOps: moving
// where a person's keys are written is the security tier's duty, beside erasing
// a person's credentials. Four-eyes coverage of the two writes is GOV4's.
func (s *Server) mountKeyDomainRoutes(securityOps chi.Router) {
	securityOps.Get("/key-domains", s.handleListKeyDomains)
	securityOps.Put("/key-domains/assignments/{subject_type}/{subject}", s.handlePutKeyDomainAssignment)
	securityOps.Delete("/key-domains/assignments/{subject_type}/{subject}", s.handleDeleteKeyDomainAssignment)
}

// keyDomainRow is one domain of GET /key-domains: how many live keys it holds,
// where its key is, and whether boot proved it. Every declared domain was proven
// (a domain that could not be fails boot); a domain only an assignment or a live
// key names is not declared, so it is not proven.
type keyDomainRow struct {
	Domain   string `json:"domain"`
	Declared bool   `json:"declared"`
	LiveKeys int    `json:"live_keys"`
	Key      string `json:"key,omitempty"`
	Proven   bool   `json:"proven"`
}

type keyDomainsResponse struct {
	Domains     []keyDomainRow         `json:"domains"`
	Assignments []keydomain.Assignment `json:"assignments"`
	// PrincipalKeys is WARDYN_PRINCIPAL_KEYS on: with it off, domains place
	// audit-record keys only.
	PrincipalKeys bool `json:"principal_keys"`
}

func (s *Server) keyDomainsOr501(w http.ResponseWriter) (*keydomain.Service, bool) {
	if s.cfg.KeyDomains == nil {
		writeErrorReason(w, http.StatusNotImplemented, reasonKeyDomainsStoreUnavailable, "key domains require the Postgres store backend")
		return nil, false
	}
	return s.cfg.KeyDomains, true
}

// handleListKeyDomains answers the declared domains with how many live keys each
// holds, and every assignment.
func (s *Server) handleListKeyDomains(w http.ResponseWriter, r *http.Request) {
	svc, ok := s.keyDomainsOr501(w)
	if !ok {
		return
	}
	usage, err := svc.Usage(r.Context())
	if err != nil {
		writeServerError(w, r, "count key domain usage", err)
		return
	}
	assignments, err := svc.List(r.Context())
	if err != nil {
		writeServerError(w, r, "list key domain assignments", err)
		return
	}
	rows := make([]keyDomainRow, len(usage))
	for i, u := range usage {
		key := s.cfg.KeyDomainKeys[u.Domain]
		if u.Domain == keydomain.Default {
			key = s.defaultKeyDomainKey()
		}
		rows[i] = keyDomainRow{Domain: u.Domain, Declared: u.Declared, LiveKeys: u.LiveKeys, Key: key, Proven: u.Declared}
	}
	writeJSON(w, http.StatusOK, keyDomainsResponse{Domains: rows, Assignments: assignments, PrincipalKeys: s.cfg.PrincipalKeys})
}

// defaultKeyDomainKey says where the default domain's key is: the credential key
// service, or the deployment's local key.
func (s *Server) defaultKeyDomainKey() string {
	if s.cfg.SecretKeyService != "" {
		return s.cfg.SecretKeyService
	}
	return "Credential key"
}

// keyDomainChange is one validated assignment write. Decoding and validating
// it (decodeKeyDomainChange) is separate from doing it (applyKeyDomainChange),
// so a four-eyes queue can hold a validated change and apply it once a second
// human has agreed.
type keyDomainChange struct {
	Delete      bool
	SubjectType string
	Subject     string
	Domain      string
}

// keyDomainActor is who an applied change is attributed to.
type keyDomainActor struct {
	typ       types.ActorType
	principal string
}

// keyDomainRequest is the PUT body.
type keyDomainRequest struct {
	Domain string `json:"domain"`
}

const (
	keyDomainOwnerUnresolvedMsg = "That email address doesn't match anyone this deployment knows, so nothing was changed. " +
		"Use the person's subject, as the Audit log shows it."
	keyDomainOwnerAmbiguousMsg = "That matches more than one person, so nothing was changed. Use the person's subject exactly."
	maxKeyDomainSubjectLen     = 512
)

// decodeKeyDomainChange validates the request into a change, or answers it and
// returns false. It writes nothing.
func (s *Server) decodeKeyDomainChange(w http.ResponseWriter, r *http.Request, del bool) (keyDomainChange, bool) {
	c := keyDomainChange{Delete: del, SubjectType: chi.URLParam(r, "subject_type")}
	bad := func(msg string) (keyDomainChange, bool) {
		writeErrorReason(w, http.StatusBadRequest, reasonKeyDomainRequestInvalid, msg)
		return c, false
	}
	raw := chi.URLParam(r, "subject")
	if r.URL.RawPath != "" {
		if u, err := url.PathUnescape(raw); err == nil {
			raw = u
		}
	}
	raw = strings.TrimSpace(raw)
	if !del {
		var req keyDomainRequest
		if !decodeStrict(w, r, &req) {
			return c, false
		}
		if c.Domain = strings.TrimSpace(req.Domain); c.Domain == "" {
			return bad("Name the key domain: send {\"domain\": \"<name>\"}, or \"default\" for the credential key.")
		}
	}
	switch c.SubjectType {
	case keydomain.SubjectAll:
		if raw != keydomain.SubjectAll {
			return bad(`For subject_type "all" the subject in the path is "all".`)
		}
	case keydomain.SubjectGroup:
		g, ok := oidc.CanonicalGroupSubject(raw)
		if !ok || len(g) > maxKeyDomainSubjectLen || !controlCharFree(g) {
			return bad("A group subject must be printable ASCII: it is matched against the group list of a person's last sign-in.")
		}
		c.Subject = g
	case keydomain.SubjectUser:
		if raw == "" || len(raw) > maxKeyDomainSubjectLen || !controlCharFree(raw) {
			return bad("Name the person, by subject or email.")
		}
		owner, _, refusal := s.resolveSecretOwner(r.Context(), raw)
		switch refusal {
		case "":
			c.Subject = owner
		case secretOwnerAmbiguousMsg:
			writeErrorReason(w, http.StatusUnprocessableEntity, reasonOwnerAmbiguous, keyDomainOwnerAmbiguousMsg)
			return c, false
		default:
			writeErrorReason(w, http.StatusUnprocessableEntity, reasonOwnerUnresolved, keyDomainOwnerUnresolvedMsg)
			return c, false
		}
	default:
		return bad(fmt.Sprintf("subject_type must be %q, %q or %q.", keydomain.SubjectUser, keydomain.SubjectGroup, keydomain.SubjectAll))
	}
	return c, true
}

// keyDomainApplied is what an applied change did.
type keyDomainApplied struct {
	Assignment keydomain.Assignment
	// Previous is the assignment a set replaced or a delete removed.
	Previous *keydomain.Assignment
	Created  bool
	Found    bool
}

// applyKeyDomainChange does c and writes its audit row. A set to a domain the
// file does not declare returns keydomain.ErrUnknownDomain with nothing written;
// a delete of a missing assignment is Found false, with nothing written or
// audited.
func (s *Server) applyKeyDomainChange(ctx context.Context, by keyDomainActor, c keyDomainChange) (keyDomainApplied, error) {
	svc := s.cfg.KeyDomains
	var out keyDomainApplied
	if c.Delete {
		prev, found, err := svc.Delete(ctx, c.SubjectType, c.Subject)
		if err != nil || !found {
			return out, err
		}
		out.Previous, out.Found = &prev, true
		s.recordAudit(ctx, s.auditEvent(nil, by.typ, by.principal, "key_domain.assignment.delete", keyDomainTarget(c), "success",
			mustJSON(map[string]any{"subject_type": c.SubjectType, "subject": c.Subject, "domain": prev.Domain})))
		return out, nil
	}
	if prev, found, err := svc.Get(ctx, c.SubjectType, c.Subject); err != nil {
		return out, err
	} else if found {
		out.Previous = &prev
	}
	created, err := svc.Set(ctx, keydomain.Assignment{SubjectType: c.SubjectType, Subject: c.Subject, Domain: c.Domain, SetBy: by.principal})
	if err != nil {
		return out, err
	}
	out.Created, out.Found = created, true
	got, _, err := svc.Get(ctx, c.SubjectType, c.Subject)
	if err != nil {
		return out, err
	}
	out.Assignment = got
	data := map[string]any{"subject_type": c.SubjectType, "subject": c.Subject, "domain": c.Domain, "created": created}
	if out.Previous != nil {
		data["previous_domain"] = out.Previous.Domain
	}
	s.recordAudit(ctx, s.auditEvent(nil, by.typ, by.principal, "key_domain.assignment.set", keyDomainTarget(c), "success", mustJSON(data)))
	return out, nil
}

func keyDomainTarget(c keyDomainChange) string {
	if c.SubjectType == keydomain.SubjectAll {
		return keydomain.SubjectAll
	}
	return c.SubjectType + ":" + c.Subject
}

func (s *Server) handlePutKeyDomainAssignment(w http.ResponseWriter, r *http.Request) {
	svc, ok := s.keyDomainsOr501(w)
	if !ok {
		return
	}
	c, ok := s.decodeKeyDomainChange(w, r, false)
	if !ok {
		return
	}
	if !svc.Has(c.Domain) {
		s.refuse(w, r, authz.Deny(authz.ReasonKeyDomainUnknown, keyDomainTarget(c), fmt.Sprintf(
			"The key domain %q is not declared in the deployment's key domains file, so nothing was changed. Declared: %s.",
			c.Domain, declaredKeyDomains(svc))))
		return
	}
	// Setting a group must not leave anyone with two groups in different
	// domains and no user assignment: their next key would be refused by name.
	if c.SubjectType == keydomain.SubjectGroup {
		n, err := svc.AmbiguousIfGroup(r.Context(), c.Subject, c.Domain)
		if err != nil {
			writeServerError(w, r, "check key domain membership", err)
			return
		}
		if n > 0 {
			s.refuse(w, r, authz.Deny(authz.ReasonKeyDomainAmbiguous, keyDomainTarget(c), fmt.Sprintf(
				"%d people last signed in with this group and another group assigned to a different domain, and have no assignment of their own, so their next key would be refused. "+
					"Assign each of them to one domain as a user first, or give both groups the same domain. Nothing was changed.", n)))
			return
		}
	}
	out, err := s.applyKeyDomainChange(r.Context(), keyDomainActor{actorTypeFromRequest(r), principalFromRequest(r)}, c)
	if err != nil {
		writeServerError(w, r, "set key domain assignment", err)
		return
	}
	status := http.StatusOK
	if out.Created {
		status = http.StatusCreated
	}
	writeJSON(w, status, out.Assignment)
}

func (s *Server) handleDeleteKeyDomainAssignment(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.keyDomainsOr501(w); !ok {
		return
	}
	c, ok := s.decodeKeyDomainChange(w, r, true)
	if !ok {
		return
	}
	out, err := s.applyKeyDomainChange(r.Context(), keyDomainActor{actorTypeFromRequest(r), principalFromRequest(r)}, c)
	if err != nil {
		writeServerError(w, r, "delete key domain assignment", err)
		return
	}
	if !out.Found {
		writeErrorReason(w, http.StatusNotFound, reasonKeyDomainAssignmentNotFound, "No such key domain assignment.")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// declaredKeyDomains lists the names a set may use, for the refusal sentence.
func declaredKeyDomains(svc *keydomain.Service) string {
	return strings.Join(append([]string{keydomain.Default}, svc.Declared()...), ", ")
}
