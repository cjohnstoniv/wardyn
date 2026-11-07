// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A remote MCP tool whose rule is approve_each is enforced here, for every harness and every
// process that reaches the broker: the call waits on its open request for a person to approve
// that call with its exact arguments, then the broker sends exactly what was approved, once.
// The approval is keyed by types.MCPCallKey (server, RAW tool name, canonical arguments and
// extra params), so two tools that share a display name, or a call with other arguments, never
// spend each other's approval. The spend table lives in this proxy only: a new proxy asks again.

const (
	// maxMCPCallsWaiting bounds calls held open at once; each holds a goroutine and a connection.
	maxMCPCallsWaiting = 16
	// maxMCPCallKeys bounds the distinct calls one run's proxy remembers.
	maxMCPCallKeys = 1024
	// defaultMCPCallWindow is the window, in seconds, of a harness with no entry of its own:
	// under Codex CLI's 60-second default tool timeout.
	defaultMCPCallWindow = 45

	mcpBatchCode    = -32002
	mcpBatchMessage = "Send this tool call on its own: it needs a person's approval"
)

// mcpCallWindows are the default windows by harness, in seconds: Claude Code's idle timer for a
// network MCP server is 300 s, so it waits 240; Codex CLI times a tool out at 60 s.
var mcpCallWindows = map[string]int{"claude-code": 240, "codex-cli": defaultMCPCallWindow}

// mcpApproveEachSecond is what one second of a window lasts. A var only so a test of a window
// need not wait real seconds.
var mcpApproveEachSecond = time.Second

// What the agent reads when its call was not sent.
const (
	mcpCallWaitingText  = "Wardyn is waiting for a person to approve this call (request %s). It was not sent. Call the tool again with the same arguments to keep waiting, or to send it once it is approved. A call with different arguments asks again."
	mcpCallDeclinedText = "A person declined this call, so Wardyn did not send it. The same call is refused for the rest of this run; a call with different arguments asks again."
	mcpCallClosedText   = "The request for this call closed without a decision, so Wardyn did not send it. Call the tool again to ask again."
	mcpCallTooLargeText = "This call's arguments are larger than Wardyn can show a person (32 KiB), so it was not sent."
	mcpCallBusyText     = "Too many calls are already waiting for a person in this run. Try again later."
	mcpCallKeysText     = "This run has asked about too many different calls; no more will be asked about."
	mcpCallRepeatText   = "This call repeats a key in its arguments, so Wardyn cannot show it exactly. It was not sent."
	mcpCallNoAskText    = "Wardyn could not ask a person about this call, so it was not sent. Call the tool again to ask again."
	mcpCallRefusedText  = "Wardyn refused the request for a person's approval of this call (%d), so it was not sent."
)

// setApproveEach takes the sealed config's windows; nil keeps the defaults.
func (b *mcpBroker) setApproveEach(c *MCPApproveEachConfig) {
	if b == nil || c == nil {
		return
	}
	b.windows = c.Windows
}

// window is how long, in seconds, harness's call waits for a person.
func (b *mcpBroker) window(harness string) int {
	if w, ok := b.windows[harness]; ok {
		return w
	}
	if w, ok := mcpCallWindows[harness]; ok {
		return w
	}
	return defaultMCPCallWindow
}

// mcpCalls is the per-proxy approve_each state. The zero value is ready to use.
type mcpCalls struct {
	mu     sync.Mutex
	byKey  map[string]*mcpCallKey
	active int
}

// mcpCallKey is one call key's state: its current approval, if any, and the approvals already
// spent on it. A spent approval is never spent again: its id stays in spent while any call of
// the key is still waiting, since that call may yet read a delayed answer naming it. Once no call
// of the key waits, no answer can arrive, so the set is dropped.
type mcpCallKey struct {
	id    uuid.UUID     // the current approval; uuid.Nil when there is none
	state approvalState // what id came to: apPending, apApproved or apDenied
	live  int           // calls of this key holding a waiting slot
	spent map[uuid.UUID]bool
}

// admit files key and takes a waiting slot. A call whose current approval is approved spends it
// here, in the same step, and comes back apApproved to send; a declined one comes back apDenied.
// Neither takes a slot. One that cannot wait comes back with why. A pending call comes back with
// the approval to rejoin; one with no current approval with none.
func (c *mcpCalls) admit(key string) (uuid.UUID, approvalState, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	k := c.byKey[key]
	switch {
	case k != nil && k.id != uuid.Nil && k.state == apApproved:
		id := k.id
		c.spend(key, k)
		return id, apApproved, ""
	case k != nil && k.id != uuid.Nil && k.state == apDenied:
		return k.id, k.state, ""
	case k == nil && len(c.byKey) >= maxMCPCallKeys:
		return uuid.Nil, apNone, mcpCallKeysText
	case c.active >= maxMCPCallsWaiting:
		return uuid.Nil, apNone, mcpCallBusyText
	}
	if k == nil {
		if c.byKey == nil {
			c.byKey = map[string]*mcpCallKey{}
		}
		k = &mcpCallKey{}
		c.byKey[key] = k
	}
	c.active++
	k.live++
	return k.id, apPending, ""
}

// leave gives back key's waiting slot.
func (c *mcpCalls) leave(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.active--
	if k := c.byKey[key]; k != nil {
		k.live--
		c.tidy(key, k)
	}
}

// tidy drops the spent set once no call of key waits, and the key once nothing is left of it.
func (c *mcpCalls) tidy(key string, k *mcpCallKey) {
	if k.live > 0 {
		return
	}
	k.spent = nil
	if k.id == uuid.Nil {
		delete(c.byKey, key)
	}
}

// claim spends approval id on key, which the caller has read is approved: true for exactly one
// caller, which then sends the call. Only key's current approval can be claimed, so an answer
// about any other id (spent, superseded or never installed) sends nothing.
func (c *mcpCalls) claim(key string, id uuid.UUID) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	k := c.byKey[key]
	if k == nil || id == uuid.Nil || k.id != id || k.state == apDenied {
		return false
	}
	c.spend(key, k)
	return true
}

// spend uses key's current approval: it is remembered as spent while a call of key waits, and
// key has no current approval after it. The caller holds c.mu.
func (c *mcpCalls) spend(key string, k *mcpCallKey) {
	if k.spent == nil {
		k.spent = map[uuid.UUID]bool{}
	}
	k.spent[k.id] = true
	k.id, k.state = uuid.Nil, apNone
	c.tidy(key, k)
}

// install files a newly raised approval and says which approval the caller waits on: the new one
// when key has no current approval, else the current one (a request for this exact call is
// already open), and uuid.Nil when id was already spent (a raise answered late with a row that was
// since used), so the caller asks again.
func (c *mcpCalls) install(key string, id uuid.UUID) uuid.UUID {
	c.mu.Lock()
	defer c.mu.Unlock()
	k := c.byKey[key]
	switch {
	case k == nil || k.spent[id]:
		return uuid.Nil
	case k.id == uuid.Nil:
		k.id, k.state = id, apPending
	}
	return k.id
}

// update stores what key's current approval came to, and only that one: an answer about any
// other id changes nothing. apNone (closed undecided) clears it, so the next call asks again.
func (c *mcpCalls) update(key string, id uuid.UUID, state approvalState) {
	c.mu.Lock()
	defer c.mu.Unlock()
	k := c.byKey[key]
	if k == nil || k.id != id || id == uuid.Nil {
		return
	}
	if state == apNone {
		k.id = uuid.Nil
	}
	k.state = state
	c.tidy(key, k)
}

// approveEachCall is one approve_each call read for asking and sending.
type approveEachCall struct {
	id    json.RawMessage
	scope types.MCPCallScope
	body  []byte // the request the broker sends once the call is approved
}

// holdApproveEach answers or forwards a POST holding an approve_each call, and reports whether
// it did. A body with none is left to forward unchanged.
func (x *mcpExchange) holdApproveEach(w http.ResponseWriter, body []byte) bool {
	msgs, batch, err := parseMCPMessages(body)
	if err != nil || !slices.ContainsFunc(msgs, func(m mcpMessage) bool {
		return m.tool != "" && x.p.mcpCallEffect(x.harness, x.server.key, m.tool) == types.ToolApproveEach
	}) {
		return false
	}
	if batch || msgs[0].id == nil {
		// One call per request, so its answer is the person's; a notification has no answer.
		x.record(egress.Deny, ruleSourceMCPApprovalRefused, uuid.Nil, "an approve_each call sent in a batch or as a notification")
		var out []map[string]any
		for _, m := range msgs {
			if m.id != nil {
				out = append(out, jsonRPCError(m.id, mcpBatchCode, mcpBatchMessage))
			}
		}
		if len(out) == 0 {
			w.WriteHeader(http.StatusAccepted)
		} else {
			writeJSONRPC(w, out)
		}
		return true
	}
	call, text, err := x.readCall(msgs[0])
	switch {
	case err != nil:
		x.record(egress.Deny, ruleSourceMCPApprovalRefused, uuid.Nil, "an approve_each call Wardyn could not read")
		writeJSONRPC(w, jsonRPCError(msgs[0].id, -32700, "Wardyn could not read this MCP request: "+err.Error()))
	case text != "":
		x.record(egress.Deny, ruleSourceMCPApprovalRefused, uuid.Nil, text)
		writeToolError(w, msgs[0].id, text)
	default:
		x.awaitCall(w, call)
	}
	return true
}

// readCall builds the call's scope and the body sent once it is approved. The approved object is
// params without name and _meta: the arguments (absent reads as {}) and every other key (extra),
// each canonical. text is the agent's sentence for a call that cannot be shown exactly.
func (x *mcpExchange) readCall(m mcpMessage) (approveEachCall, string, error) {
	// A repeated key at any depth: two parsers could read the call two ways.
	if _, err := types.CanonicalJSON(m.params); err != nil {
		if errors.Is(err, types.ErrCanonicalRepeatedKey) {
			return approveEachCall{}, mcpCallRepeatText, nil
		}
		return approveEachCall{}, "", err
	}
	params, err := exactKeys(m.params, "name", "arguments", "_meta")
	if err != nil {
		return approveEachCall{}, "", err
	}
	var progress json.RawMessage
	if meta, ok := params["_meta"]; ok && string(meta) != "null" {
		mo, err := exactKeys(meta, "progressToken")
		if err != nil {
			return approveEachCall{}, "", fmt.Errorf("_meta: %w", err)
		}
		progress = mo["progressToken"]
	}
	args := params["arguments"]
	rest := map[string]json.RawMessage{}
	for k, v := range params {
		if k != "name" && k != "arguments" && k != "_meta" {
			rest[k] = v
		}
	}
	var extra json.RawMessage
	if len(rest) > 0 {
		if extra, err = json.Marshal(rest); err != nil {
			return approveEachCall{}, "", err
		}
	}
	key, err := types.MCPCallKey(x.server.key, m.tool, args, extra)
	if errors.Is(err, types.ErrMCPCallTooLarge) {
		return approveEachCall{}, mcpCallTooLargeText, nil
	} else if err != nil {
		return approveEachCall{}, "", err
	}
	scope := types.MCPCallScope{Tool: "mcp__" + x.server.key + "__" + claudeToolName(m.tool), MCPServer: x.server.key,
		MCPHost: x.server.host, CallSHA256: key}
	if shown := claudeToolName(m.tool); shown != m.tool {
		scope.MCPTool = m.tool
	}
	if len(x.p.harnessCreds) > 0 {
		scope.Harness = x.harness
	}
	if len(bytes.TrimSpace(args)) == 0 {
		args = json.RawMessage(`{}`)
	}
	canon := func(raw []byte) []byte { out, _ := types.CanonicalJSON(raw); return out } // checked above
	scope.Arguments = canon(args)
	if extra != nil {
		if ext := canon(extra); string(ext) != "{}" {
			scope.Extra = ext
		}
	}
	return approveEachCall{id: m.id, scope: scope, body: buildMCPCall(canon(m.id), canon(params["name"]), scope, canon(progress))}, "", nil
}

// buildMCPCall is the request sent for an approved call: the broker's own envelope around the
// canonical arguments and extra params the person saw, and of _meta only the progress token.
func buildMCPCall(id, name []byte, s types.MCPCallScope, progress []byte) []byte {
	var b bytes.Buffer
	b.WriteString(`{"jsonrpc":"2.0","id":`)
	b.Write(id)
	b.WriteString(`,"method":"tools/call","params":{"name":`)
	b.Write(name)
	b.WriteString(`,"arguments":`)
	b.Write(s.Arguments)
	if len(s.Extra) > 2 { // a canonical object: its members follow the arguments
		b.WriteByte(',')
		b.Write(s.Extra[1 : len(s.Extra)-1])
	}
	if len(progress) > 0 {
		b.WriteString(`,"_meta":{"progressToken":`)
		b.Write(progress)
		b.WriteByte('}')
	}
	b.WriteString(`}}`)
	return b.Bytes()
}

// awaitCall spends an approved call, refuses a declined one, or asks (or rejoins the request
// pending for it) and waits on the open request up to the harness's window.
func (x *mcpExchange) awaitCall(w http.ResponseWriter, c approveEachCall) {
	calls, key := &x.p.mcp.calls, c.scope.CallSHA256
	id, state, why := calls.admit(key)
	switch {
	case state == apApproved: // admit spent it for this call
		x.forwardApproved(w, c, id)
		return
	case state == apDenied:
		x.record(egress.Deny, ruleSourceMCPApprovalDenied, id, "a declined call, refused for the run")
		writeToolError(w, c.id, mcpCallDeclinedText)
		return
	case why != "":
		x.record(egress.Deny, ruleSourceMCPApprovalRefused, uuid.Nil, why)
		writeToolError(w, c.id, why)
		return
	}
	defer calls.leave(key)
	if x.p.approval == nil {
		x.record(egress.Deny, ruleSourceMCPApprovalRefused, uuid.Nil, "this proxy has no way to ask")
		writeToolError(w, c.id, mcpCallNoAskText)
		return
	}
	window := x.p.mcp.window(x.harness)
	c.scope.HoldSeconds = window
	ctx, cancel := context.WithTimeout(x.r.Context(), time.Duration(window)*mcpApproveEachSecond)
	defer cancel()
	if id != uuid.Nil {
		// A retry rejoining the pending request: the console reads it as held for this window too.
		x.raise(ctx, c.scope, id)
	} else if id = x.ask(ctx, w, c); id == uuid.Nil {
		return
	}
	tick := time.NewTicker(holdPollInterval)
	defer tick.Stop()
	for {
		// Polled at once as well as on each tick: a request decided while nobody waited is
		// answered without a delay.
		if ar, ok := x.p.approval.fetch(ctx, id); ok {
			switch ar.State {
			case types.ApprovalApproved:
				calls.update(key, id, apApproved)
				if calls.claim(key, id) {
					x.forwardApproved(w, c, id)
					return
				}
				// Another call spent it: this one asks again, in its own window.
				if id = x.ask(ctx, w, c); id == uuid.Nil {
					return
				}
			case types.ApprovalDenied:
				calls.update(key, id, apDenied)
				x.record(egress.Deny, ruleSourceMCPApprovalDenied, id, "declined by a person")
				writeToolError(w, c.id, mcpCallDeclinedText)
				return
			case types.ApprovalExpired, types.ApprovalCancelled:
				calls.update(key, id, apNone)
				x.record(egress.Deny, ruleSourceMCPApprovalClosed, id, "closed without a decision")
				writeToolError(w, c.id, mcpCallClosedText)
				return
			}
		}
		select {
		case <-ctx.Done():
			if x.r.Context().Err() != nil {
				return // the client went away; the request stays pending for a retry
			}
			x.record(egress.Pending, ruleSourceMCPApprovalWait, id, "waiting for a person")
			writeToolError(w, c.id, fmt.Sprintf(mcpCallWaitingText, id))
			return
		case <-tick.C:
		}
	}
}

// ask raises the call's approval and returns the one to wait on (install); uuid.Nil when it could
// not, the refusal already answered. A raise answered with an approval already spent asks once more.
func (x *mcpExchange) ask(ctx context.Context, w http.ResponseWriter, c approveEachCall) uuid.UUID {
	for range 2 {
		id, err := x.raise(ctx, c.scope, uuid.Nil)
		var refused *raiseRefusedError
		switch {
		case errors.As(err, &refused):
			x.record(egress.Deny, ruleSourceMCPApprovalRefused, uuid.Nil, fmt.Sprintf("the request for an approval was refused (%d)", refused.status))
			writeToolError(w, c.id, fmt.Sprintf(mcpCallRefusedText, refused.status))
			return uuid.Nil
		case err != nil:
			x.record(egress.Deny, ruleSourceMCPApprovalRefused, uuid.Nil, "the request for an approval could not be raised")
			writeToolError(w, c.id, mcpCallNoAskText)
			return uuid.Nil
		}
		if got := x.p.mcp.calls.install(c.scope.CallSHA256, id); got != uuid.Nil {
			return got
		}
	}
	x.record(egress.Deny, ruleSourceMCPApprovalRefused, uuid.Nil, "the request for an approval named one already used")
	writeToolError(w, c.id, mcpCallNoAskText)
	return uuid.Nil
}

// raise posts the call's tool_call approval, or rejoins the pending one when rejoin is set.
func (x *mcpExchange) raise(ctx context.Context, scope types.MCPCallScope, rejoin uuid.UUID) (uuid.UUID, error) {
	var rejoinID *uuid.UUID
	if rejoin != uuid.Nil {
		rejoinID = &rejoin
	}
	body, err := json.Marshal(struct {
		Kind           types.ApprovalKind `json:"kind"`
		RequestedScope types.MCPCallScope `json:"requested_scope"`
		RejoinID       *uuid.UUID         `json:"rejoin_id,omitempty"`
	}{types.ApprovalToolCall, scope, rejoinID})
	if err != nil {
		return uuid.Nil, err
	}
	return x.p.approval.raiseBytes(ctx, body)
}

// forwardApproved sends the approved call; its row names the approval.
func (x *mcpExchange) forwardApproved(w http.ResponseWriter, c approveEachCall, id uuid.UUID) {
	x.approvalID = &id
	x.forward(w, c.body)
}

// record writes one approve_each row.
func (x *mcpExchange) record(decision egress.Decision, ruleSource string, id uuid.UUID, cause string) {
	if x.p.sink == nil {
		return
	}
	d := decisionLog(x.req, decision, ruleSource)
	if id != uuid.Nil {
		d.ApprovalID = &id
	}
	d.Cause = cause
	x.p.sink.emit(d)
}

// writeToolError answers a call with a tool result marked isError, so the model reads why its
// call was not sent.
func writeToolError(w http.ResponseWriter, id json.RawMessage, text string) {
	writeJSONRPC(w, map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}}, "isError": true}})
}
