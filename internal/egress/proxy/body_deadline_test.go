// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bufio"
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A request's read time is re-armed after a hold only while its body has bytes on the connection. A
// request with none left, by having no body or a body a gate already read, is past net/http's last read,
// and a deadline set then ends the connection's background read: the request is cancelled and every
// later one on the connection fails. Each test serves the inner server's way, on one kept-alive
// connection, with the read time cut to 200 ms and an answer slower than that.

const slowAnswer = 500 * time.Millisecond

func shortReadTime(t *testing.T) {
	prev := mitmReadTimeout
	mitmReadTimeout = 200 * time.Millisecond
	t.Cleanup(func() { mitmReadTimeout = prev })
}

// slowUpstream answers what it was sent, after slowAnswer when the query says slow.
func slowUpstream(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "body: "+err.Error(), http.StatusBadRequest)
			return
		}
		if r.URL.Query().Has("slow") {
			time.Sleep(slowAnswer)
		}
		path := r.URL.Path
		if rest, ok := strings.CutPrefix(path, ViaOrgPathPrefix); ok { // <grant id>/<path>
			path = rest[strings.Index(rest, "/"):]
		}
		_, _ = io.WriteString(w, "answer "+r.Method+" "+path+" "+string(b))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// keptAlive is one client connection to a server with the inner server's read time, serving host.
type keptAlive struct {
	conn net.Conn
	br   *bufio.Reader
}

func newKeptAlive(t *testing.T, p *Proxy, host string) *keptAlive {
	t.Helper()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.serveMITMRequest(w, r, host, 443)
	}))
	srv.Config.ReadTimeout = mitmReadTimeout
	srv.Start()
	t.Cleanup(srv.Close)
	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return &keptAlive{conn: conn, br: bufio.NewReader(conn)}
}

func (k *keptAlive) write(t *testing.T, raw string) {
	t.Helper()
	_ = k.conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.WriteString(k.conn, raw); err != nil {
		t.Fatal(err)
	}
}

// answer reads one response: its status and body, or the read error.
func (k *keptAlive) answer() (int, string, error) {
	_ = k.conn.SetDeadline(time.Now().Add(5 * time.Second))
	resp, err := http.ReadResponse(k.br, nil)
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), err
}

// wantAnswers sends first (slow) and then second on the connection, and checks each is answered.
func (k *keptAlive) wantAnswers(t *testing.T, first, wantFirst, second, wantSecond string) {
	t.Helper()
	for _, step := range []struct{ raw, want, label string }{{first, wantFirst, "slow request"}, {second, wantSecond, "next request on the same connection"}} {
		start := time.Now()
		k.write(t, step.raw)
		status, body, err := k.answer()
		if err != nil || status != http.StatusOK || body != step.want {
			t.Fatalf("%s: status %d body %q err %v after %v, want 200 %q", step.label, status, body, err, time.Since(start).Round(10*time.Millisecond), step.want)
		}
	}
}

// viaOrgRunProxy is a run's proxy whose via_org route to host goes to org, with the given host rules.
func viaOrgRunProxy(t *testing.T, org *httptest.Server, host string, rules ...types.HostRule) *Proxy {
	t.Helper()
	orgAddr := upstreamAddr(org)
	return newProxy(Options{RunID: uuid.New(), Resolver: publicResolver{},
		Sink:            &decisionSink{out: &bytes.Buffer{}, ch: make(chan egress.DecisionLog, 64)},
		ViaOrgRoutes:    []ViaOrgRoute{{GrantID: uuid.New(), Host: host}},
		ControlPlaneURL: "http://" + orgAddr, RunToken: newTokenSource("RUNTOK"),
		Dial:   redirectDial(orgAddr),
		Policy: CompilePolicy(types.RunPolicySpec{AllowedDomains: []string{host}, HostRules: []types.HostRuleSet{{Rules: rules}}})})
}

func rawRequest(method, host, target, extra, body string) string {
	if body != "" {
		extra += "Content-Length: " + strconv.Itoa(len(body)) + "\r\n"
	}
	return method + " " + target + " HTTP/1.1\r\nHost: " + host + "\r\n" + extra + "\r\n" + body
}

// A held via_org DELETE has no body: its answer, slower than the read time, still arrives, and so does
// the next request's.
func TestHeldBodilessViaOrgRequestKeepsItsConnection(t *testing.T) {
	shortReadTime(t)
	shortHostCallWindow(t)
	org := slowUpstream(t)
	p := viaOrgRunProxy(t, org, viaOrgHost,
		types.HostRule{Host: viaOrgHost, Methods: []string{http.MethodDelete}, Effect: types.HostRuleHold},
		types.HostRule{Host: viaOrgHost, Effect: types.HostRuleAllow})
	k := newKeptAlive(t, p, viaOrgHost)
	k.wantAnswers(t, rawRequest(http.MethodDelete, viaOrgHost, "/v1/x?slow=1", "", ""), "answer DELETE /v1/x ",
		rawRequest(http.MethodGet, viaOrgHost, "/v1/y", "", ""), "answer GET /v1/y ")
}

// A direct-lane GET whose upstream answers after the read time is answered, and so is the next request.
func TestSlowBodilessMITMRequestKeepsItsConnection(t *testing.T) {
	shortReadTime(t)
	up := slowUpstream(t)
	const host = "files.example"
	inj := &injector{byHost: map[string]*injEntry{host: {grantID: uuid.New(), header: injectedHeader{name: "Authorization", value: "Bearer t"}}}}
	p, _ := newLocalRouteProxy(t, "http://cp.invalid", "RUNTOK", upstreamAddr(up), inj, nil)
	p.mitmHosts, p.mitmPorts = map[string]bool{host: true}, map[string]int{host: 443}
	p.mitmPlaintext = map[string]bool{plaintextKey(host, 443): true}
	k := newKeptAlive(t, p, host)
	k.wantAnswers(t, rawRequest(http.MethodGet, host, "/a?slow=1", "", ""), "answer GET /a ",
		rawRequest(http.MethodGet, host, "/b", "", ""), "answer GET /b ")
}

// A held via_org POST whose body allowed_models already read and replayed has nothing left on the
// connection: its slow answer arrives, and so does the next request's.
func TestHeldViaOrgRequestWithABufferedBodyKeepsItsConnection(t *testing.T) {
	shortReadTime(t)
	shortHostCallWindow(t)
	org := slowUpstream(t)
	p := viaOrgRunProxy(t, org, anthropicHost,
		types.HostRule{Host: anthropicHost, Methods: []string{http.MethodPost}, Paths: []string{"/v1/messages"}, Effect: types.HostRuleHold},
		types.HostRule{Host: anthropicHost, Effect: types.HostRuleAllow})
	p.allowedModels = modelSet([]string{"claude-sonnet-5"})
	const body = `{"model":"claude-sonnet-5"}`
	k := newKeptAlive(t, p, anthropicHost)
	k.wantAnswers(t, rawRequest(http.MethodPost, anthropicHost, "/v1/messages?slow=1", "", body), "answer POST /v1/messages "+body,
		rawRequest(http.MethodPost, anthropicHost, "/v1/messages", "", body), "answer POST /v1/messages "+body)
}

// A held via_org POST whose chunked body is still on the connection keeps its fresh read time: the client
// sends the rest after the headers' read time has lapsed, and the whole body arrives; then the next
// request is answered too.
func TestHeldViaOrgRequestWithAnUnreadChunkedBodyGetsItsReadTime(t *testing.T) {
	shortReadTime(t)
	shortHostCallWindow(t)
	hostCallSecond = 10 * time.Millisecond // the window adds 900 ms to the re-armed read time
	org := slowUpstream(t)
	p := viaOrgRunProxy(t, org, viaOrgHost, viaOrgHoldRules()...)
	k := newKeptAlive(t, p, viaOrgHost)
	k.write(t, "POST /v1/messages?slow=1 HTTP/1.1\r\nHost: "+viaOrgHost+"\r\nTransfer-Encoding: chunked\r\n\r\n5\r\nfirst\r\n")
	time.Sleep(mitmReadTimeout + 100*time.Millisecond) // past the headers' 200 ms, well inside the re-armed 1.1 s
	k.write(t, "4\r\nlast\r\n0\r\n\r\n")
	if status, body, err := k.answer(); err != nil || status != http.StatusOK || body != "answer POST /v1/messages firstlast" {
		t.Fatalf("held chunked POST: status %d body %q err %v, want the whole body to arrive", status, body, err)
	}
	k.write(t, rawRequest(http.MethodGet, viaOrgHost, "/v1/y", "", ""))
	if status, body, err := k.answer(); err != nil || status != http.StatusOK || body != "answer GET /v1/y " {
		t.Fatalf("next request on the same connection: status %d body %q err %v", status, body, err)
	}
}

// A held request whose body never arrives still meets its read time: the connection ends within the
// re-armed bound instead of waiting on the body forever, and the organisation never answers a whole body.
func TestHeldRequestWithAStalledBodyStillTimesOut(t *testing.T) {
	shortReadTime(t)
	shortHostCallWindow(t)
	var whole atomic.Bool
	org := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.ReadAll(r.Body); err == nil {
			whole.Store(true)
			_, _ = io.WriteString(w, "org answer")
		}
	}))
	t.Cleanup(org.Close)
	p := viaOrgRunProxy(t, org, viaOrgHost, viaOrgHoldRules()...)
	k := newKeptAlive(t, p, viaOrgHost)
	start := time.Now()
	k.write(t, "POST /v1/messages HTTP/1.1\r\nHost: "+viaOrgHost+"\r\nContent-Length: 100\r\n\r\nonly ten b")
	status, body, err := k.answer()
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("stalled body: answered after %v (status %d err %v), want the read time to end it", elapsed, status, err)
	}
	if whole.Load() || body == "org answer" {
		t.Fatalf("stalled body: status %d body %q, organisation saw a whole body; want the read to fail", status, body)
	}
}

// What the re-arm reads: a network body is unread until it reaches EOF or is closed, whatever replaces
// r.Body after; no body is never unread; a chunked body (no length) is unread until its end.
func TestNetworkBodyUnread(t *testing.T) {
	if networkBodyUnread(trackNetworkBody(httptest.NewRequest(http.MethodGet, "/", nil))) {
		t.Error("a request with no body is unread")
	}
	r := trackNetworkBody(httptest.NewRequest(http.MethodPost, "/", strings.NewReader("abc")))
	if !networkBodyUnread(r) {
		t.Error("an unread body is not unread")
	}
	b, _ := io.ReadAll(r.Body)
	r.Body = io.NopCloser(bytes.NewReader(b)) // what a gate that buffers the body leaves
	if networkBodyUnread(r) {
		t.Error("a body read to EOF and replayed is unread")
	}
	chunked := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("abc"))
	chunked.ContentLength = -1
	chunked = trackNetworkBody(chunked)
	if _, _ = chunked.Body.Read(make([]byte, 1)); !networkBodyUnread(chunked) {
		t.Error("a chunked body read in part is not unread")
	}
	closed := trackNetworkBody(httptest.NewRequest(http.MethodPost, "/", strings.NewReader("abc")))
	if _ = closed.Body.Close(); networkBodyUnread(closed) {
		t.Error("a closed body is unread")
	}
}

// Every read deadline a handler sets goes through rearmBodyDeadlineAfter, and these are its callers. A
// new caller must be added here, after checking its request is one an entry tracks (trackNetworkBody).
func TestBodyDeadlineRearmCallers(t *testing.T) {
	want := []string{
		"aws_sigv4.go:serveAWSSigV4", "azure_arm_door.go:serveAzureARM", "gateway_identity.go:attachGatewayIdentity",
		"kube_api_door.go:serveKubeAPI", "mitm.go:rearmBodyDeadline", "mitm.go:serveAdmittedMITMRequest",
		"mitm.go:serveMITMRequest", "via_org.go:ServeBroker", "via_org.go:ServeInjected",
	}
	// rc holds a ResponseController when it is NewResponseController's result, chained or kept in a
	// variable; a deadline set through either is the handler's own.
	isController := func(e ast.Expr, held map[string]bool) bool {
		switch x := e.(type) {
		case *ast.CallExpr:
			s, ok := x.Fun.(*ast.SelectorExpr)
			return ok && s.Sel.Name == "NewResponseController"
		case *ast.Ident:
			return held[x.Name]
		}
		return false
	}
	files, _ := filepath.Glob("*.go")
	fset := token.NewFileSet()
	var got []string
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			held := map[string]bool{}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if as, ok := n.(*ast.AssignStmt); ok {
					for i, rhs := range as.Rhs {
						if i >= len(as.Lhs) {
							break
						}
						if id, ok := as.Lhs[i].(*ast.Ident); ok && isController(rhs, held) {
							held[id.Name] = true
						}
					}
				}
				if vs, ok := n.(*ast.ValueSpec); ok {
					for i, v := range vs.Values {
						if i < len(vs.Names) && isController(v, held) {
							held[vs.Names[i].Name] = true
						}
					}
				}
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				switch c := call.Fun.(type) {
				case *ast.Ident:
					if c.Name == "rearmBodyDeadline" || c.Name == "rearmBodyDeadlineAfter" {
						got = append(got, name+":"+fn.Name.Name)
					}
				case *ast.SelectorExpr:
					if c.Sel.Name == "SetReadDeadline" && isController(c.X, held) && fn.Name.Name != "rearmBodyDeadlineAfter" {
						t.Errorf("%s: %s sets a handler's read deadline itself, not through rearmBodyDeadlineAfter", fset.Position(call.Pos()), fn.Name.Name)
					}
				}
				return true
			})
		}
	}
	slices.Sort(got)
	got = slices.Compact(got)
	if !slices.Equal(got, want) {
		t.Errorf("rearm callers:\n got %v\nwant %v", got, want)
	}
}

// A model call through Proxy.ServeHTTP whose body the route read before the gateway identity mint is past
// its last read: the gateway's answer, slower than the read time, arrives, and so does the next call's.
// The listener sets no read time of its own, as the proxy's agent-facing one does not.
func TestGatewayIdentityModelCallWithABufferedBodyKeepsItsConnection(t *testing.T) {
	shortReadTime(t)
	g := newGwidRig(t, gwidRoutes(), false)
	var calls atomic.Int64
	g.gw.mu.Lock()
	g.gw.respond = func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			time.Sleep(slowAnswer)
		}
		_, _ = io.WriteString(w, "gateway answer")
	}
	g.gw.mu.Unlock()
	g.p.allowedModels = modelSet([]string{"claude-sonnet-5"}) // the route reads the body to check the model
	srv := httptest.NewServer(g.p)
	t.Cleanup(srv.Close)
	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	k := &keptAlive{conn: conn, br: bufio.NewReader(conn)}
	call := rawRequest(http.MethodPost, "example.com", llmAnthropicPrefix+"messages", "", `{"model":"claude-sonnet-5"}`)
	k.wantAnswers(t, call, "gateway answer", call, "gateway answer")
	if g.mint.hits.Load() == 0 {
		t.Fatal("no gateway identity was minted, so the call never passed the re-arm after the mint")
	}
}
