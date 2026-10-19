// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Command wardyn-guest-agent runs as root inside a Linux VM sandbox and is how wardyn-runnerd reaches
// it: the harness launch, terminals and exec streams, managed files and folder shares. It serves the
// guestagent protocol on the virtio-serial port org.wardyn.agent.0, which reaches only the QEMU
// process outside the guest; it opens no network listener. Every exec runs as the image's agent user
// (uid 1000).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"os/user"
	"strconv"
	"syscall"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/version"
	"github.com/cjohnstoniv/wardyn/internal/vm/guestagent"
)

func main() {
	port := flag.String("port", "/dev/virtio-ports/"+guestagent.PortName, "the virtio-serial port to serve")
	out := flag.String("out", "/run/wardyn-guest", "directory for detached execs' output (a tmpfs)")
	flag.Parse()
	if err := run(*port, *out); err != nil {
		slog.Error("wardyn-guest-agent", "err", err)
		os.Exit(1)
	}
}

func run(port, out string) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	cred, err := agentCred()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(out, 0o700); err != nil {
		return err
	}
	// udev creates the port's name a moment after the device appears.
	for deadline := time.Now().Add(time.Minute); ; {
		if _, err := os.Stat(port); err == nil {
			break
		} else if !errors.Is(err, os.ErrNotExist) || time.Now().After(deadline) {
			return fmt.Errorf("the guest agent port %s: %w", port, err)
		}
		time.Sleep(200 * time.Millisecond)
	}
	srv := &guestagent.Server{Version: version.String(), Cred: cred, Home: guestagent.AgentHome, OutDir: out, Mount: guestagent.MountVirtiofs}
	slog.Info("wardyn-guest-agent serving", "port", port, "version", version.String())
	return guestagent.ServePort(ctx, port, srv.Handler())
}

// agentCred is the agent user with its supplementary groups, as a container exec gets them.
func agentCred() (*syscall.Credential, error) {
	u, err := user.LookupId(strconv.Itoa(guestagent.AgentUID))
	if err != nil {
		return nil, fmt.Errorf("the image has no agent user (uid %d): %w", guestagent.AgentUID, err)
	}
	cred := &syscall.Credential{Uid: guestagent.AgentUID, Gid: guestagent.AgentGID}
	gids, err := u.GroupIds()
	if err != nil {
		return nil, err
	}
	for _, g := range gids {
		if n, err := strconv.ParseUint(g, 10, 32); err == nil && n != guestagent.AgentGID {
			cred.Groups = append(cred.Groups, uint32(n))
		}
	}
	return cred, nil
}
