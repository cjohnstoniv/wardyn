// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
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
		Short: "Get or replace the operator-wide corporate baseline (proxy / egress redirects / SCM hosts)",
		Long: "Read or replace the operator-wide corporate baseline. The document carries secret\n" +
			"NAMES, never secret values, so it is safe to save alongside the repo:\n\n" +
			"    wardyn site-config get > corp-baseline.json      # before a reset\n" +
			"    wardyn site-config apply corp-baseline.json      # after `make setup`\n\n" +
			"`apply` REPLACES the whole document (the server contract), so edit what `get`\n" +
			"produced rather than sending a fragment. A document saved before egress_redirects\n" +
			"existed (still keyed by artifact_overrides) is folded automatically on apply.",
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
			// Strict decode, same shape as decodeSpecStrict in policy.go and as
			// the server's own decodeStrict. `apply` REPLACES the whole document,
			// so a key a non-strict decode drops is not a no-op: the setting the
			// operator meant to write is absent from what we re-marshal, and the
			// stored one is deleted. The server can never catch the typo — it
			// only ever sees the fields that survived this decode.
			var cfg types.SiteConfig
			dec := json.NewDecoder(bytes.NewReader(raw))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&cfg); err != nil {
				return fmt.Errorf("parse site config JSON: %w", err)
			}
			// PutSiteConfig strips Integrations before the request (the server
			// 400s on a non-empty one and carries the STORED rows forward
			// instead), so a captured document's integrations are neither sent
			// nor restored. Say so — silence here reads as a restore that
			// happened.
			if n := len(cfg.Integrations); n > 0 {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: %d integration(s) in this file were not applied — integrations are managed through their own endpoints (`/api/v1/integrations`, or Settings), never this document; the stored ones are left as they are\n", n)
			}
			out, dangling, err := client().PutSiteConfig(cmd.Context(), cfg)
			if err != nil {
				return err
			}
			for _, name := range dangling {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: secret %q is referenced but not set — restore it with `wardyn secret set %s`\n", name, name)
			}
			return emitJSON(out)
		},
	}
}
