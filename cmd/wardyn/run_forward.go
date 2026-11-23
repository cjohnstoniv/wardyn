// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/spf13/cobra"

	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// runForwardCmd returns `wardyn run forward <run-id> [local:]port...`: ssh -L
// to the sandbox's own loopback through the SSH gateway, or, with the gateway
// off and the operator's WARDYN_ATTACH_FORWARD on, over the organisation. It
// reaches nothing the gateway's -L does not already allow (docs/SSH.md, section 4).
func runForwardCmd(client clientFn) *cobra.Command {
	var advertisedProxy bool
	cmd := &cobra.Command{
		Use:   "forward <run-id> [<local-port>:]<sandbox-port>...",
		Short: "Forward local ports to a running sandbox over the SSH gateway, or the organisation",
		Long: `Forward ports on this computer to ports on the sandbox's own loopback over the
SSH gateway (WARDYN_SSH_LISTEN), with your registered SSH key, until Ctrl-C.
With the gateway off, it forwards through the organisation instead when the
operator has turned that on (WARDYN_ATTACH_FORWARD): each connection takes its
own attach ticket, and no SSH key is used.

3000 forwards 127.0.0.1:3000 here to 127.0.0.1:3000 in the sandbox; 8080:3000
listens on 8080 here. The listener is bound to 127.0.0.1 only. The gateway
refuses any destination but the sandbox's loopback. A local port already in
use ends the command instead of leaving a forward that does not exist.

Without the gateway, and without WARDYN_ATTACH_FORWARD, or without a registered
key on the gateway path, it stops with one line saying so. The key is made by
'wardyn ssh-key ensure', never by this command.
`,
		Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if _, err := parseID("run", args[0]); err != nil {
				return err
			}
			var forwards []string
			var pairs []forwardPair
			for _, spec := range args[1:] {
				local, remote, err := splitForward(spec)
				if err != nil {
					return err
				}
				forwards = append(forwards, formatForward(local, remote))
				pairs = append(pairs, forwardPair{local, remote})
			}
			c := client()
			if _, err := resolveSSHGateway(cmd.Context(), c); errors.Is(err, errSSHGatewayOff) {
				return forwardOverOrganisation(cmd, c, args[0], pairs)
			}
			acc, err := resolveSSHAccess(cmd.Context(), c, "forward", args[0], advertisedProxy)
			if err != nil {
				return err
			}
			for _, f := range forwards {
				fmt.Fprintf(cmd.ErrOrStderr(), "forwarding %s (Ctrl-C to stop)\n", f)
			}
			return runSSHBinary(cmd.Context(), forwardSSHArgs(acc, args[0], forwards))
		},
	}
	cmd.Flags().BoolVar(&advertisedProxy, "advertised-proxy", false, "connect through the ProxyCommand this deployment advertises (it runs on this computer)")
	return cmd
}

// forwardPair is one forward: the local port and the sandbox's.
type forwardPair struct{ local, remote int }

// splitForward reads "R" or "L:R" into its two ports.
func splitForward(spec string) (local, remote int, err error) {
	l, r, found := strings.Cut(spec, ":")
	if !found {
		l, r = spec, spec
	}
	ports := [2]int{}
	for i, p := range []string{l, r} {
		n, aerr := strconv.Atoi(p)
		if aerr != nil || n < 1 || n > 65535 || strings.Trim(p, "0123456789") != "" {
			return 0, 0, fmt.Errorf("forward: %q is not <sandbox-port> or <local-port>:<sandbox-port> (ports are 1 to 65535)", spec)
		}
		ports[i] = n
	}
	return ports[0], ports[1], nil
}

// formatForward is ssh's "127.0.0.1:L:127.0.0.1:R".
func formatForward(local, remote int) string {
	return "127.0.0.1:" + strconv.Itoa(local) + ":127.0.0.1:" + strconv.Itoa(remote)
}

// forwardSSHArgs is ssh(1)'s argv for forward.
func forwardSSHArgs(acc sshAccess, runID string, forwards []string) []string {
	a := []string{"-N", "-o", "ExitOnForwardFailure=yes"}
	for _, f := range forwards {
		a = append(a, "-L", f)
	}
	return append(a, sshConnArgs(acc.gw, runID, acc.identity)...)
}

// forwardNoTicket is the line for a mint that answered nothing usable.
const forwardNoTicket = "could not get an attach ticket from the organisation"

// forwardOlderServer is the refusal when the check finds no forward route: an older server, or a gateway the
// operator left off.
const forwardOlderServer = "forward: the operator has not turned SSH on for this deployment, so there is no gateway to reach (WARDYN_SSH_LISTEN), and this server offers no forwarding over the organisation"

// forwardOverOrganisation is `run forward` with the gateway off: every pair's local listener relays each accepted
// connection over its own WebSocket, GET /runs/{id}/attach/forward, with its own single-use attach ticket. It never
// dials without a ticket.
func forwardOverOrganisation(cmd *cobra.Command, c *sdk.Client, runID string, pairs []forwardPair) error {
	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt)
	defer stop()
	if err := forwardPreCheck(ctx, c, runID, pairs[0].remote); err != nil {
		return err
	}
	var lns []net.Listener
	closeAll := func() {
		for _, ln := range lns {
			_ = ln.Close()
		}
	}
	for _, p := range pairs {
		ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(p.local))
		if err != nil {
			closeAll()
			return fmt.Errorf("forward: 127.0.0.1:%d is not free here: %w", p.local, err)
		}
		lns = append(lns, ln)
	}
	var mu sync.Mutex
	logf := func(format string, a ...any) {
		mu.Lock()
		defer mu.Unlock()
		fmt.Fprintf(cmd.ErrOrStderr(), format+"\n", a...)
	}
	var wg sync.WaitGroup
	for i, p := range pairs {
		logf("forwarding 127.0.0.1:%d to the sandbox's port %d through the organisation (Ctrl-C to stop)", p.local, p.remote)
		wg.Add(1)
		go func(ln net.Listener, remote int) {
			defer wg.Done()
			for {
				conn, err := ln.Accept()
				if err != nil {
					return
				}
				wg.Add(1)
				go func() {
					defer wg.Done()
					forwardConn(ctx, c, runID, remote, conn, logf)
				}()
			}
		}(lns[i], p.remote)
	}
	<-ctx.Done()
	closeAll()
	wg.Wait()
	return nil
}

// forwardPreCheck mints one ticket and asks the route to run its checks without opening anything (?check=1), so a
// deployment that has not turned forwarding on, or an older server, ends the command before any port is bound.
func forwardPreCheck(ctx context.Context, c *sdk.Client, runID string, port int) error {
	t, err := mintAttachTicket(ctx, c, runID)
	if err != nil {
		return fmt.Errorf("forward: %w", err)
	}
	if t == "" {
		return errors.New("forward: " + forwardNoTicket)
	}
	q := url.Values{"port": {strconv.Itoa(port)}, "check": {"1"}, "ticket": {t}}
	target := strings.TrimRight(c.BaseURL, "/") + "/api/v1/runs/" + runID + "/attach/forward?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return errors.New("forward: " + redactTicket(err.Error()))
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := orgClient(c.BaseURL, 30*time.Second).Do(req)
	if err != nil {
		return errors.New("forward: " + redactTicket(err.Error()))
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	if resp.StatusCode >= 200 && resp.StatusCode <= 299 {
		return nil
	}
	apiErr := sdk.NewAPIError(resp.StatusCode, body)
	// An older server has no such route: chi answers a plain-text 404, or a 405. A 404 that carries a reason
	// (run_not_found) is the server's own answer.
	if resp.StatusCode == http.StatusMethodNotAllowed || (resp.StatusCode == http.StatusNotFound && apiErr.Reason == "") {
		return errors.New(forwardOlderServer)
	}
	return fmt.Errorf("forward: %w", apiErr)
}

// forwardConn relays one accepted local connection over its own WebSocket and ticket, and closes it. A refusal
// prints one line and leaves the listener running.
func forwardConn(ctx context.Context, c *sdk.Client, runID string, remote int, local net.Conn, logf func(string, ...any)) {
	defer local.Close()
	mctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	t, err := mintAttachTicket(mctx, c, runID)
	cancel()
	if err != nil {
		logf("forward: %d: %s", remote, apiSentence(err))
		return
	}
	if t == "" {
		logf("forward: %d: %s", remote, forwardNoTicket)
		return
	}
	q := url.Values{"port": {strconv.Itoa(remote)}, "ticket": {t}}
	var hdr http.Header
	if c.Token != "" {
		hdr = http.Header{"Authorization": []string{"Bearer " + c.Token}}
	}
	ws, resp, err := websocket.Dial(ctx, buildWSURL(c.BaseURL, runID, "")+"/forward?"+q.Encode(), &websocket.DialOptions{
		HTTPHeader: hdr,
		HTTPClient: orgClient(c.BaseURL, 0),
	})
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		if resp != nil {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
			err = sdk.NewAPIError(resp.StatusCode, body)
		}
		logf("forward: %d: %s", remote, apiSentence(err))
		return
	}
	// NetConn sets no read limit. A WebSocket has no half-close, so when either direction ends both close: a local
	// client that sends and then half-closes may lose the reply.
	nc := websocket.NetConn(ctx, ws, websocket.MessageBinary)
	defer nc.Close()
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(nc, local); done <- struct{}{} }()
	go func() { _, _ = io.Copy(local, nc); done <- struct{}{} }()
	<-done
	_ = nc.Close()
	_ = local.Close()
	<-done
}

// apiSentence is the server's own sentence for an API error (its envelope's error), else err's text.
func apiSentence(err error) string {
	var ae *sdk.APIError
	if errors.As(err, &ae) {
		var env struct {
			Error string `json:"error"`
		}
		if json.Unmarshal([]byte(ae.Body), &env) == nil && env.Error != "" {
			return env.Error
		}
	}
	return redactTicket(err.Error())
}

// ticketQuery matches a ticket's value in a URL query, which a transport error's text carries whole.
var ticketQuery = regexp.MustCompile(`ticket=[^&\s"']*`)

// redactTicket hides every ticket value in s, so a transport error that quotes the request's URL never puts a
// ticket that was not spent on stderr.
func redactTicket(s string) string { return ticketQuery.ReplaceAllString(s, "ticket=REDACTED") }
