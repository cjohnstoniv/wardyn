/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The policy preview as the Policy panel's read view shows it. The check hook
// (use-run-checks.ts) drops its answer the moment the draft changes, which is
// right for anything that asserts "current". A person reading the policy still
// needs something to read while the next answer is on its way, or while their
// source does not parse and no answer is coming: the last authorized preview
// stays, never as fresh, and only for the same person and the same policy
// choice. It is never offered for another person, mode or saved policy.
import * as React from "react";
import { HttpError } from "../../../lib/api/core";
import { getErrorMessage } from "../../../lib/format";
import type { PolicyPreviewResult } from "../../../lib/types/policy-preview";
import type { NewRunPolicyPreview } from "../../wardyn/policy-document/new-run-policy-panel-body";
import { retryAfterDelay } from "./use-run-checks";

export interface PolicyPreviewRead {
  result: PolicyPreviewResult | null;
  error: unknown;
  busy: boolean;
  fresh: boolean;
}

const seconds = (ms: number) => Math.max(1, Math.ceil(ms / 1000));

/**
 * `scope` names whose draft this is and which policy it chose (person, sign-in,
 * mode, saved policy). `held` is a custom source that does not parse: nothing
 * is read for it, so only an earlier answer can be shown.
 */
export function usePolicyReadPreview(read: PolicyPreviewRead, scope: string, held: boolean): NewRunPolicyPreview {
  const [kept, setKept] = React.useState<{ scope: string; result: PolicyPreviewResult } | null>(null);
  const current = read.result;
  React.useEffect(() => {
    if (current) setKept({ scope, result: current });
  }, [current, scope]);
  const earlier = kept?.scope === scope ? kept.result : null;

  // A rate-limited read counts down to the moment its one retry is due.
  const limited = !held && read.error instanceof HttpError && read.error.status === 429 ? read.error : null;
  const [limit, setLimit] = React.useState<{ error: HttpError; until: number } | null>(null);
  const [tick, setTick] = React.useState(0);
  React.useEffect(() => {
    const delay = limited ? retryAfterDelay(limited.retryAfter, Date.now()) : null;
    setLimit(limited && delay !== null ? { error: limited, until: Date.now() + delay } : null);
  }, [limited]);
  React.useEffect(() => {
    if (!limit || limit.until - Date.now() <= 1000) return;
    const timer = setTimeout(() => setTick((n) => n + 1), 1000);
    return () => clearTimeout(timer);
  }, [limit, tick]);

  if (held) return { result: earlier, busy: false, fresh: false };
  const firstDelay = limited ? retryAfterDelay(limited.retryAfter, Date.now()) : null;
  const rateLimitSeconds = !limited
    ? null
    : limit?.error === limited
      ? seconds(limit.until - Date.now())
      : firstDelay !== null
        ? seconds(firstDelay)
        : null;
  return {
    result: current ?? earlier,
    busy: read.busy,
    fresh: !!current && read.fresh,
    // A rate limit with a known wait has its own sentence; any other failure keeps the server's.
    error: read.error && rateLimitSeconds === null ? getErrorMessage(read.error) : null,
    rateLimitSeconds,
  };
}
