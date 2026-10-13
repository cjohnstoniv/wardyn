/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// One card of "Repositories, drives and folders": what it is, where it
// appears, its branch or access, Edit and Remove; open, what happens to it and
// what it is stored on.

import { ChevronRight } from "lucide-react";
import { Button } from "../../../../ui/button";
import { cn } from "../../../../ui/utils";
import { InfoTip } from "../../../../wardyn/info-tip";
import { Chip } from "../../../../wardyn/primitives";
import { NR_FILES } from "../../../../wardyn/copy/new-run/files";
import { NR_SHARED } from "../../../../wardyn/copy/new-run/shared";
import type { FileItem } from "./file-items";
import { KIND_INFO, kindStyle, KindBadge, KindTile, PathText } from "./kind-visuals";
import { LockedAccess } from "./locked-access";

const LABEL = "text-sm font-medium text-muted-foreground";

export function AttachCard({ item, open, onToggle, onEdit, onRemove }: {
  item: FileItem;
  open: boolean;
  onToggle: () => void;
  onEdit: () => void;
  onRemove: () => void;
}) {
  const kind = item.kind === "computer" ? "folder" : item.kind;
  const bodyId = `nr-attach-body-${item.key.replace(/[^a-zA-Z0-9_-]/g, "-")}`;
  const access = item.kind === "repo"
    ? <span className="font-mono text-[13px]">{item.branch ?? NR_FILES.BRANCH_PLACEHOLDER}</span>
    : item.locked ? <LockedAccess id={bodyId} />
      : <span>{item.readOnly ? NR_FILES.ACCESS_READ : NR_FILES.ACCESS_EDIT}</span>;
  return (
    <div
      data-testid="nr-attach-card"
      style={kindStyle(kind)}
      className="border-b border-border shadow-[inset_3px_0_0_var(--kind)] last:rounded-b-[10px] last:border-b-0"
    >
      <div
        className={cn(
          "grid min-h-[60px] grid-cols-[minmax(0,1.25fr)_minmax(0,1fr)_minmax(118px,auto)_auto] items-center gap-2.5 py-[9px] pr-2.5 pl-3 text-sm",
          "max-[760px]:grid-cols-[minmax(0,1fr)_auto] max-[760px]:gap-y-1 max-[760px]:[grid-template-areas:'toggle_actions'_'path_access']",
          "max-[520px]:gap-x-1.5 max-[520px]:pt-2 max-[520px]:pr-2 max-[520px]:pb-2.5 max-[520px]:pl-2.5",
        )}
      >
        <button
          type="button"
          aria-expanded={open}
          aria-controls={bodyId}
          onClick={onToggle}
          className="flex min-w-0 items-center gap-[9px] rounded-[7px] p-1 text-left hover:bg-muted max-[760px]:[grid-area:toggle]"
        >
          <ChevronRight aria-hidden="true" className={cn("size-3.5 flex-none text-muted-foreground transition-transform", open && "rotate-90")} />
          <KindTile kind={kind} />
          <span className="flex min-w-0 flex-wrap items-center gap-1.5">
            <strong className="font-semibold [overflow-wrap:anywhere]">{item.name}</strong>
            <KindBadge kind={kind} />
            {item.starts && <Chip tone="success">{NR_FILES.STARTS_HERE}</Chip>}
          </span>
        </button>
        <span className="min-w-0 truncate text-muted-foreground max-[760px]:pl-[60px] max-[760px]:[grid-area:path] max-[520px]:pl-[29px] max-[520px]:whitespace-normal">
          {/* One line, cut with an ellipsis, until the phone layout lets it wrap after "/". */}
          {item.path && <PathText path={item.path} className="whitespace-nowrap [&_wbr]:hidden max-[520px]:whitespace-normal max-[520px]:[&_wbr]:inline" />}
        </span>
        <span className="min-w-0 whitespace-nowrap max-[760px]:justify-self-end max-[760px]:[grid-area:access]">{access}</span>
        <span className="flex items-center justify-end gap-0.5 max-[760px]:[grid-area:actions]">
          <Button type="button" variant="ghost" size="sm" aria-label={NR_FILES.EDIT_NAMED(item.name)} onClick={onEdit}>{NR_FILES.EDIT}</Button>
          <Button type="button" variant="ghost" size="sm" aria-label={NR_FILES.REMOVE_NAMED(item.name)} disabled={!!item.removeBlocked} onClick={onRemove}>{NR_SHARED.REMOVE}</Button>
          {/* Why Remove is off, opened by a tap: the folder leaves only with its workspace's repositories. */}
          {item.removeBlocked && <InfoTip label={item.removeBlocked}>{item.removeBlocked}</InfoTip>}
        </span>
      </div>
      {open && (
        <dl id={bodyId} className="m-0 grid gap-2.5 pt-1 pr-[15px] pb-[15px] pl-[59px] max-[520px]:px-3 max-[520px]:pb-3.5">
          <div className="grid grid-cols-[150px_minmax(0,1fr)] items-center gap-3 border-t border-border pt-2.5 max-[520px]:grid-cols-1 max-[520px]:gap-1">
            <dt className={LABEL}>{NR_FILES.LIFE_LABEL}</dt>
            <dd className="m-0 text-sm leading-[1.45]">{KIND_INFO[item.kind].body}</dd>
          </div>
          {item.kind !== "repo" && (item.storedOn.length > 0 || item.hostPath) && (
            <div className="grid grid-cols-[150px_minmax(0,1fr)] items-center gap-3 border-t border-border pt-2.5 max-[520px]:grid-cols-1 max-[520px]:gap-1">
              <dt className={LABEL}>{NR_FILES.STORED_ON}</dt>
              <dd className="m-0 flex flex-wrap items-center gap-1.5">
                {item.storedOn.map((c) => <Chip key={c}>{c}</Chip>)}
                {item.hostPath && <PathText path={item.hostPath} />}
              </dd>
            </div>
          )}
        </dl>
      )}
    </div>
  );
}
