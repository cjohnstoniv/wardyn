// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// TestRenameMap pins the 0.10.0 clean break: the governance surface (units, memberships, their
// refusals and audit rows) answers exactly what the 0.9 surface answered once every renamed key and
// value is mapped back. testdata/rename/corpus.origin.json is that surface as the base of this lane
// recorded it (RENAME_CAPTURE_ORIGIN=<path> re-records it on a tree with the old names); testdata/
// rename/map.json is the closed list of renames, written by hand and read by nothing else, so a key
// the branch spells differently from the map fails here.
//
// Guarded by WARDYN_TEST_PG (throwawayPGPool): skipped cleanly when unset, must PASS when set.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/store"
)

type renameMapFile struct {
	Keys   map[string]string `json:"keys"`
	Values map[string]string `json:"values"`
}

var renameTimestamp = regexp.MustCompile(`\d{4}-\d\d-\d\dT[\d:.]+(Z|[+-]\d\d:\d\d)`)

// renameStep is one request of the scenario and what it answered.
type renameStep struct {
	Step   string `json:"step"`
	Status int    `json:"status"`
	Body   any    `json:"body"`
}

// rekey returns v with every object key and every string value mapped through m. Both sides of the
// comparison are decoded JSON, so key order never matters.
func rekey(v any, m renameMapFile) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			if nk, ok := m.Keys[k]; ok {
				k = nk
			}
			out[k] = rekey(val, m)
		}
		// A diff lists its changed fields sorted by name, so a renamed field moves in the list.
		if arr, ok := out["changed"].([]any); ok {
			sort.Slice(arr, func(i, j int) bool { return fmt.Sprint(arr[i]) < fmt.Sprint(arr[j]) })
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i := range x {
			out[i] = rekey(x[i], m)
		}
		return out
	case string:
		if nv, ok := m.Values[x]; ok {
			return nv
		}
		return x
	}
	return v
}

func decodeAny(t *testing.T, raw string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, raw)
	}
	return v
}

func TestRenameMap(t *testing.T) {
	pool := throwawayPGPool(t)
	ctx := context.Background()
	h := newHarness(t)
	srv := New(baseTestConfig(h, store.NewPG(pool)))

	ids := map[string]string{}
	type rawStep struct {
		step   string
		status int
		body   string
	}
	var raws []rawStep
	call := func(step, method, path, body string) string {
		w := do(t, srv, method, path, adminToken, body)
		raws = append(raws, rawStep{step, w.Code, w.Body.String()})
		return w.Body.String()
	}
	mask := func(text string) string {
		for id, name := range ids {
			text = strings.ReplaceAll(text, id, name)
		}
		return renameTimestamp.ReplaceAllString(text, "<time>")
	}
	idOf := func(raw string, path ...string) string {
		var v any = decodeAny(t, raw)
		for _, p := range path {
			v = v.(map[string]any)[p]
		}
		return v.(string)
	}

	raw := call("create standalone", http.MethodPost, "/api/v1/governance/units",
		`{"name":"alpha","ceiling":{"allowed_domains":["pypi.org"],"min_confinement_class":"CC2"},"limits":{"max_concurrent_runs":3}}`)
	alpha := idOf(raw, "unit", "id")
	ids[alpha] = "<alpha>"
	call("duplicate name", http.MethodPost, "/api/v1/governance/units", `{"name":"alpha","ceiling":{"allowed_domains":["pypi.org"]}}`)
	raw = call("create composed", http.MethodPost, "/api/v1/governance/units",
		`{"name":"beta","parent_id":"`+alpha+`","overlay":{"denied_domains":["blocked.example"]}}`)
	beta := idOf(raw, "unit", "id")
	ids[beta] = "<beta>"
	call("unit without a name", http.MethodPost, "/api/v1/governance/units", `{"ceiling":{}}`)
	raw = call("membership", http.MethodPost, "/api/v1/governance/memberships",
		`{"subject_type":"user","subject":"alice@corp.example","unit_id":"`+beta+`","priority":2}`)
	member := idOf(raw, "id")
	ids[member] = "<membership>"
	call("membership of no unit", http.MethodPost, "/api/v1/governance/memberships",
		`{"subject_type":"user","subject":"bob@corp.example","unit_id":"00000000-0000-4000-8000-000000000001"}`)
	call("membership without a unit", http.MethodPost, "/api/v1/governance/memberships", `{"subject_type":"user","subject":"bob@corp.example"}`)
	call("read", http.MethodGet, "/api/v1/governance", "")
	call("check", http.MethodPost, "/api/v1/governance/check", `{"user_subjects":["alice@corp.example"]}`)
	call("check nobody", http.MethodPost, "/api/v1/governance/check", `{"user_subjects":["nobody@corp.example"]}`)
	call("update", http.MethodPut, "/api/v1/governance/units/"+beta,
		`{"name":"beta","parent_id":"`+alpha+`","overlay":{"denied_domains":["blocked.example","other.example"]}}`)
	call("update unknown", http.MethodPut, "/api/v1/governance/units/00000000-0000-4000-8000-000000000002", `{"name":"ghost","ceiling":{}}`)
	call("delete with a child", http.MethodDelete, "/api/v1/governance/units/"+alpha, "")
	call("delete in use", http.MethodDelete, "/api/v1/governance/units/"+beta, "")
	call("delete membership", http.MethodDelete, "/api/v1/governance/memberships/"+member, "")
	call("delete membership again", http.MethodDelete, "/api/v1/governance/memberships/"+member, "")
	call("delete", http.MethodDelete, "/api/v1/governance/units/"+beta, "")

	var steps []renameStep
	for _, r := range raws {
		var parsed any
		if text := mask(r.body); strings.TrimSpace(text) != "" {
			parsed = decodeAny(t, text)
		}
		steps = append(steps, renameStep{Step: r.step, Status: r.status, Body: parsed})
	}

	type auditRow struct {
		Action string `json:"action"`
		Data   any    `json:"data"`
	}
	var audit []auditRow
	for _, ev := range h.audit.snapshot() {
		if !strings.HasPrefix(ev.Action, "governance.") {
			continue
		}
		text := mask(string(ev.Data))
		var data any
		if strings.TrimSpace(text) != "" {
			data = decodeAny(t, text)
		}
		audit = append(audit, auditRow{ev.Action, data})
	}
	got := rekey(decodeAny(t, mustJSONString(t, map[string]any{"steps": steps, "audit": audit})), renameMapFile{})

	if path := os.Getenv("RENAME_CAPTURE_ORIGIN"); path != "" {
		if err := os.WriteFile(path, []byte(mustJSONString(t, got)+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Skipf("recorded %s", path)
	}

	var m renameMapFile
	if err := json.Unmarshal(mustReadFile(t, "testdata/rename/map.json"), &m); err != nil {
		t.Fatal(err)
	}
	want := rekey(decodeAny(t, string(mustReadFile(t, "testdata/rename/corpus.origin.json"))), m)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the branch's governance surface differs from the origin corpus re-keyed by testdata/rename/map.json\n got: %s\nwant: %s",
			mustJSONString(t, got), mustJSONString(t, want))
	}
	_ = ctx
}

func mustJSONString(t *testing.T, v any) string {
	t.Helper()
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
