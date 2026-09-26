// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/ssh"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

type sshRegistrationStore struct {
	*sshMemStore
	tokens    sessionTokenStore
	beforeAdd func()
}

func (s *sshRegistrationStore) ListAPITokens(ctx context.Context) ([]types.APIToken, error) {
	return s.tokens.ListAPITokens(ctx)
}
func (s *sshRegistrationStore) ListAPITokensByPrincipal(ctx context.Context, p string) ([]types.APIToken, error) {
	return s.tokens.ListAPITokensByPrincipal(ctx, p)
}
func (s *sshRegistrationStore) RevokeAPIToken(ctx context.Context, id uuid.UUID, p string, at time.Time) (types.APIToken, error) {
	return s.tokens.RevokeAPIToken(ctx, id, p, at)
}
func (s *sshRegistrationStore) ListWorkspaces(context.Context) ([]types.Workspace, error) {
	return nil, nil
}
func (s *sshRegistrationStore) AddSSHKey(ctx context.Context, k types.SSHPublicKey) (types.SSHPublicKey, error) {
	if s.beforeAdd != nil {
		s.beforeAdd()
	}
	return s.sshMemStore.AddSSHKey(ctx, k)
}
func (s *sshRegistrationStore) DeleteSSHKeys(_ context.Context, p string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	count := 0
	for fp, key := range s.keys {
		if p == "" || key.Principal == p {
			delete(s.keys, fp)
			count++
		}
	}
	return count, nil
}

func sshRevocationCookie(t *testing.T, principal string, at time.Time) *http.Cookie {
	t.Helper()
	data, err := json.Marshal(oidc.Session{
		V: oidc.SessionCodecVersion, Sub: principal, Email: "alice@example.com",
		Role: oidc.RoleUser, UserType: types.UserTypeStandard,
		IssuedAt: at, Expiry: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	mac := hmac.New(sha256.New, accessTestHMACKey)
	mac.Write(data)
	return &http.Cookie{Name: "wardyn_session", Value: base64.RawURLEncoding.EncodeToString(data) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))}
}

func TestSSHRegistrationCannotOutliveSessionRevocation(t *testing.T) {
	for _, phase := range []string{"body", "insert"} {
		for _, target := range []string{"sub-alice", "alice@example.com", "all"} {
			t.Run(phase+"/"+target, func(t *testing.T) {
				testSSHRegistrationRevocation(t, phase, target)
			})
		}
	}
}

func testSSHRegistrationRevocation(t *testing.T, phase, target string) {
	t.Helper()
	mem, run, _ := sshOwnedRunningRun(t)
	const principal = "sub-alice"
	run.CreatedBy = principal
	mem.putRun(run)
	st := &sshRegistrationStore{sshMemStore: mem, tokens: sessionTokenStore{toks: []types.APIToken{
		{ID: uuid.New(), Principal: principal, Email: "alice@example.com"},
	}}}
	base := time.Now().UTC().Add(-time.Minute)
	var seconds atomic.Int64
	now := func() time.Time { return base.Add(time.Duration(seconds.Load()) * time.Second) }
	rev := newCutoffRevocations()
	rev.nowFunc = now
	auth := newAccessAuth(t, nil, oidc.RoleUser, nil, nil, func(c *oidc.Config) { c.Revocations = rev })
	h := newSSHTestHarness(t, st, &sshFakeRunner{execFn: func(runner.ExecSpec) (*runner.ExecSession, error) { return fakeEchoExecSession(), nil }}, func(c *Config) {
		c.OIDC, c.SessionRevocations, c.Now = auth, rev, now
	})
	priv, pub := mustSSHKeypair(t)
	fp := ssh.FingerprintSHA256(pub)
	mem.putKey(types.SSHPublicKey{Fingerprint: fp, Principal: principal, PublicKey: string(ssh.MarshalAuthorizedKey(pub)), CreatedAt: base.Add(-time.Second)})
	client, err := sshDial(t, h, run.ID.String(), priv)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	checkSSHNewChannels(t, client, true)

	payload, _ := json.Marshal(addSSHKeyRequest{PublicKey: string(ssh.MarshalAuthorizedKey(pub))})
	body := newHeldBody(string(payload))
	release := sync.OnceFunc(func() { close(body.release) })
	t.Cleanup(release)
	entered := body.entered
	if phase == "insert" {
		release()
		addEntered, addRelease := make(chan struct{}), make(chan struct{})
		release = sync.OnceFunc(func() { close(addRelease) })
		t.Cleanup(release)
		st.beforeAdd = sync.OnceFunc(func() { close(addEntered); <-addRelease })
		entered = addEntered
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/me/ssh-keys", body)
	req.AddCookie(sshRevocationCookie(t, principal, base.Add(-time.Second)))
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		w := httptest.NewRecorder()
		panicFails(t, h.srv.Handler()).ServeHTTP(w, req)
		done <- w
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("registration never reached the held boundary")
	}
	seconds.Store(1)
	revokeBody := `{"all":true}`
	if target != "all" {
		revokeBody = `{"sub":"` + target + `"}`
	}
	if w := do(t, h.srv, http.MethodPost, "/api/v1/sessions/revoke", adminToken, revokeBody); w.Code != http.StatusNoContent {
		t.Fatalf("revoke=%d body=%s", w.Code, w.Body.String())
	}
	seconds.Store(2)
	release()
	var registered *httptest.ResponseRecorder
	select {
	case registered = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("registration never completed")
	}
	if phase == "body" && registered.Code != http.StatusForbidden {
		t.Errorf("registration held across revoke=%d want 403; body=%s", registered.Code, registered.Body.String())
	}
	if phase == "insert" && registered.Code != http.StatusCreated {
		t.Fatalf("insert after the last check=%d want 201; body=%s", registered.Code, registered.Body.String())
	}
	checkSSHNewChannels(t, client, false)
	if late, err := sshDial(t, h, run.ID.String(), priv); err == nil {
		late.Close()
		t.Error("registration admitted before cutoff still authenticates after revoke")
	}
	if _, err := st.GetSSHKeyByFingerprint(context.Background(), fp); err == nil {
		if err := st.DeleteSSHKey(context.Background(), fp, principal); err != nil {
			t.Fatal(err)
		}
	}
	seconds.Store(3)
	w := doSSO(t, h.srv, http.MethodPost, "/api/v1/me/ssh-keys", sshRevocationCookie(t, principal, now()), string(payload))
	if w.Code != http.StatusCreated {
		t.Fatalf("fresh registration=%d body=%s", w.Code, w.Body.String())
	}
	fresh, err := sshDial(t, h, run.ID.String(), priv)
	if err != nil {
		t.Fatalf("fresh registration cannot authenticate: %v", err)
	}
	defer fresh.Close()
	checkSSHNewChannels(t, fresh, true)
}

func TestSSHGateway_SessionCutoffAndReadFailureRefuseAuthAndChannels(t *testing.T) {
	for _, mode := range []string{"sub", "all", "read_failure"} {
		t.Run(mode, func(t *testing.T) {
			st, run, principal := sshOwnedRunningRun(t)
			priv, pub := mustSSHKeypair(t)
			base := time.Now().UTC().Add(-time.Minute)
			key := types.SSHPublicKey{Fingerprint: ssh.FingerprintSHA256(pub), Principal: principal, PublicKey: string(ssh.MarshalAuthorizedKey(pub)), CreatedAt: base}
			st.putKey(key)
			rev := newCutoffRevocations()
			rev.nowFunc = func() time.Time { return base.Add(time.Second) }
			h := newSSHTestHarness(t, st, &sshFakeRunner{execFn: func(runner.ExecSpec) (*runner.ExecSession, error) { return fakeEchoExecSession(), nil }}, func(c *Config) { c.SessionRevocations = rev })
			client, err := sshDial(t, h, run.ID.String(), priv)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			checkSSHNewChannels(t, client, true)
			switch mode {
			case "sub":
				_ = rev.RevokeSub(context.Background(), principal)
			case "all":
				_ = rev.RevokeAll(context.Background())
			case "read_failure":
				rev.mu.Lock()
				rev.err = errors.New("cutoff store unavailable")
				rev.mu.Unlock()
			}
			checkSSHNewChannels(t, client, false)
			if late, err := sshDial(t, h, run.ID.String(), priv); err == nil {
				late.Close()
				t.Error("unanswerable or revoked key still authenticates")
			}
			rev.mu.Lock()
			rev.err = nil
			rev.mu.Unlock()
			key.CreatedAt = base.Add(2 * time.Second)
			st.putKey(key)
			fresh, err := sshDial(t, h, run.ID.String(), priv)
			if err != nil {
				t.Fatal(err)
			}
			defer fresh.Close()
			checkSSHNewChannels(t, fresh, true)
		})
	}
}

func TestSSHRegistration_CutoffReadFailureRefusesBeforeInsert(t *testing.T) {
	h := newHarness(t)
	st := newSSHMemStore()
	cfg := baseTestConfig(h, st)
	rev := newCutoffRevocations()
	rev.err = errors.New("cutoff store unavailable")
	cfg.SessionRevocations = rev
	srv := New(cfg)
	_, pub := mustSSHKeypair(t)
	body, _ := json.Marshal(addSSHKeyRequest{PublicKey: string(ssh.MarshalAuthorizedKey(pub))})
	w := do(t, srv, http.MethodPost, "/api/v1/me/ssh-keys", adminToken, string(body))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("register=%d want 500; body=%s", w.Code, w.Body.String())
	}
	if keys, _ := st.ListSSHKeysByPrincipal(context.Background(), adminTokenPrincipal); len(keys) != 0 {
		t.Fatalf("unanswerable cutoff inserted %+v", keys)
	}
}

type sshCanonicalCutoffFailure struct{ *fakeSessionRevocations }

func (s sshCanonicalCutoffFailure) RevokeSub(ctx context.Context, principal string) error {
	if principal == "sub-alice" {
		return errors.New("canonical cutoff unavailable")
	}
	return s.fakeSessionRevocations.RevokeSub(ctx, principal)
}

func TestRevokeSessions_CanonicalCutoffFailureStillDeletesCredentials(t *testing.T) {
	st := sshOffboardingStore()
	h := newHarness(t)
	cfg := baseTestConfig(h, st)
	cfg.OIDC = &oidc.Authenticator{}
	cfg.SessionRevocations = sshCanonicalCutoffFailure{&fakeSessionRevocations{}}
	srv := New(cfg)
	w := do(t, srv, http.MethodPost, "/api/v1/sessions/revoke", adminToken, `{"sub":"alice@example.com"}`)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("revoke=%d want500 body=%s", w.Code, w.Body.String())
	}
	if len(st.keys) != 1 || st.keys[0].Principal != "sub-bob" || len(st.revoked) == 0 {
		t.Fatalf("independent credential work was skipped: %+v", st)
	}
	assertSSHDeletionAudit(t, h.audit, "sub-alice", "success", 2)
	ev := lastAuditEvent(t, h.audit.events, "session.revoke")
	data := auditData(t, ev)
	if ev.Outcome != "failure" || data["ssh_keys_deleted"] != float64(2) || data["error"] != "canonical cutoff unavailable" {
		t.Fatalf("partial session audit=%+v data=%v", ev, data)
	}
}
