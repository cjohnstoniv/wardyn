// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// An approve_each call may carry an operation key, params._meta[types.MCPOperationKeyMeta]: the caller's
// own name for the operation the call performs. The key rides the raised scope
// (types.MCPCallScope.OperationKey), outside the call's hash, and is never sent to the server. A keyed
// call is sent only once the control plane has recorded its send (forwardKeyed), and the control plane
// answers any later call with that key in the run's namespace with that send, or as a key used for
// another call, for 24 hours from the send (types.MCPCallSendWindow), so neither a retry nor a new proxy
// sends it twice within them. This proxy holds no time for a send: the window is the control plane's.

// What the agent reads for a keyed call that was not sent.
const (
	mcpCallOperationKeyText   = mcpCallRepeatText
	mcpCallKeyReusedText      = mcpCallNoAskText
	mcpCallSendUnrecordedText = mcpCallNoAskText
)

// operationSentText is what the agent reads for a call whose operation was already sent as e says.
func operationSentText(e *operationSentError) string { return mcpCallNoAskText }

// operationKeyOf reads a call's operation key from its _meta member; false when it is not a JSON string
// that types.ValidOperationKey accepts.
func operationKeyOf(raw json.RawMessage) (string, bool) {
	var key string
	if json.Unmarshal(raw, &key) != nil || !types.ValidOperationKey(key) {
		return "", false
	}
	return key, true
}

// mcpTableKey is the key a call is filed under in this proxy's table: its hash, joined with its
// operation key when it has one, so two operations that make the same call each get their own approval.
func mcpTableKey(s types.MCPCallScope) string {
	if s.OperationKey == "" {
		return s.CallSHA256
	}
	return s.CallSHA256 + "|" + s.OperationKey
}

// operationSentError is the control plane's 409 for a keyed call: its key was already sent in its
// namespace, with another call (reused), or with this one under approvalID at sentAt, answered once its
// server's answer was recorded.
type operationSentError struct {
	reused     bool
	approvalID uuid.UUID
	sentAt     time.Time
	answered   bool
}

func (e *operationSentError) Error() string {
	if e.reused {
		return "operation key already sent with another call"
	}
	return "operation already sent under approval " + e.approvalID.String()
}

// operationConflict reads a 409's body: an operationSentError when it is one of a keyed call's two
// conflicts, else an error naming what and the status, as any other unexpected status reads.
func operationConflict(body io.Reader, what string) error {
	var a types.MCPCallSentAnswer
	if json.NewDecoder(io.LimitReader(body, 64<<10)).Decode(&a) == nil {
		switch {
		case a.Reason == types.MCPOperationKeyReused:
			return &operationSentError{reused: true}
		case a.Reason == types.MCPOperationAlreadySent && a.ApprovalID != uuid.Nil:
			return &operationSentError{approvalID: a.ApprovalID, sentAt: a.SentAt, answered: a.Answered}
		}
	}
	return fmt.Errorf("%s: status %d", what, http.StatusConflict)
}

// markSent posts approval id's send record. Before the send (answered false) only a 200 is a record, and
// a 409 names the send that holds the key; once the server answered (answered true) the stamp is a 204.
func (a *approvalClient) markSent(ctx context.Context, id uuid.UUID, answered bool) error {
	if a == nil {
		return errors.New("record send: no control plane")
	}
	body, want := `{"answered":false}`, http.StatusOK
	if answered {
		body, want = `{"answered":true}`, http.StatusNoContent
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		controlPlaneEndpoint(a.base, "/api/v1/internal/approvals/"+id.String()+"/sent"), strings.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+a.token.Get())
	resp, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("record send: %w", err)
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case want:
		return nil
	case http.StatusConflict:
		return operationConflict(resp.Body, "record send")
	default:
		return fmt.Errorf("record send: status %d", resp.StatusCode)
	}
}

// forwardKeyed sends an approved keyed call only once the control plane has recorded its send. A key
// already sent is answered with that send (refuseSent), and any other failure sends nothing. Once the
// server answered, with any status, the record is stamped answered; that stamp is best-effort.
func (x *mcpExchange) forwardKeyed(w http.ResponseWriter, c approveEachCall, id uuid.UUID) {
	ctx, cancel := context.WithTimeout(x.r.Context(), approvalCallTimeout)
	err := x.p.approval.markSent(ctx, id, false)
	cancel()
	var sent *operationSentError
	switch {
	case errors.As(err, &sent):
		x.refuseSent(w, c, sent)
		return
	case err != nil:
		x.record(egress.Deny, ruleSourceMCPApprovalRefused, id, "the send could not be recorded")
		writeToolError(w, c.id, mcpCallSendUnrecordedText)
		return
	}
	x.approvalID = &id
	x.forward(w, c.body)
	if x.upstreamAnswered {
		// The agent's answer is written; a stamp lost here leaves the send reading as unanswered.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(x.r.Context()), approvalCallTimeout)
		_ = x.p.approval.markSent(ctx, id, true)
		cancel()
	}
}

// refuseSent answers a keyed call whose key the control plane already has a send for: refused when that
// send was another call's, else told when this call was sent and under which approval. Neither is sent.
func (x *mcpExchange) refuseSent(w http.ResponseWriter, c approveEachCall, e *operationSentError) {
	if e.reused {
		x.record(egress.Deny, ruleSourceMCPApprovalRefused, uuid.Nil, "an operation key already used for another call")
		writeToolError(w, c.id, mcpCallKeyReusedText)
		return
	}
	x.record(egress.Deny, ruleSourceMCPApprovalSent, e.approvalID, "an operation already sent under this approval")
	writeToolError(w, c.id, operationSentText(e))
}
