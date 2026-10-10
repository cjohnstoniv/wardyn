// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import "fmt"

// The sentences of the run-mode refusals New Run shows inline. The console
// prints the server's sentence on a refusal and uses the same bytes for its own
// pre-check (ui/src/app/lib/run-mode-refusals.ts), so the two never disagree;
// TestRunModeRefusalSentencesMatchGolden pins both sides to one table
// (ui/src/app/lib/run-mode-refusals.golden.json). Change a sentence in both
// files and the table together.

func runModeRequiredMsg() string {
	return "Choose what you're starting: a background task or an interactive environment."
}

func runWorkloadRequiredMsg() string {
	return "A background task needs something to run. Choose an agent task or a command."
}

// runModeConflictMsg says a field cannot ride beside the run-mode fields and
// what carries it instead.
func runModeConflictMsg(field, instead string) string {
	return fmt.Sprintf("%s cannot be sent with the run-mode fields. %s", field, instead)
}

func startupToolUnknownMsg(tool string) string {
	return fmt.Sprintf("The startup names %s, which this run does not include. Include it as a tool or choose another startup.", tool)
}

func startFolderAttachmentMsg(attachment string) string {
	return fmt.Sprintf("%s is not attached to this run, so it cannot hold the starting folder.", attachment)
}

func startFolderShapeMsg(subpath string) string {
	return fmt.Sprintf("%s is not a folder inside the attachment. Use a relative path with no \"..\".", subpath)
}

func noRepositoriesConflictMsg() string {
	return "You chose no repositories or drives, but something is attached. Remove it or remove the choice."
}

// runModeUnavailableMsg is the refusal of a valid field this server cannot honour
// yet: said out loud, never dropped.
func runModeUnavailableMsg(path string) string {
	return fmt.Sprintf("%s is not available on this server yet, so the run was not created.", path)
}

// runBackgroundOnlyMsg is what every interactive door of a background run says;
// door names the surface ("terminal", "SSH access", ...).
func runBackgroundOnlyMsg(door string) string {
	return fmt.Sprintf("This run is a background task, so it has no %s. Its logs, audit trail and status stay available.", door)
}

// idleStopBackgroundMsg: a background task ends when its command or agent exits,
// so a stop-when-idle setting has nothing to apply to.
func idleStopBackgroundMsg() string {
	return "A background task ends when its work exits, so it has no idle stop. Remove it, or choose an interactive environment."
}
