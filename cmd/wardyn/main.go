// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Command wardyn is the operator CLI for the Wardyn control plane. It talks to
// wardynd's public REST API over HTTP using the admin bearer token. Server URL
// and token come from WARDYN_URL / WARDYN_ADMIN_TOKEN (overridable per-flag).
package main

import (
	"errors"
	"fmt"
	"net/http"
	"os"

	"github.com/cjohnstoniv/wardyn/internal/cliutil"
	"github.com/spf13/cobra"
)

// exitError carries a specific process exit code through the cobra error
// return (run --wait maps run outcomes to codes CI can branch on).
type exitError struct {
	code int
	err  error
}

func (e *exitError) Error() string { return e.err.Error() }
func (e *exitError) Unwrap() error { return e.err }

func main() {
	if err := rootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "wardyn:", err)
		var ee *exitError
		if errors.As(err, &ee) {
			os.Exit(ee.code)
		}
		// An auth/authz failure exits 2 so a script can tell "not authorized" apart
		// from a 404/409/other API error or a network failure (all were exit 1) (U087).
		var ae *apiError
		if errors.As(err, &ae) && (ae.StatusCode == http.StatusUnauthorized || ae.StatusCode == http.StatusForbidden) {
			os.Exit(2)
		}
		os.Exit(1)
	}
}

func rootCmd() *cobra.Command {
	var (
		serverURL string
		token     string
	)
	root := &cobra.Command{
		Use:           "wardyn",
		Short:         "Wardyn control-plane CLI",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().StringVar(&serverURL, "url", cliutil.EnvOr("WARDYN_URL", "http://localhost:8080"),
		"control plane base URL (env WARDYN_URL)")
	root.PersistentFlags().StringVar(&token, "token", os.Getenv("WARDYN_ADMIN_TOKEN"),
		"admin bearer token (env WARDYN_ADMIN_TOKEN)")

	// client() resolves the configured client lazily so flags are parsed first.
	client := func() *apiClient { return &apiClient{baseURL: serverURL, token: token} }

	root.AddCommand(
		runCmd(client),
		runsCmd(client),
		approvalsCmd(client),
		approveCmd(client),
		denyCmd(client),
		auditCmd(client),
		killCmd(client),
		policyCmd(client),
		secretCmd(client),
		attachCmd(client),
		recordCmd(client),
		setupCmd(),
	)
	return root
}
