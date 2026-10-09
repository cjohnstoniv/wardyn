/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// One Access row (#1914): the component's name, its status and why it is there
// on one line; the sentences the run's shape makes true of it under that; and,
// opened, where it reaches, how its secrets travel and what is still needed.
// Open state belongs to the panel (one row open at a time), so this holds none.
//
// A row that holds Launch (D14) is an ISSUE, not a hint: error tone and icon,
// aria-invalid and aria-describedby on the control that owns it, and its
// sentence printed here. The same sentence is counted on the panel nav and
// linked above Launch.
import * as React from "react";
import { Link } from "react-router-dom";
import { ChevronDown, TriangleAlert } from "lucide-react";
import { Button } from "../../ui/button";
import { cn } from "../../ui/utils";
import { Chip } from "../../wardyn/primitives";
import { ACCESS_ROWS as T, ADD_ACCESS } from "../../wardyn/copy/components";
import type { AccessRow as Row } from "./access-rows-model";

export interface AccessRowProps {
  row: Row;
  open: boolean;
  onToggle: () => void;
  /** Where a person adds a secret of their own. */
  secretsPath: string;
  /** Wraps a link that leaves New Run, so unsaved work is asked about first. */
  guardLink: (to: string) => (e: React.MouseEvent) => void;
  /** Takes this component off the run; absent for a row the person did not add. */
  onRemove?: () => void;
}

function Part({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div>
      <div className="text-xs font-medium text-muted-foreground">{label}</div>
      {children}
    </div>
  );
}

export function AccessRow({ row, open, onToggle, secretsPath, guardLink, onRemove }: AccessRowProps) {
  const bodyId = `${row.domId}-body`;
  const issueId = `${row.domId}-issue`;
  return (
    <li
      id={row.domId}
      className={cn("rounded-lg border bg-card", row.blocking ? "border-danger" : "border-border")}
    >
      <h3>
        <button
          type="button"
          aria-expanded={open}
          aria-controls={open ? bodyId : undefined}
          aria-invalid={row.blocking || undefined}
          aria-describedby={row.issueText ? issueId : undefined}
          onClick={onToggle}
          className="flex w-full items-start justify-between gap-3 rounded-lg px-3 py-2 text-left outline-none focus-visible:ring-[3px] focus-visible:ring-ring"
        >
          <span className="min-w-0">
            <span className="flex flex-wrap items-center gap-2 text-sm font-medium text-foreground">
              {row.title}
              <Chip tone={row.tone}>{row.statusLabel}</Chip>
            </span>
            {row.reason && <span className="block text-xs text-muted-foreground">{row.reason}</span>}
          </span>
          <ChevronDown
            aria-hidden
            className={cn("mt-0.5 size-4 shrink-0 text-muted-foreground transition-transform", open && "rotate-180")}
          />
        </button>
      </h3>
      {row.issueText && (
        <p id={issueId} className="flex items-start gap-1.5 px-3 pb-2 text-sm text-danger">
          <TriangleAlert className="mt-0.5 size-4 shrink-0" aria-hidden="true" />
          {row.issueText}
        </p>
      )}
      {row.statusNote && <p className="px-3 pb-2 text-xs text-muted-foreground">{row.statusNote}</p>}
      {row.disclosures.length > 0 && (
        <ul className="space-y-0.5 px-3 pb-2 text-xs text-muted-foreground">
          {row.disclosures.map((d) => (
            <li key={d}>{d}</li>
          ))}
        </ul>
      )}
      {open && (
        <div id={bodyId} className="space-y-3 border-t border-border px-3 py-3 text-sm">
          {row.laneNote && <p className="text-muted-foreground">{row.laneNote}</p>}
          {row.org && (
            <Part label={T.BODY.ORG}>
              <p className="font-mono text-xs">{row.org}</p>
            </Part>
          )}
          {row.repos.length > 0 && (
            <Part label={T.BODY.REPOS}>
              <ul className="font-mono text-xs">
                {row.repos.map((r) => (
                  <li key={r}>{r}</li>
                ))}
              </ul>
            </Part>
          )}
          {row.hosts.length > 0 && (
            <Part label={T.BODY.HOSTS}>
              <ul className="font-mono text-xs">
                {row.hosts.map((h) => (
                  <li key={h}>{h}</li>
                ))}
              </ul>
            </Part>
          )}
          {row.secrets.length > 0 && (
            <Part label={T.BODY.SECRETS}>
              <ul className="text-xs">
                {row.secrets.map((s, at) => (
                  <li key={at}>
                    {T.BODY.SECRET_DELIVERY[s.delivery]}. {s.shared ? T.BODY.SECRET_SHARED : T.BODY.SECRET_OWN}.
                  </li>
                ))}
              </ul>
            </Part>
          )}
          {row.configKeys.length > 0 && (
            <Part label={T.BODY.CONFIG}>
              <p className="font-mono text-xs">{row.configKeys.join(", ")}</p>
            </Part>
          )}
          {row.requirements.length > 0 && (
            <Part label={T.BODY.NEEDS}>
              <ul className="space-y-1.5">
                {row.requirements.map((req) => (
                  <li key={req.id}>
                    <div>{req.label}</div>
                    {req.detail && <div className="text-xs text-muted-foreground">{req.detail}</div>}
                    {req.fix?.action === "add_secret" && (
                      <Link to={secretsPath} className="text-xs font-medium text-info hover:underline" onClick={guardLink(secretsPath)}>
                        {T.BODY.ADD_SECRET}
                      </Link>
                    )}
                  </li>
                ))}
              </ul>
            </Part>
          )}
          {onRemove && (
            <Button type="button" variant="outline" size="sm" aria-label={ADD_ACCESS.REMOVE_NAMED(row.title)} onClick={onRemove}>
              {ADD_ACCESS.REMOVE}
            </Button>
          )}
        </div>
      )}
    </li>
  );
}
