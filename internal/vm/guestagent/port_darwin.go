// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package guestagent

import (
	"context"
	"fmt"
	"net"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// DialHost opens a vsock stream from the guest to the host on port. The host side is the helper's
// VZVirtioSocketDevice listener; whether a macOS 27 guest's dial reaches it is MAC-PROOF P5.
func DialHost(ctx context.Context, port uint32) (net.Conn, error) {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM, 0)
	if err != nil {
		return nil, fmt.Errorf("vsock socket: %w", err)
	}
	if err := connectVM(ctx, fd, &unix.SockaddrVM{CID: VsockCIDHost, Port: port}); err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("vsock connect to the host on port %d: %w", port, err)
	}
	if err := unix.SetNonblock(fd, true); err != nil {
		_ = unix.Close(fd)
		return nil, err
	}
	return &vsockConn{f: os.NewFile(uintptr(fd), "vsock")}, nil
}

// connectVM connects fd, giving up when ctx ends. The socket is blocking, so a cancelled ctx closes it.
func connectVM(ctx context.Context, fd int, sa unix.Sockaddr) error {
	stop := context.AfterFunc(ctx, func() { _ = unix.Shutdown(fd, unix.SHUT_RDWR) })
	defer stop()
	err := unix.Connect(fd, sa)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

// vsockConn is a net.Conn over a vsock descriptor: net.FileConn knows no AF_VSOCK, so the file carries
// the stream itself.
type vsockConn struct{ f *os.File }

func (c *vsockConn) Read(p []byte) (int, error)         { return c.f.Read(p) }
func (c *vsockConn) Write(p []byte) (int, error)        { return c.f.Write(p) }
func (c *vsockConn) Close() error                       { return c.f.Close() }
func (c *vsockConn) LocalAddr() net.Addr                { return vsockAddr{} }
func (c *vsockConn) RemoteAddr() net.Addr               { return vsockAddr{} }
func (c *vsockConn) SetDeadline(t time.Time) error      { return c.f.SetDeadline(t) }
func (c *vsockConn) SetReadDeadline(t time.Time) error  { return c.f.SetReadDeadline(t) }
func (c *vsockConn) SetWriteDeadline(t time.Time) error { return c.f.SetWriteDeadline(t) }
