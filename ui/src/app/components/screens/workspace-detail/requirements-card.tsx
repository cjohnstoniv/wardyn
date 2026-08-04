/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The workspace detail page's durable Requirements editor. Model access is
// the FIRST group (a container's only configurable thing), followed by the
// SAME contract groups the wizard's Requirements step renders — reused
// directly from workspace-wizard/step-requirements.tsx (StepRequirements) so
// the two surfaces can never disagree about the copy or the lane control.
// Unlike the wizard (which stages edits until "Accept & finish"), every lane
// toggle here PERSISTS immediately — this is the durable editor, not a wizard
// step pending confirmation.
import * as React from "react";
import { toast } from "sonner";
import { Button } from "../../ui/button";
import { Chip } from "../../wardyn/primitives";
import { Mono } from "../../wardyn/code-block";
import { getErrorMessage } from "../../../lib/format";
import { workspaces as workspacesApi } from "../../../lib/api/workspaces";
import type { WorkspaceRequirementsMap, WorkspaceSourceInput } from "../../../lib/api/workspaces";
import type { SetupStatus, Workspace, WorkspaceProfile } from "../../../lib/types";
import { setup as setupApi } from "../../../lib/api/setup";
import { C } from "../../../lib/workspace-copy";
import { StepRequirements } from "../workspace-wizard/step-requirements";
import type { PowerSource, SourceRow } from "../workspace-wizard/wizard-types";
import { WorkspaceLLMCredDialog, llmCredLabel, llmCredTone } from "../workspace-llm-cred";
import { SectionCard } from "./section-card";

// Boundary-local typed cast-read for the composition-model fields the shared
// Workspace type doesn't carry yet — same idiom lib/api/workspaces.ts and
// new-run/wizard-types.ts each already use at their own boundary, rather than
// importing workspace-wizard's WorkspaceWithComposition (a different owner's
// internal type) into this file.
function requirementsOf(ws: Workspace): WorkspaceRequirementsMap {
  return (ws as unknown as { requirements?: WorkspaceRequirementsMap }).requirements ?? {};
}
function sourcesOf(ws: Workspace): SourceRow[] {
  const raw = (ws as unknown as { sources?: WorkspaceSourceInput[] }).sources ?? [];
  if (raw.length > 0) {
    return raw.map((s, i) => ({
      id: `src-${i}`,
      type: s.type,
      path: s.path ?? "",
      source: s.source ?? "",
      ref: s.ref ?? "",
      target: s.target ?? "",
    }));
  }
  // Pre-composition workspace (single legacy kind/source/ref) — synthesize the
  // one source StepRequirements needs to know about (its write-access group
  // keys off a local_dir's path).
  const kind = ws.kind;
  if (kind === "container") return [];
  return [
    {
      id: "src-legacy",
      type: kind,
      path: kind === "local_dir" ? ws.source : "",
      source: kind === "repo" ? ws.source : "",
      ref: ws.ref ?? "",
      target: ws.default_target ?? "",
    },
  ];
}

// Maps the workspace's REAL llm_cred binding onto the wizard's PowerSource
// shape — the one idiom StepRequirements' Record/Egress/Secrets tabs key off,
// on both surfaces (see step-requirements.tsx's `powerSource` prop comment).
// Unlike the wizard (which can't yet write a real llm_cred — wizard-types.ts's
// own comment), the detail page's ws.llm_cred IS the real, persisted binding,
// so "nothing resolves" here means something more concrete than the wizard's
// unreachable-today `{kind:"none"}`: a pinned api_key whose secret was since
// deleted from the store — the same `broken` condition ModelAccessGroup below
// already flags.
function resolvedPowerSource(ws: Workspace, storedSecretNames: string[]): PowerSource {
  const cred = ws.llm_cred;
  if (!cred?.mode) return { kind: "default" };
  const broken = cred.mode === "api_key" && !!cred.api_key_secret && !storedSecretNames.includes(cred.api_key_secret);
  if (broken) return { kind: "none" };
  return { kind: "pinned", integrationId: cred.mode, name: llmCredLabel(cred) };
}

export function RequirementsCard({
  ws,
  storedSecretNames,
  onWorkspaceUpdated,
  onSecretStored,
}: {
  ws: Workspace;
  storedSecretNames: string[];
  onWorkspaceUpdated: (w: Workspace) => void;
  onSecretStored: (name: string) => void;
}) {
  // The integrations a workspace may name (StepRequirements renders the
  // section). Fetched here rather than threaded from the page: this card is the
  // only consumer, and a failed fetch simply means the section doesn't render.
  const [setupStatus, setSetupStatus] = React.useState<SetupStatus | null>(null);
  React.useEffect(() => {
    let live = true;
    setupApi
      .getSetupStatus()
      .then((st) => live && setSetupStatus(st))
      .catch(() => {});
    return () => {
      live = false;
    };
  }, []);

  const isContainer = ws.kind === "container";
  const profile = (ws.profile ?? null) as WorkspaceProfile | null;
  const recipe = profile?.setup_commands ?? [];

  const [saving, setSaving] = React.useState(false);
  const persist = async (next: WorkspaceRequirementsMap) => {
    setSaving(true);
    try {
      onWorkspaceUpdated(await workspacesApi.setRequirements(ws.id, next));
    } catch (e) {
      toast.error("Failed to save requirements", { description: getErrorMessage(e) });
    } finally {
      setSaving(false);
    }
  };

  return (
    <SectionCard
      title="Requirements"
      subtitle={isContainer ? undefined : "What this workspace carries into every run — and what a run has to ask for."}
      right={saving ? <span className="text-[0.6875rem] text-muted-foreground">Saving…</span> : undefined}
    >
      <ModelAccessGroup ws={ws} storedSecretNames={storedSecretNames} onSaved={onWorkspaceUpdated} />

      {isContainer ? (
        <p className="text-[0.6875rem] leading-snug text-muted-foreground">
          {C.IMAGE_ENV} Its model access is the one thing you configure here.
        </p>
      ) : (
        <>
          {recipe.length > 0 && (
            <p className="text-[0.6875rem] leading-snug text-muted-foreground">
              Detected build recipe:{" "}
              {recipe.map((c, i) => (
                <React.Fragment key={`${c.command}-${i}`}>
                  <Mono className="text-[0.6875rem] text-foreground">{c.command}</Mono>
                  {i < recipe.length - 1 ? " · " : ""}
                </React.Fragment>
              ))}{" "}
              — written into AGENTS.md; Wardyn never runs these.
            </p>
          )}
          <StepRequirements
            profile={profile}
            sources={sourcesOf(ws)}
            requirements={requirementsOf(ws)}
            onChange={(next) => void persist(next)}
            storedSecretNames={storedSecretNames}
            onSecretStored={onSecretStored}
            powerSource={resolvedPowerSource(ws, storedSecretNames)}
            status={setupStatus}
          />
        </>
      )}
    </SectionCard>
  );
}

// Model access — the FIRST group in the Requirements card (and, for a
// container, the only configurable thing). Reuses the existing, already-wired
// WorkspaceLLMCredDialog (PUT /workspaces/{id}/llm-cred) rather than the
// wizard's not-yet-wired PowerSource concept (wizard-types.ts's own header
// comment: "deliberately NOT wired to a real llm_cred write yet").
function ModelAccessGroup({
  ws,
  storedSecretNames,
  onSaved,
}: {
  ws: Workspace;
  storedSecretNames: string[];
  onSaved: (w: Workspace) => void;
}) {
  const [open, setOpen] = React.useState(false);
  const cred = ws.llm_cred;
  const broken = cred?.mode === "api_key" && !!cred.api_key_secret && !storedSecretNames.includes(cred.api_key_secret);

  return (
    <div className="space-y-2">
      <div className="flex flex-wrap items-center gap-2">
        <h4 className="text-[0.8125rem] font-semibold text-foreground">Model access</h4>
        <Chip tone={llmCredTone(cred?.mode)} mono={cred?.mode === "api_key"}>
          {llmCredLabel(cred)}
        </Chip>
        <span className="ml-auto" />
        <Button size="sm" variant="outline" className="h-7" onClick={() => setOpen(true)}>
          {cred?.mode ? "Change…" : "Bind model access"}
        </Button>
      </div>
      {!cred?.mode ? (
        <p className="text-[0.6875rem] leading-snug text-muted-foreground">
          Nothing bound — runs that pick this workspace use the server&apos;s global model provider. Bind
          one when this environment must use a different key, account, or Bedrock model.
        </p>
      ) : broken ? (
        <div className="space-y-1.5 rounded-lg border border-warning/30 bg-warning-subtle p-2.5">
          <p className="text-[0.6875rem] leading-snug text-warning">
            The secret <span className="font-mono">{cred.api_key_secret}</span> isn&apos;t in the store. Runs
            that pick this workspace quietly fall back to the server&apos;s global provider — this binding
            does nothing right now.
          </p>
        </div>
      ) : (
        <p className="text-[0.6875rem] leading-snug text-muted-foreground">{C.MODEL_INJECT}</p>
      )}
      <WorkspaceLLMCredDialog
        workspace={open ? ws : null}
        onOpenChange={setOpen}
        onSaved={(w) => {
          onSaved(w);
          setOpen(false);
        }}
      />
    </div>
  );
}
