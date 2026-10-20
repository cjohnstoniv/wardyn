/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it } from "vitest";
import { HttpError } from "../../../../lib/api/core";
import { SCHEDULE_REASONS, SCHEDULE_REFUSAL } from "../../../../lib/schedule-refusals";
import { NR_RUN } from "../../../wardyn/copy/new-run/run";
import { refusalText } from "./refusal-text";

const refused = (message: string, reason: string) => new HttpError(422, message, reason);

describe("refusalText", () => {
  it("keeps the server's own sentence, which names the times", () => {
    const sentence = SCHEDULE_REFUSAL.TOO_FAR("2026-10-27 15:00 UTC", "2026-10-26 14:00 UTC", 24);
    expect(refusalText(refused(sentence, "start_at_too_far"))).toBe(sentence);
  });

  it("gives every start-time refusal that came back as its code alone a sentence, never the code", () => {
    for (const reason of SCHEDULE_REASONS) {
      for (const bare of [reason, "", "Unprocessable Entity"]) {
        const text = refusalText(refused(bare, reason));
        expect(text, `${reason} / ${bare}`).not.toBe(reason);
        expect(text, `${reason} / ${bare}`).toMatch(/\s/);
      }
    }
    expect(refusalText(refused("start_at_past", "start_at_past"))).toBe(NR_RUN.START_PAST);
    expect(refusalText(refused("start_at_too_far", "start_at_too_far"))).toBe(NR_RUN.START_FAR);
    expect(refusalText(refused("", "scheduled_runs_off"))).toBe(SCHEDULE_REFUSAL.OFF());
  });

  it("leaves every other refusal as it came", () => {
    expect(refusalText(refused("runner_pool_full", "runner_pool_full"))).toBe("runner_pool_full");
    expect(refusalText(new Error("boom"))).toBe("boom");
  });
});
