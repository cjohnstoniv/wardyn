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

// drive.go — read/replace the admin-registered drives and their allocations,
// the same get-then-apply shape site-config.go established for the operator-
// wide baseline: `subcommandGroup`, `emitJSON`, strict decoding, and `-`
// meaning stdin.
//
// Why this exists as a CLI: pkg/client/client.go's Coverage census named
// /api/v1/drives as SDK-uncovered since 0.7 — an admin who wanted a drive
// scripted or backed up had no way to do it besides the console or raw HTTP,
// even though the family already has exactly the shape `get`/`apply` needs.
//
//	wardyn drive get > drives.json      # a snapshot, or before a reset
//	wardyn drive apply drives.json      # restore, or hand-authored additions
//
// `apply` UPSERTS every drive and grant the file names, over the existing
// POST /drives, PUT /drives/{id} and POST /drives/grants routes — there is no
// bulk-write route, and this command adds none. A drive with an id `get`
// already issued is REPLACED in place; one with none is CREATED. Nothing the
// file omits is touched, and nothing is deleted — `get` immediately followed
// by `apply` is therefore a no-op, the same round trip site-config's pair
// promises for the operator baseline.
func driveCmd(client clientFn) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "drive",
		Short: "Get or replace the admin-registered drives and their allocations",
		Long: "Read or upsert the admin-registered drives (storage an admin allocates to a\n" +
			"user, a group, or everyone) and their allocations:\n\n" +
			"    wardyn drive get > drives.json\n" +
			"    wardyn drive apply drives.json\n\n" +
			"`apply` upserts every drive and grant the file names — a drive whose id `get`\n" +
			"already issued is REPLACED in place, one with none is CREATED, and a grant is\n" +
			"always upserted by its (subject_type, subject) natural key. Nothing the file\n" +
			"omits is touched and nothing is deleted, so `wardyn drive get > f && wardyn\n" +
			"drive apply f` is a no-op.",
	}
	cmd.AddCommand(driveGetCmd(client), driveApplyCmd(client))
	return subcommandGroup(cmd)
}

func driveGetCmd(client clientFn) *cobra.Command {
	return &cobra.Command{
		Use:   "get",
		Short: "Print every registered drive and allocation as JSON",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			doc, err := client().GetDrives(cmd.Context())
			if err != nil {
				return err
			}
			return emitJSON(cmd.OutOrStdout(), doc)
		},
	}
}

func driveApplyCmd(client clientFn) *cobra.Command {
	return &cobra.Command{
		Use:   "apply [file]",
		Short: "Upsert the drives and allocations in a JSON file (or stdin with '-')",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			src := "-"
			if len(args) == 1 {
				src = args[0]
			}
			var raw []byte
			var err error
			if src == "-" {
				raw, err = io.ReadAll(cmd.InOrStdin())
			} else {
				raw, err = os.ReadFile(src)
			}
			if err != nil {
				return fmt.Errorf("read drives document: %w", err)
			}
			// Strict decode, the same shape as site-config's own apply and the
			// server's decodeStrict: a key this file mistypes must surface as a
			// parse error, not silently vanish from what ApplyDrives then sends.
			var doc sdk.DrivesDocument
			dec := json.NewDecoder(bytes.NewReader(raw))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&doc); err != nil {
				return fmt.Errorf("parse drives document JSON: %w", err)
			}
			out, err := client().ApplyDrives(cmd.Context(), doc)
			if err != nil {
				return err
			}
			return emitJSON(cmd.OutOrStdout(), out)
		},
	}
}
