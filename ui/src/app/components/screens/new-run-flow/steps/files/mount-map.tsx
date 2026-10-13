/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Mount map: a read-only view of where files appear in the sandbox. The paths
// are the server's: the run's own preflight `attachments`, the last answer kept
// while a newer one is asked.

import { NR_FILES } from "../../../../wardyn/copy/new-run/files";
import { FilesGroup } from "./kind-visuals";
import type { RunAttachment } from "../../../../../lib/types/new-run-contract";

const ROW = "grid min-w-0 grid-cols-[minmax(170px,0.8fr)_minmax(0,1fr)] gap-3 max-[520px]:grid-cols-1 max-[520px]:gap-0.5";

export function MountMap({ attachments, size }: { attachments: readonly RunAttachment[]; size: string }) {
  return (
    <FilesGroup titleId="nr-mount-title" title={NR_FILES.MOUNT_TITLE} body={NR_FILES.MOUNT_BODY}>
      <ul className="grid gap-2 px-[15px] py-[13px] font-mono text-sm leading-[1.45]">
        <li className={ROW}>
          <code className="[overflow-wrap:anywhere]">/</code>
          <span className="text-muted-foreground">{NR_FILES.MOUNT_MAIN(size)}</span>
        </li>
        {attachments.map((a) => (
          <li key={`${a.kind}:${a.workspace_id ?? a.drive_id ?? ""}:${a.repo ?? ""}:${a.target}`} className={ROW}>
            <code className="[overflow-wrap:anywhere]">└ {a.target}</code>
            <span className="text-muted-foreground [overflow-wrap:anywhere]">
              {NR_FILES.MOUNT_ITEM(a.name)}
              {a.starts_here && ` ${NR_FILES.MOUNT_STARTS}`}
              {a.read_only && ` ${NR_FILES.MOUNT_READ_ONLY}`}
            </span>
          </li>
        ))}
      </ul>
    </FilesGroup>
  );
}
