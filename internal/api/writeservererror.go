// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"log/slog"
	"net/http"
)

// writeServerError is writeError's 5xx twin: it LOGS the underlying error and
// writes only msg to the caller.
//
// It exists because ~149 sites in this package wrote
// `writeError(w, 500, "<action>: "+err.Error())`, and err there is raw
// pgx/driver text — the DB host, port, user and database name from a dial
// failure, the SQLSTATE plus the table or constraint name from a refused
// statement. The member tier reaches plenty of those sites (a run read, a
// workspace read, the approvals list), so an ordinary member could read the
// deployment's database topology out of one transient store failure. Two files
// already followed this convention by hand (attach_ticket.go, audit.go); this
// is that convention with a name.
//
// The error is not DROPPED — that would trade a disclosure for a blind
// operator. It goes to the log with the method and path, which is where an
// operator diagnosing a 500 is already looking.
//
// The caller keeps writing its own msg rather than a generic one: "get run"
// and "list approvals" failing are different incidents to the human reading
// the console, and the action name discloses nothing the route did not.
func writeServerError(w http.ResponseWriter, r *http.Request, msg string, err error) {
	slog.ErrorContext(r.Context(), "api: "+msg,
		slog.String("method", r.Method),
		slog.String("path", r.URL.Path),
		slog.Any("err", err),
	)
	writeError(w, http.StatusInternalServerError, msg)
}

// loggedMsg is writeServerError for the few 5xx sites that keep their own
// status code (a 503 from an unreachable runner): it logs err against msg and
// hands back msg alone, so the call site stays one line and its body stays
// free of driver text.
func loggedMsg(ctx context.Context, msg string, err error) string {
	slog.ErrorContext(ctx, "api: "+msg, slog.Any("err", err))
	return msg
}
