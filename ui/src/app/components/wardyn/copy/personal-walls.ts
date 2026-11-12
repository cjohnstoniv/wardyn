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
  RESTART_NOTE:
    "A restarted run takes on what you add to Never these sites, Credentials kept out and Refuse every push. Any other change, including Clipboard and files, tool rules, paths never pushed and Stop when idle, needs a new run. A restart never removes a limit the run started with.",
  // Rc.12 wall presets: the shared form's Pushes section, shown in Your walls too.
  PUSHES: "Pushes",
  REFUSE_PUSH: "Refuse every push",
  REFUSE_PUSH_HELP: "Refuses every push the run makes: new commits, branch and tag changes, and deletes. The run can still fetch.",
  // The limits a walls document carries beyond sites, clipboard and credentials: capability lists, tool rules,
  // paths never pushed, a recording floor and an idle stop.
  LIMIT_NONE: "No limit",
  GITHUB_CAPS: "GitHub capabilities",
  ADO_CAPS: "Azure DevOps capabilities",
  CAPS_ONLY: "Only these capabilities",
  CAPS_HELP: "A run keeps only the capabilities on this list that it would have anyway. If none are left, the run is refused.",
  CAPS_COUNT: (n: number, total: number) => `${n} of ${total} selected`,
  CAPS_READS_ONLY: "Reads only",
  CAPS_EMPTY: "Pick at least one capability, or choose No limit.",
  CAPS_NO_PUSH: (cap: string, forge: string) => `Without \u201C${cap}\u201D, pushes to ${forge} through Wardyn's git broker are refused.`,
  CAPS_NO_PUSH_BROKER_OFF: "On an install with the git token broker off, a git token in the sandbox can still push; the run carries a warning that says so.",
  TOOLS: "Tool rules",
  TOOLS_HELP: "Each rule holds a tool for review, asks for a person's approval of each call of an MCP tool, or refuses it. Rules apply on runs whose tool calls wait for review, except approve_each, which the proxy enforces on any run. A rule never loosens one the run already has.",
  TOOLS_NONE: "No tool rules. Your runs keep their policy's.",
  TOOLS_STAR_HINT: "Type * as the tool to cover every tool no other rule here names.",
  TOOLS_TOOL: "Tool",
  TOOLS_EFFECT: "Effect",
  TOOLS_PLACEHOLDER: "Bash",
  TOOLS_ADD: "Add rule",
  TOOLS_BLANK: "Type a tool, or remove this rule.",
  TOOLS_TOOL_N: (n: number) => `Tool ${n}`,
  TOOLS_EFFECT_N: (n: number) => `Effect ${n}`,
  TOOLS_REMOVE_N: (n: number) => `Remove rule ${n}`,
  PATHS: "Paths never pushed",
  PATHS_HELP: "Refuses the push outright. Anchored at the repository root; ** crosses path segments.",
  PATHS_NONE: "No paths. Pushes follow the run's own push rules.",
  PATHS_ADD: "Add path",
  PATHS_BLANK: "Type a path, or remove this one.",
  PATHS_OFF_NOTE: "Every push is refused while Refuse every push is on, so these paths change nothing until it is off.",
  PATHS_ADO_NOTE: "On Azure DevOps a push is refused while the repository still holds a listed path, even when the push leaves it alone.",
  PATHS_BROKER_NOTE: "Paths never pushed are checked on pushes through Wardyn's git broker. A push over SSH, or with a git token in the sandbox on an install with the broker off, is not checked.",
  PATHS_PATH_N: (n: number) => `Path ${n}`,
  PATHS_REMOVE_N: (n: number) => `Remove path ${n}`,
  RECORDING: "Record at least",
  RECORDING_NONE: "No minimum",
  RECORDING_UNSET: "Your runs are recorded at the level their policy or organisation sets.",
  IDLE: "Stop when idle",
  IDLE_AFTER: "Stop after",
  IDLE_SUFFIX: "idle",
  IDLE_UNIT: (n: number, unit: "seconds" | "minutes") => (n === 1 ? unit.slice(0, -1) : unit),
  IDLE_HELP: "An interactive run that sits idle this long is stopped; a background task has no idle stop. A policy with a shorter limit still stops it sooner.",
  IDLE_PROBLEM: "Enter a whole number of seconds or minutes, 1 or more.",
  IDLE_NUMBER: "Idle time",
  IDLE_UNIT_LABEL: "Idle time unit",
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

// The grantable GitHub capabilities, grouped, in the words the server uses for each (internal/ghscope/capability.go,
// pinned by personal-walls.test.ts). Azure DevOps has its own names and groups in lib/ado-capabilities.ts.
export const GITHUB_CAPABILITY_GROUPS: { name: string; caps: { cap: string; label: string }[] }[] = [
  {
    name: "Code",
    caps: [
      { cap: "code_read", label: "Clone, fetch and browse repositories, commits, branches, releases and pull requests" },
      { cap: "code_write", label: "Push commits with git, inside this run's own branch unless its policy allows any branch" },
      { cap: "workflows_write", label: "Push changes to workflow files under .github/workflows" },
    ],
  },
  {
    name: "Pull requests and issues",
    caps: [
      { cap: "pr", label: "Open, update and review pull requests" },
      { cap: "pr_write", label: "Open, update and comment on pull requests, and request reviewers" },
      { cap: "pr_review", label: "Submit, approve, request changes on and dismiss pull request reviews" },
      { cap: "issues_read", label: "Read issues, comments, labels and milestones" },
      { cap: "issues_write", label: "Create and change issues, comments, labels and milestones" },
    ],
  },
  {
    name: "Actions",
    caps: [
      { cap: "actions_read", label: "Read workflows, runs, jobs, logs and artifacts" },
      { cap: "actions_execute", label: "Dispatch, re-run or cancel a workflow run" },
      { cap: "actions_admin", label: "Enable or disable workflows, approve held runs, and delete runs, logs, artifacts and caches" },
    ],
  },
  {
    name: "Packages",
    caps: [
      { cap: "packages_read", label: "Read the packages of the organisations that own this run's repositories" },
      { cap: "packages_write", label: "Delete or restore those packages and their versions" },
    ],
  },
  {
    name: "Administration and profile",
    caps: [
      { cap: "repo_admin", label: "Change or delete repositories, their collaborators, rulesets, branch protection and Actions settings, and create repositories" },
      { cap: "org_read", label: "Read the organisation's profile, members, teams and repository list" },
      { cap: "org_admin", label: "Change the organisation's settings, members, teams and invitations" },
      { cap: "identity", label: "Read your own GitHub profile" },
    ],
  },
];
export const GITHUB_CAPABILITIES = GITHUB_CAPABILITY_GROUPS.flatMap((g) => g.caps.map((c) => c.cap));
// The short name of the push capability, as the line under a list that leaves it out says it.
export const GITHUB_PUSH_NAME = "Push commits with git";

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
