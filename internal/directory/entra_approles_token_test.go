// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package directory

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// A 401 from the token endpoint is a credential problem for every kind: it is returned, it does not latch
// App Roles off, and the next search reaches Graph once the token works again.
func TestAppRolesTokenPhase401IsAnErrorNotADenial(t *testing.T) {
	f := newFakeGraph(t)
	var broken atomic.Bool
	broken.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if broken.Load() {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"invalid_client"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"tok","token_type":"Bearer","expires_in":3600}`))
	}))
	t.Cleanup(srv.Close)
	dir, err := newEntra(EntraConfig{TenantID: "t", ClientID: "c", ClientSecret: "s"}, srv.URL, f.srv.URL+"/v1.0")
	if err != nil {
		t.Fatal(err)
	}
	d := dir.(*entraDirectory)

	_, err = d.Search(context.Background(), "war", KindAppRole)
	var pe *ProviderError
	if !errors.As(err, &pe) || pe.Op != "token" {
		t.Fatalf("err = %v, want the token ProviderError returned", err)
	}
	if d.approleDenied.Load() {
		t.Fatal("a token-phase 401 latched App Roles off")
	}

	broken.Store(false)
	if _, err := d.Search(context.Background(), "war", KindAppRole); err != nil {
		t.Fatalf("search after the token recovered: %v", err)
	}
	if n := f.spCalls.Load(); n != 1 {
		t.Errorf("servicePrincipals calls = %d, want 1: the next search must try Graph again", n)
	}
}
