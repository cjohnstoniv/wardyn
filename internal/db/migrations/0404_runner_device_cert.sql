-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- A runner's MDM device certificate chains (#2975): what the runner presented over its link, each chain's
-- device-key signature already checked. Whether a chain is trusted, current and unrevoked is decided each
-- time against the organisation's configured MDM CA and CRL, never stored.
ALTER TABLE runners
    ADD COLUMN IF NOT EXISTS device_cert jsonb,
    ADD COLUMN IF NOT EXISTS device_cert_reported_at timestamptz;
