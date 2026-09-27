// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestCapFilterEqualsAllowedPerID: capFilter(ids) is exactly the ids the
// one-value resolver allows, in input order, for every kind, caller and grant
// state — the list half can never disagree with the door that refuses.
func TestCapFilterEqualsAllowedPerID(t *testing.T) {
	allow, deny := types.CapabilityAllow, types.CapabilityDeny
	ids := []string{"alpha", "beta", "api.example.com", "*.example.com", "alpha"}
	grantSets := map[string]func(kind string) []types.CapabilityGrant{
		"none": func(string) []types.CapabilityGrant { return nil },
		"user allow": func(k string) []types.CapabilityGrant {
			return []types.CapabilityGrant{grant(types.CapabilitySubjectUser, capEmail, k, "alpha", allow)}
		},
		"all deny": func(k string) []types.CapabilityGrant {
			return []types.CapabilityGrant{grant(types.CapabilitySubjectAll, "", k, "beta", deny)}
		},
		"group deny, wildcard allow": func(k string) []types.CapabilityGrant {
			return []types.CapabilityGrant{
				grant(types.CapabilitySubjectGroup, "contractors", k, "api.example.com", deny),
				grant(types.CapabilitySubjectAll, "", k, capWildcard, allow),
			}
		},
	}
	callers := map[string]context.Context{
		"member":                memberCtx([]string{"contractors"}),
		"member, stale groups":  memberCtx(nil),
		"security admin":        withOIDCGroups(operatorCtx(capSub, capEmail, oidc.RoleSecurityAdmin), []string{}),
		"admin":                 operatorCtx(capSub, capEmail, oidc.RoleAdmin),
		"admin token (no user)": context.Background(),
	}
	for _, kind := range capabilityKinds {
		for gname, gs := range grantSets {
			for _, enforced := range []bool{false, true} {
				for cname, ctx := range callers {
					st := &capStore{grants: gs(kind), enf: map[string]bool{kind: enforced}}
					srv := capServer(st)
					var want []string
					for _, id := range ids {
						ok, err := srv.capSeamAllowed(ctx, kind, id)
						if err != nil {
							t.Fatalf("%s/%s/%v/%s: capSeamAllowed(%q): %v", kind, gname, enforced, cname, id, err)
						}
						if ok {
							want = append(want, id)
						}
					}
					got, err := srv.capFilter(ctx, kind, ids)
					if err != nil {
						t.Fatalf("%s/%s/%v/%s: capFilter: %v", kind, gname, enforced, cname, err)
					}
					if !slices.Equal(got, want) {
						t.Errorf("%s/%s/enforced=%v/%s: capFilter = %q, want %q", kind, gname, enforced, cname, got, want)
					}
				}
			}
		}
	}
}

// TestCapFilterFailsClosed: a store that cannot answer yields no ids and an
// error, never the unfiltered list; an unknown kind is an error, not a pass.
func TestCapFilterFailsClosed(t *testing.T) {
	srv := capServer(&capStore{err: errors.New("pg down")})
	if got, err := srv.capFilter(memberCtx([]string{}), capAgent, []string{"claude-code"}); err == nil || got != nil {
		t.Fatalf("store error: capFilter = %q, %v; want nil and an error", got, err)
	}
	if got, err := capServer(&capStore{}).capFilter(memberCtx([]string{}), "no_such_kind", []string{"x"}); err == nil || got != nil {
		t.Fatalf("unknown kind: capFilter = %q, %v; want nil and an error", got, err)
	}
	rows := capVisible(memberCtx([]string{}), srv, capAgent, []string{"claude-code", "codex-cli"}, func(s string) string { return s })
	if len(rows) != 0 {
		t.Fatalf("capVisible on a store error = %q, want no rows", rows)
	}
}

// carrierServer is a member-reachable server over site, whose capability rows
// are grants: stored integrations and an Azure DevOps per-user row resolve for
// real, everything else answers empty.
func carrierServer(t *testing.T, site types.SiteConfig, grants []types.CapabilityGrant, storeErr error) *Server {
	t.Helper()
	h := newHarness(t)
	cfg := baseTestConfig(h, &capStore{Store: r3IntegStore{}, site: site, grants: grants, err: storeErr})
	cfg.OIDC = &oidc.Authenticator{}
	cfg.ADOEntra = adoTestEntraSource
	cfg.Secrets = &memSecrets{m: map[string][]byte{}}
	cfg.MaskRegistry = secretmask.NewRegistry()
	return New(cfg)
}

func memberCookie(t *testing.T) *http.Cookie {
	return ssoSession(t, capSub, capEmail, oidc.RoleUser)
}

// getJSON GETs path and decodes the body's top-level keys.
func getJSON(t *testing.T, srv *Server, path string, cookie *http.Cookie) (string, map[string]json.RawMessage) {
	t.Helper()
	w := doSSO(t, srv, http.MethodGet, path, cookie, "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", path, w.Code, w.Body.String())
	}
	var m map[string]json.RawMessage
	_ = json.Unmarshal(w.Body.Bytes(), &m) // an array body leaves m nil; the raw body is returned too
	return w.Body.String(), m
}

var (
	carrierIntegKept    = types.Integration{ID: "team-wiki", Name: "Team wiki", Kind: "artifactory"}
	carrierIntegRefused = types.Integration{ID: "corp-vault", Name: "Corp vault", Kind: "artifactory"}
)

// TestListCarriersRefusedReadsAsAbsent is the no-oracle property for every
// per-person list: a resource the caller may not use is indistinguishable from
// one the deployment does not have. Each carrier is read in a world where the
// resource exists and is refused to this member, and in a world where it does
// not exist at all; the two answers must be byte-identical. A world where it is
// allowed must show it, so the comparison is never vacuous.
func TestListCarriersRefusedReadsAsAbsent(t *testing.T) {
	deny := func(kind, value string) []types.CapabilityGrant {
		return []types.CapabilityGrant{grant(types.CapabilitySubjectAll, "", kind, value, types.CapabilityDeny)}
	}

	t.Run("integrations: GET /setup/status and GET /integrations", func(t *testing.T) {
		present := types.SiteConfig{Integrations: []types.Integration{carrierIntegKept, carrierIntegRefused}}
		absent := types.SiteConfig{Integrations: []types.Integration{carrierIntegKept}}
		for _, path := range []string{"/api/v1/setup/status", "/api/v1/integrations"} {
			refused, _ := getJSON(t, carrierServer(t, present, deny(capIntegration, carrierIntegRefused.ID), nil), path, memberCookie(t))
			missing, _ := getJSON(t, carrierServer(t, absent, nil, nil), path, memberCookie(t))
			if refused != missing {
				t.Errorf("GET %s: a refused integration is distinguishable from an absent one\nrefused: %s\nabsent:  %s", path, refused, missing)
			}
			if strings.Contains(refused, carrierIntegRefused.ID) || strings.Contains(refused, carrierIntegRefused.Name) {
				t.Errorf("GET %s names the refused integration: %s", path, refused)
			}
			allowed, _ := getJSON(t, carrierServer(t, present, nil, nil), path, memberCookie(t))
			if !strings.Contains(allowed, carrierIntegRefused.ID) {
				t.Errorf("GET %s hides an integration nothing refuses: %s", path, allowed)
			}
		}
	})

	t.Run("harnesses: GET /setup/status", func(t *testing.T) {
		harnessIDs := func(srv *Server) []string {
			_, m := getJSON(t, srv, "/api/v1/setup/status", memberCookie(t))
			var tools []SetupHarnessTool
			if raw, ok := m["harnesses"]; ok {
				if err := json.Unmarshal(raw, &tools); err != nil {
					t.Fatalf("decode harnesses: %v", err)
				}
			}
			var out []string
			for _, tool := range tools {
				out = append(out, tool.ID)
			}
			return out
		}
		var catalog []string
		for _, d := range harnessCatalog {
			catalog = append(catalog, d.ID)
		}
		if got := harnessIDs(carrierServer(t, types.SiteConfig{}, nil, nil)); !slices.Equal(got, catalog) {
			t.Fatalf("no grants: harnesses = %q, want the whole catalog %q", got, catalog)
		}
		got := harnessIDs(carrierServer(t, types.SiteConfig{}, deny(capAgent, "codex-cli"), nil))
		want := slices.DeleteFunc(slices.Clone(catalog), func(id string) bool { return id == "codex-cli" })
		if !slices.Equal(got, want) {
			t.Errorf("codex-cli denied: harnesses = %q, want the catalog without it %q", got, want)
		}
	})

	t.Run("Azure DevOps access: GET /me/scm-access and GET /setup/status", func(t *testing.T) {
		present := adoTestSiteConfig(false)
		refusedSrv := carrierServer(t, present, deny(capWorkspaceProvider, scmTestRowID), nil)
		missingSrv := carrierServer(t, types.SiteConfig{}, nil, nil)
		refused, _ := getJSON(t, refusedSrv, "/api/v1/me/scm-access", memberCookie(t))
		missing, _ := getJSON(t, missingSrv, "/api/v1/me/scm-access", memberCookie(t))
		if refused != missing {
			t.Errorf("GET /me/scm-access: a refused org is distinguishable from none\nrefused: %s\nabsent:  %s", refused, missing)
		}
		_, st := getJSON(t, refusedSrv, "/api/v1/setup/status", memberCookie(t))
		if raw, ok := st["scm_access"]; ok {
			t.Errorf("GET /setup/status carries scm_access %s for a refused org; with no org it is omitted", raw)
		}
		allowed, _ := getJSON(t, carrierServer(t, present, nil, nil), "/api/v1/me/scm-access", memberCookie(t))
		if !strings.Contains(allowed, `"kind":"azure_devops"`) {
			t.Errorf("GET /me/scm-access hides an org nothing refuses: %s", allowed)
		}
	})

	t.Run("an admin is exempt: the refused rows stay", func(t *testing.T) {
		site := adoTestSiteConfig(false)
		site.Integrations = []types.Integration{carrierIntegKept, carrierIntegRefused}
		grants := slices.Concat(deny(capIntegration, carrierIntegRefused.ID), deny(capAgent, "codex-cli"),
			deny(capWorkspaceProvider, scmTestRowID))
		admin := ssoSession(t, "sub-admin", "admin@corp.example", oidc.RoleAdmin)
		body, _ := getJSON(t, carrierServer(t, site, grants, nil), "/api/v1/setup/status", admin)
		for _, want := range []string{carrierIntegRefused.ID, `"id":"codex-cli"`, `"scm_access"`} {
			if !strings.Contains(body, want) {
				t.Errorf("GET /setup/status as an admin dropped %s: %s", want, body)
			}
		}
	})
}

// TestListCarriersFailClosed: when the grant tables cannot be read, a member's
// lists come back empty — never unfiltered — and the page still answers 200.
func TestListCarriersFailClosed(t *testing.T) {
	site := adoTestSiteConfig(false)
	site.Integrations = []types.Integration{carrierIntegKept}
	srv := carrierServer(t, site, nil, errors.New("pg down"))
	_, st := getJSON(t, srv, "/api/v1/setup/status", memberCookie(t))
	for _, key := range []string{"harnesses", "integrations", "scm_access"} {
		if raw, ok := st[key]; ok {
			t.Errorf("GET /setup/status on an unreadable grant table carries %s = %s, want it omitted", key, raw)
		}
	}
	if body, _ := getJSON(t, srv, "/api/v1/me/scm-access", memberCookie(t)); strings.TrimSpace(body) != "[]" {
		t.Errorf("GET /me/scm-access on an unreadable grant table = %s, want []", body)
	}
}

// capKindsGolden is every version the kind table has had. A change to capKinds
// appends the next version here and bumps capKindsVersion; an entry is never
// edited, because a client compares the number, not the list.
var capKindsGolden = map[int]string{
	1: "egress_host:narrowing,secret:narrowing,workspace:narrowing,image:widening,agent:narrowing,integration:narrowing,workspace_provider:narrowing",
	2: "egress_host:narrowing,secret:narrowing,workspace:narrowing,image:widening,agent:narrowing,integration:narrowing,workspace_provider:narrowing,model_provider:narrowing",
	3: "egress_host:narrowing,secret:narrowing,workspace:narrowing,image:widening,agent:narrowing,integration:narrowing,workspace_provider:narrowing,model_provider:narrowing,feature:narrowing,policy:narrowing",
}

// TestCapKindsVersionPinsTheTable fails on a kind-table change that does not
// bump capKindsVersion, and checks GET /me/capabilities reports it.
func TestCapKindsVersionPinsTheTable(t *testing.T) {
	var rows []string
	for _, k := range capabilityKinds {
		dir := "narrowing"
		if capKinds[k].direction == capWidening {
			dir = "widening"
		}
		rows = append(rows, k+":"+dir)
	}
	if got := strings.Join(rows, ","); got != capKindsGolden[capKindsVersion] {
		t.Fatalf("the kind table changed without a version bump:\n got %s\nv%d %s\nappend version %d to capKindsGolden and bump capKindsVersion",
			got, capKindsVersion, capKindsGolden[capKindsVersion], capKindsVersion+1)
	}
	if _, ok := capKindsGolden[capKindsVersion+1]; ok {
		t.Fatalf("capKindsGolden has version %d but capKindsVersion is %d", capKindsVersion+1, capKindsVersion)
	}

	h := newHarness(t)
	h.srv.cfg.OIDC = &oidc.Authenticator{}
	h.srv.cfg.Store = &capStore{}
	h.srv.router = h.srv.routes()
	_, m := getJSON(t, h.srv, "/api/v1/me/capabilities", memberCookie(t))
	if got := string(m["kinds_version"]); got != strconv.Itoa(capKindsVersion) {
		t.Fatalf("GET /me/capabilities kinds_version = %q, want %d", got, capKindsVersion)
	}
}
