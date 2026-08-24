/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// SafetyMeter — a 4-segment safety bar for a policy spec, driven by the
// server's deterministic grade (POST /policies/grade, composer.Grade). Housed
// beside primitives.tsx rather than in it: unlike RiskBadge (a pure prop→chip),
// this one debounces and calls the API, so it lives on its own.
//
// The label derives from the WIRE's three levels + item shape, with a REACHABLE
// split (OverallLevel is the MAX, so "low overall with mediums present" is an
// empty state that never occurs):
//   Safest   = overall low
//   Guarded  = overall medium
//   Elevated = overall high with exactly ONE high item
//   Weakest  = overall high with 2+ high items
// Tones come from the same low/medium/high → success/warning/danger scale the
// Chip primitive uses; Elevated and Weakest both read as high (danger) and are
// distinguished only by how far the bar fills.
import * as React from "react";
import { cn } from "../ui/utils";
import { runs } from "../../lib/api/runs";
import type { PolicyGrade, RunPolicySpec } from "../../lib/types";

export type SafetyLabel = "Safest" | "Guarded" | "Elevated" | "Weakest";

// The whole point of round-2 H-2: derive the four labels from the max level +
// the high-item count so every one is reachable. Pure, so the mapping is unit-
// tested directly from real-shaped Grade outputs.
export function safetyLabel(grade: PolicyGrade): SafetyLabel {
  if (grade.overall_risk === "low") return "Safest";
  if (grade.overall_risk === "medium") return "Guarded";
  // overall high: OverallLevel is the max, so at least one high item exists —
  // one high is Elevated, two or more is Weakest.
  const highs = grade.risk_assessment.filter((i) => i.risk_level === "high").length;
  return highs >= 2 ? "Weakest" : "Elevated";
}

// segments = how far the bar fills (risk climbs left→right); color is the tone.
const META: Record<SafetyLabel, { segments: number; color: string }> = {
  Safest: { segments: 1, color: "text-success" },
  Guarded: { segments: 2, color: "text-warning" },
  Elevated: { segments: 3, color: "text-danger" },
  Weakest: { segments: 4, color: "text-danger" },
};

const SEGMENTS = [0, 1, 2, 3];
const DEBOUNCE_MS = 500;

// The rationales driving the grade are the items AT the overall (max) level —
// the ones that set it. Capped at 3 so the tooltip stays a tooltip.
// ponytail: fixed cap; a "+N more" affordance is the upgrade if operators ask.
function topRationales(grade: PolicyGrade): string[] {
  return grade.risk_assessment
    .filter((i) => i.risk_level === grade.overall_risk)
    .slice(0, 3)
    .map((i) => i.rationale);
}

export function SafetyMeter({
  spec,
  interactive,
  note,
  className,
}: {
  /** The parsed spec to grade, or null on a parse failure (dims the meter). */
  spec: RunPolicySpec | null;
  /** Grade hint — only the never-reap rationale reads it. Run wizard passes the
   *  screen's mode; /policies passes nothing (the conservative default-false). */
  interactive?: boolean;
  /** Run instance only: the doc-vs-resolved-run contrast appended to the title,
   *  so the meter's tooltip says how it differs from Preflight. */
  note?: string;
  className?: string;
}) {
  const [grade, setGrade] = React.useState<PolicyGrade | null>(null);
  const [error, setError] = React.useState(false);

  // Key the debounce on the spec's CONTENT, not its identity: the panel reparses
  // (a fresh object) every render, so an identity dep would refire endlessly.
  const specKey = spec ? JSON.stringify(spec) : null;
  React.useEffect(() => {
    if (specKey == null) return; // parse failure — the dim is handled in render.
    let live = true;
    const t = setTimeout(() => {
      runs
        .gradePolicy(JSON.parse(specKey) as RunPolicySpec, interactive)
        .then((g) => {
          if (live) {
            setGrade(g);
            setError(false);
          }
        })
        // A spec that parses client-side can still be illegal server-side (a
        // 400 from validatePolicySpec). Dim rather than crash — the meter is
        // advisory, and the Save/Preflight path surfaces the real error.
        .catch(() => {
          if (live) setError(true);
        });
    }, DEBOUNCE_MS);
    return () => {
      live = false;
      clearTimeout(t);
    };
  }, [specKey, interactive]);

  const dimmed = specKey == null || error;
  const label = grade ? safetyLabel(grade) : null;
  const active = !dimmed && label != null;
  const meta = active ? META[label] : null;
  const segments = meta?.segments ?? 0;

  const caption = active
    ? label
    : specKey == null
      ? "Fix the JSON first"
      : error
        ? "Can't grade this spec"
        : "Grading…";

  const title =
    ["Safety of the policy document as written", note].filter(Boolean).join(" — ") +
    (active && grade ? `. ${label}: ${topRationales(grade).join("; ")}` : ".");

  return (
    <div
      data-testid="safety-meter"
      data-safety={active ? label : ""}
      title={title}
      aria-label={title}
      className={cn("flex items-center gap-2 text-xs", dimmed && "opacity-60", className)}
    >
      <span className="label-eyebrow shrink-0">Safety</span>
      <span className={cn("flex items-center gap-2", meta?.color ?? "text-muted-foreground")}>
        <span className="flex items-center gap-0.5" aria-hidden>
          {SEGMENTS.map((i) => (
            <span
              key={i}
              className={cn("h-1.5 w-5 rounded-full", i < segments ? "bg-current" : "bg-border")}
            />
          ))}
        </span>
        <span className="font-medium">{caption}</span>
      </span>
    </div>
  );
}
