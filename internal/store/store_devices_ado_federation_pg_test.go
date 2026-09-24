// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A laptop that signs in to Azure DevOps locally forwards the three action
// families that evidence it — scm.ado.signin.captured,
// credential.capability.requested and the egress rows whose rule_source is
// brokered:ado* — and the organisation's own audit filters (the ones GET
// /api/v1/audit applies) find them as they find its own rows: by action, by
// the person, by outcome and by the laptop's run, with the payload a SIEM keys
// on (rule_source, owner, source) as the laptop wrote it.
func TestPG_Devices_ADOAuditFamiliesFederateAndFilter(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	st := store.NewPG(pool)
	d := federationDevice(t, st)
	person := "entra-sub-" + uuid.NewString()
	laptopRun := uuid.New() // the laptop's own run, not in this organisation's agent_runs
	agent := "spiffe://wardyn.local/run/" + laptopRun.String()
	brokered := []string{"brokered:ado", "brokered:ado:denied", "brokered:ado-git", "brokered:ado-git:upstream-not-signed-in"}

	type claim struct {
		runID     *uuid.UUID
		actorType types.ActorType
		actor     string
		action    string
		target    string
		outcome   string
		data      map[string]any
	}
	claims := []claim{
		{nil, types.ActorHuman, person, "scm.ado.signin.captured", "wardyn-harness-ado-row1-oauth", "success", map[string]any{
			"provider": "ado", "source": "login", "tenant_id": "t-1", "client_id": "c-1",
			"scopes": []string{"499b84ac-1321-427f-aa17-267ca6975798/vso.code"}, "expires_at": "2026-09-23T12:00:00Z"}},
		{nil, types.ActorHuman, person, "scm.ado.signin.captured", "wardyn-harness-ado-row1-oauth", "failure", map[string]any{
			"provider": "ado", "source": "login", "reason": "store_error", "error": "secret store unavailable"}},
		{&laptopRun, types.ActorSystem, "wardynd", "credential.capability.requested", uuid.NewString(), "success", map[string]any{
			"capability": "code_write", "organisation": "contoso", "provider_row": "row1", "owner": person,
			"first_use": "wait_for_review", "method": "POST", "path": "/contoso/proj/_apis/git/pullrequests"}},
	}
	for _, src := range brokered {
		action, outcome := "egress.allow", "success"
		if strings.Contains(src, ":denied") || strings.Contains(src, "upstream") {
			action, outcome = "egress.deny", "denied"
		}
		claims = append(claims, claim{&laptopRun, types.ActorAgent, agent, action, "dev.azure.com", outcome, map[string]any{
			"host": "dev.azure.com", "port": 443, "method": "GET", "path": "/contoso/_apis/" + src, "rule_source": src}})
	}

	var rows []types.FederatedAuditEvent
	prev := ""
	for i, c := range claims {
		data, err := json.Marshal(c.data)
		if err != nil {
			t.Fatal(err)
		}
		ev := types.AuditEvent{ID: uuid.New(), Time: time.Now().UTC().Truncate(time.Microsecond), RunID: c.runID,
			ActorType: c.actorType, Actor: c.actor, Action: c.action, Target: c.target, Outcome: c.outcome,
			SourceIP: "192.168.1.20", PrevHash: prev, Data: data}
		ev.RowHash = auditRowHash(t, pool, prev, ev)
		prev = ev.RowHash
		rows = append(rows, types.FederatedAuditEvent{AuditEvent: ev, Seq: int64(i + 1)})
	}
	if res, err := st.IngestDeviceAudit(ctx, d.ID, testPeer, rows); err != nil || res.Accepted != len(rows) {
		t.Fatalf("ingest: %+v %v, want all %d rows accepted", res, err, len(rows))
	}

	query := func(runID *uuid.UUID, f store.AuditFilter) []types.AuditEvent {
		t.Helper()
		got, err := st.QueryAuditEventsFilteredPage(ctx, runID, f, store.Page{Limit: 100})
		if err != nil {
			t.Fatal(err)
		}
		for _, ev := range got {
			if !f.Matches(ev) {
				t.Errorf("SQL filter %+v returned %s %q that the Go filter rejects", f, ev.Action, ev.Actor)
			}
			var origin struct {
				DeviceOrigin struct {
					DeviceID string `json:"device_id"`
				} `json:"device_origin"`
			}
			if json.Unmarshal(ev.Data, &origin) != nil || origin.DeviceOrigin.DeviceID != d.ID.String() {
				t.Errorf("%s row data %s does not name device %s as its origin", ev.Action, ev.Data, d.ID)
			}
		}
		return got
	}
	field := func(ev types.AuditEvent, key string) string {
		var m map[string]any
		_ = json.Unmarshal(ev.Data, &m)
		s, _ := m[key].(string)
		return s
	}

	signins := query(nil, store.AuditFilter{Action: "scm.ado.signin.captured", Actor: person})
	if len(signins) != 2 {
		t.Fatalf("?action=scm.ado.signin.captured&actor=<person>: %d rows, want 2", len(signins))
	}
	for _, ev := range signins {
		if ev.ActorType != types.ActorHuman || field(ev, "source") != "login" || ev.SourceIP != testPeer {
			t.Errorf("sign-in row %+v: want a human actor, source login, source_ip the observed peer", ev)
		}
	}
	if got := query(nil, store.AuditFilter{Action: "scm.ado.signin.captured", Actor: person, Outcome: "failure"}); len(got) != 1 {
		t.Errorf("?outcome=failure on the sign-ins: %d rows, want 1", len(got))
	}
	if got := query(nil, store.AuditFilter{Actor: person}); len(got) != 2 {
		t.Errorf("?actor=<person>: %d rows, want the person's 2 sign-ins", len(got))
	}

	caps := query(&laptopRun, store.AuditFilter{Action: "credential.capability.requested"})
	if len(caps) != 1 || field(caps[0], "owner") != person {
		t.Fatalf("?action=credential.capability.requested under the laptop run: %+v, want one row owned by the person", caps)
	}

	egress := query(&laptopRun, store.AuditFilter{ActionPrefix: "egress."})
	if len(egress) != len(brokered) {
		t.Fatalf("?action_prefix=egress. under the laptop run: %d rows, want %d", len(egress), len(brokered))
	}
	for i, ev := range egress {
		if ev.ActorType != types.ActorAgent || field(ev, "rule_source") != brokered[i] {
			t.Errorf("egress row %d: %s rule_source %q, want an agent row with rule_source %q", i, ev.Action, field(ev, "rule_source"), brokered[i])
		}
	}
	if got := query(&laptopRun, store.AuditFilter{Action: "egress.deny"}); len(got) != 2 {
		t.Errorf("?action=egress.deny under the laptop run: %d rows, want the 2 brokered:ado refusals", len(got))
	}
}
