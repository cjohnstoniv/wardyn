// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// claimIntoPool puts a runner just claimed into the person's own user pool named arg (an id or an exact
// name), makes that pool first if the person has none by that name, and with makeDefault makes it their
// default. A runner in no pool runs nothing, so this is what turns a claim into a runner that is used.
// The claim has already happened, so a failure here says so and names the command that finishes it.
func claimIntoPool(cmd *cobra.Command, c *sdk.Client, runnerID uuid.UUID, arg string, makeDefault bool) error {
	pool, fresh, err := ownPoolNamed(cmd, c, arg)
	if err == nil {
		err = poolMembers(cmd, c, pool, []string{runnerID.String()}, true, fresh)
	}
	if err == nil && makeDefault {
		err = makePoolDefault(cmd, c, pool)
	}
	if err != nil {
		return fmt.Errorf("the runner is claimed but is not in pool %q: %w; finish with `wardyn runner pool add`", arg, err)
	}
	return nil
}

// ownPoolNamed is the person's user pool an argument names (its id, or a name sdk.Client.OwnRunnerPool
// finds or makes), and whether it was just made for it.
func ownPoolNamed(cmd *cobra.Command, c *sdk.Client, arg string) (pool sdk.RunnerPool, fresh bool, err error) {
	if id, perr := uuid.Parse(arg); perr == nil {
		pool, err = c.GetRunnerPool(cmd.Context(), id)
		return pool, false, err
	}
	pool, fresh, err = c.OwnRunnerPool(cmd.Context(), arg)
	if fresh {
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "created user pool %s (%s)\n", pool.Name, pool.ID)
	}
	return pool, fresh, err
}
