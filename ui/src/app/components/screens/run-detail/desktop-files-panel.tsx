/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The run's Files panel (mock M7-Files): files move between this browser and ~/Transfers in the
// desktop under the run's desktop rules, and the clipboard strip says which way the desktop relay
// lets the clipboard through (M7-Phone). The server decides every rule; this panel only shows it.
import * as React from "react";
import { RotateCw } from "lucide-react";
import { desktopFiles, type DesktopFilesView } from "../../../lib/api/desktop-files";
import { approvals as approvalsApi } from "../../../lib/api/approvals";
import { HttpError } from "../../../lib/api/core";
import { shortTime } from "../../../lib/format";
import type { ApprovalRequest, FileDownloadScope } from "../../../lib/types/approvals";
import { usePoll } from "../../../lib/use-poll";
import { Button } from "../../ui/button";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "../../ui/tabs";
import { Mono } from "../../wardyn/code-block";
import { DESKTOP_FILES as C } from "../../wardyn/copy";
import { DATA_OUT_RUN } from "../../wardyn/copy/data-out-run";
import { DesktopReviewRow } from "./desktop-review-row";
import { useRunDataOut } from "./use-run-data-out";

type UploadRow = {
  key: number;
  name: string;
  bytes: number;
  state: "sending" | "delivered" | "too_large" | "failed";
  line?: string;
};

type DownloadMark = { line: string } | { error: string };

// Sizes as the board writes them: whole KB, one decimal for MB and GB.
export function transferSize(n: number): string {
  if (n < 1024) return `${n} B`;
  if (n < 1024 ** 2) return `${Math.round(n / 1024)} KB`;
  if (n < 1024 ** 3) return `${(n / 1024 ** 2).toFixed(1)} MB`;
  return `${(n / 1024 ** 3).toFixed(1)} GB`;
}

// A paused run's folder cannot be read. A look never wakes it, so the panel looks again until it has woken.
const PAUSED_POLL_MS = 4000;
// Under review a decider answers the requests without this browser: read them while the From tab is open.
export const REVIEW_POLL_MS = 5000;

const clock = () => shortTime(new Date().toISOString());
const message = (e: unknown) => (e instanceof Error ? e.message : String(e));

function Blocked({ title, why }: { title: string; why?: string }) {
  return (
    <div className="mt-2 rounded-lg border border-warning/30 bg-warning-subtle px-3 py-2.5">
      <p className="text-xs font-medium text-foreground">{title}</p>
      <p className="mt-0.5 text-meta text-muted-foreground">{C.refused}</p>
      {why && <p className="mt-0.5 text-meta text-muted-foreground">{why}</p>}
    </div>
  );
}

// noDesktop: the run has no desktop (a plain sandbox), so only the reviewed way out is left: no clipboard strip,
// no To tab, and the folder is "in the run".
export function DesktopFilesPanel({ runId, vm = false, noDesktop = false }: { runId: string; vm?: boolean; noDesktop?: boolean }) {
  // The run's data_out rule; unknown (undefined) draws nothing new.
  const dataOut = useRunDataOut(runId);
  const [view, setView] = React.useState<DesktopFilesView | null>(null);
  const [loadError, setLoadError] = React.useState<{ text: string; retry: boolean } | null>(null);
  const [paused, setPaused] = React.useState<string | null>(null);
  const [uploads, setUploads] = React.useState<UploadRow[]>([]);
  const [marks, setMarks] = React.useState<Record<string, DownloadMark>>({});
  // Files whose download is in flight: a second click must not start a second transfer.
  const [fetching, setFetching] = React.useState<ReadonlySet<string>>(new Set());
  // This run's file_download requests, newest first. null until read; `requestsFailed` when the read was refused.
  const [requests, setRequests] = React.useState<ApprovalRequest[] | null>(null);
  const [requestsFailed, setRequestsFailed] = React.useState(false);
  const nextKey = React.useRef(0);
  const input = React.useRef<HTMLInputElement>(null);

  const loadRequests = React.useCallback(async () => {
    try {
      const rows = await approvalsApi.listApprovals("", runId);
      setRequests(rows.filter((r) => r.kind === "file_download").sort((a, b) => b.requested_at.localeCompare(a.requested_at)));
      setRequestsFailed(false);
    } catch {
      setRequestsFailed(true);
    }
  }, [runId]);

  const load = React.useCallback(async () => {
    try {
      const next = await desktopFiles.list(runId);
      setView(next);
      setLoadError(null);
      setPaused(null);
      if (next.rules.download === "review") void loadRequests();
    } catch (e) {
      if (e instanceof HttpError && e.status === 409 && e.reason === "run_inspect_paused") {
        // Not a failure: the rules already on screen still hold, and an upload they allow wakes the run.
        setPaused(e.message);
        setLoadError(null);
      } else {
        // A dropped connection or a 5xx may pass on its own, so it gets a plain sentence and a retry; a refusal
        // the server worded (4xx) is printed as it is and is not retried, because asking again gets the same answer.
        const unreadable = e instanceof HttpError && e.reason === "data_out_unreadable";
        const transient = !(e instanceof HttpError) || e.status >= 500;
        setLoadError({ text: transient && !unreadable ? C.loadFailed : message(e), retry: transient || unreadable });
        setPaused(null);
      }
    }
  }, [runId, loadRequests]);
  React.useEffect(() => {
    void load();
  }, [load]);
  usePoll(load, PAUSED_POLL_MS, paused === null);
  const [tab, setTab] = React.useState("to");
  usePoll(loadRequests, REVIEW_POLL_MS, !(view?.rules.download === "review" && (tab === "from" || noDesktop)));

  const patch = (key: number, next: Partial<UploadRow>) =>
    setUploads((rows) => rows.map((r) => (r.key === key ? { ...r, ...next } : r)));

  async function send(files: FileList | null) {
    for (const file of Array.from(files ?? [])) {
      const key = nextKey.current++;
      setUploads((rows) => [...rows, { key, name: file.name, bytes: file.size, state: "sending" }]);
      try {
        const done = await desktopFiles.upload(runId, file);
        patch(key, { state: "delivered", line: C.hashLine(done.sha256, clock()) });
      } catch (e) {
        if (e instanceof HttpError && e.status === 413) patch(key, { state: "too_large", line: C.tooLargeSub });
        else patch(key, { state: "failed", line: message(e) });
      }
    }
    if (input.current) input.current.value = "";
  }

  async function fetchFile(name: string) {
    setFetching((f) => new Set(f).add(name));
    try {
      const { blob, sha256 } = await desktopFiles.download(runId, name);
      const url = URL.createObjectURL(blob);
      const a = document.createElement("a");
      a.href = url;
      a.download = name;
      a.click();
      URL.revokeObjectURL(url);
      setMarks((m) => ({ ...m, [name]: { line: C.downloaded(clock(), sha256) } }));
    } catch (e) {
      setMarks((m) => ({ ...m, [name]: { error: message(e) } }));
    } finally {
      setFetching((f) => {
        const next = new Set(f);
        next.delete(name);
        return next;
      });
    }
  }

  if (loadError) {
    return (
      <section aria-label={C.title} data-testid="desktop-files" className="mt-4">
        <p className="text-xs font-medium text-foreground">{C.title}</p>
        <p className="mt-1 font-mono text-meta text-muted-foreground">{loadError.text}</p>
        {loadError.retry && (
          <Button size="sm" variant="outline" className="mt-1.5" onClick={() => void load()}>
            <RotateCw aria-hidden="true" />
            {DATA_OUT_RUN.RETRY}
          </Button>
        )}
      </section>
    );
  }
  if (!view) {
    if (paused === null) return null;
    return (
      <section aria-label={C.title} data-testid="desktop-files" className="mt-4">
        <p className="text-xs font-medium text-foreground">{C.title}</p>
        <p data-testid="desktop-files-paused" className="mt-1 text-meta text-muted-foreground">
          {paused}
        </p>
      </section>
    );
  }
  const { rules } = view;
  const status = { sending: C.sending, delivered: C.delivered, too_large: C.tooLarge, failed: C.notSent };

  return (
    <section aria-label={C.title} data-testid="desktop-files" className="mt-4">
      <p className="text-xs font-medium text-foreground">{C.title}</p>
      {noDesktop ? null : vm ? (
        <p data-testid="desktop-files-no-clipboard" className="mt-1.5 text-meta text-muted-foreground">{C.clipboardVM}</p>
      ) : (
        <dl className="mt-1.5 flex flex-wrap gap-x-4 gap-y-1 text-meta">
          {([[C.pasteIn, rules.paste_in], [C.copyOut, rules.copy_out]] as const).map(([label, rule]) => (
            <div key={label} className="flex gap-1.5">
              <dt className="text-muted-foreground">{label}</dt>
              <dd className="font-medium text-foreground">{rule === "allowed" ? C.allowed : C.blocked}</dd>
            </div>
          ))}
        </dl>
      )}
      <Tabs defaultValue={noDesktop ? "from" : "to"} className="mt-2" onValueChange={(v) => {
          setTab(v);
          if (v === "from") void load();
        }}>
        {!noDesktop && (
          <TabsList>
            <TabsTrigger value="to">{C.toTab}</TabsTrigger>
            <TabsTrigger value="from">{C.fromTab}</TabsTrigger>
          </TabsList>
        )}
        <p className="mt-2 flex flex-wrap items-center gap-1.5 text-meta text-muted-foreground">
          {C.folderLabel}
          <Mono className="text-meta text-foreground">{view.folder}</Mono>
          {noDesktop ? DATA_OUT_RUN.FOLDER_IN_RUN : C.folderWhere}
        </p>
        {noDesktop && (
          <p data-testid="desktop-files-no-desktop" className="mt-1.5 text-meta text-muted-foreground">
            {DATA_OUT_RUN.FILES_NO_DESKTOP}
          </p>
        )}
        {!noDesktop && <TabsContent value="to">
          {rules.upload !== "allowed" ? (
            <Blocked title={C.uploadBlocked} />
          ) : (
            <div
              className="mt-2 rounded-lg border border-dashed border-border px-3 py-3 text-center"
              onDragOver={(e) => e.preventDefault()}
              onDrop={(e) => {
                e.preventDefault();
                void send(e.dataTransfer.files);
              }}
            >
              <p className="text-xs text-foreground">{C.drop}</p>
              <p className="mt-0.5 text-meta text-muted-foreground">{C.dropSub}</p>
              <input ref={input} type="file" multiple hidden aria-label={C.choose} onChange={(e) => void send(e.target.files)} />
              <Button size="sm" variant="outline" className="mt-2" onClick={() => input.current?.click()}>
                {C.choose}
              </Button>
            </div>
          )}
          <ul className="mt-2 space-y-1.5">
            {uploads.map((u) => (
              <li key={u.key} data-testid="desktop-upload">
                <div className="flex items-center justify-between gap-3 text-xs">
                  <span className="truncate text-foreground">{u.name}</span>
                  <span className="shrink-0 text-meta text-muted-foreground">
                    {transferSize(u.bytes)} · {status[u.state]}
                  </span>
                </div>
                {u.line && <p className="font-mono text-meta text-muted-foreground">{u.line}</p>}
              </li>
            ))}
          </ul>
          <p className="mt-2 text-meta text-muted-foreground">{C.everyFile}</p>
        </TabsContent>}
        <TabsContent value="from">
          {rules.download !== "allowed" && rules.download !== "review" ? (
            <>
              <Blocked title={C.downloadBlocked} why={dataOut === "review" || dataOut === "blocked" ? DATA_OUT_RUN.FILES_BLOCKED : undefined} />
              <p className="mt-2 text-meta text-muted-foreground">{C.blockedEmpty}</p>
              <p className="mt-1 text-meta text-muted-foreground">{C.refusedLogged}</p>
            </>
          ) : rules.download === "review" ? (
            <>
              {paused !== null && (
                <p data-testid="desktop-files-paused" className="mt-2 text-meta text-muted-foreground">
                  {paused}
                </p>
              )}
              {paused === null && view.files.length === 0 && <p className="mt-2 text-meta text-muted-foreground">{C.emptyFolder}</p>}
              <ul className="mt-2 space-y-1.5">
                {view.files.map((f) => (
                  <DesktopReviewRow
                    key={f.name}
                    runId={runId}
                    file={f}
                    latest={requests?.find((r) => (r.requested_scope as Partial<FileDownloadScope>).name === f.name)}
                    onAsked={(r) => setRequests((rows) => [r, ...(rows ?? [])])}
                    onRefused={() => void loadRequests()}
                  />
                ))}
              </ul>
              {requestsFailed && (
                <div className="mt-2 border-t border-border pt-2">
                  <p className="text-meta text-muted-foreground">{C.requestsUnreadable}</p>
                  <Button size="sm" variant="outline" className="mt-1.5" onClick={() => void loadRequests()}>
                    <RotateCw aria-hidden="true" />
                    {C.retry}
                  </Button>
                </div>
              )}
              <p className="mt-2 text-meta text-muted-foreground">{C.reviewFooter}</p>
            </>
          ) : (
            <>
              {paused !== null && (
                <p data-testid="desktop-files-paused" className="mt-2 text-meta text-muted-foreground">
                  {paused}
                </p>
              )}
              {paused === null && view.files.length === 0 && <p className="mt-2 text-meta text-muted-foreground">{C.emptyFolder}</p>}
              <ul className="mt-2 space-y-1.5">
                {view.files.map((f) => {
                  const mark = marks[f.name];
                  return (
                    <li key={f.name} data-testid="desktop-download">
                      <div className="flex items-center justify-between gap-3 text-xs">
                        <span className="truncate text-foreground">{f.name}</span>
                        <span className="flex shrink-0 items-center gap-2 text-meta text-muted-foreground">
                          {transferSize(f.bytes)}
                          <Button size="sm" variant="outline" aria-label={`${C.download} ${f.name}`} disabled={fetching.has(f.name)} onClick={() => void fetchFile(f.name)}>
                            {C.download}
                          </Button>
                        </span>
                      </div>
                      {mark && "line" in mark && <p className="font-mono text-meta text-muted-foreground">{mark.line}</p>}
                      {mark && "error" in mark && (
                        <p className="font-mono text-meta text-muted-foreground">
                          {C.downloadFailed}: {mark.error}
                        </p>
                      )}
                    </li>
                  );
                })}
              </ul>
              <p className="mt-2 text-meta text-muted-foreground">{C.downloadsAllowed}</p>
            </>
          )}
        </TabsContent>
      </Tabs>
    </section>
  );
}
