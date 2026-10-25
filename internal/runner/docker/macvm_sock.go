// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker && unix

package docker

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"github.com/cjohnstoniv/wardyn/internal/vm/guestagent"
)

// A macOS VM's sockets (ctl.sock, agent.sock, desktop.sock) live in its socket directory,
// <sock-root>/u<uid>/<run-key>/ (ERRATA E1), not in its run directory under the home. The helper makes
// it; every dial here first checks it as the helper made it (this login's, mode 0700, not a link, through
// u<uid>'s descriptor walk), so a directory another login planted is never dialled.

// sockDir is runID's socket directory, unchecked: the path a wait for the helper's socket polls.
func (c *MacVMConfig) sockDir(runID uuid.UUID) string {
	return macvm.SockDir(c.SockRoot, runID.String())
}

// sockPath is the socket name in runID's checked socket directory. A directory that is not there
// answers its lstat error (ENOENT), as a missing socket does.
func (c *MacVMConfig) sockPath(runID uuid.UUID, name string) (string, error) {
	if _, err := os.Lstat(c.sockDir(runID)); err != nil {
		return "", err
	}
	dir, err := macvm.RunSockDir(macvm.RunConfig{RunID: runID.String(), SockRoot: c.SockRoot}, c.sockFstat)
	if err != nil {
		return "", fmt.Errorf("docker: %w", err)
	}
	return filepath.Join(dir, name), nil
}

// dialControl dials runID's helper on the ctl.sock of its checked socket directory.
func (c *MacVMConfig) dialControl(ctx context.Context, runID uuid.UUID) (*macvm.ControlClient, error) {
	p, err := c.sockPath(runID, macvm.ControlSock)
	if err != nil {
		return nil, err
	}
	return macvm.DialControl(ctx, p)
}

// sweepSockResidue removes the socket directories of this login's runs whose run directory is gone; a
// teardown and the orphan sweep call it once they have deleted a run directory. A failure is logged and
// the next sweep retries.
func (c *MacVMConfig) sweepSockResidue() {
	if _, err := c.sweepSock(c.Layout, c.SockRoot); err != nil {
		slog.Warn("docker: remove macOS VM socket directories (the next sweep retries)", slog.String("err", err.Error()))
	}
}

// toolsDialAsks bounds recordToolsDial: the guest agent's own dial of the tools port takes up to 5 s.
var toolsDialAsks, toolsDialEvery = 15, time.Second

// recordToolsDial keeps how the run boot's guest agent dial of the tools port ended (DR-0428: a run boot
// listens on no tools port, so "refused" is what it should say), asked on the agent's control session
// (GET /v1/dial/tools), in ToolsDialFile for `macos-vm status`. An agent that has not reported yet is
// asked again; one that never does leaves the file as it was.
func (c *MacVMConfig) recordToolsDial(ctx context.Context, agent *guestagent.Client) {
	if c.ToolsDialFile == "" {
		return
	}
	for range toolsDialAsks {
		d, err := agent.ToolsDial(ctx)
		if err == nil && d.Class != "" {
			class := d.Class
			if !macvm.ValidToolsDial(class) {
				class = macvm.ToolsDialTransport
			}
			if err := os.WriteFile(c.ToolsDialFile, []byte(class+"\n"), 0o600); err != nil {
				slog.Warn("docker: keep the macOS VM's tools-port dial", slog.String("err", err.Error()))
			}
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(toolsDialEvery):
		}
	}
}
