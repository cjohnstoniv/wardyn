// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/coder/websocket"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// makeRawFn / restoreTerminalFn are seams over term.MakeRaw / term.Restore so
// tests can drive the signal/cancel -> terminal-restore path without a real
// tty backing os.Stdin.
var (
	makeRawFn         = term.MakeRaw
	restoreTerminalFn = term.Restore
)

// attachCmd returns the cobra command for `wardyn attach <run-id>`.
//
// It connects to the interactive attach WebSocket endpoint
// (GET /api/v1/runs/{id}/attach) using the admin bearer token, puts the local
// terminal into raw mode, and runs a bidirectional PTY relay:
//
//   - stdin  -> binary WebSocket frames  -> server PTY input
//   - server PTY output -> binary frames -> stdout
//   - SIGWINCH -> TEXT resize frame      -> server PTY resize
//
// The terminal is always restored on exit (deferred, including signal / error
// paths).
func attachCmd(client clientFn) *cobra.Command {
	return &cobra.Command{
		Use:   "attach <run-id>",
		Short: "Attach an interactive terminal to a running sandbox",
		Long: `Attach an interactive PTY to a RUNNING Wardyn sandbox.

Mints a single-use, 30s-TTL attach ticket with your configured token (the
same door the console's own terminal uses: POST /runs/{id}/attach-ticket,
owner-or-admin — a member may mint one for a run THEY created) and dials the
WebSocket attach endpoint with it, so a member never needs the shared admin
token to attach to their own run. When no ticket can be minted (e.g. an older
control plane without the route), the CLI falls back to dialing directly with
the configured token, exactly as before this ticket lane existed.
The local terminal is placed into raw mode for the duration of the session:
Ctrl-C is relayed to the remote PTY as input, not used locally to detach.
Send TERM/HUP/INT from another terminal (or let the remote side close the
session) to detach.

Authentication: your own token (WARDYN_TOKEN) or WARDYN_ADMIN_TOKEN (or --token).
`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// The run id is spliced straight into the WebSocket URL path
			// (buildWSURL) with no further encoding — an unvalidated value
			// could inject an extra path segment or query string into the
			// dial target. The attach endpoint itself only ever accepts a
			// UUID (internal/api routes /runs/{id}/attach through the same
			// uuid-keyed lookup every other run door uses), so anything else
			// could never have connected.
			if _, err := parseID("run", args[0]); err != nil {
				return err
			}
			return runAttach(cmd.Context(), client(), args[0])
		},
	}
}

// runAttach performs the attach flow:
//  1. Mints a single-use attach ticket with the configured token (falls back
//     to a bare dial on the configured token when minting is inconclusive).
//  2. Builds the wss/ws URL from the configured base URL, ticket included.
//  3. Dials the WebSocket.
//  4. Switches stdin to raw mode (deferred restore).
//  5. Sends an initial resize frame, wires SIGWINCH for subsequent resizes.
//  6. Runs the bidirectional pump until disconnect/EOF/signal.
func runAttach(ctx context.Context, c *sdk.Client, runID string) error {
	// TERM/HUP/INT are wired HERE, local to the attach session — never at a
	// root ExecuteContext. Raw mode clears ISIG, so an operator's Ctrl-C
	// doesn't raise SIGINT at all; but a supervisor's SIGTERM, a hung-up
	// controlling terminal (SIGHUP), or a manually sent SIGINT still hit the
	// process under its default disposition, killing it without ever running
	// the deferred term.Restore below and leaving the real terminal in raw
	// mode. A root-level signal context would instead cancel every command's
	// ctx uniformly, relabelling an interrupted `run --wait` (or any other
	// in-flight command) as if the control plane were unreachable — docs/CI.md
	// documents a specific exit-code taxonomy per command that a shared root
	// cancellation would blur. Scoping the wiring to this one command's ctx
	// means the signal drives EXACTLY the cancellation this function already
	// handles cleanly: it cancels pumpCtx below, the pump halves end, and the
	// existing restore-then-detach path runs — the same path a clean
	// disconnect or an explicit ctx cancellation already takes.
	ctx, stopSignals := signal.NotifyContext(ctx, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGINT)
	defer stopSignals()
	// NotifyContext holds the signal disposition redirected until stopSignals
	// runs, which is otherwise deferred all the way to this function's own
	// return — so if the session is wedged somewhere below that does NOT
	// observe ctx (os.Stdin.Read and os.Stdout.Write are plain blocking
	// syscalls, not ctx-aware), the FIRST signal cancels ctx but every
	// subsequent one is caught by the same channel and silently discarded:
	// what used to be killable by a second/third TERM is now unkillable by
	// any TERM at all. Reverting the moment the first signal has been
	// consumed restores that escape hatch — a second signal falls through to
	// the normal, process-killing disposition exactly as it did before this
	// wiring existed.
	go func() { <-ctx.Done(); stopSignals() }()

	// Mint-then-dial (item 1 of the ticket lane): a member's own token can
	// already mint a ticket for a run THEY own (POST /runs/{id}/attach-ticket
	// is owner-or-admin, internal/api/attach_ticket.go) even though the WS
	// route itself falls through to requireOperator for a bare bearer — so
	// minting first is what lets a member attach at all without the shared
	// admin token. The ticket is single-use with a 30s TTL (consumed by a
	// DELETE-and-return on first redemption), so it is minted here, immediately
	// before the one dial below, and NEVER cached or reused across calls.
	//
	// A definitive server answer (any HTTP status, e.g. the 404 a foreign run
	// now gets instead of the WS route's old blanket 403) is surfaced AS-IS —
	// the server has already made the authoritative call for this run and this
	// token, and silently retrying a different lane would only paper over it.
	// Only a TRANSPORT-level failure (no HTTP response at all: an unreachable
	// control plane, or an older one with no attach-ticket route to answer)
	// falls back to dialing directly with the configured token, exactly as
	// this command behaved before the ticket lane existed — so an admin-token
	// CI caller pointed at such a deployment is unaffected.
	//
	// Skipped entirely once ctx is already Done: a caller cancellation that
	// lands before the mint even starts can otherwise race the mint's own HTTP
	// round trip (does it fail fast on the dead ctx, or does it slip through
	// to a real response first?) — R-02 below needs that race not to matter,
	// and "don't bother" is also just correct: nothing this mint could return
	// is going anywhere once ctx is dead.
	var ticket string
	if ctx.Err() == nil {
		var mintErr error
		ticket, mintErr = mintAttachTicket(ctx, c, runID)
		var apiErr *sdk.APIError
		if mintErr != nil && errors.As(mintErr, &apiErr) {
			return apiErr
		}
	}

	wsURL := buildWSURL(c.BaseURL, runID, ticket)

	// Dial the WebSocket with the bearer token in the HTTP Upgrade header, when
	// one is configured (mirrors pkg/client.Client.do: local host-mode deployments
	// run without a token, so an empty token is not a client-side error here either
	// — let the server's 401 be the signal if auth is actually required). Sent
	// alongside a minted ticket too: the ticket lane (?ticket=) is checked
	// first server-side and wins when present, so the header is inert but
	// harmless in that case, and is exactly what authenticates the dial when
	// mint fell back above.
	// InsecureSkipVerify is intentionally NOT set on the client — the CLI is a
	// CLI-origin connection (not a browser), but we still want TLS validation
	// for wss:// URLs; the server's same-origin check does not apply to non-browser
	// clients (no Origin header in the dial) so the accept will succeed as long as
	// the token is valid.
	var hdr http.Header
	if c.Token != "" {
		hdr = http.Header{"Authorization": []string{"Bearer " + c.Token}}
	}
	conn, resp, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: hdr,
	})
	if err != nil {
		// A signal caught by the NotifyContext above (or any other caller
		// cancellation) arriving WHILE the handshake is in flight cancels
		// this same ctx, and coder/websocket surfaces that as a *url.Error
		// wrapping context.Canceled — which exitCodeFor would otherwise map
		// to 5 ("network unreachable") with dialHint's "is wardynd running?"
		// hint, exactly the mislabelling the signal wiring above exists to
		// avoid. A caller-initiated cancellation is a clean detach here too,
		// the same as one that arrives after the connection is up.
		if ctx.Err() != nil {
			return nil
		}
		// A rejected handshake (e.g. 401/403/404, never a network failure) leaves
		// resp non-nil with the server's real status + {"error":...} body — surface
		// it as a *sdk.APIError so exitCodeFor/dialHint classify it exactly like
		// every other API call, instead of collapsing every rejection into the raw
		// "failed to WebSocket dial: expected status 101 but got NNN" text and a
		// bare exit 1.
		if resp != nil {
			body, _ := io.ReadAll(resp.Body)
			return &sdk.APIError{Status: resp.StatusCode, Body: string(body)}
		}
		return fmt.Errorf("dial %s: %w", wsURL, err)
	}
	// CloseNow is the fail-closed backstop; a normal exit path does a clean close.
	defer conn.CloseNow()

	// Put the terminal into raw mode. We operate on stdin's fd.
	fd := int(os.Stdin.Fd())
	oldState, err := makeRawFn(fd)
	if err != nil {
		// If stdin is not a terminal (piped), continue without raw mode so the
		// command still works for scripted use (no SIGWINCH either).
		oldState = nil
	}
	if oldState != nil {
		defer restoreTerminalFn(fd, oldState) //nolint:errcheck // best-effort restore
	}

	// Send an initial resize frame so the remote PTY matches our window from
	// the start. Failure here is not fatal — the session will use the server's
	// default size and the user can resize later via SIGWINCH.
	if oldState != nil {
		cols, rows, szErr := term.GetSize(fd)
		if szErr == nil {
			_ = sendResize(ctx, conn, uint16(cols), uint16(rows))
		}
	}

	// Wire SIGWINCH so window resizes are relayed to the remote PTY.
	winchCh := make(chan os.Signal, 1)
	if oldState != nil {
		signal.Notify(winchCh, syscall.SIGWINCH)
		defer signal.Stop(winchCh)
	}

	// Derive a child context so we can cancel both pump goroutines when either
	// side ends.
	pumpCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// errCh collects the first termination reason from either pump half.
	errCh := make(chan error, 3)

	// Half 1: server -> stdout (binary PTY output frames).
	go func() {
		for {
			typ, data, rerr := conn.Read(pumpCtx)
			if rerr != nil {
				errCh <- rerr
				cancel()
				return
			}
			if typ != websocket.MessageBinary {
				// Text frames from the server are unexpected but harmless; skip.
				continue
			}
			if _, werr := os.Stdout.Write(data); werr != nil {
				errCh <- werr
				cancel()
				return
			}
		}
	}()

	// Half 2: stdin -> server (binary PTY input frames).
	go func() {
		buf := make([]byte, 4096)
		for {
			n, rerr := os.Stdin.Read(buf)
			if n > 0 {
				if werr := conn.Write(pumpCtx, websocket.MessageBinary, buf[:n]); werr != nil {
					errCh <- werr
					cancel()
					return
				}
			}
			if rerr != nil {
				if rerr == io.EOF {
					errCh <- nil // clean EOF (piped stdin finished)
				} else {
					errCh <- rerr
				}
				cancel()
				return
			}
		}
	}()

	// Half 3: SIGWINCH -> resize control frames.
	go func() {
		for {
			select {
			case <-pumpCtx.Done():
				return
			case <-winchCh:
				cols, rows, szErr := term.GetSize(fd)
				if szErr == nil {
					// A resize send error is not fatal; the session continues.
					_ = sendResize(pumpCtx, conn, uint16(cols), uint16(rows))
				}
			}
		}
	}()

	// Block until a pump half ends.
	firstErr := <-errCh
	cancel()

	// Attempt a clean WebSocket close.
	_ = conn.Close(websocket.StatusNormalClosure, "")

	// Restore terminal before printing anything so the message appears correctly.
	if oldState != nil {
		_ = restoreTerminalFn(fd, oldState)
	}

	// Classify the exit: a context-cancelled / WebSocket normal-close is a clean
	// detach, not an error the caller should surface as a non-zero exit.
	if firstErr != nil && !isNormalClose(firstErr) {
		return firstErr
	}

	fmt.Fprintln(os.Stderr, "detached")
	return nil
}

// sendResize writes a TEXT resize control frame to the server.
func sendResize(ctx context.Context, conn *websocket.Conn, cols, rows uint16) error {
	data, err := json.Marshal(map[string]any{
		"type": "resize",
		"cols": cols,
		"rows": rows,
	})
	if err != nil {
		return err
	}
	return conn.Write(ctx, websocket.MessageText, data)
}

// buildWSURL converts the base HTTP URL to a WebSocket URL for the attach
// endpoint, with the minted ticket (if any) as ?ticket=:
//
//	https://host/  ->  wss://host/api/v1/runs/<id>/attach
//	http://host/   ->  ws://host/api/v1/runs/<id>/attach
//	(ticket != "") ->  ...&/api/v1/runs/<id>/attach?ticket=<t>
//
// net/url.Values.Encode does the query-escaping — a raw ticket is 64 hex
// chars (attach_ticket.go's mintAttachTicket) and would never need it in
// practice, but hand-splicing it into the URL is the same class of mistake
// runID's own doc comment on attachCmd already calls out for the path.
func buildWSURL(baseURL, runID, ticket string) string {
	base := strings.TrimRight(baseURL, "/")
	// Replace the scheme: https -> wss, http -> ws.
	switch {
	case strings.HasPrefix(base, "https://"):
		base = "wss://" + strings.TrimPrefix(base, "https://")
	case strings.HasPrefix(base, "http://"):
		base = "ws://" + strings.TrimPrefix(base, "http://")
		// If already ws/wss or some other scheme, leave it alone.
	}
	path := base + "/api/v1/runs/" + runID + "/attach"
	if ticket == "" {
		return path
	}
	u, err := url.Parse(path)
	if err != nil {
		// base is caller-configured (WARDYN_URL/--url), already dialed
		// elsewhere in this process before reaching here — not expected to
		// fail to parse. Fall back to the un-ticketed path rather than a
		// panic or a silently-wrong dial target.
		return path
	}
	q := u.Query()
	q.Set("ticket", ticket)
	u.RawQuery = q.Encode()
	return u.String()
}

// mintAttachTicket POSTs /api/v1/runs/{id}/attach-ticket with the configured
// token and returns the single-use ticket string. Deliberately a raw request,
// not an sdk.Client method: pkg/client's doc comment and its three pinning
// tests (TestClientCoversRouteFamilies / TestRouteFamiliesCoverEveryMethod /
// TestSDKCensusNamesEveryRouteFamily) enumerate the whole attach family —
// attach, attach-ticket, attach-holder, attach/takeover — as DELIBERATELY
// unwrapped, so adding a method here would fight that pin rather than use it.
//
// Returns ("", nil) — NOT an error — in two cases the caller treats alike, by
// falling back to the pre-ticket-lane bare dial: the request never produced
// an HTTP response at all (dial failure, timeout), OR it produced one that
// cannot be the control plane's own answer — a non-2xx whose body is not the
// server's standard JSON error envelope (writeError, internal/api/http.go,
// always emits one; a router's plain-text "404 page not found" for a route
// an OLDER control plane never registered does not), or a 2xx with no
// readable ticket. Anything else — any status whose body IS that JSON
// envelope — is the server's own decisive answer and is returned as such: a
// non-2xx becomes *sdk.APIError (errors.As-able, same as every other SDK
// call) for the caller to surface verbatim rather than mask with a fallback.
func mintAttachTicket(ctx context.Context, c *sdk.Client, runID string) (string, error) {
	target := strings.TrimRight(c.BaseURL, "/") + "/api/v1/runs/" + runID + "/attach-ticket"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, nil)
	if err != nil {
		return "", nil
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	req.Header.Set("Accept", "application/json")

	hc := c.HTTPClient
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return "", nil // transport-level: inconclusive, caller falls back
	}
	defer resp.Body.Close()

	// Same 2 KiB error-body cap pkg/client.maxErrBody uses, kept local rather
	// than exported for one constant a single call site needs.
	const maxAttachTicketErrBody = 2048
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxAttachTicketErrBody))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		if !json.Valid(body) {
			return "", nil // not the server's own envelope: inconclusive, fall back
		}
		return "", &sdk.APIError{Status: resp.StatusCode, Body: string(body)}
	}

	var out struct {
		Ticket string `json:"ticket"`
	}
	if err := json.Unmarshal(body, &out); err != nil || out.Ticket == "" {
		// A 2xx that doesn't look like the mint response this CLI knows how
		// to read (e.g. a proxy or an incompatible server) is exactly as
		// inconclusive as never getting a response: fall back rather than
		// dial a URL with a garbage ?ticket=.
		return "", nil
	}
	return out.Ticket, nil
}

// isNormalClose reports whether err represents a clean WebSocket or context
// close that should be treated as a successful detach rather than an error.
func isNormalClose(err error) bool {
	if err == nil {
		return true
	}
	cs := websocket.CloseStatus(err)
	if cs == websocket.StatusNormalClosure || cs == websocket.StatusGoingAway {
		return true
	}
	// A context cancellation (from the other pump half ending cleanly) is also
	// a normal detach scenario. The pump halves share pumpCtx, and coder/websocket's
	// conn.Read/conn.Write return ctx.Err() verbatim on cancellation (see
	// read.go/write.go), so context.Canceled is a real sentinel here, not just
	// a string — errors.Is is exact, no substring guessing needed.
	return errors.Is(err, context.Canceled)
}
