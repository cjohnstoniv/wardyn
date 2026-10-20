// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A runner_resident Azure DevOps minted token lives only in the runner proxy's memory: every proxy start
// fetches it from the organisation afresh, a proxy started while the organisation is unreachable fails
// closed rather than fall back to a copy it kept, and nothing is written where the proxy runs. During an
// outage a held token stops being injected once its last-good grace past expiry is spent.
func TestInjector_ADOMintedPATLivesOnlyInMemory(t *testing.T) {
	var calls atomic.Int32
	var up atomic.Bool
	up.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if !up.Load() {
			http.Error(w, `{"error":"unavailable"}`, http.StatusServiceUnavailable)
			return
		}
		n := calls.Add(1)
		_ = json.NewEncoder(w).Encode(types.ResolvedInjection{Host: "dev.azure.com", Header: "Authorization",
			Value: fmt.Sprintf("Basic minted-pat-%d", n), JTI: uuid.NewString(), ExpiresAt: time.Now().Add(8 * time.Hour).UnixMilli()})
	}))
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("TMPDIR", dir)
	t.Setenv("HOME", dir)
	pol := CompilePolicy(types.RunPolicySpec{AllowedDomains: []string{"dev.azure.com"}})
	rules := []InjectionConfig{{GrantID: uuid.New(), InjectionRule: egress.InjectionRule{Host: "dev.azure.com", Header: "Authorization",
		SecretName: types.ADOEntraAccessTokenSecret, RequireTLS: true}}}
	start := func() (*injector, error) {
		return buildInjector(context.Background(), srv.URL, newTokenSource("tok"), pol, rules, srv.Client())
	}

	var last *injector
	for i := 1; i <= 2; i++ { // a start, then a restart
		inj, err := start()
		if err != nil {
			t.Fatalf("start %d: %v", i, err)
		}
		if h, _, err := inj.resolve("dev.azure.com"); err != nil || h.value != fmt.Sprintf("Basic minted-pat-%d", i) {
			t.Fatalf("start %d serves %q (%v), want the token it fetched itself", i, h.value, err)
		}
		last = inj
	}
	up.Store(false)
	if _, err := start(); err == nil {
		t.Fatal("a proxy started while the organisation is unreachable, from a kept copy")
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Fatalf("the proxy wrote %v (%v) where it runs, want nothing", entries, err)
	}
	last.byHost["dev.azure.com"].expiresAt = time.Now().Add(-lastGoodGrace - time.Second).UnixMilli()
	if h, _, err := last.resolve("dev.azure.com"); err == nil {
		t.Fatalf("outage past the token's grace served %q, want refused", h.value)
	}
}
