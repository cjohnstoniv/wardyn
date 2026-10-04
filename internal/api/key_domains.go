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
	"errors"
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
// a person's credentials. With WARDYN_GOVERNANCE_SECOND_HUMAN on, a human's
// write of either is held for a second security admin
// (governance_change_keydomain_kinds.go).
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
// so a four-eyes queue can hold a validated change (it is the held payload) and
// apply it once a second human has agreed (applyKeyDomainAssignmentChange).
type keyDomainChange struct {
	Delete      bool   `json:"delete,omitempty"`
	SubjectType string `json:"subject_type"`
	Subject     string `json:"subject"`
	Domain      string `json:"domain,omitempty"`
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

// keyDomainRefused is a set that the assignments, re-read under the assignment lock, no longer allow.
type keyDomainRefused struct{ d authz.Decision }

func (e *keyDomainRefused) Error() string {
	return "key domain assignment refused: " + string(e.d.Reason)
}

// applyKeyDomainChange does c and writes its audit row. It holds the assignment lock from its checks to
// its write, as an approval does, so a set re-checks the membership against every assignment written
// before it: a set that would now leave someone in two domains is *keyDomainRefused, a set to a domain
// the file does not declare likewise, and a delete of the user assignment of a person whose sign-in lost
// groups, while a group is assigned, is refused the same way, with nothing written. A delete of a
// missing assignment is Found false, with nothing written or audited.
func (s *Server) applyKeyDomainChange(ctx context.Context, by keyDomainActor, c keyDomainChange) (keyDomainApplied, error) {
	svc := s.cfg.KeyDomains
	var out keyDomainApplied
	err := svc.WriteAssignments(ctx, func(q keydomain.Querier) error {
		if c.Delete {
			prev, found, err := svc.DeleteQ(ctx, q, c.SubjectType, c.Subject)
			if err != nil || !found {
				return err
			}
			if d, err := keyDomainDeleteRefusal(ctx, func(ctx context.Context, p string) (bool, error) { return svc.TruncatedWithGroupsQ(ctx, q, p) }, c); err != nil {
				return err
			} else if d != nil {
				return &keyDomainRefused{*d} // the transaction rolls the delete back
			}
			out.Previous, out.Found = &prev, true
			return nil
		}
		ambiguous := func(ctx context.Context, group, domain string) (int, error) {
			return svc.AmbiguousIfGroupQ(ctx, q, group, domain)
		}
		truncated := func(ctx context.Context) (int, []string, error) { return svc.TruncatedUnassignedQ(ctx, q) }
		if d, err := keyDomainSetRefusal(ctx, svc, ambiguous, truncated, c); err != nil {
			return err
		} else if d != nil {
			return &keyDomainRefused{*d}
		}
		if prev, found, err := svc.GetQ(ctx, q, c.SubjectType, c.Subject, false); err != nil {
			return err
		} else if found {
			out.Previous = &prev
		}
		created, err := svc.SetQ(ctx, q, keydomain.Assignment{SubjectType: c.SubjectType, Subject: c.Subject, Domain: c.Domain, SetBy: by.principal})
		if err != nil {
			return err
		}
		out.Created, out.Found = created, true
		out.Assignment, _, err = svc.GetQ(ctx, q, c.SubjectType, c.Subject, false)
		return err
	})
	if err != nil || !out.Found {
		return keyDomainApplied{}, err
	}
	if c.Delete {
		s.recordAudit(ctx, s.auditEvent(nil, by.typ, by.principal, "key_domain.assignment.delete", keyDomainTarget(c), "success",
			mustJSON(map[string]any{"subject_type": c.SubjectType, "subject": c.Subject, "domain": out.Previous.Domain})))
		return out, nil
	}
	data := map[string]any{"subject_type": c.SubjectType, "subject": c.Subject, "domain": c.Domain, "created": out.Created}
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
	if d, err := keyDomainSetRefusal(r.Context(), svc, svc.AmbiguousIfGroup, svc.TruncatedUnassigned, c); err != nil {
		writeServerError(w, r, "check key domain membership", err)
		return
	} else if d != nil {
		s.refuse(w, r, *d)
		return
	}
	mode, ok := s.governanceWriteMode(w, r)
	if !ok {
		return
	}
	if mode == govQueue {
		s.holdKeyDomainChange(w, r, c)
		return
	}
	out, err := s.applyKeyDomainChange(r.Context(), keyDomainActor{actorTypeFromRequest(r), principalFromRequest(r)}, c)
	var refused *keyDomainRefused
	if errors.As(err, &refused) {
		s.refuse(w, r, refused.d)
		return
	}
	if err != nil {
		writeServerError(w, r, "set key domain assignment", err)
		return
	}
	if mode == govBypass {
		s.recordGovernanceBypass(r, govKindKeyDomain, keyDomainTarget(c), "success", nil)
	}
	status := http.StatusOK
	if out.Created {
		status = http.StatusCreated
	}
	writeJSON(w, status, out.Assignment)
}

// keyDomainSetRefusal is why the set c may not be made as the assignments stand, or nil: a domain
// the file does not declare, or a group whose assignment would leave someone with two groups in
// different domains and no user assignment, or while someone whose last sign-in lost groups (an Entra
// overage always does) has no user assignment, whose next key would then be refused by name. ambiguous
// and truncated count those people, on the pool for a direct write or on the decision transaction for
// a held one.
func keyDomainSetRefusal(ctx context.Context, svc *keydomain.Service, ambiguous func(ctx context.Context, group, domain string) (int, error), truncated func(ctx context.Context) (int, []string, error), c keyDomainChange) (*authz.Decision, error) {
	if !svc.Has(c.Domain) {
		d := authz.Deny(authz.ReasonKeyDomainUnknown, keyDomainTarget(c), fmt.Sprintf(
			"The key domain %q is not declared in the deployment's key domains file, so nothing was changed. Declared: %s.",
			c.Domain, declaredKeyDomains(svc)))
		return &d, nil
	}
	if c.SubjectType != keydomain.SubjectGroup {
		return nil, nil
	}
	n, err := ambiguous(ctx, c.Subject, c.Domain)
	if err != nil {
		return nil, err
	}
	if n > 0 {
		d := authz.Deny(authz.ReasonKeyDomainAmbiguous, keyDomainTarget(c), fmt.Sprintf(
			"%d people last signed in with this group and another group assigned to a different domain, and have no assignment of their own, so their next key would be refused. "+
				"Assign each of them to one domain as a user first, or give both groups the same domain. Nothing was changed.", n))
		return &d, nil
	}
	n, names, err := truncated(ctx)
	if err != nil || n == 0 {
		return nil, err
	}
	who := strings.Join(names, ", ")
	if n > len(names) {
		who += fmt.Sprintf(" and %d more", n-len(names))
	}
	d := authz.Deny(authz.ReasonKeyDomainAmbiguous, keyDomainTarget(c), fmt.Sprintf(
		"%d people last signed in with a group list that was cut short, as a Microsoft Entra group overage does, and have no assignment of their own, so once any group is assigned their next key would be refused: %s. "+
			"Assign each of them to one domain as a user first. Nothing was changed.", n, who))
	return &d, nil
}

// keyDomainDeleteRefusal is why the delete c may not be made, or nil: removing the user assignment of a
// person whose last sign-in lost groups, while any group is assigned, would leave their next key refused
// by name, the lockout keyDomainSetRefusal keeps a group write from creating. truncatedWithGroups says
// whether that holds for a principal, on the pool or on the decision transaction.
func keyDomainDeleteRefusal(ctx context.Context, truncatedWithGroups func(ctx context.Context, principal string) (bool, error), c keyDomainChange) (*authz.Decision, error) {
	if !c.Delete || c.SubjectType != keydomain.SubjectUser {
		return nil, nil
	}
	locked, err := truncatedWithGroups(ctx, c.Subject)
	if err != nil || !locked {
		return nil, err
	}
	d := authz.Deny(authz.ReasonKeyDomainAmbiguous, keyDomainTarget(c), fmt.Sprintf(
		"%s last signed in with a group list that was cut short, as a Microsoft Entra group overage does, and a group is assigned, so without their own assignment their next key would be refused. "+
			"Assign them to another domain instead of deleting it. Nothing was changed.", c.Subject))
	return &d, nil
}

func (s *Server) handleDeleteKeyDomainAssignment(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.keyDomainsOr501(w); !ok {
		return
	}
	c, ok := s.decodeKeyDomainChange(w, r, true)
	if !ok {
		return
	}
	mode, ok := s.governanceWriteMode(w, r)
	if !ok {
		return
	}
	if mode == govQueue {
		s.holdKeyDomainChange(w, r, c)
		return
	}
	out, err := s.applyKeyDomainChange(r.Context(), keyDomainActor{actorTypeFromRequest(r), principalFromRequest(r)}, c)
	var refused *keyDomainRefused
	if errors.As(err, &refused) {
		s.refuse(w, r, refused.d)
		return
	}
	if err != nil {
		writeServerError(w, r, "delete key domain assignment", err)
		return
	}
	if !out.Found {
		writeKeyDomainAssignmentNotFound(w)
		return
	}
	if mode == govBypass {
		s.recordGovernanceBypass(r, govKindKeyDomain, keyDomainTarget(c), "success", nil)
	}
	w.WriteHeader(http.StatusNoContent)
}

func writeKeyDomainAssignmentNotFound(w http.ResponseWriter) {
	writeErrorReason(w, http.StatusNotFound, reasonKeyDomainAssignmentNotFound, "No such key domain assignment.")
}

// declaredKeyDomains lists the names a set may use, for the refusal sentence.
func declaredKeyDomains(svc *keydomain.Service) string {
	return strings.Join(append([]string{keydomain.Default}, svc.Declared()...), ", ")
}
