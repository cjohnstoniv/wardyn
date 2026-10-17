/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

/*
 * Unicode 11 widths (term-t9), hermetic. xterm's default table gives an emoji
 * one cell where tmux and glibc give two, so a line drifts. The stub writes
 * an emoji and X, then a DSR request, and xterm answers with its own cursor
 * position: column 4 with the wide emoji (ESC[1;4R), column 3 with the
 * narrow one (ESC[1;3R). The DSR goes through the stub and not real tmux,
 * which would answer it itself.
 */
import { test, expect, gotoConsole, navToRoute } from "./fixtures";
import { attachModeFrame, findRunningFixture, stubAttachSocket, stubAttachTicket, stubInteractiveRun } from "./attach-stub";

test("an emoji takes two cells, so the cursor reply is column 4", async ({ page }) => {
  const { id: runId } = await findRunningFixture(page);
  await stubInteractiveRun(page, runId);
  await stubAttachTicket(page, runId);
  const replies: string[] = [];
  await stubAttachSocket(page, (_n, ws) => {
    ws.onMessage((msg) => {
      if (Buffer.isBuffer(msg)) replies.push(msg.toString("latin1"));
      else if (typeof msg === "string") replies.push(msg);
    });
    ws.send(attachModeFrame(false));
    ws.send(Buffer.from("\x1b[2J\x1b[H\u{1F600}X\x1b[6n", "utf8"));
  });
  await gotoConsole(page);
  await navToRoute(page, `/runs/${runId}`);
  await expect(page.locator(".xterm-screen").first()).toBeVisible();
  await expect.poll(() => replies.join("").match(/\x1b\[\d+;\d+R/)?.[0]).toBe("\x1b[1;4R");
});
