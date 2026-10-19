// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

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
