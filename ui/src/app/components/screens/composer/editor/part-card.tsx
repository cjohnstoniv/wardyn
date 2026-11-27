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
import { hostListError } from "../../new-run-flow/checks/custom-component-form-model";
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

// The sections whose value is a list of domain entries, judged as the proxy judges them and for an entry listed twice;
// and the one whose rules each name a host (the same host may have several rules, so no twice check).
const DOMAIN_SECTIONS = new Set(["allowed_domains", "denied_domains"]);
const RULE_SECTION = "host_rules";

// The kind of value a known policy field holds (types.RunPolicySpec), so a value of another kind never reaches New run.
const KIND: Record<string, "list" | "object" | "value"> = {
  allowed_domains: "list", denied_domains: "list", allowed_methods: "list", host_rules: "list", eligible_grants: "list",
  workspace_mounts: "list", workspace_repos: "list", tool_rules: "list", ui_apps: "list",
  llm_inspection: "object", resources: "object", push_rules: "object",
  first_use_approval: "value", first_use_hold_seconds: "value", max_holds: "value", min_confinement_class: "value",
  auto_stop_after_sec: "value", allow_all_egress: "value", refuse_ip_literals: "value", git_push_any_branch: "value",
};
const KIND_ERROR = { list: T.PART_EDIT_LIST, object: T.PART_EDIT_OBJECT, value: T.PART_EDIT_VALUE } as const;
const isPlainObject = (v: unknown): boolean => typeof v === "object" && v !== null && !Array.isArray(v);
const KIND_OK = { list: Array.isArray, object: isPlainObject, value: (v: unknown) => v !== null && typeof v !== "object" } as const;

// An IPv4 address as the server reads it: four decimal numbers from 0 to 255 with no leading zeros.
const IPV4 = /^(25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)(\.(25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)){3}$/;
const validPort = (p: string) => /^\d+$/.test(p) && Number(p) >= 1 && Number(p) <= 65535;
const isIPv6 = (h: string) => {
  if (!/^[0-9a-f:.]+$/i.test(h) || (h.match(/:/g) ?? []).length < 2) return false;
  try {
    return !!new URL(`http://[${h}]/`);
  } catch {
    return false;
  }
};
// An entry whose every label is a decimal or 0x number is an address, and only the standard spelling of one is taken.
const numberLabel = (l: string) => /^\d+$/.test(l) || /^0x[0-9a-f]+$/i.test(l);

/**
 * The sentence for a domain entry the proxy can never match, or null: a host, "*." and a host, or an IP address, each
 * with an optional port (internal/egress/proxy/policy.go ValidDomainEntry).
 */
export function domainEntryError(entry: string): string | null {
  const bad = T.DOMAIN_ENTRY_BAD(entry);
  if (entry === "" || /[\s/]/.test(entry)) return bad;
  const wild = entry.startsWith("*.");
  const rest = wild ? entry.slice(2) : entry;
  const bracket = /^\[([^\]]*)\]:(.*)$/.exec(rest);
  if (bracket) return !wild && isIPv6(bracket[1]) && validPort(bracket[2]) ? null : bad;
  if (rest.startsWith("[")) return bad;
  const colons = (rest.match(/:/g) ?? []).length;
  if (colons >= 2) return !wild && isIPv6(rest) ? null : bad;
  const [host, port] = colons === 1 ? rest.split(":") : [rest, undefined];
  if (port !== undefined && !validPort(port)) return bad;
  if (host === "" || !/^[a-z0-9.-]+$/i.test(host)) return bad;
  if (host.split(".").every(numberLabel)) return !wild && IPV4.test(host) ? null : bad;
  return null;
}

// A rule's host is one exact host (internal/types/host_rules.go validHostRule): lower case, no wildcard, port, path or
// trailing dot, and an IPv4 address written in full.
const ruleHostError = (host: string): string | null => {
  if (host.includes("*")) return T.RULE_HOST_WILDCARD(host);
  const dead = host === "" || host !== host.toLowerCase() || /[\s:/?#[\]@\\%]|[^\x21-\x7e]/.test(host) || host.startsWith(".") || host.endsWith(".");
  return dead || (/^[0-9.]+$/.test(host) && !IPV4.test(host)) ? T.RULE_HOST_BAD(host) : null;
};

/**
 * Reads the text a person typed back into a part's value. A field the policy knows must hold its kind of value; a list
 * of domains must pass the proxy's own entry check and name no entry twice; host_rules' hosts pass the server's rule.
 * Any other value only has to be JSON. Returns the value, or the sentence to show.
 */
export function parsePartText(section: string, text: string): { value: unknown } | { error: string } {
  let value: unknown;
  try {
    value = JSON.parse(text);
  } catch {
    return { error: T.PART_EDIT_INVALID };
  }
  const kind = KIND[section];
  if (kind && !KIND_OK[kind](value)) return { error: KIND_ERROR[kind] };
  if (DOMAIN_SECTIONS.has(section)) {
    if ((value as unknown[]).some((h) => typeof h !== "string")) return { error: T.PART_EDIT_INVALID };
    const bad = hostListError(value as string[], domainEntryError);
    if (bad) return { error: bad };
  }
  if (section === RULE_SECTION) {
    const sets = value as unknown[];
    if (sets.some((set) => !set || !Array.isArray((set as { rules?: unknown }).rules))) return { error: T.PART_EDIT_LIST };
    for (const set of sets as { rules: unknown[] }[]) {
      for (const rule of set.rules) {
        const host = (rule as { host?: unknown } | null)?.host;
        if (typeof host !== "string") return { error: T.PART_EDIT_INVALID };
        const bad = ruleHostError(host);
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
