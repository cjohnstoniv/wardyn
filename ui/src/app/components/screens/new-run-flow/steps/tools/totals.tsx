/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Network and Credentials totals: what the run can reach, read from the
// server's `access` block. A host is Requested until the server has answered
// this exact body; only then does its verdict show.

import { AdoLaunchNote, AdoOwnTokenNote, AdoRunTokenLine, adoLaunchNoteKind, ownTokenNoteOrgs } from "../../../../wardyn/ado-run-token-line";
import { Chip } from "../../../../wardyn/primitives";
import { NR_TOOLS } from "../../../../wardyn/copy/new-run/tools";
import type { SCMAccessPAT } from "../../../../../lib/types/ado-pat";
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

const TOTAL_CLASS = "flex min-w-0 flex-col gap-2.5 rounded-lg border border-border bg-surface-2 p-3.5";
const POLICY_CLASS = "border-t border-dashed border-border pt-2.5 text-sm text-muted-foreground";

/** A delivery code in words; a code this console does not know shows no label rather than a raw code. */
export const deliveryLabel = (delivery: string): string | undefined => DELIVERY[delivery];

/**
 * A credential's label: a model credential names its provider, or says Model
 * provider before one is known. The other kinds carry no label on the wire, so
 * they show the row they are for and their delivery.
 */
export const credentialLabel = (c: AccessCredential, harness: string | undefined, providers?: ReadonlyMap<string, string>): string | undefined =>
  c.kind === "model_provider" ? (harness && providers?.get(harness)) || NR_TOOLS.PROVIDER_LABEL : c.label || undefined;

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

/** The credential a repository on the Azure DevOps sign-in lane brings (the server's kind for it). */
const ADO_SIGN_IN = "ado_signin";

/**
 * What the Credentials total says of this run's Azure DevOps credential when the person's row creates a
 * token for each run: what that token carries, and the door when they have yet to connect.
 */
export interface AdoTokenFacts {
  /** The caller's own Azure DevOps answer (the shell's setup status). */
  access: SCMAccessPAT | undefined;
  /** The run's policy capabilities; the row's default profile stands in when it names none. */
  policyCaps: unknown;
  connecting: boolean;
  onConnect: () => void;
  blockedUrl?: string | null;
  onFallbackClick?: () => void;
}

export function Totals({ access, answered, names: harnessNames, providers, ado, ownRows }: { access: AccessFacts | undefined; answered: boolean; names?: HarnessNames; providers?: ReadonlyMap<string, string>; ado?: AdoTokenFacts; ownRows?: readonly SCMAccessPAT[] }) {
  const names = new Map((access?.rows ?? []).map((r) => [r.id, rowLabel(r, harnessNames)]));
  const rowName = new Map((access?.rows ?? []).map((r) => [r.id, r.name]));
  // Only a run that carries the Azure DevOps credential gets a token, so only its row says so.
  const adoToken = ado?.access?.token_mode === "minted_pat" && !!access?.credentials.some((c) => c.kind === ADO_SIGN_IN) ? ado : undefined;
  return (
    <section aria-label={NR_TOOLS.TOTALS_ARIA} className="grid gap-3 [grid-template-columns:repeat(auto-fit,minmax(280px,1fr))]">
      <div className={TOTAL_CLASS}>
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
        {access && <p className={POLICY_CLASS}>{unlistedSentences(access.unlisted, answered).join(" ")}</p>}
      </div>
      <div className={TOTAL_CLASS}>
        <h3 className="font-medium">{NR_TOOLS.CREDENTIALS_TITLE}</h3>
        <ul className="flex flex-col gap-1.5">
          {access?.credentials.map((c) => {
            const kind = credentialLabel(c, rowName.get(c.row), providers);
            const delivery = deliveryLabel(c.delivery);
            return (
              <li key={c.id} className="flex flex-wrap items-center justify-between gap-2">
                <span className="min-w-0 [overflow-wrap:anywhere]">
                  {kind && <span>{kind} </span>}
                  {names.has(c.row) && <span className="text-sm text-muted-foreground">{NR_TOOLS.FOR_ROW(names.get(c.row) ?? "")}</span>}
                </span>
                {delivery && <Chip className="whitespace-normal">{delivery}</Chip>}
                {adoToken && c.kind === ADO_SIGN_IN && (
                  <AdoRunTokenLine className="basis-full text-sm text-muted-foreground" policyCaps={adoToken.policyCaps} defaults={adoToken.access?.default_profile} />
                )}
              </li>
            );
          })}
          {adoToken && adoLaunchNoteKind(adoToken.access) && (
            <li>
              <AdoLaunchNote access={adoToken.access} connecting={adoToken.connecting} onConnect={adoToken.onConnect} blockedUrl={adoToken.blockedUrl} onFallbackClick={adoToken.onFallbackClick} />
            </li>
          )}
          {access?.credentials.some((c) => c.kind === ADO_SIGN_IN) && ownTokenNoteOrgs(ownRows).length > 0 && (
            <li>
              <AdoOwnTokenNote rows={ownRows} />
            </li>
          )}
          {answered && access?.credentials.length === 0 && <li className="text-sm text-muted-foreground">{NR_TOOLS.NO_CREDENTIALS}</li>}
        </ul>
        {access && renewalHolds(access.credentials) && <p className={POLICY_CLASS}>{NR_TOOLS.CREDENTIALS_POLICY}</p>}
      </div>
    </section>
  );
}
