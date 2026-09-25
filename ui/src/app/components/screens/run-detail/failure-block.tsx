/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Run failure block (M7(b)) — "What happened / What to do", above the terminal,
// for a run that ended badly.
//
// The gap: a FAILED run said what state it was in and nothing about why. The
// reason was in the audit trail all along — the CLI has read it since
// runFailureReason (cmd/wardyn/commands.go) — but the console left an operator
// to page through the trail themselves, or to call the API. A governance
// product whose failure states are only readable through its own REST API has
// not reported the failure.
//
// No new endpoint: the run page already fetches the trail for the Audit tab,
// and runEndingFromAudit (lib/api/audit.ts) reads the ending out of it.
//
// The honesty rule this block is built around: a cause this build does not
// recognise renders the state and the audit link ALONE, unless the run ROW
// itself carries a real reason (run.failure_hint, D9's pre-agent-start class
// — a mount failure etc. that never reaches the audit trail at all). No
// INVENTED reason either way, and the "What to do" list is dropped rather
// than filled with generic advice — a wrong instruction costs an operator
// more than no instruction. review R-01: failure_hint's header chip
// (run-detail-summary-header.tsx) is clipped to a handful of characters at
// 1280px; this is its other, unclipped home.
import * as React from "react";
import type { ReactNode } from "react";
import { ScrollText } from "lucide-react";
import type { AgentRun, AuditEvent, RunEndingKind } from "../../../lib/types";
import { runEndingFromAudit } from "../../../lib/api/audit";
import { absoluteTime } from "../../../lib/format";
import { AGENTS } from "../../../lib/workspace-providers-copy";
import { Button } from "../../ui/button";
import { Mono } from "../../wardyn/code-block";
import { MODEL_ACCESS_RUN_DOOR } from "../../wardyn/model-access-copy";
import { useClaimModelAccessDoor, useModelAccessDoor, useShellSetupStatus } from "../../wardyn/model-access-context";
import { useOperatorResolved, usePrincipal } from "../../wardyn/operator-context";
import { OpenInUserView, useConsoleMode } from "../../wardyn/console-view";
import { CONNECTIONS } from "../../wardyn/copy/door";
import { resolveDoor, type DoorTarget } from "../../../lib/model-access";
import { formatElapsed } from "../run-detail-summary-header";

// Copy change (M7): the two labels, the action, and the four reason bodies.
// Local to this file rather than copy.ts on purpose — nothing else renders
// them, and copy.ts owns the vocabulary that MUST agree across surfaces
// (CONSOLE-RULES §10). Sentence case, specific nouns, no filler.
const HAPPENED = "What happened";
const TODO = "What to do";
const OPEN_AUDIT = "Open audit trail";

type EndingCopy = {
  /** `elapsed` is the run's own lifetime up to the ending event. */
  happened: (elapsed: string) => string;
  /** Nodes, not strings, so a wire literal in a line can be <Mono> (§3). */
  todo: readonly ReactNode[];
};

const ENDING_COPY: Partial<Record<RunEndingKind, EndingCopy>> = {
  // run.build/failure is a BUILD, not a pull: a BYOI wrap, a request-level
  // devcontainer, or the workspace's own image (runs_create.go's buildFailed,
  // workspace_run_image.go's buildAudit). The server's own hint on the run is
  // "the run's sandbox image could not be built"; the builder's error rides
  // data.error, which the block renders verbatim above these lines.
  image: {
    happened: () =>
      "The sandbox image could not be built, so the run stopped before the agent started. There is no exit code because nothing ran.",
    todo: [
      "Fix the image or the devcontainer it builds from, then start a new run — the policy and workspace are unchanged.",
      "The audit row carries the builder's full output.",
    ],
  },
  selftest: {
    happened: () =>
      "The image selftest failed, so the run was refused before any task ran. The wrapped image is missing a tool the runner needs.",
    todo: [
      "Read the selftest output in the audit trail — it names the missing tool.",
      "Rebuild the base image with that tool, or pick a different one.",
      "Nothing was mounted and no credential was minted; there is nothing to clean up.",
    ],
  },
  killed: {
    happened: (elapsed) =>
      `An operator killed this run ${elapsed} in. The sandbox was torn down and the run's identity revoked; any credential it held stopped working at that moment.`,
    todo: [
      "The audit row names who killed it and the reason they gave.",
      "Files written to a mounted workspace are still on the host; scratch is gone.",
      "Start a new run if the work still needs doing — a killed run cannot resume.",
    ],
  },
  auto_stop: {
    happened: () =>
      "Nobody attached and nothing reached out, so the run hit its idle auto-stop window and shut itself down. This is the policy working, not a fault.",
    todo: [
      "Nothing, if you were done — the recording and the audit trail are kept.",
      <>
        Raise <Mono>auto_stop_after_sec</Mono> in the policy if the window is too short, or set it
        to <Mono>-1</Mono> for a session you plan to leave sitting.
      </>,
    ],
  },
  // `unknown` is deliberately absent — see the file header. run.failure_hint
  // (below, review R-01) covers it when the server sent one; there is still
  // no INVENTED copy for an unknown cause with no hint at all.
  //
  // `credential` is absent for a stronger reason: the SERVER's refusal is a
  // complete explanation — what the lane is, what state it is in, and that
  // nothing was substituted — and it is already on the run as failure_hint. A
  // "What happened" of our own would be that sentence in weaker words. The one
  // thing the console adds is the sign-in itself (0.7.6 Finding 3).
};

// A kill whose cascade did NOT fully succeed. run.kill carries outcome
// "failure" (plus a run.revoke/failure row) when KillSandbox, the identity
// revoke or the broker revoke failed — internal/api/runs_lifecycle.go's HONEST
// OUTCOME: the run IS marked KILLED, but the sandbox may still be live and a
// minted token may still be valid until its TTL. The clean-kill copy above
// asserts the opposite of all three, so this run gets its own.
const KILL_INCOMPLETE: EndingCopy = {
  happened: (elapsed) =>
    `An operator killed this run ${elapsed} in, and a teardown step failed. The state is KILLED, but the sandbox may still be live and a credential it held may still work.`,
  todo: [
    "The audit row names the step that failed — read it before treating this run as contained.",
    "Killing it again re-runs the same teardown; the cascade is safe to repeat.",
    "Files written to a mounted workspace are still on the host; scratch is gone.",
  ],
};

// The button each provider door gets on a failed run (#543, canon Table 2).
function doorButton(t: DoorTarget): { label: string; aria: string; note: string } {
  if (t.kind === "key") {
    return t.token
      ? { label: CONNECTIONS.ADD_TOKEN, aria: MODEL_ACCESS_RUN_DOOR.ADD_TOKEN_ARIA, note: MODEL_ACCESS_RUN_DOOR.NOTE_KEY }
      : { label: CONNECTIONS.ADD_KEY, aria: MODEL_ACCESS_RUN_DOOR.ADD_KEY_ARIA, note: MODEL_ACCESS_RUN_DOOR.NOTE_KEY };
  }
  return t.login === "anthropic"
    ? { label: CONNECTIONS.SIGN_IN_CLAUDE, aria: MODEL_ACCESS_RUN_DOOR.SIGN_IN_CLAUDE_ARIA, note: MODEL_ACCESS_RUN_DOOR.NOTE }
    : { label: AGENTS.SIGN_IN_AWS, aria: MODEL_ACCESS_RUN_DOOR.SIGN_IN_ARIA, note: MODEL_ACCESS_RUN_DOOR.NOTE };
}

// A provider run's credential refusal (design §5.7): the door of the run's OWN
// provider — the one its refusal names, never the one selected anywhere on
// screen (#146's ruling) — for the run's owner in the User view. Anyone else,
// and every run in the Admin view, reads whose credential it was and gets no
// door: a sign-in lands in the signer's own namespace and can never serve
// another person's run. The admin's own run is one click from its door.
function ProviderDoor({ run, provider }: { run: AgentRun; provider: string }) {
  const door = useModelAccessDoor();
  const { status } = useShellSetupStatus();
  const view = useConsoleMode();
  const resolved = useOperatorResolved();
  const owner = !!door.principal && run.created_by === door.principal;
  const yours = owner && view === "user";
  // null when this person has no door for it any more (removed, or no agent of
  // theirs uses it): the server's sentence stands alone.
  const target = yours ? resolveDoor(status, { provider }, "user") : null;
  useClaimModelAccessDoor(!!target);
  if (target) {
    const b = doorButton(target);
    return (
      <div className="mt-3 flex flex-wrap items-center gap-2">
        <Button size="sm" aria-label={b.aria} onClick={(e) => door.openDoor({ for: { provider }, returnTo: e.currentTarget })}>
          {b.label}
        </Button>
        <span className="text-xs leading-relaxed text-muted-foreground">{b.note}</span>
      </div>
    );
  }
  // Nothing while /me is in flight: its "" principal would tell the owner the
  // run was someone else's.
  if (yours || !resolved || !run.created_by) return null;
  return (
    <div className="mt-2 space-y-1">
      <p className="text-xs leading-relaxed text-muted-foreground">{MODEL_ACCESS_RUN_DOOR.NOT_OWNER(run.created_by)}</p>
      {owner && <OpenInUserView runId={run.id} />}
    </div>
  );
}

export function RunFailureBlock({
  run,
  audit,
  onGoAudit,
  adminView = false,
}: {
  run: AgentRun;
  /** The trail the run page already fetched. */
  audit: AuditEvent[];
  /** Switch to the Audit tab. */
  onGoAudit: () => void;
  /** M-7 (admin-member-modes-design.md §4.6): the admin monitor carries no
   *  credential door, even on the admin's own run — this block's failure
   *  sentence stays, but its sign-in button never renders there; on the
   *  admin's own per-user lane the switch link back to it does instead
   *  (QM-7). Defaults false: the user cockpit. */
  adminView?: boolean;
}) {
  const ending = runEndingFromAudit(run.state, audit);
  const door = useModelAccessDoor();
  const principal = usePrincipal();
  const credential = ending?.kind === "credential";
  // EVERY term is load-bearing, and each rules out a door that would repair
  // nothing:
  //  - the ending's own lane, not the viewer's: `reason` covers every declared
  //    mechanism (an OpenAI row's refusal included) while model_access grades
  //    Claude Code alone (Codex #14);
  //  - the roster TODAY: a sign-in repairs nothing for an agent that has since
  //    moved off bedrock_sso, and a captured session keeps grading after such a
  //    move;
  //  - `actionable`: a member under a shared credential, and a refusal whose
  //    renewal merely did not complete ("launch again in a moment", which
  //    grades live), both get the sentence and no button;
  //  - the VIEWER owns the run: the door is this person's own credential, so an
  //    admin reading a member's failed run must not be offered a sign-in that
  //    repairs nothing for that run (round-2 general S5). An empty principal is
  //    /me unresolved or a deployment with no OIDC — never matched against an
  //    equally empty created_by.
  //  - a provider run's refusal names its provider (#532): ProviderDoor below
  //    answers it instead, keyed by that provider alone (#543).
  const ownSignIn =
    credential && !ending.provider && ending.mechanism === "bedrock_sso" && !!principal && run.created_by === principal;
  const showDoor = !adminView && ownSignIn && door.bedrockSSO && door.actionable;
  // The admin view's stand-in on the admin's own run: the switch link, on the
  // per-user lane only (the shared lane's sign-in is the admin's own control,
  // and never a User-view door).
  const showSwitch = adminView && ownSignIn && door.perUser;
  // One primary recovery action per state per screen: while this block carries
  // the button the shell strip keeps its sentence and drops its own.
  useClaimModelAccessDoor(showDoor);
  // The context's status can be up to five minutes old, and a just-refused run
  // is exactly when the credential's state changed — so read it again, ONCE per
  // mount (the trail arrives after the first paint, so this fires when the
  // ending resolves, not necessarily on the first effect).
  const refresh = door.refresh;
  const refreshed = React.useRef(false);
  React.useEffect(() => {
    if (!credential || refreshed.current) return;
    refreshed.current = true;
    void refresh();
  }, [credential, refresh]);
  if (!ending) return null;
  const copy =
    ending.kind === "killed" && ending.outcome === "failure" ? KILL_INCOMPLETE : ENDING_COPY[ending.kind];
  // The ending event's own timestamp bounds the run's lifetime; without one
  // (a truncated trail) fall back to the run's last update, which is what the
  // command bar's own clock freezes at.
  const elapsed = formatElapsed(
    new Date(ending.time ?? run.updated_at).getTime() - new Date(run.created_at).getTime(),
  );

  return (
    <section
      // Hairline, not a warning tint: the state badge and the failure hint one
      // row above already carry the tone, and auto-stop is not a fault at all.
      className="shrink-0 rounded-lg border border-border bg-card p-3"
      aria-label={HAPPENED}
      data-testid="run-failure-block"
      data-ending={ending.kind}
    >
      {(copy || run.failure_hint) && (
        <>
          <p className="label-eyebrow">{HAPPENED}</p>
          {copy && (
            <p className="mt-1 text-xs leading-relaxed text-foreground">{copy.happened(elapsed)}</p>
          )}
          {/* review R-01/R-12: run.failure_hint's other home — the header
              chip (run-detail-summary-header.tsx) is hidden below 2xl
              entirely (review R-16); here the full server sentence always
              renders regardless of width. Gated on `!copy`: for a RECOGNISED
              ending (image/selftest/killed/auto_stop) `copy.happened` +
              `ending.detail` already say the same thing in the vocabulary
              that kind owns — printing failure_hint too would be the same
              fact stated three times. For `unknown` (D9's class) copy is
              undefined, so this is the ONLY line — still the real prose,
              never invented. */}
          {!copy && run.failure_hint && (
            <p className="mt-1 text-xs leading-relaxed text-foreground">{run.failure_hint}</p>
          )}
          {/* The failing step's OWN words, mono because it is a wire value
              (§3) — the same data.error the CLI's runFailureReason prints.
              Under an unrecognised ending there is no copy block at all, so
              this never appears without a cause naming it. */}
          {ending.detail && (
            <p className="mt-1 break-words text-muted-foreground">
              <Mono>{ending.detail}</Mono>
            </p>
          )}
          {copy && (
            <>
              <p className="label-eyebrow mt-3">{TODO}</p>
              <ul className="mt-1 space-y-0.5 text-xs leading-relaxed text-muted-foreground">
                {copy.todo.map((line, i) => (
                  <li key={i} className="flex gap-2">
                    <span aria-hidden="true">·</span>
                    <span>{line}</span>
                  </li>
                ))}
              </ul>
            </>
          )}
        </>
      )}

      {showDoor && (
        <div className="mt-3 flex flex-wrap items-center gap-2">
          {/* The page's one primary action, so the one `default` Button on this
              surface (CONSOLE-RULES §6) — the sentence above it is the server's
              and names no control. */}
          <Button size="sm" aria-label={MODEL_ACCESS_RUN_DOOR.SIGN_IN_ARIA} onClick={(e) => door.openDoor({ returnTo: e.currentTarget })}>
            {AGENTS.SIGN_IN_AWS}
          </Button>
          <span className="text-xs leading-relaxed text-muted-foreground">{MODEL_ACCESS_RUN_DOOR.NOTE}</span>
        </div>
      )}
      {credential && ending.provider && <ProviderDoor run={run} provider={ending.provider} />}

      {showSwitch && (
        <div className="mt-3 flex flex-wrap items-center gap-2">
          <OpenInUserView runId={run.id} />
        </div>
      )}

      <div className="mt-3 flex items-center gap-3">
        <Button variant="outline" size="sm" onClick={onGoAudit}>
          <ScrollText className="size-3.5" /> {OPEN_AUDIT}
        </Button>
        {/* 0.7.3 F7 removed this block's own clone door: the run HEADER now
            carries "Start a run like this one" for every terminal state (a
            strict superset of the 3 endings this block explains), so two
            doors would be redundant — and a Playwright strict-mode violation
            when both matched the same button name. */}
        {/* Wire values, so mono (CONSOLE-RULES §3): the action that carries the
            evidence, who the row names, and when. An unrecognised ending has
            no such row and states nothing here. */}
        {ending.action && (
          <span className="min-w-0 truncate font-mono text-meta text-muted-foreground">
            {[ending.action, ending.outcome, ending.actor, ending.time && absoluteTime(ending.time)]
              .filter(Boolean)
              .join(" · ")}
          </span>
        )}
      </div>
    </section>
  );
}
