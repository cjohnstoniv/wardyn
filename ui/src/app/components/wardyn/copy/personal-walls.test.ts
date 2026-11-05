/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it } from "vitest";
import { existsSync, readFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { CHANGE_HEADING, FIELD_LABEL, POLICY_TAB } from "../../screens/run-detail/policy-tab-copy";
import { WHY_CARD } from "../../../lib/why-card-copy";
import { NR_TOOLS } from "./new-run/tools";
import { APPROVAL_WALLS, GITHUB_CAPABILITY_GROUPS, NR_WALLS, YOUR_WALLS } from "./personal-walls";

// T4-a and T4-b, character for character as the owner approved them on board R15, and T4-c on board R16 (PLAN §6.12,
// §6.13). Editing one is a
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
  [
    "restart note",
    YOUR_WALLS.RESTART_NOTE,
    "A restarted run takes on what you add to Never these sites, Credentials kept out and Refuse every push. Any other change, including Clipboard and files, tool rules, paths never pushed and Stop when idle, needs a new run. A restart never removes a limit the run started with.",
  ],
  ["UXR-14a", YOUR_WALLS.REMOVE_ALL_TITLE, "Remove all your walls?"],
  ["UXR-14b", YOUR_WALLS.REMOVE_ALL_BODY, "Your runs go back to what your organisation allows. This also removes the walls you set from the CLI."],
  ["UXR-14c", YOUR_WALLS.CANCEL, "Cancel"],
  ["UXR-18a", YOUR_WALLS.BARRIER_NONE, "No minimum"],
  ["UXR-18b", YOUR_WALLS.UNLISTED_ORG, "Your organisation's choice"],
  ["UXR-33", YOUR_WALLS.SITE_HINT, "Enter a host, like github.com or *.github.com."],
  ["UXR-40", YOUR_WALLS.CLIPBOARD_HINT, "Copy and paste apply to app desktops in containers; VM desktops have no copy and paste yet. Upload and download also apply to a Linux VM desktop's Files."],
  ["P-22", YOUR_WALLS.PUSHES, "Pushes"],
  ["P-23", YOUR_WALLS.REFUSE_PUSH, "Refuse every push"],
  ["P-24", YOUR_WALLS.REFUSE_PUSH_HELP, "Refuses every push the run makes: new commits, branch and tag changes, and deletes. The run can still fetch."],
  ["T4-a25", YOUR_WALLS.KINDS.cloud_sts, "Cloud credentials"],
  ["T4-a26", YOUR_WALLS.KINDS.ssh_key, "SSH key"],
  ["T4-b01", NR_WALLS.ONLY_LISTED, "Only the sites listed"],
  ["T4-b02", NR_WALLS.ONLY_LISTED_HELP, "This run reaches only the listed sites and what its setup needs. Your organisation allows more."],
  ["T4-b03", NR_WALLS.LEAVE_OUT("pypi.org"), "Your walls leave out pypi.org."],
  ["G4a01", NR_WALLS.APPLY_SITES_LISTED(4), "4 listed sites, plus what the run's setup needs"],
  ["G4a02", NR_WALLS.APPLY_SITES_LISTED(1), "1 listed site, plus what the run's setup needs"],
  ["G4a03", NR_WALLS.APPLY_SITES_SETUP_ONLY, "only the sites the run's setup needs"],
  ["G4a04", NR_WALLS.APPLY_SITES_NEVER(2), "2 sites never reached"],
  ["G4a05", NR_WALLS.APPLY_CREDENTIAL_LIMITS(2), "2 credential restrictions"],
  ["G4a06", NR_WALLS.APPLY_CREDENTIAL_LIMITS(1), "1 credential restriction"],
  ["G4a07", NR_WALLS.APPLY_BARRIER("Wall"), "barrier at least Wall"],
  ["G4a08", NR_WALLS.CHANGE_WALLS, "Change your walls"],
  ["T4-b04", `${NR_WALLS.LEAVE_OUT_BEFORE}pypi.org${NR_WALLS.LEAVE_OUT_AFTER}`, "Your walls leave out pypi.org."],
  ["T4-b07", NR_WALLS.REVIEW_SITES(3), "3 sites · anything else refused"],
  ["T4-b08", NR_WALLS.REVIEW_SITE_ONE, "1 site · anything else refused"],
  ["T4-b05", NR_TOOLS.NET_POLICY_ALWAYS_DENY, "Anything else is refused and logged."],
  ["T4-b06", NR_TOOLS.NET_POLICY_ALLOW_ALL, "Any other public host is allowed, except ones your organisation blocks."],
  ["T4-c01", CHANGE_HEADING.wallsOwn, "Your walls"],
  ["T4-c02", CHANGE_HEADING.wallsOther("Lena"), "Lena's walls"],
  ["T4-c03", FIELD_LABEL.allowed_domains, "Allowed hosts"],
  ["T4-c04", FIELD_LABEL.desktop, "Clipboard and files"],
  ["T4-c05", POLICY_TAB.chipRemoved, "Removed at start"],
  ["T4-c06", POLICY_TAB.chipAdded, "Added at start"],
  ["T4-c07", CHANGE_HEADING.source_control, "Added so the run can reach its code"],
  ["T4-c08", WHY_CARD.SET_BY, "Set by"],
  ["T4-c09", WHY_CARD.PERSONAL, "Your walls"],
  ["T4-c10", WHY_CARD.OWNER, "Owner"],
  ["T4-c11", WHY_CARD.CAN_CHANGE, "Can change"],
  ["T4-c12", WHY_CARD.CAN_CHANGE_PERSONAL, "You can, in Settings."],
  ["T4-c13", WHY_CARD.COPY_DETAILS, "Copy details"],
  ["T4-c14", APPROVAL_WALLS.OWN_BEFORE + "{host}" + APPROVAL_WALLS.OWN_AFTER, "Your walls block {host}. Change them in Settings."],
  ["T4-c15", APPROVAL_WALLS.OTHER_BEFORE("Lena") + "{host}" + APPROVAL_WALLS.OTHER_AFTER, "Lena's walls block {host} for this run."],
];

describe("Your walls copy", () => {
  it.each(CANON)("%s is the approved board string", (_id, actual, canon) => {
    expect(actual).toBe(canon);
  });
});

describe("the owner's approval refusal", () => {
  // internal/api/personal_walls.go words the same refusal for the owner; the two must read alike.
  it("reads as the sentence the server sends for a denied host", () => {
    expect(APPROVAL_WALLS.OWN_BEFORE + "a.evil.example" + APPROVAL_WALLS.OWN_AFTER).toBe("Your walls block a.evil.example. Change them in Settings.");
  });
});

function repoRoot(): string {
  let dir = resolve(process.cwd());
  for (let i = 0; i < 8; i++) {
    if (existsSync(join(dir, "go.mod"))) return dir;
    dir = dirname(dir);
  }
  throw new Error("go.mod not found walking up from " + process.cwd());
}

describe("the GitHub capability checklist", () => {
  const src = readFileSync(join(repoRoot(), "internal/ghscope/capability.go"), "utf8");
  const consts = new Map<string, string>();
  for (const m of src.matchAll(/^\s*(Cap\w+)\s+Capability = "([^"]+)"/gm)) consts.set(m[1], m[2]);

  it("is exactly the grantable set in internal/ghscope/capability.go", () => {
    const block = /var grantableCapabilities = map\[Capability\]bool\{([\s\S]*?)\n\}/.exec(src);
    expect(block).not.toBeNull();
    const grantable = [...block![1].matchAll(/(Cap\w+):\s*true/g)].map((m) => consts.get(m[1]));
    expect(GITHUB_CAPABILITY_GROUPS.flatMap((g) => g.caps.map((c) => c.cap)).sort()).toEqual([...grantable].sort());
  });

  it("words each capability as the server does", () => {
    const labels = new Map<string, string>();
    const block = /var labels = map\[Capability\]string\{([\s\S]*?)\n\}/.exec(src)![1];
    for (const m of block.matchAll(/(Cap\w+):\s*"([^"]+)"/g)) labels.set(consts.get(m[1])!, m[2]);
    for (const c of GITHUB_CAPABILITY_GROUPS.flatMap((g) => g.caps)) expect(c.label).toBe(labels.get(c.cap));
  });
});
