// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// subscriptionProvider is the only managed-subscription provider today
// (claude-code / Anthropic). Adding a provider is a server-side row
// (internal/api/harnesscred.go), mirrored here.
const subscriptionProvider = "anthropic"

// setupCheckLite / setupStatusLite mirror the subset of GET /api/v1/setup/status
// this CLI renders. The full struct (internal/api.SetupStatus) is not exported
// through internal/types, so the SDK returns raw JSON and we decode the frozen
// snake_case wire contract (SetupCheck{id,label,status,detail,fix}) here.
type setupCheckLite struct {
	ID     string `json:"id"`
	Label  string `json:"label"`
	Status string `json:"status"`
	Detail string `json:"detail"`
	Fix    string `json:"fix"`
}
type setupStatusLite struct {
	Ready  bool             `json:"ready"`
	Checks []setupCheckLite `json:"checks"`
}

func fetchSetupStatus(ctx context.Context, c *sdk.Client) (setupStatusLite, error) {
	raw, err := c.SetupStatus(ctx)
	if err != nil {
		return setupStatusLite{}, err
	}
	var st setupStatusLite
	if err := json.Unmarshal(raw, &st); err != nil {
		return setupStatusLite{}, fmt.Errorf("parse setup status: %w", err)
	}
	return st, nil
}

// subscriptionConnected reports whether a managed subscription token is already
// captured — the server emits the harness_credential check ONLY when one exists
// (internal/api/setup.go harnessCredentialCheck), so its presence is the signal.
func subscriptionConnected(st setupStatusLite) bool {
	for _, c := range st.Checks {
		if c.ID == "harness_credential" {
			return true
		}
	}
	return false
}

// subscriptionCmd manages the Wardyn-managed Claude subscription: a `claude
// setup-token` captured once and injected proxy-side into every eligible run —
// the sandbox only ever holds an inert sentinel. Reserved secret name
// (wardyn-harness-anthropic-oauth) is blocked from the generic secrets API, so
// this dedicated command is the supported path.
func subscriptionCmd(client clientFn) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "subscription",
		Short: "Connect a Wardyn-managed Claude subscription (never-resident; injected proxy-side into runs)",
	}

	var reconnect bool
	connect := &cobra.Command{
		Use:   "connect",
		Short: "Store a Claude subscription setup-token (read from stdin); injected proxy-side, never resident",
		Long: "Connect a managed Claude subscription. Get a token with `claude setup-token` (opens your\n" +
			"browser), then pipe or paste it here — it is stored age-encrypted, never passed via argv,\n" +
			"and injected proxy-side so the sandbox only holds an inert sentinel.\n\n" +
			"  claude setup-token | wardyn subscription connect        # interactive\n" +
			"  printf '%s' \"$TOKEN\" | wardyn subscription connect      # headless/CI",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			c := client()
			// Idempotent: a repeated `make setup` / connect is a no-op unless
			// --reconnect is passed (a reconnect PUT also resets the aging clock).
			if !reconnect {
				if st, err := fetchSetupStatus(ctx, c); err == nil && subscriptionConnected(st) {
					fmt.Println("a managed Claude subscription is already connected — pass --reconnect to replace it")
					return nil
				}
			}
			if isTerminal(os.Stdin) {
				fmt.Fprintln(os.Stderr, "Run `claude setup-token` (opens your browser), paste the sk-ant-oat token here, then Ctrl-D:")
			}
			tok, err := readSecretValue(os.Stdin)
			if err != nil {
				return fmt.Errorf("read token from stdin: %w", err)
			}
			if tok = strings.TrimSpace(tok); tok == "" {
				return fmt.Errorf("empty token (expected a `claude setup-token` value starting with sk-ant-oat)")
			}
			if err := c.ConnectManagedSubscription(ctx, subscriptionProvider, tok); err != nil {
				return err
			}
			fmt.Println("managed Claude subscription connected — stored age-encrypted, injected proxy-side, never resident in the sandbox")
			if st, err := fetchSetupStatus(ctx, c); err == nil {
				printSubscriptionStatus(st)
			}
			return nil
		},
	}
	// --token-stdin is the default and only input path; the flag exists purely so
	// CI scripts can be explicit. No --token <value> flag: argv leaks in `ps`.
	connect.Flags().Bool("token-stdin", true, "read the setup-token from stdin (the only accepted input; argv would leak in ps)")
	connect.Flags().BoolVar(&reconnect, "reconnect", false, "replace an already-connected subscription (resets the token-age clock)")

	status := &cobra.Command{
		Use:   "status",
		Short: "Show managed Claude subscription + model-access status",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			st, err := fetchSetupStatus(cmd.Context(), client())
			if err != nil {
				return err
			}
			printSubscriptionStatus(st)
			return nil
		},
	}

	disconnect := &cobra.Command{
		Use:   "disconnect",
		Short: "Remove the managed Claude subscription token",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			err := client().DisconnectManagedSubscription(cmd.Context(), subscriptionProvider)
			if err != nil {
				// Idempotent: a not-found is success (nothing to remove).
				var apiErr *sdk.APIError
				if errors.As(err, &apiErr) && (apiErr.Status == 404 || strings.Contains(strings.ToLower(apiErr.Body), "not found")) {
					fmt.Println("no managed Claude subscription was connected")
					return nil
				}
				return err
			}
			fmt.Println("managed Claude subscription disconnected")
			return nil
		},
	}

	cmd.AddCommand(connect, status, disconnect)
	return cmd
}

// printSubscriptionStatus surfaces the model-access rows most relevant to
// subscription setup: the managed credential (when present) and the aggregate
// llm_provider readiness.
func printSubscriptionStatus(st setupStatusLite) {
	shown := false
	for _, c := range st.Checks {
		if c.ID == "harness_credential" || c.ID == "llm_provider" {
			fmt.Printf("  [%s] %s: %s\n", c.Status, c.Label, c.Detail)
			if c.Fix != "" {
				fmt.Printf("        → %s\n", c.Fix)
			}
			shown = true
		}
	}
	if !shown {
		fmt.Println("  (no model-access checks reported)")
	}
}
