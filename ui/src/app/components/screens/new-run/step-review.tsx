/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Step 5 — Review: a structured human summary, the exact policy JSON that will be
// enforced (the composed inline_policy, or the STORED spec when the run is
// attached to a saved policy), and the optional save-as-profile control.
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
import { buildSpec, comesWithLine, type WizardState } from "./wizard-types";
// Reused rather than re-derived: the SAME resolution preview step-access.tsx's
// card computes, so Review can't disagree with Access about what this run's
// model access resolves to.
import { resolveModelAccess } from "./step-access";
import { SetupChecklist } from "./compose-review";
import { CC_META } from "../../wardyn/cc-meta";
import { integrationsApi, type IntegrationRow } from "../../../lib/api/integrations";
import { RESIDENCY_META } from "../../../lib/integrations";
import { RD } from "../../../lib/workspace-copy";
import type { PreflightResult, RunPolicy, Workspace } from "../../../lib/types";
import { firstUseLabel } from "../../../lib/types";
import { isUsable } from "../../../lib/workspace-status";

export function StepReview({
  state,
  patch,
  workspaces = [],
  attachedPolicy,
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
  // The saved policy this run is attached to (state.selectedPolicyId), if any.
  // Present => the run launches by policy_id and the SERVER enforces this stored
  // spec, so Review must show it rather than buildSpec's composed approximation.
  attachedPolicy?: RunPolicy;
  // The Review preflight result (POST /runs/preflight) — the deterministic setup
  // checklist + the class the run will ACTUALLY run at. Advisory: it NEVER blocks
  // Review — while loading we show "Checking…", on error a quiet one-liner.
  preflight?: PreflightResult | null;
  preflightStatus?: "idle" | "loading" | "error";
  onAddSecret?: (name: string) => void;
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
  const primaryWorkspace = state.workspaces[0] ? byId.get(state.workspaces[0].workspaceId) : undefined;
  const isGovernedCommand = state.runType === "command";

  // Self-fetched (Review doesn't otherwise receive the ai_provider integrations
  // list) — the SAME resolution preview step-access.tsx's card renders, reused
  // here rather than re-derived so the two steps can't disagree.
  const [ai, setAi] = React.useState<IntegrationRow[]>([]);
  React.useEffect(() => {
    let alive = true;
    integrationsApi
      .list()
      .then((data) => {
        if (alive) setAi(data.ai);
      })
      .catch(() => {});
    return () => {
      alive = false;
    };
  }, []);
  const resolved = isGovernedCommand
    ? null
    : resolveModelAccess(state.agent, state.integrationId, primaryWorkspace, ai);
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
      {residentAccess && (
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
        {/* Was mislabeled "Repo" with a local dir leaking through as the
            synthetic run.repo="local:<basename>" wire label — the primary
            onboarded workspace is the truth: its own name/kind/source, or
            "Base image" for a container (an image, not a mount). */}
        {primaryWorkspace ? (
          primaryWorkspace.kind === "container" ? (
            <Summary
              label="Base image"
              value={<Mono className="text-foreground">{primaryWorkspace.source}</Mono>}
            />
          ) : (
            <Summary
              label="Workspace"
              value={
                <div>
                  <div className="text-foreground">{primaryWorkspace.name}</div>
                  <div className="text-[0.6875rem] text-muted-foreground">
                    {primaryWorkspace.kind === "repo" ? "repo" : "local dir"} · {primaryWorkspace.source}
                  </div>
                </div>
              }
            />
          )
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
            ) : resolved ? (
              <div className="flex flex-wrap items-center gap-1.5">
                <span className="text-foreground">{resolved.row.name}</span>
                <Mono className="text-muted-foreground">{resolved.row.typeLabel}</Mono>
              </div>
            ) : (
              <span className="text-warning">{RD.NONE_LINE}</span>
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
          value={firstUseLabel(inline_policy.first_use_approval)}
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
                          {w?.kind === "container" && " — image"}
                        </Mono>
                        {/* Surface scan status so a still-pending / errored workspace isn't
                            attached silently at the final gate. */}
                        {w && !isUsable(w.status) && (
                          <Chip tone={STATUS_TONE[w.status]} className="px-1.5 py-0 text-[0.625rem]">
                            {STATUS_LABEL[w.status]}
                          </Chip>
                        )}
                      </div>
                      {w && (
                        <div className="pl-0.5 text-[0.6875rem] text-muted-foreground">
                          Comes with: {comesWithLine(w)}
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
