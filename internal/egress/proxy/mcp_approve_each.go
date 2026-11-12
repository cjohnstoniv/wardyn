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
	"strconv"
	"strings"
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
	// maxSeenTools and maxSeenToolName bound the raw tool names remembered per server.
	maxSeenTools    = 256
	maxSeenToolName = 256
	// maxMCPCallsWaiting bounds calls held open at once; each holds a goroutine and a connection.
	maxMCPCallsWaiting = 16
	// maxMCPCallKeys bounds the distinct calls one run's proxy remembers.
	maxMCPCallKeys = 1024
	// defaultMCPCallWindow is the window, in seconds, of a harness with no entry of its own:
	// under Codex CLI's 60-second default tool timeout.
	defaultMCPCallWindow = 45

	// Rule sources of a tool-approval route answer for an approve_each call, with no row of its own
	// except the approval a raise made.
	ruleSourceToolApproveEach           = "policy:tool-approve-each"
	ruleSourceToolApproveEachUnroutable = "policy:tool-approve-each-unroutable"
	ruleSourceToolApproveEachDeclined   = "policy:tool-approve-each-declined"

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
	mcpCallDeclinedText = "A person declined this call, so Wardyn did not send it. This request and identical retries are refused; a call with different arguments asks again."
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
	// seen is each server's raw tool names the broker has been called with (noteTool); the
	// gate reads it to tell which raw tool a display name stands for.
	seen map[string]*seenTools
}

// mcpCallKey is one call key's state: its current approval, if any, and the approvals already
// spent on it. A spent approval is never spent again: its id stays in spent while any call of
// the key is still waiting, since that call may yet read a delayed answer naming it. A gate call in
// flight counts the same: its raise may be answered late with an approval already spent. Once no
// call of the key waits and no gate call is in flight, no answer can arrive, so the set is dropped.
type mcpCallKey struct {
	id    uuid.UUID     // the current approval; uuid.Nil when there is none
	state approvalState // what id came to: apPending, apApproved or apDenied
	live  int           // calls of this key holding a waiting slot
	gates int           // gate calls of this key in flight (gateEnter), which hold no waiting slot
	spent map[uuid.UUID]bool
}

// admit files key and takes a waiting slot. A call whose current approval is approved spends it
// here, in the same step, and comes back apApproved to send; a declined one comes back apDenied.
// Neither takes a slot. One that cannot wait comes back with why. A pending call comes back with
// the approval to rejoin; one with no current approval with none.
func (c *mcpCalls) admit(key string) (uuid.UUID, approvalState, string) { return c.admitAs(key, true) }

// admitKept is admit for a table whose answers hold for the run (host rule holds, host_call_hold.go):
// an approved key is passed without being spent, so it stays approved.
func (c *mcpCalls) admitKept(key string) (uuid.UUID, approvalState, string) {
	return c.admitAs(key, false)
}

func (c *mcpCalls) admitAs(key string, spendApproved bool) (uuid.UUID, approvalState, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	k := c.byKey[key]
	switch {
	case k != nil && k.id != uuid.Nil && k.state == apApproved:
		id := k.id
		if spendApproved {
			c.spend(key, k)
		}
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

// tidy drops the spent set once no call of key waits or is in the gate, and the key once nothing is left of it.
func (c *mcpCalls) tidy(key string, k *mcpCallKey) {
	if k.live > 0 || k.gates > 0 {
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
	if err == nil {
		for _, m := range msgs {
			x.p.mcp.calls.noteTool(x.server.key, m.tool)
		}
	}
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
	scope, err := x.p.callScope(x.server, x.harness, m.tool, args, extra)
	if errors.Is(err, types.ErrMCPCallTooLarge) {
		return approveEachCall{}, mcpCallTooLargeText, nil
	} else if err != nil {
		return approveEachCall{}, "", err
	}
	canon := func(raw []byte) []byte { out, _ := types.CanonicalJSON(raw); return out } // checked above
	return approveEachCall{id: m.id, scope: scope, body: buildMCPCall(m.id, canon(params["name"]), scope, canon(progress))}, "", nil
}

// callScope is the scope of one call of srv's raw tool with the given arguments and extra params
// (absent arguments read as {}): the one the broker raises and the gate raises, so both hash to
// the same key. harness is named on a run whose agents each hold a credential.
func (p *Proxy) callScope(srv *mcpServer, harness, raw string, args, extra json.RawMessage) (types.MCPCallScope, error) {
	key, err := types.MCPCallKey(srv.key, raw, args, extra)
	if err != nil {
		return types.MCPCallScope{}, err
	}
	shown := claudeToolName(raw)
	scope := types.MCPCallScope{Tool: "mcp__" + srv.key + "__" + shown, MCPServer: srv.key, MCPHost: srv.host, CallSHA256: key}
	if shown != raw {
		scope.MCPTool = raw
	}
	if len(p.harnessCreds) > 0 {
		scope.Harness = harness
	}
	if len(bytes.TrimSpace(args)) == 0 {
		args = json.RawMessage(`{}`)
	}
	canon := func(b []byte) []byte { out, _ := types.CanonicalJSON(b); return out } // checked by MCPCallKey
	scope.Arguments = canon(args)
	if extra != nil {
		if ext := canon(extra); string(ext) != "{}" {
			scope.Extra = ext
		}
	}
	return scope, nil
}

// buildMCPCall is the request sent for an approved call: the broker's own envelope around the
// canonical arguments and extra params the person saw, and of _meta only the progress token. The id is the
// client's own text (1.0 stays 1.0), so the server answers with the id the client sent.
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
	return x.p.raiseMCPCall(ctx, scope, rejoin)
}

// raiseMCPCall is the one raise of an MCP call's approval, for the broker and the gate alike.
func (p *Proxy) raiseMCPCall(ctx context.Context, scope types.MCPCallScope, rejoin uuid.UUID) (uuid.UUID, error) {
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
	return p.approval.raiseBytes(ctx, body)
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

// seenTools are the raw tool names one server's calls have carried, up to maxSeenTools of them.
// Past that it is full and says nothing, so the gate leaves every such question to the broker.
type seenTools struct {
	names map[string]bool
	full  bool
}

// noteTool remembers raw as a tool name server key was called with.
func (c *mcpCalls) noteTool(server, raw string) {
	if raw == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	st := c.seen[server]
	if st == nil {
		if c.seen == nil {
			c.seen = map[string]*seenTools{}
		}
		st = &seenTools{names: map[string]bool{}}
		c.seen[server] = st
	}
	switch {
	case st.names[raw]:
	case len(raw) > maxSeenToolName || len(st.names) >= maxSeenTools:
		st.full = true
	default:
		st.names[raw] = true
	}
}

// rawTool is the raw tool name of server that shown stands for, and false when it cannot be
// told. It is the one raw name seen for it; with none seen, shown itself (a name that is its own
// display form); with two or more, or a full list, none: the broker raises for the name that
// arrives.
func (c *mcpCalls) rawTool(server, shown string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var found []string
	if st := c.seen[server]; st != nil {
		if st.full {
			return "", false
		}
		for raw := range st.names {
			if claudeToolName(raw) == shown {
				found = append(found, raw)
			}
		}
	}
	switch {
	case len(found) == 1:
		return found[0], true
	case len(found) == 0 && shown != "" && claudeToolName(shown) == shown:
		return shown, true
	}
	return "", false
}

// gateEnter files key for a gate call before it looks or raises, and holds it until gateLeave:
// the key and its spent set stay while the call is in flight, so a raise answered late with an
// approval the broker already used finds it spent. It takes no waiting slot, since a gate call
// holds no connection open. False when key is new and the table has no room for it.
func (c *mcpCalls) gateEnter(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	k := c.byKey[key]
	if k == nil {
		if len(c.byKey) >= maxMCPCallKeys {
			return false
		}
		if c.byKey == nil {
			c.byKey = map[string]*mcpCallKey{}
		}
		k = &mcpCallKey{}
		c.byKey[key] = k
	}
	k.gates++
	return true
}

// gateLeave ends a gateEnter.
func (c *mcpCalls) gateLeave(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if k := c.byKey[key]; k != nil {
		k.gates--
		c.tidy(key, k)
	}
}

// look is key's current approval as the gate reads it.
func (c *mcpCalls) look(key string) (id uuid.UUID, state approvalState) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if k := c.byKey[key]; k != nil {
		return k.id, k.state
	}
	return uuid.Nil, apNone
}

// file records an approval a gate call raised and returns the one to answer: the new one when key
// has no current approval, else the current one. uuid.Nil when id was already spent. The caller
// is between gateEnter and gateLeave, so key and its spent set are there.
func (c *mcpCalls) file(key string, id uuid.UUID) uuid.UUID {
	c.mu.Lock()
	defer c.mu.Unlock()
	k := c.byKey[key]
	if k == nil || k.spent[id] {
		return uuid.Nil
	}
	if k.id == uuid.Nil {
		k.id, k.state = id, apPending
	}
	return k.id
}

// splitMCPTool reads a display name mcp__<server>__<tool>. A server key never holds two
// underscores in a row, so the first pair ends it.
func splitMCPTool(name string) (server, shown string, ok bool) {
	rest, ok := strings.CutPrefix(name, "mcp__")
	i := strings.Index(rest, "__")
	if !ok || i < 1 {
		return "", "", false
	}
	return rest[:i], rest[i+2:], true
}

// gateApproveEach answers the permission tool for a call whose rule is approve_each. The gated
// lane then waits as long as it does for any tool, and the broker spends the approval when the
// call arrives with the same arguments: one prompt. Nothing the gate answers is looser than the
// broker, which enforces the rule on every call whatever the gate said.
//   - A tool of no attached remote server: DENIED, since nothing could ask with the arguments.
//   - No usable input, a raw tool the display name does not settle, or a call the broker will
//     refuse: APPROVED with no row, and the broker asks.
//   - A declined call: DENIED for the rest of the run.
//   - Else the call's approval is raised (or the open one answered) and its id handed back.
func (p *Proxy) gateApproveEach(w http.ResponseWriter, r *http.Request, tool, matched string, input json.RawMessage) {
	server, shown, ok := splitMCPTool(tool)
	var srv *mcpServer
	if ok {
		srv = p.mcp.server(server)
	}
	if srv == nil {
		p.emitGateDecision(r, egress.Deny, ruleSourceToolApproveEachUnroutable, tool, matched, nil)
		writeToolDecision(w, "DENIED")
		return
	}
	passOn := func() {
		p.emitGateDecision(r, egress.Allow, ruleSourceToolApproveEach, tool, matched, nil)
		writeToolDecision(w, "APPROVED")
	}
	scope, ok := p.gateScope(srv, r, shown, input)
	if !ok || p.approval == nil {
		passOn()
		return
	}
	calls, key := &p.mcp.calls, scope.CallSHA256
	if !calls.gateEnter(key) {
		passOn() // no room to remember another call: the broker refuses or asks
		return
	}
	defer calls.gateLeave(key)
	id, state := calls.look(key)
	if id != uuid.Nil {
		// A person may have decided since: learn it, so a declined call is refused here and a
		// closed one asks again.
		if ar, ok := p.approval.fetch(r.Context(), id); ok {
			switch ar.State {
			case types.ApprovalApproved:
				calls.update(key, id, apApproved)
			case types.ApprovalDenied:
				calls.update(key, id, apDenied)
			case types.ApprovalExpired, types.ApprovalCancelled:
				calls.update(key, id, apNone)
			}
		}
		id, state = calls.look(key)
	}
	switch {
	case id != uuid.Nil && state == apDenied:
		p.emitGateDecision(r, egress.Deny, ruleSourceToolApproveEachDeclined, tool, matched, &id)
		writeToolDecision(w, "DENIED")
		return
	case id != uuid.Nil:
		writeGateRow(w, http.StatusOK, id)
		return
	}
	id, err := p.raiseMCPCall(r.Context(), scope, uuid.Nil)
	var refused *raiseRefusedError
	switch {
	case errors.As(err, &refused):
		p.emitGateDecision(r, egress.Deny, ruleSourceMCPApprovalRefused, tool, matched, nil)
		writeToolDecision(w, "DENIED")
		return
	case err != nil:
		p.httpError(w, "raise tool approval", err, http.StatusBadGateway)
		return
	}
	if id = calls.file(key, id); id == uuid.Nil {
		passOn()
		return
	}
	p.emitGateDecision(r, egress.Allow, ruleSourceApprovals, tool, matched, &id)
	writeGateRow(w, http.StatusCreated, id)
}

// gateScope is the scope of the call the permission tool asks about, and false when the gate
// cannot build the one the broker will: no object input, a raw tool the display name does not
// settle, or arguments the broker cannot show (a repeated key, over 32 KiB).
func (p *Proxy) gateScope(srv *mcpServer, r *http.Request, shown string, input json.RawMessage) (types.MCPCallScope, bool) {
	input = bytes.TrimSpace(input)
	if len(input) == 0 || input[0] != '{' {
		return types.MCPCallScope{}, false
	}
	raw, ok := p.mcp.calls.rawTool(srv.key, shown)
	if !ok {
		return types.MCPCallScope{}, false
	}
	scope, err := p.callScope(srv, p.harnessOf(r), raw, input, nil)
	return scope, err == nil
}

// writeGateRow answers the toolgate with the approval to poll.
func writeGateRow(w http.ResponseWriter, status int, id uuid.UUID) {
	writeJSONStatus(w, status, "application/json", map[string]string{"id": id.String(), "state": "PENDING"})
}

// emitGateDecision is emitToolDecision with the approval a row stands behind.
func (p *Proxy) emitGateDecision(r *http.Request, decision egress.Decision, ruleSource, tool, matched string, approvalID *uuid.UUID) {
	if p.sink == nil {
		return
	}
	host, port, _ := hostPortFromURL(p.controlPlaneURL)
	log := decisionLog(p.reqOf(r, host, port), decision, ruleSource)
	log.Cause = "tool " + strconv.Quote(tool)
	log.MatchedRule = matched
	log.ApprovalID = approvalID
	p.sink.emit(log)
}
