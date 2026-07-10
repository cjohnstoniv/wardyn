import { useState, type ReactNode } from "react";
import {
  Check,
  X,
  AlertTriangle,
  Copy,
  Info,
  AlertOctagon,
  Terminal,
} from "lucide-react";
import { Button } from "../../ui/button";
import { cn } from "../../ui/utils";
import { Tooltip, TooltipContent, TooltipTrigger } from "../../ui/tooltip";
import { StatusChip } from "../StatusChip";
import { TierIllustration, StrengthMeter } from "../TierIllustration";
import {
  TIER_META,
  type BarrierProbe,
  type SetupStatus,
  type TierId,
} from "../../../data/setupFixtures";
import {
  TIER_COPY,
  CONSTANT_NOTE,
  MATRIX_ROWS,
  MATRIX_WHERE,
  type MatrixRow,
} from "../../../data/tierContent";

const COLS: TierId[] = ["fence", "wall", "vault"];
const TIER_TEXT: Record<TierId, string> = {
  fence: "text-tier-fence",
  wall: "text-tier-wall",
  vault: "text-tier-vault",
};

// Flagship step (brief §6.1 / §7.2): the protection matrix IS the primary comparison surface —
// tiers as selectable columns, capabilities/protections as rows. Honesty copy preserved:
// "Doesn't stop:" is a permanent row, caveats reuse it, constant-note appears once.
export function EnvironmentStep({
  status,
  selected,
  onSelect,
}: {
  status: SetupStatus;
  selected: TierId | null;
  onSelect: (tier: TierId) => void;
}) {
  const [cmdOpen, setCmdOpen] = useState<Record<string, boolean>>({});
  const [copied, setCopied] = useState<string | null>(null);

  const probes = new Map(status.barriers.map((b) => [b.id, b]));

  function selectable(tier: TierId) {
    return !status.noRunner && probes.get(tier)?.state === "ready";
  }

  function copyCmd(tier: TierId, cmd: string) {
    navigator.clipboard?.writeText(`$ ${cmd}`).catch(() => {});
    setCopied(tier);
    window.setTimeout(() => setCopied((c) => (c === tier ? null : c)), 1500);
  }

  // Column outline classes — selected column gets a teal ring built from cell borders.
  function col(tier: TierId, pos: "head" | "mid" | "foot") {
    const sel = selected === tier;
    return cn(
      "px-4 align-top",
      pos === "head" ? "pt-4 pb-3" : "py-3",
      selectable(tier) && "cursor-pointer",
      sel && "bg-primary/5 border-x-2 border-primary",
      sel && pos === "head" && "border-t-2",
      sel && pos === "foot" && "border-b-2",
      !selectable(tier) && "opacity-70",
    );
  }

  return (
    <div className="space-y-5">
      {/* No-runner danger card — matrix remains visible in read-only mode below */}
      {status.noRunner && (
        <div className="rounded-xl border border-danger/40 bg-danger-subtle p-5">
          <div className="flex items-start gap-3">
            <AlertOctagon className="mt-0.5 size-5 text-danger" aria-hidden />
            <div>
              <div className="text-foreground">No sandbox runner — runs can't launch.</div>
              <p className="mt-1 text-sm text-muted-foreground">
                Fix: start the Docker daemon (or install a container runtime) so Wardyn can
                build a barrier, then re-check.
              </p>
            </div>
          </div>
        </div>
      )}

      <p className="text-sm text-muted-foreground">
        {status.noRunner
          ? "The tier matrix is shown for reference — tiers can't be selected until a runner is available."
          : "Weakest to strongest — pick a column to save it as the default barrier for new runs."}
      </p>

      <div className={cn("overflow-x-auto rounded-xl border", status.noRunner && "opacity-60 pointer-events-none")}>
        <table className="w-full min-w-[720px] border-collapse text-sm">
          {/* Column headers = selectable tier picker */}
          <thead>
            <tr>
              <th className="w-[220px] border-b bg-muted/40 px-4 py-3 text-left align-bottom font-normal">
                <span className="text-xs uppercase tracking-wide text-muted-foreground">
                  Barrier tier
                </span>
              </th>
              {COLS.map((tier) => {
                const probe = probes.get(tier);
                const meta = TIER_META[tier];
                const sel = selected === tier;
                const canSelect = selectable(tier);
                return (
                  <th
                    key={tier}
                    className={cn(
                      "border-b text-left align-top font-normal",
                      col(tier, "head"),
                    )}
                  >
                    <button
                      type="button"
                      onClick={() => canSelect && onSelect(tier)}
                      disabled={!selectable(tier)}
                      aria-disabled={!selectable(tier)}
                      aria-pressed={sel}
                      tabIndex={0}
                      className="focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary rounded-lg w-full text-left p-0"
                    >
                      <div className="flex items-start justify-between gap-2">
                        {/* J2 — metaphor tooltip on illustration */}
                        <Tooltip>
                          <TooltipTrigger asChild>
                            <span>
                              <TierIllustration tier={tier} />
                            </span>
                          </TooltipTrigger>
                          <TooltipContent side="top" className="max-w-[200px]">
                            {TIER_COPY[tier].metaphor}
                          </TooltipContent>
                        </Tooltip>
                        {sel && (
                          <span className="inline-flex size-6 items-center justify-center rounded-full bg-primary text-primary-foreground">
                            <Check className="size-4" aria-label="Selected default" />
                          </span>
                        )}
                      </div>
                      <div className="mt-2 flex items-center gap-2">
                        <span className={cn("text-foreground", TIER_TEXT[tier])}>
                          {meta.name}
                        </span>
                        <span className="text-xs text-muted-foreground">{meta.code}</span>
                      </div>
                      <div className="mt-2">
                        <StrengthMeter tier={tier} />
                      </div>
                      <div className="mt-3">
                        <TierHeaderState
                          tier={tier}
                          probe={probe}
                          recommended={status.recommendedTier === tier}
                          cmdOpen={!!cmdOpen[tier]}
                          copied={copied === tier}
                          onShowCmd={() =>
                            setCmdOpen((s) => ({ ...s, [tier]: true }))
                          }
                          onCopy={(cmd) => copyCmd(tier, cmd)}
                        />
                      </div>
                    </button>
                  </th>
                );
              })}
            </tr>
          </thead>

          <tbody>
            <RowLabelled label="Strength" col={col} border onCellClick={(t) => selectable(t) && onSelect(t)}>
              {(tier) => (
                <span className="text-muted-foreground">{TIER_META[tier].tagline}</span>
              )}
            </RowLabelled>

            <RowLabelled label="Mechanism" col={col} border onCellClick={(t) => selectable(t) && onSelect(t)}>
              {(tier) => (
                <span className="text-muted-foreground">{TIER_COPY[tier].mechanism}</span>
              )}
            </RowLabelled>

            {MATRIX_ROWS.map((row) => (
              <RowLabelled key={row.label} label={row.label} col={col} border center onCellClick={(t) => selectable(t) && onSelect(t)}>
                {(tier) => <MatrixCell value={row[tier]} tier={tier} />}
              </RowLabelled>
            ))}

            {/* Doesn't stop — permanent row, honesty rule */}
            <RowLabelled
              label={<span className="text-warning">Doesn't stop</span>}
              col={col}
              border
              onCellClick={(t) => selectable(t) && onSelect(t)}
            >
              {(tier) => (
                <span className="text-muted-foreground">{TIER_COPY[tier].doesntStop}</span>
              )}
            </RowLabelled>

            <RowLabelled label="Pick when" col={col} border onCellClick={(t) => selectable(t) && onSelect(t)}>
              {(tier) => (
                <span className="text-muted-foreground">{TIER_COPY[tier].pickWhen}</span>
              )}
            </RowLabelled>

            <RowLabelled label="Where it runs" col={col} pos="foot" onCellClick={(t) => selectable(t) && onSelect(t)}>
              {(tier) => (
                <span className="text-xs text-muted-foreground">{MATRIX_WHERE[tier]}</span>
              )}
            </RowLabelled>
          </tbody>
        </table>
      </div>

      {/* Constant note — exactly once, near the tier picker */}
      <div className="flex items-start gap-2 rounded-lg border bg-muted/40 p-3">
        <Info className="mt-0.5 size-4 shrink-0 text-muted-foreground" aria-hidden />
        <p className="text-sm text-muted-foreground">{CONSTANT_NOTE}</p>
      </div>

      <p className="text-xs text-muted-foreground">
        Your pick is saved in this browser as the default barrier for new runs.
      </p>
    </div>
  );
}

// A protection row: label cell + a cell per tier. `center` centers the tier cells (for icons).
function RowLabelled({
  label,
  col,
  children,
  border,
  center,
  pos = "mid",
  onCellClick,
}: {
  label: ReactNode;
  col: (tier: TierId, pos: "head" | "mid" | "foot") => string;
  children: (tier: TierId) => ReactNode;
  border?: boolean;
  center?: boolean;
  pos?: "mid" | "foot";
  onCellClick?: (tier: TierId) => void;
}) {
  return (
    <tr>
      <th
        scope="row"
        className={cn(
          "w-[220px] px-4 py-3 text-left align-top font-normal text-foreground",
          border && "border-b",
        )}
      >
        {label}
      </th>
      {COLS.map((tier) => (
        <td
          key={tier}
          className={cn(border && "border-b", center && "text-center", col(tier, pos))}
          onClick={onCellClick ? () => onCellClick(tier) : undefined}
        >
          {children(tier)}
        </td>
      ))}
    </tr>
  );
}

// ✅ / ⚠️ / ❌ cell. Caveat reuses the tier's "Doesn't stop:" line in a tooltip.
function MatrixCell({ value, tier }: { value: MatrixRow["fence"]; tier: TierId }) {
  if (value === "yes")
    return <Check className="mx-auto size-5 text-success" aria-label="Yes" />;
  if (value === "no")
    return <X className="mx-auto size-5 text-danger" aria-label="No" />;
  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <span
          className="mx-auto inline-flex items-center gap-1 text-warning"
          aria-label="Yes, with caveat"
        >
          <AlertTriangle className="size-5" aria-hidden />
        </span>
      </TooltipTrigger>
      <TooltipContent className="max-w-[240px]">
        Yes, except for that tier's residual — Doesn't stop: {TIER_COPY[tier].doesntStop}
      </TooltipContent>
    </Tooltip>
  );
}

// Per-tier live state in the column header: chip + recommended + setup command / provenance.
function TierHeaderState({
  tier,
  probe,
  recommended,
  cmdOpen,
  copied,
  onShowCmd,
  onCopy,
}: {
  tier: TierId;
  probe: BarrierProbe | undefined;
  recommended: boolean;
  cmdOpen: boolean;
  copied: boolean;
  onShowCmd: () => void;
  onCopy: (cmd: string) => void;
}) {
  const state = probe?.state ?? "needs-setup";
  return (
    <div className="space-y-2">
      <div className="flex flex-wrap items-center gap-2">
        {state === "ready" && <StatusChip kind="ready" />}
        {state === "needs-setup" && <StatusChip kind="needs-setup" />}
        {state === "incompatible" && <StatusChip kind="incompatible" />}
        {recommended && (
          <span className="inline-flex items-center rounded-full border border-primary/40 bg-primary/10 px-2 py-0.5 text-xs text-primary">
            Recommended
          </span>
        )}
      </div>

      {state === "ready" && probe?.substrate && (
        <p className="text-xs text-muted-foreground">
          Running here as <span className="font-mono">{probe.substrate}</span>
        </p>
      )}

      {state === "needs-setup" &&
        (!cmdOpen ? (
          <Button
            variant="outline"
            size="sm"
            onClick={(e) => {
              e.stopPropagation();
              onShowCmd();
            }}
          >
            <Terminal className="size-3.5" aria-hidden />
            Show setup command
          </Button>
        ) : (
          <>
            <div className="flex items-center justify-between gap-2 rounded-lg border bg-muted px-2.5 py-1.5">
              <code className="truncate font-mono text-xs text-foreground">
                $ {probe?.setupCommand}
              </code>
              <button
                onClick={(e) => {
                  e.stopPropagation();
                  if (probe?.setupCommand) onCopy(probe.setupCommand);
                }}
                className="shrink-0 text-muted-foreground hover:text-foreground"
                aria-label="Copy setup command"
              >
                {copied ? (
                  <Check className="size-4 text-success" aria-hidden />
                ) : (
                  <Copy className="size-4" aria-hidden />
                )}
              </button>
            </div>
            {probe?.stillNotDetected && (
              <p className="text-xs text-danger">{probe.stillNotDetected}</p>
            )}
          </>
        ))}

      {state === "incompatible" && probe?.reason && (
        <p className="text-xs text-muted-foreground">{probe.reason}</p>
      )}
    </div>
  );
}
