// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const (
	ghOwnRowID = "gh-own"
	ghOwnOrg   = "https://github.com/acme"
	// ghOwnToken is account 101 ("alice-gh"); ghOwnOtherToken account 202.
	ghOwnToken      = "github_pat_alice_0001"
	ghOwnOtherToken = "github_pat_carol_0002"
)

// ghOwnTestRow is a GitHub row where each person adds their own token.
func ghOwnTestRow() types.GitProvider {
	return types.GitProvider{ID: ghOwnRowID, Kind: types.GitProviderGitHub, BaseURLs: []string{ghOwnOrg},
		Lanes: []types.GitLane{types.GitLanePAT}, CredentialSource: types.CredentialSourcePerUser}
}

// ghOwnDoor is a member-reachable server over a site config, with GitHub's
// GET /user pointed at a fake that knows two tokens.
type ghOwnDoor struct {
	srv     *Server
	audit   *recRecorder
	secrets *memSecrets
	asked   *atomic.Int32
}

func newGHOwnDoor(t *testing.T, site types.SiteConfig, grants []types.CapabilityGrant) *ghOwnDoor {
	t.Helper()
	asked := &atomic.Int32{}
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		switch {
		case r.URL.Path != "/user":
			http.NotFound(w, r)
		case r.Header.Get("Authorization") == "Bearer "+ghOwnToken:
			_, _ = w.Write([]byte(`{"id":101,"login":"alice-gh"}`))
		case r.Header.Get("Authorization") == "Bearer "+ghOwnOtherToken:
			_, _ = w.Write([]byte(`{"id":202,"login":"carol-gh"}`))
		case r.Header.Get("Authorization") == "Bearer github_pat_ratelimited":
			w.WriteHeader(http.StatusForbidden)
		default:
			w.WriteHeader(http.StatusUnauthorized)
		}
	}))
	t.Cleanup(fake.Close)
	prev := gitHubOwnPATAPIBase
	gitHubOwnPATAPIBase = fake.URL
	t.Cleanup(func() { gitHubOwnPATAPIBase = prev })

	h := newHarness(t)
	secrets := &memSecrets{m: map[string][]byte{}}
	cfg := baseTestConfig(h, &capStore{Store: r3IntegStore{}, site: site, grants: grants})
	cfg.OIDC = &oidc.Authenticator{}
	cfg.Secrets = secrets
	cfg.MaskRegistry = secretmask.NewRegistry()
	cfg.Now = func() time.Time { return ownPATNow }
	return &ghOwnDoor{srv: New(cfg), audit: h.audit, secrets: secrets, asked: asked}
}

func (d *ghOwnDoor) put(t *testing.T, cookie *http.Cookie, req gitHubOwnPATRequest) (int, string) {
	t.Helper()
	body, _ := json.Marshal(req)
	w := doSSO(t, d.srv, http.MethodPut, "/api/v1/me/scm/github/token", cookie, string(body))
	return w.Code, w.Body.String()
}

func (d *ghOwnDoor) del(t *testing.T, cookie *http.Cookie, org string) *httptest.ResponseRecorder {
	t.Helper()
	return doSSO(t, d.srv, http.MethodDelete, "/api/v1/me/scm/github/token?org="+url.QueryEscape(org), cookie, "")
}

func (d *ghOwnDoor) stored(t *testing.T) (types.GitHubOwnToken, bool) {
	t.Helper()
	tok, found, err := d.srv.readGitHubOwnToken(context.Background(), capSub, ghOwnRowID)
	if err != nil {
		t.Fatal(err)
	}
	return tok, found
}

func (d *ghOwnDoor) rows(action string) []types.AuditEvent {
	return slices.DeleteFunc(d.audit.snapshot(), func(ev types.AuditEvent) bool { return ev.Action != action })
}

func ghOwnReq(token string, expires int) gitHubOwnPATRequest {
	return gitHubOwnPATRequest{Org: ghOwnOrg, Token: token, ExpiresOn: days(expires)}
}

// The happy path: the token is checked with GitHub, stored in the caller's own
// namespace bound to the numeric id, audited with neither the token nor the
// account, and listed back with the minimum permissions to grant.
func TestGitHubOwnPATPut_StoresTheCallersOwnToken(t *testing.T) {
	d := newGHOwnDoor(t, adoSite(ghOwnTestRow()), nil)
	code, body := d.put(t, memberCookie(t), ghOwnReq(ghOwnToken, 30))
	if code != http.StatusOK {
		t.Fatalf("PUT = %d %s", code, body)
	}
	var access GitHubOwnTokenAccess
	if err := json.Unmarshal([]byte(body), &access); err != nil {
		t.Fatal(err)
	}
	if access.State != modelAccessLive || access.Login != "alice-gh" || access.ExpiresOn != days(30) || access.MaxDays != 30 ||
		!slices.Equal(access.TokenPermissions, gitHubOwnPATPermissions) {
		t.Errorf("answer = %+v", access)
	}
	tok, found := d.stored(t)
	if !found || tok.Token != ghOwnToken || tok.AccountID != 101 {
		t.Fatalf("stored = %+v (found %v), want the token bound to account 101", tok, found)
	}
	if _, operatorHas := d.secrets.m[types.GitHubOwnTokenSecretName(ghOwnRowID)]; operatorHas {
		t.Error("the token landed in the operator namespace")
	}
	rows := d.rows(gitHubPATAuditOwnStore)
	if len(rows) != 1 || rows[0].Outcome != "success" || rows[0].Actor != capSub || !strings.Contains(string(rows[0].Data), `"link":"first"`) {
		t.Fatalf("store rows = %+v, want one success by the caller, link first", rows)
	}
	for _, ev := range d.audit.snapshot() {
		for _, leak := range []string{ghOwnToken, "alice-gh", "101"} {
			if strings.Contains(string(ev.Data), leak) || strings.Contains(ev.Target, leak) {
				t.Fatalf("audit row %s carries %q", ev.Action, leak)
			}
		}
	}
	lw := doSSO(t, d.srv, http.MethodGet, "/api/v1/me/scm/github/tokens", memberCookie(t), "")
	var list []GitHubOwnTokenAccess
	if lw.Code != http.StatusOK || json.Unmarshal(lw.Body.Bytes(), &list) != nil || len(list) != 1 || list[0].State != access.State || list[0].Login != access.Login || list[0].Org != ghOwnOrg {
		t.Fatalf("GET tokens = %d %s", lw.Code, lw.Body)
	}
}

// The token is bound to the GitHub account's numeric id: another account's
// token is refused unless the person says relink, and the same account (a
// renamed login included) replaces it.
func TestGitHubOwnPATPut_BindsTheNumericAccountID(t *testing.T) {
	d := newGHOwnDoor(t, adoSite(ghOwnTestRow()), nil)
	if code, body := d.put(t, memberCookie(t), ghOwnReq(ghOwnToken, 10)); code != http.StatusOK {
		t.Fatalf("first PUT = %d %s", code, body)
	}
	code, body := d.put(t, memberCookie(t), ghOwnReq(ghOwnOtherToken, 10))
	if code != http.StatusConflict || !strings.Contains(body, reasonGitHubOwnPATAccountMismatch) || strings.Contains(body, "carol-gh") {
		t.Fatalf("other account PUT = %d %s, want 409 account mismatch naming nobody", code, body)
	}
	if tok, _ := d.stored(t); tok.AccountID != 101 {
		t.Fatalf("stored account %d after a refused PUT, want 101 untouched", tok.AccountID)
	}
	req := ghOwnReq(ghOwnOtherToken, 10)
	req.Relink = true
	if code, body := d.put(t, memberCookie(t), req); code != http.StatusOK {
		t.Fatalf("relink PUT = %d %s", code, body)
	}
	if tok, _ := d.stored(t); tok.AccountID != 202 {
		t.Fatalf("stored account %d after relink, want 202", tok.AccountID)
	}
	if code, body := d.put(t, memberCookie(t), ghOwnReq(ghOwnOtherToken, 20)); code != http.StatusOK {
		t.Fatalf("same-account PUT = %d %s", code, body)
	}
	var links []string
	for _, ev := range d.rows(gitHubPATAuditOwnStore) {
		var data map[string]any
		_ = json.Unmarshal(ev.Data, &data)
		links = append(links, ev.Outcome+":"+fmtAny(data["link"])+fmtAny(data["reason"]))
	}
	want := []string{"success:first", "failure:" + reasonGitHubOwnPATAccountMismatch, "success:relink", "success:same_account"}
	if !slices.Equal(links, want) {
		t.Fatalf("store rows = %v, want %v", links, want)
	}
}

func fmtAny(v any) string {
	s, _ := v.(string)
	return s
}

// What the door refuses before anything is stored, and whether GitHub was
// asked: a classic or malformed token and a bad expiry never leave Wardyn.
func TestGitHubOwnPATPut_Refusals(t *testing.T) {
	cases := []struct {
		name       string
		req        gitHubOwnPATRequest
		status     int
		reason     string
		askGitHub  bool
		auditedBad bool
	}{
		{"a classic token", ghOwnReq("ghp_classic0001", 10), http.StatusBadRequest, reasonGitHubOwnPATTokenInvalid, false, false},
		{"a token with a space", ghOwnReq("github_pat_a b", 10), http.StatusBadRequest, reasonGitHubOwnPATTokenInvalid, false, false},
		{"an expiry today", ghOwnReq(ghOwnToken, 0), http.StatusBadRequest, reasonGitHubOwnPATExpiryInvalid, false, false},
		{"an expiry past 30 days", ghOwnReq(ghOwnToken, 31), http.StatusBadRequest, reasonGitHubOwnPATExpiryTooLong, false, false},
		{"a token GitHub rejects", ghOwnReq("github_pat_unknown", 10), http.StatusUnprocessableEntity, reasonGitHubOwnPATRejected, true, true},
		{"GitHub answers 403", ghOwnReq("github_pat_ratelimited", 10), http.StatusServiceUnavailable, reasonGitHubOwnPATCheckUnavailable, true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := newGHOwnDoor(t, adoSite(ghOwnTestRow()), nil)
			code, body := d.put(t, memberCookie(t), c.req)
			if code != c.status || !strings.Contains(body, c.reason) {
				t.Fatalf("PUT = %d %s, want %d %s", code, body, c.status, c.reason)
			}
			if _, found := d.stored(t); found {
				t.Fatal("a refused token was stored")
			}
			if asked := d.asked.Load() > 0; asked != c.askGitHub {
				t.Errorf("GitHub asked = %v, want %v", asked, c.askGitHub)
			}
			if bad := len(d.rows(gitHubPATAuditOwnStore)) == 1; bad != c.auditedBad {
				t.Errorf("failure audit row present = %v, want %v", bad, c.auditedBad)
			}
		})
	}
}

// SECURITY: both writing doors refuse in the user view (the value would land
// on the previewing admin), and with no browser session. Nothing is stored or
// removed, and GitHub is never asked.
func TestGitHubOwnPATDoors_RefusedInTheUserViewAndWithoutASession(t *testing.T) {
	d := newGHOwnDoor(t, adoSite(ghOwnTestRow()), nil)
	preview := memberModeSSOSession(t, capSub, capEmail, oidc.RoleAdmin, true)
	if code, body := d.put(t, preview, ghOwnReq(ghOwnToken, 10)); code != http.StatusConflict || !strings.Contains(body, reasonGitHubOwnPATPreviewBlocked) {
		t.Fatalf("PUT in the user view = %d %s, want 409 preview blocked", code, body)
	}
	if _, found := d.stored(t); found || d.asked.Load() != 0 {
		t.Fatalf("the user view stored a token (%v) or asked GitHub (%d)", found, d.asked.Load())
	}
	// Outside the view the same admin stores one; inside it, removal is refused too.
	admin := memberModeSSOSession(t, capSub, capEmail, oidc.RoleAdmin, false)
	if code, body := d.put(t, admin, ghOwnReq(ghOwnToken, 10)); code != http.StatusOK {
		t.Fatalf("PUT outside the user view = %d %s", code, body)
	}
	if w := d.del(t, preview, ghOwnOrg); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), reasonGitHubOwnPATPreviewBlocked) {
		t.Fatalf("DELETE in the user view = %d %s, want 409", w.Code, w.Body)
	}
	if _, found := d.stored(t); !found {
		t.Fatal("the user view removed the token")
	}
	w := do(t, d.srv, http.MethodPut, "/api/v1/me/scm/github/token", adminToken, `{"org":"`+ghOwnOrg+`"}`)
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), reasonGitHubOwnPATNoSession) {
		t.Fatalf("PUT with the admin token = %d %s, want 403 no session", w.Code, w.Body)
	}
}

// A row the caller may not use, a missing row and a GitHub row that is not an
// own-token row all answer the same 404, and GitHub is never asked.
func TestGitHubOwnPATDoors_OtherRowsReadAsUnknown(t *testing.T) {
	deny := []types.CapabilityGrant{grant(types.CapabilitySubjectAll, "", capWorkspaceProvider, ghOwnRowID, types.CapabilityDeny)}
	shared := ghOwnTestRow()
	shared.CredentialSource = types.CredentialSourceShared
	doors := map[string]*ghOwnDoor{
		"refused": newGHOwnDoor(t, adoSite(ghOwnTestRow()), deny),
		"missing": newGHOwnDoor(t, types.SiteConfig{}, nil),
		"shared":  newGHOwnDoor(t, adoSite(shared), nil),
	}
	var first string
	for name, d := range doors {
		code, body := d.put(t, memberCookie(t), ghOwnReq(ghOwnToken, 10))
		if code != http.StatusNotFound || (first != "" && body != first) || d.asked.Load() != 0 {
			t.Errorf("%s: PUT = %d %s (GitHub asked %d), want the one 404", name, code, body, d.asked.Load())
		}
		first = body
		if w := d.del(t, memberCookie(t), ghOwnOrg); w.Code != http.StatusNotFound {
			t.Errorf("%s: DELETE = %d, want 404", name, w.Code)
		}
		lw := doSSO(t, d.srv, http.MethodGet, "/api/v1/me/scm/github/tokens", memberCookie(t), "")
		if strings.TrimSpace(lw.Body.String()) != "[]" {
			t.Errorf("%s: GET tokens = %s, want []", name, lw.Body)
		}
	}
}

// A person removes their own token, also after the row was disabled or
// switched away from per_user (#1479): the removal only subtracts.
func TestGitHubOwnPATDelete_RemovesWhatTheCallerHolds(t *testing.T) {
	for _, change := range []string{"", "disabled", "not_own_token"} {
		t.Run("row_state="+change, func(t *testing.T) {
			d := newGHOwnDoor(t, adoSite(ghOwnTestRow()), nil)
			if code, body := d.put(t, memberCookie(t), ghOwnReq(ghOwnToken, 10)); code != http.StatusOK {
				t.Fatalf("PUT = %d %s", code, body)
			}
			row := ghOwnTestRow()
			switch change {
			case "disabled":
				row.Disabled = true
			case "not_own_token":
				row.CredentialSource = types.CredentialSourceShared
			}
			d.srv.cfg.Store.(*capStore).site = adoSite(row)
			if w := d.del(t, memberCookie(t), ghOwnOrg); w.Code != http.StatusNoContent {
				t.Fatalf("DELETE = %d %s", w.Code, w.Body)
			}
			if _, found := d.stored(t); found {
				t.Fatal("the token survived its DELETE")
			}
			rows := d.rows(gitHubPATAuditOwnDelete)
			if len(rows) != 1 || rows[0].Outcome != "success" || rowStateOf(t, rows[0]) != change {
				t.Fatalf("delete rows = %+v, want one success with row_state %q", rows, change)
			}
			// Nothing left: a usable row answers 204, any other 404.
			want := http.StatusNotFound
			if change == "" {
				want = http.StatusNoContent
			}
			if w := d.del(t, memberCookie(t), ghOwnOrg); w.Code != want {
				t.Fatalf("second DELETE = %d, want %d", w.Code, want)
			}
		})
	}
}

// The write boundary takes a GitHub per_user row in one shape only.
func TestValidateGitHubOwnTokenRow(t *testing.T) {
	withLanes := func(lanes ...types.GitLane) types.GitProvider {
		r := ghOwnTestRow()
		r.Lanes = lanes
		return r
	}
	ghes := ghOwnTestRow()
	ghes.BaseURLs = []string{"https://ghe.corp.example/acme"}
	badID := ghOwnTestRow()
	badID.ID = "gh.own"
	cases := []struct {
		name    string
		row     types.GitProvider
		refused bool
	}{
		{"the pat lane on github.com", ghOwnTestRow(), false},
		{"pat and ssh", withLanes(types.GitLanePAT, types.GitLaneSSH), true},
		{"the app lane only", withLanes(types.GitLaneApp), true},
		{"no lanes", withLanes(), true},
		{"a GitHub Enterprise Server address", ghes, true},
		{"an id no stored name can carry", badID, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validateWorkspaceProviders(&types.WorkspaceProviders{Git: []types.GitProvider{c.row}}, true)
			if (err != nil) != c.refused {
				t.Fatalf("err = %v, want refused %v", err, c.refused)
			}
		})
	}
}

// The sealed name is refused at the generic secrets API and every other sink.
func TestGitHubOwnTokenSecretName_IsSealed(t *testing.T) {
	name := gitHubOwnTokenSecretName(ghOwnRowID)
	if !reservedSecret(name) || !sinkReservedSecret(name) || !types.IsGitHubOwnTokenSecret(name) {
		t.Fatalf("%q: reserved %v, sink reserved %v, recognised %v — want all three", name,
			reservedSecret(name), sinkReservedSecret(name), types.IsGitHubOwnTokenSecret(name))
	}
}
