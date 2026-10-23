/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The run's Files panel (mock M7-Files): files move between this browser and ~/Transfers in the
// desktop under the run's desktop rules, and the clipboard strip says which way the desktop relay
// lets the clipboard through (M7-Phone). The server decides every rule; this panel only shows it.
import * as React from "react";
import { desktopFiles, type DesktopFilesView } from "../../../lib/api/desktop-files";
import { HttpError } from "../../../lib/api/core";
import { Button } from "../../ui/button";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "../../ui/tabs";
import { Mono } from "../../wardyn/code-block";
import { DESKTOP_FILES as C } from "../../wardyn/copy";

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

const clock = () => new Date().toTimeString().slice(0, 5);
const message = (e: unknown) => (e instanceof Error ? e.message : String(e));

function Blocked({ title }: { title: string }) {
  return (
    <div className="mt-2 rounded-lg border border-warning/30 bg-warning-subtle px-3 py-2.5">
      <p className="text-xs font-medium text-foreground">{title}</p>
      <p className="mt-0.5 text-meta text-muted-foreground">{C.refused}</p>
    </div>
  );
}

export function DesktopFilesPanel({ runId }: { runId: string }) {
  const [view, setView] = React.useState<DesktopFilesView | null>(null);
  const [loadError, setLoadError] = React.useState<string | null>(null);
  const [uploads, setUploads] = React.useState<UploadRow[]>([]);
  const [marks, setMarks] = React.useState<Record<string, DownloadMark>>({});
  const nextKey = React.useRef(0);
  const input = React.useRef<HTMLInputElement>(null);

  const load = React.useCallback(async () => {
    try {
      setView(await desktopFiles.list(runId));
      setLoadError(null);
    } catch (e) {
      setLoadError(message(e));
    }
  }, [runId]);
  React.useEffect(() => {
    void load();
  }, [load]);

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
    }
  }

  if (loadError) {
    return (
      <section aria-label={C.title} data-testid="desktop-files" className="mt-4">
        <p className="text-xs font-medium text-foreground">{C.title}</p>
        <p className="mt-1 font-mono text-meta text-muted-foreground">{loadError}</p>
      </section>
    );
  }
  if (!view) return null;
  const { rules } = view;
  const status = { sending: C.sending, delivered: C.delivered, too_large: C.tooLarge, failed: C.notSent };

  return (
    <section aria-label={C.title} data-testid="desktop-files" className="mt-4">
      <p className="text-xs font-medium text-foreground">{C.title}</p>
      <dl className="mt-1.5 flex flex-wrap gap-x-4 gap-y-1 text-meta">
        {([[C.pasteIn, rules.paste_in], [C.copyOut, rules.copy_out]] as const).map(([label, rule]) => (
          <div key={label} className="flex gap-1.5">
            <dt className="text-muted-foreground">{label}</dt>
            <dd className="font-medium text-foreground">{rule === "allowed" ? C.allowed : C.blocked}</dd>
          </div>
        ))}
      </dl>
      <Tabs defaultValue="to" className="mt-2" onValueChange={(v) => v === "from" && void load()}>
        <TabsList>
          <TabsTrigger value="to">{C.toTab}</TabsTrigger>
          <TabsTrigger value="from">{C.fromTab}</TabsTrigger>
        </TabsList>
        <p className="mt-2 flex flex-wrap items-center gap-1.5 text-meta text-muted-foreground">
          {C.folderLabel}
          <Mono className="text-meta text-foreground">{view.folder}</Mono>
          {C.folderWhere}
        </p>
        <TabsContent value="to">
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
        </TabsContent>
        <TabsContent value="from">
          {rules.download !== "allowed" ? (
            <>
              <Blocked title={C.downloadBlocked} />
              <p className="mt-2 text-meta text-muted-foreground">{C.blockedEmpty}</p>
              <p className="mt-1 text-meta text-muted-foreground">{C.refusedLogged}</p>
            </>
          ) : (
            <>
              {view.files.length === 0 && <p className="mt-2 text-meta text-muted-foreground">{C.emptyFolder}</p>}
              <ul className="mt-2 space-y-1.5">
                {view.files.map((f) => {
                  const mark = marks[f.name];
                  return (
                    <li key={f.name} data-testid="desktop-download">
                      <div className="flex items-center justify-between gap-3 text-xs">
                        <span className="truncate text-foreground">{f.name}</span>
                        <span className="flex shrink-0 items-center gap-2 text-meta text-muted-foreground">
                          {transferSize(f.bytes)}
                          <Button size="sm" variant="outline" aria-label={`${C.download} ${f.name}`} onClick={() => void fetchFile(f.name)}>
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
