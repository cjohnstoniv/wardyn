// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	gooidctest "github.com/coreos/go-oidc/v3/oidc/oidctest"
	"github.com/google/uuid"
	"golang.org/x/oauth2"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const (
	entraTenant = "0b1c2d3e-4f50-6172-8394-a5b6c7d8e9f0"
	entraObject = "9a8b7c6d-5e4f-3a2b-1c0d-ef0123456789"
	entraPerson = "entra:" + entraTenant + ":" + entraObject
)

// entraPeoplePG is peoplePG behind an authenticator whose issuer IS Entra ID
// (https://login.microsoftonline.com/<tid>/v2.0), reached over a loopback TLS
// fake through the split-horizon issuer rewrite, so the REAL sign-in callback
// runs against the real person store. signIn sets the next id_token the fake
// token endpoint returns. OnLogin does what cmd/wardynd's refreshLoginStamps
// does with the api-token and person stamps.
type entraPeoplePG struct {
	peoplePG
	issuer string
	sign   func(claims map[string]any) string
	signIn func(sub, tid, oid string)
}

func newEntraPeoplePG(t *testing.T) entraPeoplePG {
	t.Helper()
	e := newPeoplePG(t)
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	prefix := "/" + entraTenant + "/v2.0"
	issuer := "https://login.microsoftonline.com" + prefix
	idp := &gooidctest.Server{PublicKeys: []gooidctest.PublicKey{{PublicKey: priv.Public(), KeyID: "entra-key", Algorithm: "RS256"}}}
	idp.SetIssuer(issuer)
	var idToken string
	mux := http.NewServeMux()
	mux.HandleFunc(prefix+"/token", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "at", "token_type": "Bearer", "expires_in": 3600, "id_token": idToken})
	})
	mux.Handle(prefix+"/", http.StripPrefix(prefix, idp))
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)

	st := e.st
	auth, err := oidc.New(context.WithValue(context.Background(), oauth2.HTTPClient, srv.Client()), oidc.Config{
		IssuerURL: issuer, InternalIssuerURL: srv.URL + prefix,
		ClientID: "wardyn-client", ClientSecret: "secret", RedirectURL: "http://localhost/auth/callback",
		RoleMap: map[string]string{
			"root@corp.example": oidc.RoleAdmin, "sec@corp.example": oidc.RoleSecurityAdmin,
		},
		DefaultRole: oidc.RoleUser,
		OnLogin: func(ctx context.Context, sub, role, userType string, groups []string, truncated bool) {
			if err := st.RefreshAPITokenIdentity(ctx, sub, role, userType, groups, truncated); err != nil {
				t.Error(err)
			}
			if err := st.MarkPersonSignedIn(ctx, sub, time.Now().UTC()); err != nil {
				t.Error(err)
			}
		},
	}, accessTestHMACKey)
	if err != nil {
		t.Fatalf("oidc.New against the Entra-issuer fake: %v", err)
	}
	e.h.srv.cfg.OIDC = auth
	e.h.srv.router = e.h.srv.routes()
	e.super = accessSession(t, "root", "root@corp.example", oidc.RoleAdmin, []string{})
	e.sec = accessSession(t, "sec", "sec@corp.example", oidc.RoleSecurityAdmin, []string{})
	sign := func(claims map[string]any) string {
		raw, _ := json.Marshal(claims)
		return gooidctest.SignIDToken(priv, "entra-key", "RS256", string(raw))
	}
	return entraPeoplePG{peoplePG: e, issuer: issuer, sign: sign, signIn: func(sub, tid, oid string) {
		idToken = sign(map[string]any{
			"iss": issuer, "sub": sub, "aud": "wardyn-client", "nonce": "entra-nonce",
			"email": personEmail, "groups": []string{"eng"}, "tid": tid, "oid": oid,
			"iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix(),
		})
	}}
}

// callback drives GET /auth/callback for the id_token signIn set, returning
// the response and the session cookie it issued (nil when refused).
func (e entraPeoplePG) callback(t *testing.T) (*httptest.ResponseRecorder, *http.Cookie) {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/auth/callback?state=s1&code=c1", nil)
	r.AddCookie(&http.Cookie{Name: "wardyn_oidc_state", Value: "s1"})
	r.AddCookie(&http.Cookie{Name: "wardyn_oidc_nonce", Value: "entra-nonce"})
	r.AddCookie(&http.Cookie{Name: "wardyn_oidc_pkce", Value: "v1"})
	w := httptest.NewRecorder()
	panicFails(t, e.h.srv.Handler()).ServeHTTP(w, r)
	for _, c := range w.Result().Cookies() {
		if c.Name == "wardyn_session" && c.Value != "" {
			return w, c
		}
	}
	return w, nil
}

func (e entraPeoplePG) createEntraPerson(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	return doSSO(t, e.h.srv, http.MethodPost, "/api/v1/people", e.sec, body)
}

// seesToken reports whether a session's own token list (/me/tokens) holds id:
// it only ever lists the caller's own.
func (e entraPeoplePG) seesToken(t *testing.T, sess *http.Cookie, id string) bool {
	t.Helper()
	w := doSSO(t, e.h.srv, http.MethodGet, "/api/v1/me/tokens", sess, "")
	if w.Code != http.StatusOK {
		t.Fatalf("/me/tokens: %d %s", w.Code, w.Body.String())
	}
	return strings.Contains(w.Body.String(), id)
}

// TestPeopleEntra_AttachesOnlyOnExactObjectID is #1195 end to end through the
// real callback: an Entra person set up by tenant and object id is attached by
// exactly (issuer, tid, oid) and by nothing else — not their email, not their
// object id in another tenant, not a sub spelling their principal — and until
// that attach their minted token keeps its unknown-groups stamp and the row
// stays unsigned. The attach is audited naming both parties, and a later
// sign-in under a different pairwise sub (the app re-registered) is still them.
func TestPeopleEntra_AttachesOnlyOnExactObjectID(t *testing.T) {
	e := newEntraPeoplePG(t)
	ctx := context.Background()
	w := e.createEntraPerson(t, `{"tenant_id":"`+entraTenant+`","object_id":"`+entraObject+`","email":"`+personEmail+`"}`)
	var p types.Person
	if w.Code != http.StatusCreated || json.Unmarshal(w.Body.Bytes(), &p) != nil ||
		p.Principal != entraPerson || p.Issuer != e.issuer || p.TenantID != entraTenant || p.ObjectID != entraObject {
		t.Fatalf("create by object id: %d %s, want 201 keyed %s under %s", w.Code, w.Body.String(), entraPerson, e.issuer)
	}
	tok := decodeToken(t, e.mintFor(t, e.sec, entraPerson))
	untouched := func(when string) {
		t.Helper()
		got, err := e.st.GetPerson(ctx, entraPerson)
		if err != nil || got.FirstSignedInAt != nil {
			t.Errorf("%s: person = %+v (%v), want still unsigned", when, got, err)
		}
		toks, err := e.st.ListAPITokensByPrincipal(ctx, entraPerson)
		if err != nil || len(toks) != 1 || toks[0].GroupsTruncated == nil || !*toks[0].GroupsTruncated {
			t.Errorf("%s: tokens = %+v (%v), want the unknown-groups stamp kept", when, toks, err)
		}
		if n := len(e.auditRows("person.attach")); n != 0 {
			t.Errorf("%s: %d person.attach rows, want none", when, n)
		}
	}

	for _, c := range []struct{ name, sub, tid, oid string }{
		{"same email, another object id", "pairwise-b", entraTenant, "9a8b7c6d-5e4f-3a2b-1c0d-000000000000"},
		{"same object id, another tenant", "pairwise-c", "0b1c2d3e-4f50-6172-8394-000000000000", entraObject},
		{"no tid or oid claims", "pairwise-d", "", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			e.signIn(c.sub, c.tid, c.oid)
			w, sess := e.callback(t)
			if sess == nil {
				t.Fatalf("sign-in refused: %d %s", w.Code, w.Header().Get("Location"))
			}
			if e.seesToken(t, sess, tok.ID.String()) {
				t.Errorf("a non-matching sign-in sees the person's token")
			}
			untouched(c.name)
		})
	}
	for _, c := range []struct{ name, sub string }{
		{"a sub spelling the principal, another object id", entraPerson},
		{"the principal's sub in another case", strings.ToUpper(entraPerson)},
		{"a reserved sub carrying the exact key", adminTokenPrincipal},
	} {
		t.Run(c.name, func(t *testing.T) {
			oid := "9a8b7c6d-5e4f-3a2b-1c0d-000000000000"
			if c.sub == adminTokenPrincipal {
				oid = entraObject
			}
			e.signIn(c.sub, entraTenant, oid)
			w, sess := e.callback(t)
			if sess != nil || !strings.Contains(w.Header().Get("Location"), "auth_error=sign_in_refused") {
				t.Fatalf("sign-in as %q = %d %s, want refused", c.sub, w.Code, w.Header().Get("Location"))
			}
			untouched(c.name)
		})
	}

	for i, sub := range []string{"pairwise-a", "pairwise-after-reregistration"} {
		e.signIn(sub, entraTenant, entraObject)
		_, sess := e.callback(t)
		if sess == nil || !e.seesToken(t, sess, tok.ID.String()) {
			t.Fatalf("exact-key sign-in as %s: session %v, want the person with their token", sub, sess)
		}
		rows := e.auditRows("person.attach")
		if len(rows) != i+1 || rows[i].Actor != entraPerson || rows[i].Target != entraPerson ||
			!strings.Contains(string(rows[i].Data), `"sub":"`+sub+`"`) || !strings.Contains(string(rows[i].Data), `"object_id":"`+entraObject+`"`) {
			t.Fatalf("person.attach rows = %+v, want row %d naming the person and sub %s", rows, i, sub)
		}
	}
	if got, err := e.st.GetPerson(ctx, entraPerson); err != nil || got.FirstSignedInAt == nil {
		t.Errorf("person after the exact-key sign-in = %+v (%v), want first_signed_in_at stamped", got, err)
	}
	if toks, err := e.st.ListAPITokensByPrincipal(ctx, entraPerson); err != nil || len(toks) != 1 || toks[0].GroupsTruncated == nil || *toks[0].GroupsTruncated {
		t.Errorf("tokens after the exact-key sign-in = %+v (%v), want re-stamped from the sign-in's groups", toks, err)
	}
}

// TestPeopleEntra_CreateRules: on Entra, the object-id form takes exactly a
// GUID tenant and object id and no principal; it confirms the same key in any
// case, refuses one recorded under another issuer, and a principal with no
// person record mints nothing. Off Entra the object-id form is refused, so a
// non-Entra deployment keys people exactly as before.
func TestPeopleEntra_CreateRules(t *testing.T) {
	e := newEntraPeoplePG(t)
	key := `"tenant_id":"` + entraTenant + `","object_id":"` + entraObject + `"`
	if w := e.createEntraPerson(t, `{`+key+`}`); w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	if w := e.createEntraPerson(t, `{"tenant_id":"`+strings.ToUpper(entraTenant)+`","object_id":"`+entraObject+`"}`); w.Code != http.StatusOK {
		t.Errorf("confirm in upper case: %d %s, want 200", w.Code, w.Body.String())
	}
	for name, body := range map[string]string{
		"principal and object id":  `{"principal":"pat-sub",` + key + `}`,
		"tenant only":              `{"tenant_id":"` + entraTenant + `"}`,
		"object id not a GUID":     `{"tenant_id":"` + entraTenant + `","object_id":"pat@corp.example"}`,
		"braced GUID":              `{"tenant_id":"{` + entraTenant + `}","object_id":"` + entraObject + `"}`,
		"urn GUID is not 36 chars": `{"tenant_id":"urn:uuid:` + entraTenant + `","object_id":"` + entraObject + `"}`,
	} {
		if w := e.createEntraPerson(t, body); w.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s: %d %s, want 422", name, w.Code, w.Body.String())
		}
	}
	// The entra: namespace as a plain subject: no sign-in could ever become it.
	for _, p := range []string{"entra:" + entraTenant + ":9a8b7c6d-5e4f-3a2b-1c0d-000000000000", "ENTRA:X:Y"} {
		if w := e.createEntraPerson(t, `{"principal":"`+p+`"}`); w.Code != http.StatusUnprocessableEntity || errorReason(w) != "person_principal_reserved" {
			t.Errorf("plain principal %q: %d %s, want 422 person_principal_reserved", p, w.Code, w.Body.String())
		}
	}
	other := "0b1c2d3e-4f50-6172-8394-111111111111"
	if _, _, err := e.st.CreatePerson(context.Background(), types.Person{
		Principal: "entra:" + other + ":" + entraObject, Issuer: "https://sts.windows.net/" + other + "/",
		TenantID: other, ObjectID: entraObject, CreatedBy: "root",
	}); err != nil {
		t.Fatal(err)
	}
	if w := e.createEntraPerson(t, `{"tenant_id":"`+other+`","object_id":"`+entraObject+`"}`); w.Code != http.StatusConflict {
		t.Errorf("same key under another issuer: %d %s, want 409", w.Code, w.Body.String())
	}
	// A sign-in carrying that row's tid and oid, from this deployment's issuer.
	e.signIn("pairwise-x", other, entraObject)
	if _, sess := e.callback(t); sess == nil || len(e.auditRows("person.attach")) != 0 {
		t.Errorf("tid and oid equal, issuer not: session %v, attaches %d; want its own sub, none", sess, len(e.auditRows("person.attach")))
	}
	if w := e.mintFor(t, e.sec, "entra:"+entraTenant+":9a8b7c6d-5e4f-3a2b-1c0d-000000000000"); w.Code != http.StatusNotFound {
		t.Errorf("mint for an object id with no person: %d, want 404", w.Code)
	}

	plain := newPeoplePG(t)
	if plain.h.srv.cfg.OIDC.KeysPeopleByObjectID() {
		t.Fatal("a non-Entra issuer keys people by object id")
	}
	if w := doSSO(t, plain.h.srv, http.MethodPost, "/api/v1/people", plain.sec, `{`+key+`}`); w.Code != http.StatusUnprocessableEntity {
		t.Errorf("object-id form off Entra: %d %s, want 422", w.Code, w.Body.String())
	}
}

// TestPeopleEntra_KnownSubKeepsItsPrincipal: an object-id person created for
// someone who has ALREADY signed in, and is known here under their pairwise
// sub, never re-keys them. Their next sign-in keeps the sub and everything it
// owns, writes no attach, and records the ignored match as a denied
// person.attach naming both.
func TestPeopleEntra_KnownSubKeepsItsPrincipal(t *testing.T) {
	for _, c := range []struct {
		name string
		// known makes pairwise-a known from its first session and returns the
		// check its second session must pass.
		known func(t *testing.T, e entraPeoplePG, first *http.Cookie) func(second *http.Cookie) bool
	}{
		{"confirmed by sub, with a minted token", func(t *testing.T, e entraPeoplePG, _ *http.Cookie) func(*http.Cookie) bool {
			if w := e.createPerson(t, e.sec, "pairwise-a", ""); w.Code != http.StatusCreated {
				t.Fatalf("confirm by sub: %d %s", w.Code, w.Body.String())
			}
			tok := decodeToken(t, e.mintFor(t, e.sec, "pairwise-a"))
			return func(second *http.Cookie) bool { return e.seesToken(t, second, tok.ID.String()) }
		}},
		{"confirmed by sub only", func(t *testing.T, e entraPeoplePG, _ *http.Cookie) func(*http.Cookie) bool {
			if w := e.createPerson(t, e.sec, "pairwise-a", ""); w.Code != http.StatusCreated {
				t.Fatalf("confirm by sub: %d %s", w.Code, w.Body.String())
			}
			return func(second *http.Cookie) bool {
				return decodeToken(t, doSSO(t, e.h.srv, http.MethodPost, "/api/v1/me/tokens", second, `{"name":"who"}`)).Principal == "pairwise-a"
			}
		}},
		{"their own API token", func(t *testing.T, e entraPeoplePG, first *http.Cookie) func(*http.Cookie) bool {
			w := doSSO(t, e.h.srv, http.MethodPost, "/api/v1/me/tokens", first, `{"name":"mine"}`)
			tok := decodeToken(t, w)
			return func(second *http.Cookie) bool { return e.seesToken(t, second, tok.ID.String()) }
		}},
		{"a stored secret only", func(t *testing.T, e entraPeoplePG, first *http.Cookie) func(*http.Cookie) bool {
			if w := doSSO(t, e.h.srv, http.MethodPut, "/api/v1/secrets/my-key", first, `{"value":"pairwise-a-own-value"}`); w.Code/100 != 2 {
				t.Fatalf("put own secret: %d %s", w.Code, w.Body.String())
			}
			return func(second *http.Cookie) bool {
				w := doSSO(t, e.h.srv, http.MethodGet, "/api/v1/secrets", second, "")
				return w.Code == http.StatusOK && strings.Contains(w.Body.String(), `"my-key"`)
			}
		}},
		{"an SSH key only", func(t *testing.T, e entraPeoplePG, first *http.Cookie) func(*http.Cookie) bool {
			if w := doSSO(t, e.h.srv, http.MethodPost, "/api/v1/me/ssh-keys", first, featureTestKey); w.Code != http.StatusCreated {
				t.Fatalf("add own ssh key: %d %s", w.Code, w.Body.String())
			}
			return func(second *http.Cookie) bool {
				return strings.Contains(doSSO(t, e.h.srv, http.MethodGet, "/api/v1/me/ssh-keys", second, "").Body.String(), `"laptop"`)
			}
		}},
		{"a workspace only", func(t *testing.T, e entraPeoplePG, _ *http.Cookie) func(*http.Cookie) bool {
			ws, err := e.st.CreateWorkspace(context.Background(), types.Workspace{
				ID: uuid.New(), Name: "ws-a", OwnedBy: "pairwise-a", Status: types.WorkspaceScanned,
				Sources:   []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeEphemeral, Target: "/home/agent/work"}},
				CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
			})
			if err != nil {
				t.Fatal(err)
			}
			return func(second *http.Cookie) bool {
				return doSSO(t, e.h.srv, http.MethodGet, "/api/v1/workspaces/"+ws.ID.String(), second, "").Code == http.StatusOK
			}
		}},
		{"a run", func(t *testing.T, e entraPeoplePG, first *http.Cookie) func(*http.Cookie) bool {
			var run createRunResponse
			if err := json.Unmarshal(mustCreate(t, doSSO(t, e.h.srv, http.MethodPost, "/api/v1/runs", first, `{"agent":"claude-code","task":"t"}`)).Body.Bytes(), &run); err != nil {
				t.Fatal(err)
			}
			return func(second *http.Cookie) bool {
				return doSSO(t, e.h.srv, http.MethodGet, "/api/v1/runs/"+run.ID.String(), second, "").Code == http.StatusOK
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newEntraPeoplePG(t)
			e.signIn("pairwise-a", entraTenant, entraObject)
			_, first := e.callback(t)
			if first == nil {
				t.Fatal("first sign-in refused")
			}
			check := c.known(t, e, first)
			if w := e.createEntraPerson(t, `{"tenant_id":"`+entraTenant+`","object_id":"`+entraObject+`"}`); w.Code != http.StatusCreated {
				t.Fatalf("object-id create: %d %s", w.Code, w.Body.String())
			}
			e.signIn("pairwise-a", entraTenant, entraObject)
			_, second := e.callback(t)
			if second == nil || !check(second) {
				t.Fatalf("second sign-in: session %v, want still pairwise-a with what it owns", second)
			}
			rows := e.auditRows("person.attach")
			if len(rows) != 1 || rows[0].Outcome != "denied" || rows[0].Actor != "pairwise-a" || rows[0].Target != entraPerson ||
				!strings.Contains(string(rows[0].Data), `"reason":"sub_known"`) {
				t.Fatalf("person.attach rows = %+v, want one denied sub_known naming pairwise-a and the person", rows)
			}
			if p, err := e.st.GetPerson(context.Background(), entraPerson); err != nil || p.FirstSignedInAt != nil {
				t.Errorf("object-id person = %+v (%v), want never signed in", p, err)
			}
		})
	}
}

// TestPeopleEntra_ExchangeRecordsAttachOnlyWhenAdmitted: a portal's token
// exchange for an object-id person writes person.attach only once it mints;
// its own later refusal (outside the portal's group) writes none.
func TestPeopleEntra_ExchangeRecordsAttachOnlyWhenAdmitted(t *testing.T) {
	e := newEntraPeoplePG(t)
	if w := e.createEntraPerson(t, `{"tenant_id":"`+entraTenant+`","object_id":"`+entraObject+`","email":"`+personEmail+`"}`); w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	w := doSSO(t, e.h.srv, http.MethodPost, "/api/v1/admin/delegates", e.super,
		`{"name":"front end","idp_client_id":"`+delegPortalClient+`","group":"portal-users"}`)
	var d types.Delegate
	if w.Code != http.StatusCreated || json.Unmarshal(w.Body.Bytes(), &d) != nil {
		t.Fatalf("register portal: %d %s", w.Code, w.Body.String())
	}
	exchange := func(group string) int {
		subject := e.sign(map[string]any{
			"iss": e.issuer, "sub": "pairwise-a", "aud": "wardyn-client", "azp": delegPortalClient,
			"email": personEmail, "groups": []string{group}, "tid": entraTenant, "oid": entraObject,
			"iat": time.Now().Unix(), "exp": time.Now().Add(5 * time.Minute).Unix(),
		})
		form := url.Values{"grant_type": {grantTypeTokenExchange}, "subject_token": {subject}, "subject_token_type": {tokenTypeAccessToken}}
		r := httptest.NewRequest(http.MethodPost, "/api/v1/token", strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.SetBasicAuth(d.ID.String(), d.Credential)
		w := httptest.NewRecorder()
		panicFails(t, e.h.srv.Handler()).ServeHTTP(w, r)
		return w.Code
	}
	if code := exchange("eng"); code != http.StatusForbidden || len(e.auditRows("person.attach")) != 0 {
		t.Fatalf("exchange outside the portal's group: %d, %d person.attach rows; want 403 and none", code, len(e.auditRows("person.attach")))
	}
	if code := exchange("portal-users"); code != http.StatusOK || len(e.auditRows("person.attach")) != 1 {
		t.Fatalf("admitted exchange: %d, %d person.attach rows; want 200 and one", code, len(e.auditRows("person.attach")))
	}
}
