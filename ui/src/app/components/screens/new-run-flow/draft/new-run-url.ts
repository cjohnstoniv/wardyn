/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// New Run's URL grammar. The screen is addressed by the query; the draft is
// not, so a reload keeps only what the server holds (a template, a run's
// launch intent, a launched run).

import { NEW_RUN_STEPS, type NewRunStep } from "../lenses/step-issues";

export const NEW_RUN_PATH = "/runs/new";

export type NewRunLocation =
  | { screen: "start" }
  | { screen: "step"; step: NewRunStep }
  | { screen: "document"; format: "yaml" | "json" }
  | { screen: "review"; view: "pretty" | "yaml" | "json" }
  | { screen: "launched"; runId: string };

export interface NewRunFrom {
  template?: { id: string; revision?: number };
  again?: string;
}

export interface ParsedNewRunSearch extends NewRunFrom {
  location: NewRunLocation;
  /** The query was not in the grammar: show Start and replace the URL. */
  normalised: boolean;
}

const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;
const FORMATS = ["yaml", "json"] as const;
const VIEWS = ["pretty", "yaml", "json"] as const;
const START: ParsedNewRunSearch = { location: { screen: "start" }, normalised: true };

const oneOf = <T extends string>(set: readonly T[], value: string | null): T | undefined =>
  set.find((v) => v === value);

function stepLocation(step: string | null, q: URLSearchParams, fallback: NewRunLocation): NewRunLocation | undefined {
  if (step === null) return fallback;
  const guided = oneOf(NEW_RUN_STEPS, step);
  if (guided) return { screen: "step", step: guided };
  if (step === "document") {
    const format = oneOf(FORMATS, q.get("format"));
    return format && { screen: "document", format };
  }
  if (step === "review") {
    const raw = q.get("view");
    const view = raw === null ? "pretty" : oneOf(VIEWS, raw);
    return view && { screen: "review", view };
  }
  return undefined;
}

export function parseNewRunSearch(q: URLSearchParams): ParsedNewRunSearch {
  const launched = q.get("launched");
  if (launched !== null) {
    const grammar = new Set(["launched", "step", "format", "view", "template", "rev", "again"]);
    const keys = [...q.keys()];
    if (!UUID.test(launched) || keys.some((k) => !grammar.has(k)) || keys.filter((k) => k === "launched").length > 1) return START;
    return { location: { screen: "launched", runId: launched }, normalised: false };
  }
  const template = q.get("template");
  const again = q.get("again");
  if (template !== null && again !== null) return START;
  const from: NewRunFrom = {};
  if (template !== null) {
    const rev = q.get("rev");
    if (!UUID.test(template) || (rev !== null && !/^[1-9]\d{0,8}$/.test(rev))) return START;
    from.template = rev === null ? { id: template } : { id: template, revision: Number(rev) };
  }
  if (again !== null) {
    if (!UUID.test(again)) return START;
    from.again = again;
  }
  const seeded = template !== null || again !== null;
  const location = stepLocation(q.get("step"), q, seeded ? { screen: "review", view: "pretty" } : { screen: "start" });
  if (!location) return START;
  const known = new Set(["step", ...(template !== null ? ["template", "rev"] : []), ...(again !== null ? ["again"] : [])]);
  if (location.screen === "document") known.add("format");
  if (location.screen === "review") known.add("view");
  const keys = [...q.keys()];
  if (keys.some((k) => !known.has(k)) || new Set(keys).size !== keys.length) return START;
  return { location, ...from, normalised: false };
}

/** The canonical href of a New Run screen. Start and Launched name no template or run to run again. */
export function newRunHref(loc: NewRunLocation, from: NewRunFrom = {}): string {
  const q = new URLSearchParams();
  if (loc.screen === "start") return NEW_RUN_PATH;
  if (loc.screen === "launched") {
    q.set("launched", loc.runId);
    return `${NEW_RUN_PATH}?${q}`;
  }
  if (from.template) {
    q.set("template", from.template.id);
    if (from.template.revision !== undefined) q.set("rev", String(from.template.revision));
  }
  if (from.again) q.set("again", from.again);
  if (loc.screen === "step") q.set("step", loc.step);
  if (loc.screen === "document") {
    q.set("step", "document");
    q.set("format", loc.format);
  }
  if (loc.screen === "review") {
    q.set("step", "review");
    if (loc.view !== "pretty") q.set("view", loc.view);
  }
  const query = q.toString();
  return query ? `${NEW_RUN_PATH}?${query}` : NEW_RUN_PATH;
}
