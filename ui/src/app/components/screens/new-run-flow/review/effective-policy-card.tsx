/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { Button } from "../../../ui/button";
import { adoAccessSummary, HighRiskBadge } from "../../../wardyn/ado-access-summary";
import { ADO_ACCESS } from "../../../../lib/ado-access-copy";
import { Switch } from "../../../wardyn/form-primitives";
import { NR_REVIEW } from "../../../wardyn/copy/new-run/review";
import { PUSH } from "../../../wardyn/copy/push";
import type { PolicyPreviewResult } from "../../../../lib/types/policy-preview";
import type { NewRunDraft } from "../draft/run-intent";
import { BarrierChip } from "./barrier-chip";
import { harnessName, loadRoster } from "../steps/harnesses/roster";
import { useLoaded } from "../steps/shape/use-loaded";
import { changedRows, policyRows } from "./policy-rows";
import type { ReviewPreflight } from "./review-seams";

export function EffectivePolicyCard({ draft, pre, preview }: { draft: NewRunDraft; pre: ReviewPreflight | null; preview: PolicyPreviewResult | null }) {
  const [onlyChanged, setOnlyChanged] = React.useState(false);
  const changed = changedRows(draft.intent, draft.base);
  // The harness names the Tools group shows: the roster's display name, the id until it answers.
  const roster = useLoaded(loadRoster);
  const all = policyRows(draft, pre, preview, (id) => (roster.status === "ready" ? harnessName(roster.value, id) : id));
  const rows = onlyChanged ? all.filter((r) => changed.has(r.id)) : all;
  const ado = adoAccessSummary(preview?.spec.azure_devops_capabilities);
  // Only a review rule holds a push; a deny rule refuses it attended or not, so the note needs one.
  const unattendedPush = draft.intent.experience === "background" && !!preview?.spec.push_rules?.require_review_paths?.length;
  return (
    <aside aria-label={NR_REVIEW.POLICY_TITLE} className="flex flex-col overflow-hidden rounded-xl border border-border bg-card">
      <div className="min-h-[78px] border-b border-border px-5 py-[18px]">
        <h2 className="font-medium">{NR_REVIEW.POLICY_TITLE}</h2>
        <p className="text-sm text-muted-foreground">{NR_REVIEW.POLICY_BODY}</p>
      </div>
      <div className="flex items-center gap-2 border-b border-border px-[18px] py-3.5">
        <Switch checked={onlyChanged} onChange={setOnlyChanged} label={NR_REVIEW.ONLY_CHANGED} />
        <span className="text-sm">{NR_REVIEW.ONLY_CHANGED}</span>
      </div>
      {rows.length === 0 ? (
        <div className="flex flex-col items-start gap-2 px-[18px] py-3.5">
          <p className="text-sm text-muted-foreground">{NR_REVIEW.NOTHING_CHANGED}</p>
          <Button type="button" variant="outline" onClick={() => setOnlyChanged(false)}>{NR_REVIEW.SHOW_EVERYTHING}</Button>
        </div>
      ) : (
        <dl className="flex flex-col">
          {rows.map((r) => (
            <div key={r.id} data-policy-row={r.id} className="flex flex-col gap-0.5 border-b border-border px-[18px] py-3.5 last:border-b-0">
              <dt className="label-eyebrow">{r.label}</dt>
              <dd className="flex flex-col gap-0.5">
                {r.barrier && <BarrierChip cc={r.barrier} caps />}
                {r.value && <strong className="font-medium">{r.value}</strong>}
                {r.detail && <small className="text-muted-foreground">{r.detail}</small>}
              </dd>
            </div>
          ))}
        </dl>
      )}
      {/* What the run's policy grants on Azure DevOps, in one line; nothing for a policy that names none. */}
      {ado.parts.length > 0 && (
        <div data-testid="ado-access-summary" className="flex flex-col gap-0.5 border-t border-border px-[18px] py-3.5">
          <span className="label-eyebrow">{ADO_ACCESS.SECTION_TITLE}</span>
          <strong className="font-medium">{ado.parts.join(" · ")}</strong>
          {ado.highRisk && <HighRiskBadge />}
        </div>
      )}
      {unattendedPush && <p className="border-t border-border px-[18px] py-3.5 text-sm text-muted-foreground">{PUSH.RAIL_UNATTENDED}</p>}
    </aside>
  );
}
