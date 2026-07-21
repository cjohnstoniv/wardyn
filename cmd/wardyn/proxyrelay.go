// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"io"
	"net"
	"os/signal"
	"strconv"
	"sync"
	"syscall"

	"github.com/spf13/cobra"
)

// proxyrelay.go — a host-side TCP relay for the LOOPBACK-ONLY corporate proxy.
//
// Some corporate connectivity clients bind their forward proxy to 127.0.0.1
// only. That is reachable from host processes but from nothing else: a sandbox's
// 127.0.0.1 is its own, and on a VM-backed Docker host (Rancher Desktop / Lima /
// Docker Desktop) the runtime VM cannot reach the host's loopback either. So
// wardyn-proxy has no usable upstream and every APPROVED egress fails to reach
// the corp proxy — the sandbox is correctly configured and still cannot connect.
//
// This relay listens on all interfaces and forwards raw bytes to the loopback
// proxy, giving the sandbox an address it CAN reach. It is deliberately dumb: no
// TLS, no parsing, no policy. Wardyn's egress policy is enforced by wardyn-proxy
// BEFORE traffic ever arrives here — this only moves bytes across the interface
// boundary the corp client refuses to cross.
//
// It is a FOREGROUND command, not a daemon: Wardyn supervises no host processes,
// and an operator who needs it past a reboot should wrap it in their own
// user-level service (launchd/systemd --user). `wardyn setup status` points here
// when it detects a loopback-bound proxy.
//
// SECURITY: this exposes the corp proxy to anything that can reach the listen
// address. Bind it to the narrowest interface that works (--listen-addr), and
// remember the corp proxy's own authentication still applies to every request.

func setupProxyRelayCmd() *cobra.Command {
	var listenAddr string
	var targetHost string

	cmd := &cobra.Command{
		Use:   "proxy-relay <listen-port> <proxy-port>",
		Short: "Forward a reachable port to a loopback-only corporate proxy (foreground)",
		Long: "Forward <listen-port> on this host to <proxy-port> on loopback, so a sandbox can\n" +
			"reach a corporate proxy that is bound to 127.0.0.1 and would otherwise be\n" +
			"unreachable from any container.\n\n" +
			"Runs in the FOREGROUND until interrupted. Store the relay's address as the\n" +
			"upstream-proxy secret and reference it from site-config; the sandbox's egress\n" +
			"policy is still enforced by wardyn-proxy before anything reaches the relay.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			listenPort, err := strconv.Atoi(args[0])
			if err != nil || listenPort < 1 || listenPort > 65535 {
				return fmt.Errorf("listen-port %q is not a valid TCP port", args[0])
			}
			proxyPort, err := strconv.Atoi(args[1])
			if err != nil || proxyPort < 1 || proxyPort > 65535 {
				return fmt.Errorf("proxy-port %q is not a valid TCP port", args[1])
			}

			target := net.JoinHostPort(targetHost, strconv.Itoa(proxyPort))
			// Fail fast when nothing is listening on the corp proxy port: an
			// operator who mistypes it should learn now, not from a stalled run.
			probe, err := net.Dial("tcp", target)
			if err != nil {
				return fmt.Errorf("nothing is listening on %s — check the port your corporate proxy client binds: %w", target, err)
			}
			_ = probe.Close()

			ln, err := net.Listen("tcp", net.JoinHostPort(listenAddr, strconv.Itoa(listenPort)))
			if err != nil {
				return fmt.Errorf("listen on %s:%d: %w", listenAddr, listenPort, err)
			}
			defer ln.Close()

			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()
			go func() { <-ctx.Done(); _ = ln.Close() }()

			fmt.Fprintf(cmd.OutOrStdout(), "relaying %s -> %s (Ctrl-C to stop)\n", ln.Addr(), target)
			fmt.Fprintf(cmd.OutOrStdout(),
				"store this as the upstream-proxy secret, replacing <host-gateway> with the address your sandbox reaches this host on:\n"+
					"    wardyn secret set upstream-proxy-url   # then paste: http://<host-gateway>:%d\n", listenPort)

			for {
				client, err := ln.Accept()
				if err != nil {
					if ctx.Err() != nil {
						fmt.Fprintln(cmd.OutOrStdout(), "stopped")
						return nil
					}
					return fmt.Errorf("accept: %w", err)
				}
				go relayConn(client, target)
			}
		},
	}
	cmd.Flags().StringVar(&listenAddr, "listen-addr", "0.0.0.0",
		"address to listen on; narrow this when a specific interface reaches your sandbox")
	cmd.Flags().StringVar(&targetHost, "target-host", "127.0.0.1",
		"loopback address the corporate proxy is bound to")
	return cmd
}

// relayConn pumps bytes both ways between an accepted client and a fresh
// connection to the corp proxy, closing both when either side finishes.
func relayConn(client net.Conn, target string) {
	defer client.Close()
	upstream, err := net.Dial("tcp", target)
	if err != nil {
		return // the client sees a closed connection; wardyn-proxy reports the failure
	}
	defer upstream.Close()

	var wg sync.WaitGroup
	wg.Add(2)
	cp := func(dst, src net.Conn) {
		defer wg.Done()
		_, _ = io.Copy(dst, src)
		// Half-close the write side so the peer sees EOF rather than stalling.
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
	}
	go cp(client, upstream)
	go cp(upstream, client)
	wg.Wait()
}
