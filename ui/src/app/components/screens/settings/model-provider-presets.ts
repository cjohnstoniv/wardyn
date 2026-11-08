/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Gateway presets for a new Your own endpoint provider. A preset prefills the fields it names and touches
// nothing else; every field stays editable, and nothing records which preset was used.
import { PROVIDER_EDITOR } from "../../../lib/model-providers-copy";
import type { HarnessDraft, HeaderDraft, ProviderDraft } from "./model-provider-draft";

export type PresetId = "litellm" | "portkey_self_hosted" | "portkey_hosted" | "openrouter" | "other";

export const PRESET_IDS: readonly PresetId[] = ["litellm", "portkey_self_hosted", "portkey_hosted", "openrouter", "other"];

const PORTKEY_PROVIDER = "x-portkey-provider";

// A row replaces one of the same name (compared without case), in place; otherwise it goes last.
function withHeader(rows: HeaderDraft[], row: HeaderDraft): HeaderDraft[] {
  const at = rows.findIndex((r) => r.name.trim().toLowerCase() === row.name.toLowerCase());
  return at < 0 ? [...rows, row] : rows.map((r, i) => (i === at ? row : r));
}

// Patches to harnesses by id; a harness the draft does not have is left out, never created.
function withHarnesses(d: ProviderDraft, patches: Record<string, Partial<HarnessDraft>>): ProviderDraft {
  const harnesses = { ...d.harnesses };
  for (const [id, patch] of Object.entries(patches)) if (harnesses[id]) harnesses[id] = { ...harnesses[id], ...patch };
  return { ...d, harnesses };
}

export function applyPreset(draft: ProviderDraft, id: PresetId): ProviderDraft {
  switch (id) {
    case "litellm":
      return withHarnesses(draft, { "claude-code": { path: "/" }, "codex-cli": { path: "/v1" } });
    case "portkey_self_hosted":
      return withHarnesses(
        { ...draft, headers: withHeader(draft.headers, { name: PORTKEY_PROVIDER, value: "anthropic" }) },
        { "claude-code": { ticked: true, path: "/" }, "codex-cli": { ticked: false } },
      );
    case "portkey_hosted":
      return {
        ...draft,
        authHeader: "x-portkey-api-key",
        authFormat: "{token}",
        headers: withHeader(draft.headers, { name: PORTKEY_PROVIDER, value: "", placeholder: PROVIDER_EDITOR.PORTKEY_SLUG_PLACEHOLDER }),
      };
    case "openrouter":
      return withHarnesses({ ...draft, baseUrl: "https://openrouter.ai/api" }, { "claude-code": { path: "/" }, "codex-cli": { path: "/v1" } });
    case "other":
      return draft;
  }
}
