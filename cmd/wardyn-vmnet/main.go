// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Command wardyn-vmnet is a desktop VM's network relay (internal/vmnet): QEMU's
// -netdev stream socket on one side, the run's wardyn-proxy on the other.
// Every connection the desktop makes goes through it, and it hands allowed
// ones to the proxy and drops the rest. It runs in its own unprivileged
// container (no capabilities, read-only root, non-root user) on the run's
// internal network, which reaches nothing but the proxy.
//
// -stdio serves one macOS guest's frames over stdin and stdout (the framing is
// the same 4-byte length plus Ethernet frame) and answers its DHCP; -prep makes
// that relay a base-image preparation one with no proxy at all, so the guest
// reaches nothing. Without them the flow is the Linux desktop VM's, unchanged.
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/cjohnstoniv/wardyn/internal/vmnet"
)

func main() {
	listen := flag.String("listen", "", "unix socket QEMU's -netdev stream connects to (the run volume's net.sock)")
	proxyAddr := flag.String("proxy", "wardyn-proxy:3128", "the run's wardyn-proxy, host:port")
	stdio := flag.Bool("stdio", false, "serve one guest's frames over stdin and stdout instead of -listen, and answer its DHCP")
	prep := flag.Bool("prep", false, "base-image preparation: no proxy, every guest flow is refused (with -stdio or -listen)")
	flag.Parse()
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if *listen == "" && !*stdio || *listen != "" && *stdio {
		log.Error("wardyn-vmnet: exactly one of -listen and -stdio is required")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	rl, err := vmnet.New(vmnet.Config{Proxy: *proxyAddr, Log: log, DHCP: *stdio, Prep: *prep})
	if err != nil {
		log.Error("wardyn-vmnet: fatal", "err", err.Error())
		os.Exit(1)
	}
	defer rl.Close()
	if *stdio {
		// stdout carries frames only; logs are on stderr. The guest's side ends
		// the stream (EOF on stdin) or a signal does.
		done := make(chan error, 1)
		go func() { done <- rl.ServeStream(os.Stdin, os.Stdout) }()
		select {
		case err = <-done:
		case <-ctx.Done():
			return
		}
		if err != nil {
			log.Error("wardyn-vmnet: fatal", "err", err.Error())
			os.Exit(1)
		}
		return
	}
	_ = os.Remove(*listen) // a socket a previous relay left behind
	if err := rl.Listen(ctx, *listen); err != nil {
		log.Error("wardyn-vmnet: fatal", "err", err.Error())
		os.Exit(1)
	}
}
