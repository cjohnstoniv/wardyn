/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// M9's "Models runs may use": a local model server's allowlist, entered as chips.
// Enter, a comma or a paste of several names adds them; the count is out of the
// most the API takes. Nothing is checked here beyond trimming and dropping a
// repeat: a name the server refuses comes back in the save refusal, and the
// chip it names is marked.
import * as React from "react";
import { X } from "lucide-react";
import { LOCAL_MODEL_EDITOR as T } from "../../../lib/local-model-copy";
import { Button } from "../../ui/button";
import { Input } from "../../ui/input";
import { cn } from "../../ui/utils";

// Names split on commas and any whitespace: a model name has neither.
const split = (text: string, commas = true) => text.split(commas ? /[\s,]+/ : /\s+/).filter(Boolean);

// The words a chip list wears. Models' own are the default; a different list (the sign-in scopes) passes its own.
export interface ChipCopy {
  none: string;
  placeholder: string;
  add: string;
  /** The count beside Add; absent shows none. */
  count?: (n: number) => string;
  hint: string;
  remove: (name: string) => string;
}
const MODEL_COPY: ChipCopy = {
  none: T.MODELS_NONE,
  placeholder: T.MODELS_PLACEHOLDER,
  add: T.MODELS_ADD,
  count: T.MODELS_COUNT,
  hint: T.MODELS_HINT,
  remove: T.MODEL_REMOVE,
};

export function ModelChips({
  id,
  value,
  onChange,
  isBad,
  invalid,
  copy = MODEL_COPY,
  commas = true,
}: {
  id: string;
  value: string[];
  onChange: (next: string[]) => void;
  /** A chip the last refused save named. */
  isBad: (name: string) => boolean;
  /** The refusal is about the list itself (empty, or over the most the API takes). */
  invalid?: boolean;
  copy?: ChipCopy;
  /** Whether a comma ends a name. A model name has none; a sign-in scope may (`read,write` is one scope). */
  commas?: boolean;
}) {
  const [text, setText] = React.useState("");
  const add = (names: string[]) => {
    const next = [...value];
    for (const n of names) if (!next.includes(n)) next.push(n);
    if (next.length !== value.length) onChange(next);
    setText("");
  };
  const commit = () => add(split(text, commas));

  return (
    <div
      data-invalid={invalid || undefined}
      className={cn("space-y-2", invalid && "rounded-lg border border-danger p-2")}
    >
      {value.length === 0 ? (
        <p className="text-body text-muted-foreground">{copy.none}</p>
      ) : (
        <ul className="flex flex-wrap gap-1.5">
          {value.map((name) => (
            <li
              key={name}
              data-bad={isBad(name) || undefined}
              className={cn(
                "inline-flex h-7 items-center gap-1.5 rounded-full border border-border-strong bg-muted pl-2.5 pr-1.5 font-mono text-xs",
                isBad(name) && "border-danger bg-danger-subtle text-danger",
              )}
            >
              {name}
              <button
                type="button"
                aria-label={copy.remove(name)}
                className="text-muted-foreground hover:text-foreground"
                onClick={() => onChange(value.filter((n) => n !== name))}
              >
                <X className="size-3" aria-hidden="true" />
              </button>
            </li>
          ))}
        </ul>
      )}
      <div className="flex items-center gap-2">
        <Input
          id={id}
          aria-invalid={invalid || undefined}
          className="h-8 flex-1 font-mono"
          placeholder={copy.placeholder}
          value={text}
          onChange={(e) => {
            // A comma ends a name as Enter does; a paste of several lands whole.
            if ((commas ? /[\s,]/ : /\s/).test(e.target.value.slice(-1)) && e.target.value.trim()) add(split(e.target.value, commas));
            else setText(e.target.value);
          }}
          onPaste={(e) => {
            e.preventDefault();
            add(split(text + e.clipboardData.getData("text"), commas));
          }}
          onKeyDown={(e) => {
            if (e.key === "Enter") {
              e.preventDefault();
              commit();
            }
          }}
          onBlur={commit}
        />
        <Button type="button" variant="outline" size="sm" onClick={commit}>
          {copy.add}
        </Button>
        {copy.count && <span className="whitespace-nowrap text-xs text-muted-foreground">{copy.count(value.length)}</span>}
      </div>
      <p className="text-xs leading-snug text-muted-foreground">{copy.hint}</p>
    </div>
  );
}
