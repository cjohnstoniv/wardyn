// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/ssh"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// cappedKeysSSOServer is an SSO-live Server over st, so a member-mode cookie
// reaches handleAddSSHKey with the clamp applied and the key lands in a store
// the gateway harness can share.
func cappedKeysSSOServer(t *testing.T, st *sshMemStore) (*Server, *harness) {
	t.Helper()
	h := newHarness(t)
	cfg := baseTestConfig(h, st)
	cfg.OIDC = &oidc.Authenticator{}
	return New(cfg), h
}

func addKeyBody(t *testing.T, pub ssh.PublicKey) string {
	t.Helper()
	b, err := json.Marshal(map[string]string{"name": "laptop", "public_key": string(ssh.MarshalAuthorizedKey(pub))})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestSSHKeys_UserViewAddIsCapped pins D4-A's key door: an admin in the user
// view (member mode) may register a key, and it is stored capped at member; the
// same admin outside the view registers an uncapped admin key, exactly as
// before, and that row's audit datum carries no capped marker.
func TestSSHKeys_UserViewAddIsCapped(t *testing.T) {
	st := newSSHMemStore()
	srv, h := cappedKeysSSOServer(t, st)

	cases := []struct {
		name       string
		memberMode bool
		wantRole   string
	}{
		{"user view: allowed and capped", true, oidc.RoleUser},
		{"admin view: uncapped admin key, as today", false, oidc.RoleAdmin},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, pub := mustSSHKeypair(t)
			cookie := memberModeSSOSession(t, memberModeAdminSub, memberModeAdminEmail, oidc.RoleAdmin, tc.memberMode)
			w := doSSO(t, srv, http.MethodPost, "/api/v1/me/ssh-keys", cookie, addKeyBody(t, pub))
			if w.Code != http.StatusCreated {
				t.Fatalf("add = %d, want 201: %s", w.Code, w.Body.String())
			}
			var added types.SSHPublicKey
			if err := json.Unmarshal(w.Body.Bytes(), &added); err != nil {
				t.Fatal(err)
			}
			stored, err := st.GetSSHKeyByFingerprint(t.Context(), added.Fingerprint)
			if err != nil {
				t.Fatal(err)
			}
			if stored.Capped != tc.memberMode || stored.Role != tc.wantRole || stored.Principal != memberModeAdminSub {
				t.Errorf("stored = capped:%v role:%q principal:%q, want capped:%v role:%q principal:%q",
					stored.Capped, stored.Role, stored.Principal, tc.memberMode, tc.wantRole, memberModeAdminSub)
			}
			data := auditData(t, lastAuditEvent(t, h.audit.events, "ssh_key.add"))
			if v, ok := data["capped"]; ok != tc.memberMode || (ok && v != true) {
				t.Errorf("ssh_key.add datum = %v, want capped:true only on the user-view row", data)
			}
		})
	}
}

// TestSSHGateway_CappedKeyNeverOverrides pins D4-A at the gateway: a capped key
// reaches its owner's own runs and nothing else, even when its row reads admin
// (a store the DB CHECK does not guard) and its stamp is fresh. The key under
// test is registered through the real key door in the user view, then offered
// to the gateway; an uncapped admin key on the same run is the control that
// keeps the refusal from passing vacuously.
func TestSSHGateway_CappedKeyNeverOverrides(t *testing.T) {
	st := newSSHMemStore()
	srv, _ := cappedKeysSSOServer(t, st)

	otherRun := uuid.New()
	ownRun := uuid.New()
	st.putRun(types.AgentRun{ID: otherRun, CreatedBy: "bob@example.com", State: types.RunRunning, SandboxRef: "sbx-bob"})
	st.putRun(types.AgentRun{ID: ownRun, CreatedBy: memberModeAdminSub, State: types.RunRunning, SandboxRef: "sbx-own"})

	cappedPriv, cappedPub := mustSSHKeypair(t)
	userView := memberModeSSOSession(t, memberModeAdminSub, memberModeAdminEmail, oidc.RoleAdmin, true)
	if w := doSSO(t, srv, http.MethodPost, "/api/v1/me/ssh-keys", userView, addKeyBody(t, cappedPub)); w.Code != http.StatusCreated {
		t.Fatalf("user-view add = %d, want 201: %s", w.Code, w.Body.String())
	}

	now := time.Now()
	forcedPriv, forcedPub := mustSSHKeypair(t)
	st.putKey(types.SSHPublicKey{
		Fingerprint: ssh.FingerprintSHA256(forcedPub), Principal: "root2@example.com",
		PublicKey: string(ssh.MarshalAuthorizedKey(forcedPub)), Role: oidc.RoleAdmin, RoleCheckedAt: &now, Capped: true,
	})
	adminPriv, adminPub := mustSSHKeypair(t)
	st.putKey(types.SSHPublicKey{
		Fingerprint: ssh.FingerprintSHA256(adminPub), Principal: "root@example.com",
		PublicKey: string(ssh.MarshalAuthorizedKey(adminPub)), Role: oidc.RoleAdmin, RoleCheckedAt: &now,
	})

	h := newSSHTestHarness(t, st, &sshFakeRunner{})

	client, err := sshDial(t, h, otherRun.String(), adminPriv)
	if err != nil {
		t.Fatalf("control: an uncapped fresh admin key must keep its override: %v", err)
	}
	client.Close()

	for _, tc := range []struct {
		name  string
		priv  ed25519.PrivateKey
		actor string
	}{
		{"registered in the user view", cappedPriv, memberModeAdminSub},
		{"capped row that reads admin", forcedPriv, "root2@example.com"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := sshDial(t, h, otherRun.String(), tc.priv); err == nil {
				t.Fatal("a capped key reached another human's run; the cap must refuse the override")
			}
			var ev *types.AuditEvent
			deadline := time.Now().Add(time.Second)
			for ev == nil && time.Now().Before(deadline) {
				for _, e := range h.audit.snapshot() {
					if e.Action == "ssh.authenticate" && e.Outcome == "failure" && e.Actor == tc.actor {
						ev = &e
					}
				}
				time.Sleep(5 * time.Millisecond)
			}
			if ev == nil {
				t.Fatalf("no ssh.authenticate failure for %s; events=%s", tc.actor, auditDump(h.audit.snapshot(), otherRun))
			}
			if reason := auditData(t, *ev)["reason"]; reason != "capped key (registered in the user view): no admin override" {
				t.Errorf("ssh.authenticate failure reason = %v, want the capped-key reason", reason)
			}
		})
	}

	client, err = sshDial(t, h, ownRun.String(), cappedPriv)
	if err != nil {
		t.Fatalf("a capped key must still reach its owner's own run: %v", err)
	}
	client.Close()
}
