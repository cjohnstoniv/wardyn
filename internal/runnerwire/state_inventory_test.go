// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerwire

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestStateInventoryStrictPagesAndFinalBarrier(t *testing.T) {
	id := uuid.New()
	first := uuid.MustParse("00000000-0000-0000-0000-000000000001")
	last := uuid.MustParse("00000000-0000-0000-0000-000000000002")
	initial := StateInventory{}
	page := StatePage{InventoryID: id, Runs: []RunState{{RunID: first}}}
	next, err := initial.Next(page)
	if err != nil || initial.ID != uuid.Nil || next.Complete {
		t.Fatalf("partial inventory advanced original or completed: %+v %v", next, err)
	}
	for _, bad := range []StatePage{
		page,
		{InventoryID: uuid.New(), After: first, Runs: []RunState{{RunID: last}}, Final: true},
		{InventoryID: id, After: first},
		{InventoryID: id, After: first, Runs: []RunState{{RunID: first}}, Final: true},
		{InventoryID: id, After: first, Runs: []RunState{{RunID: last}, {RunID: first}}, Final: true},
		{InventoryID: id, After: first, Runs: make([]RunState, StatePageRows+1), Final: true},
		{InventoryID: id, After: first, Runs: []RunState{{RunID: last, AgentState: strings.Repeat("x", MaxControlFrame)}}, Final: true},
	} {
		if unchanged, err := next.Next(bad); err == nil || unchanged != next {
			t.Fatalf("bad page advanced inventory: %+v %v", unchanged, err)
		}
	}
	final := StatePage{InventoryID: id, After: first, Runs: []RunState{{RunID: last}}, Final: true}
	complete, err := next.Next(final)
	if err != nil || !complete.Complete || complete.Last != last {
		t.Fatalf("final: %+v %v", complete, err)
	}
	if _, err := complete.Next(final); err == nil {
		t.Fatal("accepted page after Final")
	}
	if _, err := complete.Next(StatePage{InventoryID: id, After: last, Final: true}); err == nil {
		t.Fatal("accepted page after Final")
	}
	if empty, err := initial.Next(StatePage{InventoryID: uuid.New(), Final: true}); err != nil || !empty.Complete {
		t.Fatalf("empty inventory: %+v %v", empty, err)
	}
}
