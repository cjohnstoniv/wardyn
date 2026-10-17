// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Every row a refused dry run writes is marked dry_run, repeats are summarised
// by an appended row, and the same refusal at launch (run_id NULL too) is
// neither marked nor coalesced.
func TestPreflightMarksAndCoalescesDryRunDenials(t *testing.T) {
	h := newHarness(t)
	h.srv.cfg.OIDC = &oidc.Authenticator{}
	below := &recRecorder{}
	denials := &audit.DenialCoalescer{Inner: below}
	h.srv.cfg.Audit = audit.DelegationRecorder{Inner: audit.DryRunRecorder{Inner: denials}}
	h.srv.router = h.srv.routes()
	member := ssoSession(t, "sub-member", "member@corp.example", oidc.RoleUser)
	const body = `{"agent":"claude-code","image":"ubuntu:24.04"}`

	rows := func(action string) []types.AuditEvent {
		var out []types.AuditEvent
		for _, ev := range below.snapshot() {
			if ev.Action == action {
				out = append(out, ev)
			}
		}
		return out
	}
	for range 3 {
		if w := doSSO(t, h.srv, http.MethodPost, "/api/v1/runs/preflight", member, body); w.Code != http.StatusForbidden {
			t.Fatalf("preflight = %d, want 403: %s", w.Code, w.Body.String())
		}
	}
	denied := rows("authz.denied")
	if len(denied) != 1 {
		t.Fatalf("%d authz.denied rows after three identical dry runs, want 1", len(denied))
	}
	var first map[string]any
	if err := json.Unmarshal(denied[0].Data, &first); err != nil || first["dry_run"] != true || first["reason"] != "byoi_user" {
		t.Fatalf("first row data = %s (err %v), want dry_run true and the full datum", denied[0].Data, err)
	}
	if denied[0].RunID != nil {
		t.Errorf("run_id = %v, want NULL", denied[0].RunID)
	}

	// The same refusal at launch: unmarked, and written every time.
	for range 3 {
		if w := doSSO(t, h.srv, http.MethodPost, "/api/v1/runs", member, body); w.Code != http.StatusForbidden {
			t.Fatalf("launch = %d, want 403: %s", w.Code, w.Body.String())
		}
	}
	denied = rows("authz.denied")
	if len(denied) != 4 {
		t.Fatalf("%d authz.denied rows after three launch refusals, want 4 in all", len(denied))
	}
	for _, ev := range denied[1:] {
		var d map[string]any
		if err := json.Unmarshal(ev.Data, &d); err != nil {
			t.Fatal(err)
		}
		if _, ok := d["dry_run"]; ok || ev.RunID != nil {
			t.Errorf("launch refusal data = %s run_id = %v, want unmarked with run_id NULL", ev.Data, ev.RunID)
		}
	}

	denials.Flush(context.Background())
	sum := rows(audit.CoalesceAction)
	if len(sum) != 1 {
		t.Fatalf("%d summary rows, want 1", len(sum))
	}
	var d map[string]any
	if err := json.Unmarshal(sum[0].Data, &d); err != nil || d["count"] != float64(3) || d["reason"] != "byoi_user" || d["dry_run"] != true {
		t.Errorf("summary data = %s (err %v), want count 3 for byoi_user", sum[0].Data, err)
	}
}
