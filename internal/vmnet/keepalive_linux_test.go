// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package vmnet

import (
	"context"
	"net"
	"syscall"
	"testing"
)

// The proxy leg is a native socket from the relay's default dial: read its
// keepalive options back from the kernel.
func TestProxyLegHasKeepalive(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	c, err := newProxyClient(l.Addr().String(), nil).dial(context.Background(), "tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	rc, err := c.(*net.TCPConn).SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	get := func(level, opt int) (v int) {
		_ = rc.Control(func(fd uintptr) { v, _ = syscall.GetsockoptInt(int(fd), level, opt) })
		return v
	}
	if on, idle, intvl, cnt := get(syscall.SOL_SOCKET, syscall.SO_KEEPALIVE), get(syscall.IPPROTO_TCP, syscall.TCP_KEEPIDLE),
		get(syscall.IPPROTO_TCP, syscall.TCP_KEEPINTVL), get(syscall.IPPROTO_TCP, syscall.TCP_KEEPCNT); on != 1 ||
		idle != int(keepaliveIdle.Seconds()) || intvl != int(keepaliveInterval.Seconds()) || cnt != keepaliveCount {
		t.Fatalf("keepalive on=%d idle=%d interval=%d count=%d", on, idle, intvl, cnt)
	}
}
