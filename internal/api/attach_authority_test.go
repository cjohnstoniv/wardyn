// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"cmp"
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
	"github.com/cjohnstoniv/wardyn/internal/store"
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
	failed   int // lookups that answered with an error
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
		r.mu.Lock()
		r.failed++
		r.mu.Unlock()
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

// failedReads is how many lookups have answered with an error.
func (r *attachRevocations) failedReads() int { r.mu.Lock(); defer r.mu.Unlock(); return r.failed }
func (r *attachRevocations) setHang(n int)    { r.mu.Lock(); r.hangNext = n; r.mu.Unlock() }

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
	units  *authorityUnits
	rev    *attachRevocations
	run    types.AgentRun
	audit  *sshTestRecorder
	offset atomic.Int64 // added to the server's clock
}

const authorityTick = 20 * time.Millisecond

// authorityUnits is the rig's store with the governance profiles a test sets (none by default).
type authorityUnits struct {
	*touchCountingStore
	mu       sync.Mutex
	profiles []types.GovernanceUnit
}

func (s *authorityUnits) GetGovernanceUnitChain(_ context.Context, id uuid.UUID) ([]types.GovernanceUnit, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return unitChainOf(s.profiles, nil, id)
}

func (s *authorityUnits) set(u types.GovernanceUnit) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.profiles = []types.GovernanceUnit{u}
}

func newAuthorityRig(t *testing.T, roleStampTTL time.Duration) *authorityRig {
	t.Helper()
	return newAuthorityRigWith(t, roleStampTTL, nil)
}

// newAuthorityRigWith is newAuthorityRig with configure applied to the server's config before it is built.
func newAuthorityRigWith(t *testing.T, roleStampTTL time.Duration, configure func(*Config)) *authorityRig {
	t.Helper()
	rig := &authorityRig{ast: newAuthzStore(), rev: newAttachRevocations()}
	rig.units = &authorityUnits{touchCountingStore: &touchCountingStore{authzStore: rig.ast}}
	cfg := baseTestConfig(newHarness(t), rig.units)
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
	if configure != nil {
		configure(&cfg)
	}
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
// writer and observer, on both lanes, at the first check. A lookup that fails or runs out of its bound is not a
// cut: it rides the session budget (TestAttachAuthority_RidesAnUnansweredCheck, ..._ZeroBudget...).
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
	if _, _, err := rig.ast.RevokeDelegate(context.Background(), via.Delegate, time.Now(), false); err != nil {
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

// TestAttachAuthority_AnotherPersonsCookieDoesNotBoundATicket: a ticket presented beside another
// person's session cookie keeps its own bound, writer and observer: that cookie's earlier deadline
// ends nothing.
func TestAttachAuthority_AnotherPersonsCookieDoesNotBoundATicket(t *testing.T) {
	rig := newAuthorityRig(t, 30*time.Minute)
	owner := rig.cookie(t, holderOwner, time.Now().UTC().Add(time.Hour))
	other := rig.cookie(t, holderSecond, time.Now().UTC().Add(10*time.Minute))
	var ends [2]<-chan error
	for i, readOnly := range []bool{false, true} {
		_, ends[i] = rig.dial(t, other, rig.ticket(t, owner), readOnly)
	}
	rig.offset.Store(int64(29 * time.Minute))
	time.Sleep(10 * authorityTick)
	for i, end := range ends {
		select {
		case err := <-end:
			t.Fatalf("socket %d (0 writer, 1 observer) ended at another person's cookie deadline: %v", i, err)
		default:
		}
	}
}

type closeOrder struct {
	mu       sync.Mutex
	events   []string
	holder   *attachHolder
	done     chan struct{}
	wantCode websocket.StatusCode // zero: attachAuthEndedCode
}

func (o *closeOrder) Close(code websocket.StatusCode, reason string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.holder.evicted.Load() {
		o.events = append(o.events, "close-before-input-stopped")
	}
	o.events = append(o.events, "close "+reason)
	if want := cmp.Or(o.wantCode, attachAuthEndedCode); code != want {
		o.events = append(o.events, "wrong code")
	}
	return nil
}

// TestEndAttachAuthority_ClosesBeforeCancel: input stops first, then the close frame is sent, and
// only then is the socket's context cancelled. When the session budget ran out the close is 1013 with its
// own reason, in the same order.
func TestEndAttachAuthority_ClosesBeforeCancel(t *testing.T) {
	for _, tc := range []struct {
		name  string
		spent bool
		code  websocket.StatusCode
		want  string
	}{
		{"a refusal", false, attachAuthEndedCode, "close session ended, cancel"},
		{"the budget ran out", true, websocket.StatusTryAgainLater, "close " + sessionUnconfirmedReason + ", cancel"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := &closeOrder{holder: &attachHolder{}, done: make(chan struct{}), wantCode: tc.code}
			endAttachAuthority(o, o.holder, func() {
				o.mu.Lock()
				o.events = append(o.events, "cancel")
				o.mu.Unlock()
				close(o.done)
			}, tc.spent)
			select {
			case <-o.done:
			case <-time.After(3 * time.Second):
				t.Fatal("the socket context was never cancelled")
			}
			o.mu.Lock()
			defer o.mu.Unlock()
			if got := strings.Join(o.events, ", "); got != tc.want {
				t.Fatalf("order = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestAttachAuthority_ProfileEditEndsAnOpenTerminal: the re-check asks the attach door's deny_interactive of
// the run as it stands now, so a profile edit that denies interactive sessions ends an open terminal at its
// next check and a profile that does not leaves it open. A governed super admin's ticket is re-checked too,
// and exempt on an operator-owned run, as at the door (#3398).
func TestAttachAuthority_ProfileEditEndsAnOpenTerminal(t *testing.T) {
	h := newUIHarness(t, okBackend())
	set := walledRun(h)
	r := httptest.NewRequest(http.MethodGet, "/api/v1/runs/"+h.run.ID.String()+"/attach", nil)
	check := func(role string) func(context.Context) error {
		ta := ticketActor{actorType: types.ActorHuman, principal: h.owner, role: role, authorizedAt: time.Now()}
		return h.srv.attachAuthority(r.WithContext(withTicketActor(r.Context(), ta)), h.run.ID)
	}
	member := check(oidc.RoleUser)
	if member == nil {
		t.Fatal("a member's ticket socket has no re-check")
	}
	if err := member(context.Background()); err != nil {
		t.Fatalf("checked under a profile that denies nothing: %v", err)
	}
	set(types.GovernanceLimits{DenyUIApps: true}) // the attach door asks deny_interactive alone
	if err := member(context.Background()); err != nil {
		t.Fatalf("checked under a profile that allows interactive sessions: %v", err)
	}
	set(types.GovernanceLimits{DenyInteractive: true})
	if err := member(context.Background()); !errors.Is(err, errAttachAuthorityEnded) {
		t.Fatalf("checked under a profile that now denies interactive sessions: %v, want %v", err, errAttachAuthorityEnded)
	}
	h.srv.cfg.GovernAdminRuns = true // ungoverned, a super admin's ticket has nothing to re-check here
	admin := check(oidc.RoleAdmin)
	if admin == nil {
		t.Fatal("a governed super admin's ticket socket has no re-check")
	}
	if err := admin(context.Background()); !errors.Is(err, errAttachAuthorityEnded) {
		t.Fatalf("a governed super admin's ticket on their own run: %v, want %v", err, errAttachAuthorityEnded)
	}
	h.run.OperatorOwned = true
	h.store.putRun(h.run)
	if err := admin(context.Background()); err != nil {
		t.Fatalf("a super admin's ticket on an operator-owned run: %v, want exempt", err)
	}
}

// TestAttachAuthority_ProfileEditEndsAnOpenCookieTerminal: the cookie lane asks the door's deny_interactive
// again too, with refuseNoTicketAttach's exemption: a governed caller's terminal on their own run ends at the
// next check once the profile denies interactive sessions, and one on an operator-owned run stays open (#3398).
func TestAttachAuthority_ProfileEditEndsAnOpenCookieTerminal(t *testing.T) {
	for name, operatorOwned := range map[string]bool{"their own run": false, "an operator-owned run": true} {
		t.Run(name, func(t *testing.T) {
			rig := newAuthorityRigWith(t, 30*time.Minute, func(c *Config) { c.GovernAdminRuns = true })
			unit := types.GovernanceUnit{ID: uuid.New(), Name: "walled"}
			rig.units.set(unit)
			rig.ast.mu.Lock()
			rig.run.OperatorOwned, rig.run.UnitID = operatorOwned, &unit.ID
			rig.ast.runs[rig.run.ID] = rig.run
			rig.ast.mu.Unlock()
			ends := rig.pair(t, "cookie", rig.cookie(t, holderOwner, time.Now().UTC().Add(time.Hour)))
			unit.Limits.DenyUIApps = true // the attach door asks deny_interactive alone
			rig.units.set(unit)
			time.Sleep(10 * authorityTick)
			for i, end := range ends {
				select {
				case err := <-end:
					t.Fatalf("socket %d ended under a profile that allows interactive sessions: %v", i, err)
				default:
				}
			}
			unit.Limits.DenyInteractive = true
			rig.units.set(unit)
			if !operatorOwned {
				wantAuthEnded(t, ends)
				return
			}
			time.Sleep(10 * authorityTick)
			for i, end := range ends {
				select {
				case err := <-end:
					t.Fatalf("socket %d on an operator-owned run ended: %v", i, err)
				default:
				}
			}
		})
	}
}

// sponsorLookupStore is sponsorUIStore with a sponsorship lookup that fails with err when it is set.
type sponsorLookupStore struct {
	*sponsorUIStore
	err error
}

func (s *sponsorLookupStore) ServiceStillSponsored(ctx context.Context, name, sponsor string, openedAt time.Time) (bool, error) {
	if s.err != nil {
		return false, s.err
	}
	return s.sponsorUIStore.ServiceStillSponsored(ctx, name, sponsor, openedAt)
}

// TestAttachAuthority_SponsorTerminalEndsWithTheSponsorship: a terminal a sponsor opened on a service
// identity's run is checked against the sponsorship as the handshake did, from the ticket's own authority
// time. A sponsorship that ended (replaced, suspended or retired), one that began again after the ticket
// (A to B to A), and a lookup that fails each end it at the next check; a sponsor who still is one, and a
// person on their own run whatever the service store says, keep it. The check is the one a ticket's writer
// socket and observer socket both run.
func TestAttachAuthority_SponsorTerminalEndsWithTheSponsorship(t *testing.T) {
	opened := time.Now().Add(-time.Minute)
	type sponsorRig struct {
		h  *uiHarness
		st *sponsorLookupStore
	}
	newRig := func(t *testing.T, serviceRun bool) sponsorRig {
		h := newUIHarness(t, okBackend())
		st := &sponsorLookupStore{sponsorUIStore: &sponsorUIStore{uiMemStore: h.store}}
		st.still.Store(true)
		h.srv.cfg.Store = st
		if serviceRun {
			h.run.CreatedBy = "service:ci-bot"
			h.store.putRun(h.run)
		}
		return sponsorRig{h: h, st: st}
	}
	checkAs := func(g sponsorRig, principal string) func(context.Context) error {
		r := httptest.NewRequest(http.MethodGet, "/api/v1/runs/"+g.h.run.ID.String()+"/attach", nil)
		ta := ticketActor{actorType: types.ActorHuman, principal: principal, role: oidc.RoleUser, authorizedAt: opened}
		check := g.h.srv.attachAuthority(r.WithContext(withTicketActor(r.Context(), ta)), g.h.run.ID)
		if check == nil {
			t.Fatal("a sponsor's ticket socket has no re-check")
		}
		return check
	}
	ended := func(t *testing.T, err error) {
		t.Helper()
		if !errors.Is(err, errAttachAuthorityEnded) {
			t.Fatalf("check = %v, want %v", err, errAttachAuthorityEnded)
		}
	}
	t.Run("a sponsor who still is one keeps the terminal", func(t *testing.T) {
		g := newRig(t, true)
		if err := checkAs(g, "sponsor-sub")(context.Background()); err != nil {
			t.Fatalf("check = %v, want nil", err)
		}
	})
	t.Run("a sponsorship that ended ends it, and a restored one is read afresh", func(t *testing.T) {
		g := newRig(t, true)
		check := checkAs(g, "sponsor-sub")
		g.st.still.Store(false) // replaced, suspended or retired: the store answers not sponsored
		ended(t, check(context.Background()))
		g.st.still.Store(true)
		if err := check(context.Background()); err != nil {
			t.Fatalf("check after the sponsorship was restored = %v, want nil", err)
		}
	})
	t.Run("a sponsorship that began again after the ticket ends it", func(t *testing.T) {
		g := newRig(t, true)
		g.st.sponsorSince.Store(opened.Add(time.Second).Unix())
		ended(t, checkAs(g, "sponsor-sub")(context.Background()))
	})
	t.Run("a sponsorship that began before the ticket keeps it", func(t *testing.T) {
		g := newRig(t, true)
		g.st.sponsorSince.Store(opened.Add(-time.Second).Unix())
		if err := checkAs(g, "sponsor-sub")(context.Background()); err != nil {
			t.Fatalf("check = %v, want nil", err)
		}
	})
	t.Run("a service identity that is gone ends it", func(t *testing.T) {
		g := newRig(t, true)
		g.st.err = store.ErrNotFound
		ended(t, checkAs(g, "sponsor-sub")(context.Background()))
	})
	t.Run("a lookup that fails ends it", func(t *testing.T) {
		g := newRig(t, true)
		g.st.err = errors.New("store down")
		ended(t, checkAs(g, "sponsor-sub")(context.Background()))
	})
	t.Run("another person's ticket on a service run ends", func(t *testing.T) {
		g := newRig(t, true)
		ended(t, checkAs(g, "someone-else")(context.Background()))
	})
	t.Run("a person's own run is not asked of the service store", func(t *testing.T) {
		g := newRig(t, false)
		g.st.still.Store(false)
		g.st.err = errors.New("store down")
		if err := checkAs(g, g.h.owner)(context.Background()); err != nil {
			t.Fatalf("check = %v, want nil", err)
		}
	})
}

// watchedSocket is an attach socket whose input-dropped frames and end a test reads.
type watchedSocket struct {
	c     *websocket.Conn
	drops chan int64
	end   chan error
}

// dialWatched is dial that keeps the input-dropped control frames instead of discarding them.
func (rig *authorityRig) dialWatched(t *testing.T, cookie *http.Cookie, ticket string, wantReadOnly bool) *watchedSocket {
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
	ws := &watchedSocket{c: c, drops: make(chan int64, 64), end: make(chan error, 1)}
	go func() {
		for {
			typ, data, err := c.Read(context.Background())
			if err != nil {
				ws.end <- err
				return
			}
			var msg attachInputDroppedMsg
			if typ == websocket.MessageText && json.Unmarshal(data, &msg) == nil && msg.Type == "input-dropped" {
				ws.drops <- msg.Bytes
			}
		}
	}()
	return ws
}

// pairWatched is pair with watched sockets: the writer first, then the observer.
func (rig *authorityRig) pairWatched(t *testing.T, lane string, cookie *http.Cookie) [2]*watchedSocket {
	t.Helper()
	var out [2]*watchedSocket
	for i, readOnly := range []bool{false, true} {
		tk, sent := "", cookie
		if lane != "cookie" {
			tk = rig.ticket(t, cookie)
		}
		if lane == "ticket" {
			sent = nil
		}
		out[i] = rig.dialWatched(t, sent, tk, readOnly)
	}
	return out
}

func (rig *authorityRig) budget(d, retry time.Duration) {
	rig.srv.sessionBudgets.test = ptr(d)
	rig.srv.sessionBudgets.retry = retry
}

// wantClosedWith waits for each socket's end and holds it to code and reason.
func wantClosedWith(t *testing.T, socks [2]*watchedSocket, code websocket.StatusCode, reason string) {
	t.Helper()
	for i, ws := range socks {
		select {
		case err := <-ws.end:
			var ce websocket.CloseError
			if !errors.As(err, &ce) || ce.Code != code || ce.Reason != reason {
				t.Fatalf("socket %d (0 writer, 1 observer) ended with %v, want close %d %q", i, err, code, reason)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("socket %d (0 writer, 1 observer) is still open", i)
		}
	}
}

func (rig *authorityRig) wantNoDetach(t *testing.T, reason string) {
	t.Helper()
	for _, ev := range rig.audit.snapshot() {
		if ev.Action == "session.detach" && strings.Contains(string(ev.Data), reason) {
			t.Errorf("session.detach = %s, want none with reason %q", ev.Data, reason)
		}
	}
}

// resultsOf is the result of each session.unconfirmed row, and the surface of the first.
func resultsOf(data []map[string]any) (results []string) {
	for _, d := range data {
		results = append(results, d["result"].(string))
	}
	return results
}

// TestAttachAuthority_RidesAnUnansweredCheck: a terminal whose re-check gets no answer stays open, takes no input
// and tells its client so while the window is open, takes input again once a check answers, and writes one
// rode_through row per socket and no session.detach.
func TestAttachAuthority_RidesAnUnansweredCheck(t *testing.T) {
	for _, lane := range []string{"cookie", "ticket"} {
		t.Run(lane, func(t *testing.T) {
			rig := newAuthorityRig(t, 0)
			rig.budget(time.Minute, 10*time.Millisecond)
			socks := rig.pairWatched(t, lane, rig.cookie(t, holderOwner, time.Now().UTC().Add(time.Hour)))
			sess := waitForSession(t, rig.srv.cfg.Runner.(*holderTestRunner), 0)
			rig.rev.setFail(1 << 30)
			time.Sleep(10 * authorityTick) // well past 3 authority intervals
			for i, ws := range socks {
				select {
				case err := <-ws.end:
					t.Fatalf("socket %d ended while its check had no answer: %v", i, err)
				default:
				}
			}
			if err := socks[0].c.Write(context.Background(), websocket.MessageBinary, []byte("lost")); err != nil {
				t.Fatal(err)
			}
			select {
			case n := <-socks[0].drops:
				if n != int64(len("lost")) {
					t.Fatalf("input-dropped bytes = %d, want %d", n, len("lost"))
				}
			case <-time.After(3 * time.Second):
				t.Fatal("the writer's input was dropped without telling its client")
			}
			rig.rev.setFail(0)
			waitFor(t, "the window to close on both sockets", func() bool {
				rows, _ := unconfirmedRows(rig.audit)
				return len(rows) == 2
			})
			if err := socks[0].c.Write(context.Background(), websocket.MessageBinary, []byte("kept")); err != nil {
				t.Fatal(err)
			}
			waitFor(t, "the input after the window", func() bool {
				sess.mu.Lock()
				defer sess.mu.Unlock()
				for _, w := range sess.writes {
					if strings.Contains(string(w), "kept") {
						return true
					}
				}
				return false
			})
			sess.mu.Lock()
			for _, w := range sess.writes {
				if strings.Contains(string(w), "lost") {
					t.Error("input typed while the window was open reached the shell")
				}
			}
			sess.mu.Unlock()
			_, data := wantUnconfirmedRows(t, rig.audit, 2)
			for _, d := range data {
				if d["result"] != "rode_through" || d["surface"] != sessionSurfaceTerminal {
					t.Errorf("row %v, want rode_through on the terminal", d)
				}
			}
			rig.wantNoDetach(t, "")
		})
	}
}

// TestAttachAuthority_BudgetEndsWithAReconnectingClose: with every lookup failing, the socket closes 1013 with the
// unconfirmed reason once the budget has run, records it as the detach reason, and writes one ended row.
func TestAttachAuthority_BudgetEndsWithAReconnectingClose(t *testing.T) {
	for _, lane := range []string{"cookie", "ticket"} {
		t.Run(lane, func(t *testing.T) {
			rig := newAuthorityRig(t, 0)
			rig.budget(100*time.Millisecond, 10*time.Millisecond)
			socks := rig.pairWatched(t, lane, rig.cookie(t, holderOwner, time.Now().UTC().Add(time.Hour)))
			failedAt := time.Now()
			rig.rev.setFail(1 << 30)
			wantClosedWith(t, socks, websocket.StatusTryAgainLater, sessionUnconfirmedReason)
			if d := time.Since(failedAt); d < 100*time.Millisecond {
				t.Errorf("closed %v after the first failure, want at least the 100 ms budget", d)
			}
			rig.wantDetachReasons(t, 2, sessionUnconfirmedReason)
			_, data := wantUnconfirmedRows(t, rig.audit, 2)
			if got := resultsOf(data); got[0] != "ended" || got[1] != "ended" {
				t.Errorf("results = %v, want two ended", got)
			}
		})
	}
}

// TestAttachAuthority_RevokeInsideTheWindowEndsAtOnce: a definite revoke that answers while the window is open
// ends the socket at the next retry, with the refusal's close, and the window's one row is refused.
func TestAttachAuthority_RevokeInsideTheWindowEndsAtOnce(t *testing.T) {
	for _, lane := range []string{"cookie", "ticket"} {
		t.Run(lane, func(t *testing.T) {
			rig := newAuthorityRig(t, 0)
			rig.budget(time.Hour, 3*authorityTick)
			socks := rig.pairWatched(t, lane, rig.cookie(t, holderOwner, time.Now().UTC().Add(time.Hour)))
			rig.rev.setFail(1 << 30)
			waitFor(t, "the window to open on both sockets", func() bool { return rig.rev.failedReads() >= 3 })
			_ = rig.rev.RevokeSub(context.Background(), holderOwner)
			rig.rev.setFail(0)
			wantClosedWith(t, socks, attachAuthEndedCode, attachAuthEndedReason)
			_, data := wantUnconfirmedRows(t, rig.audit, 2)
			if got := resultsOf(data); got[0] != "refused" || got[1] != "refused" {
				t.Errorf("results = %v, want two refused and no ended", got)
			}
		})
	}
}

// TestAttachAuthority_ZeroBudgetEndsAtTheFirstUnansweredCheck: at budget 0 a lookup that fails, and one that runs
// out of its bound, each end the socket at that check with the reconnecting close.
func TestAttachAuthority_ZeroBudgetEndsAtTheFirstUnansweredCheck(t *testing.T) {
	for name, apply := range map[string]func(*authorityRig){
		"one failed lookup":         func(rig *authorityRig) { rig.rev.setFail(2) },
		"one lookup past its bound": func(rig *authorityRig) { rig.rev.setHang(2) },
	} {
		for _, lane := range []string{"cookie", "ticket"} {
			t.Run(lane+"/"+name, func(t *testing.T) {
				rig := newAuthorityRig(t, 0)
				rig.budget(0, 10*time.Millisecond)
				socks := rig.pairWatched(t, lane, rig.cookie(t, holderOwner, time.Now().UTC().Add(time.Hour)))
				apply(rig)
				wantClosedWith(t, socks, websocket.StatusTryAgainLater, sessionUnconfirmedReason)
				rig.wantDetachReasons(t, 2, sessionUnconfirmedReason)
			})
		}
	}
}

// TestAttachAuthority_MissingRunEndsAtOnce: a run the store no longer has is an answer, not a failed read.
func TestAttachAuthority_MissingRunEndsAtOnce(t *testing.T) {
	for _, lane := range []string{"cookie", "ticket"} {
		t.Run(lane, func(t *testing.T) {
			rig := newAuthorityRig(t, 0)
			rig.budget(time.Hour, 10*time.Millisecond)
			socks := rig.pairWatched(t, lane, rig.cookie(t, holderOwner, time.Now().UTC().Add(time.Hour)))
			rig.ast.mu.Lock()
			delete(rig.ast.runs, rig.run.ID)
			rig.ast.mu.Unlock()
			wantClosedWith(t, socks, attachAuthEndedCode, attachAuthEndedReason)
		})
	}
}

// unmeetableStore adds a restriction that denies every placement to the rig's store: the run's profile
// composition then meets to nothing, a *composer.OverlayError wrapped by foldRestrictionLimits.
type unmeetableStore struct {
	*authorityUnits
	store.RestrictionStore // nil: the fold calls only GetRestrictionsByID
	id                     uuid.UUID
}

func (s *unmeetableStore) GetRestrictionsByID(context.Context, []uuid.UUID) ([]types.Restriction, error) {
	deny := []types.Placement{types.PlacementLocal, types.PlacementRemote}
	return []types.Restriction{{ID: s.id, Name: "walled", Limits: types.RestrictionLimits{DenyPlacements: &deny}}}, nil
}

// TestAttachAuthority_UnmeetableRestrictionEndsAtOnce: a profile composition nothing satisfies refuses again on
// every later check, so it ends the socket at the next check instead of riding the budget.
func TestAttachAuthority_UnmeetableRestrictionEndsAtOnce(t *testing.T) {
	rig := newAuthorityRigWith(t, 30*time.Minute, func(c *Config) { c.GovernAdminRuns = true })
	rig.budget(time.Hour, 10*time.Millisecond)
	restriction := uuid.New()
	rig.srv.cfg.Store = &unmeetableStore{authorityUnits: rig.units, id: restriction}
	socks := rig.pairWatched(t, "cookie", rig.cookie(t, holderOwner, time.Now().UTC().Add(time.Hour)))
	rig.ast.mu.Lock()
	rig.run.OperatorOwned, rig.run.RestrictionIDs = false, []uuid.UUID{restriction}
	rig.ast.runs[rig.run.ID] = rig.run
	rig.ast.mu.Unlock()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/runs/"+rig.run.ID.String()+"/attach", nil)
	ta := ticketActor{actorType: types.ActorHuman, principal: holderOwner, role: oidc.RoleUser, authorizedAt: time.Now()}
	check := rig.srv.attachAuthority(r.WithContext(withTicketActor(r.Context(), ta)), rig.run.ID)
	if err := check(context.Background()); !errors.Is(err, errAttachAuthorityEnded) || errors.Is(err, errSessionUnconfirmed) {
		t.Fatalf("ticket lane check = %v, want a refusal that is not unconfirmed", err)
	}
	wantClosedWith(t, socks, attachAuthEndedCode, attachAuthEndedReason)
	noUnconfirmedRows(t, rig.audit)
}

// TestAttachAuthority_SocketEndingInsideAWindowWritesClosed: a client that leaves while the window is open
// writes the window's closed row, and the socket's detach reason is not the unconfirmed one.
func TestAttachAuthority_SocketEndingInsideAWindowWritesClosed(t *testing.T) {
	for _, lane := range []string{"cookie", "ticket"} {
		t.Run(lane, func(t *testing.T) {
			rig := newAuthorityRig(t, 0)
			rig.budget(time.Hour, 3*authorityTick)
			socks := rig.pairWatched(t, lane, rig.cookie(t, holderOwner, time.Now().UTC().Add(time.Hour)))
			rig.rev.setFail(1 << 30)
			waitFor(t, "the window to open on both sockets", func() bool { return rig.rev.failedReads() >= 3 })
			for _, ws := range socks {
				_ = ws.c.Close(websocket.StatusNormalClosure, "bye")
			}
			_, data := wantUnconfirmedRows(t, rig.audit, 2)
			if got := resultsOf(data); got[0] != "closed" || got[1] != "closed" {
				t.Errorf("results = %v, want two closed and no ended", got)
			}
			rig.wantNoDetach(t, sessionUnconfirmedReason)
		})
	}
}
