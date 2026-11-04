// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"

	"github.com/spf13/cobra"

	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// runPlacementFlags are the `wardyn run` flags that say where a run goes. Placement and runner are passed to the
// server unchanged: it validates them (placement.Request.Validate) and owns the refusals, as with --confinement.
type runPlacementFlags struct {
	placement, runner, pool string
}

func (f *runPlacementFlags) register(cmd *cobra.Command) {
	cmd.Flags().StringVar(&f.placement, "placement", "", "where the run's sandbox lives: remote (a remote-hosted runner) or local (one of your own computers); unset lets the pool decide")
	cmd.Flags().StringVar(&f.runner, "runner", "", "pin a local run to this runner of yours in the pool; unset, the pool picks one of your online runners — see 'wardyn runner'")
	cmd.Flags().StringVar(&f.pool, "pool", "", "runner pool id to start the run on; unset uses your default pool, else the organisation's — see 'wardyn runner pool list'")
}

// apply copies the flags into body. --pool is parsed here so a typo names its flag before any request.
func (f runPlacementFlags) apply(body *sdk.CreateRunRequest) error {
	pool, err := poolIDFlag("--pool", f.pool)
	if err != nil {
		return err
	}
	if pool != nil {
		body.RunnerPoolID = pool.String()
	}
	body.Placement = sdk.Placement(f.placement)
	body.RunnerID = f.runner
	return nil
}

// runMachineFlags are the `wardyn run` flags that say what kind of sandbox a run gets, and whether a macOS VM
// run keeps its machine. Like the placement flags they go to the server unchanged (the keep flags map onto
// CreateRunRequest.Machine one for one), and they stand beside --file because a document never carries them:
// the kind is entered at each launch. Only flags that contradict each other are refused here; whether the run
// may keep or resume a machine is the server's to judge and word.
type runMachineFlags struct {
	kind, macOSBase string
	window          bool
	keep            bool
	name, resume    string
	fresh           bool
}

func (f *runMachineFlags) register(cmd *cobra.Command) {
	cmd.Flags().StringVar(&f.kind, "machine-kind", "", "the kind of sandbox: container (the default), linux_vm or macos_vm; a macos_vm run needs a registered Mac that offers it — see 'wardyn runner'")
	cmd.Flags().StringVar(&f.macOSBase, "macos-base", "", "the macOS base a --machine-kind macos_vm run starts from: Standard (the default) or the name of another base the Mac holds")
	cmd.Flags().BoolVar(&f.window, "window", false, "open a window on the run's Mac once the VM is up and prepared (a --machine-kind macos_vm run on a Mac you registered); like 'wardyn window'. Ask again with 'wardyn window' if it does not")
	cmd.Flags().BoolVar(&f.keep, "keep-machine", false, "keep this run's macOS VM on your own Mac when the run stops (with --experience interactive --machine-kind macos_vm); see 'wardyn machines'")
	cmd.Flags().StringVar(&f.name, "machine-name", "", "the name of the machine --keep-machine keeps; empty lets the server pick one")
	cmd.Flags().StringVar(&f.resume, "resume-machine", "", "start this run from a machine you kept earlier, by id or name (with --experience interactive --machine-kind macos_vm)")
	cmd.Flags().BoolVar(&f.fresh, "fresh", false, "with --resume-machine: delete what is on the machine and start it again from the Mac's current base")
	addPreRunE(cmd, func(cmd *cobra.Command, _ []string) error { return f.check(cmd.Flags().Changed("file")) })
}

// addPreRunE runs check after the command's earlier PreRunE, so each flag group's register can add its own
// check: a plain assignment would drop the one registered before it.
func addPreRunE(cmd *cobra.Command, check func(*cobra.Command, []string) error) {
	prev := cmd.PreRunE
	cmd.PreRunE = func(c *cobra.Command, args []string) error {
		if prev != nil {
			if err := prev(c, args); err != nil {
				return err
			}
		}
		return check(c, args)
	}
}

// check refuses the keep flags that contradict each other, or a --file document, before any request. Each
// message is a plain sentence that says what to change.
func (f runMachineFlags) check(withFile bool) error {
	switch {
	case !f.keep && f.name == "" && f.resume == "" && !f.fresh:
		return nil
	case withFile:
		return errors.New("a run document can't keep or resume a machine: use the flags for that run instead of --file")
	case f.keep && f.resume != "":
		return errors.New("--keep-machine and --resume-machine can't go together: --keep-machine keeps a new machine, and --resume-machine starts from one you kept (it is kept again when the run stops)")
	case f.name != "" && f.resume != "":
		return errors.New("--machine-name names a new machine and can't go with --resume-machine, which already names the machine to resume")
	case f.name != "" && !f.keep:
		return errors.New("--machine-name names the machine --keep-machine keeps: add --keep-machine, or drop --machine-name")
	case f.fresh && f.resume == "":
		return errors.New("--fresh starts a machine you resume over again: add --resume-machine <id|name>, or drop --fresh")
	}
	return nil
}

// apply copies the flags into body after any --file document, which cannot carry any of these fields.
func (f runMachineFlags) apply(body *sdk.CreateRunRequest) {
	body.MachineKind, body.MacOSBase = f.kind, f.macOSBase
	if f.window {
		body.DesktopWindow = sdk.DesktopWindowAtStart
	}
	switch {
	case f.keep:
		body.Machine = &sdk.RunMachine{Keep: true, Name: f.name}
	case f.resume != "":
		body.Machine = &sdk.RunMachine{Resume: f.resume, Fresh: f.fresh}
	}
}
