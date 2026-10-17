// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package offline

import (
	"context"
	"errors"

	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/google/uuid"
)

// StatePages snapshots identities, then queries and emits bounded ordered pages.
// The fresh-session caller must keep execution admission closed until Final.
func (m *Manager) StatePages(ctx context.Context, send func(runnerwire.StatePage) error) error {
	if send == nil {
		return errors.New("STATE inventory requires a checked page consumer")
	}
	m.mu.Lock()
	if m.online || m.state.Revoked {
		m.mu.Unlock()
		return errors.New("STATE inventory requires closed execution admission")
	}
	if err := m.ensureDurable(); err != nil {
		m.mu.Unlock()
		return err
	}
	ids := m.inventoryIDs()
	m.mu.Unlock()
	inventory := uuid.New()
	cursor := runnerwire.StateInventory{}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n := min(len(ids), runnerwire.StatePageRows)
		rows, err := m.stateRows(ctx, ids[:n])
		if err != nil {
			return err
		}
		page := runnerwire.StatePage{InventoryID: inventory, After: cursor.Last, Runs: rows, Final: n == len(ids)}
		next, err := cursor.Next(page)
		if err != nil {
			return err
		}
		if err := send(page); err != nil {
			return err
		}
		if next.Complete {
			return nil
		}
		cursor, ids = next, ids[n:]
	}
}

func (m *Manager) inventoryIDs() []uuid.UUID {
	ids := m.runIDs()
	visible := ids[:0]
	for _, id := range ids {
		if m.state.Runs[id].Quarantine == nil && !m.state.Runs[id].Retired {
			visible = append(visible, id)
		}
	}
	return visible
}
