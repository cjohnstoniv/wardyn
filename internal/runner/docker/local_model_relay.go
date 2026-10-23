// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// runnerLocalModelsKey is proxy.Config.RunnerLocalModels' key.
const runnerLocalModelsKey = "runner_local_models"

// withRunnerLocalModels has a runner's relay serve each laptop local model in cfgJSON
// (LocalModelSettings.AllowOnLaptopRunners, #2957) and names them in the proxy's
// runner_local_models, which it takes off every other config: only this driver, on a runner, sets
// it. Off a runner (no ControlPlaneRelay) the flag changes nothing: the organisation's proxy reaches
// the server as any local model.
func (d *Driver) withRunnerLocalModels(cfgJSON []byte) ([]byte, error) {
	var c struct {
		LocalModels map[string]types.LocalModelSettings `json:"local_models"`
	}
	if err := json.Unmarshal(cfgJSON, &c); err != nil {
		return nil, fmt.Errorf("docker: local models: config: %w", err)
	}
	var relayed []string
	for raw, s := range c.LocalModels {
		if !s.AllowOnLaptopRunners || d.cfg.ControlPlaneRelay == nil {
			continue
		}
		port, err := laptopModelPort(raw)
		if err == nil && d.cfg.LocalModelRelay == nil {
			err = fmt.Errorf("this runner does not relay a local model server")
		}
		if err == nil {
			err = d.cfg.LocalModelRelay(port)
		}
		if err != nil {
			return nil, fmt.Errorf("docker: local model %q: %w", raw, err)
		}
		relayed = append(relayed, raw)
	}
	if len(relayed) == 0 {
		return withoutProxyConfigKey(cfgJSON, runnerLocalModelsKey)
	}
	slices.Sort(relayed)
	return runner.WithProxyConfigKeys(cfgJSON, map[string]any{runnerLocalModelsKey: relayed})
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
