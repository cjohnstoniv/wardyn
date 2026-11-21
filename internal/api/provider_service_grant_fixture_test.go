// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The shared doubles of the service grant tests: a scripted identity provider reached through the server's
// ServiceGrantTransport (no network), a secret store that keeps revisions, a site store that implements
// store.ServiceGrantStore in memory, and a clock. The fixture builds on oidcRunFixture, whose row already signs
// people in, so the row a service grant lives on is a real one.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const (
	svcOwner        = "service:ci-bot"
	svcOtherOwner   = "service:other-bot"
	svcClientID     = "svc-client"
	svcClientSecret = "svc-client-secret-0001-abcdefgh"
	svcTokenURL     = "https://idp.example.test/oauth/token"
)

// svcClock is the fake clock every server of a fixture reads.
type svcClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *svcClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *svcClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// svcReq is one request the identity provider received.
type svcReq struct {
	Method string
	URL    string
	Header http.Header
	Form   url.Values
	Raw    string
	// Deadline is the request context's deadline (zero when it has none).
	Deadline time.Time
}

// svcAnswer is what the scripted identity provider does with a request. Zero means the default success.
type svcAnswer struct {
	Status int
	Body   string
	Header http.Header
	Block  <-chan struct{} // the answer waits here, or for the request's context
	Err    error           // a transport failure
}

// svcIdP is the identity provider: an http.RoundTripper that records each request and answers from respond.
type svcIdP struct {
	mu      sync.Mutex
	reqs    []svcReq
	respond func(n int, r svcReq) svcAnswer
}

func svcToken(n int) string { return fmt.Sprintf("svc-access-token-%04d-0123456789abcdefghijkl", n) }

func svcOK(n, expiresIn int) svcAnswer {
	return svcAnswer{Status: 200, Body: fmt.Sprintf(`{"access_token":%q,"token_type":"Bearer","expires_in":%d}`, svcToken(n), expiresIn)}
}

func svcOAuthError(status int, code string) svcAnswer {
	return svcAnswer{Status: status, Body: fmt.Sprintf(`{"error":%q,"error_description":"never kept"}`, code)}
}

func (i *svcIdP) RoundTrip(req *http.Request) (*http.Response, error) {
	raw, _ := io.ReadAll(req.Body)
	form, _ := url.ParseQuery(string(raw))
	r := svcReq{Method: req.Method, URL: req.URL.String(), Header: req.Header.Clone(), Form: form, Raw: string(raw)}
	r.Deadline, _ = req.Context().Deadline()
	i.mu.Lock()
	i.reqs = append(i.reqs, r)
	n, fn := len(i.reqs), i.respond
	i.mu.Unlock()
	a := svcOK(n, 3600)
	if fn != nil {
		if got := fn(n, r); got.Status != 0 || got.Err != nil {
			a = got
		}
	}
	if a.Block != nil {
		select {
		case <-a.Block:
		case <-req.Context().Done():
			return nil, req.Context().Err()
		}
	}
	if a.Err != nil {
		return nil, a.Err
	}
	h := a.Header
	if h == nil {
		h = http.Header{"Content-Type": {"application/json"}}
	}
	return &http.Response{StatusCode: a.Status, Header: h, Body: io.NopCloser(strings.NewReader(a.Body)), Request: req}, nil
}

func (i *svcIdP) requests() []svcReq {
	i.mu.Lock()
	defer i.mu.Unlock()
	return append([]svcReq(nil), i.reqs...)
}

func (i *svcIdP) count() int { return len(i.requests()) }

// svcSecrets is an operator-namespace secret store that keeps a revision per name, can fail or hold each
// operation, and records what it was asked. For("") is the store itself; another owner gets an empty view.
type svcSecrets struct {
	mu     sync.Mutex
	m      map[string][]byte
	rev    map[string]int
	noRev  bool  // the store keeps no revision: Revision answers ErrNoRevision
	revErr error // Revision fails
	getErr error
	putErr error
	delErr error
	// putPartial makes Put store the value and then answer ErrRowNotWritten: the value reached the external
	// store but its row was not written.
	putPartial bool
	// revDeadlines are the context deadlines Revision was called with, in order (zero when none).
	revDeadlines []time.Time
	// afterRev runs after a Revision call has its answer, outside the store's lock; it may block.
	afterRev func()
	// revDelay makes the next Revision call under a context with a deadline take this long, or end at the deadline.
	revDelay time.Duration
	// hold runs before each operation, outside the store's lock; it may block.
	hold  func(op, name string)
	calls []string
}

func newSvcSecrets() *svcSecrets { return &svcSecrets{m: map[string][]byte{}, rev: map[string]int{}} }

func (s *svcSecrets) Name() string { return "svc-mem" }

func (s *svcSecrets) note(op, name string) func() {
	s.mu.Lock()
	s.calls = append(s.calls, op+" "+name)
	hold := s.hold
	s.mu.Unlock()
	if hold != nil {
		hold(op, name)
	}
	return func() {}
}

func (s *svcSecrets) Put(ctx context.Context, name string, v []byte) error {
	s.note("put", name)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.putErr != nil {
		return s.putErr
	}
	if want, ok := secretstore.IfRevisionFrom(ctx); ok && !s.noRev && want != s.revOf(name) {
		return secretstore.ErrRevisionChanged
	}
	s.m[name] = append([]byte(nil), v...)
	s.rev[name]++
	if s.putPartial {
		return secretstore.ErrRowNotWritten
	}
	return nil
}

func (s *svcSecrets) Get(_ context.Context, name string) ([]byte, error) {
	s.note("get", name)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.getErr != nil {
		return nil, s.getErr
	}
	v, ok := s.m[name]
	if !ok {
		return nil, secretstore.ErrNotFound
	}
	return append([]byte(nil), v...), nil
}

func (s *svcSecrets) Delete(ctx context.Context, name string) error {
	s.note("delete", name)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.delErr != nil {
		return s.delErr
	}
	if want, ok := secretstore.IfRevisionFrom(ctx); ok && !s.noRev && want != s.revOf(name) {
		return secretstore.ErrRevisionChanged
	}
	if _, ok := s.m[name]; !ok {
		return secretstore.ErrNotFound
	}
	delete(s.m, name)
	s.rev[name]++
	return nil
}

func (s *svcSecrets) revOf(name string) string {
	if _, ok := s.m[name]; !ok {
		return ""
	}
	return fmt.Sprint(s.rev[name])
}

// Revision implements secretstore.Revisioned.
func (s *svcSecrets) Revision(ctx context.Context, name string) (string, error) {
	s.mu.Lock()
	dl, bounded := ctx.Deadline()
	s.revDeadlines = append(s.revDeadlines, dl)
	delay := s.revDelay
	if bounded {
		s.revDelay = 0 // one read only: the publish's, not the failure's re-reads after it
	}
	s.mu.Unlock()
	if delay > 0 && bounded {
		select { // a store read that takes delay and honours its context's deadline, as a database read does
		case <-time.After(delay):
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	s.mu.Lock()
	rev, err, after := s.revOf(name), s.revErr, s.afterRev
	if s.noRev {
		rev, err = "", secretstore.ErrNoRevision
	} else if err != nil {
		rev = ""
	}
	s.mu.Unlock()
	if after != nil {
		after() // the read has its answer; the caller has not yet seen it
	}
	return rev, err
}

func (s *svcSecrets) List(context.Context) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for k := range s.m {
		out = append(out, k)
	}
	return out, nil
}

func (s *svcSecrets) DeleteEverywhere(_ context.Context, names []string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, name := range names {
		if _, ok := s.m[name]; ok {
			delete(s.m, name)
			s.rev[name]++
			n++
		}
	}
	return n, nil
}

func (s *svcSecrets) Holders(_ context.Context, names []string) (map[string][]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string][]string{}
	for _, name := range names {
		if _, ok := s.m[name]; ok {
			out[name] = []string{""}
		}
	}
	return out, nil
}

func (s *svcSecrets) For(owner string) secretstore.Store {
	if owner == "" {
		return s
	}
	return newSvcSecrets()
}

// set writes a value as a direct writer would (another process, the external store), bumping its revision and
// bypassing every hook and error of the double.
func (s *svcSecrets) set(name string, v []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[name] = append([]byte(nil), v...)
	s.rev[name]++
}

// rotateInPlace writes a value in the store itself, as a rotation in the external store does: the value changes
// and the revision the database keeps for it does not, so no revision check can see it.
func (s *svcSecrets) rotateInPlace(name string, v []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[name] = append([]byte(nil), v...)
}

func (s *svcSecrets) has(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.m[name]
	return ok
}

func (s *svcSecrets) callsOf(op string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, c := range s.calls {
		if strings.HasPrefix(c, op+" ") {
			n++
		}
	}
	return n
}

// svcStore is the fixture's store: the run fixture's store with store.ServiceGrantStore added in memory. A
// grant lives on its row, as the real store attaches it, so the site document is the whole truth here; the
// claims and the status rows are the only extra state.
type svcStore struct {
	*azA3Store
	gmu       sync.Mutex
	activated bool
	cleanups  map[string]store.ServiceGrantCleanup
	status    map[string]store.ServiceGrantStatusRow
	seen      map[string]time.Time
	now       func() time.Time
	// putGate runs inside PutSiteConfigGrants, before the write; it may block or fail the write.
	putGate func() error
	// upsertGate runs inside UpsertServiceGrantStatus; it may block.
	upsertGate func(row store.ServiceGrantStatusRow)
	settles    int
	// getErr, when set, is asked before each configuration read; an error fails the read.
	getErr func() error
	// docErr, when set, is asked before each read of the grant keys and claims; an error fails the read.
	docErr func() error
}

func (s *svcStore) GetSiteConfig(ctx context.Context) (types.SiteConfig, error) {
	s.gmu.Lock()
	g := s.getErr
	s.gmu.Unlock()
	if g != nil {
		if err := g(); err != nil {
			return types.SiteConfig{}, err
		}
	}
	return s.azA3Store.GetSiteConfig(ctx)
}

func (s *svcStore) PutSiteConfig(_ context.Context, cfg types.SiteConfig) (types.SiteConfig, error) {
	s.azureSiteStore.mu.Lock()
	defer s.azureSiteStore.mu.Unlock()
	s.azureSiteStore.sc = cfg
	return cfg, nil
}

func (s *svcStore) PutSiteConfigGrants(ctx context.Context, cfg types.SiteConfig, change store.ServiceGrantChange) (types.SiteConfig, error) {
	if s.putGate != nil {
		if err := s.putGate(); err != nil {
			return types.SiteConfig{}, err
		}
	}
	s.gmu.Lock()
	activated := s.activated
	s.gmu.Unlock()
	if !activated && cfg.ModelProviders != nil {
		for _, p := range cfg.ModelProviders.Providers {
			if p.OIDC != nil && p.OIDC.ServiceGrant != nil {
				return types.SiteConfig{}, store.ErrServiceGrantsNotActivated
			}
		}
	}
	saved, err := s.PutSiteConfig(ctx, cfg)
	if err != nil {
		return saved, err
	}
	s.gmu.Lock()
	defer s.gmu.Unlock()
	if s.cleanups == nil {
		s.cleanups = map[string]store.ServiceGrantCleanup{}
	}
	for uid, c := range change.Claims {
		s.cleanups[uid] = c
	}
	return saved, nil
}

func (s *svcStore) ServiceGrantDoc(ctx context.Context) (store.ServiceGrantDoc, error) {
	s.gmu.Lock()
	derr := s.docErr
	s.gmu.Unlock()
	if derr != nil {
		if err := derr(); err != nil {
			return store.ServiceGrantDoc{}, err
		}
	}
	sc, _ := s.azA3Store.GetSiteConfig(ctx) // the grant keys read separately from the roster: getErr does not fail them
	doc := store.ServiceGrantDoc{Grants: map[string]types.ProviderServiceGrant{}, Cleanups: map[string]store.ServiceGrantCleanup{}}
	if sc.ModelProviders != nil {
		for _, p := range sc.ModelProviders.Providers {
			if p.OIDC != nil && p.OIDC.ServiceGrant != nil {
				doc.Grants[p.UID] = *p.OIDC.ServiceGrant
			}
		}
	}
	s.gmu.Lock()
	defer s.gmu.Unlock()
	doc.Activated = s.activated
	for uid, c := range s.cleanups {
		doc.Cleanups[uid] = c
	}
	return doc, nil
}

func (s *svcStore) SettleServiceGrantCleanup(_ context.Context, uid, claimID string) (bool, error) {
	s.gmu.Lock()
	defer s.gmu.Unlock()
	if c, ok := s.cleanups[uid]; !ok || c.ID != claimID {
		return false, nil
	}
	delete(s.cleanups, uid)
	s.settles++
	return true, nil
}

func (s *svcStore) UpsertServiceGrantStatus(_ context.Context, row store.ServiceGrantStatusRow) error {
	if s.upsertGate != nil {
		s.upsertGate(row)
	}
	s.gmu.Lock()
	defer s.gmu.Unlock()
	if s.status == nil {
		s.status = map[string]store.ServiceGrantStatusRow{}
	}
	key := row.ProviderUID + "|" + row.ReplicaID
	if old, ok := s.status[key]; ok {
		if row.AttemptedAt.Before(old.AttemptedAt) {
			return nil
		}
		if row.LastOKAt == nil {
			row.LastOKAt, row.ValidUntil = old.LastOKAt, old.ValidUntil
		}
	}
	s.status[key] = row
	return nil
}

func (s *svcStore) TouchServiceGrantReplica(_ context.Context, replica string) error {
	s.gmu.Lock()
	defer s.gmu.Unlock()
	if s.seen == nil {
		s.seen = map[string]time.Time{}
	}
	s.seen[replica] = s.now()
	return nil
}

func (s *svcStore) ServiceGrantStatuses(_ context.Context, uids []string) ([]store.ServiceGrantStatusRow, error) {
	s.gmu.Lock()
	defer s.gmu.Unlock()
	var out []store.ServiceGrantStatusRow
	for _, row := range s.status {
		for _, uid := range uids {
			if row.ProviderUID == uid {
				row.Live = s.now().Sub(s.seen[row.ReplicaID]) < store.ServiceGrantLiveWindow && !s.seen[row.ReplicaID].IsZero()
				out = append(out, row)
			}
		}
	}
	return out, nil
}

func (s *svcStore) SweepServiceGrantStatuses(context.Context) (int64, error) { return 0, nil }

func (s *svcStore) statusOf(uid, replica string) (store.ServiceGrantStatusRow, bool) {
	s.gmu.Lock()
	defer s.gmu.Unlock()
	row, ok := s.status[uid+"|"+replica]
	return row, ok
}

// svcFix is one wired server: the run fixture's custom endpoint row carries a service grant listing svcOwner.
type svcFix struct {
	*oidcRunFixture
	t       *testing.T
	st      *svcStore
	sec     *svcSecrets
	idp     *svcIdP
	clock   *svcClock
	uid     string
	mux     http.Handler
	records int
}

func newSvcFix(t *testing.T) *svcFix {
	t.Helper()
	rf := newOIDCRunFixture(t)
	clock := &svcClock{t: adoTestNow}
	sec := newSvcSecrets()
	sec.m[oidcSecretRef] = []byte(oidcClientSecret)
	idp := &svcIdP{}
	st := &svcStore{azA3Store: rf.st, activated: true, now: clock.Now}
	f := &svcFix{oidcRunFixture: rf, t: t, st: st, sec: sec, idp: idp, clock: clock, uid: oidcUID}
	cfg := &rf.srv.cfg
	cfg.Store, cfg.Secrets, cfg.Now, cfg.ServiceGrantTransport = st, sec, clock.Now, idp
	f.setRow(func(p *types.ModelProvider) {
		o := *p.OIDC
		o.ServiceGrant = &types.ProviderServiceGrant{
			GrantType: types.ServiceGrantTypeClientCreds, TokenURL: svcTokenURL, ClientID: svcClientID, Scope: "api.read",
			ServiceIdentities: []string{svcOwner},
		}
		p.OIDC = &o
	})
	rf.st.run.CreatedBy = svcOwner
	r := chi.NewRouter()
	rf.srv.mountServiceGrantRoutes(r)
	f.mux = r
	return f
}

// setRow edits the one provider row in the site document.
func (f *svcFix) setRow(edit func(*types.ModelProvider)) {
	f.site.mu.Lock()
	defer f.site.mu.Unlock()
	block := &types.ModelProviders{Providers: append([]types.ModelProvider(nil), f.site.sc.ModelProviders.Providers...)}
	edit(&block.Providers[0])
	f.site.sc.ModelProviders = block
}

func (f *svcFix) provider() types.ModelProvider { return f.liveRow(f.t) }

func (f *svcFix) grant() *types.ProviderServiceGrant { return f.provider().OIDC.ServiceGrant }

func (f *svcFix) secretName() string { return serviceGrantSecretName(f.uid) }

// writeSecret stores the grant's record the way the route does, for the row's present destination.
func (f *svcFix) writeSecret(secret string) {
	f.t.Helper()
	p := f.provider()
	raw, _ := json.Marshal(serviceGrantRecord{V: 1, TokenURL: p.OIDC.ServiceGrant.TokenURL, ClientID: p.OIDC.ServiceGrant.ClientID,
		BaseURL: p.BaseURL, ClientSecret: secret})
	f.sec.set(f.secretName(), raw)
}

// withSecret is a fixture with the record written.
func newSvcFixWithSecret(t *testing.T) *svcFix {
	t.Helper()
	f := newSvcFix(t)
	f.writeSecret(svcClientSecret)
	return f
}

// token asks for a token as the refresher does (no snapshot, trigger refresh).
func (f *svcFix) token(trigger string) (serviceGrantMinted, error) {
	return f.srv.serviceGrantToken(context.Background(), f.provider(), "", trigger)
}

func (f *svcFix) mustToken(trigger string) serviceGrantMinted {
	f.t.Helper()
	tok, err := f.token(trigger)
	if err != nil {
		f.t.Fatalf("token(%s): %v", trigger, err)
	}
	return tok
}

// mintRows are the mint audit rows, in order.
func (f *svcFix) mintRows() []types.AuditEvent { return f.audit.find(auditServiceGrantMintAction) }

// body decodes a JSON body into a map.
func svcBody(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatalf("body %q: %v", w.Body.String(), err)
	}
	return m
}

func (f *svcFix) serve(method, path string, body any) *httptest.ResponseRecorder {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = strings.NewReader(string(b))
	}
	r := httptest.NewRequest(method, path, rd)
	w := httptest.NewRecorder()
	f.mux.ServeHTTP(w, r)
	return w
}

// putSecretBody is the secret route's body for the row's present destination.
func (f *svcFix) putSecretBody(value string) putServiceGrantSecretRequest {
	p := f.provider()
	return putServiceGrantSecretRequest{Value: value, TokenURL: p.OIDC.ServiceGrant.TokenURL, ClientID: p.OIDC.ServiceGrant.ClientID, BaseURL: p.BaseURL}
}

func (f *svcFix) secretPath() string {
	return "/model-providers/" + f.provider().ID + "/service-grant/secret"
}

// auditJSON is every audit row as text, for a leak sweep.
func (f *svcFix) auditAll() string { return f.auditText() }

// svcBasic decodes a Basic Authorization header into its two parts, undoing the form encoding.
func svcBasic(t *testing.T, h string) (id, secret string) {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(h, "Basic "))
	if err != nil {
		t.Fatalf("basic header %q: %v", h, err)
	}
	a, b, _ := strings.Cut(string(raw), ":")
	id, _ = url.QueryUnescape(a)
	secret, _ = url.QueryUnescape(b)
	return id, secret
}

var errSvcDown = errors.New("svc: store down")

// svcAwait waits for ch to close and fails the test after ten seconds: no wait in these tests is unbounded.
// It runs on the test's goroutine; a double's hook uses svcWaited and reports with t.Errorf.
func svcAwait(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	if !svcWaited(ch) {
		t.Fatalf("timed out waiting for %s", what)
	}
}

func svcWaited(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	case <-time.After(10 * time.Second):
		return false
	}
}
