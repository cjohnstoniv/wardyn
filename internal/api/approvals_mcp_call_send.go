// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// An approve_each MCP call may carry an operation key (types.MCPCallScope.OperationKey). A keyed call is
// sent at most once in its namespace: the run owner's, within the run's correlation ID, else within the
// run, both read from the run row internalAuth's liveness gate read for the token, never from the body.
// The run proxy records the send here before it sends (handleInternalMCPCallSent), and a raise of a key
// already recorded is answered with that send instead of asking a person again (refuseSentOperation).
// Nothing here reads or writes for a keyless call.

// What a keyed call's refusals and faults say to the run proxy.
const (
	operationAlreadySentMsg = "this operation key was already sent with this call; approval_id, sent_at and answered say under which approval, when, and whether its server's answer was recorded"
	operationKeyReusedMsg   = "this operation key was already sent with another call"
	mcpCallSendFaultMsg     = "check or record an MCP call's send"
)

// errMCPCallSendsUnavailable is logged when a keyed call meets a server with no send record: no store
// that keeps one, or no run row read for the token.
var errMCPCallSendsUnavailable = errors.New("api: no MCP call send record on this server")

// mountInternalApprovalSettleRoutes registers the doors that settle an approval the run raised:
// wardyn-toolgate's own give-up signal, which closes the row it raised instead of leaving it PENDING for
// the sweep, and a keyed MCP call's send record.
func (s *Server) mountInternalApprovalSettleRoutes(r chi.Router) {
	r.Post("/internal/approvals/{id}/expire", s.handleInternalExpireApproval)
	r.Post("/internal/approvals/{id}/sent", s.handleInternalMCPCallSent)
}

// operationNamespace is the namespace of run's operation keys: its owner, and its correlation ID when it
// has one, else the run itself.
func operationNamespace(run types.AgentRun) (owner, scope string) {
	if run.CorrelationID != "" {
		return run.CreatedBy, "corr:" + run.CorrelationID
	}
	return run.CreatedBy, "run:" + run.ID.String()
}

// mcpCallSendsFor is the send record and the run row of r's token, and false when either is missing.
func (s *Server) mcpCallSendsFor(r *http.Request) (store.MCPCallSendStore, types.AgentRun, bool) {
	sends, ok := s.cfg.Store.(store.MCPCallSendStore)
	run, live := liveRunFromContext(r.Context())
	return sends, run, ok && live
}

// refuseSentOperation answers the raise of a keyed MCP call whose key was already sent in its run's
// namespace with that send (writeOperationSent), and reports true. A keyless call reads nothing and comes
// back false, as does a key never sent. A key the server cannot check is a 500: the call is neither asked
// about nor sent.
func (s *Server) refuseSentOperation(w http.ResponseWriter, r *http.Request, sc types.MCPCallScope) bool {
	if sc.OperationKey == "" {
		return false
	}
	sends, run, ok := s.mcpCallSendsFor(r)
	if !ok {
		writeServerError(w, r, mcpCallSendFaultMsg, errMCPCallSendsUnavailable)
		return true
	}
	owner, scope := operationNamespace(run)
	got, err := sends.GetMCPCallSend(r.Context(), owner, scope, sc.OperationKey)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return false
	case err != nil:
		writeServerError(w, r, mcpCallSendFaultMsg, err)
		return true
	}
	writeOperationSent(w, got, sc.CallSHA256)
	return true
}

// writeOperationSent answers 409 for a key already sent: operation_key_reused when it was sent with
// another call, which the answer names nothing of, else operation_already_sent with the approval it was
// sent under, when, and whether its server's answer was recorded.
func writeOperationSent(w http.ResponseWriter, got store.MCPCallSend, callSHA256 string) {
	if got.CallSHA256 != callSHA256 {
		writeErrorReason(w, http.StatusConflict, reasonOperationKeyReused, operationKeyReusedMsg)
		return
	}
	writeJSON(w, http.StatusConflict, types.MCPCallSentAnswer{Error: operationAlreadySentMsg, Reason: reasonOperationAlreadySent,
		ApprovalID: got.ApprovalID, SentAt: got.SentAt, Answered: got.AnsweredAt != nil})
}

// mcpCallSentRequest is a send record's body: answered false before the call is sent, true once its
// server answered.
type mcpCallSentRequest struct {
	Answered *bool `json:"answered"`
}

// handleInternalMCPCallSent is the run proxy's record of a keyed MCP call's send, made before it sends,
// for an APPROVED MCP call row of the token's own run that carries an operation key; any other approval
// is 404. The first record of a key in its namespace is a 200 naming it; any later one, this approval's
// own included, is a 409 naming the first (writeOperationSent), and the proxy then sends nothing.
// answered true stamps the record once the server answered: 204, or 404 when this approval recorded no
// send for this run.
func (s *Server) handleInternalMCPCallSent(w http.ResponseWriter, r *http.Request) {
	claims, err := claimsFromContext(r)
	if err != nil {
		writeErrorReason(w, http.StatusUnauthorized, reasonMissingRunClaims, "missing run claims")
		return
	}
	id, ok := parseIDParam(w, r, "id", "approval")
	if !ok {
		return
	}
	var body mcpCallSentRequest
	if err := types.DecodeOneJSON(http.MaxBytesReader(w, r.Body, maxSidecarBody), &body, true); err != nil || body.Answered == nil {
		writeErrorReason(w, http.StatusBadRequest, reasonInternalApprovalRequestInvalid, "invalid approval request")
		return
	}
	ap, err := s.cfg.Approvals.Get(r.Context(), id)
	if notFoundIf(w, err, "approval", reasonApprovalNotFound) {
		return
	}
	if err != nil {
		writeServerError(w, r, "get approval", err)
		return
	}
	sc, shaped := mcpCallScopeShaped(ap.RequestedScope)
	if ap.RunID != claims.RunID || ap.State != types.ApprovalApproved || !isMCPCallRow(ap) || !shaped || !types.ValidOperationKey(sc.OperationKey) {
		// Do not confirm another run's approval, or one with no send to record.
		writeErrorReason(w, http.StatusNotFound, reasonApprovalNotFound, "approval not found")
		return
	}
	sends, run, ok := s.mcpCallSendsFor(r)
	if !ok {
		writeServerError(w, r, mcpCallSendFaultMsg, errMCPCallSendsUnavailable)
		return
	}
	if *body.Answered {
		found, err := sends.MarkMCPCallAnswered(r.Context(), id, claims.RunID)
		switch {
		case err != nil:
			writeServerError(w, r, mcpCallSendFaultMsg, err)
		case !found:
			writeErrorReason(w, http.StatusNotFound, reasonApprovalNotFound, "approval not found")
		default:
			writeJSON(w, http.StatusNoContent, nil)
		}
		return
	}
	owner, scope := operationNamespace(run)
	got, made, err := sends.RecordMCPCallSend(r.Context(), store.MCPCallSend{Owner: owner, OperationScope: scope,
		OperationKey: sc.OperationKey, ApprovalID: id, RunID: claims.RunID, CallSHA256: sc.CallSHA256})
	switch {
	case err != nil:
		writeServerError(w, r, mcpCallSendFaultMsg, err)
	case !made:
		writeOperationSent(w, got, sc.CallSHA256)
	default:
		writeJSON(w, http.StatusOK, types.MCPCallSentAnswer{ApprovalID: got.ApprovalID, SentAt: got.SentAt})
	}
}
