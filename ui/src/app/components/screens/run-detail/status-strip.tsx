/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { HttpError } from "../../../lib/api/core";
import { runs as runsApi } from "../../../lib/api/runs";
import { usePoll } from "../../../lib/use-poll";
import { cn } from "../../ui/utils";
import { RUN_COCKPIT } from "../../wardyn/copy";
import type { EvidenceFacts, WidgetContext } from "./widget-registry";

const POLL_MS = 10000;

// #3038: what the Files and Sandbox tiles can say. Both read the same two evidence endpoints the widgets under
// them poll; anything that is not a real reading (no sandbox, no exec channel, no git) is "nothing to show".
// The cockpit reads them once and hands them to the strip and the widget gate through the widget context.
// A macOS VM's CPU comes from inside the guest, and only the Sandbox card has room to say so (#3924): skipGuestCPU
// leaves the tile with nothing to show, without running a second read in the guest.
//
// One failed read is not a verdict (UXR-41): a good value already on the tile stays, and with none yet the tile says
// it could not load. Only the runner answering "no" (501 no exec channel, 409 no sandbox to read) is nothing to show.
// `failed` rides beside the facts so the shared EvidenceFacts shape (widget-registry.ts) is unchanged.
export type EvidenceReading = EvidenceFacts & { failed?: { files?: true; sandbox?: true } };

const refused = (e: unknown) => e instanceof HttpError && (e.status === 501 || e.status === 409);

export function useEvidenceFacts(runId: string, live: boolean, skipGuestCPU = false): EvidenceReading | undefined {
  const [facts, setFacts] = React.useState<EvidenceReading | undefined>(undefined);
  const load = React.useCallback(async () => {
    const [files, resources] = await Promise.allSettled([runsApi.getFiles(runId), skipGuestCPU ? Promise.resolve(undefined) : runsApi.getResources(runId)]);
    setFacts((prev) => {
      const next: EvidenceReading = { files: null, sandbox: null };
      if (files.status === "fulfilled") {
        next.files = files.value.vcs !== "none" && files.value.vcs !== "unknown" ? files.value.files.length : null;
      } else if (prev && !prev.failed?.files) {
        next.files = prev.files;
      } else if (!refused(files.reason)) {
        next.failed = { ...next.failed, files: true };
      }
      if (resources.status === "fulfilled") {
        const cpu = resources.value?.cpu_percent;
        next.sandbox = cpu !== undefined ? `${Math.round(Math.max(0, Math.min(100, cpu)))}%` : null;
      } else if (prev && !prev.failed?.sandbox) {
        next.sandbox = prev.sandbox;
      } else if (!refused(resources.reason)) {
        next.failed = { ...next.failed, sandbox: true };
      }
      return next;
    });
  }, [runId, skipGuestCPU]);
  React.useEffect(() => {
    void load();
  }, [load]);
  usePoll(load, POLL_MS, !live);
  return facts;
}

function Tile({ label, value, tone, title }: { label: string; value?: React.ReactNode; tone?: "warning" | "muted"; title?: string }) {
  return (
    <div data-testid="status-tile" className="flex min-w-0 items-baseline justify-between gap-2 rounded-lg border border-border bg-card px-3 py-1.5">
      <span className="shrink-0 text-meta text-muted-foreground">{label}</span>
      {value === undefined ? (
        <span className="h-4 w-14 animate-pulse rounded bg-muted" />
      ) : (
        <span title={title ?? (typeof value === "string" ? value : undefined)} className={cn("truncate text-xs font-medium", tone === "warning" ? "text-warning" : tone === "muted" ? "text-muted-foreground" : "text-foreground")}>
          {value}
        </span>
      )}
    </div>
  );
}

// The four tiles above the Overview's canvas: Egress, Files, Sandbox, Credentials. The empty ones say so once, in
// a fixed phrase, instead of each widget spending a box on a sentence.
export function RunStatusStrip({ ctx }: { ctx: WidgetContext }) {
  const facts: EvidenceReading | undefined = ctx.evidence;
  const minted = ctx.audit.filter((e) => e.action === "credential.mint" && e.outcome === "success").length;
  // Held is the alarm and leads; then what the run was allowed, then refused, so a failed install's denies are not
  // hidden behind a large allow count (UXR-23). The counts are the same RUN_COCKPIT sentences, joined by " · ".
  const allowed = ctx.egress.filter((e) => e.decision === "allow").length;
  const denied = ctx.egress.filter((e) => e.decision === "deny").length;
  const egressParts = [
    ctx.heldCount > 0 && { text: RUN_COCKPIT.held(ctx.heldCount), warn: true },
    allowed > 0 && { text: RUN_COCKPIT.allow(allowed), warn: false },
    denied > 0 && { text: RUN_COCKPIT.deny(denied), warn: true },
  ].filter((p): p is { text: string; warn: boolean } => p !== false);
  const egressValue =
    egressParts.length === 0 ? (
      RUN_COCKPIT.tileNoneYet
    ) : (
      <>
        {egressParts.map((p, i) => (
          <React.Fragment key={p.text}>
            {i > 0 && " · "}
            <span className={p.warn ? "text-warning" : undefined}>{p.text}</span>
          </React.Fragment>
        ))}
      </>
    );
  const failed = facts?.failed;
  const filesValue =
    facts &&
    (failed?.files
      ? RUN_COCKPIT.loadError
      : facts.files === null
        ? RUN_COCKPIT.tileNotInspectable
        : facts.files === 0
          ? RUN_COCKPIT.tileNoneYet
          : RUN_COCKPIT.tileFilesCount(facts.files));
  const sandboxValue = facts && (failed?.sandbox ? RUN_COCKPIT.loadError : (facts.sandbox ?? RUN_COCKPIT.tileNotAvailable));
  const mutedWords: readonly string[] = [RUN_COCKPIT.tileNoneYet, RUN_COCKPIT.tileNotInspectable, RUN_COCKPIT.tileNotAvailable, RUN_COCKPIT.loadError];
  return (
    <div role="group" aria-label={RUN_COCKPIT.tilesLabel} className="grid shrink-0 grid-cols-2 gap-3 px-2.5 pt-2.5 xl:grid-cols-4">
      <Tile label={RUN_COCKPIT.tileEgress} value={egressValue} title={egressParts.map((p) => p.text).join(" · ")} tone={egressParts.length === 0 ? "muted" : undefined} />
      <Tile label={RUN_COCKPIT.tileFiles} value={filesValue} tone={filesValue !== undefined && mutedWords.includes(filesValue) ? "muted" : undefined} />
      <Tile label={RUN_COCKPIT.tileSandbox} value={sandboxValue} tone={sandboxValue !== undefined && mutedWords.includes(sandboxValue) ? "muted" : undefined} />
      <Tile label={RUN_COCKPIT.tileCredentials} value={RUN_COCKPIT.tileCredentialsValue(ctx.grants.length, minted)} />
    </div>
  );
}
