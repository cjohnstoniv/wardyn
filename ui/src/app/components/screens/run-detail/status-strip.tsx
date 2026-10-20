/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { runs as runsApi } from "../../../lib/api/runs";
import { usePoll } from "../../../lib/use-poll";
import { cn } from "../../ui/utils";
import { RUN_COCKPIT } from "../../wardyn/copy";
import type { WidgetContext } from "./widget-registry";

const POLL_MS = 10000;

// #3038: what the Files and Sandbox tiles can say. Both read the same two evidence endpoints the widgets under
// them poll; anything that is not a real reading (no sandbox, no exec channel, no git, an error) is "nothing to show".
type Facts = { files: number | null; sandbox: string | null };

function useEvidenceFacts(runId: string, live: boolean): Facts | undefined {
  const [facts, setFacts] = React.useState<Facts | undefined>(undefined);
  const load = React.useCallback(async () => {
    const [files, resources] = await Promise.allSettled([runsApi.getFiles(runId), runsApi.getResources(runId)]);
    setFacts({
      files: files.status === "fulfilled" && files.value.vcs !== "none" && files.value.vcs !== "unknown" ? files.value.files.length : null,
      sandbox:
        resources.status === "fulfilled"
          ? resources.value.cpu_percent !== undefined
            ? `${Math.round(Math.max(0, Math.min(100, resources.value.cpu_percent)))}% CPU`
            : null
          : null,
    });
  }, [runId]);
  React.useEffect(() => {
    void load();
  }, [load]);
  usePoll(load, POLL_MS, !live);
  return facts;
}

function Tile({ label, value, tone }: { label: string; value?: string; tone?: "warning" | "muted" }) {
  return (
    <div data-testid="status-tile" className="flex min-w-0 items-baseline justify-between gap-2 rounded-lg border border-border bg-card px-3 py-1.5">
      <span className="shrink-0 text-meta text-muted-foreground">{label}</span>
      {value === undefined ? (
        <span className="h-4 w-14 animate-pulse rounded bg-muted" />
      ) : (
        <span className={cn("truncate text-xs font-medium", tone === "warning" ? "text-warning" : tone === "muted" ? "text-muted-foreground" : "text-foreground")}>
          {value}
        </span>
      )}
    </div>
  );
}

// The four tiles above the Overview's canvas: Egress, Files, Sandbox, Credentials. The empty ones say so once, in
// a fixed phrase, instead of each widget spending a box on a sentence.
export function RunStatusStrip({ ctx }: { ctx: WidgetContext }) {
  const facts = useEvidenceFacts(ctx.run.id, !ctx.finished);
  const minted = ctx.audit.filter((e) => e.action === "credential.mint" && e.outcome === "success").length;
  // Held is the alarm and leads; otherwise what the run was allowed, then refused; nothing recorded is "None yet".
  const allowed = ctx.egress.filter((e) => e.decision === "allow").length;
  const denied = ctx.egress.filter((e) => e.decision === "deny").length;
  const egressValue =
    ctx.heldCount > 0
      ? RUN_COCKPIT.held(ctx.heldCount)
      : allowed > 0
        ? RUN_COCKPIT.allow(allowed)
        : denied > 0
          ? RUN_COCKPIT.deny(denied)
          : RUN_COCKPIT.tileNoneYet;
  const filesValue = facts && (facts.files === null ? RUN_COCKPIT.tileNotInspectable : facts.files === 0 ? RUN_COCKPIT.tileNoneYet : RUN_COCKPIT.tileFilesChanged(facts.files));
  const sandboxValue = facts && (facts.sandbox ?? RUN_COCKPIT.tileNotAvailable);
  return (
    <div role="group" aria-label={RUN_COCKPIT.tilesLabel} className="mb-2 grid shrink-0 grid-cols-2 gap-3 lg:grid-cols-4">
      <Tile label={RUN_COCKPIT.tileEgress} value={egressValue} tone={ctx.heldCount > 0 ? "warning" : egressValue === RUN_COCKPIT.tileNoneYet ? "muted" : undefined} />
      <Tile label={RUN_COCKPIT.tileFiles} value={filesValue} tone={filesValue === RUN_COCKPIT.tileNoneYet || filesValue === RUN_COCKPIT.tileNotInspectable ? "muted" : undefined} />
      <Tile label={RUN_COCKPIT.tileSandbox} value={sandboxValue} tone={sandboxValue === RUN_COCKPIT.tileNotAvailable ? "muted" : undefined} />
      <Tile label={RUN_COCKPIT.tileCredentials} value={RUN_COCKPIT.tileCredentialsValue(ctx.grants.length, minted)} />
    </div>
  );
}
