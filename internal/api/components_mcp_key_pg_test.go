// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The organisation component write door keeps MCP server keys unique: an
// organisation's tool rules name a server by its key, so two organisation MCP
// components may not share one. Guarded by WARDYN_TEST_PG; skipped cleanly
// when unset.

import (
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
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
