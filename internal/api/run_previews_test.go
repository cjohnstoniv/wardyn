// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The preview routes' tests turn the key on with withSiteFeatureHooks, so none of them runs in parallel.

// previewRows is an in-memory run_previews table with store_run_previews.go's semantics, and a revoke above a
// ceiling that a test can fail.
type previewRows struct {
	previewMu sync.Mutex
	rows      []types.RunPreview
	aboveErrs []error                   // what the next RevokeRunPreviewsAbove calls answer, first first
	above     []types.PreviewVisibility // every ceiling RevokeRunPreviewsAbove was asked
}

func previewLive(p types.RunPreview) bool { return p.RevokedAt == nil && p.ExpiresAt.After(time.Now()) }

func (s *previewRows) UpsertRunPreview(_ context.Context, runID uuid.UUID, port int, vis types.PreviewVisibility, ttl time.Duration, by string, _ []byte) (types.RunPreview, bool, error) {
	s.previewMu.Lock()
	defer s.previewMu.Unlock()
	now := time.Now().UTC()
	for i := range s.rows {
		p := &s.rows[i]
		if p.RunID != runID || p.Port != port || p.RevokedAt != nil {
			continue
		}
		if previewLive(*p) && p.Visibility == vis {
			p.ExpiresAt, p.UpdatedAt = now.Add(ttl), now
			return *p, false, nil
		}
		p.RevokedAt = &now
	}
	p := types.RunPreview{ID: uuid.New(), RunID: runID, Port: port, Visibility: vis, CreatedBy: by, CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(ttl)}
	s.rows = append(s.rows, p)
	return p, true, nil
}

func (s *previewRows) ListRunPreviews(_ context.Context, runID uuid.UUID) ([]types.RunPreview, error) {
	s.previewMu.Lock()
	defer s.previewMu.Unlock()
	var out []types.RunPreview
	for _, p := range s.rows {
		if p.RunID == runID && previewLive(p) {
			out = append(out, p)
		}
	}
	slices.SortFunc(out, func(a, b types.RunPreview) int { return a.Port - b.Port })
	return out, nil
}

func (s *previewRows) LiveRunPreview(_ context.Context, runID uuid.UUID, port int) (types.RunPreview, error) {
	s.previewMu.Lock()
	defer s.previewMu.Unlock()
	for _, p := range s.rows {
		if p.RunID == runID && p.Port == port && previewLive(p) {
			return p, nil
		}
	}
	return types.RunPreview{}, store.ErrNotFound
}

func (s *previewRows) RevokeRunPreview(_ context.Context, runID uuid.UUID, port int, _ string) (types.RunPreview, error) {
	s.previewMu.Lock()
	defer s.previewMu.Unlock()
	for i := range s.rows {
		if p := &s.rows[i]; p.RunID == runID && p.Port == port && p.RevokedAt == nil {
			now := time.Now().UTC()
			p.RevokedAt = &now
			return *p, nil
		}
	}
	return types.RunPreview{}, store.ErrNotFound
}

func (s *previewRows) RevokeRunPreviewsAbove(_ context.Context, ceiling types.PreviewVisibility, _ string) ([]types.RunPreview, error) {
	s.previewMu.Lock()
	defer s.previewMu.Unlock()
	s.above = append(s.above, ceiling)
	if len(s.aboveErrs) > 0 {
		err := s.aboveErrs[0]
		s.aboveErrs = s.aboveErrs[1:]
		return nil, err
	}
	out := []types.RunPreview{}
	for i := range s.rows {
		if p := &s.rows[i]; p.RevokedAt == nil && !p.Visibility.AtMost(ceiling) {
			now := time.Now().UTC()
			p.RevokedAt = &now
			out = append(out, *p)
		}
	}
	return out, nil
}

// putPreview stores a live row as no route would: past the ceiling and host rows.
func (s *previewRows) putPreview(runID uuid.UUID, port int, vis types.PreviewVisibility) types.RunPreview {
	s.previewMu.Lock()
	defer s.previewMu.Unlock()
	now := time.Now().UTC()
	p := types.RunPreview{ID: uuid.New(), RunID: runID, Port: port, Visibility: vis, CreatedBy: "alice", CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(time.Hour)}
	s.rows = append(s.rows, p)
	return p
}

func (s *previewRows) liveCount() int {
	s.previewMu.Lock()
	defer s.previewMu.Unlock()
	n := 0
	for _, p := range s.rows {
		if previewLive(p) {
			n++
		}
	}
	return n
}

// previewTestStore is the UI harness's store with a site config a test sets and the run_previews table.
type previewTestStore struct {
	*uiMemStore
	*previewRows
	siteMu  sync.Mutex
	site    types.SiteConfig
	siteErr error
}

func (s *previewTestStore) GetSiteConfig(context.Context) (types.SiteConfig, error) {
	s.siteMu.Lock()
	defer s.siteMu.Unlock()
	return s.site, s.siteErr
}

// GetDelegateByID answers a delegated request's portal as one with no issuer, so no portal pin applies.
func (s *previewTestStore) GetDelegateByID(_ context.Context, id uuid.UUID) (types.Delegate, error) {
	return types.Delegate{ID: id, Name: "portal"}, nil
}

func (s *previewTestStore) setSite(sc types.SiteConfig, err error) {
	s.siteMu.Lock()
	defer s.siteMu.Unlock()
	s.site, s.siteErr = sc, err
}

// previewSite is a site config whose preview_urls is enabled as on, up to max.
func previewSite(on bool, max string) types.SiteConfig {
	return types.SiteConfig{FeatureSwitches: &types.SiteFeatureSwitches{PreviewURLs: &types.PreviewURLsFeature{Enabled: &on, MaxVisibility: max}}}
}

// withPreviewsAvailable installs the registered preview_urls hook with available on, as the routes' tests run it
// before the release serves previews. Every other key reads unregistered.
func withPreviewsAvailable(t *testing.T) {
	t.Helper()
	h, ok := siteFeatureHookFor(featurePreviewURLs)
	if !ok || h.validate == nil || h.afterWrite == nil {
		t.Fatal("preview_urls registers no hook with a validate and an afterWrite")
	}
	h.available = func() bool { return true }
	withSiteFeatureHooks(t, map[siteFeatureKey]siteFeatureHook{featurePreviewURLs: h})
}

// previewRig is the UI harness in host mode, with previews on up to org and alice's running run, which declares
// the app "code" on uiTestPort.
type previewRig struct {
	t  *testing.T
	h  *uiHarness
	st *previewTestStore
}

func newPreviewRig(t *testing.T) *previewRig {
	t.Helper()
	withPreviewsAvailable(t)
	h := newUIHarness(t, okBackend())
	h.srv.cfg.UIOriginTemplate = "https://{run}.ui.example.com"
	st := &previewTestStore{uiMemStore: h.store, previewRows: &previewRows{}, site: previewSite(true, "org")}
	h.srv.cfg.Store = st
	return &previewRig{t: t, h: h, st: st}
}

// previewWho is the identity a request carries.
type previewWho func(context.Context) context.Context

func previewPerson(sub, role string) previewWho {
	return func(ctx context.Context) context.Context {
		return withHumanIdentity(ctx, sub, sub+"@example.com", role, types.UserTypeStandard, nil, false)
	}
}

var (
	previewOwner    = previewPerson("alice", oidc.RoleUser)
	previewMember   = previewPerson("bob", oidc.RoleUser)
	previewSuper    = previewPerson("root", oidc.RoleAdmin)
	previewSecurity = previewPerson("sec", oidc.RoleSecurityAdmin)
)

// set changes the run and stores it.
func (g *previewRig) set(f func(run *types.AgentRun)) {
	g.t.Helper()
	f(&g.h.run)
	g.h.store.putRun(g.h.run)
}

func (g *previewRig) do(who previewWho, method, path, body string) *httptest.ResponseRecorder {
	g.t.Helper()
	r := chi.NewRouter()
	g.h.srv.mountRunPreviewRoutes(r)
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req = req.WithContext(who(req.Context()))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func (g *previewRig) path(port string) string {
	return "/runs/" + g.h.run.ID.String() + "/previews/" + port
}

func (g *previewRig) put(who previewWho, port, body string) *httptest.ResponseRecorder {
	return g.do(who, http.MethodPut, g.path(port), body)
}

func (g *previewRig) ticket(who previewWho, port string) *httptest.ResponseRecorder {
	return g.do(who, http.MethodPost, g.path(port)+"/ticket", "")
}

// stored is the ticket a mint answered with, as the store keeps it.
func (g *previewRig) stored(rec *httptest.ResponseRecorder) store.AttachTicket {
	g.t.Helper()
	tok := ticketOf(g.t, rec)
	g.h.store.mu.Lock()
	defer g.h.store.mu.Unlock()
	return g.h.store.tickets[tok]
}

// preview is the row a PUT answered with.
func (g *previewRig) preview(what string, rec *httptest.ResponseRecorder) types.RunPreview {
	g.t.Helper()
	var b struct {
		Preview types.RunPreview `json:"preview"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &b) != nil || b.Preview.ID == uuid.Nil {
		g.t.Fatalf("%s: %d %s, want 200 with a preview", what, rec.Code, rec.Body.String())
	}
	return b.Preview
}

// refused fails unless rec is status with reason and a preview.deny row carries the reason. Each call moves the
// clock past the auth.fail bound, so the next refusal's row is never dropped.
func (g *previewRig) refused(what string, rec *httptest.ResponseRecorder, status int, reason string) {
	g.t.Helper()
	if rec.Code != status || respondedReason(g.t, rec) != reason {
		g.t.Fatalf("%s: %d %s, want %d %s", what, rec.Code, rec.Body.String(), status, reason)
	}
	rows := g.h.audit.auditRows("preview.deny", "denied")
	if len(rows) == 0 || !strings.Contains(string(rows[len(rows)-1].Data), `"reason":"`+reason+`"`) {
		g.t.Fatalf("%s: preview.deny rows %+v, want the last one with reason %s", what, rows, reason)
	}
	g.h.clock.advance(time.Minute)
}

// TestPreviewPrivateEntryRuleOnly: a private preview is created and opened by the person the run's entry
// rule admits. A member who is not the run's creator gets the run-not-found 404 at both routes, with an
// authz.denied row and the same body a missing run answers; a super admin gets run_owner_only at the create and
// the 404 at the ticket. The owner's ticket is an owner ticket for the preview's app alone, member-stamped.
func TestPreviewPrivateEntryRuleOnly(t *testing.T) {
	g := newPreviewRig(t)
	p := g.preview("owner's create", g.put(previewOwner, "3000", `{}`))
	if p.Visibility != types.PreviewPrivate || p.Port != 3000 || p.CreatedBy != "alice" {
		t.Fatalf("created %+v, want alice's private preview of 3000", p)
	}
	if rows := g.h.audit.auditRows("preview.create", "success"); len(rows) != 1 || rows[0].Actor != "alice" || rows[0].Target != "__preview-3000" {
		t.Fatalf("preview.create rows = %+v, want one by alice on __preview-3000", rows)
	}
	tk := g.stored(g.ticket(previewOwner, "3000"))
	if tk.Caller != "" || tk.App != "__preview-3000" || tk.Role != oidc.RoleUser || tk.Principal != "alice" || tk.RunID != g.h.run.ID {
		t.Fatalf("owner's ticket = %+v, want an owner ticket for __preview-3000, member-stamped", tk)
	}
	if rows := g.h.audit.auditRows("preview.open", "success"); len(rows) != 1 || !strings.Contains(string(rows[0].Data), `"caller":"owner"`) {
		t.Fatalf("preview.open rows = %+v, want one naming the owner", rows)
	}

	missing := g.do(previewMember, http.MethodPost, "/runs/"+uuid.NewString()+"/previews/3000/ticket", "")
	denied := len(g.h.audit.auditRows("authz.denied", "denied"))
	for _, c := range []struct {
		what string
		rec  *httptest.ResponseRecorder
	}{
		{"a member's create", g.put(previewMember, "3001", `{}`)},
		{"a member's ticket", g.ticket(previewMember, "3000")},
		{"a super admin's ticket", g.ticket(previewSuper, "3000")},
	} {
		if c.rec.Code != http.StatusNotFound || respondedReason(t, c.rec) != reasonRunNotFound {
			t.Errorf("%s: %d %s, want the run-not-found 404", c.what, c.rec.Code, c.rec.Body.String())
		}
	}
	if rows := g.h.audit.auditRows("authz.denied", "denied"); len(rows) < denied+3 {
		t.Errorf("authz.denied rows went from %d to %d, want one per refusal", denied, len(rows))
	}
	if rec := g.ticket(previewMember, "3000"); rec.Body.String() != missing.Body.String() {
		t.Errorf("a member's ticket answers %q, a missing run %q: want them byte-identical", rec.Body.String(), missing.Body.String())
	}
	if rec := g.put(previewSuper, "3001", `{}`); rec.Code != http.StatusForbidden || respondedReason(t, rec) != "run_owner_only" {
		t.Errorf("a super admin's create: %d %s, want 403 run_owner_only", rec.Code, rec.Body.String())
	}
	if n := len(g.st.rows); n != 1 {
		t.Errorf("stored %d previews, want only the owner's", n)
	}
}

// TestPreviewAboveCeilingRefused: a visibility above the ceiling is refused at create, at a change and at
// the ticket, with 403 preview_above_ceiling; public is above every ceiling while public links are not served.
func TestPreviewAboveCeilingRefused(t *testing.T) {
	g := newPreviewRig(t)
	g.refused("public under org", g.put(previewOwner, "3000", `{"visibility":"public"}`), http.StatusForbidden, reasonPreviewAboveCeiling)
	org := g.preview("org under org", g.put(previewOwner, "3000", `{"visibility":"org"}`))
	g.st.setSite(previewSite(true, "private"), nil)
	g.refused("org under private", g.put(previewOwner, "3001", `{"visibility":"org"}`), http.StatusForbidden, reasonPreviewAboveCeiling)
	g.refused("a change to org under private", g.put(previewOwner, "3000", `{"visibility":"org","ttl_seconds":7200}`), http.StatusForbidden, reasonPreviewAboveCeiling)
	g.refused("an org row's ticket under private", g.ticket(previewOwner, "3000"), http.StatusForbidden, reasonPreviewAboveCeiling)
	g.refused("an org caller's ticket under private", g.ticket(previewMember, "3000"), http.StatusForbidden, reasonPreviewAboveCeiling)
	if live, err := g.st.LiveRunPreview(context.Background(), g.h.run.ID, 3000); err != nil || live.ID != org.ID || !live.ExpiresAt.Equal(org.ExpiresAt) {
		t.Errorf("the org row after the refusals = %+v (%v), want it unchanged", live, err)
	}
	g.st.setSite(previewSite(true, "public"), nil)
	g.refused("public under a stored public", g.put(previewOwner, "3002", `{"visibility":"public"}`), http.StatusForbidden, reasonPreviewAboveCeiling)
}

// TestPreviewDefaultsPrivate: an absent visibility creates a private preview, and changes a live one to
// private, whatever the ceiling; an absent ttl_seconds is 24 hours.
func TestPreviewDefaultsPrivate(t *testing.T) {
	g := newPreviewRig(t)
	p := g.preview("create with no visibility", g.put(previewOwner, "3000", `{}`))
	if p.Visibility != types.PreviewPrivate || p.ExpiresAt.Sub(p.CreatedAt) != types.PreviewTTLDefault {
		t.Fatalf("created %+v, want private for 24 hours", p)
	}
	org := g.preview("org", g.put(previewOwner, "3001", `{"visibility":"org"}`))
	changed := g.preview("change with no visibility", g.put(previewOwner, "3001", `{"ttl_seconds":7200}`))
	if changed.Visibility != types.PreviewPrivate || changed.ID == org.ID {
		t.Fatalf("changed %+v, want a new private row in place of org row %s", changed, org.ID)
	}
	rows := g.h.audit.auditRows("preview.update", "success")
	if len(rows) != 1 || !strings.Contains(string(rows[0].Data), `"from":"org"`) || !strings.Contains(string(rows[0].Data), `"replaced":"`+org.ID.String()+`"`) {
		t.Fatalf("preview.update rows = %+v, want one naming the org row it replaced", rows)
	}
}

// TestPreviewSwitchOff (the routes): with the switch off or absent the create and the ticket answer 403
// preview_urls_off, and an unreadable site config refuses both; the list and the revoke still answer.
func TestPreviewSwitchOff(t *testing.T) {
	g := newPreviewRig(t)
	row := g.st.putPreview(g.h.run.ID, 3000, types.PreviewPrivate)
	for _, sc := range []types.SiteConfig{previewSite(false, "org"), {}} {
		g.st.setSite(sc, nil)
		g.refused("create, switch off", g.put(previewOwner, "3001", `{}`), http.StatusForbidden, reasonPreviewURLsOff)
		g.refused("ticket, switch off", g.ticket(previewOwner, "3000"), http.StatusForbidden, reasonPreviewURLsOff)
	}
	g.st.setSite(types.SiteConfig{}, errors.New("database is down"))
	g.refused("create, site config unreadable", g.put(previewOwner, "3001", `{}`), http.StatusServiceUnavailable, reasonRosterUnreadable)
	g.refused("ticket, site config unreadable", g.ticket(previewOwner, "3000"), http.StatusServiceUnavailable, reasonRosterUnreadable)
	if rec := g.do(previewOwner, http.MethodGet, "/runs/"+g.h.run.ID.String()+"/previews", ""); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), row.ID.String()) {
		t.Fatalf("list, switch off: %d %s, want 200 naming row %s", rec.Code, rec.Body.String(), row.ID)
	}
	if rec := g.do(previewOwner, http.MethodDelete, g.path("3000"), ""); rec.Code != http.StatusNoContent || g.st.liveCount() != 0 {
		t.Fatalf("revoke, switch off: %d %s with %d live, want 204 and none live", rec.Code, rec.Body.String(), g.st.liveCount())
	}
	if rows := g.h.audit.auditRows("preview.revoke", "success"); len(rows) != 1 || rows[0].Actor != "alice" {
		t.Fatalf("preview.revoke rows = %+v, want alice's one", rows)
	}
	if rec := g.do(previewOwner, http.MethodDelete, g.path("3000"), ""); rec.Code != http.StatusNoContent {
		t.Fatalf("revoke again: %d %s, want the same 204", rec.Code, rec.Body.String())
	}
	if rec := g.do(previewSecurity, http.MethodGet, "/runs/"+g.h.run.ID.String()+"/previews", ""); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"previews":[]`) {
		t.Fatalf("a security admin's list: %d %s, want 200 with no live preview", rec.Code, rec.Body.String())
	}
}

// TestPreviewConsoleOnly: a preview is created by a person signed in to the console only: a service
// identity, an agent, a portal's delegated token, an issuer portal's person and a public link's visitor are not
// one, and a create by any of them on its own run (which passes the run's entry check) is 403 preview_console_only.
func TestPreviewConsoleOnly(t *testing.T) {
	const portalPerson = "delegate:person:abcdefghijklmnopqrstuvwxyz:abcd"
	via := types.DelegationVia{Delegate: uuid.New(), Grant: uuid.New()}
	for _, c := range []struct {
		name, creator string
		who           previewWho
	}{
		{"a service identity", types.ServicePrincipalPrefix + "payments", func(ctx context.Context) context.Context {
			return withServiceIdentity(ctx, "payments", "alice")
		}},
		{"a portal's delegated token", "alice", func(ctx context.Context) context.Context {
			return audit.WithDelegation(previewOwner(ctx), via)
		}},
		{"an issuer portal's person", portalPerson, previewPerson(portalPerson, oidc.RoleUser)},
		{"an agent", "agent-1", func(ctx context.Context) context.Context {
			return context.WithValue(ctx, ticketActorCtxKey{}, ticketActor{actorType: types.ActorAgent, principal: "agent-1"})
		}},
		{"a public link's visitor", types.PreviewLinkPrincipalPrefix + "x", func(ctx context.Context) context.Context {
			return context.WithValue(ctx, ticketActorCtxKey{}, ticketActor{actorType: types.ActorHuman, principal: types.PreviewLinkPrincipalPrefix + "x"})
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			g := newPreviewRig(t)
			g.set(func(run *types.AgentRun) { run.CreatedBy = c.creator })
			req := httptest.NewRequest(http.MethodPut, "/", nil).WithContext(c.who(context.Background()))
			if previewConsolePerson(req) {
				t.Fatalf("%s reads as a console person", c.name)
			}
			g.refused(c.name, g.put(c.who, "3000", `{}`), http.StatusForbidden, reasonPreviewConsoleOnly)
			if g.st.liveCount() != 0 {
				t.Errorf("%s created a preview", c.name)
			}
		})
	}
	req := httptest.NewRequest(http.MethodPut, "/", nil).WithContext(previewOwner(context.Background()))
	if !previewConsolePerson(req) {
		t.Error("a signed-in person does not read as a console person")
	}
}

// TestPreviewOrgTicketWho: an org preview's ticket goes to the run's owner as an owner ticket and to any
// other console person as the preview:org caller; anyone else, and anyone at all on a private row, gets the
// run-not-found 404. In path mode an org row refuses its ticket, and an org create, with preview_needs_host_mode.
func TestPreviewOrgTicketWho(t *testing.T) {
	g := newPreviewRig(t)
	g.preview("org create", g.put(previewOwner, "3000", `{"visibility":"org"}`))
	if tk := g.stored(g.ticket(previewOwner, "3000")); tk.Caller != "" || tk.App != "__preview-3000" {
		t.Fatalf("owner's ticket = %+v, want an owner ticket", tk)
	}
	tk := g.stored(g.ticket(previewMember, "3000"))
	if tk.Caller != previewCallerOrg || tk.App != "__preview-3000" || tk.Role != oidc.RoleUser || tk.Principal != "bob" {
		t.Fatalf("a member's ticket = %+v, want caller %s for __preview-3000, member-stamped", tk, previewCallerOrg)
	}
	if rows := g.h.audit.auditRows("preview.open", "success"); len(rows) != 2 || !strings.Contains(string(rows[1].Data), `"caller":"preview:org"`) {
		t.Fatalf("preview.open rows = %+v, want the second naming preview:org", rows)
	}
	for _, c := range []struct {
		what string
		who  previewWho
	}{
		{"a service identity", func(ctx context.Context) context.Context { return withServiceIdentity(ctx, "payments", "alice") }},
		{"a portal's delegated token", func(ctx context.Context) context.Context {
			return audit.WithDelegation(previewMember(ctx), types.DelegationVia{Delegate: uuid.New(), Grant: uuid.New()})
		}},
		{"an issuer portal's person", previewPerson("delegate:person:abcdefghijklmnopqrstuvwxyz:abcd", oidc.RoleUser)},
	} {
		if rec := g.ticket(c.who, "3000"); rec.Code != http.StatusNotFound {
			t.Errorf("%s's ticket: %d %s, want the run-not-found 404", c.what, rec.Code, rec.Body.String())
		}
	}
	g.preview("private create", g.put(previewOwner, "3001", `{}`))
	if rec := g.ticket(previewMember, "3001"); rec.Code != http.StatusNotFound {
		t.Errorf("a member's ticket to a private row: %d %s, want the run-not-found 404", rec.Code, rec.Body.String())
	}
	g.h.srv.cfg.UIOriginTemplate = ""
	g.refused("an org row's ticket in path mode", g.ticket(previewMember, "3000"), http.StatusForbidden, reasonPreviewNeedsHostMode)
	g.refused("an org create in path mode", g.put(previewOwner, "3002", `{"visibility":"org"}`), http.StatusForbidden, reasonPreviewNeedsHostMode)
	g.preview("a private create in path mode", g.put(previewOwner, "3002", `{}`))
}

// TestPreviewPortRefusals: a preview's port is 1024 to 65535 and not the proxy's 3128, written as a plain
// decimal (400 preview_port_reserved), and not a declared web app's (409 preview_port_declared).
func TestPreviewPortRefusals(t *testing.T) {
	g := newPreviewRig(t)
	for _, port := range []string{"0", "1", "80", "1023", "65536", "3128", "-3000", "+3000", "03000", "3000.0", "abc"} {
		g.refused("port "+port, g.put(previewOwner, port, `{}`), http.StatusBadRequest, reasonPreviewPortReserved)
	}
	g.refused("the declared app's port", g.put(previewOwner, "8080", `{}`), http.StatusConflict, reasonPreviewPortDeclared)
	for _, port := range []string{"1024", "3127", "3129", "65535"} {
		if p := g.preview("port "+port, g.put(previewOwner, port, `{}`)); p.Port <= 0 {
			t.Errorf("port %s stored as %d", port, p.Port)
		}
	}
	if rec := g.do(previewOwner, http.MethodDelete, g.path("80"), ""); rec.Code != http.StatusBadRequest || respondedReason(t, rec) != reasonPreviewPortReserved {
		t.Errorf("revoke of port 80: %d %s, want 400 %s", rec.Code, rec.Body.String(), reasonPreviewPortReserved)
	}
	if rec := g.ticket(previewOwner, "80"); rec.Code != http.StatusNotFound {
		t.Errorf("ticket of port 80: %d %s, want the run-not-found 404", rec.Code, rec.Body.String())
	}
}

// TestPreviewProfileRefusals (the routes): a run that is not running refuses with 409 preview_not_running, a
// background-only run with 409 run_background_only, and a profile that denies UI apps or interactive sessions
// with 403 preview_profile, at the create and at the ticket.
func TestPreviewProfileRefusals(t *testing.T) {
	g := newPreviewRig(t)
	g.preview("create", g.put(previewOwner, "3000", `{}`))
	set := walledRun(g.h)
	for _, l := range []types.GovernanceLimits{{DenyUIApps: true}, {DenyInteractive: true}} {
		set(l)
		g.refused("create under the profile", g.put(previewOwner, "3001", `{}`), http.StatusForbidden, reasonPreviewProfile)
		g.refused("ticket under the profile", g.ticket(previewOwner, "3000"), http.StatusForbidden, reasonPreviewProfile)
	}
	set(types.GovernanceLimits{})
	g.set(func(run *types.AgentRun) { run.Experience = types.ExperienceBackground })
	g.refused("create, background run", g.put(previewOwner, "3001", `{}`), http.StatusConflict, reasonRunBackgroundOnly)
	g.refused("ticket, background run", g.ticket(previewOwner, "3000"), http.StatusConflict, reasonRunBackgroundOnly)
	g.set(func(run *types.AgentRun) { run.Experience, run.State = types.ExperienceInteractive, types.RunStopped })
	g.refused("create, stopped run", g.put(previewOwner, "3001", `{}`), http.StatusConflict, reasonPreviewNotRunning)
	g.refused("ticket, stopped run", g.ticket(previewOwner, "3000"), http.StatusConflict, reasonPreviewNotRunning)
}

// TestPreviewDataOut: a preview needs the run's data_out rule allowed, at the create and at the ticket.
func TestPreviewDataOut(t *testing.T) {
	g := newPreviewRig(t)
	g.preview("create", g.put(previewOwner, "3000", `{}`))
	for _, rule := range []types.TransferRule{types.TransferReview, types.TransferBlocked} {
		g.h.store.putEffectivePolicy(g.h.run.ID, types.RunPolicySpec{MinConfinementClass: types.CC2, DataOut: rule,
			UIApps: []types.UIApp{{Name: "code", Port: uiTestPort, Path: "/ide"}}})
		g.refused("create, data_out "+string(rule), g.put(previewOwner, "3001", `{}`), http.StatusForbidden, reasonPreviewDataOut)
		g.refused("ticket, data_out "+string(rule), g.ticket(previewOwner, "3000"), http.StatusForbidden, reasonPreviewDataOut)
	}
}

// previewSiteStore is the site config PUT's store with the run_previews table.
type previewSiteStore struct {
	*fakeSiteConfigStore
	*previewRows
}

// TestPreviewCeilingLoweredRevokes: each saved write that changes features.preview_urls revokes the rows
// above the ceiling it leaves (every row once previews are off), as the writer, and drops this replica's cached
// inbound sessions. A revoke that fails writes a preview.revoke failure row and is run again by the next write that
// changes the key; a write that does not change the key does not call the hook.
func TestPreviewCeilingLoweredRevokes(t *testing.T) {
	withPreviewsAvailable(t)
	st := &previewSiteStore{fakeSiteConfigStore: &fakeSiteConfigStore{cfg: previewSite(true, "org")}, previewRows: &previewRows{}}
	h := newHarness(t)
	cfg := baseTestConfig(h, st)
	cfg.UIListenAddr, cfg.UISessionKey, cfg.UIOriginTemplate = ":8081", make([]byte, 32), "https://{run}.ui.example.com"
	srv := New(cfg)
	put := func(what, body string) {
		t.Helper()
		if w := do(t, srv, http.MethodPut, "/api/v1/site-config", adminToken, body); w.Code != http.StatusOK {
			t.Fatalf("%s: %d %s, want 200", what, w.Code, w.Body.String())
		}
	}
	revokes := func(outcome string) []types.AuditEvent {
		var out []types.AuditEvent
		for _, ev := range h.audit.snapshot() {
			if ev.Action == "preview.revoke" && ev.Outcome == outcome {
				out = append(out, ev)
			}
		}
		return out
	}
	runID := uuid.New()
	st.putPreview(runID, 3000, types.PreviewPrivate)
	org := st.putPreview(runID, 3001, types.PreviewOrg)

	put("lower the ceiling to private", `{"features":{"preview_urls":{"enabled":true,"max_visibility":"private"}}}`)
	if rows := revokes("success"); !slices.Equal(st.above, []types.PreviewVisibility{types.PreviewPrivate}) || len(rows) != 1 ||
		*rows[0].RunID != runID || !strings.Contains(string(rows[0].Data), org.ID.String()) || !strings.Contains(string(rows[0].Data), `"ceiling":"private"`) {
		t.Fatalf("after lowering to private: asked %v, revoke rows %+v; want the org row revoked under private", st.above, rows)
	}
	if st.liveCount() != 1 || srv.uiInbound.drops.Load() != 1 {
		t.Fatalf("after lowering: %d live, %d drops; want the private row and one drop", st.liveCount(), srv.uiInbound.drops.Load())
	}

	put("a write that does not change the key", `{"features":{"org_sites":{"enabled":false}}}`)
	if len(st.above) != 1 || srv.uiInbound.drops.Load() != 1 {
		t.Fatalf("a write that leaves preview_urls as it is called the hook: asked %v, %d drops", st.above, srv.uiInbound.drops.Load())
	}

	st.aboveErrs = []error{errors.New("database is down")}
	put("switch off, the revoke fails", `{"features":{"preview_urls":{"enabled":false}}}`)
	if fails := revokes("failure"); len(fails) != 1 || fails[0].RunID != nil || st.liveCount() != 1 || srv.uiInbound.drops.Load() != 2 {
		t.Fatalf("a failed revoke: failure rows %+v, %d live, %d drops; want one failure row, the row kept, a drop", fails, st.liveCount(), srv.uiInbound.drops.Load())
	}

	put("the next write that changes the key", `{"features":{"preview_urls":{"enabled":false,"max_visibility":"org"}}}`)
	if want := []types.PreviewVisibility{types.PreviewPrivate, "", ""}; !slices.Equal(st.above, want) || st.liveCount() != 0 || len(revokes("success")) != 2 {
		t.Fatalf("the retry: asked %v, %d live, %d success rows; want %v, none live, the private row revoked", st.above, st.liveCount(), len(revokes("success")), want)
	}
}

// TestPreviewPublicUnavailableUntilServed (until public links are served): max_visibility public is refused
// with site_config_invalid, and a stored public reads private, in previewCeiling and in setup's
// preview_max_visibility alike, so a public create is refused above the ceiling.
func TestPreviewPublicUnavailableUntilServed(t *testing.T) {
	g := newPreviewRig(t)
	st := &previewSiteStore{fakeSiteConfigStore: &fakeSiteConfigStore{cfg: previewSite(true, "org")}, previewRows: &previewRows{}}
	hs := newHarness(t)
	cfg := baseTestConfig(hs, st)
	cfg.UIListenAddr, cfg.UISessionKey, cfg.UIOriginTemplate = ":8081", make([]byte, 32), "https://{run}.ui.example.com"
	w := do(t, New(cfg), http.MethodPut, "/api/v1/site-config", adminToken, `{"features":{"preview_urls":{"enabled":true,"max_visibility":"public"}}}`)
	if w.Code != http.StatusBadRequest || respondedReason(t, w) != reasonSiteConfigInvalid || !strings.Contains(w.Body.String(), "not available in this release") {
		t.Fatalf("max_visibility public: %d %s, want 400 %s, not available in this release", w.Code, w.Body.String(), reasonSiteConfigInvalid)
	}
	stored := previewSite(true, "public")
	if got := previewCeiling(stored); got != types.PreviewPrivate {
		t.Errorf("previewCeiling(stored public) = %s, want private", got)
	}
	if got := string(setupFeatureFlags(t, stored)["preview_max_visibility"]); got != `"private"` {
		t.Errorf("setup's preview_max_visibility for a stored public = %s, want \"private\"", got)
	}
	g.st.setSite(stored, nil)
	g.refused("public create under a stored public", g.put(previewOwner, "3000", `{"visibility":"public"}`), http.StatusForbidden, reasonPreviewAboveCeiling)
}

// TestPreviewUnavailableUntilServed (until previews are served end to end): the key registers with its
// validate and afterWrite, but is unavailable, so a write that turns it on is refused feature_unavailable and a
// stored true reads off: the create answers preview_urls_off.
func TestPreviewUnavailableUntilServed(t *testing.T) {
	h, ok := siteFeatureHookFor(featurePreviewURLs)
	if !ok || h.validate == nil || h.afterWrite == nil {
		t.Fatal("preview_urls registers no hook with a validate and an afterWrite")
	}
	if siteFeatureAvailable(featurePreviewURLs) || previewURLsOn(previewSite(true, "org")) {
		t.Fatal("preview_urls is available before this release serves previews")
	}
	fake := &fakeSiteConfigStore{}
	wantSiteFeatureRefusal(t, putSiteFeatures(t, fake, `{"features":{"preview_urls":{"enabled":true}}}`), fake, reasonSiteFeatureUnavailable)

	hs := newUIHarness(t, okBackend())
	st := &previewTestStore{uiMemStore: hs.store, previewRows: &previewRows{}, site: previewSite(true, "org")}
	hs.srv.cfg.Store = st
	g := &previewRig{t: t, h: hs, st: st}
	g.refused("create while unavailable", g.put(previewOwner, "3000", `{}`), http.StatusForbidden, reasonPreviewURLsOff)
}
