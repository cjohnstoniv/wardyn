/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The server's three drop warnings (composer.ClampMember's "dropped N egress domain(s) not in operator
// allowlist: a,b", dropUncoveredPrivateLiterals' "dropped private address entry "x": ..." and a workspace's
// "dropped clone host "x" of workspace "w": ...") name the hosts in operator words, for the audit trail and
// the API. Review says them in the person's words (NR_REVIEW.DROPPED_HOST_ONE and DROPPED_HOSTS_MANY).
// The walls step's "your walls narrowed <knob>" names the knob in the console's words. Anything else the server
// says passes through unchanged.

import { NR_REVIEW } from "../../../wardyn/copy/new-run/review";
import { NR_WALLS } from "../../../wardyn/copy/personal-walls";
import { FIELD_LABEL } from "../../run-detail/policy-tab-copy";

const EGRESS_DROP = /^dropped \d+ egress domain\(s\) not in operator allowlist: (.+)$/;
const PRIVATE_DROP = /^dropped private address entry "(.+?)": not in operator allowlist/;
// The step that meets a person's own walls into the run names the knob it narrowed by its wire name.
const WALLS_NARROWED = /^your walls narrowed (.+)$/;
const CLONE_DROP = /^dropped clone host "(.+?)" of /;

/** "a", "a and b", "a, b and c". */
function joinAnd(items: string[]): string {
  return items.length < 2 ? items.join("") : `${items.slice(0, -1).join(", ")} and ${items[items.length - 1]}`;
}

/**
 * The walls step's warning for the host lists. Review says it by host instead (the Access group's "Your walls leave
 * out {host}.", board R15 T4-b), so this sentence is not shown again as a notice.
 */
export const isWallsHostWarning = (warning: string): boolean => {
  const knob = WALLS_NARROWED.exec(warning)?.[1];
  return knob === "allowed_domains" || knob === "denied_domains";
};

/** The server's sentence, or the dropped-host sentence when it is one of the three drop warnings. */
export function reviewWarningText(warning: string): string {
  const knob = WALLS_NARROWED.exec(warning)?.[1];
  if (knob) return NR_WALLS.NARROWED(FIELD_LABEL[knob]?.toLowerCase() ?? knob);
  const private_ = PRIVATE_DROP.exec(warning)?.[1] ?? CLONE_DROP.exec(warning)?.[1];
  const hosts = EGRESS_DROP.exec(warning)?.[1].split(",") ?? (private_ ? [private_] : []);
  if (hosts.length === 0) return warning;
  return hosts.length === 1 ? NR_REVIEW.DROPPED_HOST_ONE(hosts[0]) : NR_REVIEW.DROPPED_HOSTS_MANY(joinAnd(hosts));
}
