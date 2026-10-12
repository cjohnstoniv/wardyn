/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Mount map: a read-only view of where files appear in the sandbox. The paths
// are the server's: the run's own preflight `attachments`, or, while that is
// still being asked, the workspace's `clone_target` for what is included.

import { NR_FILES } from "../../../../wardyn/copy/new-run/files";
import type { RunAttachment } from "../../../../../lib/types/new-run-contract";
import type { RunIntent } from "../../draft/run-intent";
import { repoIncluded, startsAt } from "../../lenses/files-lens";
import type { RepoRow } from "./files-seams";

/** The included repositories as the server would list them, for the moment its answer is not current. */
export function draftAttachments(intent: RunIntent, rows: readonly RepoRow[]): RunAttachment[] {
  return rows.flatMap((r) =>
    repoIncluded(intent, r.workspaceId, r.repo) && r.path
      ? [{
        kind: "repository" as const, workspace_id: r.workspaceId, repo: r.repo, name: r.name, target: r.path, read_only: false,
        starts_here: startsAt(intent, r.workspaceId, r.repo, r.siblings.length),
      }]
      : [],
  );
}

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
