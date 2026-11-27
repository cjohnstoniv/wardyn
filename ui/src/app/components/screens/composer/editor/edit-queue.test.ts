/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { afterEach, describe, expect, it, vi } from "vitest";
import { assistant } from "../../../../lib/api/assistant";
import { afterEdits, flushEdits, hasLine, onEditFailure, postEdit, releaseEdits } from "./edit-queue";

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
    edits.mockRejectedValue(new Error("refused"));
    one("q-fail");
    await expect(flushEdits("q-fail")).resolves.toBe("refused");
    edits.mockResolvedValue({ seq: 1 });
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

  it("sends the last draft again when a caller waits and the last post had failed, and answers with that attempt", async () => {
    edits.mockRejectedValueOnce(new Error("refused")).mockResolvedValueOnce({ seq: 1 });
    postEdit("q-retry", { a: 1 }, new Map([["a", "1"]]));
    await expect(flushEdits("q-retry")).resolves.toBeNull();
    expect(edits).toHaveBeenCalledTimes(2);
    expect(edits.mock.calls[1]).toEqual(edits.mock.calls[0]);
    await Promise.resolve();
    expect(hasLine("q-retry")).toBe(false);
  });

  it("keeps the failure when the one retry fails too, and tries once more at the next wait", async () => {
    edits.mockRejectedValue(new Error("still refused"));
    one("q-retry-fail");
    await expect(flushEdits("q-retry-fail")).resolves.toBe("still refused");
    expect(edits).toHaveBeenCalledTimes(2);
    await expect(flushEdits("q-retry-fail")).resolves.toBe("still refused");
    expect(edits).toHaveBeenCalledTimes(3);
    edits.mockResolvedValue({ seq: 1 });
    await expect(flushEdits("q-retry-fail")).resolves.toBeNull();
  });

  it("runs a job only once the session's edits are settled, and not at all when they could not be saved", async () => {
    let answer: () => void = () => undefined;
    edits.mockImplementation(() => new Promise((ok) => (answer = () => ok({ seq: 1 }))));
    one("q-job");
    const job = vi.fn().mockResolvedValue("sent");
    const done = afterEdits("q-job", job);
    await Promise.resolve();
    expect(job).not.toHaveBeenCalled();
    answer();
    await expect(done).resolves.toEqual({ value: "sent" });
    edits.mockReset().mockRejectedValue(new Error("refused"));
    one("q-job-fail");
    const other = vi.fn();
    await expect(afterEdits("q-job-fail", other)).resolves.toEqual({ failure: "refused" });
    expect(other).not.toHaveBeenCalled();
  });

  it("names a failed field again in the next post, and drops it once a post that names it is taken", async () => {
    edits.mockRejectedValueOnce(new Error("refused")).mockResolvedValue({ seq: 1 });
    postEdit("q-union", { allowed_domains: ["a.example"] }, new Map([["allowed_domains", '["a.example"]']]));
    postEdit("q-union", { allowed_domains: ["a.example"], allowed_methods: ["GET"] }, new Map([["allowed_methods", '["GET"]']]));
    await flushEdits("q-union");
    const named = (n: number) => edits.mock.calls[n][1].changed.map((c) => c.field).sort();
    expect(edits).toHaveBeenCalledTimes(2);
    expect(named(1)).toEqual(["allowed_domains", "allowed_methods"]);
    postEdit("q-union", { allowed_methods: ["GET", "HEAD"] }, new Map([["allowed_methods", '["GET","HEAD"]']]));
    await flushEdits("q-union");
    expect(named(2)).toEqual(["allowed_methods"]);
  });

  it("lets go of a session's line, failure and all, when the session ends", async () => {
    edits.mockRejectedValue(new Error("refused"));
    one("q-end");
    await flushEdits("q-end");
    expect(hasLine("q-end")).toBe(true);
    releaseEdits("q-end");
    expect(hasLine("q-end")).toBe(false);
    await expect(flushEdits("q-end")).resolves.toBeNull();
  });
});
