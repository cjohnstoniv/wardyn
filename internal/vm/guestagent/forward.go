// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package guestagent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"runtime"
	"strconv"
	"time"
)

// A VM sandbox has no socat, so the SSH gateway's `-L` forward and the UI-app relay exec the guest
// agent's own binary in this mode: `wardyn-guest-agent forward <port>` copies its stdin and stdout to
// the guest's own 127.0.0.1:<port>. It takes a port and nothing else: no host, no address family, no
// name. The caller (the gateway) has already refused a destination that is not the sandbox's own
// loopback and a port the ask does not name; this is the second wall, so an exec of the mode can
// reach nothing but a listener on 127.0.0.1.
const (
	// ForwardVerb is the first argument that selects the mode.
	ForwardVerb = "forward"
	// ForwardProbeFlag, before the port, only checks that something listens: it dials, closes and
	// exits 0, or exits 1 if the dial fails.
	ForwardProbeFlag = "-probe"
	// LinuxAgentPath is where the Linux VM image installs the agent (deploy/images/vm-linux).
	LinuxAgentPath = "/usr/local/bin/wardyn-guest-agent"

	forwardLoopback    = "127.0.0.1"
	forwardDialTimeout = 10 * time.Second
)

// MacScreenSharingPort reports whether port is one of the macOS guest's Screen Sharing ports,
// 5900-5909 on its loopback. They are the desktop door, which the window's session and recording
// accounting covers, so a forward never reaches them.
func MacScreenSharingPort(port int) bool { return port >= 5900 && port <= 5909 }

// ForwardArgv is the argv that runs the forward mode of the agent binary at agentPath.
func ForwardArgv(agentPath string, port int) []string {
	return []string{agentPath, ForwardVerb, strconv.Itoa(port)}
}

// ForwardProbeArgv is the argv of the listening check, for a shell script to use in place of socat.
func ForwardProbeArgv(agentPath string, port int) []string {
	return []string{agentPath, ForwardVerb, ForwardProbeFlag, strconv.Itoa(port)}
}

// ParseForwardPort reads the one argument the mode takes: decimal digits only, 1 to 65535. A host, a
// "host:port", a sign, a hex or empty string, and any further argument are refused. On macOS the
// Screen Sharing ports are refused too.
func ParseForwardPort(arg string) (int, error) {
	return parseForwardPort(runtime.GOOS, arg)
}

func parseForwardPort(goos, arg string) (int, error) {
	if arg == "" {
		return 0, errors.New("forward needs a port")
	}
	for _, r := range arg {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("forward takes a port number only, not %q", arg)
		}
	}
	n, err := strconv.Atoi(arg)
	if err != nil || n < 1 || n > 65535 {
		return 0, fmt.Errorf("forward port %q is outside 1-65535", arg)
	}
	if goos == "darwin" && MacScreenSharingPort(n) {
		return 0, fmt.Errorf("forward port %d is the guest's Screen Sharing; open the run's window instead", n)
	}
	return n, nil
}

// forwardDialer is net.Dialer.DialContext, so a test can see what the mode dials.
type forwardDialer func(ctx context.Context, network, addr string) (net.Conn, error)

func dialForward(ctx context.Context, dial forwardDialer, port int) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, forwardDialTimeout)
	defer cancel()
	// tcp4 and the literal address: nothing is resolved, and ::1 or another loopback is never tried.
	return dial(ctx, "tcp4", net.JoinHostPort(forwardLoopback, strconv.Itoa(port)))
}

// Forward copies in to the connection and the connection to out until the connection closes. An end
// of in half-closes the connection and the reply is still read to its end; an error on either side
// ends both.
func Forward(ctx context.Context, port int, in io.Reader, out io.Writer) error {
	return forward(ctx, (&net.Dialer{}).DialContext, port, in, out)
}

func forward(ctx context.Context, dial forwardDialer, port int, in io.Reader, out io.Writer) error {
	conn, err := dialForward(ctx, dial, port)
	if err != nil {
		return err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()

	go func() {
		_, err := io.Copy(conn, in)
		if cw, ok := conn.(interface{ CloseWrite() error }); ok && err == nil {
			_ = cw.CloseWrite()
		} else if err != nil {
			_ = conn.Close() // the writer broke: end the read too
		}
	}()
	_, err = io.Copy(out, conn)
	if err != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

// ForwardMain runs the mode for the agent's main, given the arguments after "forward", and returns
// the process exit code.
func ForwardMain(ctx context.Context, args []string, in io.Reader, out, errw io.Writer) int {
	probe := false
	if len(args) > 0 && args[0] == ForwardProbeFlag {
		probe, args = true, args[1:]
	}
	if len(args) != 1 {
		_, _ = fmt.Fprintln(errw, "usage: wardyn-guest-agent forward [-probe] <port>")
		return 2
	}
	port, err := ParseForwardPort(args[0])
	if err != nil {
		_, _ = fmt.Fprintln(errw, err)
		return 2
	}
	if probe {
		conn, err := dialForward(ctx, (&net.Dialer{}).DialContext, port)
		if err != nil {
			return 1
		}
		_ = conn.Close()
		return 0
	}
	if err := Forward(ctx, port, in, out); err != nil {
		_, _ = fmt.Fprintln(errw, "forward:", err)
		return 1
	}
	return 0
}
