-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- When a portal's revoke also cut the gateway access of the runs it launched
-- (DELETE /api/v1/admin/delegates/{id}?cut_in_flight=true). The gateway mint reads it for every run
-- whose created_via names the portal. NULL, the default, leaves those runs their access.
ALTER TABLE delegates ADD COLUMN IF NOT EXISTS cut_in_flight_at TIMESTAMPTZ;
