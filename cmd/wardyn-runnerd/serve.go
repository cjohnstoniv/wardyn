// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"

	"github.com/spf13/cobra"

	"github.com/cjohnstoniv/wardyn/internal/runneridentity"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
)

type runnerHooksBuilder func(context.Context, runneridentity.Identity, string) (runnerwire.RunnerHooks, error)

func newServeCommand(build runnerHooksBuilder) *cobra.Command {
	var org, stateDir string
	cmd := &cobra.Command{Use: "serve", Short: "Connect this registered runner to its organisation", Args: cobra.NoArgs}
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		if stateDir == "" {
			var err error
			stateDir, err = runneridentity.DefaultDir()
			if err != nil {
				return err
			}
		}
		// Validate the persisted key, mode and organisation binding before a
		// builder can contact Docker or any network endpoint.
		identity, err := runneridentity.Load(stateDir, org)
		if err != nil {
			return err
		}
		if build == nil {
			return errors.New("runner execution and durable offline handlers are unavailable")
		}
		hooks, err := build(cmd.Context(), identity, stateDir)
		if err != nil {
			return err
		}
		client, err := runnerwire.NewClient(runnerwire.ClientConfig{OrgURL: org, RunnerID: identity.RunnerID.String(), PrivateKey: identity.PrivateKey, Hooks: hooks})
		if err != nil {
			return err
		}
		return client.Run(cmd.Context())
	}
	cmd.Flags().StringVar(&org, "org", "", "organisation public HTTPS URL used at registration")
	cmd.Flags().StringVar(&stateDir, "state-dir", "", "private runner state directory (defaults to the user configuration directory)")
	_ = cmd.MarkFlagRequired("org")
	return cmd
}

// productionRunnerHooks is the final composition point for H4's stream/output
// adapter and H6's durable pending/state/spool/lease enforcement. Until both
// production implementations are linked, serving must fail before connecting.
// This is an explicit incomplete release dependency, not an empty-state mode.
func productionRunnerHooks(context.Context, runneridentity.Identity, string) (runnerwire.RunnerHooks, error) {
	return runnerwire.RunnerHooks{}, errors.New("runner serve requires the production stream/output and durable offline/spool adapters; this build cannot serve runs yet")
}
