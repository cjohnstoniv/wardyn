/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it } from "vitest";
import { NR_TOOLS } from "./new-run/tools";
import { YOUR_WALLS } from "./personal-walls";

// T4-a and T4-b, character for character as the owner approved them on board R15 (PLAN §6.12, §6.13). Editing one is a
// copy change and needs the mock's owner sign-off.
const CANON: [id: string, actual: string, canon: string][] = [
  ["T4-a01", YOUR_WALLS.TITLE, "Your walls"],
  ["T4-a02", YOUR_WALLS.LEDE, "Limits you set on every run you start. They only narrow what your organisation allows."],
  ["T4-a03", YOUR_WALLS.SITES, "Sites"],
  ["T4-a04", YOUR_WALLS.SITES_ANY, "Any site your organisation allows"],
  ["T4-a05", YOUR_WALLS.SITES_ONLY, "Only these sites"],
  ["T4-a06", YOUR_WALLS.SITES_ONLY_HINT, "Your runs still reach what their own setup needs: repositories, model and tools."],
  ["T4-a07", YOUR_WALLS.NEVER, "Never these sites"],
  ["T4-a08", YOUR_WALLS.ADD_SITE, "Add a site"],
  ["T4-a09", YOUR_WALLS.UNLISTED, "When a site is not listed"],
  ["T4-a10", YOUR_WALLS.UNLISTED_BLOCK, "Block it"],
  ["T4-a11", YOUR_WALLS.UNLISTED_ASK, "Block and ask for review"],
  ["T4-a12", YOUR_WALLS.UNLISTED_HOLD, "Hold for review"],
  ["T4-a13", YOUR_WALLS.BARRIER, "Barrier at least"],
  ["T4-a14", YOUR_WALLS.CLIPBOARD, "Clipboard and files"],
  ["T4-a15", YOUR_WALLS.PASTE_IN, "Block paste in"],
  ["T4-a16", YOUR_WALLS.COPY_OUT, "Block copy out"],
  ["T4-a17", YOUR_WALLS.UPLOAD, "Block upload"],
  ["T4-a18", YOUR_WALLS.DOWNLOAD, "Block download"],
  ["T4-a19", YOUR_WALLS.CREDENTIALS, "Credentials kept out"],
  ["T4-a20", YOUR_WALLS.ADD_KIND, "Add a kind"],
  ["T4-a21", YOUR_WALLS.SAVE, "Save walls"],
  ["T4-a22", YOUR_WALLS.REMOVE_ALL, "Remove all walls"],
  ["T4-a23", YOUR_WALLS.SAVED, "Saved. Runs you start from now on use these walls."],
  ["T4-a24", YOUR_WALLS.RESTART_NOTE, "Runs already started keep their sites. Blocked sites, clipboard and credentials also apply when they restart."],
  ["T4-a25", YOUR_WALLS.KINDS.cloud_sts, "Cloud credentials"],
  ["T4-a26", YOUR_WALLS.KINDS.ssh_key, "SSH key"],
  ["T4-b01", NR_TOOLS.ONLY_LISTED, "Only the sites listed"],
  ["T4-b02", NR_TOOLS.ONLY_LISTED_HELP, "This run reaches only the listed sites and what its setup needs. Your organisation allows more."],
  ["T4-b03", NR_TOOLS.WALLS_LEAVE_OUT("pypi.org"), "Your walls leave out pypi.org."],
  ["T4-b04", `${NR_TOOLS.WALLS_LEAVE_OUT_BEFORE}pypi.org${NR_TOOLS.WALLS_LEAVE_OUT_AFTER}`, "Your walls leave out pypi.org."],
  ["T4-b05", NR_TOOLS.NET_POLICY_ALWAYS_DENY, "Anything else is refused and logged."],
  ["T4-b06", NR_TOOLS.NET_POLICY_ALLOW_ALL, "Any other public host is allowed, except ones your organisation blocks."],
];

describe("Your walls copy", () => {
  it.each(CANON)("%s is the approved board string", (_id, actual, canon) => {
    expect(actual).toBe(canon);
  });
});
