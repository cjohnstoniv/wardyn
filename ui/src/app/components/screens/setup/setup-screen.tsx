/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// SetupScreen — the "Getting started" first-run FUNNEL (redesign). Collapses the
// old tour-then-wizard double stepper into ONE progress model: a dismissible
// IntroPanel above a single stepper with honest, reality-driven badges (B2/B4),
// a ready-host fast path (B3), and no-dead-end re-check feedback (B5).
//
// Read-only against GET /api/v1/setup/status (the FROZEN SetupStatus contract in
// lib/types.ts) except for setSecret() writes in the provider/credentials steps.
// Never traps the operator: "Finish later" and launching both dismiss it, and
// every AppShell nav item stays reachable while it's open.
import * as React from "react";
import {
  AlertTriangle,
  ArrowLeft,
  ArrowRight,
  Check,
  CircleCheck,
  Cloud,
  Github,
  Info,
  KeyRound,
  Loader2,
  Plus,
  Rocket,
  RotateCw,
  ScanSearch,
  X,
} from "lucide-react";
import type {
  ConfinementClass,
  HostProxyDetection,
  HostProxySetting,
  SetupCheck,
  SetupCheckStatus,
  SetupStatus,
  SiteConfig,
  Workspace,
  WorkspaceProfile,
} from "../../../lib/types";
import { api } from "../../../lib/api";
import { getErrorMessage } from "../../../lib/format";
import { lsGet, lsSet } from "../../../lib/storage";
import { Chip, ConfinementChip, SectionLabel } from "../../wardyn/primitives";
import { StatusChip } from "../../wardyn/status-chip";
import { CC_HINTS, CC_META } from "../../wardyn/cc-meta";
import {
  getDefaultCc,
  resolveDefaultCc,
  setDefaultCc,
  strongestAvailable,
} from "../../wardyn/default-confinement";
import { BTN, EXIT_VERB, RUN_MODE } from "../../wardyn/copy";
import { Button } from "../../ui/button";
import { Input } from "../../ui/input";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "../../ui/select";
import { DomainPillList, Field } from "../new-run/step-shell";
import { AddSecretDialog } from "../secrets";
import { NewRunDialog } from "../new-run/new-run-dialog";
import { RunnerTiers, LlmAccess, ComposerBackends } from "./setup-sections";
import { STATUS_TONE, STATUS_LABEL } from "../workspaces";
import { ImportWorkspaceDialog } from "../import-workspace/import-panel";
import { SetupGuideDialog, type SetupGuide } from "./setup-guide";
import { toast } from "sonner";
import {
  IntroPanel,
  deriveReadiness,
  hasGitCredential,
  lastCheckedLabel,
  type Readiness,
} from "../onboarding/intro";

// ------------------------------------------------------------
// Dismiss flag — via lib/storage's private-mode-tolerant lsGet/lsSet.
// ------------------------------------------------------------
const DISMISS_KEY = "wardyn-setup-dismissed";

export function setupDismissed(): boolean {
  return lsGet(DISMISS_KEY) === "1";
}

export function dismissSetup(): void {
  lsSet(DISMISS_KEY, "1");
}

// Pure decision helper (unit-testable, and used verbatim by App.tsx): guide the
// operator into "Getting started" on a fresh, local, single-operator console.
// `ready` means runs *can* launch on this host — NOT that the operator has been
// onboarded — so a brand-new control plane with no runs yet still opens even when
// ready (that is the whole point of first-run setup). An explicit dismissal
// (Finish later / launch) or a first launched run stops it, and it never
// force-opens on a hosted/SSO multi-admin control plane.
export function shouldOpenSetup(status: SetupStatus, dismissed: boolean): boolean {
  if (dismissed || status.auth.mode !== "local") return false;
  return !status.has_runs || !status.ready;
}

// ------------------------------------------------------------
// Steps — ids/labels FROZEN (App.tsx + tests read these). The step body heading
// is the friendlier, task-shaped title (D5/B9).
// ------------------------------------------------------------
export type SetupStepId =
  | "environment"
  | "provider"
  | "host_proxy"
  | "scm_provider"
  | "artifact_repo"
  | "workspaces"
  | "credentials"
  | "review"
  | "launch";

export const SETUP_STEPS: { id: SetupStepId; label: string }[] = [
  { id: "environment", label: "Environment" },
  { id: "provider", label: "Model/Harness Provider" },
  { id: "host_proxy", label: "Host Proxy" },
  { id: "scm_provider", label: "SCM Provider" },
  { id: "artifact_repo", label: "Artifact Redirect" },
  { id: "workspaces", label: "Workspaces" },
  { id: "credentials", label: "Credentials" },
  { id: "review", label: "Review" },
  { id: "launch", label: "Launch" },
];

const STEP_HEADING: Record<SetupStepId, string> = {
  environment: "Pick your barrier",
  provider: "Connect a model or agent harness",
  host_proxy: "Corporate host proxy",
  scm_provider: "Source control provider",
  artifact_repo: "Artifact registry redirection",
  workspaces: "Onboard a workspace",
  credentials: "Repo & cloud credentials",
  review: "Review readiness",
  launch: "Launch your first run",
};

// Client-side fallback copy for well-known check ids, used only when the
// backend's own `check.fix` is absent/terse. Sourced from the same honest
// CC_HINTS wording the confinement chip/wizard use, so the copy can't drift.
const FIX_HINTS: Record<string, { fix: string }> = {
  confinement: { fix: `Enable gVisor as a Docker runtime for the Wall (default) tier: ${CC_HINTS.CC2}` },
  gvisor: { fix: `Enable gVisor as a Docker runtime for the Wall (default) tier: ${CC_HINTS.CC2}` },
  wsl: {
    fix: "WSL2 splits networking from Windows — bind wardynd to 0.0.0.0 (not localhost) so the console and agents on the Windows side can reach it.",
  },
};

// ------------------------------------------------------------
// Honest per-step badges (B4) — reflect reality, never a false "Done". The
// circle-done state is the same honest signal; credentials is always Optional (B8).
// ------------------------------------------------------------
type StepBadge = { text: string; tone: "success" | "warning" | "neutral" };

// Badge for a corporate-baseline step (Host Proxy / SCM Provider / Artifact
// Redirect): always non-blocking (B8-style) — these backend checks are
// hardcoded "info"-tier (see hostProxyCheck/scm_provider/artifactRepoCheck in
// internal/api/setup.go), never "ok", so a check.status==='ok' read could
// NEVER show "Configured" even once the operator had wired it up (M21). The
// badge instead derives readiness client-side from the actual SiteConfig field
// each step's own body edits — the honest default stays a neutral "Optional"
// nudge until that field is genuinely set.
function siteConfigBadge(
  cfg: SiteConfig | null,
  checkId: "host_proxy" | "scm_provider" | "artifact_repo",
): StepBadge {
  const configured =
    checkId === "host_proxy"
      ? !!cfg?.upstream_proxy_secret_ref
      : checkId === "scm_provider"
        ? !!cfg?.scm_hosts?.length
        : !!cfg?.artifact_overrides && Object.keys(cfg.artifact_overrides).length > 0;
  return configured
    ? { text: "Configured", tone: "success" }
    : { text: "Optional", tone: "neutral" };
}

function stepBadges(
  status: SetupStatus,
  r: Readiness,
  workspaces: Workspace[],
  siteConfig: SiteConfig | null,
): Record<SetupStepId, StepBadge> {
  return {
    environment: r.barrierReady
      ? { text: `Ready · ${r.barrierCount} of 3 barriers`, tone: "success" }
      : { text: "Needs setup", tone: "warning" },
    provider: r.llmReady
      ? { text: r.llmLabel ? `Ready · ${r.llmLabel}` : "Ready", tone: "success" }
      : { text: "Needs setup", tone: "warning" },
    host_proxy: siteConfigBadge(siteConfig, "host_proxy"),
    scm_provider: siteConfigBadge(siteConfig, "scm_provider"),
    artifact_repo: siteConfigBadge(siteConfig, "artifact_repo"),
    // Recommended, not required (B8-style): onboarding a workspace lets a first run
    // touch your own code, but the ephemeral path still launches with none — so the
    // empty state is a neutral "Optional" nudge, never a red "Needs setup". (The
    // step body carries the stronger "add at least one" copy; the badge word stays
    // "Optional" to avoid clashing with the barrier tier's "Recommended" chip.)
    workspaces: workspaces.length
      ? { text: `Ready · ${workspaces.length} onboarded`, tone: "success" }
      : { text: "Optional", tone: "neutral" },
    credentials: { text: "Optional", tone: "neutral" },
    // Review rolls up every check. It's "warning" only when a real blocker exists
    // (a failing check), else a neutral/green summary — the readiness verdict, not
    // a per-topic nag (those live on their own steps now).
    review: status.checks.some((c) => c.status === "fail")
      ? { text: "Needs attention", tone: "warning" }
      : r.ready
        ? { text: "All essentials ready", tone: "success" }
        : { text: "Review what's left", tone: "neutral" },
    launch: status.has_runs
      ? { text: "First run launched", tone: "success" }
      : r.ready
        ? { text: "Ready to launch", tone: "success" }
        : { text: "Set up the essentials first", tone: "neutral" },
  };
}

function stepDone(
  status: SetupStatus,
  r: Readiness,
  workspaces: Workspace[],
): Record<SetupStepId, boolean> {
  return {
    environment: r.barrierReady && !status.checks.some((c) => c.status === "fail"),
    provider: r.llmReady,
    // Non-blocking, so "done" is purely cosmetic — true only once the backend
    // itself reports the check as "ok" (never inferred client-side).
    host_proxy: status.checks.find((c) => c.id === "host_proxy")?.status === "ok",
    scm_provider: status.checks.find((c) => c.id === "scm_provider")?.status === "ok",
    artifact_repo: status.checks.find((c) => c.id === "artifact_repo")?.status === "ok",
    workspaces: workspaces.length > 0,
    credentials: hasGitCredential(status),
    review: r.ready && !status.checks.some((c) => c.status === "fail"),
    launch: status.has_runs,
  };
}

// ------------------------------------------------------------
// Funnel stepper — ONE stepper (B2). Clickable; honest sub-badges (B4).
// ------------------------------------------------------------
function FunnelStepper({
  current,
  onJump,
  done,
  badges,
}: {
  current: SetupStepId;
  onJump: (id: SetupStepId) => void;
  done: Record<SetupStepId, boolean>;
  badges: Record<SetupStepId, StepBadge>;
}) {
  const badgeColor: Record<StepBadge["tone"], string> = {
    success: "text-success",
    warning: "text-warning",
    neutral: "text-muted-foreground",
  };
  return (
    <div className="flex items-stretch gap-1.5">
      {SETUP_STEPS.map((s, i) => {
        const active = s.id === current;
        const isDone = done[s.id];
        const badge = badges[s.id];
        return (
          <button
            key={s.id}
            onClick={() => onJump(s.id)}
            className={
              active
                ? "flex min-w-0 flex-1 items-center gap-2.5 rounded-[10px] border border-border-strong bg-muted p-2.5 text-left transition-colors"
                : "flex min-w-0 flex-1 items-center gap-2.5 rounded-[10px] border border-border bg-card p-2.5 text-left transition-colors hover:bg-muted/60"
            }
          >
            <span
              className={
                active
                  ? "flex size-6 shrink-0 items-center justify-center rounded-full border border-primary bg-primary text-[12px] font-semibold text-primary-foreground"
                  : isDone
                    ? "flex size-6 shrink-0 items-center justify-center rounded-full border border-success/30 bg-success-subtle text-[12px] font-semibold text-success"
                    : "flex size-6 shrink-0 items-center justify-center rounded-full border border-border text-[12px] font-semibold text-muted-foreground"
              }
            >
              {isDone && !active ? <Check className="size-3.5" /> : i + 1}
            </span>
            <span className="min-w-0 flex-1">
              <span
                className={`block truncate text-[13px] font-semibold ${active ? "text-foreground" : "text-muted-foreground"}`}
              >
                {s.label}
              </span>
              <span className={`block truncate text-[11px] font-medium ${badgeColor[badge.tone]}`}>
                {badge.text}
              </span>
            </span>
          </button>
        );
      })}
    </div>
  );
}

// ------------------------------------------------------------
// Environment step — barrier-led (B9/D5), with re-check feedback (B5).
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

function CheckRow({ check }: { check: SetupCheck }) {
  const Icon = CHECK_ICON[check.status] ?? Info;
  const color = CHECK_COLOR[check.status] ?? "text-muted-foreground";
  const fix = check.fix || FIX_HINTS[check.id]?.fix;
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

function EnvironmentStep({
  status,
  onRecheck,
  rechecking,
  recheckedOnce,
  lastCheckedAt,
  selected,
  onSelect,
}: {
  status: SetupStatus;
  onRecheck: () => void;
  rechecking: boolean;
  recheckedOnce: boolean;
  lastCheckedAt: Date | null;
  selected: ConfinementClass;
  onSelect: (cc: ConfinementClass) => void;
}) {
  // The barrier step is barrier-only now (D5): the runner check IS the barrier, and
  // it's shown inline on the tier cards. The cross-cutting checks (LLM, proxy, SCM,
  // artifact, secrets…) and permanent host facts moved to their own steps + the
  // Review step — they were never barrier-specific.
  return (
    <div className="space-y-4">
      <div className="flex items-center justify-between gap-3">
        <p className="text-[13.5px] leading-relaxed text-muted-foreground">
          Every run sits behind one. Start where you are — you can harden later without changing anything
          else.
        </p>
        <div className="flex shrink-0 items-center gap-2.5">
          {lastCheckedAt && (
            <span className="text-[11.5px] text-muted-foreground">{lastCheckedLabel(lastCheckedAt)}</span>
          )}
          <Button variant="outline" size="sm" onClick={onRecheck} disabled={rechecking}>
            {rechecking ? <Loader2 className="size-3.5 animate-spin" /> : <RotateCw className="size-3.5" />}
            {BTN.recheck}
          </Button>
        </div>
      </div>

      <RunnerTiers
        runner={status.runner}
        platform={status.platform}
        rechecking={rechecking}
        recheckedOnce={recheckedOnce}
        selected={selected}
        onSelect={onSelect}
      />

      <p className="text-xs text-muted-foreground">
        The rest of setup — model, proxy, SCM, artifacts, workspaces — has its own step. The final{" "}
        <span className="font-medium text-foreground">Review</span> step rolls everything up before you launch.
      </p>
    </div>
  );
}

// ------------------------------------------------------------
// Review step — the consolidated readiness rollup (its own step, before Launch).
// Every cross-cutting check grouped by status (blockers → warnings → ready), plus
// the permanent "About this host" facts. These used to be dumped onto the barrier
// step even though they span steps 2–7; here they're a single honest go/no-go view.
// ------------------------------------------------------------
function ReviewStep({
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
            className={`text-[11px] font-semibold tabular-nums ${
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
        <p className="text-[13.5px] leading-relaxed text-muted-foreground">
          Everything setup checked, in one place. {readiness.ready
            ? "The essentials are ready — you can launch."
            : "Nothing here blocks a first run, but review what's still open."}
        </p>
        <div className="flex shrink-0 items-center gap-2.5">
          {lastCheckedAt && (
            <span className="text-[11.5px] text-muted-foreground">{lastCheckedLabel(lastCheckedAt)}</span>
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
        A check in the wrong place?{" "}
        <button
          type="button"
          onClick={() => onJump("environment")}
          className="font-medium text-primary hover:underline"
        >
          Jump back to any step
        </button>{" "}
        from the stepper above — the Review only summarizes; each item is fixed on its own step.
      </p>
    </div>
  );
}

// ------------------------------------------------------------
// Provider step
// ------------------------------------------------------------
function ProviderStep({
  status,
  readiness,
  onAddSecret,
  onSetup,
  onRecheck,
  rechecking,
}: {
  status: SetupStatus;
  readiness: Readiness;
  onAddSecret: (name: string) => void;
  onSetup: (g: SetupGuide) => void;
  onRecheck: () => void;
  rechecking: boolean;
}) {
  // Guidance for the most common first-run snag: a personal machine running the
  // sealed (compose/team) control plane, which can't see the host's Claude login —
  // so this step reads "not connected" even when the operator IS logged in. Only
  // shown when the model is genuinely undetected AND we're blind-in-compose on a
  // local box (host_like === false + local auth); host mode never sees it.
  const suggestHostMode =
    !readiness.llmReady && status.deployment?.host_like === false && status.auth.mode === "local";

  return (
    <div className="space-y-5">
      <p className="text-[13.5px] leading-relaxed text-muted-foreground">
        {readiness.llmReady
          ? `One connected path is enough — you're already covered by ${readiness.llmLabel || "a connected model"}.`
          : "Wardyn needs a way for the agent to talk to an LLM — a stored API key the proxy injects, or a resident CLI subscription."}
      </p>

      {suggestHostMode && (
        <div className="flex items-start gap-2.5 rounded-lg border border-border bg-muted/40 p-3">
          <Info className="mt-0.5 size-4 shrink-0 text-primary" />
          <div className="min-w-0 flex-1 space-y-1.5 text-[12.5px] leading-relaxed">
            <p className="text-foreground">
              Sandboxing your <span className="font-medium">own machine</span>, and already logged into the
              Claude CLI? This is the containerized (team) setup — wardynd runs sealed in a container that
              can&apos;t see your host&apos;s <code className="rounded bg-background/70 px-1 py-0.5 text-[11.5px]">~/.claude</code>{" "}
              login, which is why it reads &quot;not connected&quot; even though you are. The local setup uses
              your existing login automatically — no re-login, no stored key:
            </p>
            <p>
              <code className="rounded bg-background/70 px-1.5 py-0.5 font-mono text-[12px] text-foreground">
                make setup-host
              </code>
            </p>
          </div>
        </div>
      )}

      <LlmAccess
        status={status}
        onAddSecret={onAddSecret}
        onSetup={onSetup}
        onRecheck={onRecheck}
        rechecking={rechecking}
      />

      <section className="space-y-2">
        <SectionLabel>AI Run Composer backends</SectionLabel>
        <p className="text-[11px] leading-snug text-muted-foreground">
          The backend that writes the run proposal — advisory only; it never gets the run&apos;s own model
          credentials. (Those come from the LLM access above.)
        </p>
        <ComposerBackends status={status} onAddSecret={onAddSecret} />
      </section>

      {status.restart_required && (
        <p className="flex items-center gap-1.5 text-xs text-muted-foreground">
          <AlertTriangle className="size-3.5 text-warning" />
          restart wardynd to apply{status.restart_reason ? `: ${status.restart_reason}` : ""}.
        </p>
      )}
    </div>
  );
}

// ------------------------------------------------------------
// Corporate-baseline steps (Host Proxy / SCM Provider / Artifact Redirect) — all
// three are non-blocking "info"-tier checks (see internal/api/setup.go's
// hostProxyCheck/scm_provider/artifactRepoCheck): they never gate readiness, they
// just let an operator wire the SiteConfig baseline every run inherits. Each loads
// the current SiteConfig itself (GET is idempotent/cheap) and PUTs a shallow merge
// on top of it so editing one field never clobbers what another step wrote.
// ------------------------------------------------------------
const ARTIFACT_ECOSYSTEMS = ["npm", "pip", "cargo", "maven", "go", "nuget"] as const;

// Shared load/save plumbing for the three steps above: load the current
// SiteConfig once on mount (an absent config is a fresh `{}`, not an error),
// and PUT a shallow-merged next value, rolling local state back only on
// success so a rejected PUT doesn't desync the UI from the server.
function useSiteConfig(onLoad?: (c: SiteConfig) => void) {
  const [cfg, setCfg] = React.useState<SiteConfig | null>(null);
  const onLoadRef = React.useRef(onLoad);
  onLoadRef.current = onLoad;

  React.useEffect(() => {
    api
      .getSiteConfig()
      .then((c) => {
        setCfg(c);
        onLoadRef.current?.(c);
      })
      .catch(() => setCfg({}));
  }, []);

  const save = async (next: SiteConfig, errorMessage: string): Promise<boolean> => {
    try {
      await api.putSiteConfig(next);
      setCfg(next);
      return true;
    } catch (e) {
      toast.error(errorMessage, { description: getErrorMessage(e) });
      return false;
    }
  };

  return { cfg, save };
}

function RecheckButton({ onRecheck, rechecking }: { onRecheck: () => void; rechecking: boolean }) {
  return (
    <Button variant="ghost" size="sm" onClick={onRecheck} disabled={rechecking}>
      {rechecking ? <Loader2 className="size-3.5 animate-spin" /> : <RotateCw className="size-3.5" />}
      {BTN.recheck}
    </Button>
  );
}

function ProxySettingRow({ label, setting }: { label: string; setting: HostProxySetting }) {
  return (
    <div className="flex items-center justify-between gap-2 text-xs">
      <span className="shrink-0 text-muted-foreground">{label}</span>
      <div className="flex min-w-0 items-center gap-1.5">
        <span className="truncate font-mono text-foreground" title={setting.value}>
          {setting.value}
        </span>
        <Chip tone="neutral">{setting.source}</Chip>
        {setting.has_credentials && <Chip tone="warning">creds</Chip>}
      </div>
    </div>
  );
}

// HostProxyBreakdown renders the masked host-proxy detection (setup.go host_proxy)
// read-only. Values are already masked server-side. Renders nothing when the host
// has no proxy configured (Go always emits at least {has_credentials:false}).
function HostProxyBreakdown({ detection: d }: { detection: HostProxyDetection }) {
  const envRows: Array<[string, HostProxySetting | undefined]> = [
    ["HTTP_PROXY", d.http_proxy],
    ["HTTPS_PROXY", d.https_proxy],
    ["ALL_PROXY", d.all_proxy],
    ["NO_PROXY", d.no_proxy],
  ];
  const git = d.git_proxy;
  const hasEnv = envRows.some(([, s]) => s);
  const hasGit = !!(git && (git.http_proxy || git.https_proxy));
  const hasTools = !!(d.tool_configs && d.tool_configs.length);
  const hasMismatch = !!(d.env_case_mismatch && d.env_case_mismatch.length);
  if (!hasEnv && !hasGit && !hasTools && !d.pac && !hasMismatch) return null;

  return (
    <div className="space-y-2 rounded-lg border border-border p-3">
      <p className="text-xs font-medium text-foreground">Detected on this host</p>
      {hasEnv &&
        envRows.map(([label, s]) => (s ? <ProxySettingRow key={label} label={label} setting={s} /> : null))}
      {hasGit && (
        <>
          {git!.http_proxy && <ProxySettingRow label="git http.proxy" setting={git!.http_proxy} />}
          {git!.https_proxy && <ProxySettingRow label="git https.proxy" setting={git!.https_proxy} />}
        </>
      )}
      {hasTools &&
        d.tool_configs!.map((t, i) => (
          <div key={`${t.tool}-${i}`} className="flex items-center justify-between gap-2 text-xs">
            <span className="shrink-0 text-muted-foreground">
              {t.tool} <span className="opacity-70">({t.path})</span>
            </span>
            <div className="flex min-w-0 items-center gap-1.5">
              <span className="truncate font-mono text-foreground" title={t.setting.value}>
                {t.setting.value}
              </span>
              <Chip tone="neutral">{t.setting.source}</Chip>
            </div>
          </div>
        ))}
      {d.pac && (
        <div className="flex items-center justify-between gap-2 text-xs">
          <span className="shrink-0 text-muted-foreground">PAC / auto-config</span>
          <div className="flex min-w-0 items-center gap-1.5">
            <span className="truncate font-mono text-foreground" title={d.pac.url}>
              {d.pac.url}
            </span>
            <Chip tone="warning" title="Never fetched — resolve the effective proxy manually">
              manual
            </Chip>
          </div>
        </div>
      )}
      {hasMismatch && (
        <div className="flex flex-wrap items-center gap-1.5">
          <span className="text-xs text-muted-foreground">Env case mismatch:</span>
          {d.env_case_mismatch!.map((m) => (
            <Chip key={m} tone="warning" mono>
              {m}
            </Chip>
          ))}
        </div>
      )}
      {d.has_credentials && (
        <div className="flex items-start gap-1.5 text-xs">
          <Chip tone="warning">credential</Chip>
          <span className="text-muted-foreground">
            A proxy credential was detected (masked above). Store it as a secret below so runs can
            authenticate to the upstream proxy.
          </span>
        </div>
      )}
    </div>
  );
}

function HostProxyStep({
  status,
  onRecheck,
  rechecking,
}: {
  status: SetupStatus;
  onRecheck: () => void;
  rechecking: boolean;
}) {
  const check = status.checks.find((c) => c.id === "host_proxy");
  const [secretName, setSecretName] = React.useState("");
  const [saving, setSaving] = React.useState(false);
  const { cfg, save: saveConfig } = useSiteConfig((c) => setSecretName(c.upstream_proxy_secret_ref ?? ""));

  const save = async () => {
    const name = secretName.trim();
    setSaving(true);
    const next: SiteConfig = { ...(cfg ?? {}), upstream_proxy_secret_ref: name || undefined };
    if (await saveConfig(next, "Failed to save the upstream proxy secret")) {
      toast.success("Upstream proxy secret saved");
    }
    setSaving(false);
  };

  return (
    <div className="space-y-5">
      <p className="text-[13.5px] leading-relaxed text-muted-foreground">
        Wardyn detected these host proxy settings; the sandbox reaches the internet only through
        wardyn-proxy, which can chain through your corporate proxy.
      </p>

      {check && (
        <ul>
          <CheckRow check={check} />
        </ul>
      )}

      {status.host_proxy && <HostProxyBreakdown detection={status.host_proxy} />}

      <Field
        label="Upstream proxy secret name"
        htmlFor="host-proxy-secret"
        hint="Create this secret (holding the upstream proxy URL) via the Add-secret flow in the Provider or Credentials step first."
      >
        <div className="flex gap-2">
          <Input
            id="host-proxy-secret"
            value={secretName}
            onChange={(e) => setSecretName(e.target.value)}
            placeholder="upstream-proxy-url"
            className="font-mono"
          />
          <Button variant="outline" onClick={save} disabled={saving || cfg === null}>
            {saving ? <Loader2 className="size-4 animate-spin" /> : "Save"}
          </Button>
        </div>
      </Field>

      <RecheckButton onRecheck={onRecheck} rechecking={rechecking} />
    </div>
  );
}

function ScmProviderStep({
  status,
  onAddSecret,
  onRecheck,
  rechecking,
}: {
  status: SetupStatus;
  onAddSecret: (name: string) => void;
  onRecheck: () => void;
  rechecking: boolean;
}) {
  const check = status.checks.find((c) => c.id === "scm_provider");
  const [host, setHost] = React.useState("");
  const [saving, setSaving] = React.useState(false);
  const { cfg, save: saveConfig } = useSiteConfig();

  const addHost = async () => {
    const h = host.trim().toLowerCase();
    if (!h) return;
    setSaving(true);
    const hosts = Array.from(new Set([...(cfg?.scm_hosts ?? []), h]));
    const next: SiteConfig = { ...(cfg ?? {}), scm_hosts: hosts };
    if (await saveConfig(next, "Failed to add the SCM host")) setHost("");
    setSaving(false);
  };

  const removeHost = async (h: string) => {
    const next: SiteConfig = { ...(cfg ?? {}), scm_hosts: (cfg?.scm_hosts ?? []).filter((x) => x !== h) };
    await saveConfig(next, "Failed to remove the SCM host");
  };

  return (
    <div className="space-y-5">
      <p className="text-[13.5px] leading-relaxed text-muted-foreground">
        GitHub (App recommended — or a PAT / SSH over 443) and Azure DevOps (PAT or SSH) both clone
        through the credential broker; agents never see the raw token.
      </p>

      {check && (
        <ul>
          <CheckRow check={check} />
        </ul>
      )}

      <div className="space-y-2.5 rounded-xl border border-border p-4">
        <p className="text-sm font-medium text-foreground">Add a per-host credential</p>
        <p className="text-xs leading-snug text-muted-foreground">
          Store the PAT as a secret named <span className="font-mono">git-pat-&lt;host-slug&gt;</span> (e.g.{" "}
          <span className="font-mono">git-pat-github-com</span>,{" "}
          <span className="font-mono">git-pat-dev-azure-com</span>), then reference it from a git_pat grant.
        </p>
        <Button variant="outline" size="sm" onClick={() => onAddSecret("git-pat-github-com")}>
          <KeyRound className="size-3.5" /> Add PAT secret
        </Button>
      </div>

      <Field
        label="Self-hosted GHES / ADO Server host"
        htmlFor="scm-host"
        hint="Adds a custom SCM host (e.g. ghes.corp.internal) so runs can clone from it."
      >
        <div className="flex gap-2">
          <Input
            id="scm-host"
            value={host}
            onChange={(e) => setHost(e.target.value)}
            placeholder="ghes.corp.internal"
          />
          <Button variant="outline" onClick={addHost} disabled={saving || cfg === null || !host.trim()}>
            {saving ? <Loader2 className="size-4 animate-spin" /> : "Add"}
          </Button>
        </div>
      </Field>

      <DomainPillList domains={cfg?.scm_hosts ?? []} onRemove={removeHost} />

      <RecheckButton onRecheck={onRecheck} rechecking={rechecking} />
    </div>
  );
}

function ArtifactRepoStep({
  status,
  onRecheck,
  rechecking,
}: {
  status: SetupStatus;
  onRecheck: () => void;
  rechecking: boolean;
}) {
  const check = status.checks.find((c) => c.id === "artifact_repo");
  const [eco, setEco] = React.useState<string>(ARTIFACT_ECOSYSTEMS[0]);
  const [baseUrl, setBaseUrl] = React.useState("");
  const [tokenRef, setTokenRef] = React.useState("");
  const [saving, setSaving] = React.useState(false);
  const { cfg, save: saveConfig } = useSiteConfig();

  const overrides = cfg?.artifact_overrides ?? {};

  const save = async () => {
    const url = baseUrl.trim();
    if (!url) return;
    setSaving(true);
    const nextOverrides = { ...overrides, [eco]: { base_url: url, token_secret_ref: tokenRef.trim() || undefined } };
    const next: SiteConfig = { ...(cfg ?? {}), artifact_overrides: nextOverrides };
    if (await saveConfig(next, "Failed to save the artifact override")) {
      setBaseUrl("");
      setTokenRef("");
    }
    setSaving(false);
  };

  const remove = async (name: string) => {
    const nextOverrides = { ...overrides };
    delete nextOverrides[name];
    const next: SiteConfig = { ...(cfg ?? {}), artifact_overrides: nextOverrides };
    await saveConfig(next, "Failed to remove the artifact override");
  };

  return (
    <div className="space-y-5">
      <p className="text-[13.5px] leading-relaxed text-muted-foreground">
        Redirect npm/pip/cargo/maven/go/nuget to a corporate Artifactory/Nexus mirror so runs never
        reach the public registries.
      </p>

      {check && (
        <ul>
          <CheckRow check={check} />
        </ul>
      )}

      {Object.keys(overrides).length > 0 && (
        <ul className="space-y-1.5">
          {Object.entries(overrides).map(([name, ov]) => (
            <li
              key={name}
              className="flex items-center justify-between gap-2 rounded-md border border-border px-2.5 py-1.5 text-xs"
            >
              <span className="min-w-0 truncate font-mono">
                {name} → {ov.base_url}
                {ov.token_secret_ref && (
                  <span className="text-muted-foreground"> · token: {ov.token_secret_ref}</span>
                )}
              </span>
              <button
                type="button"
                onClick={() => remove(name)}
                aria-label={`Remove ${name} override`}
                className="shrink-0 text-muted-foreground transition-colors hover:text-foreground"
              >
                <X className="size-3.5" />
              </button>
            </li>
          ))}
        </ul>
      )}

      <div className="space-y-3 rounded-xl border border-border p-4">
        <div className="grid grid-cols-[140px_1fr] gap-2.5">
          <Field label="Ecosystem" htmlFor="artifact-eco">
            <Select value={eco} onValueChange={setEco}>
              <SelectTrigger id="artifact-eco">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                {ARTIFACT_ECOSYSTEMS.map((e) => (
                  <SelectItem key={e} value={e}>
                    {e}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </Field>
          <Field label="Base URL" htmlFor="artifact-url">
            <Input
              id="artifact-url"
              value={baseUrl}
              onChange={(e) => setBaseUrl(e.target.value)}
              placeholder="https://artifactory.corp.internal/api/npm/npm-remote"
              className="font-mono"
            />
          </Field>
        </div>
        <Field
          label="Token secret name (optional)"
          htmlFor="artifact-token"
          hint="Injected proxy-side at fetch time — the sandbox never holds it."
        >
          <Input
            id="artifact-token"
            value={tokenRef}
            onChange={(e) => setTokenRef(e.target.value)}
            placeholder="artifactory-token"
            className="font-mono"
          />
        </Field>
        <Button variant="outline" size="sm" onClick={save} disabled={saving || cfg === null || !baseUrl.trim()}>
          {saving ? <Loader2 className="size-3.5 animate-spin" /> : <Plus className="size-3.5" />} Add/Update
        </Button>
      </div>

      <RecheckButton onRecheck={onRecheck} rechecking={rechecking} />
    </div>
  );
}

// ------------------------------------------------------------
// Workspaces step — onboard the local dirs/repos a run may attach. Recommended,
// not required: the composer's ephemeral path still launches with none. Reuses
// AddWorkspaceDialog + the /workspaces status vocabulary so the two can't drift.
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

function WorkspacesStep({
  workspaces,
  loading,
  onReload,
}: {
  workspaces: Workspace[];
  loading: boolean;
  onReload: () => void;
}) {
  // "Add workspace" now routes new imports through the guided Import panel (Source
  // → Scan → Configure → Verify → Finalize). importWsId set => resume that
  // workspace's import; undefined => start fresh on the Source step.
  const [importOpen, setImportOpen] = React.useState(false);
  const [importWsId, setImportWsId] = React.useState<string | undefined>(undefined);
  const openImport = (id?: string) => {
    setImportWsId(id);
    setImportOpen(true);
  };
  const [scanning, setScanning] = React.useState<Set<string>>(new Set());

  // Best-effort scan → always refresh (repo scans run async and return 202, local
  // dirs resolve inline) so the row reflects the latest status either way.
  const scan = async (w: Workspace) => {
    setScanning((s) => new Set(s).add(w.id));
    try {
      const { async: isAsync } = await api.scanWorkspace(w.id);
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
      <p className="text-[13.5px] leading-relaxed text-muted-foreground">
        A run attaches an onboarded directory or repo — never a raw host path. Add at least one so your
        first run has somewhere to work. A task that needs no repo can still run in an ephemeral scratch
        directory. Importing walks you through scan → configure → an optional Record step that learns
        what each task really uses → verify.
      </p>

      {loading ? (
        <p className="text-sm text-muted-foreground">Loading workspaces…</p>
      ) : workspaces.length === 0 ? (
        <div className="rounded-xl border border-dashed border-border p-6 text-center">
          <p className="text-sm text-muted-foreground">No workspaces onboarded yet.</p>
          <Button className="mt-3" onClick={() => openImport()}>
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
                    <span className="block truncate text-[11px] text-muted-foreground">{summary}</span>
                  )}
                </span>
                {scanning.has(w.id) ? (
                  <Chip tone="neutral" dot>
                    <Loader2 className="size-3 animate-spin" /> Scanning…
                  </Chip>
                ) : (
                  <Chip tone={STATUS_TONE[w.status]} dot>
                    {STATUS_LABEL[w.status]}
                  </Chip>
                )}
                {w.status === "ready" ? (
                  <Button variant="ghost" size="sm" onClick={() => scan(w)} disabled={scanning.has(w.id)}>
                    <ScanSearch className="size-3.5" /> Scan
                  </Button>
                ) : (
                  <Button variant="ghost" size="sm" onClick={() => openImport(w.id)}>
                    <ScanSearch className="size-3.5" /> Resume import
                  </Button>
                )}
              </li>
              );
            })}
          </ul>
          <Button variant="outline" size="sm" onClick={() => openImport()}>
            <Plus className="size-4" /> Add workspace
          </Button>
        </>
      )}

      {/* The guided import overlay — its own Dialog on top; returns here via
          onReload + onOpenChange(false) (like NewRunDialog returns to SetupScreen). */}
      <ImportWorkspaceDialog
        open={importOpen}
        onOpenChange={setImportOpen}
        workspaceId={importWsId}
        onReload={onReload}
      />
    </div>
  );
}

// ------------------------------------------------------------
// Credentials step — Optional (B8): excluded from readiness, never blocks launch.
// ------------------------------------------------------------
function CredentialsStep({
  status,
  appId,
  onAppIdChange,
  onSaveAppId,
  savingAppId,
  patHost,
  onPatHostChange,
  onAddSecret,
  onVerify,
  verifying,
}: {
  status: SetupStatus;
  appId: string;
  onAppIdChange: (v: string) => void;
  onSaveAppId: () => void;
  savingAppId: boolean;
  patHost: string;
  onPatHostChange: (v: string) => void;
  onAddSecret: (name: string) => void;
  onVerify: () => void;
  verifying: boolean;
}) {
  return (
    <div className="space-y-5">
      <p className="text-[13.5px] leading-relaxed text-muted-foreground">
        Only needed if a run touches a private repo or a cloud account. Skipping this never blocks a launch —
        and it doesn&apos;t count against readiness.
      </p>

      <div className="space-y-3 rounded-xl border border-border p-4">
        <div className="flex items-center gap-2">
          <Github className="size-4 text-foreground" />
          <h3 className="text-sm font-semibold text-foreground">GitHub App</h3>
          <span className="ml-auto">
            <StatusChip status={status.secrets.github_app ? "ready" : "needs-setup"} />
          </span>
        </div>
        <p className="text-xs leading-snug text-muted-foreground">
          The broker mints short-lived, scoped tokens from this — agents never see the real key.
        </p>
        <Field label="App ID" htmlFor="setup-github-app-id">
          <div className="flex gap-2">
            <Input
              id="setup-github-app-id"
              value={appId}
              onChange={(e) => onAppIdChange(e.target.value)}
              placeholder="123456"
              className="font-mono"
            />
            <Button variant="outline" onClick={onSaveAppId} disabled={savingAppId || !appId.trim()}>
              {savingAppId ? <Loader2 className="size-4 animate-spin" /> : "Save"}
            </Button>
          </div>
        </Field>
        <div className="flex flex-wrap items-center gap-2">
          <Button variant="outline" size="sm" onClick={() => onAddSecret("github-app-key")}>
            <KeyRound className="size-3.5" /> Add private key (PEM)
          </Button>
          <Button variant="ghost" size="sm" onClick={onVerify} disabled={verifying}>
            {verifying ? <Loader2 className="size-3.5 animate-spin" /> : <RotateCw className="size-3.5" />}
            Verify
          </Button>
        </div>
      </div>

      <div className="space-y-3 rounded-xl border border-border p-4">
        <div className="flex items-center gap-2">
          <Cloud className="size-4 text-muted-foreground" />
          <h3 className="text-sm font-semibold text-foreground">Personal access token</h3>
        </div>
        <p className="text-xs leading-snug text-muted-foreground">
          For a non-GitHub host (Azure DevOps, GitLab). The PAT is stored here as a named secret; it&apos;s
          bound to a specific host per run in the New Run wizard&apos;s git-credential card — the host below
          is informational only.
        </p>
        <Field label="Host (set per run in the wizard)" htmlFor="setup-pat-host">
          <Input
            id="setup-pat-host"
            value={patHost}
            onChange={(e) => onPatHostChange(e.target.value)}
            placeholder="dev.azure.com"
          />
        </Field>
        <Button variant="outline" size="sm" onClick={() => onAddSecret("ado-pat")}>
          <KeyRound className="size-3.5" /> Add PAT
        </Button>
      </div>
    </div>
  );
}

// ------------------------------------------------------------
// Launch step — example config marked "Example — not live config" (B12).
// ------------------------------------------------------------
function LaunchStep({ status, onLaunch, onOpenRuns }: { status: SetupStatus; onLaunch: () => void; onOpenRuns: () => void }) {
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
      <p className="text-[13.5px] leading-relaxed text-muted-foreground">
        Describe a task in plain language — the composer proposes a safe config you review before anything
        starts. Working on a specific repo? Onboard it in the Workspaces step first, or the run has no
        access to it.
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
        <dl className="grid grid-cols-[auto_1fr] gap-x-3.5 gap-y-2 text-[13px]">
          {example.map(([k, v]) => (
            <React.Fragment key={k}>
              <dt className="text-muted-foreground">{k}</dt>
              <dd className="text-foreground">{v}</dd>
            </React.Fragment>
          ))}
        </dl>
        <div className="mt-4 flex flex-wrap items-center gap-2">
          <Button onClick={onLaunch}>
            <Rocket className="size-4" /> Launch your first run
          </Button>
          <Button variant="outline" onClick={onOpenRuns}>
            Open Runs
          </Button>
        </div>
      </div>

      <p className="text-xs text-muted-foreground">
        If you finish without launching, Runs greets you with this same shortcut until your first run exists.
      </p>
    </div>
  );
}

// ------------------------------------------------------------
// SetupScreen
// ------------------------------------------------------------
export function SetupScreen({ onDone }: { onDone: () => void }) {
  const [stepId, setStepId] = React.useState<SetupStepId>("environment");
  const [status, setStatus] = React.useState<SetupStatus | null>(null);
  const [rechecking, setRechecking] = React.useState(false);
  const [recheckedOnce, setRecheckedOnce] = React.useState(false);
  const [lastCheckedAt, setLastCheckedAt] = React.useState<Date | null>(null);
  const [introHidden, setIntroHidden] = React.useState(false);
  const [fastPathHidden, setFastPathHidden] = React.useState(false);
  const [secretNames, setSecretNames] = React.useState<string[]>([]);
  const [workspaces, setWorkspaces] = React.useState<Workspace[]>([]);
  const [wsLoading, setWsLoading] = React.useState(true);
  const [addSecretOpen, setAddSecretOpen] = React.useState(false);
  const [addSecretName, setAddSecretName] = React.useState("");
  const [newRunOpen, setNewRunOpen] = React.useState(false);
  const [appId, setAppId] = React.useState("");
  const [savingAppId, setSavingAppId] = React.useState(false);
  const [patHost, setPatHost] = React.useState("");
  const [guide, setGuide] = React.useState<SetupGuide | null>(null);
  // Default-barrier pick (E3). Null until an explicit click — until then the
  // effective selection is the resolved default (persisted pick if this host runs
  // it, else strongest available). Clicking a ready card both selects and persists.
  const [ccOverride, setCcOverride] = React.useState<ConfinementClass | null>(null);
  const selectDefault = React.useCallback((cc: ConfinementClass) => {
    setCcOverride(cc);
    setDefaultCc(cc);
  }, []);

  const recheck = React.useCallback((manual = false) => {
    setRechecking(true);
    return api
      .getSetupStatus()
      .then((s) => {
        setStatus(s);
        setLastCheckedAt(new Date());
        if (manual) setRecheckedOnce(true);
      })
      .finally(() => setRechecking(false));
  }, []);

  const loadSecrets = React.useCallback(() => {
    api.listSecrets().then(setSecretNames).catch(() => setSecretNames([]));
  }, []);

  const loadWorkspaces = React.useCallback(() => {
    setWsLoading(true);
    api
      .listWorkspaces()
      .then(setWorkspaces)
      .catch(() => setWorkspaces([]))
      .finally(() => setWsLoading(false));
  }, []);

  React.useEffect(() => {
    recheck();
    loadSecrets();
    loadWorkspaces();
    // run once on mount — the loaders are stable (useCallback([]))
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const stepIdx = SETUP_STEPS.findIndex((s) => s.id === stepId);
  const isLast = stepIdx === SETUP_STEPS.length - 1;

  const finish = React.useCallback(() => {
    dismissSetup();
    onDone();
  }, [onDone]);

  const openAddSecret = (name: string) => {
    setAddSecretName(name);
    setAddSecretOpen(true);
  };

  const saveAppId = async () => {
    const id = appId.trim();
    if (!id) return;
    setSavingAppId(true);
    try {
      await api.setSecret("github-app-id", id);
      await recheck();
    } catch (e) {
      // Without this catch a failed save is an unhandled rejection: the spinner
      // stops with zero feedback and the operator believes the App ID saved.
      toast.error("Failed to save App ID", {
        description: getErrorMessage(e),
      });
    } finally {
      setSavingAppId(false);
    }
  };

  // M21: badges for the corporate-baseline steps read the actual SiteConfig
  // (see siteConfigBadge) rather than the backend's always-"info" checks.
  const { cfg: siteConfig } = useSiteConfig();

  const readiness = status ? deriveReadiness(status) : null;
  const badges = status && readiness ? stepBadges(status, readiness, workspaces, siteConfig) : null;
  const done = status && readiness ? stepDone(status, readiness, workspaces) : null;
  const strongest = status ? strongestAvailable(status.runner.confinement_classes) : undefined;
  // Effective default-barrier selection: the explicit click if any, else the
  // persisted pick — BOTH re-resolved against live availability, so a class that
  // vanishes on a recheck (e.g. Docker stops mid-session) degrades to the
  // strongest available card instead of leaving zero cards selected.
  const selectedCc = resolveDefaultCc(
    ccOverride ?? getDefaultCc(),
    status?.runner.confinement_classes ?? [],
  );

  // Persist the resolved default the first time we can (no explicit pick yet), so the
  // recommended/strongest-available tier the picker SHOWS is the one actually saved.
  // Otherwise consumers that read the stored default (e.g. the import SecurityChip)
  // fall back to CC1/Fence and disagree with what the barrier step displays — you pick
  // Vault, but the import shows Fence. Clicking a card still overrides + re-persists.
  React.useEffect(() => {
    if (status && !getDefaultCc()) setDefaultCc(selectedCc);
  }, [status, selectedCc]);

  return (
    <div className="mx-auto max-w-[860px] px-7 pb-14 pt-6">
      {/* Header */}
      <div className="mb-4 flex items-start justify-between gap-4">
        <div>
          <h1 className="text-[1.5rem] font-semibold leading-tight tracking-tight text-foreground">
            Getting started
          </h1>
          <p className="mt-1 text-sm text-muted-foreground">Let agents work while you keep your keys.</p>
        </div>
        {introHidden && (
          <button
            onClick={() => setIntroHidden(false)}
            className="shrink-0 text-[12.5px] font-medium text-muted-foreground transition-colors hover:text-foreground"
          >
            Show intro
          </button>
        )}
      </div>

      {!introHidden && <IntroPanel onHide={() => setIntroHidden(true)} />}

      {!status || !readiness || !badges || !done ? (
        <p className="text-sm text-muted-foreground">Checking Wardyn&apos;s setup…</p>
      ) : (
        <>
          {/* Ready-host fast path (B3). Gated on llmReady too: a barrier without a
              connected model isn't actually ready to run, and this keeps the
              "{llmLabel || 'a model is connected'}" copy honest — it only renders
              when a model really is connected, never as a fabricated fallback. */}
          {readiness.ready && readiness.llmReady && !fastPathHidden && (
            <div className="mb-4 flex flex-wrap items-center gap-3 rounded-[13px] border border-primary/35 bg-primary-subtle/60 px-4 py-3.5">
              <Rocket className="size-5 shrink-0 text-primary" />
              <div className="min-w-[240px] flex-1">
                <div className="text-sm font-semibold text-foreground">
                  You&apos;re ready — launch your first run now.
                </div>
                <p className="mt-0.5 text-[12.5px] leading-snug text-muted-foreground">
                  {strongest ? CC_META[strongest].label : "A barrier"} is up and{" "}
                  {readiness.llmLabel || "a model is connected"}. That&apos;s enough for a first run — you can
                  harden anytime.
                </p>
              </div>
              <div className="flex shrink-0 gap-2">
                <Button size="sm" onClick={() => setStepId("launch")}>
                  Launch your first run
                </Button>
                <Button size="sm" variant="outline" onClick={() => setFastPathHidden(true)}>
                  Keep setting up
                </Button>
              </div>
            </div>
          )}

          {/* One stepper (B2) with honest badges (B4) */}
          <div className="mb-5">
            <FunnelStepper current={stepId} onJump={setStepId} done={done} badges={badges} />
          </div>

          {/* Step body */}
          <h2 className="mb-1.5 text-[17px] font-semibold tracking-tight text-foreground">
            {stepId === "credentials" ? (
              <span className="inline-flex items-center gap-2.5">
                {STEP_HEADING[stepId]}
                <Chip tone="neutral" className="uppercase tracking-wide">
                  Optional
                </Chip>
              </span>
            ) : (
              STEP_HEADING[stepId]
            )}
          </h2>

          {stepId === "environment" && (
            <EnvironmentStep
              status={status}
              onRecheck={() => recheck(true)}
              rechecking={rechecking}
              recheckedOnce={recheckedOnce}
              lastCheckedAt={lastCheckedAt}
              selected={selectedCc}
              onSelect={selectDefault}
            />
          )}
          {stepId === "provider" && (
            <ProviderStep
              status={status}
              readiness={readiness}
              onAddSecret={openAddSecret}
              onSetup={setGuide}
              onRecheck={() => recheck(true)}
              rechecking={rechecking}
            />
          )}
          {stepId === "host_proxy" && (
            <HostProxyStep status={status} onRecheck={() => recheck(true)} rechecking={rechecking} />
          )}
          {stepId === "scm_provider" && (
            <ScmProviderStep
              status={status}
              onAddSecret={openAddSecret}
              onRecheck={() => recheck(true)}
              rechecking={rechecking}
            />
          )}
          {stepId === "artifact_repo" && (
            <ArtifactRepoStep status={status} onRecheck={() => recheck(true)} rechecking={rechecking} />
          )}
          {stepId === "workspaces" && (
            <WorkspacesStep workspaces={workspaces} loading={wsLoading} onReload={loadWorkspaces} />
          )}
          {stepId === "credentials" && (
            <CredentialsStep
              status={status}
              appId={appId}
              onAppIdChange={setAppId}
              onSaveAppId={saveAppId}
              savingAppId={savingAppId}
              patHost={patHost}
              onPatHostChange={setPatHost}
              onAddSecret={openAddSecret}
              onVerify={() => recheck(true)}
              verifying={rechecking}
            />
          )}
          {stepId === "review" && (
            <ReviewStep
              status={status}
              readiness={readiness}
              onRecheck={() => recheck(true)}
              rechecking={rechecking}
              lastCheckedAt={lastCheckedAt}
              onJump={setStepId}
            />
          )}
          {stepId === "launch" && (
            <LaunchStep status={status} onLaunch={() => setNewRunOpen(true)} onOpenRuns={onDone} />
          )}
        </>
      )}

      {/* Footer — one exit verb (B13) */}
      <div className="mt-7 flex items-center justify-between border-t border-border pt-4">
        <div>
          <button
            onClick={finish}
            className="text-[13.5px] font-medium text-muted-foreground transition-colors hover:text-foreground"
          >
            {EXIT_VERB}
          </button>
          <span className="mt-0.5 block text-[11px] text-muted-foreground/70">{BTN.finishLaterHint}</span>
        </div>
        <div className="flex items-center gap-2">
          <Button
            variant="outline"
            onClick={() => setStepId(SETUP_STEPS[Math.max(0, stepIdx - 1)].id)}
            disabled={stepIdx === 0}
          >
            <ArrowLeft className="size-4" /> Back
          </Button>
          {!isLast && (
            <Button onClick={() => setStepId(SETUP_STEPS[Math.min(SETUP_STEPS.length - 1, stepIdx + 1)].id)}>
              Next: {SETUP_STEPS[stepIdx + 1].label} <ArrowRight className="size-4" />
            </Button>
          )}
        </div>
      </div>

      <AddSecretDialog
        open={addSecretOpen}
        onOpenChange={setAddSecretOpen}
        existingNames={secretNames}
        initialName={addSecretName}
        onSaved={() => {
          loadSecrets();
          recheck();
        }}
      />
      <NewRunDialog
        open={newRunOpen}
        onOpenChange={setNewRunOpen}
        onCreated={() => {
          dismissSetup();
          onDone();
        }}
      />
      <SetupGuideDialog
        guide={guide}
        onClose={() => setGuide(null)}
        onRecheck={() => recheck(true)}
        rechecking={rechecking}
      />
    </div>
  );
}
