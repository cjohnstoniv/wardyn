/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Mount map: a read-only view of where files appear in the sandbox. The paths
// are the server's: the run's own preflight `attachments`, the last answer kept
// while a newer one is asked.

import { NR_FILES } from "../../../../wardyn/copy/new-run/files";
import type { RunAttachment } from "../../../../../lib/types/new-run-contract";

export function MountMap({ attachments, size }: { attachments: readonly RunAttachment[]; size: string }) {
  return (
    <section aria-labelledby="nr-mount-title" className="flex flex-col gap-2 rounded-lg border border-border p-4">
      <div>
        <h2 id="nr-mount-title" className="font-semibold">{NR_FILES.MOUNT_TITLE}</h2>
        <p className="text-sm text-muted-foreground">{NR_FILES.MOUNT_BODY}</p>
      </div>
      <ul className="flex flex-col gap-1 text-sm">
        <li className="flex flex-wrap gap-x-3">
          <code className="font-mono">/</code>
          <span>{NR_FILES.MOUNT_MAIN(size)}</span>
        </li>
        {attachments.map((a) => (
          <li key={`${a.kind}:${a.workspace_id ?? a.drive_id ?? ""}:${a.repo ?? ""}:${a.target}`} className="flex flex-wrap gap-x-3">
            <code className="font-mono">└ {a.target}</code>
            <span>
              {NR_FILES.MOUNT_ITEM(a.name)}
              {a.starts_here && ` ${NR_FILES.MOUNT_STARTS}`}
              {a.read_only && ` ${NR_FILES.MOUNT_READ_ONLY}`}
            </span>
          </li>
        ))}
      </ul>
    </section>
  );
}
