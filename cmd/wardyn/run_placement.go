// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
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

// runMachineFlags are the `wardyn run` flags that say what kind of sandbox a run gets. Like the placement
// flags they go to the server unchanged, and they stand beside --file because a document never carries them:
// the kind is entered at each launch.
type runMachineFlags struct {
	kind, macOSBase string
}

func (f *runMachineFlags) register(cmd *cobra.Command) {
	cmd.Flags().StringVar(&f.kind, "machine-kind", "", "the kind of sandbox: container (the default), linux_vm or macos_vm; a macos_vm run needs a registered Mac that offers it — see 'wardyn runner'")
	cmd.Flags().StringVar(&f.macOSBase, "macos-base", "", "the macOS base a --machine-kind macos_vm run starts from: Standard (the default) or the name of another base the Mac holds")
}

// apply copies the flags into body after any --file document, which cannot carry either field.
func (f runMachineFlags) apply(body *sdk.CreateRunRequest) {
	body.MachineKind, body.MacOSBase = f.kind, f.macOSBase
}
