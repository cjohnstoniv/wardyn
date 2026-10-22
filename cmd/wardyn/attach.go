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
	"time"

	"github.com/coder/websocket"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// makeRawFn / restoreTerminalFn / getSizeFn are seams over term.MakeRaw /
// term.Restore / term.GetSize so tests can drive the signal/cancel ->
// terminal-restore path, and a promotion's resend-the-window-size path,
// without a real tty backing os.Stdin.
var (
	makeRawFn         = term.MakeRaw
	restoreTerminalFn = term.Restore
	getSizeFn         = term.GetSize
)

// attachStdin is the input Half 2 relays; nil means os.Stdin. A test seam, so a
// piped session's EOF can be driven without closing the process's real stdin.
var attachStdin io.Reader

// attachDrainIdle is how long a piped session (stdin at EOF) keeps showing
// output after the last frame before it detaches. A var so tests can shorten it.
var attachDrainIdle = 2 * time.Second

// attachCmd returns the cobra command for `wardyn attach <run-id>`.
//
// It mints an attach ticket, then attaches on this computer when the run's
// runner is here (attach_local.go) and through the organisation's attach
// WebSocket (GET /api/v1/runs/{id}/attach) otherwise, puts the local terminal
// into raw mode, and runs a bidirectional PTY relay:
//
//   - stdin  -> PTY input
//   - PTY output -> stdout
//   - SIGWINCH -> resize
//
// The terminal is always restored on exit (deferred, including signal / error
// paths).
func attachCmd(client clientFn) *cobra.Command {
	var opts attachOptions
	cmd := &cobra.Command{
		Use:   "attach <run-id>",
		Short: "Attach an interactive terminal to a running sandbox",
		Long: `Attach an interactive PTY to a RUNNING Wardyn sandbox.

Mints a single-use, 30s-TTL attach ticket with your configured token (the
same door the console's own terminal uses: POST /runs/{id}/attach/ticket,
owner-or-admin — a member may mint one for a run THEY created) and attaches
with it, so a member never needs the shared admin token to attach to their own
run. When no ticket can be minted (e.g. an older control plane without the
route), the CLI falls back to dialing directly with the configured token.

When the run is on your own runner on this computer, the terminal stays on this
computer: the CLI talks to the runner's local socket, and the organisation
still admits, records and can end the session. Otherwise the terminal goes
through the organisation. One line on stderr says which path was taken.
--via local fails instead of going through the organisation; --via relay
always goes through it. On Windows the runner lives in WSL2, so auto goes
through the organisation and --via local is refused.

--take-over takes the terminal from whoever holds it (audited as
session.takeover) before attaching.

The local terminal is placed into raw mode for the duration of the session:
Ctrl-C goes to the sandbox, not used locally to detach. Close the window (or
send TERM/HUP/INT from another terminal, or let the remote side close the
session) to detach.
With piped input (not a terminal), attach sends what it reads, then keeps
showing output until the session closes or stays quiet for 2 seconds, and exits 0.

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
			return runAttach(cmd.Context(), client(), args[0], opts)
		},
	}
	cmd.Flags().StringVar(&opts.via, "via", viaAuto,
		"auto: on this computer when the run's runner is here, else through the organisation; local: fail rather than go through the organisation; relay: always through the organisation")
	cmd.Flags().BoolVar(&opts.takeOver, "take-over", false,
		"take the terminal from whoever holds it (audited) before attaching")
	return cmd
}

// dialError reports a failed attach dial against safeURL (the URL without the
// ticket). A minted ticket stays live until redeemed: keep it out of the error,
// the transport's *url.Error included (exitCodeFor still classes it as one).
func dialError(safeURL, ticket string, err error) error {
	if ue := (*url.Error)(nil); errors.As(err, &ue) {
		return &url.Error{Op: "dial", URL: safeURL, Err: ue.Err}
	}
	msg := err.Error()
	if ticket != "" {
		msg = strings.ReplaceAll(msg, ticket, "<redacted>")
	}
	return fmt.Errorf("dial %s: %s", safeURL, msg)
}

// runAttach performs the attach flow:
//  1. Mints a single-use attach ticket with the configured token (falls back
//     to a bare dial on the configured token when minting is inconclusive).
//  2. With --take-over, takes the terminal from its holder.
//  3. Opens the session: on this computer when it can (openLocal), else, or
//     when the organisation answers that it cannot, through the organisation.
//  4. Prints the transport line and runs the pump (pumpAttach).
func runAttach(ctx context.Context, c *sdk.Client, runID string, opts attachOptions) error {
	// --via is settled before any call: on Windows, --via local is refused here.
	tryLocal, mustLocal, err := planVia(opts.via, localAttachOS)
	if err != nil {
		return err
	}
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
	// already mint a ticket for a run THEY own (POST /runs/{id}/attach/ticket
	// is owner-or-admin, internal/api/attach_ticket.go) even though the WS
	// route itself falls through to requireOperator for a bare bearer — so
	// minting first is what lets a member attach at all without the shared
	// admin token. The ticket is single-use with a 30s TTL (consumed by a
	// DELETE-and-return on first redemption), so it is minted here, immediately
	// before the dial, and NEVER cached or reused across calls.
	//
	// A definitive server answer (any HTTP status, e.g. the 404 a foreign run
	// gets) is surfaced AS-IS — the server has already made the authoritative
	// call for this run and this token, and silently retrying a different
	// lane would only paper over it. Only a TRANSPORT-level failure (no HTTP
	// response at all: an unreachable control plane, or an older one with no
	// attach-ticket route to answer) falls back to dialing directly with the
	// configured token, so an admin-token CI caller pointed at such a
	// deployment is unaffected.
	//
	// Skipped entirely once ctx is already Done: nothing this mint could
	// return is going anywhere once ctx is dead (R-02 below).
	ticket, err := mintIfLive(ctx, c, runID)
	if err != nil {
		return err
	}
	if opts.takeOver && ctx.Err() == nil {
		if err := takeOverAttach(ctx, c, runID); err != nil {
			return err
		}
	}

	pipe, line, err := openAttach(ctx, c, runID, ticket, tryLocal, mustLocal)
	if err != nil || pipe == nil {
		return err // nil, nil: the caller cancelled during the dial, a clean detach
	}
	fmt.Fprintln(os.Stderr, line)
	return pumpAttach(ctx, pipe)
}

// mintIfLive mints a ticket unless ctx is already done. A server's answer is
// returned as an error; an inconclusive mint is "" (the bare dial).
func mintIfLive(ctx context.Context, c *sdk.Client, runID string) (string, error) {
	if ctx.Err() != nil {
		return "", nil
	}
	ticket, err := mintAttachTicket(ctx, c, runID)
	var apiErr *sdk.APIError
	if err != nil && errors.As(err, &apiErr) {
		return "", apiErr
	}
	return ticket, nil
}

// errLocalNotHere prefixes why --via local could not attach on this computer.
const errLocalNotHere = "local attach is not available for this run: "

// openAttach opens the session (design ATTACH §2.2, §3.5.3). It tries the
// local transport when asked and falls back to the organisation only on a
// transport's answer: no usable pointer file or socket, the socket refusing,
// 409 attach_local_unavailable, a first frame without transport "local" (an
// older organisation, whose socket then simply runs as a relay attach), or
// close 4409 (once, with a new ticket). Every other answer, each refusal of
// DESIGN-ATTACH 4.4 included, is returned as the server gave it and never
// retried. mustLocal (--via local) turns every fallback into an error.
// A nil pipe with a nil error is a cancellation during the dial.
func openAttach(ctx context.Context, c *sdk.Client, runID, ticket string, tryLocal, mustLocal bool) (*attachPipe, string, error) {
	if tryLocal && ctx.Err() == nil {
		lc, why := (*localConn)(nil), "the organisation issued no attach ticket"
		if ticket != "" {
			lc, why = openLocal(ctx, c, runID)
		}
		if lc == nil && mustLocal {
			return nil, "", errors.New(errLocalNotHere + why)
		}
		if lc != nil {
			p, line, err, relay := tryLocalAttach(ctx, c, runID, ticket, lc, mustLocal)
			if !relay {
				return p, line, err
			}
			// A transport's answer: through the organisation, with a new
			// ticket (the first was spent on the local try).
			if ticket, err = mintIfLive(ctx, c, runID); err != nil {
				return nil, "", err
			}
		}
	}
	ws, err := dialAttach(ctx, c, runID, ticket, "")
	if ws == nil {
		return nil, "", err
	}
	p := relayPipe(ws, nil)
	return &p, relayAttachLine(c.BaseURL), nil
}

// tryLocalAttach dials the WebSocket for the local transport with lc's nonce
// and reads its first frame. relay is true when the answer was a transport's
// (409 attach_local_unavailable, close 4409) and the caller should go through
// the organisation; lc is closed then.
func tryLocalAttach(ctx context.Context, c *sdk.Client, runID, ticket string, lc *localConn, mustLocal bool) (p *attachPipe, line string, err error, relay bool) {
	ws, err := dialAttach(ctx, c, runID, ticket, lc.nonce)
	if ws == nil {
		_ = lc.conn.Close()
		return nil, "", err, err != nil && localUnavailable(err) && !mustLocal
	}
	typ, data, rerr := ws.Read(ctx)
	switch {
	case rerr == nil && isLocalModeFrame(typ, data):
		lp := localPipe(ws, lc, data)
		return &lp, localAttachLine(lc.runner), nil, false
	case rerr == nil:
		// An organisation older than the local transport ignored the query:
		// this socket is an ordinary relay attach, and it works.
		_ = lc.conn.Close()
		if mustLocal {
			_ = ws.Close(websocket.StatusNormalClosure, "")
			return nil, "", errors.New(errLocalNotHere + "the organisation does not offer it"), false
		}
		rp := relayPipe(ws, &wsFrame{typ: typ, data: data})
		return &rp, relayAttachLine(c.BaseURL), nil, false
	}
	_ = lc.conn.Close()
	_ = ws.CloseNow()
	if ctx.Err() != nil {
		return nil, "", nil, false
	}
	if localGrantFailed(rerr) && !mustLocal {
		return nil, "", nil, true
	}
	return nil, "", rerr, false
}

// takeOverAttach is --take-over: POST /runs/{id}/attach/takeover, the route
// the console's Take over button uses. Nobody attached (409
// attach_takeover_no_holder) leaves nothing to take; every other refusal is
// the server's, shown as it is.
func takeOverAttach(ctx context.Context, c *sdk.Client, runID string) error {
	target := strings.TrimRight(c.BaseURL, "/") + "/api/v1/runs/" + runID + "/attach/takeover"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, nil)
	if err != nil {
		return err
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := rawHTTPClient(c).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		apiErr := sdk.NewAPIError(resp.StatusCode, body)
		if apiErr.Status == http.StatusConflict && apiErr.Reason == "attach_takeover_no_holder" {
			return nil
		}
		return apiErr
	}
	var out struct {
		PreviousHolder string `json:"previous_holder"`
	}
	if json.Unmarshal(body, &out) == nil && out.PreviousHolder != "" {
		fmt.Fprintln(os.Stderr, "took the terminal from "+out.PreviousHolder)
	}
	return nil
}

// dialAttach dials the attach WebSocket, for the local transport when nonce is
// set. A nil conn with a nil error is the caller's cancellation during the
// handshake, a clean detach.
func dialAttach(ctx context.Context, c *sdk.Client, runID, ticket, nonce string) (*websocket.Conn, error) {
	wsURL := buildWSURL(c.BaseURL, runID, ticket)
	if nonce != "" {
		wsURL = withLocalTransport(wsURL, nonce)
	}

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
	// orgClient with no Timeout: the handshake must not follow a redirect (it
	// would replay the bearer at Location), the session itself outlives any
	// whole-request deadline, and the organisation's anchors apply.
	conn, resp, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: hdr,
		HTTPClient: orgClient(c.BaseURL, 0),
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
			return nil, nil
		}
		// A rejected handshake (e.g. 401/403/404, never a network failure) leaves
		// resp non-nil with the server's real status + {"error":...} body — surface
		// it as a *sdk.APIError so exitCodeFor/dialHint classify it exactly like
		// every other API call, instead of collapsing every rejection into the raw
		// "failed to WebSocket dial: expected status 101 but got NNN" text and a
		// bare exit 1.
		if resp != nil {
			body, _ := io.ReadAll(resp.Body)
			return nil, sdk.NewAPIError(resp.StatusCode, body)
		}
		return nil, dialError(buildWSURL(c.BaseURL, runID, ""), ticket, err)
	}
	return conn, nil
}

// pumpAttach runs an open session until it ends:
//  1. Switches stdin to raw mode (deferred restore).
//  2. Sends an initial resize, wires SIGWINCH for subsequent resizes.
//  3. Runs the bidirectional pump until disconnect/EOF/signal.
func pumpAttach(ctx context.Context, pipe *attachPipe) error {
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

	// Send an initial resize so the remote PTY matches our window from the
	// start. Failure here is not fatal — the session will use the server's
	// default size and the user can resize later via SIGWINCH.
	if oldState != nil {
		cols, rows, szErr := term.GetSize(fd)
		if szErr == nil {
			_ = pipe.resize(ctx, uint16(cols), uint16(rows))
		}
	}

	// Relay window resizes to the remote PTY (SIGWINCH, or a size poll on Windows).
	winchCh := make(chan struct{}, 1)
	if oldState != nil {
		stopWatch := watchResize(ctx, fd, winchCh)
		defer stopWatch()
	}

	// Derive a child context so we can cancel both pump goroutines when either
	// side ends.
	pumpCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// errCh collects the first termination reason from either pump half.
	errCh := make(chan error, 3)

	// stdinEOF closes when piped stdin ends, and activity pings on every frame
	// Half 1 shows: together they let the session keep draining output after the
	// input is gone, then detach once it goes quiet.
	stdinEOF := make(chan struct{})
	activity := make(chan struct{}, 1)
	in := attachInput()

	// attachMode tracks the read/write state this session has been told about
	// across attach-mode frames (see handleAttachModeFrame) — touched only by
	// the output half's mode callback, so it needs no lock.
	var attachMode attachModeState

	// outDone is Half 1's own result too, so an end Half 2 saw first can give
	// way to the close reason Half 1 is about to read (settleAttachEnd).
	outDone := make(chan error, 1)

	// Half 1: session output -> stdout. stdout stays byte-identical PTY
	// output, never control JSON.
	go func() {
		err := pipe.output(pumpCtx, func(data []byte) error {
			if _, werr := os.Stdout.Write(data); werr != nil {
				return werr
			}
			signalAttachActivity(activity)
			return nil
		}, func(data []byte) {
			handleAttachModeFrame(pumpCtx, pipe.resize, fd, oldState, data, &attachMode)
		})
		outDone <- err
		errCh <- err
		cancel()
	}()

	// Half 2: stdin -> session input. It does not cancel the session on an
	// error: the session's own close may be what failed the write, and
	// cancelling would abort Half 1's read of its reason.
	go func() {
		buf := make([]byte, 4096)
		for {
			n, rerr := in.Read(buf)
			if n > 0 {
				if werr := pipe.input(pumpCtx, buf[:n]); werr != nil {
					errCh <- werr
					return
				}
			}
			if rerr != nil {
				if rerr == io.EOF {
					close(stdinEOF) // piped stdin finished: stop sending, keep draining
					return
				}
				errCh <- rerr
				return
			}
		}
	}()

	// Half 3: resize notifications -> resize.
	go func() {
		for {
			select {
			case <-pumpCtx.Done():
				return
			case <-winchCh:
				cols, rows, szErr := term.GetSize(fd)
				if szErr == nil {
					// A resize send error is not fatal; the session continues.
					_ = pipe.resize(pumpCtx, uint16(cols), uint16(rows))
				}
			}
		}
	}()

	// Block until a pump half ends (or, after stdin EOF, the output goes quiet).
	firstErr := settleAttachEnd(waitAttachEnd(errCh, stdinEOF, activity), outDone, attachCloseGrace)
	cancel()

	// Attempt a clean close.
	pipe.close()

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

// attachModeState tracks, across the lifetime of one attach session, what the
// server's attach-mode text frames (internal/api/attach_holder.go's
// attachModeMsg) have told this socket about its own read/write mode: the
// frame sent on every connect, and — for an observer — a possible second one
// on promotion. Read and written only from Half 1's goroutine, so it needs no
// lock of its own.
type attachModeState struct {
	seenFirst bool
	readOnly  bool
}

// clientAttachModeHolder is the CLI's own minimal decode of the "holder" half
// of the server's attach-mode frame — only the two fields the notice below
// prints.
type clientAttachModeHolder struct {
	Principal string `json:"principal"`
	Source    string `json:"source"`
}

// clientAttachModeFrame is the CLI's own minimal decode of the server's
// attach-mode text frame. Deliberately not internal/api's own attachModeMsg
// type — the CLI talks to the server over the wire only (see
// mintAttachTicket's doc comment on why attach's client-side calls stay
// unwrapped), so it decodes just the fields it prints and ignores the rest.
type clientAttachModeFrame struct {
	Type     string                  `json:"type"`
	ReadOnly bool                    `json:"read_only"`
	Holder   *clientAttachModeHolder `json:"holder"`
}

// handleAttachModeFrame is Half 1's handler for a TEXT frame from the server.
// stdout stays byte-identical PTY output — everything here goes to STDERR.
//
// A frame that fails to decode, or whose "type" is not "attach-mode", is
// silently ignored: an unrecognised control frame must never crash the CLI
// or print noise, since the protocol may grow more of them later.
//
// On the FIRST attach-mode frame (sent the instant the socket opens,
// read_only=false included — see attachModeMsg's own doc comment), a
// read-only dial prints one line naming the holder and where they attached
// from. A writer dial prints nothing; being the writer is the normal case.
//
// A LATER frame only ever arrives on a promotion — attach_holder.go sends a
// second attach-mode frame solely when this socket's observer was promoted
// to writer — so any true->false transition after the first frame prints the
// promotion line and re-sends this client's own window size (the promoted
// socket never resized the shared PTY while it was only watching, so the
// server still has the departed writer's geometry).
func handleAttachModeFrame(ctx context.Context, resize func(context.Context, uint16, uint16) error, fd int, oldState *term.State, data []byte, state *attachModeState) {
	var msg clientAttachModeFrame
	if err := json.Unmarshal(data, &msg); err != nil || msg.Type != "attach-mode" {
		return
	}

	if !state.seenFirst {
		state.seenFirst = true
		state.readOnly = msg.ReadOnly
		if msg.ReadOnly {
			fmt.Fprintln(os.Stderr, attachHolderNotice(msg.Holder))
		}
		return
	}

	promoted := state.readOnly && !msg.ReadOnly
	state.readOnly = msg.ReadOnly
	if !promoted {
		return
	}

	fmt.Fprintln(os.Stderr, "attach: promoted — you can now type")
	if oldState == nil {
		return // piped stdin: no real tty to size from.
	}
	if cols, rows, szErr := getSizeFn(fd); szErr == nil {
		_ = resize(ctx, uint16(cols), uint16(rows))
	}
}

// attachHolderNotice renders the "held by X (attached from Y)" fragment of
// the read-only notice. holder is nil only in the impossible-in-practice case
// attachModeMsg's own doc comment calls out (a holder that vanished between
// registration and the write) — printed plainly rather than panicking on a
// nil dereference.
func attachHolderNotice(holder *clientAttachModeHolder) string {
	if holder == nil || holder.Principal == "" {
		return "read-only: another session holds the terminal"
	}
	switch holder.Source {
	case "ssh":
		return "read-only: held by " + holder.Principal + " (attached over SSH)"
	case "web":
		return "read-only: held by " + holder.Principal + " (attached from a browser)"
	default:
		return "read-only: held by " + holder.Principal
	}
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

// mintAttachTicket POSTs /api/v1/runs/{id}/attach/ticket (renamed from
// attach-ticket in 0.8 — docs/sdk.md "Renamed in 0.8"; the old path still
// answers, as a chi alias, for one minor) with the configured token and
// returns the single-use ticket string. Deliberately a raw request, not an
// sdk.Client method: pkg/client's doc comment and its three pinning tests
// (TestClientCoversRouteFamilies / TestRouteFamiliesCoverEveryMethod /
// TestSDKCensusNamesEveryRouteFamily) enumerate the whole attach family —
// attach, attach/ticket, attach/holder, attach/takeover — as DELIBERATELY
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
	target := strings.TrimRight(c.BaseURL, "/") + "/api/v1/runs/" + runID + "/attach/ticket"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, nil)
	if err != nil {
		return "", nil
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := rawHTTPClient(c).Do(req)
	if err != nil {
		return "", nil // transport-level: inconclusive, caller falls back
	}
	defer resp.Body.Close()

	// Same 2 KiB error-body cap pkg/client.maxErrBody uses, kept local rather
	// than exported for one constant a single call site needs.
	const maxAttachTicketErrBody = 2048
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxAttachTicketErrBody))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		// A 3xx is decisive whatever its body: it is an interposed hop (or a
		// mistyped --url) answering, and a bare-dial fallback would send the
		// bearer to that same hop.
		if !json.Valid(body) && (resp.StatusCode < 300 || resp.StatusCode > 399) {
			return "", nil // not the server's own envelope: inconclusive, fall back
		}
		return "", sdk.NewAPIError(resp.StatusCode, body)
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

// attachCloseGrace bounds how long an end without a close reason waits for the
// output half's: longer than localEndGrace, which the local output half may
// spend waiting for the organisation's close.
var attachCloseGrace = 3 * time.Second

// settleAttachEnd is the session's end as shown. When the first end carries
// no WebSocket close (typically a keystroke's write failing because the
// server just closed the session), it waits up to grace for the output half,
// which reads the close, and prefers that close: the reason the server gave
// (session ended, taken over, lease lost, 4409) is what the person sees, as
// DESIGN-ATTACH 4.4 rows 18-19 require. An output half that ended cleanly (the
// local pipe, which has no close frame) means the shell exited first, so that is the end shown.
func settleAttachEnd(first error, out <-chan error, grace time.Duration) error {
	if first == nil || websocket.CloseStatus(first) != -1 {
		return first
	}
	t := time.NewTimer(grace)
	defer t.Stop()
	select {
	case err := <-out:
		if err == nil || websocket.CloseStatus(err) != -1 {
			return err
		}
	case <-t.C:
	}
	return first
}

// waitAttachEnd blocks until a pump half reports its end and returns that error.
// After stdin EOF the remote command's output is still arriving, so it also
// returns (nil) once attachDrainIdle passes with no frame shown, rather than
// detaching before any of that output is.
func waitAttachEnd(errCh <-chan error, stdinEOF <-chan struct{}, activity <-chan struct{}) error {
	var idle *time.Timer
	var idleC <-chan time.Time
	defer func() {
		if idle != nil {
			idle.Stop()
		}
	}()
	for {
		select {
		case err := <-errCh:
			return err
		case <-stdinEOF:
			stdinEOF = nil
			idle = time.NewTimer(attachDrainIdle)
			idleC = idle.C
		case <-activity:
			if idle != nil {
				idle.Reset(attachDrainIdle)
			}
		case <-idleC:
			return nil
		}
	}
}

// attachInput is what Half 2 relays: attachStdin when a test set it, else os.Stdin.
func attachInput() io.Reader {
	if attachStdin != nil {
		return attachStdin
	}
	return os.Stdin
}

// signalAttachActivity records, without blocking, that a frame was just shown.
func signalAttachActivity(activity chan<- struct{}) {
	select {
	case activity <- struct{}{}:
	default:
	}
}
