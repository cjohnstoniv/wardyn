// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	gooidctest "github.com/coreos/go-oidc/v3/oidc/oidctest"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"golang.org/x/oauth2"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const regConsoleIssuer = "https://console.example.com/oauth2/default"

// regCensusQuerier is the transaction a fake registration decides on: it answers the registry lock, the
// binding read and the census counts from the fake store's script, and nothing else.
type regCensusQuerier struct{ f *regStore }

var regCensusSQL = regexp.MustCompile(`(?s)FROM (\w+)(?: \w+)?\s+(?:JOIN \w+ \w+ ON [^W]*)?WHERE (?:lower\((\w+)(?:::text)?\)|e\.action|issuer =)`)

func (q regCensusQuerier) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.NewCommandTag("SELECT 1"), nil
}

func (q regCensusQuerier) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errors.New("regCensusQuerier: no query")
}

type regRow struct {
	n   int
	err error
}

func (r regRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	switch d := dest[0].(type) {
	case *int:
		*d = r.n
	}
	return nil
}

func (q regCensusQuerier) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	q.f.mu.Lock()
	defer q.f.mu.Unlock()
	if strings.Contains(sql, "FROM portal_issuers WHERE issuer_key") {
		for issuer := range q.f.bound {
			if types.PortalIssuerKey(issuer) == args[0] {
				return regRow{n: 1}
			}
		}
		return regRow{err: pgx.ErrNoRows}
	}
	if strings.Contains(sql, "FROM governance_units WHERE id") {
		for _, u := range q.f.units {
			if u.ID == args[0] {
				return regRow{n: 1}
			}
		}
		return regRow{err: pgx.ErrNoRows}
	}
	if strings.Contains(sql, "FROM user_types WHERE id") {
		if args[0] == "known" {
			return regRow{n: 1}
		}
		return regRow{err: pgx.ErrNoRows}
	}
	name := ""
	switch m := regCensusSQL.FindStringSubmatch(sql); {
	case strings.Contains(sql, "audit_events e"):
		name = "audit_events.data"
	case m != nil && m[2] != "":
		name = m[1] + "." + m[2]
	case m != nil:
		name = m[1] + ".issuer"
	}
	return regRow{n: q.f.census[name]}
}

// regStore is the store of a registration test: the portal store of the mem tests, the registry written
// in memory, and the few reads a registration makes.
type regStore struct {
	store.Store
	*fakeDelegateStore
	mu        sync.Mutex
	activated bool
	units     []types.GovernanceUnit
	census    map[string]int                // "table.column" -> count, for a key no binding holds
	bound     map[string]types.PortalIssuer // by exact issuer
	console   []types.ConsoleIssuer
	created   []types.Delegate
}

func newRegStore() *regStore {
	return &regStore{
		fakeDelegateStore: newFakeDelegateStore(), activated: true,
		units:  []types.GovernanceUnit{{ID: uuid.New(), Name: "platform"}},
		census: map[string]int{}, bound: map[string]types.PortalIssuer{},
	}
}

func (f *regStore) PortalIssuersActivated(context.Context) (bool, error) { return f.activated, nil }
func (f *regStore) GetUserType(_ context.Context, id string) (types.UserType, error) {
	if id == "known" {
		return types.UserType{ID: id}, nil
	}
	return types.UserType{}, store.ErrNotFound
}
func (f *regStore) ListGovernanceUnits(context.Context) ([]types.GovernanceUnit, error) {
	return f.units, nil
}
func (f *regStore) WriteGovernance(ctx context.Context, fn func(store.Querier) error) error {
	return fn(regCensusQuerier{f})
}
func (f *regStore) DryRunGovernance(ctx context.Context, fn func(store.Querier) error) error {
	return fn(regCensusQuerier{f})
}
func (f *regStore) ConsoleIssuersQ(context.Context, store.Querier) ([]types.ConsoleIssuer, error) {
	return f.console, nil
}
func (f *regStore) RecordConsoleIssuerQ(context.Context, store.Querier, string, string) error {
	return nil
}
func (f *regStore) RetirePortalIssuerQ(context.Context, store.Querier, string) error { return nil }
func (f *regStore) UpdateDelegateAdmissionQ(context.Context, store.Querier, uuid.UUID, types.PortalAdmission, types.PortalAdmission) error {
	return nil
}

func (f *regStore) BindPortalIssuerQ(_ context.Context, _ store.Querier, issuer, norm, claim, by string) (types.PortalIssuer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if b, ok := f.bound[issuer]; ok {
		switch {
		case b.InternalIssuerNorm != norm:
			return types.PortalIssuer{}, store.ErrIssuerAddressConflict
		case b.PersonIDClaim != claim:
			return types.PortalIssuer{}, store.ErrIssuerProfileConflict
		}
		return b, nil
	}
	for other := range f.bound {
		if types.NormIssuerURL(other) == types.NormIssuerURL(issuer) {
			return types.PortalIssuer{}, store.ErrIssuerSpellingConflict
		}
	}
	b := types.PortalIssuer{IssuerKey: types.PortalIssuerKey(issuer), Issuer: issuer, InternalIssuerNorm: norm, PersonIDClaim: claim, CreatedBy: by}
	f.bound[issuer] = b
	return b, nil
}

func (f *regStore) CreateDelegateQ(_ context.Context, _ store.Querier, d types.Delegate, digest string) (types.Delegate, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d.CredentialSHA256 = digest
	f.created = append(f.created, d)
	f.fakeDelegateStore.mu.Lock()
	f.fakeDelegateStore.delegates[d.ID] = d
	f.fakeDelegateStore.mu.Unlock()
	return d, nil
}

// regConsoleAuth is a console authenticator whose public issuer is issuer.
func regConsoleAuth(t *testing.T, issuer string) *oidc.Authenticator {
	t.Helper()
	u := strings.TrimPrefix(issuer, "https://")
	prefix := u[strings.Index(u, "/"):]
	idp := &gooidctest.Server{}
	idp.SetIssuer(issuer)
	mux := http.NewServeMux()
	mux.Handle(prefix+"/", http.StripPrefix(prefix, idp))
	fake := httptest.NewTLSServer(mux)
	t.Cleanup(fake.Close)
	a, err := oidc.New(context.WithValue(context.Background(), oauth2.HTTPClient, fake.Client()), oidc.Config{
		IssuerURL: issuer, InternalIssuerURL: fake.URL + prefix, ClientID: "wardyn-client", ClientSecret: "secret",
		RedirectURL: "http://localhost/auth/callback", DefaultRole: oidc.RoleUser,
	}, accessTestHMACKey)
	if err != nil {
		t.Fatalf("oidc.New: %v", err)
	}
	return a
}

type regEnv struct {
	srv *Server
	st  *regStore
}

func newRegEnv(t *testing.T) regEnv {
	t.Helper()
	h := newHarness(t)
	st := newRegStore()
	cfg := baseTestConfig(h, st)
	cfg.OIDC = regConsoleAuth(t, regConsoleIssuer)
	return regEnv{srv: New(cfg), st: st}
}

// regBody is a valid access-token issuer registration, edited by mut (a nil value deletes the key).
func regBody(mut map[string]any) string {
	b := map[string]any{
		"name": "portal", "idp_client_id": "0oa1", "issuer": "https://idp.example.com/oauth2/default",
		"token_profile": "okta_access_token", "audience": "api://wardyn", "required_scopes": []string{"wardyn.run"},
		"portal_managed_admission": true, "user_type": types.UserTypeStandard, "run_in_under": "platform",
	}
	for k, v := range mut {
		if v == nil {
			delete(b, k)
		} else {
			b[k] = v
		}
	}
	raw, _ := json.Marshal(b)
	return string(raw)
}

func (e regEnv) register(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	return do(t, e.srv, http.MethodPost, "/api/v1/admin/delegates", adminToken, body)
}

// wantRefusal checks the answer is status with reason and that nothing was stored.
func (e regEnv) wantRefusal(t *testing.T, name, body string, status int, reason string) {
	t.Helper()
	before := len(e.st.created)
	w := e.register(t, body)
	if w.Code != status || reasonOf(t, w) != reason {
		t.Errorf("%s: %d %s, want %d %s", name, w.Code, w.Body.String(), status, reason)
	}
	if len(e.st.created) != before {
		t.Errorf("%s: a refused registration stored a portal", name)
	}
}

func (e regEnv) wantCreated(t *testing.T, name, body string) types.Delegate {
	t.Helper()
	w := e.register(t, body)
	if w.Code != http.StatusCreated {
		t.Fatalf("%s: %d %s, want 201", name, w.Code, w.Body.String())
	}
	var d types.Delegate
	if err := json.Unmarshal(w.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestPortalRegistration_PinRequired(t *testing.T) {
	e := newRegEnv(t)
	e.wantRefusal(t, "no pin", regBody(map[string]any{"run_in_under": nil}), 422, reasonDelegateIssuerPinRequired)
	e.wantRefusal(t, "blank pin", regBody(map[string]any{"run_in_under": "  "}), 422, reasonDelegateIssuerPinRequired)
	e.wantRefusal(t, "unknown unit", regBody(map[string]any{"run_in_under": "nowhere"}), 422, reasonDelegateRunInUnderInvalid)
	d := e.wantCreated(t, "pinned", regBody(nil))
	if d.RunInUnder == nil || *d.RunInUnder != e.st.units[0].ID {
		t.Errorf("pin = %v, want the platform unit", d.RunInUnder)
	}
}

func TestPortalRegistration_ConsoleIssuerRefused(t *testing.T) {
	e := newRegEnv(t)
	for _, iss := range []string{regConsoleIssuer, regConsoleIssuer + "/", "https://Console.Example.com:443/oauth2/default/"} {
		e.wantRefusal(t, iss, regBody(map[string]any{"issuer": iss}), 409, reasonDelegateIssuerIsConsole)
	}
	e.wantCreated(t, "another issuer", regBody(nil))
	e.st.console = []types.ConsoleIssuer{{IssuerNorm: types.NormIssuerURL("https://old.example.com/a"), Issuer: "https://old.example.com/a"}}
	e.wantRefusal(t, "a past console issuer", regBody(map[string]any{"issuer": "https://old.example.com/a/"}), 409, reasonDelegateIssuerIsConsole)
	cfg := e.srv.cfg
	cfg.OIDC = nil
	e.srv = New(cfg)
	e.wantRefusal(t, "no console sign-in", regBody(map[string]any{"issuer": "https://idp2.example.com"}), 409, reasonPortalIssuersNeedSSO)
}

func TestPortalRegistration_URLShapeRefused(t *testing.T) {
	e := newRegEnv(t)
	t.Setenv(envPortalInternalIssuerHosts, "idp.internal.example.com")
	long := "https://idp.example.com/" + strings.Repeat("a", portalURLMaxLen)
	for _, iss := range []string{"http://idp.example.com", "HTTPS://idp.example.com", "ftp://idp.example.com", "https://", "https:///x",
		"https://user:pw@idp.example.com", "https://idp.example.com?x=1", "https://idp.example.com#f", "https://idp.example.com/a b",
		"https://idp.example.com/\t", " https://idp.example.com", "https://idp.example.com:99999x", "idp.example.com", long} {
		e.wantRefusal(t, "issuer "+iss, regBody(map[string]any{"issuer": iss}), 422, reasonDelegateIssuerURLInvalid)
		e.wantRefusal(t, "internal "+iss, regBody(map[string]any{"internal_issuer": iss}), 422, reasonDelegateIssuerURLInvalid)
	}
	// A trailing slash is part of the issuer's exact bytes and is kept.
	d := e.wantCreated(t, "trailing slash", regBody(map[string]any{"issuer": "https://idp.example.com/oauth2/default/", "internal_issuer": "https://idp.internal.example.com/oauth2/default"}))
	if d.Issuer != "https://idp.example.com/oauth2/default/" {
		t.Errorf("issuer stored as %q, want the exact bytes", d.Issuer)
	}
}

func TestPortalRegistration_InternalIssuerHostNotListedRefused(t *testing.T) {
	e := newRegEnv(t)
	internal := func(u string) string { return regBody(map[string]any{"internal_issuer": u}) }
	t.Setenv(envPortalInternalIssuerHosts, "")
	e.wantRefusal(t, "no list", internal("https://idp.internal.example.com"), 422, reasonDelegateInternalIssuerNotListed)
	t.Setenv(envPortalInternalIssuerHosts, "other.internal.example.com, IDP.internal.example.com:8443 ,[fd00::1]")
	e.wantRefusal(t, "listed on another port", internal("https://idp.internal.example.com"), 422, reasonDelegateInternalIssuerNotListed)
	e.wantRefusal(t, "unlisted host", internal("https://idp.example.com"), 422, reasonDelegateInternalIssuerNotListed)
	e.wantCreated(t, "listed with its port", internal("https://idp.internal.example.com:8443/oauth2/default"))
	e.wantCreated(t, "listed ipv6", regBody(map[string]any{"issuer": "https://idp3.example.com", "internal_issuer": "https://[fd00::1]"}))
}

func TestPortalRegistration_NoOutboundCallBeforeExchange(t *testing.T) {
	var calls atomic.Int32
	prev := http.DefaultTransport
	http.DefaultTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, errors.New("outbound call: " + r.URL.String())
	})
	t.Cleanup(func() { http.DefaultTransport = prev })
	e := newRegEnv(t)
	calls.Store(0) // the console fixture's own discovery is not a registration's
	t.Setenv(envPortalInternalIssuerHosts, "idp.internal.example.com")
	e.wantCreated(t, "registration", regBody(map[string]any{"internal_issuer": "https://idp.internal.example.com/oauth2/default"}))
	e.wantRefusal(t, "refused registration", regBody(map[string]any{"issuer": "https://x.example.com", "audience": nil}), 422, reasonPortalRegistrationInvalid)
	if n := calls.Load(); n != 0 {
		t.Errorf("registration made %d outbound call(s) through the default transport, want 0", n)
	}
}

func TestPortalRegistration_DigestNeverInDiffOrReads(t *testing.T) {
	e := newRegEnv(t)
	d := e.wantCreated(t, "registration", regBody(map[string]any{"required_claim": map[string]string{"claim": "tenant", "value": "prod"}}))
	digest := e.st.created[0].CredentialSHA256
	if digest == "" || digest != hashPortalCredential(d.Credential) {
		t.Fatalf("digest %q is not the SHA-256 of the returned credential", digest)
	}
	// What a reader of the held change is served: the rendered diff, the change row, and the portal list.
	held := e.st.created[0]
	held.Credential = ""
	after := delegateDiffViewOf(held)
	change := types.GovernanceChange{TargetKind: govKindDelegate, Op: "create", TargetKey: held.ID.String(),
		Payload: mustJSON(heldDelegate{Delegate: held, Digest: digest}),
		Diff:    renderGovernanceDiff(nil, after, changedPaths(nil, after), nil)}
	served, _ := json.Marshal(change)
	list := do(t, e.srv, http.MethodGet, "/api/v1/admin/delegates", adminToken, "")
	for name, body := range map[string]string{"change": string(served), "diff": string(change.Diff), "list": list.Body.String()} {
		if strings.Contains(body, digest) || strings.Contains(body, d.Credential) || strings.Contains(body, "credential") {
			t.Errorf("%s carries the credential or its digest: %s", name, body)
		}
	}
	if !strings.Contains(string(change.Diff), "tenant") || !strings.Contains(string(change.Diff), "prod") {
		t.Errorf("the diff must show the claim name and value the approver approves: %s", change.Diff)
	}
}

func TestPortalRegistration_LegacyPathUnchanged(t *testing.T) {
	e := newRegEnv(t)
	w := e.register(t, `{"name":" Support portal ","idp_client_id":" portal-client ","group":"Portal-Users"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("legacy registration = %d %s", w.Code, w.Body.String())
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	keys := map[string]bool{}
	for k := range got {
		keys[k] = true
	}
	for _, k := range []string{"id", "name", "idp_client_id", "group", "registered_by", "created_at", "credential"} {
		if !keys[k] {
			t.Errorf("legacy answer lost %q: %s", k, w.Body.String())
		}
		delete(keys, k)
	}
	if len(keys) != 0 {
		t.Errorf("legacy answer gained keys %v: %s", keys, w.Body.String())
	}
	var d types.Delegate
	_ = json.Unmarshal(w.Body.Bytes(), &d)
	if !strings.HasPrefix(d.Credential, "wdp_") || d.Name != "Support portal" || d.Group != "portal-users" || d.Issuer != "" {
		t.Errorf("legacy portal = %+v", d)
	}
	if len(e.st.created) != 0 || len(e.st.bound) != 0 {
		t.Error("a console portal went through the issuer registry")
	}
	// An unknown field is still a 400, and an issuer-only field on this path is refused, never ignored.
	if w := e.register(t, `{"name":"p","idp_client_id":"c","group":"g","surprise":1}`); w.Code != http.StatusBadRequest {
		t.Errorf("unknown field = %d, want 400", w.Code)
	}
	for _, body := range []string{`"token_profile":"okta_id_token"`, `"user_type":"standard"`, `"audience":"a"`, `"max_subject_token_age_seconds":300`, `"required_claim":{"claim":"a","value":"b"}`} {
		e.wantRefusal(t, body, `{"name":"p","idp_client_id":"c","group":"g",`+body+`}`, 422, reasonDelegateIssuerFieldNeedsIssuer)
	}
}

func TestGovernanceDelegateKind_RegisteredWithOperatorApprover(t *testing.T) {
	kind, ok := governanceChangeKinds[govKindDelegate]
	if !ok || kind.apply == nil {
		t.Fatal("no governance kind row for a held portal registration")
	}
	if kind.approver.reason != authz.ReasonAdminSurface || kind.approver.reason == securityApprover.reason {
		t.Errorf("approver reason = %q, want the operator tier's %q", kind.approver.reason, authz.ReasonAdminSurface)
	}
	if got := kind.forChange(types.GovernanceChange{TargetKind: govKindDelegate}).approver.reason; got != authz.ReasonAdminSurface {
		t.Errorf("approver for a change = %q, want the operator tier", got)
	}
	if govKindDelegate != "delegate" {
		t.Errorf("kind = %q", govKindDelegate)
	}
}

func TestPortalRegistration_AccessProfileNeedsAudienceAndScopes(t *testing.T) {
	e := newRegEnv(t)
	for name, mut := range map[string]map[string]any{
		"no audience":     {"audience": nil},
		"blank audience":  {"audience": "  "},
		"no scopes":       {"required_scopes": nil},
		"empty scopes":    {"required_scopes": []string{}},
		"blank scope":     {"required_scopes": []string{"a", " "}},
		"scope with room": {"required_scopes": []string{"a b"}},
		"no client id":    {"idp_client_id": ""},
	} {
		e.wantRefusal(t, name, regBody(mut), 422, reasonPortalRegistrationInvalid)
	}
	e.wantRefusal(t, "profile unknown", regBody(map[string]any{"token_profile": "jwt"}), 422, reasonDelegateTokenProfileInvalid)
	e.wantRefusal(t, "no profile", regBody(map[string]any{"token_profile": nil}), 422, reasonDelegateTokenProfileInvalid)
	// An ID-token portal needs neither, and refuses them rather than ignoring them.
	id := map[string]any{"token_profile": "okta_id_token", "audience": nil, "required_scopes": nil}
	e.wantCreated(t, "ID-token portal", regBody(id))
	e.wantRefusal(t, "ID-token with audience", regBody(map[string]any{"token_profile": "okta_id_token", "required_scopes": nil}), 422, reasonDelegateAccessFieldsNeedAccess)
	e.wantCreated(t, "access portal", regBody(map[string]any{"issuer": "https://idp9.example.com"}))
}

func TestPortalRegistration_EmailDomainsNeedIDToken(t *testing.T) {
	e := newRegEnv(t)
	e.wantRefusal(t, "access token", regBody(map[string]any{"email_domains": []string{"corp.example"}}), 422, reasonDelegateEmailDomainsNeedIDToken)
	id := map[string]any{"token_profile": "okta_id_token", "audience": nil, "required_scopes": nil}
	id["email_domains"] = []string{" Corp.Example ", "corp.example", "b.example"}
	d := e.wantCreated(t, "ID token", regBody(id))
	if strings.Join(d.EmailDomains, ",") != "corp.example,b.example" {
		t.Errorf("email domains = %v, want folded and deduplicated", d.EmailDomains)
	}
	id["email_domains"] = []string{"a@b.example"}
	e.wantRefusal(t, "with an @", regBody(id), 422, reasonDelegateEmailDomainInvalid)
}

func TestPortalRegistration_MaxAgeBounds(t *testing.T) {
	e := newRegEnv(t)
	for _, v := range []int{299, 601, 0, -1} {
		e.wantRefusal(t, strconv.Itoa(v), regBody(map[string]any{"max_subject_token_age_seconds": v}), 422, reasonDelegateMaxAgeInvalid)
	}
	if d := e.wantCreated(t, "absent", regBody(nil)); d.MaxSubjectTokenAgeSeconds != 300 || d.GrantTTLSeconds != 300 {
		t.Errorf("absent = %d/%d, want 300/300", d.MaxSubjectTokenAgeSeconds, d.GrantTTLSeconds)
	}
	for _, v := range []int{300, 600} {
		if d := e.wantCreated(t, strconv.Itoa(v), regBody(map[string]any{"max_subject_token_age_seconds": v})); d.MaxSubjectTokenAgeSeconds != v {
			t.Errorf("max age %d stored as %d", v, d.MaxSubjectTokenAgeSeconds)
		}
	}
}

func TestPortalRegistration_GrantTTLBounds(t *testing.T) {
	e := newRegEnv(t)
	for _, v := range []int{299, 0} {
		e.wantRefusal(t, strconv.Itoa(v), regBody(map[string]any{"grant_ttl_seconds": v}), 422, reasonDelegateGrantTTLInvalid)
	}
	e.wantRefusal(t, "above max age", regBody(map[string]any{"grant_ttl_seconds": 301}), 422, reasonDelegateGrantTTLAboveMaxAge)
	e.wantRefusal(t, "above a larger max age", regBody(map[string]any{"max_subject_token_age_seconds": 450, "grant_ttl_seconds": 451}), 422, reasonDelegateGrantTTLAboveMaxAge)
	if d := e.wantCreated(t, "absent", regBody(map[string]any{"max_subject_token_age_seconds": 450})); d.GrantTTLSeconds != 450 {
		t.Errorf("absent grant ttl = %d, want the max age 450", d.GrantTTLSeconds)
	}
	if d := e.wantCreated(t, "shorter", regBody(map[string]any{"max_subject_token_age_seconds": 600, "grant_ttl_seconds": 300})); d.GrantTTLSeconds != 300 {
		t.Errorf("grant ttl = %d, want 300", d.GrantTTLSeconds)
	}
}

func TestPortalRegistration_PortalManagedFlagRefusedWithGroupOrConsole(t *testing.T) {
	e := newRegEnv(t)
	e.wantRefusal(t, "group and flag", regBody(map[string]any{"group": "staff"}), 422, reasonDelegatePortalManagedWithGroup)
	e.wantRefusal(t, "no group, no flag", regBody(map[string]any{"portal_managed_admission": nil}), 422, reasonDelegateGroupOrPortalManaged)
	e.wantRefusal(t, "console portal", `{"name":"p","idp_client_id":"c","group":"g","portal_managed_admission":true}`, 422, reasonDelegatePortalManagedNeedsIssuer)
	e.wantRefusal(t, "group not ASCII", regBody(map[string]any{"group": "grüppe", "portal_managed_admission": nil}), 422, reasonDelegateGroupInvalid)
	if d := e.wantCreated(t, "group only", regBody(map[string]any{"group": " Staff ", "portal_managed_admission": nil})); d.Group != "staff" || d.PortalManagedAdmission {
		t.Errorf("group portal = %+v", d)
	}
}

func TestPortalRegistration_ClaimNamesValidated(t *testing.T) {
	e := newRegEnv(t)
	claim := func(c, v string) string {
		return regBody(map[string]any{"required_claim": map[string]string{"claim": c, "value": v}, "groups_claim": "memberships"})
	}
	for _, c := range []string{"iss", "sub", "aud", "exp", "iat", "nbf", "jti", "cid", "uid", "scp", "scope", "client_id", "azp", "auth_time", "ver", "groups", "memberships"} {
		e.wantRefusal(t, "reserved "+c, claim(c, "x"), 422, reasonDelegateClaimReserved)
	}
	for _, c := range []string{"a b", "a/b", "a$", strings.Repeat("c", 65), "tenänt"} {
		e.wantRefusal(t, "bad name "+c, claim(c, "x"), 422, reasonDelegateClaimInvalid)
	}
	e.wantRefusal(t, "claim only", claim("tenant", ""), 422, reasonDelegateClaimIncomplete)
	e.wantRefusal(t, "value only", claim("", "prod"), 422, reasonDelegateClaimIncomplete)
	e.wantRefusal(t, "empty pair", regBody(map[string]any{"required_claim": map[string]string{}}), 422, reasonDelegateClaimIncomplete)
	e.wantRefusal(t, "control value", claim("tenant", "a\nb"), 422, reasonDelegateClaimInvalid)
	e.wantRefusal(t, "long value", claim("tenant", strings.Repeat("v", 257)), 422, reasonDelegateClaimInvalid)
	d := e.wantCreated(t, "valid", claim("Tenant_1.a:b-c", "prod env"))
	if d.RequiredClaim == nil || d.RequiredClaim.Value != "prod env" {
		t.Errorf("claim = %+v", d.RequiredClaim)
	}
}

func TestPortalRegistration_LegacyGroupStillRequired(t *testing.T) {
	e := newRegEnv(t)
	for _, body := range []string{`{"name":"p","idp_client_id":"c","group":""}`, `{"name":"p","idp_client_id":"c","group":"  "}`, `{"name":"p","idp_client_id":"c"}`} {
		e.wantRefusal(t, body, body, 422, reasonDelegateGroupInvalid)
	}
}

func TestPortalRegistration_RenewalRefusedOnConsolePortal(t *testing.T) {
	e := newRegEnv(t)
	e.wantRefusal(t, "console portal", `{"name":"p","idp_client_id":"c","group":"g","ui_session_renewal":true}`, 422, reasonDelegateRenewalNeedsIssuer)
	if d := e.wantCreated(t, "issuer portal", regBody(map[string]any{"ui_session_renewal": true})); !d.UISessionRenewal {
		t.Error("an issuer portal's renewal opt-in was not stored")
	}
	if d := e.wantCreated(t, "default", regBody(map[string]any{"issuer": "https://idp4.example.com"})); d.UISessionRenewal {
		t.Error("renewal defaults on")
	}
}

func TestPortalRegistration_RefusedBeforeActivation(t *testing.T) {
	e := newRegEnv(t)
	e.st.activated = false
	e.wantRefusal(t, "not activated", regBody(nil), 409, reasonPortalIssuersNotActivated)
	// The console path never waits for it.
	e.wantCreated(t, "console portal", `{"name":"p","idp_client_id":"c","group":"g"}`)
	e.st.activated = true
	e.wantCreated(t, "activated", regBody(nil))
}

func TestPortalRegistration_NamespaceCensusRefusesEachSource(t *testing.T) {
	e := newRegEnv(t)
	sources := []string{"agent_runs.created_by", "principal_identities.issuer", "capability_grants.subject", "run_policies.spec", "run_queue.launch", "audit_events.data"}
	for _, src := range sources {
		e.st.census = map[string]int{src: 2}
		w := e.register(t, regBody(nil))
		if w.Code != http.StatusConflict || reasonOf(t, w) != reasonDelegateNamespaceInUse {
			t.Errorf("%s: %d %s, want 409 %s", src, w.Code, w.Body.String(), reasonDelegateNamespaceInUse)
			continue
		}
		if !strings.Contains(w.Body.String(), src+" (2)") {
			t.Errorf("%s: the refusal does not name its source with a count: %s", src, w.Body.String())
		}
		for _, other := range sources {
			if other != src && strings.Contains(w.Body.String(), other) {
				t.Errorf("%s: the refusal names %s too", src, other)
			}
		}
	}
	if len(e.st.created) != 0 || len(e.st.bound) != 0 {
		t.Error("a refused registration left a portal or a binding")
	}
	// A bound key's own people are expected: the census is not read for it.
	e.st.census = map[string]int{}
	e.wantCreated(t, "first", regBody(nil))
	e.st.census = map[string]int{"agent_runs.created_by": 9}
	e.wantCreated(t, "second portal on a bound issuer", regBody(map[string]any{"name": "second"}))
}

// A caller named only in the configuration this process holds (the default policy's UI-app callers, the
// operator allowlist) is a source the stored census cannot see: a first registration of that issuer is refused
// 409 delegate_namespace_in_use, naming the source with its count and never the value, and nothing is bound.
// Another key's reference does not count, a bound key skips the census, and no reference registers.
func TestPortalRegistration_ConfiguredNamespaceReferenceRefused(t *testing.T) {
	e := newRegEnv(t)
	const issuer = "https://idp.example.com/oauth2/default"
	key := types.PortalIssuerKey(issuer)
	person := types.PortalPrincipal(key, "00uSole")
	foreign := types.PortalPrincipal(types.PortalIssuerKey("https://other.example.com"), "00uOther")

	if got := e.srv.configuredNamespaceCensus(key); len(got) != 0 {
		t.Fatalf("a clean configuration counted %v", got)
	}
	cases := []struct {
		name  string
		set   func()
		want  string
		count int
	}{
		{"default policy caller", func() {
			e.srv.cfg.DefaultPolicy.UIApps = []types.UIApp{{Name: "app", Port: 8080, Inbound: true, Callers: []string{"user:" + person}}}
		}, censusConfiguredDefaultPolicy, 1},
		{"default policy caller in another letter case", func() {
			e.srv.cfg.DefaultPolicy.UIApps = []types.UIApp{{Name: "app", Port: 8080, Inbound: true, Callers: []string{"user:" + strings.ToUpper(person), "user:" + person}}}
		}, censusConfiguredDefaultPolicy, 2},
		{"operator allowlist", func() {
			e.srv.cfg.OperatorEmails = []string{"ops@corp.example", " " + strings.ToUpper(person)}
		}, censusConfiguredOperatorEmails, 1},
	}
	for _, c := range cases {
		e.srv.cfg.DefaultPolicy.UIApps, e.srv.cfg.OperatorEmails = nil, nil
		c.set()
		w := e.register(t, regBody(nil))
		if w.Code != http.StatusConflict || reasonOf(t, w) != reasonDelegateNamespaceInUse {
			t.Errorf("%s: %d %s, want 409 %s", c.name, w.Code, w.Body.String(), reasonDelegateNamespaceInUse)
			continue
		}
		if want := fmt.Sprintf("%s (%d)", c.want, c.count); !strings.Contains(w.Body.String(), want) {
			t.Errorf("%s: the refusal does not name %q: %s", c.name, want, w.Body.String())
		}
		if strings.Contains(strings.ToLower(w.Body.String()), strings.ToLower(person)) || strings.Contains(w.Body.String(), "00uSole") {
			t.Errorf("%s: the refusal carries the value: %s", c.name, w.Body.String())
		}
	}
	if len(e.st.created) != 0 || len(e.st.bound) != 0 {
		t.Fatal("a refused registration left a portal or a binding")
	}
	// Another key's reference is that key's orphan, not this one's.
	e.srv.cfg.DefaultPolicy.UIApps = []types.UIApp{{Name: "app", Port: 8080, Inbound: true, Callers: []string{"user:" + foreign}}}
	e.srv.cfg.OperatorEmails = nil
	e.wantCreated(t, "control", regBody(nil))
	// The key is bound now: its own people may be named anywhere, and a second portal registers.
	e.srv.cfg.DefaultPolicy.UIApps = []types.UIApp{{Name: "app", Port: 8080, Inbound: true, Callers: []string{"user:" + person}}}
	e.wantCreated(t, "second portal on a bound issuer", regBody(map[string]any{"name": "second"}))
}
