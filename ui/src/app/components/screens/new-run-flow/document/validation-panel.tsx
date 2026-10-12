/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The run document's status: how many problems there are, or that it is valid.
// The editor lists the server's problems on their lines; `lines` carries only
// the ones that have no line of their own (the local parse, the size cap, a
// failed request). The route's announcer says the result, so this is not live.

import type { DocumentCopy } from "./copy-seam";

export type DocumentStatus =
  | { kind: "ready" }
  | { kind: "valid" }
  | { kind: "issues"; count: number; lines: readonly string[] };

export function ValidationPanel({ id, status, copy }: { id: string; status: DocumentStatus; copy: DocumentCopy }) {
  return (
    <div id={id} data-testid="validation-panel" data-status={status.kind} className="flex flex-col gap-1 text-sm">
      {status.kind === "ready" && <p className="text-muted-foreground">{copy.ready}</p>}
      {status.kind === "valid" && <p>{copy.valid}</p>}
      {status.kind === "issues" && (
        <>
          <p className="font-medium text-danger">{copy.issues(status.count)}</p>
          {status.lines.map((line, i) => (
            <p key={i} className="select-text whitespace-pre-wrap break-words text-danger">{line}</p>
          ))}
        </>
      )}
    </div>
  );
}
