/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// New Run's Workspace card: the workspace <Select>, the reason an ungranted
// selection costs something, the Add-workspace ghost button — and, since 0.7,
// the member's own USER DRIVE.
//
// Lifted out of new-run-screen.tsx unchanged (the Select, its placeholder and
// the ghost button are byte-for-byte what shipped) so that adding the drive
// block leaves both files well under the 1000-line file gate rather than
// pushing a 914-line screen through it.
//
// ─── WHY THE DRIVE LIVES HERE AND NOT IN THE ADD-WORKSPACE DIALOG ──────────
//
// A drive is NOT a workspace, and the card says so by being orthogonal to its
// own Select: the block renders the same whatever the Select says, "Ephemeral
// scratch — no repo" included. It is one checkbox UNDER the Select, never a
// fourth OptionCard in the dialog — a dialog entry would make it a source you
// onboard, which is exactly the conflation the copy exists to prevent.
//
// ─── FOUR STATES, AND THE ABSENT ONE MATTERS MOST ──────────────────────────
//
// The allocation (/me.user_drive) and the door
// (/me.user_drive_denied_by_profile) are two independent bits because there
// are four states and one field can only carry three:
//
//	allocated, door open      the checkbox, its hint and its mode sentence
//	allocated, paused         NR_PAUSED, where the checkbox would be
//	door shut (either way)    NR_DENIED, where the checkbox would be
//	none, door open           NOTHING — no checkbox, no line, today's card
//
// The reason lines render IN PLACE OF the checkbox: an unmountable drive is
// not a disabled checkbox with a tooltip, it is one sentence where the
// checkbox would be. The last row is the absent-row doctrine (§2.5): a member
// with no allocation and no door sees the card exactly as it was before this
// feature existed.
//
// The door is checked FIRST, and independently of the allocation, because
// "denied AND unallocated" is a real state in which the obvious advice — ask
// an admin for an allocation — is the wrong advice.
import * as React from "react";
import { Plus } from "lucide-react";
import type { MeCapabilities, Workspace } from "../../../lib/types";
import type { MeUserDrive } from "../../../lib/api/health";
import { capabilityAllowed } from "../../../lib/capabilities";
import { DENIED } from "../../../lib/permissions-copy";
import { DRIVE_MEMBER } from "../../../lib/user-drives-copy";
import { driveModeWord, driveSizeLabel } from "../../../lib/user-drives-display";
import { statusWord } from "../../../lib/workspace-status";
import { Button } from "../../ui/button";
import { Checkbox } from "../../ui/checkbox";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "../../ui/select";
import { makeMono } from "../../wardyn/code-block";
import { Chip } from "../../wardyn/primitives";
import { SectionCard } from "./new-run-primitives";
import type { WizardState } from "./wizard-types";

// Backtick-mono rendering (user-drives-prompt.md §7's header rule): the mount
// target is a literal and renders font-mono wherever it appears, while the
// frozen string carries it as PLAIN TEXT — mono is a display concern the
// consuming component applies, never baked into canon. The drive NAME is not
// here: it is a human-chosen label, and the strings already quote it.
const DRIVE_TARGET = "/home/agent/drive";

const withDriveTarget = makeMono([DRIVE_TARGET]);

// One line where the checkbox would be. Same class as the ungranted-selection
// note above it — both are "the reason your selection costs something", and
// this card already has exactly one shape for that.
function ReasonLine({ children }: { children: React.ReactNode }) {
  return (
    <p className="mt-3 text-xs text-muted-foreground" data-testid="nr-drive-reason">
      {children}
    </p>
  );
}

function DriveBlock({
  drive,
  deniedBy,
  enabled,
  readOnly,
  patch,
}: {
  drive: MeUserDrive | null;
  deniedBy: string;
  enabled: boolean;
  readOnly: boolean;
  patch: (p: Partial<WizardState>) => void;
}) {
  // The door outranks everything, allocation or not — see the header note.
  if (deniedBy) return <ReasonLine>{DRIVE_MEMBER.NR_DENIED(deniedBy)}</ReasonLine>;
  // No allocation and no door: no checkbox, no line, no placeholder.
  if (!drive) return null;
  if (drive.paused) return <ReasonLine>{DRIVE_MEMBER.NR_PAUSED}</ReasonLine>;

  // The mode of the MOUNT THIS RUN GETS, not of the allocation: a writable
  // allocation narrowed for this run is read-only here, so the hint's {mode}
  // and the sentence after it flip together and neither promises persistence a
  // read-only mount cannot give.
  //
  // A NARROWING NEEDS A MOUNT TO NARROW. With the checkbox off this run mounts
  // nothing — wizard-spec.ts emits `drive` only for driveEnabled, so
  // `read_only` would ride nothing — and the sentence above is an OFFER, not a
  // description of this run. So `driveReadOnly` is read only while the mount is
  // on: the toggle is absent below rather than live over a mount that is not
  // happening, and the offer describes the allocation as the admin granted it.
  // The mock draws both toggle states with the checkbox ON
  // (docs/design/user-drives-mock/index.html 7a/7b) and none with it off (7c).
  const narrowed = enabled && readOnly;
  const writable = drive.writable && !narrowed;
  const mode = driveModeWord(writable);
  const size = driveSizeLabel(drive.size_mib);
  return (
    <div className="mt-3.5" data-testid="nr-drive">
      <div className="flex items-start gap-2">
        <Checkbox
          id="nr-drive-mount"
          className="mt-0.5"
          checked={enabled}
          onCheckedChange={(v) => patch({ driveEnabled: v === true })}
        />
        <div className="min-w-0">
          <label htmlFor="nr-drive-mount" className="text-xs font-medium text-foreground">
            {DRIVE_MEMBER.NR_CHECKBOX}
          </label>
          <p className="mt-0.5 text-xs leading-snug text-muted-foreground">
            {withDriveTarget(
              size
                ? DRIVE_MEMBER.NR_HINT(drive.name, size, mode)
                : DRIVE_MEMBER.NR_HINT_NOSIZE(drive.name, mode),
            )}{" "}
            {writable ? DRIVE_MEMBER.NR_RW_NOTE : DRIVE_MEMBER.NR_RO_NOTE}
          </p>
        </div>
      </div>
      {/* Only a WRITABLE allocation being MOUNTED can be narrowed, and the
          narrowing defaults OFF (Q5): a run may narrow what an admin granted,
          never widen it, so there is no matching control on a read-only
          allocation to disable — and none on an unmounted one, which has no
          mount to make read-only. Absent, never disabled: the same shape the
          reason lines above take. */}
      {enabled && drive.writable && (
        <label
          htmlFor="nr-drive-readonly"
          className="mt-2.5 ml-6 flex items-center gap-2 text-xs text-foreground"
        >
          <Checkbox
            id="nr-drive-readonly"
            checked={readOnly}
            onCheckedChange={(v) => patch({ driveReadOnly: v === true })}
          />
          {DRIVE_MEMBER.NR_READONLY_TOGGLE}
        </label>
      )}
    </div>
  );
}

export function WorkspaceCard({
  state,
  patch,
  workspaces,
  caps,
  onAddWorkspace,
  drive,
  driveDeniedBy,
}: {
  state: WizardState;
  patch: (p: Partial<WizardState>) => void;
  workspaces: Workspace[];
  caps: MeCapabilities | null;
  onAddWorkspace: () => void;
  // /me's two drive bits. Both default to "nothing to show" for an unresolved
  // or failed read, which renders as today's card — the honest answer, since
  // the server resolver itself answers every failure with nil.
  drive: MeUserDrive | null;
  driveDeniedBy: string;
}) {
  // Whether the workspace this run is aimed at is one the caller may launch
  // against. Advisory — denyMemberRequest is the real gate.
  const pickedWorkspaceId = state.workspaces[0]?.workspaceId;
  const selectedWorkspaceUngranted =
    !!pickedWorkspaceId && !capabilityAllowed(caps, "workspace", pickedWorkspaceId);

  return (
    <SectionCard title="Workspace">
      <Select
        value={state.workspaces[0]?.workspaceId ?? "__none__"}
        onValueChange={(v) =>
          patch({ workspaces: v === "__none__" ? [] : [{ workspaceId: v, enabledOptional: [] }] })
        }
      >
        <SelectTrigger>
          <SelectValue placeholder="Ephemeral scratch — no repo" />
        </SelectTrigger>
        <SelectContent>
          <SelectItem value="__none__">Ephemeral scratch — no repo</SelectItem>
          {workspaces.map((w: Workspace) => (
            <SelectItem key={w.id} value={w.id}>
              {w.name}
              {statusWord(w.status) === "Import failed" && (
                <span className="text-danger"> — import failed</span>
              )}
              {!capabilityAllowed(caps, "workspace", w.id) && (
                <Chip tone="neutral">{DENIED.WORKSPACE_CHIP}</Chip>
              )}
            </SelectItem>
          ))}
        </SelectContent>
      </Select>
      {/* The reason rides the SELECTION, not each row: a Radix item's
          content is what the closed trigger renders, so a per-row
          paragraph would end up inside the trigger. The chip above
          annotates every ungranted row; this says what it costs. */}
      {selectedWorkspaceUngranted && (
        <p className="mt-2 text-xs text-muted-foreground">{DENIED.WORKSPACE_BODY}</p>
      )}
      <DriveBlock
        drive={drive}
        deniedBy={driveDeniedBy}
        enabled={state.driveEnabled}
        readOnly={state.driveReadOnly}
        patch={patch}
      />
      <Button
        type="button"
        variant="ghost"
        size="sm"
        className="mt-2 px-1"
        onClick={onAddWorkspace}
      >
        <Plus className="size-4" /> Add workspace
      </Button>
    </SectionCard>
  );
}
