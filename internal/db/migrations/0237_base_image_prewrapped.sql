-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- Pre-wrapped base images: an image built FROM agent-base, pinned by digest, that runs as is with no image
-- builder. base_images.kind admits 'prewrapped' beside 'registry', 'custom' and 'byo'. Only a prewrapped row
-- carries a registration check, which runs on the cluster:
--   check_status   'checking' while an attempt runs, then 'ready' or 'refused'; NULL on every other kind.
--   check_attempt  the running (or last) attempt; only that attempt's verdict is written.
--   check_deadline when that attempt stops counting; a 'checking' row past it reads as interrupted.
--   check_reason   the refusal's code and what it names ('' when ready or checking).
--   accepted_arches the node architectures the check passed on (at least one when ready).
--   checked_at     when the verdict was written.
-- A row that is not 'ready' is never offered to a workspace or a run.
--
-- base_images exists already (0031), so no new grant. WIDENS one CHECK and adds columns; changes no row.
-- Re-runnable: each column and constraint is added only if absent, or dropped first.
ALTER TABLE base_images
    DROP CONSTRAINT IF EXISTS base_images_kind_check,
    ADD CONSTRAINT base_images_kind_check CHECK (kind IN ('registry', 'custom', 'byo', 'prewrapped')),
    ADD COLUMN IF NOT EXISTS check_status text,
    ADD COLUMN IF NOT EXISTS check_attempt uuid,
    ADD COLUMN IF NOT EXISTS check_deadline timestamptz,
    ADD COLUMN IF NOT EXISTS check_reason text NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS accepted_arches text[] NOT NULL DEFAULT '{}',
    ADD COLUMN IF NOT EXISTS checked_at timestamptz;
ALTER TABLE base_images
    DROP CONSTRAINT IF EXISTS base_images_check_status_check,
    ADD CONSTRAINT base_images_check_status_check CHECK (check_status IN ('checking', 'ready', 'refused')),
    DROP CONSTRAINT IF EXISTS base_images_check_state,
    ADD CONSTRAINT base_images_check_state CHECK (
        (kind = 'prewrapped') = (check_status IS NOT NULL)
        AND (check_status IS NULL OR (check_attempt IS NOT NULL AND check_deadline IS NOT NULL))
        AND (check_status IS DISTINCT FROM 'ready' OR cardinality(accepted_arches) >= 1)
    );
