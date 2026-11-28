// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/secretspresent"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/kek"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// secretFactsStore keeps one run's secret facts, its grants and its sealed proxy config in memory, with the
// store's write rules: a dispatch write ORs each boolean and replaces the jti, a revive write only updates a row
// the dispatch wrote.
type secretFactsStore struct {
	store.Store
	mu        sync.Mutex
	facts     map[uuid.UUID]store.SecretFacts
	grants    []types.CredentialGrant
	sealed    map[uuid.UUID][]byte
	readErr   error
	writeErr  error
	reviveErr error
	writes    int
	failed    bool
}

func newSecretFactsStore() *secretFactsStore {
	return &secretFactsStore{facts: map[uuid.UUID]store.SecretFacts{}, sealed: map[uuid.UUID][]byte{}}
}

func (s *secretFactsStore) ReadRunSecretFacts(_ context.Context, runID uuid.UUID, _ string, _ *time.Time) (store.SecretFacts, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.readErr != nil {
		return store.SecretFacts{}, s.readErr
	}
	f, ok := s.facts[runID]
	if !ok {
		return store.SecretFacts{}, store.ErrNotFound
	}
	if f.Now.IsZero() {
		f.Now = time.Now()
	}
	return f, nil
}

func (s *secretFactsStore) RecordRunDispatchSecretFacts(_ context.Context, runID uuid.UUID, d store.DispatchSecretFacts) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.writeErr != nil {
		return s.writeErr
	}
	s.writes++
	f := s.facts[runID]
	if f.Dispatch != nil {
		d.BedrockResident = d.BedrockResident || f.Dispatch.BedrockResident
		d.GitTokenResident = d.GitTokenResident || f.Dispatch.GitTokenResident
	}
	f.Dispatch = &d
	s.facts[runID] = f
	return nil
}

func (s *secretFactsStore) RecordRevivedSecretFacts(_ context.Context, runID uuid.UUID, git bool, jti string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.reviveErr != nil {
		return s.reviveErr
	}
	f, ok := s.facts[runID]
	if !ok || f.Dispatch == nil {
		return nil
	}
	d := *f.Dispatch
	d.GitTokenResident, d.RecordedJTI = d.GitTokenResident || git, jti
	f.Dispatch = &d
	s.facts[runID] = f
	return nil
}

func (s *secretFactsStore) ListGrantsByRun(context.Context, uuid.UUID) ([]types.CredentialGrant, error) {
	return s.grants, nil
}

func (s *secretFactsStore) PutRunProxyConfig(_ context.Context, id uuid.UUID, sealed []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sealed[id] = sealed
	return nil
}

func (s *secretFactsStore) GetRunProxyConfig(_ context.Context, id uuid.UUID) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if b, ok := s.sealed[id]; ok {
		return b, nil
	}
	return nil, store.ErrNotFound
}

func (s *secretFactsStore) DeleteRunProxyConfig(_ context.Context, id uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sealed, id)
	return nil
}

func (s *secretFactsStore) PurgeTerminalRunProxyConfigs(context.Context) (int64, error) {
	return 0, nil
}

func (s *secretFactsStore) UpdateRunStateIf(_ context.Context, _ uuid.UUID, _, to types.RunState) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failed = s.failed || to == types.RunFailed
	return true, nil
}

var (
	_ store.RunSecretFacts  = (*secretFactsStore)(nil)
	_ store.RunProxyConfigs = (*secretFactsStore)(nil)
)

// testRunToken signs a run token whose jti is jti and whose expiry passed an hour ago, as a stored one usually has.
func testRunToken(t *testing.T, jti string) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: key}, (&jose.SignerOptions{}).WithType("JWT"))
	if err != nil {
		t.Fatal(err)
	}
	tok, err := jwt.Signed(signer).Claims(jwt.Claims{ID: jti, Subject: "spiffe://wardyn.test/agent-run/x",
		Expiry: jwt.NewNumericDate(time.Now().Add(-time.Hour))}).Serialize()
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// secretsFixture is a server over a secretFactsStore with a run config key, and one run whose dispatch row and
// stored proxy config name the same token: a run with no secret, until a case adds one.
type secretsFixture struct {
	srv   *Server
	st    *secretFactsStore
	audit *memAudit
	run   types.AgentRun
}

func newSecretsFixture(t *testing.T) *secretsFixture {
	t.Helper()
	h := newHarness(t)
	st := newSecretFactsStore()
	aud := &memAudit{}
	cfg := baseTestConfig(h, st)
	cfg.Audit, cfg.RunConfigKey = aud, make([]byte, kek.DEKSize)
	srv := New(cfg)
	f := &secretsFixture{srv: srv, st: st, audit: aud,
		run: types.AgentRun{ID: uuid.New(), State: types.RunStarting, MachineKind: types.MachineLinuxVM, CreatedBy: "sub-owner"}}
	jti := uuid.NewString()
	st.facts[f.run.ID] = store.SecretFacts{GrantKinds: map[string]int{}, Dispatch: &store.DispatchSecretFacts{RecordedJTI: jti}}
	f.storeToken(t, jti)
	return f
}

// storeToken stores a proxy config whose run token has jti, as a dispatch or a revive stores it.
func (f *secretsFixture) storeToken(t *testing.T, jti string) {
	t.Helper()
	if err := f.srv.storeRunProxyConfig(context.Background(), f.run.ID, []byte(`{"run_token":"`+testRunToken(t, jti)+`"}`)); err != nil {
		t.Fatal(err)
	}
}

func (f *secretsFixture) edit(fn func(*store.SecretFacts)) {
	f.st.mu.Lock()
	defer f.st.mu.Unlock()
	facts := f.st.facts[f.run.ID]
	fn(&facts)
	f.st.facts[f.run.ID] = facts
}

func (f *secretsFixture) gate(t *testing.T) []secretspresent.Reason {
	t.Helper()
	present, why := f.srv.snapshotSecretsGate(context.Background(), f.run.ID, admissionWindow())
	if present != (len(why) > 0) {
		t.Fatalf("gate = %v %v: present and reasons disagree", present, why)
	}
	return why
}

// TestSnapshot_RefusedWhenSecretsPresent covers every kind of secret a run can hold: one row per kind of
// secret the run ever held, each refused with its reason, and a run with none admitted.
func TestSnapshot_RefusedWhenSecretsPresent(t *testing.T) {
	if why := newSecretsFixture(t).gate(t); why != nil {
		t.Fatalf("a run with no secret = %v, want admitted", why)
	}
	for _, tc := range []struct {
		name string
		set  func(*store.SecretFacts)
		want secretspresent.Reason
	}{
		{"env_secret grant", func(f *store.SecretFacts) { f.GrantKinds["env_secret"] = 1 }, secretspresent.ResidentGrant},
		{"file_secret grant", func(f *store.SecretFacts) { f.GrantKinds["file_secret"] = 1 }, secretspresent.ResidentGrant},
		{"ssh_key grant", func(f *store.SecretFacts) { f.GrantKinds["ssh_key"] = 2 }, secretspresent.ResidentGrant},
		{"a grant of a kind nothing classifies", func(f *store.SecretFacts) { f.GrantKinds["new_kind"] = 1 }, secretspresent.ResidentGrant},
		{"a person's component with an env secret", func(f *store.SecretFacts) {
			f.Components = []store.SecretComponent{{Deliveries: []string{"header", "env"}}}
		}, secretspresent.ResidentGrant},
		{"a person's component with a file secret", func(f *store.SecretFacts) {
			f.Components = []store.SecretComponent{{Deliveries: []string{"file"}}}
		}, secretspresent.ResidentGrant},
		{"the AWS CLI card (an org row)", func(f *store.SecretFacts) {
			f.Components = []store.SecretComponent{{Org: true}}
		}, secretspresent.ResidentGrant},
		{"a component whose definition an erasure cleared", func(f *store.SecretFacts) {
			f.Components = []store.SecretComponent{{Cleared: true}}
		}, secretspresent.ResidentGrant},
		{"a Bedrock AWS sign-in in a resident mode", func(f *store.SecretFacts) { f.Dispatch.BedrockResident = true }, secretspresent.ResidentGrant},
		{"a git token with the dispatch fact set", func(f *store.SecretFacts) { f.Dispatch.GitTokenResident = true }, secretspresent.GitToken},
		{"a takeover", func(f *store.SecretFacts) { f.TakeoverGiven = true }, secretspresent.Takeover},
		{"a desktop sign-in", func(f *store.SecretFacts) { f.SigninBegun = true }, secretspresent.SigninStore},
		{"a harness sign-in run", func(f *store.SecretFacts) { f.HarnessLogin = true }, secretspresent.HarnessLogin},
		{"a support session", func(f *store.SecretFacts) { f.SupportSessionAccepted = true }, secretspresent.SupportSession},
	} {
		f := newSecretsFixture(t)
		f.edit(tc.set)
		if why := f.gate(t); !slices.Equal(why, []secretspresent.Reason{tc.want}) {
			t.Errorf("%s: gate = %v, want [%s]", tc.name, why, tc.want)
		}
	}
	// A header-delivered secret of a person's component, and a brokered grant, stay outside the sandbox.
	f := newSecretsFixture(t)
	f.edit(func(sf *store.SecretFacts) {
		sf.GrantKinds["github_token"], sf.GrantKinds["api_key"], sf.GrantKinds["git_pat"] = 1, 1, 1
		sf.Components = []store.SecretComponent{{Deliveries: []string{"header"}}}
	})
	if why := f.gate(t); why != nil {
		t.Errorf("brokered grants and a header secret = %v, want admitted", why)
	}
}

// grantKindConsts is every GrantKind const internal/types declares, read from its source.
func grantKindConsts(t *testing.T) []string {
	t.Helper()
	return typedStringConsts(t, "../types/types.go", func(name string, typ ast.Expr) bool {
		id, ok := typ.(*ast.Ident)
		return ok && id.Name == "GrantKind"
	})
}

// typedStringConsts parses file and returns the value of every string const keep accepts.
func typedStringConsts(t *testing.T, file string, keep func(name string, typ ast.Expr) bool) []string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), file, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs := spec.(*ast.ValueSpec)
			for i, name := range vs.Names {
				if i >= len(vs.Values) || !keep(name.Name, vs.Type) {
					continue
				}
				if lit, ok := vs.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
					v, err := strconv.Unquote(lit.Value)
					if err != nil {
						t.Fatal(err)
					}
					out = append(out, v)
				}
			}
		}
	}
	return out
}

// TestSecretsPresent_CoversEveryGrantKind: every GrantKind is classified resident or brokered, exactly once, and
// every brokered kind has the TTL its mints are judged by. A new kind fails here until someone says which it is.
func TestSecretsPresent_CoversEveryGrantKind(t *testing.T) {
	kinds := grantKindConsts(t)
	if len(kinds) < 7 {
		t.Fatalf("read only %d GrantKind consts %v; the source reader is broken", len(kinds), kinds)
	}
	for _, k := range kinds {
		_, brokered := brokeredGrantKinds[types.GrantKind(k)]
		if residentGrantKinds[types.GrantKind(k)] == brokered {
			t.Errorf("grant kind %q is classified resident=%v brokered=%v; want exactly one", k, residentGrantKinds[types.GrantKind(k)], brokered)
		}
		if brokered && brokeredGrantKinds[types.GrantKind(k)] <= 0 {
			t.Errorf("brokered grant kind %q has no TTL", k)
		}
	}
	for k := range residentGrantKinds {
		if !slices.Contains(kinds, string(k)) {
			t.Errorf("resident grant kind %q is not a GrantKind const", k)
		}
	}
	for k := range brokeredGrantKinds {
		if !slices.Contains(kinds, string(k)) {
			t.Errorf("brokered grant kind %q is not a GrantKind const", k)
		}
	}
}

// TestSecretsPresent_CoversEveryComponentDelivery: every component secret delivery mode is classified resident or
// not, and nothing else is.
func TestSecretsPresent_CoversEveryComponentDelivery(t *testing.T) {
	modes := typedStringConsts(t, "../types/component.go", func(name string, _ ast.Expr) bool {
		return strings.HasPrefix(name, "ComponentDelivery")
	})
	if len(modes) < 3 {
		t.Fatalf("read only %d delivery modes %v; the source reader is broken", len(modes), modes)
	}
	for _, m := range modes {
		if _, ok := componentDeliveryResident[m]; !ok {
			t.Errorf("delivery mode %q is not classified in componentDeliveryResident", m)
		}
	}
	for m := range componentDeliveryResident {
		if !slices.Contains(modes, m) {
			t.Errorf("componentDeliveryResident classifies %q, which is no delivery mode", m)
		}
	}
	f := newSecretsFixture(t)
	f.edit(func(sf *store.SecretFacts) {
		sf.Components = []store.SecretComponent{{Deliveries: []string{"carrier_pigeon"}}}
	})
	if why := f.gate(t); !slices.Equal(why, []secretspresent.Reason{secretspresent.ResidentGrant}) {
		t.Errorf("an unknown delivery mode = %v, want resident_grant", why)
	}
}

// TestSecretsPresent_GitTokenFromDispatchRow: the git token's fact is the run's own row, written by its dispatch
// and by every revive, never the server's broker setting now.
func TestSecretsPresent_GitTokenFromDispatchRow(t *testing.T) {
	ctx := context.Background()
	f := newSecretsFixture(t)
	f.edit(func(sf *store.SecretFacts) { sf.Dispatch.GitTokenResident = true })
	f.srv.cfg.DisableGitPATBroker = false
	if why := f.gate(t); !slices.Equal(why, []secretspresent.Reason{secretspresent.GitToken}) {
		t.Errorf("the row set and the broker on now = %v, want git_token", why)
	}
	f = newSecretsFixture(t)
	f.srv.cfg.DisableGitPATBroker = true
	if why := f.gate(t); why != nil {
		t.Errorf("the row clear and the broker off now = %v, want admitted", why)
	}

	// Dispatched with the broker on (row clear), revived with it off: the revive's proxy hands the PAT over, and
	// its write sets the fact.
	f = newSecretsFixture(t)
	pat := types.CredentialGrant{ID: uuid.New(), Spec: types.GrantSpec{Kind: types.GrantGitPAT}}
	f.st.grants = []types.CredentialGrant{pat}
	fresh := uuid.NewString()
	f.srv.recordRevivedSecretFacts(ctx, f.run, &proxy.Config{}, fresh)
	f.storeToken(t, fresh)
	if why := f.gate(t); !slices.Equal(why, []secretspresent.Reason{secretspresent.GitToken}) {
		t.Errorf("revived with the broker off = %v, want git_token", why)
	}
	// A revive that brokers the PAT adds nothing, and never clears what an earlier write set.
	f.srv.recordRevivedSecretFacts(ctx, f.run, &proxy.Config{BrokeredPATGrantIDs: []uuid.UUID{pat.ID}}, fresh)
	if why := f.gate(t); !slices.Equal(why, []secretspresent.Reason{secretspresent.GitToken}) {
		t.Errorf("a later brokered revive = %v, want git_token still", why)
	}

	// A revive whose facts write failed leaves the row at the old token while the stored config has the new one.
	f = newSecretsFixture(t)
	f.st.reviveErr = errors.New("down")
	fresh = uuid.NewString()
	f.srv.recordRevivedSecretFacts(ctx, f.run, &proxy.Config{}, fresh)
	f.storeToken(t, fresh)
	if why := f.gate(t); !slices.Equal(why, []secretspresent.Reason{secretspresent.Unknown}) {
		t.Errorf("a revive whose facts write failed = %v, want unknown", why)
	}

	// A revive while the switch is off still moves the jti, so the gate reads the row.
	f = newSecretsFixture(t)
	if machineSnapshotsOn(siteFeaturesDoc(t, `{"features":{"machine_snapshots":{"enabled":true}}}`)) {
		t.Fatal("the switch reads on before the key is registered")
	}
	fresh = uuid.NewString()
	f.srv.recordRevivedSecretFacts(ctx, f.run, &proxy.Config{}, fresh)
	f.storeToken(t, fresh)
	if why := f.gate(t); why != nil {
		t.Errorf("a revive while the switch is off = %v, want admitted", why)
	}

	// Neither the gate nor the gatherer reads the server's broker setting.
	for _, file := range []string{"run_secrets_present.go", "run_snapshot_gate.go"} {
		parsed, err := parser.ParseFile(token.NewFileSet(), file, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(parsed, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok && id.Name == "DisableGitPATBroker" {
				t.Errorf("%s reads DisableGitPATBroker; the git token's fact is the run's dispatch row", file)
			}
			return true
		})
	}
}

// TestSnapshot_RefusedWithinMintTTL: a brokered credential minted for the run counts while it may still be
// valid, judged at admission against the database's clock and after a cut against the cut's own window; a mint
// whose grant row is gone counts for as long as the run lives.
func TestSnapshot_RefusedWithinMintTTL(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name  string
		mints map[string]time.Time
		w     mintWindow
		want  bool
	}{
		{"a github token minted a minute ago", map[string]time.Time{"github_token": now.Add(-time.Minute)}, admissionWindow(), true},
		{"a github token minted two hours ago", map[string]time.Time{"github_token": now.Add(-2 * time.Hour)}, admissionWindow(), false},
		{"a mint whose grant row is gone, a day ago", map[string]time.Time{"": now.Add(-24 * time.Hour)}, admissionWindow(), true},
		{"a mint of an unknown kind, a day ago", map[string]time.Time{"new_kind": now.Add(-24 * time.Hour)}, admissionWindow(), true},
		{"valid past the cut's start", map[string]time.Time{"api_key": now.Add(-50 * time.Minute)},
			cutWindow(now.Add(-20*time.Minute), now.Add(-19*time.Minute)), true},
		{"expired before the cut's start", map[string]time.Time{"api_key": now.Add(-90 * time.Minute)},
			cutWindow(now.Add(-20*time.Minute), now.Add(-19*time.Minute)), false},
	} {
		in := secretsInputs(store.SecretFacts{Now: now, NewestMints: tc.mints, Dispatch: &store.DispatchSecretFacts{}}, tc.w)
		if in.RecentCredential != tc.want {
			t.Errorf("%s: recent = %v, want %v", tc.name, in.RecentCredential, tc.want)
		}
	}
	f := newSecretsFixture(t)
	f.edit(func(sf *store.SecretFacts) {
		sf.NewestMints = map[string]time.Time{"github_token": time.Now().Add(-time.Minute)}
	})
	if why := f.gate(t); !slices.Equal(why, []secretspresent.Reason{secretspresent.RecentCredential}) {
		t.Errorf("gate with a fresh mint = %v, want recent_credential", why)
	}
}

// TestSecretsPresent_FailsClosedOnEveryUnreadableFact: each fact the gate cannot read answers unknown.
func TestSecretsPresent_FailsClosedOnEveryUnreadableFact(t *testing.T) {
	unknown := []secretspresent.Reason{secretspresent.Unknown}
	for _, tc := range []struct {
		name string
		set  func(t *testing.T, f *secretsFixture)
	}{
		{"a store error", func(_ *testing.T, f *secretsFixture) { f.st.readErr = errors.New("down") }},
		{"the run row is gone", func(_ *testing.T, f *secretsFixture) { delete(f.st.facts, f.run.ID) }},
		{"a store that keeps no secret facts", func(_ *testing.T, f *secretsFixture) { f.srv.cfg.Store = &proxyOnlyStore{st: f.st} }},
		{"no dispatch row (dispatched while the switch was off)", func(_ *testing.T, f *secretsFixture) {
			f.edit(func(sf *store.SecretFacts) { sf.Dispatch = nil })
		}},
		{"a stale recorded jti", func(t *testing.T, f *secretsFixture) { f.storeToken(t, uuid.NewString()) }},
		{"an empty recorded jti", func(t *testing.T, f *secretsFixture) {
			f.edit(func(sf *store.SecretFacts) { sf.Dispatch.RecordedJTI = "" })
		}},
		{"no stored proxy config", func(_ *testing.T, f *secretsFixture) { delete(f.st.sealed, f.run.ID) }},
		{"a revive that failed after its facts write, while the old proxy runs", func(_ *testing.T, f *secretsFixture) {
			f.srv.recordRevivedSecretFacts(context.Background(), f.run, &proxy.Config{}, uuid.NewString())
		}},
		{"a server with no run config key", func(_ *testing.T, f *secretsFixture) { f.srv.cfg.RunConfigKey = nil }},
		{"a stored token that is not a token", func(t *testing.T, f *secretsFixture) {
			if err := f.srv.storeRunProxyConfig(context.Background(), f.run.ID, []byte(`{"run_token":"not-a-jwt"}`)); err != nil {
				t.Fatal(err)
			}
		}},
		{"audit rows of the run dropped by retention", func(_ *testing.T, f *secretsFixture) {
			f.edit(func(sf *store.SecretFacts) { sf.AuditRowsDropped = true })
		}},
	} {
		f := newSecretsFixture(t)
		tc.set(t, f)
		if why := f.gate(t); !slices.Equal(why, unknown) {
			t.Errorf("%s: gate = %v, want unknown", tc.name, why)
		}
	}
}

// proxyOnlyStore keeps proxy configs and nothing of the secret facts.
type proxyOnlyStore struct {
	store.Store
	st *secretFactsStore
}

func (p *proxyOnlyStore) PutRunProxyConfig(ctx context.Context, id uuid.UUID, b []byte) error {
	return p.st.PutRunProxyConfig(ctx, id, b)
}

func (p *proxyOnlyStore) GetRunProxyConfig(ctx context.Context, id uuid.UUID) ([]byte, error) {
	return p.st.GetRunProxyConfig(ctx, id)
}

func (p *proxyOnlyStore) DeleteRunProxyConfig(ctx context.Context, id uuid.UUID) error {
	return p.st.DeleteRunProxyConfig(ctx, id)
}

func (p *proxyOnlyStore) PurgeTerminalRunProxyConfigs(ctx context.Context) (int64, error) {
	return p.st.PurgeTerminalRunProxyConfigs(ctx)
}

// TestDispatch_RecordsSecretFactsOnlyWhenOn: a linux_vm run dispatched while the switch is on gets its row, with
// the Bedrock and git facts its dispatch decided and its token's jti; with the switch off, or another machine,
// nothing is written; a write that fails fails the run with its reason. The write sits in resolveLLMInjections
// before the Bedrock row, so a run it fails never shows a Bedrock credential handed over.
func TestDispatch_RecordsSecretFactsOnlyWhenOn(t *testing.T) {
	ctx := context.Background()
	on := siteFeaturesDoc(t, `{"features":{"machine_snapshots":{"enabled":true}}}`)
	jti := uuid.NewString()
	p := dispatchParams{RunToken: testRunToken(t, jti), PATBroker: false,
		Policy: types.RunPolicySpec{EligibleGrants: []types.GrantSpec{{Kind: types.GrantGitPAT}}}}
	sso := llmTransport{bedrockReady: true, bedrockAudit: bedrockTransportAudit{mode: "sso-inject"}}

	f := newSecretsFixture(t)
	delete(f.st.facts, f.run.ID)
	if !f.srv.recordDispatchSecretFacts(ctx, f.run, p, sso, on, true, true) || f.st.writes != 0 {
		t.Fatalf("switch not registered: ok, %d writes; want ok, 0", f.st.writes)
	}
	withSiteFeatureHooks(t, availableSiteFeatures(featureMachineSnapshots))
	if f.srv.recordDispatchSecretFacts(ctx, f.run, p, sso, on, true, false) || f.st.writes != 0 {
		t.Fatalf("a dispatch an earlier check refused: went on, or wrote %d rows", f.st.writes)
	}
	off := siteFeaturesDoc(t, `{"features":{"machine_snapshots":{"enabled":false}}}`)
	container := f.run
	container.MachineKind = ""
	for name, call := range map[string]func() bool{
		"switch off":             func() bool { return f.srv.recordDispatchSecretFacts(ctx, f.run, p, sso, off, true, true) },
		"site config unreadable": func() bool { return f.srv.recordDispatchSecretFacts(ctx, f.run, p, sso, on, false, true) },
		"a container run":        func() bool { return f.srv.recordDispatchSecretFacts(ctx, container, p, sso, on, true, true) },
	} {
		if !call() || f.st.writes != 0 {
			t.Errorf("%s: wrote %d rows, want none", name, f.st.writes)
		}
	}
	if !f.srv.recordDispatchSecretFacts(ctx, f.run, p, sso, on, true, true) {
		t.Fatal("switch on: the write failed the run")
	}
	got := f.st.facts[f.run.ID].Dispatch
	if got == nil || !got.BedrockResident || !got.GitTokenResident || got.RecordedJTI != jti {
		t.Errorf("switch on: row = %+v, want Bedrock and git resident and jti %s", got, jti)
	}

	f = newSecretsFixture(t)
	delete(f.st.facts, f.run.ID)
	bearer := llmTransport{bedrockReady: true, bedrockAudit: bedrockTransportAudit{mode: "bearer"}}
	brokered := p
	brokered.PATBroker = true
	if !f.srv.recordDispatchSecretFacts(ctx, f.run, brokered, bearer, on, true, true) {
		t.Fatal("bearer and broker on: the write failed the run")
	}
	if got := f.st.facts[f.run.ID].Dispatch; got == nil || got.BedrockResident || got.GitTokenResident {
		t.Errorf("bearer and broker on: row = %+v, want neither resident", got)
	}

	f = newSecretsFixture(t)
	f.st.writeErr = errors.New("down")
	if f.srv.recordDispatchSecretFacts(ctx, f.run, p, sso, on, true, true) {
		t.Fatal("a failed write let the dispatch go on")
	}
	if !f.st.failed {
		t.Error("a failed write did not fail the run")
	}
	rows := f.audit.find("run.dispatch")
	if len(rows) != 1 || rows[0].Outcome != "failure" || !strings.Contains(string(rows[0].Data), reasonRunSecretFactsUnrecorded) {
		t.Errorf("run.dispatch rows = %+v, want one failure naming %s", rows, reasonRunSecretFactsUnrecorded)
	}

	// The call's place: in resolveLLMInjections, once, before the Bedrock row.
	parsed, err := parser.ParseFile(token.NewFileSet(), "runs_dispatch_llm.go", nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	var calls []string
	for _, d := range parsed.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == "resolveLLMInjections" {
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				if c, ok := n.(*ast.CallExpr); ok {
					if sel, ok := c.Fun.(*ast.SelectorExpr); ok && (sel.Sel.Name == "recordDispatchSecretFacts" || sel.Sel.Name == "recordBedrockTransport") {
						calls = append(calls, sel.Sel.Name)
					}
				}
				return true
			})
		}
	}
	if !slices.Equal(calls, []string{"recordDispatchSecretFacts", "recordBedrockTransport"}) {
		t.Errorf("resolveLLMInjections calls %v, want the facts write once, before the Bedrock row", calls)
	}
}

// TestRunTokenJTI_ReadsExpiredToken: the jti comes out of an expired token read back from a stored config, and a
// token that is malformed, or carries no jti, is an error.
func TestRunTokenJTI_ReadsExpiredToken(t *testing.T) {
	jti := uuid.NewString()
	if got, err := runTokenJTI(testRunToken(t, jti)); err != nil || got != jti {
		t.Fatalf("runTokenJTI of an expired token = %q, %v; want %q", got, err, jti)
	}
	f := newSecretsFixture(t)
	f.storeToken(t, jti)
	if got, err := f.srv.currentRunJTI(context.Background(), f.run.ID); err != nil || got != jti {
		t.Fatalf("currentRunJTI from the stored config = %q, %v; want %q", got, err, jti)
	}
	for _, bad := range []string{"", "not-a-jwt", "a.b.c", testRunToken(t, "")} {
		if got, err := runTokenJTI(bad); err == nil {
			t.Errorf("runTokenJTI(%.20q) = %q, want an error", bad, got)
		}
	}
}

// TestMachineSnapshotsFeature_RefusesSettingsNotYetBuilt: validate keeps every setting in its bounds, refuses a
// sleep setting this release cannot act on, and the key reads off, with nothing registered, whatever is stored.
func TestMachineSnapshotsFeature_RefusesSettingsNotYetBuilt(t *testing.T) {
	ctx := context.Background()
	validate := func(m types.MachineSnapshotsFeature) error {
		return validateMachineSnapshotsFeature(ctx, nil, &types.SiteFeatureSwitches{MachineSnapshots: &m}, nil)
	}
	on := true
	if err := validate(types.MachineSnapshotsFeature{Enabled: &on, MaxPerPerson: 8, IdleDays: 90,
		WarmPool: []types.WarmPoolTarget{{Image: "ghcr.io/acme/dev:1", Count: 4}, {Image: "ghcr.io/acme/web:1"}}}); err != nil {
		t.Errorf("settings in bounds: %v", err)
	}
	if err := validateMachineSnapshotsFeature(ctx, nil, nil, nil); err != nil {
		t.Errorf("no block: %v", err)
	}
	for name, m := range map[string]types.MachineSnapshotsFeature{
		"sleep_after_disconnect_sec in bounds, not built": {SleepAfterDisconnectSec: 600},
		"sleep_after_disconnect_sec below 60":             {SleepAfterDisconnectSec: 59},
		"sleep_after_disconnect_sec above a day":          {SleepAfterDisconnectSec: 86401},
		"sleep_after_disconnect_sec negative":             {SleepAfterDisconnectSec: -1},
		"max_per_person above 8":                          {MaxPerPerson: 9},
		"max_per_person negative":                         {MaxPerPerson: -1},
		"idle_days above 90":                              {IdleDays: 91},
		"a warm pool entry with no image":                 {WarmPool: []types.WarmPoolTarget{{Image: " ", Count: 1}}},
		"a warm pool count above 4":                       {WarmPool: []types.WarmPoolTarget{{Image: "a", Count: 5}}},
		"an image listed twice":                           {WarmPool: []types.WarmPoolTarget{{Image: "a"}, {Image: "a"}}},
	} {
		if err := validate(m); err == nil {
			t.Errorf("%s: accepted", name)
		} else if _, own := errors.AsType[*siteFeatureRefusal](err); own {
			t.Errorf("%s: %v answers its own reason, want site_config_invalid", name, err)
		}
	}
	stored := siteFeaturesDoc(t, `{"features":{"machine_snapshots":{"enabled":true,"max_per_person":3}}}`)
	if siteFeatureAvailable(featureMachineSnapshots) || machineSnapshotsOn(stored) {
		t.Error("machine_snapshots is available or reads on with nothing registered")
	}
	if got := machineSnapshotsSettings(stored); got.MaxPerPerson != 3 || got.IdleDays != machineSnapshotsIdleDaysDefault || got.Enabled != nil {
		t.Errorf("settings = %+v, want max_per_person 3, the idle default and no switch", got)
	}
	if got := machineSnapshotsSettings(types.SiteConfig{}); got.MaxPerPerson != machineSnapshotsMaxPerPersonDefault {
		t.Errorf("settings of no block = %+v, want the defaults", got)
	}
}

// TestSnapshotReasonsAreWireShaped: every snapshot reason is one snake_case word list and unique.
func TestSnapshotReasonsAreWireShaped(t *testing.T) {
	all := []string{reasonSnapshotsOff, reasonSnapshotBusy, reasonSnapshotNoSpace, reasonSnapshotSecretsPresent,
		reasonSnapshotMachineKindUnsupported, reasonSnapshotInUse, reasonForkContextChanged, reasonForkImageNotAdmitted,
		reasonForkBaseChanged, reasonForkNotDelegable, reasonForkSupportSession, reasonSnapshotSourceEnded,
		reasonSnapshotCommitUnconfirmed, reasonSnapshotOrphaned, reasonForkSnapshotUnavailable, reasonRunSecretFactsUnrecorded}
	seen := map[string]bool{}
	for _, r := range all {
		if !reasonWireValueShape.MatchString(r) || seen[r] {
			t.Errorf("snapshot reason %q is not wire-shaped, or is listed twice", r)
		}
		seen[r] = true
	}
}
