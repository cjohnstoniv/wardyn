/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// "Your computer" (M4, #2982, owner-approved 2026-10-19): a folder on the person's computer for a sandbox
// elsewhere, browsed through their own folder-only runner (`wardyn-runnerd serve --substrate folders`).
// The Choose step lists the runner's folders, the Mount path step places the folder and says which way
// changes go, and the Files card shows what the sandbox gets. The run's `local_folder` carries the choice;
// Wardyn copies the folder in before the agent starts and, Both ways, brings changes back when the run ends.

import * as React from "react";
import { ChevronRight, Lock, TriangleAlert } from "lucide-react";
import { Link } from "react-router-dom";
import { Button } from "../../../../ui/button";
import { cn } from "../../../../ui/utils";
import { Chip } from "../../../../wardyn/primitives";
import { NR_FILES } from "../../../../wardyn/copy/new-run/files";
import { NR_SHARED } from "../../../../wardyn/copy/new-run/shared";
import { computerFolders, COMPUTER_FOLDERS_OFF, COMPUTER_RUNNER_OFFLINE, type FolderEntry, type FolderSurvey } from "../../../../../lib/api/computer-folders";
import { HttpError } from "../../../../../lib/api/core";
import { runners as runnersApi } from "../../../../../lib/api/runners";
import { setup as setupApi } from "../../../../../lib/api/setup";
import { fmtBytes, relativeTime } from "../../../../../lib/format";
import type { RunnerView } from "../../../../../lib/types/runners";
import type { RunLocalFolderInput } from "../../../../../lib/types/run-create";
import type { RunIntent } from "../../draft/run-intent";
import { withKeys } from "../../lenses/lens-keys";
import { KindTile, PathText } from "./kind-visuals";

/** Where the folder lands when the person names no other place (internal/api sshSyncDefaultDir). */
export const COMPUTER_DEFAULT_DIR = "/home/agent/work";

/** The folder the Choose step picked. */
export interface ComputerChoice {
  runnerId: string;
  runner: string;
  path: string;
  name: string;
}

const count = (n: number) => n.toLocaleString("en");
const FIELD_LABEL = "text-xs font-semibold";
const baseName = (path: string) => path.replace(/\/+$/, "").split("/").pop() || path;

/** WARDYN_COMPUTER_FOLDERS as /setup/status says it: undefined until it answers, false on a failed read. */
export function useComputerFoldersAllowed(): boolean | undefined {
  const [allowed, setAllowed] = React.useState<boolean>();
  React.useEffect(() => {
    let alive = true;
    setupApi.getSetupStatus()
      .then((s) => alive && setAllowed(s.runner.computer_folders === true))
      .catch(() => alive && setAllowed(false));
    return () => {
      alive = false;
    };
  }, []);
  return allowed;
}

/** The person's own claimed runners that share folders; null while loading. */
function useFolderRunners(): { list: RunnerView[] | null; error?: string } {
  const [state, setState] = React.useState<{ list: RunnerView[] | null; error?: string }>({ list: null });
  React.useEffect(() => {
    let alive = true;
    runnersApi.listMine()
      .then((all) => alive && setState({ list: all.filter((r) => r.state === "claimed" && r.folders === true) }))
      .catch((e: unknown) => alive && setState({ list: [], error: e instanceof Error ? e.message : String(e) }));
    return () => {
      alive = false;
    };
  }, []);
  return state;
}

/** Sets the run's computer folder. */
export function setLocalFolder(intent: RunIntent, folder: RunLocalFolderInput): RunIntent {
  return withKeys(intent, { local_folder: folder });
}

/** Clears the run's computer folder. */
export function clearLocalFolder(intent: RunIntent): RunIntent {
  const next = { ...intent };
  delete next.local_folder;
  return next;
}

function Notice({ title, body, tone = "muted", children }: { title: string; body?: string; tone?: "muted" | "danger"; children?: React.ReactNode }) {
  return (
    <div role={tone === "danger" ? "alert" : "status"} className={cn("flex flex-col gap-1.5 rounded-[10px] border px-3.5 py-3", tone === "danger" ? "border-danger bg-danger-subtle" : "border-border bg-muted")}>
      <strong className="flex items-center gap-1.5 text-sm font-semibold">
        {tone === "danger" && <TriangleAlert className="size-3.5 flex-none text-danger" aria-hidden="true" />}
        {title}
      </strong>
      {body && <p className="text-[13px] text-muted-foreground">{body}</p>}
      {children}
    </div>
  );
}

/**
 * The Choose step for "Your computer": the runner, its folders (names, file counts and sizes only) and
 * what the sandbox would see of the one picked.
 */
export function ComputerChoose({ value, onChange, onNotNow }: {
  value: ComputerChoice | null;
  onChange: (choice: ComputerChoice | null) => void;
  onNotNow: () => void;
}) {
  const { list: folderRunners, error: runnersError } = useFolderRunners();
  const online = (folderRunners ?? []).filter((r) => r.online);
  const [runnerId, setRunnerId] = React.useState<string | null>(value?.runnerId ?? null);
  const runner = online.find((r) => r.id === runnerId) ?? online[0];
  const [dir, setDir] = React.useState("");
  const [roots, setRoots] = React.useState<string[]>([]);
  const [listing, setListing] = React.useState<{ dir: string; folders: FolderEntry[] } | null>(null);
  const [failure, setFailure] = React.useState<{ reason: string; message: string } | null>(null);
  const [survey, setSurvey] = React.useState<{ path: string; survey?: FolderSurvey; error?: string } | null>(null);
  const runnerKey = runner?.id ?? "";

  React.useEffect(() => {
    if (!runnerKey) return;
    let alive = true;
    setFailure(null);
    computerFolders.list(runnerKey, dir)
      .then((l) => {
        if (!alive) return;
        setListing({ dir, folders: l.folders });
        if (!dir) setRoots(l.folders.map((f) => f.path));
      })
      .catch((e: unknown) => alive && setFailure({ reason: e instanceof HttpError ? e.reason : "", message: e instanceof Error ? e.message : String(e) }));
    return () => {
      alive = false;
    };
  }, [runnerKey, dir]);

  const picked = value && value.runnerId === runnerKey ? value.path : "";
  React.useEffect(() => {
    if (!runnerKey || !picked) return;
    let alive = true;
    computerFolders.list(runnerKey, picked, true)
      .then((l) => alive && setSurvey({ path: picked, survey: l.survey }))
      .catch((e: unknown) => alive && setSurvey({ path: picked, error: e instanceof Error ? e.message : String(e) }));
    return () => {
      alive = false;
    };
  }, [runnerKey, picked]);

  if (folderRunners === null) return <p className="text-sm text-muted-foreground" aria-busy="true">…</p>;
  if (folderRunners.length === 0) {
    return (
      <Notice title={NR_FILES.COMPUTER_NO_RUNNER} body={NR_FILES.COMPUTER_NO_RUNNER_BODY}>
        {runnersError && <p className="text-[13px] text-muted-foreground">{runnersError}</p>}
        <span className="flex flex-wrap items-center gap-3 pt-1">
          <Link to="/account" className="text-sm font-semibold text-primary hover:underline">{NR_FILES.COMPUTER_SET_UP}</Link>
          <Button type="button" variant="ghost" size="sm" onClick={onNotNow}>{NR_FILES.COMPUTER_NOT_NOW}</Button>
        </span>
      </Notice>
    );
  }
  const offline = !runner || failure?.reason === COMPUTER_RUNNER_OFFLINE;
  if (offline) {
    const away = runner ?? folderRunners[0];
    return (
      <Notice tone="danger" title={NR_FILES.COMPUTER_OFFLINE} body={NR_FILES.COMPUTER_OFFLINE_BODY(away.name)}>
        <p className="text-[13px] text-muted-foreground">
          {NR_FILES.COMPUTER_LAST_SEEN(away.name, away.last_seen_at ? relativeTime(away.last_seen_at) : "—")}
        </p>
      </Notice>
    );
  }
  if (failure?.reason === COMPUTER_FOLDERS_OFF) return <Notice title={NR_FILES.COMPUTER_BLOCKED} />;

  const root = roots.find((r) => dir === r || dir.startsWith(`${r}/`));
  const crumbs = root ? [root, ...dir.slice(root.length).split("/").filter(Boolean).map((_, i, all) => `${root}/${all.slice(0, i + 1).join("/")}`)] : [];
  const pick = (f: FolderEntry) => onChange({ runnerId: runner.id, runner: runner.name, path: f.path, name: f.name });
  const seen = survey && survey.path === picked ? survey : null;

  return (
    <div className="flex flex-col gap-3">
      <div className="flex flex-wrap items-center gap-2 text-sm">
        <span className={FIELD_LABEL}>{NR_FILES.COMPUTER_LABEL}</span>
        {online.length > 1 ? (
          <select
            aria-label={NR_FILES.COMPUTER_LABEL}
            className="h-8 rounded-md border border-border-strong bg-input-background px-2 text-sm"
            value={runner.id}
            onChange={(e) => {
              setRunnerId(e.target.value);
              setDir("");
              onChange(null);
            }}
          >
            {online.map((r) => <option key={r.id} value={r.id}>{r.name}</option>)}
          </select>
        ) : (
          <strong className="font-semibold">{runner.name}</strong>
        )}
        <Chip>{NR_FILES.COMPUTER_ONLINE}</Chip>
      </div>
      <nav aria-label={NR_FILES.START_FOLDER_LABEL} className="flex flex-wrap items-center gap-1 text-[13px]">
        <span className={FIELD_LABEL}>{NR_FILES.START_FOLDER_LABEL}</span>
        {crumbs.map((c, i) => (
          <React.Fragment key={c}>
            {i > 0 && <ChevronRight className="size-3 text-muted-foreground" aria-hidden="true" />}
            <button type="button" className="font-mono text-primary hover:underline disabled:text-foreground disabled:no-underline" disabled={c === dir} onClick={() => setDir(c)}>
              {i === 0 ? c : baseName(c)}
            </button>
          </React.Fragment>
        ))}
      </nav>
      {failure && failure.reason !== COMPUTER_RUNNER_OFFLINE && <p role="alert" className="text-sm text-danger">{failure.message}</p>}
      <div role="radiogroup" aria-label={NR_FILES.COMPUTER_TITLE} className="overflow-hidden rounded-[9px] border border-border">
        <div className="grid grid-cols-[18px_minmax(0,1fr)_72px_80px_28px] gap-2.5 border-b border-border bg-muted px-[13px] py-1.5 text-xs font-semibold text-muted-foreground" aria-hidden="true">
          <span />
          <span>{NR_FILES.COMPUTER_COL_NAME}</span>
          <span className="text-right">{NR_FILES.COMPUTER_COL_FILES}</span>
          <span className="text-right">{NR_FILES.COMPUTER_COL_SIZE}</span>
          <span />
        </div>
        {(listing?.dir === dir ? listing.folders : []).map((f) => (
          <div key={f.path} className={cn("grid grid-cols-[18px_minmax(0,1fr)_72px_80px_28px] items-center gap-2.5 border-b border-border px-[13px] py-2 text-sm last:border-b-0", picked === f.path && "bg-primary/10")}>
            {/* A shared root is opened, never sent whole: the runner refuses it (folders.go folder). */}
            {roots.includes(f.path) ? <span aria-hidden="true" /> : (
              <input type="radio" name="nr-computer-folder" aria-label={f.name} className="size-4 accent-primary" checked={picked === f.path} onChange={() => pick(f)} />
            )}
            <span className="truncate font-semibold">{f.name}</span>
            <span className="text-right tabular-nums text-muted-foreground">{count(f.files)}{f.more ? "+" : ""}</span>
            <span className="text-right tabular-nums text-muted-foreground">{fmtBytes(f.bytes)}{f.more ? "+" : ""}</span>
            <button type="button" aria-label={NR_FILES.COMPUTER_OPEN(f.name)} className="grid size-7 place-items-center rounded-md text-muted-foreground hover:bg-muted" onClick={() => setDir(f.path)}>
              <ChevronRight className="size-4" aria-hidden="true" />
            </button>
          </div>
        ))}
      </div>
      {picked && (
        <div className="flex flex-col gap-1 rounded-[10px] border border-border px-3.5 py-3 text-sm" data-testid="nr-computer-will-see">
          <span className={FIELD_LABEL}>{NR_FILES.COMPUTER_WILL_SEE}</span>
          {seen?.survey ? (
            <>
              <span>{NR_FILES.COMPUTER_COUNT(count(seen.survey.files), fmtBytes(seen.survey.bytes))}</span>
              <LeftOut survey={seen.survey} />
            </>
          ) : seen?.error ? (
            <span role="alert" className="text-danger">{seen.error}</span>
          ) : (
            <span aria-busy="true" className="text-muted-foreground">…</span>
          )}
          <label className="mt-1 flex items-center gap-2 text-[13px] text-muted-foreground">
            {/* `wardyn sync`'s rules always leave out what .gitignore lists, so this is shown, never switched off. */}
            <input type="checkbox" checked disabled className="size-4 accent-primary" />
            {NR_FILES.COMPUTER_GITIGNORE}
          </label>
        </div>
      )}
      <p className="flex items-start gap-1.5 text-xs text-muted-foreground">
        <Lock className="mt-px size-3 flex-none" aria-hidden="true" />
        {NR_FILES.COMPUTER_BROWSE_NOTE}
      </p>
    </div>
  );
}

function LeftOut({ survey }: { survey: FolderSurvey }) {
  const names = [...(survey.left_out ?? []), ...(survey.ignored > 0 ? [NR_FILES.COMPUTER_IGNORED(count(survey.ignored))] : [])];
  if (!names.length) return null;
  const text = names.length === 1 ? names[0] : NR_FILES.LIST_AND(names.slice(0, -1).join(", "), names[names.length - 1]);
  return (
    <span className="text-muted-foreground">
      {NR_FILES.COMPUTER_LEFT_OUT}: {text}
    </span>
  );
}

/** The Mount path step for "Your computer": where it lands and which way changes go. */
export function ComputerMount({ choice, path, onPath, direction, onDirection, error, errorId, inputId }: {
  choice: ComputerChoice;
  path: string;
  onPath: (path: string) => void;
  direction: "both" | "in";
  onDirection: (d: "both" | "in") => void;
  error?: string;
  errorId: string;
  inputId: string;
}) {
  return (
    <div className="grid gap-4">
      <div className="flex flex-wrap items-center gap-2.5">
        <KindTile kind="computer" />
        <strong className="text-[15px] font-semibold">{choice.name}</strong>
        <Chip>{NR_FILES.COMPUTER_TITLE}</Chip>
        <span className="text-[13px] text-muted-foreground">{NR_FILES.COMPUTER_ON(choice.runner)}</span>
      </div>
      <div className="flex flex-col gap-1.5">
        <label htmlFor={inputId} className={FIELD_LABEL}>{NR_FILES.STEP_MOUNT}</label>
        <input
          id={inputId}
          className="h-[42px] rounded-md border border-border-strong bg-input-background px-3 font-mono text-[13px]"
          value={path}
          aria-invalid={!!error || undefined}
          aria-describedby={errorId}
          onChange={(e) => onPath(e.target.value)}
        />
        {error ? (
          <p id={errorId} className="flex items-center gap-1.5 text-xs text-danger"><TriangleAlert className="size-3.5 flex-none" aria-hidden="true" />{error}</p>
        ) : (
          <small id={errorId} className="text-xs text-muted-foreground">{NR_FILES.MOUNT_HINT}</small>
        )}
      </div>
      <Changes direction={direction} onDirection={onDirection} name="nr-computer-changes-dialog" />
    </div>
  );
}

function Changes({ direction, onDirection, name }: { direction: "both" | "in"; onDirection: (d: "both" | "in") => void; name: string }) {
  return (
    <fieldset className="flex flex-col gap-1.5">
      <legend className={cn(FIELD_LABEL, "mb-1.5")}>{NR_FILES.COMPUTER_CHANGES}</legend>
      {([["both", NR_FILES.COMPUTER_BOTH, NR_FILES.COMPUTER_BOTH_BODY], ["in", NR_FILES.COMPUTER_COPY_IN, NR_FILES.COMPUTER_COPY_IN_BODY]] as const).map(([value, label, body]) => (
        <label key={value} className={cn("flex items-start gap-2.5 rounded-[9px] border border-border px-3 py-2 text-sm", direction === value && "border-primary bg-primary/10")}>
          <input type="radio" name={name} className="mt-0.5 size-4 accent-primary" checked={direction === value} onChange={() => onDirection(value)} />
          <span className="flex flex-col gap-0.5">
            <strong className="font-semibold">{label}</strong>
            <span className="text-[13px] text-muted-foreground">{body}</span>
          </span>
        </label>
      ))}
    </fieldset>
  );
}

/** The Files list's card for the run's computer folder. */
export function ComputerFolderCard({ folder, onDirection, onChange, onRemove }: {
  folder: RunLocalFolderInput;
  onDirection: (d: "both" | "in") => void;
  onChange: () => void;
  onRemove: () => void;
}) {
  const { list } = useFolderRunners();
  const runner = list?.find((r) => r.id === folder.runner_id);
  const name = folder.name || baseName(folder.path ?? "");
  const [survey, setSurvey] = React.useState<FolderSurvey>();
  React.useEffect(() => {
    if (!folder.runner_id || !folder.path || !runner?.online) return;
    let alive = true;
    computerFolders.list(folder.runner_id, folder.path, true).then((l) => alive && setSurvey(l.survey)).catch(() => undefined);
    return () => {
      alive = false;
    };
  }, [folder.runner_id, folder.path, runner?.online]);
  return (
    <div className="flex flex-col gap-3 border-t border-border px-[15px] py-3.5 first:border-t-0" data-testid="nr-computer-card">
      <div className="flex flex-wrap items-center gap-2.5">
        <KindTile kind="computer" />
        <strong className="text-sm font-semibold">{name}</strong>
        <Chip>{NR_FILES.COMPUTER_TITLE}</Chip>
        <PathText path={folder.remote_dir || COMPUTER_DEFAULT_DIR} className="text-muted-foreground" />
        <span className="ml-auto flex gap-1">
          <Button type="button" variant="ghost" size="sm" className="px-2.5 text-xs font-semibold text-muted-foreground" onClick={onChange}>{NR_FILES.COMPUTER_CHANGE}</Button>
          <Button type="button" variant="ghost" size="sm" className="px-2.5 text-xs font-semibold text-muted-foreground" aria-label={NR_FILES.REMOVE_NAMED(name)} onClick={onRemove}>{NR_SHARED.REMOVE}</Button>
        </span>
      </div>
      <dl className="grid grid-cols-[minmax(0,140px)_minmax(0,1fr)] gap-x-4 gap-y-2 text-sm">
        <dt className="font-medium text-muted-foreground">{NR_FILES.COMPUTER_FROM}</dt>
        <dd className="flex flex-wrap items-center gap-2">
          <span>{NR_FILES.COMPUTER_ON(runner?.name ?? "")}</span>
          {runner?.online ? <Chip>{NR_FILES.COMPUTER_ONLINE}</Chip> : runner && <Chip>{NR_FILES.COMPUTER_OFFLINE}</Chip>}
        </dd>
        <dt className="font-medium text-muted-foreground">{NR_FILES.COMPUTER_SEES}</dt>
        <dd>
          {survey ? NR_FILES.COMPUTER_COUNT(count(survey.files), fmtBytes(survey.bytes)) : "—"}
          {survey && <span className="block"><LeftOut survey={survey} /></span>}
        </dd>
        <dt className="font-medium text-muted-foreground">{NR_FILES.COMPUTER_FIRST_SYNC}</dt>
        <dd className="text-muted-foreground">{NR_FILES.COMPUTER_FIRST_SYNC_BODY}</dd>
      </dl>
      <Changes direction={folder.direction ?? "in"} onDirection={onDirection} name="nr-computer-changes" />
    </div>
  );
}
