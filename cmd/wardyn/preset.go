// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// preset.go — read/upsert the admin-managed launch presets, the same
// get-then-apply shape as `wardyn drive`:
//
//	wardyn preset get > presets.json    # a snapshot
//	wardyn preset apply presets.json    # restore, or hand-authored presets
//
// `apply` PUTs every preset the file names, by name. Nothing the file omits is
// touched or deleted, and an unchanged preset keeps its version, so `get`
// followed by `apply` is a no-op.
func presetCmd(client clientFn) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "preset",
		Short: "Get or upsert the launch presets",
		Long: "Read or upsert the launch presets (named bundles of run fields a launcher\n" +
			"starts with `preset` instead of a full spec):\n\n" +
			"    wardyn preset get > presets.json\n" +
			"    wardyn preset apply presets.json\n\n" +
			"`apply` upserts every preset the file names, by name; a changed preset moves\n" +
			"to its next version, an unchanged one keeps its version. Nothing the file\n" +
			"omits is touched and nothing is deleted, so `wardyn preset get > f && wardyn\n" +
			"preset apply f` is a no-op.",
	}
	cmd.AddCommand(presetGetCmd(client), presetApplyCmd(client))
	return subcommandGroup(cmd)
}

func presetGetCmd(client clientFn) *cobra.Command {
	return &cobra.Command{
		Use:   "get",
		Short: "Print every launch preset as JSON",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			doc, err := client().ListPresets(cmd.Context())
			if err != nil {
				return err
			}
			return emitJSON(cmd.OutOrStdout(), doc)
		},
	}
}

func presetApplyCmd(client clientFn) *cobra.Command {
	return &cobra.Command{
		Use:   "apply [file]",
		Short: "Upsert the launch presets in a JSON file (or stdin with '-')",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var raw []byte
			var err error
			if len(args) == 0 || args[0] == "-" {
				raw, err = io.ReadAll(cmd.InOrStdin())
			} else {
				raw, err = os.ReadFile(args[0])
			}
			if err != nil {
				return fmt.Errorf("read presets document: %w", err)
			}
			var doc sdk.PresetsDocument
			dec := json.NewDecoder(bytes.NewReader(raw))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&doc); err != nil {
				return fmt.Errorf("parse presets document JSON: %w", err)
			}
			out, err := client().ApplyPresets(cmd.Context(), doc)
			if err != nil {
				return err
			}
			return emitJSON(cmd.OutOrStdout(), out)
		},
	}
}
