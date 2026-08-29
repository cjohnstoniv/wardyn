/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The member's own Getting Started — replaces MemberSetupNotice's one-line
// bounce. A single scrolling page of SectionCards (no rail, no stepper): what
// an admin already set up for you, then four things a member can actually DO
// (add a workspace, bring your own key, launch a run, connect a terminal),
// plus one purely informational note about approvals.
//
// Colour budget (CONSOLE-RULES): exactly one `default` (teal) action at a
// time — the first not-done actionable section's — everything else outline;
// zero teal once every actionable section is done. "What's set up for you"
// and "Approvals you can decide" are informational (no done state) and never
// enter that computation; "Your model key" reports its own done-ness back
// via YourModelKey's onDoneChange (it owns its own fetch — see that file).
import * as React from "react";
import { Link } from "react-router-dom";
import { AlertTriangle, FolderOpen, KeyRound, Rocket, ShieldCheck, Terminal } from "lucide-react";
import { Button } from "../../ui/button";
import { Chip, DoneChip, SectionCard, SectionLabel } from "../../wardyn/primitives";
import { CC_META } from "../../wardyn/cc-meta";
import { strongestAvailable } from "../../wardyn/default-confinement";
import { useMemberLocalDirRoot } from "../../wardyn/operator-context";
import { setup as setupApi } from "../../../lib/api/setup";
import { secrets as secretsApi } from "../../../lib/api/secrets";
import { runs as runsApi } from "../../../lib/api/runs";
import { sshKeys as sshKeysApi } from "../../../lib/api/ssh-keys";
import { useWorkspaceList } from "../../../lib/use-workspace-list";
import { MEMBER_WORKSPACE } from "../../../lib/permissions-copy";
import { markMemberGettingStartedSeen } from "../setup/setup-gate";
import { episodesFor, MEMBER_SECTION_IDS } from "../../../lib/demo-videos";
import { EpisodeRow } from "./episode-card";
import { YourModelKey } from "./your-model-key";
import type { AgentRun, SetupStatus } from "../../../lib/types";

type Variant = "default" | "outline";

export function MemberGettingStarted({ onDone: _onDone }: { onDone: () => void }) {
  const [status, setStatus] = React.useState<SetupStatus | null>(null);
  const [retryTick, setRetryTick] = React.useState(0);

  React.useEffect(() => {
    let active = true;
    setupApi.getSetupStatus().then((s) => {
      if (active) setStatus(s);
    });
    return () => {
      active = false;
    };
  }, [retryTick]);

  const { workspaces, loading: wsLoading, error: wsError, reload: reloadWorkspaces } = useWorkspaceList();
  const memberLocalDirRoot = useMemberLocalDirRoot();
  React.useEffect(() => {
    reloadWorkspaces();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const [ownKeyOrProvided, setOwnKeyOrProvided] = React.useState(false);
  const [mine, setMine] = React.useState<string[] | null>(null);
  React.useEffect(() => {
    let active = true;
    secretsApi
      .listSecretsMine()
      .then((r) => active && setMine(r.mine))
      .catch(() => active && setMine([]));
    return () => {
      active = false;
    };
  }, []);

  const [ownRuns, setOwnRuns] = React.useState<AgentRun[] | null>(null);
  React.useEffect(() => {
    let active = true;
    runsApi
      .listRuns()
      .then((r) => active && setOwnRuns(r))
      .catch(() => {
        /* fail-closed: leave ownRuns null (not done), write no flag */
      });
    return () => {
      active = false;
    };
  }, []);

  const [sshKeyCount, setSshKeyCount] = React.useState<number | null>(null);
  React.useEffect(() => {
    let active = true;
    sshKeysApi
      .listKeys()
      .then((r) => active && setSshKeyCount(r.length))
      .catch(() => {
        /* fail-closed: leave sshKeyCount null (not done) */
      });
    return () => {
      active = false;
    };
  }, []);

  const unreachable = status?.unreachable === true;
  const llmReady = !unreachable && status?.llm_ready === true;
  const hasOwnKey = mine?.includes("anthropic-api-key") ?? false;

  const workspaceDone = !unreachable && !wsLoading && workspaces.length > 0;
  const firstRunDone = !unreachable && (ownRuns?.length ?? 0) > 0;
  const connectDone = !unreachable && (sshKeyCount ?? 0) > 0;

  // Mark the funnel "seen" the FIRST time this screen observes a non-empty
  // own-runs list — never on a failed fetch (ownRuns stays null, firstRunDone
  // stays false, this effect never fires).
  React.useEffect(() => {
    if (firstRunDone) markMemberGettingStartedSeen();
  }, [firstRunDone]);

  // Colour budget over the four ACTIONABLE sections, in page order. The other
  // two (setup summary, approvals) are informational and never win the one
  // `default` slot.
  const actionable: { key: string; done: boolean }[] = [
    { key: "workspace", done: workspaceDone },
    { key: "model-key", done: ownKeyOrProvided },
    { key: "first-run", done: firstRunDone },
    { key: "connect-tools", done: connectDone },
  ];
  const firstNotDone = actionable.find((s) => !s.done)?.key;
  const variantFor = (key: string): Variant => (key === firstNotDone ? "default" : "outline");

  const strongest = status ? strongestAvailable(status.runner.confinement_classes) : undefined;

  const memberEpisodes = MEMBER_SECTION_IDS.flatMap((id) => episodesFor(id));

  return (
    <div className="mx-auto w-full max-w-[880px] px-6 py-8">
      <h1 className="text-2xl font-semibold tracking-tight text-foreground">Getting started</h1>
      <p className="mt-2 text-muted-foreground">
        You&apos;re a member of this Wardyn. Your admin set the ceiling; you run inside it.
      </p>

      <div className="mt-8 space-y-4">
        <SectionCard title="What's set up for you">
          {unreachable ? (
            <div role="alert" className="rounded-lg border border-warning/40 bg-warning-subtle p-3 text-sm text-warning">
              <div className="flex items-start gap-2">
                <AlertTriangle className="mt-0.5 size-4 shrink-0" />
                <div>
                  <p className="font-medium">Couldn&apos;t reach Wardyn.</p>
                  <p className="mt-1 text-warning/90">
                    Nothing below is marked done until it can be checked — a broken connection is not a finished
                    step.
                  </p>
                </div>
              </div>
              <Button
                variant="outline"
                size="sm"
                className="mt-3"
                onClick={() => setRetryTick((n) => n + 1)}
              >
                Retry
              </Button>
            </div>
          ) : (
            <>
              <div className="flex flex-wrap gap-2">
                {strongest && <Chip tone="neutral">Barrier · {CC_META[strongest].label}</Chip>}
                {hasOwnKey ? (
                  <Chip tone="success">Model access · Your key</Chip>
                ) : llmReady ? (
                  <Chip tone="success">Model access · Provided by your admin</Chip>
                ) : null}
                {status?.auth.mode === "sso" && <Chip tone="info">Sign-in · SSO</Chip>}
              </div>
              <p className="mt-3 text-sm text-muted-foreground">
                Your admin configured the barrier, network and shared credentials. Your runs inherit them.
              </p>
            </>
          )}
        </SectionCard>

        <SectionCard
          title="Add your workspace"
          Icon={FolderOpen}
          right={workspaceDone ? <DoneChip /> : undefined}
        >
          <p className="text-sm text-muted-foreground">
            A repo or directory a run can attach. Runs can only attach what is listed here.
          </p>
          <p className="mt-1 text-sm text-muted-foreground">{memberLocalDirHint(memberLocalDirRoot)}</p>
          {wsError && <p className="mt-1 text-xs text-danger">Couldn&apos;t check your workspaces.</p>}
          {!workspaceDone && (
            <Button asChild variant={variantFor("workspace")} size="sm" className="mt-3">
              <Link to="/workspaces">Add workspace</Link>
            </Button>
          )}
        </SectionCard>

        <YourModelKey
          llmReady={llmReady}
          known={!unreachable}
          variant={variantFor("model-key")}
          onDoneChange={setOwnKeyOrProvided}
        />

        <SectionCard title="Your first run" Icon={Rocket} right={firstRunDone ? <DoneChip /> : undefined}>
          <p className="text-sm text-muted-foreground">Launch a governed run against your workspace.</p>
          <p className="mt-1 text-sm text-muted-foreground">
            Your policy is clamped to your admin&apos;s ceiling. Preflight shows exactly what launch will do —
            read its warnings before you go.
          </p>
          {!firstRunDone && (
            <Button asChild variant={variantFor("first-run")} size="sm" className="mt-3">
              <Link to="/runs/new">New run</Link>
            </Button>
          )}
        </SectionCard>

        <SectionCard title="Approvals you can decide" Icon={ShieldCheck}>
          <p className="text-sm text-muted-foreground">
            When one of your runs reaches a host that isn&apos;t on the list, it holds at the door.
          </p>
          <p className="mt-1 text-sm text-muted-foreground">
            You decide — once, for this run, until, or always. Credential and tool-call approvals stay with your
            admin.
          </p>
          <Button asChild variant="outline" size="sm" className="mt-3">
            <Link to="/approvals">Open approvals</Link>
          </Button>
        </SectionCard>

        <SectionCard title="Connect your tools" Icon={Terminal} right={connectDone ? <DoneChip /> : undefined}>
          <p className="text-sm text-muted-foreground">Attach from your own terminal or editor over SSH.</p>
          <p className="mt-1 text-sm text-muted-foreground">
            Register a key once: <code className="rounded bg-muted px-1 py-0.5 font-mono text-xs">wardyn ssh-key ensure</code>
          </p>
          {!connectDone && (
            <Button asChild variant={variantFor("connect-tools")} size="sm" className="mt-3">
              <Link to="/ssh-keys">
                <KeyRound className="size-3.5" /> Add SSH key
              </Link>
            </Button>
          )}
        </SectionCard>
      </div>

      {memberEpisodes.length > 0 && (
        <div className="mt-10">
          <SectionLabel>Watch</SectionLabel>
          <div className="mt-2">
            {memberEpisodes.map((e) => (
              <EpisodeRow key={e.id} episode={e} />
            ))}
          </div>
        </div>
      )}
    </div>
  );
}

// One fact, one string — never a second copy of the same root-boundary
// sentence (permissions-copy.ts's own note on ROOT_HINT/LOCAL_DIR_UNAVAILABLE_BODY).
function memberLocalDirHint(root: string | null): string {
  return root ? MEMBER_WORKSPACE.ROOT_HINT(root) : MEMBER_WORKSPACE.LOCAL_DIR_UNAVAILABLE_BODY;
}
