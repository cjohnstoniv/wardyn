/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { Loader2 } from "lucide-react";
import { toast } from "sonner";
import { workspaces as api } from "../../lib/api/workspaces";
import { secrets as secretsApi } from "../../lib/api/secrets";
import { getErrorMessage } from "../../lib/format";
import type { Workspace, WorkspaceLLMCred, WorkspaceLLMCredMode } from "../../lib/types";
import { Button } from "../ui/button";
import { Input } from "../ui/input";
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

// Human label for the current model/harness binding (list/detail display).
// "" (or absent) => no binding; the run falls back to the global provider
// config, matching the backend's Mode="" semantics.
export function llmCredLabel(cred?: WorkspaceLLMCred): string {
  switch (cred?.mode) {
    case "managed":
      return "Managed subscription";
    case "api_key":
      return cred.api_key_secret ? `API key: ${cred.api_key_secret}` : "API key";
    case "bedrock": {
      const bits = [cred.bedrock?.region, cred.bedrock?.model].filter(Boolean);
      return bits.length ? `Bedrock: ${bits.join("/")}` : "Bedrock";
    }
    default:
      return "None";
  }
}
// success = injected proxy-side (managed/api_key), warning = resident at run
// time (bedrock's static/SSO creds) — mirrors the setup screen's residency
// framing (llm-access.tsx PROXY_INJECTED_CHIP / BEDROCK_RESIDENT_CHIP).
export function llmCredTone(mode: WorkspaceLLMCredMode | undefined): "neutral" | "success" | "warning" {
  if (mode === "managed" || mode === "api_key") return "success";
  if (mode === "bedrock") return "warning";
  return "neutral";
}

const CRED_MODES: { value: WorkspaceLLMCredMode | "none"; label: string }[] = [
  { value: "none", label: "None" },
  { value: "managed", label: "Managed (Wardyn subscription)" },
  { value: "api_key", label: "API key" },
  { value: "bedrock", label: "Bedrock" },
];

// The mode picker + conditional fields (secret name / bedrock region-model-
// profile) shared by the onboarding form (create) and WorkspaceLLMCredDialog
// (edit an existing workspace/container). Uncontrolled data lives in the
// caller — this just renders `value` and reports edits via `onChange`.
export function LLMCredFields({
  value,
  onChange,
}: {
  value: WorkspaceLLMCred;
  onChange: (next: WorkspaceLLMCred) => void;
}) {
  const secretListId = React.useId();
  const [secretNames, setSecretNames] = React.useState<string[] | null>(null);
  React.useEffect(() => {
    if (value.mode !== "api_key" || secretNames !== null) return;
    secretsApi.listSecrets().then(setSecretNames).catch(() => setSecretNames([]));
  }, [value.mode, secretNames]);

  return (
    <div className="space-y-2.5 rounded-lg border border-border p-3">
      <Label>Model / harness for this environment</Label>
      <RadioGroup
        value={value.mode || "none"}
        onValueChange={(v) => onChange({ mode: v === "none" ? "" : (v as WorkspaceLLMCredMode) })}
        className="flex flex-wrap gap-x-4 gap-y-1.5"
      >
        {CRED_MODES.map((m) => (
          <label key={m.value} className="flex items-center gap-1.5 text-xs">
            <RadioGroupItem value={m.value} id={`cred-mode-${m.value}`} />
            <Label htmlFor={`cred-mode-${m.value}`} className="cursor-pointer font-normal">
              {m.label}
            </Label>
          </label>
        ))}
      </RadioGroup>

      {value.mode === "api_key" && (
        <>
          <Input
            list={secretListId}
            placeholder="anthropic-api-key"
            value={value.api_key_secret ?? ""}
            onChange={(e) => onChange({ ...value, api_key_secret: e.target.value })}
            className="font-mono"
            autoComplete="off"
          />
          <datalist id={secretListId}>
            {(secretNames ?? []).map((n) => (
              <option key={n} value={n} />
            ))}
          </datalist>
        </>
      )}

      {value.mode === "bedrock" && (
        <>
        <div className="grid grid-cols-3 gap-2">
          <Input
            placeholder="Region"
            value={value.bedrock?.region ?? ""}
            onChange={(e) => onChange({ ...value, bedrock: { ...value.bedrock, region: e.target.value } })}
            className="font-mono"
            autoComplete="off"
          />
          <Input
            placeholder="Model"
            value={value.bedrock?.model ?? ""}
            onChange={(e) => onChange({ ...value, bedrock: { ...value.bedrock, model: e.target.value } })}
            className="font-mono"
            autoComplete="off"
          />
          <Input
            placeholder="AWS profile"
            value={value.bedrock?.aws_profile ?? ""}
            onChange={(e) => onChange({ ...value, bedrock: { ...value.bedrock, aws_profile: e.target.value } })}
            className="font-mono"
            autoComplete="off"
          />
        </div>
        {/* Honest expectation: binding Bedrock here selects it as this environment's
            model path (dropping a competing API-key grant), but runs currently use
            the server's GLOBAL Bedrock region/model (WARDYN_BEDROCK_REGION/MODEL).
            Per-environment region/model/profile override is not yet applied at
            dispatch — the credential values come from the secret store / ~/.aws mount. */}
        <p className="text-xs leading-relaxed text-muted-foreground">
          Selects Bedrock for this environment. Region/model/profile are stored but
          not yet applied per-run — today runs use the server's global Bedrock
          configuration; the AWS credentials come from the secret store or ~/.aws mount.
        </p>
        </>
      )}

      <p className="text-[11px] leading-snug text-muted-foreground">
        A run that picks this workspace/container inherits this model access — injected proxy-side at
        launch, never resident.
      </p>
    </div>
  );
}

// Standalone editor for an EXISTING workspace's model/harness binding — the
// onboarding form's llm_cred is create-only (the server ignores it on a
// generic PUT /workspaces/{id}), so changing it post-create goes through
// api.setWorkspaceLLMCred instead. `workspace` null => closed. Exported for
// the same reason AddWorkspaceDialog is (direct test coverage / reuse).
export function WorkspaceLLMCredDialog({
  workspace,
  onOpenChange,
  onSaved,
}: {
  workspace: Workspace | null;
  onOpenChange: (o: boolean) => void;
  onSaved: (w: Workspace) => void;
}) {
  const [cred, setCred] = React.useState<WorkspaceLLMCred>({ mode: "" });
  const [saving, setSaving] = React.useState(false);

  React.useEffect(() => {
    if (workspace) setCred(workspace.llm_cred ?? { mode: "" });
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
            A run that picks this workspace/container inherits this model access, injected proxy-side —
            never resident in the sandbox.
          </DialogDescription>
        </DialogHeader>
        <LLMCredFields value={cred} onChange={setCred} />
        <DialogFooter>
          <Button variant="ghost" onClick={() => onOpenChange(false)}>
            Cancel
          </Button>
          <Button onClick={save} disabled={saving}>
            {saving && <Loader2 className="size-4 animate-spin" />}
            Save
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
