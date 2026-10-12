// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// runner_pool_manage.go — create, show, change and delete a pool. A Self-Hosted pool is your own:
// `create` makes one, and `update` and `delete` reach it through your own routes. A Remote Provided
// pool is the organisation's: `create --remote`, and `update` or `delete` on one, need an admin.
//
//	wardyn runner pool create --name Laptop [--limits-file limits.json]
//	wardyn runner pool show <pool-id>
//	wardyn runner pool update <pool-id> --switch off
//	wardyn runner pool delete <pool-id> --yes
//	wardyn runner pool add <pool-id> <runner-id>      # one of your claimed runners, into one of your pools
//	wardyn runner pool remove <pool-id> <runner-id>
//
// --limits-file is the wire `limits` block as JSON (what `show --json` prints), "-" for stdin. An
// update that names it replaces the pool's limits whole.

func runnerPoolManageCmds(client clientFn) []*cobra.Command {
	return []*cobra.Command{runnerPoolCreateCmd(client), runnerPoolShowCmd(client), runnerPoolUpdateCmd(client), runnerPoolDeleteCmd(client),
		runnerPoolMemberCmd(client, true), runnerPoolMemberCmd(client, false)}
}

// runnerPoolMemberCmd is `add` (add true) or `remove`: one of your own runners into or out of one of your
// own Self-Hosted pools. Whether the runner may join is the server's to say; its sentence is printed.
func runnerPoolMemberCmd(client clientFn, add bool) *cobra.Command {
	use, short := "add <pool-id> <runner-id>", "Put one of your claimed runners in one of your Self-Hosted pools"
	if !add {
		use, short = "remove <pool-id> <runner-id>", "Take one of your runners out of one of your Self-Hosted pools"
	}
	return &cobra.Command{Use: use, Short: short, Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
		pool, err := uuid.Parse(args[0])
		if err != nil {
			return fmt.Errorf("parse pool id %q: %w", args[0], err)
		}
		runnerID, err := uuid.Parse(args[1])
		if err != nil {
			return fmt.Errorf("parse runner id %q: %w", args[1], err)
		}
		if !add {
			if err := client().RemoveMyRunnerFromPool(cmd.Context(), pool, runnerID); err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "runner %s is not in pool %s\n", runnerID, pool)
			return err
		}
		if _, err := client().AddMyRunnerToPool(cmd.Context(), pool, runnerID); err != nil {
			return err
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "runner %s is in pool %s\n", runnerID, pool)
		return err
	}}
}

func runnerPoolCreateCmd(client clientFn) *cobra.Command {
	var name, limitsFile string
	var remote, asJSON bool
	cmd := &cobra.Command{Use: "create", Short: "Create a Self-Hosted pool of your own (or, an admin, a Remote Provided pool with --remote)",
		Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
			limits, err := readPoolLimits(limitsFile)
			if err != nil {
				return err
			}
			var p sdk.RunnerPool
			if remote {
				req := sdk.CreateRunnerPoolRequest{Name: name, HostingType: sdk.RunnerPoolRemoteProvided, Limits: limits}
				if err := req.Validate(); err != nil {
					return err
				}
				p, err = client().CreateRunnerPool(cmd.Context(), req)
			} else {
				req := sdk.CreateMyRunnerPoolRequest{Name: name, Limits: limits}
				if err := req.Validate(); err != nil {
					return err
				}
				p, err = client().CreateMyRunnerPool(cmd.Context(), req)
			}
			if err != nil {
				return err
			}
			return printRunnerPool(cmd.OutOrStdout(), p, asJSON)
		}}
	cmd.Flags().StringVar(&name, "name", "", "the pool's name, 1 to 64 characters")
	cmd.Flags().BoolVar(&remote, "remote", false, "create a Remote Provided pool for the organisation (admin)")
	cmd.Flags().StringVar(&limitsFile, "limits-file", "", "the pool's limits as JSON (\"-\" for stdin)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit raw JSON")
	return cmd
}

func runnerPoolShowCmd(client clientFn) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{Use: "show <pool-id>", Short: "Show one pool you may use or administer", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := readPoolArg(cmd, client, args[0])
			if err != nil {
				return err
			}
			return printRunnerPool(cmd.OutOrStdout(), p, asJSON)
		}}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit raw JSON")
	return cmd
}

func runnerPoolUpdateCmd(client clientFn) *cobra.Command {
	var name, onOff, limitsFile string
	var asJSON bool
	cmd := &cobra.Command{Use: "update <pool-id>", Short: "Rename, switch or re-limit your own Self-Hosted pool (or, an admin, a Remote Provided pool)",
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
			var err error
			if req.Limits, err = readPoolLimits(limitsFile); err != nil {
				return err
			}
			if err := (sdk.UpdateRunnerPoolRequest{Revision: 1, Name: req.Name, State: req.State, Limits: req.Limits}).Validate(); err != nil {
				return err
			}
			cur, err := readPoolArg(cmd, client, args[0])
			if err != nil {
				return err
			}
			req.Revision = cur.Revision
			update := client().UpdateRunnerPool
			if cur.HostingType == sdk.RunnerPoolSelfHosted {
				update = client().UpdateMyRunnerPool
			}
			p, err := update(cmd.Context(), cur.ID, req)
			if err != nil {
				return err
			}
			return printRunnerPool(cmd.OutOrStdout(), p, asJSON)
		}}
	cmd.Flags().StringVar(&name, "name", "", "the new name")
	cmd.Flags().StringVar(&onOff, "switch", "", "on or off: whether new runs may choose the pool")
	cmd.Flags().StringVar(&limitsFile, "limits-file", "", "the pool's new limits as JSON, replacing them whole (\"-\" for stdin)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit raw JSON")
	return cmd
}

func runnerPoolDeleteCmd(client clientFn) *cobra.Command {
	var yes bool
	cmd := &cobra.Command{Use: "delete <pool-id>", Short: "Delete your own Self-Hosted pool (or, an admin, a Remote Provided pool)",
		Long: "Delete a pool. Its runners leave it, and runs waiting for it end; runs already running are not touched.",
		Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
			if !yes {
				return fmt.Errorf("deleting a pool ends the runs waiting for it: pass --yes to confirm")
			}
			cur, err := readPoolArg(cmd, client, args[0])
			if err != nil {
				return err
			}
			if cur.HostingType == sdk.RunnerPoolSelfHosted {
				return client().DeleteMyRunnerPool(cmd.Context(), cur.ID)
			}
			return client().DeleteRunnerPool(cmd.Context(), cur.ID)
		}}
	cmd.Flags().BoolVar(&yes, "yes", false, "confirm the delete")
	return cmd
}

// readPoolArg parses a pool id argument client-side and reads the pool.
func readPoolArg(cmd *cobra.Command, client clientFn, arg string) (sdk.RunnerPool, error) {
	id, err := uuid.Parse(arg)
	if err != nil {
		return sdk.RunnerPool{}, fmt.Errorf("parse pool id %q: %w", arg, err)
	}
	return client().GetRunnerPool(cmd.Context(), id)
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

func printRunnerPool(w io.Writer, p sdk.RunnerPool, asJSON bool) error {
	if asJSON {
		return emitJSON(w, p)
	}
	tw := newTab(w)
	owner := p.Owner
	if owner == "" {
		owner = "the organisation"
	}
	limits := "none of its own"
	if p.Limits != nil {
		limits = "set (show --json prints them)"
	}
	for _, row := range [][2]string{{"ID", p.ID.String()}, {"NAME", p.Name}, {"HOSTING", string(p.HostingType)}, {"OWNER", owner},
		{"STATE", string(p.State)}, {"REVISION", fmt.Sprint(p.Revision)}, {"LIMITS", limits}} {
		fmt.Fprintf(tw, "%s\t%s\n", row[0], row[1])
	}
	return tw.Flush()
}
