// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/go-chi/chi/v5"
)

func (s *Server) handleAdminDeleteSSHKeys(w http.ResponseWriter, r *http.Request) {
	raw := chi.URLParam(r, "principal")
	if r.URL.RawPath != "" {
		decoded, err := url.PathUnescape(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid principal encoding")
			return
		}
		raw = decoded
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		writeError(w, http.StatusBadRequest, "name the person whose SSH keys to remove")
		return
	}
	count, _, refusal, err := s.deleteSSHKeysFor(r, raw)
	if err != nil {
		writeServerError(w, r, "delete ssh keys", err)
		return
	}
	if refusal != "" {
		writeError(w, http.StatusUnprocessableEntity, refusal)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"count": count})
}

func (s *Server) deleteSSHKeysFor(r *http.Request, principal string) (int, string, string, error) {
	target, scope := principal, "sub"
	var refusal string
	var err error
	if principal == "" {
		target, scope = "*", "all"
	} else {
		var resolved string
		resolved, refusal, err = s.resolveSSHKeyOwner(r.Context(), principal)
		if err == nil && refusal == "" {
			principal, target = resolved, resolved
		}
	}
	count := 0
	if err == nil && refusal == "" {
		count, err = s.cfg.Store.DeleteSSHKeys(r.Context(), principal)
	}
	data := map[string]any{"scope": scope, "count": count}
	outcome := "success"
	if err != nil {
		outcome, data["error"] = "failure", err.Error()
	}
	if refusal != "" {
		outcome, data["reason"] = "failure", refusal
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		"ssh_key.delete", target, outcome, mustJSON(data)))
	return count, principal, refusal, err
}

func (s *Server) resolveSSHKeyOwner(ctx context.Context, raw string) (string, string, error) {
	keys, err := s.cfg.Store.ListSSHKeysByPrincipal(ctx, raw)
	if err != nil {
		return "", "", fmt.Errorf("resolve SSH principal keys: %w", err)
	}
	if len(keys) > 0 {
		return raw, "", nil
	}
	// A partial directory could make an ambiguous name look unique, or mistake
	// an email for an unrelated subject. Removal must not guess after a failed read.
	tokens, err := s.cfg.Store.ListAPITokens(ctx)
	if err != nil {
		return "", "", fmt.Errorf("resolve SSH principal tokens: %w", err)
	}
	workspaces, err := s.cfg.Store.ListWorkspaces(ctx)
	if err != nil {
		return "", "", fmt.Errorf("resolve SSH principal workspaces: %w", err)
	}
	var directory []principalIdentity
	for _, token := range tokens {
		if token.Principal != "" {
			directory = append(directory, principalIdentity{principal: token.Principal, email: token.Email})
		}
	}
	for _, ws := range workspaces {
		if ws.OwnedBy != "" {
			directory = append(directory, principalIdentity{principal: ws.OwnedBy})
		}
	}
	principal, _, refusal := resolvePrincipal(directory, raw)
	switch refusal {
	case secretOwnerUnresolvedMsg:
		refusal = "no known principal matches that email address; name the subject exactly"
	case secretOwnerAmbiguousMsg:
		refusal = "more than one known principal matches; name the subject exactly"
	}
	return principal, refusal, nil
}
