-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- The image admission verdict a run's create recorded for its member image (api.stepImageAdmission,
-- types.ImageAdmissionVerdict): {"mode","verdict","reason","digest","pinned_ref","signer","identity",
-- "issuer"|"key_id","attestations","site_audit"}. A queued run's start checks the image again and rewrites it.
-- NULL when admission was off, the run named no member image, an operator started it, or the run predates
-- this column.
ALTER TABLE agent_runs ADD COLUMN IF NOT EXISTS image_admission JSONB;
