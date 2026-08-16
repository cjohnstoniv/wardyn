/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Runs screen's first-run experience — what a brand-new operator sees, and
// what replaced the twelve-step setup funnel. Split out of runs.tsx purely for
// size (scripts/check-file-size.sh): RunsScreen owns the populated board, this
// owns the empty one and the no-barrier blocker.

import { Link } from "react-router-dom";
import {
  CircleCheck,
  CircleDashed,
  CircleX,
  Hexagon,
  Plus,
  RotateCw,
  ShieldX,
} from "lucide-react";
import type { ConfinementClass } from "../../lib/types";
import { CC_ORDER } from "../../lib/types";
import type { Readiness } from "../../lib/readiness";
import { Button } from "../ui/button";
import { Mono } from "../wardyn/code-block";
import { CopyButton } from "../wardyn/copy-button";
import { CC_META } from "../wardyn/cc-meta";
import { DEMOS } from "./demos/demo-catalog";

// The one hard blocker in the product: no sandbox barrier means no run can
// start, full stop. Non-dismissible by design — there's nothing to dismiss it
// TO, every other affordance on this page is dead until this is fixed.
export function NoBarrierBanner({ onRecheck }: { onRecheck: () => void }) {
  const setupCmd = "sudo wardyn setup fence";
  return (
    <div
      role="alert"
      className="mb-5 space-y-2.5 rounded-xl border border-danger/40 bg-danger-subtle px-4 py-3.5 text-sm text-danger"
    >
      <div className="flex items-start gap-2">
        <ShieldX className="mt-0.5 size-4 shrink-0" />
        <p className="font-medium">No sandbox barrier on this host. Runs cannot start.</p>
      </div>
      <div className="flex flex-wrap items-center gap-2 pl-6">
        <div className="flex items-center gap-1.5 rounded-md border border-danger/30 bg-card px-2 py-1">
          <Mono className="text-foreground">{setupCmd}</Mono>
          <CopyButton
            text={setupCmd}
            label="Copy setup command"
            className="rounded p-0.5 text-muted-foreground hover:text-foreground"
          />
        </div>
        <Button size="sm" variant="outline" onClick={onRecheck}>
          <RotateCw className="size-3.5" /> Re-check
        </Button>
      </div>
    </div>
  );
}

// RunsFirstRun — the Runs screen's own first-run experience, replacing the
// deleted 12-step setup funnel: a hexagon glyph, two DERIVED-FACT checklist
// rows (never a numbered stepper — see the two rows below), the primary
// launch actions, and a "See it work" demo grid that needs no model, key, or
// repo. `readiness`/`confinementClasses` come straight off the same setup
// status the no-barrier banner above uses — never a second, disagreeing check.
export function RunsFirstRun({
  readiness,
  confinementClasses,
  onNewRun,
}: {
  readiness: Readiness | null;
  confinementClasses: ConfinementClass[];
  onNewRun: () => void;
}) {
  const barrierLabels = CC_ORDER.filter((cc) => confinementClasses.includes(cc)).map((cc) => CC_META[cc].label);
  const barrierLoaded = readiness !== null;
  const llmReady = readiness?.llmReady ?? false;

  return (
    <div className="flex flex-col items-center gap-10 px-4 py-14">
      <div className="w-full max-w-[620px] space-y-6 text-center">
        <div className="flex flex-col items-center gap-4">
          <div className="flex size-14 items-center justify-center rounded-2xl border border-border bg-surface-2 text-muted-foreground">
            <Hexagon className="size-6" />
          </div>
          <div className="space-y-1.5">
            <h2 className="text-lg font-semibold text-foreground">No runs yet</h2>
            <p className="text-sm leading-relaxed text-muted-foreground">
              A run is a workload in a sealed box. You watch it, approve what it reaches for, and keep
              the recording.
            </p>
          </div>
        </div>

        {/* Two derived facts, not a stepper — no numbers, no progress. */}
        <ul className="divide-y divide-border rounded-xl border border-border bg-card text-left">
          <li className="flex items-start gap-3 p-3.5">
            {/* Never a green check when there's genuinely nothing available —
                that would contradict the no-barrier blocker banner above. */}
            {!barrierLoaded ? (
              <CircleDashed className="mt-0.5 size-4 shrink-0 text-muted-foreground" />
            ) : barrierLabels.length > 0 ? (
              <CircleCheck className="mt-0.5 size-4 shrink-0 text-success" />
            ) : (
              <CircleX className="mt-0.5 size-4 shrink-0 text-danger" />
            )}
            <div className="min-w-0">
              <div className="text-sm font-medium text-foreground">Sandbox barrier</div>
              <p className="mt-0.5 text-xs leading-snug text-muted-foreground">
                {!barrierLoaded
                  ? "Checking…"
                  : barrierLabels.length > 0
                    ? `${barrierLabels.join(", ")} available on this host.`
                    : "None available on this host."}
              </p>
            </div>
          </li>
          <li className="flex items-start gap-3 p-3.5">
            <CircleDashed className="mt-0.5 size-4 shrink-0 text-muted-foreground" />
            <div className="min-w-0">
              <div className="text-sm font-medium text-foreground">Model provider</div>
              <p className="mt-0.5 text-xs leading-snug text-muted-foreground">
                {llmReady ? (
                  `Connected${readiness?.llmLabel ? ` — ${readiness.llmLabel}` : ""}.`
                ) : (
                  <>
                    Not connected — agent runs need one. Governed commands run without one.{" "}
                    <Link to="/integrations" className="font-medium text-primary hover:underline">
                      Connect →
                    </Link>
                  </>
                )}
              </p>
            </div>
          </li>
        </ul>

        <div className="flex flex-wrap items-center justify-center gap-2">
          <Button onClick={onNewRun}>
            <Plus className="size-4" /> New run
          </Button>
          <Button variant="outline" asChild>
            <Link to="/demos">Try it without a repo</Link>
          </Button>
        </div>
      </div>

      <div className="w-full max-w-[900px] space-y-4">
        <div className="space-y-1 text-center">
          <h3 className="text-[0.6875rem] font-semibold uppercase tracking-wider text-muted-foreground">
            See it work
          </h3>
          <p className="text-sm text-muted-foreground">
            No model, no key, no repo. Each one runs a real governed sandbox in about a minute.
          </p>
        </div>
        <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-3">
          {DEMOS.map((demo) => {
            // The flagship agent demo needs a connected model — same gate
            // demo-screen.tsx itself uses (deriveReadiness's llmReady), so this
            // card can never promise a "Run it" the demo screen would refuse.
            const needsModel = demo.needsModel && !llmReady;
            return (
              <div
                key={demo.id}
                data-testid={`runs-empty-demo-${demo.id}`}
                className="flex flex-col gap-2.5 rounded-xl border border-border bg-card p-4"
              >
                <h4 className="text-sm font-semibold text-foreground">{demo.title}</h4>
                <p className="flex-1 text-xs leading-snug text-muted-foreground">{demo.teaches}</p>
                {needsModel ? (
                  <p className="text-xs text-muted-foreground">
                    Needs a model provider ·{" "}
                    <Link to="/integrations" className="font-medium text-primary hover:underline">
                      Connect →
                    </Link>
                  </p>
                ) : (
                  // Secondary/outline — a teal fill is reserved for the one
                  // primary action on the page ("New run" above).
                  <Button variant="outline" size="sm" className="self-start" asChild>
                    <Link to="/demos">Run it</Link>
                  </Button>
                )}
              </div>
            );
          })}
        </div>
      </div>
    </div>
  );
}
