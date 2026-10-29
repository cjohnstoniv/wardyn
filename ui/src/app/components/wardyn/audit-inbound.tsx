/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { cn } from "../ui/utils";
import { AUDIT } from "../screens/audit-copy";

/** The outcome of an inbound.allow / inbound.deny row: the word, in the tone egress rows use for the same two answers. */
export function InboundOutcome({ allowed }: { allowed: boolean }) {
  return <span className={cn("font-mono text-xs font-semibold", allowed ? "text-success" : "text-danger")}>{allowed ? AUDIT.OUTCOME_ALLOW : AUDIT.OUTCOME_DENY}</span>;
}

/** Where a service row's fact came from, in the outcome cell: those rows are not allowed or refused. */
export function SourceLabel({ source }: { source: string }) {
  return <span className="text-xs text-muted-foreground">{source}</span>;
}

/** The actor cell of an inbound.deny with no credential: there is nobody to name. */
export function NoCaller() {
  return <span className="text-xs text-muted-foreground">{AUDIT.NO_CALLER}</span>;
}
