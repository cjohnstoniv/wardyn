/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The route's seeding reads: `?template=` and `?again=` become a draft seeded
// from the server's saved document. A refused read reaches the seed-failed screen.

import type { SeedLoaders } from "../../new-run-route";
import type { RunIntent } from "../../draft/run-intent";
import { launchIntentSource, templateSource } from "./shape-sources";

// The document door has already checked the intent's fields; the draft type
// only names them.
const intentOf = (document: { intent: object }) => document.intent as RunIntent;

export const seedLoaders: SeedLoaders = {
  template: async ({ id, revision }, signal) => {
    const t = await templateSource.read(id, revision, signal);
    return {
      origin: { kind: "template", id: t.id, revision: t.revision, name: t.name, templateKind: t.kind },
      intent: intentOf(t.document),
    };
  },
  again: async (runId, signal) => {
    const found = await launchIntentSource.read(runId, signal);
    return { origin: { kind: "again", runId, title: "" }, intent: intentOf(found.document) };
  },
};
