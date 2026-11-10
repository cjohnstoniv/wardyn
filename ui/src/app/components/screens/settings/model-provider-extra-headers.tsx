/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { Plus, Trash2 } from "lucide-react";
import { PROVIDER_EDITOR } from "../../../lib/model-providers-copy";
import { Button } from "../../ui/button";
import { Input } from "../../ui/input";
import type { HeaderDraft } from "./model-provider-draft";

// The server's cap on extra headers (proxy.MaxExtraHeaders); the server's own refusal still stands past it.
const MAX_HEADERS = 8;

// The non-secret headers a gateway routes on (custom_endpoint). Names and values are only trimmed, on save:
// the server's answer is the one the admin reads. `valueHint` sits under the rows, for a row whose value a
// preset left for the admin to fill.
export function ExtraHeaders({
  rows,
  onChange,
  valueHint,
}: {
  rows: HeaderDraft[];
  onChange: (rows: HeaderDraft[]) => void;
  valueHint?: string;
}) {
  const edit = (i: number, patch: Partial<HeaderDraft>) => onChange(rows.map((r, j) => (j === i ? { ...r, ...patch } : r)));
  const full = rows.length >= MAX_HEADERS;
  return (
    <div role="group" aria-labelledby="mp-headers-label" className="space-y-2">
      <p id="mp-headers-label" className="text-sm font-medium">
        {PROVIDER_EDITOR.HEADERS_LABEL}
      </p>
      {rows.map((row, i) => (
        <div key={i} className="flex items-center gap-2">
          <Input
            className="min-w-0 flex-1 font-mono"
            placeholder={PROVIDER_EDITOR.HEADER_NAME_PLACEHOLDER}
            aria-label={`Header ${i + 1} name`}
            value={row.name}
            onChange={(e) => edit(i, { name: e.target.value })}
          />
          <Input
            className="min-w-0 flex-1 font-mono"
            placeholder={row.placeholder ?? PROVIDER_EDITOR.HEADER_VALUE_PLACEHOLDER}
            aria-label={`Header ${i + 1} value`}
            value={row.value}
            onChange={(e) => edit(i, { value: e.target.value })}
          />
          <Button
            type="button"
            variant="ghost"
            size="icon"
            aria-label={PROVIDER_EDITOR.HEADERS_REMOVE}
            onClick={() => onChange(rows.filter((_, j) => j !== i))}
          >
            <Trash2 className="size-4" aria-hidden="true" />
          </Button>
        </div>
      ))}
      {valueHint && <p className="text-xs leading-snug text-muted-foreground">{valueHint}</p>}
      <div className="flex items-center gap-2">
        <Button type="button" variant="outline" size="sm" disabled={full} onClick={() => onChange([...rows, { name: "", value: "" }])}>
          <Plus className="size-4" aria-hidden="true" />
          {PROVIDER_EDITOR.HEADERS_ADD}
        </Button>
        {full && <span className="text-xs text-muted-foreground">{PROVIDER_EDITOR.HEADERS_CAP}</span>}
      </div>
      <p className="text-xs leading-snug text-muted-foreground">{PROVIDER_EDITOR.HEADERS_HINT}</p>
    </div>
  );
}
