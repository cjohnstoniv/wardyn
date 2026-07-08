// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package embedded

import "github.com/cjohnstoniv/wardyn/internal/identity"

// Self-register the embedded provider as the default identity seam impl, so a
// blank import (cmd/wardynd) makes "embedded" selectable. The revocation store +
// signing key + audit recorder are supplied by the control plane via Deps.
func init() {
	identity.Register("embedded", func(d identity.Deps) (identity.Provider, error) {
		p, err := New(d.SigningKey, d.TrustDomain, d.Revocations, d.Audit)
		if err != nil {
			return nil, err
		}
		return p, nil
	})
}
