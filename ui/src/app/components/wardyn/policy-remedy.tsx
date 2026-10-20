/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The "Why can't I?" card (board M2, Option A) beside a refusal: which wall set
// the limit, who owns it and how to ask for a change. The server validates the
// contact on write and again on read; this file re-checks the scheme before it
// builds an href, because a console that trusts the wire for what becomes a
// link is one bad row from a javascript: URL.
//
// The refusal sentence itself is never touched here. A limit with no owner or
// way to ask still gets a card, naming the admin to ask instead of nothing.
import * as React from "react";
import { cn } from "../ui/utils";
import type { PolicyRef } from "../../lib/api/health";
import { POLICY_REMEDY } from "../../lib/governance-copy";
import { WHY_CARD } from "../../lib/why-card-copy";

const MAX_HREF = 2048;

// safeRequestHref returns v when it is an https: address (real host, no sign-in
// details) or one mailto: address with no query and no fragment, else "". It is
// the same rule the profile editor applies before saving, and the one every link
// below passes through.
export function safeRequestHref(v: string | undefined): string {
  if (!v || v.length > MAX_HREF || !/^[\x21-\x7e]+$/.test(v)) return "";
  if (/^mailto:/i.test(v)) {
    const addr = v.slice("mailto:".length);
    return /^[^@\s,;?#%<>()"']+@[^@\s,;?#%<>()"']+$/.test(addr) ? v : "";
  }
  if (!/^https:\/\//i.test(v)) return "";
  try {
    const u = new URL(v);
    return u.protocol === "https:" && u.hostname !== "" && !u.username && !u.password ? v : "";
  } catch {
    return "";
  }
}

const LINK = "underline underline-offset-2 hover:text-foreground";
const REQUEST_BUTTON =
  "inline-flex items-center gap-1 rounded-md border border-border px-2 py-0.5 text-foreground hover:bg-muted";

// setByLabel names the wall that set the limit from the refusal's own layer
// (source on a proxy attribution, whose wire predates the layer key).
function setByLabel(policy: PolicyRef): string {
  if (policy.overridden) return WHY_CARD.RUN_POLICY;
  if ((policy.layer ?? policy.source) === "deployment") return WHY_CARD.BOUNDARY;
  return policy.name ? WHY_CARD.TEAM(policy.name) : WHY_CARD.TEAM_FALLBACK;
}

export interface PolicyRemedyProps {
  policy?: PolicyRef | null;
  className?: string;
  /** Where the check ran, when the surface knows (the Enforced row). */
  enforced?: string;
  /** Show the Can change row. */
  canChange?: string;
  /** Show the Copy details button. */
  copyable?: boolean;
}

export function PolicyRemedy({ policy, className, enforced, canChange, copyable }: PolicyRemedyProps) {
  const [copied, setCopied] = React.useState(false);
  if (!policy) return null;
  const href = safeRequestHref(policy.request_url);
  const mail = policy.email ? safeRequestHref(`mailto:${policy.email}`) : "";
  const setBy = setByLabel(policy);
  let action: React.ReactNode = null;
  if (href) {
    action = (
      <a href={href} target="_blank" rel="noopener noreferrer" className={REQUEST_BUTTON}>
        {POLICY_REMEDY.LINK} <span aria-hidden="true">↗</span>
      </a>
    );
  } else if (policy.email) {
    action = mail ? (
      <a href={mail} rel="noopener noreferrer" className={LINK}>
        {POLICY_REMEDY.EMAIL(policy.email)}
      </a>
    ) : (
      POLICY_REMEDY.EMAIL(policy.email)
    );
  } else if (policy.request_text) {
    action = policy.request_text;
  } else if (policy.owner) {
    action = WHY_CARD.NO_WAY_TO_ASK(policy.owner);
  } else {
    action = WHY_CARD.NO_OWNER;
  }
  const rows: Array<[string, React.ReactNode]> = [
    [WHY_CARD.SET_BY, setBy],
    [WHY_CARD.OWNER, policy.owner || WHY_CARD.NOT_LISTED],
  ];
  if (enforced) rows.push([WHY_CARD.ENFORCED, enforced]);
  if (canChange) rows.push([WHY_CARD.CAN_CHANGE, canChange]);
  const copy = () => {
    const text = rows.map(([k, v]) => `${k}: ${typeof v === "string" ? v : ""}`.trimEnd()).join("\n");
    void navigator.clipboard?.writeText(text).then(() => setCopied(true));
  };
  return (
    <div
      data-testid="policy-remedy"
      className={cn("flex flex-col gap-1 rounded-md border border-border bg-muted/30 px-3 py-2 text-xs text-muted-foreground", className)}
    >
      <dl className="grid grid-cols-[auto_1fr] gap-x-3 gap-y-0.5">
        {rows.map(([k, v]) => (
          <React.Fragment key={k}>
            <dt>{k}</dt>
            <dd className="text-foreground">{v}</dd>
          </React.Fragment>
        ))}
      </dl>
      <div className="flex flex-wrap items-center gap-2" data-testid="policy-remedy-ask">
        {action}
        {copyable && (
          <button type="button" onClick={copy} className={cn(REQUEST_BUTTON, "text-muted-foreground")}>
            {copied ? WHY_CARD.COPIED : WHY_CARD.COPY_DETAILS}
          </button>
        )}
      </div>
    </div>
  );
}
