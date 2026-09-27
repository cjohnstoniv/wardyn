// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package vaultkv

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/secretstore"
)

func TestTransitKEK_DeletedKeyIsReportedAsServiceFailure(t *testing.T) {
	for _, tc := range []struct {
		name         string
		status       int
		message      string
		breakService func(*fakeVault)
	}{
		{"deleted key", http.StatusBadRequest, "encryption key not found", func(f *fakeVault) { f.transit.name = "" }},
		{"other bad request", http.StatusBadRequest, "forced", func(f *fakeVault) { f.force = []int{http.StatusBadRequest} }},
		{"revoked policy", http.StatusForbidden, "permission denied", func(f *fakeVault) { f.revoked = true }},
		{"missing mount", http.StatusNotFound, "forced", func(f *fakeVault) { f.force = []int{http.StatusNotFound} }},
		{"unavailable", http.StatusServiceUnavailable, "forced", func(f *fakeVault) { f.force = []int{503, 503, 503, 503} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := throwawayDB(t)
			f := newFakeVault(t)
			tr := newFakeTransit(t, f)
			s := pgStore(t, pool, nil, tr, true)
			const value = "synthetic-private-value"
			if err := s.Put(t.Context(), "credential", []byte(value)); err != nil {
				t.Fatal(err)
			}
			if got, err := s.Get(t.Context(), "credential"); err != nil || string(got) != value {
				t.Fatalf("positive read failed: %v", err)
			}
			before := f.callCount("POST transit/decrypt/")
			f.mu.Lock()
			tc.breakService(f)
			f.mu.Unlock()
			got, err := s.Get(t.Context(), "credential")
			if err == nil || len(got) != 0 || errors.Is(err, secretstore.ErrNotFound) {
				t.Fatalf("service failure returned a value or absence: %v", err)
			}
			if errors.Is(err, secretstore.ErrUnavailable) != (tc.status >= 500) {
				t.Fatalf("transience changed: %v", err)
			}
			if statusOf(err) != tc.status || !strings.Contains(err.Error(), tc.message) || !strings.Contains(err.Error(), "key service") {
				t.Errorf("failure lost the Vault class: %v", err)
			}
			for _, wrong := range []string{"forged", "corrupted", "retired", value} {
				if strings.Contains(err.Error(), wrong) {
					t.Errorf("service failure contains %q: %v", wrong, err)
				}
			}
			if f.callCount("POST transit/decrypt/") <= before {
				t.Error("the rejected read did not reach Transit")
			}
		})
	}
}
