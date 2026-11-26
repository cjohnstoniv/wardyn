// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/secretstore/subjectkey/subjectkeytest"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestAuditSalt_NeverLogged_PG: through an append, a chain verify, a device ingest that succeeds, one that is
// refused, and the two reads the sinks and the forwarder use, neither the salt a device plants in its claim nor
// any salt the database stored for a row appears in a log record, an error string or the sink read's JSON.
// The forwarder's own read carries the salt by design and is not scanned.
func TestAuditSalt_NeverLogged_PG(t *testing.T) {
	pool := subjectkeytest.ThrowawayDB(t)
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	ctx := t.Context()
	st := store.NewPG(pool)
	var errs []string
	note := func(err error) {
		if err != nil {
			errs = append(errs, err.Error())
		}
	}

	base := int64(0)
	if err := pool.QueryRow(ctx, `SELECT COALESCE(max(seq), 0) FROM audit_events`).Scan(&base); err != nil {
		t.Fatal(err)
	}
	ev := types.AuditEvent{ID: uuid.New(), Time: time.Now().UTC().Truncate(time.Microsecond), ActorType: types.ActorSystem, Actor: "salt-probe", Action: "salt.probe", Outcome: "success"}
	note(store.InsertAuditEvent(ctx, pool, &ev))
	_, err := st.VerifyAuditChain(ctx)
	note(err)

	dev, err := st.CreateDevice(ctx, types.Device{ID: uuid.New(), Name: "salt-laptop"}, "wdd_"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	planted := strings.Repeat("7e", 32)
	claim := types.AuditEvent{ID: uuid.New(), Time: time.Now().UTC().Truncate(time.Microsecond), ActorType: types.ActorSystem,
		Actor: "device-forwarder", Action: "salt.claim", Outcome: "success", SourceIP: "10.9.9.9", Data: json.RawMessage(`{"k": 1}`)}
	if err := pool.QueryRow(ctx, `SELECT audit_row_hash_v2($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12::jsonb)`,
		"", planted, claim.ID, claim.Time, claim.RunID, string(claim.ActorType), claim.Actor, claim.Action, claim.Target, claim.Outcome, claim.SourceIP, []byte(claim.Data)).Scan(&claim.RowHash); err != nil {
		t.Fatal(err)
	}
	good := types.FederatedAuditRow{FederatedAuditEvent: types.FederatedAuditEvent{AuditEvent: claim, Seq: 1}, HashFormat: 2, HashSalt: planted}
	_, err = st.IngestDeviceAudit(ctx, dev.ID, "10.9.9.9", []types.FederatedAuditRow{good})
	note(err)
	bad := good
	bad.Action = "salt.edited"
	_, err = st.IngestDeviceAudit(ctx, dev.ID, "10.9.9.9", []types.FederatedAuditRow{bad})
	if err == nil {
		t.Fatal("an edited claim was accepted")
	}
	note(err)
	malformed := good
	malformed.HashSalt = planted[:63]
	_, err = st.IngestDeviceAudit(ctx, dev.ID, "10.9.9.9", []types.FederatedAuditRow{malformed})
	note(err)

	events, err := st.ListAuditEventsAfterSeq(ctx, base, 1000)
	if err != nil || len(events) == 0 {
		t.Fatalf("sink read: %d rows, %v", len(events), err)
	}
	sinkJSON, _ := json.Marshal(events)

	secrets := []string{planted}
	rows, err := pool.Query(ctx, `SELECT hash_salt FROM audit_events WHERE hash_salt IS NOT NULL UNION SELECT claim_salt FROM audit_events WHERE claim_salt IS NOT NULL`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		secrets = append(secrets, s)
	}
	rows.Close()
	if len(secrets) < 3 {
		t.Fatalf("only %d salts to look for; the test proves nothing", len(secrets))
	}
	haystacks := map[string]string{"log": logs.String(), "errors": strings.Join(errs, "\n"), "sink read": string(sinkJSON)}
	for name, h := range haystacks {
		for _, s := range secrets {
			if strings.Contains(h, s) {
				t.Errorf("the %s names a salt (%s...)", name, fmt.Sprint(s[:4]))
			}
		}
	}
}
