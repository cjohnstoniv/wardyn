/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The member's own Getting Started — replaces MemberSetupNotice's one-line
// bounce. A single scrolling page of SectionCards (no rail, no stepper): what
// an admin already set up for you, then the things a member can actually DO
// (add a workspace, bring your own key on a LEGACY install, launch a run,
// connect a terminal), plus two purely informational notes (approvals, and —
// since #541 — model connections).
//
// Colour budget (CONSOLE-RULES): exactly one `default` (teal) action at a
// time — the first not-done actionable section's — everything else outline;
// zero teal once every actionable section is done. "What's set up for you"
// and "Approvals you can decide" are informational (no done state) and never
// enter that computation.
//
// #541 (fix review) — "Your model key" (your-model-key.tsx, model-key-state.ts)
// is NOT retired: a real provider block makes every provider a person may use
// a row on a SEPARATE page (Your account, packet MP-D's own drawing), and
// this page then keeps only the glance-level summary chip
// (connectionsSummary, lib/model-connections.ts) with no in-page action. But
// #548 has not yet converted every install to a provider block, so a legacy
// install (no `model_providers` at all — every shared or per_user roster
// install main still carries) keeps "Your model key" as the ONLY door it ever
// had, and the summary chip falls back to legacySummary, the retired-in-name-
// only card's own model_access/llm_ready reading. `providerMode` decides
// which world a given load is in.
import * as React from "react";
import { Link, useSearchParams } from "react-router-dom";
import {
  AlertTriangle,
  FolderOpen,
  KeyRound,
  Rocket,
  ShieldCheck,
  Terminal,
} from "lucide-react";
import { Button, buttonVariants } from "../../ui/button";
import {
  Chip,
  DoneChip,
  SectionCard,
  SectionLabel,
} from "../../wardyn/primitives";
import { EPISODES_COPY as EP, MEMBER_GETTING_STARTED as T } from "../../wardyn/copy";
import { connectionRows, connectionsSummary, legacySummary } from "../../../lib/model-connections";
import { MODEL_ACCESS_AGENT, isPerUserSsoRow } from "../../../lib/model-access";
import { CONNECTIONS } from "../../wardyn/copy/door";
import { useModelAccessDoor } from "../../wardyn/model-access-context";
import { ADO } from "../../../lib/ado-entra-copy";
import { useAdoConnect } from "../../../lib/hooks/use-ado-connect";
import { scmAccessCause, scmAccessChip, scmAccessNeedsConnect } from "../../../lib/scm-access-display";
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
import { YourModelKey, modelKeyProvider } from "./your-model-key";
import { modelKeyState } from "./model-key-state";
import type { AgentRun, SetupStatus } from "../../../lib/types";
import { DEMOS, type Demo } from "../demos/demo-catalog";
import { ceilingNarrows, walkableDemos } from "../setup/steps";
import { useViewAccess } from "../../wardyn/console-view";

// M-6 (D5, admin-member-modes-design.md §4.8): a demo is a sandbox run, a
// user act, so it renders here now — reusing the funnel's own DemoDetail
// (setup/demos-step.tsx) unchanged; lazy, same reasoning setup-screen.tsx
// used to lazy-load it: each demo pulls AttachTerminal → xterm, and this page
// should not pay for that until a demo is actually opened.
const DemoDetail = React.lazy(() => import("../setup/demos-step"));

type Variant = "default" | "outline";

// The state -> label map MOVED to workspace-providers-copy.ts: the Agents tab's
// admin-own chip renders the same five and had its own copy, and BOTH fell back
// to MODEL_ACCESS_NOT_CONFIGURED for anything else — one root cause, one table.
// A MISS is "no chip", never a default label (see its doc comment).

export function MemberGettingStarted() {
  const [searchParams] = useSearchParams();
  // M-6 (D5): "url" is D1, the single-operator install — the same admin
  // token runs both views, so there is no ceiling for a demo run of theirs
  // to fall under and every demo stays fully interactive. Every other access
  // tier is a real member (or an SSO admin viewing the member page) whose
  // OWN runs are bound by boundUserSpec — ceilingNarrows below decides
  // per-demo which ones that would actually rewrite.
  const ceilingApplies = useViewAccess() !== "url";
  const [status, setStatus] = React.useState<SetupStatus | null>(null);
  const [retryTick, setRetryTick] = React.useState(0);
  // The legacy (no model-providers block) member's own AWS sign-in (C4.3)
  // opens the shell's one door (#544 — this page mounted its own pane
  // before). Restored (fix review on #541): "Your model key" is the ONLY
  // legacy install's door until #548 converts every install to a provider
  // block, and the door is what its own Sign-in button opens.
  const door = useModelAccessDoor();
  const openAwsDoor = () => door.openDoor({ for: { login: "aws" }, onSignedIn: () => setRetryTick((n) => n + 1) });

  React.useEffect(() => {
    let active = true;
    // getSetupStatus only ever rejects on a 401, already routed to the
    // module-level onUnauthorized handler (core.ts) before it gets here.
    void setupApi.getSetupStatus().then((s) => {
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
    // eslint-disable-next-line react-hooks/exhaustive-deps -- run once on mount; reload is stable (useCallback([]))
  }, []);

  // The member's own secret names, for the restored "Your model key" card
  // (legacy path only) — fetched once here so a Save/Remove inside it calls
  // back to re-fetch, one source of truth.
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
  // operator-context.tsx's UserDriveMeta. null for a member with none, for
  // an older daemon, and for a read that failed: all three render as today's
  // page, no chip and no sentence.
  //
  // The door is read here too. This page does not merely NAME the allocation —
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

  // Restored (fix review on #541): a real provider block makes Your account's
  // Your model connections the ONLY door (MP-D's own drawing) — every legacy
  // install (no provider block, since the admin funnel writes none until
  // #548 lands) keeps "Your model key" as the door it always had. Computed
  // before `actionable` and `connectionsChip` below since both branch on it.
  //
  // `!= null` (not `!!`): the server sends model_providers with no
  // `omitempty` now (#541 fix review), so a real block granting this caller
  // nothing reads `[]` — still provider mode, just the "No providers" state
  // — never the same wire shape as no block at all (`null`/absent).
  const providerMode = status?.model_providers != null;
  // legacyMode additionally requires `status` itself to have loaded: while it
  // is null (the pre-fetch window), providerMode already reads false, and
  // rendering "Your model key" then would flash it for every install,
  // including a real-provider one, until the read resolves.
  const legacyMode = !!status && !providerMode;

  // X3-F3: the summary chip and the pane must agree on WHICH key — the name
  // follows the org's agent roster, not a hardcoded provider. Unused (and the
  // card unrendered) under a real provider block.
  const modelKeyProviderRow = modelKeyProvider(status?.harnesses);
  const hasOwnKey = mine?.includes(modelKeyProviderRow.secretName) ?? false;
  // Appendix A finding 2 — a per_user roster row is graded on THIS caller's
  // own model_access, never the deployment-wide llmReady.
  const modelKeyDone =
    !unreachable &&
    modelKeyState({
      hasOwn: hasOwnKey,
      llmReady,
      modelAccess: status?.model_access,
      credentialSource: modelKeyProviderRow.credentialSource,
      mechanism: modelKeyProviderRow.mechanism,
    }).done;

  const workspaceDone = !unreachable && !wsLoading && workspaces.length > 0;
  const firstRunDone = !unreachable && (ownRuns?.length ?? 0) > 0;
  const connectDone = !unreachable && (sshKeyCount ?? 0) > 0;

  // Colour budget over the ACTIONABLE sections, in page order. "model-key"
  // enters the rotation only while its own card is actually on screen (the
  // legacy path) — under a provider block it has no card to hand the one
  // `default` slot to. The others (setup summary, approvals, and — since
  // #541 — model connections, which sends the person to a SEPARATE page
  // rather than an in-page action) are informational and never win it.
  const actionable: { key: string; done: boolean }[] = [
    { key: "workspace", done: workspaceDone },
    ...(legacyMode ? [{ key: "model-key", done: modelKeyDone }] : []),
    { key: "first-run", done: firstRunDone },
    { key: "connect-tools", done: connectDone },
  ];
  const firstNotDone = actionable.find((s) => !s.done)?.key;
  const variantFor = (key: string): Variant =>
    key === firstNotDone ? "default" : "outline";

  const strongest = status
    ? strongestAvailable(status.runner.confinement_classes)
    : undefined;

  // #541 (§5.4, packet MP-D): the SAME predicate the Your account page's own
  // header chip reads (model-connections-card.tsx) — one Ready/Needs
  // you/Not-set-up answer, never two independently-computed copies, WHEN
  // there is a provider block to read. Fix review: an install with none
  // falls back to legacySummary, the "Your model key" card's own
  // model_access/llm_ready reading — connectionsSummary's rows are always
  // empty there, and "Not set up by your admin" over a working shared
  // credential was the regression this fixes. null while `status` itself
  // hasn't loaded yet: no claim before there is an answer to make one from.
  const connectionsChip = status ? (providerMode ? connectionsSummary(connectionRows(status)) : legacySummary(status)) : null;
  // The per_user roster row (the pre-provider per-person AWS-SSO lane) — the
  // ONLY legacy shape SETUP_SUMMARY_HELPER's "shared credentials" claim is
  // false for; a real provider block gets its OWN canon lede (CONNECTIONS.LEDE,
  // copy/door.ts) below rather than reusing this one; add no new string.
  const isPerUserModelAccess = !!status?.harnesses?.some((h) => h.id === MODEL_ACCESS_AGENT && isPerUserSsoRow(h));

  // #386: the Azure DevOps chip + its fallback connect control — the same
  // popup-driven flow the New Run rail's launch door uses.
  const scmChip = status?.scm_access ? scmAccessChip(status.scm_access.state, status.scm_access.source, status.scm_access.cause) : null;
  const { connecting: adoConnecting, connect: adoConnect, connectFallback: adoConnectFallback, blockedUrl: adoBlockedUrl } = useAdoConnect();
  const handleAdoConnect = async () => {
    if (await adoConnect()) setRetryTick((n) => n + 1);
  };
  // review follow-up N1: the fallback link opens sign-in in a new tab; this
  // starts the SAME poll (bounded) so the chip still updates on return.
  const handleAdoFallbackClick = () => {
    void adoConnectFallback().then((ok) => {
      if (ok) setRetryTick((n) => n + 1);
    });
  };

  // Shape C (approved mock round 2026-08-31): the member's own path leads
  // (every member-audience episode — which now includes 13, whose lesson is
  // the member terminal), then the core "watch first" set.
  const memberEpisodes = EPISODES.filter((e) => e.audience === "member");
  const coreEpisodes = EPISODES.filter((e) => e.path === "core");

  // M-6 (D5): the same two Demo.section groups the admin funnel used to walk
  // (setup/steps.ts's PHASES, before M-6), gated by the same precondition
  // that used to drop an unmet one from that walk (walkableDemos — needsModel
  // without a connected model, needsSecret without that secret stored).
  // Owner ruling 2026-09-25: a demo the caller's own governance ceiling would
  // narrow (ceilingNarrows) is dropped from this list too, so it never
  // renders here — it is not offered watch-only (#850's own mock round
  // decides that, separately). KNOWN GAPS, all in #850.
  // internal/api/setup.go's redactSetupStatusForUser zeroes secrets.present
  // and providers for a caller the server answers as a user, so a
  // needsSecret demo (five of the eight "secrets" ones) is never offered to
  // an SSO user, and neither is a needsModel one unless the model access is a
  // managed subscription or Bedrock. A single-operator install (D1) is
  // unaffected by all of this — ceilingApplies is false, and its status is
  // never redacted.
  const walkable = new Set(walkableDemos(status).map((d) => d.id));
  const visible = (d: Demo) => walkable.has(d.id) && !(ceilingApplies && ceilingNarrows(d));
  const egressDemos = DEMOS.filter((d) => d.section === "egress" && visible(d));
  const secretsDemos = DEMOS.filter((d) => d.section === "secrets" && visible(d));
  // /demos and a shared link both redirect into a `?step=<id>` deep link
  // (App.tsx) — honor it here the same way the funnel used to: open that one
  // demo's row, silently ignoring an id this page doesn't offer (unmet
  // precondition, or not a real demo id) rather than erroring.
  const deepLinkedDemoId = searchParams.get("step");

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
                  {/* F7-F3/F7-F15: was warning text at 90% opacity — the
                      dilution gate now forbids diluting any guarded semantic
                      token. */}
                  <p className="mt-1 text-warning">{T.UNREACHABLE_BODY}</p>
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
                {/* #541 (§5.4): Ready / Needs you / Not set up by your admin
                    — the same chip Your account's own header carries. null
                    (status not loaded yet) claims nothing, same as every
                    other chip on this row. */}
                {connectionsChip && <Chip tone={connectionsChip.tone}>{connectionsChip.label}</Chip>}
                {/* #386: one more chip from the six states, a second subject
                    (§6.2 — "In the common case that is the whole of it: no
                    action line, no button"). */}
                {scmChip && <Chip tone={scmChip.tone}>{scmChip.label}</Chip>}
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
              {/* #386: `not_configured`'s cause line + CONNECT_ADO — the fallback
                  states only (§2.2/§7.5); `live` (every source) and
                  `shared_expired` render neither line nor button here, the
                  common case spending nothing (§0.1). */}
              {scmAccessNeedsConnect(status?.scm_access?.state) && status?.scm_access && (
                <>
                  <p className="mt-2 text-sm text-warning">{scmAccessCause(status.scm_access.cause)}</p>
                  <Button
                    size="sm"
                    variant="outline"
                    className="mt-3"
                    disabled={adoConnecting}
                    onClick={() => void handleAdoConnect()}
                  >
                    {ADO.CONNECT_ADO}
                  </Button>
                  {/* review finding F9: the browser refused the popup outright;
                      N1: the fallback link's own click also starts the poll. */}
                  {adoBlockedUrl && (
                    <p className="mt-1 text-sm text-muted-foreground">
                      {ADO.CONNECT_POPUP_BLOCKED}{" "}
                      <a
                        href={adoBlockedUrl}
                        target="_blank"
                        rel="noopener noreferrer"
                        className={buttonVariants({ variant: "outline", size: "sm" })}
                        onClick={handleAdoFallbackClick}
                      >
                        {ADO.CONNECT_POPUP_OPEN}
                      </a>
                    </p>
                  )}
                </>
              )}
              {status?.scm_access?.state === "shared_expired" && (
                <p className="mt-2 text-sm text-warning">{ADO.ACCESS_SHARED_EXPIRED_ACTION}</p>
              )}
              <p className="mt-3 text-sm text-muted-foreground">
                {providerMode
                  ? CONNECTIONS.LEDE
                  : isPerUserModelAccess
                    ? T.SETUP_SUMMARY_HELPER_PER_USER
                    : T.SETUP_SUMMARY_HELPER}
              </p>
              {/* The chip names the profile; this says what having one means.
                  Both render only when there IS one. */}
              {governanceProfile && (
                <p className="mt-1 text-sm text-muted-foreground">
                  {MEMBER.GS_BODY(governanceProfile)}
                </p>
              )}
              {/* Packet M-B (modes-b.html): your model connections live in
                  Your account. */}
              <p className="mt-3 text-sm text-muted-foreground">
                <Link to="/account" className="font-medium text-info hover:underline">
                  {T.MODEL_CONNECTIONS}
                </Link>{" "}
                {T.MODEL_CONNECTIONS_WHERE}
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

        {/* Restored (fix review on #541): the legacy install's own door — a
            real provider block makes Your account's Your model connections
            the only one (MP-D's own drawing), so this never renders beside
            it. `legacyMode` (not `!providerMode`) also waits for `status`
            itself to load, so the card never flashes on before the read
            resolves. */}
        {legacyMode && (
          <YourModelKey
            llmReady={llmReady}
            mine={mine}
            harnesses={status?.harnesses}
            modelAccess={status?.model_access}
            onSignInAws={openAwsDoor}
            /* U-13: while the door is open the card's own button is a second,
               live-looking way into the same one door. */
            signInOpen={door.open}
            known={!unreachable}
            variant={variantFor("model-key")}
            onChanged={loadSecrets}
          />
        )}

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

      {egressDemos.length > 0 && (
        <div className="mt-10">
          <SectionLabel>{T.DEMOS_EGRESS_TITLE}</SectionLabel>
          <div className="mt-2">
            {egressDemos.map((d) => (
              <DemoRow
                key={d.id}
                demo={d}
                barrierReady={!!strongest}
                githubAppReady={status ? !!status.secrets.github_app : true}
                defaultOpen={d.id === deepLinkedDemoId}
              />
            ))}
          </div>
        </div>
      )}
      {secretsDemos.length > 0 && (
        <div className="mt-6">
          <SectionLabel>{T.DEMOS_SECRETS_TITLE}</SectionLabel>
          <div className="mt-2">
            {secretsDemos.map((d) => (
              <DemoRow
                key={d.id}
                demo={d}
                barrierReady={!!strongest}
                githubAppReady={status ? !!status.secrets.github_app : true}
                defaultOpen={d.id === deepLinkedDemoId}
              />
            ))}
          </div>
        </div>
      )}

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

// M-6 (D5) — one demo row: title + Open/Close, the same "opens IN PLACE
// below the row" shape EpisodeRow (episode-card.tsx) already uses for
// episodes on this same page. `barrierReady`/`githubAppReady` mirror what
// setup-screen.tsx used to pass DemoDetail. `onJump` is left OFF DemoDetail
// (not a no-op) — the funnel's "finish the Environment step first" hand-off
// names a step this page doesn't have (the barrier is an admin fact, never a
// member's to set), and DemoDetail renders that hint as plain text with no
// onJump rather than a button that would go nowhere. `onDemoLaunched` is
// likewise a no-op: useDemoRuns (demo-runner.tsx) already writes the durable
// per-browser "launched" signal on its own; nothing on this page currently
// reads it back into a done marker.
function DemoRow({
  demo,
  barrierReady,
  githubAppReady,
  defaultOpen = false,
}: {
  demo: Demo;
  barrierReady: boolean;
  githubAppReady: boolean;
  defaultOpen?: boolean;
}) {
  const [open, setOpen] = React.useState(defaultOpen);
  // A ?step=<id> deep link opens a row below the page's three setup cards —
  // off-screen without this, since the pre-open renders with no scroll of
  // its own.
  const rowRef = React.useRef<HTMLDivElement>(null);
  React.useEffect(() => {
    if (defaultOpen) rowRef.current?.scrollIntoView({ block: "start" });
    // Deliberately once, on mount only — `open` toggling later (the member
    // closing/reopening the row by hand) must not re-scroll them away from
    // wherever they are.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);
  return (
    <div ref={rowRef} className="border-b border-border py-3 last:border-b-0">
      <div className="flex items-center gap-2">
        <span className="min-w-0 flex-1 truncate text-sm text-foreground">{demo.title}</span>
        {open ? (
          <Button variant="ghost" size="sm" onClick={() => setOpen(false)}>
            {EP.CLOSE}
          </Button>
        ) : (
          <Button variant="outline" size="sm" onClick={() => setOpen(true)}>
            {T.DEMO_OPEN}
          </Button>
        )}
      </div>
      {open && (
        <div className="mt-3">
          <React.Suspense fallback={<p className="text-sm text-muted-foreground">Loading demo…</p>}>
            <DemoDetail
              demo={demo}
              barrierReady={barrierReady}
              githubAppReady={githubAppReady}
              onDemoLaunched={() => {}}
            />
          </React.Suspense>
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
