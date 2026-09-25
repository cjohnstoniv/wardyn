// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/spf13/cobra"
)

// siteConfigFieldsAfter066 names the SiteConfig keys added after 0.6.6 (mirrors
// internal/api/site_config.go's siteConfigFieldsAfter066 — kept as a separate
// literal because this package cannot import an internal/api unexported var;
// nothing enforces the two lists staying in sync beyond this comment). A key
// here that this file's JSON does not MENTION is exactly the shape
// carryForwardUnnamedSiteConfigFields preserves server-side; add a key here
// when you add one to internal/api's list.
var siteConfigFieldsAfter066 = []string{"upstream_proxy_no_proxy", "internal_hosts", "workspace_providers", "agent_providers"}

// omittedPostV066Fields reports which of siteConfigFieldsAfter066 raw (the
// file's own bytes, not the decoded struct) never MENTIONS — an absent key,
// not a present-but-empty one, since only absence triggers the server's
// carry-forward.
//
// A JSON `null` COUNTS AS OMITTED here, and that is not a liberty: these fields
// are pointers with `omitempty`, so `apply` strict-decodes `null` into a nil
// pointer and re-marshals the document WITHOUT the key — the wire the server
// sees is byte-identical to an absent key, and it carries the old block forward.
// Reporting `null` as present printed nothing and let an operator believe they
// had cleared a block they had in fact preserved. (`{}` is the clear form on
// both doors — docs/OPERATIONS.md's provider-doors grid, DESKTOP.md's MDM row.)
func omittedPostV066Fields(raw []byte) ([]string, error) {
	var present map[string]json.RawMessage
	if err := json.Unmarshal(raw, &present); err != nil {
		return nil, err
	}
	var omitted []string
	for _, f := range siteConfigFieldsAfter066 {
		v, ok := present[f]
		if !ok || string(bytes.TrimSpace(v)) == "null" {
			omitted = append(omitted, f)
		}
	}
	return omitted, nil
}

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
// This document never holds secret values — only secret NAMES (refs), so a
// captured site-config is safe to keep beside the repo. Restore the secrets
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
	return subcommandGroup(cmd)
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
			return emitJSON(cmd.OutOrStdout(), cfg)
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
			// F285: a field this file's version predates (upstream_proxy_no_proxy,
			// internal_hosts, workspace_providers, agent_providers all landed after
			// 0.6.6) is left as the server already has it, not cleared — the same
			// carry-forward that protects integrations above, generalized to every
			// key an older `get` could not have written. Computed against the
			// FILE's own bytes (raw), not the decoded struct — a present-but-empty
			// value ({} or []) MENTIONS the key and must stay silent, only an
			// absent one is this carry-forward's business.
			omitted, err := omittedPostV066Fields(raw)
			if err != nil {
				return fmt.Errorf("parse site config JSON: %w", err)
			}
			out, dangling, onboardingIgnored, err := client().PutSiteConfig(cmd.Context(), cfg)
			if err != nil {
				return err
			}
			// Printed only after a successful apply — an operator debugging a
			// rejected file doesn't need a note about fields that were never
			// reached.
			if len(omitted) > 0 {
				fmt.Fprintf(cmd.ErrOrStderr(), "note: this file omits %s — left as the server already has it, not cleared; pass it explicitly to change it\n", strings.Join(omitted, ", "))
			}
			for _, name := range dangling {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: secret %q is referenced but not set — restore it with `wardyn secret set %s`\n", name, name)
			}
			// Same rule as the integrations warning above: onboarding_completed_at
			// is server-owned, so a captured document's copy is dropped on the
			// write (the server carries its OWN mark forward). Say so — silence
			// here reads as a restore that happened, and this file is exactly the
			// one an operator applies after a reset, when the mark it carries is
			// the pre-reset install's.
			if onboardingIgnored {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: onboarding_completed_at in this file was not applied — the setup flow owns that mark on this install; the stored one is left as it is\n")
			}
			return emitJSON(cmd.OutOrStdout(), out)
		},
	}
}
