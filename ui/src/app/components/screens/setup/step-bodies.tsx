/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Step bodies still standing after the 13->9 Getting Started collapse:
// Workspaces, Review, and Launch. HostProxyStep/ArtifactRepoStep used to live
// here too, kept alive only by the Integrations "Add integration" dialog's
// mirror/proxy hand-off; that hand-off retired when Corporate network became
// the single home for both, and the two bodies went with it. Mostly
// presentational — the caller owns SetupStatus AND the fetched SiteConfig (the
// sole owner — see useSiteConfigStep below), while each body owns its OWN
// writes (scanWorkspace, and the SiteConfig saves via the caller-owned
// saveSiteConfig).
import * as React from "react";
import { useNavigate } from "react-router-dom";
import { AlertTriangle, Info, Loader2, Plus, CircleCheck, Rocket, RotateCw, ScanSearch } from "lucide-react";
import type {
  SetupCheck,
  SetupCheckStatus,
  SetupStatus,
  SiteConfig,
  Workspace,
  WorkspaceProfile,
} from "../../../lib/types";
import { workspaces as workspacesApi } from "../../../lib/api/workspaces";
import { getErrorMessage } from "../../../lib/format";
import { Chip, ConfinementChip, SectionLabel } from "../../wardyn/primitives";
import { CC_META } from "../../wardyn/cc-meta";
import { BTN, OPERATOR_ONLY_REASON, RUN_MODE } from "../../wardyn/copy";
import { useOperator } from "../../wardyn/operator-context";
import { Button } from "../../ui/button";
import { SourcesLibrary } from "../sources-library";
import { ImageCatalog } from "../image-catalog";
import { WorkspaceWizard } from "../workspace-wizard/wizard";
import type { Readiness } from "../onboarding/intro";
import { lastCheckedLabel } from "../onboarding/intro";
import { toast } from "sonner";
import type { SetupStepId, StepBadge } from "./steps";
import { isUsable, statusTone, statusWord } from "../../../lib/workspace-status";

// ------------------------------------------------------------
// Shared check-row primitives (Review + the Corporate network step).
// ------------------------------------------------------------
const CHECK_ICON: Record<SetupCheckStatus, React.ElementType> = {
  ok: CircleCheck,
  warn: AlertTriangle,
  fail: AlertTriangle,
  info: Info,
};
const CHECK_COLOR: Record<SetupCheckStatus, string> = {
  ok: "text-success",
  warn: "text-warning",
  fail: "text-danger",
  info: "text-muted-foreground",
};

export function CheckRow({ check }: { check: SetupCheck }) {
  const Icon = CHECK_ICON[check.status] ?? Info;
  const color = CHECK_COLOR[check.status] ?? "text-muted-foreground";
  const fix = check.fix;
  return (
    <li className="flex items-start gap-2.5 rounded-lg border border-border p-3">
      <Icon className={`mt-0.5 size-4 shrink-0 ${color}`} />
      <div className="min-w-0 flex-1 space-y-0.5">
        <p className="text-sm font-medium text-foreground">{check.label}</p>
        {check.detail && <p className="text-xs text-muted-foreground">{check.detail}</p>}
        {check.status !== "ok" && fix && (
          <p className="text-xs text-muted-foreground">
            <span className="font-medium text-foreground">Fix: </span>
            {fix}
          </p>
        )}
      </div>
    </li>
  );
}

// ------------------------------------------------------------
// Review step — the consolidated readiness rollup (its own step, before Launch).
// Every cross-cutting check grouped by status (blockers → warnings → ready), plus
// the permanent "About this host" facts. These used to be dumped onto the barrier
// step even though they span steps 2–7; here they're a single honest go/no-go view.
// ------------------------------------------------------------
export function ReviewStep({
  status,
  readiness,
  onRecheck,
  rechecking,
  lastCheckedAt,
  onJump,
}: {
  status: SetupStatus;
  readiness: Readiness;
  onRecheck: () => void;
  rechecking: boolean;
  lastCheckedAt: Date | null;
  onJump: (id: SetupStepId) => void;
}) {
  // Actionable checks (exclude permanent platform facts — those are reference).
  const actionable = status.checks.filter((c) => !c.platform);
  const infoNotes = status.checks.filter((c) => c.platform);
  const blockers = actionable.filter((c) => c.status === "fail");
  const warnings = actionable.filter((c) => c.status === "warn");
  const ready = actionable.filter((c) => c.status === "ok" || c.status === "info");
  const group = (label: string, tone: StepBadge["tone"], checks: SetupCheck[]) =>
    checks.length > 0 && (
      <section className="space-y-2" key={label}>
        <div className="flex items-center gap-2">
          <SectionLabel>{label}</SectionLabel>
          <span
            className={`text-[0.6875rem] font-semibold tabular-nums ${
              tone === "warning" ? "text-warning" : tone === "success" ? "text-success" : "text-muted-foreground"
            }`}
          >
            {checks.length}
          </span>
        </div>
        <ul className="space-y-2">
          {checks.map((c) => (
            <CheckRow key={c.id} check={c} />
          ))}
        </ul>
      </section>
    );

  return (
    <div className="space-y-4">
      <div className="flex items-center justify-between gap-3">
        <p className="text-sm leading-relaxed text-muted-foreground">
          Every setup check, in one place. {readiness.ready && readiness.llmReady
            ? "The essentials are ready — you can launch."
            : "Not everything a first run needs is ready yet — review what's left."}
        </p>
        <div className="flex shrink-0 items-center gap-2.5">
          {lastCheckedAt && (
            <span className="text-[0.7188rem] text-muted-foreground">{lastCheckedLabel(lastCheckedAt)}</span>
          )}
          <Button variant="outline" size="sm" onClick={onRecheck} disabled={rechecking}>
            {rechecking ? <Loader2 className="size-3.5 animate-spin" /> : <RotateCw className="size-3.5" />}
            {BTN.recheck}
          </Button>
        </div>
      </div>

      {group("Blocking", "warning", blockers)}
      {group("Worth a look", "neutral", warnings)}
      {group("Ready", "success", ready)}

      {infoNotes.length > 0 && (
        <section className="space-y-2">
          <SectionLabel>About this host</SectionLabel>
          <p className="text-xs text-muted-foreground">
            Permanent facts about this machine — nothing to set up, just good to know.
          </p>
          <ul className="space-y-2">
            {infoNotes.map((c) => (
              <CheckRow key={c.id} check={c} />
            ))}
          </ul>
        </section>
      )}

      <p className="text-xs text-muted-foreground">
        Something needs a fix?{" "}
        <button
          type="button"
          onClick={() => onJump("environment")}
          className="font-medium text-primary hover:underline"
        >
          Back to the first step
        </button>
        , or jump straight to any step from the phase rail on the left — Review only summarizes; each
        item is fixed on its own step.
      </p>
    </div>
  );
}

// ------------------------------------------------------------
// SiteConfig writes: ONE owner (V2). The orchestrator (setup-screen) holds the
// fetched doc and hands each writing surface `siteConfig` + `reloadSiteConfig`/
// `saveSiteConfig`. HARD CONSTRAINT: every such surface re-GETs
// (reloadSiteConfig()) in a mount effect, on entry, before any save — the PUT is
// a shallow merge on top of the CURRENT doc, so a copy that's gone stale since
// another step's edit would otherwise silently clobber it.
//
// This hook is that prologue plus a `saving` flag around each write, written
// once so the guard can't drift between copies. `mutate` PUTs via the
// orchestrator-owned saveSiteConfig and reports failure as `false` — toasting
// the server's reason — so a caller only commits its own local field state once
// the PUT actually lands. Sole remaining consumer: corp-network-step.tsx.
// ------------------------------------------------------------
export function useSiteConfigStep(
  reloadSiteConfig: () => Promise<void>,
  saveSiteConfig: (next: SiteConfig) => Promise<void>,
) {
  const [saving, setSaving] = React.useState(false);

  React.useEffect(() => {
    reloadSiteConfig();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const mutate = async (next: SiteConfig, errorMessage: string): Promise<boolean> => {
    setSaving(true);
    try {
      await saveSiteConfig(next);
      return true;
    } catch (e) {
      toast.error(errorMessage, { description: getErrorMessage(e) });
      return false;
    } finally {
      setSaving(false);
    }
  };

  return { saving, mutate };
}

// ------------------------------------------------------------
// Workspaces step — onboard the local dirs/repos a run may attach. Recommended,
// not required: the composer's ephemeral path still launches with none. Reuses
// WorkspaceWizard + the /workspaces status vocabulary so the two can't drift.
// ------------------------------------------------------------
// One-line "what the scan found" summary for a ready workspace row, e.g.
// "2 languages · 3 secrets needed · postgres, redis". "needed" counts only the
// non-optional required_secrets; services show the first two (a run cares about
// the shape of the demand, not an exhaustive list). Null when the profile hasn't
// been scanned yet, or has nothing worth summarizing.
function workspaceProfileSummary(raw: Workspace["profile"]): string | null {
  if (!raw) return null;
  const p = raw as WorkspaceProfile;
  const parts: string[] = [];
  const langs = p.languages?.length ?? 0;
  if (langs) parts.push(`${langs} language${langs === 1 ? "" : "s"}`);
  // Count DECLARED config/credential secrets (from .env keys, ${} placeholders,
  // SealedSecrets, secretKeyRef) — NOT the code/CI-only references, which mix in
  // plain config. Report "needed" (must-supply) when any are non-optional, else
  // the declared total, so a k8s/SealedSecret-heavy repo (all optional/deploy-time)
  // never reads as secret-less.
  const declared = (p.required_secrets ?? []).filter((s) => s.kind !== "code" && s.kind !== "ci");
  const needed = declared.filter((s) => !s.optional).length;
  if (needed) parts.push(`${needed} secret${needed === 1 ? "" : "s"} needed`);
  else if (declared.length) parts.push(`${declared.length} secret${declared.length === 1 ? "" : "s"} declared`);
  const services = p.services_needed ?? [];
  if (services.length) {
    const shown = services.slice(0, 2).join(", ");
    parts.push(services.length > 2 ? `${shown} +${services.length - 2}` : shown);
  }
  const leaks = (p.leak_findings ?? []).length;
  if (leaks) parts.push(`${leaks} suspected leak${leaks === 1 ? "" : "s"}`);
  return parts.length ? parts.join(" · ") : null;
}

// Tier 1 — its own Getting-started step: the shared library of directories &
// repos, configured once and attached by any number of workspaces.
export function SourcesStep({ workspaces }: { workspaces: Workspace[] }) {
  const operator = useOperator();
  return (
    <div className="space-y-4">
      <p className="text-sm leading-relaxed text-muted-foreground">
        Configured once — each directory or repo carries its own requirements and scan, and every
        workspace that needs it attaches the same entry.
        {!operator && ` ${OPERATOR_ONLY_REASON}`}
      </p>
      <SourcesLibrary workspaces={workspaces} embedded />
    </div>
  );
}

// Tier 2 — its own Getting-started step: the shared base-image catalog.
export function ImagesStep({ workspaces }: { workspaces: Workspace[] }) {
  const operator = useOperator();
  return (
    <div className="space-y-4">
      <p className="text-sm leading-relaxed text-muted-foreground">
        Shared across workspaces — a registry image, a custom recipe, or one you bring. A workspace
        without one runs on its derived recommended build.
        {!operator && ` ${OPERATOR_ONLY_REASON}`}
      </p>
      <ImageCatalog workspaces={workspaces} embedded />
    </div>
  );
}

export function WorkspacesStep({
  workspaces,
  loading,
  onReload,
}: {
  workspaces: Workspace[];
  loading: boolean;
  onReload: () => void;
}) {
  // Onboard/scan are workspace writes — operator-only (see http.go).
  const operator = useOperator();
  const navigate = useNavigate();
  // "Add workspace" now routes through the four-step wizard (Sources -> Base
  // image -> Requirements -> Done) instead of the retired guided Import panel.
  const [wizardOpen, setWizardOpen] = React.useState(false);
  const [scanning, setScanning] = React.useState<Set<string>>(new Set());

  // Best-effort scan → always refresh (repo scans run async and return 202, local
  // dirs resolve inline) so the row reflects the latest status either way.
  const scan = async (w: Workspace) => {
    setScanning((s) => new Set(s).add(w.id));
    try {
      const { async: isAsync } = await workspacesApi.scanWorkspace(w.id);
      if (isAsync) {
        toast.info(`Scan started for "${w.name}"`, {
          description: "A governed scan run is analyzing the repo; the status updates when it completes.",
        });
      }
    } catch (e) {
      toast.error(`Failed to scan "${w.name}"`, {
        description: getErrorMessage(e),
      });
    } finally {
      onReload();
      setScanning((s) => {
        const next = new Set(s);
        next.delete(w.id);
        return next;
      });
    }
  };

  return (
    <div className="space-y-4">
      <p className="text-sm leading-relaxed text-muted-foreground">
        A workspace brings your directories &amp; repos and a base image together — that combination is
        what a run attaches, never a raw host path (ephemeral scratch works too, so a task that needs
        no repo can still run).
        {!operator && ` ${OPERATOR_ONLY_REASON}`}
      </p>

      {loading ? (
        <p className="text-sm text-muted-foreground">Loading workspaces…</p>
      ) : workspaces.length === 0 ? (
        <div className="rounded-xl border border-dashed border-border p-6 text-center">
          <p className="text-sm text-muted-foreground">No workspaces onboarded yet.</p>
          <Button className="mt-3" onClick={() => setWizardOpen(true)} disabled={!operator}>
            <Plus className="size-4" /> Onboard your first workspace
          </Button>
        </div>
      ) : (
        <>
          <ul className="space-y-2">
            {workspaces.map((w) => {
              const summary = workspaceProfileSummary(w.profile);
              return (
              <li key={w.id} className="flex items-center gap-3 rounded-lg border border-border p-3">
                <span className="min-w-0 flex-1">
                  <span className="block truncate text-sm font-medium text-foreground">{w.name}</span>
                  <span className="block truncate font-mono text-xs text-muted-foreground">
                    {w.kind === "repo" ? "repo" : "local dir"} · {w.source}
                  </span>
                  {summary && (
                    <span className="block truncate text-[0.6875rem] text-muted-foreground">{summary}</span>
                  )}
                  {statusTone(w.status).tone === "danger" && (
                    // The failure reason itself is only in the import overlay's scan
                    // pane (the toast is ephemeral) — point at the recovery path
                    // inline instead of leaving a bare red chip.
                    <span className="block text-[0.6875rem] text-danger">
                      Scan failed — Resume import to see what went wrong and retry.
                    </span>
                  )}
                </span>
                {scanning.has(w.id) ? (
                  <Chip tone="neutral" dot>
                    <Loader2 className="size-3 animate-spin" /> Scanning…
                  </Chip>
                ) : (
                  <Chip tone={statusTone(w.status).tone} dot>
                    {statusWord(w.status)}
                  </Chip>
                )}
                {isUsable(w.status) ? (
                  <Button
                    variant="ghost"
                    size="sm"
                    onClick={() => scan(w)}
                    disabled={!operator || scanning.has(w.id)}
                  >
                    <ScanSearch className="size-3.5" /> Scan
                  </Button>
                ) : (
                  // Resuming an import is now the detail page's job (its own
                  // primary action offers Scan now / Retry scan contextually) —
                  // this just opens it. Navigation, not a write, so it isn't
                  // operator-gated.
                  <Button
                    variant="ghost"
                    size="sm"
                    onClick={() => navigate(`/workspaces/${encodeURIComponent(w.id)}`)}
                  >
                    <ScanSearch className="size-3.5" /> Open
                  </Button>
                )}
              </li>
              );
            })}
          </ul>
          <Button variant="outline" size="sm" onClick={() => setWizardOpen(true)} disabled={!operator}>
            <Plus className="size-4" /> Add workspace
          </Button>
        </>
      )}

      {/* The four-step "Add workspace" wizard — its own Dialog on top; returns
          here via onClose (like NewRunDialog returns to SetupScreen). Mounted
          only while open (its Dialog is unconditionally open internally). */}
      {wizardOpen && (
        <WorkspaceWizard
          origin="setup"
          onClose={() => setWizardOpen(false)}
          onWorkspaceCreated={onReload}
          onOpenWorkspace={(id) => {
            setWizardOpen(false);
            navigate(`/workspaces/${encodeURIComponent(id)}`);
          }}
        />
      )}
    </div>
  );
}

// ------------------------------------------------------------
// Launch step — example config marked "Example — not live config" (B12).
// ------------------------------------------------------------
export function LaunchStep({
  status,
  onLaunch,
  onOpenRuns,
  // canLaunch gates on a barrier only (readiness.ready) — an interactive run works
  // with no model. llmReady drives a NON-blocking amber notice when a barrier is up
  // but no model is connected: the run still launches, you just drive it by hand.
  canLaunch,
  llmReady = false,
}: {
  status: SetupStatus;
  onLaunch: () => void;
  onOpenRuns: () => void;
  canLaunch: boolean;
  llmReady?: boolean;
}) {
  const example: [string, React.ReactNode][] = [
    ["Task", '"Add a health check endpoint and a unit test for it"'],
    ["Agent", "Claude Code"],
    [
      "Barrier",
      <span className="inline-flex items-center gap-1.5" key="barrier">
        <ConfinementChip value="CC1" /> ready now — harden to {CC_META.CC2.label} later
      </span>,
    ],
    ["Mode", `${RUN_MODE.interactive.label} — ${RUN_MODE.interactive.blurb}`],
  ];
  return (
    <div className="space-y-4">
      <p className="text-sm leading-relaxed text-muted-foreground">
        Configure your first run — pick the agent, what it can reach, and how strongly it&apos;s walled
        off. If an AI composer backend is configured, you can describe the task in plain language instead.
        Working on a specific repo? Onboard it in the Workspaces step first, or the run can&apos;t reach it.
      </p>

      {status.has_runs && (
        <p className="flex items-center gap-1.5 text-sm text-success">
          <CircleCheck className="size-4" /> You&apos;ve already launched a run on this control plane.
        </p>
      )}

      <div className="rounded-xl border border-border bg-card p-4">
        <div className="mb-3 flex items-center gap-2">
          <Chip tone="info" className="uppercase tracking-wide">
            Example
          </Chip>
          <span className="text-xs text-muted-foreground">
            Not live config — just to show the shape of a run.
          </span>
        </div>
        <dl className="grid grid-cols-[auto_1fr] gap-x-3.5 gap-y-2 text-sm">
          {example.map(([k, v]) => (
            <React.Fragment key={k}>
              <dt className="text-muted-foreground">{k}</dt>
              <dd className="text-foreground">{v}</dd>
            </React.Fragment>
          ))}
        </dl>
        <div className="mt-4 flex flex-wrap items-center gap-2">
          <Button onClick={onLaunch} disabled={!canLaunch}>
            <Rocket className="size-4" /> Launch your first run
          </Button>
          <Button variant="outline" onClick={onOpenRuns}>
            Open Runs
          </Button>
        </div>
        {!canLaunch ? (
          <p className="mt-2 text-xs text-muted-foreground">
            A sandbox barrier is required first.
          </p>
        ) : (
          !llmReady && (
            <p className="mt-2 flex items-start gap-1.5 rounded-md border border-border bg-muted/40 px-2.5 py-1.5 text-xs leading-snug text-muted-foreground">
              <Info className="mt-0.5 size-3.5 shrink-0" aria-hidden="true" />
              No model connected — that's fine. Run a plain governed command (bring your own
              container), or drive an interactive run yourself over an attached terminal. A model is
              only needed to run an agent under Wardyn's harness or for the AI Composer.
            </p>
          )
        )}
      </div>

      <p className="text-xs text-muted-foreground">
        If you finish without launching, Runs greets you with this same shortcut until your first run exists.
      </p>
    </div>
  );
}
