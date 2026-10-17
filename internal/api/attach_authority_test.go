// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// attachRevocations is a revocation store that orders cuts by a sequence, as the Postgres one does:
// RevokeSub (admin revoke) ends every credential, CutSessions (sign-out, session-only cut) ends the
// lookups that read cuts. failNext and hangNext make the next lookups fail or hang until their
// context ends.
type attachRevocations struct {
	mu       sync.Mutex
	seq      int64
	revoked  map[string]int64
	cut      map[string]int64
	failNext int
	hangNext int
	asked    int
}

func newAttachRevocations() *attachRevocations {
	return &attachRevocations{seq: 10, revoked: map[string]int64{}, cut: map[string]int64{}}
}

func (r *attachRevocations) current() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.seq
}

func (r *attachRevocations) lookup(ctx context.Context, sub, email string, seq int64, cuts bool) (bool, error) {
	r.mu.Lock()
	r.asked++
	fail, hang := r.failNext > 0, r.hangNext > 0
	if fail {
		r.failNext--
	}
	if hang {
		r.hangNext--
	}
	var high int64
	for _, k := range []string{sub, email, "*"} {
		if k == "" {
			continue
		}
		high = max(high, r.revoked[k])
		if cuts {
			high = max(high, r.cut[k])
		}
	}
	r.mu.Unlock()
	if hang {
		<-ctx.Done()
		return false, ctx.Err()
	}
	if fail {
		return false, errors.New("revocation store unreachable")
	}
	return high > seq, nil
}

func (r *attachRevocations) IsSessionRevoked(ctx context.Context, sub, email string, _ time.Time) (bool, error) {
	return r.lookup(ctx, sub, email, 0, true)
}

func (r *attachRevocations) IsSessionRevokedSeq(ctx context.Context, sub, email string, seq int64) (bool, error) {
	return r.lookup(ctx, sub, email, seq, true)
}

func (r *attachRevocations) RevocationSeq(context.Context) (int64, error) { return r.current(), nil }

func (r *attachRevocations) SessionStatus(ctx context.Context, sub, email string, issue oidc.Issue, epoch int64) (oidc.SessionStatus, error) {
	revoked, err := r.lookup(ctx, sub, email, issue.Seq, epoch >= 0)
	if err != nil || !revoked {
		return oidc.SessionLive, err
	}
	return oidc.SessionRevoked, nil
}

func (r *attachRevocations) stamp(m map[string]int64, sub string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seq++
	m[sub] = r.seq
	return nil
}

func (r *attachRevocations) RevokeSub(_ context.Context, sub string) error {
	return r.stamp(r.revoked, sub)
}
func (r *attachRevocations) CutSessions(_ context.Context, sub string) error {
	return r.stamp(r.cut, sub)
}
func (r *attachRevocations) RevokeAll(context.Context) error { return r.stamp(r.revoked, "*") }

func (r *attachRevocations) setFail(n int) { r.mu.Lock(); r.failNext = n; r.mu.Unlock() }
func (r *attachRevocations) setHang(n int) { r.mu.Lock(); r.hangNext = n; r.mu.Unlock() }

var (
	_ oidc.IdentityRevocations = (*attachRevocations)(nil)
	_ oidc.RevocationOrder     = (*attachRevocations)(nil)
	_ oidc.SessionCutter       = (*attachRevocations)(nil)
)

// authorityRig is an attach server with a real OIDC authenticator (so the session cookie, sign-out
// and the handshake check are the production ones), a sequenced revocation store, a clock the test
// moves, and a millisecond authority interval.
type authorityRig struct {
	srv    *Server
	ts     *httptest.Server
	ast    *authzStore
	rev    *attachRevocations
	run    types.AgentRun
	audit  *sshTestRecorder
	offset atomic.Int64 // added to the server's clock
}

const authorityTick = 20 * time.Millisecond

func newAuthorityRig(t *testing.T, roleStampTTL time.Duration) *authorityRig {
	t.Helper()
	rig := &authorityRig{ast: newAuthzStore(), rev: newAttachRevocations()}
	st := &touchCountingStore{authzStore: rig.ast}
	cfg := baseTestConfig(newHarness(t), st)
	rig.audit = &sshTestRecorder{}
	cfg.Audit = rig.audit
	cfg.Runner = &holderTestRunner{}
	cfg.OIDC = newAccessAuth(t, nil, "", nil, nil, func(c *oidc.Config) {
		c.Revocations = rig.rev
		c.RoleStampTTL = roleStampTTL
	})
	cfg.SessionRevocations = rig.rev
	cfg.RoleStampTTL = roleStampTTL
	cfg.Now = func() time.Time { return time.Now().Add(time.Duration(rig.offset.Load())) }
	rig.srv = New(cfg)
	rig.srv.authorityTest = attachAuthorityTimings{every: authorityTick, lookup: 100 * time.Millisecond}
	rig.run = types.AgentRun{ID: uuid.New(), CreatedBy: holderOwner, OperatorOwned: true, State: types.RunRunning, SandboxRef: "sbx-1"}
	rig.ast.mu.Lock()
	rig.ast.runs[rig.run.ID] = rig.run
	rig.ast.mu.Unlock()
	rig.ts = httptest.NewServer(panicFails(t, rig.srv.Handler()))
	t.Cleanup(rig.ts.Close)
	return rig
}

// cookie is an admin session cookie for sub, signed in now, recording the current revocation sequence.
func (rig *authorityRig) cookie(t *testing.T, sub string, expiry time.Time) *http.Cookie {
	t.Helper()
	payload, err := json.Marshal(oidc.Session{
		V: oidc.SessionCodecVersion, Sub: sub, Email: sub, Role: oidc.RoleAdmin, UserType: types.UserTypeStandard,
		Expiry: expiry, IssuedAt: time.Now().UTC(), RevocationSeq: rig.rev.current(), Groups: []string{},
	})
	if err != nil {
		t.Fatal(err)
	}
	mac := hmac.New(sha256.New, accessTestHMACKey)
	mac.Write(payload)
	return &http.Cookie{Name: "wardyn_session",
		Value: base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))}
}

func (rig *authorityRig) post(t *testing.T, path string, cookie *http.Cookie) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, rig.ts.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Origin", rig.ts.URL)
	req.AddCookie(cookie)
	resp, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp
}

// ticket mints an attach ticket the way the console does, through the cookie.
func (rig *authorityRig) ticket(t *testing.T, cookie *http.Cookie) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, rig.ts.URL+"/api/v1/runs/"+rig.run.ID.String()+"/attach-ticket", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Origin", rig.ts.URL)
	req.AddCookie(cookie)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Ticket string `json:"ticket"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&out) != nil || out.Ticket == "" {
		t.Fatalf("mint attach ticket: %d", resp.StatusCode)
	}
	return out.Ticket
}

// dial opens the attach socket with ?ticket= (ticket lane) and/or the cookie (cookie lane), reads the
// attach-mode frame, checks the mode and then drains the socket, returning where its read ended.
func (rig *authorityRig) dial(t *testing.T, cookie *http.Cookie, ticket string, wantReadOnly bool) (*websocket.Conn, <-chan error) {
	t.Helper()
	url := "ws" + strings.TrimPrefix(rig.ts.URL, "http") + "/api/v1/runs/" + rig.run.ID.String() + "/attach"
	opts := &websocket.DialOptions{}
	if ticket != "" {
		url += "?ticket=" + ticket
	}
	if cookie != nil {
		opts.HTTPHeader = http.Header{"Cookie": {cookie.String()}}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, url, opts)
	if err != nil {
		t.Fatalf("dial attach: %v", err)
	}
	t.Cleanup(func() { _ = c.CloseNow() })
	if mode := readAttachMode(t, c); mode.ReadOnly != wantReadOnly {
		t.Fatalf("attach mode read_only = %v, want %v", mode.ReadOnly, wantReadOnly)
	}
	done := make(chan error, 1)
	go func() {
		for {
			if _, _, err := c.Read(context.Background()); err != nil {
				done <- err
				return
			}
		}
	}()
	return c, done
}

// pair opens a writer and an observer socket for alice on the lane: "cookie", "ticket" (a ticket the
// cookie minted, presented alone) or "ticket+cookie" (the ticket with the cookie beside it, as the
// console's handshake carries it).
func (rig *authorityRig) pair(t *testing.T, lane string, cookie *http.Cookie) [2]<-chan error {
	t.Helper()
	var ends [2]<-chan error
	for i, readOnly := range []bool{false, true} {
		tk, sent := "", cookie
		if lane != "cookie" {
			tk = rig.ticket(t, cookie)
		}
		if lane == "ticket" {
			sent = nil
		}
		_, ends[i] = rig.dial(t, sent, tk, readOnly)
	}
	return ends
}

// wantDetachReasons waits for n session.detach rows and holds each to reason: a session the credential
// re-check ended records that, not the pump's own reason for returning.
func (rig *authorityRig) wantDetachReasons(t *testing.T, n int, reason string) {
	t.Helper()
	var rows []types.AuditEvent
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		rows = rows[:0]
		for _, ev := range rig.audit.snapshot() {
			if ev.Action == "session.detach" && ev.RunID != nil && *ev.RunID == rig.run.ID {
				rows = append(rows, ev)
			}
		}
		if len(rows) >= n {
			break
		}
	}
	if len(rows) != n {
		t.Fatalf("session.detach rows = %d, want %d", len(rows), n)
	}
	for _, ev := range rows {
		if !strings.Contains(string(ev.Data), `"reason":"`+reason+`"`) {
			t.Errorf("session.detach = %s, want reason %q", ev.Data, reason)
		}
	}
}

func wantAuthEnded(t *testing.T, ends [2]<-chan error) {
	t.Helper()
	for i, end := range ends {
		select {
		case err := <-end:
			if got := websocket.CloseStatus(err); got != attachAuthEndedCode {
				t.Fatalf("socket %d (0 writer, 1 observer) ended with %v (status %d), want close %d", i, err, got, attachAuthEndedCode)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("socket %d (0 writer, 1 observer) is still open after the cut", i)
		}
	}
}

// TestAttachAuthority_CutsEndOpenSockets: every way a sign-in stops authorizing ends an open terminal,
// writer and observer, on both lanes, at the first check.
func TestAttachAuthority_CutsEndOpenSockets(t *testing.T) {
	type cut struct {
		name  string
		ttl   time.Duration
		lanes []string
		apply func(t *testing.T, rig *authorityRig, cookie *http.Cookie)
	}
	both := []string{"cookie", "ticket"}
	cuts := []cut{
		{name: "sign-out", lanes: both, apply: func(t *testing.T, rig *authorityRig, cookie *http.Cookie) {
			if resp := rig.post(t, "/api/v1/auth/logout", cookie); resp.StatusCode != http.StatusFound {
				t.Fatalf("sign-out: %d", resp.StatusCode)
			}
		}},
		{name: "session-only cut", lanes: both, apply: func(t *testing.T, rig *authorityRig, _ *http.Cookie) {
			_ = rig.rev.CutSessions(context.Background(), holderOwner)
		}},
		{name: "admin revoke", lanes: both, apply: func(t *testing.T, rig *authorityRig, _ *http.Cookie) {
			_ = rig.rev.RevokeSub(context.Background(), holderOwner)
		}},
		{name: "one failed lookup", lanes: both, apply: func(t *testing.T, rig *authorityRig, _ *http.Cookie) {
			rig.rev.setFail(2) // one for each socket: each must end on its own first failure
		}},
		{name: "one lookup past its bound", lanes: both, apply: func(t *testing.T, rig *authorityRig, _ *http.Cookie) {
			rig.rev.setHang(2)
		}},
		{name: "cookie expiry", lanes: []string{"cookie", "ticket+cookie"}, apply: func(t *testing.T, rig *authorityRig, _ *http.Cookie) {
			rig.offset.Store(int64(time.Hour + time.Second))
		}},
		{name: "role-stamp deadline before the cookie expiry", ttl: 30 * time.Minute, lanes: []string{"cookie", "ticket", "ticket+cookie"},
			apply: func(t *testing.T, rig *authorityRig, _ *http.Cookie) {
				rig.offset.Store(int64(31 * time.Minute))
			}},
	}
	for _, c := range cuts {
		for _, lane := range c.lanes {
			t.Run(lane+"/"+c.name, func(t *testing.T) {
				rig := newAuthorityRig(t, c.ttl)
				cookie := rig.cookie(t, holderOwner, time.Now().UTC().Add(time.Hour))
				ends := rig.pair(t, lane, cookie)
				c.apply(t, rig, cookie)
				wantAuthEnded(t, ends)
				rig.wantDetachReasons(t, 2, attachAuthEndedReason)
			})
		}
	}
}

// TestAttachAuthority_TicketDelegationWithdrawn: a terminal opened through a portal ends when the
// portal's grant is withdrawn.
func TestAttachAuthority_TicketDelegationWithdrawn(t *testing.T) {
	rig := newAuthorityRig(t, 0)
	_, via := seedDelegation(t, rig.ast.fakeDelegateStore, holderOwner)
	var ends [2]<-chan error
	for i, readOnly := range []bool{false, true} {
		tok, err := mintAttachTicket(audit.WithDelegation(context.Background(), via), rig.ast, rig.run.ID, types.ActorHuman,
			holderOwner, oidc.RoleAdmin, oidc.Issue{At: time.Now(), Seq: rig.rev.current()})
		if err != nil {
			t.Fatal(err)
		}
		_, ends[i] = rig.dial(t, nil, tok, readOnly)
	}
	if _, err := rig.ast.RevokeDelegate(context.Background(), via.Delegate, time.Now()); err != nil {
		t.Fatal(err)
	}
	wantAuthEnded(t, ends)
}

// TestAttachAuthority_LiveSessionStaysOpen (characterization): a live session keeps its writer and
// observer sockets across many check intervals, on both lanes: a sign-in after an earlier cut, a cut
// of another person and a clock short of both deadlines end nothing.
func TestAttachAuthority_LiveSessionStaysOpen(t *testing.T) {
	for _, lane := range []string{"cookie", "ticket", "ticket+cookie"} {
		t.Run(lane, func(t *testing.T) {
			rig := newAuthorityRig(t, 30*time.Minute)
			_ = rig.rev.CutSessions(context.Background(), holderOwner) // an earlier sign-out
			cookie := rig.cookie(t, holderOwner, time.Now().UTC().Add(time.Hour))
			ends := rig.pair(t, lane, cookie)
			_ = rig.rev.CutSessions(context.Background(), holderSecond)
			rig.offset.Store(int64(29 * time.Minute))
			time.Sleep(10 * authorityTick)
			for i, end := range ends {
				select {
				case err := <-end:
					t.Fatalf("socket %d ended on a live session: %v", i, err)
				default:
				}
			}
		})
	}
}

type closeOrder struct {
	mu     sync.Mutex
	events []string
	holder *attachHolder
	done   chan struct{}
}

func (o *closeOrder) Close(code websocket.StatusCode, reason string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.holder.evicted.Load() {
		o.events = append(o.events, "close-before-input-stopped")
	}
	o.events = append(o.events, "close "+reason)
	if code != attachAuthEndedCode {
		o.events = append(o.events, "wrong code")
	}
	return nil
}

// TestEndAttachAuthority_ClosesBeforeCancel: input stops first, then the close frame is sent, and
// only then is the socket's context cancelled.
func TestEndAttachAuthority_ClosesBeforeCancel(t *testing.T) {
	o := &closeOrder{holder: &attachHolder{}, done: make(chan struct{})}
	endAttachAuthority(o, o.holder, func() {
		o.mu.Lock()
		o.events = append(o.events, "cancel")
		o.mu.Unlock()
		close(o.done)
	})
	select {
	case <-o.done:
	case <-time.After(3 * time.Second):
		t.Fatal("the socket context was never cancelled")
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if got := strings.Join(o.events, ", "); got != "close session ended, cancel" {
		t.Fatalf("order = %q, want the close frame, then cancel", got)
	}
}
