// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func mintServer(t *testing.T, token string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The injector resolves via GET /api/v1/internal/injection/{grantID}:
		// the control plane returns the header name + FORMATTED secret value
		// (format applied server-side; the proxy never sees raw secrets
		// except as the final injectable value).
		if r.Method != http.MethodGet || !strings.Contains(r.URL.Path, "/internal/injection/") {
			http.Error(w, "bad path", http.StatusNotFound)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		gid := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		_ = json.NewEncoder(w).Encode(resolvedInjection{
			Host:   "api.test",
			Header: "Authorization",
			Value:  "Bearer minted-" + gid[:8],
			JTI:    uuid.NewString(),
		})
	}))
}

// staticInj builds an injector with STATIC (never re-resolved) entries — the
// api-key injection shape tests exercise. Dynamic re-resolution is covered
// separately (TestInjectorReResolvesNearExpiry).
func staticInj(entries map[string]injectedHeader) *injector {
	m := make(map[string]*injEntry, len(entries))
	for h, hd := range entries {
		m[h] = &injEntry{header: hd}
	}
	return &injector{byHost: m}
}

func TestBuildInjectorMintsAndFormats(t *testing.T) {
	cp := mintServer(t, "tok")
	defer cp.Close()

	pol := CompilePolicy(types.RunPolicySpec{AllowedDomains: []string{"api.test"}})
	gid := uuid.New()
	rules := []InjectionConfig{{
		InjectionRule: egress.InjectionRule{Host: "api.test", Header: "Authorization", Format: "Bearer %s"},
		GrantID:       gid,
	}}
	inj, err := buildInjector(context.Background(), cp.URL, newTokenSource("tok"), pol, rules, cp.Client())
	if err != nil {
		t.Fatalf("buildInjector: %v", err)
	}
	h, ok := inj.byHost["api.test"]
	if !ok {
		t.Fatalf("rule not registered")
	}
	want := "Bearer minted-" + gid.String()[:8]
	if h.header.value != want {
		t.Fatalf("header value = %q, want %q", h.header.value, want)
	}
	if h.header.name != "Authorization" {
		t.Fatalf("header name = %q", h.header.name)
	}
}

// A DYNAMIC injection (non-zero ExpiresAt — the subscription OAuth token) must be
// re-resolved via the control plane once it nears expiry, and NOT re-resolved
// while still fresh. This is what keeps the injected credential from going stale.
func TestInjectorReResolvesNearExpiry(t *testing.T) {
	var calls int32
	nearExp := time.Now().Add(time.Minute).UnixMilli() // inside injectRefreshMargin
	farExp := time.Now().Add(time.Hour).UnixMilli()    // comfortably fresh
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		val, exp := "Bearer token-1", nearExp
		if atomic.AddInt32(&calls, 1) > 1 {
			val, exp = "Bearer token-2", farExp
		}
		_ = json.NewEncoder(w).Encode(resolvedInjection{
			Host: "api.test", Header: "Authorization", Value: val, JTI: uuid.NewString(), ExpiresAt: exp,
		})
	}))
	defer srv.Close()

	pol := CompilePolicy(types.RunPolicySpec{AllowedDomains: []string{"api.test"}})
	rules := []InjectionConfig{{
		InjectionRule: egress.InjectionRule{Host: "api.test", Header: "Authorization", Format: "Bearer %s"},
		GrantID:       uuid.New(),
	}}
	inj, err := buildInjector(context.Background(), srv.URL, newTokenSource("tok"), pol, rules, srv.Client())
	if err != nil {
		t.Fatalf("buildInjector: %v", err)
	}

	// Startup mint got token-1 (near expiry); the next resolve must re-resolve.
	h, ok, rerr := inj.resolve("api.test")
	if rerr != nil || !ok {
		t.Fatalf("resolve: ok=%v err=%v", ok, rerr)
	}
	if h.value != "Bearer token-2" {
		t.Fatalf("expected re-resolved token-2, got %q (calls=%d)", h.value, atomic.LoadInt32(&calls))
	}
	// token-2 is far-expiry: a subsequent resolve must NOT hit the control plane.
	before := atomic.LoadInt32(&calls)
	if _, _, err := inj.resolve("api.test"); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&calls); got != before {
		t.Errorf("fresh token must not re-resolve (calls %d -> %d)", before, got)
	}
}

func TestBuildInjectorRefusesNonExactHost(t *testing.T) {
	cp := mintServer(t, "tok")
	defer cp.Close()

	// Wildcard allow only — injection must refuse (would leak secret to any
	// subdomain and widen egress trust).
	pol := CompilePolicy(types.RunPolicySpec{AllowedDomains: []string{"*.api.test"}})
	rules := []InjectionConfig{{
		InjectionRule: egress.InjectionRule{Host: "a.api.test", Header: "Authorization", Format: "Bearer %s"},
		GrantID:       uuid.New(),
	}}
	_, err := buildInjector(context.Background(), cp.URL, newTokenSource("tok"), pol, rules, cp.Client())
	if err == nil {
		t.Fatalf("expected refusal for wildcard-only host")
	}
	if !strings.Contains(err.Error(), "exact allowlist") {
		t.Fatalf("error = %v, want exact-allowlist refusal", err)
	}
}

func TestBuildInjectorFailsClosedOnMintError(t *testing.T) {
	// Server returns 409 (approval pending) -> startup must fail.
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]string{"approval_id": uuid.NewString()})
	}))
	defer cp.Close()

	pol := CompilePolicy(types.RunPolicySpec{AllowedDomains: []string{"api.test"}})
	rules := []InjectionConfig{{
		InjectionRule: egress.InjectionRule{Host: "api.test"},
		GrantID:       uuid.New(),
	}}
	_, err := buildInjector(context.Background(), cp.URL, newTokenSource("tok"), pol, rules, cp.Client())
	if err == nil {
		t.Fatalf("expected fail-closed on 409 mint")
	}
}

func TestBuildInjectorRequiresGrantID(t *testing.T) {
	pol := CompilePolicy(types.RunPolicySpec{AllowedDomains: []string{"api.test"}})
	rules := []InjectionConfig{{InjectionRule: egress.InjectionRule{Host: "api.test"}}}
	if _, err := buildInjector(context.Background(), "http://unused", newTokenSource("tok"), pol, rules, nil); err == nil {
		t.Fatalf("expected error for missing grant_id")
	}
}

func TestLoadConfigDefaultsAndValidation(t *testing.T) {
	dir := t.TempDir()
	good := Config{
		RunID:           uuid.New(),
		ControlPlaneURL: "http://wardynd:8080",
		RunToken:        "tok",
		Policy:          types.RunPolicySpec{AllowedDomains: []string{"api.test"}},
	}
	path := filepath.Join(dir, "good.json")
	b, _ := json.Marshal(good)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if c.Listen != defaultListen {
		t.Errorf("listen default = %q, want %q", c.Listen, defaultListen)
	}
	if c.DecisionBufferSize != defaultBufferSize {
		t.Errorf("buffer default = %d", c.DecisionBufferSize)
	}

	// Missing required fields -> error.
	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte(`{"control_plane_url":"http://x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(bad); err == nil {
		t.Fatalf("expected validation error for missing run_id/token")
	}

	if _, err := LoadConfig(filepath.Join(dir, "nope.json")); err == nil {
		t.Fatalf("expected error for missing file")
	}

	// Missing control_plane_url / run_token, and malformed JSON, are each
	// rejected by the same required-field / parse checks above.
	if _, err := LoadConfigBytes([]byte(`{"run_id":"` + uuid.New().String() + `","run_token":"t"}`)); err == nil || !strings.Contains(err.Error(), "control_plane_url is required") {
		t.Fatalf("expected control_plane_url required error, got %v", err)
	}
	if _, err := LoadConfigBytes([]byte(`{"run_id":"` + uuid.New().String() + `","control_plane_url":"http://x"}`)); err == nil || !strings.Contains(err.Error(), "run_token is required") {
		t.Fatalf("expected run_token required error, got %v", err)
	}
	if _, err := LoadConfigBytes([]byte(`{not json`)); err == nil || !strings.Contains(err.Error(), "parse config") {
		t.Fatalf("expected parse config error, got %v", err)
	}

	// Explicit Listen/DecisionBufferSize override the defaults.
	explicit := `{"run_id":"` + uuid.New().String() + `","control_plane_url":"http://wardynd:8080","run_token":"t","listen":"127.0.0.1:9999","decision_buffer_size":7}`
	ec, err := LoadConfigBytes([]byte(explicit))
	if err != nil {
		t.Fatalf("LoadConfigBytes explicit: %v", err)
	}
	if ec.Listen != "127.0.0.1:9999" || ec.DecisionBufferSize != 7 {
		t.Errorf("explicit overrides not kept: listen=%q buffer=%d", ec.Listen, ec.DecisionBufferSize)
	}
}
