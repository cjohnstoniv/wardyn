// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

// FederatedRowSQL is true for an audit_events row a device forwarded and false (never NULL) for the
// organisation's own: the actor carries the device's "device:<id>/" prefix and data.device_origin
// names the same device. It lives here so readers outside internal/store (key domains) apply the same
// test as store.FederatedDeviceID.
const FederatedRowSQL = `COALESCE(starts_with(actor, 'device:' || (data->'device_origin'->>'device_id') || '/'), false)`
