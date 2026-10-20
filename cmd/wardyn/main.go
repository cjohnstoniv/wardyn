// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Command wardyn is the operator CLI for the Wardyn control plane. It talks to
// wardynd's public REST API over HTTP using the admin bearer token. Server URL
// and token come from WARDYN_URL / WARDYN_ADMIN_TOKEN (overridable per-flag).
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"sync"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/certbundle"
	"github.com/cjohnstoniv/wardyn/internal/cliprofile"
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
		if hint := caHint(err); hint != "" {
			fmt.Fprintln(os.Stderr, "wardyn:", hint)
		} else if hint := dialHint(err); hint != "" {
			fmt.Fprintln(os.Stderr, "wardyn:", hint)
		}
		if hint := noOrgHint(err, urlFromDefault); hint != "" {
			fmt.Fprintln(os.Stderr, "wardyn:", hint)
		}
		if hint := policyHint(err); hint != "" {
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
	// Auth failure: we DID reach wardynd but it refused the credential. The
	// hint follows the 401's reason (auth_hint.go): single sign-on with no
	// admin token, a token the organisation does not know, or today's text,
	// which names the compose stack's admin token for first contact.
	var ae *sdk.APIError
	if errors.As(err, &ae) && ae.Status == http.StatusUnauthorized {
		return unauthorizedHint(ae)
	}
	return ""
}

// defaultURL is where the CLI dials when nothing names an organisation.
const defaultURL = "http://localhost:8080"

// urlFromDefault is whether the URL the running command dials is defaultURL because --url, WARDYN_URL and
// the profile all named none; main reads it to tell a person with no organisation set what to do.
var urlFromDefault bool

// noOrgHint is the line printed after a refused dial against the built-in default: that dial reached no
// organisation because none was set. A URL the person named gets none, as a typed API error does.
func noOrgHint(err error, fromDefault bool) string {
	var ue *url.Error
	if !fromDefault || !errors.As(err, &ue) {
		return ""
	}
	return "no organisation set: run `wardyn login <url>` with your organisation's address"
}

// resolveURL settles the control plane URL: --url, then WARDYN_URL, then the profile `wardyn login`
// wrote, then defaultURL. current is the flag's value, which already carries WARDYN_URL. A profile that
// cannot be read is reported and skipped, so it never blocks a command that names its own URL.
func resolveURL(cmd *cobra.Command, current string) (resolved string, fromDefault bool) {
	if cmd.Flags().Changed("url") || os.Getenv("WARDYN_URL") != "" {
		return current, false
	}
	dir, err := cliprofile.Dir()
	if err != nil {
		return current, true
	}
	profileURL, err := cliprofile.Read(dir)
	if err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "wardyn: ignoring the CLI profile: %v\n", err)
		return current, true
	}
	if profileURL == "" {
		return current, true
	}
	return profileURL, false
}

// noRedirect hands a 3xx back to the caller instead of following it. A followed
// redirect turns a write into a GET of wherever Location points (so it "succeeds"
// against a login page) and replays the body and bearer there.
func noRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// rawHTTPClient is the client for a request the SDK has no method for: the
// SDK client's own (the CLI always sets one), else orgClient for its URL. A
// client the caller did supply is used as it is: its redirect policy is theirs.
func rawHTTPClient(c *sdk.Client) *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return orgClient(c.BaseURL, 0)
}

// caHint names the way out of a TLS failure the anchors can fix: the presented
// root's subject and fingerprint with the `wardyn ca add` line, or the remove
// line for a stored anchor file refused at use. "" for anything else.
func caHint(err error) string {
	var refused *anchorRefusedError
	if errors.As(err, &refused) {
		return fmt.Sprintf("remove the refused anchors with `wardyn ca remove %s`, then add the bundle again with `wardyn ca add`", refused.origin)
	}
	var cve *tls.CertificateVerificationError
	var ua x509.UnknownAuthorityError
	if !errors.As(err, &cve) || !errors.As(err, &ua) || len(cve.UnverifiedCertificates) == 0 {
		return ""
	}
	root := cve.UnverifiedCertificates[len(cve.UnverifiedCertificates)-1]
	return fmt.Sprintf("no trusted root signs the presented chain, which ends at %s (%s); "+
		"if your organisation inspects TLS, add its CA with `wardyn ca add <file>`", root.Subject, certbundle.Fingerprint(root))
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
			// included (the CLI's client returns a redirect instead of
			// following it, so one from an interposed proxy surfaces here).
			// All of them are failures of the CLI-to-control-plane request
			// itself, so they classify with the 4xx class — never on the
			// catch-all 1, which docs/CI.md reserves for a FAILED run's own
			// task exit code.
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
// reverse proxy or a CI-internal topology is legitimate; WARDYN_CLI_ALLOW_PLAINTEXT
// silences it. `run ssh` and `sync` go further and refuse (refusePlainHTTPGateway),
// whatever this variable says.
//
// ponytail: fourth inline loopback predicate in the tree (internal/api/http.go,
// cmd/wardynd/main.go, internal/egress/proxy/policy.go) — all unexported and
// server-side. Consolidate only if a fifth appears.
func warnPlaintextToken(w io.Writer, rawURL, token string) {
	if token == "" || cliutil.EnvBool("WARDYN_CLI_ALLOW_PLAINTEXT", false) {
		return
	}
	if host := plaintextHost(rawURL); host != "" {
		fmt.Fprintf(w, "wardyn: WARNING: sending the admin token in cleartext to %s over http:// — "+
			"use https://, or set WARDYN_CLI_ALLOW_PLAINTEXT=1 if TLS terminates in front of it\n", host)
	}
}

// plaintextHost is the host of a plain http:// URL that is not loopback, else "".
func plaintextHost(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "http" {
		return ""
	}
	host := u.Hostname()
	if host == "localhost" || net.ParseIP(host).IsLoopback() {
		return ""
	}
	return host
}

// tokenSource names the variable the bearer came from when --token was not given.
func tokenSource(adminEnv, token string) string {
	switch {
	case token == "":
		return ""
	case adminEnv != "":
		return "WARDYN_ADMIN_TOKEN"
	}
	return "WARDYN_TOKEN"
}

// tokenFromLogin is authHintFor.tokenFrom when the bearer is this computer's stored CLI sign-in.
const tokenFromLogin = "wardyn login"

// sessionState is the stored CLI sign-in the running command uses, resolved at most once, and only when
// --token, WARDYN_ADMIN_TOKEN and WARDYN_TOKEN all name none.
type sessionState struct {
	once sync.Once
	s    storedSession
	err  error
}

// resolve loads the stored sign-in for serverURL: the item is looked up by that URL alone, so it is only
// ever sent to the organisation it was made for.
func (st *sessionState) resolve(serverURL string) {
	st.once.Do(func() {
		origin, err := cliprofile.Normalize(serverURL)
		if err != nil {
			return
		}
		st.s, st.err = loadSession(context.Background(), origin)
	})
}

func rootCmd() *cobra.Command {
	var (
		serverURL string
		token     string
		sess      = &sessionState{}
	)
	root := &cobra.Command{
		Use:           "wardyn",
		Short:         "Wardyn control-plane CLI",
		Version:       version.String(),
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
			from := "--token"
			if token == "" {
				token = cliutil.EnvOr("WARDYN_ADMIN_TOKEN", os.Getenv("WARDYN_TOKEN"))
				from = tokenSource(os.Getenv("WARDYN_ADMIN_TOKEN"), token)
			}
			serverURL, urlFromDefault = resolveURL(cmd, serverURL)
			authHintFor.url, authHintFor.tokenFrom, authHintFor.session = serverURL, from, nil
			warnPlaintextToken(cmd.ErrOrStderr(), serverURL, token)
		},
	}
	root.PersistentFlags().StringVar(&serverURL, "url", cliutil.EnvOr("WARDYN_URL", defaultURL),
		"control plane base URL (env WARDYN_URL; else the one `wardyn login` saved; else this default)")
	// WARDYN_ADMIN_TOKEN takes precedence, then WARDYN_TOKEN — resolved in
	// PersistentPreRun above, NOT here. A non-empty string default is echoed by
	// cobra as `(default "<value>")` in `wardyn --help` and in the usage block
	// every structural error prints, which put the fleet-wide bearer in
	// cleartext in every terminal capture, CI log and screenshot of one. The
	// declared default stays empty; the env is read where the token is USED.
	// Passing --token puts the secret in argv (visible in `ps`); prefer
	// the env var.
	root.PersistentFlags().StringVar(&token, "token", "",
		"admin bearer token (env WARDYN_ADMIN_TOKEN or WARDYN_TOKEN; --token is visible in the process list, prefer the env var)")

	// client() resolves the configured SDK client lazily so flags are parsed
	// first. The 30s per-request timeout bounds each poll of `run --wait` (whose
	// loop can call the API ~900 times over its default 30m deadline) so a hung
	// server can't wedge a single request forever; the shared client reuses one
	// connection pool across those polls.
	//
	// With no --token, WARDYN_ADMIN_TOKEN or WARDYN_TOKEN, the bearer is the stored CLI sign-in for
	// serverURL (wardyn login), renewed when its access token is spent. A sign-in that cannot be used
	// leaves the request without a bearer and puts the reason in the 401's hint.
	client := func() *sdk.Client {
		t := token
		if t == "" {
			sess.resolve(serverURL)
			if t = sess.s.token; t != "" && authHintFor.tokenFrom != tokenFromLogin {
				authHintFor.tokenFrom = tokenFromLogin
				warnPlaintextToken(root.ErrOrStderr(), serverURL, t)
			}
			authHintFor.session = sess.err
		}
		return &sdk.Client{BaseURL: serverURL, Token: t, HTTPClient: orgClient(serverURL, 30*time.Second)}
	}

	root.AddCommand(
		loginCmd(func() (string, bool) { return serverURL, urlFromDefault }),
		whoamiCmd(client, sess),
		logoutCmd(func() string { return serverURL }),
		runCmd(client),
		attachCmd(client),
		approvalCmd(client),
		auditCmd(client),
		policyCmd(client),
		workspaceCmd(client),
		sourceCmd(client),
		driveCmd(client),
		secretCmd(client),
		sshKeyCmd(client),
		syncCmd(client),
		recordCmd(client),
		setupCmd(client),
		siteConfigCmd(client),
		presetCmd(client),
		templateCmd(client),
		governanceCmd(client),
		sessionCmd(client),
		peopleCmd(client),
		personCmd(client),
		invitationCmd(client),
		serviceIdentityCmd(client),
		tokenCmd(client),
		deviceCmd(client),
		runnerCmd(client),
		supportBundleCmd(client),
		caCmd(client),
	)
	return root
}
