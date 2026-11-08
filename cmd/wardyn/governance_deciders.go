// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// governanceDecidersCmd sets who besides the security tier may decide each kind of hold for runs
// launched in a unit.
//
//	wardyn governance deciders set Platform --file deciders.json
//
// The file is the PUT body: {"rules": [...]}. An empty list clears the unit's rules. A write the
// server holds for a second approver (always, for a unit admin's) prints the pending change and
// exits 0, as `governance set` does.
func governanceDecidersCmd(client clientFn) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "deciders",
		Short: "Say who besides the security tier may decide each kind of hold in a unit",
	}
	var file string
	set := &cobra.Command{
		Use:   "set <unit>",
		Short: "Replace a unit's deciders rules from a JSON file (the unit by name or id)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			rules, err := readDeciderRules(file)
			if err != nil {
				return err
			}
			c := client()
			doc, err := c.GetGovernance(cmd.Context())
			if err != nil {
				return err
			}
			unit, err := governanceUnitByArg(doc, args[0])
			if err != nil {
				return err
			}
			saved, pending, err := c.PutUnitDeciders(cmd.Context(), unit.ID, rules)
			if err != nil {
				return err
			}
			if pending != nil {
				printGovernancePending(cmd.ErrOrStderr(), sdk.GovernanceApplyResult{Pending: []sdk.GovernanceChange{*pending}})
				return nil
			}
			return emitJSON(cmd.OutOrStdout(), saved)
		},
	}
	set.Flags().StringVar(&file, "file", "", `JSON file {"rules": [...]}; an empty list clears the unit's rules`)
	_ = set.MarkFlagRequired("file")
	cmd.AddCommand(set)
	return subcommandGroup(cmd)
}

// readDeciderRules reads the rules list of the file at path, refusing unknown fields and a file with
// no "rules" key, so a typo cannot clear a unit's rules.
func readDeciderRules(path string) ([]sdk.DeciderRule, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Rules []sdk.DeciderRule `json:"rules"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if doc.Rules == nil {
		return nil, errors.New(path + `: no "rules" list (use {"rules": []} to clear)`)
	}
	return doc.Rules, nil
}
