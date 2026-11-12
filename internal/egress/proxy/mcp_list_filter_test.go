// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const (
	listFilteredSource   = "brokered:mcp:list-filtered"
	listUnreadableSource = "brokered:mcp:list-unreadable"
	listUnreadableCode   = -32002
)

// listUpstream records what the MCP server was sent; the reply is the test's.
type listUpstream struct {
	mu        sync.Mutex
	acceptEnc []string
}

func (u *listUpstream) lastAcceptEncoding() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.acceptEnc) == 0 {
		return "<none>"
	}
	return u.acceptEnc[len(u.acceptEnc)-1]
}

// newListFixture is a proxy whose one server, linear, is answered by h, and on for
// claude-code and codex-cli.
func newListFixture(t *testing.T, spec types.RunPolicySpec, h http.HandlerFunc) (*mcpFixture, *listUpstream) {
	t.Helper()
	u := &listUpstream{}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/internal/gateway-identity" {
			exp := time.Now().Add(10 * time.Minute)
			_ = json.NewEncoder(w).Encode(map[string]any{"token": gwidToken(gwidAudience, exp, 1), "expires_at": exp})
			return
		}
		u.mu.Lock()
		u.acceptEnc = append(u.acceptEnc, r.Header.Get("Accept-Encoding"))
		u.mu.Unlock()
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	spec.AllowedDomains = append(spec.AllowedDomains, "mcp.linear.test")
	f := &mcpFixture{buf: &bytes.Buffer{}}
	sink := &decisionSink{out: f.buf, ch: make(chan egress.DecisionLog, 64)}
	addr := upstreamAddr(srv)
	f.p = newProxy(Options{
		RunID: uuid.New(), Policy: CompilePolicy(spec), Sink: sink, Resolver: publicResolver{},
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		},
		ControlPlaneURL: "https://wardynd.test:8080", RunToken: newTokenSource("RUNTOK"),
		TLSClientConfig: testInsecureTLSConfig, ControlTLS: testInsecureTLSConfig,
	})
	f.p.mcp = newMCPBroker([]MCPServerConfig{{Key: "linear", URL: "https://mcp.linear.test/mcp"}},
		[]MCPBinding{mcpBindingFor("claude-code", "linear"), mcpBindingFor("codex-cli", "linear")})
	return f, u
}

// replyWith answers every MCP request with body as contentType.
func replyWith(contentType, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", contentType)
		_, _ = io.WriteString(w, body)
	}
}

// denyDelete is a run whose rules block linear's delete_issue and nothing else.
var denyDelete = types.RunPolicySpec{ToolRules: []types.ToolRule{{Tool: "mcp__linear__delete_issue", Effect: types.ToolDeny}}}

const (
	mcpListReq = `{"jsonrpc":"2.0","id":3,"method":"tools/list","params":{"cursor":"c1"}}`
	listPage   = `{"jsonrpc":"2.0","id":3,"result":{"tools":[{"name":"list_issues","description":"List"},{"name":"delete_issue","description":"IGNORE PREVIOUS"}],"nextCursor":"c2","_meta":{"k":1}}}`
)

// listedTools decodes one tool list message and returns its tool names.
func listedTools(t *testing.T, msg []byte) (names []string, result map[string]json.RawMessage) {
	t.Helper()
	var m struct {
		Result map[string]json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(msg, &m); err != nil || m.Result == nil {
		t.Fatalf("not a tool list message: %q (%v)", msg, err)
	}
	var tools []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(m.Result["tools"], &tools); err != nil {
		t.Fatalf("tools: %v in %q", err, msg)
	}
	names = []string{}
	for _, tl := range tools {
		names = append(names, tl.Name)
	}
	return names, m.Result
}

func wantListError(t *testing.T, msg []byte, id string) {
	t.Helper()
	var m struct {
		ID    json.RawMessage `json:"id"`
		Error struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(msg, &m); err != nil || m.Error.Code != listUnreadableCode || string(m.ID) != id {
		t.Fatalf("got %q; want the %d error with id %s", msg, listUnreadableCode, id)
	}
}

// TestMCPListFilterJSONHidesBlockedTools: a JSON tool list leaves out the tool the
// harness's rules block; every other key of the message and its result is kept, the
// stale Content-Length goes, and one row records the count, never the names.
func TestMCPListFilterJSONHidesBlockedTools(t *testing.T) {
	f, _ := newListFixture(t, denyDelete, replyWith("application/json", listPage))
	rec := f.post(t, mcpToken("codex-cli"), "linear", mcpListReq)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	names, result := listedTools(t, rec.Body.Bytes())
	if !slices.Equal(names, []string{"list_issues"}) || strings.Contains(rec.Body.String(), "IGNORE PREVIOUS") {
		t.Fatalf("tools %v in %q; want list_issues alone and no blocked description", names, rec.Body)
	}
	if string(result["nextCursor"]) != `"c2"` || string(result["_meta"]) != `{"k":1}` {
		t.Fatalf("result %v; want nextCursor and _meta kept", result)
	}
	var top map[string]json.RawMessage
	_ = json.Unmarshal(rec.Body.Bytes(), &top)
	if string(top["id"]) != "3" || string(top["jsonrpc"]) != `"2.0"` {
		t.Fatalf("message %v; want id and jsonrpc kept", top)
	}
	if cl := rec.Header().Get("Content-Length"); cl != "" {
		t.Fatalf("Content-Length %q survived the rewrite", cl)
	}
	d := findDecision(t, f.buf, listFilteredSource)
	if d.Decision != egress.Allow || d.Cause != "hid 1 of 2 tools" {
		t.Fatalf("row %s %q; want allow \"hid 1 of 2 tools\"", d.Decision, d.Cause)
	}
}

// TestMCPListFilterInactivePassesBytes: a run with no deny rule that could touch the
// server gets the server's bytes, its Content-Length and its own Accept-Encoding.
func TestMCPListFilterInactivePassesBytes(t *testing.T) {
	for name, spec := range map[string]types.RunPolicySpec{
		"no rules":              {},
		"hold only":             {ToolRules: []types.ToolRule{{Tool: "mcp__linear__delete_issue", Effect: types.ToolHold}}},
		"deny on another serve": {ToolRules: []types.ToolRule{{Tool: "mcp__github__delete_issue", Effect: types.ToolDeny}}},
	} {
		t.Run(name, func(t *testing.T) {
			f, u := newListFixture(t, spec, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Content-Length", "200")
				_, _ = io.WriteString(w, listPage+strings.Repeat(" ", 200-len(listPage)))
			})
			rec := f.post(t, mcpToken("codex-cli"), "linear", mcpListReq, "Accept-Encoding", "br")
			if want := listPage + strings.Repeat(" ", 200-len(listPage)); rec.Body.String() != want {
				t.Fatalf("body %q, want the server's bytes", rec.Body)
			}
			if rec.Header().Get("Content-Length") != "200" || u.lastAcceptEncoding() != "br" {
				t.Fatalf("Content-Length %q, upstream Accept-Encoding %q; want 200 and br", rec.Header().Get("Content-Length"), u.lastAcceptEncoding())
			}
			if strings.Contains(f.buf.String(), "brokered:mcp:list-") {
				t.Fatalf("a list row was written on an inactive exchange: %s", f.buf)
			}
		})
	}
}

// flushRec records what was written between flushes.
type flushRec struct {
	*httptest.ResponseRecorder
	cur    bytes.Buffer
	chunks []string
}

func (r *flushRec) Write(b []byte) (int, error) {
	r.cur.Write(b)
	return r.ResponseRecorder.Write(b)
}

func (r *flushRec) Flush() {
	if r.cur.Len() > 0 {
		r.chunks = append(r.chunks, r.cur.String())
		r.cur.Reset()
	}
	r.ResponseRecorder.Flush()
}

func (f *mcpFixture) serveFlushed(t *testing.T, method, body string, hdr ...string) *flushRec {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req := mustLocalReq(t, method, "/wardyn/mcp/"+mcpToken("codex-cli")+"/linear", rdr)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rec := &flushRec{ResponseRecorder: httptest.NewRecorder()}
	f.p.ServeHTTP(rec, req)
	rec.Flush()
	return rec
}

const (
	sseComment  = ": keepalive\n\n"
	sseProgress = "id: 1\nevent: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\",\"params\":{\"progressToken\":1}}\n\n"
	sseList     = "id: 2\r\nretry: 1000\r\n: page\r\ndata: {\"jsonrpc\":\"2.0\",\"id\":3,\"result\":{\"tools\":[{\"name\":\"list_issues\"},\r\ndata: {\"name\":\"delete_issue\"}],\"nextCursor\":\"c2\"}}\r\n\r\n"
	sseResult   = "id: 3\ndata: {\"jsonrpc\":\"2.0\",\"id\":4,\"result\":{\"content\":[]}}\n\n"
)

// sseData is the data of the one event chunk holds, and its other lines.
func sseData(t *testing.T, chunk string) (data string, other []string) {
	t.Helper()
	if !strings.HasSuffix(chunk, "\n\n") {
		t.Fatalf("event %q does not end with a blank line", chunk)
	}
	for _, line := range strings.Split(strings.TrimSuffix(chunk, "\n\n"), "\n") {
		if v, ok := strings.CutPrefix(line, "data: "); ok {
			if data != "" {
				t.Fatalf("event %q has more than one data line", chunk)
			}
			data = v
			continue
		}
		other = append(other, line)
	}
	return data, other
}

// TestMCPListFilterSSE: in an event stream, every other event passes byte-identical,
// the tool list event keeps its id, retry and comment lines and gets one data line, and
// each event is flushed on its own.
func TestMCPListFilterSSE(t *testing.T) {
	f, _ := newListFixture(t, denyDelete, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "\xef\xbb\xbf"+sseComment+sseProgress+sseList+sseResult)
	})
	rec := f.serveFlushed(t, http.MethodPost, mcpListReq)
	if len(rec.chunks) != 4 {
		t.Fatalf("%d flushed chunks %q; want one per event", len(rec.chunks), rec.chunks)
	}
	if rec.chunks[0] != "\xef\xbb\xbf"+sseComment || rec.chunks[1] != sseProgress || rec.chunks[3] != sseResult {
		t.Fatalf("chunks %q; want the other events byte-identical", rec.chunks)
	}
	data, other := sseData(t, rec.chunks[2])
	if !slices.Equal(other, []string{"id: 2", "retry: 1000", ": page"}) {
		t.Fatalf("other lines %q; want id, retry and the comment kept", other)
	}
	names, result := listedTools(t, []byte(data))
	if !slices.Equal(names, []string{"list_issues"}) || string(result["nextCursor"]) != `"c2"` {
		t.Fatalf("tools %v cursor %s; want list_issues and c2", names, result["nextCursor"])
	}
	if d := findDecision(t, f.buf, listFilteredSource); d.Cause != "hid 1 of 2 tools" {
		t.Fatalf("row cause %q", d.Cause)
	}
}

// TestMCPListFilterBatchResponse: a batch answer is filtered element by element, and an
// element that is not a tool list keeps its bytes.
func TestMCPListFilterBatchResponse(t *testing.T) {
	other := `{"jsonrpc":"2.0", "id":4, "result":{"content":[]}}`
	f, _ := newListFixture(t, denyDelete, replyWith("application/json", "["+listPage+",\n "+other+"]"))
	rec := f.post(t, mcpToken("codex-cli"), "linear", "["+mcpListReq+"]")
	var batch []json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &batch); err != nil || len(batch) != 2 {
		t.Fatalf("body %q: %v; want a batch of two", rec.Body, err)
	}
	if names, _ := listedTools(t, batch[0]); !slices.Equal(names, []string{"list_issues"}) {
		t.Fatalf("tools %v; want list_issues", names)
	}
	if string(batch[1]) != other {
		t.Fatalf("second element %q, want %q byte-identical", batch[1], other)
	}
}

// TestMCPListFilterResumedGET: a GET stream resumed with Last-Event-ID may carry a
// tool list answer, and it is filtered too.
func TestMCPListFilterResumedGET(t *testing.T) {
	f, _ := newListFixture(t, denyDelete, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.Header.Get("Last-Event-ID") != "1" {
			http.Error(w, "want a resumed GET", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		_, _ = io.WriteString(w, sseList)
	})
	rec := f.serveFlushed(t, http.MethodGet, "", "Last-Event-ID", "1")
	if rec.Code != http.StatusOK || len(rec.chunks) != 1 {
		t.Fatalf("status %d chunks %q", rec.Code, rec.chunks)
	}
	data, _ := sseData(t, rec.chunks[0])
	if names, _ := listedTools(t, []byte(data)); !slices.Equal(names, []string{"list_issues"}) {
		t.Fatalf("tools %v; want list_issues", names)
	}
}

// TestMCPListFilterEmptyPageKeepsCursor: a page whose every tool is hidden answers an
// empty array with its cursor, so a paging client asks for the next page.
func TestMCPListFilterEmptyPageKeepsCursor(t *testing.T) {
	spec := types.RunPolicySpec{ToolRules: []types.ToolRule{{Tool: "*", Effect: types.ToolDeny}}}
	f, _ := newListFixture(t, spec, replyWith("application/json", listPage))
	rec := f.post(t, mcpToken("codex-cli"), "linear", mcpListReq)
	names, result := listedTools(t, rec.Body.Bytes())
	if len(names) != 0 || string(result["tools"]) != "[]" || string(result["nextCursor"]) != `"c2"` {
		t.Fatalf("result %q; want tools [] and nextCursor c2", rec.Body)
	}
	if d := findDecision(t, f.buf, listFilteredSource); d.Cause != "hid 2 of 2 tools" {
		t.Fatalf("row cause %q", d.Cause)
	}
}

// TestMCPListFilterAmbiguousFailsClosed: a list a client could read two ways is
// answered with an error carrying its id; an entry with an ambiguous name is dropped;
// a body that is not JSON is answered with an error whose id is null.
func TestMCPListFilterAmbiguousFailsClosed(t *testing.T) {
	both := `{"jsonrpc":"2.0","id":3,"result":{"tools":[{"name":"list_issues"}],"Tools":[{"name":"delete_issue"}]}}`
	f, _ := newListFixture(t, denyDelete, replyWith("application/json", both))
	rec := f.post(t, mcpToken("codex-cli"), "linear", mcpListReq)
	wantListError(t, bytes.TrimSpace(rec.Body.Bytes()), "3")
	if strings.Contains(rec.Body.String(), "delete_issue") {
		t.Fatalf("the blocked tool reached the agent: %q", rec.Body)
	}
	wantDecision(t, f.buf, egress.Deny, listUnreadableSource)

	repeated := `{"jsonrpc":"2.0","id":3,"result":{"tools":[{"name":"list_issues","name":"delete_issue"},{"name":"get_issue"},{"name":7}]}}`
	f, _ = newListFixture(t, denyDelete, replyWith("application/json", repeated))
	rec = f.post(t, mcpToken("codex-cli"), "linear", mcpListReq)
	if names, _ := listedTools(t, rec.Body.Bytes()); !slices.Equal(names, []string{"get_issue"}) {
		t.Fatalf("tools %v; want the ambiguous and nameless entries dropped", names)
	}

	f, _ = newListFixture(t, denyDelete, replyWith("application/json", `{"jsonrpc":"2.0","id":3,"result":{"tools":[{"name":"delete_issue"}],}}`))
	rec = f.post(t, mcpToken("codex-cli"), "linear", mcpListReq)
	wantListError(t, bytes.TrimSpace(rec.Body.Bytes()), "null")
}

// TestMCPListFilterOverCapEndsResponse: one message over 10 MiB ends the response: a
// JSON answer is a 502, an event stream ends with an error event after what came before.
func TestMCPListFilterOverCapEndsResponse(t *testing.T) {
	huge := `{"jsonrpc":"2.0","id":3,"result":{"tools":[{"name":"delete_issue","description":"` + strings.Repeat("x", 10<<20) + `"}]}}`
	f, _ := newListFixture(t, denyDelete, replyWith("application/json", huge))
	rec := f.post(t, mcpToken("codex-cli"), "linear", mcpListReq)
	if rec.Code != http.StatusBadGateway || strings.Contains(rec.Body.String(), "delete_issue") {
		t.Fatalf("status %d (%d bytes); want 502 and nothing of the list", rec.Code, rec.Body.Len())
	}
	wantDecision(t, f.buf, egress.Deny, listUnreadableSource)

	f, _ = newListFixture(t, denyDelete, replyWith("text/event-stream", sseProgress+"data: "+huge+"\n\n"+sseResult))
	fr := f.serveFlushed(t, http.MethodPost, mcpListReq)
	if len(fr.chunks) != 2 || fr.chunks[0] != sseProgress {
		t.Fatalf("%d chunks; want the first event and one error event", len(fr.chunks))
	}
	data, _ := sseData(t, fr.chunks[1])
	wantListError(t, []byte(data), "null")
	wantDecision(t, f.buf, egress.Deny, listUnreadableSource)
}

// TestMCPListFilterEncodedRefused: an active exchange asks the server for identity
// bytes, and an answer that arrives encoded anyway is refused, never relayed unread.
func TestMCPListFilterEncodedRefused(t *testing.T) {
	f, u := newListFixture(t, denyDelete, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = io.WriteString(w, "\x1f\x8bnot-really-gzip")
	})
	rec := f.post(t, mcpToken("codex-cli"), "linear", mcpListReq, "Accept-Encoding", "gzip")
	if u.lastAcceptEncoding() != "identity" {
		t.Fatalf("upstream Accept-Encoding %q, want identity", u.lastAcceptEncoding())
	}
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status %d, want 502", rec.Code)
	}
	wantDecision(t, f.buf, egress.Deny, listUnreadableSource)
}

// TestMCPListAndCallShareOnePredicate: over rule sets and the names a harness may give
// one tool, a tool is left out of the list exactly when its call is refused.
func TestMCPListAndCallShareOnePredicate(t *testing.T) {
	type rules struct {
		spec    types.RunPolicySpec
		harness []types.HarnessToolRules
	}
	deny := func(tool string) types.ToolRule { return types.ToolRule{Tool: tool, Effect: types.ToolDeny} }
	sets := map[string]rules{
		"run-wide raw":       {spec: types.RunPolicySpec{ToolRules: []types.ToolRule{deny("mcp__linear__delete.issue")}}},
		"run-wide claude":    {spec: types.RunPolicySpec{ToolRules: []types.ToolRule{deny("mcp__linear__delete_issue")}}},
		"run-wide star":      {spec: types.RunPolicySpec{ToolRules: []types.ToolRule{deny("*"), {Tool: "mcp__linear__get", Effect: types.ToolAllow}}}},
		"run-wide hold":      {spec: types.RunPolicySpec{ToolRules: []types.ToolRule{{Tool: "mcp__linear__delete_issue", Effect: types.ToolHold}}}},
		"allow raw, deny id": {spec: types.RunPolicySpec{ToolRules: []types.ToolRule{{Tool: "mcp__linear__del-ete", Effect: types.ToolAllow}, deny("mcp__linear__del_ete")}}},
		"harness default": {harness: []types.HarnessToolRules{
			{Harness: "codex-cli", Default: types.ToolDeny, Rules: []types.ToolRule{{Tool: "mcp__linear__get", Effect: types.ToolAllow}}},
			{Harness: "claude-code", Default: types.ToolAllow}}},
		"harness unknown effect": {harness: []types.HarnessToolRules{{Harness: "codex-cli", Default: types.ToolAllow, Rules: []types.ToolRule{{Tool: "mcp__linear__get", Effect: "a_later_effect"}}}}},
		"harness astral":         {harness: []types.HarnessToolRules{{Harness: "codex-cli", Default: types.ToolAllow, Rules: []types.ToolRule{deny("mcp__linear__admin__delete")}}}},
	}
	tools := []string{"delete_issue", "delete.issue", "del-ete", "get", "admin\U0001F600delete"}
	var mu sync.Mutex
	listed := ""
	hiddenSeen, shownSeen := 0, 0
	for name, rs := range sets {
		f, _ := newListFixture(t, rs.spec, func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			w.Header().Set("Content-Type", "application/json")
			if !bytes.Contains(b, []byte("tools/list")) {
				_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"content":[]}}`)
				return
			}
			mu.Lock()
			page, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 3, "result": map[string]any{"tools": []any{map[string]string{"name": listed}}}})
			mu.Unlock()
			_, _ = w.Write(page)
		})
		f.p.harnessRules = rs.harness
		for _, tool := range tools {
			mu.Lock()
			listed = tool
			mu.Unlock()
			for _, h := range []string{"codex-cli", "claude-code"} {
				refused := strings.Contains(f.post(t, mcpToken(h), "linear", mcpCall(1, tool)).Body.String(), mcpBlockedMessage)
				names, _ := listedTools(t, f.post(t, mcpToken(h), "linear", mcpListReq).Body.Bytes())
				hidden := len(names) == 0
				if hidden != refused {
					t.Errorf("%s, %s, %q: hidden=%v refused=%v", name, h, tool, hidden, refused)
				}
				if hidden {
					hiddenSeen++
				} else {
					shownSeen++
				}
			}
		}
	}
	if hiddenSeen == 0 || shownSeen == 0 {
		t.Fatalf("hidden %d, shown %d: the table must exercise both", hiddenSeen, shownSeen)
	}
}

// TestMCPListFilterGatewayRoute: a server with a gateway identity route has its lists
// filtered too.
func TestMCPListFilterGatewayRoute(t *testing.T) {
	f, _ := newListFixture(t, denyDelete, replyWith("application/json", listPage))
	t.Cleanup(func() { procRegistry.Evict(f.p.maskKey) })
	withMCPGatewayRoute(f, "linear", "/mcp")
	rec := f.post(t, mcpToken("codex-cli"), "linear", mcpListReq)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if names, _ := listedTools(t, rec.Body.Bytes()); !slices.Equal(names, []string{"list_issues"}) {
		t.Fatalf("tools %v; want list_issues", names)
	}
	findDecision(t, f.buf, listFilteredSource)
}

// TestMCPListChangedPassesThrough: the server's list_changed notice reaches the agent
// unchanged on a filtered exchange, so it lists again and gets the filtered list.
func TestMCPListChangedPassesThrough(t *testing.T) {
	ev := "event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/tools/list_changed\"}\n\n"
	f, _ := newListFixture(t, denyDelete, replyWith("text/event-stream", ev))
	rec := f.serveFlushed(t, http.MethodGet, "")
	if len(rec.chunks) != 1 || rec.chunks[0] != ev {
		t.Fatalf("chunks %q; want the notice byte-identical", rec.chunks)
	}
}

// TestMCPListFilterHybridMessageFailsClosed: a message that is both a request and a
// tool list answer could be read either way, so it is replaced with the error.
func TestMCPListFilterHybridMessageFailsClosed(t *testing.T) {
	hybrid := `{"jsonrpc":"2.0","id":3,"method":"tools/list","result":{"tools":[{"name":"delete_issue"}]}}`
	f, _ := newListFixture(t, denyDelete, replyWith("application/json", hybrid))
	rec := f.post(t, mcpToken("codex-cli"), "linear", mcpListReq)
	wantListError(t, bytes.TrimSpace(rec.Body.Bytes()), "3")
	wantDecision(t, f.buf, egress.Deny, listUnreadableSource)
}

// TestMCPListFilterOtherMediaTypeFailsClosed: on a filtered exchange, an answer that
// is neither JSON nor an event stream is replaced with the error; an answer with no
// body keeps its status; an exchange that is not filtered passes any media type.
func TestMCPListFilterOtherMediaTypeFailsClosed(t *testing.T) {
	f, _ := newListFixture(t, denyDelete, replyWith("text/plain", listPage))
	rec := f.post(t, mcpToken("codex-cli"), "linear", mcpListReq)
	if strings.Contains(rec.Body.String(), "delete_issue") {
		t.Fatalf("the blocked tool reached the agent: %q", rec.Body)
	}
	wantListError(t, bytes.TrimSpace(rec.Body.Bytes()), "null")
	wantDecision(t, f.buf, egress.Deny, listUnreadableSource)

	f, _ = newListFixture(t, denyDelete, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusAccepted) })
	if rec := f.post(t, mcpToken("codex-cli"), "linear", `{"jsonrpc":"2.0","method":"notifications/initialized"}`); rec.Code != http.StatusAccepted || rec.Body.Len() != 0 {
		t.Fatalf("status %d body %q; want 202 and no body", rec.Code, rec.Body)
	}

	f, _ = newListFixture(t, types.RunPolicySpec{}, replyWith("text/plain", listPage))
	if rec := f.post(t, mcpToken("codex-cli"), "linear", mcpListReq); rec.Body.String() != listPage {
		t.Fatalf("body %q; want the bytes on an exchange that is not filtered", rec.Body)
	}
}

// TestMCPListFilterLogNamesNoTool: the proxy's log of a filtered list carries counts
// only. A server that echoes a credential as a hidden tool's name gets nothing into
// the log, which is written before the relay's masking.
func TestMCPListFilterLogNamesNoTool(t *testing.T) {
	const secret = "Bearer fabricated-injected-credential-7f3a9c"
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	page, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 3, "result": map[string]any{"tools": []any{map[string]string{"name": secret}, map[string]string{"name": "get"}}}})
	spec := types.RunPolicySpec{ToolRules: []types.ToolRule{{Tool: "*", Effect: types.ToolDeny}, {Tool: "mcp__linear__get", Effect: types.ToolAllow}}}
	f, _ := newListFixture(t, spec, replyWith("application/json", string(page)))
	rec := f.post(t, mcpToken("codex-cli"), "linear", mcpListReq)
	if names, _ := listedTools(t, rec.Body.Bytes()); !slices.Equal(names, []string{"get"}) {
		t.Fatalf("tools %v; want get alone", names)
	}
	if logs.Len() == 0 {
		t.Fatalf("no log line captured; the filter's log must still be written")
	}
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, "fabricated-injected-credential") {
			t.Fatalf("a hidden tool's name reached the log: %s", line)
		}
	}
}

// TestMCPListFilterShortFirstEventIsNotHeld: a first event shorter than a byte-order mark reaches the
// client as soon as it is complete, while the server waits to send more.
func TestMCPListFilterShortFirstEventIsNotHeld(t *testing.T) {
	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close() })
	s := &mcpSSEFilter{src: pr, br: bufio.NewReader(pr), start: true}
	go func() { _, _ = pw.Write([]byte("\n")) }() // an empty event, then the server waits
	got := make(chan string, 1)
	go func() {
		b := make([]byte, 16)
		n, _ := s.Read(b)
		got <- string(b[:n])
	}()
	select {
	case v := <-got:
		if v != "\n" {
			t.Fatalf("read %q, want the empty event", v)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a one-byte first event was held back waiting for a byte-order mark")
	}
}
