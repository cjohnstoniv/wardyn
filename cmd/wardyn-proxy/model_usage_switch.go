// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"log/slog"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
)

// applyModelUsageSwitch is the admin's opt-out from model token counting: WARDYN_MODEL_USAGE=off turns this
// proxy's meter off. Counting is on by default, so unset, empty and "on" change nothing. The setting
// travels in the proxy's environment and not in its config, so a proxy older than the meter ignores it
// where a config key would stop it starting (runner.ProxySidecarEnvKnobs). It returns the exit code main
// must use: 0 to carry on, 2 for a value that states no intent, which must not silently count.
func applyModelUsageSwitch(cfg *proxy.Config, v string) int {
	switch v {
	case "", "on":
	case "off":
		slog.Info("wardyn-proxy: WARDYN_MODEL_USAGE=off, model tokens are not counted")
		cfg.DisableModelUsage = true
	default:
		slog.Error("wardyn-proxy: WARDYN_MODEL_USAGE has an unrecognized value; want on or off", slog.String("value", v))
		return 2
	}
	return 0
}
