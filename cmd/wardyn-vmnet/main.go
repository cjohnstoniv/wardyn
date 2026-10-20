// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Command wardyn-vmnet is a desktop VM's network relay (internal/vmnet): QEMU's
// -netdev stream socket on one side, the run's wardyn-proxy on the other.
// Every connection the desktop makes goes through it, and it hands allowed
// ones to the proxy and drops the rest. It runs in its own unprivileged
// container (no capabilities, read-only root, non-root user) on the run's
// internal network, which reaches nothing but the proxy.
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
	flag.Parse()
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if *listen == "" {
		log.Error("wardyn-vmnet: -listen is required")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	rl, err := vmnet.New(vmnet.Config{Proxy: *proxyAddr, Log: log})
	if err != nil {
		log.Error("wardyn-vmnet: fatal", "err", err.Error())
		os.Exit(1)
	}
	defer rl.Close()
	_ = os.Remove(*listen) // a socket a previous relay left behind
	if err := rl.Listen(ctx, *listen); err != nil {
		log.Error("wardyn-vmnet: fatal", "err", err.Error())
		os.Exit(1)
	}
}
