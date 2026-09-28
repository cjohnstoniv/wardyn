// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package vaultkv

import (
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

func tokenFileClient(t *testing.T, mode string) (*fakeVault, *client, string) {
	t.Helper()
	f := newFakeVault(t)
	f.mu.Lock()
	token := f.issue()
	f.jwts[token] = "wardyn"
	f.mu.Unlock()
	path := writeFile(t, " \t"+token+"\r\n\n")
	c, err := newClient(Config{Addr: f.srv.URL, Auth: mode, AuthMount: "kubernetes", Role: "wardyn", TokenFile: path, K8sTokenFile: path})
	if err != nil {
		t.Fatal(err)
	}
	c.backoff = 0
	return f, c, path
}

func unsafeTokenModes() []os.FileMode {
	modes := []os.FileMode{0o620, 0o602}
	if os.Geteuid() != 0 {
		modes = append(modes, 0o644)
	}
	return modes
}

func TestTokenFile_RejectsUnsafeMode(t *testing.T) {
	for _, auth := range []string{AuthTokenFile, AuthKubernetes} {
		for _, mode := range unsafeTokenModes() {
			t.Run(fmt.Sprintf("%s/%04o", auth, mode), func(t *testing.T) {
				f, c, path := tokenFileClient(t, auth)
				if err := os.Chmod(path, mode); err != nil {
					t.Fatal(err)
				}
				err := c.login(t.Context())
				if err == nil || !strings.Contains(err.Error(), "mode") || !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "WARDYN_VAULT_") {
					t.Errorf("unsafe token file did not fail with setting/path/mode: %v", err)
				}
				if f.callCount("") != 0 {
					t.Error("unsafe token file reached Vault")
				}
				if err != nil && strings.Contains(err.Error(), "tok-1") {
					t.Error("file refusal disclosed its value")
				}
			})
		}
	}
}

func TestTokenFile_RechecksModeAfterUnauthorized(t *testing.T) {
	for _, auth := range []string{AuthTokenFile, AuthKubernetes} {
		for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
			for _, mode := range unsafeTokenModes() {
				t.Run(fmt.Sprintf("%s/%d/%04o", auth, status, mode), func(t *testing.T) {
					f, c, path := tokenFileClient(t, auth)
					if err := c.login(t.Context()); err != nil {
						t.Fatalf("positive login: %v", err)
					}
					f.mu.Lock()
					fresh := f.issue()
					f.jwts[fresh] = "wardyn"
					f.force = []int{status}
					before := len(f.calls)
					f.mu.Unlock()
					replacement := writeFile(t, fresh+"\n")
					if err := os.Chmod(replacement, mode); err != nil {
						t.Fatal(err)
					}
					if err := os.Rename(replacement, path); err != nil {
						t.Fatal(err)
					}
					_, err := c.call(t.Context(), http.MethodGet, "wardyn/config", nil, nil)
					if statusOf(err) != status || !strings.Contains(err.Error(), "mode") || !strings.Contains(err.Error(), path) {
						t.Errorf("unsafe replacement token file was not refused after %d: %v", status, err)
					}
					if got := f.callCount("") - before; got != 1 {
						t.Errorf("requests after rotation = %d; want only the refused operation", got)
					}
					if err := os.Chmod(path, 0o640); err != nil {
						t.Fatal(err)
					}
					c.reloginAt = time.Time{}
					f.mu.Lock()
					f.force = []int{status}
					f.mu.Unlock()
					if _, err := c.call(t.Context(), http.MethodGet, "wardyn/config", nil, nil); err != nil {
						t.Fatalf("safe replacement did not recover: %v", err)
					}
				})
			}
		}
	}
}

func TestTokenFile_AcceptsSupportedVolumeOwnership(t *testing.T) {
	// Ownership combinations are pinned by cliutil's shared policy table;
	// these real files prove both Vault login callers retain group-readable delivery.
	for _, auth := range []string{AuthTokenFile, AuthKubernetes} {
		for _, mode := range []os.FileMode{0o400, 0o440, 0o600, 0o640} {
			t.Run(fmt.Sprintf("%s/%04o", auth, mode), func(t *testing.T) {
				_, c, path := tokenFileClient(t, auth)
				if err := os.Chmod(path, mode); err != nil {
					t.Fatal(err)
				}
				if err := c.login(t.Context()); err != nil {
					t.Fatalf("supported mode or token whitespace refused: %v", err)
				}
			})
		}
	}
}
