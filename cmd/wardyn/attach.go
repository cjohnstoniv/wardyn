// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/coder/websocket"
	"github.com/spf13/cobra"
	"golang.org/x/term"
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

Connects to the WebSocket attach endpoint using the admin bearer token.
The local terminal is placed into raw mode for the duration of the session;
press Ctrl-C or close the session to detach.

Authentication: WARDYN_ADMIN_TOKEN (or --token).
`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAttach(cmd.Context(), client(), args[0])
		},
	}
}

// runAttach performs the attach flow:
//  1. Builds the wss/ws URL from the configured base URL.
//  2. Dials the WebSocket with the admin bearer token.
//  3. Switches stdin to raw mode (deferred restore).
//  4. Sends an initial resize frame, wires SIGWINCH for subsequent resizes.
//  5. Runs the bidirectional pump until disconnect/EOF/Ctrl-C.
func runAttach(ctx context.Context, c *apiClient, runID string) error {
	if c.token == "" {
		return fmt.Errorf("no admin token set (use --token or WARDYN_ADMIN_TOKEN)")
	}

	wsURL := buildWSURL(c.baseURL, runID)

	// Dial the WebSocket with the bearer token in the HTTP Upgrade header.
	// InsecureSkipVerify is intentionally NOT set on the client — the CLI is a
	// CLI-origin connection (not a browser), but we still want TLS validation
	// for wss:// URLs; the server's same-origin check does not apply to non-browser
	// clients (no Origin header in the dial) so the accept will succeed as long as
	// the token is valid.
	conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{
			"Authorization": []string{"Bearer " + c.token},
		},
	})
	if err != nil {
		return fmt.Errorf("dial %s: %w", wsURL, err)
	}
	// CloseNow is the fail-closed backstop; a normal exit path does a clean close.
	defer conn.CloseNow()

	// Put the terminal into raw mode. We operate on stdin's fd.
	fd := int(os.Stdin.Fd())
	oldState, err := term.MakeRaw(fd)
	if err != nil {
		// If stdin is not a terminal (piped), continue without raw mode so the
		// command still works for scripted use (no SIGWINCH either).
		oldState = nil
	}
	if oldState != nil {
		defer term.Restore(fd, oldState) //nolint:errcheck // best-effort restore
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
		_ = term.Restore(fd, oldState)
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
// endpoint:
//
//	https://host/  ->  wss://host/api/v1/runs/<id>/attach
//	http://host/   ->  ws://host/api/v1/runs/<id>/attach
func buildWSURL(baseURL, runID string) string {
	base := strings.TrimRight(baseURL, "/")
	// Replace the scheme: https -> wss, http -> ws.
	switch {
	case strings.HasPrefix(base, "https://"):
		base = "wss://" + strings.TrimPrefix(base, "https://")
	case strings.HasPrefix(base, "http://"):
		base = "ws://" + strings.TrimPrefix(base, "http://")
		// If already ws/wss or some other scheme, leave it alone.
	}
	return base + "/api/v1/runs/" + runID + "/attach"
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
	// a normal detach scenario.
	if strings.Contains(err.Error(), "context canceled") {
		return true
	}
	return false
}
