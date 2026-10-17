// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"encoding/json"
	"errors"
)

// withTrustedCA returns the proxy config b with trusted_ca_pem set to the
// runner's own bundle (Config.TrustedCAPEM). startProxy applies it on create
// and on ReplaceProxy alike, since a revive rebuilds from the organisation's
// stored config, which never carries a CA for a runner. The value REPLACES
// whatever is there: the organisation's roots stay off a runner's proxy. An
// empty bundle leaves b untouched.
func withTrustedCA(b []byte, pemBundle string) ([]byte, error) {
	return withConfigString(b, "trusted_ca_pem", pemBundle)
}

// withUpstreamProxy returns the proxy config b with upstream_proxy_url set to the runner's own
// corporate upstream proxy (Config.UpstreamProxyURL), on create and on revive alike for the same
// reason as withTrustedCA. It replaces whatever is there and leaves b untouched when u is empty.
func withUpstreamProxy(b []byte, u string) ([]byte, error) {
	return withConfigString(b, "upstream_proxy_url", u)
}

// withConfigString sets the string key of the proxy config b to value; an empty value leaves b alone.
func withConfigString(b []byte, key, value string) ([]byte, error) {
	if value == "" {
		return b, nil
	}
	var data map[string]json.RawMessage
	if err := json.Unmarshal(b, &data); err != nil {
		return nil, err
	}
	if data == nil {
		return nil, errors.New("proxy config is empty")
	}
	v, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	data[key] = v
	return json.Marshal(data)
}
