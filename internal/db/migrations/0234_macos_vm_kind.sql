-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- macOS virtual machines (machine_kind macos_vm), off unless the organisation turns them on
-- (WARDYN_MACOS_VMS). agent_runs.machine_kind admits the new kind beside '' and 'linux_vm'; a macOS
-- guest keeps its sign-ins on the run's own disk, which desktop_vm_sign_ins.store records as
-- 'run_disk'. Both tables exist already, so no new grant.
--
-- WIDENS two CHECKs and changes no row. Re-runnable: each constraint is dropped if present first.
ALTER TABLE agent_runs
    DROP CONSTRAINT IF EXISTS agent_runs_machine_kind,
    ADD CONSTRAINT agent_runs_machine_kind CHECK (machine_kind IN ('', 'linux_vm', 'macos_vm'));
ALTER TABLE desktop_vm_sign_ins
    DROP CONSTRAINT IF EXISTS desktop_vm_sign_ins_store_check,
    ADD CONSTRAINT desktop_vm_sign_ins_store_check CHECK (store IN ('memory', 'none', 'run_disk'));
