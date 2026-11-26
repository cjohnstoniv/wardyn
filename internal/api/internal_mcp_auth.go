// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// An MCP server that starts from a command runs behind an MCP proxy of its own, whose control-plane token is
// the run's identity for mcpSandboxAudience. mcpSandboxAuth, ahead of internalAuth on the internal group,
// serves that token on five routes and nothing else; on any other route, and for any other token, it steps
// aside and internalAuth refuses the MCP audience. It does not read the feature switch: a token exists only
// for a row made while the switch was on, and a sandbox still running when it is turned off keeps its
// trail and its renewal until the run ends it.

// mcpSandboxRoutes are the five routes an MCP proxy's token is served on, keyed by method and chi route
// pattern (chi has resolved the route before a group's middleware runs, as auditStrictIntent relies on).
// wardyn-toolgate's give-up and the agent's MCP call keys (/expire, /sent) are not the MCP proxy's.
var mcpSandboxRoutes = map[string]func(*Server, http.ResponseWriter, *http.Request){
	"POST /api/v1/internal/decisions":          (*Server).handlePostDecision,
	"GET /api/v1/internal/injection/{grantID}": (*Server).handleMCPSandboxInjection,
	"POST /api/v1/internal/token/renew":        (*Server).handleInternalMCPTokenRenew,
	"POST /api/v1/internal/approvals":          (*Server).handleInternalRequestApproval,
	"GET /api/v1/internal/approvals/{id}":      (*Server).handleInternalGetApproval,
}

type mcpSandboxRowKey struct{}

// mcpSandboxRowFrom is the server row mcpSandboxAuth matched the request's token to.
func mcpSandboxRowFrom(ctx context.Context) (store.RunMCPSandbox, bool) {
	row, ok := ctx.Value(mcpSandboxRowKey{}).(store.RunMCPSandbox)
	return row, ok
}

// mcpSandboxCaller reports whether the request is an MCP proxy's. Its decisions are not the run's activity:
// a server that polls a host must keep the run neither from idling out nor from pausing, so decisionTouch
// asks this before shouldTouch (which takes the run's debounce slot when it answers true) and before the
// pause's presence stamp.
func mcpSandboxCaller(ctx context.Context) bool { return audit.MCPSandboxOf(ctx) != "" }

// decisionTouch is handlePostDecision's activity step for one decision: the idle clock (TouchRun, once per
// touchDebounce) and the pause's presence clock, neither for a run that is over. An MCP proxy's decision moves
// neither, and is judged so ahead of shouldTouch, so it never takes the agent's debounce slot. Carved out of
// handlePostDecision, which sits at its gocyclo gate.
func (s *Server) decisionTouch(ctx context.Context, runID uuid.UUID, kind types.MachineKind, isOver func() bool, dl egress.DecisionLog) {
	if s.cfg.Store != nil && !mcpSandboxCaller(ctx) && s.shouldTouch(runID, kind, dl) && !isOver() {
		_ = s.cfg.Store.TouchRun(ctx, runID)
	}
	if !mcpSandboxCaller(ctx) && decisionIsActivity(kind, dl) && s.pause.stampDue(false, runID, s.cfg.Now()) && !isOver() {
		s.noteAgentActive(ctx, runID)
	}
}

// tokenAudienceHint is the aud a bearer JWT says it carries, unverified: which door verifies it, never a
// decision. A token that is not a JWT has none.
func tokenAudienceHint(tok string) []string {
	c, ok := tokenPayloadHint(tok)
	if !ok {
		return nil
	}
	var one string
	if json.Unmarshal(c.Aud, &one) == nil {
		return []string{one}
	}
	var many []string
	_ = json.Unmarshal(c.Aud, &many)
	return many
}

// tokenHint is the two claims the MCP door reads from a compact token's payload before, or without, a verify.
type tokenHint struct {
	Aud json.RawMessage `json:"aud"`
	JTI string          `json:"jti"`
}

// tokenPayloadHint decodes tok's payload unverified. A caller trusts what it reads only as far as a verify of
// the same token has gone.
func tokenPayloadHint(tok string) (tokenHint, bool) {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return tokenHint{}, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return tokenHint{}, false
	}
	var c tokenHint
	return c, json.Unmarshal(raw, &c) == nil
}

// refuseMCPSandboxToken answers an MCP proxy's token that did not verify. The run's once-only
// run.identity.expire row and the decision door's run.report.drop row are the agent's evidence, so it writes
// neither: it writes one auth.fail row and answers 401, or 503 when the run behind a revoked token could not
// be read. An expired token names its run, and its signature has been checked, so its jti is read from the
// payload and the row is labelled with the key of the server row that holds that jti; this costs one read of
// the run's rows, as a live token's post does.
func (s *Server) refuseMCPSandboxToken(w http.ResponseWriter, r *http.Request, tok string, err error) {
	var unreadable *runUnreadableError
	if errors.As(err, &unreadable) {
		slog.ErrorContext(r.Context(), "api: read the run behind an MCP proxy's revoked token",
			slog.String("run_id", unreadable.RunID.String()), slog.Any("err", unreadable.Err))
		writeErrorReason(w, http.StatusServiceUnavailable, reasonInternalLivenessReadFailed, "read run failed; retry")
		return
	}
	var expired *identity.ExpiredTokenError
	if hint, ok := tokenPayloadHint(tok); ok && hint.JTI != "" && errors.As(err, &expired) {
		if key := s.mcpSandboxKeyOfJTI(r.Context(), expired.RunID, hint.JTI); key != "" {
			r = r.WithContext(audit.WithMCPSandbox(r.Context(), key))
		}
	}
	s.auditAuthFailedAs(r, internalAuthActor, "invalid_run_token")
	writeErrorReason(w, http.StatusUnauthorized, reasonInvalidRunToken, "invalid run token")
}

// mcpSandboxKeyOfJTI is the key of runID's server row whose current or previous jti is jti, or "" when no row
// holds it or the rows could not be read.
func (s *Server) mcpSandboxKeyOfJTI(ctx context.Context, runID uuid.UUID, jti string) string {
	st, ok := s.cfg.Store.(store.RunMCPSandboxStore)
	if !ok {
		return ""
	}
	rows, _ := st.ListRunMCPSandboxes(ctx, runID) // a failed read leaves the refusal's row unlabelled
	for _, row := range rows {
		if row.CPTokenJTI == jti || row.PrevCPTokenJTI == jti {
			return row.ServerKey
		}
	}
	return ""
}

// mcpSandboxAuth serves an MCP proxy's token on its five routes. For that token on one of them it never calls
// next: it verifies the token for the MCP audience, refuses a relayed request (no run reached through a
// runner has an MCP sandbox), holds the run to internalAuth's liveness and relay gates, matches the token to
// its server's row, then calls the route's handler with the claims, the run and the row's key in context.
func (s *Server) mcpSandboxAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok, ok := bearerToken(r)
		rctx := chi.RouteContext(r.Context())
		if !ok || s.cfg.Identity == nil || rctx == nil || !slices.Contains(tokenAudienceHint(tok), mcpSandboxAudience) {
			next.ServeHTTP(w, r)
			return
		}
		handler, served := mcpSandboxRoutes[r.Method+" "+rctx.RoutePattern()]
		if !served {
			next.ServeHTTP(w, r)
			return
		}
		claims, err := s.verifyRunTokenFor(r, tok, mcpSandboxAudience)
		if err != nil {
			s.refuseMCPSandboxToken(w, r, tok, err)
			return
		}
		r = r.WithContext(contextWithClaims(r.Context(), claims))
		if runner, relayed := relayRunner(r.Context()); relayed {
			s.denyRelayed(w, r, claims, placement.ReasonRelayRouteRefused, runner.String())
			return
		}
		run, ok := s.refuseTerminalRun(s.decisionGate(w, r, claims), r, claims)
		if !ok || s.refuseRelayBinding(s.decisionGate(w, r, claims), r, claims, run) {
			return
		}
		row, ok := s.mcpSandboxRowFor(w, r, claims)
		if !ok {
			return
		}
		ctx := context.WithValue(audit.WithMCPSandbox(contextWithLiveRun(r.Context(), run), row.ServerKey), mcpSandboxRowKey{}, row)
		handler(s, w, r.WithContext(ctx))
	})
}

// mcpSandboxRowFor is the run's server row the token names: the one whose current jti it is, or whose previous
// jti it is while that token has not lapsed. A stopped row is refused, except at the decision door within
// terminalUploadGrace of its stop, so the rows its proxy flushes as it stops still land; a row with no server
// key is refused as no row (the table refuses one too). It writes its own
// refusal and returns false once it has.
func (s *Server) mcpSandboxRowFor(w http.ResponseWriter, r *http.Request, claims *identity.Claims) (store.RunMCPSandbox, bool) {
	unknown := func() (store.RunMCPSandbox, bool) {
		s.auditAuthFailedAs(r, internalAuthActor, reasonMCPSandboxUnknown)
		writeErrorReason(w, http.StatusForbidden, reasonMCPSandboxUnknown, "no MCP server of this run holds this token")
		return store.RunMCPSandbox{}, false
	}
	st, ok := s.cfg.Store.(store.RunMCPSandboxStore)
	if !ok || claims.JTI == "" {
		return unknown()
	}
	rows, err := st.ListRunMCPSandboxes(r.Context(), claims.RunID)
	if err != nil {
		slog.ErrorContext(r.Context(), "api: read the run's MCP servers for its proxy's token",
			slog.String("run_id", claims.RunID.String()), slog.Any("err", err))
		writeErrorReason(s.decisionGate(w, r, claims), http.StatusServiceUnavailable, reasonInternalLivenessReadFailed, "read the run's MCP servers failed; retry")
		return store.RunMCPSandbox{}, false
	}
	var now time.Time // the database's: prev_cp_token_until and stopped_at are compared on one clock
	for _, row := range rows {
		current := row.CPTokenJTI == claims.JTI
		if !current && (row.PrevCPTokenJTI != claims.JTI || row.PrevCPTokenUntil == nil) {
			continue
		}
		if now.IsZero() && (!current || row.State == store.MCPSandboxStopped) {
			if now, err = s.dbNow(r.Context()); err != nil {
				writeErrorReason(s.decisionGate(w, r, claims), http.StatusServiceUnavailable, reasonInternalLivenessReadFailed, "read the database's clock failed; retry")
				return store.RunMCPSandbox{}, false
			}
		}
		if !current && !now.Before(*row.PrevCPTokenUntil) {
			continue
		}
		if row.State == store.MCPSandboxStopped &&
			(r.URL.Path != decisionsDoor || row.StoppedAt == nil || now.Sub(*row.StoppedAt) > terminalUploadGrace) {
			return unknown()
		}
		if row.ServerKey == "" { // the key is the label its rows carry, and an empty one reads as the agent's
			return unknown()
		}
		return row, true
	}
	return unknown()
}

// handleMCPSandboxInjection resolves an injection grant for an MCP proxy only when the grant is its own
// server's.
func (s *Server) handleMCPSandboxInjection(w http.ResponseWriter, r *http.Request) {
	row, _ := mcpSandboxRowFrom(r.Context())
	id, err := uuid.Parse(chi.URLParam(r, "grantID"))
	if err != nil || !slices.Contains(row.GrantIDs, id) {
		s.auditAuthFailedAs(r, internalAuthActor, reasonMCPSandboxUnknown)
		writeErrorReason(w, http.StatusForbidden, reasonMCPSandboxUnknown, "this MCP server holds no such grant")
		return
	}
	s.handleInternalInjection(w, r)
}

// handleInternalMCPTokenRenew renews an MCP proxy's token: it mints the MCP audience again under the run's
// SPIFFE ID and moves the row to it, keeping the old jti until the old token's expiry, so posts the proxy
// queued under it still land. It refuses as the run token's renewal does (a run that is over, kept or
// containment pending) and refuses a row that stopped or moved on. It never stamps the agent's token lineage.
func (s *Server) handleInternalMCPTokenRenew(w http.ResponseWriter, r *http.Request) {
	claims, err := claimsFromContext(r)
	row, inRow := mcpSandboxRowFrom(r.Context())
	if err != nil || !inRow {
		writeErrorReason(w, http.StatusUnauthorized, reasonMissingRunClaims, "missing run claims")
		return
	}
	st, ok := s.cfg.Store.(store.RunMCPSandboxStore)
	if !ok {
		writeErrorReason(w, http.StatusServiceUnavailable, reasonRunRenewStoreUnavailable, "run store not configured")
		return
	}
	run, ok := s.mcpRenewRun(w, r, claims)
	if !ok {
		return
	}
	sponsor := claims.Sponsor
	if isServicePrincipal(claims.Sub) {
		var serr error
		if sponsor, serr = s.runSponsor(r.Context(), claims.Sub); errors.Is(serr, errServiceRetired) {
			s.auditRenewDenied(r, claims, "service_identity_retired")
			writeErrorReason(w, http.StatusForbidden, reasonServiceIdentityInactive, "the service identity that started this run is retired")
			return
		} else if serr != nil {
			writeServerError(w, r, "renew an MCP server's token", serr)
			return
		}
	}
	id, err := s.mintRunIdentityAs(r.Context(), claims.RunID, runWorkloadOf(claims.SPIFFEID, claims.RunID), claims.Sub, sponsor, claims.OperatorOwned, mcpSandboxAudience)
	if err != nil {
		writeServerError(w, r, "renew an MCP server's token", err)
		return
	}
	moved, err := st.RotateMCPSandboxToken(r.Context(), claims.RunID, row.Ordinal, claims.JTI, id.JTI, claims.Expiry)
	if err != nil {
		writeErrorReason(w, http.StatusServiceUnavailable, reasonRunRenewStampFailed, loggedMsg(r.Context(), "record an MCP server's renewed token", err))
		return
	}
	if !moved {
		s.auditRenewDenied(r, claims, reasonMCPSandboxUnknown)
		writeErrorReason(w, http.StatusForbidden, reasonMCPSandboxUnknown, "this token is not its MCP server's current one, or the server stopped")
		return
	}
	ev := s.auditEvent(&claims.RunID, types.ActorAgent, claims.SPIFFEID, "identity.renew", id.JTI, "success", mustJSON(map[string]any{
		"prev_jti": claims.JTI, "expires_at": id.Expiry.UTC().Format(time.RFC3339), "run_state": run.State,
	}))
	ev.SourceIP = r.RemoteAddr
	s.recordAudit(r.Context(), ev)
	writeJSON(w, http.StatusOK, tokenRenewResponse{Token: id.Token, ExpiresAt: id.Expiry.UTC().Format(time.RFC3339)})
}

// mcpRenewRun is the run an MCP token's renewal is for, after the run token renewal's own refusals: a run
// that is terminal, past its lifetime cap, kept, or containment pending. It writes its own refusal and
// returns false once it has.
func (s *Server) mcpRenewRun(w http.ResponseWriter, r *http.Request, claims *identity.Claims) (types.AgentRun, bool) {
	run, err := s.cfg.Store.GetRun(r.Context(), claims.RunID)
	if errors.Is(err, store.ErrNotFound) {
		s.auditRenewDenied(r, claims, reasonRunNotFound)
		writeErrorReason(w, http.StatusForbidden, reasonRunNotFound, "run not found")
		return run, false
	}
	if err != nil {
		writeErrorReason(w, http.StatusServiceUnavailable, reasonRunRenewReadFailed, loggedMsg(r.Context(), "read run", err))
		return run, false
	}
	refuse := func(reason, wire, msg string) (types.AgentRun, bool) {
		s.auditRenewDenied(r, claims, reason)
		writeErrorReason(w, http.StatusForbidden, wire, msg)
		return run, false
	}
	if isTerminalRunState(run.State) {
		return refuse("run_terminal:"+string(run.State), string(authz.ReasonRunTerminal), "run is terminal")
	}
	if run.LifetimeCapAt != nil {
		now, err := s.dbNow(r.Context())
		if err != nil {
			writeErrorReason(w, http.StatusServiceUnavailable, reasonRunRenewReadFailed, loggedMsg(r.Context(), "read the database's clock", err))
			return run, false
		}
		if pastLifetimeCap(run, now) {
			return refuse("run_past_lifetime", string(authz.ReasonRunTerminal), "run has reached its lifetime cap")
		}
	}
	if runIsKept(run) {
		return refuse("run_lost:"+string(run.LostReason), string(authz.ReasonRunKept), "run is lost")
	}
	pending, perr := s.runContainmentPending(r.Context(), run)
	if perr != nil {
		writeErrorReason(w, http.StatusServiceUnavailable, reasonRunRenewReadFailed, loggedMsg(r.Context(), "read run containment", perr))
		return run, false
	}
	if pending {
		return refuse(reasonRunContainmentPending, reasonRunContainmentPending, "run is containment pending: its runner deleted it and is waiting for proof that it stopped")
	}
	return run, true
}

// reservedScopeKey reports whether scope names, at top level and in any letter case (encoding/json matches
// keys so), mcp_sandbox: the label only the control plane writes, from the MCP proxy's own row.
func reservedScopeKey(scope json.RawMessage) bool {
	var top map[string]json.RawMessage
	if json.Unmarshal(scope, &top) != nil {
		return false
	}
	for k := range top {
		if strings.EqualFold(k, "mcp_sandbox") {
			return true
		}
	}
	return false
}

// mcpSandboxScope is handleInternalRequestApproval's MCP step, after its own refusals and before the rejoin.
// A scope naming mcp_sandbox is refused, whoever sends it. An MCP proxy may raise only an egress hold, which
// is labelled with its server's key. A rejoin reaches only a row of the caller's own label: it reads the row
// first, so a refused rejoin stamps nothing. It writes its own refusal and returns false once it has.
func (s *Server) mcpSandboxScope(w http.ResponseWriter, r *http.Request, body *internalApprovalRequest) bool {
	if reservedScopeKey(body.RequestedScope) {
		s.auditAuthFailedAs(r, internalApprovalActor, reasonReservedScopeKey)
		writeErrorReason(w, http.StatusBadRequest, reasonReservedScopeKey, "requested_scope may not name mcp_sandbox")
		return false
	}
	if key := audit.MCPSandboxOf(r.Context()); key != "" {
		if body.Kind != types.ApprovalEgressDomain {
			s.auditAuthFailedAs(r, internalApprovalActor, reasonUnsupportedInternalApprovalKind)
			writeErrorReason(w, http.StatusBadRequest, reasonUnsupportedInternalApprovalKind, "unsupported approval kind for internal request")
			return false
		}
		var top map[string]json.RawMessage
		if json.Unmarshal(body.RequestedScope, &top) != nil || top == nil {
			writeErrorReason(w, http.StatusBadRequest, reasonInternalApprovalRequestInvalid, "invalid approval request")
			return false
		}
		top["mcp_sandbox"], _ = json.Marshal(key)
		body.RequestedScope, _ = json.Marshal(top)
	}
	if body.RejoinID == nil {
		return true
	}
	ap, err := s.cfg.Approvals.Get(r.Context(), *body.RejoinID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return true // the rejoin answers its own miss
	case err != nil:
		slog.ErrorContext(r.Context(), "api: read the approval a raise rejoins", slog.Any("err", err))
		writeErrorReason(w, http.StatusServiceUnavailable, reasonInternalLivenessReadFailed, "read the approval failed; retry")
		return false
	case !mcpRowMatchesCaller(r.Context(), ap):
		writeErrorReason(w, http.StatusNotFound, reasonApprovalNotFound, "no pending approval to rejoin")
		return false
	}
	return true
}
