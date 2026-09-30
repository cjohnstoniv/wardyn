// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// ado_pat_refusal.go is what the Azure DevOps row's admin banner reads to learn
// that a person's launch was refused because the organisation restricts who may
// create tokens (#1449): the newest such refusal in the last seven days, read
// from the audit rows the mint already writes (adoPATAuditMintDenied). It adds
// no audit action, reason or column.
//
// Admin only (operatorOnly): the answer names a person other than the caller.
// The person is given as the email Wardyn holds for them, the value an admin
// already reads for that person; a refusal whose person has no email on file is
// passed over, never answered with the subject, and nothing else of the audit
// row (owner subject, organisation, scope, Azure DevOps' own error) is returned.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/cjohnstoniv/wardyn/internal/store"
)

const (
	// adoPATRefusalWindow is how far back a refusal still counts.
	adoPATRefusalWindow = 7 * 24 * time.Hour
	// adoPATRefusalScan bounds the audit read: the newest this many policy-
	// blocked refusals on the row inside the window (the store filters on the
	// refusal and the row, so other denials never count against it). More than
	// one, because a refused person with no email on file is passed over.
	adoPATRefusalScan = 50
)

// adoPATRefusal is the answer: who was refused and when. Nothing else.
type adoPATRefusal struct {
	Person string    `json:"person"`
	At     time.Time `json:"at"`
}

// mountADOPATRefusalRoute mounts the read on the operator tier, beside the
// provider rows and the organisation check it explains.
func (s *Server) mountADOPATRefusalRoute(operatorOnly chi.Router) {
	operatorOnly.Get("/workspace-providers/git/{id}/ado-pat-refusal", s.handleADOPATRefusal)
}

// handleADOPATRefusal serves GET /workspace-providers/git/{id}/ado-pat-refusal:
// the newest policy-blocked mint refusal on row {id} in the last seven days, as
// {person, at}, or 204 when there is none (or none whose person has an email).
func (s *Server) handleADOPATRefusal(w http.ResponseWriter, r *http.Request) {
	ctx, rowID := r.Context(), chi.URLParam(r, "id")
	pager, ok := s.cfg.Store.(store.Pager)
	if !ok {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	rows, err := pager.QueryAuditEventsFilteredPage(ctx, nil, store.AuditFilter{
		Action:       adoPATAuditMintDenied,
		Since:        s.cfg.Now().Add(-adoPATRefusalWindow),
		DataContains: string(mustJSON(map[string]any{"refusal": reasonADOPATPolicyBlocked, "provider_row": rowID})),
	}, store.Page{Limit: adoPATRefusalScan})
	if err != nil {
		writeServerError(w, r, "read the Azure DevOps token refusals", err)
		return
	}
	emails := map[string]string{} // owner -> email, "" when none: one lookup per person
	for _, ev := range rows {     // newest first
		var d struct {
			Owner string `json:"owner"`
		}
		if json.Unmarshal(ev.Data, &d) != nil || d.Owner == "" {
			continue
		}
		email, seen := emails[d.Owner]
		if !seen {
			if email, err = s.principalEmail(ctx, d.Owner); err != nil {
				writeServerError(w, r, "read the refused person", err)
				return
			}
			emails[d.Owner] = email
		}
		if email != "" {
			writeJSON(w, http.StatusOK, adoPATRefusal{Person: email, At: ev.Time.UTC()})
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// principalEmail is the email Wardyn holds for the person keyed by principal:
// their People record, else one of their API tokens; "" when it holds none.
func (s *Server) principalEmail(ctx context.Context, principal string) (string, error) {
	if ps, ok := s.cfg.Store.(store.PersonStore); ok {
		p, err := ps.GetPerson(ctx, principal)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return "", err
		}
		if err == nil && p.Email != "" {
			return p.Email, nil
		}
	}
	toks, err := s.cfg.Store.ListAPITokensByPrincipal(ctx, principal)
	if err != nil {
		return "", err
	}
	for _, t := range toks {
		if e := strings.TrimSpace(t.Email); e != "" {
			return e, nil
		}
	}
	return "", nil
}
