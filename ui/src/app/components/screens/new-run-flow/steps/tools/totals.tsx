/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Network and Credentials totals: what the run can reach, read from the
// server's `access` block. A host is Requested until the server has answered
// this exact body; only then does its verdict show.

import { Chip } from "../../../../wardyn/primitives";
import { NR_TOOLS } from "../../../../wardyn/copy/new-run/tools";
import type { AccessCredential, AccessFacts, AccessUnlisted } from "../../../../../lib/types/new-run-access";
import { hostBadge, rowLabel, type HarnessNames, type HostBadge } from "../../lenses/tools-lens";

const DELIVERY: Record<string, string> = {
  proxy: NR_TOOLS.DELIVERY_PROXY,
  git_broker: NR_TOOLS.DELIVERY_GIT_BROKER,
  git_helper: NR_TOOLS.DELIVERY_GIT_HELPER,
  sandbox_env: NR_TOOLS.DELIVERY_ENV,
  sandbox_file: NR_TOOLS.DELIVERY_FILE,
  sandbox_short_lived: NR_TOOLS.DELIVERY_SHORT_LIVED,
};
const BADGE: Record<HostBadge, { text: string; tone: "neutral" | "success" | "info" | "danger" }> = {
  requested: { text: NR_TOOLS.NET_REQUESTED, tone: "neutral" },
  allowed: { text: NR_TOOLS.NET_ALLOWED, tone: "success" },
  needs_approval: { text: NR_TOOLS.NET_NEEDS_APPROVAL, tone: "info" },
  not_allowed: { text: NR_TOOLS.NET_NOT_ALLOWED, tone: "danger" },
};

/** A delivery code in words; a code this console does not know shows no label rather than a raw code. */
export const deliveryLabel = (delivery: string): string | undefined => DELIVERY[delivery];

/** The credential kinds the canon names; the rest show the row they are for and their delivery. */
const credentialKind = (c: AccessCredential): string | undefined => (c.kind === "model_provider" ? NR_TOOLS.PROVIDER_LABEL : undefined);

/** "Anything else" for the clamped, effective spec, then whether the organisation's limits have been checked. */
export function unlistedSentences(u: AccessUnlisted, answered: boolean): string[] {
  if (!answered) return [NR_TOOLS.NET_POLICY_CEILING];
  const admin = u.decider !== "you";
  const out: string[] = [];
  if (u.allow_all) out.push(NR_TOOLS.NET_POLICY_ALLOW_ALL);
  else if (u.mode === "always_deny") out.push(NR_TOOLS.NET_POLICY_ALWAYS_DENY);
  else if (u.mode === "deny_with_review") out.push(admin ? NR_TOOLS.NET_POLICY_DENY_REVIEW_ADMIN : NR_TOOLS.NET_POLICY_DENY_REVIEW_YOU);
  else if (u.mode === "wait_for_review") {
    out.push(admin ? NR_TOOLS.NET_POLICY_HELD_ADMIN : NR_TOOLS.NET_POLICY_HELD_YOU);
    if (u.hold_sec) out.push(NR_TOOLS.NET_POLICY_TIMEOUT(u.hold_sec));
  }
  out.push(NR_TOOLS.NET_POLICY_CHECKED);
  return out;
}

/**
 * The policy line says short-lived credentials are renewed while the run lives.
 * That is untrue once a short-lived credential is not renewed (a Bedrock SSO
 * sign-in with the proxy-inject switch off), so the line is withheld then and
 * each credential row states its own delivery.
 */
export const renewalHolds = (credentials: readonly AccessCredential[]): boolean =>
  !credentials.some((c) => c.delivery === "sandbox_short_lived" && !c.renewed);

export function Totals({ access, answered, names: harnessNames }: { access: AccessFacts | undefined; answered: boolean; names?: HarnessNames }) {
  const names = new Map((access?.rows ?? []).map((r) => [r.id, rowLabel(r, harnessNames)]));
  return (
    <section aria-label={NR_TOOLS.TOTALS_ARIA} className="grid gap-4 md:grid-cols-2">
      <div className="flex min-w-0 flex-col gap-2">
        <h3 className="font-medium">{NR_TOOLS.NETWORK_TITLE}</h3>
        <ul className="flex flex-col gap-1.5">
          {access?.hosts.map((h) => {
            const from = h.rows.flatMap((id) => names.get(id) ?? []);
            const badge = BADGE[hostBadge(h, answered)];
            return (
              <li key={h.host} className="flex flex-wrap items-center justify-between gap-2">
                <span className="min-w-0 [overflow-wrap:anywhere]">
                  <span className="font-mono text-sm">{h.host}</span>
                  {from.length > 0 && <span className="text-sm text-muted-foreground"> {NR_TOOLS.FROM_ROW(from.join(", "))}</span>}
                </span>
                <Chip tone={badge.tone} className="whitespace-normal">{badge.text}</Chip>
              </li>
            );
          })}
          {answered && access?.hosts.length === 0 && <li className="text-sm text-muted-foreground">{NR_TOOLS.NO_HOSTS}</li>}
        </ul>
        {access && <p className="text-sm text-muted-foreground">{unlistedSentences(access.unlisted, answered).join(" ")}</p>}
      </div>
      <div className="flex min-w-0 flex-col gap-2">
        <h3 className="font-medium">{NR_TOOLS.CREDENTIALS_TITLE}</h3>
        <ul className="flex flex-col gap-1.5">
          {access?.credentials.map((c) => {
            const kind = credentialKind(c);
            const delivery = deliveryLabel(c.delivery);
            return (
              <li key={c.id} className="flex flex-wrap items-center justify-between gap-2">
                <span className="min-w-0 [overflow-wrap:anywhere]">
                  {kind && <span>{kind} </span>}
                  {names.has(c.row) && <span className="text-sm text-muted-foreground">{NR_TOOLS.FOR_ROW(names.get(c.row) ?? "")}</span>}
                </span>
                {delivery && <Chip className="whitespace-normal">{delivery}</Chip>}
              </li>
            );
          })}
          {answered && access?.credentials.length === 0 && <li className="text-sm text-muted-foreground">{NR_TOOLS.NO_CREDENTIALS}</li>}
        </ul>
        {access && renewalHolds(access.credentials) && <p className="text-sm text-muted-foreground">{NR_TOOLS.CREDENTIALS_POLICY}</p>}
      </div>
    </section>
  );
}
