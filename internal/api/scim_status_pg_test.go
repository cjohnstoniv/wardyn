// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// scimStatusOf reads GET /scim/status as the admin.
func (e *scimEnv) scimStatusOf(n *scimNode) scimStatus {
	e.t.Helper()
	w := do(e.t, n.srv, http.MethodGet, "/api/v1/scim/status", adminToken, "")
	if w.Code != http.StatusOK {
		e.t.Fatalf("GET /scim/status = %d %s", w.Code, w.Body.String())
	}
	var out scimStatus
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		e.t.Fatalf("decode scim status: %v: %s", err, w.Body.String())
	}
	return out
}

// The card's read: the deactivated people, the steps still failing with their error, the token slot that
// matched last, and the drives a purge listed that still exist. The test nodes record audit in memory, so
// the rows the status reads from audit_events are mirrored there first.
func TestSCIMStatus(t *testing.T) {
	e := newSCIMEnv(t, func(c *Config) { c.SCIM.PurgeAfter = 48 * time.Hour })
	purged := e.seedPurgeSubject(purgeSub, purgeEmail, purgeOID)
	const stuckSub, stuckEmail = "sub-stuck", "stuck@corp.example"
	e.seedEntra(stuckSub, stuckEmail, "aaaaaaaa-0000-4000-8000-0000000000c1")
	stuck := e.postUserID(e.a, "aaaaaaaa-0000-4000-8000-0000000000c1", stuckEmail, stuckEmail)
	e.seedRun(stuckSub, types.RunRunning)
	const goneSub, goneEmail = "sub-gone", "gone@corp.example"
	e.seedEntra(goneSub, goneEmail, "aaaaaaaa-0000-4000-8000-0000000000c2")
	e.postUserID(e.a, "aaaaaaaa-0000-4000-8000-0000000000c2", goneEmail, goneEmail)

	e.a.runner.setFailKills(1)
	if w := e.patch(e.a, stuck, patchOf("false")); w.Code != http.StatusInternalServerError {
		t.Fatalf("suspend with a failing runner = %d, want 500", w.Code)
	}
	e.suspendOn(e.a, goneEmail)
	if w := e.del(e.a, purged.id); w.Code != http.StatusNoContent {
		t.Fatalf("DELETE = %d", w.Code)
	}
	for _, ev := range e.a.h.audit.snapshot() {
		if err := store.InsertAuditEvent(context.Background(), e.pool, &ev); err != nil {
			t.Fatal(err)
		}
	}

	got := e.scimStatusOf(e.a)
	if !got.Configured || got.LastTokenSlot != scimSlotPrimary || got.PurgeAfterSeconds != int64((48*time.Hour)/time.Second) || got.KeepWorkspaces {
		t.Errorf("facts = %+v, want configured, primary, 48h, workspaces handed over", got)
	}
	var names []string
	for _, d := range got.Deactivated {
		names = append(names, d.Person)
		if d.PurgeAfter == nil {
			t.Errorf("%s has no purge schedule with a purge delay set", d.Person)
		}
	}
	slices.Sort(names)
	if !slices.Equal(names, []string{goneEmail, stuckEmail}) {
		t.Errorf("deactivated = %v, want the two suspended people and not the purged one", names)
	}
	if len(got.Pending) != 1 || got.Pending[0].Person != stuckEmail || got.Pending[0].Step != jobStepKillRun || got.Pending[0].LastError == "" {
		t.Errorf("pending = %+v, want the stuck person's kill_run with its error", got.Pending)
	}
	if len(got.Drives) != 1 || got.Drives[0].Person != purgeEmail || got.Drives[0].Drive != purged.drive {
		t.Errorf("drives = %+v, want %q of %s", got.Drives, purged.drive, purgeEmail)
	}

	// A reclaimed drive leaves the list.
	grants, err := e.st.ListUserDriveGrants(context.Background())
	if err != nil || len(grants) == 0 {
		t.Fatalf("drive grants = %d, %v", len(grants), err)
	}
	if _, err := e.pool.Exec(context.Background(), `DELETE FROM user_drive_grants`); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(context.Background(), `DELETE FROM user_drives`); err != nil {
		t.Fatal(err)
	}
	if got := e.scimStatusOf(e.a); len(got.Drives) != 0 {
		t.Errorf("drives after the reclaim = %+v, want none", got.Drives)
	}
}

// With SCIM off the card still renders, so the read answers configured false and empty lists, not null.
func TestSCIMStatusUnconfigured(t *testing.T) {
	e := newSCIMEnv(t, func(c *Config) { c.SCIM = nil })
	w := do(t, e.a.srv, http.MethodGet, "/api/v1/scim/status", adminToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /scim/status = %d %s", w.Code, w.Body.String())
	}
	var raw map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if raw["configured"] != false || raw["deactivated"] == nil || raw["pending"] == nil || raw["drives"] == nil {
		t.Errorf("body = %s, want configured false and three empty lists", w.Body.String())
	}
}
