// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// importRowTables are the tables a write of a policy, a secret, a grant, a template, a workload, a run or a token
// would change. The import door must leave every one as it found it.
var importRowTables = []string{"run_policies", "secrets", "credential_grants", "run_templates", "workloads", "agent_runs", "api_tokens", "approvals"}

func (e svcEnv) countRows(t *testing.T) map[string]int {
	t.Helper()
	out := map[string]int{}
	for _, table := range importRowTables {
		var n int
		if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM `+table).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		out[table] = n
	}
	return out
}

// TestServiceIdentity_TemplateImportReadsNothingOfItsOwn: a service identity may POST /templates/import
// (`wardyn diff` and `apply --dry-run` call it to normalise a file). The door only parses and normalises the
// text it is sent, so a document that names a stored policy, a secret grant and a workload the service cannot
// reach is answered with that text, normalised, and nothing else: the answer is byte-equal to an admin's for
// the same document, holds nothing of what is stored, and no row is created. The neighbouring route that
// saves a template stays off the allow-list.
func TestServiceIdentity_TemplateImportReadsNothingOfItsOwn(t *testing.T) {
	e := newSvcEnv(t)
	e.mustCreate(t, svcName, svcSponsor)
	tok := e.mustMint(t, e.sponsor, svcName).Token

	// An admin's stored policy, with a secret grant the service has no part in.
	const storedName = "admin-only-policy-name"
	policyID := e.storePolicy(t, storedName, "admin-only-secret", true)
	const workload = `"experience":"background","workload":{"kind":"agent_task","agent":"claude-code","task":"fix the build"},"no_repositories_or_drives":true`
	docs := []struct{ name, src string }{
		{"names a stored policy", docIntent(`"policy_id":"` + policyID + `",` + workload)},
		{"carries a secret grant", docIntent(`"inline_policy":{"allowed_domains":["dev.azure.com"],"eligible_grants":[` +
			`{"kind":"git_pat","requires_approval":false,"scope":{"host":"dev.azure.com","secret_name":"admin-only-secret"}}]},` + workload)},
	}

	before := e.countRows(t)
	for _, d := range docs {
		name, src := d.name, d.src
		body := docJSON(t, client.TemplateImportRequest{Format: client.TemplateFormatJSON, Kind: client.RunDocumentKind, Source: src})
		svc := e.as(t, tok, http.MethodPost, "/api/v1/templates/import", body)
		adm := do(t, e.h.srv, http.MethodPost, "/api/v1/templates/import", adminToken, body)
		if svc.Code != http.StatusOK || adm.Code != http.StatusOK {
			t.Fatalf("%s: service %d %s, admin %d %s, want 200 for both", name, svc.Code, svc.Body.String(), adm.Code, adm.Body.String())
		}
		if svc.Body.String() != adm.Body.String() {
			t.Errorf("%s: the service's answer differs from the admin's:\nservice %s\nadmin   %s", name, svc.Body.String(), adm.Body.String())
		}
		var res client.TemplateImportResult
		if err := json.Unmarshal(svc.Body.Bytes(), &res); err != nil || res.Document == nil || len(res.Diagnostics) != 0 {
			t.Errorf("%s: answer %s (%v), want the normalised document", name, svc.Body.String(), err)
		}
		if strings.Contains(svc.Body.String(), storedName) {
			t.Errorf("%s: the answer holds the stored policy's name: %s", name, svc.Body.String())
		}
	}
	if after := e.countRows(t); len(after) != len(before) {
		t.Fatalf("row counts %v, want the tables of %v", after, before)
	} else {
		for table, n := range before {
			if after[table] != n {
				t.Errorf("%s rows: %d before the imports, %d after, want no change", table, n, after[table])
			}
		}
	}

	// The same token is still refused the routes it lacks, including the one that saves a template.
	for _, route := range []struct{ method, path string }{
		{http.MethodPost, "/api/v1/templates"},
		{http.MethodGet, "/api/v1/templates"},
	} {
		w := e.as(t, tok, route.method, route.path, `{}`)
		if w.Code != http.StatusForbidden || respReason(w) != "service_scope" {
			t.Errorf("%s %s = %d %s, want 403 service_scope", route.method, route.path, w.Code, strings.TrimSpace(w.Body.String()))
		}
	}
}
