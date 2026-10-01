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
import type { AgentRun, AuditEvent, RunEnding, RunEndingKind } from "../../../lib/types";
import type { HeldCredential, HeldCredentials } from "../../../lib/held-credentials";
import { runEndingFromAudit } from "../../../lib/api/audit";
import { absoluteTime } from "../../../lib/format";
import { AGENTS } from "../../../lib/workspace-providers-copy";
import { Button } from "../../ui/button";
import { Mono } from "../../wardyn/code-block";
import { MODEL_ACCESS_RUN_DOOR } from "../../wardyn/model-access-copy";
import { useClaimModelAccessDoor, useModelAccessDoor, useShellSetupStatus } from "../../wardyn/model-access-context";
import { useOperatorResolved } from "../../wardyn/operator-context";
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

// What a KILLED run says, by what its trail proves (#1487; runEndingFromAudit's
// `evidence`). Each outcome states only what Wardyn knows: a clean kill never
// claims what a teardown it did not see would have done to an upstream secret,
// a failed step is never paired with "scratch is gone", and a run with no kill
// record says it cannot confirm — it does not invent who, when or how.
const KILL_AUDIT_ROW = "The audit row names who killed it and the reason they gave.";
const KILL_START_NEW = "Start a new run if the work still needs doing — a killed run cannot resume.";
const KILL_AGAIN = "Kill it again to run teardown once more; the cascade is safe to repeat.";
const KILL_SCRATCH_GONE = "Files written to a mounted workspace are still on the host; scratch is gone.";
// run.kill carries outcome "failure" (plus a run.revoke/failure row) when
// KillSandbox, the identity revoke or the broker revoke failed —
// internal/api/runs_lifecycle.go's HONEST OUTCOME: the run IS marked KILLED,
// but the sandbox may still be live and a minted token may still be valid.
const KILL_PARTIAL_AUDIT_ROW = "The audit row names the step that failed — read it before treating this run as contained.";
const KILL_PARTIAL_RETRY = "Killing it again re-runs the same teardown; the cascade is safe to repeat.";
const KILL_PARTIAL_SCRATCH =
  "Whether scratch files remain is not confirmed. Files written to a mounted workspace are still on the host.";

// What Wardyn cannot take back from a run it has killed: a GitHub token already
// minted (it lives out its TTL), a git token or SSH key (stored upstream), an
// environment secret (resolved into the sandbox at launch). Facts from
// lib/held-credentials.ts — kind and host only.
const GITHUB_HELD = "A GitHub token it already held stays valid until it expires, within an hour.";
const ENV_SECRET_HELD =
  "Wardyn can't revoke a secret this run received as an environment variable — rotate it where it was issued.";
const HELD_UNREADABLE = "Wardyn couldn't read which credentials this run held.";
const HELD_RANK: Record<HeldCredential["kind"], number> = { github_token: 0, git_pat: 1, ssh_key: 2, env_secret: 3 };

function heldLines(held: HeldCredentials | undefined): string[] {
  if (!held) return []; // still loading: say nothing rather than "couldn't read"
  if (!held.readable) return [HELD_UNREADABLE];
  return [...held.items]
    .sort((a, b) => HELD_RANK[a.kind] - HELD_RANK[b.kind])
    .map((c) =>
      c.kind === "github_token"
        ? GITHUB_HELD
        : c.kind === "env_secret"
          ? ENV_SECRET_HELD
          : `Wardyn can't revoke the ${c.kind === "git_pat" ? "git token" : "SSH key"} this run used — it stays live until you rotate it on ${c.host}.`,
    );
}

function killedCopy(evidence: RunEnding["evidence"], held: HeldCredentials | undefined): EndingCopy {
  if (evidence === "partial") {
    return {
      happened: (elapsed) =>
        `An operator killed this run ${elapsed} in, and a teardown step failed. The state is KILLED, but teardown is not confirmed: the sandbox may still be live, and a credential it held may still work.`,
      todo: [KILL_PARTIAL_AUDIT_ROW, KILL_PARTIAL_RETRY, KILL_PARTIAL_SCRATCH, ...heldLines(held)],
    };
  }
  if (evidence === "unknown") {
    return {
      happened: () =>
        "This run is marked KILLED, but its audit trail has no kill record, so Wardyn can't confirm who killed it or whether teardown finished.",
      todo: [KILL_AGAIN, KILL_START_NEW],
    };
  }
  return {
    happened: (elapsed) =>
      `An operator killed this run ${elapsed} in. Wardyn stopped and removed the sandbox and will issue it no more credentials.`,
    todo: [KILL_AUDIT_ROW, KILL_SCRATCH_GONE, ...heldLines(held), KILL_START_NEW],
  };
}

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
      {owner && <OpenInUserView />}
    </div>
  );
}

export function RunFailureBlock({
  run,
  audit,
  held,
  onGoAudit,
}: {
  run: AgentRun;
  /** The trail the run page already fetched. */
  audit: AuditEvent[];
  /** Credentials the run held that a kill cannot revoke (#1487); absent while loading. */
  held?: HeldCredentials;
  /** Switch to the Audit tab. */
  onGoAudit: () => void;
}) {
  const ending = runEndingFromAudit(run.state, audit);
  const door = useModelAccessDoor();
  const credential = ending?.kind === "credential";
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
  const copy = ending.kind === "killed" ? killedCopy(ending.evidence, held) : ENDING_COPY[ending.kind];
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
      data-evidence={ending.evidence}
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

      {credential && ending.provider && <ProviderDoor run={run} provider={ending.provider} />}


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
        {ending.action && ending.evidence !== "unknown" && (
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
