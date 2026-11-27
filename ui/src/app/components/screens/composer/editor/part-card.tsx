/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// One part of the run draft: its name in the run policy Summary's words, what state it is in, and the controls that
// move it. A proposed part offers Accept and Discard; an accepted one offers Edit and Discard. A policy field is
// changed the way the run document editor changes it (the field's own value, as JSON), with Guided setup's host
// check on a list of hosts. Every value is text.
import * as React from "react";
import { NR_SAVE } from "../../../wardyn/copy/new-run/save";
import { NR_TOOLS } from "../../../wardyn/copy/new-run/tools";
import { Chip } from "../../../wardyn/primitives";
import { ASSISTANT as T } from "../../../wardyn/copy/assistant";
import { CHANGES } from "../../../../lib/governance-copy";
import { Button } from "../../../ui/button";
import { Textarea } from "../../../ui/textarea";
import { hostError, hostListError } from "../../new-run-flow/checks/custom-component-form-model";
import { FIELD_LABEL, SUMMARY } from "../../run-detail/policy-tab-copy";

// The part names of a draft, in the Summary's words. A name this map lacks falls through to the Summary's field labels.
const SECTION_LABEL: Record<string, string> = {
  allowed_hosts: SUMMARY.allowedHosts,
  network: SUMMARY.network,
  credentials: SUMMARY.credentials,
  tools: SUMMARY.tools,
  repositories: SUMMARY.repos,
  barrier: SUMMARY.barrier,
  image: NR_TOOLS.DETAIL_IMAGE,
};
export const sectionLabel = (section: string) => SECTION_LABEL[section] ?? FIELD_LABEL[section] ?? section;

/** A part's value as the lines the panel shows. */
export function valueLines(value: unknown): string[] {
  if (value == null) return [];
  if (Array.isArray(value)) return value.flatMap(valueLines);
  if (typeof value === "object") return Object.entries(value as Record<string, unknown>).map(([k, v]) => `${k}: ${valueLines(v).join(", ")}`);
  return [String(value)];
}

// The sections whose value is a list of hosts, which Guided setup's host check judges entry by entry and for a host
// listed twice; and the one whose rules each name a host (the same host may have several rules, so no twice check).
const HOST_SECTIONS = new Set(["allowed_domains", "denied_domains"]);
const RULE_SECTION = "host_rules";

/**
 * Reads the text a person typed back into a part's value. A list of hosts must be a list of strings that each pass
 * Guided setup's host check; any other value only has to be JSON. Returns the value, or the sentence to show.
 */
export function parsePartText(section: string, text: string): { value: unknown } | { error: string } {
  let value: unknown;
  try {
    value = JSON.parse(text);
  } catch {
    return { error: T.PART_EDIT_INVALID };
  }
  if (HOST_SECTIONS.has(section)) {
    if (!Array.isArray(value) || value.some((h) => typeof h !== "string")) return { error: T.PART_EDIT_INVALID };
    const bad = hostListError(value as string[]);
    if (bad) return { error: bad };
  }
  if (section === RULE_SECTION) {
    const sets = Array.isArray(value) ? value : null;
    if (!sets || sets.some((set) => !set || !Array.isArray((set as { rules?: unknown }).rules))) return { error: T.PART_EDIT_INVALID };
    for (const set of sets as { rules: unknown[] }[]) {
      for (const rule of set.rules) {
        const host = (rule as { host?: unknown } | null)?.host;
        if (typeof host !== "string") return { error: T.PART_EDIT_INVALID };
        const bad = hostError(host);
        if (bad) return { error: bad };
      }
    }
  }
  return { value };
}

export interface PartCardProps {
  section: string;
  value: unknown;
  status: "proposed" | "accepted";
  /** The person changed this part's value themselves. */
  edited: boolean;
  /** The server's part says it came from a build file; never read from the assistant's text. */
  fromBuildFile: boolean;
  /** A newer proposal says something other than what the person accepted or edited. */
  changedSince?: boolean;
  onAccept: () => void;
  onDiscard: () => void;
  onEdit: (value: unknown) => void;
}

export function PartCard({ section, value, status, edited, fromBuildFile, changedSince, onAccept, onDiscard, onEdit }: PartCardProps) {
  const [editing, setEditing] = React.useState(false);
  const [text, setText] = React.useState("");
  const [error, setError] = React.useState<string | null>(null);
  const accepted = status === "accepted";

  const startEdit = () => {
    setText(JSON.stringify(value ?? null, null, 2));
    setError(null);
    setEditing(true);
  };
  const finishEdit = () => {
    const read = parsePartText(section, text);
    if ("error" in read) {
      setError(read.error);
      return;
    }
    setEditing(false);
    onEdit(read.value);
  };

  return (
    <div
      data-testid="composer-part"
      data-section={section}
      data-status={status}
      className={`rounded-lg border px-2.5 py-2 ${accepted ? "border-border" : "border-info/35 bg-info-subtle/40"}`}
    >
      <div className="flex flex-wrap items-center justify-between gap-2">
        <span className="label-eyebrow">{sectionLabel(section)}</span>
        <span className="flex flex-wrap items-center gap-1.5">
          {fromBuildFile && <Chip tone="neutral">{T.PART_FROM_BUILD_FILE}</Chip>}
          {changedSince && <Chip tone="warning">{T.CHANGED_SINCE}</Chip>}
          <Chip tone={accepted ? (edited ? "primary" : "success") : "info"}>{!accepted ? T.PART_PROPOSED : edited ? T.PART_EDITED : T.PART_ACCEPTED}</Chip>
        </span>
      </div>
      {editing ? (
        <div className="mt-1 space-y-1.5">
          <Textarea aria-label={sectionLabel(section)} value={text} spellCheck={false} className="mono min-h-24 text-meta" onChange={(e) => setText(e.target.value)} />
          {error && (
            <p role="alert" className="text-body text-danger">
              {error}
            </p>
          )}
          <div className="flex gap-2">
            <Button type="button" size="sm" onClick={finishEdit}>
              {T.PART_EDIT_DONE}
            </Button>
            <Button type="button" size="sm" variant="outline" onClick={() => setEditing(false)}>
              {NR_SAVE.CANCEL_SAVE}
            </Button>
          </div>
        </div>
      ) : (
        <ul className="mt-1 flex flex-wrap gap-1.5">
          {valueLines(value).map((line, j) => (
            <li key={j} className="mono min-w-0 break-words rounded-md border border-border px-1.5 py-0.5 text-meta">
              {line}
            </li>
          ))}
        </ul>
      )}
      {!editing && (
        <div className="mt-2 flex flex-wrap gap-2">
          {accepted ? (
            <Button type="button" size="sm" variant="outline" onClick={startEdit}>
              {CHANGES.OP.update}
            </Button>
          ) : (
            <Button type="button" size="sm" onClick={onAccept}>
              {T.PART_ACCEPT}
            </Button>
          )}
          <Button type="button" size="sm" variant="outline" onClick={onDiscard}>
            {T.PART_DISCARD}
          </Button>
        </div>
      )}
    </div>
  );
}
