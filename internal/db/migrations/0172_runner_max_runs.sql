-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- A runner's "Runs at once" (0.9): how many runs holding a sandbox placement may put on it at the same
-- time. wardyn-runnerd register sends it (about half the host's CPU cores unless told otherwise) and
-- the runner's owner may change it; the server's value is the one placement holds. 64 bounds it.
--
-- A runner registered before this migration gets 2: its host's cores were never recorded, and 2 is
-- what registration suggests for a four-core laptop. Its owner may raise it. The default is dropped
-- once the existing rows hold it, so a new registration must always state its own value.
ALTER TABLE runners ADD COLUMN IF NOT EXISTS max_runs INTEGER NOT NULL DEFAULT 2;
ALTER TABLE runners ALTER COLUMN max_runs DROP DEFAULT;
ALTER TABLE runners DROP CONSTRAINT IF EXISTS runners_max_runs_range;
ALTER TABLE runners ADD CONSTRAINT runners_max_runs_range CHECK (max_runs BETWEEN 1 AND 64);

-- machine_name is the host name the runner reports at registration and on each connect, the latest
-- kept: what the machine calls itself, beside the runner's display name. It is runner-reported, never
-- an identity, and '' until a runner that sends it reports. The server refuses one past 253 bytes or
-- with a control character before it gets here.
ALTER TABLE runners ADD COLUMN IF NOT EXISTS machine_name TEXT NOT NULL DEFAULT '';
ALTER TABLE runners DROP CONSTRAINT IF EXISTS runners_machine_name_length;
ALTER TABLE runners ADD CONSTRAINT runners_machine_name_length CHECK (octet_length(machine_name) <= 253);
