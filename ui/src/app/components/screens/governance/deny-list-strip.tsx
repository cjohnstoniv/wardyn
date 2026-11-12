/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The deny lists: "Denied components" and "Denied model providers" hold ids, one per line, and the server refuses a
// name. The strip under each box lists what can be denied by name, so an admin adds an id with one click; the
// standalone profile's two rows are built here too (the composed rows are profile-overlay.tsx's).
//
// Both reads are operator-only (GET /components, GET /model-providers), so anyone else gets the box alone and the
// read is not even made. A read that is pending or has failed, and an empty list, draw nothing.
import * as React from "react";
import { Check, Plus } from "lucide-react";
import { components } from "../../../lib/api/components";
import { modelProviders } from "../../../lib/api/model-providers";
import { DENY_LISTS } from "../../wardyn/copy/deny-lists";
import { Field } from "../../wardyn/form-primitives";
import { useOperator, useOperatorResolved } from "../../wardyn/operator-context";
import { BOX_CLASS, modelLines } from "./allowed-models";

/** The limits keys these rows write. */
export const DENY_KEYS: string[] = ["deny_components", "deny_model_providers"];

type DenyList = "components" | "providers";
interface Entry {
  id: string;
  name: string;
}

function useEntries(list: DenyList): Entry[] | null {
  // Resolved too: useOperator() reads true until /me answers, and both reads are operator-only.
  const isOperator = useOperator();
  const resolved = useOperatorResolved();
  const operator = isOperator && resolved;
  const [entries, setEntries] = React.useState<Entry[] | null>(null);
  React.useEffect(() => {
    if (!operator) return;
    let live = true;
    const read: Promise<Entry[]> =
      list === "components"
        ? components.list().then((cs) => cs.map((c) => ({ id: c.id, name: c.name || c.id })))
        : modelProviders
            .getModelProviders()
            .then((l) => (l.providers.providers ?? []).flatMap((p) => (p.uid ? [{ id: p.uid, name: p.name || p.uid }] : [])));
    // Best-effort: a refused or failed read leaves the box alone.
    read.then((es) => live && setEntries(es)).catch(() => {});
    return () => {
      live = false;
    };
  }, [operator, list]);
  return entries;
}

/** One small button per name, under the box. An id the box already holds shows Check and cannot be added again. */
export function DenyListStrip({ list, listed, disabled, onAdd }: { list: DenyList; listed: string[]; disabled: boolean; onAdd: (id: string) => void }) {
  const entries = useEntries(list);
  if (!entries || entries.length === 0) return null;
  return (
    <div className="mt-3 max-w-[34rem] rounded-md border border-dashed border-border px-3 py-2.5" data-testid={`governance-deny-${list}-strip`}>
      <p className="text-body font-medium text-foreground">{list === "components" ? DENY_LISTS.STRIP_COMPONENTS : DENY_LISTS.STRIP_PROVIDERS}</p>
      <div className="mt-2 flex flex-wrap items-center gap-x-2 gap-y-1.5">
        {entries.map((e) => {
          const has = listed.includes(e.id);
          return (
            <button
              key={e.id}
              type="button"
              aria-label={DENY_LISTS.ADD(e.name)}
              disabled={disabled || has}
              onClick={() => onAdd(e.id)}
              className="inline-flex min-w-0 max-w-full items-center gap-1.5 rounded-md border border-border bg-surface-2 px-2 py-1 text-xs enabled:hover:bg-accent disabled:opacity-70"
            >
              {has ? <Check className="size-3 shrink-0" aria-hidden /> : <Plus className="size-3 shrink-0" aria-hidden />}
              <span className="min-w-0 break-all font-mono">{e.name}</span>
              <span className="shrink-0 font-mono text-meta text-muted-foreground">{e.id.slice(0, 8)}</span>
            </button>
          );
        })}
      </div>
    </div>
  );
}

/** A standalone profile's deny row: the box, then the strip. An empty box sends no key (the caller drops it). */
export function DenyListRow({ which, value, disabled, onChange }: { which: DenyList; value: string[]; disabled: boolean; onChange: (ids: string[]) => void }) {
  const [text, setText] = React.useState(() => value.join("\n"));
  const listed = modelLines(text);
  const components = which === "components";
  const id = `governance-limit-deny-${which}`;
  const set = (next: string) => {
    setText(next);
    onChange(modelLines(next));
  };
  const add = (m: string) => set(text.trim() === "" ? m : `${text.replace(/\s+$/, "")}\n${m}`);
  const label = components ? DENY_LISTS.COMPONENTS_LABEL : DENY_LISTS.PROVIDERS_LABEL;
  return (
    <div className="mt-3 border-t border-border pt-3 first-of-type:border-t-0" data-testid={id}>
      <Field label={label} htmlFor={`${id}-value`} hint={components ? DENY_LISTS.COMPONENTS_HINT : DENY_LISTS.PROVIDERS_HINT} className="max-w-[34rem]">
        <textarea
          id={`${id}-value`}
          rows={Math.min(8, Math.max(3, listed.length + 1))}
          className={BOX_CLASS}
          placeholder={DENY_LISTS.PLACEHOLDER}
          disabled={disabled}
          value={text}
          onChange={(e) => set(e.target.value)}
        />
      </Field>
      <DenyListStrip list={which} listed={listed} disabled={disabled} onAdd={add} />
    </div>
  );
}
