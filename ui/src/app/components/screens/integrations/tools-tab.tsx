/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Tools tab — mockup's Harnesses frame. A tool is installable client software
// the image carries; an integration is what it connects through (T.LAW). Four
// of the six rows are wiring-derived (git / package managers / Claude Code /
// Codex CLI show the currently configured integration(s) that power them, or
// "Nothing yet."); gh CLI and Your own tools are permanent facts — Wardyn
// never wires either, so their "Powered by" column is always "—".
import { TOOLS, T } from "../../../lib/integrations";
import type { IntegrationRow, IntegrationsData } from "../../../lib/api/integrations";
import type { EgressRedirect } from "../../../lib/types";
import { AgentBadge } from "../../wardyn/primitives";
import { Chip } from "../../wardyn/primitives";
import { Mono } from "../../wardyn/code-block";

function hostnameOf(url: string): string {
  try {
    return new URL(url).hostname || url;
  } catch {
    return url;
  }
}

// A configured integration's name as a "powered by" chip — `· default` when
// that row's OWN capability matrix marks it the default lane for `capability`.
function poweredChip(row: IntegrationRow, capability: RegExp) {
  const isDefault = row.chips.some((c) => !c.muted && capability.test(c.label) && c.label.includes("· default"));
  return { key: row.id, label: isDefault ? `${row.name} · default` : row.name };
}

function ToolRow({
  name,
  sub,
  agent,
  drive,
  powered,
  poweredNone,
  wire,
  image,
  extra,
}: {
  name?: string;
  sub?: string;
  agent?: "claude-code" | "codex-cli";
  drive?: boolean;
  powered?: { key: string; label: string }[];
  poweredNone?: string;
  wire: string;
  image?: string;
  extra?: string;
}) {
  return (
    <div className="flex flex-col gap-2.5 rounded-xl border border-border bg-card p-4">
      <div className="flex flex-wrap items-center gap-2.5">
        {agent ? <AgentBadge agent={agent} /> : <span className="text-sm font-medium text-foreground">{name}</span>}
        {sub && <Mono className="text-[0.6875rem]">{sub}</Mono>}
        {drive && (
          <Chip tone="cyan" className="text-[0.6875rem]">
            can drive a run
          </Chip>
        )}
      </div>
      <div className="grid grid-cols-[160px_1fr] items-start gap-x-3.5 gap-y-2 text-xs">
        <span className="pt-0.5 text-[0.6875rem] uppercase tracking-wide text-muted-foreground">Powered by</span>
        {powered && powered.length > 0 ? (
          <div className="flex flex-wrap gap-1.5">
            {powered.map((p) => (
              <Chip key={p.key} tone="info" className="text-[0.6875rem]">
                {p.label}
              </Chip>
            ))}
          </div>
        ) : (
          <span className="pt-0.5 text-[0.6875rem] text-muted-foreground">{poweredNone ?? "—"}</span>
        )}
        <span className="text-[0.6875rem] uppercase tracking-wide text-muted-foreground">How Wardyn wires it</span>
        <p className="text-[0.75rem] leading-snug text-foreground">{wire}</p>
        {image && (
          <>
            <span className="text-[0.6875rem] uppercase tracking-wide text-muted-foreground">Image</span>
            <p className="text-[0.75rem] leading-snug text-foreground">{image}</p>
          </>
        )}
      </div>
      {extra && <p className="text-[0.6875rem] leading-snug text-muted-foreground">{extra}</p>}
    </div>
  );
}

export function ToolsTab({
  data,
  redirects,
}: {
  data: IntegrationsData;
  /** SiteConfig.egress_redirects, straight from the caller. Redirects stopped
   *  being integration rows when Corporate network took them over, but they're
   *  still what a package manager fetches THROUGH — so the row keeps naming
   *  the mirror instead of falsely reading "Nothing yet." */
  redirects: EgressRedirect[];
}) {
  const claudeCapable = data.ai.filter((r) => r.chips.some((c) => !c.muted && /^Claude Code/.test(c.label)));
  const codexCapable = data.ai.filter((r) => r.chips.some((c) => !c.muted && /^Codex CLI/.test(c.label)));

  return (
    <div className="space-y-4" aria-label="Tools">
      <ToolRow
        name="git"
        powered={data.scm.map((r) => ({ key: r.id, label: r.name }))}
        poweredNone="Nothing yet."
        wire={TOOLS.git.wire}
        extra={TOOLS.git.extra}
      />
      <ToolRow
        name="Package managers"
        sub="npm · pip · cargo · maven · go · nuget"
        powered={redirects.map((r, i) => ({ key: `redirect:${i}`, label: hostnameOf(r.to) }))}
        poweredNone="Nothing yet."
        wire={TOOLS.package_managers.wire}
      />
      <ToolRow
        agent="claude-code"
        drive
        powered={claudeCapable.map((r) => poweredChip(r, /^Claude Code/))}
        poweredNone="Nothing yet."
        wire={TOOLS.claude_code.wire}
        image={TOOLS.claude_code.image}
      />
      <ToolRow
        agent="codex-cli"
        drive
        powered={codexCapable.map((r) => poweredChip(r, /^Codex CLI/))}
        poweredNone="Nothing yet."
        wire={TOOLS.codex_cli.wire}
        image={TOOLS.codex_cli.image}
      />
      <ToolRow name="gh CLI" poweredNone="—" wire={TOOLS.gh_cli.wire} />
      <ToolRow name="Your own tools" sub="BYO image" poweredNone="—" wire={TOOLS.own_tools.wire} />

      <div className="rounded-xl border border-border bg-muted/50 px-4 py-3.5">
        <p className="text-sm font-semibold leading-relaxed text-foreground">{T.LAW}</p>
      </div>
    </div>
  );
}
