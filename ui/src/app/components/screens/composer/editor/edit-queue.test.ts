/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { afterEach, describe, expect, it, vi } from "vitest";
import { assistant } from "../../../../lib/api/assistant";
import { flushEdits, hasLine, onEditFailure, postEdit } from "./edit-queue";

const edits = vi.spyOn(assistant, "postEdits");
afterEach(() => edits.mockReset());
const one = (id: string) => postEdit(id, { a: 1 }, new Map([["a", "1"]]));

describe("the edit queue", () => {
  it("forgets a session's line once it has drained with nothing to show", async () => {
    edits.mockResolvedValue({ seq: 1 });
    one("q-ok");
    expect(hasLine("q-ok")).toBe(true);
    await expect(flushEdits("q-ok")).resolves.toBeNull();
    await Promise.resolve();
    expect(hasLine("q-ok")).toBe(false);
  });

  it("keeps a line that holds a failure until a later post is taken, and while an editor listens", async () => {
    edits.mockRejectedValueOnce(new Error("refused")).mockResolvedValue({ seq: 1 });
    one("q-fail");
    await expect(flushEdits("q-fail")).resolves.toBe("refused");
    await Promise.resolve();
    expect(hasLine("q-fail")).toBe(true);
    const seen: (string | null)[] = [];
    const stop = onEditFailure("q-fail", (f) => seen.push(f));
    one("q-fail");
    await flushEdits("q-fail");
    await Promise.resolve();
    expect(seen).toEqual(["refused", null]);
    expect(hasLine("q-fail")).toBe(true);
    stop();
    expect(hasLine("q-fail")).toBe(false);
  });
});
