/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { NR_SHAPE } from "../../../../wardyn/copy/new-run/shape";
import { NR_TEMPLATES } from "../../../../wardyn/copy/new-run/templates";
import { Label } from "../../../../ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "../../../../ui/select";
import type { TemplateSummary } from "../../../../../lib/types/templates";
import { templateSource } from "./shape-sources";
import { INLINE_CHOICE, INLINE_LABEL, useShown } from "./inline-choice";
import { useLoaded } from "./use-loaded";

const scopeLabel = (t: TemplateSummary): string =>
  t.scope === "person" ? NR_TEMPLATES.SCOPE_ME : t.scope === "org" ? NR_TEMPLATES.SCOPE_ORG : NR_TEMPLATES.GROUP_LABEL(t.group_label ?? t.group_id ?? "");

const list = (signal: AbortSignal) => templateSource.list(signal);

/**
 * Both kinds, as plain options (Make App.tsx:1216-1228). The shown row is the one picked, else the
 * first; it is reported up so Continue knows where to go.
 */
export function TemplatePicker({ selectedId, onPick }: { selectedId?: string; onPick: (template: TemplateSummary) => void }) {
  const loaded = useLoaded(list);
  const templates = loaded.status === "ready" ? loaded.value.templates : [];
  const shown = useShown(templates, (t) => t.id === selectedId, onPick);
  if (loaded.status === "error") return <div className={INLINE_CHOICE}><p role="alert" className="text-sm text-danger">{loaded.message}</p></div>;
  if (loaded.status === "ready" && templates.length === 0) {
    return <div className={INLINE_CHOICE}><p className="text-sm text-muted-foreground">{NR_SHAPE.EMPTY_TEMPLATES}</p></div>;
  }
  return (
    <div className={INLINE_CHOICE}>
      <Label htmlFor="nr-template" className={INLINE_LABEL}>{NR_SHAPE.TEMPLATE_LABEL}</Label>
      <Select
        value={shown?.id ?? ""}
        disabled={loaded.status === "loading"}
        onValueChange={(id) => {
          const picked = templates.find((t) => t.id === id);
          if (picked) onPick(picked);
        }}
      >
        <SelectTrigger id="nr-template" aria-busy={loaded.status === "loading"} className="w-full bg-card dark:bg-card">
          <SelectValue />
        </SelectTrigger>
        <SelectContent>
          {templates.map((t) => (
            <SelectItem key={t.id} value={t.id}>{NR_SHAPE.TEMPLATE_OPTION(t.name, scopeLabel(t))}</SelectItem>
          ))}
        </SelectContent>
      </Select>
    </div>
  );
}
