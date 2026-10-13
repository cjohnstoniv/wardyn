// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// bearerShaped is design section 8's definition of a credential-looking string: a string starting wdn_, wdg_,
// wdp_, wdr_, wdd_ or wde_. An attach ticket (the "ticket" key) and a Set-Cookie value are the other two shapes.
var bearerShaped = regexp.MustCompile(`\b(?:wdn|wdg|wdp|wdr|wdd|wde)_[0-9A-Za-z_-]{8,}`)

var runInPath = regexp.MustCompile(`/runs/([0-9a-f-]{36})/`)

// walkCaller is one of the three people the walk signs in as.
type walkCaller struct {
	name   string // "super", "security" or "member"
	sub    string // the principal their session carries
	cookie *http.Cookie
}

// foundCredential is one bearer-shaped value a route handed back.
type foundCredential struct {
	kind   string // "bearer", "ticket" or "cookie"
	value  string
	cookie *http.Cookie
	body   []byte // the response it came out of, for a door that needs more than the secret (a portal's id)
	runID  uuid.UUID
}

// scanCredentials finds every bearer-shaped value in a response: the prefixed strings anywhere in the body, any
// "ticket" string, and every Set-Cookie value.
func scanCredentials(w *httptest.ResponseRecorder, path string) []foundCredential {
	var out []foundCredential
	body := w.Body.Bytes()
	for _, m := range bearerShaped.FindAllString(string(body), -1) {
		out = append(out, foundCredential{kind: "bearer", value: m, body: body})
	}
	var doc any
	if json.Unmarshal(body, &doc) == nil {
		var visit func(v any)
		visit = func(v any) {
			switch x := v.(type) {
			case map[string]any:
				for k, child := range x {
					if s, ok := child.(string); ok && k == "ticket" && s != "" {
						fc := foundCredential{kind: "ticket", value: s}
						if m := runInPath.FindStringSubmatch(path); m != nil {
							fc.runID, _ = uuid.Parse(m[1])
						}
						out = append(out, fc)
					}
					visit(child)
				}
			case []any:
				for _, child := range x {
					visit(child)
				}
			}
		}
		visit(doc)
	}
	for _, c := range w.Result().Cookies() {
		if c.Value != "" {
			out = append(out, foundCredential{kind: "cookie", value: c.Value, cookie: &http.Cookie{Name: c.Name, Value: c.Value}})
		}
	}
	return out
}

// walkMintRoutes are the routes that hand a credential back. They run first, so a route that rewrites site
// configuration later in the walk cannot make them refuse for a reason that is not the one under test.
var walkMintRoutes = []string{
	"POST /api/v1/me/tokens",
	"POST /api/v1/me/invitations/{id}/claim",
	"POST /api/v1/me/service-identities/{name}/tokens",
	"POST /api/v1/runs/{id}/attach-ticket",
	"POST /api/v1/runs/{id}/attach/ticket",
	"POST /api/v1/admin/delegates",
	"POST /api/v1/admin/devices/enrolment-tokens",
	"POST /api/v1/runners/tokens",
	"POST /api/v1/me/runners/tokens",
	"POST /api/v1/people/{principal}/tokens",
}

// walkMustYield is the proof the walk is not vacuous: each of these routes has to have handed the caller a
// credential, which then reached its door. A route that stops yielding for a reason of the walk's own setup would
// otherwise pass by finding nothing.
var walkMustYield = map[string][]string{
	"POST /api/v1/me/tokens":                           {"super", "security", "member"},
	"POST /api/v1/me/invitations/{id}/claim":           {"super", "security", "member"},
	"POST /api/v1/me/service-identities/{name}/tokens": {"super", "security", "member"},
	"POST /api/v1/runs/{id}/attach-ticket":             {"super", "security", "member"},
	"POST /api/v1/runs/{id}/attach/ticket":             {"super", "security", "member"},
	"POST /api/v1/admin/delegates":                     {"super"},
	"POST /api/v1/admin/devices/enrolment-tokens":      {"super"},
	"POST /api/v1/runners/tokens":                      {"super"},
	"POST /api/v1/me/runners/tokens":                   {"super", "security", "member"},
}

type credentialWalk struct {
	probeEnv
	callers     []walkCaller
	invitations map[string]uuid.UUID // by caller: a pending invitation addressed to them
	server      *httptest.Server
	// reached counts, by route and caller, the credentials that were presented at their door and were
	// accepted there as the caller (or a service identity, or a machine).
	reached map[string]int
	found   map[string]int
}

func newCredentialWalk(t *testing.T) *credentialWalk {
	t.Helper()
	w := &credentialWalk{probeEnv: newProbeEnv(t), invitations: map[string]uuid.UUID{}, reached: map[string]int{}, found: map[string]int{}}
	w.callers = []walkCaller{{"super", "root", w.super}, {"security", "sec", w.sec}, {"member", "mem", w.mem}}
	for _, c := range w.callers {
		w.mustCreate(t, "walk-"+c.name, c.sub)
		inv := inviteEnv{peoplePG: w.peoplePG}
		resp := inv.invite(t, w.sec, c.sub, `{}`)
		var rec types.Invitation
		if resp.Code != http.StatusCreated || json.Unmarshal(resp.Body.Bytes(), &rec) != nil {
			t.Fatalf("invite %s: %d %s", c.sub, resp.Code, resp.Body.String())
		}
		w.invitations[c.name] = rec.ID
	}
	w.server = httptest.NewServer(panicFails(t, w.h.srv.Handler()))
	t.Cleanup(w.server.Close)
	return w
}

// pathFor fills a route pattern's parameters with real fixtures where the route is a credential mint, and with an
// unknown id elsewhere, so the walk reaches the handler without destroying what later routes need.
func (w *credentialWalk) pathFor(t *testing.T, key, pattern string, rc classifiedRoute, c walkCaller) string {
	t.Helper()
	if rc.class == classOwner && rc.entity == entityRun {
		id := w.seedRun(t, c.sub, nil).ID.String()
		return routeParamRe.ReplaceAllString(pattern, id)
	}
	return routeParamRe.ReplaceAllStringFunc(pattern, func(p string) string {
		switch {
		case p == "{principal}":
			return personSub
		case p == "{name}" && strings.Contains(pattern, "service-identities"):
			return "walk-" + c.name
		case p == "{id}" && key == "POST /api/v1/me/invitations/{id}/claim":
			return w.invitations[c.name].String()
		}
		return uuid.NewString()
	})
}

// bodyFor is the minimal valid body of each mint route (from that route's own tests); every other route gets the
// generic body TestAuthzMatrix sends.
func (w *credentialWalk) bodyFor(key string, rc classifiedRoute, c walkCaller) string {
	method, _, _ := strings.Cut(key, " ")
	switch key {
	case "POST /api/v1/me/tokens", "POST /api/v1/me/service-identities/{name}/tokens":
		return `{"name":"walk","ttl_seconds":3600}`
	case "POST /api/v1/admin/delegates":
		return `{"name":"walk-portal-` + c.name + `","idp_client_id":"walk-client-` + c.name + `","group":"walk-users"}`
	case "POST /api/v1/admin/devices/enrolment-tokens":
		return `{"name":"walk-laptop-` + c.name + `"}`
	case "POST /api/v1/runners/tokens":
		return `{"owner":"` + personSub + `"}`
	case "POST /api/v1/service-identities":
		return `{"name":"made-by-` + c.name + `","sponsor":"` + c.sub + `"}`
	case "POST /api/v1/people/{principal}/tokens":
		return `{"name":"ci"}`
	}
	return bodyFor(method, rc)
}

// meAs is GET /me for a credential: the status and the principal it answers as.
func (w *credentialWalk) meAs(t *testing.T, cookie *http.Cookie, bearer string) (int, string) {
	t.Helper()
	r := w.ask(t, cookie, bearer, http.MethodGet, "/api/v1/me", "")
	var me struct {
		Principal string `json:"principal"`
	}
	_ = json.Unmarshal(r.Body.Bytes(), &me)
	return r.Code, me.Principal
}

// present takes one credential to its own door and fails the test if it authenticates as a person other than c.
// It reports whether the credential was accepted at its door.
func (w *credentialWalk) present(t *testing.T, route string, c walkCaller, fc foundCredential) bool {
	t.Helper()
	who := route + " as " + c.name + ": " + fc.kind + " " + redact(fc.value)
	asCaller := func(principal string) bool { return principal == c.sub || strings.HasPrefix(principal, "service:") }
	switch fc.kind {
	case "cookie":
		code, principal := w.meAs(t, fc.cookie, "")
		if code == http.StatusOK && !asCaller(principal) {
			t.Errorf("LEAK %s authenticates as %q, want %q or a service:", who, principal, c.sub)
		}
		return code == http.StatusOK
	case "ticket":
		return w.presentTicket(t, who, c, fc)
	}
	// Every bearer prefix is first offered as an API bearer: only a wdn_ or wdg_ may authenticate as a person.
	code, principal := w.meAs(t, nil, fc.value)
	prefix := fc.value[:4]
	switch prefix {
	case "wdn_", "wdg_":
		if code == http.StatusOK && !asCaller(principal) {
			t.Errorf("LEAK %s authenticates as %q, want %q or a service:", who, principal, c.sub)
		}
		return code == http.StatusOK && asCaller(principal)
	}
	// wdp_, wdr_, wdd_ and wde_ name a portal, a runner, a device and a device's enrolment: none of them is a person.
	if code == http.StatusOK {
		t.Errorf("LEAK %s authenticates on the API as %q, want it refused", who, principal)
	}
	switch prefix {
	case "wdp_":
		var d types.Delegate
		_ = json.Unmarshal(fc.body, &d)
		r := httptest.NewRequest(http.MethodPost, "/api/v1/token", strings.NewReader(url.Values{
			"grant_type": {grantTypeTokenExchange}, "subject_token": {"not-a-person's-token"}, "subject_token_type": {tokenTypeAccessToken}}.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.SetBasicAuth(d.ID.String(), fc.value)
		rec := httptest.NewRecorder()
		panicFails(t, w.h.srv.Handler()).ServeHTTP(rec, r)
		if rec.Code/100 == 2 || bearerShaped.MatchString(rec.Body.String()) {
			t.Errorf("LEAK %s trades for a person's token with no person's token: %d %s", who, rec.Code, rec.Body.String())
		}
		return rec.Code == http.StatusBadRequest || rec.Code == http.StatusUnauthorized // reached the portal's door
	case "wde_":
		r := w.ask(t, nil, "", http.MethodPost, "/api/v1/devices/enrol", `{"token":"`+fc.value+`"}`)
		var enrolled types.DeviceEnrolResponse
		if r.Code != http.StatusCreated || json.Unmarshal(r.Body.Bytes(), &enrolled) != nil {
			t.Errorf("%s: the device enrolment door answered %d %s, want 201", who, r.Code, r.Body.String())
			return false
		}
		// The device credential the enrolment returns is bearer-shaped too, and no person's.
		w.present(t, route+" -> enrol", c, foundCredential{kind: "bearer", value: enrolled.Token})
		return true
	case "wdr_":
		pub, _, _ := ed25519.GenerateKey(rand.Reader)
		body, _ := json.Marshal(types.RunnerRegisterRequest{Token: fc.value, PublicKey: pub, Name: "walk-runner", Limits: testRunnerLimits()})
		r := w.ask(t, nil, "", http.MethodPost, "/api/v1/runners/register", string(body))
		if r.Code != http.StatusCreated {
			t.Errorf("%s: the runner registration door answered %d %s, want 201", who, r.Code, r.Body.String())
			return false
		}
		return true
	case "wdd_":
		return true // the device credential is only ever offered to the API above, which refused it
	}
	return false
}

// presentTicket redeems an attach ticket at the attach WebSocket and reads who the attach audit row names.
func (w *credentialWalk) presentTicket(t *testing.T, who string, c walkCaller, fc foundCredential) bool {
	t.Helper()
	if fc.runID == uuid.Nil {
		t.Errorf("%s: no run id in the route it came from", who)
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(w.server.URL, "http")+"/api/v1/runs/"+fc.runID.String()+"/attach?ticket="+fc.value, nil)
	if err != nil {
		t.Errorf("%s: the attach door refused the ticket the route returned: %v", who, err)
		return false
	}
	defer func() { _ = conn.CloseNow() }()
	ev := waitForRecAudit(t, w.h.audit, fc.runID, "session.attach", "success")
	if ev.Actor != c.sub {
		t.Errorf("LEAK %s opens a session attributed to %q, want %q", who, ev.Actor, c.sub)
	}
	return ev.Actor == c.sub
}

// redactBody keeps an answer short enough to log and drops anything bearer-shaped from it.
func redactBody(s string) string {
	s = bearerShaped.ReplaceAllStringFunc(s, redact)
	if len(s) > 120 {
		s = s[:120] + "..."
	}
	return strings.TrimSpace(s)
}

func redact(s string) string {
	if len(s) > 10 {
		return s[:8] + "..."
	}
	return s
}

// TestNoRouteReturnsAnotherPersonsCredential walks the TestAuthzMatrix route table as the super admin, a security
// admin and a member, over the real routes and stores (design section 8). Every bearer-shaped value found in a
// non-error response is presented to its own door at once, and the identity it yields must be the caller or a
// service: principal; a portal, device, runner or enrolment credential must not authenticate as anyone on the API.
func TestNoRouteReturnsAnotherPersonsCredential(t *testing.T) {
	w := newCredentialWalk(t)
	registered := map[string]bool{}
	if err := walkRoutes(w.h.srv.router, registered); err != nil {
		t.Fatal(err)
	}
	for key := range registered {
		if _, ok := routeMatrix[key]; !ok {
			t.Errorf("UNCLASSIFIED route %q: add it to routeMatrix", key)
		}
	}
	keys := make([]string, 0, len(routeMatrix))
	for key := range routeMatrix {
		if registered[key] {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	ordered := slices.Clone(walkMintRoutes)
	for _, key := range keys {
		if !slices.Contains(walkMintRoutes, key) {
			ordered = append(ordered, key)
		}
	}
	walked := 0
	for _, key := range ordered {
		rc, ok := routeMatrix[key]
		if !ok || !registered[key] {
			t.Fatalf("mint route %q is not a registered, classified route", key)
		}
		method, pattern, _ := strings.Cut(key, " ")
		for _, c := range w.callers {
			path := w.pathFor(t, key, pattern, rc, c)
			resp := w.ask(t, c.cookie, "", method, path, w.bodyFor(key, rc, c))
			walked++
			if slices.Contains(walkMintRoutes, key) {
				t.Logf("mint route %s as %s: %d %s", key, c.name, resp.Code, redactBody(resp.Body.String()))
			}
			if resp.Code >= 400 {
				continue
			}
			for _, fc := range scanCredentials(resp, path) {
				w.found[key+"|"+c.name]++
				accepted := w.present(t, key, c, fc)
				if accepted {
					w.reached[key+"|"+c.name]++
				}
				t.Logf("credential from %s as %s: %s %s, accepted at its door: %v", key, c.name, fc.kind, redact(fc.value), accepted)
			}
		}
	}
	for key, callers := range walkMustYield {
		for _, name := range callers {
			if w.found[key+"|"+name] == 0 || w.reached[key+"|"+name] == 0 {
				t.Errorf("VACUOUS %s as %s handed back %d credentials, %d reached their door; the walk must reach this mint",
					key, name, w.found[key+"|"+name], w.reached[key+"|"+name])
			}
		}
	}
	t.Logf("walked %d requests over %d routes; credentials found: %d, accepted at their door: %d",
		walked, len(ordered), sumCounts(w.found), sumCounts(w.reached))
}

func sumCounts(m map[string]int) int {
	n := 0
	for _, v := range m {
		n += v
	}
	return n
}

// walkRoutes records every "METHOD /pattern" the router registers, keyed as routeMatrix is.
func walkRoutes(r chi.Routes, into map[string]bool) error {
	return chi.Walk(r, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		into[method+" "+route] = true
		return nil
	})
}
