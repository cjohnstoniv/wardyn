/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Runners (0.9, packet D-UI-b, owner-approved 2026-10-10; the two ledes are mock M-POOLS v6's, approved
// 2026-10-12): the Admin -> Runners page (RUNNERS), what both it and Your account say about one runner
// (RUNNER), and the My runner card (MY_RUNNER). Strings are the packet's, character for character
// (copy-amendments.md section 1). LAZY: reached only through
// the runners screen and the My runner card, never re-exported from the eager copy.ts barrel; the nav
// label's one eager string is RUNNERS_NAV_TITLE in lib/nav-copy.ts.
//
// Words the placement vocabulary owns ("Online", "Offline", "You haven't registered a runner.") are
// defined here with D-UI-a's text, because the keys they were written under were not built.

import { RUNNER_OFFLINE_WORD, RUNNERS_NAV_TITLE, runnerLabel, runOnChip } from "../../../lib/nav-copy";

export const RUNNERS = {
  NAV: RUNNERS_NAV_TITLE,
  LEAD: "A runner is what runs sandboxes. A self-hosted runner is a computer someone registered; a remote-hosted runner is a Docker host or Kubernetes cluster this server is set up with. Each runs several runs at once, up to its Runs at once, under your policy and audit. What a runner reports about itself is labelled as reported.",
  SEARCH: "Search runners",
  FILTER_ACTIVE: "Active",
  FILTER_REVOKED: "Revoked",
  FILTER_ALL: "All",
  COL_STATE: "State",
  WAITING_CLAIM: (owner: string) => `Waiting for ${owner} to claim it. Only they can.`,
  EXPIRES_IN: (left: string) => `expires in ${left}`,
  MINT_CTA: "Create a registration token",
  MINT_TITLE: "Registration token for a person",
  MINT_LEAD: "The token registers one computer for this person. Only they can claim it, from their own account — you can't claim it for them.",
  FIELD_PERSON: "Person",
  PERSON_HINT: "Their email or sign-in subject.",
  MINT_SUBMIT: "Create token",
  MINT_DONE_TITLE: (person: string) => `Registration token for ${person}`,
  MINT_SHOWN_ONCE: "Shown once. Copy it now and deliver it to their computer as a file only they can read — for example with your device management tool.",
  COMMAND_LEAD: "On their computer, the runner registers with:",
  MINT_NEXT: (person: string) => `Their computer then waits for ${person} to claim it in Your account → My runner.`,
  CLOSE_UNCOPIED_TITLE: "Close without copying the token?",
  CLOSE_UNCOPIED_BODY: "You can't see this token again. If you need it, create another.",
  KEEP_OPEN: "Keep it open",
  CLOSE_ANYWAY: "Close",
  UNUSED_TITLE: "Unused registration tokens",
  UNUSED_LEAD: "Tokens that haven't registered a computer yet. Each works once, until it expires.",
  COL_FOR: "For",
  COL_CREATED_BY: "Created by",
  CREATED_BY_YOU: "you",
  COL_EXPIRES: "Expires",
  REVOKE_TOKEN: "Revoke",
  REVOKE_TOKEN_TITLE: (person: string) => `Revoke the unused token for ${person}?`,
  REVOKE_TOKEN_BODY: "A computer that hasn't registered with it yet can't. Nothing that already registered is affected.",
  REVOKE: "Revoke",
  REVOKE_TITLE: (name: string) => `Revoke ${name}?`,
  REVOKE_BODY:
    "Every run on it ends now, and their credentials are revoked at once. Its runner stops their network now, or when it next connects if it's offline. To use this computer again, its owner registers it again.",
  OFF_TITLE: "Runners are off",
  OFF_BODY:
    "People can't register their own computers to run their runs. With runners on, each person can register a computer that runs only their own runs, under your policy and audit.",
  OFF_SECURITY: "A super admin can turn runners on here.",
  TURN_ON: "Turn on runners",
  TURN_ON_TITLE: "Turn on runners?",
  TURN_ON_BODY:
    "People can then register their own computers from Your account. Every run on one still goes through your policy and audit, and what a runner reports about itself is labelled as reported.",
  TURN_ON_REFUSED_TITLE: "Runners can't be turned on yet",
  TURN_OFF: "Turn off runners",
  TURN_OFF_TITLE: "Turn off runners?",
  TURN_OFF_BODY:
    "Every runner's connection closes and no new run can go to one. Runs already on a runner keep going under the policy they started with, as they would offline — after about an hour their network stops and they're marked lost.",
  EMPTY_TITLE: "No runners yet",
  EMPTY_BODY: "People register a computer from Your account → My runner. You can also create a registration token for someone and deliver it to their computer.",
  FETCH_FAILED_TITLE: "Couldn't load runners",
  FETCH_FAILED_BODY:
    "Something went wrong reaching the server. Runners already claimed keep running their owners' runs under your policy — this list just can't show them right now.",
  FILTER_EMPTY_ACTIVE: "No active runners.",
  FILTER_EMPTY_REVOKED: "No revoked runners.",
  SEARCH_EMPTY: (q: string) => `No runner matches "${q}".`,
} as const;

export const RUNNER = {
  CANCEL: "Cancel",
  // D-UI-a's placement words, reused byte for byte (NEW_RUN_FLOW.PLACEMENT.ONLINE / OFFLINE_WORD).
  ONLINE: "Online",
  OFFLINE: RUNNER_OFFLINE_WORD,
  TOKEN_LABEL: "Registration token",
  COPY_TOKEN: "Copy token",
  TOKEN_EXPIRES: (when: string) => `Expires ${when}. It works once.`,
  COPY_COMMAND: "Copy command",
  DONE: "Done",
  STATE_UNCLAIMED: "Not claimed yet",
  STATE_REVOKED: "Revoked",
  REPORTED: "Reported by this runner",
  REPORTED_AT: (when: string) => `reported ${when}`,
  POSTURE: "Posture",
  OS: (os: string, version: string) => `${os} ${version}`,
  MDM_YES: "Managed by MDM",
  MDM_NO: "Not managed by MDM",
  DISK_YES: "Disk encrypted",
  DISK_NO: "Disk not encrypted",
  NOT_REPORTED: "Not reported yet",
  BARRIERS: "Barriers",
  ROOTS: "Allowed folders",
  ROOTS_NONE: "None, so runs on it can't use a folder from that computer.",
  FINGERPRINT: "Key fingerprint",
  VERSION: "Version",
  REGISTERED: (when: string) => `Registered ${when}`,
  CLAIMED: (when: string) => `Claimed ${when}`,
  REVOKED_BY: (who: string, when: string) => `Revoked by ${who} ${when}`,
  NEVER_CONNECTED: "Never connected",
  REVOKE_RUNS: (n: number) => (n === 1 ? "1 run is on it now." : `${n} runs are on it now.`),
  REVOKE_CONFIRM: "Revoke runner",
  REVOKED_TOAST: (name: string) => `${name} revoked`,
} as const;

export const MY_RUNNER = {
  TITLE: "My runner",
  LEDE: "A computer you registered runs your own runs, several at once up to its Runs at once, under your organization's policy and audit. One runner per computer is enough. While it's offline it runs nothing new.",
  NO_RUNNER: "You haven't registered a runner.",
  SUMMARY_NONE: "Not registered",
  SUMMARY_ONE: (name: string, state: string) => `${name} · ${state}`,
  SUMMARY_MANY: (n: number, online: number) => `${n} runners · ${online} online`,
  SUMMARY_WAITING: (n: number) => `${n} waiting for you to claim`,
  REGISTER: "Register a runner",
  TOKEN_SHOWN_ONCE: "Shown once. It works once, for 1 hour.",
  TOKEN_EXPIRES: (when: string) => `Expires ${when}.`,
  STEP_SAVE_RUN: "On the computer you're registering, save it to a file only you can read, then run:",
  STEP_CLAIM: "The runner prints its key fingerprint. Then claim it below, or run wardyn runner claim on that computer with your own API token in WARDYN_TOKEN. The admin token cannot claim.",
  TOKEN_EXPIRED: "This token has expired. Register again for a new one.",
  REGISTERED_NOW: (name: string) => `${name} registered. Claim it below.`,
  WAITING_LINE: (when: string, left: string) => `Registered ${when} · claim it within ${left}`,
  SOURCE_SELF: "From a token you created.",
  SOURCE_ADMIN: (who: string) => `From a token ${who} created for you.`,
  CLAIM: "Claim",
  NOT_MINE: "This isn't mine",
  CLAIM_TITLE: (name: string) => `Claim ${name}`,
  CLAIM_LEAD: (name: string) =>
    `On ${name}, run wardyn runner claim with your own API token in WARDYN_TOKEN. Or paste the fingerprint its runner printed when it registered — from that computer, not from this page.`,
  FIELD_FINGERPRINT: "Fingerprint from your computer",
  FINGERPRINT_HINT: (name: string) => `Copy it from the runner's output on ${name}.`,
  CLAIM_SUBMIT: "Claim runner",
  FIELD_POOL: "Put it in a pool (optional)",
  POOL_HINT: "Pick one of your pools, or type a new name to make one. A claimed runner in no pool runs nothing.",
  POOL_PLACEHOLDER: "Pool name",
  POOL_DEFAULT: "Make it my default pool",
  CLAIM_POOL_FAILED: (name: string, pool: string, why: string) => `${name} is claimed but isn't in ${pool}. Add it from My pools. ${why}`,
  // Owner-approved 2026-10-17: the runner joined the pool; only making it the default failed.
  CLAIM_DEFAULT_FAILED: (name: string, pool: string, why: string) => `${name} is in ${pool} but isn't your default pool yet. Set it from My pools. ${why}`,
  CLAIM_MISMATCH: (name: string) =>
    `That fingerprint doesn't match ${name}'s key. Check you're claiming the computer you registered. If you didn't register ${name}, revoke it.`,
  CLAIMED_TOAST: (name: string) => `${name} is claimed. It can run your runs now.`,
  NOT_MINE_BODY: "It has never run anything as you, and once revoked it never can. Then tell your security team: someone registered it with a token made for you.",
  LAST_SEEN: (when: string) => `Last seen ${when}`,
  ROOTS_HINT: "Runs on it can use folders inside these only. They're set on the computer itself.",
  REVOKE_OWN_BODY:
    "Runs on it end now, and their credentials are revoked. Its runner stops their network now, or when it next connects. To use this computer again, register it again.",
  FETCH_FAILED: "Couldn't load your runners.",
  REVOKED_BY: (name: string, who: string, when: string) => `${name} was revoked by ${who} ${when}. Register it again to use it.`,
} as const;

// Run detail (packet M-RN-3). The packet's "Placement" is not a console word (DECISIONS 2026-10-09), so the
// Identity row reads "Runs on" and the values name the kind of runner: a run on its owner's computer is
// "{name} · your computer", a run on a remote-hosted runner "Remote-hosted runner"; the Runner and Pool rows
// below it name the runner and the pool.
export const RUN_PLACEMENT = {
  LABEL: "Runs on",
  /** pending W-52: the recommended text until the owner answers. */
  REMOTE: "Remote-hosted runner",
  /** pending W-51: the recommended text until the owner answers. */
  MINE: (name: string) => `${name} · your computer`,
  OTHERS: (name: string, owner: string) => `${name} · ${owner}'s runner`,
  /**
   * pending H11-P1: recommended text until the owner answers. The approved FILLED text, "The only placement
   * this run could use.", carries the banned word "placement".
   */
  FILLED: "The only place this run could run.",
  /** The Identity rows beside "Runs on": the runner by name (its id when the name is unknown) and its pool. */
  RUNNER: "Runner",
  POOL: "Pool",
  CHIP: runOnChip,
  EVIDENCE: "Evidence",
  REPORTED_BY: (name: string) => `Reported by ${name}`,
  REPORTED_NOTE: (name: string) =>
    `${name} is a computer its owner administers, so this run's barrier, egress decisions made on ${name}, recording, output and exit code are as ${name} reported them.`,
} as const;

/** Pending a mock round: the run's not_recorded, on the Identity card. */
export const RUN_RECORDING = {
  LABEL: "Recording",
  org_off: "Not recorded; the organization kept no recordings when it started.",
  runner_declined: "Not recorded by the runner; the organization kept recordings when it started.",
} as const;

export const RUN_SYNC = {
  LABEL: "Sync",
  NONE: "Nothing synced yet.",
  BYTES: (to: string, from: string) => `${to} to the sandbox · ${from} from the sandbox`,
  LAST: (when: string) => `Last activity ${when}`,
  OPEN_SINCE: (when: string) => `Open since ${when}`,
  OPEN_NOTE: "Byte counts update when a session ends.",
  ERROR: (text: string) => `Last session ended with an error: ${text}`,
  MORE_DIRS: (n: number) => `and ${n} more in the Audit tab`,
  SFTP_LABEL: "sftp",
  LANE_TITLE: "Sync a folder",
  LANE_BODY:
    "Copies a folder from your computer into the sandbox — and back only if you add --pull. wardyn sync never copies .git, .envrc, .direnv or editor task files, never follows a symlink, and never makes a pulled file executable.",
  LANE_COMMAND: (id: string) => `wardyn sync ${id} ./my-folder`,
  COPY: "Copy sync command",
} as const;

export const LOCAL_RUN = {
  OFFLINE_TITLE: (name: string) => `${name} is offline`,
  OFFLINE_BODY: (name: string, when: string) =>
    `This run keeps going there under the policy it started with. Until ${name} reconnects, Wardyn can't reach its terminal or files, and a host that would need approval is refused, because no one can approve it until ${name} reconnects. Last seen ${when}.`,
  OFFLINE_SPOOL: "Egress decisions it makes meanwhile appear here when it reconnects.",
  LAPSE_TITLE: (name: string) => `${name} has been offline for over an hour`,
  LAPSE_BODY: (name: string, when: string) =>
    `Its runner stops this run's network on its own after an hour offline, and keeps the agent and files. Wardyn last heard from ${name} at ${when}, so it can't confirm that yet.`,
  UNKNOWN_TITLE: (name: string) => `Wardyn can't tell from here whether ${name} is connected`,
  UNKNOWN_BODY: (name: string, when: string) =>
    `Wardyn last heard from ${name} at ${when}, and this server can't say whether it is online now. After an hour offline its runner stops this run's network on its own and keeps the agent and files. If you revive it while ${name} is out of reach, Wardyn says so.`,
  REVIVE_WHEN_BACK: (name: string) => `You can revive it once ${name} is back online.`,
  STOPPED_TITLE: "This run's network was stopped",
  STOPPED_BODY: (name: string) =>
    `${name} reports its runner stopped this run's network after an hour offline. The agent and files are kept on ${name}. Revive reconnects it.`,
  STOPPED_LATE: (name: string) =>
    `${name} was offline for over an hour, and this run's network was still on when it came back, so Wardyn had its runner stop it then. The agent and files are kept on ${name}. Revive reconnects it.`,
  REVOKED_TITLE: (name: string) => `${name} was revoked`,
  REVOKED_BODY: (name: string) => `This run's credentials were revoked, and ${name} was told to stop its network. Anything it wrote stays on ${name}.`,
  REVOKED_OFFLINE_TITLE: (name: string) => `${name} was revoked while offline`,
  REVOKED_OFFLINE_BODY: (name: string) =>
    `This run's credentials were revoked at once; ${name} stops its network when it next connects. Anything it wrote stays on ${name}.`,
  REVOKED_TODO_AUDIT: (name: string) => `The audit row for revoking ${name} names who did it.`,
  REVOKED_TODO_FILES: (name: string) => `The sandbox and its files stay on ${name}. Wardyn can't remove them; the computer's owner can.`,
  KILL_DONE: (elapsed: string, name: string) =>
    `An operator killed this run ${elapsed} in. ${name} reported it removed the sandbox; Wardyn issues it no more credentials.`,
  KILL_FILES_LOCAL: (name: string) => `Files the run wrote to a folder on ${name} are still there; scratch is gone.`,
  // A kill whose stop was queued for an offline runner (run.kill data.sandbox_stop "queued"): the credentials
  // are gone, the sandbox is not yet.
  KILL_QUEUED: (elapsed: string, name: string) =>
    `This run was killed ${elapsed} in while ${name} was offline. Its credentials were revoked at once, and ${name} was told to remove the sandbox when it next connects.`,
  KILL_QUEUED_FILES: (name: string) => `Until ${name} reconnects, the sandbox and anything the run wrote stay there.`,
  TERMINAL_OFFLINE: (name: string) => `${name} is offline, so its terminal can't be reached. The session keeps running there.`,
  SPOOLED: (when: string) => `Recorded offline at ${when}`,
  SPOOL_FULL: "Refused — the runner's offline log was full",
} as const;

export { runnerLabel };
