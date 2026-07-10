/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// EnvironmentStep — the flagship Getting-started step: the protection matrix IS
// the tier picker. Tiers are selectable columns (Fence / Wall / Vault), the
// capabilities/protections are rows. Honesty rules preserved verbatim from the
// other tier surfaces: "Doesn't stop" is a permanent row, caveat cells reuse each
// tier's residual line, and the every-tier constant note appears exactly once.
//
// Pure/presentational: the orchestrator (setup-screen) owns the SetupStatus
// fetch, the persisted `selected`, and the host re-check that bumps
// `recheckToken`. This component only renders and reports clicks.
//
// Copy/data are REUSED, never re-authored:
//   - cc-meta.ts:  CC_META (label/tagline/mechanism/doesntProtect), CC_ORDER,
//     CC_MATRIX_ROWS, CC_MATRIX_WHERE, CONFINEMENT_CONSTANT_NOTE
//   - copy.ts:     RESIDUAL_PREFIX ("Doesn't stop:"), BTN.showSetupCommand
//   - setup-guide.ts: TIER_GUIDES (the `wardyn setup <tier>` commands)
//   - primitives / status-chip / barrier-strength-strip / tier-illustration
// The tier-state derivation (available / needs-setup / incompatible + the
// /dev/kvm reason) and the PICK_WHEN / NOT_DETECTED copy live only here — this
// step is the sole barrier picker in Getting started.
import * as React from "react";
import { Check, X, AlertTriangle, Copy, Info, AlertOctagon, Terminal } from "lucide-react";
import { Button } from "../../ui/button";
import { cn } from "../../ui/utils";
import { StatusChip } from "../../wardyn/status-chip";
import { Chip } from "../../wardyn/primitives";
import { BarrierStrengthStrip } from "../../wardyn/barrier-strength-strip";
import { TierIllustration } from "../../wardyn/tier-illustration";
import {
  CC_META,
  CC_ORDER,
  CC_MATRIX_ROWS,
  CC_MATRIX_WHERE,
  CONFINEMENT_CONSTANT_NOTE,
  type CCMark,
} from "../../wardyn/cc-meta";
import { RESIDUAL_PREFIX, BTN, type StatusKind } from "../../wardyn/copy";
import { Mono } from "../../wardyn/code-block";
import { TIER_GUIDES } from "./setup-guide";
import { useCopyToClipboard } from "../../../lib/use-copy-to-clipboard";
import type { ConfinementClass, SetupStatus } from "../../../lib/types";

type TierState = "ready" | "todo" | "incompatible";

// Each tier's friendly name wears its metal text token (theme.css metal ramp) —
// matches the design snapshot; same tokens the illustration/strength strip use.
const TIER_NAME_TINT: Record<ConfinementClass, string> = {
  CC1: "text-fence-fg",
  CC2: "text-wall-fg",
  CC3: "text-vault-fg",
};

// The strongest-COMPATIBLE recommendation, computed from the probe alone — a
// kvm-capable host recommends Vault even before its runtime is installed (its
// card just says "Needs setup"). NEVER demoted for a merely-uninstalled tier;
// only a hardware-impossible one is skipped. Exported so the orchestrator and
// tests share this one rule.
export function recommendedTier(status: SetupStatus): ConfinementClass {
  const available = new Set(status.runner.confinement_classes ?? []);
  const kvm =
    status.platform.kvm ?? !(status.platform.wsl || /darwin|mac/i.test(status.platform.os));
  const incompatible = (cc: ConfinementClass) => cc === "CC3" && !kvm && !available.has(cc);
  const compatible = CC_ORDER.filter((cc) => !incompatible(cc));
  return compatible[compatible.length - 1] ?? "CC1";
}

// Per-tier "pick this when…" guidance — the sole copy (the barrier picker lives
// only here).
const PICK_WHEN: Record<ConfinementClass, string> = {
  CC1: "Trying Wardyn out, or the code is your own — quickest start, works on any host.",
  CC2: "Real work on real repos — closes the Fence's holes so the agent never touches your kernel.",
  CC3: "Untrusted code or secrets nearby — the strongest box Wardyn can build.",
};

// The concrete "still not detected" line shown once a re-check has run and the
// tier is still missing.
const NOT_DETECTED: Partial<Record<ConfinementClass, string>> = {
  CC2: "Still not detected — gVisor's runsc runtime isn't listed in `docker info` runtimes yet.",
  CC3: "Still not detected — no kata runtime in `docker info` yet.",
};

export function EnvironmentStep({
  status,
  selected,
  recommended,
  onSelect,
  recheckToken = 0,
  rechecking = false,
}: {
  status: SetupStatus;
  selected: ConfinementClass | null;
  /** Override the internal strongest-compatible pick (tests / orchestrator). */
  recommended?: ConfinementClass;
  onSelect: (cc: ConfinementClass) => void;
  /** Increments each completed host re-check — drives the still-not-detected line. */
  recheckToken?: number;
  /** While a host re-check is in flight: chips show "Checking…", detail lines hide. */
  rechecking?: boolean;
}) {
  const classes = status.runner.confinement_classes ?? [];
  // No barrier can be built -> runs can't launch. Matrix stays visible read-only.
  const noDriver = status.runner.driver === "none";
  const noRunner = noDriver || classes.length === 0;
  const available = new Set(classes);
  const rec = recommended ?? recommendedTier(status);

  // Incompatible vs needs-setup is a HARDWARE fact (the backend probes /dev/kvm).
  // Only a genuinely KVM-less host marks Vault incompatible; an old daemon without
  // the kvm field falls back to the WSL/macOS heuristic rather than overclaiming.
  const kvmProbed = typeof status.platform.kvm === "boolean";
  const kvm =
    status.platform.kvm ?? !(status.platform.wsl || /darwin|mac/i.test(status.platform.os));
  const vaultIncompatibleReason = kvmProbed
    ? "Vault needs KVM virtualization and this host doesn't expose /dev/kvm — a hardware/VM limit no install can fix. (On WSL2, enable nested virtualization; on a laptop/desktop, enable virtualization in firmware; a containerized wardynd needs /dev/kvm mapped in.)"
    : "Vault likely can't run here — WSL/macOS hosts usually can't register a Kata runtime, and this daemon predates the /dev/kvm probe that would say for sure. Upgrade wardynd for a definitive answer.";

  const tierState = (cc: ConfinementClass): TierState => {
    if (available.has(cc)) return "ready";
    if (cc === "CC3" && !kvm) return "incompatible";
    return "todo";
  };
  const selectable = (cc: ConfinementClass) => !noRunner && tierState(cc) === "ready";
  const selectCell = (cc: ConfinementClass) => selectable(cc) && onSelect(cc);

  // WAI-ARIA radiogroup keyboard contract. Roving tabindex: the selected radio
  // (or the first selectable one when none is picked) is the single tab stop;
  // the rest are tabIndex=-1 and reached with arrows. Arrows move focus AND
  // selection to the next/previous SELECTABLE tier, wrapping and skipping
  // disabled ones. Refs let a keypress move DOM focus to the sibling radio.
  const radioRefs = React.useRef<Partial<Record<ConfinementClass, HTMLButtonElement | null>>>({});
  const selectableList = CC_ORDER.filter(selectable);
  const tabStop =
    selected && selectable(selected) ? selected : (selectableList[0] ?? CC_ORDER[0]);
  const moveSelection = (from: ConfinementClass, dir: 1 | -1) => {
    if (selectableList.length === 0) return;
    const i = selectableList.indexOf(from);
    const base = i === -1 ? 0 : i;
    const next = selectableList[(base + dir + selectableList.length) % selectableList.length];
    onSelect(next);
    radioRefs.current[next]?.focus();
  };
  const onRadioKeyDown = (e: React.KeyboardEvent, cc: ConfinementClass) => {
    if (noRunner) return;
    if (e.key === "ArrowRight" || e.key === "ArrowDown") {
      e.preventDefault();
      moveSelection(cc, 1);
    } else if (e.key === "ArrowLeft" || e.key === "ArrowUp") {
      e.preventDefault();
      moveSelection(cc, -1);
    }
  };

  // Column cell classes — the selected column gets a teal (--primary) ring built
  // from cell borders; non-selectable columns dim.
  const col = (cc: ConfinementClass, pos: "head" | "mid" | "foot") => {
    const sel = selected === cc;
    return cn(
      "px-4 align-top",
      pos === "head" ? "pt-4 pb-3" : "py-3",
      selectable(cc) && "cursor-pointer",
      sel && "bg-primary/5 border-x-2 border-primary",
      sel && pos === "head" && "border-t-2",
      sel && pos === "foot" && "border-b-2",
      !selectable(cc) && "opacity-70",
    );
  };

  return (
    <div className="space-y-5">
      {/* No-runner danger card — the matrix stays visible read-only below. */}
      {noRunner && (
        <div className="rounded-xl border border-danger/40 bg-danger-subtle p-5">
          <div className="flex items-start gap-3">
            <AlertOctagon className="mt-0.5 size-5 text-danger" aria-hidden />
            <div>
              <div className="text-foreground">No sandbox runner — runs can't launch.</div>
              <p className="mt-1 text-sm text-muted-foreground">
                {noDriver ? (
                  <>
                    Fix: start wardynd with <Mono>-runner docker</Mono> (built with -tags docker).
                  </>
                ) : (
                  "Fix: start the Docker daemon (or install a container runtime) so Wardyn can build a barrier, then re-check."
                )}
              </p>
            </div>
          </div>
        </div>
      )}

      <p className="text-sm text-muted-foreground">
        {noRunner
          ? "The tier matrix is shown for reference — tiers can't be selected until a runner is available."
          : "Weakest to strongest — pick a column to save it as the default barrier for new runs."}
      </p>

      {/* The picker: a radiogroup whose radios are the three column headers. */}
      <div
        role="radiogroup"
        aria-label="Barrier tier"
        className={cn(
          "overflow-x-auto rounded-xl border",
          noRunner && "pointer-events-none opacity-60",
        )}
      >
        <table className="w-full min-w-[720px] border-collapse text-sm">
          <thead>
            <tr>
              <th className="w-[220px] border-b bg-muted/40 px-4 py-3 text-left align-bottom font-normal">
                <span className="text-xs uppercase tracking-wide text-muted-foreground">
                  Barrier tier
                </span>
              </th>
              {CC_ORDER.map((cc) => {
                const st = tierState(cc);
                const sel = selected === cc;
                const canSelect = selectable(cc);
                const meta = CC_META[cc];
                return (
                  <th
                    key={cc}
                    // Header dead-zone fix: the padding around the radio also selects
                    // (selectCell no-ops for unselectable tiers).
                    onClick={() => selectCell(cc)}
                    className={cn("border-b text-left align-top font-normal", col(cc, "head"))}
                  >
                    {/* Exactly one radio per column; its accessible name contains the
                        tier name (meta.label) so `role=radio, name:/Fence/` selectors
                        resolve. Interactive controls (Show setup command / Copy) are
                        SIBLINGS below, never nested inside this button. */}
                    <button
                      type="button"
                      role="radio"
                      aria-checked={sel}
                      disabled={!canSelect}
                      aria-disabled={!canSelect}
                      tabIndex={cc === tabStop ? 0 : -1}
                      ref={(el) => {
                        radioRefs.current[cc] = el;
                      }}
                      onClick={(e) => {
                        // The th's dead-zone handler also selects; stop the bubble so
                        // a radio click fires onSelect exactly once.
                        e.stopPropagation();
                        if (canSelect) onSelect(cc);
                      }}
                      onKeyDown={(e) => onRadioKeyDown(e, cc)}
                      className={cn(
                        "w-full rounded-lg p-0 text-left",
                        "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary",
                        canSelect ? "cursor-pointer" : "cursor-default",
                      )}
                    >
                      <div className="flex items-start justify-between gap-2">
                        <TierIllustration cc={cc} title={meta.metaphor} />
                        {sel && (
                          <span className="inline-flex size-6 items-center justify-center rounded-full bg-primary text-primary-foreground">
                            <Check className="size-4" aria-hidden />
                          </span>
                        )}
                      </div>
                      <div className="mt-2 flex items-center gap-2">
                        <span className={TIER_NAME_TINT[cc]}>{meta.label}</span>
                        <span className="text-xs text-muted-foreground">{cc}</span>
                      </div>
                      <div className="mt-2">
                        <BarrierStrengthStrip tier={cc} />
                      </div>
                    </button>

                    <ColumnState
                      cc={cc}
                      state={st}
                      recommended={cc === rec}
                      rechecking={rechecking}
                      disabled={noRunner}
                      incompatibleReason={st === "incompatible" ? vaultIncompatibleReason : undefined}
                      substrate={status.runner.confinement_substrates?.[cc]}
                      recheckToken={recheckToken}
                    />
                  </th>
                );
              })}
            </tr>
          </thead>

          <tbody>
            <RowLabelled label="Strength" col={col} onCellClick={selectCell}>
              {(cc) => <span className="text-muted-foreground">{CC_META[cc].tagline}</span>}
            </RowLabelled>

            <RowLabelled label="Mechanism" col={col} onCellClick={selectCell}>
              {(cc) => <span className="text-muted-foreground">{CC_META[cc].mechanism}</span>}
            </RowLabelled>

            {CC_MATRIX_ROWS.map((row) => (
              <RowLabelled key={row.label} label={row.label} col={col} center onCellClick={selectCell}>
                {(cc) => <MatrixCell mark={row.cells[cc]} cc={cc} />}
              </RowLabelled>
            ))}

            {/* Doesn't stop — permanent row (honesty rule), amber label. */}
            <RowLabelled
              label={<span className="text-warning">{RESIDUAL_PREFIX}</span>}
              col={col}
              onCellClick={selectCell}
            >
              {(cc) => <span className="text-muted-foreground">{CC_META[cc].doesntProtect}</span>}
            </RowLabelled>

            <RowLabelled label="Pick when" col={col} onCellClick={selectCell}>
              {(cc) => <span className="text-muted-foreground">{PICK_WHEN[cc]}</span>}
            </RowLabelled>

            <RowLabelled label="Where it runs" col={col} foot onCellClick={selectCell}>
              {(cc) => (
                <span className="text-xs text-muted-foreground">{CC_MATRIX_WHERE.cells[cc]}</span>
              )}
            </RowLabelled>
          </tbody>
        </table>
      </div>

      {/* Constant note — exactly once, near the picker. */}
      <div className="flex items-start gap-2 rounded-lg border bg-muted/40 p-3">
        <Info className="mt-0.5 size-4 shrink-0 text-muted-foreground" aria-hidden />
        <p className="text-sm text-muted-foreground">{CONFINEMENT_CONSTANT_NOTE}</p>
      </div>

      <p className="text-xs text-muted-foreground">
        Your pick is saved in this browser as the default barrier for new runs.
      </p>
    </div>
  );
}

// One protection row: a row-header label cell + one cell per tier. `center`
// centers the tier cells (for the check/caveat/x icons). `foot` drops the bottom
// border so the selected-column ring closes cleanly on the last row.
function RowLabelled({
  label,
  col,
  children,
  center,
  foot,
  onCellClick,
}: {
  label: React.ReactNode;
  col: (cc: ConfinementClass, pos: "head" | "mid" | "foot") => string;
  children: (cc: ConfinementClass) => React.ReactNode;
  center?: boolean;
  foot?: boolean;
  onCellClick: (cc: ConfinementClass) => void;
}) {
  const pos = foot ? "foot" : "mid";
  return (
    <tr>
      <th
        scope="row"
        className={cn(
          "w-[220px] px-4 py-3 text-left align-top font-normal text-foreground",
          !foot && "border-b",
        )}
      >
        {label}
      </th>
      {CC_ORDER.map((cc) => (
        <td
          key={cc}
          className={cn(!foot && "border-b", center && "text-center", col(cc, pos))}
          onClick={() => onCellClick(cc)}
        >
          {children(cc)}
        </td>
      ))}
    </tr>
  );
}

// ✓ / ⚠ / ✗ cell. A caveat isn't a bare "partly" — its native title names the
// residual risk, reusing RESIDUAL_PREFIX + the tier's doesntProtect line verbatim.
function MatrixCell({ mark, cc }: { mark: CCMark; cc: ConfinementClass }) {
  if (mark === "yes") return <Check className="mx-auto size-5 text-success" aria-label="Yes" />;
  if (mark === "no") return <X className="mx-auto size-5 text-danger" aria-label="No" />;
  const label = `${RESIDUAL_PREFIX} ${CC_META[cc].doesntProtect}`;
  return (
    <span
      title={label}
      aria-label="Yes, with caveat"
      className="mx-auto inline-flex text-warning"
    >
      <AlertTriangle className="size-5" aria-hidden />
    </span>
  );
}

// The live-state block under each column's radio: status chip + Recommended, then
// the state-specific action — ready shows the running substrate, needs-setup
// reveals the `wardyn setup <tier>` command (+ still-not-detected after a
// re-check), incompatible shows the concrete /dev/kvm reason.
function ColumnState({
  cc,
  state,
  recommended,
  rechecking,
  disabled,
  incompatibleReason,
  substrate,
  recheckToken,
}: {
  cc: ConfinementClass;
  state: TierState;
  recommended: boolean;
  /** A host re-check is in flight — chip shows "Checking…", detail lines suppressed. */
  rechecking: boolean;
  /** No runner: the setup-command / copy controls are inert (not just pointer-none). */
  disabled: boolean;
  incompatibleReason?: string;
  substrate?: string;
  recheckToken: number;
}) {
  const [cmdOpen, setCmdOpen] = React.useState(false);
  // The recheckToken captured when the command was revealed — a later bump means
  // a host re-check completed while the panel was open, so it's "still not detected".
  const [openedAt, setOpenedAt] = React.useState(0);
  const { copied, copyAsync } = useCopyToClipboard(1400);
  const guide = TIER_GUIDES[cc];
  // Mirror RunnerTiers' rowStatus: a re-check in flight reads "Checking…" for any
  // not-yet-ready tier, overriding needs-setup/incompatible until it resolves.
  const chipStatus: StatusKind =
    rechecking && state !== "ready"
      ? "checking"
      : state === "ready"
        ? "ready"
        : state === "incompatible"
          ? "incompatible"
          : "needs-setup";
  const stillNotDetected =
    !rechecking && cmdOpen && recheckToken > openedAt && state === "todo"
      ? NOT_DETECTED[cc]
      : undefined;

  return (
    <div className="mt-3 space-y-2">
      <div className="flex flex-wrap items-center gap-2">
        <StatusChip status={chipStatus} reason={incompatibleReason} />
        {recommended && (
          <Chip tone="primary" className="uppercase tracking-wide">
            Recommended
          </Chip>
        )}
      </div>

      {state === "ready" && substrate && (
        <p className="text-xs text-muted-foreground">
          Running here as <span className="font-mono">{substrate}</span>
        </p>
      )}

      {state === "todo" &&
        guide &&
        (!cmdOpen ? (
          <Button
            variant="outline"
            size="sm"
            disabled={disabled}
            onClick={() => {
              setCmdOpen(true);
              setOpenedAt(recheckToken);
            }}
          >
            <Terminal className="size-3.5" aria-hidden />
            {BTN.showSetupCommand}
          </Button>
        ) : (
          <>
            <div className="flex items-center justify-between gap-2 rounded-lg border bg-muted px-2.5 py-1.5">
              <code className="truncate font-mono text-xs text-foreground">$ {guide.command}</code>
              <button
                type="button"
                disabled={disabled}
                onClick={() => void copyAsync(guide.command)}
                className="shrink-0 text-muted-foreground hover:text-foreground disabled:opacity-50"
                aria-label="Copy setup command"
              >
                {copied ? (
                  <Check className="size-4 text-success" aria-hidden />
                ) : (
                  <Copy className="size-4" aria-hidden />
                )}
              </button>
            </div>
            {stillNotDetected && <p className="text-xs text-danger">{stillNotDetected}</p>}
          </>
        ))}

      {state === "incompatible" && incompatibleReason && (
        <p className="text-xs leading-snug text-muted-foreground">{incompatibleReason}</p>
      )}
    </div>
  );
}
