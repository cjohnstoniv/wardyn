// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The config an allow-all run with one api_key grant was dispatched with: the grant's injection
// rule, and no exact allowlist entry for its host (allow-all names none, and dispatch adds a
// grant's own host only for ssh_key and git_pat). The proxy refuses it at boot, so such a run never
// started. Dispatch now withholds a credential the proxy's policy does not bind
// (TestDispatch_AllowAllAPIKeyGrantIsWithheld in internal/api); this refusal stays the proxy's own.
func TestProxyBoot_AllowAllRefusesAnAPIKeyInjection(t *testing.T) {
	const host = "api.example.com"
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/internal/injection/") {
			http.Error(w, "bad path", http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(types.ResolvedInjection{Host: host, Header: "Authorization", Value: "Bearer k", JTI: uuid.NewString()})
	}))
	defer cp.Close()
	boot := func(pol types.RunPolicySpec) error {
		raw, err := json.Marshal(&Config{
			RunID: uuid.New(), ControlPlaneURL: cp.URL, RunToken: "tok", Listen: "127.0.0.1:0",
			Policy: pol,
			Injection: []InjectionConfig{{
				InjectionRule: egress.InjectionRule{Host: host, Header: "Authorization", Format: "Bearer %s"},
				GrantID:       uuid.New(),
			}},
		})
		if err != nil {
			t.Fatal(err)
		}
		cfg, err := LoadConfigBytes(raw)
		if err != nil {
			t.Fatalf("LoadConfigBytes: %v", err)
		}
		srv, err := NewServer(context.Background(), cfg, cp.Client(), &bytes.Buffer{})
		if err == nil {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = srv.Shutdown(ctx)
		}
		return err
	}
	err := boot(types.RunPolicySpec{AllowAllEgress: true})
	if err == nil || !strings.Contains(err.Error(), "is not in the exact allowlist") {
		t.Fatalf("NewServer over allow-all with an api_key injection = %v, want the exact-allowlist refusal", err)
	}
	t.Logf("allow-all boot refused: %v", err)
	// The same rule boots once its host is named exactly: the refusal is the missing entry.
	if err := boot(types.RunPolicySpec{AllowAllEgress: true, AllowedDomains: []string{host}}); err != nil {
		t.Fatalf("NewServer with %s named exactly = %v, want it to boot", host, err)
	}
}
