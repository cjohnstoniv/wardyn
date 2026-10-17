// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

type sshKeyReadFailureStore struct {
	*sshMemStore
	failed atomic.Bool
}

func (s *sshKeyReadFailureStore) GetSSHKeyByFingerprint(ctx context.Context, fp string) (types.SSHPublicKey, error) {
	if s.failed.Load() {
		return types.SSHPublicKey{}, errors.New("key registry unavailable")
	}
	return s.sshMemStore.GetSSHKeyByFingerprint(ctx, fp)
}

func checkSSHNewChannels(t *testing.T, client *ssh.Client, wantAllowed bool) {
	t.Helper()
	for _, kind := range []string{"session", "direct-tcpip"} {
		var payload []byte
		if kind == "direct-tcpip" {
			payload = ssh.Marshal(sshDirectTCPIPMsg{DestHost: "127.0.0.1", DestPort: 9999})
		}
		ch, reqs, err := client.OpenChannel(kind, payload)
		if err == nil {
			go ssh.DiscardRequests(reqs)
			_ = ch.Close()
		}
		if (err == nil) != wantAllowed {
			t.Errorf("%s channel error=%v wantAllowed=%v", kind, err, wantAllowed)
			continue
		}
		if !wantAllowed {
			var denied *ssh.OpenChannelError
			if !errors.As(err, &denied) || denied.Reason != ssh.Prohibited {
				t.Fatalf("%s refusal=%v, want prohibited", kind, err)
			}
		}
	}
}

func TestSSHGateway_RevokedKeyCannotOpenAnotherChannel(t *testing.T) {
	st, run, principal := sshOwnedRunningRun(t)
	priv, pub := mustSSHKeypair(t)
	fp := ssh.FingerprintSHA256(pub)
	st.putKey(types.SSHPublicKey{Fingerprint: fp, Principal: principal, PublicKey: string(ssh.MarshalAuthorizedKey(pub))})
	h := newSSHTestHarness(t, st, &sshFakeRunner{execFn: func(runner.ExecSpec) (*runner.ExecSession, error) { return fakeEchoExecSession(), nil }})
	client, err := sshDial(t, h, run.ID.String(), priv)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	checkSSHNewChannels(t, client, true)
	if err := st.DeleteSSHKey(context.Background(), fp, principal); err != nil {
		t.Fatal(err)
	}
	checkSSHNewChannels(t, client, false)
	ev := waitForAudit(t, h.audit, run.ID, "ssh.channel.reject", "failure")
	if ev == nil {
		t.Fatal("channel rejection was not audited")
	}
	if ev.Actor != principal {
		t.Fatalf("rejection audit actor=%q", ev.Actor)
	}
}

func TestSSHGateway_KeyReadFailureRefusesNewChannel(t *testing.T) {
	mem, run, principal := sshOwnedRunningRun(t)
	st := &sshKeyReadFailureStore{sshMemStore: mem}
	priv, pub := mustSSHKeypair(t)
	st.putKey(types.SSHPublicKey{Fingerprint: ssh.FingerprintSHA256(pub), Principal: principal, PublicKey: string(ssh.MarshalAuthorizedKey(pub))})
	h := newSSHTestHarness(t, st, &sshFakeRunner{execFn: func(runner.ExecSpec) (*runner.ExecSession, error) { return fakeEchoExecSession(), nil }})
	client, err := sshDial(t, h, run.ID.String(), priv)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	checkSSHNewChannels(t, client, true)
	st.failed.Store(true)
	checkSSHNewChannels(t, client, false)
	st.failed.Store(false)
	checkSSHNewChannels(t, client, true)
}

func TestSSHGateway_ChangedKeyCannotOpenAnotherChannel(t *testing.T) {
	for _, tc := range []struct {
		name     string
		override bool
		change   func(*types.SSHPublicKey)
	}{
		{"principal", false, func(k *types.SSHPublicKey) { k.Principal = "another-person" }},
		{"material", false, func(k *types.SSHPublicKey) {
			_, p := mustSSHKeypair(t)
			k.PublicKey = string(ssh.MarshalAuthorizedKey(p))
		}},
		{"malformed", false, func(k *types.SSHPublicKey) { k.PublicKey = "invalid public key" }},
		{"registered_again", false, func(k *types.SSHPublicKey) { k.CreatedAt = k.CreatedAt.Add(time.Second) }},
		{"role", true, func(k *types.SSHPublicKey) { k.Role = oidc.RoleUser }},
		{"capped", true, func(k *types.SSHPublicKey) { k.Capped = true }},
		{"expired_stamp", true, func(k *types.SSHPublicKey) { old := time.Now().Add(-2 * defaultSSHRoleTTL); k.RoleCheckedAt = &old }},
		{"missing_stamp", true, func(k *types.SSHPublicKey) { k.RoleCheckedAt = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, run, principal := sshOwnedRunningRun(t)
			priv, pub := mustSSHKeypair(t)
			now := time.Now().UTC()
			k := types.SSHPublicKey{Fingerprint: ssh.FingerprintSHA256(pub), Principal: principal, PublicKey: string(ssh.MarshalAuthorizedKey(pub)), CreatedAt: now, Role: oidc.RoleAdmin, RoleCheckedAt: &now}
			if tc.override {
				run.CreatedBy = "another-owner"
				run.OperatorOwned = true // the carve-out: the only run an admin key may enter (#1476)
				st.putRun(run)
			}
			st.putKey(k)
			h := newSSHTestHarness(t, st, &sshFakeRunner{execFn: func(runner.ExecSpec) (*runner.ExecSession, error) { return fakeEchoExecSession(), nil }})
			client, err := sshDial(t, h, run.ID.String(), priv)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			checkSSHNewChannels(t, client, true)
			tc.change(&k)
			st.putKey(k)
			checkSSHNewChannels(t, client, false)
		})
	}
}
