// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The chained partition_dropped event carries the caller's actor type: a human
// is human, the shared admin token (not a person) is system, and the sweeper's
// name is system whatever type is passed. Attestation follows the name.
func TestPG_RetentionDrop_RecordsTheCallersActorType(t *testing.T) {
	for _, tc := range []struct {
		name         string
		actor        string
		given        types.ActorType
		wantType     string
		wantAttested bool
	}{
		{"a person", "ops@example.com", types.ActorHuman, "human", true},
		{"the admin token", "admin-token", types.ActorSystem, "system", true},
		{"the sweeper's name with a human type", store.SystemAuditActor, types.ActorHuman, "system", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newRetChain(t)
			c.window(t, 30)
			d, err := store.NewPG(c.pool).DropAuditPartition(context.Background(), "audit_events_legacy", c.digest(t, "audit_events_legacy"), tc.actor, tc.given)
			if err != nil {
				t.Fatalf("drop: %v", err)
			}
			var atype, who, data string
			if err := c.pool.QueryRow(context.Background(), `SELECT actor_type, actor, data::text FROM audit_events WHERE seq = $1`, d.EventSeq).Scan(&atype, &who, &data); err != nil {
				t.Fatalf("read the drop event: %v", err)
			}
			attested := strings.Contains(data, `"attested": true`)
			if atype != tc.wantType || who != tc.actor || attested != tc.wantAttested {
				t.Errorf("event = %s %s attested=%v, want %s %s attested=%v", atype, who, attested, tc.wantType, tc.actor, tc.wantAttested)
			}
			var anchorActor string
			if err := c.pool.QueryRow(context.Background(), `SELECT actor FROM audit_chain_anchors WHERE event_seq = $1`, d.EventSeq).Scan(&anchorActor); err != nil || anchorActor != tc.actor {
				t.Errorf("anchor actor = %q (%v), want %q", anchorActor, err, tc.actor)
			}
		})
	}
}

// A type the database does not know is refused, and the legacy three-argument call still works.
func TestPG_RetentionDrop_ActorTypeIsValidatedAndOptional(t *testing.T) {
	c := newRetChain(t)
	c.window(t, 30)
	ctx := context.Background()
	digest := c.digest(t, "audit_events_legacy")
	pg := store.NewPG(c.pool)
	if _, err := pg.DropAuditPartition(ctx, "audit_events_legacy", digest, "ops@example.com", types.ActorType("agent")); err == nil || !strings.Contains(err.Error(), "p_actor_type") {
		t.Fatalf("an agent actor type: %v, want a refusal naming p_actor_type", err)
	}
	c.nothingDropped(t)
	var seq int64
	if err := c.pool.QueryRow(ctx, `SELECT dropped_event_seq FROM audit_retention_drop('audit_events_legacy', $1, 'ops@example.com')`, digest).Scan(&seq); err != nil {
		t.Fatalf("three-argument drop: %v", err)
	}
	if got := scalar[string](t, c.pool, `SELECT actor_type FROM audit_events WHERE seq = $1`, seq); got != "human" {
		t.Errorf("a drop with no type recorded %q, want human (the 0123 rule)", got)
	}
}
