/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it } from "vitest";
import { newRunHref, parseNewRunSearch, type NewRunFrom, type NewRunLocation } from "./new-run-url";

const T = "d1111111-1111-4111-8111-111111111111";
const R = "e1111111-1111-4111-8111-111111111111";
const parse = (query: string) => parseNewRunSearch(new URLSearchParams(query));
const START = { location: { screen: "start" }, normalised: true };

// The grammar's table, as [query, location, from].
const TABLE: [string, NewRunLocation, NewRunFrom][] = [
  ["", { screen: "start" }, {}],
  ...(["shape", "run", "harnesses", "files", "tools"] as const).map((step): [string, NewRunLocation, NewRunFrom] =>
    [`step=${step}`, { screen: "step", step }, {}]),
  ["step=document&format=yaml", { screen: "document", format: "yaml" }, {}],
  ["step=document&format=json", { screen: "document", format: "json" }, {}],
  ["step=review", { screen: "review", view: "pretty" }, {}],
  ["step=review&view=yaml", { screen: "review", view: "yaml" }, {}],
  ["step=review&view=json", { screen: "review", view: "json" }, {}],
  [`template=${T}&step=review`, { screen: "review", view: "pretty" }, { template: { id: T } }],
  [`template=${T}&rev=4&step=review`, { screen: "review", view: "pretty" }, { template: { id: T, revision: 4 } }],
  [`again=${R}&step=review`, { screen: "review", view: "pretty" }, { again: R }],
  [`again=${R}&step=files`, { screen: "step", step: "files" }, { again: R }],
  [`launched=${R}`, { screen: "launched", runId: R }, {}],
];

describe("New Run URL grammar (U8)", () => {
  it.each(TABLE)("?%s parses to its screen", (query, location, from) => {
    expect(parse(query)).toEqual({ location, ...from, normalised: false });
  });

  it.each(TABLE)("?%s is the canonical href of its screen", (query, location, from) => {
    expect(newRunHref(location, from)).toBe(query ? `/runs/new?${query}` : "/runs/new");
  });

  it("opens Review by default for a template or a run to run again", () => {
    expect(parse(`template=${T}`)).toEqual({ location: { screen: "review", view: "pretty" }, template: { id: T }, normalised: false });
    expect(parse(`again=${R}`)).toEqual({ location: { screen: "review", view: "pretty" }, again: R, normalised: false });
  });

  it("never carries a template or run to run again into Start or Launched", () => {
    const from = { template: { id: T, revision: 4 }, again: R };
    expect(newRunHref({ screen: "start" }, from)).toBe("/runs/new");
    expect(newRunHref({ screen: "launched", runId: R }, from)).toBe(`/runs/new?launched=${R}`);
    expect(parse(newRunHref({ screen: "start" }, { template: { id: T } })).location).toEqual({ screen: "start" });
  });

  it("lets launched win over every other key", () => {
    expect(parse(`step=files&again=${R}&launched=${R}`)).toEqual({ location: { screen: "launched", runId: R }, normalised: false });
  });

  it.each([
    "step=review2", "step=Files", "step=start", "step=document", "step=document&format=xml", "step=review&view=raw",
    `template=not-a-uuid`, `template=${T}&rev=0`, `template=${T}&rev=x`, `template=${T}&again=${R}`,
    "again=123", "launched=", "launched=../admin",
    "foo=1", "step=shape&foo=1", "rev=3", "step=shape&format=yaml", "step=review&format=json", "step=document&format=yaml&view=pretty",
    "step=shape&step=run", `again=${R}&rev=2`, `launched=${R}&junk=1`, `launched=${R}&launched=${R}`,
  ])("?%s is outside the grammar: Start, replacing the URL", (query) => {
    expect(parse(query)).toEqual(START);
  });
});
