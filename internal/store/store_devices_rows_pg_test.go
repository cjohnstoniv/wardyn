// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// ingestEvents pushes rows with no hash format, the way a device that writes format 1 rows does.
func ingestEvents(ctx context.Context, st interface {
	IngestDeviceAudit(ctx context.Context, deviceID uuid.UUID, peer string, rows []types.FederatedAuditRow) (store.DeviceIngestResult, error)
}, deviceID uuid.UUID, peer string, rows []types.FederatedAuditEvent) (store.DeviceIngestResult, error) {
	out := make([]types.FederatedAuditRow, len(rows))
	for i, r := range rows {
		out[i] = types.FederatedAuditRow{FederatedAuditEvent: r}
	}
	return st.IngestDeviceAudit(ctx, deviceID, peer, out)
}
