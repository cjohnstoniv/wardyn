/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Step 4, Files: the main drive every run has, the repositories it may open,
// and where it starts. Drives are the next section's (drives-section); the
// mount map and the summary already read them from the run's preflight.

import * as React from "react";
import { Chip } from "../../../../wardyn/primitives";
import { NR_FILES } from "../../../../wardyn/copy/new-run/files";
import { NR_SHARED } from "../../../../wardyn/copy/new-run/shared";
import { workspaces } from "../../../../../lib/api/workspaces";
import { intentToWire } from "../../draft/intent-to-wire";
import { chooseNothing, mibToGibText, parseGiB, repoIncluded, startsAt } from "../../lenses/files-lens";
import { stepIssues } from "../../lenses/local-issues";
import { issue, type StepIssue } from "../../lenses/step-issues";
import { useNewRunDispatch, useNewRunDraft, type NewRunScreenProps } from "../../new-run-route";
import { flowSteps, StepShell } from "../step-shell";
import { useLoaded } from "../shape/use-loaded";
import { readMainDrive, repoRows, useFilesPreflight } from "./files-seams";
import { FilesSummary, summaryText } from "./files-summary";
import { lifecycleOf, MainDriveSection, useMainDriveSize } from "./main-drive-section";
import { draftAttachments, MountMap } from "./mount-map";
import { RepositoriesSection } from "./repositories-section";

const STEP_LABELS = {
  shape: NR_SHARED.STEP_SHAPE, run: NR_SHARED.STEP_RUN, harnesses: NR_SHARED.STEP_HARNESSES, files: NR_SHARED.STEP_FILES, tools: NR_SHARED.STEP_TOOLS,
};
const EARLIER = ["shape", "run", "harnesses"] as const;
const list = () => workspaces.listWorkspaces();

export function FilesStep({ go }: NewRunScreenProps) {
  const draft = useNewRunDraft();
  const dispatch = useNewRunDispatch();
  const intent = draft.intent;
  const loaded = useLoaded(list);
  const rows = React.useMemo(() => (loaded.status === "ready" ? repoRows(loaded.value) : []), [loaded]);

  // Facts need a body the server would read; a draft still missing an earlier step's answer asks nothing.
  const wire = React.useMemo(() => intentToWire(intent), [intent]);
  const earlierOpen = EARLIER.some((s) => stepIssues(s, intent).length > 0);
  const { result: pre, current } = useFilesPreflight(earlierOpen ? null : wire.body);
  const view = readMainDrive(pre, intent.experience);
  const size = useMainDriveSize(view);
  const life = lifecycleOf(intent, view);

  const attachments = current && pre?.attachments ? pre.attachments : draftAttachments(intent, rows);
  const pathOf = (r: (typeof rows)[number]) =>
    (current ? pre?.attachments?.find((a) => a.kind === "repository" && a.workspace_id === r.workspaceId && a.repo === r.repo)?.target : undefined) ?? r.path;
  const noExtra = intent.no_repositories_or_drives === true;
  const sizeMib = parseGiB(size.text);
  const sizeText = sizeMib === undefined ? "0" : mibToGibText(sizeMib);
  const startsName = rows.find((r) => repoIncluded(intent, r.workspaceId, r.repo) && startsAt(intent, r.workspaceId, r.repo, r.siblings.length))?.name ??
    attachments.find((a) => a.starts_here)?.name;

  const issues: StepIssue[] = [...(size.missing ? [issue("files", "main_drive_size_required")] : []), ...stepIssues("files", intent)];
  const keptNote = life.kind === "deleted" ? NR_FILES.NO_EXTRA_DELETED : life.kind === "kept" ? NR_FILES.NO_EXTRA_KEPT(life.grace) : undefined;

  return (
    <StepShell
      eyebrow={NR_SHARED.EYEBROW(4)}
      title={NR_FILES.TITLE}
      lede={NR_FILES.LEDE}
      progress={{ label: NR_SHARED.PROGRESS_ARIA, steps: flowSteps(STEP_LABELS, "files") }}
      back={{ label: NR_SHARED.BACK, onBack: () => go({ screen: "step", step: "harnesses" }) }}
      primary={{
        label: NR_SHARED.CONTINUE_TO(NR_SHARED.STEP_TOOLS),
        onActivate: () => {
          size.commit();
          go({ screen: "step", step: "tools" });
        },
      }}
      issues={issues}
    >
      <div className="flex flex-col gap-4">
        <MainDriveSection view={view} size={size} />
        {loaded.status === "error" && <p role="alert" className="text-sm text-danger">{loaded.message}</p>}
        <RepositoriesSection
          rows={rows}
          pathOf={pathOf}
          startsFocusKey={rows.find((r) => repoIncluded(intent, r.workspaceId, r.repo))?.key}
        />
        <section aria-labelledby="nr-computer-title" className="flex flex-col gap-1 rounded-lg border border-border p-4 opacity-70">
          <div className="flex items-start justify-between gap-3">
            <div>
              <h2 id="nr-computer-title" className="font-semibold">{NR_FILES.COMPUTER_TITLE}</h2>
              <p className="text-sm text-muted-foreground">{NR_FILES.COMPUTER_BODY}</p>
            </div>
            <Chip>{NR_SHARED.COMING_SOON}</Chip>
          </div>
          <p className="text-sm">{NR_FILES.COMPUTER_SOON}</p>
        </section>
        <label className="flex items-start gap-3 rounded-lg border border-border p-4">
          <input
            id="nr-files-choice"
            type="radio"
            name="nr-files-choice"
            className="mt-1 accent-primary"
            checked={noExtra}
            onChange={() => dispatch({ type: "edit", apply: chooseNothing })}
          />
          <span className="flex flex-col gap-1">
            <strong className="font-medium">{NR_FILES.NO_EXTRA}</strong>
            {noExtra && keptNote && <small className="text-sm text-muted-foreground">{keptNote}</small>}
          </span>
        </label>
        <MountMap attachments={attachments} size={sizeText} />
        <FilesSummary
          text={summaryText({
            size: sizeText,
            noExtra,
            chosen: attachments.length > 0 || (intent.workspaces?.length ?? 0) > 0 || !!intent.drive?.enabled,
            startsName,
            keptDrives: attachments.filter((a) => a.kind === "drive").map((a) => a.name),
            life,
          })}
        />
      </div>
    </StepShell>
  );
}
