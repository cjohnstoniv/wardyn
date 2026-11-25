// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"

	"github.com/google/uuid"
	"github.com/moby/moby/client"
)

// bindProxyListeners returns cfgJSON with each listener the started proxy id opens bound to its own address on
// the network that listener serves, keeping each port, keyed by the proxy's component label:
//   - the agent's proxy: listen on its address on the run's own network. The proxy also sits on the
//     control-plane-facing network, which other containers share, and on the run's relay link; binding to the
//     run-network address keeps its listener off both, so only the run's own agent can reach it.
//   - an MCP proxy: listen on its MCP network address, so only its MCP container reaches it, and
//     mcp_relay_inbound.listen on its relay-link address, so only the agent's proxy reaches the relay. Its DNS
//     listener is bound by its environment (createMCPProxy).
//
// The digest the caller takes is of the bytes this returns.
func (d *Driver) bindProxyListeners(ctx context.Context, runID uuid.UUID, id string, cfgJSON []byte) ([]byte, error) {
	res, err := d.cli.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if err != nil {
		return nil, fmt.Errorf("docker: inspect proxy for its listen address: %w", err)
	}
	addrOn := func(network string) (netip.Addr, error) {
		var addr netip.Addr
		if ns := res.Container.NetworkSettings; ns != nil {
			if ep := ns.Networks[network]; ep != nil {
				addr = ep.IPAddress
			}
		}
		if !addr.IsValid() || addr.IsUnspecified() {
			return netip.Addr{}, fmt.Errorf("docker: proxy has no IP on the per-run internal network %q to listen on", network)
		}
		return addr, nil
	}
	var data map[string]json.RawMessage
	if err := json.Unmarshal(cfgJSON, &data); err != nil {
		return nil, fmt.Errorf("docker: proxy config: %w", err)
	}
	if res.Container.Config == nil || res.Container.Config.Labels[labelComponent] != componentMCPProxy {
		addr, err := addrOn(internalNetName(runID))
		if err != nil {
			return nil, err
		}
		if data["listen"], err = listenOn(data["listen"], addr, "listen"); err != nil {
			return nil, err
		}
		return json.Marshal(data)
	}
	ord, ok := mcpOrdinalOf(res.Container.Name, runID)
	if !ok {
		return nil, fmt.Errorf("docker: MCP proxy %s is not named for run %s", res.Container.Name, runID)
	}
	mcpAddr, err := addrOn(mcpNetName(runID, ord))
	if err != nil {
		return nil, err
	}
	relayAddr, err := addrOn(relayLinkName(runID))
	if err != nil {
		return nil, err
	}
	if data["listen"], err = listenOn(data["listen"], mcpAddr, "listen"); err != nil {
		return nil, err
	}
	var inbound map[string]json.RawMessage
	if err := json.Unmarshal(data["mcp_relay_inbound"], &inbound); err != nil || inbound == nil {
		return nil, fmt.Errorf("docker: an MCP proxy's config needs mcp_relay_inbound")
	}
	if inbound["listen"], err = listenOn(inbound["listen"], relayAddr, "mcp_relay_inbound.listen"); err != nil {
		return nil, err
	}
	if data["mcp_relay_inbound"], err = json.Marshal(inbound); err != nil {
		return nil, err
	}
	return json.Marshal(data)
}

// listenOn is the JSON listen address raw with its host replaced by addr, keeping its port; key names it in errors.
func listenOn(raw json.RawMessage, addr netip.Addr, key string) (json.RawMessage, error) {
	var listen string
	if err := json.Unmarshal(raw, &listen); err != nil {
		return nil, fmt.Errorf("docker: proxy config %s: %w", key, err)
	}
	_, port, err := net.SplitHostPort(listen)
	if err != nil {
		return nil, fmt.Errorf("docker: proxy config %s %q: %w", key, listen, err)
	}
	return json.Marshal(net.JoinHostPort(addr.Unmap().String(), port))
}
