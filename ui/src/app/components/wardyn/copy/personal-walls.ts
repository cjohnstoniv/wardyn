/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Settings → Your walls (T4-a, owner-approved board R15). The canon strings are byte-equal to the
// board and pinned by personal-walls.test.ts; the four marked "not on the board" are the card's own
// failure and accessibility words. The card is imported directly by the lazy account screen.

export const YOUR_WALLS = {
  TITLE: "Your walls",
  LEDE: "Limits you set on every run you start. They only narrow what your organisation allows.",
  SITES: "Sites",
  SITES_ANY: "Any site your organisation allows",
  SITES_ONLY: "Only these sites",
  SITES_ONLY_HINT: "Your runs still reach what their own setup needs: repositories, model and tools.",
  NEVER: "Never these sites",
  ADD_SITE: "Add a site",
  SITE_HINT: "Enter a host, like github.com or *.github.com.",
  UNLISTED: "When a site is not listed",
  UNLISTED_ORG: "Your organisation's choice",
  UNLISTED_BLOCK: "Block it",
  UNLISTED_ASK: "Block and ask for review",
  UNLISTED_HOLD: "Hold for review",
  BARRIER: "Barrier at least",
  BARRIER_NONE: "No minimum",
  CLIPBOARD: "Clipboard and files",
  CLIPBOARD_HINT: "Copy and paste apply to app desktops in containers; VM desktops have no copy and paste yet. Upload and download also apply to a Linux VM desktop's Files.",
  PASTE_IN: "Block paste in",
  COPY_OUT: "Block copy out",
  UPLOAD: "Block upload",
  DOWNLOAD: "Block download",
  CREDENTIALS: "Credentials kept out",
  ADD_KIND: "Add a kind",
  SAVE: "Save walls",
  REMOVE_ALL: "Remove all walls",
  REMOVE_ALL_TITLE: "Remove all your walls?",
  REMOVE_ALL_BODY: "Your runs go back to what your organisation allows. This also removes the walls you set from the CLI.",
  CANCEL: "Cancel",
  SAVED: "Saved. Runs you start from now on use these walls.",
  RESTART_NOTE: "Runs already started keep their sites. Blocked sites, clipboard and credentials also apply when they restart.",
  // The credential kinds a person may keep out, in the words the policy summary uses (the board shows two).
  KINDS: {
    github_token: "GitHub access",
    api_key: "API key",
    git_pat: "Git access token",
    ssh_key: "SSH key",
    cloud_sts: "Cloud credentials",
    env_secret: "Environment secret",
    file_secret: "File secret",
  } as Record<string, string>,
  // Not on the board.
  LOAD_FAILED: "Couldn't load your walls.",
  RETRY: "Retry",
  SAVE_FAILED: "Couldn't save your walls.",
  REMOVE_SITE: (site: string) => `Remove ${site}`,
} as const;

// New Run's side of the walls (T4-b, board R15). Kept out of NR_TOOLS and NR_REVIEW, whose keys the New Run canon
// document freezes one for one. The switch sits under Network on step 5; the leave-out note names a host the
// person's own walls take out of the run, drawn with the host in monospace between its two halves.
export const NR_WALLS = {
  ONLY_LISTED: "Only the sites listed",
  ONLY_LISTED_HELP: "This run reaches only the listed sites and what its setup needs. Your organisation allows more.",
  LEAVE_OUT_BEFORE: "Your walls leave out ",
  LEAVE_OUT_AFTER: ".",
  LEAVE_OUT: (host: string | number) => `Your walls leave out ${host}.`,
  // Review's Network value for a run that reaches only the listed sites (board R15, T4-b).
  REVIEW_SITES: (n: string | number) => `${n} sites · anything else refused`,
  REVIEW_SITE_ONE: "1 site · anything else refused",
  // Not on the board: the walls step's own warning on Review for a knob the board draws no note for, with the knob
  // in the Policy tab's field words. A host the walls leave out has the board's LEAVE_OUT note instead.
  NARROWED: (knob: string | number) => `Your walls narrowed ${knob}.`,
  // The one neutral line above Review's groups and the Tools totals: each knob the person's saved walls name, as a
  // clause. A listed-sites clause says the run still reaches what its setup needs (YOUR_WALLS.SITES_ONLY_HINT is the
  // longer form, shown on hover).
  APPLY_BEFORE: "Your walls apply: ",
  APPLY_AFTER: ".",
  APPLY_JOIN: ", ",
  APPLY_SITES_LISTED: (n: string | number) => `${n} listed ${n === 1 ? "site" : "sites"}, plus what the run's setup needs`,
  APPLY_SITES_SETUP_ONLY: "only the sites the run's setup needs",
  APPLY_SITES_NEVER: (n: string | number) => `${n} ${n === 1 ? "site" : "sites"} never reached`,
  APPLY_BARRIER: (cls: string) => `barrier at least ${cls}`,
  APPLY_NO_TRANSFER: (what: string) => `no ${what}`,
  APPLY_TRANSFERS: { paste_in: "paste in", copy_out: "copy out", upload: "upload", download: "download" } as Record<string, string>,
  APPLY_OR: " or ",
  APPLY_CREDENTIAL_LIMITS: (n: string | number) => `${n} credential ${n === 1 ? "restriction" : "restrictions"}`,
  CHANGE_WALLS: "Change your walls",
} as const;

// T4-c's approval refusal (board R16): shown on a request whose host the run owner's walls deny, with the Why card
// under it. Each sentence is drawn with the host in monospace between its two halves. "in Settings" is where the
// board says "on Your account", which the console now calls Settings (the Why card's own Can change row says the same).
export const APPROVAL_WALLS = {
  OWN_BEFORE: "Your walls block ",
  OWN_AFTER: ". Change them in Settings.",
  OTHER_BEFORE: (owner: string) => `${owner}'s walls block `,
  OTHER_AFTER: " for this run.",
} as const;
