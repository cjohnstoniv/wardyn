/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The workspace detail page's durable Requirements editor. Model access is
// the FIRST group, followed by the SAME contract groups the wizard's
// Requirements step renders — reused
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
import { useOperator } from "../../wardyn/operator-context";
import { OPERATOR_ONLY_REASON } from "../../wardyn/copy";

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
      writable: s.writable ?? false,
    }));
  }
  // Pre-composition workspace (single legacy kind/source/ref) — synthesize the
  // one source StepRequirements needs to know about (its write-access group
  // keys off a local_dir's path).
  const kind = ws.kind;
  return [
    {
      id: "src-legacy",
      type: kind,
      path: kind === "local_dir" ? ws.source : "",
      source: kind === "repo" ? ws.source : "",
      ref: ws.ref ?? "",
      target: ws.default_target ?? "",
      // Workspace.writable isn't a real response field (the API never sends
      // it — see lib/types/workspaces.ts) — read the primary attachment's
      // own writability instead, the actual source of truth.
      writable: ws.attachments?.[0]?.writable ?? false,
    },
  ];
}

// Maps the workspace's REAL llm_cred binding onto the wizard's PowerSource
// shape — the one idiom StepRequirements' Record/Egress/Secrets tabs key off,
// on both surfaces (see step-requirements.tsx's `powerSource` prop comment).
// The binding names an Integration outright now, so the id maps 1:1 (the old
// broken-secret probe died with the inline api_key shape — the Integration
// owns its credential and the Integrations screen is where its health shows).
function resolvedPowerSource(ws: Workspace): PowerSource {
  const cred = ws.llm_cred;
  if (!cred?.integration_ref) return { kind: "default" };
  return { kind: "pinned", integrationId: cred.integration_ref, name: llmCredLabel(cred) };
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
  const operator = useOperator();
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

  const profile = (ws.profile ?? null) as WorkspaceProfile | null;
  const recipe = profile?.setup_commands ?? [];

  const [saving, setSaving] = React.useState(false);
  // Local, optimistic copy of the requirements map. PUT-per-toggle used to
  // derive `next` straight from the `ws` PROP, which only updates once the
  // PREVIOUS PUT resolves: two toggles fired inside one round trip both read
  // the same stale base, and the second's write silently discards the first
  // (PUT is a full replace). Composing on this local copy instead means each
  // toggle always builds on the one just applied, regardless of the server's
  // timing.
  const [pending, setPending] = React.useState<WorkspaceRequirementsMap>(() => requirementsOf(ws));
  // Reconciles `pending` with the server whenever `ws` changes out from under
  // this card. This card is NOT remounted on every out-of-band refresh — only
  // load(true)'s foreground gate tears the detail page's subtree down
  // (workspace-detail.tsx); the Edit-workspace overlay's onClose uses
  // load(false), which can NULL the requirements contract server-side
  // (sourcesChanged resets it — internal/api/workspaces.go) without
  // remounting this card. Skipped while a write of OUR OWN is in flight, so
  // an in-progress optimistic edit doesn't get clobbered by a `ws` snapshot
  // that predates it.
  React.useEffect(() => {
    if (!saving) setPending(requirementsOf(ws));
  }, [ws, saving]);
  const persist = async (next: WorkspaceRequirementsMap) => {
    setPending(next);
    setSaving(true);
    try {
      const updated = await workspacesApi.setRequirements(ws.id, next);
      setPending(requirementsOf(updated));
      onWorkspaceUpdated(updated);
    } catch (e) {
      // Roll back to the server's own last-known state — a rejected write
      // must not leave a phantom lane rendered (and composing onto every
      // later write) forever.
      setPending(requirementsOf(ws));
      toast.error("Failed to save requirements", { description: getErrorMessage(e) });
    } finally {
      setSaving(false);
    }
  };

  return (
    <SectionCard
      title="Requirements"
      subtitle="What this workspace carries into every run — and what a run has to ask for."
      right={
        saving ? (
          <span className="text-[0.6875rem] text-muted-foreground">Saving…</span>
        ) : !operator ? (
          <Chip tone="neutral">{OPERATOR_ONLY_REASON}</Chip>
        ) : undefined
      }
    >
      {/* Every control below (model-access bind, lane toggles, holding-block
          approve) is operatorOnly server-side. A native disabled fieldset
          gates the whole subtree at once, same as the rest of the console's
          Buttons already render when disabled — `contents` keeps it out of
          the box model since there's no existing wrapper div to repurpose
          here. `space-y-4` is repeated on it: SectionCard's own space-y-4 is a
          direct-child selector, and `contents` makes this fieldset's CHILDREN
          (not the fieldset itself) the effective direct children once the
          fieldset drops out of the box tree — the rule has to be on the
          element whose children it should space. */}
      <fieldset disabled={!operator} className="contents space-y-4">
        <ModelAccessGroup ws={ws} onSaved={onWorkspaceUpdated} />

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
          requirements={pending}
          onChange={(next) => void persist(next)}
          storedSecretNames={storedSecretNames}
          onSecretStored={onSecretStored}
          powerSource={resolvedPowerSource(ws)}
          status={setupStatus}
        />
      </fieldset>
    </SectionCard>
  );
}

// Model access — the FIRST group in the Requirements card. Reuses the
// existing, already-wired WorkspaceLLMCredDialog (PUT /workspaces/{id}/llm-cred)
// rather than the wizard's not-yet-wired PowerSource concept (wizard-types.ts's
// own header comment: "deliberately NOT wired to a real llm_cred write yet").
function ModelAccessGroup({
  ws,
  onSaved,
}: {
  ws: Workspace;
  onSaved: (w: Workspace) => void;
}) {
  const [open, setOpen] = React.useState(false);
  const cred = ws.llm_cred;
  const bound = !!cred?.integration_ref;

  return (
    <div className="space-y-2">
      <div className="flex flex-wrap items-center gap-2">
        <h4 className="text-[0.8125rem] font-semibold text-foreground">Model access</h4>
        <Chip tone={llmCredTone(cred)} mono={bound}>
          {llmCredLabel(cred)}
        </Chip>
        <span className="ml-auto" />
        <Button size="sm" variant="outline" className="h-7" onClick={() => setOpen(true)}>
          {bound ? "Change…" : "Bind model access"}
        </Button>
      </div>
      {!bound ? (
        <p className="text-[0.6875rem] leading-snug text-muted-foreground">
          Nothing bound — runs that pick this workspace use the server&apos;s global model provider. Bind
          one when this environment must use a different key, account, or Bedrock model.
        </p>
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
