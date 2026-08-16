// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Per-user run-detail widget layout. The run cockpit lets a user arrange the
// evidence widgets; that arrangement is theirs and has to survive a new
// machine, so it is a server row keyed by principal — ui/src/app/lib/storage.ts
// is localStorage and is explicitly NOT the answer here.
package api

import "net/http"

// handleGetRunLayout serves GET /api/v1/me/run-layout.
// handlePutRunLayout serves PUT /api/v1/me/run-layout.
//
// CONTRACT (lane A4 — fill this in):
//
//   - Follow internal/store/store_sshkeys.go + migration 0033_ssh_public_keys.sql
//     line for line. Same per-principal scoping posture, same ErrNotFound-not-403
//     behaviour (someone else's row is "not found", never a distinguishable 403 —
//     no existence leak).
//
//   - Scope by principalFromRequest(r) ONLY. Never accept a principal in the
//     body. That is the whole security surface of this endpoint.
//
//   - Migration 0035_ui_layouts.sql:
//     ui_run_layouts (principal text, preset text, layout jsonb, updated_at
//     timestamptz, PRIMARY KEY (principal, preset)).
//
//   - preset is a CLOSED set: "live" | "finished". Validate server-side and 400
//     anything else — the console has exactly two default layouts by design and
//     an open string is an unbounded row-per-typo.
//
//   - layout is [{widget, x, y, w, h}]. Validate shape and bound the count;
//     reject an unknown widget id rather than storing it (a stored id no build
//     renders is a layout that silently loses a widget on restore).
//
//   - Store methods GetRunLayout / PutRunLayout go on store.Store + PG. There
//     is exactly ONE implementation (var _ Store = PG{}) — no memory twin to
//     keep in sync.
//
//   - GET with no saved row is NOT an error: return the empty/default shape so
//     the console falls through to its situational default. A 404 here would
//     make "I have never saved a layout" look like a failure.
func (s *Server) handleGetRunLayout(w http.ResponseWriter, r *http.Request) {
	// ponytail: honest "no saved layout" until lane A4 lands — the console
	// already falls back to the situational default for this shape.
	writeJSON(w, http.StatusOK, map[string]any{"preset": "", "layout": []any{}})
}

func (s *Server) handlePutRunLayout(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotImplemented, "layout persistence is not implemented on this build")
}
