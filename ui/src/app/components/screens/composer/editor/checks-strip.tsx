/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The checks strip: what the server found out about the draft as it stands. Validity, the change review's verdict and
// its one sentence, the risk grade and, for a unit change, how many approvals saving will wait for. Every word is
// the server's answer read through the change review's own render function and chips; nothing here is a model's, and
// nothing here words a verdict.
import { renderReviewSentence, CHANGE_REVIEW } from "../../../../lib/change-review-copy";
import { fieldLabel } from "../../../../lib/change-review";
import type { AssistantChecks, AssistantTarget } from "../../../../lib/api/assistant";
import type { ChangeReview } from "../../../../lib/types";
import { RAIL_CHECK } from "../../../wardyn/copy/new-run-rail";
import { ASSISTANT as T } from "../../../wardyn/copy/assistant";
import { Chip } from "../../../wardyn/primitives";
import { RiskChip, VerdictChip } from "../../governance/change-review-sections";

/** The one sentence the change review writes for this review, or null when the change does not widen. */
export function checksSentence(review: ChangeReview | null | undefined, subject: string): string | null {
  return renderReviewSentence({
    verdict: review?.verdict,
    target: subject,
    widens: (review?.widens ?? []).map((w) => ({ ...w, label: fieldLabel(w.field) ?? w.field })),
    also: [],
  });
}

// The graded items the draft adds or changes, with the server's reason for each grade.
const sameItem = (a: { field: string; value: string; risk_level: string }, b: { field: string; value: string; risk_level: string }) =>
  a.field === b.field && a.value === b.value && a.risk_level === b.risk_level;

export interface ChecksStripProps {
  checks: AssistantChecks | null;
  /** What the sentence says the change is about: the unit's name. */
  subject: string;
  kind: AssistantTarget["kind"];
  /** A check is out. */
  busy?: boolean;
  /** The server's sentences for what failed (the checks could not be read, an edit was not taken); each is shown. */
  failed?: string | null | readonly (string | null)[];
  /** The draft left a host or field out because the ceiling refused it. */
  leftOut?: boolean;
}

export function ChecksStrip({ checks, subject, kind, busy, failed, leftOut }: ChecksStripProps) {
  const review = checks?.review ?? undefined;
  const verdict = review?.verdict;
  const sentence = checksSentence(review, subject);
  const risk = checks?.risk?.after ? { before: checks.risk.before, after: checks.risk.after } : undefined;
  const fresh = risk ? risk.after.items.filter((a) => !(risk.before?.items ?? []).some((b) => sameItem(a, b))) : [];
  const errors = checks?.errors ?? [];
  const failures = (Array.isArray(failed) ? failed : [failed]).filter((f): f is string => !!f);
  const widens = verdict === "widens" ? (review?.widens ?? []) : [];
  // Said only when the server's review checked the draft and found nothing that widens: a run's review is "not_checked"
  // today, which claims nothing, and a left-out host is outside the ceiling by definition.
  const within = checks?.valid === true && kind === "run" && verdict === "narrows_held" && !leftOut;
  return (
    <section data-testid="composer-checks" aria-busy={busy ? true : undefined} className="min-w-0 space-y-2 rounded-lg border border-border px-2.5 py-2">
      <div className="flex flex-wrap items-center gap-2">
        <h3 className="label-eyebrow">{T.CHECKS}</h3>
        {busy && <span className="text-meta text-muted-foreground">{RAIL_CHECK.CHECKING}</span>}
      </div>
      {failures.map((f, i) => (
        <p key={i} role="alert" className="text-body text-danger">
          {f}
        </p>
      ))}
      {checks && (
        <>
          <div className="flex flex-wrap items-center gap-2">
            {checks.valid ? <Chip tone="success">{T.VALID}</Chip> : null}
            {checks.valid && review && (kind !== "run" || verdict === "widens") ? <VerdictChip review={review} /> : null}
            {within ? <Chip tone="success">{T.WITHIN_CEILING}</Chip> : null}
            {checks.valid && risk ? <RiskChip risk={risk} /> : null}
          </div>
          {errors.length > 0 && (
            <ul data-testid="composer-check-errors" className="space-y-0.5 text-body text-danger">
              {errors.map((e, i) => (
                <li key={i}>
                  <span className="mono">{e.field}</span>: {e.message}
                </li>
              ))}
            </ul>
          )}
          {widens.length > 0 && (
            <ul className="space-y-0.5 text-body">
              {widens.map((w) => {
                const label = fieldLabel(w.field) ?? w.field;
                return <li key={w.field}>{w.entries && w.entries.length > 0 ? T.WIDENS_DETAIL(label, w.entries.length) : label}</li>;
              })}
            </ul>
          )}
          {sentence && <p className="text-body text-foreground">{sentence}</p>}
          {verdict === "not_checked" && kind !== "run" && <p className="text-body text-foreground">{CHANGE_REVIEW.NOT_CHECKED_NOTE}</p>}
          {fresh.length > 0 && (
            <ul className="space-y-0.5 text-body text-foreground">
              {fresh.map((i) => (
                <li key={`${i.field}:${i.value}`}>{i.rationale}</li>
              ))}
            </ul>
          )}
          {kind === "unit" && checks.valid && checks.approvals_needed > 0 && <p className="text-body font-medium text-foreground">{T.HELD_FOR(checks.approvals_needed)}</p>}
        </>
      )}
      {kind === "run" && <p className="text-meta text-muted-foreground">{RAIL_CHECK.HINT}</p>}
    </section>
  );
}
