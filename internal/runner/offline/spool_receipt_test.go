// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package offline

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/runnerspool"
	"github.com/google/uuid"
)

func TestStateSpoolReceiptIsReadOnlyAcceptedPrefix(t *testing.T) {
	m, _, _, org := managerFixture(t)
	id := createLocal(t, m, org)
	rows, err := allState(t.Context(), m)
	if err != nil || len(rows) != 1 || rows[0].SpoolReceipt == nil {
		t.Fatalf("empty receipt: %+v %v", rows, err)
	}
	empty := *rows[0].SpoolReceipt
	if empty.SpoolID == uuid.Nil || empty.Seq != 0 || empty.Hash != "" || empty.ReportedDrops != 0 {
		t.Fatalf("empty identity: %+v", empty)
	}
	spool, err := runnerspool.Open(filepath.Join(m.dir, "spools", id.String()), id)
	if err != nil {
		t.Fatal(err)
	}
	defer spool.Close()
	if err := spool.Append(t.Context(), egress.DecisionLog{Request: egress.Request{RunID: id}}); err != nil {
		t.Fatal(err)
	}
	if err := spool.Refuse(t.Context()); err != nil {
		t.Fatal(err)
	}
	rows, err = allState(t.Context(), m)
	if err != nil {
		t.Fatal(err)
	}
	if *rows[0].SpoolReceipt != empty || rows[0].SpoolDepth != 1 || rows[0].SpoolDrops != 1 {
		t.Fatal("STATE acknowledged unaccepted event/drop")
	}
	if err := spool.Replay(t.Context(), func(context.Context, []runnerspool.Record) error { return nil }); err != nil {
		t.Fatal(err)
	}
	accepted, err := spool.Receipt(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	rows, err = allState(t.Context(), m)
	if err != nil || *rows[0].SpoolReceipt != accepted {
		t.Fatal("STATE receipt differs from durable accepted prefix")
	}
	if accepted.Seq != 1 || accepted.Hash == "" || accepted.ReportedDrops != 1 || accepted.SpoolID != empty.SpoolID {
		t.Fatalf("accepted receipt: %+v", accepted)
	}
}
