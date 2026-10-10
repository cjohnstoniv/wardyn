// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

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
	return out, nil
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

func (f *runnerRegistrationFake) ListUnusedRunnerRegistrationTokens(_ context.Context, now time.Time) ([]types.RunnerRegistrationToken, error) {
	f.lock.Lock()
	defer f.lock.Unlock()
	out := []types.RunnerRegistrationToken{}
	for _, tok := range f.tokens {
		if tok.ConsumedAt == nil && tok.ExpiresAt.After(now) {
			out = append(out, tok)
		}
	}
	return out, nil
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

func (f *runnerRegistrationFake) CountActiveRunsByRunner(context.Context) (map[uuid.UUID]int, error) {
	return f.runs, nil
}

// PutSiteConfig persists, unlike the matrix double it embeds, so a test reads back what a door wrote.
func (f *runnerRegistrationFake) PutSiteConfig(_ context.Context, cfg types.SiteConfig) (types.SiteConfig, error) {
	f.authzStore.mu.Lock()
	defer f.authzStore.mu.Unlock()
	f.authzStore.siteCfg = cfg
	return cfg, nil
}

func (f *runnerRegistrationFake) runnersEnabled() bool {
	cfg, _ := f.authzStore.GetSiteConfig(context.Background())
	return runnersEnabled(cfg)
}

func (f *runnerRegistrationFake) addRunner(srv *Server, owner, name string, state types.RunnerState, age time.Duration) types.Runner {
	id := uuid.New()
	row := types.Runner{ID: id, Owner: owner, Name: name, State: state, KeyFingerprint: strings.Repeat("ab", 32), PublicKey: make([]byte, 32),
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
	if v := got[waiting.ID]; !v.KeyFingerprintAbbreviated || len(v.KeyFingerprint) >= len(waiting.KeyFingerprint) || v.ClaimExpiresAt == nil || v.MintedBy != "admin" {
		t.Fatalf("a waiting runner must show an abbreviated fingerprint, its expiry and who minted: %+v", v)
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
	if ev.Target != tokenID.String() || strings.Contains(string(ev.Data), raw) {
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
	srv.RecordRunnerConnected(context.Background(), id, "0.9.0")
	srv.RecordRunnerDisconnected(context.Background(), id, &seen)
	for _, action := range []string{"runner.connect", "runner.disconnect"} {
		ev := lastAuditEvent(t, h.audit.snapshot(), action)
		if ev.ActorType != types.ActorSystem || ev.Actor != "runner:"+id.String() || ev.Target != id.String() {
			t.Fatalf("%s: %+v", action, ev)
		}
	}
}
