/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Step 4, Files, as the owner-approved Files prototype (2026-10-13) draws it:
// the main drive every run has; one list of the repositories, drives and
// folders it attaches, added and edited through one dialog; the starting
// folder chosen from that list; the server's mount map; a sticky summary.

import * as React from "react";
import { TriangleAlert } from "lucide-react";
import { Button } from "../../../../ui/button";
import { cn } from "../../../../ui/utils";
import { useOperator } from "../../../../wardyn/operator-context";
import { NR_FILES } from "../../../../wardyn/copy/new-run/files";
import { NR_SHARED } from "../../../../wardyn/copy/new-run/shared";
import { workspaces } from "../../../../../lib/api/workspaces";
import { useK8sRunner } from "../../../../../lib/use-k8s-runner";
import { intentToWire } from "../../draft/intent-to-wire";
import type { RunIntent } from "../../draft/run-intent";
import { attachDrive, chooseNothing, mibToGibText, parseGiB, setStartsAtDrive, setStartSubpath } from "../../lenses/files-lens";
import { stepIssues } from "../../lenses/local-issues";
import { issue, type StepIssue } from "../../lenses/step-issues";
import { useNewRunDispatch, useNewRunDraft, type NewRunScreenProps } from "../../new-run-route";
import { flowSteps, StepShell, useAnnounce } from "../step-shell";
import { useLoaded } from "../shape/use-loaded";
import { AddDialog, type AddDialogProps, type DialogOpen } from "./add-dialog";
import { AttachCard } from "./attach-card";
import { fileItems, folderSources, legacyIdOf, removeItem, startIn, type FileItem, type ItemSources } from "./file-items";
import { useDriveCatalogue } from "./file-sources";
import { readMainDrive, repoRows, useFilesPreflight } from "./files-seams";
import { FilesSummary, summaryText } from "./files-summary";
import { FilesGroup } from "./kind-visuals";
import { lifecycleOf, MainDriveSection, useMainDriveSize } from "./main-drive-section";
import { MountMap } from "./mount-map";
import { cleanSubfolder, StartingFolder, SUBFOLDER_ID } from "./starting-folder";

const STEP_LABELS = {
  shape: NR_SHARED.STEP_SHAPE, run: NR_SHARED.STEP_RUN, harnesses: NR_SHARED.STEP_HARNESSES, files: NR_SHARED.STEP_FILES, tools: NR_SHARED.STEP_TOOLS,
};
const EARLIER = ["shape", "run", "harnesses"] as const;
// Choosing nothing yet is the step's open question, not a reason to withhold the pool's facts that frame it.
const FILES_CHOICE = "files_choice_required";
/** The control a held Continue focuses while nothing is added or chosen (files_choice_required). */
const ADD_ID = "nr-files-choice";
const list = () => workspaces.listWorkspaces();

/** Whether the person has chosen nothing for the step yet, the only time a default may be chosen for them. */
export const nothingChosen = (intent: RunIntent): boolean =>
  !intent.workspaces?.length && !intent.drive && !intent.drives?.length && !intent.start_folder && intent.no_repositories_or_drives === undefined;

export function FilesStep({ go, createUserDrive }: NewRunScreenProps & Pick<AddDialogProps, "createUserDrive">) {
  const draft = useNewRunDraft();
  const dispatch = useNewRunDispatch();
  const announce = useAnnounce();
  const intent = draft.intent;
  const edit = React.useCallback((apply: (i: RunIntent) => RunIntent) => dispatch({ type: "edit", apply }), [dispatch]);
  const loaded = useLoaded(list);
  const rows = React.useMemo(() => (loaded.status === "ready" ? repoRows(loaded.value) : []), [loaded]);
  const folders = React.useMemo(() => (loaded.status === "ready" ? folderSources(loaded.value) : []), [loaded]);

  // Nothing is sent while a step's local issues are open; the size has no issue here, as an untouched prefill is not a request.
  const wire = React.useMemo(() => intentToWire(intent), [intent]);
  const open = [...EARLIER, "files" as const].some((step) => stepIssues(step, intent).some((i) => i.code !== FILES_CHOICE));
  const pre = useFilesPreflight(open ? null : wire.body);
  const view = readMainDrive(pre, intent.experience);
  const size = useMainDriveSize(view);
  const life = lifecycleOf(intent, view);
  const drives = useDriveCatalogue(pre?.runner_pool?.id ?? intent.runner_pool_id);
  const k8s = useK8sRunner();
  const operator = useOperator();

  const attachments = pre?.attachments ?? [];
  const src: ItemSources = { rows, folders, catalogue: drives.catalogue, attachments };
  const items = fileItems(intent, src);
  const legacyId = legacyIdOf(drives.catalogue);
  const noExtra = intent.no_repositories_or_drives === true;
  const start = items.find((i) => i.starts);

  // Environment opens with the person's own drive when it can attach, once, and only while nothing is chosen (A18).
  const preselected = React.useRef(false);
  const legacy = drives.catalogue?.drives.find((d) => d.legacy && !d.unavailable_reason);
  const preselectId = !drives.reason ? legacy?.id : undefined;
  React.useEffect(() => {
    if (preselected.current || !preselectId || intent.shape !== "environment" || !nothingChosen(intent)) return;
    preselected.current = true;
    edit((i) => setStartsAtDrive(attachDrive(i, preselectId, preselectId), preselectId, preselectId));
  }, [preselectId, intent, edit]);

  const [expanded, setExpanded] = React.useState<readonly string[]>([]);
  const [dialog, setDialog] = React.useState<DialogOpen | null>(null);
  const [attempted, setAttempted] = React.useState(false);
  const [subText, setSubText] = React.useState(intent.start_folder?.subpath ?? "");
  const sub = cleanSubfolder(subText);

  const onSave: AddDialogProps["onSave"] = (apply, saved, mode) => {
    edit((i) => {
      const next = apply(i);
      if (mode === "edit" || fileItems(next, src).some((x) => x.starts)) return next;
      // The first thing added is where the run starts, until the person moves it.
      const added = fileItems(next, src).find((x) => x.key === saved.key);
      return added ? startIn(added, legacyId)(next) : next;
    });
    if (mode === "add") setExpanded((e) => [...e, saved.key]);
    announce(mode === "add" ? NR_FILES.ADDED(saved.name) : NR_FILES.UPDATED(saved.name));
    setDialog(null);
  };
  const remove = (item: FileItem) => {
    edit((i) => {
      const next = removeItem(item, src)(i);
      if (!item.starts) return next;
      // The start moves to the first item left.
      const first = fileItems(next, src)[0];
      return first ? startIn(first, legacyId)(next) : next;
    });
    if (item.starts) setSubText("");
    announce(NR_FILES.REMOVED(item.name));
  };

  const issues: StepIssue[] = [
    ...(size.missing ? [issue("files", "main_drive_size_required")] : []),
    ...stepIssues("files", intent),
    ...(sub.bad ? [{ ...issue("files", "starts_here_required"), focus: SUBFOLDER_ID }] : []),
  ];
  const keptNote = life.kind === "deleted" ? NR_FILES.NO_EXTRA_DELETED : life.kind === "kept" ? NR_FILES.NO_EXTRA_KEPT(life.grace) : undefined;
  const sizeMib = parseGiB(size.text);
  const sizeText = sizeMib === undefined ? "0" : mibToGibText(sizeMib);
  const settled = noExtra || (items.length > 0 && !!start);
  const empty = !noExtra && items.length === 0;

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
      <div className="flex flex-col gap-3">
        <MainDriveSection view={view} size={size} />
        {loaded.status === "error" && <p role="alert" className="text-sm text-danger">{loaded.message}</p>}
        <FilesGroup
          titleId="nr-attach-title"
          title={NR_FILES.ATTACH_TITLE}
          body={NR_FILES.ATTACH_BODY}
          aside={
            <Button
              id={ADD_ID}
              type="button"
              variant="outline"
              className="flex-none"
              aria-label={NR_FILES.ADD_ARIA}
              // A held Continue marks this button invalid and focuses it: that is when the empty error shows.
              onFocus={(e) => e.currentTarget.getAttribute("aria-invalid") === "true" && setAttempted(true)}
              onClick={() => setDialog({ mode: "add" })}
            >
              {NR_FILES.ADD}
            </Button>
          }
        >
          {items.length > 0 ? (
            <div className="grid">
              {items.map((item) => (
                <AttachCard
                  key={item.key}
                  item={item}
                  open={expanded.includes(item.key)}
                  onToggle={() => setExpanded((e) => (e.includes(item.key) ? e.filter((k) => k !== item.key) : [...e, item.key]))}
                  onEdit={() => setDialog({ mode: "edit", item })}
                  onRemove={() => remove(item)}
                />
              ))}
            </div>
          ) : (
            <div className="px-[15px] py-3">
              <label className={cn("flex items-start gap-2.5 rounded-[10px] border border-border bg-muted px-[15px] py-3.5", noExtra && "border-warning bg-warning-subtle")}>
                <input
                  type="radio"
                  name="nr-files-choice"
                  className="mt-0.5 size-[17px] accent-primary"
                  checked={noExtra}
                  onChange={() => edit(chooseNothing)}
                />
                <span className="flex flex-col gap-[5px]">
                  <strong className="text-sm font-semibold">{NR_FILES.NO_EXTRA}</strong>
                  {noExtra && keptNote && (
                    <small className="flex items-start gap-1.5 text-sm text-muted-foreground">
                      <TriangleAlert className="mt-0.5 size-3.5 flex-none" aria-hidden="true" />
                      {keptNote}
                    </small>
                  )}
                </span>
              </label>
            </div>
          )}
        </FilesGroup>
        {attempted && empty && (
          <p className="flex items-center gap-1.5 text-sm text-danger">
            <TriangleAlert className="size-3.5" aria-hidden="true" />
            {NR_FILES.CHOOSE_FILES}
          </p>
        )}
        <StartingFolder
          items={items}
          text={subText}
          onText={(text) => {
            setSubText(text);
            const next = cleanSubfolder(text);
            edit((i) => setStartSubpath(i, next.bad ? undefined : next.value));
          }}
          onStart={(item) => edit(startIn(item, legacyId))}
        />
        <MountMap attachments={attachments} size={sizeText} />
        <FilesSummary
          tone={settled ? "done" : attempted ? "error" : "prompt"}
          text={summaryText({
            size: sizeText,
            noExtra,
            chosen: items.length > 0,
            startsName: start && `${start.name}${sub.value && !sub.bad ? `/${sub.value}` : ""}`,
            keptDrives: items.filter((i) => i.kind !== "repo").map((i) => i.name),
            life,
          })}
        />
      </div>
      <AddDialog
        open={dialog}
        onClose={() => setDialog(null)}
        intent={intent}
        src={src}
        items={items}
        folderReason={k8s ? NR_FILES.FOLDER_REMOTE : undefined}
        driveReason={drives.reason}
        operator={operator}
        onSave={onSave}
        createUserDrive={createUserDrive}
        reloadDrives={drives.reload}
      />
    </StepShell>
  );
}
