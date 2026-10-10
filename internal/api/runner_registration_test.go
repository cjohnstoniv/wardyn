// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/federation"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/google/uuid"
)

type runnerRegistrationFake struct {
	*authzStore
	store.RunnerStore
	lock   sync.Mutex
	tokens map[string]types.RunnerRegistrationToken
	rows   map[uuid.UUID]types.Runner
}

func newRunnerRegistrationServer(t *testing.T) (*Server, *runnerRegistrationFake, *harness) {
	t.Helper()
	f := &runnerRegistrationFake{authzStore: newAuthzStore(), tokens: map[string]types.RunnerRegistrationToken{}, rows: map[uuid.UUID]types.Runner{}}
	h := newHarness(t)
	cfg := baseTestConfig(h, f)
	cfg.OIDC = &oidc.Authenticator{}
	cfg.Secrets = getErrStore{getErr: secretstore.ErrNotFound}
	cfg.RunnerOrgURL = "https://org.example.com"
	return New(cfg), f, h
}

func (f *runnerRegistrationFake) MintRunnerRegistrationToken(_ context.Context, raw string, token types.RunnerRegistrationToken) (types.RunnerRegistrationToken, error) {
	f.lock.Lock()
	defer f.lock.Unlock()
	f.tokens[raw] = token
	return token, nil
}
func (f *runnerRegistrationFake) ConsumeRunnerRegistrationToken(_ context.Context, raw, hash string, now time.Time) (types.RunnerRegistrationToken, bool, error) {
	f.lock.Lock()
	defer f.lock.Unlock()
	token, ok := f.tokens[raw]
	if !ok || token.ConsumedAt != nil || !token.ExpiresAt.After(now) || token.OrgURLSHA256 != hash {
		return types.RunnerRegistrationToken{}, false, nil
	}
	token.ConsumedAt = &now
	f.tokens[raw] = token
	return token, true, nil
}
func (f *runnerRegistrationFake) CreateRunner(_ context.Context, row types.Runner) (types.Runner, error) {
	f.lock.Lock()
	defer f.lock.Unlock()
	for _, old := range f.rows {
		if old.KeyFingerprint == row.KeyFingerprint {
			return types.Runner{}, store.ErrConflict
		}
	}
	row.State = types.RunnerUnclaimed
	row.CreatedAt = time.Now().UTC()
	f.rows[row.ID] = row
	return row, nil
}
func (f *runnerRegistrationFake) GetRunner(_ context.Context, id uuid.UUID) (types.Runner, error) {
	f.lock.Lock()
	defer f.lock.Unlock()
	row, ok := f.rows[id]
	if !ok {
		return row, store.ErrNotFound
	}
	return row, nil
}
func (f *runnerRegistrationFake) ClaimRunner(_ context.Context, id uuid.UUID, owner, fp string, now time.Time) (types.Runner, error) {
	f.lock.Lock()
	defer f.lock.Unlock()
	row, ok := f.rows[id]
	if !ok {
		return row, store.ErrNotFound
	}
	if row.State != types.RunnerUnclaimed {
		return row, store.ErrConflict
	}
	if row.Owner != owner || row.KeyFingerprint != fp || !row.CreatedAt.Add(24*time.Hour).After(now) {
		return row, store.ErrRunnerClaimMismatch
	}
	row.State = types.RunnerClaimed
	row.ClaimedAt = &now
	f.rows[id] = row
	return row, nil
}

func TestRunnerRegistrationTokenTTLAndAdminBoundary(t *testing.T) {
	for _, tc := range []struct {
		name, path, body, sub, role string
		ttl                         time.Duration
		want                        int
	}{
		{"self", "/me/runners/tokens", "{}", "alice", oidc.RoleUser, time.Hour, 201},
		{"admin for person", "/runners/tokens", `{"owner":"alice"}`, "operator", oidc.RoleAdmin, 72 * time.Hour, 201},
		{"member cannot mint for another", "/runners/tokens", `{"owner":"bob"}`, "alice", oidc.RoleUser, 0, 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _, h := newRunnerRegistrationServer(t)
			cookie := memberModeSSOSession(t, tc.sub, tc.sub+"@example.com", tc.role, false)
			w := doSSO(t, srv, http.MethodPost, "/api/v1"+tc.path, cookie, tc.body)
			if w.Code != tc.want {
				t.Fatalf("status%d: %s", w.Code, w.Body.String())
			}
			if tc.want != 201 {
				return
			}
			var token types.RunnerRegistrationToken
			if err := json.Unmarshal(w.Body.Bytes(), &token); err != nil {
				t.Fatal(err)
			}
			if len(token.Token) != 68 || !strings.HasPrefix(token.Token, "wdr_") || token.Owner != "alice" || token.MintedBy != tc.sub || token.ExpiresAt.Sub(token.CreatedAt) != tc.ttl {
				t.Fatalf("bad token metadata: %+v", token)
			}
			ev := lastAuditEvent(t, h.audit.snapshot(), "runner.token.create")
			if strings.Contains(string(ev.Data), token.Token) {
				t.Fatal("raw token in audit")
			}
		})
	}
	srv, _, _ := newRunnerRegistrationServer(t)
	if w := do(t, srv, http.MethodPost, "/api/v1/me/runners/tokens", adminToken, "{}"); w.Code != 403 {
		t.Fatalf("admin bearer minted self token: %d", w.Code)
	}
}

func TestRunnerRegistrationAnonymousUnclaimedAndSingleUse(t *testing.T) {
	srv, f, h := newRunnerRegistrationServer(t)
	raw := newBearer("wdr_")
	now := time.Now().UTC()
	f.tokens[raw] = types.RunnerRegistrationToken{ID: uuid.New(), Owner: "private-owner", MintedBy: "private-admin", CreatedAt: now, ExpiresAt: now.Add(time.Hour), OrgURLSHA256: federation.OrgURLSHA256(srv.cfg.RunnerOrgURL)}
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(types.RunnerRegisterRequest{Token: raw, PublicKey: pub, Name: "laptop"})
	w := do(t, srv, http.MethodPost, "/api/v1/runners/register", "", string(body))
	if w.Code != 201 {
		t.Fatalf("register:%d %s", w.Code, w.Body.String())
	}
	for _, secret := range []string{"private-owner", "private-admin", raw, "owner", "minted_by"} {
		if strings.Contains(w.Body.String(), secret) {
			t.Fatalf("runner response exposed %q", secret)
		}
	}
	var result types.RunnerRegistration
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.State != types.RunnerUnclaimed || result.Fingerprint != runnerwire.Fingerprint(pub) {
		t.Fatalf("bad registration:%+v", result)
	}
	row := f.rows[result.RunnerID]
	if row.Owner != "private-owner" || row.State != types.RunnerUnclaimed || row.OrgURLSHA256 != federation.OrgURLSHA256(srv.cfg.RunnerOrgURL) {
		t.Fatal("registration lost owner/binding or auto-claimed")
	}
	if w := do(t, srv, http.MethodPost, "/api/v1/runners/register", "", string(body)); w.Code != 401 {
		t.Fatalf("token reused:%d", w.Code)
	}
	ev := lastAuditEvent(t, h.audit.snapshot(), "runner.enrol")
	if ev.ActorType != types.ActorSystem || strings.Contains(string(ev.Data), raw) {
		t.Fatal("registration audit identity or secret leak")
	}
}

func TestRunnerClaimOnlyOwnerAndObservedFingerprint(t *testing.T) {
	for _, tc := range []struct {
		name, sub, role, fp, bearer string
		state                       types.RunnerState
		want                        int
	}{
		{"owner", "alice", oidc.RoleUser, "fingerprint", "", types.RunnerUnclaimed, 200},
		{"admin personal own", "alice", oidc.RoleAdmin, "fingerprint", "", types.RunnerUnclaimed, 200},
		{"admin for other", "bob", oidc.RoleAdmin, "fingerprint", "", types.RunnerUnclaimed, 403},
		{"other owner", "bob", oidc.RoleUser, "fingerprint", "", types.RunnerUnclaimed, 403},
		{"wrong fingerprint", "alice", oidc.RoleUser, "wrong", "", types.RunnerUnclaimed, 403},
		{"revoked", "alice", oidc.RoleUser, "fingerprint", "", types.RunnerRevoked, 403},
		{"admin bearer", "", "", "fingerprint", adminToken, types.RunnerUnclaimed, 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, f, h := newRunnerRegistrationServer(t)
			id := uuid.New()
			f.rows[id] = types.Runner{ID: id, Owner: "alice", KeyFingerprint: "fingerprint", State: tc.state, CreatedAt: time.Now().UTC(), OrgURLSHA256: federation.OrgURLSHA256(srv.cfg.RunnerOrgURL)}
			body, _ := json.Marshal(types.RunnerClaimRequest{Fingerprint: tc.fp})
			path := "/api/v1/me/runners/" + id.String() + "/claim"
			cookie := memberModeSSOSession(t, tc.sub, tc.sub+"@example.com", tc.role, false)
			w := doSSO(t, srv, http.MethodPost, path, cookie, string(body))
			if tc.bearer != "" {
				w = do(t, srv, http.MethodPost, path, tc.bearer, string(body))
			}
			if w.Code != tc.want {
				t.Fatalf("claim:%d %s", w.Code, w.Body.String())
			}
			if tc.want == 200 {
				if f.rows[id].State != types.RunnerClaimed {
					t.Fatal("claim not stored")
				}
				lastAuditEvent(t, h.audit.snapshot(), "runner.claim")
			} else if f.rows[id].State != tc.state {
				t.Fatal("refused claim changed row")
			}
		})
	}
}

func TestRunnerRegistrationUnsetOrgURLFailsClosed(t *testing.T) {
	srv, _, _ := newRunnerRegistrationServer(t)
	srv.cfg.RunnerOrgURL = ""
	w := do(t, srv, http.MethodPost, "/api/v1/runners/register", "", "{}")
	if w.Code != 503 || !strings.Contains(w.Body.String(), "WARDYN_RUNNER_ORG_URL") {
		t.Fatalf("missing org configuration:%d %s", w.Code, w.Body.String())
	}
}

func TestRunnerRegistrationBoundTokenRefusalDoesNotConsume(t *testing.T) {
	srv, f, _ := newRunnerRegistrationServer(t)
	raw := newBearer("wdr_")
	now := time.Now().UTC()
	f.tokens[raw] = types.RunnerRegistrationToken{ID: uuid.New(), Owner: "alice", CreatedAt: now, ExpiresAt: now.Add(time.Hour), OrgURLSHA256: federation.OrgURLSHA256("https://other.example.com")}
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(types.RunnerRegisterRequest{Token: raw, PublicKey: pub, Name: "laptop"})
	w := do(t, srv, http.MethodPost, "/api/v1/runners/register", "", string(body))
	if w.Code != 401 || len(f.rows) != 0 || f.tokens[raw].ConsumedAt != nil {
		t.Fatalf("foreign-bound token spent or admitted:%d %s", w.Code, w.Body.String())
	}
}

func TestRunnerRegistrationCannotReactivateRevokedKey(t *testing.T) {
	srv, f, _ := newRunnerRegistrationServer(t)
	raw := newBearer("wdr_")
	now := time.Now().UTC()
	orgHash := federation.OrgURLSHA256(srv.cfg.RunnerOrgURL)
	f.tokens[raw] = types.RunnerRegistrationToken{ID: uuid.New(), Owner: "alice", CreatedAt: now, ExpiresAt: now.Add(time.Hour), OrgURLSHA256: orgHash}
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	oldID := uuid.New()
	f.rows[oldID] = types.Runner{ID: oldID, Owner: "alice", State: types.RunnerRevoked, KeyFingerprint: runnerwire.Fingerprint(pub), PublicKey: pub, OrgURLSHA256: orgHash}
	body, _ := json.Marshal(types.RunnerRegisterRequest{Token: raw, PublicKey: pub, Name: "laptop"})
	w := do(t, srv, http.MethodPost, "/api/v1/runners/register", "", string(body))
	if w.Code != 409 || len(f.rows) != 1 || f.rows[oldID].State != types.RunnerRevoked {
		t.Fatalf("revoked key reactivated:%d %s", w.Code, w.Body.String())
	}
}
