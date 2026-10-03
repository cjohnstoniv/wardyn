// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

type sessionSSHStore struct {
	sessionTokenStore
	keys         []types.SSHPublicKey
	deleteErr    error
	directoryErr error
	tokenErr     bool
}

func (s *sessionSSHStore) ListAPITokens(ctx context.Context) ([]types.APIToken, error) {
	if s.directoryErr != nil {
		return nil, s.directoryErr
	}
	return s.sessionTokenStore.ListAPITokens(ctx)
}

func (s *sessionSSHStore) ListAPITokensByPrincipal(ctx context.Context, p string) ([]types.APIToken, error) {
	if s.tokenErr {
		return nil, errors.New("token list unavailable")
	}
	return s.sessionTokenStore.ListAPITokensByPrincipal(ctx, p)
}

func (s *sessionSSHStore) ListSSHKeysByPrincipal(_ context.Context, p string) ([]types.SSHPublicKey, error) {
	var out []types.SSHPublicKey
	for _, k := range s.keys {
		if k.Principal == p {
			out = append(out, k)
		}
	}
	return out, nil
}

func (s *sessionSSHStore) DeleteSSHKeys(_ context.Context, p string) (int, error) {
	if s.deleteErr != nil {
		return 0, s.deleteErr
	}
	var keep []types.SSHPublicKey
	count := 0
	for _, k := range s.keys {
		if p == "" || k.Principal == p {
			count++
		} else {
			keep = append(keep, k)
		}
	}
	s.keys = keep
	return count, nil
}

func sshOffboardingServer(t *testing.T, st *sessionSSHStore) (*Server, *fakeSessionRevocations, *recRecorder) {
	t.Helper()
	h := newHarness(t)
	cfg := baseTestConfig(h, st)
	cfg.OIDC = &oidc.Authenticator{}
	cutoffs := &fakeSessionRevocations{}
	cfg.SessionRevocations = cutoffs
	return New(cfg), cutoffs, h.audit
}

func sshOffboardingStore() *sessionSSHStore {
	return &sessionSSHStore{
		sessionTokenStore: sessionTokenStore{toks: []types.APIToken{
			{Principal: "sub-alice", Email: "alice@example.com"},
			{Principal: "sub-bob", Email: "bob@example.com"},
		}},
		keys: []types.SSHPublicKey{
			{Fingerprint: "alice-1", Principal: "sub-alice"},
			{Fingerprint: "alice-2", Principal: "sub-alice"},
			{Fingerprint: "bob-1", Principal: "sub-bob"},
		},
	}
}

func TestRevokeSessions_DeletesSSHKeysForResolvedPrincipal(t *testing.T) {
	for _, target := range []string{"sub-alice", "alice@example.com", "ALICE@EXAMPLE.COM"} {
		t.Run(target, func(t *testing.T) {
			st := sshOffboardingStore()
			srv, cutoffs, audit := sshOffboardingServer(t, st)
			admin := ssoSession(t, "responder", "responder@example.com", oidc.RoleSecurityAdmin)
			body, _ := json.Marshal(map[string]string{"sub": target})
			w := doSSO(t, srv, http.MethodPost, "/api/v1/sessions/revoke", admin, string(body))
			if w.Code != http.StatusNoContent {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
			if len(st.keys) != 1 || st.keys[0].Principal != "sub-bob" {
				t.Fatalf("remaining SSH keys=%+v", st.keys)
			}
			if len(cutoffs.revokedSubs) == 0 {
				t.Fatal("session cutoff not written")
			}
			assertSSHDeletionAudit(t, audit, "sub-alice", "success", 2)
			for _, ev := range audit.snapshot() {
				if ev.Action == "session.revoke" {
					var data map[string]any
					if err := json.Unmarshal(ev.Data, &data); err != nil {
						t.Fatal(err)
					}
					if data["ssh_keys_deleted"] != float64(2) {
						t.Fatalf("session audit=%s", ev.Data)
					}
				}
			}
		})
	}
}

func TestRevokeSessions_AllDeletesSSHKeys(t *testing.T) {
	st := sshOffboardingStore()
	srv, cutoffs, audit := sshOffboardingServer(t, st)
	admin := ssoSession(t, "responder", "responder@example.com", oidc.RoleSecurityAdmin)
	w := doSSO(t, srv, http.MethodPost, "/api/v1/sessions/revoke", admin, `{"all":true}`)
	if w.Code != http.StatusNoContent {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if len(st.keys) != 0 || cutoffs.revokedAll != 1 {
		t.Fatalf("keys=%v cutoff=%+v", st.keys, cutoffs)
	}
	assertSSHDeletionAudit(t, audit, "*", "success", 3)
}

func TestRevokeSessions_SSHDeleteFailureIsAudited(t *testing.T) {
	for _, body := range []string{`{"sub":"sub-alice"}`, `{"all":true}`} {
		t.Run(body, func(t *testing.T) {
			st := sshOffboardingStore()
			st.deleteErr = errors.New("SSH key delete unavailable")
			srv, cutoffs, audit := sshOffboardingServer(t, st)
			admin := ssoSession(t, "responder", "responder@example.com", oidc.RoleSecurityAdmin)
			w := doSSO(t, srv, http.MethodPost, "/api/v1/sessions/revoke", admin, body)
			if w.Code != http.StatusInternalServerError {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
			if len(st.keys) != 3 || len(st.revoked) == 0 || len(cutoffs.revokedSubs)+cutoffs.revokedAll == 0 {
				t.Fatalf("wrong partial result: %+v cutoffs=%+v", st, cutoffs)
			}
			var found bool
			for _, ev := range audit.snapshot() {
				if ev.Action != "session.revoke" {
					continue
				}
				found = true
				var data map[string]any
				if err := json.Unmarshal(ev.Data, &data); err != nil {
					t.Fatal(err)
				}
				if ev.Outcome != "failure" || data["ssh_keys_deleted"] != float64(0) || data["tokens_revoked"] != float64(len(st.revoked)) {
					t.Fatalf("partial audit=%+v", ev)
				}
			}
			if !found {
				t.Fatal("partial failure was not audited")
			}
		})
	}
}

func TestRevokeSessions_TokenFailureStillDeletesSSHKeys(t *testing.T) {
	st := sshOffboardingStore()
	st.tokenErr = true
	srv, _, audit := sshOffboardingServer(t, st)
	admin := ssoSession(t, "responder", "responder@example.com", oidc.RoleSecurityAdmin)
	w := doSSO(t, srv, http.MethodPost, "/api/v1/sessions/revoke", admin, `{"sub":"sub-alice"}`)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if len(st.keys) != 1 || st.keys[0].Principal != "sub-bob" {
		t.Fatalf("token failure left SSH keys usable: %+v", st.keys)
	}
	assertSSHDeletionAudit(t, audit, "sub-alice", "success", 2)
}

func assertSSHDeletionAudit(t *testing.T, audit *recRecorder, target, outcome string, count int) {
	t.Helper()
	var found bool
	for _, ev := range audit.snapshot() {
		if ev.Action != "ssh_key.delete" {
			continue
		}
		found = true
		var data map[string]any
		if err := json.Unmarshal(ev.Data, &data); err != nil {
			t.Fatal(err)
		}
		if ev.Target != target || ev.Outcome != outcome || data["count"] != float64(count) {
			t.Fatalf("SSH deletion audit=%+v data=%v", ev, data)
		}
	}
	if !found {
		t.Fatal("SSH deletion was not audited")
	}
}

func TestRevokeSessions_TokenDirectoryFailureStillDeletesExactSSHPrincipal(t *testing.T) {
	st := sshOffboardingStore()
	st.directoryErr = errors.New("token directory unavailable")
	srv, _, audit := sshOffboardingServer(t, st)
	admin := ssoSession(t, "responder", "responder@example.com", oidc.RoleSecurityAdmin)
	w := doSSO(t, srv, http.MethodPost, "/api/v1/sessions/revoke", admin, `{"sub":"sub-alice"}`)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if len(st.keys) != 1 || st.keys[0].Principal != "sub-bob" {
		t.Fatalf("token directory failure left exact SSH keys usable: %+v", st.keys)
	}
	assertSSHDeletionAudit(t, audit, "sub-alice", "success", 2)
}

// revocationAuditRows is the credential audit rows of one revoke, in order, as
// (action, target, outcome, actor type, actor, data) strings.
func revocationAuditRows(audit *recRecorder) []string {
	var rows []string
	for _, ev := range audit.snapshot() {
		if ev.Action == "token.revoke" || ev.Action == "ssh_key.delete" {
			rows = append(rows, strings.Join([]string{ev.Action, ev.Target, ev.Outcome, string(ev.ActorType), ev.Actor, string(ev.Data)}, "|"))
		}
	}
	return rows
}

// TestRevokePersonCredentials_RequestFreeMatchesRoute: the deprovisioning job
// runs the route's credential sequence with no request, and the same cutoffs,
// deletions and audit rows (actor included) come out.
func TestRevokePersonCredentials_RequestFreeMatchesRoute(t *testing.T) {
	routeSt := sshOffboardingStore()
	routeSrv, routeCutoffs, routeAudit := sshOffboardingServer(t, routeSt)
	if w := do(t, routeSrv, http.MethodPost, "/api/v1/sessions/revoke", adminToken, `{"sub":"alice@example.com"}`); w.Code != http.StatusNoContent {
		t.Fatalf("route status=%d body=%s", w.Code, w.Body.String())
	}

	st := sshOffboardingStore()
	srv, cutoffs, audit := sshOffboardingServer(t, st)
	ctx := withActor(context.Background(), types.ActorSystem, adminTokenPrincipal)
	res, err := srv.revokePersonCredentials(ctx, "alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if res.Tokens != 1 || res.Keys != 2 || !res.Stamped {
		t.Fatalf("result=%+v", res)
	}
	if !slices.Equal(cutoffs.revokedSubs, routeCutoffs.revokedSubs) || len(cutoffs.revokedSubs) != 2 || cutoffs.revokedSubs[1] != "sub-alice" {
		t.Fatalf("cutoffs=%v route=%v", cutoffs.revokedSubs, routeCutoffs.revokedSubs)
	}
	if len(st.keys) != 1 || len(st.revoked) != len(routeSt.revoked) || len(st.keys) != len(routeSt.keys) {
		t.Fatalf("keys=%+v revoked=%v route keys=%+v revoked=%v", st.keys, st.revoked, routeSt.keys, routeSt.revoked)
	}
	rows, routeRows := revocationAuditRows(audit), revocationAuditRows(routeAudit)
	if len(rows) != 2 || !slices.Equal(rows, routeRows) {
		t.Fatalf("audit rows=%q route=%q", rows, routeRows)
	}
}

func TestRevokePersonCredentials_EmptyPrincipalChangesNothing(t *testing.T) {
	st := sshOffboardingStore()
	srv, cutoffs, audit := sshOffboardingServer(t, st)
	if _, err := srv.revokePersonCredentials(context.Background(), ""); err == nil {
		t.Fatal("empty principal was accepted")
	}
	if len(st.revoked) != 0 || len(st.keys) != 3 || len(cutoffs.revokedSubs)+cutoffs.revokedAll != 0 || len(audit.snapshot()) != 0 {
		t.Fatalf("empty principal changed state: %+v cutoffs=%+v audit=%d", st, cutoffs, len(audit.snapshot()))
	}
}

func TestRevokePersonCredentials_UnknownEmailIsSuccessWithZeroDeletions(t *testing.T) {
	st := sshOffboardingStore()
	srv, cutoffs, _ := sshOffboardingServer(t, st)
	res, err := srv.revokePersonCredentials(context.Background(), "nobody@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if res.Tokens != 0 || res.Keys != 0 || !res.Stamped || len(st.revoked) != 0 || len(st.keys) != 3 {
		t.Fatalf("result=%+v state=%+v", res, st)
	}
	if len(cutoffs.revokedSubs) != 1 || cutoffs.revokedSubs[0] != "nobody@example.com" {
		t.Fatalf("cutoffs=%v", cutoffs.revokedSubs)
	}
}
