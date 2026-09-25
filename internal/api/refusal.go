// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"cmp"
	"context"
	"log/slog"
	"net/http"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// strictRefusals makes an unregistered reason panic. This package's tests set
// it, so a door that invents a reason fails its own test; production answers
// 500 and still records the row instead of crashing.
var strictRefusals bool

// refuse writes d — the registry's status for d's reason, and d's sentence or
// the reason's own — and, when the registry audits the reason, its
// authz.denied row. It returns true so a door can `return s.refuse(...)`.
//
// The one refusal emitter: a door never writes an authz status and its row
// separately, so a new gate cannot ship the error without the audit event, and
// every row carries the same datum (authz.Datum) — including the member-mode
// marker, which a hand-built map at any one site would silently lose.
//
// An unregistered reason fails closed: 500, never the 403 the door meant, so
// the only way to ship a new reason is to register it (and document it, which
// TestAuthzDeniedReasonsAreDocumented demands).
func (s *Server) refuse(w http.ResponseWriter, r *http.Request, d authz.Decision) bool {
	ref, ok := authz.Lookup(d.Reason)
	if !ok {
		writeError(w, http.StatusInternalServerError, "internal error: unregistered refusal reason")
		s.recordRefusal(r.Context(), r, d)
		return true
	}
	writeError(w, ref.Effect.Status(), cmp.Or(d.Sentence, ref.Sentence))
	if ref.Audit {
		s.recordRefusal(r.Context(), r, d)
	}
	return true
}

// recordRefusal writes d's authz.denied row alone: for a drop (no response of
// its own), a 404 twin whose body the caller writes whether or not the row
// exists, or a decision reached with only a ctx. r may be nil; the row is then
// the ctx's human's and carries no method.
func (s *Server) recordRefusal(ctx context.Context, r *http.Request, d authz.Decision) {
	if _, ok := authz.Lookup(d.Reason); !ok {
		if strictRefusals {
			panic("authz: refusal reason " + string(d.Reason) + " is not registered in internal/authz")
		}
		slog.ErrorContext(ctx, "api: refusal with an unregistered reason", slog.String("reason", string(d.Reason)),
			slog.String("target", d.Target))
	}
	// Guarded on the sink: auditEvent stamps from cfg.Now, which a Server
	// assembled without New() does not have.
	if s.cfg.Audit == nil {
		return
	}
	actor, subject, method := types.ActorHuman, oidcHumanFromContext(ctx), ""
	if r != nil {
		actor, subject, method = actorTypeFromRequest(r), principalFromRequest(r), r.Method
	}
	s.recordAudit(ctx, s.refusalEvent(ctx, actor, subject, method, d))
}

// refusalEvent builds d's authz.denied row for subject.
func (s *Server) refusalEvent(ctx context.Context, actor types.ActorType, subject, method string, d authz.Decision) types.AuditEvent {
	p := authz.Principal{Subject: subject, MemberView: oidc.MemberModeFromContext(ctx), UserType: oidc.UserTypeFromContext(ctx)}
	return s.auditEvent(d.RunID, actor, subject, authz.AuditAction, d.Target, "denied", mustJSON(authz.Datum(d, p, method)))
}
