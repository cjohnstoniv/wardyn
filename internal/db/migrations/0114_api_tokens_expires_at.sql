-- Copyright 2025 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- A per-user API token may carry an expiry. NULL means no expiry, which is what
-- every row minted before this column has; the auth-time lookup treats an
-- expired row exactly as it treats a revoked one.
ALTER TABLE api_tokens ADD COLUMN IF NOT EXISTS expires_at TIMESTAMPTZ NULL;
