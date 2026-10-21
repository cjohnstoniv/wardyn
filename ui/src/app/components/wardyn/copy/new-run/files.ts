/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// New Run canon strings for step 4, Files, byte for byte from docs/design/new-run-canon.md.
// Lazy-only: imported by the New Run flow and never re-exported through copy.ts, whose barrel is in the eager
// entry chunk. new-run-copy.test.ts keeps this file and the doc equal in both directions.

export const NR_FILES = {
  TITLE: "What files does it need?",
  LEDE:
    "Pick the repositories, drives and folders it can open. It starts in the one you mark. Anything not on a drive is deleted when the run ends.",
  MAIN_TITLE: "Main drive",
  MAIN_BODY: "The sandbox's own disk: system, tools and /home/agent. Every run has one.",
  SIZE_LABEL: "Size",
  /** `size` carries its unit: "4 GiB" or "1000 MiB". */
  UP_TO: (size: string | number) => `Up to ${size} in this pool`,
  CLAMP_POOL: (size: string | number, pool: string | number) => `Clamped to ${size} by the ${pool} pool.`,
  /** pending W-11: the recommended text until the owner answers. */
  CLAMP_PROFILE: (size: string | number, profile: string | number) => `Clamped to ${size} by ${profile}.`,
  /** pending W-11: the recommended text until the owner answers. */
  CLAMP_SERVER: (size: string | number) => `Clamped to ${size} by this server.`,
  CLAMP_POLICY: (size: string | number) => `Clamped to ${size} by this run's policy.`,
  /** pending W-11: the recommended text until the owner answers. */
  CLAMP_ORG: (size: string | number) => `Clamped to ${size} by your organisation's limits.`,
  /** Shown whenever the field's text is not the exact stored size. */
  SIZE_EXACT: (mib: string | number) => `Exactly ${mib} MiB`,
  SIZE_ERROR: "Set a size for the main drive.",
  ENFORCED: "Enforced",
  ENFORCED_TIP: "Writes past this size fail.",
  ENFORCED_BY_STOP: "Enforced by stopping the run",
  ENFORCED_BY_STOP_TIP:
    "If /tmp, /home/agent/work and /home/agent/.cache together pass this size, Kubernetes stops the run. Other paths aren't counted.",
  NOT_ENFORCED: "Not enforced on this runner",
  NOT_ENFORCED_ARIA: "Disk size is not enforced on this runner",
  NOT_ENFORCED_TIP: "This runner's storage can't cap a container's disk; the size is a target, not a limit.",
  REPORTED_BY_RUNNER: "(reported by the runner)",
  LIFE_LABEL: "What happens to it",
  LIFE_DELETED: "Deleted when the run ends.",
  LIFE_KEPT: (max: string | number, grace: string | number) =>
    `Kept after its end time so you can extend and resume it, up to its ${max} maximum lifetime. Deleted when you end the run, or ${grace} after its end time.`,
  /** pending W-12: the recommended text until the owner answers. */
  LIFE_KEPT_UNLIMITED: (grace: string | number) =>
    `Kept after its end time so you can extend and resume it. Deleted when you end the run, or ${grace} after its end time.`,
  LIFE_NO_END: "Kept until you end the run.",
  REUSE_LABEL: "Reuse it across runs",
  REUSE_ARIA: "About reusing this drive",
  REUSE_TIP: "Save this disk when the run ends and start another run from it.",
  REPOS_TITLE: "Repositories",
  REPOS_BODY: "Copied onto the main drive. Changes leave only as commits you push.",
  REPOSITORY_BADGE: "Repository",
  INCLUDE_ARIA: (name: string | number) => `Include ${name}`,
  BRANCH_ARIA: (name: string | number) => `Branch for ${name}`,
  STARTS_HERE: "Starts here",
  ADD_REPOSITORY: "+ Add repository",
  /** pending W-13: the recommended text until the owner answers. */
  BRANCH_PLACEHOLDER: "default branch",
  /** pending W-13: the recommended text until the owner answers. */
  BRANCH_FALLBACK:
    "Branches couldn't be listed. Type a branch, tag or commit; leave it empty for the default branch.",
  /** pending W-13: the recommended text until the owner answers. */
  BRANCH_OTHER: "Other…",
  /** pending W-14: the recommended text until the owner answers. */
  REF_NOT_CHECKED: "Not checked until the run starts.",
  USER_DRIVES_TITLE: "Remote user drives",
  SHARED_DRIVES_TITLE: "Remote shared drives",
  DRIVES_BODY: "Live storage. Changes are kept.",
  /** pending W-16: the recommended text until the owner answers. */
  SHARED_DRIVES_BODY: "Live storage. Changes are kept. Each person gets their own folder on the share.",
  SOURCE_PERSONAL: "Personal",
  SOURCE_TEAM: "Team",
  /** pending W-15: the recommended text until the owner answers. */
  SOURCE_ORG: "Organisation",
  FAMILY_DOCKER_VOLUME: "Docker volume",
  FAMILY_HOST_FOLDER: "Host folder",
  FAMILY_CLUSTER_VOLUME: "Cluster volume",
  ACCESS_EDIT: "Can edit",
  ACCESS_READ: "Read-only",
  ACCESS_ARIA: (name: string | number) => `Access for ${name}`,
  LOCKED_ARIA: "Why this drive is read-only",
  LOCKED_TIP: "Set by your admin",
  NOT_IN_POOL: "Not available in this pool's runners.",
  CREATE_USER_DRIVE: "+ Create user drive",
  CREATE_SHARED_DRIVE: "+ Create shared drive",
  MANAGE_DRIVES: "Manage drives →",
  /** pending W-17: the recommended text until the owner answers. */
  CREATE_DISABLED: "Your organisation doesn't let people create drives. Ask an admin.",
  /** pending W-17: the recommended text until the owner answers. */
  CREATE_DENIED: (profile: string | number) => `Not allowed by ${profile}.`,
  /** pending W-17: the recommended text until the owner answers. */
  CREATE_LIMIT: (n: string | number, limit: string | number) => `You have ${n} of ${limit} drives.`,
  /** pending W-17: the recommended text until the owner answers. */
  CREATE_UNSUPPORTED: "This server can't create drives.",
  /** pending W-17: the recommended text until the owner answers. */
  CREATE_NO_DELETE: "This server can't delete drives, so it can't create them.",
  /** pending W-17: the recommended text until the owner answers. */
  DELETE_DRIVE: "Delete drive",
  /** pending W-17: the recommended text until the owner answers. */
  DELETE_CONFIRM: (name: string | number) => `Delete ${name}? Its files are deleted and can't be recovered.`,
  /** pending W-17: the recommended text until the owner answers. */
  DRIVE_DELETED: (name: string | number) => `${name} deleted.`,
  CREATE_TITLE: "Create user drive",
  CREATE_INTRO: "Wardyn assigns the mount path. You can attach the drive to this run after it is created.",
  DRIVE_NAME: "Drive name",
  DRIVE_NAME_PLACEHOLDER: "e.g. Investigation files",
  CREATE_CONFIRM: "Create drive",
  DRIVE_CREATED: (name: string | number) => `${name} created and added to this run.`,
  COMPUTER_TITLE: "Your computer",
  COMPUTER_BODY: "Folders on your laptop, desktop or VDI.",
  // M4 (#2982, owner-approved 2026-10-19): a folder on your computer, through your own runner.
  COMPUTER_BLOCKED: "Your organisation doesn't allow folders from personal computers.",
  COMPUTER_NO_RUNNER: "No runner on your computer yet.",
  COMPUTER_NO_RUNNER_BODY:
    "A runner connects out to Wardyn and shares only the folders you pick. Nothing connects in to your computer.",
  COMPUTER_SET_UP: "Set up a runner",
  COMPUTER_NOT_NOW: "Not now",
  COMPUTER_OFFLINE: "Runner offline",
  COMPUTER_LAST_SEEN: (name: string | number, when: string | number) => `${name} · last seen ${when}`,
  COMPUTER_OFFLINE_BODY: (name: string | number) =>
    `Start the runner on ${name}, then try again. A run that uses this folder can't start until it is back.`,
  COMPUTER_LABEL: "Computer",
  COMPUTER_OPEN: (name: string | number) => `Open ${name}`,
  COMPUTER_COL_NAME: "Name",
  COMPUTER_COL_FILES: "Files",
  COMPUTER_COL_SIZE: "Size",
  COMPUTER_WILL_SEE: "The sandbox will see",
  COMPUTER_GITIGNORE: "Leave out what .gitignore lists",
  COMPUTER_BROWSE_NOTE: "Only folder names and sizes reach Wardyn while you browse. No file is sent until the run starts.",
  COMPUTER_NEXT: "Next",
  COMPUTER_FROM: "From",
  COMPUTER_ON: (name: string | number) => `on ${name}`,
  COMPUTER_ONLINE: "Runner online",
  COMPUTER_CHANGES: "Changes",
  COMPUTER_BOTH: "Both ways",
  COMPUTER_COPY_IN: "Copy in only",
  COMPUTER_BOTH_BODY: "Edits in the sandbox come back to your computer when the run ends.",
  COMPUTER_COPY_IN_BODY: "A copy from your computer. Changes stay in the sandbox.",
  COMPUTER_SEES: "The sandbox sees",
  COMPUTER_COUNT: (files: string | number, size: string | number) => `${files} files · ${size}`,
  COMPUTER_LEFT_OUT: "Left out",
  COMPUTER_IGNORED: (count: string | number) => `${count} paths in .gitignore`,
  COMPUTER_CHANGE: "Change",
  COMPUTER_FIRST_SYNC: "First sync",
  COMPUTER_FIRST_SYNC_BODY: "Starts when you launch. The run waits until it finishes.",
  NO_EXTRA: "No extra files — only the main drive",
  NO_EXTRA_DELETED: "Only the main drive. Nothing from this run is kept.",
  /** pending W-12: the recommended text until the owner answers. */
  NO_EXTRA_KEPT: (grace: string | number) =>
    `Only the main drive. It is deleted ${grace} after the run's end time.`,
  /** Lead wording 2026-10-17 (FR-0099): the Review blocker for a run with no folder marked as where it starts. */
  STARTS_HERE_REQUIRED: "Choose the folder the run starts in.",
  CHOOSE_FILES: "Add what it needs, or choose No extra files.",
  MOUNT_TITLE: "Mount map",
  MOUNT_BODY: "Read-only view of where files appear in the sandbox.",
  MOUNT_MAIN: (size: string | number) => `Main drive · ${size}`,
  MOUNT_ITEM: (name: string | number) => `← ${name}`,
  MOUNT_STARTS: "(Starts here)",
  MOUNT_READ_ONLY: "(read-only)",
  SUMMARY_MAIN: (size: string | number) => `Main drive ${size}`,
  SUMMARY_STARTS: (name: string | number) => `starts in ${name}`,
  SUMMARY_KEPT_ONE: (names: string | number) => `${names} is kept`,
  SUMMARY_KEPT_MANY: (names: string | number) => `${names} are kept`,
  SUMMARY_DELETED: "the main drive is deleted when the run ends",
  /** pending W-12: the recommended text until the owner answers. */
  SUMMARY_KEPT: (grace: string | number) => `the main drive is kept ${grace} after its end time`,
  /** pending W-12: the recommended text until the owner answers. */
  SUMMARY_NO_END: "the main drive is kept until you end the run",
  SUMMARY_NO_EXTRA: (size: string | number, life: string | number) =>
    `Main drive ${size} · no extra files · ${life}.`,
  // Keep the work folder as a drive (F2KEEP-1..3, #1991).
  KEEP_WHAT:
    "Keeps the work folder, /home/agent/work, as your own drive named after this run. Your home folder, harness settings and /tmp are not kept.",
  KEEP_WARNING:
    "Anything an agent leaves in the folder comes back in the next run that uses it, including files you didn't ask for. Check it before you reuse it.",
  KEEP_COUNTS: (n: string | number, limit: string | number) =>
    `Counts toward your drives. You have ${n} of ${limit} drives. Kept until you delete it.`,
  KEEP_UNENFORCED: "On this runner its size is not enforced.",
  KEEP_LIFE: "The work folder is kept as a drive. The rest is deleted when the run ends.",
  KEEP_SUMMARY: "the work folder is kept as a drive after the run",
  KEEP_STARTS_MAIN: "the main drive",
  KEEP_LIMIT: (n: string | number, limit: string | number) =>
    `You have ${n} of ${limit} drives. Delete one to keep this run's work folder.`,
  /** The server's own sentence (keepWorkFolderDisabledMsg), for a rerun that asks to keep while the keep switch is off. */
  KEEP_DISABLED: "Your organisation doesn't let runs keep their work folder as a drive. Ask an admin.",
  KEEP_LIMIT_HINT: "Or untick “Reuse it across runs” to launch without keeping it.",
  DRIVE_KEPT_FROM: (run: string | number) => `From run ${run} · Kept until you delete it.`,
  /** Not on the boards: the run's title is gone or empty, so the line names no run. */
  DRIVE_KEPT_FROM_UNTITLED: "From an earlier run · Kept until you delete it.",
  DRIVE_SIZE_NOT_ENFORCED: "Size not enforced",
  DRIVE_SIZE_NOT_ENFORCED_TIP: "A Docker volume has no size cap.",
  // Owner-approved Files prototype (2026-10-13): one list of cards, the Add / Edit dialog and the starting folder.
  ATTACH_TITLE: "Repositories, drives and folders",
  ATTACH_BODY: "Each one appears in the sandbox at its mount path.",
  ADD: "+ Add",
  ADD_ARIA: "Add a repository, drive or folder",
  KIND_USER_DRIVE: "User drive",
  KIND_SHARED_DRIVE: "Shared drive",
  KIND_FOLDER: "Folder",
  FOLDER_TITLE: "Folder on this machine",
  FOLDER_BODY: "A folder on the computer running Wardyn.",
  SHARED_SUB: "Provided by your admin. You get your own folder on it.",
  FOLDER_REMOTE: "Wardyn runs on a remote server, so it can't open folders on this machine.",
  EDIT: "Edit",
  EDIT_NAMED: (name: string | number) => `Edit ${name}`,
  REMOVE_NAMED: (name: string | number) => `Remove ${name}`,
  STORED_ON: "Stored on",
  ADDED: (name: string | number) => `${name} added.`,
  UPDATED: (name: string | number) => `${name} updated.`,
  REMOVED: (name: string | number) => `${name} removed.`,
  DIALOG_TITLE: "Add to this run",
  ADD_REPO_TITLE: "Add repository",
  ADD_USER_DRIVE_TITLE: "Add user drive",
  /** A team drive's groups: "Shared with: platform-team". */
  SHARED_WITH: (groups: string) => `Shared with: ${groups}`,
  /** Under a writable team drive: what the others in the group will see. */
  TEAM_OTHERS_SEE: "Others in this group see what your runs write here.",
  ADD_SHARED_DRIVE_TITLE: "Add shared drive",
  ADD_FOLDER_TITLE: "Add folder",
  STEPS_ARIA: "Steps",
  STEP_TYPE: "Type",
  STEP_CHOOSE: "Choose",
  STEP_MOUNT: "Mount path",
  ALREADY_ADDED: "Already added to this run.",
  BRANCH_LABEL: "Branch",
  MOUNT_HINT: "Where it appears in the sandbox.",
  PATH_INVALID: "Use a path inside /home/agent.",
  PATH_TAKEN: (name: string | number, path: string | number) => `${name} is already mounted at ${path}.`,
  ACCESS_LABEL: "Access",
  SAVE: "Save",
  ADD_CONFIRM: "Add",
  START_TITLE: "Starting folder",
  START_BODY: "Where it starts. Choose from what you added above.",
  START_FOLDER_LABEL: "Folder",
  START_OPTION: (name: string | number, path: string | number) => `${name} · ${path}`,
  START_MAIN: "Main drive · /home/agent",
  SUBFOLDER_LABEL: "Subfolder",
  /** pending W-48: the recommended text until the owner answers. */
  FOLDERS_NONE: "No folders onboarded yet. Add one in Workspaces.",
  /** pending W-49: the recommended text until the owner answers. */
  FOLDER_WITH_REPOS: (workspace: string | number) => `This folder comes with ${workspace}'s repositories. Remove them to remove it.`,
  SUBFOLDER_PLACEHOLDER: "e.g. src",
  SUBFOLDER_ERROR: (name: string | number) => `Use a folder inside ${name}.`,
  LIST_COMMA: (a: string | number, b: string | number) => `${a}, ${b}`,
  LIST_AND: (a: string | number, b: string | number) => `${a} and ${b}`,
} as const;
