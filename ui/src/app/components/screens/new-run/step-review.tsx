/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Step 5 — Review: a structured human summary, the exact policy JSON that will be
// enforced (the composed inline_policy, or the STORED spec when the run is
// attached to a saved policy), and the optional save-as-profile control.
import * as React from "react";
import { ArrowUpRight, KeyRound, RefreshCw, TriangleAlert, WandSparkles } from "lucide-react";
import { Switch } from "../../ui/switch";
import { Input } from "../../ui/input";
import { Label } from "../../ui/label";
import { Checkbox } from "../../ui/checkbox";
import { Button } from "../../ui/button";
import { cn } from "../../ui/utils";
import { Mono, YamlBlock } from "../../wardyn/code-block";
import { ConfinementChip, Chip, RiskBadge } from "../../wardyn/primitives";
import { RUN_MODE, RISK_ATTRIBUTION, SETUP_RESIDENCY_NOTE } from "../../wardyn/copy";
import type { StatusKind } from "../../wardyn/copy";
import { StatusChip } from "../../wardyn/status-chip";
import { Field } from "../../wardyn/form-primitives";
import { sourceSubLine } from "../workspaces";
import {
  agentLabel,
  buildSpec,
  comesWithLine,
  impliedEgressHosts,
  primaryWorkspaceId,
  type WizardState,
} from "./wizard-types";
// Reused rather than re-derived: the SAME resolution preview step-access.tsx's
// card computes, so Review can't disagree with Access about what this run's
// model access resolves to.
import { resolveModelAccess } from "./step-access";
import { CC_META } from "../../wardyn/cc-meta";
import { integrationsApi, type IntegrationRow } from "../../../lib/api/integrations";
import { RESIDENCY_META } from "../../../lib/integrations";
import { RD } from "../../../lib/workspace-copy";
import { setup as setupApi } from "../../../lib/api/setup";
import type {
  PreflightResult,
  RiskItem,
  RiskLevel,
  RunPolicy,
  SetupItem,
  WireIntegration,
  Workspace,
} from "../../../lib/types";
import { firstUseLabel } from "../../../lib/types";
import { statusTone, statusWord } from "../../../lib/workspace-status";

// D6/claim3: the Egress summary used to print a bare count — the one screen
// whose job is "show what you're launching" made the operator open the raw
// JSON to learn which hosts a grant silently added. Names them instead,
// truncated so a long allowlist doesn't blow out the summary grid; the full
// list is still exact in the YamlBlock below regardless.
function truncatedHostList(hosts: string[], max = 4): string {
  if (hosts.length === 0) return "none";
  if (hosts.length <= max) return hosts.join(", ");
  return `${hosts.slice(0, max).join(", ")}, +${hosts.length - max} more`;
}

export function StepReview({
  state,
  patch,
  workspaces = [],
  attachedPolicy,
  preflight = null,
  preflightStatus = "idle",
  acknowledged = false,
  onAcknowledge = () => {},
  onAddSecret,
  onFixWorkspace,
}: {
  state: WizardState;
  patch: (p: Partial<WizardState>) => void;
  // The onboarded-workspace list state.workspaces[].workspaceId resolves
  // against — needed to render human-readable names/sources below. Optional
  // (defaults to []) so a caller that hasn't loaded it yet still renders.
  workspaces?: Workspace[];
  // The saved policy this run is attached to (state.selectedPolicyId), if any.
  // Present => the run launches by policy_id and the SERVER enforces this stored
  // spec, so Review must show it rather than buildSpec's composed approximation.
  attachedPolicy?: RunPolicy;
  // The Review preflight result (POST /runs/preflight) — the deterministic setup
  // checklist + the class the run will ACTUALLY run at. Advisory: it NEVER blocks
  // Review — while loading we show "Checking…", on error a quiet one-liner.
  preflight?: PreflightResult | null;
  preflightStatus?: "idle" | "loading" | "error";
  // The HIGH-risk acknowledgment (N1 fix): the wizard OWNS this bit (resets it
  // whenever a fresh preflight lands, since a stale ack must never survive a
  // spec change) and also reads it to gate the Launch button — StepReview only
  // renders the checkbox. Optional/defaulted so a caller with no risk data
  // (older preflight fixture, or a test) never has to thread it through.
  acknowledged?: boolean;
  onAcknowledge?: (v: boolean) => void;
  // `kind` is the checklist item's own SetupItem.kind (SetupChecklistRow
  // below forwards it) — UI-RUN-1: the wizard's onAddSecret
  // handler branches on it so only a genuine llm_access row can ever route a
  // saved secret name into llmSecretName; every other kind (a git_pat's own
  // "secret" row included) must not.
  onAddSecret?: (name: string, kind: string) => void;
  onFixWorkspace?: (workspaceId: string) => void;
}) {
  const { run, inline_policy: composed } = React.useMemo(
    () => buildSpec(state, workspaces),
    [state, workspaces],
  );
  // buildSpec NORMALIZES (it force-adds the api_key injection host, unions the
  // workspaces' required hosts, dedupes) and wizardStateFromProposal is an
  // explicitly best-effort inverse — so a state loaded FROM a stored policy does
  // not compose back to it byte-for-byte. While attached, everything below must
  // read the stored spec, or Review would show a policy the server won't enforce.
  const inline_policy = attachedPolicy?.spec ?? composed;
  const byId = new Map(workspaces.map((w) => [w.id, w]));
  // The server's own primary pick (mounts-then-repos), not raw selection order
  // (PARITY-3) — so this cell can't name a different workspace's credential
  // binding than the run actually inherits.
  const primaryWsId = primaryWorkspaceId(state.workspaces, workspaces);
  const primaryWorkspace = primaryWsId ? byId.get(primaryWsId) : undefined;
  const isGovernedCommand = state.runType === "command";

  // Self-fetched (Review doesn't otherwise receive the ai_provider integrations
  // list) — the SAME resolution preview step-access.tsx's card renders, reused
  // here rather than re-derived so the two steps can't disagree.
  const [ai, setAi] = React.useState<IntegrationRow[]>([]);
  // UI-RUN-2: gates the first paint the same way ModelAccessCard's own `loaded`
  // does (step-access.tsx, M5) — before this settles, `resolved`/`noLlmCred`
  // are computed off default/empty state, so the amber "No model access" box
  // must not render as a false positive before there's anything to conclude.
  const [aiLoaded, setAiLoaded] = React.useState(false);
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
  // The effective wire rows — resolveModelAccess's tier-3 needs this to tell a
  // genuinely-marked DefaultFor:agent_runs row from merely the first
  // compatible one, same as step-access.tsx's own card. bedrockReady is the
  // same status.bedrock?.ready its bedrock carve-out also needs.
  const [integrations, setIntegrations] = React.useState<WireIntegration[] | undefined>(undefined);
  const [bedrockReady, setBedrockReady] = React.useState<boolean | undefined>(undefined);
  const [statusLoaded, setStatusLoaded] = React.useState(false);
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
  // The THIRD loaded-gate input: preflight is a separate, per-entry fetch
  // (POST /runs/preflight, kicked off by the wizard on stepping into Review)
  // whose OWN verdict (llm_access satisfied?) can override the local
  // resolution below — a "loading" preflight is exactly as unresolved as the
  // two self-fetches above.
  const loaded = aiLoaded && statusLoaded && preflightStatus !== "loading";
  const resolved = isGovernedCommand
    ? null
    : resolveModelAccess(state.agent, state.integrationId, primaryWorkspace, ai, integrations, bedrockReady);
  // A resident credential (a "warning"-toned residency, e.g. a host-CLI
  // subscription mount) means reduced isolation — surfaced the same way
  // regardless of WHICH integration ended up resolved.
  const residentAccess = resolved && RESIDENCY_META[resolved.row.residency].tone === "warning";

  // No-model-access surfacing (B3): nothing resolved for this run, so it will
  // launch but its first model call 404s. Never contradict the server: the
  // preflight's llm_access verdict is the same one launch uses, so when it says
  // access is provisioned, it wins over this local resolution preview.
  // (Preflight is advisory and may be absent/loading: fall back to the local
  // resolution rather than silently hiding a real gap.) Irrelevant for a
  // governed command — there is no agent, so nothing needs to resolve.
  const llmAccessItem = preflight?.setup_items?.find((i) => i.kind === "llm_access");
  const noLlmCred =
    !isGovernedCommand && !resolved && llmAccessItem?.status !== "satisfied";

  // The run will run at enforced_confinement_class, which the deterministic
  // blast-radius floor can raise ABOVE the operator's pick when the run holds a
  // write-capable / third-party production credential (RequiredConfinementFloor).
  const enforced = preflight?.enforced_confinement_class;
  const raised = !!enforced && enforced !== state.confinementClass;

  // Deterministic risk grade (N1 fix): derived off the OPTIONAL preflight
  // field — absent/older-server preflight ⇒ empty inputs ⇒ needsAck false ⇒
  // RiskPanel renders nothing below (never a fake gate).
  const highItems = React.useMemo(
    () => preflight?.risk_assessment?.filter((r) => r.risk_level === "high") ?? [],
    [preflight],
  );
  const needsAck = highItems.length > 0;
  const why = React.useMemo(() => whyRisky(preflight?.risk_assessment ?? []), [preflight]);

  const egressValue = inline_policy.allow_all_egress
    ? `Allow all (deny-list only)${
        inline_policy.denied_domains?.length
          ? `, ${inline_policy.denied_domains.length} denied`
          : ""
      }`
    : `${truncatedHostList(
        // Implied (grant-added) hosts FIRST: buildSpec appends them last, so a
        // ≥4-host allowlist would truncate away exactly the hosts this row
        // exists to reveal (D6). Order here is display-only — the enforced
        // list in the YamlBlock below is untouched.
        (() => {
          const implied = impliedEgressHosts(state, workspaces).map((h) => h.host);
          const impliedSet = new Set(implied);
          return [
            ...implied.filter((h) => inline_policy.allowed_domains.includes(h)),
            ...inline_policy.allowed_domains.filter((h) => !impliedSet.has(h)),
          ];
        })(),
      )}${
        inline_policy.denied_domains?.length
          ? `, ${inline_policy.denied_domains.length} denied`
          : ""
      }`;

  return (
    <div className="space-y-5">
      {residentAccess && (
        <Chip tone="warning" dot>
          Reduced isolation: credential resident in sandbox
        </Chip>
      )}
      {/* UI-RUN-2: gated on `loaded` — before the self-fetches settle, noLlmCred
          is computed off default/empty state and would otherwise assert "No
          model access" on every entry, even for a working pinned integration. */}
      {loaded && noLlmCred && (
        <div
          className="flex items-start gap-2 rounded-lg border border-warning/40 bg-warning-subtle p-3 text-xs leading-relaxed text-warning"
          data-testid="review-no-model-access"
        >
          <TriangleAlert className="mt-0.5 size-3.5 shrink-0" aria-hidden="true" />
          <div>
            <span className="font-semibold">No model access.</span> This run has no LLM credential, so it
            will launch but its first model call will 404.{" "}
            {state.mode === "batch" && (
              <span data-testid="review-batch-no-model">
                An autonomous run can't perform its task without model access.{" "}
              </span>
            )}
            Connect a provider under Integrations, pin one on the workspace's Model access, or use
            Access → Override for this run.
          </div>
        </div>
      )}

      {/* Preflight: a DRY-RUN of launch's resolution + gating (POST /runs/preflight).
          Advisory only — while it resolves we show "Checking…", and any error shows a
          quiet one-liner; neither ever blocks Review. */}
      {preflightStatus === "loading" && (
        <p className="text-xs text-muted-foreground" data-testid="preflight-checking">
          Checking setup…
        </p>
      )}
      {preflightStatus === "error" && (
        <p className="text-xs text-muted-foreground" data-testid="preflight-unavailable">
          Preflight unavailable — the setup checklist and risk grade couldn&apos;t be
          computed for this spec. You can still launch.
        </p>
      )}
      {raised && enforced && (
        <div
          className="flex items-start gap-2 rounded-lg border border-warning/40 bg-warning-subtle p-3 text-xs leading-relaxed text-warning"
          data-testid="preflight-cc-raise"
        >
          <TriangleAlert className="mt-0.5 size-3.5 shrink-0" aria-hidden="true" />
          <div>
            Launches at {CC_META[enforced].label} — raised automatically because this run holds
            write-capable or third-party production credentials.
          </div>
        </div>
      )}
      {preflight && preflight.setup_items.length > 0 && (
        <div data-testid="preflight-checklist">
          <SetupChecklist
            items={preflight.setup_items}
            onAddSecret={onAddSecret}
            onFixWorkspace={onFixWorkspace}
          />
        </div>
      )}

      {/* Clamp notices — the SAME "Tightened by policy" block ComposeReview shows.
          Non-empty only when a member's inline_policy was bounded or a grant was
          dropped; tells them WHY the enforced policy differs from what they typed,
          since launch itself stays silent (resolveRunPolicy's contract). */}
      {preflight?.warnings && preflight.warnings.length > 0 && (
        <div
          className="flex items-start gap-2 rounded-lg border border-warning/30 bg-warning-subtle p-3 text-xs leading-relaxed text-warning"
          data-testid="preflight-clamp-warnings"
        >
          <WandSparkles className="mt-0.5 size-3.5 shrink-0" aria-hidden="true" />
          <div>
            <span className="font-medium">Tightened by policy:</span>
            <ul className="mt-1 list-disc space-y-0.5 pl-4">
              {preflight.warnings.map((w, i) => (
                <li key={i}>{w}</li>
              ))}
            </ul>
          </div>
        </div>
      )}

      <div className="grid grid-cols-2 gap-x-4 gap-y-3 rounded-lg border border-border p-3 text-sm">
        <Summary label="Agent" value={<Mono className="text-foreground">{run.agent}</Mono>} />
        <Summary
          label="Mode"
          value={run.interactive ? RUN_MODE.interactive.label : RUN_MODE.autonomous.label}
        />
        {/* Was mislabeled "Repo" with a local dir leaking through as the
            synthetic run.repo="local:<basename>" wire label — the primary
            onboarded workspace is the truth: its own name/kind/source. */}
        {primaryWorkspace ? (
          <Summary
            label="Workspace"
            value={
              <div>
                <div className="text-foreground">{primaryWorkspace.name}</div>
                <div className="text-[0.6875rem] text-muted-foreground">
                  {sourceSubLine(primaryWorkspace)}
                </div>
              </div>
            }
          />
        ) : (
          <Summary label="Workspace" value="none (ephemeral scratch)" />
        )}
        {run.image && (
          <Summary
            label="Image"
            value={<Mono className="break-all text-foreground">{run.image}</Mono>}
          />
        )}
        <Summary
          label="Model access"
          value={
            isGovernedCommand ? (
              <span className="text-muted-foreground">{RD.EXEC_LINE}</span>
            ) : !loaded ? (
              // UI-RUN-2: `resolved` is meaningless before the self-fetches
              // settle — the sibling ModelAccessCard's own first-paint gate,
              // ported here so Review can't assert either verdict early.
              <span className="text-muted-foreground">{RD.RESOLVING_LINE}</span>
            ) : resolved ? (
              <div className="flex flex-wrap items-center gap-1.5">
                <span className="text-foreground">{resolved.row.name}</span>
                <Mono className="text-muted-foreground">{resolved.row.typeLabel}</Mono>
              </div>
            ) : !noLlmCred ? (
              // resolveModelAccess found nothing to NAME, but the server's
              // preflight — the AUTHORITATIVE check, launch's own resolver —
              // says llm_access is satisfied anyway (the checklist above says
              // how). Never contradict it: showing RD.NONE_LINE here would be
              // the exact bug the warning box above this already refuses to
              // repeat.
              <span className="text-muted-foreground">Provisioned — see the checklist above.</span>
            ) : (
              <span className="text-warning">{RD.NONE_LINE(agentLabel(state.agent))}</span>
            )
          }
        />
        <Summary
          label="Confinement"
          value={<ConfinementChip value={state.confinementClass} />}
        />
        <Summary label="Egress" value={egressValue} />
        <Summary
          label="Grants"
          value={
            (inline_policy.eligible_grants?.length ?? 0) === 0 ? (
              "none"
            ) : (
              <div className="flex flex-wrap gap-1">
                {inline_policy.eligible_grants!.map((g, i) => (
                  <Chip key={i} tone="info" mono className="px-1.5 py-0 text-[0.625rem]">
                    {String(g.kind)}
                  </Chip>
                ))}
              </div>
            )
          }
        />
        <Summary
          label="Lifecycle"
          value={
            inline_policy.auto_stop_after_sec === -1
              ? "Never reap"
              : inline_policy.auto_stop_after_sec != null
                ? `Auto-stop after ${Math.round(inline_policy.auto_stop_after_sec / 60)} min`
                : "Platform default"
          }
        />
        <Summary
          label="First-use approval"
          value={firstUseLabel(inline_policy.first_use_approval, inline_policy.allow_all_egress)}
        />
        {state.workspaces.length > 0 && (
          <Summary
            label="Workspaces"
            value={
              <div className="space-y-1.5">
                {state.workspaces.map((sel, i) => {
                  const w = byId.get(sel.workspaceId);
                  // Read the ACTUAL composed mount rather than the raw sel.readOnly
                  // flag — a workspace's requirements contract can resolve write
                  // access differently than the flag alone suggests (see
                  // wizard-types.ts's resolvedMountReadOnly); this is the exact
                  // value the JSON below will show.
                  const mount = (inline_policy.workspace_mounts ?? []).find(
                    (m) => w && m.source === w.source,
                  );
                  return (
                    <div key={sel.workspaceId}>
                      <div className="flex items-center gap-1.5">
                        {i === 0 && (
                          <Chip tone="primary" className="px-1.5 py-0 text-[0.625rem]">
                            primary
                          </Chip>
                        )}
                        <Mono className="text-foreground">
                          {w?.name ?? sel.workspaceId} ({w?.source ?? "unresolved"})
                          {w?.kind === "local_dir" && mount && ` — ${mount.read_only ? "ro" : "rw"}`}
                        </Mono>
                        {/* Surface a FAILED workspace so it is never attached silently at
                            the final gate. Keyed on the failure itself, not on
                            isUsable: attachment never depended on this status (an
                            errored workspace attaches fine), so isUsable is now true
                            for every status — and keying the warning off it silenced
                            the one place whose whole job is to warn here. */}
                        {w && statusWord(w.status) === "Import failed" && (
                          <Chip tone={statusTone(w.status).tone} className="px-1.5 py-0 text-[0.625rem]">
                            {statusWord(w.status)}
                          </Chip>
                        )}
                      </div>
                      {w && (
                        <div className="pl-0.5 text-[0.6875rem] text-muted-foreground">
                          {/* sel carries this run's enabledOptional — without it, an
                              Optional row toggled in Basics (e.g. an egress host) never
                              showed up here, so the one screen whose whole job is "show
                              what you're launching" silently dropped the edit. */}
                          Comes with: {comesWithLine(w, sel)}
                        </div>
                      )}
                    </div>
                  );
                })}
              </div>
            }
          />
        )}
      </div>

      <div>
        <Label className="text-[0.6875rem] uppercase tracking-wide text-muted-foreground">
          {attachedPolicy
            ? `policy “${attachedPolicy.name}” (sent by reference as policy_id)`
            : "inline_policy (sent verbatim)"}
        </Label>
        <YamlBlock value={inline_policy} className="mt-1.5" />
      </div>

      {/* --- deterministic risk grade + HIGH-only acknowledgment gate (D8),
          fed by THIS step's own preflight response (POST /runs/preflight).
          Preflight is advisory: absent/failed (or an older server with no
          risk_assessment) renders no panel and no gate, the same non-blocking
          degrade the setup checklist above already has — a preflight outage
          must never itself block Review. --- */}
      {preflight && preflight.risk_assessment && preflight.risk_assessment.length > 0 && (
        <RiskPanel
          // Fallback direction matters: a response carrying items but no
          // overall must degrade toward the items' own worst grade, never
          // toward a rosier "low" badge above a red HIGH list.
          overallRisk={preflight.overall_risk ?? (highItems.length > 0 ? "high" : "low")}
          why={why}
          highItems={highItems}
          needsAck={needsAck}
          acknowledged={acknowledged}
          onAcknowledge={onAcknowledge}
          attribution="Graded deterministically by Wardyn's rules."
        />
      )}

      {/* Hidden when the run is already based on a recorded profile — re-saving the
          same spec as a new policy is redundant. */}
      {!state.selectedProfile && (
        <div className="rounded-lg border border-border p-3">
          <div className="flex items-center justify-between">
            <div>
              <Label htmlFor="save-profile">Save as a reusable policy</Label>
              <p className="mt-0.5 text-[0.6875rem] text-muted-foreground">
                Persist this spec as a named policy so future runs can reference it.
              </p>
            </div>
            <Switch
              id="save-profile"
              checked={state.saveAsProfile}
              onCheckedChange={(c) => patch({ saveAsProfile: c })}
            />
          </div>
          {state.saveAsProfile && (
            <div className="mt-3 border-t border-border pt-3">
              <Field label="Profile name" htmlFor="profile-name" required>
                <Input
                  id="profile-name"
                  placeholder="payments-interactive"
                  value={state.profileName}
                  onChange={(e) => patch({ profileName: e.target.value })}
                  required
                />
              </Field>
            </div>
          )}
        </div>
      )}
    </div>
  );
}

function Summary({ label, value }: { label: string; value: React.ReactNode }) {
  return (
    <div>
      <div className="text-[0.6875rem] uppercase tracking-wide text-muted-foreground">{label}</div>
      <div className="mt-0.5 text-foreground">{value}</div>
    </div>
  );
}

// whyRisky: the "why" line — HIGH rationales if any, else the top few rationales.
// All are the grader's already-human-readable strings (never the model's).
// Moved from the (now-deleted) AI Run Composer's compose-quick-review.tsx —
// this is a pure function over preflight.risk_assessment, which survives it.
export function whyRisky(risk_assessment: RiskItem[]): string[] {
  const high = risk_assessment.filter((r) => r.risk_level === "high").map((r) => r.rationale);
  const src = high.length > 0 ? high : risk_assessment.map((r) => r.rationale);
  return src.slice(0, 3);
}

// Deterministic risk grade (D8): RISK_ATTRIBUTION makes clear Wardyn's rules
// graded this — not the model — and ONLY a HIGH grade gates launch behind an
// explicit acknowledgment. Container tone escalates by whether it needs an
// acknowledgment, not by the raw grade (see `tone` below, §5) — the high items
// list the grader's plain-language rationales (never the raw wire field /
// invariant ref — D4).
// Moved from the (now-deleted) AI Run Composer's compose-review.tsx — the
// manual wizard was already the SOLE remaining caller (it renders this off
// its own preflight response, POST /runs/preflight).
export function RiskPanel({
  overallRisk,
  why,
  highItems,
  needsAck,
  acknowledged,
  onAcknowledge,
  attribution = RISK_ATTRIBUTION,
}: {
  overallRisk: RiskLevel;
  why: string[];
  highItems: RiskItem[];
  needsAck: boolean;
  acknowledged: boolean;
  onAcknowledge: (v: boolean) => void;
  // The composer default ("…not the model") is load-bearing on the AI path;
  // the manual wizard consulted no model, so it passes its own path-neutral
  // sentence instead of implying one existed.
  attribution?: string;
}) {
  // Tone by whether it needs YOU, not by grade (§5) — a Medium default (the
  // typical CC2 host) used to paint itself amber on every run, training
  // operators to ignore the colour that must survive for the HIGH case.
  // RiskBadge below still renders the real "Medium" label in amber; only the
  // container chrome stops alarming for a state that requires no action.
  const tone = needsAck ? "border-danger/40 bg-danger-subtle" : "border-border bg-muted/30";
  return (
    <div className={cn("space-y-2 rounded-xl border p-4", tone)}>
      <div className="flex flex-wrap items-center gap-2">
        <span className="text-sm font-medium text-foreground">Risk:</span>
        <RiskBadge level={overallRisk} />
        {/* whyRisky() returns the HIGH items' rationales first, so when needsAck is
            true why[0] === highItems[0].rationale and the "High-risk configuration"
            list below already prints it — showing it inline too duplicated the
            sentence on the launch-blocked path. Inline why is for the Medium/Low
            case (no list); when there's a list, let the list own it. */}
        {!needsAck && why[0] && <span className="text-[0.7813rem] text-foreground">{why[0]}</span>}
      </div>
      {/* Non-exhaustive on purpose — the deterministic grader (composer/risk.go)
          has more HIGH triggers (weakest barrier tier, never-reap, …) and this
          copy must never claim to enumerate them. */}
      <p className="text-[0.6875rem] leading-relaxed text-muted-foreground">
        {attribution} High-graded choices — such as the weakest barrier tier, allow-all
        egress, host-writable mounts, or write-capable credentials — require an explicit
        acknowledgment before launch.
      </p>

      {needsAck && (
        <div className="mt-1 border-t border-danger/30 pt-3" data-testid="high-risk-section">
          <div className="flex items-center gap-2 text-danger">
            <TriangleAlert className="size-4" aria-hidden="true" />
            <span className="text-sm font-semibold">High-risk configuration</span>
          </div>
          <ul className="mt-2 list-disc space-y-0.5 pl-4 text-xs text-danger">
            {highItems.map((item, i) => (
              <li key={`${item.field}-${i}`}>{item.rationale}</li>
            ))}
          </ul>
          <label className="mt-3 flex items-start gap-2">
            <Checkbox
              id="ack-high-risk"
              checked={acknowledged}
              onCheckedChange={(c) => onAcknowledge(c === true)}
              className="mt-0.5"
            />
            <span className="text-xs text-foreground">
              I understand the high-risk items above and want to launch this run anyway.
            </span>
          </label>
        </div>
      )}
    </div>
  );
}

// Doctor-style setup readiness (decision 2/3/4): each row is label+detail |
// StatusChip | action button — the same DOM shape as the (retired) Getting-
// started funnel's AccessRow used, so the vocabulary never drifted between
// the two surfaces. Item text (label/detail/required_by) is server-derived
// but rendered as plain React text nodes ONLY — never dangerouslySetInnerHTML
// — the same caution as everywhere else a field could in principle be
// influenced by the analyzer's output.
// Moved from the (now-deleted) AI Run Composer's compose-review.tsx — the
// manual wizard was already the sole remaining caller.
export function SetupChecklist({
  items,
  onAddSecret,
  onFixWorkspace,
}: {
  items: SetupItem[];
  // `kind` is the row's own SetupItem.kind, forwarded verbatim (UI-RUN-1) —
  // the wizard's onAddSecret handler branches on it so only a genuine
  // llm_access row can ever route a saved secret name into llmSecretName;
  // every other kind (a git_pat's own "secret" row included) must not.
  onAddSecret?: (name: string, kind: string) => void;
  onFixWorkspace?: (workspaceId: string) => void;
}) {
  return (
    <div className="space-y-2">
      <span className="label-eyebrow">Setup checklist</span>
      <ul className="space-y-2">
        {items.map((item) => (
          <SetupChecklistRow
            key={item.id}
            item={item}
            onAddSecret={onAddSecret}
            onFixWorkspace={onFixWorkspace}
          />
        ))}
      </ul>
    </div>
  );
}

// v1 is declared-present, never live-verified (decision 3) — "satisfied" maps to
// the same green "ready" chip used everywhere else, but the label override below
// says "Configured", never "Ready"/"Verified", so the checklist can't be misread
// as a live health probe. Any status this build doesn't recognize degrades to the
// neutral "unverified" chip rather than crashing.
function setupStatusKind(status: string): StatusKind {
  if (status === "satisfied") return "ready";
  if (status === "missing") return "needs-setup";
  return "unverified";
}

function SetupChecklistRow({
  item,
  onAddSecret,
  onFixWorkspace,
}: {
  item: SetupItem;
  onAddSecret?: (name: string, kind: string) => void;
  onFixWorkspace?: (workspaceId: string) => void;
}) {
  const fix = item.fix;
  // Only a MISSING llm_access/secret gets the destructive treatment (the run will
  // do nothing, or 422s at launch — H1); every other gap (workspace/repo_credential/
  // egress/backend/config_pair, or any "unverified" item) stays a plain card — its
  // StatusChip tone (amber for needs-setup, neutral for unverified) already carries
  // the signal, so the card itself never needs to escalate too (decision 4). backend
  // and config_pair are host/config state, not a credential absence, so a missing
  // one is never destructive even though it blocks the SAME way a missing secret does.
  const destructive = item.status === "missing" && (item.kind === "llm_access" || item.kind === "secret");
  const residencyNote = item.residency ? SETUP_RESIDENCY_NOTE[item.residency] : undefined;
  // Deep-link to the workspace this row is about, when one is resolvable — the
  // SAME "<kind>:<key>" id-fallback the wizard's own re-flip logic leans on for
  // a "workspace" item without an explicit fix.workspace_id (compose_setup.go's
  // stable id contract). There is no per-id /workspaces/:id route yet, so this
  // lands on the list — still the right next step, just not a precise deep link.
  const workspaceId =
    fix?.workspace_id ?? (item.kind === "workspace" ? item.id.slice(item.kind.length + 1) : undefined);
  return (
    <li
      className={cn(
        "flex flex-wrap items-center gap-3 rounded-xl border p-3.5",
        destructive ? "border-danger/40 bg-danger-subtle" : "border-border bg-card",
      )}
      data-testid={`setup-item-${item.id}`}
    >
      <div className="min-w-[200px] flex-1">
        <div className="text-sm font-semibold text-foreground">{item.label}</div>
        <p className="mt-0.5 text-xs leading-snug text-muted-foreground">Required by {item.required_by}</p>
        {item.detail && <p className="mt-0.5 text-xs leading-snug text-muted-foreground">{item.detail}</p>}
        {residencyNote && (
          <p className="mt-0.5 text-[0.6875rem] leading-snug text-muted-foreground">{residencyNote}</p>
        )}
      </div>
      <StatusChip
        status={setupStatusKind(item.status)}
        label={item.status === "satisfied" ? "Configured" : undefined}
      />
      {fix?.action === "add_secret" && fix.secret_name && onAddSecret && (
        <Button
          size="sm"
          variant="outline"
          className="shrink-0 gap-1.5"
          onClick={() => onAddSecret(fix.secret_name!, item.kind)}
        >
          <KeyRound className="size-3.5" /> Add secret
        </Button>
      )}
      {fix?.action === "scan_workspace" && fix.workspace_id && onFixWorkspace && (
        <Button
          size="sm"
          variant="outline"
          className="shrink-0 gap-1.5"
          onClick={() => onFixWorkspace(fix.workspace_id!)}
        >
          <RefreshCw className="size-3.5" /> Scan workspace
        </Button>
      )}
      {workspaceId && (
        // Plain <a>, not react-router's <Link>: this checklist is shared with
        // the (retired) composer Review, which did not guarantee a Router
        // ancestor in every test/host context — kept as a plain anchor since
        // it's still cheap and correct here.
        <Button asChild size="sm" variant="ghost" className="shrink-0 gap-1.5">
          <a href="/workspaces">
            Open workspace <ArrowUpRight className="size-3.5" />
          </a>
        </Button>
      )}
    </li>
  );
}
