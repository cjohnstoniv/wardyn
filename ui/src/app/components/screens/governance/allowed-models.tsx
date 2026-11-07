/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The "Allowed models" limit: one model per line, written exactly as the provider names it. A blank box is no
// limit and sends nothing for the key; there is no deny-all value. The standalone editor draws the row here; the
// composed editor's row (profile-overlay.tsx) shares the hints and the info note.
//
// The helper strip lists what the model providers use, so an admin can add a name with one click. It reads
// GET /model-providers, which only an operator may call, so anyone else gets the hints alone and the read is not
// even made. The small model is never added for the admin.
import * as React from "react";
import { Check, Plus } from "lucide-react";
import { modelProviders } from "../../../lib/api/model-providers";
import { ALLOWED_MODELS as T } from "../../../lib/governance-copy";
import type { ModelProvider } from "../../../lib/types/site";
import { cn } from "../../ui/utils";
import { Field } from "../../wardyn/form-primitives";
import { useOperator, useOperatorResolved } from "../../wardyn/operator-context";
import { Note } from "./display";

export const ALLOWED_MODELS_KEY = "allowed_models";

/** The box's lines as a list: trimmed, blank lines dropped. */
export const modelLines = (text: string): string[] =>
  text
    .split("\n")
    .map((l) => l.trim())
    .filter(Boolean);

/** The box's hint, then the small-model hint under it. `first` is the row's own sentence. */
export function ModelHintText({ first }: { first: string }) {
  return (
    <>
      {first}
      <span className="mt-1 block">{T.SMALL_HINT}</span>
    </>
  );
}

/** Under the box while the list is not empty: what the proxy does to check it. */
export function ModelReadsNote() {
  return (
    <div className="mt-2 max-w-[34rem]" data-testid="governance-allowed-models-note">
      <Note tone="info">{T.READS_NOTE}</Note>
    </div>
  );
}

// ---- the helper strip -----------------------------------------------------

export interface ProviderModels {
  id: string;
  name: string;
  /** The models the admin fixed for a harness, once each. */
  models: string[];
  /** The small models, once each. */
  small: string[];
  /** Models named, but no small model: Claude Code asks for its own. */
  noSmall: boolean;
}

/** What each hosted provider uses. A local model keeps its own list, and a disabled provider is not offered. */
export function providerModels(providers: ModelProvider[]): ProviderModels[] {
  const once = (xs: (string | undefined)[]) => [...new Set(xs.filter((x): x is string => !!x))];
  return providers
    .filter((p) => !p.disabled && p.kind !== "local_model")
    .map((p) => {
      const models = once((p.harnesses ?? []).map((h) => h.model));
      const small = once((p.harnesses ?? []).map((h) => h.fast_model));
      // Only Claude Code asks for a small model, so only its harness can be missing one; on Azure Foundry an
      // unset small model is the model itself, so nothing is missing there.
      const noSmall =
        (p.kind as string) !== "azure_foundry" && (p.harnesses ?? []).some((h) => h.harness === "claude-code" && !!h.model && !h.fast_model);
      return { id: p.id, name: p.name || p.id, models, small, noSmall };
    });
}

function useProviderModels(): ProviderModels[] | null {
  // Resolved too: useOperator() reads true until /me answers, and the read is operator-only.
  const isOperator = useOperator();
  const resolved = useOperatorResolved();
  const operator = isOperator && resolved;
  const [list, setList] = React.useState<ProviderModels[] | null>(null);
  React.useEffect(() => {
    if (!operator) return;
    let live = true;
    // Best-effort: a refused or failed read leaves the hints alone.
    modelProviders
      .getModelProviders()
      .then((l) => live && setList(providerModels(l.providers.providers ?? [])))
      .catch(() => {});
    return () => {
      live = false;
    };
  }, [operator]);
  return list;
}

function ModelStrip({ providers, listed, disabled, onAdd }: { providers: ProviderModels[]; listed: string[]; disabled: boolean; onAdd: (m: string) => void }) {
  const button = (m: string, small: boolean) => {
    const has = listed.includes(m);
    return (
      <button
        key={`${m}:${small}`}
        type="button"
        aria-label={T.ADD(m)}
        disabled={disabled || has}
        onClick={() => onAdd(m)}
        className="inline-flex min-w-0 max-w-full items-center gap-1.5 rounded-md border border-border bg-surface-2 px-2 py-1 text-xs enabled:hover:bg-accent disabled:opacity-70"
      >
        {has ? <Check className="size-3 shrink-0" aria-hidden /> : <Plus className="size-3 shrink-0" aria-hidden />}
        <span className="min-w-0 break-all font-mono">{m}</span>
        {small && <span className="shrink-0 text-meta text-muted-foreground">{T.SMALL_TAG}</span>}
      </button>
    );
  };
  return (
    <div className="mt-3 max-w-[34rem] rounded-md border border-dashed border-border px-3 py-2.5" data-testid="governance-allowed-models-strip">
      <p className="text-body font-medium text-foreground">{T.STRIP_TITLE}</p>
      {providers.map((p) => (
        <div key={p.id} className="mt-2 space-y-1.5">
          <div className="flex flex-wrap items-center gap-x-2 gap-y-1.5">
            <span className="text-xs text-muted-foreground">{p.name}</span>
            {p.models.map((m) => button(m, false))}
            {p.small.map((m) => button(m, true))}
          </div>
          {p.models.length === 0 && p.small.length === 0 && <p className="text-xs text-muted-foreground">{T.NOT_HELD(p.name)}</p>}
          {p.noSmall && <p className="text-xs text-muted-foreground">{T.NO_SMALL(p.name)}</p>}
        </div>
      ))}
    </div>
  );
}

// ---- the standalone row ---------------------------------------------------

export const BOX_CLASS = "w-full max-w-[34rem] rounded-md border border-border bg-input-background px-3 py-2 font-mono text-xs";

export function AllowedModelsRow({ value, disabled, onChange }: { value: string[]; disabled: boolean; onChange: (models: string[]) => void }) {
  const [text, setText] = React.useState(() => value.join("\n"));
  const providers = useProviderModels();
  const listed = modelLines(text);
  const id = "governance-limit-allowed-models";
  const set = (next: string) => {
    setText(next);
    onChange(modelLines(next));
  };
  const add = (m: string) => set(text.trim() === "" ? m : `${text.replace(/\s+$/, "")}\n${m}`);
  return (
    <div className="mt-3 border-t border-border pt-3 first-of-type:border-t-0" data-testid={id}>
      <Field label={T.LABEL} htmlFor={`${id}-value`} hint={<ModelHintText first={T.HINT} />} className="max-w-[34rem]">
        <textarea
          id={`${id}-value`}
          rows={Math.min(8, Math.max(3, listed.length + 1))}
          className={BOX_CLASS}
          placeholder={T.PLACEHOLDER}
          disabled={disabled}
          value={text}
          onChange={(e) => set(e.target.value)}
        />
      </Field>
      {listed.length > 0 && <ModelReadsNote />}
      {providers && providers.length > 0 && <ModelStrip providers={providers} listed={listed} disabled={disabled} onAdd={add} />}
    </div>
  );
}

/** The helper strip under the composed row's box, for those who can read the model providers. */
export function ProviderModelStrip({ listed, disabled, onAdd }: { listed: string[]; disabled: boolean; onAdd: (m: string) => void }) {
  const providers = useProviderModels();
  return providers && providers.length > 0 ? <ModelStrip providers={providers} listed={listed} disabled={disabled} onAdd={onAdd} /> : null;
}

/** The composed row's error under the box: Narrow is on and nothing is listed. */
export function NarrowEmptyError({ id }: { id: string }) {
  return (
    <p id={id} role="alert" className={cn("mt-2 max-w-[34rem] rounded-md border border-danger/40 bg-danger/10 px-3 py-2 text-xs text-danger")}>
      {T.NARROW_EMPTY}
    </p>
  );
}
