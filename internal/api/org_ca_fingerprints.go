// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"fmt"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/certbundle"
)

// ParseOrgCAFingerprints reads WARDYN_ORG_CA_FINGERPRINTS: the SHA-256
// fingerprints of the CA certificates an operator publishes for `wardyn ca
// add`, comma-separated, each "sha256:" and 64 hex digits (colons inside the
// hex are dropped). It returns them as "sha256:<lowercase hex>" for
// api.Config.OrgCAFingerprints; empty => nil, nothing published. A malformed
// entry refuses boot, naming the setting.
func ParseOrgCAFingerprints(raw string) ([]string, error) {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if strings.TrimSpace(part) == "" {
			continue
		}
		fp, err := certbundle.NormalizeFingerprint(part)
		if err != nil {
			return nil, fmt.Errorf("WARDYN_ORG_CA_FINGERPRINTS: %w", err)
		}
		out = append(out, fp)
	}
	return out, nil
}
