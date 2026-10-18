// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The organisation component write door keeps MCP server keys unique: an
// organisation's tool rules name a server by its key, so two organisation MCP
// components may not share one. Guarded by WARDYN_TEST_PG; skipped cleanly
// when unset.

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func orgMCPDef(host string) string {
	return `{"hosts":["` + host + `"],"mcp":{"url":"https://` + host + `/mcp","transport":"http"}}`
}

func TestComponents_OrgMCPKeyCollisionRefused(t *testing.T) {
	e := newComponentsPG(t)
	put := func(id uuid.UUID, name, def string) *http.Response {
		w := e.do(t, http.MethodPut, "/api/v1/components/"+id.String(), e.admin, saveComponentBody(name, def))
		return w.Result()
	}
	first, prod := uuid.New(), uuid.New()
	for id, name := range map[uuid.UUID]string{first: "Linear", prod: "Atlassian Confluence Cloud Production"} {
		if res := put(id, name, orgMCPDef("mcp."+strings.ToLower(strings.Fields(name)[0])+".test")); res.StatusCode != http.StatusCreated {
			t.Fatalf("create %q = %d", name, res.StatusCode)
		}
	}
	// Same key after slugging, and after the cut to the key's length.
	for _, name := range []string{"Linear.", "linear!", "Atlassian Confluence Cloud Production EU"} {
		w := e.do(t, http.MethodPut, "/api/v1/components/"+uuid.NewString(), e.admin, saveComponentBody(name, orgMCPDef("mcp.other.test")))
		if w.Code != http.StatusConflict || errorReasonOf(w.Body.String()) != reasonComponentNameConflict || !strings.Contains(w.Body.String(), "server key") {
			t.Errorf("%q = %d %s, want 409 %s naming the server key", name, w.Code, w.Body.String(), reasonComponentNameConflict)
		}
	}
	// A component that is no MCP server, and the server's own update, are not refused.
	if res := put(uuid.New(), "Linear!", `{"hosts":["api.linear.test"]}`); res.StatusCode != http.StatusCreated {
		t.Errorf("non-MCP component named like it = %d, want 201", res.StatusCode)
	}
	if res := put(first, "Linear", orgMCPDef("mcp.linear.test")); res.StatusCode != http.StatusOK {
		t.Errorf("update of the server itself = %d, want 200", res.StatusCode)
	}
}

// listBarrierStore holds the first component list until a second one is read, up to a second, so two
// saves that check the server key without a lock both read the table before either writes.
type listBarrierStore struct {
	store.PG
	mu     sync.Mutex
	calls  int
	second chan struct{}
}

func (b *listBarrierStore) ListComponents(ctx context.Context, owner string) ([]types.Component, error) {
	rows, err := b.PG.ListComponents(ctx, owner)
	b.mu.Lock()
	b.calls++
	n := b.calls
	b.mu.Unlock()
	switch n {
	case 1:
		select {
		case <-b.second:
		case <-time.After(time.Second):
		}
	case 2:
		close(b.second)
	}
	return rows, err
}

// Two concurrent saves of organisation MCP servers whose names give one key: exactly one is saved.
func TestComponents_ConcurrentOrgMCPKeyCollisionSavesOne(t *testing.T) {
	e := newComponentsPG(t)
	e.h.srv.cfg.Store = &listBarrierStore{PG: e.st, second: make(chan struct{})}
	codes := make([]int, 2)
	var wg sync.WaitGroup
	for i, name := range []string{"Linear", "Linear!"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i] = e.do(t, http.MethodPut, "/api/v1/components/"+uuid.NewString(), e.admin, saveComponentBody(name, orgMCPDef("mcp.linear.test"))).Code
		}()
	}
	wg.Wait()
	slices.Sort(codes)
	if codes[0] != http.StatusCreated || codes[1] != http.StatusConflict {
		t.Fatalf("concurrent saves = %v, want one 201 and one 409", codes)
	}
}
