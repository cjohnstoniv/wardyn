// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Command wardyn is the operator CLI for the Wardyn control plane. It talks to
// wardynd's public REST API over HTTP using the admin bearer token. Server URL
// and token come from WARDYN_URL / WARDYN_ADMIN_TOKEN (overridable per-flag).
package main

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/cliutil"
	"github.com/cjohnstoniv/wardyn/internal/version"
	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
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
// keys on the whole *url.Error transport class, not just ECONNREFUSED —
// any failure to reach the daemon deserves the same "is wardynd running?" nudge.
func dialHint(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		// `wardyn setup` is deliberately NOT named here: it's a subcommand group
		// (status/wall/vault/...), so a bare `wardyn setup` prints help and exits
		// 0 rather than starting wardynd — a dead-end recovery step. (Its RunE,
		// added by subcommandGroup, prints exactly that help; the group has no
		// work of its own to do.)
		return "is wardynd running? (start it with `make setup`, the compose quick-start; point the CLI elsewhere with --url or WARDYN_URL)"
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
// every other non-2xx=3), a transport failure (*url.Error) is 5, and anything
// else is 1.
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
		default:
			// pkg/client mints an *sdk.APIError for EVERY non-2xx status, 3xx
			// included (nothing sets CheckRedirect, so a redirect from an
			// interposed proxy is never followed). All of them are failures of
			// the CLI-to-control-plane request itself, so they classify with
			// the 4xx class — never on the catch-all 1, which docs/CI.md
			// reserves for a FAILED run's own task exit code.
			return 3
		}
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		return 5
	}
	return 1
}

// warnPlaintextToken warns when the admin bearer is about to travel in
// cleartext: plain http:// to a NON-loopback wardynd puts the fleet-wide
// credential on the wire on every request (and on the attach WebSocket, which
// inherits the same --url). Advisory only, never a refusal — a TLS-terminating
// reverse proxy or a CI-internal topology is legitimate; WARDYN_ALLOW_PLAINTEXT
// silences it.
//
// ponytail: fourth inline loopback predicate in the tree (internal/api/http.go,
// cmd/wardynd/main.go, internal/egress/proxy/policy.go) — all unexported and
// server-side. Consolidate only if a fifth appears.
func warnPlaintextToken(w io.Writer, rawURL, token string) {
	if token == "" || cliutil.EnvBool("WARDYN_ALLOW_PLAINTEXT", false) {
		return
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "http" {
		return
	}
	host := u.Hostname()
	if host == "localhost" || net.ParseIP(host).IsLoopback() {
		return
	}
	fmt.Fprintf(w, "wardyn: WARNING: sending the admin token in cleartext to %s over http:// — "+
		"use https://, or set WARDYN_ALLOW_PLAINTEXT=1 if TLS terminates in front of it\n", host)
}

func rootCmd() *cobra.Command {
	var (
		serverURL string
		token     string
	)
	root := &cobra.Command{
		Use:           "wardyn",
		Short:         "Wardyn control-plane CLI",
		Version:       version.Version,
		SilenceErrors: true,
		// SilenceUsage is deferred to PersistentPreRun so a structural USAGE
		// error (unknown flag, unknown command, wrong arg count — all raised
		// BEFORE pre-run) still prints usage, while an error returned FROM a
		// RunE (a runtime/API failure) does not.
		// required-flag errors are validated by cobra AFTER pre-run,
		// so they surface as a concise "required flag(s) X not set" without the
		// full usage block — acceptable; the message is already actionable.
		PersistentPreRun: func(cmd *cobra.Command, _ []string) {
			cmd.SilenceUsage = true
			// The env token is resolved HERE, not as the flag's default, so it
			// never becomes cobra's printed `(default "...")`. Pre-run beats
			// every RunE (and so every client() call), and an explicit --token
			// has already been parsed into `token` by now, so it still wins.
			if token == "" {
				token = cliutil.EnvOr("WARDYN_ADMIN_TOKEN", os.Getenv("WARDYN_TOKEN"))
			}
			warnPlaintextToken(cmd.ErrOrStderr(), serverURL, token)
		},
	}
	root.PersistentFlags().StringVar(&serverURL, "url", cliutil.EnvOr("WARDYN_URL", "http://localhost:8080"),
		"control plane base URL (env WARDYN_URL)")
	// WARDYN_ADMIN_TOKEN takes precedence, then WARDYN_TOKEN — resolved in
	// PersistentPreRun above, NOT here. A non-empty string default is echoed by
	// cobra as `(default "<value>")` in `wardyn --help` and in the usage block
	// every structural error prints, which put the fleet-wide bearer in
	// cleartext in every terminal capture, CI log and screenshot of one. The
	// declared default stays empty; the env is read where the token is USED.
	// NOTE: passing --token puts the secret in argv (visible in `ps`); prefer
	// the env var.
	root.PersistentFlags().StringVar(&token, "token", "",
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
		approvalDecisionCmd(client, "approve", "Approve a pending approval request", (*sdk.Client).Approve),
		approvalDecisionCmd(client, "deny", "Deny a pending approval request", (*sdk.Client).Deny),
		auditCmd(client),
		logsCmd(client),
		policyCmd(client),
		workspaceCmd(client),
		sourceCmd(client),
		secretCmd(client),
		attachCmd(client),
		sshCmd(client),
		sshKeyCmd(client),
		recordCmd(client),
		subscriptionCmd(client),
		setupCmd(client),
		siteConfigCmd(client),
		sessionsCmd(client),
		supportBundleCmd(client),
	)
	return root
}
