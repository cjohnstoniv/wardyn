-- Copyright 2025 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- An attach ticket carries the authority it was admitted under (#1474): when the
-- minting request was admitted, and the verified email the person's session
-- revoke may name. Redemption checks both against the revoke cutoff, so a ticket
-- minted before a revoke cannot open a session stamped after it.
--
-- Both are NULL with no default: a DEFAULT now() would give a row written by an
-- older binary an authority time of "just now", failing open. A NULL
-- authorized_at is refused at redemption. An old row lives 30 seconds, so
-- nothing of value is lost.
ALTER TABLE attach_tickets
    ADD COLUMN IF NOT EXISTS authorized_at TIMESTAMPTZ NULL,
    ADD COLUMN IF NOT EXISTS email TEXT NULL;
