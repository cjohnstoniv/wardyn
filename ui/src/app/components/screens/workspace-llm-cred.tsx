/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { Loader2 } from "lucide-react";
import { toast } from "sonner";
import { workspaces as api } from "../../lib/api/workspaces";
import { modelProviders as modelProvidersApi, type ModelProvider } from "../../lib/api/model-providers";
import { getErrorMessage } from "../../lib/format";
import type { Workspace, WorkspaceLLMCred } from "../../lib/types";
import { Button } from "../ui/button";
import { Label } from "../ui/label";
import { RadioGroup, RadioGroupItem } from "../ui/radio-group";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "../ui/dialog";
import { OPERATOR_ONLY_REASON } from "../wardyn/copy";
import { useOperator } from "../wardyn/operator-context";

// Human label for the current model-provider binding (list/detail display).
// The binding names a model provider by id; absent/"" => no binding, the run
// chooses its provider itself. Pass the fetched providers when you have them to
// show the provider's display name; without them the id itself is the honest
// label (it is what Model providers shows and audit logs carry).
export function llmCredLabel(cred?: WorkspaceLLMCred, providers?: ModelProvider[]): string {
  const ref = cred?.provider_ref;
  if (!ref) return "None";
  return providers?.find((p) => p.id === ref)?.name ?? ref;
}
// success = a named binding; neutral = no binding (the run chooses).
export function llmCredTone(cred?: WorkspaceLLMCred): "neutral" | "success" {
  return cred?.provider_ref ? "success" : "neutral";
}

// The binding picker: "None" + every model provider the server knows.
// Uncontrolled data lives in the caller — this renders `value` and reports
// edits via `onChange`. Providers are fetched once on mount; a fetch failure
// renders the None-only honest floor (the binding can still be cleared, never
// invented).
export function LLMCredFields({
  value,
  onChange,
}: {
  value: WorkspaceLLMCred;
  onChange: (next: WorkspaceLLMCred) => void;
}) {
  const [providers, setProviders] = React.useState<ModelProvider[] | null>(null);
  React.useEffect(() => {
    let live = true;
    modelProvidersApi
      .getModelProviders()
      .then((d) => live && setProviders(d.providers.providers ?? []))
      .catch(() => live && setProviders([]));
    return () => {
      live = false;
    };
  }, []);

  const ref = value.provider_ref ?? "";
  return (
    <div className="space-y-2.5 rounded-lg border border-border p-3">
      <Label>Model provider for this environment</Label>
      <RadioGroup
        value={ref || "none"}
        onValueChange={(v) => onChange({ provider_ref: v === "none" ? "" : v })}
        className="flex flex-col gap-1.5"
      >
        <label className="flex items-center gap-1.5 text-xs">
          <RadioGroupItem value="none" id="cred-ref-none" />
          <Label htmlFor="cred-ref-none" className="cursor-pointer font-normal">
            None — each run chooses its model provider
          </Label>
        </label>
        {(providers ?? []).map((p) => (
          <label key={p.id} className="flex items-center gap-1.5 text-xs">
            <RadioGroupItem value={p.id} id={`cred-ref-${p.id}`} />
            <Label htmlFor={`cred-ref-${p.id}`} className="cursor-pointer font-normal">
              {p.name || p.id} <span className="font-mono text-muted-foreground">{p.kind}</span>
            </Label>
          </label>
        ))}
        {/* A stored ref whose provider no longer lists: still selectable/clearable, named honestly. */}
        {ref && providers !== null && !providers.some((p) => p.id === ref) && (
          <label className="flex items-center gap-1.5 text-xs">
            <RadioGroupItem value={ref} id="cred-ref-current" />
            <Label htmlFor="cred-ref-current" className="cursor-pointer font-mono font-normal">
              {ref} (not in the Model providers list)
            </Label>
          </label>
        )}
      </RadioGroup>
      {providers === null && <p className="text-meta leading-snug text-muted-foreground">Loading model providers…</p>}
      <p className="text-meta leading-snug text-muted-foreground">
        A run that picks this workspace/container uses this model provider unless it chooses one itself — with
        its launcher&apos;s own credential for it.
      </p>
    </div>
  );
}

// Standalone editor for an EXISTING workspace's model-provider binding — the
// onboarding form's llm_cred is create-only (the server ignores it on a
// generic PUT /workspaces/{id}), so changing it post-create goes through
// api.setWorkspaceLLMCred instead. `workspace` null => closed. Exported for
// direct test coverage / reuse, the same reason other standalone dialogs in
// this codebase are (e.g. AddSecretDialog).
export function WorkspaceLLMCredDialog({
  workspace,
  onOpenChange,
  onSaved,
}: {
  workspace: Workspace | null;
  onOpenChange: (o: boolean) => void;
  onSaved: (w: Workspace) => void;
}) {
  // PUT /workspaces/{id}/llm-cred — operator-only (see http.go).
  const operator = useOperator();
  const [cred, setCred] = React.useState<WorkspaceLLMCred>({});
  const [saving, setSaving] = React.useState(false);

  React.useEffect(() => {
    if (workspace) setCred(workspace.llm_cred ?? {});
  }, [workspace]);

  const save = async () => {
    if (!workspace) return;
    setSaving(true);
    try {
      const updated = await api.setWorkspaceLLMCred(workspace.id, cred);
      onSaved(updated);
      toast.success(`Model access updated for "${workspace.name}"`);
    } catch (e) {
      toast.error("Failed to update model access", { description: getErrorMessage(e) });
    } finally {
      setSaving(false);
    }
  };

  return (
    <Dialog open={!!workspace} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>Model access — {workspace?.name}</DialogTitle>
          <DialogDescription>
            A run that picks this workspace/container uses this model provider unless it chooses one itself.
          </DialogDescription>
        </DialogHeader>
        <LLMCredFields value={cred} onChange={setCred} />
        {!operator && (
          <p id="llm-cred-operator-reason" className="text-xs font-medium text-warning">
            {OPERATOR_ONLY_REASON}
          </p>
        )}
        <DialogFooter>
          <Button variant="ghost" onClick={() => onOpenChange(false)}>
            Cancel
          </Button>
          <Button
            onClick={save}
            disabled={!operator || saving}
            aria-describedby={operator ? undefined : "llm-cred-operator-reason"}
          >
            {saving && <Loader2 className="size-4 animate-spin" />}
            Save
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
