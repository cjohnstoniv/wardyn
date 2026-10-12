/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { Chip } from "../../../../wardyn/primitives";
import { NR_SHAPE } from "../../../../wardyn/copy/new-run/shape";
import { NR_TEMPLATES } from "../../../../wardyn/copy/new-run/templates";
import { Label } from "../../../../ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "../../../../ui/select";
import type { TemplateSummary } from "../../../../../lib/types/templates";
import { templateSource } from "./shape-sources";
import { useLoaded } from "./use-loaded";

const scopeLabel = (t: TemplateSummary): string =>
  t.scope === "person" ? NR_TEMPLATES.SCOPE_ME : t.scope === "org" ? NR_TEMPLATES.SCOPE_ORG : NR_TEMPLATES.GROUP_LABEL(t.group_label ?? t.group_id ?? "");

const list = (signal: AbortSignal) => templateSource.list(signal);

/** Both kinds, a policy marked as one. Choosing a row opens Review on that revision. */
export function TemplatePicker({ selectedId, onPick }: { selectedId?: string; onPick: (template: TemplateSummary) => void }) {
  const loaded = useLoaded(list);
  if (loaded.status === "error") return <p role="alert" className="text-sm text-danger">{loaded.message}</p>;
  if (loaded.status === "ready" && loaded.value.templates.length === 0) {
    return <p className="text-sm text-muted-foreground">{NR_SHAPE.EMPTY_TEMPLATES}</p>;
  }
  const templates = loaded.status === "ready" ? loaded.value.templates : [];
  return (
    <div className="flex flex-col gap-2">
      <Label htmlFor="nr-template">{NR_SHAPE.TEMPLATE_LABEL}</Label>
      <Select
        value={selectedId ?? ""}
        disabled={loaded.status === "loading"}
        onValueChange={(id) => {
          const picked = templates.find((t) => t.id === id);
          if (picked) onPick(picked);
        }}
      >
        <SelectTrigger id="nr-template" aria-busy={loaded.status === "loading"} className="w-full">
          <SelectValue />
        </SelectTrigger>
        <SelectContent>
          {templates.map((t) => (
            <SelectItem key={t.id} value={t.id}>
              {NR_SHAPE.TEMPLATE_OPTION(t.name, scopeLabel(t))}
              {t.kind === "policy" && <Chip>{NR_TEMPLATES.POLICY_BADGE}</Chip>}
            </SelectItem>
          ))}
        </SelectContent>
      </Select>
    </div>
  );
}
