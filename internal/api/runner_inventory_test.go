// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/federation"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func (f *runnerRegistrationFake) ListRunners(context.Context) ([]types.Runner, error) {
	f.lock.Lock()
	defer f.lock.Unlock()
	out := make([]types.Runner, 0, len(f.rows))
	for _, row := range f.rows {
		out = append(out, row)
	}
	slices.SortFunc(out, func(a, b types.Runner) int { return b.CreatedAt.Compare(a.CreatedAt) })
	return out, nil
}

func (f *runnerRegistrationFake) ListRunnersPage(ctx context.Context, filter types.RunnerFilter, now time.Time, p store.Page) ([]types.Runner, error) {
	all, _ := f.ListRunners(ctx)
	var out []types.Runner
	for _, row := range all {
		if runnerLapsed(row, now) || (filter == types.RunnerFilterActive && row.State == types.RunnerRevoked) ||
			(filter == types.RunnerFilterRevoked && row.State != types.RunnerRevoked) {
			continue
		}
		out = append(out, row)
	}
	return fakePage(out, p), nil
}

func fakePage[T any](rows []T, p store.Page) []T {
	if p.Offset >= len(rows) {
		return nil
	}
	rows = rows[p.Offset:]
	if p.Limit > 0 && len(rows) > p.Limit {
		rows = rows[:p.Limit]
	}
	return rows
}

func (f *runnerRegistrationFake) ListRunnersByOwner(_ context.Context, owner string) ([]types.Runner, error) {
	all, _ := f.ListRunners(context.Background())
	var out []types.Runner
	for _, row := range all {
		if row.Owner == owner {
			out = append(out, row)
		}
	}
	return out, nil
}

func (f *runnerRegistrationFake) ListUnusedRunnerRegistrationTokensPage(_ context.Context, now time.Time, owner string, p store.Page) ([]types.RunnerRegistrationToken, error) {
	f.lock.Lock()
	defer f.lock.Unlock()
	var out []types.RunnerRegistrationToken
	for _, tok := range f.tokens {
		if tok.ConsumedAt == nil && tok.ExpiresAt.After(now) && (owner == "" || tok.Owner == owner) {
			out = append(out, tok)
		}
	}
	slices.SortFunc(out, func(a, b types.RunnerRegistrationToken) int { return b.CreatedAt.Compare(a.CreatedAt) })
	return fakePage(out, p), nil
}

func (f *runnerRegistrationFake) ExpireUnclaimedRunners(_ context.Context, createdBefore time.Time) (int, error) {
	f.lock.Lock()
	defer f.lock.Unlock()
	n := 0
	for id, row := range f.rows {
		if row.State == types.RunnerUnclaimed && row.CreatedAt.Before(createdBefore) {
			delete(f.rows, id)
			n++
		}
	}
	return n, nil
}

func (f *runnerRegistrationFake) RevokeRunnerRegistrationToken(_ context.Context, id uuid.UUID, now time.Time) (types.RunnerRegistrationToken, error) {
	f.lock.Lock()
	defer f.lock.Unlock()
	for raw, tok := range f.tokens {
		if tok.ID == id && tok.ConsumedAt == nil && tok.ExpiresAt.After(now) {
			tok.ConsumedAt = &now
			f.tokens[raw] = tok
			return tok, nil
		}
	}
	return types.RunnerRegistrationToken{}, store.ErrNotFound
}

func (f *runnerRegistrationFake) CountActiveRunsByRunner(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]int, error) {
	out := map[uuid.UUID]int{}
	for _, id := range ids {
		if n := f.runs[id]; n > 0 {
			out[id] = n
		}
	}
	return out, nil
}

// PutSiteConfig persists, unlike the matrix double it embeds, so a test reads back what a door wrote.
func (f *runnerRegistrationFake) PutSiteConfig(_ context.Context, cfg types.SiteConfig) (types.SiteConfig, error) {
	f.authzStore.mu.Lock()
	defer f.authzStore.mu.Unlock()
	f.authzStore.siteCfg = cfg
	return cfg, nil
}

func (f *runnerRegistrationFake) ListAPITokensByPrincipal(_ context.Context, principal string) ([]types.APIToken, error) {
	if email := f.emails[principal]; email != "" {
		return []types.APIToken{{Principal: principal, Email: email}}, nil
	}
	return nil, nil
}

func (f *runnerRegistrationFake) runnersEnabled() bool {
	cfg, _ := f.authzStore.GetSiteConfig(context.Background())
	return runnersEnabled(cfg)
}

func (f *runnerRegistrationFake) addRunner(srv *Server, owner, name string, state types.RunnerState, age time.Duration) types.Runner {
	id := uuid.New()
	sha := sha256.Sum256(id[:])
	row := types.Runner{ID: id, Owner: owner, Name: name, State: state, KeyFingerprint: hex.EncodeToString(sha[:]), PublicKey: make([]byte, 32),
		CreatedAt: time.Now().UTC().Add(-age), OrgURLSHA256: federation.OrgURLSHA256(srv.cfg.RunnerOrgURL), MintedBy: "admin"}
	f.lock.Lock()
	f.rows[id] = row
	f.lock.Unlock()
	return row
}

func runnerViewsOf(t *testing.T, w *httptest.ResponseRecorder) []types.RunnerView {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var out []types.RunnerView
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func viewIDs(views []types.RunnerView) map[uuid.UUID]types.RunnerView {
	out := map[uuid.UUID]types.RunnerView{}
	for _, v := range views {
		out[v.ID] = v
	}
	return out
}

func TestMeRunnersShowOnlyTheCallersOwn(t *testing.T) {
	srv, f, _ := newRunnerRegistrationServer(t)
	mine := f.addRunner(srv, "alice", "laptop", types.RunnerClaimed, time.Hour)
	waiting := f.addRunner(srv, "alice", "desk", types.RunnerUnclaimed, time.Hour)
	revoked := f.addRunner(srv, "alice", "old", types.RunnerRevoked, time.Hour)
	lapsed := f.addRunner(srv, "alice", "lapsed", types.RunnerUnclaimed, 25*time.Hour)
	theirs := f.addRunner(srv, "bob", "bobs", types.RunnerClaimed, time.Hour)
	theirsWaiting := f.addRunner(srv, "bob", "bobs-waiting", types.RunnerUnclaimed, time.Hour)
	alice := memberModeSSOSession(t, "alice", "alice@example.com", oidc.RoleUser, false)

	w := doSSO(t, srv, http.MethodGet, "/api/v1/me/runners", alice, "")
	got := viewIDs(runnerViewsOf(t, w))
	if len(got) != 2 || got[mine.ID].ID == uuid.Nil || got[waiting.ID].ID == uuid.Nil {
		t.Fatalf("alice sees %d runners, want exactly her claimed and waiting ones", len(got))
	}
	for _, other := range []types.Runner{theirs, theirsWaiting, revoked, lapsed} {
		if _, leaked := got[other.ID]; leaked {
			t.Fatalf("alice's list holds %s (%s)", other.Name, other.State)
		}
		if strings.Contains(w.Body.String(), other.Name) {
			t.Fatalf("alice's list names %q", other.Name)
		}
	}
	if got[mine.ID].Owner != "" || got[mine.ID].KeyFingerprintAbbreviated || got[mine.ID].KeyFingerprint != mine.KeyFingerprint {
		t.Fatalf("a claimed runner on its owner's list must show the whole fingerprint and no owner: %+v", got[mine.ID])
	}
	fp := waiting.KeyFingerprint
	if v := got[waiting.ID]; !v.KeyFingerprintAbbreviated || v.KeyFingerprint != fp[:8]+"…"+fp[len(fp)-4:] || v.ClaimExpiresAt == nil || v.MintedBy != "admin" {
		t.Fatalf("a waiting runner must show its first 8 and last 4 characters, its expiry and who minted: %+v", v)
	}
	if strings.Contains(w.Body.String(), fp) || strings.Contains(w.Body.String(), fp[:20]) {
		t.Fatal("the whole fingerprint of an unclaimed runner is in the person's own list")
	}
	detail := doSSO(t, srv, http.MethodGet, "/api/v1/me/runners/"+waiting.ID.String(), alice, "")
	if detail.Code != http.StatusOK || strings.Contains(detail.Body.String(), fp[:20]) {
		t.Fatalf("detail of an unclaimed runner on /me: %d, and it must not carry the whole fingerprint", detail.Code)
	}

	absent := doSSO(t, srv, http.MethodGet, "/api/v1/me/runners/"+uuid.NewString(), alice, "")
	for _, other := range []types.Runner{theirs, theirsWaiting, revoked, lapsed} {
		w := doSSO(t, srv, http.MethodGet, "/api/v1/me/runners/"+other.ID.String(), alice, "")
		if w.Code != http.StatusNotFound || w.Body.String() != absent.Body.String() {
			t.Fatalf("GET /me/runners/%s = %d %s, want the bytes of an absent id", other.Name, w.Code, w.Body.String())
		}
	}
	if w := doSSO(t, srv, http.MethodGet, "/api/v1/me/runners/"+mine.ID.String(), alice, ""); w.Code != http.StatusOK {
		t.Fatalf("own runner: %d", w.Code)
	}
}

func TestMeRunnersAreNotAnAdminsInventory(t *testing.T) {
	srv, f, _ := newRunnerRegistrationServer(t)
	f.addRunner(srv, "alice", "laptop", types.RunnerClaimed, time.Hour)
	admin := memberModeSSOSession(t, "carol", "carol@example.com", oidc.RoleAdmin, false)
	if got := runnerViewsOf(t, doSSO(t, srv, http.MethodGet, "/api/v1/me/runners", admin, "")); len(got) != 0 {
		t.Fatalf("an admin's own list holds another person's runner: %+v", got)
	}
	if w := do(t, srv, http.MethodGet, "/api/v1/me/runners", adminToken, ""); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), reasonRunnerSessionRequired) {
		t.Fatalf("admin bearer on /me/runners = %d %s", w.Code, w.Body.String())
	}
}

func TestRunnersListIsTheSecurityTiersAndNamesTheOwner(t *testing.T) {
	srv, f, _ := newRunnerRegistrationServer(t)
	claimed := f.addRunner(srv, "alice", "laptop", types.RunnerClaimed, time.Hour)
	waiting := f.addRunner(srv, "bob", "desk", types.RunnerUnclaimed, time.Hour)
	revoked := f.addRunner(srv, "alice", "old", types.RunnerRevoked, time.Hour)
	lapsed := f.addRunner(srv, "cara", "lapsed", types.RunnerUnclaimed, 25*time.Hour)
	f.runs = map[uuid.UUID]int{claimed.ID: 2}
	srv.cfg.RunnerOnline = func(id uuid.UUID) bool { return id == claimed.ID || id == waiting.ID }

	for _, path := range []string{"/api/v1/runners", "/api/v1/runners/" + claimed.ID.String()} {
		member := memberModeSSOSession(t, "alice", "alice@example.com", oidc.RoleUser, false)
		if w := doSSO(t, srv, http.MethodGet, path, member, ""); w.Code != http.StatusForbidden {
			t.Fatalf("a member read %s: %d", path, w.Code)
		}
	}

	sec := memberModeSSOSession(t, "sam", "sam@example.com", oidc.RoleSecurityAdmin, false)
	list := func(query string) map[uuid.UUID]types.RunnerView {
		return viewIDs(runnerViewsOf(t, doSSO(t, srv, http.MethodGet, "/api/v1/runners"+query, sec, "")))
	}
	active := list("")
	if len(active) != 2 || active[claimed.ID].ID == uuid.Nil || active[waiting.ID].ID == uuid.Nil {
		t.Fatalf("default filter must be active (claimed and waiting): %d rows", len(active))
	}
	if v := active[claimed.ID]; v.Owner != "alice" || !v.Online || v.RunsActive != 2 || v.KeyFingerprint != claimed.KeyFingerprint {
		t.Fatalf("claimed row: %+v", v)
	}
	if v := active[waiting.ID]; v.Owner != "bob" || v.Online || v.KeyFingerprintAbbreviated || v.KeyFingerprint != waiting.KeyFingerprint {
		t.Fatalf("a waiting runner is never online and an admin sees the whole fingerprint: %+v", v)
	}
	if got := list("?state=revoked"); len(got) != 1 || got[revoked.ID].ID == uuid.Nil {
		t.Fatalf("revoked filter: %d rows", len(got))
	}
	if got := list("?state=all"); len(got) != 3 || got[lapsed.ID].ID != uuid.Nil {
		t.Fatalf("all filter must hold every live row and no lapsed unclaimed one: %d rows", len(got))
	}
	if w := doSSO(t, srv, http.MethodGet, "/api/v1/runners?state=bogus", sec, ""); w.Code != http.StatusBadRequest {
		t.Fatalf("bad filter: %d", w.Code)
	}
	if w := doSSO(t, srv, http.MethodGet, "/api/v1/runners/"+revoked.ID.String(), sec, ""); w.Code != http.StatusOK {
		t.Fatalf("detail of a revoked runner: %d", w.Code)
	}
	if w := doSSO(t, srv, http.MethodGet, "/api/v1/runners/"+lapsed.ID.String(), sec, ""); w.Code != http.StatusNotFound {
		t.Fatalf("detail of a lapsed runner: %d", w.Code)
	}

	srv.cfg.RunnerOnline = nil
	if v := viewIDs(runnerViewsOf(t, doSSO(t, srv, http.MethodGet, "/api/v1/runners", sec, "")))[claimed.ID]; v.Online {
		t.Fatal("a runner is shown online with no hub to ask")
	}
	empty, _, _ := newRunnerRegistrationServer(t)
	if w := doSSO(t, empty, http.MethodGet, "/api/v1/runners", sec, ""); strings.TrimSpace(w.Body.String()) != "[]" {
		t.Fatalf("an empty inventory must be [] not null: %q", w.Body.String())
	}
}

func TestRunnerTokensSecurityAdminRevokesButCannotMint(t *testing.T) {
	srv, f, h := newRunnerRegistrationServer(t)
	now := time.Now().UTC()
	raw := newBearer("wdr_")
	tokenID := uuid.New()
	f.tokens[raw] = types.RunnerRegistrationToken{ID: tokenID, Owner: "alice", MintedBy: "admin", CreatedAt: now, ExpiresAt: now.Add(time.Hour), OrgURLSHA256: federation.OrgURLSHA256(srv.cfg.RunnerOrgURL)}
	sec := memberModeSSOSession(t, "sam", "sam@example.com", oidc.RoleSecurityAdmin, false)
	member := memberModeSSOSession(t, "alice", "alice@example.com", oidc.RoleUser, false)

	if w := doSSO(t, srv, http.MethodGet, "/api/v1/runners/tokens", member, ""); w.Code != http.StatusForbidden {
		t.Fatalf("member listed tokens: %d", w.Code)
	}
	if w := doSSO(t, srv, http.MethodDelete, "/api/v1/runners/tokens/"+tokenID.String(), member, ""); w.Code != http.StatusForbidden {
		t.Fatalf("member revoked a token: %d", w.Code)
	}
	if w := doSSO(t, srv, http.MethodPost, "/api/v1/runners/tokens", sec, `{"owner":"alice"}`); w.Code != http.StatusForbidden {
		t.Fatalf("security admin minted a token: %d", w.Code)
	}

	w := doSSO(t, srv, http.MethodGet, "/api/v1/runners/tokens", sec, "")
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), raw) || strings.Contains(w.Body.String(), `"token"`) {
		t.Fatalf("token list = %d %s: it must never carry a token value", w.Code, w.Body.String())
	}
	var listed []types.RunnerRegistrationToken
	if err := json.Unmarshal(w.Body.Bytes(), &listed); err != nil || len(listed) != 1 || listed[0].ID != tokenID || listed[0].Owner != "alice" {
		t.Fatalf("listed %+v err %v", listed, err)
	}

	// Revoking works while runners are off: a leaked token must always be cuttable.
	f.siteCfg.Runners = nil
	if w := doSSO(t, srv, http.MethodDelete, "/api/v1/runners/tokens/"+tokenID.String(), sec, ""); w.Code != http.StatusNoContent {
		t.Fatalf("revoke: %d %s", w.Code, w.Body.String())
	}
	ev := lastAuditEvent(t, h.audit.snapshot(), "runner.token.revoke")
	if ev.Target != tokenID.String() || strings.Contains(string(ev.Data), raw) || !strings.Contains(string(ev.Data), `"expires_at"`) {
		t.Fatalf("revoke audit: %+v", ev)
	}
	if w := doSSO(t, srv, http.MethodDelete, "/api/v1/runners/tokens/"+tokenID.String(), sec, ""); w.Code != http.StatusNotFound {
		t.Fatalf("second revoke: %d", w.Code)
	}
	f.siteCfg.Runners = &types.RunnerSettings{Enabled: true}
	if got := f.tokens[raw]; got.ConsumedAt == nil {
		t.Fatal("revoked token still redeemable in the store")
	}
	if w := doSSO(t, srv, http.MethodGet, "/api/v1/runners/tokens", sec, ""); strings.TrimSpace(w.Body.String()) != "[]" {
		t.Fatalf("revoked token still listed: %s", w.Body.String())
	}
}

func TestRunnersSwitchIsSuperAdminOnlyAndAudited(t *testing.T) {
	srv, f, h := newRunnerRegistrationServer(t)
	f.siteCfg.Runners = nil
	srv.cfg.ControlPlaneURL = "https://wardynd.internal:8443"
	admin := memberModeSSOSession(t, "carol", "carol@example.com", oidc.RoleAdmin, false)
	sec := memberModeSSOSession(t, "sam", "sam@example.com", oidc.RoleSecurityAdmin, false)
	member := memberModeSSOSession(t, "alice", "alice@example.com", oidc.RoleUser, false)
	for name, c := range map[string]*http.Cookie{"member": member, "security admin": sec} {
		if w := doSSO(t, srv, http.MethodPut, "/api/v1/runners/settings", c, `{"enabled":true}`); w.Code != http.StatusForbidden || f.runnersEnabled() {
			t.Fatalf("%s turned runners on: %d", name, w.Code)
		}
	}
	if w := doSSO(t, srv, http.MethodPut, "/api/v1/runners/settings", admin, `{}`); w.Code != http.StatusBadRequest || f.runnersEnabled() {
		t.Fatalf("an empty body switched something: %d", w.Code)
	}
	if w := doSSO(t, srv, http.MethodGet, "/api/v1/runners/settings", sec, ""); w.Code != http.StatusOK || strings.Contains(w.Body.String(), "true") {
		t.Fatalf("settings read: %d %s", w.Code, w.Body.String())
	}

	if w := doSSO(t, srv, http.MethodPut, "/api/v1/runners/settings", admin, `{"enabled":true}`); w.Code != http.StatusOK || !f.runnersEnabled() {
		t.Fatalf("super admin turn on: %d %s", w.Code, w.Body.String())
	}
	ev := lastAuditEvent(t, h.audit.snapshot(), "runners.enabled.set")
	var data struct{ Before, After bool }
	if err := json.Unmarshal(ev.Data, &data); err != nil || data.Before || !data.After || ev.Actor != "carol" {
		t.Fatalf("audit %+v data %s", ev, ev.Data)
	}
	if w := doSSO(t, srv, http.MethodGet, "/api/v1/runners/settings", sec, ""); !strings.Contains(w.Body.String(), `"enabled":true`) {
		t.Fatalf("settings read after: %s", w.Body.String())
	}
	if w := doSSO(t, srv, http.MethodPut, "/api/v1/runners/settings", admin, `{"enabled":false}`); w.Code != http.StatusOK || f.runnersEnabled() {
		t.Fatalf("turn off: %d", w.Code)
	}
}

func TestRunnersSwitchRefusesAnOrgURLThatIsNotHTTPS(t *testing.T) {
	admin := memberModeSSOSession(t, "carol", "carol@example.com", oidc.RoleAdmin, false)
	for _, tc := range []struct {
		name, orgURL, controlURL string
		want                     int
	}{
		{"https", "https://org.example.com", "", http.StatusOK},
		{"loopback http for development", "http://127.0.0.1:8080", "", http.StatusOK},
		{"plain http", "http://org.example.com", "", http.StatusConflict},
		{"unset", "", "", http.StatusConflict},
		{"userinfo", "https://user:pw@org.example.com", "", http.StatusConflict},
		{"plain http control plane", "https://org.example.com", "http://wardynd.internal:8080", http.StatusConflict},
		{"https control plane", "https://org.example.com", "https://wardynd.internal:8443", http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, f, h := newRunnerRegistrationServer(t)
			f.siteCfg.Runners = nil
			srv.cfg.RunnerOrgURL, srv.cfg.ControlPlaneURL = tc.orgURL, tc.controlURL
			w := doSSO(t, srv, http.MethodPut, "/api/v1/runners/settings", admin, `{"enabled":true}`)
			if w.Code != tc.want || f.runnersEnabled() != (tc.want == http.StatusOK) {
				t.Fatalf("%d %s (enabled=%v)", w.Code, w.Body.String(), f.runnersEnabled())
			}
			if tc.want != http.StatusOK {
				if !strings.Contains(w.Body.String(), reasonRunnersOrgURLInvalid) {
					t.Fatalf("refusal carries no reason: %s", w.Body.String())
				}
				for _, ev := range h.audit.snapshot() {
					if ev.Action == "runners.enabled.set" {
						t.Fatal("a refused switch wrote an audit row")
					}
				}
			}
		})
	}
	srv, f, _ := newRunnerRegistrationServer(t)
	f.siteCfg.Runners = &types.RunnerSettings{Enabled: true}
	srv.cfg.RunnerOrgURL, srv.cfg.ControlPlaneURL = "http://org.example.com", "http://wardynd.internal:8080"
	if w := doSSO(t, srv, http.MethodPut, "/api/v1/runners/settings", admin, `{"enabled":false}`); w.Code != http.StatusOK || f.runnersEnabled() {
		t.Fatalf("turning off must never be refused: %d", w.Code)
	}
}

func TestRunnerPresenceAuditRows(t *testing.T) {
	srv, _, h := newRunnerRegistrationServer(t)
	id := uuid.New()
	seen := time.Now().UTC()
	srv.RecordRunnerConnected(context.Background(), id, "0.9.0", true)
	srv.RecordRunnerDisconnected(context.Background(), id, &seen)
	for _, action := range []string{"runner.connect", "runner.disconnect"} {
		ev := lastAuditEvent(t, h.audit.snapshot(), action)
		if ev.ActorType != types.ActorSystem || ev.Actor != "runner:"+id.String() || ev.Target != id.String() {
			t.Fatalf("%s: %+v", action, ev)
		}
	}
	if data := lastAuditEvent(t, h.audit.snapshot(), "runner.connect").Data; !strings.Contains(string(data), `"resumed":true`) || !strings.Contains(string(data), `"version":"0.9.0"`) {
		t.Fatalf("runner.connect data = %s, want version and resumed (PLAN §12.2)", data)
	}
}

// Every credential that acts for someone other than a signed-in person is refused on the /me reads,
// with the reason that names the missing session.
func TestMeRunnersRefuseNonPersonCredentials(t *testing.T) {
	srv, f, _ := newRunnerRegistrationServer(t)
	row := f.addRunner(srv, "alice", "laptop", types.RunnerClaimed, time.Hour)
	for name, with := range map[string]func(context.Context) context.Context{
		"device": func(c context.Context) context.Context {
			return context.WithValue(c, deviceCtxKey{}, types.Device{ID: uuid.New()})
		},
		"delegated": func(c context.Context) context.Context { return audit.WithDelegation(c, types.DelegationVia{}) },
		"scim":      func(c context.Context) context.Context { return withSCIMCaller(c, "slot") },
	} {
		for _, path := range []string{"/me/runners", "/me/runners/" + row.ID.String()} {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			ctx := with(withOIDCHuman(req.Context(), "alice"))
			w := httptest.NewRecorder()
			chi := chiRouteCtx(ctx, row.ID.String())
			if strings.HasSuffix(path, "/runners") {
				srv.handleListMyRunners(w, req.WithContext(chi))
			} else {
				srv.handleGetMyRunner(w, req.WithContext(chi))
			}
			if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), reasonRunnerSessionRequired) {
				t.Errorf("%s on %s = %d %s", name, path, w.Code, w.Body.String())
			}
		}
	}
}

func chiRouteCtx(ctx context.Context, id string) context.Context {
	rc := chi.NewRouteContext()
	rc.URLParams.Add("id", id)
	return context.WithValue(ctx, chi.RouteCtxKey, rc)
}

func TestRunnersAndTokensListPage(t *testing.T) {
	srv, f, _ := newRunnerRegistrationServer(t)
	for i := range 5 {
		f.addRunner(srv, "alice", "r"+string(rune('a'+i)), types.RunnerClaimed, time.Duration(i+1)*time.Hour)
	}
	now := time.Now().UTC()
	org := federation.OrgURLSHA256(srv.cfg.RunnerOrgURL)
	for i, owner := range []string{"alice", "alice", "bob"} {
		f.tokens[newBearer("wdr_")] = types.RunnerRegistrationToken{ID: uuid.New(), Owner: owner, MintedBy: "admin", CreatedAt: now.Add(-time.Duration(i) * time.Minute), ExpiresAt: now.Add(time.Hour), OrgURLSHA256: org}
	}
	sec := memberModeSSOSession(t, "sam", "sam@example.com", oidc.RoleSecurityAdmin, false)

	w := doSSO(t, srv, http.MethodGet, "/api/v1/runners?limit=2", sec, "")
	if got := runnerViewsOf(t, w); len(got) != 2 || w.Header().Get("X-Wardyn-Truncated") != "true" {
		t.Fatalf("limit=2: %d rows, truncated=%q", len(got), w.Header().Get("X-Wardyn-Truncated"))
	}
	w = doSSO(t, srv, http.MethodGet, "/api/v1/runners?limit=2&offset=4", sec, "")
	if got := runnerViewsOf(t, w); len(got) != 1 || w.Header().Get("X-Wardyn-Truncated") != "" {
		t.Fatalf("last page: %d rows, truncated=%q", len(got), w.Header().Get("X-Wardyn-Truncated"))
	}
	if w := doSSO(t, srv, http.MethodGet, "/api/v1/runners?limit=-1", sec, ""); w.Code != http.StatusBadRequest {
		t.Fatalf("bad limit: %d", w.Code)
	}

	count := func(query string) (int, string) {
		w := doSSO(t, srv, http.MethodGet, "/api/v1/runners/tokens"+query, sec, "")
		var out []types.RunnerRegistrationToken
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || w.Code != http.StatusOK {
			t.Fatalf("tokens%s: %d %s", query, w.Code, w.Body.String())
		}
		return len(out), w.Header().Get("X-Wardyn-Truncated")
	}
	if n, _ := count(""); n != 3 {
		t.Fatalf("all tokens = %d", n)
	}
	if n, _ := count("?owner=alice"); n != 2 {
		t.Fatalf("owner filter = %d, want 2", n)
	}
	if n, tr := count("?limit=1"); n != 1 || tr != "true" {
		t.Fatalf("token page = %d truncated=%q", n, tr)
	}
	if n, _ := count("?owner=nobody"); n != 0 {
		t.Fatalf("unknown owner = %d", n)
	}
}

func TestRunnersSwitchOffCallsTheHookExactlyOnTheTransition(t *testing.T) {
	srv, f, _ := newRunnerRegistrationServer(t)
	srv.cfg.ControlPlaneURL = "https://wardynd.internal:8443"
	var calls int
	srv.cfg.RunnersDisabled = func(context.Context) { calls++ }
	admin := memberModeSSOSession(t, "carol", "carol@example.com", oidc.RoleAdmin, false)
	put := func(body string) int {
		return doSSO(t, srv, http.MethodPut, "/api/v1/runners/settings", admin, body).Code
	}
	f.siteCfg.Runners = &types.RunnerSettings{Enabled: false}
	for _, step := range []struct {
		body  string
		code  int
		calls int
	}{
		{`{"enabled":false}`, 200, 0}, // off while off
		{`{"enabled":true}`, 200, 0},  // on
		{`{"enabled":true}`, 200, 0},  // on while on
		{`{"enabled":false}`, 200, 1}, // the transition
		{`{"enabled":false}`, 200, 1}, // off while off again
		{`{}`, 400, 1},                // refused
		{`{"enabled":true}`, 200, 1},  // on again
		{`{"enabled":false}`, 200, 2}, // every transition fires, not just the first
	} {
		if code := put(step.body); code != step.code || calls != step.calls {
			t.Fatalf("PUT %s: code %d calls %d, want %d and %d", step.body, code, calls, step.code, step.calls)
		}
	}
	srv.cfg.RunnersDisabled = nil
	f.siteCfg.Runners = &types.RunnerSettings{Enabled: true}
	if put(`{"enabled":false}`) != 200 {
		t.Fatal("a nil hook broke turning off")
	}
}

func TestRunnersSwitchWithoutEnabledNamesItsOwnReason(t *testing.T) {
	srv, _, _ := newRunnerRegistrationServer(t)
	admin := memberModeSSOSession(t, "carol", "carol@example.com", oidc.RoleAdmin, false)
	w := doSSO(t, srv, http.MethodPut, "/api/v1/runners/settings", admin, `{}`)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), reasonRunnersEnabledRequired) {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}

func TestWatcherTickExpiresLapsedUnclaimedRunners(t *testing.T) {
	srv, f, _ := newRunnerRegistrationServer(t)
	lapsed := f.addRunner(srv, "alice", "old", types.RunnerUnclaimed, 25*time.Hour)
	live := f.addRunner(srv, "alice", "new", types.RunnerUnclaimed, time.Hour)
	claimed := f.addRunner(srv, "alice", "kept", types.RunnerClaimed, 48*time.Hour)
	recent := time.Now()
	if err := srv.runWatcherTick(context.Background(), &recent); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if _, ok := f.rows[lapsed.ID]; ok {
		t.Fatal("the periodic sweep left a lapsed unclaimed runner")
	}
	if _, ok := f.rows[live.ID]; !ok {
		t.Fatal("the sweep deleted a runner still inside its wait")
	}
	if _, ok := f.rows[claimed.ID]; !ok {
		t.Fatal("the sweep deleted a claimed runner")
	}
}

func TestMeRunnersNameTheMinterByEmailWhenSomeoneElseMinted(t *testing.T) {
	srv, f, _ := newRunnerRegistrationServer(t)
	f.emails = map[string]string{"admin-sub": "admin@example.com"}
	byAdmin := f.addRunner(srv, "alice", "from-admin", types.RunnerUnclaimed, time.Hour)
	byAdminRow := f.rows[byAdmin.ID]
	byAdminRow.MintedBy = "admin-sub"
	f.rows[byAdmin.ID] = byAdminRow
	own := f.addRunner(srv, "alice", "own", types.RunnerUnclaimed, time.Hour)
	ownRow := f.rows[own.ID]
	ownRow.MintedBy = "alice"
	f.rows[own.ID] = ownRow
	unknown := f.addRunner(srv, "alice", "who", types.RunnerUnclaimed, time.Hour) // minter "admin": no email held

	alice := memberModeSSOSession(t, "alice", "alice@example.com", oidc.RoleUser, false)
	got := viewIDs(runnerViewsOf(t, doSSO(t, srv, http.MethodGet, "/api/v1/me/runners", alice, "")))
	if v := got[byAdmin.ID]; v.MintedBy != "admin-sub" || v.MintedByEmail != "admin@example.com" {
		t.Fatalf("a token an admin made must name the admin: %+v", v)
	}
	if v := got[own.ID]; v.MintedByEmail != "" {
		t.Fatalf("a token the owner made needs no name: %+v", v)
	}
	if v := got[unknown.ID]; v.MintedBy != "admin" || v.MintedByEmail != "" {
		t.Fatalf("with no email held the subject stays and the read still succeeds: %+v", v)
	}
	sec := memberModeSSOSession(t, "sam", "sam@example.com", oidc.RoleSecurityAdmin, false)
	if v := viewIDs(runnerViewsOf(t, doSSO(t, srv, http.MethodGet, "/api/v1/runners", sec, "")))[byAdmin.ID]; v.MintedByEmail != "" {
		t.Fatal("the admin view needs no minter email")
	}
}
