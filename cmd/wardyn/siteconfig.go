// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/spf13/cobra"
)

// siteconfig.go — read/replace the operator-wide corporate baseline (upstream
// proxy ref, artifact-registry overrides, SCM hosts) from the host.
//
// Why this exists as a CLI: site-config lives in Postgres, so `make reset-all`
// (which deletes the volume) takes it with them. Without a host-side way to
// re-apply it, an operator who resets comes back up with the UI configured but
// corporate egress silently broken, and the only fix is to re-click through the
// wizard. `get` before a reset and `apply` after makes it a two-command
// round-trip that scripts/up.sh can automate.
//
// SECRET VALUES NEVER LIVE HERE — the document holds secret NAMES (refs) only,
// so a captured site-config is safe to keep beside the repo. Restore the secrets
// themselves with `wardyn secret set`, which reads the value on stdin.

func siteConfigCmd(client clientFn) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "site-config",
		Short: "Get or replace the operator-wide corporate baseline (proxy / artifact mirrors / SCM hosts)",
		Long: "Read or replace the operator-wide corporate baseline. The document carries secret\n" +
			"NAMES, never secret values, so it is safe to save alongside the repo:\n\n" +
			"    wardyn site-config get > corp-baseline.json      # before a reset\n" +
			"    wardyn site-config apply corp-baseline.json      # after `make setup`\n\n" +
			"`apply` REPLACES the whole document (the server contract), so edit what `get`\n" +
			"produced rather than sending a fragment.",
	}
	cmd.AddCommand(siteConfigGetCmd(client), siteConfigApplyCmd(client))
	return cmd
}

func siteConfigGetCmd(client clientFn) *cobra.Command {
	return &cobra.Command{
		Use:   "get",
		Short: "Print the current site config as JSON",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := client().GetSiteConfig(cmd.Context())
			if err != nil {
				return err
			}
			return emitJSON(cfg)
		},
	}
}

func siteConfigApplyCmd(client clientFn) *cobra.Command {
	return &cobra.Command{
		Use:   "apply [file]",
		Short: "Replace the site config from a JSON file (or stdin with '-')",
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
				return fmt.Errorf("read site config: %w", err)
			}
			var cfg types.SiteConfig
			if err := json.Unmarshal(raw, &cfg); err != nil {
				return fmt.Errorf("parse site config JSON: %w", err)
			}
			out, err := client().PutSiteConfig(cmd.Context(), cfg)
			if err != nil {
				return err
			}
			return emitJSON(out)
		},
	}
}
