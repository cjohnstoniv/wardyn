// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package remote

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
)

// PeerConfig is what boot builds for replica-to-replica runner calls under high availability, before any store
// exists: this replica's internal address, the install's peer key, the internal CA and the control-plane URL's
// host, the one name the serving certificate carries.
type PeerConfig struct {
	URL        string
	Key        []byte
	Roots      *x509.CertPool
	ServerName string
}

// PeerLoop is the hub's way to the holder record: it claims a session's row after its inventory begins, touches
// the rows of the sessions whose link is up, and drops a session's row when the session ends.
type PeerLoop interface {
	Claim(ctx context.Context, runnerID, sessionID uuid.UUID) (bool, error)
	Touch(ctx context.Context, sessions []uuid.UUID) ([]uuid.UUID, error)
	Drop(ctx context.Context, runnerID, sessionID uuid.UUID) error
}

// PeerRoute is the replica that holds a runner's session, its internal address and the session it holds.
type PeerRoute struct {
	ReplicaID string
	PeerURL   string
	SessionID uuid.UUID
}

// PeerPath is the peer door on the internal listener.
const PeerPath = "/api/v1/internal/runner-peer"

// Peer door methods that are not runnerwire methods: a pin opened to take a stream, and a runner's output open
// forwarded to the run's launcher.
const (
	PeerMethodStream = "stream"
	PeerMethodOutput = "output"
)

// PeerTokenTTL is the longest a peer token is valid for; the door refuses an exp further ahead.
const PeerTokenTTL = 30 * time.Second

// peerTokenLife is how far ahead the dialer sets exp, under PeerTokenTTL so a small clock difference is admitted.
const peerTokenLife = 20 * time.Second

// The peer token's headers.
const (
	HeaderPeerFrom    = "Wardyn-Peer-From"
	HeaderPeerTo      = "Wardyn-Peer-To"
	HeaderPeerRunner  = "Wardyn-Peer-Runner"
	HeaderPeerSession = "Wardyn-Peer-Session"
	HeaderPeerMethod  = "Wardyn-Peer-Method"
	HeaderPeerExp     = "Wardyn-Peer-Exp"
	HeaderPeerTarget  = "Wardyn-Peer-Target"
	HeaderPeerMac     = "Wardyn-Peer-Mac"
	// HeaderPeerRefused carries a refusal's class once the mac has verified.
	HeaderPeerRefused = "Wardyn-Peer-Refused"
)

// PeerToken is one peer WebSocket's credential: both replicas, the runner, the session, the method of the pin's
// first call, its expiry (unix seconds) and the target run (the nil UUID for every method but output).
type PeerToken struct {
	From, To        string
	Runner, Session uuid.UUID
	Method          string
	Exp             int64
	Target          uuid.UUID
}

// peerTokenVersion is the encoding's version byte, bound into the mac.
const peerTokenVersion = 0x01

// encode is the bytes the mac covers: the version, then each field as a 4-byte big-endian length and its bytes,
// in the order from, to, runner, session, method, exp, target.
func (t PeerToken) encode() []byte {
	var b bytes.Buffer
	b.WriteByte(peerTokenVersion)
	field := func(v []byte) {
		var n [4]byte
		binary.BigEndian.PutUint32(n[:], uint32(len(v)))
		b.Write(n[:])
		b.Write(v)
	}
	var exp [8]byte
	binary.BigEndian.PutUint64(exp[:], uint64(t.Exp))
	field([]byte(t.From))
	field([]byte(t.To))
	field(t.Runner[:])
	field(t.Session[:])
	field([]byte(t.Method))
	field(exp[:])
	field(t.Target[:])
	return b.Bytes()
}

// MAC is HMAC-SHA256 under key of the token's encoding.
func (t PeerToken) MAC(key []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(t.encode())
	return m.Sum(nil)
}

// SetHeaders writes the token and its mac under key.
func (t PeerToken) SetHeaders(h http.Header, key []byte) {
	h.Set(HeaderPeerFrom, t.From)
	h.Set(HeaderPeerTo, t.To)
	h.Set(HeaderPeerRunner, t.Runner.String())
	h.Set(HeaderPeerSession, t.Session.String())
	h.Set(HeaderPeerMethod, t.Method)
	h.Set(HeaderPeerExp, strconv.FormatInt(t.Exp, 10))
	h.Set(HeaderPeerTarget, t.Target.String())
	h.Set(HeaderPeerMac, base64.RawURLEncoding.EncodeToString(t.MAC(key)))
}

// ErrPeerTokenMissing is a request with a peer field absent or unreadable.
var ErrPeerTokenMissing = errors.New("runner peer token: missing field")

// ReadPeerToken reads the token and its mac from h. Any absent or unreadable field is ErrPeerTokenMissing.
func ReadPeerToken(h http.Header) (PeerToken, []byte, error) {
	var t PeerToken
	vals := map[string]string{}
	for _, k := range []string{HeaderPeerFrom, HeaderPeerTo, HeaderPeerRunner, HeaderPeerSession, HeaderPeerMethod, HeaderPeerExp, HeaderPeerTarget, HeaderPeerMac} {
		v := h.Get(k)
		if v == "" {
			return PeerToken{}, nil, ErrPeerTokenMissing
		}
		vals[k] = v
	}
	var err error
	t.From, t.To, t.Method = vals[HeaderPeerFrom], vals[HeaderPeerTo], vals[HeaderPeerMethod]
	if t.Runner, err = uuid.Parse(vals[HeaderPeerRunner]); err != nil {
		return PeerToken{}, nil, ErrPeerTokenMissing
	}
	if t.Session, err = uuid.Parse(vals[HeaderPeerSession]); err != nil {
		return PeerToken{}, nil, ErrPeerTokenMissing
	}
	if t.Target, err = uuid.Parse(vals[HeaderPeerTarget]); err != nil {
		return PeerToken{}, nil, ErrPeerTokenMissing
	}
	if t.Exp, err = strconv.ParseInt(vals[HeaderPeerExp], 10, 64); err != nil {
		return PeerToken{}, nil, ErrPeerTokenMissing
	}
	mac, err := base64.RawURLEncoding.DecodeString(vals[HeaderPeerMac])
	if err != nil {
		return PeerToken{}, nil, ErrPeerTokenMissing
	}
	return t, mac, nil
}

// Verify reports whether mac is the token's mac under key.
func (t PeerToken) Verify(key, mac []byte) bool { return hmac.Equal(t.MAC(key), mac) }

// PeerTransport sends a call or a pinned stream to the replica that holds a runner's session. Lookup reads the
// runner's fresh holder row; it is the store's, which this package does not import.
type PeerTransport struct {
	cfg    PeerConfig
	self   string
	lookup func(ctx context.Context, runnerID uuid.UUID) (PeerRoute, bool, error)
	// dial opens the peer WebSocket; a test replaces it.
	dial func(ctx context.Context, route PeerRoute, h http.Header) (runnerwire.Conn, error)
	now  func() time.Time
}

// NewPeerTransport is the peer client of replica self.
func NewPeerTransport(cfg PeerConfig, self string, lookup func(ctx context.Context, runnerID uuid.UUID) (PeerRoute, bool, error)) *PeerTransport {
	pt := &PeerTransport{cfg: cfg, self: self, lookup: lookup, now: time.Now}
	pt.dial = pt.dialWebSocket
	return pt
}

// Self is this replica's id.
func (pt *PeerTransport) Self() string { return pt.self }

// ClientTLS is the peer client's TLS config: the internal CA, verified by the control-plane URL's host while it
// dials the pod address, HTTP/1.1 for the WebSocket upgrade, TLS 1.3.
func ClientTLS(cfg PeerConfig) *tls.Config {
	return &tls.Config{RootCAs: cfg.Roots, ServerName: cfg.ServerName, NextProtos: []string{"http/1.1"}, MinVersion: tls.VersionTLS13}
}

func (pt *PeerTransport) dialWebSocket(ctx context.Context, route PeerRoute, h http.Header) (runnerwire.Conn, error) {
	u := strings.TrimSuffix(route.PeerURL, "/") + PeerPath
	u = "wss" + strings.TrimPrefix(strings.TrimPrefix(u, "https"), "wss")
	hc := &http.Client{Transport: &http.Transport{TLSClientConfig: ClientTLS(pt.cfg), ForceAttemptHTTP2: false}}
	c, resp, err := websocket.Dial(ctx, u, &websocket.DialOptions{HTTPClient: hc, HTTPHeader: h, CompressionMode: websocket.CompressionDisabled})
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		return nil, err
	}
	return runnerwire.NewWebSocket(c), nil
}

// route reads the runner's holder. A row naming this replica, or none, is no route: a request never comes back
// here, and this replica has no session for the runner.
func (pt *PeerTransport) route(ctx context.Context, runnerID uuid.UUID) (PeerRoute, bool) {
	r, ok, err := pt.lookup(ctx, runnerID)
	if err != nil || !ok || r.ReplicaID == pt.self || r.PeerURL == "" {
		return PeerRoute{}, false
	}
	return r, true
}

// open dials route's door for one pin whose first operation is method, and runs the org end of a runnerwire peer
// over it. The peer ends when ctx ends or the door closes.
func (pt *PeerTransport) open(ctx context.Context, runnerID uuid.UUID, route PeerRoute, method string, target uuid.UUID) (*runnerwire.Peer, error) {
	tok := PeerToken{From: pt.self, To: route.ReplicaID, Runner: runnerID, Session: route.SessionID, Method: method,
		Exp: pt.now().Add(peerTokenLife).Unix(), Target: target}
	h := http.Header{}
	tok.SetHeaders(h, pt.cfg.Key)
	dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	conn, err := pt.dial(dctx, route, h)
	cancel()
	if err != nil {
		return nil, errOffline
	}
	p := runnerwire.NewPeer(conn, runnerwire.PeerConfig{Org: true})
	go func() { _ = p.Run(context.WithoutCancel(ctx)) }()
	return p, nil
}

// call is one unpinned call through the holder: the holder row is read at this call, and the WebSocket lives for
// this call only.
func (pt *PeerTransport) call(ctx context.Context, runnerID uuid.UUID, method string, args, result any, opts ...runnerwire.CallOption) error {
	route, ok := pt.route(ctx, runnerID)
	if !ok {
		return errOffline
	}
	p, err := pt.open(ctx, runnerID, route, method, uuid.Nil)
	if err != nil {
		return err
	}
	defer p.Close()
	return peerErr(p, p.Call(ctx, method, args, result, opts...))
}

// peerErr answers a call whose peer link went down (the door closed it: the session moved, or the request was
// refused) as the runner being offline here, never as another session's answer.
func peerErr(p *runnerwire.Peer, err error) error {
	if err != nil && (!p.Online() || errors.Is(err, runnerwire.ErrLinkDown)) {
		return errOffline
	}
	return err
}

// peerPinned is a pin on another replica's session for the runner: every call and stream claim goes to the
// replica and session captured when the pin was made, over one WebSocket opened at the pin's first operation. It
// never re-reads the holder row, never falls back to a local session that appears later, and never follows a new
// holder.
type peerPinned struct {
	pt     *PeerTransport
	runner uuid.UUID
	route  PeerRoute

	mu sync.Mutex
	p  *runnerwire.Peer
}

func (s *peerPinned) peer(ctx context.Context, method string) (*runnerwire.Peer, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.p != nil {
		if !s.p.Online() {
			return nil, errOffline
		}
		return s.p, nil
	}
	p, err := s.pt.open(ctx, s.runner, s.route, method, uuid.Nil)
	if err != nil {
		return nil, err
	}
	s.p = p
	return p, nil
}

func (s *peerPinned) Call(ctx context.Context, method string, args, result any, opts ...runnerwire.CallOption) error {
	p, err := s.peer(ctx, method)
	if err != nil {
		return err
	}
	return peerErr(p, p.Call(ctx, method, args, result, opts...))
}

// TakeStream claims a stream a call on this pin opened. Before any call there is no WebSocket and no stream.
func (s *peerPinned) TakeStream(id uint32) (*runnerwire.Stream, error) {
	s.mu.Lock()
	p := s.p
	s.mu.Unlock()
	if p == nil {
		return nil, runnerwire.ErrNoStream
	}
	st, err := p.TakeStream(id)
	if err != nil && !p.Online() {
		return nil, errOffline
	}
	return st, err
}

// ServePeer serves one peer WebSocket on this hub: the holder's end of a pin the token names. It is refused
// errOffline unless this replica holds runnerID's session sessionID now. Every call is replayed on that pinned
// session and nowhere else; a request is never sent on to another replica. The first call must be the token's
// method, else refused is told "wrong_method" and the WebSocket is closed. It returns when the WebSocket ends,
// and closes it once the pinned session is replaced.
func (h *Hub) ServePeer(ctx context.Context, conn runnerwire.Conn, runnerID, sessionID uuid.UUID, method string, refused func(class string)) error {
	pin, ok := h.pinHeld(runnerID, sessionID)
	if !ok {
		return errOffline
	}
	return servePin(ctx, conn, pin, method, refused)
}

// servePin runs the holder's end of one peer WebSocket on pin.
func servePin(ctx context.Context, conn runnerwire.Conn, pin pinnedPeer, method string, refused func(class string)) error {
	r := &peerRelay{pin: pin, method: method, refused: refused, toRunner: map[uint32]uint32{}}
	ws := runnerwire.NewPeer(conn, runnerwire.PeerConfig{Org: false, OnCall: r.serveCall})
	r.ws = ws
	go func() {
		select {
		case <-pin.p.Done():
		case <-ws.Done():
		}
		ws.Close()
	}()
	return ws.Run(ctx)
}

// HoldsSession reports whether this replica holds runnerID's session sessionID ready now.
func (h *Hub) HoldsSession(runnerID, sessionID uuid.UUID) bool {
	_, ok := h.pinHeld(runnerID, sessionID)
	return ok
}

// pinHeld is the local pin on runnerID's session sessionID, when this replica holds it ready now. It never takes
// the Link's peer path: one hop only.
func (h *Hub) pinHeld(runnerID, sessionID uuid.UUID) (pinnedPeer, bool) {
	h.mu.Lock()
	s := h.sessions[runnerID]
	h.mu.Unlock()
	if s == nil || s.link == nil {
		return pinnedPeer{}, false
	}
	return s.link.pinLocal(sessionID, s.readyID.Load())
}

// pinLocal pins the current session when it is up and readyID names sessionID.
func (l *Link) pinLocal(sessionID uuid.UUID, readyID *string) (pinnedPeer, bool) {
	p := l.current()
	if p == nil || !l.Online() || readyID == nil || *readyID != sessionID.String() || l.current() != p {
		return pinnedPeer{}, false
	}
	return pinnedPeer{l: l, p: p}, true
}

// peerRelay replays a peer WebSocket's calls on one pinned runner session. A byte stream the runner opens for a
// call is opened again on the WebSocket for the same call and copied both ways; stream ids in a call's arguments
// and reply are translated between the two sides.
type peerRelay struct {
	pin     pinnedPeer
	ws      *runnerwire.Peer
	method  string
	refused func(class string)

	mu       sync.Mutex
	first    bool
	toRunner map[uint32]uint32 // WebSocket stream id -> runner stream id
}

// peerStreamKeys are the top-level argument and reply fields that name a byte stream.
var peerStreamKeys = []string{"stream", "exec", "stderr"}

func (r *peerRelay) serveCall(ctx context.Context, id uint32, method string, args json.RawMessage) (any, error) {
	r.mu.Lock()
	if !r.first {
		r.first = true
		if method != r.method {
			r.mu.Unlock()
			if r.refused != nil {
				r.refused("wrong_method")
			}
			r.ws.Close()
			return nil, errOffline
		}
	}
	r.mu.Unlock()
	if r.pin.l.current() != r.pin.p {
		r.ws.Close()
		return nil, errOffline
	}
	args = r.rewrite(args, func(ws uint32) (uint32, bool) {
		r.mu.Lock()
		defer r.mu.Unlock()
		x, ok := r.toRunner[ws]
		return x, ok
	})
	var in any // no arguments stay none
	if len(args) > 0 {
		in = args
	}
	var raw json.RawMessage
	err := r.pin.Call(ctx, method, in, &raw, runnerwire.WithEvents(func(ev runnerwire.Event) {
		ev.Call = id
		_ = r.ws.Event(ctx, ev)
	}))
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, nil
	}
	return r.rewrite(raw, func(x uint32) (uint32, bool) { return r.relayStream(ctx, id, x) }), nil
}

// rewrite replaces each stream id field of a JSON object with what swap gives; any other value is kept.
func (r *peerRelay) rewrite(raw json.RawMessage, swap func(uint32) (uint32, bool)) json.RawMessage {
	var m map[string]json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &m) != nil {
		return raw
	}
	changed := false
	for _, k := range peerStreamKeys {
		v, ok := m[k]
		if !ok {
			continue
		}
		var n uint32
		if json.Unmarshal(v, &n) != nil || n == 0 {
			continue
		}
		if to, ok := swap(n); ok {
			m[k], _ = json.Marshal(to)
			changed = true
		}
	}
	if !changed {
		return raw
	}
	out, err := json.Marshal(m)
	if err != nil {
		return raw
	}
	return out
}

// relayStream takes the runner's stream x for call id on the pinned session, opens its twin on the WebSocket for
// the same call, and copies bytes both ways until either side ends.
func (r *peerRelay) relayStream(ctx context.Context, id, x uint32) (uint32, bool) {
	st, err := r.pin.TakeStream(x)
	if err != nil {
		return 0, false
	}
	tw, err := r.ws.Open(ctx, runnerwire.Open{Kind: st.Kind(), Call: id})
	if err != nil {
		_ = st.Close()
		return 0, false
	}
	r.mu.Lock()
	r.toRunner[tw.ID()] = x
	r.mu.Unlock()
	go peerCopy(tw, st)
	go peerCopy(st, tw)
	return tw.ID(), true
}

// peerCopy copies src to dst: an end of src closes dst for writes, and an error resets dst.
func peerCopy(dst, src *runnerwire.Stream) {
	if _, err := io.Copy(dst, src); err != nil {
		dst.Abort(runnerwire.ResetCancelled)
		return
	}
	_ = dst.CloseWrite()
}

// peerTouchEvery is how often the hub touches the holder rows of the sessions whose link is up.
var peerTouchEvery = 10 * time.Second

// claimHolder records this replica as the holder of the runner's new session. A failed claim is retried by the
// next touch, which claims again for a session with no row.
func (h *Hub) claimHolder(ctx context.Context, runnerID uuid.UUID, session string) {
	sid, err := uuid.Parse(session)
	if h.cfg.Peer == nil || err != nil {
		return
	}
	_, _ = h.cfg.Peer.Claim(ctx, runnerID, sid)
}

// dropHolder deletes the ended session's row, only while it still names that session.
func (h *Hub) dropHolder(runnerID uuid.UUID, session string) {
	sid, err := uuid.Parse(session)
	if h.cfg.Peer == nil || err != nil {
		return
	}
	_ = h.cfg.Peer.Drop(h.ctx, runnerID, sid)
}

// peerLoop touches the holder rows of every session whose link is up, and claims again for one the touch did not
// return (another replica's drop, or a restore deleted its row); the claim is still gated on the inventory session.
// A session in its resume window with no link is not touched, so its row ages out of every route.
func (h *Hub) peerLoop() {
	t := time.NewTicker(peerTouchEvery)
	defer t.Stop()
	for {
		select {
		case <-h.ctx.Done():
			return
		case <-t.C:
			h.touchHolders(h.ctx)
		}
	}
}

func (h *Hub) touchHolders(ctx context.Context) {
	held := h.heldSessions()
	if len(held) == 0 {
		return
	}
	sessions := make([]uuid.UUID, 0, len(held))
	for sid := range held {
		sessions = append(sessions, sid)
	}
	tctx, cancel := context.WithTimeout(ctx, peerTouchEvery)
	defer cancel()
	touched, err := h.cfg.Peer.Touch(tctx, sessions)
	if err != nil {
		return
	}
	for _, sid := range touched {
		delete(held, sid)
	}
	for sid, runnerID := range held {
		_, _ = h.cfg.Peer.Claim(tctx, runnerID, sid)
	}
}

// heldSessions is every session this hub holds with its link up, by session id.
func (h *Hub) heldSessions() map[uuid.UUID]uuid.UUID {
	h.mu.Lock()
	type held struct {
		id uuid.UUID
		s  *hubSession
	}
	all := make([]held, 0, len(h.sessions))
	for id, s := range h.sessions {
		all = append(all, held{id, s})
	}
	h.mu.Unlock()
	out := map[uuid.UUID]uuid.UUID{}
	for _, e := range all {
		rid := e.s.readyID.Load()
		if rid == nil || e.s.link == nil || !e.s.link.Online() {
			continue
		}
		if sid, err := uuid.Parse(*rid); err == nil {
			out[sid] = e.id
		}
	}
	return out
}
