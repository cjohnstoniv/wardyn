/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { TriangleAlert } from "lucide-react";
import { Link } from "react-router-dom";
import { absoluteTime, shortDate } from "../../../lib/format";
import type { TemplateSummary } from "../../../lib/types/templates";
import { Button } from "../../ui/button";
import { cn } from "../../ui/utils";
import { NR_RUN } from "../../wardyn/copy/new-run/run";
import { NR_TEMPLATES as T } from "../../wardyn/copy/new-run/templates";
import { InfoTip } from "../../wardyn/info-tip";
import { Chip } from "../../wardyn/primitives";
import { NEW_RUN_PATH } from "../new-run-flow/draft/new-run-url";
import { includedGroups, needsSetupLabel, ownerLine, scopeLabel, whyReadOnly } from "./template-display";

// The five columns of the list at md and up; below that the cells stack, each under its own label.
export const ROW_GRID = "md:grid-cols-[minmax(0,2.2fr)_minmax(0,1.2fr)_minmax(0,1.6fr)_minmax(0,1fr)_auto]";

const startHref = (t: TemplateSummary) =>
  `${NEW_RUN_PATH}?${new URLSearchParams({ template: t.id, rev: String(t.revision) })}`;

/** The cell label: visible at phone width, where the header row is hidden; read aloud from md up, where the header row is decoration. */
const CellLabel = ({ children }: { children: string }) => <span className="label-eyebrow mb-1 block md:sr-only">{children}</span>;

export function TemplateRow({
  t,
  canPublish,
  busy,
  onRename,
  onDelete,
  onPublish,
}: {
  t: TemplateSummary;
  /** The Admin view offers the organisation: copy a row that is not already the organisation's. */
  canPublish: boolean;
  busy: boolean;
  onRename: () => void;
  onDelete: () => void;
  onPublish: () => void;
}) {
  const nameId = React.useId();
  const whyId = React.useId();
  const needs = needsSetupLabel(t);
  const owner = ownerLine(t);
  const why = t.writable ? null : whyReadOnly(t);
  // aria-disabled, not disabled: the control stays focusable and describes itself by the reason. Sighted readers
  // open the same reason from the info tip beside the actions, on tap, click or keyboard (tooltips open on tap).
  const locked = !t.writable;
  const guard = (act: () => void) => (locked ? undefined : act);
  const lockedProps = locked
    ? { "aria-disabled": true as const, "aria-describedby": why ? `${nameId} ${whyId}` : nameId }
    : { "aria-describedby": nameId };
  const lockedClass = cn(locked && "aria-disabled:cursor-not-allowed aria-disabled:opacity-50 aria-disabled:hover:bg-transparent");

  return (
    <li className={`grid items-center gap-x-4 gap-y-3 border-t border-border px-4 py-3 first:border-t-0 ${ROW_GRID}`}>
      <div className="min-w-0 space-y-1">
        <div className="flex flex-wrap items-center gap-2">
          <strong id={nameId} className="font-medium text-foreground">
            {t.name}
          </strong>
          {t.kind === "policy" && <Chip tone="info">{T.POLICY_BADGE}</Chip>}
          {needs && (
            <Chip tone="warning">
              <TriangleAlert aria-hidden="true" className="size-3" />
              {needs}
            </Chip>
          )}
        </div>
        {t.description && <p className="text-sm text-muted-foreground [overflow-wrap:anywhere]">{t.description}</p>}
      </div>
      <div className="min-w-0 space-y-1">
        <CellLabel>{T.COL_AVAILABLE}</CellLabel>
        <Chip tone={t.scope === "person" ? "primary" : "neutral"}>{scopeLabel(t)}</Chip>
        {owner && <p className="text-xs text-muted-foreground">{owner}</p>}
      </div>
      <div className="min-w-0">
        <CellLabel>{T.COL_INCLUDES}</CellLabel>
        <div className="flex flex-wrap gap-1">
          {includedGroups(t).map((g) => (
            <Chip key={g}>{g}</Chip>
          ))}
        </div>
      </div>
      <div className="min-w-0 text-sm text-muted-foreground tabular-nums">
        <CellLabel>{T.COL_UPDATED}</CellLabel>
        <time dateTime={t.updated_at} title={absoluteTime(t.updated_at)}>
          {shortDate(t.updated_at)}
        </time>
        <p className="text-xs">{T.REVISION(t.revision)}</p>
      </div>
      <div className="space-y-1.5 md:justify-self-end">
        <div className="flex flex-wrap gap-1.5 md:justify-end">
          <Button asChild size="sm">
            <Link to={startHref(t)} aria-describedby={nameId}>
              {T.USE}
            </Link>
          </Button>
          <Button type="button" variant="ghost" size="sm" className={lockedClass} {...lockedProps} onClick={guard(onRename)}>
            {T.RENAME}
          </Button>
          <Button type="button" variant="ghost" size="sm" className={lockedClass} {...lockedProps} onClick={guard(onDelete)}>
            {T.DELETE}
          </Button>
          {why && <InfoTip label={NR_RUN.ABOUT_ARIA(t.name)}>{why}</InfoTip>}
          {canPublish && (
            <Button type="button" variant="outline" size="sm" disabled={busy} aria-describedby={nameId} onClick={onPublish}>
              {T.PUBLISH}
            </Button>
          )}
        </div>
        {why && (
          <span id={whyId} className="sr-only">
            {why}
          </span>
        )}
      </div>
    </li>
  );
}
