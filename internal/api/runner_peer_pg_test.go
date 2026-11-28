// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/hoptls"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/remote"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
)

// peerDoorCAPEM is the internal CA of every test that mounts the runner peer door (R6), made once.
var peerDoorCAPEM = sync.OnceValue(func() string {
	blob, err := hoptls.NewCA(time.Now())
	if err != nil {
		panic(err)
	}
	ca, err := hoptls.ParseCA(blob)
	if err != nil {
		panic(err)
	}
	return string(ca.CertPEM)
})

// peerDoorMount is the Config shape that mounts the runner peer door: HA with the internal listener.
func peerDoorMount(c *Config) { c.HA, c.ControlPlaneCAPEM = true, peerDoorCAPEM() }

// peerTestCA is a CA the peer tests mint serving certificates from, made once.
var peerTestCA = sync.OnceValue(func() *hoptls.CA {
	blob, err := hoptls.NewCA(time.Now())
	if err != nil {
		panic(err)
	}
	ca, err := hoptls.ParseCA(blob)
	if err != nil {
		panic(err)
	}
	return ca
})

// peerTestHost is the control-plane host the peer tests' serving certificate names.
const peerTestHost = "cp.peer.test"

var peerTestKey = bytes.Repeat([]byte{0x5a}, 32)

// peerShape mounts the door and gives the server a peer config under peerTestCA; the URL is set once the door's
// TLS server is up.
func peerShape(pc *remote.PeerConfig) func(*Config) {
	return func(c *Config) {
		c.HA, c.ControlPlaneCAPEM, c.RunnerPeer = true, string(peerTestCA().CertPEM), pc
	}
}

func newPeerConfig() *remote.PeerConfig {
	return &remote.PeerConfig{Key: peerTestKey, Roots: hopRootsPool(peerTestCA().CertPEM), ServerName: peerTestHost}
}

func hopRootsPool(pem []byte) *x509.CertPool {
	p := x509.NewCertPool()
	p.AppendCertsFromPEM(pem)
	return p
}

// serveDoor serves srv's internal handler over TLS with a certificate for peerTestHost and sets pc.URL to it.
func serveDoor(t *testing.T, srv *Server, pc *remote.PeerConfig) *httptest.Server {
	t.Helper()
	cert, err := peerTestCA().ServingCert(peerTestHost, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewUnstartedServer(srv.InternalHandler())
	ts.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{"http/1.1"}, MinVersion: tls.VersionTLS13}
	ts.StartTLS()
	t.Cleanup(ts.Close)
	pc.URL = ts.URL
	return ts
}

// peerRequest is a door request carrying tok signed under key.
func peerRequest(tok remote.PeerToken, key []byte) *http.Request {
	r := httptest.NewRequest(http.MethodGet, remote.PeerPath, nil)
	if key != nil {
		tok.SetHeaders(r.Header, key)
	}
	return r
}

// R6: the door is absent without HA or without the internal listener, is one GET route under peerDoorMount, and is
// mounted outside mountInternalRoutes.
func TestPeerDoorNotMountedWithoutHA(t *testing.T) {
	keys := func(shape ...func(*Config)) []string {
		srv, _, _, _ := newAuthzMatrixServer(t, shape...)
		var got []string
		_ = chi.Walk(srv.router, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
			if strings.Contains(route, "/internal/runner-peer") {
				got = append(got, method+" "+route)
			}
			return nil
		})
		w := httptest.NewRecorder()
		srv.InternalHandler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, remote.PeerPath, nil))
		if len(got) == 0 && w.Code != http.StatusNotFound {
			t.Errorf("unmounted door answered %d, want 404", w.Code)
		}
		return got
	}
	if got := keys(); len(got) != 0 {
		t.Errorf("without HA the door is mounted: %v", got)
	}
	if got := keys(func(c *Config) { c.HA = true }); len(got) != 0 {
		t.Errorf("HA without the internal listener mounts the door: %v", got)
	}
	if got := keys(peerDoorMount); len(got) != 1 || got[0] != "GET /api/v1/internal/runner-peer" {
		t.Errorf("peerDoorMount walk = %v, want exactly GET /api/v1/internal/runner-peer", got)
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "routes.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	inInternal, anywhere := false, false
	ast.Inspect(f, func(n ast.Node) bool {
		fd, ok := n.(*ast.FuncDecl)
		if !ok {
			return true
		}
		ast.Inspect(fd, func(m ast.Node) bool {
			if sel, ok := m.(*ast.SelectorExpr); ok && sel.Sel.Name == "mountRunnerPeerRoute" {
				anywhere = true
				if fd.Name.Name == "mountInternalRoutes" {
					inInternal = true
				}
			}
			return true
		})
		return false
	})
	if !anywhere || inInternal {
		t.Errorf("mountRunnerPeerRoute called = %v, inside mountInternalRoutes = %v; want called outside it", anywhere, inInternal)
	}
}

// R21: refusals of one class are logged at most once a minute, with the count since the last line.
func TestPeerRefusalWarnIsRateBound(t *testing.T) {
	logs := captureSlog(t)
	var now time.Time
	var mu sync.Mutex
	now = time.Now()
	srv, _, _, _ := newAuthzMatrixServer(t, peerShape(newPeerConfig()), func(c *Config) {
		c.Now = func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	})
	tok := remote.PeerToken{From: "replica-x", To: "y", Runner: uuid.New(), Session: uuid.New(), Method: runnerwire.MethodStatus, Exp: now.Add(10 * time.Second).Unix()}
	bad := bytes.Repeat([]byte{1}, 32)
	for range 100 {
		w := httptest.NewRecorder()
		srv.InternalHandler().ServeHTTP(w, peerRequest(tok, bad))
		if w.Code != http.StatusForbidden || w.Header().Get(remote.HeaderPeerRefused) != "" {
			t.Fatalf("bad mac = %d, class %q; want 403, no class", w.Code, w.Header().Get(remote.HeaderPeerRefused))
		}
	}
	if n := strings.Count(logs.String(), "runner peer request refused"); n != 1 {
		t.Fatalf("100 refusals in one second logged %d lines, want 1:\n%s", n, logs.String())
	}
	mu.Lock()
	now = now.Add(time.Minute + time.Second)
	mu.Unlock()
	w := httptest.NewRecorder()
	srv.InternalHandler().ServeHTTP(w, peerRequest(tok, bad))
	out := logs.String()
	lines := strings.Split(strings.TrimSpace(out), "\n")
	last := lines[len(lines)-1]
	if !strings.Contains(last, "class=bad_mac") || !strings.Contains(last, "refused=100") {
		t.Fatalf("the next line = %q, want class=bad_mac and refused=100", last)
	}
	for _, v := range []string{tok.Runner.String(), tok.Session.String(), w.Header().Get(remote.HeaderPeerMac)} {
		if v != "" && strings.Contains(out, v) {
			t.Fatalf("a refusal line holds a header value %q", v)
		}
	}
}

// R7: no log line carries a token, a mac or a key.
func TestPeerLogsNoSecrets(t *testing.T) {
	logs := captureSlog(t)
	pc := newPeerConfig()
	srv, _, _, _ := newAuthzMatrixServer(t, peerShape(pc))
	now := time.Now()
	tok := remote.PeerToken{From: "replica-x", To: "not-me", Runner: uuid.New(), Session: uuid.New(), Method: runnerwire.MethodStatus, Exp: now.Add(10 * time.Second).Unix()}
	var macs []string
	for _, key := range [][]byte{peerTestKey, bytes.Repeat([]byte{2}, 32), nil} {
		r := peerRequest(tok, key)
		macs = append(macs, r.Header.Get(remote.HeaderPeerMac))
		srv.InternalHandler().ServeHTTP(httptest.NewRecorder(), r)
	}
	out := logs.String()
	if !strings.Contains(out, "runner peer request refused") {
		t.Fatalf("no refusal was logged:\n%s", out)
	}
	secrets := []string{tok.Runner.String(), tok.Session.String(), string(peerTestKey), strconv.FormatInt(tok.Exp, 10)}
	for _, m := range macs {
		if m != "" {
			secrets = append(secrets, m)
		}
	}
	for _, s := range secrets {
		if strings.Contains(out, s) {
			t.Errorf("a log line holds %q:\n%s", s, out)
		}
	}
}

// peerFixture is a replica holding a connected runner's session, with the door served over TLS.
type peerFixture struct {
	*runnerLeaseFixture
	pc *remote.PeerConfig
	ts *httptest.Server
}

func newPeerFixture(t *testing.T) *peerFixture {
	t.Helper()
	pc := newPeerConfig()
	f := newRunnerLeaseFixture(t, peerShape(pc))
	ts := serveDoor(t, f.srv, pc)
	return &peerFixture{runnerLeaseFixture: f, pc: pc, ts: ts}
}

// session is the runner's current inventory session.
func (f *peerFixture) session(t *testing.T) uuid.UUID {
	t.Helper()
	var s uuid.UUID
	if err := f.st.Pool.QueryRow(f.ctx, `SELECT session_id FROM runner_inventories WHERE runner_id=$1`, f.rn.ID).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

// waitHolder waits until the runner's holder row names session.
func (f *peerFixture) waitHolder(t *testing.T, session uuid.UUID) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		r, ok, err := f.st.RunnerSessionHolder(f.ctx, f.rn.ID)
		if err == nil && ok && r.SessionID == session && r.ReplicaID == f.srv.replicaName() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the holder row never named session %s on this replica: %+v %v %v", session, r, ok, err)
		}
	}
}

// otherReplica is a second server on the fixture's database, with its own peer config, whose Link for the runner
// routes through the holder.
func (f *peerFixture) otherReplica(t *testing.T) *remote.Link {
	t.Helper()
	cfg := baseTestConfig(newHarness(t), f.st)
	cfg.BaseCtx = f.ctx
	peerShape(newPeerConfig())(&cfg)
	b := New(cfg)
	p := b.runnerPeering()
	if p == nil {
		t.Fatal("the second replica has no peering")
	}
	l := &remote.Link{}
	l.UsePeer(f.rn.ID, p.trans)
	return l
}

func isOffline(err error) bool { return errors.Is(err, runner.ErrRunnerOffline) }

// R1 and R1b: a call through the holder reaches the session it named; once the runner moves to a new session, a call
// under the pin answers offline, never on the new session.
func TestPeerCallRefusedAfterSessionMoves_PG(t *testing.T) {
	f := newPeerFixture(t)
	s1 := f.session(t)
	f.waitHolder(t, s1)
	link := f.otherReplica(t)
	ctx, cancel := context.WithTimeout(f.ctx, 20*time.Second)
	defer cancel()
	if err := link.Call(ctx, runnerwire.MethodStatus, map[string]string{"ref": "fake-none"}, nil); isOffline(err) {
		t.Fatalf("an unpinned call through the holder = %v, want the runner's answer", err)
	}
	sess, err := link.Pin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.Call(ctx, runnerwire.MethodStatus, map[string]string{"ref": "fake-none"}, nil); isOffline(err) {
		t.Fatalf("a pinned call through the holder = %v, want the runner's answer", err)
	}
	f.disconnect()
	f.disconnect = f.connect(t)
	s2 := f.session(t)
	if s2 == s1 {
		t.Fatal("the runner did not move to a new session")
	}
	if err := sess.Call(ctx, runnerwire.MethodStatus, map[string]string{"ref": "fake-none"}, nil); !isOffline(err) {
		t.Fatalf("a call under the old pin after the move = %v, want offline", err)
	}
	if err := sess.Call(ctx, runnerwire.MethodResize, runnerwire.ResizeArgs{Stream: 2, Cols: 1, Rows: 1}, nil); !isOffline(err) {
		t.Fatalf("a resize under the old pin after the move = %v, want offline", err)
	}
}

// R1b: a pin's stream controls after the move answer offline (the WebSocket is closed by the holder).
func TestPeerPinnedStreamRefusedAfterMove_PG(t *testing.T) {
	f := newPeerFixture(t)
	f.waitHolder(t, f.session(t))
	link := f.otherReplica(t)
	ctx, cancel := context.WithTimeout(f.ctx, 20*time.Second)
	defer cancel()
	sess, err := link.Pin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_ = sess.Call(ctx, runnerwire.MethodStatus, map[string]string{"ref": "fake-none"}, nil)
	f.disconnect()
	f.disconnect = f.connect(t)
	for _, c := range []struct {
		method string
		args   any
	}{{runnerwire.MethodResize, runnerwire.ResizeArgs{Stream: 2, Cols: 1, Rows: 1}}, {runnerwire.MethodWait, map[string]string{"ref": "fake-none"}}} {
		if err := sess.Call(ctx, c.method, c.args, nil); !isOffline(err) {
			t.Fatalf("%s under the pin after the move = %v, want offline", c.method, err)
		}
	}
	if _, err := sess.TakeStream(2); err == nil {
		t.Fatal("a stream claim under the pin after the move succeeded")
	}
}

// R2: a request without a valid token for this replica is refused before any frame reaches a runner.
func TestPeerDoorRefusesBadToken_PG(t *testing.T) {
	f := newPeerFixture(t)
	s := f.session(t)
	f.waitHolder(t, s)
	self := f.srv.replicaName()
	now := time.Now()
	good := remote.PeerToken{From: "other", To: self, Runner: f.rn.ID, Session: s, Method: runnerwire.MethodStatus, Exp: now.Add(10 * time.Second).Unix()}
	cases := []struct {
		name  string
		edit  func(*remote.PeerToken)
		key   []byte
		strip string
		class string
	}{
		{name: "no token", key: nil},
		{name: "missing header", key: peerTestKey, strip: remote.HeaderPeerMac},
		{name: "bad mac", key: bytes.Repeat([]byte{3}, 32)},
		{name: "wrong to", edit: func(t *remote.PeerToken) { t.To = "another" }, key: peerTestKey, class: peerRefusedWrongReplica},
		{name: "expired", edit: func(t *remote.PeerToken) { t.Exp = now.Add(-time.Second).Unix() }, key: peerTestKey, class: peerRefusedExpired},
		{name: "exp over 30 s ahead", edit: func(t *remote.PeerToken) { t.Exp = now.Add(time.Minute).Unix() }, key: peerTestKey, class: peerRefusedExpired},
		{name: "wrong runner", edit: func(t *remote.PeerToken) { t.Runner = uuid.New() }, key: peerTestKey, class: peerRefusedNotLiveSession},
		{name: "wrong session", edit: func(t *remote.PeerToken) { t.Session = uuid.New() }, key: peerTestKey, class: peerRefusedNotLiveSession},
		{name: "target on a runnerwire method", edit: func(t *remote.PeerToken) { t.Target = uuid.New() }, key: peerTestKey, class: peerRefusedWrongMethod},
		{name: "nil target on output", edit: func(t *remote.PeerToken) { t.Method = remote.PeerMethodOutput }, key: peerTestKey, class: peerRefusedWrongMethod},
	}
	for _, c := range cases {
		tok := good
		if c.edit != nil {
			c.edit(&tok)
		}
		r := peerRequest(tok, c.key)
		if c.strip != "" {
			r.Header.Del(c.strip)
		}
		w := httptest.NewRecorder()
		f.srv.InternalHandler().ServeHTTP(w, r)
		if w.Code != http.StatusForbidden || w.Header().Get(remote.HeaderPeerRefused) != c.class {
			t.Errorf("%s: %d, class %q; want 403, class %q", c.name, w.Code, w.Header().Get(remote.HeaderPeerRefused), c.class)
		}
	}
	// Wrong method: a valid token for status whose first call is another method closes the WebSocket.
	h := http.Header{}
	good.SetHeaders(h, peerTestKey)
	hc := &http.Client{Transport: &http.Transport{TLSClientConfig: remote.ClientTLS(*f.pc)}}
	c, resp, err := websocket.Dial(f.ctx, "wss"+strings.TrimPrefix(f.ts.URL, "https")+remote.PeerPath, &websocket.DialOptions{HTTPClient: hc, HTTPHeader: h, CompressionMode: websocket.CompressionDisabled})
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		t.Fatalf("a valid token was not upgraded: %v", err)
	}
	p := runnerwire.NewPeer(runnerwire.NewWebSocket(c), runnerwire.PeerConfig{Org: true})
	ctx, cancel := context.WithTimeout(f.ctx, 10*time.Second)
	defer cancel()
	go func() { _ = p.Run(ctx) }()
	before := f.execs.Load()
	if err := p.Call(ctx, runnerwire.MethodExec, map[string]string{"ref": "fake-none"}, nil); err == nil {
		t.Fatal("a first call of another method was served")
	}
	select {
	case <-p.Done():
	case <-ctx.Done():
		t.Fatal("the door did not close the WebSocket after a wrong method")
	}
	if f.execs.Load() != before {
		t.Fatal("a wrong-method call reached the runner")
	}
}

// R4b (api half): with this replica's row deleted under it, one touch claims it again for the current inventory
// session; with the inventory moved to another session first, nothing is written.
func TestTouchReclaimsMissingHolderRow_PG(t *testing.T) {
	f := newPeerFixture(t)
	s := f.session(t)
	f.waitHolder(t, s)
	if _, err := f.st.Pool.Exec(f.ctx, `DELETE FROM runner_session_holders WHERE runner_id=$1`, f.rn.ID); err != nil {
		t.Fatal(err)
	}
	hub := f.srv.runnerHub(f.st)
	hub.TouchHolders(f.ctx)
	f.waitHolder(t, s)
	if _, err := f.st.Pool.Exec(f.ctx, `DELETE FROM runner_session_holders WHERE runner_id=$1`, f.rn.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.st.BeginRunnerInventory(f.ctx, f.rn.ID, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	hub.TouchHolders(f.ctx)
	if r, ok, err := f.st.RunnerSessionHolder(f.ctx, f.rn.ID); err != nil || ok {
		t.Fatalf("a touch with the inventory moved wrote %+v, %v, %v", r, ok, err)
	}
}
