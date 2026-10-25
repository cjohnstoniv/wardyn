// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"sync"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"github.com/cjohnstoniv/wardyn/internal/vmnet"
)

// prepNet is prepare's prep network: vmnet's no-egress preparation relay on
// the guest NIC's datagram socket. The embedded relay gives it DHCPRequests.
type prepNet struct {
	*vmnet.Relay
	closeOnce sync.Once
}

var _ macvm.PrepNetwork = (*prepNet)(nil)

// newPrepNet builds the relay, logging its refusals and drops to log.
func newPrepNet(log *slog.Logger) (macvm.PrepNetwork, error) {
	r, err := vmnet.New(vmnet.Config{Prep: true, Log: log})
	if err != nil {
		return nil, err
	}
	return &prepNet{Relay: r}, nil
}

// Serve relays nic until Close. nic stays prepare's, which closes it.
func (p *prepNet) Serve(nic *os.File) error { return p.ServeDatagram(nic) }

func (p *prepNet) DialGuest(ctx context.Context, port int) (net.Conn, error) {
	if port < 1 || port > 65535 {
		return nil, fmt.Errorf("wardyn-macvm: guest port %d is not 1-65535", port)
	}
	return p.Relay.DialGuest(ctx, uint16(port))
}

// Close stops the relay; a second Close does nothing.
func (p *prepNet) Close() error {
	p.closeOnce.Do(p.Relay.Close)
	return nil
}
