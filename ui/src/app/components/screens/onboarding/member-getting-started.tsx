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
import {
  AGENTS,
  MODEL_ACCESS_ACTIONABLE,
  MODEL_ACCESS_CHIP_LABEL,
  modelAccessActionLine,
} from "../../../lib/workspace-providers-copy";
import { HarnessLoginPane } from "../settings/harness-login-pane";
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
import { modelKeyState, ownKeyApplies } from "./model-key-state";
import type { AgentRun, SetupStatus } from "../../../lib/types";

type Variant = "default" | "outline";

// The state -> label map MOVED to workspace-providers-copy.ts: the Agents tab's
// admin-own chip renders the same five and had its own copy, and BOTH fell back
// to MODEL_ACCESS_NOT_CONFIGURED for anything else — one root cause, one table.
// A MISS is "no chip", never a default label (see its doc comment).

// U-1 (W6 blind lens) — WHOSE credential the state describes, which the label
// table alone cannot say. Under a row that is NOT per_user the server still
// projects `live`/`expiring` (memberModelAccess, internal/api/modelaccess.go):
// that is the ADMIN's shared credential, graded for this member. Rendering
// MODEL_ACCESS_LIVE ("Your AWS sign-in") there claimed a sign-in the member does
// not have, directly above a card reading "Provided by your admin" and a New Run
// rail reading "Admin's credential". Every other state keeps the table's label:
// shared_expired already names the admin, and the per-person states are only
// ever emitted for a per_user row. A miss is still NO chip, never a default.
function modelAccessChip(
  state: string,
  perUser: boolean,
): { label: string; tone: "success" | "warning" } | null {
  if (!perUser && (state === "live" || state === "expiring")) {
    return { label: T.MODEL_ACCESS_PROVIDED_CHIP, tone: "success" };
  }
  const label = MODEL_ACCESS_CHIP_LABEL[state];
  return label ? { label, tone: state === "live" ? "success" : "warning" } : null;
}

export function MemberGettingStarted() {
  const [status, setStatus] = React.useState<SetupStatus | null>(null);
  const [retryTick, setRetryTick] = React.useState(0);
  // The member's own AWS sign-in pane (C4.3) — opens IN PLACE under the card,
  // per the mock; HarnessLoginPane is reused unchanged.
  const [awsLoginOpen, setAwsLoginOpen] = React.useState(false);

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
  // X3-F3: the summary chip and the pane must agree on WHICH key — the name
  // follows the org's agent roster, not a hardcoded provider.
  const modelKeyProviderRow = modelKeyProvider(status?.harnesses);
  const hasOwnKey = mine?.includes(modelKeyProviderRow.secretName) ?? false;
  const isPerUserModelAccess = modelKeyProviderRow.credentialSource === "per_user";
  // FIX PASS 1 (REVIEW-1.md H1) — hasOwnKey alone is not enough: a member's
  // own API key can never satisfy a per_user OR a shared-Bedrock row
  // (mechanismSatisfied refuses it on provider mismatch either way), so the
  // chip row's success chip, action line and Sign-in button must all ignore
  // a leftover `mine` write the same way the card already does.
  const ownKeyCounts =
    hasOwnKey &&
    ownKeyApplies({ credential_source: modelKeyProviderRow.credentialSource, mechanism: modelKeyProviderRow.mechanism });

  const workspaceDone = !unreachable && !wsLoading && workspaces.length > 0;
  // Appendix A finding 2 — ONE predicate for both the checklist and the
  // card (your-model-key.tsx): a per_user roster row is graded on THIS
  // caller's own model_access, never on the deployment-wide llm_ready.
  // U-9 (W6 blind lens): `mechanism` was the one input the card passed and this
  // call did not, so "ONE predicate" was two — under a SHARED Bedrock row with a
  // leftover own key the page graded "own" (done) while the card graded the
  // credential expired. Both readings now take the same row.
  const modelKeyDone =
    !unreachable &&
    modelKeyState({
      hasOwn: hasOwnKey,
      llmReady,
      modelAccess: status?.model_access,
      credentialSource: modelKeyProviderRow.credentialSource,
      mechanism: modelKeyProviderRow.mechanism,
    }).done;
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

  // U-1: the chip row's own model-access chip, or null for a state outside the
  // five (unknown ≠ not configured — the absent-row doctrine).
  const accessChip = status?.model_access
    ? modelAccessChip(status.model_access.state, isPerUserModelAccess)
    : null;

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
                {ownKeyCounts ? (
                  <Chip tone="success">{T.MODEL_ACCESS_OWN_CHIP}</Chip>
                ) : status?.model_access && status.model_access.state !== "not_applicable" ? (
                  // C4.5/C3: the chip stops reading llm_ready (a DEPLOYMENT
                  // fact) and reads THIS caller's own model-access state —
                  // success ONLY for live; the action rides its own line
                  // below, never inside the chip.
                  //
                  // A state outside the five renders NO CHIP: the old `??`
                  // fallback claimed "Not signed in" over, say, an
                  // `expired_renewable` credential that dispatch renews — a
                  // false claim with no CTA to act on it. Unknown ≠ not
                  // configured, and the server's own action line (below) is the
                  // one thing still worth rendering.
                  //
                  // not_applicable (finding 5) is excluded from this branch
                  // entirely, not merely from the label lookup: it is the
                  // admin-token principal's own answer ("this is a shared
                  // token, not a person"), so a TRUTHY model_access object
                  // must still fall through to the llm_ready arm below —
                  // otherwise the deployment-wide "Provided by your admin"
                  // chip that arm exists to show is lost under exactly the
                  // caller (automation, the shared token) most likely to hit it.
                  // #158 gives it its own chip instead (below, beside this
                  // fall-through) rather than folding it back into this branch.
                  //
                  // U-1: WHICH label is modelAccessChip's call, not the table's
                  // — a shared row's `live` is the admin's credential.
                  accessChip ? (
                    <Chip tone={accessChip.tone}>{accessChip.label}</Chip>
                  ) : null
                ) : llmReady && !isPerUserModelAccess ? (
                  // model_access absent (older daemon, or the fetch failed),
                  // or not_applicable — today's rendering, unchanged, EXCEPT
                  // (FIX PASS 1, REVIEW-1.md H1b) under a per_user row: that
                  // chip would contradict the card's per_user body/lede
                  // directly beside it. A shared row (Bedrock or otherwise)
                  // still gets it — llmReady is the correct deployment-wide
                  // answer there.
                  <Chip tone="success">{T.MODEL_ACCESS_PROVIDED_CHIP}</Chip>
                ) : null}
                {/* #158: not_applicable's own chip, beside whatever the
                    fall-through above rendered (or didn't) — the old code
                    rendered NOTHING here when llmReady was false, which read
                    as unknown rather than as this caller's real, deliberate
                    answer ("a mechanism, not a person"). */}
                {status?.model_access?.state === "not_applicable" && (
                  <Chip tone="neutral">{AGENTS.MODEL_ACCESS_NOT_APPLICABLE}</Chip>
                )}
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
              {/* The server's own words, verbatim, as the chip row's own line
                  — never reworded client-side (C4.5). */}
              {!ownKeyCounts && status?.model_access?.action && (
                <p className="mt-2 text-sm text-warning">{modelAccessActionLine(status.model_access)}</p>
              )}
              {/* not_configured / expired_signin / expiring are the per_user
                  states — the member's OWN sign-in. shared_expired (an
                  admin's dead credential) and live get no button: there is
                  either nothing to do, or nothing this member can do about it. */}
              {!ownKeyCounts &&
                status?.model_access &&
                MODEL_ACCESS_ACTIONABLE.has(status.model_access.state) &&
                (awsLoginOpen ? (
                  <div className="mt-3 max-w-md">
                    <HarnessLoginPane
                      provider="aws"
                      /* This CTA renders for the per_user states only, so the
                         org's access portal is the admin's stored one and the
                         server uses it regardless of what is typed — the member
                         is told, not asked. */
                      startURLManaged
                      onDone={() => {
                        setAwsLoginOpen(false);
                        setRetryTick((n) => n + 1);
                      }}
                      onCancel={() => setAwsLoginOpen(false)}
                    />
                  </div>
                ) : (
                  // outline: this card is informational (file header) and
                  // never enters the page's one-teal-at-a-time computation.
                  //
                  // U-13 (a11y): this page carries TWO buttons whose visible
                  // text is "Sign in to AWS" (this one and the card's), plus a
                  // plain-text action line saying the same words — a screen
                  // reader listing the buttons got the same name twice with
                  // nothing to choose by. The aria-label keeps the visible text
                  // and names the section it is in; it starts with the visible
                  // text so a by-name lookup still finds it.
                  <Button
                    size="sm"
                    variant="outline"
                    className="mt-3"
                    aria-label={T.SIGN_IN_AWS_ARIA_SUMMARY}
                    onClick={() => setAwsLoginOpen(true)}
                  >
                    {AGENTS.SIGN_IN_AWS}
                  </Button>
                ))}
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
                        className="font-medium text-info hover:underline"
                        onClick={handleAdoFallbackClick}
                      >
                        {ADO.CONNECT_ADO}
                      </a>
                    </p>
                  )}
                </>
              )}
              {status?.scm_access?.state === "shared_expired" && (
                <p className="mt-2 text-sm text-warning">{ADO.ACCESS_SHARED_EXPIRED_ACTION}</p>
              )}
              <p className="mt-3 text-sm text-muted-foreground">
                {isPerUserModelAccess ? T.SETUP_SUMMARY_HELPER_PER_USER : T.SETUP_SUMMARY_HELPER}
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
          harnesses={status?.harnesses}
          modelAccess={status?.model_access}
          onSignInAws={() => setAwsLoginOpen(true)}
          /* U-13: the pane opens in the card ABOVE this one and moves no focus,
             so while it is open the card's own button is a no-op that reads as
             a second, live way in. */
          signInOpen={awsLoginOpen}
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
