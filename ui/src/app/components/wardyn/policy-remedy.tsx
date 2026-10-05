/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Request access remedy (mock packet M10 S4): who owns the policy that
// refused someone and how to ask for a change, as one muted line beside the
// refusal. The server validates the contact on write and again on read; this
// file re-checks the scheme before it builds an href, because a console that
// trusts the wire for what becomes a link is one bad row from a javascript: URL.
//
// The refusal sentence itself is never touched here. With no owner, link, email
// or text there is nothing to say, and nothing renders.
import { cn } from "../ui/utils";
import type { PolicyRef } from "../../lib/api/health";
import { POLICY_REMEDY } from "../../lib/governance-copy";

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

export function PolicyRemedy({ policy, className }: { policy?: PolicyRef | null; className?: string }) {
  if (!policy) return null;
  const href = safeRequestHref(policy.request_url);
  const mail = policy.email ? safeRequestHref(`mailto:${policy.email}`) : "";
  let action: React.ReactNode = null;
  if (href) {
    action = (
      <a href={href} target="_blank" rel="noopener noreferrer" className={LINK}>
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
  }
  if (!policy.owner && !action) return null;
  return (
    <span data-testid="policy-remedy" className={cn("text-xs text-muted-foreground", className)}>
      {policy.owner && POLICY_REMEDY.OWNER(policy.owner)}
      {policy.owner && action && POLICY_REMEDY.SEP}
      {action}
    </span>
  );
}
