/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Host rules as lines of words, one per host the rules name. Review's Network row and the policy summary's Host rules row
// both read them from here, so the two screens say the same thing. The words follow EvalHostRules (internal/types/host_rules.go):
// a set that names a host refuses what none of its rules match, the first matching rule in a set decides, and when several
// sets name a host a request is refused if any set refuses it, held if none refuses and any holds, and allowed otherwise.

import type { HostRule, HostRuleSet } from "../../../../lib/types/policy";
import { HOST_RULES } from "../../run-detail/policy-tab-copy";

export interface HostRuleLine {
  host: string;
  text: string;
}

export interface HostRuleLines {
  /** One line per host, in the order hosts first appear across the sets; at most `cap` of them when a cap is given. */
  lines: HostRuleLine[];
  /** Says how many hosts the cap left out; absent when none were. */
  more?: string;
}

const listOf = (items: readonly string[]) => items.join(HOST_RULES.listJoin);

function clause(r: HostRule): string {
  const methods = r.methods?.length ? listOf(r.methods) : HOST_RULES.anyMethod;
  const where = r.paths?.length ? HOST_RULES.within(listOf(r.paths)) : HOST_RULES.anywhere;
  return HOST_RULES[r.effect](methods, where);
}

const clauses = (rules: readonly HostRule[]) => rules.map(clause).join(HOST_RULES.otherwise);

export function hostRuleLines(sets: readonly HostRuleSet[] | undefined, cap?: number): HostRuleLines {
  const hosts = [...new Set((sets ?? []).flatMap((s) => s.rules.map((r) => r.host)))];
  const all = hosts.map((host): HostRuleLine => {
    const naming = (sets ?? []).map((s) => s.rules.filter((r) => r.host === host)).filter((rules) => rules.length > 0);
    if (naming.length === 1) {
      return { host, text: (naming[0].length === 1 ? HOST_RULES.one : HOST_RULES.several)(host, clauses(naming[0])) };
    }
    const items = naming.map((rules, i) => HOST_RULES.setItem(i + 1, clauses(rules))).join(" ");
    return { host, text: HOST_RULES.setsNaming(host, naming.length, items) };
  });
  if (cap === undefined || all.length <= cap) return { lines: all };
  const left = all.length - cap;
  return { lines: all.slice(0, cap), more: left === 1 ? HOST_RULES.moreOne : HOST_RULES.moreHosts(left) };
}
