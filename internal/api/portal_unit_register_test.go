// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

func newPinRegisterServer(t *testing.T, units ...types.GovernanceUnit) memDelegateServer {
	t.Helper()
	h := newHarness(t)
	ds := newFakeDelegateStore()
	srv := New(baseTestConfig(h, struct {
		treeUnitsStore
		*fakeDelegateStore
	}{treeUnitsStore{Store: nil, units: units}, ds}))
	return memDelegateServer{srv: srv, h: h, ds: ds}
}

// TestRegisterDelegateRunInUnder: run_in_under names an existing unit by id or exact name and is
// answered as the unit's id; omitted, the portal is unpinned and the answer carries no such key.
func TestRegisterDelegateRunInUnder(t *testing.T) {
	plat := types.GovernanceUnit{ID: uuid.New(), Name: "platform"}
	m := newPinRegisterServer(t, plat)
	register := func(pin string) (int, map[string]any) {
		body := `{"name":"p","idp_client_id":"c","group":"g"`
		if pin != "" {
			body += `,"run_in_under":"` + pin + `"`
		}
		w := do(t, m.srv, http.MethodPost, "/api/v1/admin/delegates", adminToken, body+`}`)
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	for _, pin := range []string{"platform", plat.ID.String()} {
		if code, out := register(pin); code != http.StatusCreated || out["run_in_under"] != plat.ID.String() {
			t.Errorf("pin %q: %d %v, want 201 with run_in_under %s", pin, code, out, plat.ID)
		}
	}
	if code, out := register(""); code != http.StatusCreated {
		t.Errorf("no pin: %d %v, want 201", code, out)
	} else if _, ok := out["run_in_under"]; ok {
		t.Errorf("an unpinned portal answers run_in_under: %v", out)
	}
	rows := m.h.auditOutcomes("delegate.create")
	if len(rows) != 3 {
		t.Fatalf("delegate.create rows = %d, want 3", len(rows))
	}
	var pinnedRows int
	for _, ev := range m.h.audit.snapshot() {
		if ev.Action == "delegate.create" && strings.Contains(string(ev.Data), `"run_in_under":"`+plat.ID.String()+`"`) {
			pinnedRows++
		}
	}
	if pinnedRows != 2 {
		t.Errorf("rows naming the pin = %d, want 2 (the unpinned registration writes the bytes it always did)", pinnedRows)
	}
}

// TestRegisterDelegateRunInUnderRefusals: a unit that does not exist, by name or by id, is refused and
// nothing is registered.
func TestRegisterDelegateRunInUnderRefusals(t *testing.T) {
	m := newPinRegisterServer(t, types.GovernanceUnit{ID: uuid.New(), Name: "platform"})
	for _, pin := range []string{"no-such-unit", uuid.NewString()} {
		w := do(t, m.srv, http.MethodPost, "/api/v1/admin/delegates", adminToken,
			`{"name":"p","idp_client_id":"c","group":"g","run_in_under":"`+pin+`"}`)
		if w.Code != http.StatusUnprocessableEntity || reasonOf(t, w) != "delegate_run_in_under_invalid" {
			t.Errorf("pin %q: %d %s, want 422 delegate_run_in_under_invalid", pin, w.Code, w.Body.String())
		}
	}
	if list, _ := m.ds.ListDelegates(context.Background()); len(list) != 0 {
		t.Errorf("refused registrations stored %d portal(s)", len(list))
	}
}
