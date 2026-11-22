// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// An audit event is stored only when both its event time and its id match: the same id at another time is not
// present, nor is the same time under another id, and the exact pair is, whatever zone or sub-microsecond
// part the caller's time carries.
func TestAuditEventsStored_MatchesTimeAndIDTogether_PG(t *testing.T) {
	st, pool := isolatedStore(t)
	ctx := context.Background()
	at := time.Now().UTC().Truncate(time.Second).Add(123456 * time.Microsecond)
	ev := types.AuditEvent{ID: uuid.New(), Time: at, ActorType: types.ActorSystem, Actor: "test", Action: "test.present", Target: "t", Outcome: "success"}
	if err := store.InsertAuditEvent(ctx, pool, &ev); err != nil {
		t.Fatal(err)
	}
	other := uuid.New()
	for _, c := range []struct {
		name string
		key  store.AuditEventKey
		want bool
	}{
		{"the stored pair", store.AuditEventKey{ID: ev.ID, At: at}, true},
		{"the stored pair in another zone, with a sub-microsecond part", store.AuditEventKey{ID: ev.ID, At: at.In(time.FixedZone("x", 3600)).Add(400 * time.Nanosecond)}, true},
		{"the same id at another time", store.AuditEventKey{ID: ev.ID, At: at.Add(time.Second)}, false},
		{"the same time under another id", store.AuditEventKey{ID: other, At: at}, false},
	} {
		got, err := st.AuditEventsStored(ctx, []store.AuditEventKey{c.key})
		if err != nil {
			t.Fatal(err)
		}
		norm := store.AuditEventKey{ID: c.key.ID, At: c.key.At.UTC().Truncate(time.Microsecond)}
		if got[norm] != c.want {
			t.Errorf("%s: present = %v, want %v", c.name, got[norm], c.want)
		}
	}
	both, err := st.AuditEventsStored(ctx, []store.AuditEventKey{{ID: ev.ID, At: at}, {ID: other, At: at}, {ID: ev.ID, At: at.Add(time.Second)}})
	if err != nil || len(both) != 1 || !both[store.AuditEventKey{ID: ev.ID, At: at}] {
		t.Errorf("a batch of one stored pair and two near misses = %v, %v; want only the stored pair", both, err)
	}
	if none, err := st.AuditEventsStored(ctx, nil); err != nil || len(none) != 0 {
		t.Errorf("no keys = %v, %v; want empty", none, err)
	}
}
