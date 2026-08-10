/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Step 2 — Access: the resolved model-access card (resolution replaced the old
// per-run Anthropic-auth cards — see below), a GitHub token grant (repos +
// permission + approval + TTL), and the git PAT grant for a non-GitHub host.
//
// Model access RESOLVES from integrations (run override -> workspace pin ->
// server default -> honest none); it is no longer configured on this step. The
// three Anthropic-auth radio cards and the absolute ~/.claude host-path input
// are gone. GitHub token and Git PAT stay switch-gated blocks (already
// progressive: off = collapsed) with the same card treatment as before.
import * as React from "react";
import { Check, ChevronsUpDown, GitBranch, Plus, TriangleAlert } from "lucide-react";
import { Input } from "../../ui/input";
import { Switch } from "../../ui/switch";
import { Button } from "../../ui/button";
import { Label } from "../../ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "../../ui/select";
import { Popover, PopoverContent, PopoverTrigger } from "../../ui/popover";
import {
  Command,
  CommandEmpty,
  CommandGroup,
  CommandInput,
  CommandItem,
  CommandList,
} from "../../ui/command";
import {
  Sheet,
  SheetContent,
  SheetDescription,
  SheetFooter,
  SheetHeader,
  SheetTitle,
} from "../../ui/sheet";
import { cn } from "../../ui/utils";
import { Chip } from "../../wardyn/primitives";
import { Mono } from "../../wardyn/code-block";
import { Field } from "./step-shell";
import type { GitHubPermission, WizardState } from "./wizard-types";
import { useWorkspaceList } from "../../../lib/use-workspace-list";
import {
  integrationsApi,
  type IntegrationRow,
} from "../../../lib/api/integrations";
import { IMPOSSIBLE, RESIDENCY_META, type AiCapability } from "../../../lib/integrations";
import { RD } from "../../../lib/workspace-copy";
import { setup as setupApi } from "../../../lib/api/setup";
import type { WireIntegration, Workspace } from "../../../lib/types";

export function StepAccess({
  state,
  patch,
  secrets,
  secretsLoading,
  onAddSecret,
}: {
  state: WizardState;
  patch: (p: Partial<WizardState>) => void;
  secrets: string[];
  secretsLoading: boolean;
  onAddSecret: () => void;
}) {
  const isGovernedCommand = state.runType === "command";

  return (
    <div className="space-y-5">
      {/* --- Model access: resolved from integrations, never configured here.
          Absent entirely for a governed command (task_mode: exec) — there is
          no agent, so nothing resolves and nothing should suggest otherwise. --- */}
      {isGovernedCommand ? (
        <div className="rounded-lg border border-border p-3">
          <p className="text-[0.8125rem] leading-snug text-muted-foreground">{RD.EXEC_LINE}</p>
        </div>
      ) : (
        <ModelAccessCard
          agent={state.agent}
          integrationId={state.integrationId}
          primaryWorkspaceId={state.workspaces[0]?.workspaceId}
          onPatch={patch}
        />
      )}

      {/* --- GitHub token grant --- */}
      <div className="rounded-lg border border-border p-3">
        <div className="flex items-center justify-between gap-3">
          <div className="flex items-start gap-2">
            <GitBranch className="mt-0.5 size-4 shrink-0 text-primary" />
            <div>
              <Label htmlFor="gh-enable" className="text-sm font-semibold text-foreground">
                GitHub token
              </Label>
              <p className="mt-0.5 text-[0.6875rem] text-muted-foreground">
                Mint a short-lived, repo-scoped installation token. The broker clamps
                permissions to its ceiling regardless of what's requested.
              </p>
            </div>
          </div>
          <Switch
            id="gh-enable"
            checked={state.githubEnabled}
            onCheckedChange={(c) => patch({ githubEnabled: c })}
          />
        </div>

        {state.githubEnabled && (
          <div className="mt-3 space-y-4 border-t border-border pt-3">
            <Field label="Repositories" htmlFor="gh-repos" hint="One or more org/repo, comma or space separated.">
              <Input
                id="gh-repos"
                placeholder="acme/payments-service, acme/shared-libs"
                value={state.githubRepos}
                onChange={(e) => patch({ githubRepos: e.target.value })}
                className="font-mono"
              />
            </Field>

            <Field label="Permission">
              <Select
                value={state.githubPermission}
                onValueChange={(v) => patch({ githubPermission: v as GitHubPermission })}
              >
                <SelectTrigger>
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="read">Read — contents:read</SelectItem>
                  <SelectItem value="read+write">
                    Read + write — contents:write, pull_requests:write
                  </SelectItem>
                </SelectContent>
              </Select>
            </Field>

            <div className="flex items-center justify-between">
              <Label htmlFor="gh-approval" className="text-sm">
                Requires approval before minting
              </Label>
              <Switch
                id="gh-approval"
                checked={state.githubRequiresApproval}
                onCheckedChange={(c) => patch({ githubRequiresApproval: c })}
              />
            </div>

            <Field label="Token TTL (minutes)" htmlFor="gh-ttl">
              <Input
                id="gh-ttl"
                type="number"
                min={1}
                value={state.githubTtlMinutes}
                onChange={(e) => patch({ githubTtlMinutes: Number(e.target.value) })}
                className="w-32 font-mono"
              />
            </Field>
          </div>
        )}
      </div>

      {/* --- git PAT (git_pat grant) for a non-GitHub host --- */}
      <div className="rounded-lg border border-border p-3">
        <div className="flex items-center justify-between gap-3">
          <div className="flex items-start gap-2 min-w-0">
            <GitBranch className="mt-0.5 size-4 shrink-0 text-primary" />
            <div className="min-w-0">
              <Label htmlFor="pat-enable" className="text-sm font-semibold text-foreground">
                Git PAT (non-GitHub host)
              </Label>
              <p className="mt-0.5 text-[0.6875rem] text-muted-foreground">
                Broker a stored Personal Access Token to git for Azure DevOps /
                GitLab. Unlike an LLM key, the PAT value reaches git via the
                credential helper — Wardyn can't expire or down-scope it.
              </p>
            </div>
          </div>
          <Switch
            id="pat-enable"
            checked={state.gitPatEnabled}
            onCheckedChange={(c) => patch({ gitPatEnabled: c })}
          />
        </div>

        {state.gitPatEnabled && (
          <div className="mt-3 space-y-4 border-t border-border pt-3">
            <Field label="Host" htmlFor="pat-host" hint="The git host, e.g. dev.azure.com or gitlab.com. Unioned into allowed egress.">
              <Input
                id="pat-host"
                placeholder="dev.azure.com"
                value={state.gitPatHost}
                onChange={(e) => patch({ gitPatHost: e.target.value })}
                className="font-mono"
              />
            </Field>

            <Field label="Stored PAT secret">
              <div className="flex items-center gap-2">
                <SecretCombobox
                  value={state.gitPatSecretName}
                  onChange={(name) => patch({ gitPatSecretName: name })}
                  secrets={secrets}
                  loading={secretsLoading}
                />
                <Button type="button" variant="ghost" size="sm" onClick={onAddSecret}>
                  <Plus className="size-4" /> Add secret
                </Button>
              </div>
            </Field>

            <Field
              label="Git username (optional)"
              htmlFor="pat-username"
              hint="Defaults by host: Azure DevOps → pat, GitLab → oauth2. Override only if your host needs a different username."
            >
              <Input
                id="pat-username"
                placeholder="(auto)"
                value={state.gitPatUsername}
                onChange={(e) => patch({ gitPatUsername: e.target.value })}
                className="w-48 font-mono"
              />
            </Field>

            <div className="flex items-center justify-between">
              <Label htmlFor="pat-approval" className="text-sm">
                Requires approval before minting
              </Label>
              <Switch
                id="pat-approval"
                checked={state.gitPatRequiresApproval}
                onCheckedChange={(c) => patch({ gitPatRequiresApproval: c })}
              />
            </div>
          </div>
        )}
      </div>
    </div>
  );
}

// Best-effort client-side approximation of the server's resolution order (run
// override -> workspace pin -> server default -> none). The server is
// authoritative at launch/preflight time — this is a PREVIEW so Access can show
// something honest before that. `ai` is the ai_provider integrations list
// (integrationsApi.list().ai); `primaryWorkspace` is state.workspaces[0]
// resolved against the fetched Workspace list, if any.
export interface ResolvedModelAccess {
  row: IntegrationRow;
  because: string;
}

function agentCapability(agent: WizardState["agent"]): AiCapability {
  return agent === "codex-cli" ? "codex_cli" : "claude_code";
}

// A row can drive this agent's tool unless the IMPOSSIBLE map names a reason —
// the verbatim fact this whole redesign renders instead of a toggle.
export function incompatibleReason(row: IntegrationRow, capability: AiCapability): string | undefined {
  return row.aiType ? IMPOSSIBLE[row.aiType]?.[capability] : undefined;
}

export function resolveModelAccess(
  agent: WizardState["agent"],
  integrationId: string | undefined,
  primaryWorkspace: Workspace | undefined,
  ai: IntegrationRow[],
  // The effective wire rows (SetupStatus.integrations), only needed for the
  // tier-3 "server default" claim below — absent (or not yet loaded) just
  // means nothing can be genuinely marked yet, which correctly falls through.
  integrations?: WireIntegration[],
  // SetupStatus.bedrock?.ready (internal/api/setup.go's bedrockReady() gate,
  // echoed on the wire so the UI doesn't re-derive — and drift from — it).
  // Needed because, unlike the managed-subscription row below, the derived
  // "bedrock" row is synthesized on mere TOUCH, not on readiness — see the
  // comment on globalFallback. Absent (not yet loaded) means that half of the
  // carve-out correctly can't fire yet either.
  bedrockReady?: boolean,
): ResolvedModelAccess | null {
  const capability = agentCapability(agent);
  const compatible = (r: IntegrationRow) => !incompatibleReason(r, capability);
  // The server's tier-3 resolves only a row actually marked
  // DefaultFor:agent_runs (internal/api/llmcred.go) — never merely "the first
  // compatible row in list order". Matching that here means the client can
  // never name an integration the launch itself will not use.
  const isServerDefault = (r: IntegrationRow) =>
    !!r.serverId && !!integrations?.find((w) => w.id === r.serverId)?.default_for?.includes("agent_runs");

  if (integrationId) {
    const row = ai.find((r) => r.id === integrationId);
    if (row) return { row, because: "you overrode it for this run." };
  }
  // llm_cred names an Integration by id now — the binding IS the
  // cross-reference (the old aiType best-effort match died with the mode shape).
  const ref = primaryWorkspace?.llm_cred?.integration_ref;
  if (ref) {
    const pinned = ai.find((r) => r.id === ref);
    if (pinned) return { row: pinned, because: "this workspace pins it." };
  }
  const marked = ai.find((r) => compatible(r) && isServerDefault(r));
  if (marked) return { row: marked, because: "it's the server default for agent runs." };
  // Nothing carries the DefaultFor:agent_runs mark — but ok=false there is NOT
  // "no model access" (internal/api/llmcred.go's resolveRunIntegration doc
  // calls it "the global-provider-config fallback"): dispatch still
  // credentials the run off two purely-global carve-outs that need no marked
  // (or even stored) integration row at all — a managed subscription
  // (managedInjectReady) and the global Bedrock config
  // (resolveBedrockAuth(…, nil)) — exactly what preflight.go:161-179 folds in
  // so the checklist "stop[s] telling an operator with working Bedrock access
  // that they have none." Both are visible here ONLY as the two fixed-id
  // derived rows integrations.go synthesizes — but NOT symmetrically: the
  // managed-subscription row is synthesized precisely when managedInjectReady,
  // while the bedrock row is synthesized whenever the operator has touched ANY
  // Bedrock knob (SetupBedrock.configured — region OR model OR any
  // credential), which is strictly weaker than ready (region AND model AND a
  // credential). So the bedrock half additionally requires bedrockReady, the
  // server's own readiness verdict — never an ordinary stored-but-unmarked
  // row, which genuinely still needs the mark.
  //
  // BEDROCK IS TRIED FIRST, in dispatch's order rather than the order
  // deriveAiRows happens to push the rows: managed injection requires
  // `!t.bedrockReady` (runs_dispatch_llm.go), so an install with BOTH runs on
  // Bedrock. Naming the subscription there would not merely mislabel — the
  // residency chip is read off the row named here, and the subscription's
  // proxy_injected reads "success" while Bedrock's resident_env/resident_mount
  // reads "warning", so step-review.tsx would suppress the "credential resident
  // in sandbox" chip for a run that really does carry one.
  const globalFallback =
    ai.find((r) => compatible(r) && r.serverId === "bedrock" && bedrockReady) ??
    ai.find((r) => compatible(r) && r.serverId === "anthropic_subscription:managed");
  if (globalFallback) return { row: globalFallback, because: "the server's global provider config applies." };
  return null;
}

export function ModelAccessCard({
  agent,
  integrationId,
  primaryWorkspaceId,
  onPatch,
}: {
  agent: WizardState["agent"];
  integrationId?: string;
  primaryWorkspaceId: string | undefined;
  // Absent when this card is reused OUTSIDE the wizard (compose-form.tsx) — no
  // per-run override exists there; the footer links to the workspace instead
  // of opening OverridePeek.
  onPatch?: (p: Partial<WizardState>) => void;
}) {
  const [ai, setAi] = React.useState<IntegrationRow[]>([]);
  const [aiLoaded, setAiLoaded] = React.useState(false);
  const [integrations, setIntegrations] = React.useState<WireIntegration[] | undefined>(undefined);
  const [bedrockReady, setBedrockReady] = React.useState<boolean | undefined>(undefined);
  const [statusLoaded, setStatusLoaded] = React.useState(false);
  const [peekOpen, setPeekOpen] = React.useState(false);
  // Self-fetched (not threaded through wizard.tsx as a new prop) so every
  // existing StepAccess call site keeps working unchanged; only the id of the
  // Basics-selected primary comes from the caller (state.workspaces[0]),
  // resolved against this fetch to find its llm_cred binding. useWorkspaceList
  // does NOT fetch on its own (every other caller drives it from its own
  // effect) — reload() here is what actually populates it; its own `loading`
  // flag is this card's THIRD loaded-gate input (below), so it isn't re-tracked.
  const { workspaces, loading: workspacesLoading, reload } = useWorkspaceList();

  React.useEffect(() => {
    reload();
  }, [reload]);

  React.useEffect(() => {
    let alive = true;
    integrationsApi
      .list()
      .then((data) => {
        if (alive) setAi(data.ai);
      })
      .catch(() => {})
      .finally(() => {
        if (alive) setAiLoaded(true);
      });
    return () => {
      alive = false;
    };
  }, []);

  // Which compatible row is genuinely marked DefaultFor:agent_runs — the same
  // fact resolveModelAccess's tier-3 now requires, not merely "first in list
  // order" — plus status.bedrock?.ready, which its bedrock carve-out requires
  // for the same reason. A second fetch (integrationsApi.list() derives its
  // rows from the same three endpoints but discards the raw status), same
  // self-fetch idiom as `ai` above.
  React.useEffect(() => {
    let alive = true;
    setupApi
      .getSetupStatus()
      .then((status) => {
        if (alive) {
          setIntegrations(status.integrations);
          setBedrockReady(status.bedrock?.ready);
        }
      })
      .catch(() => {})
      .finally(() => {
        if (alive) setStatusLoaded(true);
      });
    return () => {
      alive = false;
    };
  }, []);

  const primaryWorkspace = workspaces.find((w) => w.id === primaryWorkspaceId);
  const resolved = resolveModelAccess(agent, integrationId, primaryWorkspace, ai, integrations, bedrockReady);
  // Gates the first paint (M5): before all three self-fetches above settle —
  // success OR failure, `.finally`/useWorkspaceList's own `loading` cover both
  // — `resolved` is meaningless (computed off default/empty state), so it must
  // not render as either the amber "nothing resolves" line or a false positive.
  const loaded = !workspacesLoading && aiLoaded && statusLoaded;
  // Neutral, not amber (M7): the compose-form usage (no onPatch — the real
  // agent isn't known until the proposal resolves it) when integrations DO
  // exist but simply don't match the hardcoded preview agent. The manual
  // wizard (onPatch present) is unaffected — it keeps the amber line for every
  // unresolved case, as it always has.
  const neutralMismatch = loaded && !resolved && !onPatch && ai.length > 0;

  return (
    <div className="rounded-xl border border-border bg-card p-3.5">
      <div className="flex flex-col gap-2.5">
        <span className="text-[0.6875rem] font-medium uppercase tracking-wide text-muted-foreground">
          Model access — resolved from integrations
        </span>
        {!loaded ? (
          <p className="text-[0.75rem] leading-snug text-muted-foreground">{RD.RESOLVING_LINE}</p>
        ) : resolved ? (
          <>
            <div className="flex flex-wrap items-center gap-2.5">
              <span className="text-sm font-medium text-foreground">{resolved.row.name}</span>
              <Mono className="text-[0.6875rem] text-muted-foreground">{resolved.row.typeLabel}</Mono>
              <Chip tone={RESIDENCY_META[resolved.row.residency].tone} className="text-[0.6875rem]">
                {RESIDENCY_META[resolved.row.residency].label}
              </Chip>
            </div>
            <p className="text-[0.6875rem] leading-snug text-muted-foreground">
              Applies because: {resolved.because}
            </p>
          </>
        ) : neutralMismatch ? (
          <p className="text-[0.75rem] leading-snug text-muted-foreground">{RD.AGENT_AT_REVIEW_LINE}</p>
        ) : (
          <div className="flex items-start gap-1.5 rounded-md border border-warning/40 bg-warning-subtle px-2.5 py-1.5">
            <TriangleAlert className="mt-0.5 size-3 shrink-0 text-warning" aria-hidden="true" />
            <p className="text-[0.75rem] leading-snug text-warning">{RD.NONE_LINE}</p>
          </div>
        )}
        <div className="flex justify-end">
          {onPatch ? (
            <Button type="button" size="sm" variant="outline" onClick={() => setPeekOpen(true)}>
              Override for this run…
            </Button>
          ) : (
            // Plain <a>, not react-router's <Link>: same no-Router-ancestor
            // rationale as workspace-picker.tsx's unstored-secret CTAs.
            // target="_blank" (M8): a same-tab navigation here would leave the
            // open dialog — and whatever the operator has already typed into it
            // (a compose prompt, wizard fields) — destroyed with no way back.
            <a
              href={primaryWorkspaceId ? `/workspaces/${primaryWorkspaceId}` : "/workspaces"}
              target="_blank"
              rel="noreferrer"
              className="text-[0.6875rem] font-medium text-primary"
            >
              Change on the workspace
            </a>
          )}
        </div>
      </div>
      {onPatch && (
        <OverridePeek
          open={peekOpen}
          onOpenChange={setPeekOpen}
          agent={agent}
          ai={ai}
          integrationId={integrationId}
          onChoose={(id) => onPatch({ integrationId: id })}
        />
      )}
    </div>
  );
}

function OverridePeek({
  open,
  onOpenChange,
  agent,
  ai,
  integrationId,
  onChoose,
}: {
  open: boolean;
  onOpenChange: (o: boolean) => void;
  agent: WizardState["agent"];
  ai: IntegrationRow[];
  integrationId: string | undefined;
  onChoose: (id: string | undefined) => void;
}) {
  const DEFAULT_OPTION = "__default__";
  const [draft, setDraft] = React.useState(integrationId ?? DEFAULT_OPTION);
  // Reseed the draft from the current pick every time the sheet opens, so a
  // Cancel from a previous open never leaks into the next one.
  React.useEffect(() => {
    if (open) setDraft(integrationId ?? DEFAULT_OPTION);
  }, [open, integrationId]);

  const capability = agentCapability(agent);

  return (
    <Sheet open={open} onOpenChange={onOpenChange}>
      <SheetContent className="flex w-full flex-col sm:max-w-md">
        <SheetHeader>
          <SheetTitle>Model access for this run</SheetTitle>
          <SheetDescription>
            Integrations that can power {agent === "codex-cli" ? "Codex CLI" : "Claude Code"}. This is a
            one-off choice for this run only — it changes nothing on the workspace or the server
            default.
          </SheetDescription>
        </SheetHeader>
        <div className="scroll-thin flex-1 space-y-2 overflow-y-auto px-4">
          <PeekRow
            title="Use the server default"
            selected={draft === DEFAULT_OPTION}
            onSelect={() => setDraft(DEFAULT_OPTION)}
          />
          {ai.map((row) => {
            const reason = incompatibleReason(row, capability);
            return (
              <PeekRow
                key={row.id}
                title={row.name}
                type={row.typeLabel}
                chip={
                  !reason && (
                    <Chip tone={RESIDENCY_META[row.residency].tone} className="text-[0.6875rem]">
                      {RESIDENCY_META[row.residency].label}
                    </Chip>
                  )
                }
                selected={draft === row.id}
                muted={!!reason}
                reason={reason}
                onSelect={reason ? undefined : () => setDraft(row.id)}
              />
            );
          })}
          <p className="text-[0.6875rem] leading-snug text-muted-foreground">
            Muted rows are facts, not absences — the reason is the cell copy from Integrations,
            verbatim.
          </p>
        </div>
        <SheetFooter className="flex-row justify-end gap-2">
          <Button type="button" variant="outline" size="sm" onClick={() => onOpenChange(false)}>
            Cancel
          </Button>
          <Button
            type="button"
            size="sm"
            onClick={() => {
              onChoose(draft === DEFAULT_OPTION ? undefined : draft);
              onOpenChange(false);
            }}
          >
            Use this integration
          </Button>
        </SheetFooter>
      </SheetContent>
    </Sheet>
  );
}

function PeekRow({
  title,
  type,
  chip,
  selected,
  muted,
  reason,
  onSelect,
}: {
  title: string;
  type?: string;
  chip?: React.ReactNode;
  selected: boolean;
  muted?: boolean;
  reason?: string;
  onSelect?: () => void;
}) {
  return (
    <button
      type="button"
      disabled={!onSelect}
      onClick={onSelect}
      className={cn(
        "flex w-full flex-col items-start gap-1 rounded-lg border p-2.5 text-left transition-colors",
        selected ? "border-primary bg-primary/10" : "border-border",
        muted && "opacity-60",
        !onSelect && "cursor-default",
      )}
    >
      <div className="flex flex-wrap items-center gap-2">
        {!muted && (
          <span
            className={cn(
              "inline-flex size-3.5 shrink-0 items-center justify-center rounded-full border",
              selected ? "border-primary" : "border-border-strong",
            )}
          >
            {selected && <span className="size-1.5 rounded-full bg-primary" />}
          </span>
        )}
        <span className="text-[0.8125rem] font-medium text-foreground">{title}</span>
        {type && <Mono className="text-[0.6875rem] text-muted-foreground">{type}</Mono>}
        {chip}
      </div>
      {reason && <p className="text-[0.6875rem] leading-snug text-muted-foreground">{reason}</p>}
    </button>
  );
}

function SecretCombobox({
  value,
  onChange,
  secrets,
  loading,
}: {
  value: string;
  onChange: (name: string) => void;
  secrets: string[];
  loading: boolean;
}) {
  const [open, setOpen] = React.useState(false);
  return (
    <Popover open={open} onOpenChange={setOpen}>
      <PopoverTrigger asChild>
        <Button
          type="button"
          variant="outline"
          role="combobox"
          aria-expanded={open}
          className="w-64 justify-between font-mono"
        >
          <span className={cn(!value && "font-sans text-muted-foreground")}>
            {value || (loading ? "Loading secrets…" : "Select a secret…")}
          </span>
          <ChevronsUpDown className="size-4 opacity-50" />
        </Button>
      </PopoverTrigger>
      <PopoverContent className="w-64 p-0" align="start">
        <Command>
          <CommandInput placeholder="Search secrets…" />
          <CommandList>
            <CommandEmpty>{loading ? "Loading…" : "No secrets found."}</CommandEmpty>
            <CommandGroup>
              {value && (
                <CommandItem
                  value="__none__"
                  onSelect={() => {
                    onChange("");
                    setOpen(false);
                  }}
                  className="text-muted-foreground"
                >
                  Clear selection
                </CommandItem>
              )}
              {secrets.map((name) => (
                <CommandItem
                  key={name}
                  value={name}
                  onSelect={(v) => {
                    onChange(v);
                    setOpen(false);
                  }}
                  className="font-mono"
                >
                  <Check
                    className={cn("size-4", value === name ? "opacity-100" : "opacity-0")}
                  />
                  {name}
                </CommandItem>
              ))}
            </CommandGroup>
          </CommandList>
        </Command>
      </PopoverContent>
    </Popover>
  );
}
