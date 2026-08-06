/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Step ④ Build — the image build as its OWN, followable process. It used to
// happen lazily inside the first session launch, freezing that click behind
// minutes of invisible work. Entering this step kicks the build (idempotent:
// the server is single-flight per workspace and answers from the cache when
// the profile hasn't changed) and polls until a terminal state. An explicit
// image pick has nothing to build and says so.
import * as React from "react";
import { CircleCheck, Loader2, RotateCw, TriangleAlert } from "lucide-react";
import { workspaces as workspacesApi, type WorkspaceBuildState } from "../../../lib/api/workspaces";
import { usePoll } from "../../../lib/use-poll";
import { Mono } from "../../wardyn/code-block";
import { Chip } from "../../wardyn/primitives";
import { Button } from "../../ui/button";

export const BUILD_BLURB =
  "The sandbox boots from one image. This builds it now — visibly — so your verify session and every run start fast. First build installs the detected toolchain (a few minutes); it's cached until the profile changes.";

function elapsedLabel(startedAt?: string): string {
  if (!startedAt) return "";
  const secs = Math.max(0, Math.floor((Date.now() - new Date(startedAt).getTime()) / 1000));
  return secs >= 60 ? `${Math.floor(secs / 60)}m ${secs % 60}s` : `${secs}s`;
}

// The build's real output, tail-following: fixed height so it doesn't shove
// the rest of the step around, auto-scrolled to the newest line. Rendered in
// building/done/failed alike — the failure line plus the log IS the
// debugging story, so it stays up once the build stops.
function BuildLogPane({ log }: { log: string[] }) {
  const ref = React.useRef<HTMLDivElement>(null);
  // Follow the tail unless the operator scrolled up to read earlier output —
  // otherwise a new line would yank them back down mid-read. Starts true
  // (nothing to disagree with yet, and it's what makes the FIRST render land
  // on the newest line rather than the top); onScroll re-evaluates it any
  // time the pane moves, whether the operator scrolled or this effect did.
  const autoFollow = React.useRef(true);
  const onScroll = () => {
    const el = ref.current;
    if (el) autoFollow.current = el.scrollHeight - el.scrollTop - el.clientHeight < 40;
  };
  React.useEffect(() => {
    const el = ref.current;
    if (el && autoFollow.current) el.scrollTop = el.scrollHeight;
  }, [log.length]);
  return (
    <div
      ref={ref}
      onScroll={onScroll}
      data-testid="build-log"
      className="scroll-thin h-56 overflow-y-auto rounded-md border border-border bg-surface-1/60 p-2"
    >
      {log.map((line, i) => (
        <div key={i} className="font-mono text-[0.6875rem] leading-relaxed text-muted-foreground">
          {line}
        </div>
      ))}
    </div>
  );
}

export function StepBuild({
  workspaceId,
  onStateChange,
  agentTools = [],
}: {
  workspaceId: string;
  /** Reports the current terminal-or-not state so the wizard can gate Continue. */
  onStateChange: (state: WorkspaceBuildState) => void;
  /** What the recommended build actually bakes (agentToolsCarried), passed
   *  only while that lane is what's building — the wizard already gates it
   *  to the "recommended" choice before handing it down. */
  agentTools?: string[];
}) {
  const [build, setBuild] = React.useState<WorkspaceBuildState | null>(null);
  const [, forceTick] = React.useReducer((n: number) => n + 1, 0);

  // Latest-ref for the parent callback: the wizard passes a fresh arrow every
  // render, and letting it into kick's deps re-fired the mount effect on each
  // state patch — an infinite POST loop. Kick depends on workspaceId ONLY.
  const onStateChangeRef = React.useRef(onStateChange);
  React.useEffect(() => {
    onStateChangeRef.current = onStateChange;
  });
  const apply = React.useCallback((b: WorkspaceBuildState) => {
    setBuild(b);
    onStateChangeRef.current(b);
  }, []);

  const kick = React.useCallback(() => {
    workspacesApi
      .buildWorkspace(workspaceId)
      .then(apply)
      .catch(() => {
        // Network hiccup on the kick: the poll below keeps reporting honestly.
      });
  }, [workspaceId, apply]);

  React.useEffect(kick, [kick]);
  usePoll(() => {
    void workspacesApi.getWorkspaceBuild(workspaceId).then(apply).catch(() => {});
    forceTick(); // keep the elapsed label moving even between polls
  }, 3000, build?.state !== "building" && build !== null);

  if (!build) {
    return <p className="text-sm text-muted-foreground">Checking the image…</p>;
  }

  return (
    <div className="space-y-3" data-testid="step-build">
      {build.state === "building" && (
        <div className="space-y-2 rounded-lg border border-border bg-surface-2/40 p-3">
          <p className="flex items-center gap-2 text-sm font-medium text-foreground">
            <Loader2 className="size-4 animate-spin" /> Building the workspace image…
            {build.started_at && (
              <span className="font-normal text-muted-foreground">{elapsedLabel(build.started_at)}</span>
            )}
          </p>
          <p className="text-[0.6875rem] leading-snug text-muted-foreground">
            Installing the detected toolchain into the image. This runs server-side — closing this
            dialog doesn&apos;t stop it.
          </p>
          {build.log && build.log.length > 0 && <BuildLogPane log={build.log} />}
        </div>
      )}
      {build.state === "done" && (
        <div className="space-y-1.5 rounded-lg border border-success/30 bg-success-subtle p-3">
          <p className="flex items-center gap-2 text-sm font-medium text-success">
            <CircleCheck className="size-4" /> Image ready
          </p>
          {build.image && <Mono className="text-xs">{build.image}</Mono>}
          {/* build.detail is the server's own AUTHORITATIVE backstop on top of
              the wizard's client-side preview (agentTools): resolveBuildView
              sets it on a "done" state ONLY for the repo-own-devcontainer
              caveat (that lane builds the repo's devcontainer as-is and bakes
              nothing) — so it, not just agentTools, gates the claim, and its
              own text explains the gap instead of leaving it silent. */}
          {agentTools.length > 0 && !build.detail && (
            <div className="flex flex-wrap items-center gap-1.5">
              <span className="text-[0.6875rem] font-semibold uppercase tracking-wide text-muted-foreground">
                Carries
              </span>
              {agentTools.map((t) => (
                <Chip key={t} tone="neutral" mono>
                  {t}
                </Chip>
              ))}
            </div>
          )}
          {build.detail && <p className="text-[0.6875rem] leading-snug text-warning">{build.detail}</p>}
          <p className="text-[0.6875rem] leading-snug text-muted-foreground">
            Cached until the profile changes — sessions and runs boot it immediately.
          </p>
          {build.log && build.log.length > 0 && <BuildLogPane log={build.log} />}
        </div>
      )}
      {build.state === "nothing_to_build" && (
        <div className="space-y-1.5 rounded-lg border border-border bg-surface-2/40 p-3">
          <p className="flex items-center gap-2 text-sm font-medium text-foreground">
            <CircleCheck className="size-4 text-success" /> Nothing to build
          </p>
          {build.image && <Mono className="text-xs">{build.image}</Mono>}
          <p className="text-[0.6875rem] leading-snug text-muted-foreground">
            {build.detail ?? "This image boots as-is."}
          </p>
        </div>
      )}
      {build.state === "failed" && (
        <div className="space-y-2 rounded-lg border border-danger/40 bg-danger-subtle p-3">
          <p className="flex items-center gap-2 text-sm font-medium text-danger">
            <TriangleAlert className="size-4" /> Build failed
          </p>
          {build.detail && <p className="text-xs leading-snug text-danger/90">{build.detail}</p>}
          {build.log && build.log.length > 0 && <BuildLogPane log={build.log} />}
          <Button type="button" size="sm" variant="outline" onClick={kick}>
            <RotateCw className="size-3.5" /> Retry build
          </Button>
        </div>
      )}
      {build.state === "none" && (
        <div className="space-y-1.5 rounded-lg border border-warning/30 bg-warning-subtle p-3">
          <p className="text-xs leading-snug text-warning">
            {build.detail ??
              "No image built yet — this host has no devcontainer builder, so sessions boot the stock agent image."}
          </p>
        </div>
      )}
    </div>
  );
}
