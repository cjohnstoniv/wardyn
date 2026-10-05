// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// The key-domain assignment target kind: the half that HOLDS a set or delete (the handler has already
// decoded and validated it as a direct write is) and the half that APPLIES a held one inside the
// decision transaction, holding the assignment lock the direct write holds, re-validating against the
// assignments it finds and writing through the key-domain service's Querier forms.
//
// No exemption: moving where a person's next keys are made is a custody decision either way.
package api

import (
	"context"
	"fmt"
	"net/http"

	"github.com/cjohnstoniv/wardyn/internal/secretstore/keydomain"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// keyDomainDiffView is an assignment as the reviewer reads it.
type keyDomainDiffView struct {
	SubjectType string `json:"subject_type"`
	Subject     string `json:"subject"`
	Domain      string `json:"domain"`
}

// keyDomainState is the target of a key-domain change as it stands: the assignment, nil when none. A
// proposal and its approval hash one and the same thing.
func keyDomainState(cur keydomain.Assignment, found bool) any {
	if !found {
		return map[string]any{"assignment": nil}
	}
	return map[string]any{"assignment": cur}
}

// holdKeyDomainChange stores a validated key-domain set or delete as a pending change. A delete of an
// assignment that does not exist is the direct 404, never held.
func (s *Server) holdKeyDomainChange(w http.ResponseWriter, r *http.Request, c keyDomainChange) {
	cur, found, err := s.cfg.KeyDomains.Get(r.Context(), c.SubjectType, c.Subject)
	if err != nil {
		writeServerError(w, r, "hold key domain assignment", err)
		return
	}
	if c.Delete && !found {
		writeKeyDomainAssignmentNotFound(w)
		return
	}
	if d, err := keyDomainDeleteRefusal(r.Context(), s.cfg.KeyDomains.TruncatedWithGroups, c); err != nil {
		writeServerError(w, r, "hold key domain assignment", err)
		return
	} else if d != nil {
		s.refuse(w, r, *d)
		return
	}
	p := govProposal{kind: govKindKeyDomain, op: "upsert", key: keyDomainTarget(c), payload: c, baseState: keyDomainState(cur, found)}
	var before, after *keyDomainDiffView
	if found {
		before = &keyDomainDiffView{SubjectType: cur.SubjectType, Subject: cur.Subject, Domain: cur.Domain}
		p.before = before
	}
	if c.Delete {
		p.op = "delete"
	} else {
		after = &keyDomainDiffView{SubjectType: c.SubjectType, Subject: c.Subject, Domain: c.Domain}
		p.after = after
	}
	p.changed = changedPaths(before, after)
	s.proposeGovernanceChange(w, r, p)
}

// applyKeyDomainAssignmentChange applies a held key-domain change inside the decision transaction: the
// assignment lock taken first (keydomain.LockAssignments, the lock every assignment write holds), the
// assignment read FOR UPDATE and compared with what the proposal reviewed, the set re-validated
// against the assignments as they now stand, then the write on q. Under the lock no other group's
// assignment can land between the re-validation and the write.
func applyKeyDomainAssignmentChange(s *Server, r *http.Request, q store.Querier, ch types.GovernanceChange) (govApplied, error) {
	ctx := r.Context()
	svc := s.cfg.KeyDomains
	if svc == nil {
		return govApplied{}, writeRefusal(http.StatusNotImplemented, reasonKeyDomainsStoreUnavailable, "key domains require the Postgres store backend")
	}
	var c keyDomainChange
	if err := decodeHeldPayload(ch.Payload, &c); err != nil {
		return govApplied{}, fmt.Errorf("governance change %s: payload: %w", ch.ID, err)
	}
	if err := keydomain.LockAssignments(ctx, q); err != nil {
		return govApplied{}, err
	}
	cur, found, err := svc.GetQ(ctx, q, c.SubjectType, c.Subject, true)
	if err != nil {
		return govApplied{}, err
	}
	if computeETag(keyDomainState(cur, found)) != ch.BaseHash {
		return govApplied{}, store.ErrGovernanceChangeStale
	}
	target := keyDomainTarget(c)
	if c.Delete {
		if d, err := keyDomainDeleteRefusal(ctx, func(ctx context.Context, p string) (bool, error) { return svc.TruncatedWithGroupsQ(ctx, q, p) }, c); err != nil {
			return govApplied{}, err
		} else if d != nil {
			return govApplied{}, &govRefusal{why: string(d.Reason), write: func(w http.ResponseWriter, r *http.Request) { s.refuse(w, r, *d) }}
		}
		prev, deleted, err := svc.DeleteQ(ctx, q, c.SubjectType, c.Subject)
		if err != nil {
			return govApplied{}, err
		}
		if !deleted {
			return govApplied{}, store.ErrGovernanceChangeStale
		}
		return govApplied{action: "key_domain.assignment.delete", target: target,
			data: map[string]any{"subject_type": c.SubjectType, "subject": c.Subject, "domain": prev.Domain}}, nil
	}
	ambiguous := func(ctx context.Context, group, domain string) (int, error) {
		return svc.AmbiguousIfGroupQ(ctx, q, group, domain)
	}
	if d, err := keyDomainSetRefusal(ctx, svc, ambiguous, func(ctx context.Context) (int, []string, error) { return svc.TruncatedUnassignedQ(ctx, q) }, c); err != nil {
		return govApplied{}, err
	} else if d != nil {
		return govApplied{}, &govRefusal{why: string(d.Reason), write: func(w http.ResponseWriter, r *http.Request) { s.refuse(w, r, *d) }}
	}
	created, err := svc.SetQ(ctx, q, keydomain.Assignment{SubjectType: c.SubjectType, Subject: c.Subject, Domain: c.Domain, SetBy: ch.ProposedBy})
	if err != nil {
		return govApplied{}, err
	}
	data := map[string]any{"subject_type": c.SubjectType, "subject": c.Subject, "domain": c.Domain, "created": created}
	if found {
		data["previous_domain"] = cur.Domain
	}
	return govApplied{action: "key_domain.assignment.set", target: target, data: data}, nil
}
