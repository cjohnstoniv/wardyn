// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/moby/moby/client"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// runnerLocalModelsKey is proxy.Config.RunnerLocalModels' key.
const runnerLocalModelsKey = "runner_local_models"

// LocalModelRelay is wardyn-runnerd's relay for laptop local models (#2957). OpenLocalModel serves
// each port at the relay target to runID's proxy alone, whose addresses are peers: a connection from
// any other address is closed before a byte is relayed. A port's listener opens with the first run
// that names it and closes when CloseLocalModel ends the last; Open again for runID replaces what it
// had.
type LocalModelRelay interface {
	OpenLocalModel(runID uuid.UUID, ports []uint16, peers []netip.Addr) error
	CloseLocalModel(runID uuid.UUID)
}

// withRunnerLocalModels returns the ports a runner's relay serves for each laptop local model in
// cfgJSON (LocalModelSettings.AllowOnLaptopRunners) and names those servers in the proxy's
// runner_local_models, which it takes off every other config: only this driver, on a runner, sets
// it. Off a runner (no ControlPlaneRelay) the flag changes nothing: the organisation's proxy reaches
// the server as any local model. The relay opens once the proxy runs (openLocalModels).
func (d *Driver) withRunnerLocalModels(cfgJSON []byte) ([]byte, []uint16, error) {
	var c struct {
		LocalModels map[string]types.LocalModelSettings `json:"local_models"`
	}
	if err := json.Unmarshal(cfgJSON, &c); err != nil {
		return nil, nil, fmt.Errorf("docker: local models: config: %w", err)
	}
	var relayed []string
	var ports []uint16
	for raw, s := range c.LocalModels {
		if !s.AllowOnLaptopRunners || d.cfg.ControlPlaneRelay == nil {
			continue
		}
		port, err := laptopModelPort(raw)
		if err == nil && d.cfg.LocalModelRelay == nil {
			err = fmt.Errorf("this runner does not relay a local model server")
		}
		if err != nil {
			return nil, nil, fmt.Errorf("docker: local model %q: %w", raw, err)
		}
		relayed, ports = append(relayed, raw), append(ports, port)
	}
	if len(relayed) == 0 {
		out, err := withoutProxyConfigKey(cfgJSON, runnerLocalModelsKey)
		return out, nil, err
	}
	slices.Sort(relayed)
	slices.Sort(ports)
	out, err := runner.WithProxyConfigKeys(cfgJSON, map[string]any{runnerLocalModelsKey: relayed})
	return out, slices.Compact(ports), err
}

// openLocalModels has the relay serve ports to the started proxy id alone: every address it holds on
// any network, the only sources the relay admits for runID.
func (d *Driver) openLocalModels(ctx context.Context, runID uuid.UUID, id string, ports []uint16) error {
	if len(ports) == 0 {
		return nil
	}
	res, err := d.cli.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if err != nil {
		return fmt.Errorf("docker: inspect proxy for the local model relay: %w", err)
	}
	var peers []netip.Addr
	if ns := res.Container.NetworkSettings; ns != nil {
		for _, ep := range ns.Networks {
			if ep == nil {
				continue
			}
			for _, a := range []netip.Addr{ep.IPAddress, ep.GlobalIPv6Address} {
				if a.IsValid() && !a.IsUnspecified() {
					peers = append(peers, a.Unmap())
				}
			}
		}
	}
	if len(peers) == 0 {
		return fmt.Errorf("docker: the proxy has no address for the local model relay to admit")
	}
	if err := d.cfg.LocalModelRelay.OpenLocalModel(runID, ports, peers); err != nil {
		return fmt.Errorf("docker: local model relay: %w", err)
	}
	return nil
}

// closeLocalModels ends runID's admission at the relay; its last run closes a port's listener.
func (d *Driver) closeLocalModels(runID uuid.UUID) {
	if d.cfg.LocalModelRelay != nil {
		d.cfg.LocalModelRelay.CloseLocalModel(runID)
	}
}

// laptopModelPort is the port of a laptop local model's base URL, whose host must be
// types.LaptopModelHost (the API holds it there at write).
func laptopModelPort(raw string) (uint16, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return 0, err
	}
	if strings.ToLower(strings.TrimSuffix(u.Hostname(), ".")) != types.LaptopModelHost {
		return 0, fmt.Errorf("a laptop local model must be on %s", types.LaptopModelHost)
	}
	port := map[string]uint64{"http": 80, "https": 443}[u.Scheme]
	if p := u.Port(); p != "" {
		if port, err = strconv.ParseUint(p, 10, 16); err != nil {
			return 0, fmt.Errorf("port %q", p)
		}
	}
	if port == 0 {
		return 0, fmt.Errorf("no port")
	}
	return uint16(port), nil
}
