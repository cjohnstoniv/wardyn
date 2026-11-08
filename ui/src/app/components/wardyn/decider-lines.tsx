/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { User } from "lucide-react";
import type { ApprovalRequest } from "../../lib/types";
import { DECIDERS_INBOX as C } from "./copy/deciders-inbox";
import { useSecurityOperator } from "./operator-context";

// "a", "a or b", "a, b or c": the join the server's own refusal sentences use, with no Oxford comma.
export function joinOr(list: string[]): string {
  if (list.length <= 1) return list.join("");
  return `${list.slice(0, -1).join(", ")} or ${list[list.length - 1]}`;
}

// One `deciders` entry as a phrase. `ownCard` is the owner's own card: there the owner entry is "you".
export function deciderPhrase(entry: string, ownCard: boolean): string {
  if (entry === "owner") return ownCard ? C.OWNER_SELF : C.OWNER_OTHER;
  if (entry === "unit_admins") return C.UNIT_ADMINS;
  if (entry === "security") return C.SECURITY;
  if (entry.startsWith("group:")) return C.GROUP(entry.slice("group:".length));
  return entry;
}

// The sentence under a decided row for its `decision_basis`, null when the row says none or the console does not
// know the value. A unit admin's basis carries the unit id: its name comes from `unitNames`, else the general wording.
export function basisSentence(basis: string | undefined, unitNames?: Record<string, string>): string | null {
  if (!basis) return null;
  if (basis === "owner") return C.BASIS_OWNER;
  if (basis === "security") return C.BASIS_SECURITY;
  if (basis === "policy") return C.BASIS_POLICY;
  if (basis === "admin_token") return C.BASIS_ADMIN_TOKEN;
  if (basis.startsWith("group:")) return C.BASIS_GROUP(basis.slice("group:".length));
  if (basis.startsWith("unit_admin:")) {
    const name = unitNames?.[basis.slice("unit_admin:".length)];
    return name ? C.BASIS_UNIT_ADMIN_OF(name) : C.BASIS_UNIT_ADMIN;
  }
  return null;
}

type Row = Pick<ApprovalRequest, "deciders" | "requested_by" | "second_human">;

// Who may decide a pending request, in the server's order. Nothing when no rule governs the row. "you" is only the
// run's owner reading their own card: a security operator's Pending list carries other people's runs too.
export function DecidedByLine({ item }: { item: Row }) {
  const securityOperator = useSecurityOperator();
  if (!item.deciders || item.deciders.length === 0) return null;
  const own = !item.requested_by && !securityOperator;
  return (
    <p className="mt-1 text-xs text-muted-foreground">
      <span className="font-medium text-foreground">{C.DECIDED_BY_LABEL}</span>{" "}
      {joinOr(item.deciders.map((e) => deciderPhrase(e, own)))}
    </p>
  );
}

export function SecondPersonLine({ item }: { item: Row }) {
  if (!item.second_human) return null;
  return <p className="mt-1 text-xs text-muted-foreground">{C.SECOND_PERSON}</p>;
}

// A decider's card names who asked instead of the run row: a decider cannot read the run, so the row's own fetch
// would draw "run unavailable" and an Open run link that leads nowhere. No link and no run read.
export function RequestedByRow({ who, runId }: { who: string; runId?: string }) {
  const t = C.REQUESTED_BY(who, runId ? runId.slice(0, 8) : "");
  return (
    <div className="mt-3 flex flex-wrap items-center gap-x-3 gap-y-1.5 rounded-lg border border-border bg-background px-3 py-2">
      <User className="size-4 shrink-0 text-muted-foreground" aria-hidden="true" />
      <span className="min-w-0 text-sm text-foreground">
        {t.before}
        <span className="font-medium">{t.who}</span>
      </span>
      {runId && (
        <span className="text-xs text-muted-foreground">
          run <span className="font-mono">{t.run}</span>
        </span>
      )}
      {runId && <span className="ml-auto whitespace-nowrap text-xs text-muted-foreground">{C.CANT_OPEN_RUN}</span>}
    </div>
  );
}
