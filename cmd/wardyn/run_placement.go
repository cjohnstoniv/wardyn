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
	cmd.Flags().StringVar(&f.placement, "placement", "", "where the run's sandbox lives: remote (the organisation's executor) or local (a runner you registered); unset lets the server choose when only one placement is eligible")
	cmd.Flags().StringVar(&f.runner, "runner", "", "runner id for --placement local (needed when more than one of your runners is online — see 'wardyn runner')")
	cmd.Flags().StringVar(&f.pool, "pool", "", "runner pool id to start the run on (optional; unset keeps today's placement, and once the server manages pools applies your own default, then the organisation's — see 'wardyn runner pool list')")
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