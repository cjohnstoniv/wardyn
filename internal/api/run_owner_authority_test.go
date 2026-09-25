// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const ownerEmail = "owner@corp.example"

// newOwnerFixture is newReviveFixture's lost run with SSO mounted, launched
// with an agent, one workspace, and one repo on the github provider row
// capProviderSite configures. It returns the workspace id.
func newOwnerFixture(t *testing.T) (*reviveFixture, uuid.UUID) {
	t.Helper()
	f := newReviveFixture(t)
	f.srv.cfg.OIDC = &oidc.Authenticator{}
	f.srv.router = f.srv.routes()
	ws := uuid.New()
	f.st.run.WorkspaceIDs = []uuid.UUID{ws}
	f.st.site = capProviderSite()
	f.editConfig(t, func(c *proxy.Config) {
		c.Policy.WorkspaceRepos = []types.WorkspaceRepo{{Repo: capProviderRepo, Target: "/home/agent/work/app"}}
	})
	return f, ws
}

func (f *reviveFixture) editConfig(t *testing.T, edit func(*proxy.Config)) {
	t.Helper()
	c, err := proxy.LoadConfigBytes(f.rr.cfg)
	if err != nil {
		t.Fatalf("fixture config: %v", err)
	}
	edit(c)
	if f.rr.cfg, err = json.Marshal(c); err != nil {
		t.Fatal(err)
	}
}

// reviveAs POSTs the revive as the run's owner (a member session) or, with a
// nil cookie, as the admin token.
func (f *reviveFixture) reviveAs(t *testing.T, owner bool) (int, string) {
	t.Helper()
	path := "/api/v1/runs/" + f.run.ID.String() + "/revive"
	if !owner {
		w := do(t, f.srv, http.MethodPost, path, adminToken, "")
		return w.Code, w.Body.String()
	}
	w := doSSO(t, f.srv, http.MethodPost, path, ssoSession(t, f.run.CreatedBy, ownerEmail, oidc.RoleUser), "")
	return w.Code, w.Body.String()
}

// assertReviveRefused: the refusal left the run lost with its proxy stopped,
// and is audited as run.revive denied with reason.
func (f *reviveFixture) assertReviveRefused(t *testing.T, reason string) {
	t.Helper()
	if len(f.rr.replaced) != 0 {
		t.Error("a refused revive replaced the proxy")
	}
	if lostAt, _ := f.st.lost(); lostAt == nil {
		t.Error("a refused revive cleared the lost mark")
	}
	var reasons []string
	for _, ev := range f.audit.eventsFor(f.run.ID, "run.revive") {
		if ev.Outcome == "denied" {
			reasons = append(reasons, leaseAuditData(t, ev)["reason"].(string))
		}
	}
	if !slices.Equal(reasons, []string{reason}) {
		t.Errorf("run.revive denied reasons = %v, want [%s]", reasons, reason)
	}
}

// TestRevive_RechecksOwnerGrants: for each launch door the run can be read
// back for, revoking the owner's grant (a deny row on their email, or the
// allow withdrawn from an enforced kind) refuses the revive, naming the kind,
// whether the owner or an admin asks. With the grant in place both revive.
func TestRevive_RechecksOwnerGrants(t *testing.T) {
	for _, door := range []struct{ kind, value, named string }{
		{capAgent, "claude-code", "claude-code"},
		{capWorkspace, "", ""}, // the fixture's workspace id
		{capWorkspaceProvider, capProviderRowID, "this deployment's github provider"},
	} {
		for _, caller := range []struct {
			name  string
			owner bool
		}{{"owner", true}, {"admin", false}} {
			for _, rev := range []struct {
				name    string
				revoked bool
				caps    func(sub, value string) []types.CapabilityGrant
			}{
				{"deny row on the owner's email", true, func(_, v string) []types.CapabilityGrant {
					return []types.CapabilityGrant{grant(types.CapabilitySubjectUser, ownerEmail, door.kind, v, types.CapabilityDeny)}
				}},
				{"allow withdrawn", true, func(string, string) []types.CapabilityGrant { return nil }},
				{"still granted", false, func(sub, v string) []types.CapabilityGrant {
					return []types.CapabilityGrant{grant(types.CapabilitySubjectUser, sub, door.kind, v, types.CapabilityAllow)}
				}},
			} {
				t.Run(door.kind+"/"+caller.name+"/"+rev.name, func(t *testing.T) {
					f, ws := newOwnerFixture(t)
					value, named := door.value, door.named
					if door.kind == capWorkspace {
						value, named = ws.String(), ws.String()
					}
					f.st.caps = rev.caps(f.run.CreatedBy, value)
					f.st.enf = map[string]bool{door.kind: true}
					code, body := f.reviveAs(t, caller.owner)
					if !rev.revoked {
						if code != http.StatusOK {
							t.Fatalf("revive with the grant in place = %d %s, want 200", code, body)
						}
						return
					}
					want := "the " + door.kind + " capability for " + named
					if code != http.StatusForbidden || !strings.Contains(body, want) {
						t.Fatalf("revive = %d %s; want 403 naming %q", code, body, want)
					}
					if strings.Contains(body, "https://github.com/acme") || (door.kind == capWorkspaceProvider && strings.Contains(body, capProviderRowID)) {
						t.Errorf("refusal %s discloses the provider row", body)
					}
					f.assertReviveRefused(t, "capability_"+door.kind)
				})
			}
		}
	}
}

// TestRevive_FailsClosedOnGrantsStoreError: capAllowedForSub must refuse a
// revive when the capability-grants store cannot be read, never treat the
// error as an implicit allow — an outage must not silently grant a
// capability the owner may since have lost.
func TestRevive_FailsClosedOnGrantsStoreError(t *testing.T) {
	f := newReviveFixture(t)
	f.st.grantsErr = errors.New("grants store: connection reset")
	code, body := f.reviveAs(t, false) // a non-owner admin
	if code != http.StatusServiceUnavailable {
		t.Fatalf("revive with the grants store erroring = %d %s, want 503", code, body)
	}
	if len(f.rr.replaced) != 0 {
		t.Error("a refused revive replaced the proxy")
	}
	if lostAt, _ := f.st.lost(); lostAt == nil {
		t.Error("a refused revive cleared the lost mark")
	}
}

// TestRevive_AnAdminCannotRuleOutTheOwnersGroups: an admin's revive knows the
// owner by sub alone, so a group deny covering the run's agent refuses it
// even though no row names the owner.
func TestRevive_AnAdminCannotRuleOutTheOwnersGroups(t *testing.T) {
	f, _ := newOwnerFixture(t)
	f.st.caps = []types.CapabilityGrant{grant(types.CapabilitySubjectGroup, "contractors", capAgent, "claude-code", types.CapabilityDeny)}
	if code, body := f.reviveAs(t, false); code != http.StatusForbidden {
		t.Fatalf("revive = %d %s, want 403", code, body)
	}
	f.assertReviveRefused(t, "capability_"+capAgent)
}

// TestRevive_HonoursAvailableTo (#903 x #893): a value restricted by
// "Available to" counts as enforced, and only an allow naming it lets the owner
// in; a wildcard allow names nobody. An admin's revive, which knows the owner
// by sub alone, must answer as the owner's own revive does.
func TestRevive_HonoursAvailableTo(t *testing.T) {
	for _, tc := range []struct {
		name     string
		enforced bool
		caps     func(sub string) []types.CapabilityGrant
		want     int
	}{
		{"restricted, switch off, no allow", false, func(string) []types.CapabilityGrant { return nil }, http.StatusForbidden},
		{"restricted, wildcard allow for all", true, func(string) []types.CapabilityGrant {
			return []types.CapabilityGrant{grant(types.CapabilitySubjectAll, "", capAgent, capWildcard, types.CapabilityAllow)}
		}, http.StatusForbidden},
		{"restricted, allow naming the value for the owner", true, func(sub string) []types.CapabilityGrant {
			return []types.CapabilityGrant{grant(types.CapabilitySubjectUser, sub, capAgent, "claude-code", types.CapabilityAllow)}
		}, http.StatusOK},
	} {
		for _, owner := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/owner=%v", tc.name, owner), func(t *testing.T) {
				f, _ := newOwnerFixture(t)
				f.st.caps = tc.caps(f.run.CreatedBy)
				f.st.enf = map[string]bool{capAgent: tc.enforced}
				f.st.restricted = map[string]map[string]bool{capAgent: {"claude-code": true}}
				if code, body := f.reviveAs(t, owner); code != tc.want {
					t.Fatalf("revive = %d %s, want %d", code, body, tc.want)
				}
				if tc.want == http.StatusForbidden {
					f.assertReviveRefused(t, "capability_"+capAgent)
				}
			})
		}
	}
}

// ownerUserTypes are the user types the #1019 fixtures' GetUserType finds.
var ownerUserTypes = []string{"contractor", "partner"}

func getOwnerUserType(id string) (types.UserType, error) {
	if slices.Contains(ownerUserTypes, id) {
		return types.UserType{ID: id}, nil
	}
	return types.UserType{}, store.ErrNotFound
}

// userTypeReviveStore and userTypeLeaseStore are the revive and end-wait
// fixtures' stores with ownerUserTypes.
type userTypeReviveStore struct{ *reviveStore }

func (userTypeReviveStore) GetUserType(_ context.Context, id string) (types.UserType, error) {
	return getOwnerUserType(id)
}

type userTypeLeaseStore struct{ *leaseStore }

func (userTypeLeaseStore) GetUserType(_ context.Context, id string) (types.UserType, error) {
	return getOwnerUserType(id)
}

// TestReviveRestartExtend_AnAdminCountsTheOwnersStampedUserType (#1019): an admin's
// revive, restart with current limits or extension knows the owner by sub and
// by the user type stamped on the run, so an allow row for that type lets the
// owner's restricted value in. Another type, a stamp naming a type deleted
// since, or no stamp (a pre-0080 run) is refused.
func TestReviveRestartExtend_AnAdminCountsTheOwnersStampedUserType(t *testing.T) {
	otherAdmin := ssoSession(t, "sub-other-admin", "admin@corp.example", oidc.RoleAdmin)
	for _, tc := range []struct {
		name, stamp string
		allowed     bool
	}{
		{"owner stamped the allowed type", "contractor", true},
		{"owner stamped another type", "partner", false},
		{"stamped type deleted", "contractor-old", false},
		{"no stamp", "", false},
	} {
		want := http.StatusForbidden
		if tc.allowed {
			want = http.StatusOK
		}
		// arrange restricts the agent to one user type's allow row: the
		// allowed type's, or in the deleted case the deleted type's own, so
		// only the failed lookup refuses.
		arrange := func(st *leaseStore) {
			subject := "contractor"
			if tc.stamp == "contractor-old" {
				subject = tc.stamp
			}
			st.run.UserType = tc.stamp
			st.caps = []types.CapabilityGrant{grant(types.CapabilitySubjectUserType, subject, capAgent, "claude-code", types.CapabilityAllow)}
			st.enf = map[string]bool{capAgent: true}
			st.restricted = map[string]map[string]bool{capAgent: {"claude-code": true}}
		}
		t.Run("revive/"+tc.name, func(t *testing.T) {
			f, _ := newOwnerFixture(t)
			f.srv.cfg.Store = userTypeReviveStore{f.rs}
			arrange(f.st)
			if code, body := f.reviveAs(t, false); code != want {
				t.Fatalf("admin revive = %d %s, want %d", code, body, want)
			}
			if !tc.allowed {
				f.assertReviveRefused(t, "capability_"+capAgent)
			}
		})
		t.Run("restart/"+tc.name, func(t *testing.T) {
			f, _ := newOwnerFixture(t)
			f.srv.cfg.Store = userTypeReviveStore{f.rs}
			arrange(f.st)
			f.st.run.LostAt, f.st.run.LostReason = nil, ""
			w := doSSO(t, f.srv, http.MethodPost, "/api/v1/admin/runs/restart", otherAdmin, `{"run_ids":["`+f.run.ID.String()+`"]}`)
			var out struct {
				Results []adminRestartResult `json:"results"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || w.Code != http.StatusOK || len(out.Results) != 1 {
				t.Fatalf("restart = %d %s (%v), want 200 with one result", w.Code, w.Body.String(), err)
			}
			if r := out.Results[0]; r.OK != tc.allowed || (!tc.allowed && !strings.Contains(r.Error, "the agent capability for claude-code")) {
				t.Fatalf("result = %+v; want ok=%v, a refusal naming the agent capability", r, tc.allowed)
			}
		})
		t.Run("extend/"+tc.name, func(t *testing.T) {
			f := newEndWaitFixture(t, types.RunLimits{MaxEndAheadSec: 30 * 86400})
			f.srv.cfg.Store = userTypeLeaseStore{f.st}
			arrange(f.st)
			if code, _ := f.patch(t, otherAdmin, endsAtBody(f.now.Add(7*24*time.Hour))); code != want {
				t.Fatalf("admin extend = %d, want %d", code, want)
			}
		})
	}
}

// modelCredFixture is newOwnerFixture whose surviving api.anthropic.com
// injection is an api_key grant on anthropic-api-key, held by an enabled
// integration and present in the operator's namespace.
func newModelCredFixture(t *testing.T) (*reviveFixture, *memSecrets) {
	t.Helper()
	f, _ := newOwnerFixture(t)
	c, err := proxy.LoadConfigBytes(f.rr.cfg)
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(c.Injection, func(in proxy.InjectionConfig) bool { return in.Host == "api.anthropic.com" })
	f.st.credGrants = []types.CredentialGrant{{ID: c.Injection[i].GrantID, RunID: f.run.ID,
		Spec: apiKeyGrantSpec("api.anthropic.com", "anthropic-api-key")}}
	f.st.site.Integrations = []types.Integration{{ID: "anthropic", Name: "Anthropic", Kind: types.IntegrationKindAnthropicAPIKey,
		Secrets: []types.IntegrationSecret{{Role: "api_key", SecretName: "anthropic-api-key"}}}}
	sec := &memSecrets{m: map[string][]byte{"anthropic-api-key": []byte("sk-ant-test")}}
	f.srv.cfg.Secrets = sec
	return f, sec
}

// TestRevive_RefusesChangedOrDeletedProvider: the model credential the revived
// proxy would inject is re-checked up front. Its secret erased from every
// namespace the injection sink reads, or the integration holding it disabled,
// refuses the revive naming the host, instead of a proxy whose first model
// call fails. Either namespace the sink reads (the owner's, then the
// operator's) is enough, and an integration deleted with its secret kept is
// not detectable here: the run row does not name its integration.
func TestRevive_RefusesChangedOrDeletedProvider(t *testing.T) {
	for _, tc := range []struct {
		name    string
		arrange func(*reviveFixture, *memSecrets)
		reason  string
	}{
		{"unchanged", func(*reviveFixture, *memSecrets) {}, ""},
		{"credential erased", func(_ *reviveFixture, sec *memSecrets) { delete(sec.m, "anthropic-api-key") }, "model_credential_erased"},
		{"integration disabled", func(f *reviveFixture, _ *memSecrets) { f.st.site.Integrations[0].Disabled = true }, "model_provider_disabled"},
		{"integration deleted, secret kept", func(f *reviveFixture, _ *memSecrets) { f.st.site.Integrations = nil }, ""},
		{"the owner's own key kept, the operator's erased", func(f *reviveFixture, sec *memSecrets) {
			if err := sec.For(f.run.CreatedBy).Put(context.Background(), "anthropic-api-key", []byte("sk-ant-own")); err != nil {
				t.Fatal(err)
			}
			delete(sec.m, "anthropic-api-key")
		}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, sec := newModelCredFixture(t)
			tc.arrange(f, sec)
			code, body := f.reviveAs(t, true)
			if tc.reason == "" {
				if code != http.StatusOK {
					t.Fatalf("revive = %d %s, want 200", code, body)
				}
				return
			}
			if code != http.StatusConflict || !strings.Contains(body, "api.anthropic.com") {
				t.Fatalf("revive = %d %s; want 409 naming the host", code, body)
			}
			f.assertReviveRefused(t, tc.reason)
		})
	}
}

// TestRevive_DoesNotReuseAStaleRenderedConfig: the deployment-wide parts of the
// rendered config come from the current configuration, and the internal-host
// lift only narrows. A site config that cannot be read refuses rather than
// reuse the rendered copy.
func TestRevive_DoesNotReuseAStaleRenderedConfig(t *testing.T) {
	oldCA, _, err := generateRunCA(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	newCA, _, err := generateRunCA(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	kept := types.InternalHost{HostSuffix: "kept.corp.example", CIDRs: []string{"10.0.0.0/8"}}
	stale := func(t *testing.T) *reviveFixture {
		f, _ := newOwnerFixture(t)
		f.editConfig(t, func(c *proxy.Config) {
			c.UpstreamProxyURL = "http://old-corp-proxy.example:3128"
			c.UpstreamProxyNoProxy = []string{"old.corp.example"}
			c.TrustedCAPEM = string(oldCA)
			c.LLMUpstreams = map[string]string{"api.anthropic.com": "https://old-gw.corp.example"}
			c.InternalHosts = []types.InternalHost{{HostSuffix: "gone.corp.example"}, kept}
		})
		f.st.site.UpstreamProxyNoProxy = []string{"new.corp.example"}
		f.st.site.InternalHosts = []types.InternalHost{kept, {HostSuffix: "new.corp.example"}}
		f.srv.cfg.TrustedCAPEM = string(newCA)
		f.srv.cfg.LLMGateways = map[string]string{"api.anthropic.com": "https://new-gw.corp.example"}
		return f
	}

	f := stale(t)
	if code, body := f.reviveAs(t, false); code != http.StatusOK {
		t.Fatalf("revive = %d %s, want 200", code, body)
	}
	c := f.newConfig(t)
	if c.UpstreamProxyURL != "" || !slices.Equal(c.UpstreamProxyNoProxy, []string{"new.corp.example"}) ||
		c.TrustedCAPEM != string(newCA) || !maps.Equal(c.LLMUpstreams, f.srv.cfg.LLMGateways) {
		t.Errorf("revived config upstream %q no_proxy %v gateways %v (trusted CA current: %v); want the current deployment's",
			c.UpstreamProxyURL, c.UpstreamProxyNoProxy, c.LLMUpstreams, c.TrustedCAPEM == string(newCA))
	}
	if len(c.InternalHosts) != 1 || c.InternalHosts[0].HostSuffix != kept.HostSuffix {
		t.Errorf("internal hosts = %+v; want only the one still configured, never one added since", c.InternalHosts)
	}

	f = stale(t)
	f.st.siteErr = errors.New("site config unreadable")
	if code, body := f.reviveAs(t, false); code != http.StatusServiceUnavailable {
		t.Fatalf("revive with the site config unreadable = %d %s, want 503", code, body)
	}
	if len(f.rr.replaced) != 0 {
		t.Error("a revive that could not read the site config replaced the proxy with the rendered copy")
	}
}

// profileStore gives the end-wait fixture's store a profile list.
type profileStore struct {
	*leaseStore
	profiles []types.GovernanceProfile
}

func (s *profileStore) ListGovernanceProfiles(context.Context) ([]types.GovernanceProfile, error) {
	return s.profiles, nil
}

// TestPatchRunEnds_RechecksOwnerStillResolves: moving a run's end later, or to
// No end, keeps its sandbox and credentials alive, so it re-checks its owner's
// authority as a revive does: the captured profile must still exist and the
// run's launch doors must still be open to the owner, whoever asks. Moving
// the end earlier needs neither.
func TestPatchRunEnds_RechecksOwnerStillResolves(t *testing.T) {
	week := func(f *endWaitFixture) string { return endsAtBody(f.now.Add(7 * 24 * time.Hour)) }
	soon := func(f *endWaitFixture) string { return endsAtBody(f.now.Add(time.Hour)) }
	noEnd := func(*endWaitFixture) string { return `{"ends_at":null}` }
	admin := func(t *testing.T) *http.Cookie {
		return ssoSession(t, "sub-admin", "admin@corp.example", oidc.RoleAdmin)
	}
	agentDenied := func(_ *endWaitFixture, st *profileStore) {
		st.caps = []types.CapabilityGrant{grant(types.CapabilitySubjectUser, ownerEmail, capAgent, "claude-code", types.CapabilityDeny)}
	}
	profileGone := func(_ *endWaitFixture, st *profileStore) { st.profiles = nil }
	for _, tc := range []struct {
		name    string
		as      func(*testing.T) *http.Cookie
		body    func(*endWaitFixture) string
		arrange func(*endWaitFixture, *profileStore)
		code    int
		reason  string
	}{
		{"extend, profile gone", ownerSessionAs, week, profileGone, http.StatusConflict, "profile_gone"},
		{"extend, agent denied", ownerSessionAs, week, agentDenied, http.StatusForbidden, "capability_agent"},
		{"extend by a super admin, agent denied", admin, week, agentDenied, http.StatusForbidden, "capability_agent"},
		{"no end, agent denied", ownerSessionAs, noEnd, agentDenied, http.StatusForbidden, "capability_agent"},
		{"extend, still granted", ownerSessionAs, week, func(*endWaitFixture, *profileStore) {}, http.StatusOK, ""},
		{"shorten, agent denied", ownerSessionAs, soon, agentDenied, http.StatusOK, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newEndWaitFixture(t, types.RunLimits{MaxEndAheadSec: 30 * 86400, AllowNoEnd: true, UserChangesLimits: true})
			profile := types.GovernanceProfile{ID: uuid.New(), Name: "contractors"}
			f.st.run.GovernanceProfileID = &profile.ID
			st := &profileStore{leaseStore: f.st, profiles: []types.GovernanceProfile{profile}}
			f.srv.cfg.Store = st
			tc.arrange(f, st)
			code, _ := f.patch(t, tc.as(t), tc.body(f))
			if code != tc.code {
				t.Fatalf("PATCH = %d, want %d", code, tc.code)
			}
			if tc.reason == "" {
				return
			}
			if end, _ := f.stored(); end == nil || !end.Equal(f.end) {
				t.Errorf("stored end = %v; a refused change moved it", end)
			}
			rows := f.rows(t, "run.end.set")
			if len(rows) != 1 || rows[0].Outcome != "denied" || leaseAuditData(t, rows[0])["reason"] != tc.reason {
				t.Errorf("run.end.set rows = %+v; want one denied row, reason %s", rows, tc.reason)
			}
		})
	}
}

func ownerSessionAs(t *testing.T) *http.Cookie {
	return ssoSession(t, endWaitOwner, ownerEmail, oidc.RoleUser)
}

// TestRecheck_AnAdminOwnedRunIsHeldToItsOwnersSubRows pins the by-sub rule for
// an owner who launched as an admin, exempt at the launch gate: another
// admin's revive, restart with current limits or extension knows the owner by
// sub alone, not by role, so under an enforced kind with no `all` or
// owner-sub allow row it is refused. The owner's own session, or an allow row
// for the owner's sub, passes.
func TestRecheck_AnAdminOwnedRunIsHeldToItsOwnersSubRows(t *testing.T) {
	otherAdmin := ssoSession(t, "sub-other-admin", "admin@corp.example", oidc.RoleAdmin)
	for _, tc := range []struct {
		name     string
		byOwner  bool
		subAllow bool
		code     int
	}{
		{"by its owner", true, false, http.StatusOK},
		{"by another admin", false, false, http.StatusForbidden},
		{"by another admin, allow row for the owner's sub", false, true, http.StatusOK},
	} {
		caps := func(owner string) []types.CapabilityGrant {
			if !tc.subAllow {
				return nil
			}
			return []types.CapabilityGrant{grant(types.CapabilitySubjectUser, owner, capAgent, "claude-code", types.CapabilityAllow)}
		}
		t.Run("revive "+tc.name, func(t *testing.T) {
			f, _ := newOwnerFixture(t)
			f.st.run.GovernanceProfileID = nil // a super admin captures none
			f.st.caps, f.st.enf = caps(f.run.CreatedBy), map[string]bool{capAgent: true}
			as := otherAdmin
			if tc.byOwner {
				as = ssoSession(t, f.run.CreatedBy, ownerEmail, oidc.RoleAdmin)
			}
			w := doSSO(t, f.srv, http.MethodPost, "/api/v1/runs/"+f.run.ID.String()+"/revive", as, "")
			if w.Code != tc.code {
				t.Fatalf("revive = %d %s, want %d", w.Code, w.Body.String(), tc.code)
			}
			if tc.code != http.StatusOK {
				f.assertReviveRefused(t, "capability_"+capAgent)
			}
		})
		t.Run("extend "+tc.name, func(t *testing.T) {
			f := newEndWaitFixture(t, types.RunLimits{MaxEndAheadSec: 30 * 86400})
			f.st.caps, f.st.enf = caps(endWaitOwner), map[string]bool{capAgent: true}
			as := otherAdmin
			if tc.byOwner {
				as = ssoSession(t, endWaitOwner, ownerEmail, oidc.RoleAdmin)
			}
			if code, _ := f.patch(t, as, endsAtBody(f.now.Add(7*24*time.Hour))); code != tc.code {
				t.Fatalf("PATCH = %d, want %d", code, tc.code)
			}
		})
	}
	t.Run("restart with current limits by another admin", func(t *testing.T) {
		f, _ := newOwnerFixture(t)
		f.st.run.LostAt, f.st.run.LostReason, f.st.run.GovernanceProfileID = nil, "", nil
		f.st.enf = map[string]bool{capAgent: true}
		w := doSSO(t, f.srv, http.MethodPost, "/api/v1/admin/runs/restart", otherAdmin, `{"run_ids":["`+f.run.ID.String()+`"]}`)
		var out struct {
			Results []adminRestartResult `json:"results"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || w.Code != http.StatusOK {
			t.Fatalf("restart = %d %s (%v), want 200", w.Code, w.Body.String(), err)
		}
		if len(out.Results) != 1 || out.Results[0].OK || !strings.Contains(out.Results[0].Error, "the agent capability for claude-code") {
			t.Fatalf("results = %+v; want the admin-owned run refused naming the agent capability", out.Results)
		}
		if len(f.rr.replaced) != 0 {
			t.Error("a refused restart replaced the proxy")
		}
	})
}
