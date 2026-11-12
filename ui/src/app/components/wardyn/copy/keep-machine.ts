/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Kept macOS VM machines, character for character as the approved boards draw them (KM-1 to KM-8, with the joint verdict's
// corrections); the id in each comment is the board's. The server's refusal sentences (KS-xx) show as the server sends
// them and are not copied here, except the two the console words itself: the fresh start's base line and the delete that
// could not be confirmed.

/** Whole GB of a disk size, for "{gb} GB". */
export const gbOf = (bytes: number): number => Math.round(bytes / 1024 ** 3);

/** "{n} of {limit} kept machines used" (K-12 and KS-16), singular at a limit of one. */
export const keptCount = (n: number, limit: number): string => `${n} of ${limit} kept ${limit === 1 ? "machine" : "machines"} used`;

/** "{n} days" for the idle limit the server states; without it, the sentences name no number. */
const afterDays = (days: number | undefined): string => (days === undefined ? "after it goes unused for the time your admin sets" : `${days} ${days === 1 ? "day" : "days"} after its last use`);

/** KM-1: New Run, the machine section. */
export const KEEP_NEW_RUN = {
  /** K-01 */
  LABEL: "Between runs",
  /** K-02 */
  DELETE: "Delete this machine when the run ends",
  /** K-03 */
  KEEP: "Keep this machine for next time",
  /** K-04 */
  KEEP_DETAIL: (mac: string, gb: number, days?: number) =>
    `Kept on ${mac}, up to ${gb} GB. It stays until you delete it, or ${days === undefined ? "until it has gone unused for the time your admin sets" : `${days} ${days === 1 ? "day" : "days"} after its last use`}. Your files, tools and sign-ins stay. Wardyn cleans up the settings and certificates it added when the run stops, or before the machine's next run if the stop was abrupt. The run page says if anything was left.`,
  /** K-05 */
  NAME: "Name",
  /** K-06 */
  NAME_HINT: "Yours alone, for resuming it later: 1 to 64 letters, digits, spaces, - and _. Leave it empty and Wardyn picks one.",
  /** K-07 */
  SECRETS: "A kept machine can't take secrets that live inside it: environment secrets, file secrets, SSH keys and git tokens. Use credentials the proxy adds.",
  /** K-08 */
  RESUME: (name: string) => `Resume ${name}`,
  /** K-09 */
  RESUME_META: (date: string, base: string, gb: number) => `Last used ${date} · ${base} base · ${gb} GB written`,
  /** K-10 */
  FRESH: "Start fresh",
  /** K-11 */
  FRESH_HINT: (name: string, mac: string) => `Deletes everything on ${name} and starts it again from ${mac}'s current base, under the same name.`,
  /** The server's kept_machine_fresh_base sentence: a fresh start keeps the machine's own base, so the base picker is not offered. */
  FRESH_BASE: (base: string) => `A fresh start keeps your kept machine's base, ${base}. Start a new kept machine to use a different base.`,
  /** K-13 */
  IN_USE: (run: string) => `In use by ${run}. Stop that run first.`,
  /** K-14, the changes panel's Sign-ins row when Keep or Resume is chosen (replaces MV-C04) */
  SIGN_INS: (mac: string) => `Kept on this machine's disk on ${mac} while this machine is kept. The Mac's own snapshots or backups may keep a copy for a while.`,
  /** The read of the person's kept machines failed: the choice stays and Launch waits. */
  READ_FAILED: "Wardyn couldn't read your kept machines. Try again.",
  /** A resume the list proved gone: the choice is cleared, and says so. */
  GONE: "Your kept machine is gone. This run starts a new machine, deleted when the run ends.",
  /** A keep or resume the server's answer says this run can't have. */
  UNAVAILABLE: "Keeping a machine isn't available for this run, so its machine is deleted when the run ends.",
  /** A keep at the person's limit. */
  AT_LIMIT: "You've reached your limit of kept machines, so this run's machine is deleted when it ends.",
  /** A keep or resume that the shape or mode chosen since can no longer have. */
  INTERACTIVE_ONLY: "Only an interactive macOS VM run can keep or resume a machine, so this run's machine is deleted when it ends.",
  RETRY: "Try again",
  /** K-15 */
  NOT_SCRUBBED: "Its last run didn't stop normally; Wardyn cleans it when it starts.",
} as const;

/** KM-2: Review, the lines in Where it runs. */
export const KEEP_REVIEW = {
  /** K-25 */
  MACHINE: (name: string) => `Machine · ${name}`,
  /** K-26, K-25's name when none was typed */
  NEW_UNNAMED: "a new kept machine",
  /** K-20 */
  KEEP: (mac: string, gb: number) => `New kept machine on ${mac}, up to ${gb} GB`,
  /** K-22 */
  RESUME: (name: string, date: string) => `Resumes ${name}, last used ${date}. Wardyn removed the last run's settings and certificates from it.`,
  /** K-23 */
  RESUME_NOT_CLEANED: (name: string, date: string) =>
    `Resumes ${name}, last used ${date}. Its last run didn't stop normally, so Wardyn removes that run's settings and certificates before this one starts.`,
  /** K-24 */
  FRESH: (name: string, mac: string) => `Resets ${name} to ${mac}'s current base, then starts it. Everything on it now is deleted.`,
} as const;

/** KM-3: the run page's Machine card. */
export const KEEP_RUN = {
  /** K-30 */
  TITLE: "Machine",
  /** The card's two rows, as the board draws them. */
  ROW_NAME: "Name",
  ROW_MAC: "Mac",
  ROW_BASE: "Base",
  ROW_WRITTEN: "Written",
  ROW_EXPIRES: "Expires",
  /** K-31 */
  WHILE_RUNNING: "Kept when this run stops",
  /** K-32 */
  NEW: (mac: string) => `New on ${mac} for this run.`,
  /** K-33 without its date: a claim overwrites the machine's last-used time, so the run read cannot say when it was last used before this run. */
  RESUMED: "Resumed.",
  /** K-34 */
  FRESH: (mac: string) => `Started fresh from ${mac}'s current base.`,
  /** K-35 */
  KEPT: (mac: string) => `Kept on ${mac}`,
  /** K-36 */
  SCRUBBED: "Wardyn removed this run's settings, certificate and credential placeholders when it stopped.",
  /** An ended run's card, on a kept machine that still needs its cleanup: said of the machine, never of which run stopped badly. */
  NEEDS_CLEANUP: "This machine still needs cleanup before its next run.",
  /** An ended run's card shows the machine as it is now: a run after this one may have used it since, so no stop result is claimed. */
  AS_NOW: "This is the machine as it is now. A run after this one may have used it since.",
  /** An ended run whose machine a run holds: the run read doesn't say which. */
  BUSY: "This machine is in use now.",
  /** K-37 */
  NOT_SCRUBBED: "This run didn't stop normally. Wardyn removes its settings and certificate when the machine next starts. None of them is a live credential.",
  /** K-38 */
  HELD: (mac: string, name: string) => `Kept on ${mac}. Resuming ${name} ends this run now.`,
  /** K-39 */
  DELETED: "This machine was deleted. The audit log says who deleted it and why.",
  /** K-40 */
  CHIP_KEEPS: "Keeps",
  CHIP_KEPT: "Kept",
  CHIP_DELETING: "Deleting",
  CHIP_DELETED: "Deleted",
  /** K-42 */
  LINK: "Your kept machines",
  /** K-43, the Desktop card's disk note for a kept machine */
  DISK_KEPT: (mac: string, name: string) => `The machine was kept on ${mac} as ${name}.`,
  /** K-44 */
  STOPPING: "Stopping. Wardyn removes this run's settings before it keeps the machine.",
  /** K-45, the Desktop tile's sign-in note on a run that keeps its machine (replaces DESKTOP.signInsNoteRunDisk there) */
  SIGN_INS_NOTE: (mac: string) =>
    `Sign-ins you make in this desktop stay on this kept machine on ${mac} while this machine is kept. The Mac's own snapshots or backups may keep a copy for a while. While the run goes on, the agent can use them too. Closing this tab doesn't remove them.`,
} as const;

/** KM-4: Your settings, Your kept machines. */
export const KEEP_LIST = {
  /** K-50 */
  TITLE: "Your kept machines",
  /** K-51 */
  SUMMARY: (n: number, mac: string) => `${n} on ${mac}`,
  /** K-52 */
  LEDE: (days?: number) => `macOS VMs kept on your own Mac between runs, with your files, tools and sign-ins. Wardyn deletes one ${afterDays(days)}${days === undefined ? "" : " (the current setting)"}. Each machine shows its own date.`,
  /** K-53 */
  META: (mac: string, base: string, gb: number) => `${mac} · ${base} base · ${gb} GB written`,
  /** K-54 */
  DATES: (used: string, exp: string) => `Last used ${used} · Expires ${exp}`,
  /** K-55 */
  CHIP_KEPT: "Kept",
  CHIP_IN_USE: "In use",
  CHIP_PREPARING: "Preparing",
  CHIP_DELETING: "Deleting",
  CHIP_EXPIRED: "Expired",
  CHIP_NOT_CLEANED: "Not cleaned yet",
  /** K-56 */
  IN_USE: (run: string) => `In use by ${run}. Stop that run to delete it.`,
  /** A machine a run is still making: its Mac is preparing it for that run. */
  PREPARING: (run: string) => `Being prepared for ${run}. Stop that run to delete it.`,
  /** K-58 */
  EXPIRES_SOON: (n: number) => `Expires in ${n} ${n === 1 ? "day" : "days"}. Resume it to keep it longer.`,
  /** K-59 */
  EXPIRED: (date: string, mac: string) => `Expired ${date}. Deleted when ${mac} is back online.`,
  /** K-60 */
  NOT_SCRUBBED: "Its last run didn't stop normally. Wardyn removes that run's settings and certificate when it next starts.",
  /** The delete that could not be confirmed (503 kept_machine_delete_unconfirmed): the row stays, as Deleting. */
  DELETE_UNCONFIRMED: "Wardyn couldn't confirm the delete. It will keep trying; the machine can't be resumed meanwhile.",
  DELETE: "Delete",
  /** K-61 */
  CONFIRM_TITLE: (name: string) => `Delete ${name}?`,
  /** K-62 */
  CONFIRM_BODY: (name: string, mac: string) => `This deletes ${name} from ${mac}: your files, tools and sign-ins on it. It can't be undone.`,
  CANCEL: "Cancel",
  /** K-63 */
  EMPTY: "You have no kept machines. On New Run, choose Keep this machine for next time for a macOS VM on your own Mac.",
  /** K-64 */
  TOAST_DELETED: (name: string, mac: string) => `${name} was deleted from ${mac}.`,
  /** K-65 */
  TOAST_DEFERRED: (name: string, mac: string) => `${name} is deleted when ${mac} is back online.`,
  LOAD_FAILED: "Your kept machines couldn't be loaded.",
  RETRY: "Try again",
  /** The name of the Mac when the server did not send one. */
  THIS_MAC: "its Mac",
  NONE: "None",
} as const;

/** KM-5: the three site settings on the macOS virtual machines card. */
export const KEEP_SETTINGS = {
  /** K-70 */
  LABEL: "Allow kept machines",
  /** K-71 */
  HINT: "Off by default. A person can keep their macOS VM on their own Mac between runs, with their files, tools and sign-ins. Wardyn refuses runs that would put lasting credentials (environment secrets, file secrets, SSH keys, git tokens) on a kept machine. Run-scoped tokens can enter the guest; Wardyn cleans them up after a normal stop or before its next run. Off refuses every new keep and resume.",
  /** K-72 */
  IDLE_LABEL: "Delete a kept machine unused for",
  /** K-73 */
  IDLE_HINT: "1 to 90 days. Default 30. Each Mac also deletes its own expired machines, offline too.",
  /** K-74 */
  PER_LABEL: "Kept machines per person",
  /** K-75 */
  PER_HINT: "1 to 4. Default 1. Each Mac also caps the disk its kept machines may use (110 GB unless its owner changes it).",
  /** K-76 */
  IDLE_INVALID: "Enter a whole number of days from 1 to 90.",
  /** K-77 */
  PER_INVALID: "Enter a whole number from 1 to 4.",
} as const;

/** KM-6: the governance profile's deny_kept_machines. */
export const KEEP_POLICY = {
  /** K-80 */
  LABEL: "Deny kept machines",
  /** K-81 */
  HINT: "Stops people keeping a macOS VM between runs or resuming one. Machines already kept stay on their Macs until deleted or expired. Has no effect while kept machines are off for this install.",
  /** K-82 */
  CHIP: "Can't keep machines",
} as const;
