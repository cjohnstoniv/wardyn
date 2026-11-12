/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { ChevronDown, ChevronRight } from "lucide-react";
import { LOCAL_MODEL_EDITOR } from "../../../lib/local-model-copy";
import { PROVIDER_EDITOR } from "../../../lib/model-providers-copy";
import { Checkbox } from "../../ui/checkbox";
import { Input } from "../../ui/input";
import { Field } from "../../wardyn/form-primitives";
import {
  incompatibleReason,
  isBedrock,
  isEndpoint,
  isLocal,
  refusedHarness,
  type HarnessRow,
  type ProviderDraft,
} from "./model-provider-draft";

// One agent under "Use with": a checkbox, and under a tick the agent's Model and
// (endpoint kind) Path. Collapsed unless a required field is empty (QB-2); an
// agent the kind can't drive is disabled with the catalog's reason, never hidden.
export function HarnessBlock({
  harness,
  draft,
  refused,
  onChange,
  modelPlaceholder,
  modelFieldHint,
}: {
  harness: HarnessRow;
  draft: ProviderDraft;
  refused: string | null;
  onChange: (patch: Partial<ProviderDraft["harnesses"][string]>) => void;
  // A preset's own placeholder and hint for the Model field; each falls back to the usual when absent.
  modelPlaceholder?: string;
  modelFieldHint?: string;
}) {
  const row = draft.harnesses[harness.id];
  const endpoint = isEndpoint(draft.kind);
  const bedrock = isBedrock(draft.kind);
  const reason = incompatibleReason(draft.kind, harness.id);
  const needsPath = endpoint && row.ticked && !row.path.trim();
  // Model is required on a Bedrock provider (mp400ModelNeed) — E3's colfoot:
  // "Model is required for Bedrock, so that harness's settings are open until
  // it is filled" (QB-2), the same rule already applied to an endpoint's Path.
  const needsModel = bedrock && row.ticked && !row.model.trim();
  const local = isLocal(draft.kind);
  // A local model's Model is a pick from the list, so it is always shown.
  const [open, setOpen] = React.useState(needsPath || needsModel || local);
  React.useEffect(() => {
    if (needsPath || needsModel) setOpen(true);
  }, [needsPath, needsModel]);
  const expanded = row.ticked && open;
  const base = `mp-h-${harness.id}`;
  const pathHint = harness.id === "codex-cli" ? PROVIDER_EDITOR.PATH_HINT_CODEX : PROVIDER_EDITOR.PATH_HINT_CLAUDE;
  const modelHint = bedrock ? PROVIDER_EDITOR.MODEL_HINT_BEDROCK : PROVIDER_EDITOR.MODEL_HINT;
  // The pick shown is the one a save sends (providerFrom). A pick no longer in the list stays shown,
  // so the server's refusal of it points at what is on screen.
  const picked = row.model || draft.models[0] || "";
  const models = picked && !draft.models.includes(picked) ? [...draft.models, picked] : draft.models;

  return (
    <div className="rounded-lg border border-border">
      <div className="flex items-start gap-2 px-3 py-2">
        <Checkbox
          id={base}
          className="mt-0.5"
          checked={row.ticked}
          disabled={!!reason}
          onCheckedChange={(v) => onChange({ ticked: v === true })}
        />
        <div className={reason ? "min-w-0 flex-1 opacity-60" : "min-w-0 flex-1"}>
          <label htmlFor={base} className="text-body font-normal">
            {harness.display}
          </label>
          {reason && <p className="mt-0.5 text-xs text-muted-foreground">{reason}</p>}
        </div>
        {row.ticked && (
          <button
            type="button"
            aria-label={harness.display}
            aria-expanded={expanded}
            aria-controls={`${base}-body`}
            // A required, empty Path or Model stays open: collapsing would
            // unmount the input its `required` check lives on.
            disabled={needsPath || needsModel}
            onClick={() => setOpen(!expanded)}
            className="text-muted-foreground disabled:opacity-50"
          >
            {expanded ? <ChevronDown className="size-4" /> : <ChevronRight className="size-4" />}
          </button>
        )}
      </div>
      {expanded && (
        <div id={`${base}-body`} className={endpoint ? "grid gap-4 border-t border-border p-3 sm:grid-cols-2" : "border-t border-border p-3"}>
          {local ? (
            <Field
              label={PROVIDER_EDITOR.MODEL}
              htmlFor={`${base}-model`}
              hint={harness.id === "claude-code" ? LOCAL_MODEL_EDITOR.MODEL_HINT_CLAUDE : LOCAL_MODEL_EDITOR.MODEL_HINT}
            >
              <select
                id={`${base}-model`}
                aria-invalid={refusedHarness(refused) === harness.id || undefined}
                className="border-input bg-input-background aria-invalid:border-destructive h-9 w-full rounded-md border px-3 font-mono text-sm outline-none focus-visible:border-ring focus-visible:ring-[3px] focus-visible:ring-ring"
                value={picked}
                onChange={(e) => onChange({ model: e.target.value })}
              >
                {models.map((m) => (
                  <option key={m} value={m}>
                    {m}
                  </option>
                ))}
              </select>
            </Field>
          ) : (
            <Field label={PROVIDER_EDITOR.MODEL} htmlFor={`${base}-model`} hint={modelFieldHint ?? modelHint} required={bedrock}>
              <Input
                id={`${base}-model`}
                required={bedrock}
                className="font-mono"
                placeholder={modelPlaceholder}
                value={row.model}
                onChange={(e) => onChange({ model: e.target.value })}
              />
            </Field>
          )}
          {endpoint && (
            <Field label={PROVIDER_EDITOR.PATH} htmlFor={`${base}-path`} hint={pathHint} required>
              <Input
                id={`${base}-path`}
                required
                className="font-mono"
                placeholder="/"
                value={row.path}
                onChange={(e) => onChange({ path: e.target.value })}
              />
            </Field>
          )}
        </div>
      )}
    </div>
  );
}
