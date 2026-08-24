/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// RecordPane's stateless session chrome: the stage-chip table and its two
// honesty caveats, the detected-command pills, the auth line, and the small
// note/pill primitives. Split out of record-pane.tsx (which the 0.6 merge
// pushed past the 1000-line file gate) purely by seam — these take props and
// render, they read none of the pane's state.

import { Info, ShieldCheck } from "lucide-react";
import type { RecordResult } from "../../../lib/types";
import type { SessionStage } from "./session-helpers";
import { Chip } from "../../wardyn/primitives";
import { Mono } from "../../wardyn/code-block";
import { CopyButton } from "../../wardyn/copy-button";

// Same table+one-Chip idiom as primitives.tsx's runStateMeta: one row per
// SessionStage instead of a 42-line if-chain of near-identical Chips.
const STAGE_CHIP_META: Record<
  SessionStage,
  { tone: "info" | "danger" | "success" | "neutral" | "warning"; label: string; pulse?: boolean }
> = {
  recording: { tone: "info", label: "Recording…", pulse: true },
  replaying: { tone: "info", label: "Replaying confined…", pulse: true },
  record_failed: { tone: "danger", label: "Record failed" },
  replay_failed: { tone: "danger", label: "Replay failed" },
  // Overridden by the settled entry's own verdict below — see replayedChipMeta.
  // The table row is the LEGACY (verdict-less) case, so its tone is neutral.
  replayed: { tone: "neutral", label: "Replayed confined" },
  recorded: { tone: "neutral", label: "Recorded" },
};

// "Replayed clean" means clean FOR WHAT WAS REPLAYED: reconcileRecordRun
// finalizes on ANY terminal state, so a replay the operator ends 30 seconds in
// earns the same verdict as a full one. Non-negotiable on the chip that claims
// the green — same discipline as the kernel-blind caveat below.
const CLEAN_SCOPE_CAVEAT =
  "Clean for what was replayed — a replay you ended early earns the same verdict as a full one, so this speaks only for the steps that actually ran.";
// The CC3 kernel-blind caveat, in the same words RecordReviewCard's HonestyNote
// uses (a hardware VM the syscall sensor can't see into) — egress is proxy-side
// and still complete, which is exactly what the verdict is derived from.
const KERNEL_BLIND_CAVEAT =
  "This ran inside a hardware VM (Vault/CC3); the syscall sensor can't see into it, so exec, file-write, and connect observations may be incomplete. Egress (proxy-side) is still complete.";

// The settled-CONFINED chip is the loop's verdict, so it's the one stage whose
// chip reads the record result instead of the stage alone: the server stamps
// `clean`/`caught` on a confined entry (recordmode.CleanReplay). Absent `clean`
// is an OLD ROW — unknown, not "not clean" — so it keeps today's wording with
// the tone dropped to neutral rather than claiming a green nothing proved.
function replayedChipMeta(rr: RecordResult): { tone: "success" | "warning" | "neutral"; label: string; title: string } {
  const blind = rr.kernel_sensor_blind ? " " + KERNEL_BLIND_CAVEAT : "";
  if (rr.clean === true) return { tone: "success", label: "Replayed clean", title: CLEAN_SCOPE_CAVEAT + blind };
  if (rr.clean === false) {
    // caught counts denied/held hosts only, but clean can also fail with ZERO
    // of those: an allow released by a live mid-replay approval (the standing
    // policy didn't earn it), or a truncated capture. "caught 0" would show a
    // warning over an empty list — name the real causes instead.
    if (!((rr.caught ?? 0) > 0)) {
      return {
        tone: "warning",
        label: "Replayed — not clean",
        title:
          "No host was denied or held, but this replay isn't clean: an allow was released by a live approval during the replay (the standing policy didn't earn it), or the capture was truncated. Replay again without approving live." +
          blind,
      };
    }
    return {
      tone: "warning",
      label: `Replayed — caught ${rr.caught}`,
      title:
        "Hosts were denied or held for approval during this replay — approve the ones this workspace legitimately needs and replay again." +
        blind,
    };
  }
  return {
    tone: "neutral",
    label: "Replayed confined",
    title: "This replay predates the clean/caught verdict — whether it was clean is unknown." + blind,
  };
}

export function stageChip(stage: SessionStage, rr?: RecordResult) {
  const m: (typeof STAGE_CHIP_META)[SessionStage] & { title?: string } =
    stage === "replayed" && rr ? replayedChipMeta(rr) : STAGE_CHIP_META[stage];
  return (
    <Chip tone={m.tone} dot={!!m.pulse} pulse={m.pulse} title={m.title} className="ml-auto">
      {m.label}
    </Chip>
  );
}

// DetectedHints — scan-detected commands as copy pills, guidance for what to run in
// an attached session (open record or confined replay). No-op when none detected.
export function DetectedHints({ commands }: { commands: string[] }) {
  if (commands.length === 0) return null;
  return (
    <div className="flex flex-wrap items-center gap-2 text-[0.6875rem] text-muted-foreground">
      <span>Detected commands:</span>
      {commands.slice(0, 4).map((c) => (
        <CopyPill key={c} text={c} />
      ))}
    </div>
  );
}

// AuthModeLine — the auth the session actually ran with (saved on the record result).
// Lets the operator SEE that a replay uses their configured provider, not a fallback.
export function AuthModeLine({ rr }: { rr: RecordResult }) {
  if (!rr.llm_mode || rr.llm_mode === "none") return null;
  const label =
    rr.llm_mode === "subscription" ? "Claude subscription" : rr.llm_mode === "api-key" ? "API key" : rr.llm_mode;
  return (
    <p className="flex flex-wrap items-center gap-1.5 text-[0.6875rem] text-muted-foreground" data-testid="session-auth-mode">
      <ShieldCheck className="size-3 shrink-0 text-success" />
      Model access: <span className="font-medium text-foreground">{label}</span>
      {rr.model ? (
        <>
          {" · "}
          <Mono className="text-foreground">{rr.model}</Mono>
        </>
      ) : null}
    </p>
  );
}

// A muted advisory note (the masking + sensor-blind honesty lines).
export function HonestyNote({ text }: { text: string }) {
  return (
    <p className="flex items-start gap-1.5 text-[0.6875rem] leading-snug text-muted-foreground">
      <Info className="mt-0.5 size-3 shrink-0" />
      <span>{text}</span>
    </p>
  );
}

// A small copy-to-clipboard command pill for the interactive suggested command.
// Exported so the demo-sandbox screen can reuse the exact same pill for its
// numbered curl instructions instead of duplicating it.
export function CopyPill({ text }: { text: string }) {
  return (
    <CopyButton
      text={text}
      label="Copy command"
      iconClassName="size-3"
      className="gap-1.5 rounded-md border border-border bg-surface-2/60 px-2 py-0.5 hover:text-foreground"
    >
      <Mono className="text-foreground">{text}</Mono>
    </CopyButton>
  );
}
