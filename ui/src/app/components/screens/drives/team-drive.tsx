/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The team-drive half of the drive editor (#1988), behind
// storage.user_drive.team_drives: one folder shared by the groups a drive is
// allocated to. Three decisions here are security, not styling:
//
//  1. A team drive is READ-ONLY unless the admin ticks "Members can write". The
//     one toggle sets the drive's writable bit AND every allocation's
//     members_can_write: the server mounts read-only unless both say write, so
//     a toggle that set only one would show "can write" for a mount that is not.
//  2. Ticking Team drive starts that toggle OFF, whatever the drive said before.
//  3. Allocations are groups only and sent `additional`, so giving a group a team
//     drive never removes the other drives it holds.
//
// Every product string comes from user-drives-copy.ts.
import * as React from "react";
import { drives as api, type UserDriveGrant } from "../../../lib/api/drives";
import { DRIVES, PERM } from "../../../lib/user-drives-copy";
import { Button } from "../../ui/button";
import { DirectoryCombobox } from "../../wardyn/directory-combobox";
import { Field, Switch } from "../../wardyn/form-primitives";
import { Note } from "./display";

/** The editor's Team drive switch, its one write toggle and the notes the backend earns. */
export function TeamDriveFields({
  team,
  onTeam,
  write,
  onWrite,
  backend,
  disabled,
  writeUnknown = false,
}: {
  team: boolean;
  onTeam: (on: boolean) => void;
  write: boolean;
  onWrite: (on: boolean) => void;
  backend: string;
  disabled: boolean;
  /** The grants page was cut, so the write setting of some allocations is not known here. */
  writeUnknown?: boolean;
}) {
  return (
    <div className="grid gap-4 border-t border-border pt-5 md:col-span-2">
      <Field label={DRIVES.FIELD_TEAM} hint={DRIVES.TEAM_HINT}>
        <Switch checked={team} onChange={onTeam} disabled={disabled} label={DRIVES.FIELD_TEAM} />
      </Field>
      {team && (
        <div className="grid gap-4 md:pl-6">
          <Field label={DRIVES.FIELD_TEAM_WRITE} hint={DRIVES.TEAM_WRITE_HINT}>
            <Switch checked={write} onChange={onWrite} disabled={disabled || writeUnknown} label={DRIVES.FIELD_TEAM_WRITE} />
          </Field>
          {writeUnknown && (
            <Note tone="amber">
              <span>{DRIVES.TEAM_WRITE_UNKNOWN}</span>
            </Note>
          )}
          {backend === "docker_volume" && (
            <Note tone="amber">
              <span>{DRIVES.TEAM_ONE_HOST}</span>
            </Note>
          )}
          {backend === "docker_volume" && !write && (
            <Note tone="amber">
              <span>{DRIVES.TEAM_EMPTY_READ_ONLY}</span>
            </Note>
          )}
          {backend === "k8s_pvc_static" && (
            <Note>
              <span>{DRIVES.TEAM_CLAIM_NOTE}</span>
            </Note>
          )}
        </div>
      )}
    </div>
  );
}

/** "Allocated to" for a team drive: the group chips and the picker that adds one. */
export function TeamGroups({
  groups,
  onChange,
  disabled,
  addDisabled = false,
}: {
  groups: string[];
  onChange: (next: string[]) => void;
  disabled: boolean;
  /** The grants page was cut: a group typed here may already hold an allocation the page omits, and adding it would overwrite that one. */
  addDisabled?: boolean;
}) {
  const [pick, setPick] = React.useState("");
  const add = () => {
    const name = pick.trim();
    if (name && !groups.includes(name)) onChange([...groups, name]);
    setPick("");
  };
  return (
    <div className="grid gap-2 border-t border-border pt-5 md:col-span-2">
      <span className="text-sm font-medium text-foreground">{DRIVES.COL_ALLOCATED}</span>
      {groups.length > 0 && (
        <ul className="flex flex-wrap gap-2" data-testid="drives-team-groups">
          {groups.map((g) => (
            <li key={g} className="inline-flex items-center gap-1.5 rounded-full border border-border bg-muted py-0.5 pr-1 pl-2.5 text-sm">
              <span>{g}</span>
              <button
                type="button"
                aria-label={`${PERM.REMOVE} ${g}`}
                disabled={disabled}
                onClick={() => onChange(groups.filter((x) => x !== g))}
                className="rounded-full px-1.5 text-muted-foreground hover:text-foreground disabled:cursor-not-allowed disabled:opacity-50"
              >
                ×
              </button>
            </li>
          ))}
        </ul>
      )}
      <div className="flex max-w-[520px] gap-2">
        <div className="min-w-0 flex-1">
          <DirectoryCombobox label={PERM.SUBJECT_GROUP} kind="group" value={pick} onChange={setPick} disabled={disabled || addDisabled} placeholder={DRIVES.TEAM_GROUP_PLACEHOLDER} />
        </div>
        <Button type="button" variant="outline" disabled={disabled || addDisabled || !pick.trim()} onClick={add}>
          {DRIVES.ADD_CTA}
        </Button>
      </div>
      <p className="text-meta text-muted-foreground">{DRIVES.TEAM_ALLOC_HINT}</p>
    </div>
  );
}

/**
 * Makes a saved team drive's allocations match the editor: the groups removed
 * are unallocated, the groups added are allocated, and an existing allocation
 * whose write bit differs from the toggle is re-sent with its other fields kept.
 * A null write leaves existing allocations' write bits alone; a group allocated now takes `fresh`.
 * Runs AFTER the drive is saved, so a drive that just became writable is
 * writable before any allocation says members can write.
 */
export async function syncTeamGroups(driveId: string, groups: string[], write: boolean | null, grants: UserDriveGrant[], fresh: boolean = write === true): Promise<void> {
  const mine = grants.filter((g) => g.drive_id === driveId && g.subject_type === "group");
  for (const g of mine) {
    if (!groups.includes(g.subject)) await api.deleteGrant(g.id);
  }
  for (const subject of groups) {
    const had = mine.find((g) => g.subject === subject);
    if (had && (write === null || !!had.members_can_write === write)) continue;
    await api.upsertGrant({
      subject_type: "group",
      subject,
      drive_id: driveId,
      additional: true,
      priority: had?.priority ?? 0,
      ...(had?.size_mib_override ? { size_mib_override: had.size_mib_override } : {}),
      ...(had?.writable_override != null ? { writable_override: had.writable_override } : {}),
      enabled: had?.enabled ?? true,
      members_can_write: had ? write === true : fresh,
    });
  }
}
