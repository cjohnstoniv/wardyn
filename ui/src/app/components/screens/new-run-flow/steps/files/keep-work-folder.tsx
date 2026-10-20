/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// "Reuse it across runs" (#1991): keeps the run's work folder, and only that, as the person's own drive.
// The catalogue's create door says whether the organisation offers it. With the switch off the row is
// today's: a disabled tick and Coming soon.

import * as React from "react";
import { Lock, TriangleAlert } from "lucide-react";
import type { MemberDriveCreate } from "../../../../../lib/api/me-drives";
import { InfoTip } from "../../../../wardyn/info-tip";
import { Chip } from "../../../../wardyn/primitives";
import { NR_FILES } from "../../../../wardyn/copy/new-run/files";
import { NR_SHARED } from "../../../../wardyn/copy/new-run/shared";
import { setKeepWorkFolder } from "../../lenses/files-lens";
import { useNewRunDispatch, useNewRunDraft } from "../../new-run-route";
import { createReasonText } from "./file-sources";

export type KeepDoor =
  | { kind: "today" }
  | { kind: "open"; count: number; limit: number; unenforced: boolean }
  | { kind: "closed"; reason?: string };

/**
 * What the row offers. `asked`: the draft already keeps the folder (a template or a rerun can carry it), so the
 * tick stays live to be taken back, whatever the door says now.
 */
export function keepDoor(create: MemberDriveCreate | undefined, asked: boolean): KeepDoor {
  if (create?.allowed && create.keep_work_folder) {
    return { kind: "open", count: create.count, limit: create.limit, unenforced: create.enforcement === "none" };
  }
  if (!create?.keep_work_folder && !asked) return { kind: "today" };
  const reason = create?.reason === "limit_reached" ? NR_FILES.KEEP_LIMIT(create.count, create.limit)
    : create?.reason === "disabled" ? NR_FILES.CREATE_DISABLED
      // The switch is off but the draft asks (a rerun carries it): say why the tick is locked.
      : create && (createReasonText(create) ?? (create.keep_work_folder ? undefined : NR_FILES.CREATE_DISABLED));
  return { kind: "closed", reason: reason || undefined };
}

/** Wraps the paths of a sentence in code, so they read as paths. */
function withPaths(text: string): React.ReactNode[] {
  return text.split(/(\/home\/agent\/work|\/tmp)/).map((part, i) => (i % 2 ? <code key={i} className="font-mono">{part}</code> : part));
}

export function KeepWorkFolder({ door }: { door: KeepDoor }) {
  const draft = useNewRunDraft();
  const dispatch = useNewRunDispatch();
  const reasonId = React.useId();
  const asked = draft.intent.keep_work_folder === true;
  const tip = <InfoTip label={NR_FILES.REUSE_ARIA}>{NR_FILES.REUSE_TIP}</InfoTip>;
  // The rule row: checkbox, label and Coming soon in one wrapper, the tooltip beside it, on one line that wraps whole (#2860).
  if (door.kind === "today") {
    return (
      <div className="flex flex-wrap items-center gap-2 border-t border-border pt-[11px] text-sm">
        <span className="flex flex-wrap items-center gap-2 opacity-60">
          <input id="nr-reuse-drive" type="checkbox" disabled className="size-4" />
          <label htmlFor="nr-reuse-drive" className="font-semibold">{NR_FILES.REUSE_LABEL}</label>
          <Chip>{NR_SHARED.COMING_SOON}</Chip>
        </span>
        {tip}
      </div>
    );
  }
  const reason = door.kind === "closed" ? door.reason : undefined;
  return (
    <div className="flex flex-col gap-2.5 border-t border-border pt-[11px]">
      <div className="flex flex-wrap items-center gap-2 text-sm">
        <span className={asked || door.kind === "open" ? "flex flex-wrap items-center gap-2" : "flex flex-wrap items-center gap-2 opacity-60"}>
          <input
            id="nr-reuse-drive"
            type="checkbox"
            className="size-4 accent-primary"
            checked={asked}
            disabled={!asked && door.kind === "closed"}
            aria-describedby={reason ? reasonId : undefined}
            onChange={(e) => dispatch({ type: "edit", apply: (i) => setKeepWorkFolder(i, e.target.checked) })}
          />
          <label htmlFor="nr-reuse-drive" className="font-semibold">{NR_FILES.REUSE_LABEL}</label>
        </span>
        {tip}
      </div>
      {reason && (
        <p id={reasonId} className="flex items-start gap-1.5 pl-6 text-xs text-muted-foreground">
          <Lock aria-hidden="true" className="mt-0.5 size-3 flex-none" />
          <span>{reason}</span>
        </p>
      )}
      {asked && door.kind === "open" && (
        <div className="flex flex-col gap-2.5 pl-6" data-testid="nr-keep-work-folder">
          <p className="text-sm">{withPaths(NR_FILES.KEEP_WHAT)}</p>
          <div role="note" className="flex items-start gap-2 rounded-lg border border-[color-mix(in_srgb,var(--warning)_35%,var(--border))] bg-warning-subtle px-3 py-2.5 text-sm">
            <TriangleAlert aria-hidden="true" className="mt-0.5 size-4 flex-none text-warning" />
            <span>{NR_FILES.KEEP_WARNING}</span>
          </div>
          <p className="text-xs text-muted-foreground">
            {NR_FILES.KEEP_COUNTS(door.count, door.limit)}
            {door.unenforced && ` ${NR_FILES.KEEP_UNENFORCED}`}
          </p>
        </div>
      )}
    </div>
  );
}
