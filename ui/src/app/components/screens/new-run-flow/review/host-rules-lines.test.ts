/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it } from "vitest";
import type { HostRule, HostRuleSet } from "../../../../lib/types/policy";
import { hostRuleLines } from "./host-rules-lines";

// Every expected string below is the approved board's text (RN, H-10 to H-31), written out in full.
const rule = (host: string, effect: HostRule["effect"], methods?: string[], paths?: string[]): HostRule => ({ host, effect, ...(methods && { methods }), ...(paths && { paths }) });
const texts = (sets: HostRuleSet[] | undefined, cap?: number) => hostRuleLines(sets, cap).lines.map((l) => l.text);
const simple = (n: number): HostRuleSet[] => [{ rules: Array.from({ length: n }, (_, i) => rule(`h${i + 1}.example.com`, "allow", ["GET"], ["/public"])) }];
const simpleLine = (i: number) => `h${i}.example.com: allows GET within /public. Any other request is refused.`;

describe("hostRuleLines", () => {
  it("one set, one host: the host, what it allows and that anything else is refused", () => {
    expect(texts([{ rules: [rule("api.example.com", "allow", ["GET"], ["/public"])] }])).toEqual([
      "api.example.com: allows GET within /public. Any other request is refused.",
    ]);
  });

  it("three hosts in first-appearance order; a host with two rules reads them in order, first match deciding", () => {
    const sets = [{ rules: [rule("api.example.com", "allow", ["GET"], ["/public"]), rule("registry.example.com", "allow", ["GET", "HEAD"]), rule("files.example.com", "deny", undefined, ["/downloads/private"]), rule("files.example.com", "allow", ["GET"], ["/downloads"])] }];
    expect(hostRuleLines(sets, 8)).toEqual({
      lines: [
        { host: "api.example.com", text: "api.example.com: allows GET within /public. Any other request is refused." },
        { host: "registry.example.com", text: "registry.example.com: allows GET, HEAD anywhere. Any other request is refused." },
        { host: "files.example.com", text: "files.example.com: refuses any method within /downloads/private; otherwise allows GET within /downloads. The first matching rule decides, and any other request is refused." },
      ],
    });
  });

  it("exactly eight hosts is the cap: eight lines and no more-line", () => {
    const out = hostRuleLines(simple(8), 8);
    expect(out.lines.map((l) => l.text)).toEqual([1, 2, 3, 4, 5, 6, 7, 8].map(simpleLine));
    expect(out.more).toBeUndefined();
  });

  it("over the cap: the first eight, then the count of the rest, singular for one", () => {
    const nine = hostRuleLines(simple(9), 8);
    expect(nine.lines.map((l) => l.text)).toEqual([1, 2, 3, 4, 5, 6, 7, 8].map(simpleLine));
    expect(nine.more).toBe("and 1 more host with rules");
    const eleven = hostRuleLines(simple(11), 8);
    expect(eleven.lines).toHaveLength(8);
    expect(eleven.more).toBe("and 3 more hosts with rules");
  });

  it("no cap given: every host", () => {
    expect(hostRuleLines(simple(11)).lines).toHaveLength(11);
    expect(hostRuleLines(simple(11)).more).toBeUndefined();
  });

  it("a hold rule says a person decides; a second rule on the host makes it a first-match line", () => {
    const sets = [{ rules: [rule("deploy.example.com", "hold", ["POST"], ["/releases"]), rule("deploy.example.com", "allow", ["GET"]), rule("api.example.com", "allow", ["GET"], ["/public"])] }];
    expect(texts(sets)).toEqual([
      "deploy.example.com: holds POST within /releases for a person to decide; otherwise allows GET anywhere. The first matching rule decides, and any other request is refused.",
      "api.example.com: allows GET within /public. Any other request is refused.",
    ]);
  });

  it("two sets naming one host: one line, each set's clauses, then the stricter-wins sentence; a host one set names stays plain", () => {
    const sets = [{ rules: [rule("api.example.com", "allow", ["GET", "POST"], ["/v1"]), rule("registry.example.com", "allow", ["GET", "HEAD"])] }, { rules: [rule("api.example.com", "allow", ["GET"], ["/v1"])] }];
    expect(texts(sets)).toEqual([
      "api.example.com: 2 rule sets name this host. Set 1: allows GET, POST within /v1. Set 2: allows GET within /v1. Each set refuses what it does not list. A request is refused if any set refuses it. If none refuses it, it is held for a person if any set holds it, and allowed otherwise.",
      "registry.example.com: allows GET, HEAD anywhere. Any other request is refused.",
    ]);
  });

  it("joins several paths with a comma and a space, and a long path stays whole", () => {
    expect(texts([{ rules: [rule("registry-mirror.internal.example.com", "allow", ["GET", "HEAD"], ["/artifacts/releases/2026/q4", "/signed"])] }])).toEqual([
      "registry-mirror.internal.example.com: allows GET, HEAD within /artifacts/releases/2026/q4, /signed. Any other request is refused.",
    ]);
  });

  it.each([["absent", undefined], ["no sets", []], ["a set with no rules", [{ rules: [] }]]])("%s: no lines and no more-line", (_name, sets) => {
    expect(hostRuleLines(sets as HostRuleSet[] | undefined, 8)).toEqual({ lines: [] });
  });
});
