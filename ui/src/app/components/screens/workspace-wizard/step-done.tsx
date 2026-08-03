/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Step ④ Done — mock frames WzS4Happy / WzS4Running / WzS4Unmet
// (mockup/wardyn-workspaces.js's AddWorkspaceWizard bodyDone section).
import { AlertTriangle, Check, Loader2 } from "lucide-react";
import { Button } from "../../ui/button";
import { cn } from "../../ui/utils";
import {
  summarizeRequirements,
  unmetRequiredSecrets,
  type PowerSource,
  type WorkspaceRequirementsMap,
} from "./wizard-types";

export type DoneVariant = "usable" | "scanning" | "failed";

const STRENGTHEN_CARDS: { focus: "record" | "env" | "model"; title: string; desc: string }[] = [
  {
    focus: "record",
    title: "Record a session",
    desc: "Drive it once in an open sandbox to learn what it really reaches, then promote those hosts.",
  },
  { focus: "env", title: "Env as code", desc: "Generate a devcontainer.json / AGENTS.md you can commit." },
  { focus: "model", title: "Model access", desc: "Bind a model this workspace's runs use." },
];

function modelAccessDesc(powerSource: PowerSource): string {
  if (powerSource.kind === "pinned") return `Bound: ${powerSource.name}. Change it any time on its page.`;
  return "Bind a model this workspace's runs use.";
}

export function StepDone({
  name,
  variant,
  requirements,
  storedSecretNames,
  leakCount,
  powerSource,
  onOpenDetail,
  onRescan,
}: {
  name: string;
  variant: DoneVariant;
  requirements: WorkspaceRequirementsMap;
  storedSecretNames: string[];
  leakCount: number;
  powerSource: PowerSource;
  onOpenDetail: (focus?: "record" | "env" | "model") => void;
  onRescan: () => void;
}) {
  const scanning = variant === "scanning";
  const failed = variant === "failed";
  const settled = !scanning && !failed;

  const unmet = unmetRequiredSecrets(requirements, storedSecretNames);
  const summary = summarizeRequirements(requirements);

  const warnLines: string[] = [];
  if (settled) {
    if (unmet.length) {
      warnLines.push(
        `${unmet.length} required secret${unmet.length > 1 ? "s aren't" : " isn't"} stored yet — runs start without ${
          unmet.length > 1 ? "them" : "it"
        }; whatever needs ${unmet.length > 1 ? "them" : "it"} will fail at that point.`,
      );
    }
    if (leakCount > 0) {
      warnLines.push(
        `${leakCount} suspected committed secret${leakCount > 1 ? "s" : ""} — rotate or remove before mounting; see its page.`,
      );
    }
  }

  const cards = STRENGTHEN_CARDS.map((c) => (c.focus === "model" ? { ...c, desc: modelAccessDesc(powerSource) } : c));

  return (
    <div className="space-y-4">
      <div className="space-y-1.5">
        <div className="flex items-center gap-2.5">
          <span
            className={cn(
              "inline-flex size-7 shrink-0 items-center justify-center rounded-full",
              failed ? "bg-danger-subtle text-danger" : scanning ? "bg-info-subtle text-info" : "bg-success-subtle text-success",
            )}
          >
            {scanning ? (
              <Loader2 className="size-3.5 animate-spin" />
            ) : failed ? (
              <AlertTriangle className="size-4" />
            ) : (
              <Check className="size-4" />
            )}
          </span>
          <span className="text-base font-semibold text-foreground">
            {failed ? `${name} exists — its scan failed.` : `${name} is usable.`}
          </span>
        </div>
        <p className="text-sm text-muted-foreground">{failed ? "Runs can attach it." : "Runs can attach it now."}</p>
      </div>

      {scanning && (
        <div className="flex items-center gap-2.5 rounded-lg border border-border bg-surface-2/40 p-3">
          <Loader2 className="size-3.5 shrink-0 animate-spin text-muted-foreground" />
          <p className="text-xs leading-snug text-muted-foreground">
            The scan is still running — its requirements appear on its page when it lands.
          </p>
        </div>
      )}

      {failed && (
        <div className="space-y-2 rounded-lg border border-border bg-surface-2/40 p-3">
          <p className="text-xs text-muted-foreground">No contract yet — nothing will be attached automatically.</p>
          <Button type="button" size="sm" variant="outline" onClick={onRescan}>
            Rescan on its page →
          </Button>
        </div>
      )}

      {settled && (
        <div className="space-y-1 rounded-lg border border-border p-3">
          <p className="text-xs text-foreground">
            <strong>Always:</strong> {summary.always.length ? summary.always.join(" · ") : "nothing beyond the auto-allowed set"}
          </p>
          {summary.onRequest.length > 0 && (
            <p className="text-xs text-foreground">
              <strong>On request:</strong> {summary.onRequest.join(" · ")}
            </p>
          )}
        </div>
      )}

      {warnLines.length > 0 && (
        <div className="space-y-2">
          {warnLines.map((line, i) => (
            <div key={i} className="rounded-lg border border-warning/30 bg-warning-subtle px-3 py-2">
              <p className="text-[0.6875rem] leading-snug text-warning">{line}</p>
            </div>
          ))}
        </div>
      )}

      <div className="space-y-2">
        <p className="text-[0.6875rem] font-semibold uppercase tracking-wide text-muted-foreground">
          Make it stronger — optional
        </p>
        <div className="grid grid-cols-1 gap-2 sm:grid-cols-3">
          {cards.map((c) => (
            <button
              key={c.focus}
              type="button"
              onClick={() => onOpenDetail(c.focus)}
              className="space-y-1 rounded-lg border border-border p-3 text-left transition-colors hover:border-border-strong focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
            >
              <p className="text-xs font-medium text-foreground">{c.title}</p>
              <p className="text-[0.6875rem] leading-snug text-muted-foreground">{c.desc}</p>
            </button>
          ))}
        </div>
      </div>
    </div>
  );
}
