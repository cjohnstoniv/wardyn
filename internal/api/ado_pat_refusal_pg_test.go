// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestPG_ADOPATRefusal_ARowIngestedFromADeviceIsNotARefusal forges the row the
// way a laptop can: a self-consistent ado_pat.mint.denied claim forwarded
// through the real IngestDeviceAudit. The admin read answers 204; a row the
// organisation wrote itself, of the same shape, still answers 200.
func TestPG_ADOPATRefusal_ARowIngestedFromADeviceIsNotARefusal(t *testing.T) {
	pool := throwawayPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	for _, p := range []types.Person{{Principal: "sub-forged", Email: "forged@corp.example", CreatedBy: "t"}, {Principal: "sub-genuine", Email: "genuine@corp.example", CreatedBy: "t"}} {
		if _, _, err := pg.CreatePerson(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	dev, err := pg.CreateDevice(ctx, types.Device{ID: uuid.New(), Name: "laptop"}, "wdd_"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	data := func(owner string) json.RawMessage {
		d, _ := json.Marshal(map[string]any{"refusal": reasonADOPATPolicyBlocked, "provider_row": refusalRow, "owner": owner})
		return d
	}
	forged := types.AuditEvent{ID: uuid.New(), Time: time.Now().UTC().Truncate(time.Microsecond), ActorType: types.ActorSystem,
		Actor: "wardynd", Action: adoPATAuditMintDenied, Target: uuid.NewString(), Outcome: "failure", Data: data("sub-forged")}
	if err := pool.QueryRow(ctx, `SELECT audit_row_hash($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11::jsonb)`,
		"", forged.ID, forged.Time, forged.RunID, string(forged.ActorType), forged.Actor,
		forged.Action, forged.Target, forged.Outcome, forged.SourceIP, []byte(forged.Data)).Scan(&forged.RowHash); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.IngestDeviceAudit(ctx, dev.ID, "10.9.9.9", []types.FederatedAuditEvent{{AuditEvent: forged, Seq: 1}}); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	h := newHarness(t)
	cfg := baseTestConfig(h, pg)
	cfg.OIDC = &oidc.Authenticator{}
	srv := New(cfg)
	admin := ssoSession(t, "sub-admin", "admin@corp.example", oidc.RoleAdmin)
	if w := getRefusal(t, srv, admin, refusalRow); w.Code != http.StatusNoContent || w.Body.Len() != 0 {
		t.Fatalf("a forwarded row: status %d body %q, want 204 and nothing", w.Code, w.Body.String())
	}

	own := types.AuditEvent{ID: uuid.New(), Time: time.Now().UTC().Add(-time.Hour), ActorType: types.ActorSystem,
		Actor: "wardynd", Action: adoPATAuditMintDenied, Target: uuid.NewString(), Outcome: "failure", Data: data("sub-genuine")}
	if err := store.InsertAuditEvent(ctx, pool, &own); err != nil {
		t.Fatal(err)
	}
	w := getRefusal(t, srv, admin, refusalRow)
	if w.Code != http.StatusOK || !json.Valid(w.Body.Bytes()) || !strings.Contains(w.Body.String(), "genuine@corp.example") {
		t.Fatalf("an organisation row: status %d body %q, want the genuine person", w.Code, w.Body.String())
	}
}
