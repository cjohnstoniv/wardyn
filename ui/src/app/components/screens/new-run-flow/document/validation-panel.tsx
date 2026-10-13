/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The run document's status: how many problems there are and each one's words,
// or that it is valid. The editor only marks the lines. The route's announcer
// says the result, so this is not live.

import { Check, TriangleAlert } from "lucide-react";
import { cn } from "../../../ui/utils";
import type { NR_DOCUMENT } from "../../../wardyn/copy/new-run/document";

export type DocumentStatus =
  | { kind: "ready" }
  | { kind: "valid" }
  | { kind: "issues"; count: number; lines: readonly string[] };

export function ValidationPanel({ id, status, copy }: { id: string; status: DocumentStatus; copy: Pick<typeof NR_DOCUMENT, "READY" | "VALID" | "ISSUES"> }) {
  // Make's .validation-panel (snapshot index.css:397-402): a bordered box, a check on the ready line, an alert on the count.
  return (
    <div id={id} data-testid="validation-panel" data-status={status.kind} className="flex flex-col gap-1.5 rounded-lg border border-border bg-card px-3.5 py-3 text-xs">
      {status.kind !== "issues" && (
        <p className={cn("flex items-center gap-1.5", status.kind === "ready" && "text-muted-foreground")}>
          <Check aria-hidden="true" className="size-[15px] shrink-0 text-success" />
          {status.kind === "ready" ? copy.READY : copy.VALID}
        </p>
      )}
      {status.kind === "issues" && (
        <>
          <p className="flex items-center gap-1.5 font-semibold text-danger">
            <TriangleAlert aria-hidden="true" className="size-[15px] shrink-0" />
            {copy.ISSUES(status.count)}
          </p>
          {status.lines.map((line, i) => (
            <p key={i} className="select-text whitespace-pre-wrap break-words text-muted-foreground">{line}</p>
          ))}
        </>
      )}
    </div>
  );
}
