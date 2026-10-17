/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// OWNER-WORDING (FUG-L749 N3): the one sentence Review says for a host the ceiling dropped from the
// run's allowed list. It is in no canon table yet, so the lead moves it into copy/new-run/review.ts and
// docs/design/new-run-canon.md when the owner confirms the wording.
//
// The server's two drop warnings (composer.ClampMember's "dropped N egress domain(s) not in operator
// allowlist: a,b" and dropUncoveredPrivateLiterals' "dropped private address entry "x": ...") name the
// hosts in operator words. Anything else the server says passes through unchanged.

export const DROPPED_HOSTS = {
  ONE: (host: string) => `${host} is outside your ceiling and was dropped.`,
  MANY: (hosts: string) => `${hosts} are outside your ceiling and were dropped.`,
} as const;

const EGRESS_DROP = /^dropped \d+ egress domain\(s\) not in operator allowlist: (.+)$/;
const PRIVATE_DROP = /^dropped private address entry "(.+?)": not in operator allowlist/;

/** "a", "a and b", "a, b and c". */
function joinAnd(items: string[]): string {
  return items.length < 2 ? items.join("") : `${items.slice(0, -1).join(", ")} and ${items[items.length - 1]}`;
}

/** The server's sentence, or the ceiling-dropped-host sentence when it is one of the two drop warnings. */
export function reviewWarningText(warning: string): string {
  const private_ = PRIVATE_DROP.exec(warning)?.[1];
  const hosts = EGRESS_DROP.exec(warning)?.[1].split(",") ?? (private_ ? [private_] : []);
  if (hosts.length === 0) return warning;
  return hosts.length === 1 ? DROPPED_HOSTS.ONE(hosts[0]) : DROPPED_HOSTS.MANY(joinAnd(hosts));
}
