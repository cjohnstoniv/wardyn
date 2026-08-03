/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Getting-started step bodies: the corporate-baseline steps (Host Proxy /
// SCM Provider / Artifact Redirect), Workspaces, Credentials, Review, and Launch.
// Mostly presentational — the orchestrator (setup-screen) owns SetupStatus AND
// the fetched SiteConfig (the sole owner — see the corporate-baseline steps
// below), while each body owns its OWN writes (setSecret, scanWorkspace, and the
// SiteConfig saves via the orchestrator-owned saveSiteConfig).
import * as React from "react";
import {
  AlertTriangle,
  Info,
  Loader2,
  Plus,
  CircleCheck,
  Rocket,
  RotateCw,
  ScanSearch,
  X,
} from "lucide-react";
import type {
  HostProxyDetection,
  HostProxySetting,
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
import { Input } from "../../ui/input";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "../../ui/select";
import { Field } from "../new-run/step-shell";
import { STATUS_TONE, STATUS_LABEL } from "../workspaces";
import { ImportWorkspaceDialog } from "../import-workspace/import-panel";
import type { Readiness } from "../onboarding/intro";
import { lastCheckedLabel } from "../onboarding/intro";
import { toast } from "sonner";
import type { SetupStepId, StepBadge } from "./steps";

// ------------------------------------------------------------
// Shared check-row primitives (Review + the corporate steps).
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
// Corporate-baseline steps (Host Proxy / SCM Provider / Artifact Redirect) — all
// three are non-blocking (see internal/api/setup.go): host_proxy/artifact_repo
// are "info"-tier; scm_provider is graded ok/warn/info against the safest-path
// ladder but never gates readiness. They just let an operator wire the
// SiteConfig baseline every run inherits.
//
// SiteConfig has ONE owner (V2): the orchestrator (setup-screen) holds the
// fetched doc and hands each step `siteConfig` + `reloadSiteConfig`/
// `saveSiteConfig`. HARD CONSTRAINT: each step below re-GETs (reloadSiteConfig())
// in a mount effect, on step entry, before any save — the PUT is a shallow merge
// on top of the CURRENT doc, so a copy that's gone stale since another step's
// edit would otherwise silently clobber it.
// ------------------------------------------------------------
const ARTIFACT_ECOSYSTEMS = ["npm", "pip", "cargo", "maven", "go", "nuget"] as const;

// The three site-config steps (host proxy / SCM host / artifact repo) share one
// prologue: the V2 clobber guard (re-GET on step entry) and a `saving` flag
// around each write. Written once here so the hard-constraint guard can't drift
// between copies. `mutate` PUTs via the orchestrator-owned saveSiteConfig and
// reports failure as `false` — toasting the server's reason — so each step only
// commits its own local field state once the PUT actually lands.
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

export function RecheckButton({ onRecheck, rechecking }: { onRecheck: () => void; rechecking: boolean }) {
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

export function HostProxyStep({
  status,
  siteConfig,
  reloadSiteConfig,
  saveSiteConfig,
  onAddSecret,
  onRecheck,
  rechecking,
}: {
  status: SetupStatus;
  siteConfig: SiteConfig | null;
  reloadSiteConfig: () => Promise<void>;
  saveSiteConfig: (next: SiteConfig) => Promise<void>;
  onAddSecret: (name: string) => void;
  onRecheck: () => void;
  rechecking: boolean;
}) {
  const check = status.checks.find((c) => c.id === "host_proxy");
  // PUT /site-config — operator-only (see http.go).
  const operator = useOperator();
  const [secretName, setSecretName] = React.useState("");
  const { saving, mutate } = useSiteConfigStep(reloadSiteConfig, saveSiteConfig);

  // Seed the field from the freshest doc exactly once (matches the old
  // useSiteConfig onLoad callback) — never again, so a later reload (e.g. from
  // the header Re-check button) can't stomp an in-progress edit.
  const seededRef = React.useRef(false);
  React.useEffect(() => {
    if (!seededRef.current && siteConfig) {
      setSecretName(siteConfig.upstream_proxy_secret_ref ?? "");
      seededRef.current = true;
    }
  }, [siteConfig]);

  const save = async () => {
    const name = secretName.trim();
    const next: SiteConfig = { ...(siteConfig ?? {}), upstream_proxy_secret_ref: name || undefined };
    if (await mutate(next, "Failed to save the upstream proxy secret")) {
      toast.success("Upstream proxy secret saved");
    }
  };

  return (
    <div className="space-y-5">
      {/* Stays true whether or not anything was detected — the check row below
          says what was actually found (and, in a container, what couldn't be
          looked at). Do not reintroduce a "Wardyn detected these settings" lede:
          it rendered unconditionally, above an empty result. */}
      <p className="text-sm leading-relaxed text-muted-foreground">
        The sandbox reaches the internet only through wardyn-proxy, which can chain through your
        corporate proxy.
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
        hint={
          operator
            ? "Store the proxy URL as a secret (Add secret), then reference its name here."
            : `Store the proxy URL as a secret (Add secret), then reference its name here. ${OPERATOR_ONLY_REASON}`
        }
      >
        <div className="flex gap-2">
          <Input
            id="host-proxy-secret"
            value={secretName}
            onChange={(e) => setSecretName(e.target.value)}
            placeholder="upstream-proxy-url"
            className="font-mono"
          />
          <Button
            variant="outline"
            onClick={() => onAddSecret(secretName.trim() || "upstream-proxy-url")}
          >
            Add secret
          </Button>
          <Button variant="outline" onClick={save} disabled={!operator || saving || siteConfig === null}>
            {saving ? <Loader2 className="size-4 animate-spin" /> : "Save"}
          </Button>
        </div>
      </Field>

      <RecheckButton onRecheck={onRecheck} rechecking={rechecking} />
    </div>
  );
}

export function ArtifactRepoStep({
  status,
  siteConfig,
  reloadSiteConfig,
  saveSiteConfig,
  onRecheck,
  rechecking,
}: {
  status: SetupStatus;
  siteConfig: SiteConfig | null;
  reloadSiteConfig: () => Promise<void>;
  saveSiteConfig: (next: SiteConfig) => Promise<void>;
  onRecheck: () => void;
  rechecking: boolean;
}) {
  const check = status.checks.find((c) => c.id === "artifact_repo");
  // PUT /site-config — operator-only (see http.go).
  const operator = useOperator();
  const [eco, setEco] = React.useState<string>(ARTIFACT_ECOSYSTEMS[0]);
  const [baseUrl, setBaseUrl] = React.useState("");
  const [tokenRef, setTokenRef] = React.useState("");
  const { saving, mutate } = useSiteConfigStep(reloadSiteConfig, saveSiteConfig);

  const overrides = siteConfig?.artifact_overrides ?? {};

  const save = async () => {
    const url = baseUrl.trim();
    if (!url) return;
    const nextOverrides = { ...overrides, [eco]: { base_url: url, token_secret_ref: tokenRef.trim() || undefined } };
    const next: SiteConfig = { ...(siteConfig ?? {}), artifact_overrides: nextOverrides };
    if (await mutate(next, "Failed to save the artifact override")) {
      setBaseUrl("");
      setTokenRef("");
    }
  };

  const remove = async (name: string) => {
    const nextOverrides = { ...overrides };
    delete nextOverrides[name];
    const next: SiteConfig = { ...(siteConfig ?? {}), artifact_overrides: nextOverrides };
    await mutate(next, "Failed to remove the artifact override");
  };

  return (
    <div className="space-y-5">
      <p className="text-sm leading-relaxed text-muted-foreground">
        Redirect npm/pip/cargo/maven/go/nuget to a corporate Artifactory/Nexus mirror so runs never
        reach the public registries.
        {!operator && ` ${OPERATOR_ONLY_REASON}`}
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
                disabled={!operator}
                aria-label={`Remove ${name} override`}
                className="shrink-0 text-muted-foreground transition-colors hover:text-foreground disabled:pointer-events-none disabled:opacity-50"
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
        <Button
          variant="outline"
          size="sm"
          onClick={save}
          disabled={!operator || saving || siteConfig === null || !baseUrl.trim()}
        >
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

export function WorkspacesStep({
  workspaces,
  loading,
  onReload,
}: {
  workspaces: Workspace[];
  loading: boolean;
  onReload: () => void;
}) {
  // Onboard/scan/import are all workspace writes — operator-only (see http.go).
  const operator = useOperator();
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
        A run attaches an onboarded directory or repo — never a raw host path. Add at least one so your
        first run has somewhere to work. A task that needs no repo can still run in an ephemeral scratch
        directory. Importing walks you through scan → configure → an optional Record step that learns
        what each task really uses → verify.
        {!operator && ` ${OPERATOR_ONLY_REASON}`}
      </p>

      {loading ? (
        <p className="text-sm text-muted-foreground">Loading workspaces…</p>
      ) : workspaces.length === 0 ? (
        <div className="rounded-xl border border-dashed border-border p-6 text-center">
          <p className="text-sm text-muted-foreground">No workspaces onboarded yet.</p>
          <Button className="mt-3" onClick={() => openImport()} disabled={!operator}>
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
                  {STATUS_TONE[w.status] === "danger" && (
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
                  <Chip tone={STATUS_TONE[w.status]} dot>
                    {STATUS_LABEL[w.status]}
                  </Chip>
                )}
                {w.status === "ready" ? (
                  <Button
                    variant="ghost"
                    size="sm"
                    onClick={() => scan(w)}
                    disabled={!operator || scanning.has(w.id)}
                  >
                    <ScanSearch className="size-3.5" /> Scan
                  </Button>
                ) : (
                  <Button variant="ghost" size="sm" onClick={() => openImport(w.id)} disabled={!operator}>
                    <ScanSearch className="size-3.5" /> Resume import
                  </Button>
                )}
              </li>
              );
            })}
          </ul>
          <Button variant="outline" size="sm" onClick={() => openImport()} disabled={!operator}>
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
// Credentials step — Optional (B8): excluded from readiness, never blocks
// launch. A lead line and a jump to SCM Provider, nothing else.
//
// ponytail: the PAT quick-add that used to sit here is DELETED, not fixed. It
// was a second door to the same AddSecretDialog sitting directly under the
// button that opens the proper one, and it got the shared facts wrong in two
// ways the SCM Provider door doesn't: it passed the raw typed string as the
// host (so "https://GHES.corp.internal/" showed that as the host while
// deriving the name git-pat-https-ghes-corp-internal), and it never wrote
// scm_hosts, so the hostname was not registered for egress. Fixing it meant
// porting panel 1's validator into a duplicate flow; deleting it is the
// shorter diff and leaves one place where a credential is added.
// ------------------------------------------------------------
export function CredentialsStep({ onJump }: { onJump: (id: SetupStepId) => void }) {
  return (
    <div className="space-y-5">
      <p className="text-sm leading-relaxed text-muted-foreground">
        Only needed if a run touches a private repo or a cloud account. Skipping this never blocks a launch —
        and it doesn&apos;t count against readiness.
      </p>

      <p className="text-sm leading-relaxed text-muted-foreground">
        Storing a credential doesn&apos;t attach it to anything: each run names the host and the
        stored secret in its own git_pat / ssh_key grant, in the New Run wizard&apos;s
        git-credential card.
      </p>

      <div className="flex flex-wrap items-center gap-3">
        <p className="text-sm text-muted-foreground">Per-provider setup lives in SCM Provider.</p>
        <Button variant="outline" size="sm" onClick={() => onJump("scm_provider")}>
          Go to SCM Provider
        </Button>
      </div>
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
