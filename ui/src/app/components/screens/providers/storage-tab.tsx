/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Storage tab of /providers — ephemeral scratch (default/max, §6.1-§6.3)
// and the drive ceiling (§6.4), then the EXISTING UserDrivesCard as the link
// out. No drives table is embedded here (Q7 — the move steps.ts forbids).
import { nonNegativeInt } from "../../../lib/format";
import type { StorageProviders } from "../../../lib/api/providers";
import type { StorageEnforcement } from "../../../lib/api/drives";
import { PROVIDERS } from "../../../lib/workspace-providers-copy";
import { Field, Switch } from "../../wardyn/form-primitives";
import { Input } from "../../ui/input";
import { UserDrivesCard } from "../setup/user-drives-card";
import { enforcementGloss, isUncappedEnforcement } from "../drives/display";

function numberField(v: number | undefined): number | "" {
  return v ? v : "";
}

export function StorageTab({
  storage,
  onChange,
  enforcement,
  operator,
}: {
  storage: StorageProviders;
  onChange: (next: StorageProviders) => void;
  /** From /setup/status's runner block — the AUTHORING daemon's driver,
   *  operators only (redactSetupStatusForMember strips it for a member). */
  enforcement: StorageEnforcement | undefined;
  operator: boolean;
}) {
  const ephemeral = storage.ephemeral ?? {};
  const userDrive = storage.user_drive ?? {};
  const gloss = enforcementGloss(enforcement);
  // Any of the three disk fields (the two here + the profile editor's
  // MaxEphemeralDiskMiB row) can leave a run with a non-zero disk_mib on a
  // host whose driver can't enforce it (§6.2) — read from the AUTHORING
  // daemon's driver, never a laptop's. Only `none` is that host: `eviction`
  // (k8s) binds the size on the pod, so it gets the gloss and no warning.
  const dockerUncapped = isUncappedEnforcement(enforcement);

  const setEphemeral = (patch: Partial<typeof ephemeral>) => onChange({ ...storage, ephemeral: { ...ephemeral, ...patch } });
  const setUserDrive = (patch: Partial<typeof userDrive>) => onChange({ ...storage, user_drive: { ...userDrive, ...patch } });

  return (
    <div className="space-y-6">
      <section>
        <h3 className="text-sm font-medium text-foreground">{PROVIDERS.EPHEMERAL_TITLE}</h3>
        <p className="mt-1 text-body text-muted-foreground">{PROVIDERS.EPHEMERAL_LEAD}</p>
        <div className="mt-3 grid gap-4 sm:grid-cols-2">
          <Field label={PROVIDERS.FIELD_DEFAULT_DISK} htmlFor="provider-default-disk" hint={PROVIDERS.DEFAULT_DISK_HINT}>
            <Input
              id="provider-default-disk"
              type="number"
              min={0}
              className="font-mono"
              disabled={!operator}
              value={numberField(ephemeral.default_disk_mib)}
              onChange={(e) => setEphemeral({ default_disk_mib: nonNegativeInt(e.target.value) })}
            />
            <span className="mt-1 block text-meta text-muted-foreground">{gloss}</span>
          </Field>
          <Field label={PROVIDERS.FIELD_MAX_DISK} htmlFor="provider-max-disk" hint={PROVIDERS.MAX_DISK_HINT}>
            <Input
              id="provider-max-disk"
              type="number"
              min={0}
              className="font-mono"
              disabled={!operator}
              value={numberField(ephemeral.max_disk_mib)}
              onChange={(e) => setEphemeral({ max_disk_mib: nonNegativeInt(e.target.value) })}
            />
            <span className="mt-1 block text-meta text-muted-foreground">{gloss}</span>
          </Field>
        </div>
        {dockerUncapped && <p className="mt-3 text-meta leading-snug text-warning">{PROVIDERS.DOCKER_UNCAPPED_WARN}</p>}
      </section>

      <hr className="border-border" />

      <section>
        <h3 className="text-sm font-medium text-foreground">{PROVIDERS.DRIVE_CEILING_TITLE}</h3>
        <div className="mt-3 grid gap-4 sm:grid-cols-2">
          <Field label={PROVIDERS.FIELD_DRIVES_ENABLED} hint={PROVIDERS.DRIVES_ENABLED_HINT}>
            <Switch
              checked={!userDrive.disabled}
              disabled={!operator}
              label={PROVIDERS.FIELD_DRIVES_ENABLED}
              onChange={(checked) => setUserDrive({ disabled: !checked })}
            />
          </Field>
          <Field label={PROVIDERS.FIELD_MAX_DRIVE} htmlFor="provider-max-drive" hint={PROVIDERS.MAX_DRIVE_HINT}>
            <Input
              id="provider-max-drive"
              type="number"
              min={0}
              className="font-mono"
              disabled={!operator}
              value={numberField(userDrive.max_size_mib)}
              onChange={(e) => setUserDrive({ max_size_mib: nonNegativeInt(e.target.value) })}
            />
          </Field>
        </div>
        <p className="mt-3 text-meta leading-snug text-muted-foreground">{PROVIDERS.CEILING}</p>
        <div className="mt-4">
          <UserDrivesCard />
        </div>
      </section>
    </div>
  );
}
