// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runneridentity"
	"github.com/cjohnstoniv/wardyn/internal/types"
	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
	"github.com/google/uuid"
	"github.com/spf13/cobra"
)

// claimStep finishes setup with the person's claim. Without --claim and a personal API token it only
// says how the person claims, which is what device management leaves for them. The service is already
// running, so a failure here names the command that finishes it.
func claimStep(cmd *cobra.Command, plan setupPlan, id runneridentity.Identity, token string) error {
	out := cmd.OutOrStdout()
	if !plan.claim || token == "" {
		if plan.claim {
			fmt.Fprintln(out, "--claim needs WARDYN_TOKEN, your own personal API token; it was not set, so nothing was claimed.")
		}
		fmt.Fprintf(out, "Key fingerprint %s. Claim this runner as its owner in the console (Your account → My runner), or run\n  WARDYN_TOKEN=<your personal API token> wardyn runner claim --pool <name> --default\n", id.Fingerprint)
		return nil
	}
	c := sdk.New(plan.org, token)
	c.HTTPClient = &http.Client{Timeout: 30 * time.Second}
	if err := claimRunner(cmd.Context(), c, id, plan, out); err != nil {
		return fmt.Errorf("the runner is installed and running but not claimed into place: %w; finish with `wardyn runner claim --pool <name>`", err)
	}
	return nil
}

// claimRunner claims the runner unless its owner already has, then puts it in the pool and makes that
// their default when asked, so a repeat run leaves the same state.
func claimRunner(ctx context.Context, c *sdk.Client, id runneridentity.Identity, plan setupPlan, out io.Writer) error {
	view, err := c.GetMyRunner(ctx, id.RunnerID)
	if err != nil {
		return err
	}
	if view.State == types.RunnerClaimed {
		fmt.Fprintf(out, "Runner %s is already claimed.\n", id.RunnerID)
	} else {
		if _, err := c.ClaimRunner(ctx, id.RunnerID, id.Fingerprint); err != nil {
			return err
		}
		fmt.Fprintf(out, "Claimed runner %s with fingerprint %s\n", id.RunnerID, id.Fingerprint)
	}
	if plan.pool == "" {
		return nil
	}
	pool, err := ownPool(ctx, c, plan.pool, out)
	if err != nil {
		return err
	}
	if _, err := c.AddMyRunnerToPool(ctx, pool.ID, id.RunnerID); err != nil {
		return err
	}
	fmt.Fprintf(out, "%s is in pool %s\n", view.Name, pool.Name)
	if !plan.makeDefault {
		return nil
	}
	d, err := c.GetMyRunnerPoolDefaults(ctx)
	if err != nil {
		return err
	}
	d.PoolID = &pool.ID
	if _, err := c.SetMyRunnerPoolDefaults(ctx, d); err != nil {
		return err
	}
	fmt.Fprintf(out, "default pool set to %s\n", pool.Name)
	return nil
}

// ownPool is the person's user pool of this name, made when they have none. A pool of an organization is
// not theirs to put a self-hosted runner in, so one with the same name is passed over. The list is the
// pools the caller may use (GET /me/runner-pools), where a user-scope row is their own.
func ownPool(ctx context.Context, c *sdk.Client, name string, out io.Writer) (sdk.RunnerPool, error) {
	list, err := c.ListMyRunnerPools(ctx, "")
	if err != nil {
		return sdk.RunnerPool{}, err
	}
	var ids []uuid.UUID
	for _, p := range list.Pools {
		if p.Scope == sdk.RunnerPoolUser && strings.EqualFold(p.Name, name) {
			ids = append(ids, p.ID)
		}
	}
	switch len(ids) {
	case 0:
		pool, err := c.CreateMyRunnerPool(ctx, sdk.CreateMyRunnerPoolRequest{Name: name})
		if err == nil {
			fmt.Fprintf(out, "created user pool %s (%s)\n", pool.Name, pool.ID)
		}
		return pool, err
	case 1:
		return c.GetRunnerPool(ctx, ids[0])
	}
	return sdk.RunnerPool{}, fmt.Errorf("%d of your pools are named %q; use `wardyn runner claim --pool <id>`", len(ids), name)
}
