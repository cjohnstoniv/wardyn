// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The tool list filter: on an exchange where the harness's rules could block one
// of the server's tools, every tool list the server answers (a JSON-RPC message
// with a result holding a tools array, alone, in a batch, or as an event of a
// stream) leaves out the tools whose calls refuseBlockedCalls refuses. It is on
// by default with no switch: it hides only tools the broker already refuses to
// call, and an exchange with no rule that could block a tool of the server
// relays the server's bytes untouched.

const (
	// mcpListUnreadableCode and mcpListUnreadableMessage replace a tool list the
	// broker cannot check, so a blocked tool's name and description never reach
	// the agent through a list the broker could not read.
	mcpListUnreadableCode    = -32002
	mcpListUnreadableMessage = "Wardyn could not check this tool list against this run's tool rules"
	mcpListTooLargeMessage   = "Wardyn ended this response: one message from this MCP server is larger than 10 MiB"
)

// mcpListFilterActive reports whether harness's rules could block some tool of
// the server key names: its set's default blocks, or the run-wide "*" does, or a
// blocking rule names a tool under one of the server's prefixes. Not active, the
// exchange is relayed exactly as before.
func (p *Proxy) mcpListFilterActive(harness, key string) bool {
	blocks := func(e types.ToolEffect) bool {
		return types.ToolEffectStrictness(e) >= types.ToolEffectStrictness(types.ToolDeny)
	}
	onServer := func(tool string) bool {
		for _, prefix := range MCPToolPrefixes(key) {
			if strings.HasPrefix(tool, prefix) {
				return true
			}
		}
		return false
	}
	if len(p.harnessRules) > 0 {
		for _, s := range p.harnessRules {
			if s.Harness != harness {
				continue
			}
			if blocks(s.Default) {
				return true
			}
			for _, r := range s.Rules {
				if blocks(r.Effect) && onServer(r.Tool) {
					return true
				}
			}
		}
		return false
	}
	if p.policy == nil {
		return false
	}
	for tool, e := range p.policy.toolRules {
		if blocks(e) && (tool == "*" || onServer(tool)) {
			return true
		}
	}
	return false
}

// listRow records one tool list decision on the exchange.
func (x *mcpExchange) listRow(decision egress.Decision, ruleSource, cause, callID string) {
	if x.p.sink == nil {
		return
	}
	log := decisionLog(x.req, decision, ruleSource)
	log.Cause, log.GatewayCallID = cause, callID
	x.p.sink.emit(log)
}

// filterToolLists puts the tool list filter on an active exchange's answer, and
// reports whether the relay may go on. Only JSON and event streams are read; an
// answer of any other media type is replaced with the JSON-RPC error unless it
// has no body (a 202 or 204 keeps its status), since a client could still parse
// a list from it. An encoded
// answer is refused: the broker asked for identity bytes and cannot read others.
// A JSON answer is read whole here, so one over 10 MiB can still be answered 502;
// an event stream is filtered one event at a time as the relay reads it.
func (x *mcpExchange) filterToolLists(w http.ResponseWriter, resp *http.Response, callID string) bool {
	mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if mediaType != "application/json" && mediaType != "text/event-stream" {
		if noBody(resp) {
			return true
		}
		x.listRow(egress.Deny, ruleSourceMCPListUnreadable, "an answer that is neither JSON nor an event stream", callID)
		msg, _ := json.Marshal(jsonRPCError(nil, mcpListUnreadableCode, mcpListUnreadableMessage))
		resp.Header.Set("Content-Type", "application/json")
		resp.Header.Del("Content-Length")
		resp.Header.Del("Content-Encoding")
		resp.Body = struct {
			io.Reader
			io.Closer
		}{bytes.NewReader(msg), resp.Body}
		return true
	}
	refuse := func(cause string) bool {
		x.listRow(egress.Deny, ruleSourceMCPListUnreadable, cause, callID)
		http.Error(w, "Wardyn could not check this MCP answer against this run's tool rules", http.StatusBadGateway)
		return false
	}
	if enc := strings.TrimSpace(resp.Header.Get("Content-Encoding")); enc != "" && !strings.EqualFold(enc, "identity") {
		return refuse("the answer was encoded")
	}
	resp.Header.Del("Content-Length")
	f := &mcpListFilter{
		hidden: func(tool string) bool { return x.p.mcpCallEffect(x.harness, x.server.key, tool) == types.ToolDeny },
		// Counts only, in the row and the log: a tool name is server text, and this
		// runs before the relay masks held credentials, so a name could echo one.
		filtered: func(hidden, total int) {
			x.listRow(egress.Allow, ruleSourceMCPListFiltered, fmt.Sprintf("hid %d of %d tools", hidden, total), callID)
			slog.InfoContext(x.r.Context(), "wardyn-proxy: MCP tool list filtered by the run's tool rules",
				slog.String("run_id", x.p.runID.String()), slog.String("mcp_server", x.server.key),
				slog.String("method", x.req.Method), slog.Int("hidden", hidden), slog.Int("total", total))
		},
		unreadable: func(cause string) { x.listRow(egress.Deny, ruleSourceMCPListUnreadable, cause, callID) },
	}
	if mediaType == "text/event-stream" {
		resp.Body = &mcpSSEFilter{src: resp.Body, br: bufio.NewReader(resp.Body), f: f, start: true}
		return true
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBrokeredBody+1))
	switch {
	case err != nil:
		return refuse("the answer could not be read")
	case len(body) > maxBrokeredBody:
		return refuse("one message was larger than 10 MiB")
	}
	out, _ := f.document(body)
	resp.Body = struct {
		io.Reader
		io.Closer
	}{bytes.NewReader(out), resp.Body}
	return true
}

// noBody reports whether resp carries no body at all. It reads at most one byte,
// which is dropped: an answer with a body is replaced whole.
func noBody(resp *http.Response) bool {
	if resp.ContentLength == 0 || resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusNotModified {
		return true
	}
	var b [1]byte
	n, err := io.ReadFull(resp.Body, b[:])
	return n == 0 && errors.Is(err, io.EOF)
}

// mcpListFilter rewrites the tool lists in one JSON document.
type mcpListFilter struct {
	hidden     func(tool string) bool
	filtered   func(hidden, total int)
	unreadable func(cause string)
}

// document filters one JSON document, a message or a batch, and reports whether
// it changed; an unchanged document is returned as the same bytes. A document
// that is not JSON could be a list read leniently, so it is replaced.
func (f *mcpListFilter) document(doc []byte) ([]byte, bool) {
	trimmed := bytes.TrimSpace(doc)
	if len(trimmed) == 0 {
		return doc, false
	}
	if !json.Valid(trimmed) {
		return f.refuse(nil, "the answer is not JSON"), true
	}
	if trimmed[0] != '[' {
		if out, changed := f.message(trimmed); changed {
			return out, true
		}
		return doc, false
	}
	var elems []json.RawMessage
	_ = json.Unmarshal(trimmed, &elems) // valid JSON that starts with '[' is an array
	parts := make([][]byte, len(elems))
	changed := false
	for i, e := range elems {
		parts[i] = e
		if out, ok := f.message(e); ok {
			parts[i], changed = out, true
		}
	}
	if !changed {
		return doc, false
	}
	return jsonArray(parts), true
}

// message rewrites one message when it is a tool list, and reports whether it
// did. A message is a list by its shape (a result object holding tools, and no
// method), never by the id of a request, since an answer may come on a resumed
// stream. Keys are matched as refuseBlockedCalls matches them: a message or
// result that spells one of its keys twice or in another case is replaced with
// an error carrying its id; an entry whose name is not one plain string is left
// out. Every other key is kept as the server wrote it.
func (f *mcpListFilter) message(raw json.RawMessage) ([]byte, bool) {
	obj, err := exactKeys(raw, "result", "method", "id", "error")
	if err != nil {
		return f.refuse(raw, "a message with an ambiguous key, or not an object"), true
	}
	result, ok := obj["result"]
	if _, isRequest := obj["method"]; isRequest {
		if ok {
			// Both a request and an answer: a client could read it as either.
			return f.refuse(raw, "a message with both a method and a result"), true
		}
		return nil, false
	}
	if t := bytes.TrimSpace(result); !ok || len(t) == 0 || t[0] != '{' {
		return nil, false
	}
	res, err := exactKeys(result, "tools", "nextCursor")
	if err != nil {
		return f.refuse(raw, "a tool list with an ambiguous key"), true
	}
	tools, ok := res["tools"]
	if !ok {
		return nil, false
	}
	var entries []json.RawMessage
	if t := bytes.TrimSpace(tools); len(t) == 0 || t[0] != '[' || json.Unmarshal(t, &entries) != nil {
		return f.refuse(raw, "a tool list whose tools are not an array"), true
	}
	kept := make([][]byte, 0, len(entries))
	for _, e := range entries {
		if name, ok := toolEntryName(e); ok && !f.hidden(name) {
			kept = append(kept, e)
		}
	}
	if len(kept) == len(entries) {
		return nil, false
	}
	f.filtered(len(entries)-len(kept), len(entries))
	return replaceKey(raw, "result", replaceKey(result, "tools", jsonArray(kept))), true
}

// jsonArray writes items, each already JSON, as one array.
func jsonArray(items [][]byte) []byte {
	return append(append([]byte{'['}, bytes.Join(items, []byte{','})...), ']')
}

// toolEntryName is a tool entry's name when the entry gives exactly one string.
func toolEntryName(entry json.RawMessage) (string, bool) {
	obj, err := exactKeys(entry, "name")
	if err != nil {
		return "", false
	}
	var name string
	if json.Unmarshal(obj["name"], &name) != nil {
		return "", false
	}
	return name, true
}

// refuse records why a list could not be checked and answers the JSON-RPC
// error that replaces it, with the message's id when that reads unambiguously.
func (f *mcpListFilter) refuse(raw json.RawMessage, cause string) []byte {
	f.unreadable(cause)
	var id json.RawMessage
	if obj, err := exactKeys(raw, "id"); err == nil {
		id = obj["id"]
	}
	out, _ := json.Marshal(jsonRPCError(id, mcpListUnreadableCode, mcpListUnreadableMessage))
	return out
}

// replaceKey writes the JSON object raw with key's value replaced by val, every
// other member kept in order with its value's bytes. raw was checked by
// exactKeys, so it is one object that names key once.
func replaceKey(raw []byte, key string, val []byte) []byte {
	dec := json.NewDecoder(bytes.NewReader(raw))
	_, _ = dec.Token()
	b := []byte{'{'}
	for first := true; dec.More(); first = false {
		t, _ := dec.Token()
		k, _ := t.(string)
		var v json.RawMessage
		_ = dec.Decode(&v)
		if !first {
			b = append(b, ',')
		}
		kb, _ := json.Marshal(k)
		b = append(append(b, kb...), ':')
		if k == key {
			v = val
		}
		b = append(b, v...)
	}
	return append(b, '}')
}

// mcpSSEFilter filters an event stream one event at a time. An event that is not
// a tool list is re-emitted as its own bytes; a rewritten one keeps its other
// lines (id, event, retry, comments) and gets one data line. Read returns at
// most one event, so the relay still flushes each event as it arrives. An event
// over 10 MiB ends the stream with an error event.
type mcpSSEFilter struct {
	src    io.ReadCloser
	br     *bufio.Reader
	f      *mcpListFilter
	out    []byte // the rest of the current event
	start  bool   // a byte order mark may open the stream
	skipLF bool   // the last line ended with a CR, so a LF next belongs to it
	err    error  // set once the source ended
}

func (s *mcpSSEFilter) Read(p []byte) (int, error) {
	for len(s.out) == 0 {
		if s.err != nil {
			return 0, s.err
		}
		s.next()
	}
	n := copy(p, s.out)
	s.out = s.out[n:]
	return n, nil
}

func (s *mcpSSEFilter) Close() error { return s.src.Close() }

// next reads one event into out. Lines end with CRLF, LF or CR; an event ends at
// a blank line. A CR waits for a LF only when one is already buffered, so a
// stream that ends lines with CR alone is never held for the next event.
func (s *mcpSSEFilter) next() {
	var raw, line []byte
	var lines []string
	if s.start {
		s.start = false
		// Wait for three bytes only when the first could begin a BOM, so a short first event is not held.
		if first, _ := s.br.Peek(1); len(first) == 1 && first[0] == 0xef {
			if bom, _ := s.br.Peek(3); bytes.Equal(bom, []byte("\xef\xbb\xbf")) {
				_, _ = s.br.Discard(3)
				raw = append(raw, bom...)
			}
		}
	}
	for {
		c, err := s.br.ReadByte()
		if err != nil {
			s.err = err
			if len(line) > 0 {
				lines = append(lines, string(line))
			}
			if len(raw) > 0 {
				s.out = s.emit(raw, lines)
			}
			return
		}
		if len(raw) >= maxBrokeredBody {
			s.f.unreadable("one message was larger than 10 MiB")
			msg, _ := json.Marshal(jsonRPCError(nil, mcpListUnreadableCode, mcpListTooLargeMessage))
			s.out, s.err = append(append([]byte("data: "), msg...), "\n\n"...), io.EOF
			return
		}
		raw = append(raw, c)
		if s.skipLF {
			s.skipLF = false
			if c == '\n' {
				continue
			}
		}
		if c != '\n' && c != '\r' {
			line = append(line, c)
			continue
		}
		if c == '\r' {
			if s.br.Buffered() == 0 {
				s.skipLF = true
			} else if b, _ := s.br.Peek(1); b[0] == '\n' {
				_, _ = s.br.Discard(1)
				raw = append(raw, '\n')
			}
		}
		if len(line) == 0 {
			s.out = s.emit(raw, lines)
			return
		}
		lines = append(lines, string(line))
		line = line[:0]
	}
}

// emit is the bytes one event is relayed as.
func (s *mcpSSEFilter) emit(raw []byte, lines []string) []byte {
	var data, other []string
	for _, l := range lines {
		if name, value, _ := strings.Cut(l, ":"); name == "data" {
			data = append(data, strings.TrimPrefix(value, " "))
			continue
		}
		other = append(other, l)
	}
	if len(data) == 0 {
		return raw
	}
	doc, changed := s.f.document([]byte(strings.Join(data, "\n")))
	if !changed {
		return raw
	}
	var compact bytes.Buffer
	if json.Compact(&compact, doc) != nil { // not reached: a changed document is valid JSON
		compact.Reset()
		compact.Write(s.f.refuse(nil, "the answer is not JSON"))
	}
	var b []byte
	for _, l := range other {
		b = append(append(b, l...), '\n')
	}
	b = append(append(b, "data: "...), compact.Bytes()...)
	return append(b, "\n\n"...)
}
