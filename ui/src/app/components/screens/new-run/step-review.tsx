/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Step 5 — Review: a structured human summary, the exact composed inline_policy
// JSON that will be sent, and the optional save-as-profile control.
import * as React from "react";
import { TriangleAlert } from "lucide-react";
import { Switch } from "../../ui/switch";
import { Input } from "../../ui/input";
import { Label } from "../../ui/label";
import { Mono, YamlBlock } from "../../wardyn/code-block";
import { ConfinementChip, Chip } from "../../wardyn/primitives";
import { RUN_MODE } from "../../wardyn/copy";
import { STATUS_TONE, STATUS_LABEL } from "../workspaces";
import { Field } from "./step-shell";
import { buildSpec, type WizardState } from "./wizard-types";
import { SetupChecklist } from "./compose-review";
import { CC_META } from "../../wardyn/cc-meta";
import type { PreflightResult, Workspace } from "../../../lib/types";
import { firstUseLabel } from "../../../lib/types";

export function StepReview({
  state,
  patch,
  workspaces = [],
  preflight = null,
  preflightStatus = "idle",
  onAddSecret,
  onFixWorkspace,
}: {
  state: WizardState;
  patch: (p: Partial<WizardState>) => void;
  // The onboarded-workspace list state.workspaces[].workspaceId resolves
  // against — needed to render human-readable names/sources below. Optional
  // (defaults to []) so a caller that hasn't loaded it yet still renders.
  workspaces?: Workspace[];
  // The Review preflight result (POST /runs/preflight) — the deterministic setup
  // checklist + the class the run will ACTUALLY run at. Advisory: it NEVER blocks
  // Review — while loading we show "Checking…", on error a quiet one-liner.
  preflight?: PreflightResult | null;
  preflightStatus?: "idle" | "loading" | "error";
  onAddSecret?: (name: string) => void;
  onFixWorkspace?: (workspaceId: string) => void;
}) {
  const { run, inline_policy } = React.useMemo(
    () => buildSpec(state, workspaces),
    [state, workspaces],
  );
  const byId = new Map(workspaces.map((w) => [w.id, w]));

  const isClaude = state.agent === "claude-code";
  const isSubscription = isClaude && state.anthropicAuth === "subscription";
  const authLabel = !isClaude
    ? "OpenAI API key"
    : state.anthropicAuth === "subscription"
      ? "Subscription (OAuth)"
      : state.anthropicAuth === "bedrock"
        ? "Bedrock"
        : "API key";

  // No-model-access surfacing (B3): the api-key path (Claude apikey / Codex) with no
  // stored secret selected — the run launches but its first model call 404s.
  // Subscription mounts creds (has access); bedrock is its own path.
  //
  // Never contradict the server. An operator-configured transport (Bedrock, a
  // managed subscription) is applied AT DISPATCH and overrides the per-run
  // api-key selection, so this local guess would otherwise tell an operator whose
  // model access demonstrably works — in red — that they have none, and point
  // them at a secret list that holds no model key. The preflight's llm_access
  // verdict is the same one launch uses; when it says access is provisioned, it
  // wins. (Preflight is advisory and may be absent/loading: fall back to the
  // local guess rather than silently hiding a real gap.)
  const llmAccessItem = preflight?.setup_items?.find((i) => i.kind === "llm_access");
  const noLlmCred =
    (!isClaude || state.anthropicAuth === "apikey") &&
    !state.llmSecretName &&
    !isSubscription &&
    llmAccessItem?.status !== "satisfied";

  // The run will run at enforced_confinement_class, which the deterministic
  // blast-radius floor can raise ABOVE the operator's pick when the run holds a
  // write-capable / third-party production credential (RequiredConfinementFloor).
  const enforced = preflight?.enforced_confinement_class;
  const raised = !!enforced && enforced !== state.confinementClass;

  const egressValue = inline_policy.allow_all_egress
    ? `Allow all (deny-list only)${
        inline_policy.denied_domains?.length
          ? `, ${inline_policy.denied_domains.length} denied`
          : ""
      }`
    : `${inline_policy.allowed_domains.length} allowed${
        inline_policy.denied_domains?.length
          ? `, ${inline_policy.denied_domains.length} denied`
          : ""
      }`;

  return (
    <div className="space-y-5">
      {isSubscription && (
        <Chip tone="warning" dot>
          Reduced isolation: credential resident in sandbox
        </Chip>
      )}
      {noLlmCred && (
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
            Go back to Access and pick a stored key (or add one).
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
          Preflight unavailable — you can still launch.
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

      <div className="grid grid-cols-2 gap-x-4 gap-y-3 rounded-lg border border-border p-3 text-sm">
        <Summary label="Agent" value={<Mono className="text-foreground">{run.agent}</Mono>} />
        <Summary
          label="Mode"
          value={run.interactive ? RUN_MODE.interactive.label : RUN_MODE.autonomous.label}
        />
        <Summary label="Repo" value={<Mono className="text-foreground">{run.repo || "—"}</Mono>} />
        {run.image && (
          <Summary
            label="Image"
            value={<Mono className="break-all text-foreground">{run.image}</Mono>}
          />
        )}
        <Summary label="Auth" value={authLabel} />
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
          value={firstUseLabel(inline_policy.first_use_approval)}
        />
        {state.workspaces.length > 0 && (
          <Summary
            label="Workspaces"
            value={
              <div className="space-y-1">
                {state.workspaces.map((sel, i) => {
                  const w = byId.get(sel.workspaceId);
                  return (
                    <div key={sel.workspaceId} className="flex items-center gap-1.5">
                      {i === 0 && (
                        <Chip tone="primary" className="px-1.5 py-0 text-[0.625rem]">
                          primary
                        </Chip>
                      )}
                      <Mono className="text-foreground">
                        {w?.name ?? sel.workspaceId} ({w?.source ?? "unresolved"})
                        {w?.kind === "local_dir" && ` — ${sel.readOnly ? "ro" : "rw"}`}
                        {w?.kind === "container" && " — image"}
                      </Mono>
                      {/* Surface scan status so a still-pending / errored workspace isn't
                          attached silently at the final gate. */}
                      {w && w.status !== "ready" && (
                        <Chip tone={STATUS_TONE[w.status]} className="px-1.5 py-0 text-[0.625rem]">
                          {STATUS_LABEL[w.status]}
                        </Chip>
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
          inline_policy (sent verbatim)
        </Label>
        <YamlBlock value={inline_policy} className="mt-1.5" />
      </div>

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
              <Field label="Profile name" htmlFor="profile-name">
                <Input
                  id="profile-name"
                  placeholder="payments-interactive"
                  value={state.profileName}
                  onChange={(e) => patch({ profileName: e.target.value })}
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
