/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The run page's Model usage card. Counts are the provider's own figures, so the
// strings say "reported" and never add Input and Cache read together.

const plural = (n: number, one: string, many: string) => `${n.toLocaleString("en-US")} ${n === 1 ? one : many}`;

// What a vendor's Input figure contains. Anthropic and Amazon Bedrock leave the cache tokens out of Input;
// OpenAI counts them inside it. A vendor that reports no cache figure gets no note.
const VENDOR_NAME: Record<string, string> = {
  anthropic: "Anthropic",
  bedrock: "Amazon Bedrock",
  openai: "OpenAI",
  ollama: "Ollama",
};
const CACHE_OUTSIDE_INPUT = ["anthropic", "bedrock"];
const CACHE_INSIDE_INPUT = ["openai"];

// One phrase per uncounted reason, written to follow a count ("3 sent no usage figures").
const REASON: Record<string, string> = {
  stream_without_usage: "sent no usage figures",
  no_usage: "sent no usage figures",
  parse_error: "had usage figures Wardyn could not read",
  body_oversize: "had a response larger than Wardyn reads",
  undecodable: "arrived compressed in a way Wardyn can't read",
  meter_error: "failed to count",
};

export const MODEL_USAGE = {
  title: "Model usage",
  live: "Live",
  finished: "Run finished",
  labelInput: "Input",
  labelOutput: "Output",
  labelCacheRead: "Cache read",
  labelCalls: "Calls",
  vendorName: (vendor: string): string => VENDOR_NAME[vendor] ?? vendor,
  calls: (n: number) => plural(n, "call", "calls"),
  // The one-model line above the totals.
  oneModel: (model: string, provider: string, n: number) => `${model} · ${provider} · ${plural(n, "call", "calls")}`,
  // Which endpoint a row is for: the provider row when one matches, then the host the calls went to.
  endpoint: (provider: string, host: string) => (provider === "" || provider === host ? host : `${provider} · ${host}`),
  noModelName: "model not reported",

  // "Reported so far", then one clause per cache rule among the vendors that have figures.
  footnote: (vendors: string[]): string => {
    const named = (list: string[]) => vendors.filter((v) => list.includes(v)).map((v) => VENDOR_NAME[v]);
    const outside = named(CACHE_OUTSIDE_INPUT);
    const inside = named(CACHE_INSIDE_INPUT);
    const parts = ["Reported so far, as each provider counts them."];
    if (outside.length > 0) parts.push(`${outside.join(" and ")}: Cache read is not part of Input.`);
    if (inside.length > 0) parts.push(`${inside.join(" and ")}: Input includes Cache read.`);
    return parts.join(" ");
  },

  // "5 calls not counted" + " (3 sent no usage figures, 2 arrived compressed ...)".
  uncountedHead: (n: number) => `${plural(n, "call", "calls")} not counted`,
  uncountedWhy: (reasons: Record<string, number>, total: number): string => {
    const byPhrase = new Map<string, number>();
    for (const [code, n] of Object.entries(reasons)) {
      const phrase = REASON[code] ?? "had another reason";
      if (n > 0) byPhrase.set(phrase, (byPhrase.get(phrase) ?? 0) + n);
    }
    const rows = [...byPhrase.entries()].sort((a, b) => b[1] - a[1] || a[0].localeCompare(b[0]));
    if (rows.length === 0) return "";
    if (rows.length === 1) return total === 1 ? `(${rows[0][0]})` : `(all ${rows[0][0]})`;
    return `(${rows.map(([phrase, n]) => `${n.toLocaleString("en-US")} ${phrase}`).join(", ")})`;
  },
  cutOffHead: (n: number) => `${plural(n, "call", "calls")} cut off`,
  cutOffTail: "before the answer ended; the tokens reported so far are counted",
  encryptedHead: (n: number) => plural(n, "encrypted connection", "encrypted connections"),
  encryptedTail: (n: number) =>
    n === 1
      ? "to a model host was not opened; its tokens aren't counted"
      : "to model hosts were not opened; their tokens aren't counted",

  emptyLive: "No model calls counted yet.",
  emptyFinished: "No model calls were counted for this run.",
  noTokensLive: "No tokens counted yet.",
  noTokensFinished: "No tokens were counted for this run.",

  loadError: "Couldn't load right now.",
  retry: "Retry",
  // A finished run can still receive usage the runner held back; one button reads it again.
  refresh: "Refresh",
  stale: "Couldn't refresh. Showing the last reading.",
  // The server has no model-usage route (a version it predates) or no store for it.
  unsupported: "This server doesn't report model usage.",
  // The server answered and refused this run: no access, or the run is gone.
  unavailable: "Model usage isn't available for this run.",
} as const;

// The reason codes and vendor dialects the card words, for the Go-parity test.
export const MODEL_USAGE_REASON_CODES = Object.keys(REASON);
export const MODEL_USAGE_VENDORS = Object.keys(VENDOR_NAME);
