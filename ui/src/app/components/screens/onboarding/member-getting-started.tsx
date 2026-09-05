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
import { EPISODES_COPY as EP, MEMBER_GETTING_STARTED as T } from "../../wardyn/copy";
import { CC_META } from "../../wardyn/cc-meta";
import { strongestAvailable } from "../../wardyn/default-confinement";
import { useMemberLocalDirRoot, useUserDrive } from "../../wardyn/operator-context";
import { setup as setupApi } from "../../../lib/api/setup";
import type { MeUserDrive } from "../../../lib/api/health";
import { secrets as secretsApi } from "../../../lib/api/secrets";
import { runs as runsApi } from "../../../lib/api/runs";
import { policies as policiesApi } from "../../../lib/api/policies";
import { sshKeys as sshKeysApi } from "../../../lib/api/ssh-keys";
import { useWorkspaceList } from "../../../lib/use-workspace-list";
import { MEMBER } from "../../../lib/governance-copy";
import { DRIVE_MEMBER } from "../../../lib/user-drives-copy";
import { driveModeWord, driveSizeLabel } from "../../../lib/user-drives-display";
import { MEMBER_WORKSPACE } from "../../../lib/permissions-copy";
import { EPISODES } from "../../../lib/demo-videos";
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

  // The governance profile bounding THIS member, named by GET
  // /policies/default. undefined for a member with no assignment (the key is
  // omitted on the wire) and for a read that failed — either way §7.6's two
  // moments do not render, which is the absent-row doctrine: no assignment, no
  // chip, no line, today's card byte-for-byte.
  const [governanceProfile, setGovernanceProfile] = React.useState<string | undefined>(undefined);
  React.useEffect(() => {
    let active = true;
    policiesApi
      .getDefaultPolicy()
      .then((p) => active && setGovernanceProfile(p.governance_profile_name))
      .catch(() => {
        /* unknown stays unknown — never name a ceiling that could not be read */
      });
    return () => {
      active = false;
    };
  }, []);

  // This member's own drive (nil-means-no-allocation), off the shell's ONE GET
  // /me rather than a second one of this page's own — see
  // operator-context.tsx's UserDriveContext. null for a member with none, for
  // an older daemon, and for a read that failed: all three render as today's
  // page, no chip and no sentence.
  //
  // THE DOOR IS READ HERE TOO. This page does not merely NAME the allocation —
  // GS_DRIVE_BODY sends the member to New run to mount it, and with the
  // profile's DenyUserDrive limit shut that instruction is refused one page
  // load later by NR_DENIED, which names the profile. So a shut door renders
  // NEITHER moment, chip or sentence: the absent-row doctrine (§2.5) applied to
  // the door, since an offer withheld claims nothing while an offer that cannot
  // be taken is a claim that is false. The refusal itself stays where the offer
  // is, on the New Run card — this page never had a mount to refuse, only one
  // to stop advertising. A DENIED chip of its own would be new copy (§7.6
  // freezes a PAUSED twin and no denied one): FILED, not invented here.
  const { drive: allocatedDrive, deniedByProfile: driveDeniedBy } = useUserDrive();
  const userDrive = driveDeniedBy ? null : allocatedDrive;

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

  // Shape C (approved mock round 2026-08-31): the member's own path leads
  // (every member-audience episode — which now includes 13, whose lesson is
  // the member terminal), then the core "watch first" set.
  const memberEpisodes = EPISODES.filter((e) => e.audience === "member");
  const coreEpisodes = EPISODES.filter((e) => e.path === "core");

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
                {/* §7.6, and neutral like BARRIER_CHIP beside it: which
                    profile bounds you is a fact about this deployment, not a
                    success or a risk. */}
                {governanceProfile && (
                  <Chip tone="neutral">{MEMBER.GS_CHIP(governanceProfile)}</Chip>
                )}
                {/* §7.6, and neutral for the same reason as the two before
                    it: which drive is allocated to you is a fact about this
                    deployment, not a success. Absent when nothing is
                    allocated — no chip, no placeholder. */}
                {userDrive && <Chip tone="neutral">{driveChipLabel(userDrive)}</Chip>}
              </div>
              <p className="mt-3 text-sm text-muted-foreground">
                {T.SETUP_SUMMARY_HELPER}
              </p>
              {/* The chip names the profile; this says what having one means.
                  Both render only when there IS one. */}
              {governanceProfile && (
                <p className="mt-1 text-sm text-muted-foreground">
                  {MEMBER.GS_BODY(governanceProfile)}
                </p>
              )}
            </>
          )}
        </SectionCard>

        <SectionCard
          title={T.WORKSPACE_TITLE}
          Icon={FolderOpen}
          right={workspaceDone ? <DoneChip /> : undefined}
        >
          <p className="text-sm text-muted-foreground">{T.WORKSPACE_BODY}</p>
          {/* Placed in THIS card because this is the one place a drive and a
              workspace get conflated: the sentence says a drive is not one.
              Only when there IS a drive — with none there is nothing to
              distinguish it from. */}
          {userDrive && (
            <p className="mt-1 text-sm text-muted-foreground">{DRIVE_MEMBER.GS_DRIVE_BODY}</p>
          )}
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
          <SectionLabel>{EP.MEMBER_YOUR_PATH}</SectionLabel>
          <div className="mt-2">
            {memberEpisodes.map((e) => (
              <EpisodeRow key={e.id} episode={e} />
            ))}
          </div>
        </div>
      )}
      {coreEpisodes.length > 0 && (
        <div className="mt-6">
          <SectionLabel>{EP.GROUP_CORE}</SectionLabel>
          <div className="mt-2">
            {coreEpisodes.map((e) => (
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

// §7.6's chip, in BARRIER_CHIP's own `Label · value` shape. Paused wins over
// the size and the mode: an allocation an admin has disabled mounts nothing
// next run, so naming its size would describe storage this member cannot
// reach. size_mib = 0 takes the _NOSIZE twin rather than putting "No
// allocation shown" inside a member's sentence.
function driveChipLabel(drive: MeUserDrive): string {
  if (drive.paused) return DRIVE_MEMBER.GS_DRIVE_CHIP_PAUSED(drive.name);
  const mode = driveModeWord(drive.writable);
  const size = driveSizeLabel(drive.size_mib);
  return size
    ? DRIVE_MEMBER.GS_DRIVE_CHIP(drive.name, size, mode)
    : DRIVE_MEMBER.GS_DRIVE_CHIP_NOSIZE(drive.name, mode);
}
