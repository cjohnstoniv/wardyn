// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/broker"
	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Run create renews an expired AWS sign-in (#1546): the renewed pair is stored
// before any run row exists, because the proxy's bootstrap re-reads the STORED
// pair; a launch whose session cannot be renewed or kept is refused with no row.

const (
	createRenewalOwner = "sub-admit-admin" // providerAdminToken's principal
	createRenewalBody  = `{"agent":"claude-code","task":"t","model_provider":"bedrock-sso"}`
)

func createRenewalBlob() awsSSOBlob {
	now := time.Now()
	return awsSSOBlob{
		AccessToken: "old-access-token-1234567890", RefreshToken: "old-refresh-token-1234567890",
		ClientID: "sso-client-id", ClientSecret: "sso-client-secret-1234567890",
		StartURL: "https://example.awsapps.com/start", Region: "us-east-1",
		AccountID: "123456789012", RoleName: "WardynBedrockRole",
		ExpiresAt: now.Add(-time.Minute), CapturedAt: now.Add(-time.Hour), RegistrationExpiresAt: now.Add(90 * 24 * time.Hour),
	}
}

// createRenewalFixture is a create server whose owner holds an expired,
// renewable session for the one Bedrock SSO provider.
func createRenewalFixture(t *testing.T) *Server {
	t.Helper()
	site := types.SiteConfig{ModelProviders: providerBlock(awsSSOTestProvider())}
	srv := providerRunFixture(t, site, &capStore{}, nil)
	srv.cfg.Secrets = &memSecrets{m: map[string][]byte{}, owned: map[string]map[string][]byte{}}
	storeSSOBlobFor(t, srv, createRenewalOwner, createRenewalBlob())
	return srv
}

func createRenewalScope() awsSSOScope {
	return chosenProvider{provider: awsSSOTestProvider(), owner: createRenewalOwner}.awsScope()
}

func createRenewalStored(t *testing.T, srv *Server) (awsSSOBlob, bool) {
	t.Helper()
	b, found, err := srv.readAWSSSOBlob(context.Background(), createRenewalScope())
	if err != nil {
		t.Fatal(err)
	}
	return b, found
}

func runRowCount(srv *Server) int {
	st := srv.cfg.Store.(*integStore)
	st.mu.Lock()
	defer st.mu.Unlock()
	return len(st.runs)
}

func renewedOIDC(t *testing.T) *atomic.Int32 {
	return fakeOIDC(t, func(w http.ResponseWriter, _ map[string]string, _ int) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"accessToken": "fresh-access-token-abcdefghij", "expiresIn": 3600,
			"refreshToken": "rotated-refresh-token-abcdefghij",
		})
	})
}

// putScript is a store whose Put on the AWS SSO pair (or, with only unset, on
// any name) answers through hook. n counts the pair's Put attempts.
type putScript struct {
	secretstore.Store
	only string
	n    *atomic.Int32
	hook func(attempt int) error
}

func (p putScript) Put(ctx context.Context, name string, v []byte) error {
	if p.only == "" || name == p.only {
		if err := p.hook(int(p.n.Add(1))); err != nil {
			return err
		}
	}
	return p.Store.Put(ctx, name, v)
}

func (p putScript) For(owner string) secretstore.Store {
	p.Store = p.Store.For(owner)
	return p
}

func failEvery(error2 string) func(int) error {
	return func(int) error { return errors.New(error2) }
}

func postCreate(t *testing.T, srv *Server) *httpResult {
	t.Helper()
	w := do(t, srv, http.MethodPost, "/api/v1/runs", providerAdminToken(srv, createRenewalOwner), createRenewalBody)
	return &httpResult{code: w.Code, body: w.Body.String()}
}

type httpResult struct {
	code int
	body string
}

// (a) expired access, live refresh: create renews, stores the pair, and only
// then makes the run. Review renews nothing.
func TestCreateRenewal_ExpiredAccessLiveRefreshLaunches(t *testing.T) {
	srv := createRenewalFixture(t)
	calls := renewedOIDC(t)

	w := do(t, srv, http.MethodPost, "/api/v1/runs/preflight", providerAdminToken(srv, createRenewalOwner), createRenewalBody)
	if w.Code >= 500 || calls.Load() != 0 {
		t.Fatalf("preflight = %d with %d CreateToken calls, want a dry check that spends nothing", w.Code, calls.Load())
	}
	if got, _ := createRenewalStored(t, srv); got.RefreshToken != "old-refresh-token-1234567890" {
		t.Fatalf("preflight changed the stored pair: %+v", got)
	}

	if res := postCreate(t, srv); res.code != http.StatusCreated {
		t.Fatalf("create = %d %s, want 201", res.code, res.body)
	}
	if calls.Load() != 1 {
		t.Errorf("CreateToken calls = %d, want 1", calls.Load())
	}
	got, found := createRenewalStored(t, srv)
	if !found || got.AccessToken != "fresh-access-token-abcdefghij" || got.RefreshToken != "rotated-refresh-token-abcdefghij" {
		t.Errorf("stored pair = %+v, want the renewed one: bootstrap reads the store", got)
	}
	if n := runRowCount(srv); n != 1 {
		t.Errorf("run rows = %d, want 1", n)
	}
	if names := namesOf(t, srv.cfg.Secrets, createRenewalOwner); len(names) != 1 {
		t.Errorf("the owner namespace holds %v, want only the sign-in (the probe row is deleted)", names)
	}
}

// (b) a dead refresh token: the 422 sign-in door, and no run row.
func TestCreateRenewal_DeadRefreshIsTheDoorWithNoRunRow(t *testing.T) {
	srv := createRenewalFixture(t)
	fakeOIDC(t, func(w http.ResponseWriter, _ map[string]string, _ int) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "invalid_grant"})
	})
	res := postCreate(t, srv)
	var body errorBody
	_ = json.Unmarshal([]byte(res.body), &body)
	if res.code != http.StatusUnprocessableEntity || body.Reason != llmRefusalAuditReason || body.Provider != "bedrock-sso" {
		t.Fatalf("create = %d %s, want the 422 model_credential door", res.code, res.body)
	}
	if n := runRowCount(srv); n != 0 {
		t.Errorf("run rows = %d, want none", n)
	}
}

// A store that refuses a write is found by the probe before any refresh token
// is spent: 503, the sign-in still good, no row.
func TestCreateRenewal_ProbeRefusedSpendsNothing(t *testing.T) {
	srv := createRenewalFixture(t)
	calls := renewedOIDC(t)
	srv.cfg.Secrets = putScript{Store: srv.cfg.Secrets, n: new(atomic.Int32), hook: failEvery("secret store is wedged")}

	res := postCreate(t, srv)
	if res.code != http.StatusServiceUnavailable || !strings.Contains(res.body, mpBRStoreUnwritable) {
		t.Fatalf("create = %d %s, want the 503 store-unwritable sentence", res.code, res.body)
	}
	if calls.Load() != 0 {
		t.Errorf("CreateToken calls = %d, want none: nothing may be redeemed against a store that cannot keep it", calls.Load())
	}
	if srv.awsSSOTokenSpent(awsSSOTokenFingerprint("old-refresh-token-1234567890")) {
		t.Error("the refresh token was marked spent although it was never redeemed")
	}
	if n := runRowCount(srv); n != 0 {
		t.Errorf("run rows = %d, want none", n)
	}
}

// (c) a Put that fails after the redeem: 503 "sign in again", no row, the old
// pair spent so the next launch gets the door at once, and the next sign-in
// launches.
func TestCreateRenewal_PersistFailureAfterRedeemRefusesThenSignInLaunches(t *testing.T) {
	srv := createRenewalFixture(t)
	calls := renewedOIDC(t)
	good := srv.cfg.Secrets
	attempts := new(atomic.Int32)
	srv.cfg.Secrets = putScript{Store: good, only: providerSecretName(awsSSOTestProviderUID, providerSSOPart),
		n: attempts, hook: failEvery("secret store is wedged")}

	res := postCreate(t, srv)
	if res.code != http.StatusServiceUnavailable || !strings.Contains(res.body, mpBRPersistFailed) {
		t.Fatalf("create = %d %s, want the 503 sign-in-again sentence", res.code, res.body)
	}
	if calls.Load() != 1 || int(attempts.Load()) != awsSSOPersistAttempts {
		t.Errorf("CreateToken calls = %d, Put attempts = %d, want 1 redeem and %d tries", calls.Load(), attempts.Load(), awsSSOPersistAttempts)
	}
	if n := runRowCount(srv); n != 0 {
		t.Errorf("run rows = %d, want none", n)
	}

	srv.cfg.Secrets = good
	res = postCreate(t, srv)
	var body errorBody
	_ = json.Unmarshal([]byte(res.body), &body)
	if res.code != http.StatusUnprocessableEntity || body.Reason != llmRefusalAuditReason || calls.Load() != 1 {
		t.Fatalf("next create = %d %s (CreateToken calls %d), want the 422 door without another redeem", res.code, res.body, calls.Load())
	}

	live := createRenewalBlob()
	live.ExpiresAt = time.Now().Add(2 * time.Hour)
	storeSSOBlobFor(t, srv, createRenewalOwner, live)
	if res = postCreate(t, srv); res.code != http.StatusCreated {
		t.Fatalf("create after a new sign-in = %d %s, want 201", res.code, res.body)
	}
}

// A transient Put failure is retried inside the renewal, under the lock.
func TestCreateRenewal_PutRetriedInsideTheRenewal(t *testing.T) {
	srv := createRenewalFixture(t)
	renewedOIDC(t)
	attempts := new(atomic.Int32)
	srv.cfg.Secrets = putScript{Store: srv.cfg.Secrets, only: providerSecretName(awsSSOTestProviderUID, providerSSOPart), n: attempts,
		hook: func(n int) error {
			if n == 1 {
				return errors.New("transient")
			}
			return nil
		}}
	if res := postCreate(t, srv); res.code != http.StatusCreated {
		t.Fatalf("create = %d %s, want 201 on the second Put", res.code, res.body)
	}
	if got, _ := createRenewalStored(t, srv); got.RefreshToken != "rotated-refresh-token-abcdefghij" {
		t.Errorf("stored pair = %+v, want the renewed one", got)
	}
}

// The proxy's bootstrap is a second door on the same store: a second server
// instance, with no memory of the create, serves the renewed pair, and
// redeems nothing itself.
func TestCreateRenewal_BootstrapOnASecondInstanceGetsTheRenewedPair(t *testing.T) {
	f := newReauthFixture(t, nil)
	f.srv.cfg.MaskRegistry = secretmask.NewRegistry()
	expired := createRenewalBlob()
	expired.StartURL, expired.Region = "https://acme.awsapps.com/start", reauthRegion
	expired.AccountID, expired.RoleName = "111122223333", "WardynAgent"
	f.putBlob(t, "alice@example.com", expired)
	calls := renewedOIDC(t)

	_, d, err := f.srv.providerLiveness(withCreateRenewal(context.Background()), reauthProvider(), "claude-code", "alice@example.com", true)
	if err != nil || d.msg != "" {
		t.Fatalf("create-time liveness = %+v, %v", d, err)
	}
	second := New(f.srv.cfg)
	w := do(t, second, http.MethodGet, "/api/v1/internal/injection/"+f.grantID.String(), f.token, "")
	var resp types.ResolvedInjection
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil || w.Code != http.StatusOK || resp.Value != "fresh-access-token-abcdefghij" {
		t.Fatalf("second instance bootstrap = %d %s, want 200 with the renewed token", w.Code, w.Body.String())
	}
	if calls.Load() != 1 {
		t.Errorf("CreateToken calls = %d, want 1 (create's): bootstrap redeems nothing", calls.Load())
	}
}

// An erase that starts between the first and second Put attempt queues behind
// the owner lock and wins: every attempt holds the lock, and bootstrap then
// finds no pair and refuses.
func TestCreateRenewal_EraseDuringPutRetryWinsAndBootstrapFailsClosed(t *testing.T) {
	f := newReauthFixture(t, nil)
	f.srv.cfg.MaskRegistry = secretmask.NewRegistry()
	expired := createRenewalBlob()
	expired.StartURL, expired.Region = "https://acme.awsapps.com/start", reauthRegion
	expired.AccountID, expired.RoleName = "111122223333", "WardynAgent"
	f.putBlob(t, "alice@example.com", expired)
	renewedOIDC(t)

	erased := make(chan struct{})
	var heldAtEveryPut atomic.Bool
	heldAtEveryPut.Store(true)
	f.srv.cfg.Secrets = putScript{Store: f.secrets, only: providerSecretName(reauthProviderUID, providerSSOPart), n: new(atomic.Int32),
		hook: func(n int) error {
			if _, release, ok := f.srv.tryLockAWSSSOOwner(context.Background(), "alice@example.com"); ok {
				release()
				heldAtEveryPut.Store(false)
			}
			if n == 1 {
				go func() {
					defer close(erased)
					var rep secretstore.EraseReport
					_ = f.srv.eraseLocked(context.Background(), "alice@example.com", "", &rep)
				}()
				time.Sleep(50 * time.Millisecond) // let the erase reach the lock
				return errors.New("transient")
			}
			select {
			case <-erased:
				t.Error("the erase ran between two Put attempts")
			default:
			}
			return nil
		}}

	if _, d, err := f.srv.providerLiveness(withCreateRenewal(context.Background()), reauthProvider(), "claude-code", "alice@example.com", true); err != nil || d.msg != "" {
		t.Fatalf("create-time liveness = %+v, %v", d, err)
	}
	<-erased
	if !heldAtEveryPut.Load() {
		t.Error("a Put of the AWS SSO pair ran without the owner lock")
	}
	w := do(t, f.srv, http.MethodGet, "/api/v1/internal/injection/"+f.grantID.String(), f.token, "")
	if w.Code == http.StatusOK || strings.Contains(w.Body.String(), "fresh-access-token") {
		t.Fatalf("bootstrap after the erase = %d %s, want a refusal with no token", w.Code, w.Body.String())
	}
}

// Every call of storeAWSSSOBlob is made with the owner lock held: its only
// callers are the renewal's persist step and the provider sign-in store, and
// each is reached only after the lock is taken in its own function.
func TestStoreAWSSSOBlobCallersHoldTheOwnerLock(t *testing.T) {
	fset := token.NewFileSet()
	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	type call struct {
		fn, callee string
		pos        token.Pos
	}
	var calls []call
	locks := map[string][]token.Pos{} // function -> positions of its lock calls
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range file.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				c, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := c.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				switch name := sel.Sel.Name; name {
				case "lockAWSSSOOwner", "tryLockAWSSSOOwner", "lockAWSSSORenewal":
					locks[fn.Name.Name] = append(locks[fn.Name.Name], c.Pos())
				case "storeAWSSSOBlob", "persistRenewedAWSSSOBlob", "storeProviderSignIn":
					calls = append(calls, call{fn.Name.Name, name, c.Pos()})
				}
				return true
			})
		}
	}
	for callee, want := range map[string][]string{
		"storeAWSSSOBlob":          {"persistRenewedAWSSSOBlob", "storeProviderSignIn"},
		"persistRenewedAWSSSOBlob": {"refreshAWSSSOBlob"},
		"storeProviderSignIn":      {"handleUploadSSOToken"},
	} {
		var got []string
		for _, c := range calls {
			if c.callee == callee {
				got = append(got, c.fn)
			}
		}
		if !sameSet(got, want) {
			t.Errorf("%s is called from %v, want %v: a new caller must hold lockAWSSSOOwner and be added here", callee, got, want)
		}
	}
	// The callers of the two entry points take the lock before they call.
	for _, c := range calls {
		if c.callee == "storeAWSSSOBlob" {
			continue
		}
		locked := false
		for _, p := range locks[c.fn] {
			locked = locked || p < c.pos
		}
		if !locked {
			t.Errorf("%s calls %s before taking the owner lock", c.fn, c.callee)
		}
	}
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := map[string]bool{}
	for _, x := range a {
		seen[x] = true
	}
	for _, x := range b {
		if !seen[x] {
			return false
		}
	}
	return true
}

// The probe's row is reserved at every door: GET /secrets never lists it, no
// grant resolves it, and a crash between its Put and Delete leaves it inert.
func TestWriteProbeIsReservedEverywhere(t *testing.T) {
	if !secretsAPIReserved(writeProbeSecretName) || !sinkReservedSecret(writeProbeSecretName) || !broker.ReservedSecretName(writeProbeSecretName) {
		t.Fatal("the write probe name is missing from a reservation map")
	}
	h, sec := newSecretsHarness(t)
	sec.m[writeProbeSecretName] = []byte("left-behind-by-a-crash")
	if names, err := reservedFilteredSecretNames(context.Background(), sec); err != nil || slices.Contains(names, writeProbeSecretName) {
		t.Errorf("GET /secrets lists %v (err %v), want no probe row", names, err)
	}
	h.broker.minted = broker.Minted{Kind: types.GrantAPIKey, JTI: "j-probe", Injection: &egress.InjectionRule{
		Host: "api.anthropic.com", Header: "Authorization", Format: "Bearer %s", SecretName: writeProbeSecretName}}
	rr := do(t, h.srv, http.MethodGet, "/api/v1/internal/injection/"+uuid.NewString(), h.mintRunToken(t, uuid.New()), "")
	if rr.Code != http.StatusForbidden || !strings.Contains(rr.Body.String(), reasonInjectionReservedSecretName) ||
		strings.Contains(rr.Body.String(), "left-behind") {
		t.Fatalf("sink = %d %s, want 403 %s and no value", rr.Code, rr.Body.String(), reasonInjectionReservedSecretName)
	}
}
