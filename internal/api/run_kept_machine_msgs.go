// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import "fmt"

// The sentences of a run's kept-machine refusals. Their reasons are in reasons_kept_machine.go, where
// every string constant is read as a reason, so the sentences live here. No sentence carries a backtick.

// Request shape (400).
const (
	keptMachineNameRuleMsg    = "Kept machine names are 1 to 64 letters, digits, spaces, hyphens or underscores."
	keptMachineKeepOrResume   = "Ask for one of machine.keep and machine.resume, not both."
	keptMachineFreshNeedsMsg  = "machine.fresh starts a kept machine again, so it needs machine.resume."
	keptMachineNameNeedsKeep  = "machine.name names a new kept machine, so it needs machine.keep."
	keptMachineNothingAsked   = "Set machine.keep, or machine.resume: machine asks for nothing."
	keptMachineSiteOffMsg     = "Kept machines are turned off for this install. An admin can turn on macos_vm.allow_kept_machines."
	keptMachineInteractiveMsg = "Only an interactive macOS VM run can keep or resume a machine."
	keptMachineGovernanceMsg  = "Your organisation's governance profile doesn't allow kept machines."
	keptMachineWorkFolderMsg  = "A kept machine keeps its work folder already, so don't also keep the work folder as a drive."
	keptMachinePoolMsg        = "Kept machines stay on one Mac, and this run could start on any Mac in its pool. Pick your own Mac to keep a machine."
	keptMachineExpiredMsg     = "This machine expired and will be deleted."
)

// keptRunNotYourMacMsg is the not-your-Mac refusal, naming the Mac; "This Mac" when it is not known.
func keptRunNotYourMacMsg(mac string) string {
	if mac == "" {
		mac = "This Mac"
	}
	return fmt.Sprintf("Wardyn keeps a machine only on a Mac whose runner you registered, for runs you start yourself. %s isn't yours, so this run can't keep or resume one.", mac)
}

// keptMachineResidentSecretsMsg is the lasting-credential refusal, naming the kinds in English.
func keptMachineResidentSecretsMsg(kinds []string) string {
	return fmt.Sprintf("This run would put %s inside the machine, and a kept machine would keep them on its disk. Use credentials the proxy adds, or don't keep this machine.", andList(kinds))
}

// keptSecretKinds is the {kinds} word list, in the order the sentence names them.
var keptSecretKinds = []string{keptKindEnv, keptKindFile, keptKindSSH, keptKindGit}

// The words, never a sentence's start, so vars: the sentence census reads every string const.
var (
	keptKindEnv  = "environment secrets"
	keptKindFile = "file secrets"
	keptKindSSH  = "SSH keys"
	keptKindGit  = "git tokens"
)

func keptMachineInUseMsg(name, run string) string {
	return fmt.Sprintf("Your machine %s is in use by run %s. Stop that run first.", name, run)
}

func keptMachineBaseTooOldMsg(name string, age, maxDays int) string {
	return fmt.Sprintf("Your machine %s was made from a macOS base prepared %d days ago, past your organisation's %d-day limit. Start it fresh, or delete it.", name, age, maxDays)
}

func keptMachineOfflineMsg(mac string) string {
	return fmt.Sprintf("%s is offline, so its kept machine can't start.", mac)
}

// keptMachineElsewhereMsg is a resume whose machine's Mac is not the one the run can have now.
func keptMachineElsewhereMsg(name, mac string) string {
	return fmt.Sprintf("This run would resume your kept machine %s on %s, which isn't available right now.", name, mac)
}

func keptMachineRunnerTooOldMsg(mac string) string {
	return fmt.Sprintf("%s can't keep machines. Upgrade it to 0.10.0-rc.12 or later.", mac)
}

// keptMachinePersonLimitMsg is the per-person limit, singular at a limit of one.
func keptMachinePersonLimitMsg(n, limit int) string {
	if limit == 1 {
		return fmt.Sprintf("%d of 1 kept machine used", n)
	}
	return fmt.Sprintf("%d of %d kept machines used", n, limit)
}

func keptMachineMacLimitMsg(mac string, limitGB, needGB int64) string {
	return fmt.Sprintf("%s keeps machines up to %d GB, and another would need up to %d GB more. Delete one with wardyn machines delete, or raise the limit on that Mac.", mac, limitGB, needGB)
}

// keptMachineMacOffMsg is a Mac whose kept-disk limit is 0.
func keptMachineMacOffMsg(mac string) string {
	return mac + " doesn't keep machines."
}

func keptMachineNameTakenMsg(name string) string {
	return fmt.Sprintf("You already have a kept machine named %s.", name)
}

// keptMachineFreshBaseMsg refuses a fresh start that names a base other than its machine's own.
func keptMachineFreshBaseMsg(base string) string {
	return fmt.Sprintf("A fresh start keeps your kept machine's base, %s. Start a new kept machine to use a different base.", base)
}

// keptMachineResetUnconfirmedMsg refuses a normal resume of a machine whose fresh start never confirmed.
func keptMachineResetUnconfirmedMsg(name string) string {
	return fmt.Sprintf("Your machine %s was being started fresh when its run ended, and Wardyn can't tell whether that finished. Start it fresh to use it.", name)
}

// keptMachineExpiredDeletedMsg is an expired machine's refusal once its Mac confirmed the delete.
func keptMachineExpiredDeletedMsg(name string, days int) string {
	return fmt.Sprintf("Your machine %s wasn't used for %d days, so it was deleted. Start a new one.", name, days)
}
