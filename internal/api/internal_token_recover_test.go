// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"cmp"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/identity/embedded"
	"github.com/cjohnstoniv/wardyn/internal/identity/identitytest"
	"github.com/cjohnstoniv/wardyn/internal/runner/remote"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// internalRoutesWithoutAuth is the closed set of /api/v1/internal/ routes that take neither internalAuth nor
// internalAuthGroundtruth: each authenticates its caller itself. A required route must be mounted by the guard's
// router; an expected one is allowed without auth when it is mounted.
var internalRoutesWithoutAuth = map[string]struct{ required bool }{
	"POST /api/v1/internal/token/recover": {required: true}, // verifies an expired run token itself (RecoveryVerifier)
	"GET /api/v1/internal/runner-peer":    {required: true}, // the replica peer door: mounted only under HA with the internal listener
}

// everyInternalMount switches on every Config field that gates an /api/v1/internal/ mount, so a conditional route
// cannot hide from the guard's walk. Secrets != nil (routes.go) is already set by newAuthzMatrixServer; the runner
// peer door is mounted only under HA with the internal listener (peerDoorMount). A later change that gates an
// internal mount on another Config field adds its switch here.
func everyInternalMount(c *Config) { peerDoorMount(c) }

const (
	ruRecoverPath = "/api/v1/internal/token/recover"
	ruRenewPath   = "/api/v1/internal/token/renew"
	ruSub         = "alice@example.com"
)

var errRuRead = errors.New("ru store: read failed")

// ruMiddlewareName is the runtime name of a middleware, e.g. "<pkg>.(*Server).internalAuth-fm".
func ruMiddlewareName(mw func(http.Handler) http.Handler) string {
	return runtime.FuncForPC(reflect.ValueOf(mw).Pointer()).Name()
}

// ruIsRunAuth reports whether mw is one of the two run-token auth middlewares: (*Server).internalAuth or
// (*Server).internalAuthGroundtruth.
func ruIsRunAuth(mw func(http.Handler) http.Handler) bool {
	name := ruMiddlewareName(mw)
	return strings.HasSuffix(name, ".(*Server).internalAuth-fm") || strings.HasSuffix(name, ".(*Server).internalAuthGroundtruth-fm")
}

// TestEveryInternalRouteWithoutAuthIsAnException walks the router with every conditional internal mount on: each
// /api/v1/internal/ route that holds neither run-token auth middleware must be named in internalRoutesWithoutAuth,
// and each route that set marks required must be mounted and hold neither.
func TestEveryInternalRouteWithoutAuthIsAnException(t *testing.T) {
	srv, _, _, _ := newAuthzMatrixServer(t, everyInternalMount)
	walked, bare := map[string]bool{}, map[string]bool{}
	err := chi.Walk(srv.router, func(method, route string, _ http.Handler, mws ...func(http.Handler) http.Handler) error {
		if !strings.HasPrefix(route, "/api/v1/internal/") {
			return nil
		}
		key := method + " " + route
		walked[key] = true
		bare[key] = true
		for _, mw := range mws {
			if ruIsRunAuth(mw) {
				bare[key] = false
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// The walk must report the middlewares of a route mounted through r.With, and name them as the guard expects:
	// the gateway mint and the ground-truth ingest each hold exactly one, so a walk that missed it would call them bare.
	for _, key := range []string{"POST /api/v1/internal/gateway-identity", "POST /api/v1/internal/groundtruth", "POST /api/v1/internal/token/renew"} {
		if !walked[key] || bare[key] {
			t.Fatalf("%s: walked=%v without auth=%v, want a walked route holding a run-token auth middleware", key, walked[key], bare[key])
		}
	}
	for key, isBare := range bare {
		if _, ok := internalRoutesWithoutAuth[key]; isBare && !ok {
			t.Errorf("%s holds neither internalAuth nor internalAuthGroundtruth and is not in internalRoutesWithoutAuth", key)
		}
	}
	for key, rule := range internalRoutesWithoutAuth {
		switch {
		case rule.required && !walked[key]:
			t.Errorf("%s is required by internalRoutesWithoutAuth and is not mounted", key)
		case rule.required && !bare[key]:
			t.Errorf("%s is listed as having no auth middleware and holds one", key)
		}
	}
}

// ruTokenClaims is the body of an embedded-provider run token (internal/identity/embedded's svidClaims).
type ruTokenClaims struct {
	jwt.Claims
	Act struct {
		Sub string `json:"sub"`
	} `json:"act"`
	Sponsor string `json:"sponsor"`
}

// ruToken signs a run token for runID with key, as the embedded provider does, that expired expiredAgo ago.
func ruToken(t *testing.T, key *ecdsa.PrivateKey, runID uuid.UUID, jti, audience string, expiredAgo time.Duration) string {
	t.Helper()
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: key}, (&jose.SignerOptions{}).WithType("JWT"))
	if err != nil {
		t.Fatal(err)
	}
	exp := time.Now().Add(-expiredAgo)
	c := ruTokenClaims{Claims: jwt.Claims{
		Issuer:    "spiffe://wardyn.local",
		Subject:   ruSub,
		Audience:  jwt.Audience{audience},
		ID:        jti,
		IssuedAt:  jwt.NewNumericDate(exp.Add(-time.Hour)),
		NotBefore: jwt.NewNumericDate(exp.Add(-time.Hour)),
		Expiry:    jwt.NewNumericDate(exp),
	}, Sponsor: ruSub}
	c.Act.Sub = "spiffe://wardyn.local/agent-run/" + runID.String()
	tok, err := jwt.Signed(signer).Claims(c).Serialize()
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func ruKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// ruRevocations is a revocation store that answers every IsRevoked with err (nil: not revoked) and counts them.
type ruRevocations struct {
	mu    sync.Mutex
	err   error
	calls int
}

func (r *ruRevocations) IsRevoked(context.Context, string, uuid.UUID) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	return false, r.err
}
func (r *ruRevocations) RevokeRun(context.Context, uuid.UUID) error         { return nil }
func (r *ruRevocations) RevokeJTI(context.Context, string, uuid.UUID) error { return nil }

func (r *ruRevocations) set(err error) {
	r.mu.Lock()
	r.err = err
	r.mu.Unlock()
}

func (r *ruRevocations) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

// ruNoRecovery is an identity provider that is not an identity.RecoveryVerifier.
type ruNoRecovery struct{ identity.Provider }

// ruStamp is one StampRunTokenRecovered call.
type ruStamp struct {
	run              uuid.UUID
	jti, presentedBy string
}

// ruStore is a store the recover door and the outage hold read: a run table, the recovery facts and the outage
// window answered as the test sets them. Every call it answers is recorded. With failAll set it answers each of
// them with an error. A call it does not answer reaches the nil embedded interfaces, which panics, and the test's
// panic catcher fails the test.
type ruStore struct {
	store.Store
	store.RunnerStore
	store.OutageWindows

	mu      sync.Mutex
	calls   []string
	failAll bool

	runs   map[uuid.UUID]types.AgentRun
	runErr error

	lineage    store.TokenLineage
	lineageErr error

	check      store.RecoveryCheck
	checkErr   error
	checkAsked []bool

	window   store.OutageWindow
	covered  bool
	coverErr error

	held    bool
	heldErr error

	containmentPending bool
	containmentErr     error
	pendingAction      bool

	stamped  bool
	stampErr error
	stamps   []ruStamp
	onStamp  func(*ruStore)

	dbNow    time.Time
	dbNowErr error

	runner     types.Runner
	runnerErr  error
	runnersOn  bool
	siteCfgErr error
}

var (
	_ store.Store             = (*ruStore)(nil)
	_ store.RunTokenRecoverer = (*ruStore)(nil)
	_ store.OutageWindows     = (*ruStore)(nil)
	_ store.RunnerStore       = (*ruStore)(nil)
	_ store.DatabaseClock     = (*ruStore)(nil)
	_ runnerHoldReader        = (*ruStore)(nil)
	_ recoveryStore           = (*ruStore)(nil)
	_ runContainmentStore     = (*ruStore)(nil)
)

func newRuStore() *ruStore { return &ruStore{runs: map[uuid.UUID]types.AgentRun{}} }

// ruPassingStore is a store for which every check of the door passes for a RUNNING server run holding token jti:
// its lineage is that token, and a window covers its expiry.
func ruPassingStore(runID uuid.UUID, jti string) *ruStore {
	st := newRuStore()
	st.runs[runID] = types.AgentRun{ID: runID, CreatedBy: ruSub, State: types.RunRunning, Placement: types.PlacementRemote}
	st.lineage = store.TokenLineage{JTI: jti, RenewedAt: time.Now().Add(-2 * time.Hour)}
	st.window, st.covered = store.OutageWindow{ID: 7, Kind: store.OutageKindControlPlane}, true
	st.stamped = true
	return st
}

// enter records one call and answers the error every call gets when failAll is set.
func (s *ruStore) enter(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, name)
	if s.failAll {
		return errRuRead
	}
	return nil
}

func (s *ruStore) with(f func(*ruStore)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f(s)
}

func (s *ruStore) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

func (s *ruStore) called(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.calls {
		if c == name {
			return true
		}
	}
	return false
}

func (s *ruStore) GetRun(_ context.Context, id uuid.UUID) (types.AgentRun, error) {
	if err := s.enter("GetRun"); err != nil {
		return types.AgentRun{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.runErr != nil {
		return types.AgentRun{}, s.runErr
	}
	run, ok := s.runs[id]
	if !ok {
		return types.AgentRun{}, store.ErrNotFound
	}
	return run, nil
}

func (s *ruStore) DatabaseNow(context.Context) (time.Time, error) {
	if err := s.enter("DatabaseNow"); err != nil {
		return time.Time{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dbNow, s.dbNowErr
}

func (s *ruStore) RunRecoveryLineage(context.Context, uuid.UUID) (store.TokenLineage, error) {
	if err := s.enter("RunRecoveryLineage"); err != nil {
		return store.TokenLineage{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lineage, s.lineageErr
}

func (s *ruStore) StampRunTokenRecovered(_ context.Context, id uuid.UUID, jti, presented string) (bool, error) {
	if err := s.enter("StampRunTokenRecovered"); err != nil {
		return false, err
	}
	s.mu.Lock()
	s.stamps = append(s.stamps, ruStamp{run: id, jti: jti, presentedBy: presented})
	stamped, err, hook := s.stamped, s.stampErr, s.onStamp
	s.mu.Unlock()
	if hook != nil {
		hook(s)
	}
	return stamped, err
}

func (s *ruStore) RunRecoveryCheck(_ context.Context, _ uuid.UUID, storeRecovering bool) (store.RecoveryCheck, error) {
	if err := s.enter("RunRecoveryCheck"); err != nil {
		return store.RecoveryCheck{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.checkAsked = append(s.checkAsked, storeRecovering)
	return s.check, s.checkErr
}

func (s *ruStore) SetRunProxyPauses(context.Context, uuid.UUID, bool) error {
	return s.enter("SetRunProxyPauses")
}

func (s *ruStore) OutageWindowCovers(context.Context, *uuid.UUID, types.Placement, time.Time) (store.OutageWindow, bool, error) {
	if err := s.enter("OutageWindowCovers"); err != nil {
		return store.OutageWindow{}, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.window, s.covered, s.coverErr
}

func (s *ruStore) RunHeldContainedOnRunner(context.Context, uuid.UUID) (bool, error) {
	if err := s.enter("RunHeldContainedOnRunner"); err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.held, s.heldErr
}

func (s *ruStore) GetRunContainmentPending(context.Context, uuid.UUID) (store.RunContainmentPendingRow, error) {
	if err := s.enter("GetRunContainmentPending"); err != nil {
		return store.RunContainmentPendingRow{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.containmentErr != nil:
		return store.RunContainmentPendingRow{}, s.containmentErr
	case s.containmentPending:
		return store.RunContainmentPendingRow{}, nil
	}
	return store.RunContainmentPendingRow{}, store.ErrNotFound
}

func (s *ruStore) ListRunNodeRecords(context.Context, uuid.UUID) ([]types.RunNodeRecord, error) {
	return nil, s.enter("ListRunNodeRecords")
}

func (s *ruStore) QueueRunNodeRecord(context.Context, store.RunNodeRecordWrite) (types.RunnerPendingAction, error) {
	return types.RunnerPendingAction{}, s.enter("QueueRunNodeRecord")
}

func (s *ruStore) RunHasPendingRunnerAction(context.Context, uuid.UUID) (bool, error) {
	if err := s.enter("RunHasPendingRunnerAction"); err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pendingAction, nil
}

func (s *ruStore) GetRunner(_ context.Context, id uuid.UUID) (types.Runner, error) {
	if err := s.enter("GetRunner"); err != nil {
		return types.Runner{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.runnerErr != nil {
		return types.Runner{}, s.runnerErr
	}
	if id != s.runner.ID {
		return types.Runner{}, store.ErrNotFound
	}
	return s.runner, nil
}

func (s *ruStore) GetSiteConfig(context.Context) (types.SiteConfig, error) {
	if err := s.enter("GetSiteConfig"); err != nil {
		return types.SiteConfig{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return types.SiteConfig{Runners: &types.RunnerSettings{Enabled: s.runnersOn}}, s.siteCfgErr
}

// ruPartialStore is a store with none of the recovery reads. It counts the reads it is asked and fails them.
type ruPartialStore struct {
	store.Store
	mu    sync.Mutex
	reads int
}

func (p *ruPartialStore) GetRun(context.Context, uuid.UUID) (types.AgentRun, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reads++
	return types.AgentRun{}, errRuRead
}

func (p *ruPartialStore) readCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.reads
}

// The stores with all but one of the three parts the door needs (store.RunTokenRecoverer, store.OutageWindows and
// the runner containment read), and with none of them. Calling a part they lack panics.
type (
	ruNoRecoveryParts struct{ *ruPartialStore }
	ruNoHoldRead      struct {
		*ruPartialStore
		store.RunTokenRecoverer
		store.OutageWindows
	}
	ruNoWindows struct {
		*ruPartialStore
		store.RunTokenRecoverer
		runnerHoldReader
	}
	ruNoRecoverer struct {
		*ruPartialStore
		store.OutageWindows
		runnerHoldReader
	}
)

// ruEnv is a server whose identity provider signs with a key the test holds, so it can sign a token that expired.
type ruEnv struct {
	t      *testing.T
	srv    *Server
	idp    *embedded.Provider
	key    *ecdsa.PrivateKey
	audit  *syncAudit
	direct http.Handler // the router, as a request that did not come through a relay reaches it
	relay  http.Handler // the router behind the relay listener's route filter
}

// newRuEnv builds a server over st. revs is its revocation store (nil: an in-memory one); wrap, when set, wraps the
// embedded provider.
func newRuEnv(t *testing.T, st store.Store, revs identity.RevocationStore, wrap func(identity.Provider) identity.Provider) *ruEnv {
	t.Helper()
	key := ruKey(t)
	if revs == nil {
		revs = identitytest.NewMemRevocationStore()
	}
	rec := &syncAudit{}
	idp, err := embedded.New(key, "wardyn.local", revs, rec)
	if err != nil {
		t.Fatalf("embedded.New: %v", err)
	}
	var provider identity.Provider = idp
	if wrap != nil {
		provider = wrap(idp)
	}
	baseCtx, cancel := context.WithCancel(context.Background())
	srv := New(Config{
		BaseCtx:         baseCtx,
		Identity:        provider,
		Audit:           rec,
		Store:           st,
		AdminToken:      adminToken,
		TrustDomain:     "wardyn.local",
		ControlPlaneURL: "http://wardynd:8080",
	})
	t.Cleanup(func() {
		cancel()
		srv.WaitBackground()
	})
	return &ruEnv{t: t, srv: srv, idp: idp, key: key, audit: rec,
		direct: panicFails(t, srv.Handler()), relay: panicFails(t, srv.relayRoutes(srv.InternalHandler()))}
}

// do sends one request. relayedBy, when set, is the runner whose relay stream carries it.
func (e *ruEnv) do(method, path, token string, relayedBy uuid.UUID) *httptest.ResponseRecorder {
	e.t.Helper()
	r := httptest.NewRequest(method, path, nil)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	r.Host = "127.0.0.1"
	r.RemoteAddr = "127.0.0.1:54321"
	h := e.direct
	if relayedBy != uuid.Nil {
		r = r.WithContext(relayConnContext(r.Context(), relayedConn{runner: relayedBy}))
		h = e.relay
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(deadlineRecorder{w}, r)
	return w
}

// recover asks the recover door.
func (e *ruEnv) recover(token string, relayedBy uuid.UUID) *httptest.ResponseRecorder {
	e.t.Helper()
	return e.do(http.MethodPost, ruRecoverPath, token, relayedBy)
}

// expired is a token for runID, signed by this server's key, that expired ago ago.
func (e *ruEnv) expired(runID uuid.UUID, jti string, ago time.Duration) string {
	e.t.Helper()
	return ruToken(e.t, e.key, runID, jti, internalAudience, ago)
}

// ruAnswer is the part of a door's answer the tests read.
type ruAnswer struct {
	Reason string `json:"reason"`
	Token  string `json:"token"`
}

func ruRead(t *testing.T, w *httptest.ResponseRecorder) ruAnswer {
	t.Helper()
	var a ruAnswer
	if err := json.Unmarshal(w.Body.Bytes(), &a); err != nil {
		t.Fatalf("answer %d %q is not JSON: %v", w.Code, w.Body.String(), err)
	}
	return a
}

// ruRecoverRows counts the identity.recover rows recorded for runID by their outcome and reason ("denied:<reason>").
func ruRecoverRows(a *syncAudit, runID uuid.UUID) map[string]int {
	rows := map[string]int{}
	for _, ev := range a.eventsFor(runID, "identity.recover") {
		var d struct {
			Reason string `json:"reason"`
		}
		_ = json.Unmarshal(ev.Data, &d)
		rows[ev.Outcome+":"+d.Reason]++
	}
	return rows
}

// ruAuthFailRows counts the auth.fail rows recorded, by reason.
func ruAuthFailRows(a *syncAudit) map[string]int {
	rows := map[string]int{}
	for _, ev := range a.snapshot() {
		if ev.Action != "auth.fail" {
			continue
		}
		var d struct {
			Reason string `json:"reason"`
		}
		_ = json.Unmarshal(ev.Data, &d)
		rows[d.Reason]++
	}
	return rows
}

// TestInternalAuthStillRefusesExpired: an expired run token is refused by every route that holds internalAuth, as
// before; the recover door is the one route that takes it.
func TestInternalAuthStillRefusesExpired(t *testing.T) {
	key := ruKey(t)
	idp, err := embedded.New(key, "wardyn.local", identitytest.NewMemRevocationStore(), &syncAudit{})
	if err != nil {
		t.Fatal(err)
	}
	srv, _, _, _ := newAuthzMatrixServer(t, everyInternalMount, func(c *Config) { c.Identity = idp })
	tok := ruToken(t, key, uuid.New(), uuid.NewString(), internalAudience, 2*time.Hour)

	param := regexp.MustCompile(`\{[^}]*\}`)
	tested := map[string]bool{}
	var routes, door [][2]string
	err = chi.Walk(srv.router, func(method, route string, _ http.Handler, mws ...func(http.Handler) http.Handler) error {
		if !strings.HasPrefix(route, "/api/v1/internal/") {
			return nil
		}
		if route == "/api/v1/internal/runner-peer" {
			door = append(door, [2]string{method, route}) // its own mac, never internalAuth: asserted below
			return nil
		}
		for _, mw := range mws {
			if strings.HasSuffix(ruMiddlewareName(mw), ".(*Server).internalAuth-fm") {
				path := strings.ReplaceAll(param.ReplaceAllString(route, uuid.NewString()), "*", "x")
				routes = append(routes, [2]string{method, path})
				tested[method+" "+route] = true
				break
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, route := range []string{"POST /api/v1/internal/token/renew", "POST /api/v1/internal/credentials/mint", "POST /api/v1/internal/approvals"} {
		if !tested[route] {
			t.Fatalf("%s is not among the routes held by internalAuth: %v", route, tested)
		}
	}
	// everyInternalMount sets the internal listener, so the public handler answers 404 for every internal path:
	// the routes are requested on the handler that serves them.
	h := panicFails(t, srv.InternalHandler())
	for _, mp := range routes {
		r := httptest.NewRequest(mp[0], mp[1], nil)
		r.Header.Set("Authorization", "Bearer "+tok)
		r.Host, r.RemoteAddr = "127.0.0.1", "127.0.0.1:54321"
		w := httptest.NewRecorder()
		h.ServeHTTP(deadlineRecorder{w}, r)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s with an expired run token = %d %s, want 401", mp[0], mp[1], w.Code, w.Body.String())
		}
	}
	// The peer door reads no run token: an expired one with no peer token is refused 403 with no class.
	if len(door) != 1 || door[0] != [2]string{http.MethodGet, "/api/v1/internal/runner-peer"} {
		t.Fatalf("peer door routes walked = %v, want GET /api/v1/internal/runner-peer", door)
	}
	dr := httptest.NewRequest(http.MethodGet, door[0][1], nil)
	dr.Header.Set("Authorization", "Bearer "+tok)
	dr.Host, dr.RemoteAddr = "127.0.0.1", "127.0.0.1:54321"
	dw := httptest.NewRecorder()
	h.ServeHTTP(deadlineRecorder{dw}, dr)
	if dw.Code != http.StatusForbidden || dw.Header().Get(remote.HeaderPeerRefused) != "" {
		t.Errorf("the peer door with an expired run token = %d class %q, want 403 and no class", dw.Code, dw.Header().Get(remote.HeaderPeerRefused))
	}

	// The same token is not refused as invalid at the recover door: it answers about the store, not the token.
	r := httptest.NewRequest(http.MethodPost, ruRecoverPath, nil)
	r.Header.Set("Authorization", "Bearer "+tok)
	r.Host, r.RemoteAddr = "127.0.0.1", "127.0.0.1:54321"
	w := httptest.NewRecorder()
	h.ServeHTTP(deadlineRecorder{w}, r)
	if w.Code == http.StatusUnauthorized {
		t.Errorf("recover door with the same expired token = %d %s, want an answer other than 401", w.Code, w.Body.String())
	}
}

// ruCase is one row of TestRecover_RefusalClasses: the door's answer to one fixture.
type ruCase struct {
	name   string
	status int
	reason string // the answer's reason; "" for a 200
	final  bool   // the proxy stops asking: 403 or 501
	row    string // the one identity.recover row it writes ("denied:<reason>"); "" for none
	stamps int    // StampRunTokenRecovered calls

	kind  string        // "" an expired token, "live", "forged" or "none"
	age   time.Duration // how long ago the expired token expired; 0 for two hours
	revs  *ruRevocations
	wrap  func(identity.Provider) identity.Provider
	other func(st *ruStore) store.Store // the store the server sees in place of st
	// mut changes the passing fixture: st, the run and rid, the runner the run is placed on when mut gives it one.
	mut     func(st *ruStore, run *types.AgentRun, rid uuid.UUID)
	relayed bool // the request comes through rid's relay
	prep    func(t *testing.T, e *ruEnv, run types.AgentRun)
	after   func(t *testing.T, e *ruEnv, st *ruStore, w *httptest.ResponseRecorder)
}

// ruOnRunner places run on rid, as a local run.
func ruOnRunner(run *types.AgentRun, rid uuid.UUID) {
	run.Placement, run.RunnerID = types.PlacementLocal, &rid
}

// ruAfterStamp makes the stamp lose and runs f against the store at that moment, as a change that lands between the
// door's checks and its stamp does.
func ruAfterStamp(f func(s *ruStore, id uuid.UUID)) func(*ruStore, *types.AgentRun, uuid.UUID) {
	return func(st *ruStore, run *types.AgentRun, _ uuid.UUID) {
		id := run.ID
		st.stamped = false
		if f != nil {
			st.onStamp = func(s *ruStore) { s.with(func(s *ruStore) { f(s, id) }) }
		}
	}
}

// TestRecover_RefusalClasses answers every reason of the door and holds its status and its class: a 403 or 501 is
// final (the proxy stops asking), everything else is retryable. The first row is the control: with every check
// passing the door answers 200.
func TestRecover_RefusalClasses(t *testing.T) {
	past := time.Now().Add(-time.Hour)
	cases := []ruCase{
		{name: "every check passes", status: 200, row: "", stamps: 1},

		{name: "the provider cannot recover", status: 501, reason: "recovery_unsupported", final: true,
			wrap: func(p identity.Provider) identity.Provider { return ruNoRecovery{p} }},
		{name: "no bearer", status: 401, reason: "missing_run_token", kind: "none"},
		{name: "the token has not expired", status: 409, reason: "token_not_expired", kind: "live"},
		{name: "the token expired over 24 h ago", status: 403, reason: "token_too_old", final: true, age: 25 * time.Hour},
		{name: "the token is forged", status: 401, reason: "invalid_run_token", kind: "forged"},
		{name: "the revocation store cannot be read", status: 401, reason: "invalid_run_token", revs: &ruRevocations{err: errRuRead}},
		{name: "the run is revoked", status: 403, reason: "run_revoked", final: true, row: "denied:run_revoked",
			prep: func(t *testing.T, e *ruEnv, run types.AgentRun) {
				if err := e.idp.RevokeRun(context.Background(), run.ID); err != nil {
					t.Fatal(err)
				}
			}},
		{name: "the store lacks a recovery part", status: 501, reason: "recovery_unsupported", final: true, row: "denied:recovery_unsupported",
			other: func(*ruStore) store.Store { return &ruNoRecoveryParts{&ruPartialStore{}} }},
		{name: "no such run", status: 403, reason: "run_not_found", final: true, row: "denied:run_not_found",
			mut: func(st *ruStore, _ *types.AgentRun, _ uuid.UUID) { st.runErr = store.ErrNotFound }},
		{name: "the run cannot be read", status: 503, reason: "recovery_read_failed",
			mut: func(st *ruStore, _ *types.AgentRun, _ uuid.UUID) { st.runErr = errRuRead }},

		{name: "the run is terminal", status: 403, reason: string(authz.ReasonRunTerminal), final: true, row: "denied:run_terminal:FAILED",
			mut: func(_ *ruStore, run *types.AgentRun, _ uuid.UUID) { run.State = types.RunFailed }},
		{name: "the run is not running", status: 403, reason: "run_not_running", final: true, row: "denied:run_not_running",
			mut: func(_ *ruStore, run *types.AgentRun, _ uuid.UUID) { run.State = types.RunPending }},
		{name: "the run is past its end", status: 403, reason: "run_past_lifetime", final: true, row: "denied:run_past_lifetime",
			mut: func(_ *ruStore, run *types.AgentRun, _ uuid.UUID) { run.EndsAt = &past }},
		{name: "the run is past its lifetime cap", status: 403, reason: "run_past_lifetime", final: true, row: "denied:run_past_lifetime",
			mut: func(_ *ruStore, run *types.AgentRun, _ uuid.UUID) { run.LifetimeCapAt = &past }},
		{name: "the run is lost", status: 403, reason: string(authz.ReasonRunKept), final: true, row: "denied:run_lost:outage",
			mut: func(_ *ruStore, run *types.AgentRun, _ uuid.UUID) {
				run.LostAt, run.LostReason = &past, types.LostOutage
			}},
		{name: "the lineage was cut at a stopped pause", status: 403, reason: "run_token_cut", final: true, row: "denied:run_token_cut",
			mut: func(st *ruStore, _ *types.AgentRun, _ uuid.UUID) { st.lineage.JTI = store.TokenCutPrefix + "3" }},
		{name: "the lineage cannot be read", status: 503, reason: "recovery_read_failed", row: "denied:recovery_read_failed",
			mut: func(st *ruStore, _ *types.AgentRun, _ uuid.UUID) { st.lineageErr = errRuRead }},
		{name: "the run's row is gone when its lineage is read", status: 403, reason: "run_not_found", final: true, row: "denied:run_not_found",
			mut: func(st *ruStore, _ *types.AgentRun, _ uuid.UUID) { st.lineageErr = store.ErrNotFound }},
		{name: "a terminal run whose lineage cannot be read is still refused for good", status: 403, reason: string(authz.ReasonRunTerminal), final: true, row: "denied:run_terminal:FAILED",
			mut: func(st *ruStore, run *types.AgentRun, _ uuid.UUID) {
				run.State = types.RunFailed
				st.lineageErr = errRuRead
			}},
		{name: "the database's clock cannot be read", status: 503, reason: "recovery_read_failed", row: "denied:recovery_read_failed",
			mut: func(st *ruStore, _ *types.AgentRun, _ uuid.UUID) { st.dbNowErr = errRuRead }},
		{name: "the runner has the run containment pending", status: 403, reason: "run_containment_pending", final: true, row: "denied:run_containment_pending", relayed: true,
			mut: func(st *ruStore, run *types.AgentRun, rid uuid.UUID) {
				ruOnRunner(run, rid)
				st.containmentPending = true
			}},
		{name: "the containment read fails", status: 503, reason: "recovery_read_failed", row: "denied:recovery_read_failed", relayed: true,
			mut: func(st *ruStore, run *types.AgentRun, rid uuid.UUID) {
				ruOnRunner(run, rid)
				st.containmentErr = errRuRead
			}},
		{name: "the runner holds the run contained", status: 403, reason: "run_held_contained", final: true, row: "denied:run_held_contained", relayed: true,
			mut: func(st *ruStore, run *types.AgentRun, rid uuid.UUID) { ruOnRunner(run, rid); st.held = true }},
		{name: "the runner's containment read fails", status: 503, reason: "recovery_read_failed", row: "denied:recovery_read_failed", relayed: true,
			mut: func(st *ruStore, run *types.AgentRun, rid uuid.UUID) { ruOnRunner(run, rid); st.heldErr = errRuRead }},
		{name: "past the limit on the database's clock", status: 403, reason: "token_too_old", final: true, row: "denied:token_too_old", age: 23 * time.Hour,
			mut: func(st *ruStore, _ *types.AgentRun, _ uuid.UUID) { st.dbNow = time.Now().Add(2 * time.Hour) }},
		{name: "the recovery facts cannot be read", status: 503, reason: "recovery_read_failed", row: "denied:recovery_read_failed",
			mut: func(st *ruStore, _ *types.AgentRun, _ uuid.UUID) { st.checkErr = errRuRead }},
		{name: "the strict allowance is spent", status: 403, reason: "strict_allowance_spent", final: true, row: "denied:strict_allowance_spent",
			mut: func(st *ruStore, _ *types.AgentRun, _ uuid.UUID) { st.check.StrictSpent = true }},
		{name: "the presented token is not the run's current one", status: 503, reason: "token_lineage_retired", row: "denied:token_lineage_retired",
			mut: func(st *ruStore, _ *types.AgentRun, _ uuid.UUID) { st.lineage.JTI = "a-newer-token" }},
		{name: "no window covers the expiry yet", status: 503, reason: "outage_window_pending", row: "denied:outage_window_pending",
			mut: func(st *ruStore, _ *types.AgentRun, _ uuid.UUID) { st.covered = false },
			after: func(t *testing.T, _ *ruEnv, _ *ruStore, w *httptest.ResponseRecorder) {
				if got := w.Header().Get("Retry-After"); got != "60" {
					t.Errorf("Retry-After = %q, want 60", got)
				}
			}},
		{name: "the window read fails", status: 503, reason: "recovery_read_failed", row: "denied:recovery_read_failed",
			mut: func(st *ruStore, _ *types.AgentRun, _ uuid.UUID) { st.coverErr = errRuRead }},
		{name: "the runner's lease is being sent", status: 503, reason: "run_renew_lease_busy", relayed: true,
			mut: func(_ *ruStore, run *types.AgentRun, rid uuid.UUID) { ruOnRunner(run, rid) },
			prep: func(t *testing.T, e *ruEnv, run types.AgentRun) {
				release, ok := e.srv.runnerOwnerSlots(run.CreatedBy).acquireLease(context.Background(), 0)
				if !ok {
					t.Fatal("the owner's lease slot was not free")
				}
				t.Cleanup(release)
			}},

		{name: "the stamp fails", status: 503, reason: "recovery_read_failed", row: "denied:recovery_read_failed", stamps: 1,
			mut: func(st *ruStore, _ *types.AgentRun, _ uuid.UUID) { st.stampErr = errRuRead }},
		{name: "the stamp loses and nothing else changed", status: 503, reason: "recovery_read_failed", row: "denied:recovery_read_failed", stamps: 1,
			mut: ruAfterStamp(nil)},
		{name: "the stamp loses and the re-read fails", status: 503, reason: "recovery_read_failed", row: "denied:recovery_read_failed", stamps: 1,
			mut: ruAfterStamp(func(s *ruStore, _ uuid.UUID) { s.runErr = errRuRead })},
		{name: "the stamp loses and the run's row is gone at the re-read", status: 403, reason: "run_not_found", final: true, row: "denied:run_not_found", stamps: 1,
			mut: ruAfterStamp(func(s *ruStore, _ uuid.UUID) { s.lineageErr = store.ErrNotFound })},
		{name: "the stamp loses and the run is gone at the re-read", status: 403, reason: "run_not_found", final: true, row: "denied:run_not_found", stamps: 1,
			mut: ruAfterStamp(func(s *ruStore, _ uuid.UUID) { s.runErr = store.ErrNotFound })},
		{name: "the stamp loses to a loss mark", status: 403, reason: string(authz.ReasonRunKept), final: true, row: "denied:run_lost:outage", stamps: 1,
			mut: ruAfterStamp(func(s *ruStore, id uuid.UUID) {
				run := s.runs[id]
				run.LostAt, run.LostReason = &past, types.LostOutage
				s.runs[id] = run
			})},
		{name: "the stamp loses to the run's end", status: 403, reason: string(authz.ReasonRunTerminal), final: true, row: "denied:run_terminal:KILLED", stamps: 1,
			mut: ruAfterStamp(func(s *ruStore, id uuid.UUID) {
				run := s.runs[id]
				run.State = types.RunKilled
				s.runs[id] = run
			})},
		{name: "the stamp loses to a cut", status: 403, reason: "run_token_cut", final: true, row: "denied:run_token_cut", stamps: 1,
			mut: ruAfterStamp(func(s *ruStore, _ uuid.UUID) { s.lineage.JTI = store.TokenCutPrefix + "4" })},
		{name: "the stamp loses to another token", status: 503, reason: "token_lineage_retired", row: "denied:token_lineage_retired", stamps: 1,
			mut: ruAfterStamp(func(s *ruStore, _ uuid.UUID) { s.lineage.JTI = "a-newer-token" })},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			runID, jti, rid := uuid.New(), uuid.NewString(), uuid.New()
			st := ruPassingStore(runID, jti)
			run := st.runs[runID]
			if c.mut != nil {
				c.mut(st, &run, rid)
			}
			st.runs[runID] = run
			var seen store.Store = st
			if c.other != nil {
				seen = c.other(st)
			}
			var revs identity.RevocationStore
			if c.revs != nil {
				revs = c.revs
			}
			e := newRuEnv(t, seen, revs, c.wrap)
			var tok string
			switch c.kind {
			case "":
				tok = e.expired(runID, jti, cmp.Or(c.age, 2*time.Hour))
			case "live":
				id, err := e.idp.MintRunIdentity(context.Background(), runID, ruSub, "", internalAudience, false)
				if err != nil {
					t.Fatal(err)
				}
				tok = id.Token
			case "forged":
				tok = ruToken(t, ruKey(t), runID, jti, internalAudience, 2*time.Hour)
			}
			if c.prep != nil {
				c.prep(t, e, run)
			}
			var relayedBy uuid.UUID
			if c.relayed {
				relayedBy = rid
			}
			w := e.recover(tok, relayedBy)
			if w.Code != c.status {
				t.Fatalf("status = %d %s, want %d %q", w.Code, w.Body.String(), c.status, c.reason)
			}
			ans := ruRead(t, w)
			if c.status == http.StatusOK {
				if ans.Token == "" {
					t.Fatalf("a 200 carries no token: %s", w.Body.String())
				}
			} else {
				if ans.Reason != c.reason {
					t.Errorf("reason = %q, want %q", ans.Reason, c.reason)
				}
				if ans.Token != "" {
					t.Errorf("a refusal handed out a token: %s", w.Body.String())
				}
				if final := w.Code == http.StatusForbidden || w.Code == http.StatusNotImplemented; final != c.final {
					t.Errorf("status %d is final=%v, want final=%v: only 403 and 501 stop the proxy asking", w.Code, final, c.final)
				}
			}
			want := map[string]int{}
			if c.row != "" {
				want[c.row] = 1
			}
			if got := ruRecoverRows(e.audit, runID); !reflect.DeepEqual(got, want) {
				t.Errorf("identity.recover rows = %v, want %v", got, want)
			}
			st.with(func(s *ruStore) {
				if len(s.stamps) != c.stamps {
					t.Errorf("stamp calls = %d, want %d", len(s.stamps), c.stamps)
				}
			})
			if c.after != nil {
				c.after(t, e, st, w)
			}
			if c.status == http.StatusOK {
				st.with(func(s *ruStore) {
					if s.stamps[0].presentedBy != jti || s.stamps[0].jti == jti {
						t.Errorf("stamp = %+v, want the presented token %s replaced by a new one", s.stamps[0], jti)
					}
					claims, err := e.idp.Verify(context.Background(), ans.Token, internalAudience)
					if err != nil || claims.JTI != s.stamps[0].jti || claims.RunID != runID {
						t.Errorf("the token handed out = %+v, %v, want the stamped token %s of run %s", claims, err, s.stamps[0].jti, runID)
					}
				})
				if rows := e.audit.eventsFor(runID, "run.token.recover"); len(rows) != 1 || rows[0].Outcome != "success" {
					t.Errorf("run.token.recover rows = %+v, want one success", rows)
				}
			}
		})
	}
}

// TestRecover_RevocationStoreErrorIsRetryable: a revocation store that cannot be read answers 401 with no token and
// nothing read from the run store; the same token recovers once the revocation store answers.
func TestRecover_RevocationStoreErrorIsRetryable(t *testing.T) {
	runID, jti := uuid.New(), uuid.NewString()
	st := ruPassingStore(runID, jti)
	revs := &ruRevocations{err: errRuRead}
	e := newRuEnv(t, st, revs, nil)
	tok := e.expired(runID, jti, 2*time.Hour)

	w := e.recover(tok, uuid.Nil)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("recover with the revocation store down = %d %s, want 401", w.Code, w.Body.String())
	}
	if ans := ruRead(t, w); ans.Token != "" || ans.Reason != "invalid_run_token" {
		t.Errorf("answer = %+v, want the invalid_run_token refusal with no token", ans)
	}
	if revs.count() != 1 || st.callCount() != 0 {
		t.Errorf("revocation reads = %d, run store calls = %d, want 1 and 0", revs.count(), st.callCount())
	}

	revs.set(nil)
	if w := e.recover(tok, uuid.Nil); w.Code != http.StatusOK {
		t.Fatalf("recover once the revocation store answers = %d %s, want 200", w.Code, w.Body.String())
	}
}

// TestRecover_409GoesBackToRenew: a token that has not expired is answered 409 token_not_expired with nothing read,
// and the renew door, where the proxy goes next, renews that same token.
func TestRecover_409GoesBackToRenew(t *testing.T) {
	runID := uuid.New()
	st := ruPassingStore(runID, "")
	e := newRuEnv(t, st, nil, nil)
	id, err := e.idp.MintRunIdentity(context.Background(), runID, ruSub, "", internalAudience, false)
	if err != nil {
		t.Fatal(err)
	}

	w := e.recover(id.Token, uuid.Nil)
	if w.Code != http.StatusConflict {
		t.Fatalf("recover with a live token = %d %s, want 409", w.Code, w.Body.String())
	}
	if ans := ruRead(t, w); ans.Reason != "token_not_expired" || ans.Token != "" {
		t.Errorf("answer = %+v, want token_not_expired with no token", ans)
	}
	if st.callCount() != 0 {
		t.Errorf("the 409 read the store %d times, want 0", st.callCount())
	}
	if rows := ruRecoverRows(e.audit, runID); len(rows) != 0 {
		t.Errorf("identity.recover rows = %v, want none for a live token", rows)
	}

	w = e.do(http.MethodPost, ruRenewPath, id.Token, uuid.Nil)
	if w.Code != http.StatusOK {
		t.Fatalf("renew with the same token = %d %s, want 200", w.Code, w.Body.String())
	}
	renewed := ruRead(t, w).Token
	if renewed == "" || renewed == id.Token {
		t.Fatalf("renew handed out %q, want a fresh token", renewed)
	}
	if claims, err := e.idp.Verify(context.Background(), renewed, internalAudience); err != nil || claims.RunID != runID {
		t.Errorf("the renewed token verifies as %+v, %v, want run %s", claims, err, runID)
	}
	st.with(func(s *ruStore) {
		if len(s.stamps) != 0 {
			t.Errorf("recovery stamps = %d, want none: the renew door does not recover", len(s.stamps))
		}
	})
}

// TestRecover_StoreWithoutRecoveryRefuses: a valid expired token at a server whose store lacks any one part of the
// recovery (the recoverer, the windows, the runner containment read) answers 501, and nothing is read from it.
func TestRecover_StoreWithoutRecoveryRefuses(t *testing.T) {
	cases := map[string]func(p *ruPartialStore) store.Store{
		"no part": func(p *ruPartialStore) store.Store { return &ruNoRecoveryParts{p} },
		"no runner containment read": func(p *ruPartialStore) store.Store {
			return &ruNoHoldRead{ruPartialStore: p}
		},
		"no windows":   func(p *ruPartialStore) store.Store { return &ruNoWindows{ruPartialStore: p} },
		"no recoverer": func(p *ruPartialStore) store.Store { return &ruNoRecoverer{ruPartialStore: p} },
	}
	for name, build := range cases {
		t.Run(name, func(t *testing.T) {
			runID, jti := uuid.New(), uuid.NewString()
			part := &ruPartialStore{}
			e := newRuEnv(t, build(part), nil, nil)
			w := e.recover(e.expired(runID, jti, 2*time.Hour), uuid.Nil)
			if w.Code != http.StatusNotImplemented {
				t.Fatalf("recover = %d %s, want 501", w.Code, w.Body.String())
			}
			if ans := ruRead(t, w); ans.Reason != "recovery_unsupported" || ans.Token != "" {
				t.Errorf("answer = %+v, want recovery_unsupported with no token", ans)
			}
			if part.readCount() != 0 {
				t.Errorf("store reads = %d, want 0", part.readCount())
			}
			if got, want := ruRecoverRows(e.audit, runID), map[string]int{"denied:recovery_unsupported": 1}; !reflect.DeepEqual(got, want) {
				t.Errorf("identity.recover rows = %v, want %v", got, want)
			}
		})
	}
}

// TestRecover_ProviderWithoutRecoveryRefuses: a server whose identity provider verifies no expired token answers 501
// before it reads the bearer or the store.
func TestRecover_ProviderWithoutRecoveryRefuses(t *testing.T) {
	runID, jti := uuid.New(), uuid.NewString()
	st := ruPassingStore(runID, jti)
	st.failAll = true
	e := newRuEnv(t, st, nil, func(p identity.Provider) identity.Provider { return ruNoRecovery{p} })
	for name, tok := range map[string]string{"an expired token": e.expired(runID, jti, 2*time.Hour), "no bearer": ""} {
		w := e.recover(tok, uuid.Nil)
		if w.Code != http.StatusNotImplemented {
			t.Fatalf("%s: recover = %d %s, want 501", name, w.Code, w.Body.String())
		}
		if ans := ruRead(t, w); ans.Reason != "recovery_unsupported" || ans.Token != "" {
			t.Errorf("%s: answer = %+v, want recovery_unsupported with no token", name, ans)
		}
	}
	if st.callCount() != 0 {
		t.Errorf("store calls = %d, want 0", st.callCount())
	}
	if got := ruAuthFailRows(e.audit)["recovery_unsupported"]; got == 0 {
		t.Errorf("auth.fail rows = %v, want a recovery_unsupported row", ruAuthFailRows(e.audit))
	}
}

// TestRecover_RevokedRunRefused: a revoked run's expired token is refused 403 run_revoked, final, with no token and
// nothing read from the store, whether the run or the token was revoked.
func TestRecover_RevokedRunRefused(t *testing.T) {
	revoke := map[string]func(e *ruEnv, runID uuid.UUID, jti string) error{
		"run": func(e *ruEnv, runID uuid.UUID, _ string) error { return e.idp.RevokeRun(context.Background(), runID) },
		"token": func(e *ruEnv, runID uuid.UUID, jti string) error {
			return e.idp.RevokeJTI(context.Background(), jti, runID)
		},
	}
	for name, do := range revoke {
		t.Run(name, func(t *testing.T) {
			runID, jti := uuid.New(), uuid.NewString()
			st := ruPassingStore(runID, jti)
			e := newRuEnv(t, st, nil, nil)
			tok := e.expired(runID, jti, 2*time.Hour)
			if err := do(e, runID, jti); err != nil {
				t.Fatal(err)
			}
			w := e.recover(tok, uuid.Nil)
			if w.Code != http.StatusForbidden {
				t.Fatalf("recover = %d %s, want 403", w.Code, w.Body.String())
			}
			if ans := ruRead(t, w); ans.Reason != "run_revoked" || ans.Token != "" {
				t.Errorf("answer = %+v, want run_revoked with no token", ans)
			}
			if st.callCount() != 0 {
				t.Errorf("store calls = %d, want 0: a revoked token is refused before the store", st.callCount())
			}
			if got, want := ruRecoverRows(e.audit, runID), map[string]int{"denied:run_revoked": 1}; !reflect.DeepEqual(got, want) {
				t.Errorf("identity.recover rows = %v, want %v", got, want)
			}
		})
	}
}

// TestRecover_ForgedSignatureRefusedBeforeStore: a token signed by another key, a token for another audience and a
// string that is no token are each refused 401 with no token, and a store that fails every call is never reached.
func TestRecover_ForgedSignatureRefusedBeforeStore(t *testing.T) {
	runID, jti := uuid.New(), uuid.NewString()
	st := ruPassingStore(runID, jti)
	st.failAll = true
	revs := &ruRevocations{err: errRuRead}
	e := newRuEnv(t, st, revs, nil)
	tokens := map[string]string{
		"another key":    ruToken(t, ruKey(t), runID, jti, internalAudience, 2*time.Hour),
		"wrong audience": ruToken(t, e.key, runID, jti, "wardyn-groundtruth", 2*time.Hour),
		"not a token":    "not-a-jwt",
	}
	for name, tok := range tokens {
		w := e.recover(tok, uuid.Nil)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s: recover = %d %s, want 401", name, w.Code, w.Body.String())
			continue
		}
		if ans := ruRead(t, w); ans.Token != "" || ans.Reason != "invalid_run_token" {
			t.Errorf("%s: answer = %+v, want the invalid_run_token refusal with no token", name, ans)
		}
	}
	if st.callCount() != 0 || revs.count() != 0 {
		t.Errorf("store calls = %d, revocation reads = %d, want 0 and 0: a token that does not verify costs no read", st.callCount(), revs.count())
	}
	if rows := ruRecoverRows(e.audit, runID); len(rows) != 0 {
		t.Errorf("identity.recover rows = %v, want none for a token that does not verify", rows)
	}
}

// TestRecover_TooOldRefusedBeforeStore: a validly signed token that expired 25 h ago is refused 403 token_too_old
// with a store and a revocation store that fail every call, neither of them is read, and 50 such requests write no
// more auth.fail rows than auditAuthFailedAs lets through.
func TestRecover_TooOldRefusedBeforeStore(t *testing.T) {
	runID, jti := uuid.New(), uuid.NewString()
	st := ruPassingStore(runID, jti)
	st.failAll = true
	revs := &ruRevocations{err: errRuRead}
	e := newRuEnv(t, st, revs, nil)
	tok := e.expired(runID, jti, 25*time.Hour)

	start := time.Now()
	for range 50 {
		w := e.recover(tok, uuid.Nil)
		if w.Code != http.StatusForbidden {
			t.Fatalf("recover = %d %s, want 403", w.Code, w.Body.String())
		}
		if ans := ruRead(t, w); ans.Reason != "token_too_old" || ans.Token != "" {
			t.Fatalf("answer = %+v, want token_too_old with no token", ans)
		}
	}
	if st.callCount() != 0 || revs.count() != 0 {
		t.Errorf("store calls = %d, revocation reads = %d, want 0 and 0", st.callCount(), revs.count())
	}
	// auditAuthFailedAs writes a burst and then one row a second: nothing more may reach the trail.
	budget := int(authFailedBurst+authFailedRatePerSec*time.Since(start).Seconds()) + 1
	rows := ruAuthFailRows(e.audit)["token_too_old"]
	if rows == 0 || rows > budget || rows >= 50 {
		t.Errorf("token_too_old auth.fail rows = %d for 50 requests, want between 1 and the limiter's %d", rows, budget)
	}
	if got := ruRecoverRows(e.audit, runID); len(got) != 0 {
		t.Errorf("identity.recover rows = %v, want none: the too-old refusal is audited as auth.fail", got)
	}
}

// TestRecover_DeniedRowsCoalesced: asks refused for the same reason write one identity.recover row per run and
// reason, not one per ask; a second reason for a run writes its own row, and so does another run.
func TestRecover_DeniedRowsCoalesced(t *testing.T) {
	runA, runB := uuid.New(), uuid.New()
	jtiA, jtiB := uuid.NewString(), uuid.NewString()
	st := newRuStore()
	for _, id := range []uuid.UUID{runA, runB} {
		st.runs[id] = types.AgentRun{ID: id, CreatedBy: ruSub, State: types.RunRunning, Placement: types.PlacementRemote}
	}
	// An empty lineage accepts its presenter, and no window covers either token's expiry yet.
	e := newRuEnv(t, st, nil, nil)
	ask := func(run uuid.UUID, jti string, n int, wantStatus int, wantReason string) {
		t.Helper()
		tok := e.expired(run, jti, 2*time.Hour)
		for range n {
			w := e.recover(tok, uuid.Nil)
			if w.Code != wantStatus {
				t.Fatalf("recover for %s = %d %s, want %d %s", run, w.Code, w.Body.String(), wantStatus, wantReason)
			}
			if ans := ruRead(t, w); ans.Reason != wantReason || ans.Token != "" {
				t.Fatalf("answer = %+v, want %s with no token", ans, wantReason)
			}
		}
	}
	rowsOf := func(run uuid.UUID) map[string]int { return ruRecoverRows(e.audit, run) }

	ask(runA, jtiA, 20, http.StatusServiceUnavailable, "outage_window_pending")
	if got, want := rowsOf(runA), map[string]int{"denied:outage_window_pending": 1}; !reflect.DeepEqual(got, want) {
		t.Fatalf("run A after 20 asks: rows = %v, want %v", got, want)
	}
	ask(runB, jtiB, 20, http.StatusServiceUnavailable, "outage_window_pending")
	if got, want := rowsOf(runB), map[string]int{"denied:outage_window_pending": 1}; !reflect.DeepEqual(got, want) {
		t.Fatalf("run B after 20 asks: rows = %v, want %v", got, want)
	}

	st.with(func(s *ruStore) { s.lineage.JTI = "a-newer-token" })
	ask(runA, jtiA, 20, http.StatusServiceUnavailable, "token_lineage_retired")
	if got, want := rowsOf(runA), map[string]int{"denied:outage_window_pending": 1, "denied:token_lineage_retired": 1}; !reflect.DeepEqual(got, want) {
		t.Errorf("run A after a second reason: rows = %v, want %v", got, want)
	}
	if got, want := rowsOf(runB), map[string]int{"denied:outage_window_pending": 1}; !reflect.DeepEqual(got, want) {
		t.Errorf("run B after run A's second reason: rows = %v, want %v", got, want)
	}
}

// TestRecover_RelayBindingHolds: a relayed recover answers only for a local run on the relaying runner, with no
// runner action pending; a local run's recover is refused off the relay, and a remote run's is unchanged.
func TestRecover_RelayBindingHolds(t *testing.T) {
	other := uuid.New()
	cases := []struct {
		name    string
		status  int
		reason  string // "" for a 200
		onRun   bool   // the run is a local run on rid; without it a local run records no runner
		remote  bool
		relayed string // "owner", "other" or "" for none
		pending bool
	}{
		{name: "the run's own runner", status: 200, onRun: true, relayed: "owner"},
		{name: "another runner", status: 403, reason: "relay_runner_mismatch", onRun: true, relayed: "other"},
		{name: "a remote run through a relay", status: 403, reason: "relay_runner_mismatch", remote: true, relayed: "owner"},
		{name: "a local run with no runner", status: 403, reason: "relay_runner_mismatch", relayed: "owner"},
		{name: "a local run off the relay", status: 403, reason: "relay_required", onRun: true},
		{name: "a runner action pending", status: 409, reason: "runner_action_pending", onRun: true, relayed: "owner", pending: true},
		{name: "a remote run off the relay", status: 200, remote: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			runID, jti, rid := uuid.New(), uuid.NewString(), uuid.New()
			st := ruPassingStore(runID, jti)
			st.pendingAction = c.pending
			run := st.runs[runID]
			if !c.remote {
				run.Placement = types.PlacementLocal
			}
			if c.onRun {
				run.RunnerID = &rid
			}
			st.runs[runID] = run
			e := newRuEnv(t, st, nil, nil)
			var relayedBy uuid.UUID
			switch c.relayed {
			case "owner":
				relayedBy = rid
			case "other":
				relayedBy = other
			}
			w := e.recover(e.expired(runID, jti, 2*time.Hour), relayedBy)
			if w.Code != c.status {
				t.Fatalf("recover = %d %s, want %d %q", w.Code, w.Body.String(), c.status, c.reason)
			}
			ans := ruRead(t, w)
			if c.status == http.StatusOK {
				if ans.Token == "" {
					t.Errorf("a 200 carries no token: %s", w.Body.String())
				}
				return
			}
			if ans.Reason != c.reason || ans.Token != "" {
				t.Errorf("answer = %+v, want %s with no token", ans, c.reason)
			}
			st.with(func(s *ruStore) {
				if len(s.stamps) != 0 {
					t.Errorf("stamp calls = %d, want 0: a refused relay stamps nothing", len(s.stamps))
				}
			})
		})
	}
}
