// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"net"
	"os"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"github.com/cjohnstoniv/wardyn/internal/vmnet"
)

// prepNet is prepare's prep network: vmnet's no-egress preparation relay on
// the guest NIC's datagram socket. The embedded relay gives it DHCPRequests.
type prepNet struct{ *vmnet.Relay }

var _ macvm.PrepNetwork = prepNet{}

func newPrepNet() (macvm.PrepNetwork, error) {
	r, err := vmnet.New(vmnet.Config{Prep: true})
	if err != nil {
		return nil, err
	}
	return prepNet{r}, nil
}

// Serve relays nic until Close. nic stays prepare's, which closes it.
func (p prepNet) Serve(nic *os.File) error { return p.ServeDatagram(nic) }

func (p prepNet) DialGuest(ctx context.Context, port int) (net.Conn, error) {
	if port < 1 || port > 65535 {
		return nil, fmt.Errorf("wardyn-macvm: guest port %d is not 1-65535", port)
	}
	return p.Relay.DialGuest(ctx, uint16(port))
}

func (p prepNet) Close() error {
	p.Relay.Close()
	return nil
}
