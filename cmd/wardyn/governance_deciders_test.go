// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// fakeDeciders answers GET /governance with one unit and records the PUT to its deciders route.
type fakeDeciders struct {
	unit sdk.GovernanceUnit
	// hold answers the PUT with a held change.
	hold bool
	put  []string
}

func (f *fakeDeciders) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/governance":
			_ = json.NewEncoder(w).Encode(sdk.GovernanceDocument{Units: []sdk.GovernanceUnit{f.unit}})
		case r.Method == http.MethodPut && r.URL.Path == "/api/v1/governance/units/"+f.unit.ID.String()+"/deciders":
			b, _ := io.ReadAll(r.Body)
			f.put = append(f.put, string(b))
			if f.hold {
				w.WriteHeader(http.StatusAccepted)
				_ = json.NewEncoder(w).Encode(map[string]any{"pending_change": map[string]any{
					"id": uuid.New(), "target_kind": "governance_unit_deciders", "op": "update", "target_key": f.unit.ID.String(),
					"state": "pending", "proposed_by": "alice",
					"proposed_at": time.Now().UTC().Format(time.RFC3339),
					"expires_at":  time.Now().Add(72 * time.Hour).UTC().Format(time.RFC3339),
				}})
				return
			}
			var req struct {
				Rules []json.RawMessage `json:"rules"`
			}
			_ = json.Unmarshal(b, &req)
			_ = json.NewEncoder(w).Encode(map[string]any{"unit_id": f.unit.ID, "rules": req.Rules})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

func writeDecidersFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "deciders.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

const decidersFileBody = `{"rules":[{"kinds":["egress_domain"],"who":["owner","group:eng-leads"],"max_scope":"run"}]}`

func TestGovernanceDeciders_SetByName(t *testing.T) {
	f := &fakeDeciders{unit: sdk.GovernanceUnit{ID: uuid.New(), Name: "Platform"}}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	out, _, err := runGovernance(t, srv.URL, "deciders", "set", "Platform", "--file", writeDecidersFile(t, decidersFileBody))
	if err != nil {
		t.Fatalf("deciders set: %v", err)
	}
	if len(f.put) != 1 || !strings.Contains(f.put[0], `"group:eng-leads"`) || !strings.Contains(f.put[0], `"max_scope":"run"`) {
		t.Fatalf("PUT bodies = %v", f.put)
	}
	if !strings.Contains(out, f.unit.ID.String()) || !strings.Contains(out, "egress_domain") {
		t.Errorf("stdout = %q, want the stored rules", out)
	}
}

func TestGovernanceDeciders_SetByIDClearsWithEmptyList(t *testing.T) {
	f := &fakeDeciders{unit: sdk.GovernanceUnit{ID: uuid.New(), Name: "Platform"}}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	if _, _, err := runGovernance(t, srv.URL, "deciders", "set", f.unit.ID.String(), "--file", writeDecidersFile(t, `{"rules":[]}`)); err != nil {
		t.Fatalf("deciders set: %v", err)
	}
	if len(f.put) != 1 || !strings.Contains(f.put[0], `"rules":[]`) {
		t.Errorf("PUT bodies = %v, want an empty rules list", f.put)
	}
}

func TestGovernanceDeciders_HeldExitsZeroAndPrintsPending(t *testing.T) {
	f := &fakeDeciders{unit: sdk.GovernanceUnit{ID: uuid.New(), Name: "Platform"}, hold: true}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	out, errOut, err := runGovernance(t, srv.URL, "deciders", "set", "Platform", "--file", writeDecidersFile(t, decidersFileBody))
	if err != nil {
		t.Fatalf("a held write is not a failure: %v", err)
	}
	if out != "" || !strings.Contains(errOut, "governance_unit_deciders") {
		t.Errorf("stdout=%q stderr=%q, want the pending change on stderr only", out, errOut)
	}
}

func TestGovernanceDeciders_RefusesBeforeAnyWrite(t *testing.T) {
	f := &fakeDeciders{unit: sdk.GovernanceUnit{ID: uuid.New(), Name: "Platform"}}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"unknown unit":  {[]string{"deciders", "set", "Nope", "--file", writeDecidersFile(t, decidersFileBody)}, `no governance unit named "Nope"`},
		"no rules key":  {[]string{"deciders", "set", "Platform", "--file", writeDecidersFile(t, `{}`)}, "rules"},
		"unknown field": {[]string{"deciders", "set", "Platform", "--file", writeDecidersFile(t, `{"rules":[],"extra":1}`)}, "extra"},
		"missing file":  {[]string{"deciders", "set", "Platform", "--file", filepath.Join(t.TempDir(), "absent.json")}, "absent.json"},
		"no --file":     {[]string{"deciders", "set", "Platform"}, "file"},
	} {
		_, _, err := runGovernance(t, srv.URL, tc.args...)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want it to contain %q", name, err, tc.want)
		}
	}
	if len(f.put) != 0 {
		t.Errorf("a refused command wrote: %v", f.put)
	}
}
