// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Command wardyn-guest-agent runs as root, from a LaunchDaemon, inside a macOS VM sandbox and is how
// wardyn-runnerd reaches it: the harness launch, terminals and exec streams, managed files, the proxy
// CA and the guest's Screen Sharing. A macOS guest has no virtio-serial port, so the agent dials the
// host over vsock (control on one port, the desktop tunnel on another) and keeps one idle connection
// open on each; it opens no network listener. Every exec runs as the guest's agent account.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/cjohnstoniv/wardyn/internal/version"
	"github.com/cjohnstoniv/wardyn/internal/vm/guestagent"
)

func main() {
	out := flag.String("out", "/var/run/wardyn-guest", "directory for detached execs' output (cleared at boot)")
	flag.Parse()
	if err := run(*out); err != nil {
		slog.Error("wardyn-guest-agent", "err", err)
		os.Exit(1)
	}
}

func run(out string) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	runner := guestagent.ExecRunner{}
	ids, err := guestagent.LookupAgentIDs(ctx, runner, guestagent.MacAgentUser)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(out, 0o700); err != nil {
		return err
	}
	desktop := &guestagent.Desktop{Run: runner, Console: guestagent.ConsoleOwner{}}
	srv := &guestagent.Server{
		Version:   version.String(),
		Cred:      &syscall.Credential{Uid: ids.UID, Gid: ids.GID, Groups: ids.Groups},
		Home:      guestagent.MacAgentHome,
		User:      guestagent.MacAgentUser,
		OutDir:    out,
		MarkerDir: "/var/run",
		Desktop:   desktop,
	}
	h, err := srv.Handler()
	if err != nil {
		return err
	}
	slog.Info("wardyn-guest-agent serving", "version", version.String())
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_ = guestagent.ServeVsock(ctx, hostDialer(guestagent.VsockControlPort), h)
	}()
	go func() {
		defer wg.Done()
		_ = guestagent.SpliceDesktop(ctx, hostDialer(guestagent.VsockDesktopPort), screenSharing, desktop.Track)
	}()
	wg.Wait()
	return nil
}

func hostDialer(port uint32) guestagent.DialFunc {
	return func(ctx context.Context) (net.Conn, error) { return guestagent.DialHost(ctx, port) }
}

// screenSharing dials the guest's own Screen Sharing, the desktop tunnel's only destination.
func screenSharing(ctx context.Context) (net.Conn, error) {
	var d net.Dialer
	c, err := d.DialContext(ctx, "tcp", guestagent.DesktopTarget)
	if err != nil {
		return nil, fmt.Errorf("Screen Sharing: %w", err)
	}
	return c, nil
}
