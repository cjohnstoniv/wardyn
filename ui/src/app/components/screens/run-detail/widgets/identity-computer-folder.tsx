/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Identity widget's "Folders from your computer" rows (M4, #2982): the run's folder held by the
// person's own runner, as GET /runs/{id}'s computer_folder reports it. The first sync (waiting, synced,
// stopped), where it is in the sandbox, which way changes go, the files each way, and, once a Both ways
// run has ended, the changes that came back. One side edits at a time, so there is no live sync to show.
import { absoluteTime, fmtBytes, shortTime } from "../../../../lib/format";
import { isTerminalRunState, type RunComputerFolder, type RunDetail } from "../../../../lib/types";
import { FORCED_KEEP_PREFIX, RUN_COMPUTER_FOLDER as C } from "../../../wardyn/copy/runners";

// Only a fate this page knows is drawn; an absent or unknown one falls back to the neutral line.
const isKnownFate = (v: unknown) => v === "retained" || v === "given_up" || v === "complete";
// What a terminal run's folder answer says about settling, for the run page's poll. "unsettled": its changes are
// kept for their copy back, or were given up while the sandbox's stop is not confirmed. "stopping": nothing is left
// to copy back and the server still reports the sandbox copy as kept, which a stop under way clears. "settled": the
// sandbox copy is confirmed deleted, so nothing is left to change. "unknown": neither a fate nor a sandbox copy,
// from a server that reports neither or a read it could not finish. null for a run that has not ended.
export type ComputerFolderSettling = "unsettled" | "stopping" | "settled" | "unknown";
export const computerFolderSettling = (run: Pick<RunDetail, "state" | "computer_folder"> | null | undefined): ComputerFolderSettling | null => {
  if (!run || !isTerminalRunState(run.state)) return null;
  const f = run.computer_folder;
  if (f?.sandbox_copy === "deleted") return "settled";
  if (f?.fate === "retained" || f?.fate === "given_up") return "unsettled";
  return f?.sandbox_copy === "kept" ? "stopping" : "unknown";
};
const count = (n: number) => n.toLocaleString("en");
// The give-up time in the viewer's zone: the app's absolute time plus a short zone name, so it is never ambiguous.
const zoneName = (iso: string) => new Intl.DateTimeFormat(undefined, { timeZoneName: "short" }).formatToParts(new Date(iso)).find((p) => p.type === "timeZoneName")?.value ?? "";
const deadline = (iso: string) => `${absoluteTime(iso)} ${zoneName(iso)}`.trim();

export function ComputerFolderRows({ run }: { run: RunDetail }) {
  const f = run.computer_folder;
  if (!f) return null;
  const ended = isTerminalRunState(run.state);
  const status = f.state === "failed" ? C.STOPPED : f.state === "synced" ? (ended ? C.ENDED : C.SYNCED) : C.WAITING;
  const refused = f.seed?.refused.length ?? 0;
  return (
    <>
      <div className="col-span-2 border-t border-border" aria-hidden />
      <dt className="col-span-2 font-semibold text-foreground" data-testid="run-computer-folder">{C.TITLE}</dt>
      <dt className="text-muted-foreground">{f.name}</dt>
      <dd className="min-w-0 text-right text-foreground">{status}</dd>
      <dt className="text-muted-foreground">{C.IN_SANDBOX}</dt>
      <dd className="min-w-0 truncate text-right font-mono text-foreground" title={f.remote_dir}>{f.remote_dir}</dd>
      <dt className="text-muted-foreground">{C.CHANGES}</dt>
      <dd className="min-w-0 text-right text-foreground">{f.direction === "both" ? C.BOTH : C.COPY_IN}</dd>
      {f.seed && !f.seed.error && (
        <>
          <dt className="text-muted-foreground">{C.TO_SANDBOX}</dt>
          <dd className="min-w-0 text-right text-foreground">{C.AT_START(count(f.seed.files), fmtBytes(f.seed.bytes))}</dd>
        </>
      )}
      {f.back && !f.back.error && (
        <>
          <dt className="text-muted-foreground">{C.BACK}</dt>
          <dd className="min-w-0 text-right text-foreground">{C.FILES(count(f.back.files))}</dd>
        </>
      )}
      {f.state === "waiting" && !ended && <p className="col-span-2 text-meta text-muted-foreground">{C.WAITING_BODY}</p>}
      {f.state === "failed" && (
        <p role="alert" className="col-span-2 text-meta text-danger">
          {f.error} {C.NOTHING_RAN}
        </p>
      )}
      {refused > 0 && <p className="col-span-2 text-meta text-muted-foreground">{C.REFUSED(count(refused))}</p>}
      {ended && f.back && !f.back.error && <p className="col-span-2 text-meta text-muted-foreground">{C.BACK_AT(shortTime(f.back.at), count(f.back.files))}</p>}
      {f.back?.error &&
        (f.back.error.startsWith(FORCED_KEEP_PREFIX) ? (
          <p className="col-span-2 text-meta text-muted-foreground">{f.back.error}</p>
        ) : (
          <p role="alert" className="col-span-2 text-meta text-danger">{C.BACK_FAILED(f.back.error)}</p>
        ))}
      {ended && f.state === "synced" && !isKnownFate(f.fate) && f.sandbox_copy !== "deleted" && <p className="col-span-2 text-meta text-muted-foreground">{C.MAY_BE_GONE}</p>}
      {ended && (f.fate === "retained" || f.fate === "given_up") && <OwedRows f={f} />}
      {ended && f.sandbox_copy === "deleted" && <p className="col-span-2 text-meta text-muted-foreground">{C.DELETED}</p>}
    </>
  );
}

// The changes still owed after the run ended: how many, which files, and what Wardyn will do or did.
// "Not counted" only when the runner never counted; a null count is never shown as 0.
function OwedRows({ f }: { f: RunComputerFolder }) {
  const given = f.fate === "given_up";
  const r = f.last_report;
  const paths = r?.paths ?? [];
  // "The sandbox was stopped" only on the server's confirmed deletion; kept, absent or unknown says nothing of the copy.
  const stopUnconfirmed = given && f.sandbox_copy !== "deleted";
  const known = r !== undefined && r.unsynced !== null;
  const lead = stopUnconfirmed
    ? C.STOP_UNCONFIRMED
    : given
      ? paths.length > 0 ? C.GAVE_UP_LIST : C.GAVE_UP_NO_LIST
      : paths.length > 0 ? C.KEPT_LIST : C.KEPT_NO_LIST;
  return (
    <>
      <dt className="text-muted-foreground">{given ? C.GIVEN_UP : C.NOT_BACK}</dt>
      <dd className="min-w-0 text-right text-foreground">{known ? C.FILES(count(r.unsynced as number)) : C.NOT_COUNTED}</dd>
      <p className="col-span-2 text-meta text-muted-foreground">{lead}</p>
      {paths.length > 0 && (
        <div className="col-span-2 min-w-0">
          <ul
            tabIndex={0}
            aria-label={C.PATHS_LABEL}
            data-testid="run-computer-folder-paths"
            className="scroll-thin max-h-40 overflow-y-auto rounded-md bg-muted p-2 font-mono text-meta text-foreground break-all"
          >
            {paths.slice(0, 50).map((p) => (
              <li key={p}>{p}</li>
            ))}
          </ul>
          {(r?.paths_more ?? 0) > 0 && <p className="mt-1 text-meta text-muted-foreground">{C.MORE(count(r?.paths_more ?? 0))}</p>}
        </div>
      )}
      {f.fate === "retained" && f.give_up_at && <p className="col-span-2 text-meta text-muted-foreground">{C.KEPT_DEADLINE(deadline(f.give_up_at))}</p>}
      {f.fate === "retained" && <p className="col-span-2 text-meta text-foreground">{C.KEEP_RUNNER_ONLINE(f.runner)}</p>}
    </>
  );
}
