-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- machine_kind is the kind of a run's sandbox: '' (a container, as every earlier run was) or
-- 'linux_vm', a Linux guest under QEMU/KVM on a registered runner. vm_machine is the virtual
-- hardware a VM run was started with ({machine_type, cpu_model, accelerator, qemu_version}), so a
-- later move or restore can be checked against it; NULL for a container. Both columns live on
-- agent_runs, so no new grant.
ALTER TABLE agent_runs ADD COLUMN IF NOT EXISTS machine_kind TEXT NOT NULL DEFAULT '';
ALTER TABLE agent_runs ADD COLUMN IF NOT EXISTS vm_machine JSONB;
ALTER TABLE agent_runs
    DROP CONSTRAINT IF EXISTS agent_runs_machine_kind,
    ADD CONSTRAINT agent_runs_machine_kind CHECK (machine_kind IN ('', 'linux_vm'));
