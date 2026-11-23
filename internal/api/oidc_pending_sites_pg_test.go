// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	gooidc "github.com/coreos/go-oidc/v3/oidc"
	jose "github.com/go-jose/go-jose/v4"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/audit/sinks"
	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const pendingClientID = "client-1"

// pendingConsole is a console authenticator whose provider does not answer until recover is called.
type pendingConsole struct {
	is   *oidcIssuer
	down atomic.Bool
	ctx  context.Context
	auth *oidc.Authenticator
}

func newPendingConsole(t *testing.T, rev oidc.SessionRevocations, edit ...func(*oidc.Config)) *pendingConsole {
	t.Helper()
	pc := &pendingConsole{is: newOIDCIssuer(t)}
	pc.down.Store(true)
	pc.ctx = gooidc.ClientContext(context.Background(), &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if pc.down.Load() {
			return nil, errors.New("dial tcp: connection refused")
		}
		return http.DefaultTransport.RoundTrip(r)
	})})
	cfg := oidc.Config{
		IssuerURL: pc.is.url, ClientID: pendingClientID, ClientSecret: "secret", RedirectURL: "http://localhost/auth/callback",
		DefaultRole: oidc.RoleUser, Revocations: rev, BackchannelLogout: true,
	}
	for _, e := range edit {
		e(&cfg)
	}
	a, err := oidc.NewPending(cfg, accessTestHMACKey)
	if err != nil {
		t.Fatalf("NewPending: %v", err)
	}
	if err := a.Discover(pc.ctx); !errors.Is(err, oidc.ErrProviderUnreachable) || !a.Pending() {
		t.Fatalf("Discover against a provider that does not answer = %v, pending %v", err, a.Pending())
	}
	pc.auth = a
	return pc
}

// recoverProvider makes the provider answer and runs the discovery that publishes the state.
func (pc *pendingConsole) recoverProvider(t *testing.T) {
	t.Helper()
	pc.down.Store(false)
	if err := pc.auth.Discover(pc.ctx); err != nil {
		t.Fatalf("Discover after the provider answered: %v", err)
	}
}

// token signs a logout token for sub; key and kid default to the provider's own.
func (pc *pendingConsole) token(t *testing.T, sub, jti string, edit func(map[string]any)) string {
	return pc.tokenSignedBy(t, pc.is.key, pc.is.kid, sub, jti, edit)
}

func (pc *pendingConsole) tokenSignedBy(t *testing.T, key *rsa.PrivateKey, kid, sub, jti string, edit func(map[string]any)) string {
	t.Helper()
	claims := map[string]any{
		"iss": pc.is.url, "aud": pendingClientID, "sub": sub, "jti": jti,
		"iat": time.Now().Unix(), "exp": time.Now().Add(2 * time.Minute).Unix(),
		"events": map[string]any{"http://schemas.openid.net/event/backchannel-logout": map[string]any{}},
	}
	if edit != nil {
		edit(claims)
	}
	return pc.is.sign(t, key, kid, jose.RS256, claims)
}

// pendingServer is a server whose console authenticator is pc's, with a session store that records its cuts.
func pendingServer(t *testing.T, pc *pendingConsole, rev *fakeSessionRevocations) (*Server, *recRecorder) {
	t.Helper()
	srv, _, _, _ := newAuthzMatrixServer(t, func(c *Config) {
		c.OIDC = pc.auth
		c.SessionRevocations = rev
	})
	return srv, srv.cfg.Audit.(*recRecorder)
}

func postHeldLogout(t *testing.T, srv *Server, token string) *httptest.ResponseRecorder {
	t.Helper()
	form := url.Values{}
	if token != "" {
		form.Set("logout_token", token)
	}
	r := httptest.NewRequest(http.MethodPost, "/auth/backchannel-logout", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Host, r.RemoteAddr = "127.0.0.1", "127.0.0.1:54321"
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	return w
}

func rowsOf(rec *recRecorder, action string) []types.AuditEvent {
	var out []types.AuditEvent
	for _, ev := range rec.snapshot() {
		if ev.Action == action {
			out = append(out, ev)
		}
	}
	return out
}

func auditDataMap(t *testing.T, ev types.AuditEvent) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(ev.Data, &m); err != nil {
		t.Fatalf("audit data %s: %v", ev.Data, err)
	}
	return m
}

func holdStates(t *testing.T, rec *recRecorder) []string {
	var out []string
	for _, ev := range rowsOf(rec, "session.revoke.hold") {
		out = append(out, fmt.Sprint(auditDataMap(t, ev)["state"]))
	}
	return out
}

func countState(states []string, want string) int {
	n := 0
	for _, s := range states {
		if s == want {
			n++
		}
	}
	return n
}

// A1: revoke-sessions mounts and cuts exactly as it does signed in.
func TestPendingOIDC_RevokeSessionsMounted(t *testing.T) {
	rev := &fakeSessionRevocations{}
	pc := newPendingConsole(t, rev)
	srv, _ := pendingServer(t, pc, rev)
	w := do(t, srv, http.MethodPost, "/api/v1/sessions/revoke", adminToken, `{"sub":"sub-alice"}`)
	if w.Code != http.StatusNoContent {
		t.Fatalf("revoke-sessions with a pending authenticator = %d %s, want 204 (the route must mount)", w.Code, w.Body.String())
	}
	if len(rev.revokedSubs) == 0 {
		t.Errorf("the revoke wrote no cutoff: %+v", rev)
	}
}

// A2: a role-mapping demotion revokes the outstanding tokens as it does signed in. The role preview reads no
// provider state, so the pending authenticator answers it.
func TestPendingOIDC_DemotionRevokesTokens(t *testing.T) {
	const group = "eng-team"
	answerable := false
	now := time.Now().UTC()
	pc := newPendingConsole(t, &fakeSessionRevocations{}, func(c *oidc.Config) { c.RoleMap = map[string]string{"chart-admin": oidc.RoleAdmin} })
	id := uuid.New()
	st := &roleMapTokenStore{toks: []types.APIToken{
		{ID: uuid.New(), Principal: "sub-alice", Email: "alice@corp.example", Role: string(oidc.RoleAdmin), Groups: []string{group}, GroupsTruncated: &answerable, CreatedAt: now},
		{ID: uuid.New(), Principal: "sub-bob", Email: "bob@corp.example", Role: string(oidc.RoleUser), Groups: []string{group}, GroupsTruncated: &answerable, CreatedAt: now},
	}}
	st.rows = []types.RoleMapping{{ID: id, Value: group, Role: oidc.RoleAdmin}}
	cfg := baseTestConfig(newHarness(t), st)
	cfg.OIDC = pc.auth
	srv := New(cfg)
	w := do(t, srv, http.MethodDelete, "/api/v1/access/mappings/"+id.String()+"?acknowledge_access_change=true", adminToken, "")
	if w.Code != http.StatusNoContent {
		t.Fatalf("delete = %d, want 204; body=%s", w.Code, w.Body.String())
	}
	live := st.liveRoles()
	if _, still := live["sub-alice"]; still {
		t.Errorf("the admin-stamped token survived the demotion with a pending authenticator: %v", live)
	}
	if _, still := live["sub-bob"]; !still {
		t.Errorf("a member token was revoked by someone else's demotion: %v", live)
	}
}

// A3: an endpoint sign-in whose issuer keys cannot be fetched is refused, whatever the console authenticator's
// state: the endpoint binding verifies against its own issuer, never the console's.
func TestEndpointOIDCCapture_UnreachableIssuerRefused(t *testing.T) {
	pc := newPendingConsole(t, &fakeSessionRevocations{})
	is := newOIDCIssuer(t)
	provider, err := gooidc.NewProvider(context.Background(), is.url)
	if err != nil {
		t.Fatalf("discovery: %v", err)
	}
	is.srv.Close() // the issuer's keys are unreachable from here on
	b := &endpointOIDCBinding{
		row:  types.ModelProvider{UID: oidcUID, OIDC: &types.ProviderOIDC{ClientID: "client-1"}},
		disc: endpointOIDCDiscovery{provider: provider},
	}
	s := &Server{cfg: Config{OIDC: pc.auth}}
	tok := is.sign(t, is.key, is.kid, jose.RS256, is.claims(nil))
	if idToken, reason := s.verifyEndpointOIDCIDToken(context.Background(), b, tok, "nonce-1"); reason == "" || idToken != nil {
		t.Fatalf("a token was accepted although its issuer's keys could not be fetched (reason %q)", reason)
	}
}

// A4: a run keeps renewing its token with a pending authenticator, as it does signed in.
func TestPendingOIDC_RunTokenRenew(t *testing.T) {
	pc := newPendingConsole(t, &fakeSessionRevocations{})
	codes := map[string]int{}
	for name, a := range map[string]*oidc.Authenticator{"pending": pc.auth, "signed in": {}} {
		h := newHarness(t)
		cfg := baseTestConfig(h, struct {
			*lifetimeCapRunStore
			fixedDBClock
		}{&lifetimeCapRunStore{capAt: time.Now().UTC().Add(time.Hour)}, fixedDBClock{at: time.Now().UTC()}})
		cfg.OIDC = a
		w := do(t, New(cfg), http.MethodPost, "/api/v1/internal/token/renew", h.mintRunToken(t, uuid.New()), "")
		codes[name] = w.Code
	}
	if codes["pending"] != codes["signed in"] {
		t.Errorf("renew answers %v: a pending authenticator must answer as a signed-in one does", codes)
	}
	if codes["pending"] == http.StatusUnauthorized || codes["pending"] == http.StatusServiceUnavailable {
		t.Errorf("renew with a pending authenticator = %d, want the run to keep renewing", codes["pending"])
	}
}

// A7: /healthz carries sso_pending only while pending.
func TestHealthz_SSOPending(t *testing.T) {
	pc := newPendingConsole(t, &fakeSessionRevocations{})
	has := func(a *oidc.Authenticator) (bool, any) {
		w := do(t, New(Config{OIDC: a}), http.MethodGet, "/healthz", "", "")
		var body map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		v, ok := body["sso_pending"]
		return ok, v
	}
	if ok, v := has(pc.auth); !ok || v != true {
		t.Errorf("pending: sso_pending = %v (present %v), want true", v, ok)
	}
	for name, a := range map[string]*oidc.Authenticator{"the zero value": {}, "no oidc": nil} {
		if ok, v := has(a); ok {
			t.Errorf("%s: sso_pending = %v, want the key absent", name, v)
		}
	}
	pc.recoverProvider(t)
	if ok, v := has(pc.auth); ok {
		t.Errorf("after discovery: sso_pending = %v, want the key absent", v)
	}
}

// A7: the sso_discovery row is emitted while pending and after logouts that were not applied, never Blocking,
// and nothing is emitted for a nil or zero-value authenticator.
func TestSetupStatus_SSODiscovery(t *testing.T) {
	pc := newPendingConsole(t, &fakeSessionRevocations{})
	ids := func(a *oidc.Authenticator) []string {
		return setupCheckIds(t, New(Config{AdminToken: adminToken, OIDC: a}))
	}
	has := func(ids []string) bool {
		for _, id := range ids {
			if id == "sso_discovery" {
				return true
			}
		}
		return false
	}
	if !has(ids(pc.auth)) {
		t.Error("no sso_discovery row while pending")
	}
	if has(ids(&oidc.Authenticator{})) || has(ids(nil)) {
		t.Error("sso_discovery emitted for an authenticator that is not pending")
	}
	for name, c := range map[string]struct {
		pending bool
		st      oidc.LogoutQueueStats
		want    bool
	}{
		"pending":            {true, oidc.LogoutQueueStats{Held: 3}, true},
		"unverifiable":       {false, oidc.LogoutQueueStats{Unverifiable: 1}, true},
		"queue full":         {false, oidc.LogoutQueueStats{RefusedQueueFull: 1}, true},
		"rate limited":       {false, oidc.LogoutQueueStats{RateLimited: 2}, true},
		"queued and applied": {false, oidc.LogoutQueueStats{Queued: 4, Applied: 4}, false},
		"nothing":            {false, oidc.LogoutQueueStats{}, false},
	} {
		chk, ok := ssoDiscoveryCheck(c.pending, c.st)
		if ok != c.want || chk.Blocking || (ok && chk.Status != "warn") {
			t.Errorf("%s: ssoDiscoveryCheck = %+v, %v; want present %v, warn, not Blocking", name, chk, ok, c.want)
		}
	}
	if got := appendSSODiscoveryCheck(nil, nil); len(got) != 0 {
		t.Errorf("a nil authenticator added %v", got)
	}
}

// P4b: while pending, a burst of well-formed logouts is held whole, none refused with 429.
func TestBackchannelPending_BurstExemptBounded(t *testing.T) {
	rev := &fakeSessionRevocations{}
	pc := newPendingConsole(t, rev)
	srv, rec := pendingServer(t, pc, rev)
	for i := 0; i < 20; i++ {
		if w := postHeldLogout(t, srv, pc.token(t, fmt.Sprintf("u%d", i), fmt.Sprintf("j%d", i), nil)); w.Code != http.StatusServiceUnavailable {
			t.Fatalf("token %d = %d %s, want 503 (held), never 429", i, w.Code, w.Body.String())
		}
	}
	if st := pc.auth.LogoutQueueStats(); st.Held != 20 || st.RateLimited != 0 {
		t.Fatalf("stats %+v, want 20 held and none rate limited", st)
	}
	// Malformed POSTs under the exemption: charged after the fact, so at most burst-many write a row.
	for i := 0; i < 20; i++ {
		postHeldLogout(t, srv, "garbage")
	}
	states := holdStates(t, rec)
	if n := countState(states, "refused_unreadable"); n < 1 || n > int(backchannelLogoutBurst)+1 {
		t.Errorf("refused_unreadable rows = %d, want between 1 and %d", n, int(backchannelLogoutBurst)+1)
	}
	if st := pc.auth.LogoutQueueStats(); st.RateLimited < 20-int(backchannelLogoutBurst)-1 {
		t.Errorf("RateLimited = %d, want the rest of the 20 malformed POSTs counted", st.RateLimited)
	}
	// The first refusal is summarised at once; the rest wait for the minute or the publish.
	if n := len(rowsOf(rec, "session.revoke_hold.coalesce")); n != 1 {
		t.Errorf("%d summary rows inside the first minute, want the one for the first refusal", n)
	}
	// With the queue full the limiter applies as at v0.10.2: the 429s are counted, and the publish records them once.
	for i := 20; i < oidc.LogoutQueueCap; i++ {
		postHeldLogout(t, srv, pc.token(t, fmt.Sprintf("u%d", i), fmt.Sprintf("j%d", i), nil))
	}
	before := pc.auth.LogoutQueueStats().RateLimited
	got429 := 0
	for i := 0; i < 50; i++ {
		if postHeldLogout(t, srv, pc.token(t, fmt.Sprintf("x%d", i), fmt.Sprintf("jx%d", i), nil)).Code == http.StatusTooManyRequests {
			got429++
		}
	}
	if got429 == 0 || pc.auth.LogoutQueueStats().RateLimited-before != got429 {
		t.Errorf("429s %d, counted %d; want every 429 while pending counted", got429, pc.auth.LogoutQueueStats().RateLimited-before)
	}
	pc.recoverProvider(t)
	if n := len(rowsOf(rec, "session.revoke_hold.coalesce")); n != 2 {
		t.Errorf("summary rows after the publish = %d, want the first-refusal row and the publish row", n)
	}
	for _, ev := range rowsOf(rec, "session.revoke_hold.coalesce") {
		if ev.Target != backchannelLogoutTarget || ev.Outcome != "failure" {
			t.Errorf("summary row target %q outcome %q, want the fixed route target and failure", ev.Target, ev.Outcome)
		}
	}
}

// A flood of well-formed tokens whose sub is a reserved principal is charged to the limiter like malformed ones:
// the hold rows it writes are bounded, and none takes a queue place.
func TestBackchannelPending_ReservedFloodBounded(t *testing.T) {
	rev := &fakeSessionRevocations{}
	pc := newPendingConsole(t, rev)
	srv, rec := pendingServer(t, pc, rev)
	for i := 0; i < 50; i++ {
		postHeldLogout(t, srv, pc.token(t, fmt.Sprintf("subject:x%d", i), fmt.Sprintf("jr%d", i), nil))
	}
	if st := pc.auth.LogoutQueueStats(); st.Held != 0 {
		t.Errorf("held %d, want none: a reserved sub takes no place", st.Held)
	}
	n := countState(holdStates(t, rec), "refused_reserved")
	if n < 1 || n > int(backchannelLogoutBurst)+1 {
		t.Errorf("refused_reserved rows = %d, want between 1 and %d for 50 posts", n, int(backchannelLogoutBurst)+1)
	}
}

// P4b: an anonymous caller who fills the queue with well-formed junk leaves a genuine logout recorded as
// refused_queue_full with its own hash, not held.
func TestBackchannelPending_CrowdOutRecorded(t *testing.T) {
	rev := &fakeSessionRevocations{}
	pc := newPendingConsole(t, rev)
	srv, rec := pendingServer(t, pc, rev)
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < oidc.LogoutQueueCap; i++ {
		postHeldLogout(t, srv, pc.tokenSignedBy(t, other, pc.is.kid, fmt.Sprintf("junk%d", i), fmt.Sprintf("jj%d", i), nil))
	}
	genuine := pc.token(t, "pairwise-1", "jg", nil)
	if w := postHeldLogout(t, srv, genuine); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("the genuine logout = %d, want 503", w.Code)
	}
	var found bool
	for _, ev := range rowsOf(rec, "session.revoke.hold") {
		d := auditDataMap(t, ev)
		if d["state"] == "refused_queue_full" && d["sub_unverified"] == "pairwise-1" && strings.HasPrefix(ev.Target, "logout_token:") && d["token_sha256"] != nil {
			found = true
		}
	}
	if !found {
		t.Errorf("no refused_queue_full row names the genuine token: %v", holdStates(t, rec))
	}
	long := pc.token(t, strings.Repeat("s", 257), "jl", nil)
	postHeldLogout(t, srv, long)
	if countState(holdStates(t, rec), "refused_unreadable") != 1 {
		t.Errorf("a sub over 256 bytes is not refused_unreadable: %v", holdStates(t, rec)[len(holdStates(t, rec))-1])
	}
}

// A6: the audit rows of a held logout: one hold row per queued, refused or unverifiable token with its hash and a
// non-person target, no raw token anywhere, and the applied cut is the ordinary session.revoke row carrying the
// queue time and the same hash.
func TestBackchannelPending_AuditRows(t *testing.T) {
	rev := &fakeSessionRevocations{}
	pc := newPendingConsole(t, rev)
	srv, rec := pendingServer(t, pc, rev)
	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	good := pc.token(t, "pairwise-1", "j1", nil)
	forged := pc.tokenSignedBy(t, other, pc.is.kid, "victim", "j2", nil)
	postHeldLogout(t, srv, good)
	postHeldLogout(t, srv, forged)
	postHeldLogout(t, srv, pc.token(t, "subject:x", "j3", nil))
	pc.recoverProvider(t)

	states := holdStates(t, rec)
	for _, want := range []string{"queued", "unverifiable", "refused_reserved"} {
		if countState(states, want) == 0 {
			t.Errorf("no %s row in %v", want, states)
		}
	}
	for _, ev := range rowsOf(rec, "session.revoke.hold") {
		d := auditDataMap(t, ev)
		if ev.Target != backchannelLogoutTarget && !(strings.HasPrefix(ev.Target, "logout_token:") && ev.Target == "logout_token:"+fmt.Sprint(d["token_sha256"])) {
			t.Errorf("hold row target %q is neither a token hash nor the route", ev.Target)
		}
		if d["sub_verified"] != false {
			t.Errorf("a hold row claims a verified sub: %v", d)
		}
	}
	var applied types.AuditEvent
	for _, ev := range rowsOf(rec, "session.revoke") {
		if auditDataMap(t, ev)["source"] == "idp_back_channel_logout" {
			applied = ev
		}
	}
	d := auditDataMap(t, applied)
	var queuedSHA any
	for _, ev := range rowsOf(rec, "session.revoke.hold") {
		if dd := auditDataMap(t, ev); dd["state"] == "queued" {
			queuedSHA = dd["token_sha256"]
		}
	}
	if applied.Target != "pairwise-1" || d["token_sha256"] == nil || d["token_sha256"] != queuedSHA || d["queued_at"] == nil {
		t.Errorf("the applied cut = %+v, want a session.revoke for pairwise-1 with queued_at and the queued row's hash %v", d, queuedSHA)
	}
	if len(rev.cutSubs) != 1 || rev.cutSubs[0] != "pairwise-1" {
		t.Errorf("cuts = %v, want only pairwise-1", rev.cutSubs)
	}
	for _, ev := range rec.snapshot() {
		for _, tok := range []string{good, forged} {
			if strings.Contains(string(ev.Data), tok) || strings.Contains(ev.Target, tok) {
				t.Errorf("row %s carries a raw logout token", ev.Action)
			}
		}
	}
}

// errTap records what the audit chain's Record returned, so a Seal refusal fails the test.
type errTap struct {
	inner audit.Recorder
	errs  []error
}

func (e *errTap) Record(ctx context.Context, ev types.AuditEvent) error {
	err := e.inner.Record(ctx, ev)
	if err != nil {
		e.errs = append(e.errs, fmt.Errorf("%s: %w", ev.Action, err))
	}
	return err
}

// A8: every hold outcome writes exactly one session.revoke.hold row that the seal accepts, under full, fields and
// off. The chain is built as introChain does, so each row goes through Seal.
func TestBackchannelPending_EveryHoldOutcomeWritesARow(t *testing.T) {
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		state string
		drive func(t *testing.T, pc *pendingConsole, srv *Server)
	}{
		{"queued", func(t *testing.T, pc *pendingConsole, srv *Server) {
			postHeldLogout(t, srv, pc.token(t, "pairwise-1", "j", nil))
		}},
		{"refused_unreadable", func(t *testing.T, pc *pendingConsole, srv *Server) { postHeldLogout(t, srv, "") }},
		{"refused_unreadable", func(t *testing.T, pc *pendingConsole, srv *Server) { postHeldLogout(t, srv, "garbage") }},
		{"refused_unreadable", func(t *testing.T, pc *pendingConsole, srv *Server) {
			postHeldLogout(t, srv, pc.token(t, "", "j", func(c map[string]any) { delete(c, "sub") }))
		}},
		{"refused_unreadable", func(t *testing.T, pc *pendingConsole, srv *Server) {
			postHeldLogout(t, srv, pc.token(t, strings.Repeat("s", 257), "j", nil))
		}},
		{"refused_queue_full", func(t *testing.T, pc *pendingConsole, srv *Server) {
			for i := 0; i < oidc.LogoutQueueCap; i++ {
				postHeldLogout(t, srv, pc.token(t, fmt.Sprintf("u%d", i), fmt.Sprintf("j%d", i), nil))
			}
			postHeldLogout(t, srv, pc.token(t, "late", "jlate", nil))
		}},
		{"refused_reserved", func(t *testing.T, pc *pendingConsole, srv *Server) {
			postHeldLogout(t, srv, pc.token(t, "subject:"+uuid.NewString(), "j", nil))
		}},
		{"refused_reserved", func(t *testing.T, pc *pendingConsole, srv *Server) { // the drain's second check
			logoutQueueSink{s: srv}.Noted(context.Background(), oidc.LogoutQueueEvent{State: oidc.LogoutRefusedReserved, TokenSHA256: strings.Repeat("b", 64), ReceivedAt: time.Now()})
		}},
		{"unverifiable", func(t *testing.T, pc *pendingConsole, srv *Server) {
			postHeldLogout(t, srv, pc.tokenSignedBy(t, other, pc.is.kid, "victim", "j", nil))
			pc.recoverProvider(t)
		}},
		{"lost_at_shutdown", func(t *testing.T, pc *pendingConsole, srv *Server) {
			postHeldLogout(t, srv, pc.token(t, "pairwise-1", "j", nil))
			oidc.ReportLogoutsLostAtShutdown(pc.auth)
		}},
	}
	for mode, mkSealer := range map[string]func() *audit.Sealer{
		"off": func() *audit.Sealer { return nil },
		"fields": func() *audit.Sealer {
			return &audit.Sealer{Keys: fixedSealKeys{}, Pending: func() []byte { return make([]byte, 32) }}
		},
		"full": func() *audit.Sealer {
			return &audit.Sealer{Keys: fixedSealKeys{}, Pending: func() []byte { return make([]byte, 32) }, SealActor: true}
		},
	} {
		for i, c := range cases {
			t.Run(fmt.Sprintf("%s/%s/%d", mode, c.state, i), func(t *testing.T) {
				rev := &fakeSessionRevocations{}
				pc := newPendingConsole(t, rev)
				srv, rec := pendingServer(t, pc, rev)
				dir := t.TempDir()
				fs, err := sinks.NewFileSink(sinks.FileConfig{Path: filepath.Join(dir, "sink.jsonl")})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = fs.Close() })
				sp, err := NewAuditSpool(filepath.Join(dir, "spool.jsonl"))
				if err != nil {
					t.Fatal(err)
				}
				tap := &errTap{inner: &chainRecorder{rec: rec, sink: fs, spool: sp, sealer: mkSealer()}}
				srv.cfg.Audit = tap
				c.drive(t, pc, srv)
				if len(tap.errs) != 0 {
					t.Errorf("the audit chain refused rows (Seal), which logs a WARN and drops the record: %v", tap.errs)
				}
				states := holdStates(t, rec)
				if countState(states, c.state) != 1 && c.state != "refused_queue_full" {
					t.Errorf("hold rows %v, want exactly one %s", states, c.state)
				}
				if c.state == "refused_queue_full" && countState(states, c.state) != 1 {
					t.Errorf("hold rows: %d refused_queue_full, want 1", countState(states, c.state))
				}
			})
		}
	}
}
