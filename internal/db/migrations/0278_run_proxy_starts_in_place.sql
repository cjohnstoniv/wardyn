-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- in_place: a start a live egress edit applied by narrowing the run's running proxy, not by replacing it.
-- A failed start's end hint reads it, whichever path stops the start. Every start recorded before this
-- migration reads false, which keeps the replace path's words.
ALTER TABLE run_proxy_starts ADD COLUMN IF NOT EXISTS in_place BOOLEAN NOT NULL DEFAULT false;
