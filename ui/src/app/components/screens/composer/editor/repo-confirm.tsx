/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Drafting from a repository, in two steps. The picker offers only a workspace the person may use and one of that
// workspace's own repository sources, never a free address. After the server has fetched and scanned the build files,
// the confirmation lists their names and sizes, what was skipped and what was held back, and nothing is sent to the
// provider until the person presses Send files.
import * as React from "react";
import { assistant, type AssistantConfirmedView, type AssistantPreview, type AssistantPurpose, type AssistantSessionRequest, type AssistantTarget } from "../../../../lib/api/assistant";
import { HttpError } from "../../../../lib/api/core";
import { getErrorMessage } from "../../../../lib/format";
import { workspaces as workspacesApi } from "../../../../lib/api/workspaces";
import type { Workspace } from "../../../../lib/types";
import { Button } from "../../../ui/button";
import { ASSISTANT as T } from "../../../wardyn/copy/assistant";
import { Notice } from "../../../wardyn/notice";
import { repoRows } from "../../new-run-flow/steps/files/files-seams";

/** What the session start sends for a repository draft: the workspace and one of its sources, never a URL. */
export interface RepoPick {
  readonly workspace_id: string;
  readonly repo: string;
}

export const repoSessionRequest = (base: Pick<AssistantSessionRequest, "purpose" | "target" | "preview_digest">, pick: RepoPick): AssistantSessionRequest => ({
  ...base,
  workspace_id: pick.workspace_id,
  repo: pick.repo,
});

export function RepoPicker({ onFetch, busy }: { onFetch: (pick: RepoPick) => void; busy?: boolean }) {
  const [list, setList] = React.useState<readonly Workspace[] | null>(null);
  const [error, setError] = React.useState<string | null>(null);
  const [workspaceId, setWorkspaceId] = React.useState("");
  const [repo, setRepo] = React.useState("");
  const wsId = React.useId();
  const repoId = React.useId();

  React.useEffect(() => {
    let live = true;
    workspacesApi
      .listWorkspaces()
      .then((w) => live && setList(w))
      .catch((e: unknown) => live && setError(getErrorMessage(e)));
    return () => {
      live = false;
    };
  }, []);

  // Only a workspace the person may use, and only the repository sources the server lists for it.
  const rows = React.useMemo(() => repoRows(list ?? []).filter((r) => !r.unavailable), [list]);
  const usable = (list ?? []).filter((w) => rows.some((r) => r.workspaceId === w.id));
  const sources = rows.filter((r) => r.workspaceId === workspaceId);

  return (
    <section data-testid="composer-repo-picker" className="min-w-0 space-y-2.5">
      <p className="text-body text-muted-foreground">{T.FETCH_EXPLAIN}</p>
      {error && <Notice tone="danger">{error}</Notice>}
      <div className="grid gap-2.5 sm:grid-cols-2">
        <div className="space-y-1">
          <label htmlFor={wsId} className="text-meta font-medium">
            {T.REPO_WORKSPACE}
          </label>
          <select
            id={wsId}
            className="h-9 w-full rounded-md border border-border bg-background px-2 text-body"
            value={workspaceId}
            onChange={(e) => {
              setWorkspaceId(e.target.value);
              setRepo("");
            }}
          >
            <option value="" />
            {usable.map((w) => (
              <option key={w.id} value={w.id}>
                {w.name}
              </option>
            ))}
          </select>
        </div>
        <div className="space-y-1">
          <label htmlFor={repoId} className="text-meta font-medium">
            {T.REPO_SOURCE}
          </label>
          <select
            id={repoId}
            className="h-9 w-full rounded-md border border-border bg-background px-2 text-body"
            value={repo}
            disabled={!workspaceId}
            onChange={(e) => setRepo(e.target.value)}
          >
            <option value="" />
            {sources.map((r) => (
              <option key={r.key} value={r.repo}>
                {r.name}
              </option>
            ))}
          </select>
        </div>
      </div>
      <Button type="button" disabled={!workspaceId || !repo || busy} onClick={() => onFetch({ workspace_id: workspaceId, repo })}>
        {T.FETCH_BUILD_FILES}
      </Button>
    </section>
  );
}

/** The build_files event: names and sizes, never contents (assistantBuildFilesEvent). */
export interface BuildFilesEvent {
  readonly files: readonly { name: string; bytes: number }[];
  readonly skipped: readonly string[];
  readonly held_back: readonly string[];
}

const names = (v: unknown): string[] => (Array.isArray(v) ? v.filter((x): x is string => typeof x === "string") : []);

/** Reads a build_files event's payload; anything that is not a name and a size is left out. */
export function buildFilesOf(payload: Record<string, unknown>): BuildFilesEvent {
  const files = Array.isArray(payload.files) ? payload.files : [];
  return {
    files: files
      .filter((f): f is { name: string; bytes: number } => !!f && typeof (f as { name?: unknown }).name === "string")
      .map((f) => ({ name: f.name, bytes: typeof f.bytes === "number" ? f.bytes : 0 })),
    skipped: names(payload.skipped),
    held_back: names(payload.held_back),
  };
}

export interface RepoConfirmProps {
  sessionId: string;
  /** The "provider · model" the files would go to. */
  provider: string;
  files: BuildFilesEvent;
  /** The digest of the preview shown at Start. */
  previewDigest: string;
  purpose: AssistantPurpose;
  target: AssistantTarget;
  /** The files were sent and the assistant's run has started, on the session the server answered with: a new one. */
  onSent: (session: AssistantConfirmedView) => void;
  /** Any other refusal: the entry is shown again, with the server's sentence. */
  onReturn: (message: string) => void;
}

export function RepoConfirm({ sessionId, provider, files, previewDigest, purpose, target, onSent, onReturn }: RepoConfirmProps) {
  const [digest, setDigest] = React.useState(previewDigest);
  // The provider the files would go to: the preview's, and the fresh preview's once a stale answer made it be read again.
  const [to, setTo] = React.useState(provider);
  // The records of the preview read again after a stale answer: shown before the person sends the new digest.
  const [records, setRecords] = React.useState<AssistantPreview["sections"] | null>(null);
  const [busy, setBusy] = React.useState(false);
  const [error, setError] = React.useState<string | null>(null);

  const send = async () => {
    if (busy) return;
    setBusy(true);
    setError(null);
    try {
      onSent(await assistant.confirmBuildFiles(sessionId, digest));
    } catch (e) {
      if (e instanceof HttpError && e.status === 409 && e.reason === "assistant_preview_stale") {
        // What is sent changed: the preview is read again, and the person presses Send files once more.
        try {
          const fresh = await assistant.postPreview(purpose, target);
          setDigest(fresh.digest);
          setTo(`${fresh.provider} · ${fresh.model}`);
          setRecords(fresh.sections ?? []);
        } catch (again) {
          setError(getErrorMessage(again));
        }
      } else if (e instanceof HttpError && e.status === 409) {
        onReturn(e.message);
      } else {
        setError(getErrorMessage(e));
      }
    } finally {
      setBusy(false);
    }
  };

  return (
    <section data-testid="composer-repo-confirm" className="min-w-0 space-y-2.5">
      <h3 className="text-body font-medium">{T.SEND_FILES_TITLE(files.files.length, to)}</h3>
      {records && <Notice tone="info">{T.PREVIEW_CHANGED}</Notice>}
      {records && (
        <div data-testid="composer-repo-records" className="min-w-0 space-y-2">
          {records.map((r, i) => (
            <div key={`${r.label}-${i}`} className="min-w-0 rounded-lg border border-border p-2.5">
              <span className="label-eyebrow">{r.label}</span>
              <pre className="mono mt-1 whitespace-pre-wrap break-words text-meta">{r.text}</pre>
            </div>
          ))}
        </div>
      )}
      <ul className="space-y-0.5">
        {files.files.map((f) => (
          <li key={f.name} className="flex flex-wrap items-baseline justify-between gap-2 text-body">
            <span className="mono min-w-0 break-all">{f.name}</span>
            <span className="text-meta text-muted-foreground">{T.FILE_BYTES(f.bytes)}</span>
          </li>
        ))}
      </ul>
      {files.skipped.length > 0 && (
        <p className="text-meta text-muted-foreground">
          {T.FILES_SKIPPED}: <span className="mono">{files.skipped.join(", ")}</span>
        </p>
      )}
      {files.held_back.length > 0 && (
        <p className="text-meta text-muted-foreground">
          {T.FILES_HELD_BACK}: <span className="mono">{files.held_back.join(", ")}</span>
        </p>
      )}
      {error && (
        <p role="alert" className="text-body text-danger">
          {error}
        </p>
      )}
      <Button type="button" disabled={busy} onClick={() => void send()}>
        {T.SEND_FILES}
      </Button>
    </section>
  );
}
