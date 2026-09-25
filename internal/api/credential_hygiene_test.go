// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/broker"
	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Credential memory, revocation and failure hygiene (CS-4, credential-storage
// design K8 and §2.8).

// failingSecrets is memSecrets whose every Get fails with err; List still
// answers, so a per-user read gets as far as the value.
type failingSecrets struct {
	*memSecrets
	err error
}

func (f failingSecrets) Get(context.Context, string) ([]byte, error) { return nil, f.err }
func (f failingSecrets) For(owner string) secretstore.Store {
	return failingSecrets{f.memSecrets.For(owner).(*memSecrets), f.err}
}

var (
	errStoreDown    = fmt.Errorf("vault GET wardyn/data/x: 503 sealed: %w", secretstore.ErrUnavailable)
	errStoreRefused = errors.New("vault GET wardyn/data/x: 403 permission denied")
)

func storedKeyGrant(approval uuid.UUID) broker.Minted {
	return broker.Minted{
		Kind: types.GrantAPIKey, JTI: "j-ttl", ApprovalID: approval,
		Injection: &egress.InjectionRule{Host: "api.anthropic.com", Header: "x-api-key", SecretName: "anthropic-api-key"},
	}
}

func decodeResolved(t *testing.T, body []byte) types.ResolvedInjection {
	t.Helper()
	var ri types.ResolvedInjection
	if err := json.Unmarshal(body, &ri); err != nil {
		t.Fatalf("decode resolve: %v (%s)", err, body)
	}
	return ri
}

// A stored key used to be injected for the run's whole life: removing it, or
// revoking Wardyn's access to it at the store, changed nothing for a run
// already using it. It now carries a ten-minute expiry, so the proxy asks
// again. An approval-gated grant stays static: its mint is single-use.
func TestInternalInjection_StoredKeyExpiresInTenMinutes(t *testing.T) {
	h, _ := newSecretsHarness(t)
	token := h.mintRunToken(t, uuid.New())

	h.broker.minted = storedKeyGrant(uuid.Nil)
	before := time.Now()
	rr := do(t, h.srv, http.MethodGet, "/api/v1/internal/injection/"+uuid.NewString(), token, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("resolve: %d %s", rr.Code, rr.Body.String())
	}
	got := time.UnixMilli(decodeResolved(t, rr.Body.Bytes()).ExpiresAt)
	if got.Before(before.Add(storedKeyTTL-time.Second)) || got.After(time.Now().Add(storedKeyTTL)) {
		t.Fatalf("expires_at = %v, want %v from now: a stored key with no expiry is held for the run's life", got, storedKeyTTL)
	}

	h.broker.minted = storedKeyGrant(uuid.New())
	rr = do(t, h.srv, http.MethodGet, "/api/v1/internal/injection/"+uuid.NewString(), token, "")
	if ri := decodeResolved(t, rr.Body.Bytes()); rr.Code != http.StatusOK || ri.ExpiresAt != 0 {
		t.Fatalf("approval-gated resolve: %d expires_at=%d, want 200 and 0 (a re-resolve would spend the approval)", rr.Code, ri.ExpiresAt)
	}
}

// A store that answered and refused is definitive and says so: not the 503 the
// proxy rides out, and not "not in the store", which would send the person to
// set a key that is already set.
func TestInternalInjection_RefusedStoredKeyIsDefinitiveAndSaysSo(t *testing.T) {
	h, sec := newSecretsHarness(t)
	h.srv.cfg.Secrets = failingSecrets{sec, errStoreRefused}
	h.srv.router = h.srv.routes()
	h.broker.minted = storedKeyGrant(uuid.Nil)
	rr := do(t, h.srv, http.MethodGet, "/api/v1/internal/injection/"+uuid.NewString(), h.mintRunToken(t, uuid.New()), "")
	if rr.Code != http.StatusFailedDependency || !strings.Contains(rr.Body.String(), "the store refused it") ||
		strings.Contains(rr.Body.String(), "not in the store") {
		t.Fatalf("refused: status = %d body=%s, want a definitive 424 naming the refusal", rr.Code, rr.Body.String())
	}
	if ev := lastAuditEvent(t, h.audit.events, "secret.read"); !strings.Contains(string(ev.Data), `"reason":"refused"`) {
		t.Fatalf("refused: audit data = %s, want the refused reason", ev.Data)
	}
}

// The managed subscription token is a stored credential too: a store outage
// is the transient 503, and a resolve carries the stored-key expiry.
func TestInternalInjection_ManagedTokenIsAStoredCredential(t *testing.T) {
	h, sec := newSecretsHarness(t)
	h.srv.cfg.SubscriptionPostureOK = true
	blob, _ := json.Marshal(managedCredBlob{Token: "sk-ant-oat01-managed-token-value"})
	sec.m[harnessCredSecretName("anthropic")] = blob
	h.broker.minted = broker.Minted{Kind: types.GrantAPIKey, JTI: "j-m",
		Injection: &egress.InjectionRule{Host: "api.anthropic.com", Header: "Authorization", SecretName: types.ManagedOAuthSecret}}
	token := h.mintRunToken(t, uuid.New())

	h.srv.cfg.ManagedToken = NewManagedCredProvider(sec, "anthropic")
	rr := do(t, h.srv, http.MethodGet, "/api/v1/internal/injection/"+uuid.NewString(), token, "")
	if ri := decodeResolved(t, rr.Body.Bytes()); rr.Code != http.StatusOK || ri.ExpiresAt == 0 {
		t.Fatalf("managed resolve: %d expires_at=%d, want 200 with the stored-key expiry", rr.Code, ri.ExpiresAt)
	}

	h.srv.cfg.ManagedToken = NewManagedCredProvider(failingSecrets{sec, errStoreDown}, "anthropic")
	rr = do(t, h.srv, http.MethodGet, "/api/v1/internal/injection/"+uuid.NewString(), token, "")
	if rr.Code != http.StatusServiceUnavailable || !strings.Contains(rr.Body.String(), "couldn't reach the service") {
		t.Fatalf("managed store outage: %d %s, want the transient 503", rr.Code, rr.Body.String())
	}
}

// The Bedrock bearer arm used to read an unreadable key as an ABSENT one: a
// store blip became "not in the store", and a refusal did too.
func TestBedrockBearerSink_StoreOutageIsTransientRefusalDefinitive(t *testing.T) {
	for _, tc := range []struct {
		err        error
		wantStatus int
		wantReason string
	}{
		{errStoreDown, http.StatusServiceUnavailable, "store-unavailable"},
		{errStoreRefused, http.StatusFailedDependency, "refused"},
	} {
		st := &bearerGuardStore{run: types.AgentRun{ID: uuid.New(), Agent: "claude-code"},
			site: bearerRow(types.CredentialSourceShared)}
		h, sec := bearerGuardHarness(t, st)
		h.srv.cfg.Secrets = failingSecrets{sec, tc.err}
		rr := resolveRecordedBearerGrant(t, h, st, "", "shared")
		assertBedrockBearerRefused(t, rr, h, tc.wantStatus, "", tc.wantReason)
	}

	st := &bearerGuardStore{run: types.AgentRun{ID: uuid.New(), Agent: "claude-code"}, site: bearerRow(types.CredentialSourceShared)}
	h, _ := bearerGuardHarness(t, st)
	if ri := decodeResolved(t, resolveRecordedBearerGrant(t, h, st, "", "shared").Body.Bytes()); ri.ExpiresAt == 0 {
		t.Error("the Bedrock bearer key resolved with no expiry; a stored key must be re-read")
	}
}

// The captured AWS SSO arm answered every store error with the 503 the proxy
// now rides out: an access-denied store would have kept a revoked session
// injected for the whole grace.
func TestResolveAWSSSOInjection_StoreOutageIsTransientRefusalDefinitive(t *testing.T) {
	for _, tc := range []struct {
		err        error
		wantStatus int
		wantReason string
	}{
		{errStoreDown, http.StatusServiceUnavailable, "store-unavailable"},
		{errStoreRefused, http.StatusForbidden, "store_error"},
	} {
		f := newReauthFixture(t, nil)
		f.putBlob(t, "alice@example.com", liveSSOBlob())
		f.srv.cfg.Secrets = failingSecrets{f.secrets, tc.err}
		if w := f.resolve(t); w.Code != tc.wantStatus {
			t.Errorf("%v: status = %d, want %d; body=%s", tc.err, w.Code, tc.wantStatus, w.Body.String())
		}
		if !f.audit.hasReason("secret.read", tc.wantReason) {
			t.Errorf("%v: no secret.read failure with reason %q", tc.err, tc.wantReason)
		}
	}
}

// The Azure DevOps lane classified every store error as unavailable, the
// class the proxy rides out: a store refusal is its own, definitive class.
func TestADORedeem_StoreRefusalIsDefinitive(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want ADOEntraFailure
	}{
		{errStoreDown, ADOEntraFailureUnavailable},
		{errStoreRefused, ADOEntraFailureStoreRefused},
	} {
		f := newADOFixture(t)
		subject := f.fake.Subject()
		if w := f.capture(t, subject); w.Code != http.StatusFound {
			t.Fatalf("capture: status %d body %q", w.Code, w.Body.String())
		}
		f.srv.cfg.Secrets = failingSecrets{f.srv.cfg.Secrets.(*memSecrets), tc.err}
		_, err := f.srv.RedeemADOEntraAccess(context.Background(), f.cfg, subject, f.cfg.Scopes)
		if got := ADOEntraClassify(err); got != tc.want {
			t.Errorf("%v: class = %q, want %q", tc.err, got, tc.want)
		}
	}
	if status, _ := adoResolveFailureAnswer(ADOEntraFailureStoreRefused); status == http.StatusServiceUnavailable {
		t.Error("a store refusal is answered 503, which the proxy rides out as transient")
	}
}

// Every re-resolve mints first, and the mint reads the control plane's own
// database. A database that did not answer fell to the mint's generic 500,
// which the proxy reads as a refusal and drops the header at once — so the
// last-good grace the docs promise for "the store (or the database) not
// answering" never engaged for the database. It is the transient 503 now; a
// database that answered with an error stays a 500.
func TestInternalInjection_MintDatabaseOutageIsTransient(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// A real driver error for a database that is not there: nothing listens on port 1.
	_, down := pgx.Connect(ctx, "postgres://wardyn@127.0.0.1:1/wardyn?sslmode=disable&connect_timeout=2")
	if down == nil {
		t.Fatal("connected to a database on port 1")
	}
	h, _ := newSecretsHarness(t)
	token := h.mintRunToken(t, uuid.New())
	for _, tc := range []struct {
		err  error
		want int
	}{
		{fmt.Errorf("broker: begin tx: %w", down), http.StatusServiceUnavailable},
		{fmt.Errorf("broker: load grant: %w", &pgconn.PgError{Code: "42P01", Message: "relation does not exist"}), http.StatusInternalServerError},
	} {
		h.broker.mintErr = tc.err
		rr := do(t, h.srv, http.MethodGet, "/api/v1/internal/injection/"+uuid.NewString(), token, "")
		if rr.Code != tc.want {
			t.Errorf("%v: status = %d body=%s, want %d", tc.err, rr.Code, rr.Body.String(), tc.want)
		}
	}
}

// Entra may answer a redemption without a refresh token; the one Wardyn holds
// stays in use, so its mask copy must stay current rather than be retired and
// swept while live (F5).
func TestADORedeem_AnAbsentRefreshTokenStaysMasked(t *testing.T) {
	f := newADOFixture(t)
	subject := f.fake.Subject()
	if w := f.capture(t, subject); w.Code != http.StatusFound {
		t.Fatalf("capture: status %d body %q", w.Code, w.Body.String())
	}
	held, _ := f.stored(t, subject)

	// A proxy in front of the tenant that strips refresh_token from a redemption's answer.
	upstream := f.cfg.AuthorityOverride
	spy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		req, err := http.NewRequest(r.Method, upstream+r.URL.Path, bytes.NewReader(body))
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		req.Header = r.Header.Clone()
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		defer func() { _ = resp.Body.Close() }()
		var out map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		delete(out, "refresh_token")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		_ = json.NewEncoder(w).Encode(out)
	}))
	defer spy.Close()
	f.cfg.AuthorityOverride = spy.URL

	if _, err := f.srv.RedeemADOEntraAccess(context.Background(), f.cfg, subject, f.cfg.Scopes); err != nil {
		t.Fatalf("redeem: %v", err)
	}
	if after, _ := f.stored(t, subject); after.RefreshToken != held.RefreshToken {
		t.Fatal("an absent refresh token replaced the held one")
	}
	f.srv.cfg.MaskRegistry.SweepGlobals(time.Now().Add(time.Hour))
	if !slices.ContainsFunc(f.srv.cfg.MaskRegistry.Snapshot(uuid.Nil), func(v []byte) bool { return string(v) == held.RefreshToken }) {
		t.Fatal("the refresh token still in use was retired and swept")
	}
}

// lostWriteSecrets is memSecrets whose every Put fails, per owner too: a
// capture gets all the way to its store write and loses it there.
type lostWriteSecrets struct{ *memSecrets }

func (p lostWriteSecrets) Put(context.Context, string, []byte) error { return errStoreDown }
func (p lostWriteSecrets) For(owner string) secretstore.Store {
	return lostWriteSecrets{p.memSecrets.For(owner).(*memSecrets)}
}

// A re-capture that fails after the token exchange (an identity it will not
// bind, a lost store write) leaves the sign-in already stored as the live
// credential. Its refresh token used to be retired at the exchange and swept
// an hour later, unmasked while still in use.
func TestADOCapture_AFailedRecaptureKeepsTheHeldTokenMasked(t *testing.T) {
	for _, tc := range []struct {
		name    string
		breakIt func(*adoFixture)
		want    int
	}{
		{"identity refused", func(f *adoFixture) { f.fake.SetSubject("someone-else") }, http.StatusForbidden},
		{"store write lost", func(f *adoFixture) {
			f.srv.cfg.Secrets = lostWriteSecrets{f.srv.cfg.Secrets.(*memSecrets)}
		}, http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newADOFixture(t)
			subject := f.fake.Subject()
			if w := f.capture(t, subject); w.Code != http.StatusFound {
				t.Fatalf("capture: status %d body %q", w.Code, w.Body.String())
			}
			held, _ := f.stored(t, subject)
			tc.breakIt(f)
			if w := f.capture(t, subject); w.Code != tc.want {
				t.Fatalf("re-capture: status %d body %q, want %d", w.Code, w.Body.String(), tc.want)
			}
			f.srv.cfg.MaskRegistry.SweepGlobals(time.Now().Add(time.Hour))
			if !slices.ContainsFunc(f.srv.cfg.MaskRegistry.Snapshot(uuid.Nil), func(v []byte) bool { return string(v) == held.RefreshToken }) {
				t.Fatal("the stored refresh token still in use was retired and swept")
			}
		})
	}
}

// The login-time capture has the same shape: a lost store write keeps the
// credential already stored, so its mask copy stays current.
func TestCaptureLoginGrant_ALostStoreWriteKeepsTheHeldTokenMasked(t *testing.T) {
	f := newADOFixture(t)
	const subject, held = "a-person", "the-refresh-token-already-stored"
	granted := strings.Join(append([]string{"openid", entraOfflineAccessScope}, f.cfg.Scopes...), " ")
	f.srv.CaptureLoginGrant(context.Background(), subject, oidc.LoginGrant{RefreshToken: held, Scope: granted, Expiry: adoTestNow.Add(time.Hour)})
	f.srv.cfg.Secrets = lostWriteSecrets{f.srv.cfg.Secrets.(*memSecrets)}
	f.srv.CaptureLoginGrant(context.Background(), subject, oidc.LoginGrant{RefreshToken: "a-refresh-token-never-stored", Scope: granted, Expiry: adoTestNow.Add(time.Hour)})

	f.srv.cfg.MaskRegistry.SweepGlobals(time.Now().Add(time.Hour))
	snap := f.srv.cfg.MaskRegistry.Snapshot(uuid.Nil)
	for _, v := range []string{held, "a-refresh-token-never-stored"} {
		if !slices.ContainsFunc(snap, func(b []byte) bool { return string(b) == v }) {
			t.Errorf("%q is not masked after the failed capture", v)
		}
	}
}

// countingSecrets counts Gets and can be made to fail.
type countingSecrets struct {
	*memSecrets
	gets atomic.Int32
	fail atomic.Bool
}

func (c *countingSecrets) Get(ctx context.Context, name string) ([]byte, error) {
	c.gets.Add(1)
	if c.fail.Load() {
		return nil, errStoreDown
	}
	return c.memSecrets.Get(ctx, name)
}

// §2.3a.4: the managed token was read from the store on every resolve. It is
// cached for a minute now, and only a successful read is cached.
func TestManagedCredProvider_CachesAMinuteButNeverAFailure(t *testing.T) {
	blob, _ := json.Marshal(managedCredBlob{Token: "sk-ant-oat01-managed-token-value"})
	st := &countingSecrets{memSecrets: &memSecrets{m: map[string][]byte{harnessCredSecretName("anthropic"): blob}}}
	p := NewManagedCredProvider(st, "anthropic").(*managedCredProvider)

	st.fail.Store(true)
	if _, err := p.Current(context.Background()); err == nil {
		t.Fatal("a store outage resolved a token")
	}
	st.fail.Store(false)
	for range 3 {
		if tok, err := p.Current(context.Background()); err != nil || tok.Value != "sk-ant-oat01-managed-token-value" {
			t.Fatalf("Current = %q, %v", tok.Value, err)
		}
	}
	if got := st.gets.Load(); got != 2 {
		t.Fatalf("store reads = %d, want 2 (the failed read, then one fill): a failure must not be cached, a success must", got)
	}
	p.cachedAt = time.Now().Add(-managedTokenCacheTTL)
	_, _ = p.Current(context.Background())
	if got := st.gets.Load(); got != 3 {
		t.Fatalf("store reads = %d after the minute, want 3", got)
	}
}

// Replacing or disconnecting the managed token lets go of the old one at once:
// the cache serves the new token (or none), and its process-wide mask copy is
// retired and then swept, instead of held for the daemon's life (F5).
func TestHarnessCredential_ReplaceAndDisconnectLetGoOfTheOldToken(t *testing.T) {
	const first, second = "sk-ant-oat01-first-managed-token", "sk-ant-oat01-second-managed-token"
	reg := secretmask.NewRegistry()
	h := newHarness(t)
	cfg := h.srv.cfg
	sec := &memSecrets{m: map[string][]byte{}}
	cfg.Secrets, cfg.MaskRegistry = sec, reg
	cfg.ManagedToken = NewManagedCredProvider(sec, "anthropic")
	h.srv = New(cfg)
	paste := func(tok string) {
		t.Helper()
		if w := do(t, h.srv, http.MethodPut, "/api/v1/setup/harness-credential/anthropic", adminToken, `{"token":"`+tok+`"}`); w.Code != http.StatusOK {
			t.Fatalf("paste: %d %s", w.Code, w.Body.String())
		}
	}
	current := func() string {
		tok, _ := h.srv.cfg.ManagedToken.Current(context.Background())
		return tok.Value
	}
	held := func(v string) bool {
		return !bytes.Contains(reg.Masker(uuid.New()).Mask([]byte(v)), []byte(v))
	}

	paste(first)
	if got := current(); got != first {
		t.Fatalf("after the first capture Current = %q", got)
	}
	paste(second)
	if got := current(); got != second {
		t.Fatalf("after replacing the token Current = %q, want the new one at once (the cache must be evicted)", got)
	}
	reg.SweepGlobals(time.Now().Add(time.Second))
	if held(first) || !held(second) {
		t.Fatalf("after the replace and a sweep: first held=%v second held=%v, want only the current token", held(first), held(second))
	}

	if w := do(t, h.srv, http.MethodDelete, "/api/v1/setup/harness-credential/anthropic", adminToken, ""); w.Code != http.StatusOK {
		t.Fatalf("disconnect: %d %s", w.Code, w.Body.String())
	}
	if got := current(); got != "" {
		t.Fatalf("after disconnect Current = %q, want nothing (the cache must be evicted)", got)
	}
	if !held(second) {
		t.Fatal("the disconnected token stopped being masked at once; it must stay masked until swept")
	}
	reg.SweepGlobals(time.Now().Add(time.Second))
	if held(second) {
		t.Fatal("the disconnected token's mask copy is still held after the sweep")
	}
}

// The periodic sweep is what finally lets go of a retired mask copy, on the
// same grace a finished run's corpus gets.
func TestSweepRunSecrets_DropsRetiredGlobalMaskCopies(t *testing.T) {
	const old = "sk-ant-oat01-retired-managed-token"
	reg := secretmask.NewRegistry()
	s := &Server{cfg: Config{MaskRegistry: reg, Now: time.Now}}
	reg.AddGlobal("", "wardyn-harness-anthropic-oauth", time.Now(), []byte(old))
	reg.EvictGlobal("", "wardyn-harness-anthropic-oauth", time.Now())
	held := func() bool { return !bytes.Contains(reg.Masker(uuid.Nil).Mask([]byte(old)), []byte(old)) }

	s.SweepRunSecrets(context.Background())
	if !held() {
		t.Fatal("a value retired just now was swept before the grace")
	}
	s.cfg.Now = func() time.Time { return time.Now().Add(RunSecretGrace + time.Minute) }
	s.SweepRunSecrets(context.Background())
	if held() {
		t.Fatal("a value retired longer than RunSecretGrace ago is still held after the sweep")
	}
}
