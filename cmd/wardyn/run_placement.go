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
	cmd.Flags().StringVar(&f.placement, "placement", "", "where the run's sandbox lives: remote (an organisation executor) or local (one of your own runners in a self-hosted pool); unset lets the pool decide")
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
