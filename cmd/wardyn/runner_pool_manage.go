// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// runner_pool_manage.go — create, show, change and delete a pool, and put runners in it. A user pool is
// your own: `create` makes one, and `update` and `delete` reach it through your own routes. An
// organization pool is the organization's: `create --scope organization`, and `update` or `delete` on
// one, need an admin. <pool> is a pool id or its exact name; <runner> a runner id or name, a self-hosted
// runner of yours, an organization runner (an admin, into an organization pool) or a remote-hosted runner.
//
//	wardyn runner pool create --name Laptop [--scope user|organization] [--runner <runner>]... [--limits-file limits.json]
//	wardyn runner pool show <pool>
//	wardyn runner pool update <pool> --switch off
//	wardyn runner pool delete <pool> --yes
//	wardyn runner pool add <pool> <runner>...
//	wardyn runner pool remove <pool> <runner>...
//
// --limits-file is the wire `limits` block as JSON (what `show --json` prints), "-" for stdin. An
// update that names it replaces the pool's limits whole.

func runnerPoolManageCmds(client clientFn) []*cobra.Command {
	return []*cobra.Command{runnerPoolCreateCmd(client), runnerPoolShowCmd(client), runnerPoolUpdateCmd(client), runnerPoolDeleteCmd(client),
		runnerPoolMemberCmd(client, true), runnerPoolMemberCmd(client, false)}
}

// runnerPoolMemberCmd is `add` (add true) or `remove`: runners into or out of a pool. A self-hosted
// runner goes only into one of your own user pools; a remote-hosted runner into an organization pool (an
// admin) or into your own user pool when it is available to people's pools. Whether each may join is the
// server's to say; its sentence is printed.
func runnerPoolMemberCmd(client clientFn, add bool) *cobra.Command {
	use, short := "add <pool> <runner>...", "Put runners in a pool: your self-hosted runners and available remote-hosted runners in your user pool, or (an admin) remote-hosted runners and organization runners in an organization pool"
	if !add {
		use, short = "remove <pool> <runner>...", "Take runners out of a pool"
	}
	var makeDefault bool
	cmd := &cobra.Command{Use: use, Short: short, Args: cobra.MinimumNArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
		pool, err := readPoolArg(cmd, client, args[0])
		if err != nil {
			return err
		}
		if err := poolMembers(cmd, client(), pool, args[1:], add, false); err != nil || !makeDefault {
			return err
		}
		return makePoolDefault(cmd, client(), pool)
	}}
	if add {
		cmd.Flags().BoolVar(&makeDefault, "default", false, "also make the pool your default, the one New Run starts with")
	}
	return cmd
}

// makePoolDefault makes pool the caller's default pool; whatever else the read carried is sent back as read.
func makePoolDefault(cmd *cobra.Command, c *sdk.Client, pool sdk.RunnerPool) error {
	d, err := c.GetMyRunnerPoolDefaults(cmd.Context())
	if err != nil {
		return err
	}
	d.PoolID = &pool.ID
	if _, err := c.SetMyRunnerPoolDefaults(cmd.Context(), d); err != nil {
		return err
	}
	kind := "pool"
	if pool.Scope == sdk.RunnerPoolUser {
		kind = "user pool"
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "default %s set to %s\n", kind, pool.Name)
	return err
}

// poolMembers adds or removes each runner, through the organization's routes for a remote-hosted or
// organization runner in an organization pool and through your own otherwise, and says what is now true. Every runner is
// read, and an unclaimed one refused, before the first write; one already in the pool is said so and not
// added again. With several runners, each one the server refuses is said on its own line after the ones
// that went through, and the command fails; a lone runner's refusal is returned as it is. fresh says
// the pool was just made, so there is nothing to read.
func poolMembers(cmd *cobra.Command, c *sdk.Client, pool sdk.RunnerPool, runnerArgs []string, add, fresh bool) error {
	ctx := cmd.Context()
	mine := pool.Scope == sdk.RunnerPoolUser
	remotes := func() ([]sdk.RemoteRunnerView, error) {
		if mine {
			return c.ListMyRemoteRunners(ctx)
		}
		return c.ListRemoteRunners(ctx)
	}
	var orgs func() ([]sdk.RunnerView, error)
	if !mine {
		var cached []sdk.RunnerView
		var read bool
		orgs = func() ([]sdk.RunnerView, error) {
			var err error
			if !read {
				cached, err = orgRunners(cmd, c)
				read = err == nil
			}
			return cached, err
		}
	}
	var members []sdk.RunnerPoolMemberView
	if !fresh {
		var err error
		if members, err = c.ListRunnerPoolMembers(ctx, pool.ID); err != nil {
			return err
		}
	}
	refs := make([]runnerRef, 0, len(runnerArgs))
	for _, arg := range runnerArgs {
		var ref runnerRef
		var err error
		if add {
			ref, err = resolveRunner(cmd, c, arg, remotes, orgs)
		} else {
			ref, err = resolveMember(pool, members, arg)
		}
		if err != nil {
			return err
		}
		refs = append(refs, ref)
	}
	if add {
		if err := readClaims(cmd, c, refs); err != nil {
			return err
		}
		for _, ref := range refs {
			if ref.unclaimed {
				return fmt.Errorf("%s is not claimed yet; claim it first with `wardyn runner claim`", ref.name)
			}
		}
	}
	var done, failed []string
	for _, ref := range refs {
		if add && ref.in(members) {
			if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%s is already in %s\n", ref.name, pool.Name); err != nil {
				return err
			}
			continue
		}
		held, err := heldForApproval(cmd, writePoolMember(ctx, c, pool.ID, ref, add, mine))
		if err != nil && len(refs) == 1 {
			return err
		}
		if err != nil {
			failed = append(failed, poolMemberFailure(ref.name, pool.Name, add, err))
			continue
		}
		if held {
			continue
		}
		label := ref.name
		if add && mine {
			label += " (" + ref.kind() + ")"
		}
		done = append(done, label)
	}
	if err := reportPoolMembers(cmd, pool.Name, done, add); err != nil {
		return err
	}
	for _, line := range failed {
		if _, err := fmt.Fprintln(cmd.OutOrStdout(), line); err != nil {
			return err
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("%d of %d runners failed; the lines above say which", len(failed), len(refs))
	}
	return nil
}

// writePoolMember adds ref to pool or takes it out, through your own routes for a pool of yours or
// one of your runners, and through the organization's otherwise (always, for an organization runner).
func writePoolMember(ctx context.Context, c *sdk.Client, poolID uuid.UUID, ref runnerRef, add, mine bool) error {
	var err error
	switch {
	case ref.org != nil && add:
		_, err = c.AddRunnerPoolRunner(ctx, poolID, *ref.org)
	case ref.org != nil:
		err = c.RemoveRunnerPoolRunner(ctx, poolID, *ref.org)
	case ref.self != nil && add:
		_, err = c.AddMyRunnerToPool(ctx, poolID, *ref.self)
	case ref.self != nil:
		err = c.RemoveMyRunnerFromPool(ctx, poolID, *ref.self)
	case add && mine:
		_, err = c.AddMyPoolRemoteRunner(ctx, poolID, ref.remote)
	case add:
		_, err = c.AddRunnerPoolRemoteRunner(ctx, poolID, ref.remote)
	case mine:
		err = c.RemoveMyPoolRemoteRunner(ctx, poolID, ref.remote)
	default:
		err = c.RemoveRunnerPoolRemoteRunner(ctx, poolID, ref.remote)
	}
	return err
}

// reportPoolMembers says which runners went in or came out of pool; it says nothing when none did.
func reportPoolMembers(cmd *cobra.Command, pool string, done []string, add bool) error {
	var err error
	switch {
	case len(done) == 0:
	case add:
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s added to %s\n", joinAnd(done), pool)
	case len(done) > 1:
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s are no longer in %s\n", joinAnd(done), pool)
	default:
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s is no longer in %s\n", done[0], pool)
	}
	return err
}

// poolMemberFailure is the line for one runner the server refused, with the server's own words.
func poolMemberFailure(runner, pool string, add bool, err error) string {
	if add {
		return fmt.Sprintf("%s was not added to %s: %v", runner, pool, err)
	}
	return fmt.Sprintf("%s was not taken out of %s: %v", runner, pool, err)
}

func runnerPoolCreateCmd(client clientFn) *cobra.Command {
	var name, scope string
	var runners []string
	var asJSON bool
	var lf poolLimitFlags
	cmd := &cobra.Command{Use: "create", Short: "Create a user pool of your own (or, an admin, an organization pool with --scope organization)",
		Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
			limits, err := lf.build(nil)
			if err != nil {
				return err
			}
			c := client()
			var p sdk.RunnerPool
			switch sdk.RunnerPoolScope(scope) {
			case sdk.RunnerPoolOrganization:
				req := sdk.CreateRunnerPoolRequest{Name: name, Limits: limits}
				if err := req.Validate(); err != nil {
					return err
				}
				p, err = c.CreateRunnerPool(cmd.Context(), req)
			case sdk.RunnerPoolUser:
				req := sdk.CreateMyRunnerPoolRequest{Name: name, Limits: limits}
				if err := req.Validate(); err != nil {
					return err
				}
				p, err = c.CreateMyRunnerPool(cmd.Context(), req)
			default:
				return fmt.Errorf("--scope is user or organization")
			}
			if err != nil {
				return err
			}
			if err := printPoolCreated(cmd.OutOrStdout(), p, asJSON, len(runners) > 0); err != nil || len(runners) == 0 {
				return err
			}
			return poolMembers(cmd, c, p, runners, true, true)
		}}
	cmd.Flags().StringVar(&name, "name", "", "the pool's name, 1 to 64 characters")
	cmd.Flags().StringVar(&scope, "scope", string(sdk.RunnerPoolUser), "user (your own) or organization (an admin's; remote-hosted runners and organization runners)")
	cmd.Flags().StringArrayVar(&runners, "runner", nil, "a runner to put in the new pool, by id or name (repeatable)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit raw JSON")
	lf.register(cmd, "own")
	return cmd
}

func runnerPoolUpdateCmd(client clientFn) *cobra.Command {
	var name, onOff string
	var asJSON bool
	var lf poolLimitFlags
	cmd := &cobra.Command{Use: "update <pool>", Short: "Rename, switch or re-limit your own user pool (or, an admin, an organization pool)",
		Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
			var req sdk.UpdateRunnerPoolRequest
			if cmd.Flags().Changed("name") {
				req.Name = &name
			}
			switch onOff {
			case "":
			case "on", "off":
				st := map[string]sdk.RunnerPoolSwitch{"on": sdk.RunnerPoolSwitchActive, "off": sdk.RunnerPoolSwitchDisabled}[onOff]
				req.State = &st
			default:
				return fmt.Errorf("--switch is on or off")
			}
			if req.Name == nil && req.State == nil && !lf.given() && lf.limitsFile == "" {
				return errors.New("name something to change: --name, --switch, --max-runs or a limit flag")
			}
			cur, err := readPoolArg(cmd, client, args[0])
			if err != nil {
				return err
			}
			if req.Limits, err = lf.build(cur.Limits); err != nil {
				return err
			}
			if err := (sdk.UpdateRunnerPoolRequest{Revision: 1, Name: req.Name, State: req.State, Limits: req.Limits}).Validate(); err != nil {
				return err
			}
			req.Revision = cur.Revision
			update := client().UpdateRunnerPool
			if cur.Scope == sdk.RunnerPoolUser {
				update = client().UpdateMyRunnerPool
			}
			p, err := update(cmd.Context(), cur.ID, req)
			if err != nil {
				return err
			}
			return printPoolUpdated(cmd.OutOrStdout(), p, asJSON)
		}}
	cmd.Flags().StringVar(&name, "name", "", "the new name")
	cmd.Flags().StringVar(&onOff, "switch", "", "on or off: whether new runs may choose the pool")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit raw JSON")
	lf.register(cmd, "new")
	return cmd
}

func runnerPoolDeleteCmd(client clientFn) *cobra.Command {
	var yes bool
	cmd := &cobra.Command{Use: "delete <pool>", Short: "Delete your own user pool (or, an admin, an organization pool)",
		Long: "Delete a pool. Its runners leave it, and runs waiting for it end; runs already running are not touched.",
		Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
			if !yes {
				return errors.New("refusing without --yes: this ends every run waiting for the pool, takes every runner out of it and clears it as your default")
			}
			cur, err := readPoolArg(cmd, client, args[0])
			if err != nil {
				return err
			}
			del := client().DeleteRunnerPool
			if cur.Scope == sdk.RunnerPoolUser {
				del = client().DeleteMyRunnerPool
			}
			if err := del(cmd.Context(), cur.ID); err != nil {
				return err
			}
			return printPoolDeleted(cmd.OutOrStdout(), cur)
		}}
	cmd.Flags().BoolVar(&yes, "yes", false, "confirm the delete")
	return cmd
}

// readPoolArg reads the pool an argument names: its id, or its exact name in your pool list.
func readPoolArg(cmd *cobra.Command, client clientFn, arg string) (sdk.RunnerPool, error) {
	id, err := poolIDArg(cmd, client, arg)
	if err != nil {
		return sdk.RunnerPool{}, err
	}
	return client().GetRunnerPool(cmd.Context(), id)
}

// poolIDArg is the id of the pool an argument names: the id as given, or the one pool in your list with
// exactly that name.
func poolIDArg(cmd *cobra.Command, client clientFn, arg string) (uuid.UUID, error) {
	if id, err := uuid.Parse(arg); err == nil {
		return id, nil
	}
	list, err := client().ListRunnerPools(cmd.Context())
	if err != nil {
		return uuid.Nil, err
	}
	var ids []uuid.UUID
	for _, p := range list.Pools {
		if p.Name == arg {
			ids = append(ids, p.ID)
		}
	}
	switch len(ids) {
	case 0:
		return uuid.Nil, fmt.Errorf("no pool in your list is named %q; name it by id or as `wardyn runner pool list` shows it", arg)
	case 1:
		return ids[0], nil
	}
	return uuid.Nil, fmt.Errorf("%d pools in your list are named %q; name it by id", len(ids), arg)
}

// readPoolLimits reads a limits block from path ("-" is stdin); "" is none. Unknown fields are refused
// here, so a misspelled limit fails before it reaches the server.
func readPoolLimits(path string) (*sdk.RunnerPoolLimits, error) {
	if path == "" {
		return nil, nil
	}
	var raw []byte
	var err error
	if path == "-" {
		raw, err = io.ReadAll(os.Stdin)
	} else {
		raw, err = os.ReadFile(path)
	}
	if err != nil {
		return nil, fmt.Errorf("read limits file: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var l sdk.RunnerPoolLimits
	if err := dec.Decode(&l); err != nil {
		return nil, fmt.Errorf("parse limits file: %w", err)
	}
	return &l, l.Validate()
}
