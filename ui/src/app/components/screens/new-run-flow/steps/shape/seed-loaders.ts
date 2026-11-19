/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The route's seeding reads: `?template=` and `?again=` become a draft seeded
// from the server's saved document. A refused read reaches the seed-failed screen.

import type { SeedLoaders } from "../../new-run-route";
import type { RunIntent } from "../../draft/run-intent";
import { withLegacyRunModeInCarriers } from "../../document/legacy-run-mode";
import { launchIntentSource, templateSource } from "./shape-sources";

// The document door has already checked the intent's fields; the draft type
// only names them. Older run-mode fields go in the carriers, as a committed run
// document's do (withLegacyRunModeInCarriers), so the draft can be launched.
const intentOf = (document: { intent: object }) => withLegacyRunModeInCarriers(document.intent as RunIntent);

// A template's absent field is one it leaves for the person to set, so a
// template with no `interactive` has chosen no run mode and is seeded as written.
// The task it leaves out is written when it is used, so a background template converts without one.
const templateIntentOf = (document: { intent: object }) =>
  typeof (document.intent as RunIntent).interactive === "boolean" ? withLegacyRunModeInCarriers(document.intent as RunIntent, true) : (document.intent as RunIntent);

export const seedLoaders: SeedLoaders = {
  template: async ({ id, revision }, signal) => {
    const t = await templateSource.read(id, revision, signal);
    return {
      origin: { kind: "template", id: t.id, revision: t.revision, name: t.name, templateKind: t.kind },
      intent: templateIntentOf(t.document),
    };
  },
  again: async (runId, signal) => {
    const found = await launchIntentSource.read(runId, signal);
    return { origin: { kind: "again", runId, title: "" }, intent: intentOf(found.document) };
  },
};
