// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"golang.org/x/crypto/ssh"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// keyLookupStore fails the SSH key lookup with err.
type keyLookupStore struct {
	store.Store
	err error
}

func (s keyLookupStore) GetSSHKeyByFingerprint(context.Context, string) (types.SSHPublicKey, error) {
	return types.SSHPublicKey{}, s.err
}

// A store fault during the key lookup is logged as one (#2782); the audit row
// keeps its reason, and a plain not-found logs nothing.
func TestSSHAuth_StoreFaultOnKeyLookupIsLogged(t *testing.T) {
	for _, tc := range []struct {
		name    string
		err     error
		wantLog bool
	}{
		{"store fault", errors.New("pool exhausted"), true},
		{"unregistered key", store.ErrNotFound, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.srv.cfg.BaseCtx = context.Background()
			h.srv.cfg.Store = keyLookupStore{Store: h.srv.cfg.Store, err: tc.err}
			logs := captureSlog(t)
			if _, err := h.srv.sshAuth(fakeSSHConn{user: uuid.NewString()}, sshKeyFor(t)); err == nil {
				t.Fatal("an unknown key was accepted")
			}
			if got := strings.Contains(logs.String(), "ssh key lookup failed"); got != tc.wantLog {
				t.Errorf("logged = %v, want %v; log = %q", got, tc.wantLog, logs.String())
			}
			var reasons []string
			for _, ev := range h.audit.snapshot() {
				reasons = append(reasons, string(ev.Data))
			}
			if len(reasons) != 1 || !strings.Contains(reasons[0], "unregistered key") {
				t.Errorf("audit rows = %v, want the unchanged unregistered key row", reasons)
			}
		})
	}
}

// runLookupStore holds one registered key and fails the run lookup with err.
type runLookupStore struct {
	store.Store
	key types.SSHPublicKey
	err error
}

func (s runLookupStore) GetSSHKeyByFingerprint(context.Context, string) (types.SSHPublicKey, error) {
	return s.key, nil
}

func (s runLookupStore) GetRun(context.Context, uuid.UUID) (types.AgentRun, error) {
	return types.AgentRun{}, s.err
}

// A failed run lookup comes after the key lookup, so its row names the key's owner, never
// "unknown"; a store fault is "run lookup failed" and a missing run keeps "unknown run" (#3508).
func TestSSHAuth_RunLookupFailureNamesTheKeyOwner(t *testing.T) {
	for _, tc := range []struct {
		name, reason string
		err          error
	}{
		{"store fault", "run lookup failed", errors.New("pool exhausted")},
		{"missing run", "unknown run", store.ErrNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.srv.cfg.BaseCtx = context.Background()
			pub := sshKeyFor(t)
			key := types.SSHPublicKey{Principal: "alice@example.com", PublicKey: string(ssh.MarshalAuthorizedKey(pub))}
			h.srv.cfg.Store = runLookupStore{Store: h.srv.cfg.Store, key: key, err: tc.err}
			if _, err := h.srv.sshAuth(fakeSSHConn{user: uuid.NewString()}, pub); err == nil {
				t.Fatal("a connection to a run that could not be read was accepted")
			}
			evs := h.audit.snapshot()
			if len(evs) != 1 || evs[0].Actor != key.Principal || !strings.Contains(string(evs[0].Data), `"reason":"`+tc.reason+`"`) {
				var got []string
				for _, ev := range evs {
					got = append(got, ev.Actor+" "+string(ev.Data))
				}
				t.Errorf("audit rows = %v, want one naming %s with reason %q", got, key.Principal, tc.reason)
			}
		})
	}
}
