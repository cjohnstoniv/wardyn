/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Starting folder: where the run starts, chosen only from what was added,
// with an optional subfolder inside it. Choosing never attaches anything.

import * as React from "react";
import { TriangleAlert } from "lucide-react";
import { Input } from "../../../../ui/input";
import { Chip } from "../../../../wardyn/primitives";
import { NR_FILES } from "../../../../wardyn/copy/new-run/files";
import { NR_SHARED } from "../../../../wardyn/copy/new-run/shared";
import type { FileItem } from "./file-items";
import { FilesGroup, PathText } from "./kind-visuals";

/** The id a held Continue focuses when something is added and nothing starts (starts_here_required). */
export const STARTS_FOCUS_ID = "nr-starts-here";
export const SUBFOLDER_ID = "nr-start-subfolder";

const SELECT_CLASS =
  "h-[42px] w-full rounded-md border border-input bg-input-background px-3 text-sm outline-none focus-visible:border-ring focus-visible:ring-[3px] focus-visible:ring-ring disabled:cursor-not-allowed disabled:opacity-60";

/** A subfolder must stay inside its item: no leading "/", no "..", no empty part. */
export function cleanSubfolder(text: string): { value?: string; bad: boolean } {
  const trimmed = text.trim();
  const value = trimmed.replace(/^\/+|\/+$/g, "");
  if (!value) return { bad: false };
  return { value, bad: trimmed.startsWith("/") || value.split("/").some((p) => p === ".." || p === "") };
}

export function StartingFolder({ items, text, onText, onStart }: {
  items: readonly FileItem[];
  /** The subfolder as typed; the step writes only a clean one to the draft. */
  text: string;
  onText: (text: string) => void;
  onStart: (item: FileItem) => void;
}) {
  // Only what the run can start in: of a workspace's folders, the first (the server opens that one).
  const choices = items.filter((i) => i.canStart);
  const start = items.find((i) => i.starts);
  const errorId = React.useId();
  const sub = cleanSubfolder(text);
  const none = choices.length === 0;
  const result = start?.path ? `${start.path}${sub.value && !sub.bad ? `/${sub.value}` : ""}` : "/home/agent";

  return (
    <FilesGroup titleId="nr-start-title" title={NR_FILES.START_TITLE} body={NR_FILES.START_BODY}>
      <div className="grid grid-cols-[minmax(0,1.3fr)_minmax(0,1fr)] gap-3 px-[15px] py-3.5 max-[520px]:grid-cols-1">
        <div className="flex flex-col gap-1.5">
          <label htmlFor={STARTS_FOCUS_ID} className="flex min-h-[26px] items-center text-xs font-semibold">{NR_FILES.START_FOLDER_LABEL}</label>
          <select
            id={STARTS_FOCUS_ID}
            className={SELECT_CLASS}
            disabled={none}
            value={none ? "main" : start?.key ?? ""}
            onChange={(e) => {
              const item = choices.find((i) => i.key === e.target.value);
              if (!item) return;
              onText("");
              onStart(item);
            }}
          >
            {none ? <option value="main">{NR_FILES.START_MAIN}</option> : (
              <>
                {!start && <option value="" disabled hidden />}
                {choices.map((i) => <option key={i.key} value={i.key}>{NR_FILES.START_OPTION(i.name, i.path ?? "")}</option>)}
              </>
            )}
          </select>
        </div>
        <div className="flex flex-col gap-1.5">
          <label htmlFor={SUBFOLDER_ID} className="flex items-center justify-between gap-2 text-xs font-semibold">
            {NR_FILES.SUBFOLDER_LABEL} <Chip tone="info">{NR_SHARED.OPTIONAL}</Chip>
          </label>
          <Input
            id={SUBFOLDER_ID}
            className="h-[42px] font-mono"
            disabled={none || !start}
            value={text}
            placeholder={NR_FILES.SUBFOLDER_PLACEHOLDER}
            aria-invalid={sub.bad || undefined}
            aria-describedby={sub.bad ? errorId : undefined}
            onChange={(e) => onText(e.target.value)}
          />
          {sub.bad && start && <p id={errorId} className="flex items-center gap-1.5 text-xs text-danger"><TriangleAlert className="size-3.5 flex-none" aria-hidden="true" />{NR_FILES.SUBFOLDER_ERROR(start.name)}</p>}
        </div>
        <p className="col-span-full flex flex-wrap items-baseline gap-2 border-t border-border pt-[11px] text-sm">
          <span className="font-semibold text-muted-foreground">{NR_FILES.STARTS_HERE}</span>
          <PathText path={result} className="text-sm font-semibold text-foreground" />
        </p>
      </div>
    </FilesGroup>
  );
}
