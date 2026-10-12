// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"fmt"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

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

// policyStricterMsg: a person's allow for a tool ("*" is their default) that the
// policy holds or blocks. The access step's own sentence, filled in.
func policyStricterMsg(tool string, policy types.ToolEffect) string {
	verb := "asks first for"
	if policy == types.ToolDeny {
		verb = "blocks"
	}
	key := "policy_stricter_tool"
	if tool == "*" {
		key = "policy_stricter_default"
	}
	return strings.NewReplacer("{asks first for|blocks}", verb, "{tool}", tool).Replace(accessSentences[key])
}

// codexBuiltinRuleMsg: a rule on one of Codex CLI's own tools, which no Wardyn
// contract reaches.
func codexBuiltinRuleMsg(tool string) string {
	return fmt.Sprintf("%s is one of Codex CLI's own tools, and Wardyn can't block those. Only its MCP tools can be blocked.", tool)
}

// startupPromptShapeMsg: only the harness that starts with the run can receive a
// starting prompt.
func startupPromptShapeMsg() string {
	return "startup.prompt: only a harness startup takes a starting prompt"
}

// harnessNotInstalledMsg: no sandbox image this server launches carries the
// harness's CLI.
func harnessNotInstalledMsg(name string) string {
	return fmt.Sprintf("%s isn't installed in this server's sandbox images, so the run was not created. Ask an admin to add an image that includes it.", name)
}

// harnessSetNoImageMsg: no declared sandbox image carries the harnesses together.
func harnessSetNoImageMsg(names ...string) string {
	both := "both"
	list := strings.Join(names, " and ")
	if len(names) > 2 {
		both = "all of them"
		list = strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
	}
	return fmt.Sprintf("No sandbox image on this server includes %s together, so the run was not created. Remove one, or ask an admin to add an image with %s.", list, both)
}

// harnessSetCustomImageMsg: several harnesses need the image an admin declared for them.
func harnessSetCustomImageMsg() string {
	return "A run with more than one harness starts from an image your admin declared for them, so it can't use a custom image."
}

// harnessSignInNotPrimaryMsg: an AWS sign-in serves only the run's primary
// harness (harnessProviderNeedsPrimary).
func harnessSignInNotPrimaryMsg(harness, provider string) string {
	return fmt.Sprintf("Model provider %s uses an AWS sign-in, which only the harness a run starts, or else lists first, can use, so the run was not created. "+
		"Make %s that harness, or choose another provider for it.", provider, harness)
}

// harnessProvidersConflictMsg: the proxy keys an injected credential by host, so
// two providers on one host cannot be kept apart.
func harnessProvidersConflictMsg(a, b string) string {
	return fmt.Sprintf("%s and %s use different model providers that reach the same host, so the proxy can't keep their credentials apart. Choose one provider for both, or another provider.", a, b)
}
