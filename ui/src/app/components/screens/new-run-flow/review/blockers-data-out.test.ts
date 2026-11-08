/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// A two-way computer folder on a run whose data_out rule keeps content in: the server refuses the check
// (422 computer_folder_data_out) and the review prints its sentence as the blocker, as sent.
import { describe, expect, it } from "vitest";
import { buildBlockers, type BlockerInput } from "./blockers";

const NOTHING: BlockerInput = { issues: [], unsupported: [], templateNeeds: [], refusal: null, setupItems: [], stale: false };
const SENTENCE = "local_folder.direction: this run's data_out rule keeps content in, so a computer folder can only go in; set direction to in";

describe("Review blockers: a computer folder under data_out", () => {
  it("prints the server's sentence as the one blocker, unchanged", () => {
    const out = buildBlockers({ ...NOTHING, refusal: { text: SENTENCE, reason: "computer_folder_data_out", field: "local_folder.direction" } });
    expect(out).toHaveLength(1);
    expect(out[0]).toMatchObject({ kind: "refusal", text: SENTENCE });
  });

  it("is a blocker whether or not the refusal names the field", () => {
    const out = buildBlockers({ ...NOTHING, refusal: { text: SENTENCE, reason: "computer_folder_data_out" } });
    expect(out.map((b) => b.text)).toEqual([SENTENCE]);
  });
});
