// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerwire

import (
	"bytes"
	"fmt"

	"github.com/google/uuid"
)

// StatePageRows bounds each checked inventory callback independently of the
// runner's lifetime run count. The encoded page also obeys MaxControlFrame.
const StatePageRows = 100

// StatePage belongs to one fresh authenticated inventory. Final permits the
// org to reconcile omissions against its original persisted candidate set.
type StatePage struct {
	InventoryID uuid.UUID  `json:"inventory_id"`
	After       uuid.UUID  `json:"after"`
	Runs        []RunState `json:"runs"`
	Final       bool       `json:"final"`
}

// StateInventory tracks order in constant memory. Its owner commits the next
// value only after durable intake accepts that page; partial pages grant no READY.
type StateInventory struct {
	ID       uuid.UUID
	Last     uuid.UUID
	Complete bool
}

// Next validates a page without advancing the receiver or inferring omissions.
func (s StateInventory) Next(page StatePage) (StateInventory, error) {
	if s.Complete || page.InventoryID == uuid.Nil || (s.ID != uuid.Nil && page.InventoryID != s.ID) || page.After != s.Last || len(page.Runs) > StatePageRows || (len(page.Runs) == 0 && !page.Final) {
		return s, fmt.Errorf("%w: invalid STATE page identity/order", ErrBadFrame)
	}
	payload, err := Marshal(page)
	if err != nil {
		return s, err
	}
	if len(payload) > MaxControlFrame {
		return s, ErrFrameTooLarge
	}
	next := s
	for _, row := range page.Runs {
		if row.RunID == uuid.Nil || bytes.Compare(row.RunID[:], next.Last[:]) <= 0 {
			return s, fmt.Errorf("%w: STATE rows are not strictly ordered", ErrBadFrame)
		}
		next.Last = row.RunID
	}
	next.ID, next.Complete = page.InventoryID, page.Final
	return next, nil
}
