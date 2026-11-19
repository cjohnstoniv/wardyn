// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// One gateway audience is never kept at two addresses, even by two saves at once: every audience writer
// holds db.GatewayAudienceLockClass across its check and write. strictListBarrier holds the first component
// list until a second one is read, so both saves' first audience checks read the tables before either
// writes, and each would pass alone. Guarded by WARDYN_TEST_PG; skipped cleanly when unset.

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/secretstore/kek"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// strictListBarrier is listBarrierStore without its fallback: the first component list waits for a second,
// and a second that never comes fails the test instead of letting the saves run one after the other, which
// would pass with no lock at all. Each audience check reads the list before any audience lock, so both
// saves' first reads arrive whatever order they run in.
type strictListBarrier struct {
	store.PG
	t      *testing.T
	mu     sync.Mutex
	calls  int
	second chan struct{}
}

func (b *strictListBarrier) ListComponents(ctx context.Context, owner string) ([]types.Component, error) {
	rows, err := b.PG.ListComponents(ctx, owner)
	b.mu.Lock()
	b.calls++
	n := b.calls
	b.mu.Unlock()
	switch n {
	case 1:
		select {
		case <-b.second:
		case <-time.After(10 * time.Second):
			// Error, not Fatal: this runs on a handler's goroutine. The test fails on reached after the saves.
			b.t.Error("both reads did not reach the barrier")
		}
	case 2:
		close(b.second)
	}
	return rows, err
}

// reached reports whether both first reads met at the barrier.
func (b *strictListBarrier) reached() bool {
	select {
	case <-b.second:
		return true
	default:
		return false
	}
}

// newAudienceRacePG is the components store with a provider that has identity on, the sealed run config a
// save with an audience needs, and the list barrier in front.
func newAudienceRacePG(t *testing.T) (componentsPG, *strictListBarrier) {
	t.Helper()
	e := newComponentsPG(t)
	if _, err := e.st.PutSiteConfig(context.Background(), testGatewaySite(testGatewayProvider())); err != nil {
		t.Fatal(err)
	}
	e.h.srv.cfg.RunConfigKey = make([]byte, kek.DEKSize)
	b := &strictListBarrier{PG: e.st, t: t, second: make(chan struct{})}
	e.h.srv.cfg.Store = b
	return e, b
}

// audienceClash reports whether body is the component save's refusal of one gateway audience at two addresses.
func audienceClash(body string) bool {
	return strings.Contains(body, `"reason":"component_definition_invalid"`) &&
		strings.Contains(body, "mcp.gateway_audience: ") && strings.Contains(body, "at different addresses")
}

func audienceMCPDef(host, audience string) string {
	return `{"hosts":["` + host + `"],"mcp":{"url":"https://` + host + `/mcp","transport":"http","gateway_audience":"` + audience + `"}}`
}

// Two organisation MCP servers at different addresses save one audience at once: exactly one is saved.
func TestGatewayAudience_ConcurrentComponentSavesKeepOne(t *testing.T) {
	e, barrier := newAudienceRacePG(t)
	type result struct {
		code int
		body string
	}
	results := make([]result, 2)
	var wg sync.WaitGroup
	for i, host := range []string{"mcp-a.corp.example", "mcp-b.corp.example"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			name := []string{"Docs A", "Docs B"}[i]
			w := e.do(t, http.MethodPut, "/api/v1/components/"+uuid.NewString(), e.admin, saveComponentBody(name, audienceMCPDef(host, "shared-aud")))
			results[i] = result{w.Code, w.Body.String()}
		}()
	}
	wg.Wait()
	if !barrier.reached() {
		t.Fatal("both reads did not reach the barrier")
	}
	saved, refused := 0, 0
	for _, r := range results {
		switch {
		case r.code == http.StatusCreated:
			saved++
		case r.code == http.StatusUnprocessableEntity && audienceClash(r.body):
			refused++
		}
	}
	if saved != 1 || refused != 1 {
		t.Fatalf("concurrent saves = %+v, want one 201 and one 422 naming the audience", results)
	}
}

// A component save and a model provider save set one audience at different addresses at once: exactly one is saved.
func TestGatewayAudience_ConcurrentComponentAndProviderSavesKeepOne(t *testing.T) {
	e, barrier := newAudienceRacePG(t)
	provider := strings.Replace(testGatewayPutOne, `"audience":"gw-aud"`, `"audience":"shared-aud"`, 1)
	var comp, prov *http.Response
	var compBody, provBody string
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		w := e.do(t, http.MethodPut, "/api/v1/components/"+uuid.NewString(), e.admin, saveComponentBody("Docs", audienceMCPDef("mcp.corp.example", "shared-aud")))
		comp, compBody = w.Result(), w.Body.String()
	}()
	go func() {
		defer wg.Done()
		w := do(t, e.h.srv, http.MethodPut, "/api/v1/model-providers", adminToken, provider)
		prov, provBody = w.Result(), w.Body.String()
	}()
	wg.Wait()
	if !barrier.reached() {
		t.Fatal("both reads did not reach the barrier")
	}
	compSaved, provSaved := comp.StatusCode == http.StatusCreated, prov.StatusCode == http.StatusOK
	switch {
	case compSaved == provSaved:
		t.Fatalf("component = %d %s, provider = %d %s; want exactly one saved", comp.StatusCode, compBody, prov.StatusCode, provBody)
	case compSaved && (prov.StatusCode != http.StatusBadRequest || !strings.Contains(provBody, "shared-aud")):
		t.Errorf("provider refused as %d %s, want 400 naming the audience", prov.StatusCode, provBody)
	case provSaved && (comp.StatusCode != http.StatusUnprocessableEntity || !audienceClash(compBody)):
		t.Errorf("component refused as %d %s, want 422 naming the audience", comp.StatusCode, compBody)
	}
}
