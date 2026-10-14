-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- Runner limits (0.9). Every runner, self-hosted (runners) and remote-hosted (remote_runners),
-- declares a total CPU, memory and disk all its runs at once may reserve, and what one run may
-- reserve. A runner with any of them NULL has declared none and is never offered a run: there is no
-- unlimited runner. CPU is milli-CPU and at least one CPU; memory and disk are MiB, disk in whole GiB;
-- the bounds are the run request contract's; no per-run amount is past its total. A runner registered
-- before this migration declares none until its owner sets them.
ALTER TABLE runners
    ADD COLUMN IF NOT EXISTS total_cpu_millis BIGINT,
    ADD COLUMN IF NOT EXISTS total_memory_mib BIGINT,
    ADD COLUMN IF NOT EXISTS total_disk_mib   BIGINT,
    ADD COLUMN IF NOT EXISTS run_cpu_millis   BIGINT,
    ADD COLUMN IF NOT EXISTS run_memory_mib   BIGINT,
    ADD COLUMN IF NOT EXISTS run_disk_mib     BIGINT;
ALTER TABLE runners DROP CONSTRAINT IF EXISTS runners_limits_valid;
ALTER TABLE runners ADD CONSTRAINT runners_limits_valid CHECK (
    num_nulls(total_cpu_millis, total_memory_mib, total_disk_mib, run_cpu_millis, run_memory_mib, run_disk_mib) IN (0, 6)
    AND total_cpu_millis BETWEEN 1000 AND 1000000 AND run_cpu_millis BETWEEN 1000 AND total_cpu_millis
    AND total_memory_mib BETWEEN 1 AND 16777216 AND run_memory_mib BETWEEN 1 AND total_memory_mib
    AND total_disk_mib BETWEEN 1024 AND 16777216 AND run_disk_mib BETWEEN 1024 AND total_disk_mib
    AND total_disk_mib % 1024 = 0 AND run_disk_mib % 1024 = 0);

-- A remote-hosted runner also declares its Runs at once here (1 to 1024), all seven set or none. Boot
-- proposes them once from what the server measures; when it cannot measure one, none is set and an
-- administrator sets them.
ALTER TABLE remote_runners
    ADD COLUMN IF NOT EXISTS max_runs         INTEGER,
    ADD COLUMN IF NOT EXISTS total_cpu_millis BIGINT,
    ADD COLUMN IF NOT EXISTS total_memory_mib BIGINT,
    ADD COLUMN IF NOT EXISTS total_disk_mib   BIGINT,
    ADD COLUMN IF NOT EXISTS run_cpu_millis   BIGINT,
    ADD COLUMN IF NOT EXISTS run_memory_mib   BIGINT,
    ADD COLUMN IF NOT EXISTS run_disk_mib     BIGINT;
ALTER TABLE remote_runners DROP CONSTRAINT IF EXISTS remote_runners_limits_valid;
ALTER TABLE remote_runners ADD CONSTRAINT remote_runners_limits_valid CHECK (
    num_nulls(max_runs, total_cpu_millis, total_memory_mib, total_disk_mib, run_cpu_millis, run_memory_mib, run_disk_mib) IN (0, 7)
    AND max_runs BETWEEN 1 AND 1024
    AND total_cpu_millis BETWEEN 1000 AND 1000000 AND run_cpu_millis BETWEEN 1000 AND total_cpu_millis
    AND total_memory_mib BETWEEN 1 AND 16777216 AND run_memory_mib BETWEEN 1 AND total_memory_mib
    AND total_disk_mib BETWEEN 1024 AND 16777216 AND run_disk_mib BETWEEN 1024 AND total_disk_mib
    AND total_disk_mib % 1024 = 0 AND run_disk_mib % 1024 = 0);

-- What a run holds on its runner while it holds a sandbox there: the CPU and memory it was given and
-- its main drive. Written by the statement that inserts the run, kept through its promotion and a
-- revive, and summed (with the count of such runs) as the runner's In use by the one query that both
-- admission and the read API run. A run already holding a sandbox now is backfilled from the size it
-- was dispatched with (0117) and its main drive (0096), so it is counted from the first start.
ALTER TABLE agent_runs
    ADD COLUMN IF NOT EXISTS reserved_cpu_millis BIGINT NOT NULL DEFAULT 0 CHECK (reserved_cpu_millis >= 0),
    ADD COLUMN IF NOT EXISTS reserved_memory_mib BIGINT NOT NULL DEFAULT 0 CHECK (reserved_memory_mib >= 0),
    ADD COLUMN IF NOT EXISTS reserved_disk_mib   BIGINT NOT NULL DEFAULT 0 CHECK (reserved_disk_mib >= 0);
UPDATE agent_runs
   SET reserved_cpu_millis = COALESCE(agent_cpu_limit_millis, 0),
       reserved_memory_mib = COALESCE(agent_memory_limit_mib, 0),
       reserved_disk_mib   = GREATEST(disk_mib, 0)
 WHERE state IN ('PENDING', 'STARTING', 'RUNNING', 'WAITING_FOR_CONFIRMATION')
   AND reserved_cpu_millis = 0 AND reserved_memory_mib = 0 AND reserved_disk_mib = 0;
