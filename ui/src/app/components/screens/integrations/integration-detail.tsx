/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Integration detail — mockup's DetailKey / DetailManaged / DetailBedrock /
// DetailGitHub frames, generalized over one IntegrationRow instead of one
// component per credential type (the mock's own four detail builders differ
// only in which data they're handed). Self-sufficient: there is no GET
// /integrations/:id either, so this re-derives the same rows the list screen
// does and looks up `id` in them — a direct link/refresh works with no router
// state hand-off required.
import * as React from "react";
import { useNavigate, useParams } from "react-router-dom";
import { Cable, Check, MoreHorizontal, RotateCw, Trash2 } from "lucide-react";
import {
  deriveIntegrations,
  describePosture,
  findRow,
  blastRadius,
  type IntegrationRow,
  type IntegrationsData,
} from "../../../lib/api/integrations";
import { setup as setupApi } from "../../../lib/api/setup";
import { health } from "../../../lib/api/health";
import { secrets as secretsApi } from "../../../lib/api/secrets";
import {
  AI_TYPES,
  BEDROCK_LANE_META,
  CATEGORY_META,
  RESIDENCY_META,
  T,
  type BedrockLane,
  type CapabilityRow,
} from "../../../lib/integrations";
import type { SetupStatus } from "../../../lib/types";
import { Button } from "../../ui/button";
import { Switch } from "../../ui/switch";
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuTrigger } from "../../ui/dropdown-menu";
import { Mono } from "../../wardyn/code-block";
import { Chip, OperatorOnlyHint, SectionLabel } from "../../wardyn/primitives";
import { EmptyState, ErrorState, TableSkeleton } from "../../wardyn/states";
import { DeleteConfirmDialog } from "../../wardyn/delete-confirm-dialog";
import { OPERATOR_ONLY_REASON } from "../../wardyn/copy";
import { useOperator } from "../../wardyn/operator-context";
import { AddSecretDialog } from "../secrets";
import { HarnessLoginPane } from "../setup/harness-login-pane";
import { CheckRow } from "../setup/step-bodies";
import { canRotateInline, deleteIntegration, primarySecretName } from "./actions";

const AGENT_SLOT = /Claude Code|Codex/;
const FEATURES_SLOT = /^Wardyn features/;
const POSTURE_CLASS: Record<"success" | "warning" | "muted", string> = {
  success: "text-success",
  warning: "text-warning",
  muted: "text-muted-foreground",
};

function Region({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <section className="space-y-2">
      <SectionLabel>{label}</SectionLabel>
      {children}
    </section>
  );
}

// Same fixed disabled-Switch-or-fact-text shape as the Add dialog's preview —
// a capability row is a FACT about the credential type, not a per-instance
// setting anything here can persist. "Make default" mirrors the mock's own
// noop handlers (ponytail: no backend concept of a per-capability default yet).
function CapabilityTable({ rows }: { rows: CapabilityRow[] }) {
  return (
    <div className="divide-y divide-border rounded-lg border border-border">
      {rows.map((r, i) => (
        <div key={i} className="flex items-center justify-between gap-3 px-3 py-2.5">
          <div className="min-w-0 flex-1 space-y-0.5">
            <div className="flex flex-wrap items-center gap-2">
              <span className="text-sm text-foreground">{r.label}</span>
              {r.def && (
                <Chip tone="primary" mono>
                  default
                </Chip>
              )}
              {r.makeDefault && (
                <button type="button" className="text-[0.6875rem] text-primary hover:underline">
                  Make default
                </button>
              )}
            </div>
            {!r.fact && r.note && <p className="text-[0.6875rem] leading-snug text-muted-foreground">{r.note}</p>}
          </div>
          {r.fact ? (
            <span className="max-w-[280px] shrink-0 text-right text-[0.6875rem] leading-snug text-muted-foreground" title={r.fact}>
              {r.fact}
            </span>
          ) : (
            <Switch checked={!!r.on} disabled aria-label={r.label} />
          )}
        </div>
      ))}
    </div>
  );
}

function BedrockLaneSubTable({ activeLane }: { activeLane?: BedrockLane }) {
  return (
    <div className="mt-2 divide-y divide-border rounded-lg border border-border">
      {(["bearer", "sso", "aws_dir", "static"] as const).map((lane) => {
        const meta = BEDROCK_LANE_META[lane];
        const res = RESIDENCY_META[meta.residency];
        const active = lane === activeLane;
        return (
          <div key={lane} className={`flex items-center gap-2.5 px-3 py-2 ${active ? "bg-primary/10" : ""}`}>
            <span className="flex-1 text-sm text-foreground">
              {meta.title}
              {meta.extra && <span className="text-muted-foreground"> · {meta.extra}</span>}
            </span>
            {active && (
              <Chip tone="primary" className="text-[0.6875rem]">
                active lane
              </Chip>
            )}
            <Chip tone={res.tone} className="text-[0.6875rem]">
              {res.label}
            </Chip>
          </div>
        );
      })}
      <p className="px-3 py-2 text-[0.6875rem] text-muted-foreground">{T.LANE_SWITCH}</p>
    </div>
  );
}

function HarnessCompatRow({ name, ok, line, image }: { name: string; ok: boolean; line: string; image?: string }) {
  return (
    <div className="grid grid-cols-[130px_1fr] items-start gap-3.5 p-3">
      <Mono className="text-foreground">{name}</Mono>
      <div className="space-y-1">
        <div className="flex items-start gap-1.5">
          {ok && <Check className="mt-0.5 size-3.5 shrink-0 text-success" />}
          <span className={ok ? "text-[0.8125rem] text-foreground" : "text-[0.6875rem] leading-snug text-muted-foreground"}>{line}</span>
        </div>
        {image && <p className="text-[0.6875rem] leading-snug text-muted-foreground">{image}</p>}
      </div>
    </div>
  );
}

function toolLine(rows: CapabilityRow[], match: RegExp): { ok: boolean; line: string } {
  const row = rows.find((r) => match.test(r.label));
  if (!row || row.fact) return { ok: false, line: row?.fact ?? "Not applicable." };
  return { ok: true, line: "This integration can drive it." };
}

function ToolCompatibilityRegion({ row }: { row: IntegrationRow }) {
  if (row.aiType) {
    const capRows = AI_TYPES[row.aiType].capabilityPreview(row.hostCli);
    const claude = toolLine(capRows, /Claude Code/);
    const codex = toolLine(capRows, /Codex/);
    return (
      <Region label="Tool compatibility">
        <div className="divide-y divide-border rounded-lg border border-border">
          <HarnessCompatRow
            name="claude-code"
            ok={claude.ok}
            line={claude.line}
            image={claude.ok ? "Image must carry the Claude Code CLI — Wardyn's built-in agent image does." : "Image must carry the Claude Code CLI."}
          />
          <HarnessCompatRow name="codex-cli" ok={codex.ok} line={codex.line} image="Image must carry the Codex CLI." />
          <HarnessCompatRow
            name="your own tools"
            ok={false}
            line="Wardyn wires no model credential — your image authenticates however it likes."
          />
        </div>
      </Region>
    );
  }
  if (row.category === "scm_host") {
    return (
      <Region label="Tool compatibility">
        <div className="divide-y divide-border rounded-lg border border-border">
          <HarnessCompatRow
            name="git"
            ok
            line={
              row.isGithubApp
                ? "Every lane here powers it — brokered (App), credential helper (PAT), or key file (SSH)."
                : "This credential powers it."
            }
            image="In every Wardyn image."
          />
        </div>
      </Region>
    );
  }
  return null;
}

type Loaded = { status: SetupStatus; data: IntegrationsData };

export function IntegrationDetailScreen() {
  const { id } = useParams<{ id: string }>();
  const navigate = useNavigate();
  const operator = useOperator();
  const [state, setState] = React.useState<"loading" | "error" | "ready">("loading");
  const [loaded, setLoaded] = React.useState<Loaded | null>(null);
  const [rotateName, setRotateName] = React.useState<string | null>(null);
  const [loginOpen, setLoginOpen] = React.useState(false);
  const [confirmDelete, setConfirmDelete] = React.useState(false);

  const load = React.useCallback(() => {
    setState("loading");
    Promise.all([setupApi.getSetupStatus(), health.getSiteConfig(), secretsApi.listSecrets()])
      .then(([status, siteConfig, secretNames]) => {
        setLoaded({ status, data: deriveIntegrations(status, siteConfig, secretNames) });
        setState("ready");
      })
      .catch(() => setState("error"));
  }, []);
  React.useEffect(load, [load]);

  if (state === "loading" || !loaded) {
    return (
      <div className="mx-auto max-w-[1100px] px-6 py-6">
        <TableSkeleton rows={3} cols={2} />
      </div>
    );
  }
  if (state === "error") {
    return (
      <div className="mx-auto max-w-[1100px] px-6 py-6">
        <ErrorState onRetry={load} />
      </div>
    );
  }

  const { status, data } = loaded;
  const row = id ? findRow(data, id) : undefined;

  if (!row) {
    return (
      <div className="mx-auto max-w-[1100px] px-6 py-6">
        <EmptyState
          icon={Cable}
          title="Integration not found"
          description="It may have been removed, or its credential deleted from Secrets."
          action={
            <Button variant="outline" onClick={() => navigate("/integrations")}>
              Back to Integrations
            </Button>
          }
        />
      </div>
    );
  }

  const resMeta = RESIDENCY_META[row.residency];
  const rotateTarget = primarySecretName(row);
  const defAgent = row.chips.some((c) => !c.muted && AGENT_SLOT.test(c.label) && c.label.includes("· default"));
  const defFeat = row.chips.some((c) => !c.muted && FEATURES_SLOT.test(c.label) && c.label.includes("· default"));

  return (
    <div className="mx-auto max-w-[1100px] space-y-6 px-6 py-6">
      <button
        type="button"
        onClick={() => navigate("/integrations")}
        className="text-xs text-muted-foreground transition-colors hover:text-foreground"
      >
        ← Integrations
      </button>

      <div className="flex flex-wrap items-center gap-3">
        <h1 className="text-xl font-semibold text-foreground">{row.name}</h1>
        <Chip tone="neutral">{CATEGORY_META[row.category].title}</Chip>
        <Mono className="text-sm">{row.typeLabel}</Mono>
        <span className="flex-1" />
        {(canRotateInline(row) || row.harnessProvider) && (
          <Button
            size="sm"
            variant="outline"
            disabled={!operator}
            title={operator ? undefined : OPERATOR_ONLY_REASON}
            onClick={() => (canRotateInline(row) && rotateTarget ? setRotateName(rotateTarget) : setLoginOpen(true))}
          >
            Rotate credential
          </Button>
        )}
        {row.canReCheck && (
          <Button size="sm" variant="outline" onClick={load}>
            <RotateCw className="size-3.5" /> Re-check
          </Button>
        )}
        <DropdownMenu>
          <DropdownMenuTrigger asChild>
            <Button variant="ghost" size="icon" className="size-8" aria-label="Integration actions">
              <MoreHorizontal className="size-4" />
            </Button>
          </DropdownMenuTrigger>
          <DropdownMenuContent align="end">
            <DropdownMenuItem className="text-danger focus:text-danger" disabled={!operator} onClick={() => setConfirmDelete(true)}>
              <Trash2 className="size-4" /> Delete integration…
              {!operator && <OperatorOnlyHint />}
            </DropdownMenuItem>
          </DropdownMenuContent>
        </DropdownMenu>
      </div>

      {loginOpen && row.harnessProvider && (
        <HarnessLoginPane
          provider={row.harnessProvider}
          onDone={() => {
            setLoginOpen(false);
            load();
          }}
          onCancel={() => setLoginOpen(false)}
        />
      )}

      {row.aiType && (
        <Region label="What this powers">
          <CapabilityTable rows={AI_TYPES[row.aiType].capabilityPreview(row.hostCli)} />
        </Region>
      )}

      <Region label="Residency">
        <div className="flex items-start gap-2.5">
          <Chip tone={resMeta.tone}>{resMeta.label}</Chip>
          <p className="flex-1 text-xs leading-snug text-muted-foreground">{resMeta.tooltip}</p>
        </div>
        {row.aiType === "bedrock" && <BedrockLaneSubTable activeLane={row.bedrockLane} />}
      </Region>

      <Region label="Credential">
        <div className="space-y-2 rounded-lg border border-border p-3.5">
          {row.secretNames.length > 0 ? (
            <div className="flex flex-wrap items-center gap-2">
              {row.secretNames.map((n) => (
                <Mono key={n} className="text-foreground">
                  {n}
                </Mono>
              ))}
            </div>
          ) : row.harnessProvider ? (
            <div className="flex items-center gap-2">
              <Chip tone="neutral" className="text-[0.6875rem]">
                container login
              </Chip>
              {/* Provenance beyond "how" is the SAME age fact the list's
                  Posture column already carries — reused, not re-derived, so
                  the two can never disagree on an aging/expiring session. */}
              <span className="text-sm text-foreground">{describePosture(row.posture).text}</span>
            </div>
          ) : (
            <p className="text-sm text-muted-foreground">No stored credential — Wardyn detects this automatically.</p>
          )}
          <p className="text-[0.6875rem] leading-snug text-muted-foreground">{T.WRITE_ONLY}</p>
        </div>
      </Region>

      <ToolCompatibilityRegion row={row} />

      {(row.checkIds.length > 0 || row.isGithubApp) && (
        <Region label="Checks">
          <div className="space-y-2">
            {row.checkIds
              .map((cid) => status.checks.find((c) => c.id === cid))
              .filter((c): c is NonNullable<typeof c> => !!c)
              .map((c) => (
                <ul key={c.id}>
                  <CheckRow check={c} />
                </ul>
              ))}
            {row.isGithubApp && (
              <>
                <GhVerdictRow row={row} />
                <p className="text-[0.6875rem] leading-snug text-muted-foreground">{T.CACHE_CAVEAT}</p>
              </>
            )}
          </div>
        </Region>
      )}

      <Region label="Danger zone">
        <div className="space-y-2.5 rounded-xl border border-danger/40 p-4">
          <p className="text-sm font-medium text-foreground">Delete this integration</p>
          <ul className="space-y-1 pl-4">
            {blastRadius(row, { isDefaultAgent: defAgent, isDefaultFeatures: defFeat }).map((line, i) => (
              <li key={i} className="list-disc text-[0.6875rem] leading-snug text-muted-foreground">
                {line}
              </li>
            ))}
          </ul>
          <Button
            size="sm"
            variant="outline"
            className="border-danger/50 text-danger hover:bg-danger-subtle hover:text-danger"
            disabled={!operator}
            title={operator ? undefined : OPERATOR_ONLY_REASON}
            onClick={() => setConfirmDelete(true)}
          >
            <Trash2 className="size-3.5" /> Delete integration…
          </Button>
        </div>
      </Region>

      <AddSecretDialog
        open={!!rotateName}
        onOpenChange={(o) => !o && setRotateName(null)}
        lockName
        initialName={rotateName ?? ""}
        onSaved={() => {
          setRotateName(null);
          load();
        }}
      />

      <DeleteConfirmDialog
        name={confirmDelete ? row.name : null}
        entity="integration"
        description={
          <ul className="space-y-1">
            {blastRadius(row, { isDefaultAgent: defAgent, isDefaultFeatures: defFeat }).map((line, i) => (
              <li key={i}>{line}</li>
            ))}
          </ul>
        }
        onOpenChange={(o) => !o && setConfirmDelete(false)}
        onDelete={() => deleteIntegration(row)}
        onDeleted={() => navigate("/integrations")}
      />
    </div>
  );
}

// The ruleset row — the ONE live check anywhere in Integrations (T.FOOTNOTE).
// Unknown until a later wave wires the real GitHub ref-confinement endpoint
// (see lib/api/integrations.ts's deriveScmRows) — never fabricated as a pass.
function GhVerdictRow({ row }: { row: IntegrationRow }) {
  const { text, tone } = describePosture(row.posture);
  const [label, checked] = text.split(" · ");
  const chipTone = tone === "success" ? "success" : tone === "warning" ? "warning" : "neutral";
  return (
    <div className="flex items-start gap-2.5 rounded-lg border border-border p-3">
      <Chip tone={chipTone} dot>
        {label}
      </Chip>
      <div className="min-w-0 flex-1 space-y-0.5">
        <span className={`text-xs ${POSTURE_CLASS[tone]}`}>{checked}</span>
        <p className="text-[0.6875rem] leading-snug text-muted-foreground">
          GitHub confirms tokens minted from this App stay confined to the refs each run requests. This row is the
          one thing on this page that really asks GitHub.
        </p>
      </div>
    </div>
  );
}
