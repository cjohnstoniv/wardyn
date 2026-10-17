// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import "github.com/cjohnstoniv/wardyn/internal/runner"

// withUpstreamProxy returns the proxy config b with upstream_proxy_url set to the runner's own
// corporate upstream proxy (Config.UpstreamProxyURL), on create and on revive alike for the same
// reason as runner.WithTrustedCA. It replaces whatever is there and leaves b untouched when u is empty.
func withUpstreamProxy(b []byte, u string) ([]byte, error) {
	if u == "" {
		return b, nil
	}
	return runner.WithProxyConfigKeys(b, map[string]any{"upstream_proxy_url": u})
}
