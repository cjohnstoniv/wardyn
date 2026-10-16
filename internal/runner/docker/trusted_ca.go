// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

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
	if pemBundle == "" {
		return b, nil
	}
	var data map[string]json.RawMessage
	if err := json.Unmarshal(b, &data); err != nil {
		return nil, err
	}
	if data == nil {
		return nil, errors.New("proxy config is empty")
	}
	v, err := json.Marshal(pemBundle)
	if err != nil {
		return nil, err
	}
	data["trusted_ca_pem"] = v
	return json.Marshal(data)
}
