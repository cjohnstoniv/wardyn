// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A held push's path list is inlined into the export for the security tier; a unit admin has no export,
// and the unit view of the row holds no paths.
func TestAuditUnitViewAndExportServeNoPushPaths(t *testing.T) {
	e := newUnitAuditEnv(t)
	ctx := context.Background()
	ap, err := e.pg.CreateApproval(ctx, types.ApprovalRequest{ID: uuid.New(), RunID: e.runA, Kind: types.ApprovalPushContent,
		State: types.ApprovalPending, RequestedAt: time.Now().UTC(), RequestedScope: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatalf("create approval: %v", err)
	}
	const held = "plans/acquisition-of-initech.md"
	if ok, err := e.pg.RecordPushPathList(ctx, ap.ID, e.runA, types.PushPathList{Paths: []string{held}}, 10); !ok || err != nil {
		t.Fatalf("record path list: %v %v", ok, err)
	}
	e.seedRow(&e.runA, pushPathsAuditAction, ap.ID.String(), map[string]any{"approval_id": ap.ID.String(), "paths_total": 1})

	w := e.call(e.adminA, http.MethodGet, "/api/v1/audit/export", "")
	if w.Code != http.StatusOK || w.Body.Len() != 0 {
		t.Fatalf("adminA's export: %d, %d bytes, want an empty 200 (a unit admin has no export)", w.Code, w.Body.Len())
	}
	row, ok := uaRow(e.read(e.adminA, "/api/v1/audit", ""), e.runA, pushPathsAuditAction)
	if !ok {
		t.Fatalf("adminA's view has no %s row of ua-a's run", pushPathsAuditAction)
	}
	if _, has := uaDataKeys(t, row)["paths"]; has {
		t.Errorf("adminA's view serves paths on %s: %s", pushPathsAuditAction, row.Data)
	}
	sec, ok := uaRow(e.read(e.alice, "/api/v1/audit/export", "?action="+pushPathsAuditAction), e.runA, pushPathsAuditAction)
	if !ok || !strings.Contains(string(uaDataKeys(t, sec)["paths"]), held) {
		t.Errorf("the security tier's export of %s = %v %s, want the path list inlined", pushPathsAuditAction, ok, sec.Data)
	}
}

// Free text under an allowed key (an error's text, a sentence) is not served to a unit admin; codes are.
func TestAuditUnitViewServesNoFreeText(t *testing.T) {
	e := newUnitAuditEnv(t)
	e.seedRow(&e.runA, "run.env_secret.resolve", e.runA.String(), map[string]any{
		"reason":   "scope invalid: secret prod-db-password is not in /home/alice/scope.json",
		"cause":    "dial tcp 10.0.0.5:5432: connection refused",
		"state":    "waiting on alice",
		"hosts":    []any{"api.example", "the host alice named"},
		"provider": "anthropic", "mode": "inject", "count": 2,
	})
	for _, path := range uaViewPaths {
		for name, evs := range map[string][]types.AuditEvent{
			"trail": e.read(e.adminA, path, "?run_id="+e.runA.String()),
			"feed":  e.read(e.adminA, path, ""),
		} {
			row, ok := uaRow(evs, e.runA, "run.env_secret.resolve")
			if !ok {
				t.Errorf("%s %s: no run.env_secret.resolve row of ua-a's run", path, name)
				continue
			}
			keys := uaDataKeys(t, row)
			for _, k := range []string{"reason", "cause", "state", "hosts"} {
				if v, has := keys[k]; has {
					t.Errorf("%s %s: the unit view serves free text under %q: %s", path, name, k, v)
				}
			}
			if string(keys["provider"]) != `"anthropic"` || string(keys["mode"]) != `"inject"` || string(keys["count"]) != "2" {
				t.Errorf("%s %s: the row's codes were not served: %s", path, name, row.Data)
			}
			if deny, _ := uaRow(evs, e.runA, "egress.deny"); string(uaDataKeys(t, deny)["reason"]) != `"not_allowed"` {
				t.Errorf("%s %s: egress.deny lost its reason code: %s", path, name, deny.Data)
			}
		}
	}
}

// ?target= answers a unit admin the same whether a target the unit view withholds is stored or not.
func TestAuditUnitTargetFilterIsNoOracle(t *testing.T) {
	e := newUnitAuditEnv(t)
	missing := "no-such-secret-" + uuid.NewString()
	for _, path := range uaPaths {
		for _, base := range []string{"", "run_id=" + e.runA.String() + "&"} {
			for _, extra := range []string{"", "&action=secret.read", "&action_prefix=secret."} {
				q := func(target string) string { return "?" + base + "target=" + url.QueryEscape(target) + extra }
				got := e.call(e.adminA, http.MethodGet, path+q("prod-db-password"), "")
				want := e.call(e.adminA, http.MethodGet, path+q(missing), "")
				if got.Code != want.Code || got.Header().Get("Content-Type") != want.Header().Get("Content-Type") ||
					!bytes.Equal(got.Body.Bytes(), want.Body.Bytes()) {
					t.Errorf("%s%s: a stored withheld target answers %d %q, a missing one %d %q", path, q("<target>"),
						got.Code, got.Body.String(), want.Code, want.Body.String())
				}
			}
		}
		if path != "/api/v1/audit" {
			continue // the export has no rows to serve a unit admin
		}
		if got := e.read(e.adminA, path, "?action_prefix=egress.&target=evil.example"); len(got) != 3 {
			t.Errorf("%s: a target filter on egress rows served %d rows, want the 3 of the subtree's runs", path, len(got))
		}
	}
}
