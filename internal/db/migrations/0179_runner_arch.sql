-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- A runner's CPU architecture (0.9.1): the Docker engine's, as wardyn-runnerd reports it at registration
-- and again on each connect, the latest kept. A person choosing an image, or reading why a pull failed on
-- a runner, needs to know whether the machine is amd64 or arm64. It is runner-reported, never an
-- identity, and '' until a runner that sends it reports, or when its engine could not be asked. The
-- server stores only the two the runner images are built for.
ALTER TABLE runners ADD COLUMN IF NOT EXISTS arch TEXT NOT NULL DEFAULT '';
ALTER TABLE runners DROP CONSTRAINT IF EXISTS runners_arch_known;
ALTER TABLE runners ADD CONSTRAINT runners_arch_known CHECK (arch IN ('', 'amd64', 'arm64'));
