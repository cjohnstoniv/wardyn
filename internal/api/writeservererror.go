// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/hostcapacity"
	"github.com/cjohnstoniv/wardyn/internal/store"
)

const runCapMsg = "this deployment is at its limit of concurrent runs (WARDYN_MAX_CONCURRENT_RUNS) — wait for one to finish, or ask an admin to stop one"

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
//
// One resolver failure is not a 5xx: errUserTypeUnknown is a refusal about the
// caller, raised through the same (bool, error) seams a store failure takes, so
// it is answered 403 with its sentence here rather than at every seam.
func writeServerError(w http.ResponseWriter, r *http.Request, msg string, err error) {
	// A revoked hybrid laptop's run refusal (createRun) is not a fault: it is
	// the one 5xx whose sentence is the remedy, whichever launcher reached it,
	// and the forwarder already logged the revocation once.
	if errors.Is(err, errOrgRevoked) {
		writeErrorReason(w, http.StatusServiceUnavailable, reasonOrgRevoked, orgRevokedMsg)
		return
	}
	// The deployment run cap is a quota like the per-principal one: 422, no audit
	// (POST /runs refuses before its identity mint, refuseRunCapFull).
	if errors.Is(err, store.ErrRunCapReached) {
		writeErrorReason(w, http.StatusUnprocessableEntity, string(authz.ReasonRunQuota), runCapMsg)
		return
	}
	if writeHostCapacityRefusal(w, r, err) {
		return
	}
	if errors.Is(err, errUserTypeUnknown) {
		// The SAME registered reason authz's own ReasonUserTypeUnknown names
		// (#656 slice 3) — this resolver failure is exactly that cause,
		// reached through the (bool, error) seam rather than authz.Deny.
		writeErrorReason(w, http.StatusForbidden, string(authz.ReasonUserTypeUnknown), userTypeUnknownMsg)
		return
	}
	slog.ErrorContext(r.Context(), "api: "+msg,
		slog.String("method", r.Method),
		slog.String("path", r.URL.Path),
		slog.Any("err", err),
	)
	// #656 slice 3: every OTHERWISE-unclassified 500 this package writes goes
	// through here, so this is the one place a generic wire reason can cover
	// all of them at once — never the driver text err carries (that stays in
	// the log line above), just a class a caller can at least branch on
	// ("something failed server-side") versus every classified reason above.
	writeErrorReason(w, http.StatusInternalServerError, reasonInternalError, msg)
}

// writeHostCapacityRefusal answers a hostcapacity refusal 503 with a
// Retry-After and logs its reason, reporting whether err was one. Like
// errOrgRevoked above it is not a fault, and every launcher's store-failure path
// already reaches writeServerError, so none needs a branch of its own.
func writeHostCapacityRefusal(w http.ResponseWriter, r *http.Request, err error) bool {
	var refused hostcapacity.ErrRefused
	if !errors.As(err, &refused) {
		return false
	}
	slog.WarnContext(r.Context(), "api: run launch refused: host capacity",
		slog.String("path", r.URL.Path), slog.String("reason", refused.Reason))
	w.Header().Set("Retry-After", "30")
	writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "host_capacity_refused", "reason": refused.Reason})
	return true
}

// loggedMsg is writeServerError for the few 5xx sites that keep their own
// status code (a 503 from an unreachable runner): it logs err against msg and
// hands back msg alone, so the call site stays one line and its body stays
// free of driver text.
func loggedMsg(ctx context.Context, msg string, err error) string {
	slog.ErrorContext(ctx, "api: "+msg, slog.Any("err", err))
	return msg
}
