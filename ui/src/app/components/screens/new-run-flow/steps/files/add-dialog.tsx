/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Add / Edit dialog of the owner-approved Files prototype: Type, then
// Choose, then Mount path. Edit opens on Mount path, pre-filled. Nothing is
// written to the draft until Add or Save.

import * as React from "react";
import { Check, ChevronRight, TriangleAlert, Lock } from "lucide-react";
import { Link } from "react-router-dom";
import { Button } from "../../../../ui/button";
import { Dialog, DialogContent, DialogTitle } from "../../../../ui/dialog";
import { Input } from "../../../../ui/input";
import { cn } from "../../../../ui/utils";
import { InfoTip } from "../../../../wardyn/info-tip";
import { Chip } from "../../../../wardyn/primitives";
import { NR_FILES } from "../../../../wardyn/copy/new-run/files";
import { NR_SHARED } from "../../../../wardyn/copy/new-run/shared";
import type { RunIntent } from "../../draft/run-intent";
import { BranchPicker } from "./branch-picker";
import { refTextKey } from "./typed-ref";
import { driveChips, driveKind, fileItems, KIND_ORDER, pathError, saveChoice, choiceKey, type FileItem, type FileKind, type ItemChoice, type ItemSources } from "./file-items";
import { driveReason } from "./file-sources";
import { KIND_INFO, KIND_LABEL, KindTile, PathText } from "./kind-visuals";
import { LockedAccess } from "./locked-access";
import { COMPUTER_DEFAULT_DIR, ComputerChoose, ComputerMount, setLocalFolder, type ComputerChoice } from "./computer-folder";

type Step = 1 | 2 | 3;
type AttachKind = Exclude<FileKind, "computer">;

/** What the dialog is doing: adding from scratch, or editing one card. */
export type DialogOpen = { mode: "add" } | { mode: "edit"; item: FileItem };

interface State {
  step: Step;
  reached: Step;
  kind: AttachKind | "computer" | null;
  sourceKey: string | null;
  /** The folder "Your computer" picked, and which way its changes go. */
  computer: ComputerChoice | null;
  direction: "both" | "in";
  /** The ref sent for a repository; undefined follows the source's own. */
  branch?: string;
  branchText: string;
  /** Whether the ref was typed (true) or picked (false) in this dialog; absent when it was not touched. */
  refTyped?: boolean;
  path: string;
  readOnly: boolean;
  attempted: boolean;
}

interface PickOption {
  key: string;
  name: string;
  chips: string[];
  /** A folder's host path, otherwise where it appears. */
  shownPath?: string;
  defaultPath?: string;
  locked: boolean;
  reason?: string;
  /** A kept work folder: the line naming its run, and whether its size is unenforced (a Docker volume). */
  kept?: { line: string; unenforced: boolean };
  ref?: string;
  workspaceId?: string;
  repo?: string;
}

export const DIALOG_PATH_ID = "nr-mount-path";
/** The key the computer folder's card and announcements go by: a run has at most one. */
export const COMPUTER_KEY = "computer";
const EDIT_SAVE_ID = "nr-mount-save";

const ADD_TITLE: Record<AttachKind | "computer", string> = {
  computer: NR_FILES.COMPUTER_TITLE,
  repo: NR_FILES.ADD_REPO_TITLE,
  userDrive: NR_FILES.ADD_USER_DRIVE_TITLE,
  sharedDrive: NR_FILES.ADD_SHARED_DRIVE_TITLE,
  folder: NR_FILES.ADD_FOLDER_TITLE,
};

const STEPS: [Step, string][] = [[1, NR_FILES.STEP_TYPE], [2, NR_FILES.STEP_CHOOSE], [3, NR_FILES.STEP_MOUNT]];

// Make's .files-link: the accent colour, weight 600, no underline.
export const FILES_LINK = "text-sm font-semibold text-primary hover:underline";
const FIELD_LABEL = "text-xs font-semibold";

const SELECT_CLASS =
  "h-[42px] w-full max-w-[220px] rounded-md border border-border-strong bg-input-background px-3 text-sm outline-none focus-visible:border-ring focus-visible:ring-[3px] focus-visible:ring-ring";

export interface AddDialogProps {
  open: DialogOpen | null;
  onClose: () => void;
  intent: RunIntent;
  src: ItemSources;
  items: readonly FileItem[];
  /** Folders cannot be opened here: the sentence why (the server runs its sandboxes on Kubernetes). The tile is then hidden. */
  folderReason?: string;
  /** WARDYN_COMPUTER_FOLDERS: false shows "Your computer" as blocked by the organisation; undefined while unknown. */
  computerAllowed?: boolean;
  /** Why no drive can be attached at all, from the catalogue. */
  driveReason?: string;
  /** The person may manage drives (Manage drives →). */
  operator: boolean;
  /** `previousKey`: the card an edit replaced, when it now names another source. */
  /** Workspaces the person added a folder from (removeItem keeps their folders). */
  foldersAdded?: ReadonlySet<string>;
  /** `ref`: set only when the person typed or picked the repository's ref here; `typed` is the typed text, "" for a pick. */
  onSave: (apply: (intent: RunIntent) => RunIntent, item: { name: string; key: string; previousKey?: string; ref?: { key: string; typed: string } }, mode: "add" | "edit") => void;
  /**
   * Seam for person-created drives: rendered under the user drives of the Choose step. `choose` selects a
   * drive by id once it is in the catalogue (`reload` re-reads it). Absent: nothing is offered.
   */
  createUserDrive?: (ctx: { choose: (driveId: string) => void; reload: () => void }) => React.ReactNode;
  reloadDrives: () => void;
}

function optionsFor(kind: AttachKind, src: ItemSources): PickOption[] {
  if (kind === "repo") {
    return src.rows.map((r) => ({ key: r.key, name: r.name, chips: [], shownPath: r.path, defaultPath: r.path, locked: false, reason: r.unavailable, ref: r.ref, workspaceId: r.workspaceId, repo: r.repo }));
  }
  if (kind === "folder") {
    return src.folders.map((f) => ({ key: f.key, name: f.name, chips: [NR_FILES.FAMILY_HOST_FOLDER, ...(f.writable ? [] : [NR_FILES.ACCESS_READ])], shownPath: f.hostPath, defaultPath: f.defaultPath, locked: !f.writable, reason: f.unavailable }));
  }
  return (src.catalogue?.drives ?? []).filter((d) => driveKind(d) === kind).map((d) => ({
    key: d.id, name: d.name, chips: [...driveChips(d), ...(d.writable ? [] : [NR_FILES.ACCESS_READ])], shownPath: d.target, defaultPath: d.target,
    locked: !d.writable, reason: driveReason(d),
    kept: d.kept_from_run ? { line: d.kept_from_run_title ? NR_FILES.DRIVE_KEPT_FROM(d.kept_from_run_title) : NR_FILES.DRIVE_KEPT_FROM_UNTITLED, unenforced: d.enforcement === "none" } : undefined,
  }));
}

function initial(open: DialogOpen, intent: RunIntent): State {
  if (open.mode === "add") {
    return { step: 1, reached: 1, kind: null, sourceKey: null, computer: null, direction: "both", branchText: "", path: "", readOnly: false, attempted: false };
  }
  const item = open.item;
  // An edit keeps the draft's own ref, not the source's, until the person picks another.
  const ref = item.kind === "repo" ? intent.workspaces?.find((w) => w.workspace_id === item.workspaceId)?.repos?.find((r) => r.repo === item.repo)?.ref : undefined;
  return {
    step: 3, reached: 3, kind: item.kind as AttachKind, sourceKey: item.driveId ?? item.key, computer: null, direction: "both", branch: ref,
    branchText: ref ?? item.branch ?? "", path: item.path ?? "", readOnly: item.readOnly, attempted: false,
  };
}

export function AddDialog(props: AddDialogProps) {
  return (
    <Dialog open={props.open !== null} onOpenChange={(o) => !o && props.onClose()}>
      {props.open && (
        <DialogContent
          className="sm:max-w-[640px]"
          aria-describedby={undefined}
          onOpenAutoFocus={(e) => {
            // Edit opens on Mount path, so focus starts in that step, not on the step list's first button.
            if (props.open?.mode !== "edit") return;
            const first = (e.currentTarget as HTMLElement).querySelector<HTMLElement>(`input#${DIALOG_PATH_ID}, #nr-mount-access, #${EDIT_SAVE_ID}`);
            if (!first) return;
            e.preventDefault();
            first.focus();
          }}
        >
          <DialogBody {...props} open={props.open} />
        </DialogContent>
      )}
    </Dialog>
  );
}

function DialogBody({ open, onClose, intent, src, items, folderReason, computerAllowed, driveReason: catalogueReason, operator, onSave, createUserDrive, reloadDrives, foldersAdded }: AddDialogProps & { open: DialogOpen }) {
  const [s, setS] = React.useState<State>(() => initial(open, intent));
  const editing = open.mode === "edit" ? open.item : undefined;
  // What an edit started from. It stays a choice even when no list carries it (a drive the catalogue does not
  // name, or the older single-drive switch with no catalogue drive behind it).
  const editedSource = editing ? (editing.driveId ?? editing.key) : undefined;
  const attachKind = s.kind === "computer" ? null : s.kind;
  const options = attachKind ? optionsFor(attachKind, src) : [];
  const source = options.find((o) => o.key === s.sourceKey);
  const choice: ItemChoice | null = attachKind && s.sourceKey
    ? { kind: attachKind, sourceKey: s.sourceKey, path: s.path.trim().replace(/\/+$/, ""), defaultPath: source?.defaultPath, branch: s.branch, readOnly: s.readOnly }
    : null;
  // What the card would be once saved: whether its path can be placed, and the path the server would give it.
  const preview = choice ? fileItems(saveChoice({ ...choice, path: choice.defaultPath ?? choice.path }, src, editing)(intent), src).find((x) => x.key === choiceKey(choice)) : undefined;
  const editable = preview?.pathEditable ?? false;
  const shownPath = editable ? s.path : preview?.path ?? source?.defaultPath ?? "";
  const error = editable || s.kind === "computer" ? pathError(s.path, items, editing?.key) : "";
  const errorId = React.useId();

  const go = (step: Step) => setS((c) => ({ ...c, step, attempted: false }));
  const choose = (o: PickOption) =>
    setS((c) => ({ ...c, sourceKey: o.key, branch: undefined, branchText: o.ref ?? "", refTyped: undefined, path: o.defaultPath ?? "", readOnly: o.locked, attempted: false }));
  // A source chosen by id from outside the list (the create form's new drive) is taken as the list's own pick
  // once its option arrives; until then it carries nothing of the source chosen before it. "" clears the pick.
  const awaited = React.useRef<string | null>(null);
  const chooseById = (id: string) => {
    awaited.current = id || null;
    setS((c) => ({ ...c, sourceKey: id || null, branch: undefined, branchText: "", path: "", readOnly: false, attempted: false }));
  };

  // A chosen source that leaves the list (the list was read again without it) is no choice: the pick and what
  // came with it go, and an edit of an item that no longer exists closes.
  const optionKeys = options.map((o) => o.key).join("\n");
  // Drives are listed from the person's drive catalogue: while it has not answered, or its last read failed, the
  // list is unknown, not empty, and nothing is judged against it.
  const listKnown = (s.kind !== "userDrive" && s.kind !== "sharedDrive") || src.catalogue !== null;
  const listed = React.useRef<readonly string[]>([]);
  const latest = React.useRef({ options, choose, editing, onClose });
  latest.current = { options, choose, editing, onClose };
  React.useEffect(() => {
    if (!listKnown) return;
    const keys = optionKeys ? optionKeys.split("\n") : [];
    const before = listed.current;
    listed.current = keys;
    const key = s.sourceKey;
    if (!key) return;
    const { options: now, choose: take, editing: edited, onClose: close } = latest.current;
    if (keys.includes(key)) {
      if (awaited.current !== key) return;
      awaited.current = null;
      const arrived = now.find((o) => o.key === key);
      if (arrived) take(arrived);
      return;
    }
    if (!before.includes(key)) return;
    if (edited) {
      close();
      return;
    }
    setS((c) => ({ ...c, sourceKey: null, branch: undefined, branchText: "", path: "", readOnly: false, attempted: false, step: c.step > 2 ? 2 : c.step, reached: c.reached > 2 ? 2 : c.reached }));
  }, [optionKeys, s.sourceKey, listKnown]);
  const save = () => {
    if (s.kind === "computer" && s.computer) {
      if (error) {
        setS((c) => ({ ...c, attempted: true }));
        document.getElementById(DIALOG_PATH_ID)?.focus();
        return;
      }
      const c = s.computer;
      onSave((i) => setLocalFolder(i, { runner_id: c.runnerId, path: c.path, name: c.name, remote_dir: s.path.trim().replace(/\/+$/, ""), direction: s.direction }),
        { name: c.name, key: COMPUTER_KEY }, "add");
      return;
    }
    if (!choice) return;
    if (error) {
      setS((c) => ({ ...c, attempted: true }));
      document.getElementById(DIALOG_PATH_ID)?.focus();
      return;
    }
    const ref = s.kind === "repo" && s.refTyped !== undefined && source?.workspaceId && source.repo
      ? { key: refTextKey(source.workspaceId, source.repo), typed: s.refTyped ? s.branch ?? "" : "" }
      : undefined;
    onSave(saveChoice(choice, src, editing, foldersAdded), { name: source?.name ?? editing?.name ?? "", key: choiceKey(choice), previousKey: editing?.key, ref }, editing ? "edit" : "add");
  };
  const title = editing ? NR_FILES.EDIT_NAMED(editing.name) : s.kind && s.step > 1 ? ADD_TITLE[s.kind] : NR_FILES.DIALOG_TITLE;
  const taken = (key: string) => items.some((x) => x.key === key && x.key !== editing?.key);

  return (
    <>
      <DialogTitle className="text-lg font-normal">{title}</DialogTitle>
      <ol aria-label={NR_FILES.STEPS_ARIA} className="mt-1 flex items-center gap-2">
        {STEPS.map(([n, label]) => {
          const active = n === s.step;
          const done = !active && n <= s.reached;
          return (
            <li key={n} className={cn("flex min-w-0 items-center gap-2", n > 1 && "flex-1 before:h-px before:flex-1 before:bg-border before:content-['']")}>
              <button
                type="button"
                disabled={n > s.reached || active || (n === 3 && !source && !editing && !s.computer)}
                aria-current={active ? "step" : undefined}
                aria-label={label}
                onClick={() => go(n)}
                className={cn("flex items-center gap-1.5 whitespace-nowrap py-0.5 text-[11px] text-muted-foreground disabled:cursor-default", done && "text-success hover:[&>b]:underline", active && "text-foreground")}
              >
                {done ? <Check className="size-3.5" aria-hidden="true" /> : <span className="tabular-nums">{n}</span>}
                <b className="font-semibold max-[520px]:hidden">{label}</b>
              </button>
            </li>
          );
        })}
      </ol>

      {s.step === 1 && (
        <ul className="flex flex-col gap-2.5">
          {/* On a remote server "Folder on this machine" is hidden rather than refused (M4). */}
          {KIND_ORDER.filter((kind) => !(kind === "folder" && folderReason)).map((kind) => {
            const reason = kind === "computer" && computerAllowed === false ? NR_FILES.COMPUTER_BLOCKED : undefined;
            const info = KIND_INFO[kind];
            const reasonId = `nr-kind-reason-${kind}`;
            return (
              <li key={kind} className="flex">
                <button
                  type="button"
                  disabled={!!reason}
                  aria-describedby={reason ? reasonId : undefined}
                  onClick={() => setS((c) => ({
                    ...c, kind, step: 2, reached: c.kind === kind ? c.reached : 2,
                    sourceKey: c.kind === kind ? c.sourceKey : null, path: c.kind === kind ? c.path : "", attempted: false,
                  }))}
                  className={cn(
                    "flex w-full items-center gap-3.5 rounded-[10px] border border-border bg-card px-3.5 py-[13px] text-left transition-colors hover:border-border-strong max-[520px]:gap-2.5 max-[520px]:p-3",
                    s.kind === kind && "border-primary bg-primary/10",
                    reason && "cursor-not-allowed hatch-unavailable hover:border-border",
                  )}
                >
                  <KindTile kind={kind} />
                  <span className="flex min-w-0 flex-1 flex-col gap-0.5">
                    <span className="flex flex-wrap items-center gap-2">
                      <strong className="text-sm font-semibold">{info.title}</strong>
                    </span>
                    <span className="text-[13px] text-muted-foreground">{info.body}</span>
                    {info.sub && <small className="text-xs text-muted-foreground">{info.sub}</small>}
                    {reason && (
                      <small id={reasonId} className="flex items-start gap-1.5 text-xs text-muted-foreground">
                        <Lock className="mt-px size-3 flex-none" aria-hidden="true" />
                        {reason}
                      </small>
                    )}
                  </span>
                  {!reason && (
                    <span aria-hidden="true" className="grid size-7 place-items-center rounded-md bg-muted text-muted-foreground max-[520px]:hidden">
                      <ChevronRight className="size-4" />
                    </span>
                  )}
                </button>
              </li>
            );
          })}
        </ul>
      )}

      {s.step === 2 && s.kind === "computer" && (
        <ComputerChoose
          value={s.computer}
          onChange={(computer) => setS((c) => ({ ...c, computer, path: c.path || COMPUTER_DEFAULT_DIR, attempted: false }))}
          onNotNow={() => go(1)}
        />
      )}

      {s.step === 3 && s.kind === "computer" && s.computer && (
        <ComputerMount
          choice={s.computer}
          path={s.path}
          onPath={(path) => setS((c) => ({ ...c, path }))}
          direction={s.direction}
          onDirection={(direction) => setS((c) => ({ ...c, direction }))}
          error={s.attempted ? error : undefined}
          errorId={errorId}
          inputId={DIALOG_PATH_ID}
        />
      )}

      {s.step === 2 && attachKind && (
        <div className="flex flex-col">
          <p className="mb-3 text-sm text-muted-foreground">{KIND_INFO[attachKind].body}</p>
          {(s.kind === "userDrive" || s.kind === "sharedDrive") && catalogueReason && (
            <p data-testid="nr-drive-reason" className="mb-3 text-sm text-muted-foreground">{catalogueReason}</p>
          )}
          {s.kind === "folder" && options.length === 0 && <p className="text-sm text-muted-foreground">{NR_FILES.FOLDERS_NONE}</p>}
          {options.length > 0 && (
            <div role="radiogroup" aria-label={KIND_INFO[attachKind].title} className="grid overflow-hidden rounded-[9px] border border-border">
              {options.map((o) => {
                const reason = o.reason ?? (taken(choiceKey({ kind: attachKind, sourceKey: o.key })) ? NR_FILES.ALREADY_ADDED : undefined);
                const selected = s.sourceKey === o.key;
                return (
                  <label
                    key={o.key}
                    className={cn(
                      "grid grid-cols-[18px_minmax(0,1fr)] items-start gap-2.5 border-b border-border px-[13px] py-[11px] text-sm last:border-b-0",
                      selected && "bg-primary/10",
                      reason ? "cursor-not-allowed text-muted-foreground" : "cursor-pointer",
                    )}
                  >
                    <input type="radio" name="nr-pick-source" className="mt-0.5 size-4 accent-primary" checked={selected} disabled={!!reason} onChange={() => choose(o)} />
                    <span className="grid min-w-0 gap-1">
                      <span className="flex flex-wrap items-center gap-1.5">
                        <strong className="font-semibold">{o.name}</strong>
                        {o.chips.map((c) => <Chip key={c}>{c}</Chip>)}
                        {o.kept?.unenforced && (
                          <>
                            <Chip tone="warning">{NR_FILES.DRIVE_SIZE_NOT_ENFORCED}</Chip>
                            <InfoTip label={NR_FILES.DRIVE_SIZE_NOT_ENFORCED}>{NR_FILES.DRIVE_SIZE_NOT_ENFORCED_TIP}</InfoTip>
                          </>
                        )}
                      </span>
                      {o.kept && <span className="text-xs text-muted-foreground">{o.kept.line}</span>}
                      {o.shownPath && <PathText path={o.shownPath} className="text-muted-foreground" />}
                      {reason && (
                        <span className="flex items-start gap-1.5 text-[13px]">
                          <Lock className="mt-0.5 size-3 flex-none" aria-hidden="true" />
                          {reason}
                        </span>
                      )}
                    </span>
                  </label>
                );
              })}
            </div>
          )}
          {s.kind === "repo" && source?.workspaceId && (
            <div className="mt-3.5 flex flex-col items-start gap-1.5">
              <span className={FIELD_LABEL}>{NR_FILES.BRANCH_LABEL}</span>
              <BranchPicker
                workspaceId={source.workspaceId}
                repo={source.repo!}
                name={source.name}
                included
                sourceRef={source.ref}
                value={s.branch}
                typed={s.branchText}
                onPick={(ref) => setS((c) => ({ ...c, branch: ref, refTyped: false }))}
                onType={(text) => setS((c) => ({ ...c, branchText: text, branch: text.trim() || undefined, refTyped: true }))}
              />
            </div>
          )}
          <div className="mt-3 flex flex-wrap items-center gap-x-4 gap-y-2 text-sm">
            {s.kind === "repo" && <Link to="/workspaces" className={FILES_LINK}>{NR_FILES.ADD_REPOSITORY}</Link>}
            {s.kind === "userDrive" && createUserDrive?.({ choose: chooseById, reload: reloadDrives })}
            {s.kind === "sharedDrive" && (
              <>
                <span className="inline-flex items-center gap-2">
                  <button type="button" disabled className="font-semibold text-muted-foreground opacity-70 disabled:cursor-not-allowed">{NR_FILES.CREATE_SHARED_DRIVE}</button>
                  <Chip>{NR_SHARED.COMING_SOON}</Chip>
                </span>
                {operator && <Link to="/admin/drives" className={FILES_LINK}>{NR_FILES.MANAGE_DRIVES}</Link>}
              </>
            )}
          </div>
        </div>
      )}

      {s.step === 3 && attachKind && (source || editing) && (
        <div className="grid gap-4">
          <div className="flex flex-wrap items-center gap-2.5">
            <KindTile kind={attachKind} />
            <strong className="text-[15px] font-semibold">{source?.name ?? editing?.name}</strong>
            <Chip>{KIND_LABEL[attachKind]}</Chip>
            {attachKind === "repo" && <code className="font-mono text-[13px] text-muted-foreground">{s.branch ?? source?.ref ?? editing?.branch ?? NR_FILES.BRANCH_PLACEHOLDER}</code>}
          </div>
          <div className="flex flex-col gap-1.5">
            <label htmlFor={DIALOG_PATH_ID} className={FIELD_LABEL}>{NR_FILES.STEP_MOUNT}</label>
            {editable ? (
              <Input
                id={DIALOG_PATH_ID}
                className="h-[42px] font-mono text-[13px]"
                value={s.path}
                aria-invalid={(s.attempted && !!error) || undefined}
                aria-describedby={errorId}
                onChange={(e) => setS((c) => ({ ...c, path: e.target.value }))}
              />
            ) : (
              <output id={DIALOG_PATH_ID} className="flex min-h-[42px] items-center rounded-md border border-border bg-muted px-3">
                <PathText path={shownPath} />
              </output>
            )}
            {s.attempted && error ? (
              <p id={errorId} className="flex items-center gap-1.5 text-xs text-danger"><TriangleAlert className="size-3.5 flex-none" aria-hidden="true" />{error}</p>
            ) : (
              <small id={errorId} className="text-xs text-muted-foreground">{NR_FILES.MOUNT_HINT}</small>
            )}
          </div>
          {attachKind !== "repo" && (
            <div className="mt-4 flex flex-col gap-1.5">
              <label htmlFor="nr-mount-access" className={FIELD_LABEL}>{NR_FILES.ACCESS_LABEL}</label>
              {source?.locked ?? editing?.locked ? (
                attachKind === "folder" ? <span className="flex min-h-[42px] items-center text-sm">{NR_FILES.ACCESS_READ}</span> : <LockedAccess id="nr-dialog" />
              ) : (
                <select id="nr-mount-access" className={SELECT_CLASS} value={s.readOnly ? "read" : "edit"} onChange={(e) => setS((c) => ({ ...c, readOnly: e.target.value === "read" }))}>
                  <option value="edit">{NR_FILES.ACCESS_EDIT}</option>
                  <option value="read">{NR_FILES.ACCESS_READ}</option>
                </select>
              )}
            </div>
          )}
        </div>
      )}

      <div className="mt-1 flex flex-wrap items-center justify-between gap-2">
        {s.step > 1 ? <Button type="button" variant="ghost" onClick={() => go((s.step - 1) as Step)}>{NR_SHARED.BACK}</Button> : <span />}
        <div className="flex gap-2">
          <Button type="button" variant="outline" onClick={onClose}>{NR_SHARED.CANCEL}</Button>
          {s.step === 2 && s.kind === "computer" && (
            <Button type="button" disabled={!s.computer} onClick={() => setS((c) => ({ ...c, step: 3, reached: 3 }))}>{NR_FILES.COMPUTER_NEXT}</Button>
          )}
          {s.step === 2 && s.kind !== "computer" && (
            <Button type="button" disabled={!source && !(editing && s.sourceKey === editedSource)} onClick={() => setS((c) => ({ ...c, step: 3, reached: 3 }))}>
              {NR_SHARED.CONTINUE} <span aria-hidden="true">→</span>
            </Button>
          )}
          {s.step === 3 && <Button id={EDIT_SAVE_ID} type="button" onClick={save}>{editing ? NR_FILES.SAVE : NR_FILES.ADD_CONFIRM}</Button>}
        </div>
      </div>
    </>
  );
}
