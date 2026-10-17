/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { Chip } from "../../../../wardyn/primitives";
import { NR_FILES } from "../../../../wardyn/copy/new-run/files";
import { NR_REVIEW } from "../../../../wardyn/copy/new-run/review";
import { NR_SHARED } from "../../../../wardyn/copy/new-run/shared";
import type { RunAttachment } from "../../../../../lib/types/new-run-contract";
import { mainDriveMiB } from "../../lenses/files-lens";
import { fieldSizeWords } from "../../steps/files/size-words";
import { readMainDrive, type FilesPreflight } from "../../steps/files/files-seams";
import { summaryText } from "../../steps/files/files-summary";
import { lifecycleOf, lifecycleText } from "../../steps/files/main-drive-section";
import { ReviewGroup } from "../review-group";
import { sourceOf } from "../provenance";
import type { MainDriveEnforcement } from "../../../../../lib/types/runner-pools";
import { stepLocation, type GroupProps } from "./group-props";
import { JumpButton } from "./jump-button";

const ENFORCEMENT: Record<MainDriveEnforcement, string | undefined> = {
  filesystem: NR_FILES.ENFORCED, eviction: NR_FILES.ENFORCED_BY_STOP, none: NR_FILES.NOT_ENFORCED, unknown: undefined,
};

const FILE_PATHS = ["workspaces", "drive", "drives", "start_folder", "no_repositories_or_drives", "resources.disk_mib"];

export function AttachmentRow({ a }: { a: RunAttachment }) {
  return (
    <div className="grid min-w-0 grid-cols-[minmax(110px,1fr)_minmax(140px,1.2fr)_auto] items-center gap-2 text-sm max-[520px]:grid-cols-1 max-[520px]:items-start">
      <strong className="font-medium">{a.name}</strong>
      <code className="font-mono text-xs break-all text-muted-foreground">{a.target}</code>
      <span className="flex flex-wrap justify-end gap-1.5 max-[520px]:justify-start">
        {a.ref && <Chip>{a.ref}</Chip>}
        <Chip>{a.read_only ? NR_FILES.ACCESS_READ : NR_FILES.ACCESS_EDIT}</Chip>
        {a.starts_here && <Chip>{NR_FILES.STARTS_HERE}</Chip>}
      </span>
    </div>
  );
}

export function WorkGroup({ draft, pre, preview, docRows, go }: GroupProps) {
  const { intent } = draft;
  const attachments = pre?.attachments ?? [];
  const count = pre?.attachments ? attachments.length : (intent.workspaces?.length ?? 0) + (intent.drive?.enabled ? 1 : 0) + (intent.drives?.length ?? 0);
  // The server's applied size when it sized one (0 means none); the request until it answers.
  const mib = pre?.main_drive?.applied_mib || mainDriveMiB(intent);
  const size = mib === undefined ? undefined : fieldSizeWords(mib);
  // The same preflight answer the Files step reads: the run's pool says what its runners do with the size.
  const drive = readMainDrive(pre as FilesPreflight | null, intent.experience);
  const enforcement = drive.facts && ENFORCEMENT[drive.facts.enforcement];
  const life = lifecycleOf(intent, drive);
  // The Files step's summary sentence; until a size is known, the names of what is attached (none: the detail says so).
  const value = size !== undefined
    ? summaryText({
      size,
      noExtra: intent.no_repositories_or_drives === true,
      chosen: count > 0,
      startsName: attachments.find((a) => a.starts_here)?.name,
      // As on the Files step: drives and folders are live storage, so their changes are kept.
      keptDrives: attachments.filter((a) => a.kind !== "repository").map((a) => a.name),
      life,
    })
    : attachments.map((a) => a.name).reduce<string | undefined>((all, n) => (all ? NR_SHARED.JOIN(all, n) : n), undefined);
  const lifeText = lifecycleText(life);
  return (
    <ReviewGroup
      id="work"
      label={NR_REVIEW.GROUP_WORK}
      value={value}
      detail={count === 0 ? NR_REVIEW.WORK_NONE : count === 1 ? NR_REVIEW.WORK_ITEM_ONE : NR_REVIEW.WORK_ITEMS(count)}
      provenance={sourceOf({ draft, pre, preview }, { fields: ["workspace_mounts", "workspace_repos"] }, FILE_PATHS, NR_REVIEW.PROV_ADDED)}
      docRows={docRows}
      editor={() => <JumpButton label={NR_REVIEW.EDIT_FILES} to={stepLocation("files")} go={go} />}
    >
      {size !== undefined && (
        <div className="my-1.5 grid grid-cols-[1fr_auto] gap-x-2.5 gap-y-1 rounded-md border border-border bg-surface-2 px-2.5 py-2 text-sm">
          <strong className="font-medium">{NR_REVIEW.MAIN_DRIVE(size)}</strong>
          {enforcement ? <span className="text-primary">{enforcement}</span> : <span />}
          {lifeText && <small className="col-span-full text-xs text-muted-foreground">{lifeText}</small>}
        </div>
      )}
      {attachments.length > 0 && (
        <div className="my-1.5 grid gap-1.5">
          {attachments.map((a) => <AttachmentRow key={`${a.kind}:${a.workspace_id ?? a.drive_id ?? a.name}:${a.repo ?? ""}`} a={a} />)}
        </div>
      )}
    </ReviewGroup>
  );
}
