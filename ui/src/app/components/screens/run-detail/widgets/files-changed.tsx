/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Files-changed widget — polls the run's workspace diff stat, read by running
// git INSIDE the sandbox (GET /runs/{id}/files). Every non-200 the API
// returns is MEANINGFUL, not a generic failure (see lib/api/runs.ts's
// getFiles doc comment: 501 = no exec primitive, 409 = no sandbox yet) — only
// an unrecognized/network error is transient, and a transient BACKGROUND poll
// blip keeps the last-good rows on screen rather than flashing an error (the
// established run-detail.tsx load() pattern); only the first (foreground)
// load surfaces it.
import * as React from "react";
import { AlertTriangle, FileDiff } from "lucide-react";
import { runs as runsApi } from "../../../../lib/api/runs";
import { HttpError } from "../../../../lib/api/core";
import type { RunFileStat, RunFilesResult } from "../../../../lib/types";
import { usePoll } from "../../../../lib/use-poll";
import { RUN_COCKPIT } from "../../../wardyn/copy";
import { WidgetCard } from "../../../wardyn/primitives";

const POLL_MS = 4000;


type FilesState =
  | { kind: "loading" }
  | { kind: "no-vcs"; path?: string }
  | { kind: "vcs-unknown"; path?: string }
  | { kind: "no-sandbox" }
  | { kind: "unsupported" }
  | { kind: "error" }
  | { kind: "ready"; data: RunFilesResult };

export function FilesChangedWidget({ runId, live }: { runId: string; live: boolean }) {
  const [state, setState] = React.useState<FilesState>({ kind: "loading" });

  const load = React.useCallback(
    (foreground: boolean) => {
      // Returned for usePoll's in-flight guard (R4-F073/F074).
      return runsApi
        .getFiles(runId)
        .then((data) => {
          setState(
            data.vcs === "none"
              ? { kind: "no-vcs", path: data.path }
              : data.vcs === "unknown"
                ? { kind: "vcs-unknown", path: data.path }
                : { kind: "ready", data },
          );
        })
        .catch((err) => {
          if (err instanceof HttpError && err.status === 501) setState({ kind: "unsupported" });
          else if (err instanceof HttpError && err.status === 409) setState({ kind: "no-sandbox" });
          else if (foreground) setState({ kind: "error" });
          // else: transient background blip — keep whatever's already on screen.
        });
    },
    [runId],
  );

  React.useEffect(() => {
    load(true);
  }, [load]);
  usePoll(() => load(false), POLL_MS, !live);

  const totals = state.kind === "ready" ? sumCounted(state.data.files) : null;

  return (
    <WidgetCard
      title="Files changed"
      Icon={FileDiff}
      right={
        totals && (totals.added > 0 || totals.deleted > 0) ? (
          <span className="font-mono text-meta text-muted-foreground">
            {totals.added > 0 && <span className="text-success">+{totals.added}</span>}
            {totals.added > 0 && totals.deleted > 0 && " "}
            {totals.deleted > 0 && <span className="text-danger">−{totals.deleted}</span>}
          </span>
        ) : undefined
      }
      bodyClassName="p-0"
    >
      {renderBody(state, live)}
    </WidgetCard>
  );
}

// Sums only the files that actually reported a count — a binary/untracked
// row contributes nothing, the same way `git diff --stat`'s own total line
// silently excludes binaries. This is an aggregate over the counted subset,
// not a per-file claim, so it's not the `?? 0` lie the per-row render must
// avoid.
function sumCounted(files: RunFileStat[]): { added: number; deleted: number } {
  return files.reduce(
    (acc, f) => ({ added: acc.added + (f.added ?? 0), deleted: acc.deleted + (f.deleted ?? 0) }),
    { added: 0, deleted: 0 },
  );
}

function renderBody(state: FilesState, live: boolean): React.ReactNode {
  switch (state.kind) {
    case "loading":
      return (
        <div className="space-y-1.5 px-2.5 py-2.5">
          <div className="h-3 w-full animate-pulse rounded bg-muted" />
          <div className="h-3 w-2/3 animate-pulse rounded bg-muted" />
        </div>
      );
    case "no-vcs":
      return <Quiet text={RUN_COCKPIT.noVcs(state.path)} />;
    case "vcs-unknown":
      return <Quiet text={RUN_COCKPIT.vcsUnknown(state.path)} />;
    case "no-sandbox":
      return <Quiet text={live ? RUN_COCKPIT.noSandboxYet : RUN_COCKPIT.sandboxGone} />;
    case "unsupported":
      return <Quiet text={RUN_COCKPIT.execUnsupported} />;
    case "error":
      return <Quiet text={RUN_COCKPIT.loadError} />;
    case "ready":
      if (state.data.files.length === 0) return <Quiet text={RUN_COCKPIT.noFilesChanged} />;
      return (
        <>
          <div className="flex flex-col divide-y divide-border">
            {state.data.files.map((f) => (
              <FileRow key={f.path} f={f} />
            ))}
          </div>
          {state.data.truncated && (
            <div className="flex items-center gap-1.5 border-t border-border px-2.5 py-1.5 text-meta text-warning">
              <AlertTriangle className="size-3 shrink-0" aria-hidden />
              {RUN_COCKPIT.filesTruncated}
            </div>
          )}
        </>
      );
  }
}

function Quiet({ text }: { text: string }) {
  return <p className="px-2.5 py-3 text-xs text-muted-foreground">{text}</p>;
}

function FileRow({ f }: { f: RunFileStat }) {
  return (
    <div className="flex items-center gap-2 px-2.5 py-1.5">
      <span className="shrink-0 font-mono text-meta text-muted-foreground" title={f.status}>
        {f.status ?? "—"}
      </span>
      <span className="min-w-0 flex-1 truncate font-mono text-xs text-foreground" title={f.path}>
        {f.path}
      </span>
      <span className="shrink-0 font-mono text-meta">
        {/* Never `?? 0`: a binary/untracked file's counts are ABSENT, not 0.
            Showing "+0 −0" on a binary asset the agent just rewrote would
            claim nothing changed, which is false — so binary renders as its
            own word, and an absent-but-not-binary count renders nothing. */}
        {f.binary ? (
          <span className="text-muted-foreground">binary</span>
        ) : (
          <>
            {!!f.added && <span className="text-success">+{f.added}</span>}
            {!!f.added && !!f.deleted && " "}
            {!!f.deleted && <span className="text-danger">−{f.deleted}</span>}
          </>
        )}
      </span>
    </div>
  );
}
