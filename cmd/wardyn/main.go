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
	"net/url"
	"os"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/cliutil"
	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
	"github.com/spf13/cobra"
)

// version is surfaced via `wardyn --version`; kept in step with CHANGELOG.md.
const version = "0.3.0"

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
		if hint := dialHint(err); hint != "" {
			fmt.Fprintln(os.Stderr, "wardyn:", hint)
		}
		os.Exit(exitCodeFor(err))
	}
}

// dialHint returns a first-contact recovery line when err is a transport
// failure reaching the daemon (a *url.Error — the same class exitCodeFor maps to
// 5: connection refused, timeout, no-such-host), else "". A typed API error means
// we did reach wardynd, so it gets no hint.
// ponytail: keys on the whole *url.Error transport class, not just ECONNREFUSED —
// any failure to reach the daemon deserves the same "is wardynd running?" nudge.
func dialHint(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		return "is wardynd running? (start it with `make setup` or `wardyn setup`; point the CLI elsewhere with --url or WARDYN_URL)"
	}
	// Auth failure: we DID reach wardynd but it refused the bearer token. Name
	// the two ways to supply one, so first contact against the compose stack
	// (which boots with an admin token set) is recoverable without docs.
	var ae *sdk.APIError
	if errors.As(err, &ae) && ae.Status == http.StatusUnauthorized {
		return "authenticate with --token or WARDYN_ADMIN_TOKEN (the compose quick-start prints its token in `wardyn setup`/deploy/compose/README.md)"
	}
	return ""
}

// exitCodeFor maps an error to a process exit code CI can branch on. A run
// outcome from --wait (*exitError) wins — it already encodes the agent/lifecycle
// result. Otherwise a typed API error maps by status class (auth=2, server=4,
// other 4xx=3), a transport failure (*url.Error) is 5, and anything else is 1.
func exitCodeFor(err error) int {
	var ee *exitError
	if errors.As(err, &ee) {
		return ee.code
	}
	var ae *sdk.APIError
	if errors.As(err, &ae) {
		switch {
		case ae.Status == 401 || ae.Status == 403:
			return 2
		case ae.Status >= 500:
			return 4
		case ae.Status >= 400:
			return 3
		}
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		return 5
	}
	return 1
}

func rootCmd() *cobra.Command {
	var (
		serverURL string
		token     string
	)
	root := &cobra.Command{
		Use:           "wardyn",
		Short:         "Wardyn control-plane CLI",
		Version:       version,
		SilenceErrors: true,
		// SilenceUsage is deferred to PersistentPreRun so a structural USAGE
		// error (unknown flag, unknown command, wrong arg count — all raised
		// BEFORE pre-run) still prints usage, while an error returned FROM a
		// RunE (a runtime/API failure) does not.
		// ponytail: required-flag errors are validated by cobra AFTER pre-run,
		// so they surface as a concise "required flag(s) X not set" without the
		// full usage block — acceptable; the message is already actionable.
		PersistentPreRun: func(cmd *cobra.Command, _ []string) { cmd.SilenceUsage = true },
	}
	root.PersistentFlags().StringVar(&serverURL, "url", cliutil.EnvOr("WARDYN_URL", "http://localhost:8080"),
		"control plane base URL (env WARDYN_URL)")
	// WARDYN_ADMIN_TOKEN takes precedence, then WARDYN_TOKEN. NOTE: passing
	// --token puts the secret in argv (visible in `ps`); prefer the env var.
	root.PersistentFlags().StringVar(&token, "token", cliutil.EnvOr("WARDYN_ADMIN_TOKEN", os.Getenv("WARDYN_TOKEN")),
		"admin bearer token (env WARDYN_ADMIN_TOKEN or WARDYN_TOKEN; --token is visible in the process list, prefer the env var)")

	// client() resolves the configured SDK client lazily so flags are parsed
	// first. The 30s per-request timeout bounds each poll of `run --wait` (whose
	// loop can call the API ~900 times over its default 30m deadline) so a hung
	// server can't wedge a single request forever; the shared client reuses one
	// connection pool across those polls.
	client := func() *sdk.Client {
		return &sdk.Client{BaseURL: serverURL, Token: token, HTTPClient: &http.Client{Timeout: 30 * time.Second}}
	}

	root.AddCommand(
		runCmd(client),
		approvalsCmd(client),
		approveCmd(client),
		denyCmd(client),
		auditCmd(client),
		policyCmd(client),
		secretCmd(client),
		attachCmd(client),
		recordCmd(client),
		setupCmd(),
	)
	return root
}
