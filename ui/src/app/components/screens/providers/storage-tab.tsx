/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Storage tab of /providers — ephemeral scratch (default/max, §6.1-§6.3)
// and the drive ceiling (§6.4), then the EXISTING UserDrivesCard as the link
// out. No drives table is embedded here (Q7 — the move steps.ts forbids).
import * as React from "react";
import { nonNegativeInt } from "../../../lib/format";
import type { StorageProviders } from "../../../lib/api/providers";
import type { StorageEnforcement } from "../../../lib/api/drives";
import { MEMBER_DRIVES, PROVIDERS } from "../../../lib/workspace-providers-copy";
import { Field, fieldHintId, Switch } from "../../wardyn/form-primitives";
import { Input } from "../../ui/input";
import { UserDrivesCard } from "../setup/user-drives-card";
import { enforcementGloss, isUncappedEnforcement } from "../drives/display";
import { parseSize, sizeField, type SizeUnit } from "../new-run-flow/lenses/files-lens";
import { NR_FILES } from "../../wardyn/copy/new-run/files";
import { NR_SHARED } from "../../wardyn/copy/new-run/shared";

function numberField(v: number | undefined): number | "" {
  return v ? v : "";
}

const UNIT_WORD: Record<SizeUnit, string> = { GiB: NR_SHARED.GIB("").trim(), MiB: NR_SHARED.MIB("").trim() };

// The member-drive size: GiB with one decimal from 0.1 GiB up, whole MiB under
// it (the New Run main drive's rule), and the exact MiB whenever the text does
// not say the stored size.
function DriveSizeField({ mib, onChange, disabled }: { mib: number | undefined; onChange: (mib: number | undefined) => void; disabled: boolean }) {
  const [typed, setTyped] = React.useState<{ text: string; unit: SizeUnit } | null>(null);
  const stored = mib && mib > 0 ? mib : undefined;
  const shown = typed ?? (stored !== undefined ? sizeField(stored) : { text: "", unit: "GiB" as SizeUnit });
  const exact = stored !== undefined && parseSize(shown.text, shown.unit) !== stored ? stored : undefined;
  return (
    <Field label={MEMBER_DRIVES.FIELD_SIZE} htmlFor="provider-member-drives-size" hint={MEMBER_DRIVES.SIZE_HINT}>
      <span className="relative flex items-center">
        <Input
          id="provider-member-drives-size"
          aria-describedby={fieldHintId("provider-member-drives-size")}
          type="number"
          inputMode="decimal"
          min={0}
          step={shown.unit === "GiB" ? 0.1 : 1}
          className="pr-12 font-mono"
          disabled={disabled}
          value={shown.text}
          onChange={(e) => {
            setTyped({ text: e.target.value, unit: shown.unit });
            onChange(parseSize(e.target.value, shown.unit));
          }}
          onBlur={() => setTyped(null)}
        />
        <span aria-hidden="true" className="pointer-events-none absolute right-3 text-sm text-muted-foreground">{UNIT_WORD[shown.unit]}</span>
      </span>
      {exact !== undefined && <span className="mt-1 block text-meta text-muted-foreground">{NR_FILES.SIZE_EXACT(exact)}</span>}
    </Field>
  );
}

export function StorageTab({
  storage,
  onChange,
  enforcement,
  kubernetes,
  operator,
}: {
  storage: StorageProviders;
  onChange: (next: StorageProviders) => void;
  /** From /setup/status's runner block — the AUTHORING daemon's driver,
   *  operators only (redactSetupStatusForUser strips it for a member). */
  enforcement: StorageEnforcement | undefined;
  /** The runner is the Kubernetes driver: the only substrate where a member drive takes a size
   *  (the server refuses one on Docker). From /setup/status's runner block. */
  kubernetes?: boolean;
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
  const memberCreate = userDrive.member_create ?? {};
  // Keeps the policy's other fields (size_mib, storage_class) as the API set them.
  // Switching on seeds the smallest valid count, since the server refuses an
  // enabled switch with none.
  const setMemberCreate = (patch: Partial<typeof memberCreate>) => setUserDrive({ member_create: { ...memberCreate, ...patch } });

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
        <div className="mt-4 grid gap-4 sm:grid-cols-2">
          <Field label={MEMBER_DRIVES.FIELD_ENABLED}>
            <Switch
              checked={!!memberCreate.enabled}
              disabled={!operator}
              label={MEMBER_DRIVES.FIELD_ENABLED}
              onChange={(checked) => setMemberCreate({ enabled: checked, ...(checked && !memberCreate.max_per_person ? { max_per_person: 1 } : {}) })}
            />
          </Field>
          <Field label={MEMBER_DRIVES.FIELD_MAX} htmlFor="provider-member-drives-max">
            <Input
              id="provider-member-drives-max"
              type="number"
              min={1}
              max={20}
              className="font-mono"
              disabled={!operator || !memberCreate.enabled}
              value={numberField(memberCreate.max_per_person)}
              onChange={(e) => setMemberCreate({ max_per_person: nonNegativeInt(e.target.value) })}
            />
          </Field>
          {kubernetes && (
            <DriveSizeField
              mib={memberCreate.size_mib}
              disabled={!operator || !memberCreate.enabled}
              onChange={(size_mib) => setMemberCreate({ size_mib })}
            />
          )}
        </div>
        <div className="mt-4">
          <UserDrivesCard />
        </div>
      </section>
    </div>
  );
}
