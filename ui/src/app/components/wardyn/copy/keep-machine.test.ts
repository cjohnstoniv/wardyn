/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it } from "vitest";
import { gbOf, keptCount, KEEP_LIST, KEEP_NEW_RUN, KEEP_POLICY, KEEP_REVIEW, KEEP_RUN, KEEP_SETTINGS } from "./keep-machine";

// Boards KM-1 to KM-6, character for character, with the joint verdict's corrections (K-04, K-71, K-14, K-45, KS-12).
// Editing one is a copy change and needs the boards' sign-off.
const CANON: [id: string, actual: string, canon: string][] = [
  ["K-01", KEEP_NEW_RUN.LABEL, "Between runs"],
  ["K-02", KEEP_NEW_RUN.DELETE, "Delete this machine when the run ends"],
  ["K-03", KEEP_NEW_RUN.KEEP, "Keep this machine for next time"],
  ["K-04", KEEP_NEW_RUN.KEEP_DETAIL("Maya's MacBook", 51), "Kept on Maya's MacBook, up to 51 GB. It stays until you delete it, or until it has gone unused for the time your admin sets. Your files, tools and sign-ins stay. Wardyn cleans up the settings and certificates it added when the run stops, or before the machine's next run if the stop was abrupt. The run page says if anything was left."],
  ["K-05", KEEP_NEW_RUN.NAME, "Name"],
  ["K-06", KEEP_NEW_RUN.NAME_HINT, "Yours alone, for resuming it later: 1 to 64 letters, digits, spaces, - and _. Leave it empty and Wardyn picks one."],
  ["K-07", KEEP_NEW_RUN.SECRETS, "A kept machine can't take secrets that live inside it: environment secrets, file secrets, SSH keys and git tokens. Use credentials the proxy adds."],
  ["K-08", KEEP_NEW_RUN.RESUME("dev"), "Resume dev"],
  ["K-09", KEEP_NEW_RUN.RESUME_META("Nov 1", "Standard", 12), "Last used Nov 1 · Standard base · 12 GB written"],
  ["K-10", KEEP_NEW_RUN.FRESH, "Start fresh"],
  ["K-11", KEEP_NEW_RUN.FRESH_HINT("dev", "Maya's MacBook"), "Deletes everything on dev and starts it again from Maya's MacBook's current base, under the same name."],
  ["KS-fresh-base", KEEP_NEW_RUN.FRESH_BASE("Standard"), "A fresh start keeps your kept machine's base, Standard. Start a new kept machine to use a different base."],
  ["K-12", keptCount(1, 2), "1 of 2 kept machines used"],
  ["K-12 singular", keptCount(1, 1), "1 of 1 kept machine used"],
  ["K-13", KEEP_NEW_RUN.IN_USE("ui-polish"), "In use by ui-polish. Stop that run first."],
  ["K-14", KEEP_NEW_RUN.SIGN_INS("Maya's MacBook"), "Kept on this machine's disk on Maya's MacBook while this machine is kept. The Mac's own snapshots or backups may keep a copy for a while."],
  ["K-15", KEEP_NEW_RUN.NOT_SCRUBBED, "Its last run didn't stop normally; Wardyn cleans it when it starts."],
  ["K-20", KEEP_REVIEW.KEEP("Maya's MacBook", 51), "New kept machine on Maya's MacBook, up to 51 GB"],
  ["K-22", KEEP_REVIEW.RESUME("dev", "Nov 1"), "Resumes dev, last used Nov 1. Wardyn removed the last run's settings and certificates from it."],
  ["K-23", KEEP_REVIEW.RESUME_NOT_CLEANED("dev", "Nov 1"), "Resumes dev, last used Nov 1. Its last run didn't stop normally, so Wardyn removes that run's settings and certificates before this one starts."],
  ["K-24", KEEP_REVIEW.FRESH("dev", "Maya's MacBook"), "Resets dev to Maya's MacBook's current base, then starts it. Everything on it now is deleted."],
  ["K-25", KEEP_REVIEW.MACHINE("dev"), "Machine · dev"],
  ["K-26", KEEP_REVIEW.NEW_UNNAMED, "a new kept machine"],
  ["K-30", KEEP_RUN.TITLE, "Machine"],
  ["K-31", KEEP_RUN.WHILE_RUNNING, "Kept when this run stops"],
  ["K-32", KEEP_RUN.NEW("Maya's MacBook"), "New on Maya's MacBook for this run."],
  ["K-33 (no date)", KEEP_RUN.RESUMED, "Resumed."],
  ["K-34", KEEP_RUN.FRESH("Maya's MacBook"), "Started fresh from Maya's MacBook's current base."],
  ["K-35", KEEP_RUN.KEPT("Maya's MacBook"), "Kept on Maya's MacBook"],
  ["K-36", KEEP_RUN.SCRUBBED, "Wardyn removed this run's settings, certificate and credential placeholders when it stopped."],
  ["K-37", KEEP_RUN.NOT_SCRUBBED, "This run didn't stop normally. Wardyn removes its settings and certificate when the machine next starts. None of them is a live credential."],
  ["K-38", KEEP_RUN.HELD("Maya's MacBook", "dev"), "Kept on Maya's MacBook. Resuming dev ends this run now."],
  ["K-39", KEEP_RUN.DELETED, "This machine was deleted. The audit log says who deleted it and why."],
  ["K-40", [KEEP_RUN.CHIP_KEEPS, KEEP_RUN.CHIP_KEPT, KEEP_RUN.CHIP_DELETING, KEEP_RUN.CHIP_DELETED].join(" · "), "Keeps · Kept · Deleting · Deleted"],
  ["K-42", KEEP_RUN.LINK, "Your kept machines"],
  ["K-43", KEEP_RUN.DISK_KEPT("Maya's MacBook", "dev"), "The machine was kept on Maya's MacBook as dev."],
  ["K-44", KEEP_RUN.STOPPING, "Stopping. Wardyn removes this run's settings before it keeps the machine."],
  ["K-45", KEEP_RUN.SIGN_INS_NOTE("Maya's MacBook"), "Sign-ins you make in this desktop stay on this kept machine on Maya's MacBook while this machine is kept. The Mac's own snapshots or backups may keep a copy for a while. While the run goes on, the agent can use them too. Closing this tab doesn't remove them."],
  ["K-50", KEEP_LIST.TITLE, "Your kept machines"],
  ["K-51", KEEP_LIST.SUMMARY(2, "Maya's MacBook"), "2 on Maya's MacBook"],
  ["K-04 with the idle days", KEEP_NEW_RUN.KEEP_DETAIL("Maya's MacBook", 51, 30), "Kept on Maya's MacBook, up to 51 GB. It stays until you delete it, or 30 days after its last use. Your files, tools and sign-ins stay. Wardyn cleans up the settings and certificates it added when the run stops, or before the machine's next run if the stop was abrupt. The run page says if anything was left."],
  ["K-04 at one day", KEEP_NEW_RUN.KEEP_DETAIL("Maya's MacBook", 51, 1).split(" Your files")[0], "Kept on Maya's MacBook, up to 51 GB. It stays until you delete it, or 1 day after its last use."],
  ["K-52 with the idle days", KEEP_LIST.LEDE(7), "macOS VMs kept on your own Mac between runs, with your files, tools and sign-ins. Wardyn deletes one 7 days after its last use (the current setting). Each machine shows its own date."],
  ["K-52", KEEP_LIST.LEDE(), "macOS VMs kept on your own Mac between runs, with your files, tools and sign-ins. Wardyn deletes one after it goes unused for the time your admin sets. Each machine shows its own date."],
  ["K-53", KEEP_LIST.META("Maya's MacBook", "Standard", 12), "Maya's MacBook · Standard base · 12 GB written"],
  ["K-54", KEEP_LIST.DATES("Nov 1", "Dec 1"), "Last used Nov 1 · Expires Dec 1"],
  ["K-55", [KEEP_LIST.CHIP_KEPT, KEEP_LIST.CHIP_IN_USE, KEEP_LIST.CHIP_DELETING, KEEP_LIST.CHIP_EXPIRED, KEEP_LIST.CHIP_NOT_CLEANED].join(" · "), "Kept · In use · Deleting · Expired · Not cleaned yet"],
  ["K-56", KEEP_LIST.IN_USE("ui-polish"), "In use by ui-polish. Stop that run to delete it."],
  ["read failed", KEEP_NEW_RUN.READ_FAILED, "Wardyn couldn't read your kept machines. Try again."],
  ["machine gone", KEEP_NEW_RUN.GONE, "Your kept machine is gone. This run starts a new machine, deleted when the run ends."],
  ["keep unavailable", KEEP_NEW_RUN.UNAVAILABLE, "Keeping a machine isn't available for this run, so its machine is deleted when the run ends."],
  ["interactive only", KEEP_NEW_RUN.INTERACTIVE_ONLY, "Only an interactive macOS VM run can keep or resume a machine, so this run's machine is deleted when it ends."],
  ["keep at limit", KEEP_NEW_RUN.AT_LIMIT, "You've reached your limit of kept machines, so this run's machine is deleted when it ends."],
  ["machine as it is now", KEEP_RUN.AS_NOW, "This is the machine as it is now. A run after this one may have used it since."],
  ["machine in use now", KEEP_RUN.BUSY, "This machine is in use now."],
  ["needs cleanup", KEEP_RUN.NEEDS_CLEANUP, "This machine still needs cleanup before its next run."],
  ["preparing", KEEP_LIST.PREPARING("ui-polish"), "Being prepared for ui-polish. Stop that run to delete it."],
  ["K-58", KEEP_LIST.EXPIRES_SOON(3), "Expires in 3 days. Resume it to keep it longer."],
  ["K-58 singular", KEEP_LIST.EXPIRES_SOON(1), "Expires in 1 day. Resume it to keep it longer."],
  ["K-59", KEEP_LIST.EXPIRED("Nov 2", "Maya's MacBook"), "Expired Nov 2. Deleted when Maya's MacBook is back online."],
  ["K-60", KEEP_LIST.NOT_SCRUBBED, "Its last run didn't stop normally. Wardyn removes that run's settings and certificate when it next starts."],
  ["delete unconfirmed", KEEP_LIST.DELETE_UNCONFIRMED, "Wardyn couldn't confirm the delete. It will keep trying; the machine can't be resumed meanwhile."],
  ["K-61", KEEP_LIST.CONFIRM_TITLE("dev"), "Delete dev?"],
  ["K-62", KEEP_LIST.CONFIRM_BODY("dev", "Maya's MacBook"), "This deletes dev from Maya's MacBook: your files, tools and sign-ins on it. It can't be undone."],
  ["K-63", KEEP_LIST.EMPTY, "You have no kept machines. On New Run, choose Keep this machine for next time for a macOS VM on your own Mac."],
  ["K-64", KEEP_LIST.TOAST_DELETED("dev", "Maya's MacBook"), "dev was deleted from Maya's MacBook."],
  ["K-65", KEEP_LIST.TOAST_DEFERRED("dev", "Maya's MacBook"), "dev is deleted when Maya's MacBook is back online."],
  ["K-70", KEEP_SETTINGS.LABEL, "Allow kept machines"],
  ["K-71", KEEP_SETTINGS.HINT, "Off by default. A person can keep their macOS VM on their own Mac between runs, with their files, tools and sign-ins. Wardyn refuses runs that would put lasting credentials (environment secrets, file secrets, SSH keys, git tokens) on a kept machine. Run-scoped tokens can enter the guest; Wardyn cleans them up after a normal stop or before its next run. Off refuses every new keep and resume."],
  ["K-72", KEEP_SETTINGS.IDLE_LABEL, "Delete a kept machine unused for"],
  ["K-73", KEEP_SETTINGS.IDLE_HINT, "1 to 90 days. Default 30. Each Mac also deletes its own expired machines, offline too."],
  ["K-74", KEEP_SETTINGS.PER_LABEL, "Kept machines per person"],
  ["K-75", KEEP_SETTINGS.PER_HINT, "1 to 4. Default 1. Each Mac also caps the disk its kept machines may use (110 GB unless its owner changes it)."],
  ["K-76", KEEP_SETTINGS.IDLE_INVALID, "Enter a whole number of days from 1 to 90."],
  ["K-77", KEEP_SETTINGS.PER_INVALID, "Enter a whole number from 1 to 4."],
  ["K-80", KEEP_POLICY.LABEL, "Deny kept machines"],
  ["K-81", KEEP_POLICY.HINT, "Stops people keeping a macOS VM between runs or resuming one. Machines already kept stay on their Macs until deleted or expired. Has no effect while kept machines are off for this install."],
  ["K-82", KEEP_POLICY.CHIP, "Can't keep machines"],
];

describe("kept machine copy", () => {
  it.each(CANON)("%s is the approved board string", (_id, actual, canon) => {
    expect(actual).toBe(canon);
  });

  it("says a disk size in whole GB", () => {
    expect(gbOf(12 * 1024 ** 3)).toBe(12);
    expect(gbOf(51 * 1024 ** 3)).toBe(51);
  });

  it("carries no backtick and no em dash", () => {
    for (const [, actual] of CANON) {
      expect(actual).not.toMatch(/[`—]/);
    }
  });
});
