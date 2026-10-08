/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// PolicyDocument — the one read-only view of a policy, shared by New Run,
// Policies, Governance and the run page. Three views of the same spec: Summary
// (plain names, the view it opens on), YAML and JSON. Switching view never
// changes anything: it is controlled, and the wrapper owns loading, provenance,
// freshness and whatever the person may do next (`notes`, `actions`).
//
// It does not parse. A wrapper that holds authored text parses it and passes
// the result as `spec` (null while the text is invalid) beside `source`, so the
// run page can show a recorded policy without loading the source parser.
import * as React from "react";
import type { PolicySourceFormat } from "../../../lib/policy-document";
import type { RunPolicySpec } from "../../../lib/types";
import type { PolicyPreviewPending } from "../../../lib/types/policy-preview";
import { POLICY_TAB } from "../../screens/run-detail/policy-tab-copy";
import { cn } from "../../ui/utils";
import { CodeBlock, JsonBlock, toYaml, YamlBlock } from "../code-block";
import { POLICY_DOCUMENT as D } from "../copy/policy-document";
import { CopyButton } from "../copy-button";
import { Segmented } from "../segmented";
import { PolicySummary, specIsRedacted, type PolicyChangeMarks, type PolicyFacts } from "./policy-summary";

export type PolicyView = "summary" | "yaml" | "json";

const VIEWS: { value: PolicyView; label: string }[] = [
  { value: "summary", label: POLICY_TAB.viewSummary },
  { value: "yaml", label: POLICY_TAB.viewYaml },
  { value: "json", label: D.JSON },
];

export const POLICY_COPY_BUTTON =
  "gap-1.5 rounded-md border border-border bg-card px-2.5 py-1.5 text-xs font-medium text-foreground hover:bg-muted disabled:cursor-not-allowed disabled:opacity-50";

export interface PolicyDocumentProps {
  /** The policy to show. Null when there is none to show, such as authored text that does not parse. */
  spec: RunPolicySpec | null;
  view: PolicyView;
  onViewChange: (view: PolicyView) => void;
  /** Some values are hidden from this reader; the raw views say so. */
  redacted?: boolean;
  /** What launch changed (run page only). */
  marks?: PolicyChangeMarks;
  facts?: PolicyFacts;
  /** The text the person wrote, when `spec` was parsed from it. Its own format's view shows it as written. */
  source?: { text: string; format: PolicySourceFormat };
  /** Launch-only checks a preview did not run. */
  pending?: readonly PolicyPreviewPending[];
  /** The spec no longer answers the current draft: it stays readable, its copies are held. */
  stale?: boolean;
  headingLevel?: 3 | 4;
  /** Status lines the wrapper owns, under the view switch. */
  notes?: React.ReactNode;
  /** The wrapper's own actions, beside the copy button. */
  actions?: React.ReactNode;
  className?: string;
}

export function PolicyDocument({
  spec,
  view,
  onViewChange,
  redacted,
  marks,
  facts,
  source,
  pending,
  stale,
  headingLevel,
  notes,
  actions,
  className,
}: PolicyDocumentProps) {
  // The view in the source's own format shows the text as written, valid or not.
  const authored = source && view === source.format ? source : null;
  // A generated or converted copy needs current, valid data.
  const held = !spec || !!stale;
  const copyText = !spec
    ? ""
    : view === "json"
      ? source?.format === "json"
        ? source.text
        : JSON.stringify(spec, null, 2)
      : source?.format === "yaml"
        ? source.text
        : toYaml(spec);
  const copyLabel = view === "json" ? D.COPY_JSON : POLICY_TAB.copyYaml;
  return (
    <div className={cn("flex min-w-0 flex-col gap-3", className)} data-testid="policy-document" data-view={view}>
      <div data-testid="policy-view-switch">
        <Segmented value={view} options={VIEWS} onChange={onViewChange} />
      </div>
      {redacted && spec && view !== "summary" && <p className="text-xs text-muted-foreground">{POLICY_TAB.redacted}</p>}
      {notes}
      {authored ? (
        <CodeBlock text={authored.text} copyLabel={D.COPY_SOURCE} />
      ) : (
        spec &&
        (view === "summary" ? (
          <PolicySummary spec={spec} marks={marks} facts={facts} pending={pending} headingLevel={headingLevel} />
        ) : (
          // A stale preview stays readable; the block's own copy is held with the button below.
          <fieldset disabled={held} className="m-0 min-w-0 border-0 p-0">
            {view === "yaml" ? <YamlBlock value={spec} /> : <JsonBlock value={spec} />}
          </fieldset>
        ))
      )}
      {(spec || source || actions) && (
        <div className="flex flex-wrap items-center gap-2">
          {(spec || source) && (
            <CopyButton text={copyText} label={copyLabel} disabled={held} className={POLICY_COPY_BUTTON}>
              {copyLabel}
            </CopyButton>
          )}
          {actions}
        </div>
      )}
    </div>
  );
}

/** A stored or default policy with nothing else to say about it: keeps its own
 *  view, and tells a reader who was shown blanked values that they were. */
export function PolicyDocumentView({ spec, className }: { spec: RunPolicySpec; className?: string }) {
  const [view, setView] = React.useState<PolicyView>("summary");
  return <PolicyDocument className={className} spec={spec} view={view} onViewChange={setView} redacted={specIsRedacted(spec)} />;
}
