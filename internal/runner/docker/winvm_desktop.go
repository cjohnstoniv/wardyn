// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"fmt"
	"io"
	"net"
	"sync"
)

// winVM is what the driver keeps for a Windows VM beyond a vmRun: its one desktop stream. A Windows
// VM has one interactive session: a new dial closes the stream before it, so the newest viewer wins
// and a dead browser tab never locks the owner out. The runner is the one place the VM lives, so this
// holds across control-plane replicas.
type winVM struct {
	mu  sync.Mutex
	cur *winDesktopConn
}

// winDesktopConn is a desktop stream that forgets itself when closed.
type winDesktopConn struct {
	net.Conn
	w *winVM
}

func (c *winDesktopConn) Close() error {
	c.w.mu.Lock()
	if c.w.cur == c {
		c.w.cur = nil
	}
	c.w.mu.Unlock()
	return c.Conn.Close()
}

// dialWinDesktop opens the Windows VM's display (QEMU's VNC socket, sock) and closes the stream it
// replaces.
func (d *Driver) dialWinDesktop(ctx context.Context, v *vmRun, sock string) (io.ReadWriteCloser, error) {
	conn, err := d.dialUnix(ctx, sock)
	if err != nil {
		return nil, fmt.Errorf("docker: VM desktop: %w", err)
	}
	nc := &winDesktopConn{Conn: conn, w: v.win}
	v.win.mu.Lock()
	old := v.win.cur
	v.win.cur = nc
	v.win.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}
	return nc, nil
}
