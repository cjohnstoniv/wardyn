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
// enter that computation. `mine` (the member's own secret names) is fetched
// ONCE here and passed to YourModelKey — it also drives this page's own
// "Model access" summary chip, so there is one round trip and one source of
// truth; a Save/Remove inside YourModelKey calls back to re-fetch it, so both
// readings flip together instead of drifting after a write.
import * as React from "react";
import { Link } from "react-router-dom";
import {
  AlertTriangle,
  FolderOpen,
  KeyRound,
  Rocket,
  ShieldCheck,
  Terminal,
} from "lucide-react";
import { Button } from "../../ui/button";
import {
  Chip,
  DoneChip,
  SectionCard,
  SectionLabel,
} from "../../wardyn/primitives";
import { MEMBER_GETTING_STARTED as T } from "../../wardyn/copy";
import { CC_META } from "../../wardyn/cc-meta";
import { strongestAvailable } from "../../wardyn/default-confinement";
import { useMemberLocalDirRoot } from "../../wardyn/operator-context";
import { setup as setupApi } from "../../../lib/api/setup";
import { secrets as secretsApi } from "../../../lib/api/secrets";
import { runs as runsApi } from "../../../lib/api/runs";
import { sshKeys as sshKeysApi } from "../../../lib/api/ssh-keys";
import { useWorkspaceList } from "../../../lib/use-workspace-list";
import { MEMBER_WORKSPACE } from "../../../lib/permissions-copy";
import { episodesFor, MEMBER_SECTION_IDS } from "../../../lib/demo-videos";
import { EpisodeRow } from "./episode-card";
import { YourModelKey } from "./your-model-key";
import type { AgentRun, SetupStatus } from "../../../lib/types";

type Variant = "default" | "outline";

export function MemberGettingStarted() {
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

  const {
    workspaces,
    loading: wsLoading,
    error: wsError,
    reload: reloadWorkspaces,
  } = useWorkspaceList();
  const memberLocalDirRoot = useMemberLocalDirRoot();
  React.useEffect(() => {
    reloadWorkspaces();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const [mine, setMine] = React.useState<string[] | null>(null);
  const loadSecrets = React.useCallback(() => {
    secretsApi
      .listSecretsMine()
      .then((r) => setMine(r.mine))
      .catch(() => setMine([]));
  }, []);
  React.useEffect(loadSecrets, [loadSecrets]);

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
  const modelKeyDone = !unreachable && (hasOwnKey || llmReady);
  const firstRunDone = !unreachable && (ownRuns?.length ?? 0) > 0;
  const connectDone = !unreachable && (sshKeyCount ?? 0) > 0;

  // Colour budget over the four ACTIONABLE sections, in page order. The other
  // two (setup summary, approvals) are informational and never win the one
  // `default` slot.
  const actionable: { key: string; done: boolean }[] = [
    { key: "workspace", done: workspaceDone },
    { key: "model-key", done: modelKeyDone },
    { key: "first-run", done: firstRunDone },
    { key: "connect-tools", done: connectDone },
  ];
  const firstNotDone = actionable.find((s) => !s.done)?.key;
  const variantFor = (key: string): Variant =>
    key === firstNotDone ? "default" : "outline";

  const strongest = status
    ? strongestAvailable(status.runner.confinement_classes)
    : undefined;

  const memberEpisodes = MEMBER_SECTION_IDS.flatMap((id) => episodesFor(id));

  return (
    <div className="mx-auto w-full max-w-[880px] px-6 py-8">
      <h1>{T.TITLE}</h1>
      <p className="mt-2 text-muted-foreground">{T.SUBTITLE}</p>

      <div className="mt-8 space-y-4">
        <SectionCard title={T.SETUP_SUMMARY_TITLE}>
          {unreachable ? (
            <div
              role="alert"
              className="rounded-lg border border-warning/40 bg-warning-subtle p-3 text-sm text-warning"
            >
              <div className="flex items-start gap-2">
                <AlertTriangle className="mt-0.5 size-4 shrink-0" />
                <div>
                  <p className="font-medium">{T.UNREACHABLE_TITLE}</p>
                  <p className="mt-1 text-warning/90">{T.UNREACHABLE_BODY}</p>
                </div>
              </div>
              <Button
                variant="outline"
                size="sm"
                className="mt-3"
                onClick={() => setRetryTick((n) => n + 1)}
              >
                {T.RETRY}
              </Button>
            </div>
          ) : (
            <>
              <div className="flex flex-wrap gap-2">
                {strongest && (
                  <Chip tone="neutral">
                    {T.BARRIER_CHIP(CC_META[strongest].label)}
                  </Chip>
                )}
                {hasOwnKey ? (
                  <Chip tone="success">{T.MODEL_ACCESS_OWN_CHIP}</Chip>
                ) : llmReady ? (
                  <Chip tone="success">{T.MODEL_ACCESS_PROVIDED_CHIP}</Chip>
                ) : null}
                {status?.auth.mode === "sso" && (
                  <Chip tone="info">{T.SIGNIN_SSO_CHIP}</Chip>
                )}
              </div>
              <p className="mt-3 text-sm text-muted-foreground">
                {T.SETUP_SUMMARY_HELPER}
              </p>
            </>
          )}
        </SectionCard>

        <SectionCard
          title={T.WORKSPACE_TITLE}
          Icon={FolderOpen}
          right={workspaceDone ? <DoneChip /> : undefined}
        >
          <p className="text-sm text-muted-foreground">{T.WORKSPACE_BODY}</p>
          <p className="mt-1 text-sm text-muted-foreground">
            {memberLocalDirHint(memberLocalDirRoot)}
          </p>
          {wsError && (
            <p className="mt-1 text-xs text-danger">{T.WORKSPACE_ERROR}</p>
          )}
          {!workspaceDone && (
            <Button
              asChild
              variant={variantFor("workspace")}
              size="sm"
              className="mt-3"
            >
              <Link to="/workspaces">{T.WORKSPACE_ACTION}</Link>
            </Button>
          )}
        </SectionCard>

        <YourModelKey
          llmReady={llmReady}
          mine={mine}
          known={!unreachable}
          variant={variantFor("model-key")}
          onChanged={loadSecrets}
        />

        <SectionCard
          title={T.FIRST_RUN_TITLE}
          Icon={Rocket}
          right={firstRunDone ? <DoneChip /> : undefined}
        >
          <p className="text-sm text-muted-foreground">{T.FIRST_RUN_BODY}</p>
          <p className="mt-1 text-sm text-muted-foreground">
            {T.FIRST_RUN_HINT}
          </p>
          {!firstRunDone && (
            <Button
              asChild
              variant={variantFor("first-run")}
              size="sm"
              className="mt-3"
            >
              <Link to="/runs/new">{T.FIRST_RUN_ACTION}</Link>
            </Button>
          )}
        </SectionCard>

        <SectionCard title={T.APPROVALS_TITLE} Icon={ShieldCheck}>
          <p className="text-sm text-muted-foreground">{T.APPROVALS_BODY}</p>
          <p className="mt-1 text-sm text-muted-foreground">
            {T.APPROVALS_HINT}
          </p>
          <Button asChild variant="outline" size="sm" className="mt-3">
            <Link to="/approvals">{T.APPROVALS_ACTION}</Link>
          </Button>
        </SectionCard>

        <SectionCard
          title={T.CONNECT_TITLE}
          Icon={Terminal}
          right={connectDone ? <DoneChip /> : undefined}
        >
          <p className="text-sm text-muted-foreground">{T.CONNECT_BODY}</p>
          <p className="mt-1 text-sm text-muted-foreground">
            {T.CONNECT_HINT_PREFIX}
            <code className="rounded bg-muted px-1 py-0.5 font-mono text-xs">
              {T.CONNECT_COMMAND}
            </code>
          </p>
          {!connectDone && (
            <Button
              asChild
              variant={variantFor("connect-tools")}
              size="sm"
              className="mt-3"
            >
              <Link to="/ssh-keys">
                <KeyRound className="size-3.5" /> {T.CONNECT_ACTION}
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
  return root
    ? MEMBER_WORKSPACE.ROOT_HINT(root)
    : MEMBER_WORKSPACE.LOCAL_DIR_UNAVAILABLE_BODY;
}
