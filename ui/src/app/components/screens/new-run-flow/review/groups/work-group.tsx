/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { Chip } from "../../../../wardyn/primitives";
import { NR_FILES } from "../../../../wardyn/copy/new-run/files";
import { NR_REVIEW } from "../../../../wardyn/copy/new-run/review";
import { NR_SHARED } from "../../../../wardyn/copy/new-run/shared";
import type { RunAttachment } from "../../../../../lib/types/new-run-contract";
import { ReviewGroup } from "../review-group";
import { sourceOf } from "../provenance";
import { mainDriveGiB, type MainDriveFacts } from "../review-seams";
import { stepLocation, type GroupProps } from "./group-props";
import { JumpButton } from "./jump-button";

const ENFORCEMENT: Record<MainDriveFacts["enforcement"], string | undefined> = {
  filesystem: NR_FILES.ENFORCED, eviction: NR_FILES.ENFORCED_BY_STOP, none: NR_FILES.NOT_ENFORCED, unknown: undefined,
};

const FILE_PATHS = ["workspaces", "drive", "start_folder", "no_repositories_or_drives", "resources.disk_mib"];

function AttachmentRow({ a }: { a: RunAttachment }) {
  return (
    <div className="flex flex-wrap items-center gap-2 text-sm">
      <strong className="font-medium">{a.name}</strong>
      <code className="font-mono text-xs">{a.target}</code>
      {a.ref && <Chip>{a.ref}</Chip>}
      <Chip>{a.read_only ? NR_FILES.ACCESS_READ : NR_FILES.ACCESS_EDIT}</Chip>
      {a.starts_here && <Chip>{NR_FILES.STARTS_HERE}</Chip>}
    </div>
  );
}

export function WorkGroup({ draft, pre, preview, docRows, go }: GroupProps) {
  const attachments = pre?.attachments ?? [];
  const count = pre?.attachments ? attachments.length : (draft.intent.workspaces?.length ?? 0) + (draft.intent.drive?.enabled ? 1 : 0);
  const names = attachments.map((a) => a.name).reduce<string | undefined>((all, n) => (all ? NR_SHARED.JOIN(all, n) : n), undefined);
  // The server's applied size when it sized one (0 means none); the request until it answers.
  const applied = pre?.main_drive?.applied_mib;
  const size = applied ? applied / 1024 : mainDriveGiB(draft.intent);
  const facts = pre?.main_drive?.facts;
  const enforcement = facts && ENFORCEMENT[facts.enforcement];
  return (
    <ReviewGroup
      id="work"
      label={NR_REVIEW.GROUP_WORK}
      value={count === 0 ? NR_REVIEW.WORK_NONE : names}
      detail={count === 0 ? undefined : count === 1 ? NR_REVIEW.WORK_ITEM_ONE : NR_REVIEW.WORK_ITEMS(count)}
      provenance={sourceOf({ draft, pre, preview }, { fields: ["workspace_mounts", "workspace_repos"] }, FILE_PATHS, NR_REVIEW.PROV_ADDED)}
      docRows={docRows}
      editor={() => <JumpButton label={NR_REVIEW.EDIT_FILES} to={stepLocation("files")} go={go} />}
    >
      {size !== undefined && (
        <p className="flex flex-wrap items-center gap-2 text-sm">
          <strong className="font-medium">{NR_REVIEW.MAIN_DRIVE(size)}</strong>
          {enforcement && <span className="text-muted-foreground">{enforcement}</span>}
        </p>
      )}
      {attachments.map((a) => <AttachmentRow key={`${a.kind}:${a.workspace_id ?? a.drive_id ?? a.name}:${a.repo ?? ""}`} a={a} />)}
    </ReviewGroup>
  );
}
