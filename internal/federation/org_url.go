// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package federation

import (
	"errors"
	"net/url"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/hoptls"
)

// CheckOrgURL rejects URL components excluded from the persisted binding.
func CheckOrgURL(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || strings.Contains(raw, "#") {
		return errors.New("runner organisation URL must be an absolute HTTP(S) URL without user info, query or fragment")
	}
	return hoptls.CheckURL(raw)
}
